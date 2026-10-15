// Package job runs one wipe at a time and persists its state so an
// interrupted multi-pass job can be resumed after a restart.
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"zeroout/internal/volume"
	"zeroout/internal/wipe"
)

type State string

const (
	Running     State = "running"
	Completed   State = "completed"
	Cancelled   State = "cancelled"
	Failed      State = "failed"
	Interrupted State = "interrupted" // container stopped mid-job
)

// Spec is what the user asks for.
type Spec struct {
	Volume   string         `json:"volume"`
	Passes   []wipe.Pattern `json:"passes"`
	Mode     wipe.Mode      `json:"mode"`
	MarginGB float64        `json:"marginGB"`
	Verify   bool           `json:"verify"`
}

type LogLine struct {
	Time time.Time `json:"time"`
	Msg  string    `json:"msg"`
}

type Job struct {
	ID         string         `json:"id"`
	Spec       Spec           `json:"spec"`
	State      State          `json:"state"`
	FirstPass  int            `json:"firstPass"`  // 0-based; non-zero when resumed
	PassesDone int            `json:"passesDone"` // total completed, including before a resume
	StartedAt  time.Time      `json:"startedAt"`
	FinishedAt *time.Time     `json:"finishedAt,omitempty"`
	Error      string         `json:"error,omitempty"`
	Progress   *wipe.Progress `json:"progress,omitempty"`
	Log        []LogLine      `json:"log"`
}

func (j *Job) clone() *Job {
	if j == nil {
		return nil
	}
	c := *j
	c.Spec.Passes = slices.Clone(j.Spec.Passes)
	c.Log = slices.Clone(j.Log)
	if j.Progress != nil {
		p := *j.Progress
		c.Progress = &p
	}
	return &c
}

func (j *Job) logf(format string, args ...any) {
	j.Log = append(j.Log, LogLine{time.Now(), fmt.Sprintf(format, args...)})
	if n := len(j.Log); n > maxLogLines {
		j.Log = slices.Delete(j.Log, 0, n-maxLogLines)
	}
}

const (
	maxLogLines = 200
	maxHistory  = 25
	saveEvery   = time.Minute
)

// ErrInvalid wraps errors caused by a bad request.
var ErrInvalid = errors.New("invalid request")

// ErrBusy is returned when a job is already running.
var ErrBusy = errors.New("a wipe is already running")

var (
	errUserCancel = errors.New("cancelled by user")
	errShutdown   = errors.New("shutting down")
)

type Options struct {
	TargetsDir     string
	DataDir        string
	AllowUnmounted bool
	// MaxBytesPerPass caps each pass (0 = no cap); for tests and trial runs.
	MaxBytesPerPass int64
	Logf            func(format string, args ...any)
}

type Manager struct {
	opts      Options
	statePath string
	saveMu    sync.Mutex // serializes writes of statePath

	mu        sync.Mutex
	current   *Job
	resumable *Job
	history   []*Job
	cancel    context.CancelCauseFunc
	done      chan struct{}
	lastSave  time.Time
}

type persisted struct {
	Current   *Job   `json:"current"`
	Resumable *Job   `json:"resumable"`
	History   []*Job `json:"history"`
}

// NewManager loads saved state, marks a job that was running when the process
// stopped as interrupted, and removes fill files it may have left behind.
func NewManager(opts Options) (*Manager, error) {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{opts: opts, statePath: filepath.Join(opts.DataDir, "state.json")}
	if b, err := os.ReadFile(m.statePath); err == nil {
		var p persisted
		if err := json.Unmarshal(b, &p); err != nil {
			opts.Logf("ignoring unreadable %s: %v", m.statePath, err)
		} else {
			m.resumable, m.history = p.Resumable, p.History
			if j := p.Current; j != nil {
				now := time.Now()
				j.State, j.FinishedAt, j.Progress = Interrupted, &now, nil
				j.Error = "ZeroOut stopped while this job was running"
				j.logf("Interrupted: container or NAS restarted")
				m.finish(j)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if vols, err := volume.List(opts.TargetsDir); err == nil {
		for _, v := range vols {
			stale := filepath.Join(v.Path, wipe.DirName)
			if _, err := os.Stat(stale); err == nil {
				opts.Logf("removing leftover fill files in %s", stale)
				if err := os.RemoveAll(stale); err != nil {
					opts.Logf("could not remove %s: %v", stale, err)
				}
			}
		}
	} else {
		opts.Logf("cannot read targets dir %s: %v", opts.TargetsDir, err)
	}
	m.save()
	return m, nil
}

// Volumes lists the wipe targets.
func (m *Manager) Volumes() ([]volume.Info, error) {
	vols, err := volume.List(m.opts.TargetsDir)
	if vols == nil {
		vols = []volume.Info{}
	}
	return vols, err
}

type Status struct {
	Current   *Job   `json:"current"`
	Resumable *Job   `json:"resumable"`
	History   []*Job `json:"history"`
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Current: m.current.clone(), Resumable: m.resumable.clone(), History: make([]*Job, len(m.history))}
	for i, j := range m.history {
		s.History[i] = j.clone()
		s.History[i].Log = nil // keep the poll response small
	}
	return s
}

// Start launches a new job.
func (m *Manager) Start(spec Spec) (*Job, error) {
	return m.start(spec, 0, 0)
}

// Resume restarts the resumable job from its first unfinished pass.
func (m *Manager) Resume() (*Job, error) {
	m.mu.Lock()
	r := m.resumable
	m.mu.Unlock()
	if r == nil {
		return nil, fmt.Errorf("%w: nothing to resume", ErrInvalid)
	}
	return m.start(r.Spec, r.PassesDone, r.PassesDone)
}

// DiscardResumable forgets the resumable job.
func (m *Manager) DiscardResumable() {
	m.mu.Lock()
	m.resumable = nil
	m.mu.Unlock()
	m.save()
}

func (m *Manager) start(spec Spec, firstPass, passesDone int) (*Job, error) {
	cfg, err := m.config(spec)
	if err != nil {
		return nil, err
	}
	cfg.FirstPass = firstPass

	m.mu.Lock()
	if m.current != nil {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	now := time.Now()
	j := &Job{
		ID:         now.UTC().Format("20060102T150405Z"),
		Spec:       spec,
		State:      Running,
		FirstPass:  firstPass,
		PassesDone: passesDone,
		StartedAt:  now,
	}
	if firstPass > 0 {
		j.logf("Resuming at pass %d of %d", firstPass+1, len(spec.Passes))
	}
	j.logf("Target %s, %d pass(es): %v, mode %s", cfg.Target, len(spec.Passes), spec.Passes, spec.Mode)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	m.current, m.resumable, m.cancel, m.done = j, nil, cancel, done
	snapshot := j.clone()
	m.mu.Unlock()

	m.opts.Logf("job %s started on %s", j.ID, cfg.Target)
	m.save()
	go m.run(ctx, j, cfg, done)
	return snapshot, nil
}

func (m *Manager) config(spec Spec) (wipe.Config, error) {
	bad := func(format string, args ...any) (wipe.Config, error) {
		return wipe.Config{}, fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
	}
	if n := len(spec.Passes); n < 1 || n > wipe.MaxPasses {
		return bad("passes must be between 1 and %d", wipe.MaxPasses)
	}
	if spec.Mode != wipe.ModeMargin && spec.Mode != wipe.ModeFull {
		return bad("mode must be %q or %q", wipe.ModeMargin, wipe.ModeFull)
	}
	if spec.MarginGB < 0 {
		return bad("margin cannot be negative")
	}
	vols, err := volume.List(m.opts.TargetsDir)
	if err != nil {
		return wipe.Config{}, err
	}
	i := slices.IndexFunc(vols, func(v volume.Info) bool { return v.Name == spec.Volume })
	if i < 0 {
		return bad("unknown volume %q", spec.Volume)
	}
	v := vols[i]
	if v.Error != "" {
		return bad("volume %s: %s", v.Name, v.Error)
	}
	if !v.IsMount && !m.opts.AllowUnmounted {
		return bad("%s is not a mount point; wiping it would fill the container's own disk", v.Path)
	}
	return wipe.Config{
		Target:      v.Path,
		Passes:      spec.Passes,
		Mode:        spec.Mode,
		MarginBytes: int64(spec.MarginGB * 1e9),
		Verify:      spec.Verify,
		MaxBytes:    m.opts.MaxBytesPerPass,
	}, nil
}

func (m *Manager) run(ctx context.Context, j *Job, cfg wipe.Config, done chan struct{}) {
	defer close(done)
	err := wipe.Run(ctx, cfg, wipe.Hooks{
		Progress: func(p wipe.Progress) {
			m.mu.Lock()
			j.Progress = &p
			due := time.Since(m.lastSave) > saveEvery
			m.mu.Unlock()
			if due {
				m.save()
			}
		},
		Log: func(s string) {
			m.mu.Lock()
			j.logf("%s", s)
			m.mu.Unlock()
			m.opts.Logf("job %s: %s", j.ID, s)
		},
		PassDone: func(i int) {
			m.mu.Lock()
			j.PassesDone = i + 1
			j.logf("Pass %d of %d complete", i+1, len(j.Spec.Passes))
			m.mu.Unlock()
			m.save()
		},
	})

	m.mu.Lock()
	now := time.Now()
	j.FinishedAt, j.Progress = &now, nil
	switch cause := context.Cause(ctx); {
	case err == nil:
		j.State = Completed
		j.logf("Completed %d pass(es) in %s", len(j.Spec.Passes)-j.FirstPass, now.Sub(j.StartedAt).Round(time.Second))
	case errors.Is(cause, errShutdown):
		j.State = Interrupted
		j.logf("Interrupted by shutdown; fill files removed")
	case errors.Is(cause, errUserCancel):
		j.State = Cancelled
		j.logf("Cancelled; fill files removed")
	default:
		j.State, j.Error = Failed, err.Error()
		j.logf("Failed: %v", err)
	}
	m.current, m.cancel = nil, nil
	m.finish(j)
	m.mu.Unlock()
	m.opts.Logf("job %s %s", j.ID, j.State)
	m.save()
}

// finish moves j into history and makes it resumable if passes remain.
// Caller holds m.mu (or is single-threaded during startup).
func (m *Manager) finish(j *Job) {
	m.history = append([]*Job{j}, m.history...)
	if len(m.history) > maxHistory {
		m.history = m.history[:maxHistory]
	}
	if j.State != Completed && j.PassesDone < len(j.Spec.Passes) {
		m.resumable = j
	}
}

// Cancel stops the running job. Fill files are removed before the job ends.
func (m *Manager) Cancel() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel == nil {
		return fmt.Errorf("%w: no wipe is running", ErrInvalid)
	}
	m.cancel(errUserCancel)
	return nil
}

// Shutdown interrupts a running job and waits for its cleanup to finish.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel(errShutdown)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) save() {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.Lock()
	m.lastSave = time.Now()
	b, err := json.MarshalIndent(persisted{Current: m.current, Resumable: m.resumable, History: m.history}, "", "  ")
	m.mu.Unlock()
	if err != nil {
		m.opts.Logf("saving state: %v", err)
		return
	}
	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		err = os.Rename(tmp, m.statePath)
	}
	if err != nil {
		m.opts.Logf("saving state: %v", err)
	}
}
