// Package job runs one job at a time — a wipe, a scan for deleted files, or a
// recovery — and persists state so an interrupted multi-pass wipe can be
// resumed after a restart.
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

	"zeroout/internal/blockdev"
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

type Kind string

const (
	KindWipe    Kind = "wipe"
	KindScan    Kind = "scan"
	KindRecover Kind = "recover"
)

// Spec is a wipe request.
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
	ID     string `json:"id"`
	Kind   Kind   `json:"kind"`
	Volume string `json:"volume"`
	State  State  `json:"state"`

	Spec       *Spec `json:"spec,omitempty"` // wipe
	FirstPass  int   `json:"firstPass"`      // wipe: 0-based; non-zero when resumed
	PassesDone int   `json:"passesDone"`     // wipe: total completed, including before a resume

	ScanID  string       `json:"scanId,omitempty"`  // scan: results id; recover: source scan
	Recover *RecoverSpec `json:"recover,omitempty"` // recover
	Found   int64        `json:"found,omitempty"`   // scan: files found; recover: files written

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
	if j.Spec != nil {
		s := *j.Spec
		s.Passes = slices.Clone(s.Passes)
		c.Spec = &s
	}
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
var ErrBusy = errors.New("another job is already running")

// ErrNotFound is returned for unknown scans or items.
var ErrNotFound = errors.New("not found")

var (
	errUserCancel = errors.New("cancelled by user")
	errShutdown   = errors.New("shutting down")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

type Options struct {
	TargetsDir string
	RecoverDir string
	DataDir    string
	// Devices maps volume names to device/image paths, overriding detection.
	Devices map[string]string
	// AllowUnmounted relaxes the mount-point, size and same-disk safety
	// checks so plain directories and image files work. Development only.
	AllowUnmounted bool
	// MaxBytesPerPass caps each wipe pass (0 = no cap); for tests and trial runs.
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

	scans  scanCache
	fsMu   sync.Mutex
	fsOpen map[string]fsHandle
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
			for _, j := range append([]*Job{p.Current, p.Resumable}, p.History...) {
				upgrade(j)
			}
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
	m.markInterruptedScans()

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

// upgrade fills in fields missing from state saved by older versions.
func upgrade(j *Job) {
	if j == nil {
		return
	}
	if j.Kind == "" {
		j.Kind = KindWipe
	}
	if j.Volume == "" && j.Spec != nil {
		j.Volume = j.Spec.Volume
	}
}

// Volumes lists the wipe targets.
func (m *Manager) Volumes() ([]volume.Info, error) {
	vols, err := volume.List(m.opts.TargetsDir)
	if vols == nil {
		vols = []volume.Info{}
	}
	return vols, err
}

func (m *Manager) volume(name string) (volume.Info, error) {
	vols, err := volume.List(m.opts.TargetsDir)
	if err != nil {
		return volume.Info{}, err
	}
	i := slices.IndexFunc(vols, func(v volume.Info) bool { return v.Name == name })
	if i < 0 {
		return volume.Info{}, invalid("unknown volume %q", name)
	}
	v := vols[i]
	if v.Error != "" {
		return v, invalid("volume %s: %s", v.Name, v.Error)
	}
	if !v.IsMount && !m.opts.AllowUnmounted {
		return v, invalid("%s is not a mount point", v.Path)
	}
	return v, nil
}

// Device resolves the raw device used to scan a volume.
func (m *Manager) Device(name string) (*blockdev.Device, error) {
	v, err := m.volume(name)
	if err != nil {
		return nil, err
	}
	return blockdev.Resolve(v.Path, m.opts.Devices[name])
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

// ---------- wipe ----------

// Start launches a new wipe.
func (m *Manager) Start(spec Spec) (*Job, error) {
	return m.startWipe(spec, 0, 0)
}

// Resume restarts the resumable wipe from its first unfinished pass.
func (m *Manager) Resume() (*Job, error) {
	m.mu.Lock()
	r := m.resumable
	m.mu.Unlock()
	if r == nil || r.Spec == nil {
		return nil, invalid("nothing to resume")
	}
	return m.startWipe(*r.Spec, r.PassesDone, r.PassesDone)
}

// DiscardResumable forgets the resumable wipe.
func (m *Manager) DiscardResumable() {
	m.mu.Lock()
	m.resumable = nil
	m.mu.Unlock()
	m.save()
}

func (m *Manager) startWipe(spec Spec, firstPass, passesDone int) (*Job, error) {
	if n := len(spec.Passes); n < 1 || n > wipe.MaxPasses {
		return nil, invalid("passes must be between 1 and %d", wipe.MaxPasses)
	}
	if spec.Mode != wipe.ModeMargin && spec.Mode != wipe.ModeFull {
		return nil, invalid("mode must be %q or %q", wipe.ModeMargin, wipe.ModeFull)
	}
	if spec.MarginGB < 0 {
		return nil, invalid("margin cannot be negative")
	}
	v, err := m.volume(spec.Volume)
	if err != nil {
		return nil, err
	}
	cfg := wipe.Config{
		Target:      v.Path,
		Passes:      spec.Passes,
		Mode:        spec.Mode,
		MarginBytes: int64(spec.MarginGB * 1e9),
		Verify:      spec.Verify,
		MaxBytes:    m.opts.MaxBytesPerPass,
		FirstPass:   firstPass,
	}
	j := &Job{Kind: KindWipe, Volume: v.Name, Spec: &spec, FirstPass: firstPass, PassesDone: passesDone}
	if firstPass > 0 {
		j.logf("Resuming at pass %d of %d", firstPass+1, len(spec.Passes))
	}
	j.logf("Target %s, %d pass(es): %v, mode %s", cfg.Target, len(spec.Passes), spec.Passes, spec.Mode)
	return m.launch(j, func(ctx context.Context) error {
		return wipe.Run(ctx, cfg, wipe.Hooks{
			Progress: func(p wipe.Progress) { m.setProgress(j, p) },
			Log:      func(s string) { m.logf(j, "%s", s) },
			PassDone: func(i int) {
				m.mu.Lock()
				j.PassesDone = i + 1
				j.logf("Pass %d of %d complete", i+1, len(spec.Passes))
				m.mu.Unlock()
				m.save()
			},
		})
	})
}

// ---------- generic job runner ----------

// launch starts j in the background if no other job is running.
func (m *Manager) launch(j *Job, run func(ctx context.Context) error) (*Job, error) {
	m.mu.Lock()
	if m.current != nil {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	now := time.Now()
	if j.ID == "" {
		j.ID = now.UTC().Format("20060102T150405Z")
	}
	j.State, j.StartedAt = Running, now
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	m.current, m.cancel, m.done = j, cancel, done
	if j.Kind == KindWipe {
		m.resumable = nil
	}
	snapshot := j.clone()
	m.mu.Unlock()

	m.opts.Logf("%s job %s started on %s", j.Kind, j.ID, j.Volume)
	m.save()
	go func() {
		defer close(done)
		err := run(ctx)
		m.mu.Lock()
		now := time.Now()
		j.FinishedAt, j.Progress = &now, nil
		switch cause := context.Cause(ctx); {
		case err == nil:
			j.State = Completed
			j.logf("Completed in %s", now.Sub(j.StartedAt).Round(time.Second))
		case errors.Is(cause, errShutdown):
			j.State = Interrupted
			j.logf("Interrupted by shutdown")
		case errors.Is(cause, errUserCancel):
			j.State = Cancelled
			j.logf("Cancelled")
		default:
			j.State, j.Error = Failed, err.Error()
			j.logf("Failed: %v", err)
		}
		m.current, m.cancel = nil, nil
		m.finish(j)
		m.mu.Unlock()
		m.opts.Logf("%s job %s %s", j.Kind, j.ID, j.State)
		m.save()
	}()
	return snapshot, nil
}

func (m *Manager) setProgress(j *Job, p wipe.Progress) {
	m.mu.Lock()
	j.Progress = &p
	due := time.Since(m.lastSave) > saveEvery
	m.mu.Unlock()
	if due {
		m.save()
	}
}

func (m *Manager) logf(j *Job, format string, args ...any) {
	m.mu.Lock()
	j.logf(format, args...)
	m.mu.Unlock()
	m.opts.Logf("%s job %s: %s", j.Kind, j.ID, fmt.Sprintf(format, args...))
}

// finish moves j into history and makes an unfinished wipe resumable.
// Caller holds m.mu (or is single-threaded during startup).
func (m *Manager) finish(j *Job) {
	m.history = append([]*Job{j}, m.history...)
	if len(m.history) > maxHistory {
		m.history = m.history[:maxHistory]
	}
	if j.Kind == KindWipe && j.Spec != nil && j.State != Completed && j.PassesDone < len(j.Spec.Passes) {
		m.resumable = j
	}
}

// Cancel stops the running job. A wipe's fill files are removed first.
func (m *Manager) Cancel() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel == nil {
		return invalid("no job is running")
	}
	m.cancel(errUserCancel)
	return nil
}

// Shutdown interrupts a running job, waits for its cleanup to finish and
// releases open devices.
func (m *Manager) Shutdown(ctx context.Context) error {
	defer m.closeFS()
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
	if err := writeFileAtomic(m.statePath, b); err != nil {
		m.opts.Logf("saving state: %v", err)
	}
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
