package ext4

import (
	"bytes"
	"context"
	"testing"

	"zeroout/internal/ext4/ext4test"
)

func TestFreeExtents(t *testing.T) {
	img := ext4test.Image{Blocks: 100, Used: []int{10, 11, 12, 50, 99}}.Build()
	fs, err := Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	if fs.BlockSize != 4096 || fs.Blocks != 100 || fs.Groups != 1 {
		t.Fatalf("parsed %+v", fs)
	}
	var got []Extent
	if err := fs.FreeExtents(context.Background(), func(e Extent) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	want := []Extent{{3, 7}, {13, 37}, {51, 48}}
	if len(got) != len(want) {
		t.Fatalf("extents = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("extents = %v, want %v", got, want)
		}
	}
	free, uninit := fs.FreeCounts()
	if free != 92 || uninit != 0 {
		t.Fatalf("FreeCounts = %d, %d", free, uninit)
	}
	for _, c := range []struct {
		start, n uint64
		want     bool
	}{{3, 7, true}, {3, 8, false}, {13, 37, true}, {49, 2, false}, {99, 1, false}, {98, 5, false}} {
		if ok, err := fs.IsFree(c.start, c.n); err != nil || ok != c.want {
			t.Errorf("IsFree(%d,%d) = %v, %v; want %v", c.start, c.n, ok, err, c.want)
		}
	}
}

func TestOpenRejectsGarbage(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 8192))); err == nil {
		t.Fatal("opened an all-zero image")
	}
	if Probe(bytes.NewReader(make([]byte, 8192)), 0) {
		t.Fatal("Probe matched zeros")
	}
}

func TestIsPow(t *testing.T) {
	for n, want := range map[uint64]bool{1: true, 3: true, 9: true, 27: true, 6: false, 25: true} {
		if got := isPow(n, 3) || isPow(n, 5); got != want {
			t.Errorf("isPow(%d) = %v", n, got)
		}
	}
}
