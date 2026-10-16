package carve

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"zeroout/internal/ext4"
)

// TestRealExt4 builds genuine ext4 filesystems with mkfs.ext4, writes files
// with debugfs (no mount needed), deletes some, and checks the scanner
// recovers exactly the deleted ones.
func TestRealExt4(t *testing.T) {
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	files := testFiles(t)
	layouts := map[string][]string{
		"4k":      {"-b", "4096"},
		"1k":      {"-b", "1024"},
		"meta_bg": {"-b", "4096", "-O", "meta_bg,^resize_inode"},
		"64bit":   {"-b", "4096", "-O", "64bit"},
		"no_csum": {"-b", "4096", "-O", "^metadata_csum,^uninit_bg"},
	}
	for name, opts := range layouts {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			img := filepath.Join(dir, "fs.img")
			if err := os.WriteFile(img, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(img, 96<<20); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"-q", "-F", "-E", "nodiscard"}, opts...)
			run(t, "mkfs.ext4", append(args, img)...)

			var cmds strings.Builder
			for k, data := range files {
				p := filepath.Join(dir, "src-"+k)
				os.WriteFile(p, data, 0o600)
				fmt.Fprintf(&cmds, "write %s %s\n", p, k)
			}
			for k := range files {
				if k != "png" { // png stays live
					fmt.Fprintf(&cmds, "rm %s\n", k)
				}
			}
			cmdFile := filepath.Join(dir, "cmds")
			os.WriteFile(cmdFile, []byte(cmds.String()), 0o600)
			run(t, "debugfs", "-w", "-f", cmdFile, img)

			raw, err := os.ReadFile(img)
			if err != nil {
				t.Fatal(err)
			}
			fs, err := ext4.Open(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]Item{}
			s := &Scanner{R: bytes.NewReader(raw), BlockSize: fs.BlockSize, TotalBlocks: fs.Blocks,
				OnItem: func(it Item) error { found[Kinds[it.Kind].Ext] = it; return nil }}
			if err := fs.FreeExtents(context.Background(), func(e ext4.Extent) error {
				return s.Scan(context.Background(), Extent{e.Start, e.Count})
			}); err != nil {
				t.Fatal(err)
			}
			for k, data := range files {
				it, ok := found[k]
				if k == "png" {
					if ok {
						t.Errorf("live png reported as deleted")
					}
					continue
				}
				if !ok {
					t.Errorf("deleted %s not found (found %v)", k, keys(found))
					continue
				}
				got := raw[it.Offset : it.Offset+it.Size]
				if !bytes.Equal(got, data) {
					t.Errorf("%s: recovered %d bytes, differs from the original %d", k, it.Size, len(data))
				}
			}
		})
	}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func keys(m map[string]Item) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}
