package blockdev

import (
	"bytes"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"zeroout/internal/ext4/ext4test"
)

func TestReaderUnalignedReads(t *testing.T) {
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i * 7)
	}
	path := filepath.Join(t.TempDir(), "dev.img")
	os.WriteFile(path, data, 0o600)
	f, _ := os.Open(path)
	defer f.Close()
	// direct=true forces the bounce-buffer path even on a normal file.
	r := &Reader{f: f, base: 8192, direct: true}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200; i++ {
		off := rng.Int64N(int64(len(data) - 8192))
		n := rng.IntN(10000) + 1
		p := make([]byte, n)
		got, err := r.ReadAt(p, off)
		want := data[8192+off : min(int64(len(data)), 8192+off+int64(n))]
		if got != len(want) || !bytes.Equal(p[:got], want) {
			t.Fatalf("ReadAt(%d, %d) = %d, %v", off, n, got, err)
		}
		if got < n && err != io.EOF {
			t.Fatalf("short read without EOF: %v", err)
		}
	}
}

func TestOverrideFindsOffsetFilesystem(t *testing.T) {
	img := ext4test.Image{Blocks: 16}.Build()
	path := filepath.Join(t.TempDir(), "bcache.img")
	os.WriteFile(path, append(make([]byte, 8192), img...), 0o600) // like bcache's 8 KiB header
	d, err := Resolve("/unused", path)
	if err != nil {
		t.Fatal(err)
	}
	if d.Offset != 8192 {
		t.Fatalf("offset = %d, want 8192", d.Offset)
	}
	if got := ParseOverrides(" volume1=/dev/a, bad ,v2=/x "); got["volume1"] != "/dev/a" || got["v2"] != "/x" || len(got) != 2 {
		t.Fatalf("ParseOverrides = %v", got)
	}
}
