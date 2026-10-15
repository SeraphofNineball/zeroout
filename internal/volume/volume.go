// Package volume reports usage and mount details for the volumes ZeroOut can wipe.
package volume

import (
	"os"
	"path/filepath"
	"sort"
)

// Usage is a snapshot of a filesystem's space accounting, in bytes.
type Usage struct {
	Total int64 `json:"total"`
	// Free counts every unallocated block, including blocks reserved for root.
	Free int64 `json:"free"`
	// Avail is what an unprivileged writer may use (Free minus the root reserve).
	Avail int64 `json:"avail"`
}

// Info describes one wipe target.
type Info struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	FSType  string `json:"fsType,omitempty"`
	Source  string `json:"source,omitempty"`
	IsMount bool   `json:"isMount"`
	Usage
	Reserved int64  `json:"reserved"`
	Error    string `json:"error,omitempty"`
}

// Describe gathers usage and mount information for path.
func Describe(name, path string) Info {
	info := Info{Name: name, Path: path}
	u, err := Stat(path)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Usage = u
	info.Reserved = u.Free - u.Avail
	info.IsMount, info.FSType, info.Source = mountInfo(path)
	return info
}

// List describes every directory directly under dir. In the container each one
// is expected to be a bind mount of a host volume (e.g. /targets/volume1).
func List(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, Describe(e.Name(), filepath.Join(dir, e.Name())))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
