package job

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"zeroout/internal/blockdev"
	"zeroout/internal/carve"
	"zeroout/internal/ext4"
	"zeroout/internal/wipe"
)

const (
	maxScans  = 10
	heatCells = 240
	itemBytes = 20 // on-disk record size
)

// KindCount summarises the files of one type found by a scan.
type KindCount struct {
	Ext       string `json:"ext"`
	Category  string `json:"category"`
	Desc      string `json:"desc"`
	Count     int64  `json:"count"`
	Bytes     int64  `json:"bytes"`
	Exact     int64  `json:"exact"`
	Truncated int64  `json:"truncated"`
}

// ScanSummary is stored as summary.json next to the scan's items.
type ScanSummary struct {
	ID         string     `json:"id"`
	Volume     string     `json:"volume"`
	Device     string     `json:"device"`
	Via        string     `json:"via"`
	State      string     `json:"state"` // running, completed, partial, failed
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`

	BlockSize       int64 `json:"blockSize"`
	FSBytes         int64 `json:"fsBytes"`
	FreeBytes       int64 `json:"freeBytes"`      // per group descriptors
	NeverUsedBytes  int64 `json:"neverUsedBytes"` // free space in never-initialised groups (not read)
	ScannedBytes    int64 `json:"scannedBytes"`
	ZeroBytes       int64 `json:"zeroBytes"`
	NonZeroBytes    int64 `json:"nonZeroBytes"`
	IdentifiedBytes int64 `json:"identifiedBytes"`
	Items           int64 `json:"items"`
	ItemBytes       int64 `json:"itemBytes"`

	KindTable []string         `json:"kindTable"` // ext for each Item.Kind index
	Kinds     []KindCount      `json:"kinds"`
	Heat      []carve.HeatCell `json:"heat"`
}

func (m *Manager) scansDir() string { return filepath.Join(m.opts.DataDir, "scans") }

// StartScan scans a volume's free space for deleted files.
func (m *Manager) StartScan(volumeName string) (*Job, error) {
	v, err := m.volume(volumeName)
	if err != nil {
		return nil, err
	}
	dev, err := blockdev.Resolve(v.Path, m.opts.Devices[v.Name])
	if err != nil {
		return nil, invalid("%v", err)
	}
	id := time.Now().UTC().Format("20060102T150405Z")
	j := &Job{ID: id, Kind: KindScan, Volume: v.Name, ScanID: id}
	j.logf("Reading %s (%s)", dev.Path, dev.Via)
	return m.launch(j, func(ctx context.Context) error {
		return m.runScan(ctx, j, v.Total, dev)
	})
}

func (m *Manager) runScan(ctx context.Context, j *Job, volTotal int64, dev *blockdev.Device) (err error) {
	blockdev.Sync() // so on-disk bitmaps include recent deletions and allocations
	r, err := blockdev.Open(dev)
	if err != nil {
		return err
	}
	defer r.Close()
	fs, err := ext4.Open(r)
	if err != nil {
		return err
	}
	// statfs reports size minus metadata overhead, so allow some slack.
	if !m.opts.AllowUnmounted && volTotal > 0 && (fs.Size() < volTotal || fs.Size() > volTotal+volTotal/10+1<<30) {
		return fmt.Errorf("the filesystem on %s (%s) doesn't match the mounted volume (%s)",
			dev.Path, fmtBytes(fs.Size()), fmtBytes(volTotal))
	}
	free, uninit := fs.FreeCounts()

	sum := &ScanSummary{
		ID: j.ScanID, Volume: j.Volume, Device: dev.Path, Via: dev.Via, State: "running", StartedAt: j.StartedAt,
		BlockSize: fs.BlockSize, FSBytes: fs.Size(),
		FreeBytes: int64(free) * fs.BlockSize, NeverUsedBytes: int64(uninit) * fs.BlockSize,
	}
	for _, k := range carve.Kinds {
		sum.KindTable = append(sum.KindTable, k.Ext)
	}
	st, err := createScanStore(m.scansDir(), sum)
	if err != nil {
		return err
	}
	m.pruneScans()
	m.logf(j, "Filesystem %s, %s free (%s never used, skipped)", fmtBytes(fs.Size()), fmtBytes(sum.FreeBytes), fmtBytes(sum.NeverUsedBytes))

	total := sum.FreeBytes - sum.NeverUsedBytes
	meter := newMeter(total)
	sc := &carve.Scanner{
		R: r, BlockSize: fs.BlockSize, TotalBlocks: fs.Blocks, HeatCells: heatCells,
		OnItem: func(it carve.Item) error {
			if err := st.add(it); err != nil {
				return err
			}
			m.mu.Lock()
			j.Found++
			m.mu.Unlock()
			return nil
		},
		OnProgress: func(done int64) {
			if p, ok := meter.update(done); ok {
				p.Phase = "scanning"
				m.setProgress(j, p)
			}
		},
	}
	m.setProgress(j, wipe.Progress{Phase: "scanning", Total: total})
	scanErr := fs.FreeExtents(ctx, func(e ext4.Extent) error {
		return sc.Scan(ctx, carve.Extent{Start: e.Start, Count: e.Count})
	})

	now := time.Now()
	sum.FinishedAt = &now
	sum.ScannedBytes = sc.Stats.ScannedBytes
	sum.ZeroBytes = sc.Stats.ZeroBytes
	sum.NonZeroBytes = sc.Stats.ScannedBytes - sc.Stats.ZeroBytes
	sum.IdentifiedBytes = sc.Stats.IdentifiedBytes
	sum.Heat = sc.Stats.Heat
	switch {
	case scanErr == nil:
		sum.State = "completed"
	case errors.Is(scanErr, context.Canceled):
		sum.State = "partial"
	default:
		sum.State, sum.Error = "failed", scanErr.Error()
	}
	if err := st.close(sum); err != nil && scanErr == nil {
		scanErr = err
	}
	m.scans.forget(sum.ID)
	m.logf(j, "Scanned %s: %s still holds data, %d files found (%s)",
		fmtBytes(sum.ScannedBytes), fmtBytes(sum.NonZeroBytes), sum.Items, fmtBytes(sum.ItemBytes))
	return scanErr
}

// ---------- on-disk store ----------

type scanStore struct {
	dir   string
	f     *os.File
	w     *bufio.Writer
	sum   *ScanSummary
	kinds map[uint16]*KindCount
	rec   [itemBytes]byte
}

func createScanStore(root string, sum *ScanSummary) (*scanStore, error) {
	dir := filepath.Join(root, sum.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, "items.bin"))
	if err != nil {
		return nil, err
	}
	s := &scanStore{dir: dir, f: f, w: bufio.NewWriterSize(f, 1<<20), sum: sum, kinds: map[uint16]*KindCount{}}
	return s, s.writeSummary()
}

func (s *scanStore) add(it carve.Item) error {
	binary.LittleEndian.PutUint64(s.rec[0:], uint64(it.Offset))
	binary.LittleEndian.PutUint64(s.rec[8:], uint64(it.Size))
	binary.LittleEndian.PutUint16(s.rec[16:], it.Kind)
	s.rec[18] = it.Flags
	if _, err := s.w.Write(s.rec[:]); err != nil {
		return err
	}
	k := s.kinds[it.Kind]
	if k == nil {
		kd := carve.Kinds[it.Kind]
		k = &KindCount{Ext: kd.Ext, Category: kd.Category, Desc: kd.Desc}
		s.kinds[it.Kind] = k
	}
	k.Count++
	k.Bytes += it.Size
	if it.Flags&carve.FlagExact != 0 {
		k.Exact++
	}
	if it.Flags&carve.FlagTruncated != 0 {
		k.Truncated++
	}
	s.sum.Items++
	s.sum.ItemBytes += it.Size
	return nil
}

func (s *scanStore) close(sum *ScanSummary) error {
	err := s.w.Flush()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	sum.Kinds = sum.Kinds[:0]
	for _, k := range s.kinds {
		sum.Kinds = append(sum.Kinds, *k)
	}
	sort.Slice(sum.Kinds, func(i, j int) bool { return sum.Kinds[i].Bytes > sum.Kinds[j].Bytes })
	if werr := s.writeSummary(); err == nil {
		err = werr
	}
	return err
}

func (s *scanStore) writeSummary() error {
	b, err := json.MarshalIndent(s.sum, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, "summary.json"), b)
}

func validScanID(id string) bool {
	return id != "" && !strings.ContainsAny(id, `/\.`)
}

func (m *Manager) readSummary(id string) (*ScanSummary, error) {
	if !validScanID(id) {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(m.scansDir(), id, "summary.json"))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var s ScanSummary
	return &s, json.Unmarshal(b, &s)
}

// Scans lists stored scans, newest first.
func (m *Manager) Scans() []*ScanSummary {
	entries, _ := os.ReadDir(m.scansDir())
	out := []*ScanSummary{}
	for _, e := range entries {
		if s, err := m.readSummary(e.Name()); err == nil {
			s.Heat = nil // the list doesn't need the heat maps
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// Scan returns one scan's summary.
func (m *Manager) Scan(id string) (*ScanSummary, error) { return m.readSummary(id) }

// DeleteScan removes a stored scan.
func (m *Manager) DeleteScan(id string) error {
	if !validScanID(id) {
		return ErrNotFound
	}
	m.mu.Lock()
	busy := m.current != nil && m.current.ScanID == id
	m.mu.Unlock()
	if busy {
		return ErrBusy
	}
	m.scans.forget(id)
	return os.RemoveAll(filepath.Join(m.scansDir(), id))
}

func (m *Manager) pruneScans() {
	all := m.Scans()
	for _, s := range all[min(len(all), maxScans):] {
		m.opts.Logf("removing old scan %s", s.ID)
		os.RemoveAll(filepath.Join(m.scansDir(), s.ID))
	}
}

// markInterruptedScans flags scans that were still running when we stopped.
func (m *Manager) markInterruptedScans() {
	entries, _ := os.ReadDir(m.scansDir())
	for _, e := range entries {
		s, err := m.readSummary(e.Name())
		if err != nil || s.State != "running" {
			continue
		}
		s.State, s.Error = "failed", "ZeroOut stopped during the scan"
		if b, err := json.MarshalIndent(s, "", "  "); err == nil {
			writeFileAtomic(filepath.Join(m.scansDir(), s.ID, "summary.json"), b)
		}
	}
}

// ---------- item queries ----------

// scanCache keeps the most recently browsed scan's items in memory.
type scanCache struct {
	mu    sync.Mutex
	id    string
	sum   *ScanSummary
	items []carve.Item
}

func (c *scanCache) forget(id string) {
	c.mu.Lock()
	if c.id == id {
		c.id, c.sum, c.items = "", nil, nil
	}
	c.mu.Unlock()
}

func (m *Manager) loadItems(id string) (*ScanSummary, []carve.Item, error) {
	m.scans.mu.Lock()
	defer m.scans.mu.Unlock()
	if m.scans.id == id {
		return m.scans.sum, m.scans.items, nil
	}
	sum, err := m.readSummary(id)
	if err != nil {
		return nil, nil, err
	}
	if sum.State == "running" {
		return nil, nil, invalid("scan is still running")
	}
	b, err := os.ReadFile(filepath.Join(m.scansDir(), id, "items.bin"))
	if err != nil {
		return nil, nil, err
	}
	items := make([]carve.Item, len(b)/itemBytes)
	for i := range items {
		r := b[i*itemBytes:]
		items[i] = carve.Item{
			Offset: int64(binary.LittleEndian.Uint64(r)),
			Size:   int64(binary.LittleEndian.Uint64(r[8:])),
			Kind:   binary.LittleEndian.Uint16(r[16:]),
			Flags:  r[18],
		}
	}
	m.scans.id, m.scans.sum, m.scans.items = id, sum, items
	return sum, items, nil
}

// ItemQuery selects items from a scan.
type ItemQuery struct {
	Exts    []string `json:"exts"`    // empty: all types
	MinSize int64    `json:"minSize"` // bytes
	Status  string   `json:"status"`  // "", "complete" (not truncated) or "partial"
	Sort    string   `json:"sort"`    // "offset" (default) or "size"
	Offset  int      `json:"offset"`
	Limit   int      `json:"limit"`
}

type ItemView struct {
	N         int    `json:"n"`
	Offset    int64  `json:"offset"`
	Size      int64  `json:"size"`
	Ext       string `json:"ext"`
	Category  string `json:"category"`
	Exact     bool   `json:"exact"`
	Truncated bool   `json:"truncated"`
}

type ItemPage struct {
	Total      int        `json:"total"`
	TotalBytes int64      `json:"totalBytes"`
	Items      []ItemView `json:"items"`
}

// selectItems returns the indices of items matching q, sorted per q.Sort.
func selectItems(sum *ScanSummary, items []carve.Item, q ItemQuery) []int {
	want := map[string]bool{}
	for _, e := range q.Exts {
		want[e] = true
	}
	var idx []int
	for i, it := range items {
		if int(it.Kind) >= len(sum.KindTable) {
			continue
		}
		if len(want) > 0 && !want[sum.KindTable[it.Kind]] || it.Size < q.MinSize {
			continue
		}
		trunc := it.Flags&carve.FlagTruncated != 0
		if q.Status == "complete" && trunc || q.Status == "partial" && !trunc {
			continue
		}
		idx = append(idx, i)
	}
	if q.Sort == "size" {
		slices.SortStableFunc(idx, func(a, b int) int {
			return compareInt64(items[b].Size, items[a].Size)
		})
	}
	return idx
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Items pages through a scan's results.
func (m *Manager) Items(id string, q ItemQuery) (*ItemPage, error) {
	sum, items, err := m.loadItems(id)
	if err != nil {
		return nil, err
	}
	idx := selectItems(sum, items, q)
	page := &ItemPage{Total: len(idx), Items: []ItemView{}}
	for _, i := range idx {
		page.TotalBytes += items[i].Size
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	for _, i := range idx[min(max(q.Offset, 0), len(idx)):min(max(q.Offset, 0)+limit, len(idx))] {
		page.Items = append(page.Items, view(sum, i, items[i]))
	}
	return page, nil
}

func view(sum *ScanSummary, n int, it carve.Item) ItemView {
	ext := sum.KindTable[it.Kind]
	cat := ""
	if k, ok := carve.KindByExt(ext); ok {
		cat = carve.Kinds[k].Category
	}
	return ItemView{N: n, Offset: it.Offset, Size: it.Size, Ext: ext, Category: cat,
		Exact: it.Flags&carve.FlagExact != 0, Truncated: it.Flags&carve.FlagTruncated != 0}
}

// ErrOverwritten means an item's blocks are no longer free: the space has
// been reused, so the deleted data is gone.
var ErrOverwritten = errors.New("this space has been reused since the scan; the deleted data is gone")

// ItemContent is an open, still-free item.
type ItemContent struct {
	View ItemView
	MIME string
	*io.SectionReader
}

// OpenItem re-checks that item n is still in free space and returns a reader
// for its bytes.
func (m *Manager) OpenItem(id string, n int) (*ItemContent, error) {
	sum, items, err := m.loadItems(id)
	if err != nil {
		return nil, err
	}
	if n < 0 || n >= len(items) || int(items[n].Kind) >= len(sum.KindTable) {
		return nil, ErrNotFound
	}
	it := items[n]
	r, fs, err := m.cachedFS(sum.Volume)
	if err != nil {
		return nil, err
	}
	if err := checkFree(fs, it); err != nil {
		return nil, err
	}
	v := view(sum, n, it)
	mime := "application/octet-stream"
	if k, ok := carve.KindByExt(v.Ext); ok {
		mime = carve.Kinds[k].MIME
	}
	return &ItemContent{View: v, MIME: mime, SectionReader: io.NewSectionReader(r, it.Offset, it.Size)}, nil
}

func checkFree(fs *ext4.FS, it carve.Item) error {
	first := uint64(it.Offset / fs.BlockSize)
	last := uint64((it.Offset + it.Size - 1) / fs.BlockSize)
	ok, err := fs.IsFree(first, last-first+1)
	if err != nil {
		return err
	}
	if !ok {
		return ErrOverwritten
	}
	return nil
}

// cachedFS keeps one open filesystem per volume for previews. That is safe
// because IsFree re-reads descriptors and bitmaps from disk on every check.
func (m *Manager) cachedFS(volumeName string) (*blockdev.Reader, *ext4.FS, error) {
	m.fsMu.Lock()
	defer m.fsMu.Unlock()
	if h, ok := m.fsOpen[volumeName]; ok {
		return h.r, h.fs, nil
	}
	r, fs, err := m.openFS(volumeName)
	if err != nil {
		return nil, nil, err
	}
	if m.fsOpen == nil {
		m.fsOpen = map[string]fsHandle{}
	}
	m.fsOpen[volumeName] = fsHandle{r, fs}
	return r, fs, nil
}

// closeFS closes the cached filesystem handles.
func (m *Manager) closeFS() {
	m.fsMu.Lock()
	defer m.fsMu.Unlock()
	for _, h := range m.fsOpen {
		h.r.Close()
	}
	m.fsOpen = nil
}

type fsHandle struct {
	r  *blockdev.Reader
	fs *ext4.FS
}

func (m *Manager) openFS(volumeName string) (*blockdev.Reader, *ext4.FS, error) {
	dev, err := m.Device(volumeName)
	if err != nil {
		return nil, nil, err
	}
	r, err := blockdev.Open(dev)
	if err != nil {
		return nil, nil, err
	}
	fs, err := ext4.Open(r)
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	return r, fs, nil
}

func fmtBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// ---------- progress meter ----------

type meter struct {
	total    int64
	last     time.Time
	lastDone int64
	rate     float64
}

func newMeter(total int64) *meter { return &meter{total: total, last: time.Now()} }

// update returns a progress snapshot at most twice a second.
func (mt *meter) update(done int64) (wipe.Progress, bool) {
	now := time.Now()
	dt := now.Sub(mt.last).Seconds()
	if dt < 0.5 {
		return wipe.Progress{}, false
	}
	inst := float64(done-mt.lastDone) / dt
	if mt.rate == 0 {
		mt.rate = inst
	} else {
		mt.rate = 0.8*mt.rate + 0.2*inst
	}
	mt.last, mt.lastDone = now, done
	p := wipe.Progress{Done: done, Total: mt.total, Rate: mt.rate}
	if mt.rate > 0 && mt.total > done {
		p.ETASeconds = float64(mt.total-done) / mt.rate
	}
	return p, true
}
