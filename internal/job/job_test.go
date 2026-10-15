package job

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zeroout/internal/wipe"
)

func newTestManager(t *testing.T, targets, data string) *Manager {
	t.Helper()
	m, err := NewManager(Options{TargetsDir: targets, DataDir: data, AllowUnmounted: true, MaxBytesPerPass: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func setup(t *testing.T) (targets, data string) {
	root := t.TempDir()
	targets, data = filepath.Join(root, "targets"), filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(targets, "vol"), 0o755); err != nil {
		t.Fatal(err)
	}
	return targets, data
}

func waitIdle(t *testing.T, m *Manager) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := m.Status(); s.Current == nil {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Status{}
}

func spec(passes ...wipe.Pattern) Spec {
	return Spec{Volume: "vol", Passes: passes, Mode: wipe.ModeFull, Verify: true}
}

func TestStartCompletes(t *testing.T) {
	targets, data := setup(t)
	m := newTestManager(t, targets, data)
	if _, err := m.Start(spec(wipe.Zero, wipe.Random)); err != nil {
		t.Fatal(err)
	}
	s := waitIdle(t, m)
	if len(s.History) != 1 || s.History[0].State != Completed || s.History[0].PassesDone != 2 {
		t.Fatalf("history = %+v", s.History)
	}
	if s.Resumable != nil {
		t.Fatal("completed job should not be resumable")
	}
}

func TestStartValidates(t *testing.T) {
	targets, data := setup(t)
	m := newTestManager(t, targets, data)
	for name, sp := range map[string]Spec{
		"unknown volume": {Volume: "../etc", Passes: []wipe.Pattern{wipe.Zero}, Mode: wipe.ModeFull},
		"no passes":      {Volume: "vol", Mode: wipe.ModeFull},
		"bad mode":       {Volume: "vol", Passes: []wipe.Pattern{wipe.Zero}, Mode: "nope"},
	} {
		if _, err := m.Start(sp); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestInterruptedJobIsResumableAfterRestart(t *testing.T) {
	targets, data := setup(t)

	// Simulate a crash: state.json says a job was running after pass 1 of 3,
	// and its fill files are still on the volume.
	j := &Job{ID: "x", Spec: spec(wipe.One, wipe.Random, wipe.Zero), State: Running, PassesDone: 1, StartedAt: time.Now()}
	b, _ := json.Marshal(persisted{Current: j})
	os.MkdirAll(data, 0o755)
	os.WriteFile(filepath.Join(data, "state.json"), b, 0o644)
	stale := filepath.Join(targets, "vol", wipe.DirName)
	os.MkdirAll(stale, 0o700)
	os.WriteFile(filepath.Join(stale, "fill-00000.bin"), make([]byte, 1024), 0o600)

	m := newTestManager(t, targets, data)
	s := m.Status()
	if s.Resumable == nil || s.Resumable.State != Interrupted || s.Resumable.PassesDone != 1 {
		t.Fatalf("resumable = %+v", s.Resumable)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale fill files were not removed on startup")
	}

	r, err := m.Resume()
	if err != nil {
		t.Fatal(err)
	}
	if r.FirstPass != 1 {
		t.Fatalf("resumed at pass index %d, want 1", r.FirstPass)
	}
	s = waitIdle(t, m)
	if s.History[0].State != Completed || s.History[0].PassesDone != 3 {
		t.Fatalf("resumed job = %+v", s.History[0])
	}
}

func TestCancelMakesJobResumable(t *testing.T) {
	targets, data := setup(t)
	m, err := NewManager(Options{TargetsDir: targets, DataDir: data, AllowUnmounted: true, MaxBytesPerPass: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(spec(wipe.Zero, wipe.Zero)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(spec(wipe.Zero)); !errors.Is(err, ErrBusy) {
		t.Fatalf("second start err = %v, want ErrBusy", err)
	}
	if err := m.Cancel(); err != nil {
		t.Fatal(err)
	}
	s := waitIdle(t, m)
	if s.History[0].State != Cancelled || s.Resumable == nil {
		t.Fatalf("after cancel: history[0]=%+v resumable=%v", s.History[0], s.Resumable)
	}
	if _, err := os.Stat(filepath.Join(targets, "vol", wipe.DirName)); !os.IsNotExist(err) {
		t.Fatal("fill files left after cancel")
	}

	m.DiscardResumable()
	m2 := newTestManager(t, targets, data) // reload from disk
	if m2.Status().Resumable != nil || len(m2.Status().History) != 1 {
		t.Fatalf("reloaded state = %+v", m2.Status())
	}
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
