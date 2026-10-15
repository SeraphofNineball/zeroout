package volume

import (
	"syscall"
	"unsafe"
)

var procGetDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// Stat returns the space accounting for the volume holding path.
func Stat(path string) (Usage, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return Usage{}, err
	}
	var avail, total, free uint64
	r, _, err := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&free)),
	)
	if r == 0 {
		return Usage{}, err
	}
	return Usage{Total: int64(total), Free: int64(free), Avail: int64(avail)}, nil
}

// mountInfo is not implemented on Windows yet (development builds only);
// every target is treated as a mount point.
func mountInfo(path string) (isMount bool, fsType, source string) {
	return true, "", ""
}
