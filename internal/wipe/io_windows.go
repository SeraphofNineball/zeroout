package wipe

import (
	"errors"
	"os"
	"syscall"
)

// Windows support is for development builds; unbuffered I/O is not used yet.

func createFile(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	return f, false, err
}

func openRead(path string) (*os.File, bool, error) {
	f, err := os.Open(path)
	return f, false, err
}

const (
	errorHandleDiskFull syscall.Errno = 39
	errorDiskFull       syscall.Errno = 112
)

func isNoSpace(err error) bool {
	return errors.Is(err, errorDiskFull) || errors.Is(err, errorHandleDiskFull)
}

func dropCaches(*os.File) {}
