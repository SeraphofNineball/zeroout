package volume

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Stat returns the space accounting for the filesystem holding path.
func Stat(path string) (Usage, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return Usage{}, err
	}
	bs := int64(s.Bsize)
	return Usage{
		Total: int64(s.Blocks) * bs,
		Free:  int64(s.Bfree) * bs,
		Avail: int64(s.Bavail) * bs,
	}, nil
}

// MountEntry is a mount point's line in /proc/self/mountinfo.
type MountEntry struct {
	MajMin string // device number, e.g. "252:1"
	FSType string
	Source string
}

// Mount returns the mount whose mount point is exactly path.
func Mount(path string) (MountEntry, bool) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return MountEntry{}, false
	}
	defer f.Close()
	want := filepath.Clean(path)
	var m MountEntry
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		// Format: id parent major:minor root mountpoint options [optional...] - fstype source superoptions
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(pre)
		if len(fields) < 5 || unescape(fields[4]) != want {
			continue
		}
		// Later entries shadow earlier ones on the same mount point; keep scanning.
		found = true
		m = MountEntry{MajMin: fields[2]}
		if pf := strings.Fields(post); len(pf) >= 2 {
			m.FSType, m.Source = pf[0], unescape(pf[1])
		}
	}
	return m, found
}

func mountInfo(path string) (isMount bool, fsType, source string) {
	m, ok := Mount(path)
	return ok, m.FSType, m.Source
}

// DeviceID identifies the filesystem holding path.
func DeviceID(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}

// unescape decodes the octal escapes (\040 for space, etc.) used in mountinfo.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, ok := octal(s[i+1 : i+4]); ok {
				b.WriteByte(v)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func octal(s string) (byte, bool) {
	var v int
	for _, c := range s {
		if c < '0' || c > '7' {
			return 0, false
		}
		v = v*8 + int(c-'0')
	}
	return byte(v), v < 256
}
