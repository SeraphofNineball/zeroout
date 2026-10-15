// Package wipe overwrites a volume's free space by filling it with files of a
// chosen pattern, one pass at a time, then deleting them.
package wipe

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"zeroout/internal/volume"
)

const (
	// DirName is the hidden directory fill files are written to on the target.
	DirName = ".zeroout"

	// MaxPasses caps pass count (35 is the Gutmann method, the longest in common use).
	MaxPasses = 35

	DefaultBlockBytes = 16 << 20
	DefaultChunkBytes = 64 << 30

	align = 4096
)

type Mode string

const (
	// ModeMargin stops each pass when only MarginBytes of user-available space remains.
	ModeMargin Mode = "margin"
	// ModeFull writes until the filesystem refuses more data, including
	// root-reserved blocks when running as root.
	ModeFull Mode = "full"
)

type Config struct {
	Target      string
	Passes      []Pattern
	Mode        Mode
	MarginBytes int64
	Verify      bool
	// FirstPass skips already-completed passes when resuming (0-based).
	FirstPass int

	BlockBytes int   // size of each write; default DefaultBlockBytes
	ChunkBytes int64 // max size of each fill file; default DefaultChunkBytes
	MaxBytes   int64 // caps bytes written per pass; 0 = no cap (for testing)
}

func (c *Config) normalize() error {
	if len(c.Passes) == 0 || len(c.Passes) > MaxPasses {
		return fmt.Errorf("passes must be between 1 and %d", MaxPasses)
	}
	if c.FirstPass < 0 || c.FirstPass >= len(c.Passes) {
		return fmt.Errorf("first pass %d out of range", c.FirstPass+1)
	}
	switch c.Mode {
	case ModeMargin:
		if c.MarginBytes < 0 {
			return errors.New("margin cannot be negative")
		}
	case ModeFull:
	default:
		return fmt.Errorf("unknown mode %q", c.Mode)
	}
	if c.BlockBytes <= 0 {
		c.BlockBytes = DefaultBlockBytes
	}
	if c.ChunkBytes <= 0 {
		c.ChunkBytes = DefaultChunkBytes
	}
	c.BlockBytes = max(c.BlockBytes/align*align, align)
	c.ChunkBytes = max(c.ChunkBytes/align*align, align)
	if fi, err := os.Stat(c.Target); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", c.Target)
	}
	return nil
}

type Phase string

const (
	PhaseWriting   Phase = "writing"
	PhaseVerifying Phase = "verifying"
	PhaseCleaning  Phase = "cleaning"
)

// Progress describes the state of a running wipe.
type Progress struct {
	Pass    int     `json:"pass"` // 1-based
	Passes  int     `json:"passes"`
	Pattern string  `json:"pattern"`
	Phase   Phase   `json:"phase"`
	Done    int64   `json:"done"`  // bytes processed in this phase
	Total   int64   `json:"total"` // expected bytes for this phase (an estimate in full mode)
	Rate    float64 `json:"rate"`  // bytes/second, smoothed
	// ETASeconds estimates the time left for the whole job, 0 if unknown.
	ETASeconds float64 `json:"etaSeconds"`
}

type Hooks struct {
	Progress func(Progress)
	Log      func(string)
	// PassDone is called after each pass's fill files have been deleted.
	PassDone func(pass int) // 0-based
}

// Run performs the configured passes on cfg.Target. Fill files are always
// removed before it returns, including on error or cancellation.
func Run(ctx context.Context, cfg Config, h Hooks) (err error) {
	if err := cfg.normalize(); err != nil {
		return err
	}
	dir := filepath.Join(cfg.Target, DirName)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing stale %s: %w", dir, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil && err == nil {
			err = fmt.Errorf("cleanup: %w", rmErr)
		}
		syncDir(cfg.Target)
	}()

	r := &runner{cfg: cfg, dir: dir, h: h, buf: alignedBuffer(cfg.BlockBytes)}
	for i := cfg.FirstPass; i < len(cfg.Passes); i++ {
		if err := r.pass(ctx, i); err != nil {
			return err
		}
		if h.PassDone != nil {
			h.PassDone(i)
		}
	}
	return nil
}

type runner struct {
	cfg  Config
	dir  string
	h    Hooks
	buf  []byte
	want []byte

	prog     Progress
	estimate int64 // bytes expected per pass
	lastEmit time.Time
	lastDone int64
}

func (r *runner) logf(format string, args ...any) {
	if r.h.Log != nil {
		r.h.Log(fmt.Sprintf(format, args...))
	}
}

func (r *runner) pass(ctx context.Context, i int) error {
	cfg := r.cfg
	p := cfg.Passes[i]
	u, err := volume.Stat(cfg.Target)
	if err != nil {
		return err
	}
	budget := int64(-1) // -1: until the volume is full
	r.estimate = u.Free
	if cfg.Mode == ModeMargin {
		budget = (u.Avail - cfg.MarginBytes) / align * align
		if budget < int64(cfg.BlockBytes) {
			return fmt.Errorf("only %s available, which is not more than the %s margin", fmtBytes(u.Avail), fmtBytes(cfg.MarginBytes))
		}
		r.estimate = budget
	}
	if cfg.MaxBytes > 0 && (budget < 0 || budget > cfg.MaxBytes) {
		budget = cfg.MaxBytes / align * align
		r.estimate = budget
	}

	var seed [32]byte
	rand.Read(seed[:])
	r.prog = Progress{Pass: i + 1, Passes: len(cfg.Passes), Pattern: p.String()}
	r.logf("Pass %d/%d (%s): writing ~%s", i+1, len(cfg.Passes), p, fmtBytes(r.estimate))
	start := time.Now()
	r.phase(PhaseWriting, r.estimate)

	type chunk struct {
		path string
		size int64
	}
	var chunks []chunk
	var written int64
	for n := 0; budget < 0 || written < budget; n++ {
		limit := cfg.ChunkBytes
		if budget >= 0 {
			limit = min(limit, budget-written)
		}
		path := filepath.Join(r.dir, fmt.Sprintf("fill-%05d.bin", n))
		size, full, err := r.writeChunk(ctx, path, limit, newSource(p, seed, n))
		if size > 0 || err == nil {
			chunks = append(chunks, chunk{path, size})
		}
		written += size
		if err != nil {
			return err
		}
		if full {
			break
		}
	}
	elapsed := time.Since(start)
	r.logf("Pass %d: wrote %s in %s (%s/s)", i+1, fmtBytes(written), elapsed.Round(time.Second), fmtBytes(int64(float64(written)/max(elapsed.Seconds(), 1e-3))))

	if cfg.Verify {
		r.phase(PhaseVerifying, written)
		for n, c := range chunks {
			if err := r.verifyChunk(ctx, c.path, c.size, newSource(p, seed, n)); err != nil {
				return err
			}
		}
		r.logf("Pass %d: verified %s", i+1, fmtBytes(written))
	}

	r.phase(PhaseCleaning, 0)
	for _, c := range chunks {
		if err := os.Remove(c.path); err != nil {
			return err
		}
	}
	syncDir(r.dir)
	return nil
}

// writeChunk fills path with up to limit bytes (limit < 0: no limit). full
// reports that the volume ran out of space.
func (r *runner) writeChunk(ctx context.Context, path string, limit int64, src *source) (written int64, full bool, err error) {
	f, direct, err := createFile(path)
	if err != nil {
		return 0, false, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil && !isNoSpace(cerr) {
			err = cerr
		}
	}()
	if src.constant() {
		src.fillAt(r.buf, 0)
	}
	write := func(step int) error {
		for limit < 0 || written < limit {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := step
			if limit >= 0 {
				n = int(min(int64(n), limit-written))
			}
			b := r.buf[:n]
			if !src.constant() {
				src.fillAt(b, written)
			}
			w, err := f.Write(b)
			written += int64(w)
			r.advance(int64(w))
			if err != nil {
				if isNoSpace(err) {
					full = true
					return nil
				}
				return err
			}
		}
		return nil
	}
	if err := write(len(r.buf)); err != nil {
		return written, full, err
	}
	if full && direct {
		// O_DIRECT stops at the first whole block that doesn't fit. Top off the
		// remaining space with small buffered writes.
		f.Close()
		if f, err = os.OpenFile(path, os.O_WRONLY, 0); err != nil {
			return written, true, err
		}
		if _, err := f.Seek(written, io.SeekStart); err != nil {
			return written, true, err
		}
		if err := write(align); err != nil {
			return written, true, err
		}
		full = true
	}
	if err := f.Sync(); err != nil && !isNoSpace(err) {
		return written, full, err
	}
	return written, full, nil
}

func (r *runner) verifyChunk(ctx context.Context, path string, size int64, src *source) error {
	f, direct, err := openRead(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if !direct {
		dropCaches(f)
	}
	if r.want == nil {
		r.want = make([]byte, len(r.buf))
	}
	if src.constant() {
		src.fillAt(r.want, 0)
	}
	for off := int64(0); off < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Reads stay block-aligned for O_DIRECT; only the final one comes back short.
		n, err := f.Read(r.buf)
		if n > 0 {
			got := r.buf[:min(int64(n), size-off)]
			want := r.want[:len(got)]
			if !src.constant() {
				src.fillAt(want, off)
			}
			if !bytes.Equal(got, want) {
				at := off
				for i := range got {
					if got[i] != want[i] {
						at += int64(i)
						break
					}
				}
				return fmt.Errorf("verification failed: %s differs at byte %d", filepath.Base(path), at)
			}
			off += int64(len(got))
			r.advance(int64(len(got)))
		}
		if err == io.EOF {
			if off < size {
				return fmt.Errorf("verification failed: %s is %d bytes, expected %d", filepath.Base(path), off, size)
			}
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) phase(ph Phase, total int64) {
	r.prog.Phase, r.prog.Done, r.prog.Total, r.prog.Rate = ph, 0, total, 0
	r.lastDone = 0
	r.emit()
}

func (r *runner) advance(n int64) {
	r.prog.Done += n
	if time.Since(r.lastEmit) >= 500*time.Millisecond {
		r.emit()
	}
}

func (r *runner) emit() {
	now := time.Now()
	if dt := now.Sub(r.lastEmit).Seconds(); dt > 0 && dt < 10 && r.prog.Done >= r.lastDone {
		inst := float64(r.prog.Done-r.lastDone) / dt
		if r.prog.Rate == 0 {
			r.prog.Rate = inst
		} else {
			r.prog.Rate = 0.8*r.prog.Rate + 0.2*inst
		}
	}
	r.lastEmit, r.lastDone = now, r.prog.Done

	r.prog.ETASeconds = 0
	if r.prog.Rate > 0 {
		left := max(r.prog.Total-r.prog.Done, 0)
		perPass := r.estimate
		if r.cfg.Verify {
			if r.prog.Phase == PhaseWriting {
				left += r.estimate
			}
			perPass *= 2
		}
		left += int64(r.prog.Passes-r.prog.Pass) * perPass
		r.prog.ETASeconds = float64(left) / r.prog.Rate
	}
	if r.h.Progress != nil {
		r.h.Progress(r.prog)
	}
}

// alignedBuffer returns a buffer whose start is aligned for O_DIRECT.
func alignedBuffer(size int) []byte {
	b := make([]byte, size+align)
	off := 0
	if rem := int(uintptr(unsafe.Pointer(&b[0])) & (align - 1)); rem != 0 {
		off = align - rem
	}
	return b[off : off+size : off+size]
}

func syncDir(path string) {
	if d, err := os.Open(path); err == nil {
		d.Sync()
		d.Close()
	}
}

func fmtBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
