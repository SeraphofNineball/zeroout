package job

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zeroout/internal/ext4/ext4test"
)

// scanFixture builds an ext4 image containing two deleted text files and a
// deleted PNG, and a manager whose "vol" volume reads from it.
func scanFixture(t *testing.T) (*Manager, string, map[int][]byte) {
	t.Helper()
	root := t.TempDir()
	targets, data, rec := filepath.Join(root, "targets"), filepath.Join(root, "data"), filepath.Join(root, "recover")
	os.MkdirAll(filepath.Join(targets, "vol"), 0o755)
	os.MkdirAll(filepath.Join(rec, "usb"), 0o755)

	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"), bytes.Repeat([]byte{7}, 5000)...)
	png = append(png, "\x00\x00\x00\x00IEND\xAE\x42\x60\x82"...)
	files := map[int][]byte{
		10: []byte(strings.Repeat("secret notes line\n", 400)),
		20: png,
		30: []byte(strings.Repeat("another deleted file\n", 50)),
	}
	img := ext4test.Image{Blocks: 64, Data: files}.Build()
	imgPath := filepath.Join(root, "vol.img")
	os.WriteFile(imgPath, img, 0o600)

	m, err := NewManager(Options{
		TargetsDir: targets, DataDir: data, RecoverDir: rec, AllowUnmounted: true,
		Devices: map[string]string{"vol": imgPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.closeFS)
	return m, imgPath, files
}

func TestScanAndRecover(t *testing.T) {
	m, imgPath, files := scanFixture(t)
	j, err := m.StartScan("vol")
	if err != nil {
		t.Fatal(err)
	}
	s := waitIdle(t, m)
	if s.History[0].State != Completed || s.History[0].Found != 3 {
		t.Fatalf("scan job = %+v", s.History[0])
	}
	sum, err := m.Scan(j.ScanID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.State != "completed" || sum.Items != 3 || sum.NonZeroBytes == 0 || len(sum.Heat) != heatCells {
		t.Fatalf("summary = %+v", sum)
	}

	page, err := m.Items(j.ScanID, ItemQuery{Exts: []string{"txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || page.TotalBytes != int64(len(files[10])+len(files[30])) {
		t.Fatalf("txt page = %+v", page)
	}
	sorted, _ := m.Items(j.ScanID, ItemQuery{Sort: "size"})
	for i := 1; i < len(sorted.Items); i++ {
		if sorted.Items[i].Size > sorted.Items[i-1].Size {
			t.Fatalf("size sort out of order: %+v", sorted.Items)
		}
	}

	// Preview returns the exact bytes.
	c, err := m.OpenItem(j.ScanID, page.Items[0].N)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(c)
	if !bytes.Equal(got, files[10]) {
		t.Fatalf("preview returned %d bytes, want the deleted file", len(got))
	}

	// Simulate the space at block 30 being reused after the scan.
	img, _ := os.ReadFile(imgPath)
	img[2*ext4test.BlockSize+30/8] |= 1 << (30 % 8)
	os.WriteFile(imgPath, img, 0o600)

	if _, err := m.OpenItem(j.ScanID, page.Items[1].N); !errors.Is(err, ErrOverwritten) {
		t.Fatalf("OpenItem on reused space: err = %v", err)
	}

	if _, err := m.StartRecover(RecoverSpec{ScanID: j.ScanID, Destination: "nope"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown destination: %v", err)
	}
	if _, err := m.StartRecover(RecoverSpec{ScanID: j.ScanID, Destination: "usb"}); err != nil {
		t.Fatal(err)
	}
	s = waitIdle(t, m)
	r := s.History[0].Recover
	if s.History[0].State != Completed || r.Written != 2 || r.Overwritten != 1 {
		t.Fatalf("recover job = %+v, spec %+v", s.History[0], r)
	}
	out, err := os.ReadFile(filepath.Join(r.Dir, "Text", "txt", "00000000a000.txt"))
	if err != nil || !bytes.Equal(out, files[10]) {
		t.Fatalf("recovered txt: %v (%d bytes)", err, len(out))
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "manifest.csv")); err != nil {
		t.Fatal(err)
	}

	if err := m.DeleteScan(j.ScanID); err != nil {
		t.Fatal(err)
	}
	if len(m.Scans()) != 0 {
		t.Fatal("scan not deleted")
	}
}
