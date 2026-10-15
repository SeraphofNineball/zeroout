package wipe

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePattern(t *testing.T) {
	for in, want := range map[string]string{"zero": "zero", "ZEROS": "zero", "0x00": "zero", "ones": "one", "random": "random", "0xaa": "0xAA"} {
		p, err := ParsePattern(in)
		if err != nil || p.String() != want {
			t.Errorf("ParsePattern(%q) = %v, %v; want %s", in, p, err, want)
		}
	}
	for _, in := range []string{"", "0x", "0x1", "0xZZ", "dod"} {
		if _, err := ParsePattern(in); err == nil {
			t.Errorf("ParsePattern(%q) succeeded", in)
		}
	}
}

// Random data generated in one go must match data generated piecewise at
// arbitrary offsets, or verification of partial writes would fail.
func TestRandomFillAtIsOffsetStable(t *testing.T) {
	var seed [32]byte
	seed[0] = 7
	whole := make([]byte, 3*randomUnit+123)
	newSource(Random, seed, 2).fillAt(whole, 0)

	src := newSource(Random, seed, 2)
	pieces := make([]byte, len(whole))
	for off, step := 0, 4096+17; off < len(pieces); off += step {
		end := min(off+step, len(pieces))
		src.fillAt(pieces[off:end], int64(off))
	}
	if !bytes.Equal(whole, pieces) {
		t.Fatal("piecewise random stream differs from whole stream")
	}

	other := make([]byte, randomUnit)
	newSource(Random, seed, 3).fillAt(other, 0)
	if bytes.Equal(whole[:randomUnit], other) {
		t.Fatal("different chunks produced identical data")
	}
}

func TestRunWritesVerifiesAndCleansUp(t *testing.T) {
	target := t.TempDir()
	var logs []string
	var done []int
	var sawVerify bool
	cfg := Config{
		Target:     target,
		Passes:     []Pattern{One, Random, Zero},
		Mode:       ModeFull,
		Verify:     true,
		BlockBytes: 1 << 20,
		ChunkBytes: 3 << 20,
		MaxBytes:   10<<20 + 512, // not chunk- or block-aligned on purpose
	}
	err := Run(context.Background(), cfg, Hooks{
		Log:      func(s string) { logs = append(logs, s) },
		PassDone: func(i int) { done = append(done, i) },
		Progress: func(p Progress) {
			if p.Phase == PhaseVerifying {
				sawVerify = true
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 3 || done[2] != 2 {
		t.Fatalf("PassDone calls = %v", done)
	}
	if !sawVerify {
		t.Fatal("no verify phase reported")
	}
	if _, err := os.Stat(filepath.Join(target, DirName)); !os.IsNotExist(err) {
		t.Fatalf("fill directory left behind: %v", err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "verified") {
		t.Fatalf("logs missing verification: %v", logs)
	}
}

func TestRunResumesFromFirstPass(t *testing.T) {
	var done []int
	cfg := Config{Target: t.TempDir(), Passes: []Pattern{Zero, Zero, Zero}, Mode: ModeFull, FirstPass: 2, MaxBytes: 1 << 20}
	if err := Run(context.Background(), cfg, Hooks{PassDone: func(i int) { done = append(done, i) }}); err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0] != 2 {
		t.Fatalf("PassDone calls = %v, want [2]", done)
	}
}

func TestRunCancelCleansUp(t *testing.T) {
	target := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := Config{Target: target, Passes: []Pattern{Zero}, Mode: ModeFull, BlockBytes: 64 << 10, MaxBytes: 64 << 20}
	err := Run(ctx, cfg, Hooks{Progress: func(p Progress) {
		cancel() // first event is the start of the writing phase
	}})
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(target, DirName)); !os.IsNotExist(err) {
		t.Fatalf("fill directory left behind after cancel: %v", err)
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	for name, cfg := range map[string]Config{
		"no passes":   {Target: dir, Mode: ModeFull},
		"too many":    {Target: dir, Mode: ModeFull, Passes: make([]Pattern, MaxPasses+1)},
		"bad mode":    {Target: dir, Mode: "x", Passes: []Pattern{Zero}},
		"missing dir": {Target: filepath.Join(dir, "nope"), Mode: ModeFull, Passes: []Pattern{Zero}},
		"huge margin": {Target: dir, Mode: ModeMargin, MarginBytes: 1 << 62, Passes: []Pattern{Zero}},
	} {
		if err := Run(context.Background(), cfg, Hooks{}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
