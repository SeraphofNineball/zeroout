package blockdev

import "os"

// Resolve only supports configured image files on Windows (development).
func Resolve(volumePath, override string) (*Device, error) {
	if override != "" {
		return resolveOverride(override)
	}
	return nil, errUnsupported
}

func openDirect(path string) (*os.File, bool, error) {
	f, err := os.Open(path)
	return f, false, err
}

func Sync() {}
