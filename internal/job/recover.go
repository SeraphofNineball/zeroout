package job

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"zeroout/internal/aligned"
	"zeroout/internal/blockdev"
	"zeroout/internal/carve"
	"zeroout/internal/volume"
)

// RecoverSpec asks for items of a scan to be copied to a destination.
type RecoverSpec struct {
	ScanID      string    `json:"scanId"`
	Destination string    `json:"destination"`
	Items       []int     `json:"items,omitempty"` // explicit item numbers; else Query
	Query       ItemQuery `json:"query"`

	// Filled in as the job runs.
	Dir         string `json:"dir,omitempty"`
	Written     int64  `json:"written"`
	Overwritten int64  `json:"overwritten"`
	Errors      int64  `json:"errors"`
}

// Destination is a place recovered files can be written.
type Destination struct {
	volume.Info
	// SameDeviceAs lists target volumes on the same filesystem; recovering
	// one of them onto this destination is refused.
	SameDeviceAs []string `json:"sameDeviceAs"`
}

// Destinations lists the recovery destinations under RecoverDir.
func (m *Manager) Destinations() []Destination {
	out := []Destination{}
	dests, err := volume.List(m.opts.RecoverDir)
	if err != nil {
		return out
	}
	targets, _ := volume.List(m.opts.TargetsDir)
	for _, d := range dests {
		dst := Destination{Info: d, SameDeviceAs: []string{}}
		switch d.FSType {
		case "overlay", "tmpfs", "aufs":
			// Docker creates a missing bind-mount source (e.g. an unplugged USB
			// drive's mount point) as an empty folder on the system disk, which
			// shows up here as overlay. Never fill the system disk.
			dst.Error = "not an external disk (is the drive plugged in? restart ZeroOut after connecting it)"
		}
		if did, err := volume.DeviceID(d.Path); err == nil {
			for _, t := range targets {
				if tid, err := volume.DeviceID(t.Path); err == nil && tid == did {
					dst.SameDeviceAs = append(dst.SameDeviceAs, t.Name)
				}
			}
		}
		out = append(out, dst)
	}
	return out
}

// StartRecover copies the selected items to a destination.
func (m *Manager) StartRecover(spec RecoverSpec) (*Job, error) {
	sum, items, err := m.loadItems(spec.ScanID)
	if err != nil {
		return nil, err
	}
	var idx []int
	if len(spec.Items) > 0 {
		for _, n := range spec.Items {
			if n < 0 || n >= len(items) {
				return nil, invalid("item %d does not exist", n)
			}
		}
		idx = slices.Clone(spec.Items)
	} else {
		idx = selectItems(sum, items, spec.Query)
	}
	if len(idx) == 0 {
		return nil, invalid("no files selected")
	}
	var total int64
	for _, i := range idx {
		total += items[i].Size
	}

	i := slices.IndexFunc(m.Destinations(), func(d Destination) bool { return d.Name == spec.Destination })
	if i < 0 {
		return nil, invalid("unknown destination %q", spec.Destination)
	}
	dst := m.Destinations()[i]
	if dst.Error != "" {
		return nil, invalid("destination %s: %s", dst.Name, dst.Error)
	}
	if !dst.IsMount && !m.opts.AllowUnmounted {
		return nil, invalid("%s is not a mount point; files would be written inside the container", dst.Path)
	}
	if slices.Contains(dst.SameDeviceAs, sum.Volume) && !m.opts.AllowUnmounted {
		return nil, invalid("%s is on the same disk as %s; writing there could overwrite the data being recovered", dst.Name, sum.Volume)
	}
	const headroom = 1 << 30
	if total+headroom > dst.Avail {
		return nil, invalid("%s needs %s but %s has only %s free", "the selection", fmtBytes(total), dst.Name, fmtBytes(dst.Avail))
	}
	if _, err := m.volume(sum.Volume); err != nil {
		return nil, err
	}

	spec.Dir = filepath.Join(dst.Path, "ZeroOut-Recovered", sum.ID)
	j := &Job{Kind: KindRecover, Volume: sum.Volume, ScanID: sum.ID, Recover: &spec}
	j.logf("Recovering %d files (%s) from scan %s to %s", len(idx), fmtBytes(total), sum.ID, spec.Dir)
	return m.launch(j, func(ctx context.Context) error {
		return m.runRecover(ctx, j, sum, items, idx, total)
	})
}

func (m *Manager) runRecover(ctx context.Context, j *Job, sum *ScanSummary, items []carve.Item, idx []int, total int64) error {
	spec := j.Recover
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return err
	}
	r, fs, err := m.openFS(sum.Volume)
	if err != nil {
		return err
	}
	defer r.Close()

	mf, err := os.Create(filepath.Join(spec.Dir, "manifest.csv"))
	if err != nil {
		return err
	}
	defer mf.Close()
	manifest := csv.NewWriter(mf)
	manifest.Write([]string{"file", "type", "offset", "size", "status"})
	defer manifest.Flush()

	meter := newMeter(total)
	var done int64
	buf := aligned.Buffer(4 << 20)
	for _, n := range idx {
		if err := ctx.Err(); err != nil {
			return err
		}
		it := items[n]
		v := view(sum, n, it)
		name := fmt.Sprintf("%012x", it.Offset)
		if v.Truncated {
			name += "-partial"
		}
		rel := filepath.Join(v.Category, v.Ext, name+"."+v.Ext)
		status := "recovered"
		err := checkFree(fs, it)
		if err == nil {
			err = copyItem(ctx, r, it, filepath.Join(spec.Dir, rel), buf, func(k int64) {
				if p, ok := meter.update(done + k); ok {
					p.Phase = "recovering"
					m.setProgress(j, p)
				}
			})
		}
		m.mu.Lock()
		switch {
		case err == nil:
			spec.Written++
			j.Found = spec.Written
		case errors.Is(err, ErrOverwritten):
			spec.Overwritten++
			status, rel = "overwritten since scan", ""
		case errors.Is(err, context.Canceled):
			m.mu.Unlock()
			return err
		default:
			spec.Errors++
			status, rel = "error: "+err.Error(), ""
			j.logf("%s at offset %d: %v", v.Ext, it.Offset, err)
		}
		m.mu.Unlock()
		manifest.Write([]string{filepath.ToSlash(rel), v.Ext, strconv.FormatInt(it.Offset, 10), strconv.FormatInt(it.Size, 10), status})
		done += it.Size
	}
	m.logf(j, "Wrote %d files to %s; %d skipped (space reused since the scan); %d errors",
		spec.Written, spec.Dir, spec.Overwritten, spec.Errors)
	if spec.Errors > 0 && spec.Written == 0 {
		return fmt.Errorf("no files could be recovered")
	}
	return nil
}

func copyItem(ctx context.Context, r *blockdev.Reader, it carve.Item, path string, buf []byte, progress func(int64)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	var copied int64
	for copied < it.Size {
		if err := ctx.Err(); err != nil {
			f.Close()
			os.Remove(path)
			return err
		}
		n := min(int64(len(buf)), it.Size-copied)
		got, err := r.ReadAt(buf[:n], it.Offset+copied)
		if got > 0 {
			if _, werr := f.Write(buf[:got]); werr != nil {
				f.Close()
				return werr
			}
			copied += int64(got)
			progress(copied)
		}
		if err != nil && int64(got) < n {
			f.Close()
			return err
		}
	}
	return f.Close()
}
