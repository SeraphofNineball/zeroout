package wipe

import (
	"errors"
	"os"
	"syscall"
)

// createFile opens path for writing with O_DIRECT so multi-terabyte fills
// bypass (and don't evict) the page cache. Falls back to buffered I/O on
// filesystems that reject O_DIRECT.
func createFile(path string) (f *os.File, direct bool, err error) {
	const flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	f, err = os.OpenFile(path, flags|syscall.O_DIRECT, 0o600)
	if errors.Is(err, syscall.EINVAL) {
		f, err = os.OpenFile(path, flags, 0o600)
		return f, false, err
	}
	return f, err == nil, err
}

// openRead opens path for verification reads, bypassing the page cache so the
// data really comes from disk.
func openRead(path string) (f *os.File, direct bool, err error) {
	f, err = os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
	if errors.Is(err, syscall.EINVAL) {
		f, err = os.Open(path)
		return f, false, err
	}
	return f, err == nil, err
}

// isNoSpace reports whether err means the volume (or a quota) is full.
func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// dropCaches asks the kernel to forget cached pages of a file before it is
// re-read for verification (only matters when O_DIRECT was unavailable).
func dropCaches(f *os.File) {
	const fadvDontneed = 4
	syscall.Syscall6(syscall.SYS_FADVISE64, f.Fd(), 0, 0, fadvDontneed, 0, 0)
}
