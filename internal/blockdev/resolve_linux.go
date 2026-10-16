package blockdev

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"zeroout/internal/volume"
)

// Resolve finds a readable device for the filesystem mounted at volumePath.
// override, if set, is used instead of auto-detection.
func Resolve(volumePath, override string) (*Device, error) {
	if override != "" {
		return resolveOverride(override)
	}
	m, ok := volume.Mount(volumePath)
	if !ok {
		return nil, fmt.Errorf("%s is not a mount point", volumePath)
	}
	switch m.FSType {
	case "ext2", "ext3", "ext4":
	default:
		return nil, fmt.Errorf("scanning %s filesystems is not supported yet (ext2/3/4 only)", m.FSType)
	}
	sys := "/sys/dev/block/" + m.MajMin
	if _, err := os.Stat(sys + "/bcache"); err == nil {
		return resolveBcache(sys, m.MajMin)
	}
	if node := findNode(m.MajMin); node != "" {
		return probeNode(node, "filesystem device")
	}
	return nil, &NotFoundError{
		Msg:     "Scanning needs read access to the volume's block device. Add it to the container's devices.",
		HostDev: hostDevName(sys),
	}
}

// resolveBcache prefers the backing (HDD) device: reading through bcache
// would push every scanned block into the SSD cache. That is only safe when
// the cache holds no unwritten (dirty) data.
func resolveBcache(sys, majmin string) (*Device, error) {
	mode := bracketed(readSys(sys + "/bcache/cache_mode"))
	state := readSys(sys + "/bcache/state")
	dirty := readSys(sys + "/bcache/dirty_data")
	clean := state == "clean" || state == "no cache" || isZeroSize(dirty)

	slaves, _ := os.ReadDir(sys + "/slaves")
	if clean && len(slaves) == 1 {
		bsys := "/sys/class/block/" + slaves[0].Name()
		if node := findNode(readSys(bsys + "/dev")); node != "" {
			return probeNode(node, fmt.Sprintf("bcache backing device, cache mode %s (reads bypass the SSD cache)", mode))
		}
		return nil, &NotFoundError{
			Msg: "This volume sits behind an SSD cache (bcache). Pass its backing device into the container " +
				"so scans read the hard drives directly instead of flooding the cache.",
			HostDev: hostDevName(bsys),
		}
	}
	if node := findNode(majmin); node != "" {
		return probeNode(node, fmt.Sprintf("bcache device, cache mode %s (cache has unwritten data, reading through it)", mode))
	}
	return nil, &NotFoundError{
		Msg:     "The SSD cache holds unwritten data, so the bcache device itself must be read. Add it to the container's devices.",
		HostDev: hostDevName(sys),
	}
}

func probeNode(path, via string) (*Device, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	off, ok := probeOffset(f)
	if !ok {
		return nil, fmt.Errorf("no ext2/3/4 filesystem found on %s", path)
	}
	return &Device{Path: path, Offset: off, Via: via}, nil
}

// findNode returns a block device node under /dev with the given major:minor.
func findNode(majmin string) string {
	maj, min, ok := splitMajMin(majmin)
	if !ok {
		return ""
	}
	var found string
	errStop := errors.New("stop")
	filepath.WalkDir("/dev", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch path {
			case "/dev/pts", "/dev/shm", "/dev/mqueue":
				return filepath.SkipDir
			}
			return nil
		}
		var st syscall.Stat_t
		if syscall.Stat(path, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFBLK {
			return nil
		}
		if devMajor(st.Rdev) == maj && devMinor(st.Rdev) == min {
			found = path
			return errStop
		}
		return nil
	})
	return found
}

func splitMajMin(s string) (uint64, uint64, bool) {
	a, b, ok := strings.Cut(s, ":")
	maj, err1 := strconv.ParseUint(a, 10, 32)
	min, err2 := strconv.ParseUint(b, 10, 32)
	return maj, min, ok && err1 == nil && err2 == nil
}

func devMajor(d uint64) uint64 { return (d>>8)&0xfff | (d>>32)&^0xfff }
func devMinor(d uint64) uint64 { return d&0xff | (d>>12)&^0xff }

// hostDevName names the device as the host sees it, preferring the stable
// /dev/mapper name for device-mapper (LVM) devices.
func hostDevName(sys string) string {
	if name := readSys(sys + "/dm/name"); name != "" {
		return "/dev/mapper/" + name
	}
	if real, err := filepath.EvalSymlinks(sys); err == nil {
		return "/dev/" + filepath.Base(real)
	}
	return ""
}

func readSys(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

// bracketed picks the active option from sysfs lists like "a [b] c".
func bracketed(s string) string {
	if i := strings.IndexByte(s, '['); i >= 0 {
		if j := strings.IndexByte(s[i:], ']'); j > 0 {
			return s[i+1 : i+j]
		}
	}
	return s
}

// isZeroSize parses bcache sizes like "0.0k" or "0".
func isZeroSize(s string) bool {
	v, err := strconv.ParseFloat(strings.TrimRight(s, "kMGTPEB"), 64)
	return err == nil && v == 0
}

func openDirect(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
	if errors.Is(err, syscall.EINVAL) {
		f, err = os.Open(path)
		return f, false, err
	}
	return f, err == nil, err
}

// Sync flushes all filesystems so on-disk bitmaps reflect recent changes.
func Sync() { syscall.Sync() }
