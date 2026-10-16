// Package blockdev finds and reads the raw block device behind a mounted
// volume, for scanning its free space.
package blockdev

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"zeroout/internal/aligned"
	"zeroout/internal/ext4"
)

// Device is where a volume's filesystem can be read from.
type Device struct {
	Path   string `json:"path"`   // device node (or image file) inside the container
	Offset int64  `json:"offset"` // byte offset of the filesystem within it
	Via    string `json:"via"`    // human description of how it was chosen
}

// NotFoundError explains which device must be passed into the container.
type NotFoundError struct {
	Msg     string `json:"msg"`
	HostDev string `json:"hostDev,omitempty"` // host path to add under compose "devices:"
}

func (e *NotFoundError) Error() string { return e.Msg }

// ParseOverrides parses ZEROOUT_DEVICES ("volume1=/dev/x,volume2=/img").
func ParseOverrides(s string) map[string]string {
	m := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		if name, path, ok := strings.Cut(strings.TrimSpace(part), "="); ok && name != "" && path != "" {
			m[name] = path
		}
	}
	return m
}

// probeOffset finds where an ext filesystem starts in r: at 0, or after a
// header such as bcache's (8 KiB by default).
func probeOffset(r io.ReaderAt) (int64, bool) {
	for off := int64(0); off <= 1<<20; off += 512 {
		if ext4.Probe(r, off) {
			return off, true
		}
	}
	return 0, false
}

// Reader reads a device with O_DIRECT where possible, so scanning terabytes
// doesn't churn the page cache. ReadAt accepts any offset and length and is
// safe for concurrent use.
type Reader struct {
	f      *os.File
	base   int64
	direct bool
}

// Open opens d for reading; offsets passed to ReadAt are filesystem-relative.
func Open(d *Device) (*Reader, error) {
	f, direct, err := openDirect(d.Path)
	if err != nil {
		return nil, err
	}
	return &Reader{f: f, base: d.Offset, direct: direct}, nil
}

func (r *Reader) Close() error { return r.f.Close() }

func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	abs := r.base + off
	const a = aligned.Align
	if !r.direct || (abs%a == 0 && len(p)%a == 0 && isAligned(p)) {
		return r.f.ReadAt(p, abs)
	}
	// Unaligned request: read the covering aligned range and copy out.
	start := abs / a * a
	end := (abs + int64(len(p)) + a - 1) / a * a
	buf := aligned.Buffer(int(end - start))
	n, err := r.f.ReadAt(buf, start)
	skip := int(abs - start)
	if n <= skip {
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	c := copy(p, buf[skip:n])
	if c < len(p) {
		if err == nil {
			err = io.EOF
		}
		return c, err
	}
	return c, nil
}

func isAligned(p []byte) bool {
	return len(p) == 0 || aligned.AddrOf(p)%aligned.Align == 0
}

// resolveOverride uses an explicitly configured device or image path.
func resolveOverride(path string) (*Device, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("configured device: %w", err)
	}
	defer f.Close()
	off, ok := probeOffset(f)
	if !ok {
		return nil, fmt.Errorf("no ext2/3/4 filesystem found on configured device %s", path)
	}
	return &Device{Path: path, Offset: off, Via: "configured in ZEROOUT_DEVICES"}, nil
}

var errUnsupported = errors.New("scanning raw devices is only supported on Linux")
