package carve

import (
	"bytes"
	"context"
	"errors"
	"io"

	"zeroout/internal/aligned"
)

// Item is a file found in free space.
type Item struct {
	Offset int64  // filesystem byte offset of the first byte
	Size   int64  // bytes
	Kind   uint16 // index into Kinds
	Flags  uint8
}

const (
	// FlagExact: the end was determined from the file's own structure.
	FlagExact uint8 = 1 << iota
	// FlagTruncated: the file continues past the free space it was found in
	// (the rest was overwritten by newer data), so the recovered copy is partial.
	FlagTruncated
)

// Extent is a run of free blocks.
type Extent struct {
	Start, Count uint64
}

// HeatCell summarises one slice of the volume.
type HeatCell struct {
	Free    int64 `json:"free"`    // free blocks scanned
	NonZero int64 `json:"nonZero"` // of those, blocks still holding data
}

// Stats accumulates over a scan.
type Stats struct {
	ScannedBytes    int64
	ZeroBytes       int64
	IdentifiedBytes int64 // non-zero bytes attributed to a found file
	Heat            []HeatCell
}

// Scanner carves files out of free extents.
type Scanner struct {
	R           io.ReaderAt // filesystem-relative
	BlockSize   int64
	TotalBlocks uint64 // filesystem size, for the heat map
	HeatCells   int
	OnItem      func(Item) error
	OnProgress  func(scannedBytes int64)

	Stats Stats

	buf  []byte
	zero []byte
	cur  *openFile
}

type openFile struct {
	kind     uint16
	start    int64
	knownEnd int64 // -1 if unknown
	st       streamer
	strict   bool
	ended    bool
	end      int64
	exact    bool
}

const readSize = 16 << 20

// Scan processes one free extent. Extents must be maximal runs of free
// blocks: a file never continues from one extent into the next.
func (s *Scanner) Scan(ctx context.Context, e Extent) error {
	if s.buf == nil {
		s.buf = aligned.Buffer(readSize / int(s.BlockSize) * int(s.BlockSize))
		s.zero = make([]byte, s.BlockSize)
		if s.HeatCells > 0 && len(s.Stats.Heat) == 0 {
			s.Stats.Heat = make([]HeatCell, s.HeatCells)
		}
	}
	bs := s.BlockSize
	start, end := int64(e.Start)*bs, int64(e.Start+e.Count)*bs
	for off := start; off < end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(int64(len(s.buf)), end-off)
		got, err := s.R.ReadAt(s.buf[:n], off)
		if err != nil && !(errors.Is(err, io.EOF) && got > 0) {
			return err
		}
		n = int64(got) / bs * bs
		if n == 0 {
			break
		}
		for i := int64(0); i < n; i += bs {
			if err := s.block(s.buf[i:i+bs], off+i, end); err != nil {
				return err
			}
		}
		off += n
		s.Stats.ScannedBytes += n
		if s.OnProgress != nil {
			s.OnProgress(s.Stats.ScannedBytes)
		}
	}
	return s.closeAt(end)
}

func (s *Scanner) block(b []byte, o, runEnd int64) error {
	bs := s.BlockSize
	zero := bytes.Equal(b, s.zero)
	if s.Stats.Heat != nil {
		c := &s.Stats.Heat[uint64(o/bs)*uint64(len(s.Stats.Heat))/max(s.TotalBlocks, 1)%uint64(len(s.Stats.Heat))]
		c.Free++
		if !zero {
			c.NonZero++
		}
	}
	if zero {
		s.Stats.ZeroBytes += bs
	}

	if c := s.cur; c != nil {
		switch {
		case c.knownEnd >= 0 && o < c.knownEnd:
			if !zero {
				s.Stats.IdentifiedBytes += bs
			}
			return nil
		case c.knownEnd >= 0:
			if err := s.emit(c.knownEnd, FlagExact); err != nil {
				return err
			}
		case c.ended:
			if err := s.finishStream(); err != nil {
				return err
			}
		}
	}
	if zero {
		if s.cur != nil {
			return s.closeEstimate(o)
		}
		return nil
	}

	if s.cur == nil || !Kinds[s.cur.kind].container {
		if f := s.detect(b, o, runEnd); f != nil {
			if s.cur != nil {
				if err := s.closeEstimate(o); err != nil {
					return err
				}
			}
			s.cur = f
			s.Stats.IdentifiedBytes += bs
			if f.st != nil {
				s.feed(b)
			}
			return nil
		}
	}

	c := s.cur
	if c == nil {
		return nil
	}
	s.Stats.IdentifiedBytes += bs
	if c.st != nil && !c.ended {
		s.feed(b)
	}
	if !c.ended && o+bs-c.start >= Kinds[c.kind].maxSize {
		return s.emit(o+bs, FlagTruncated)
	}
	return nil
}

func (s *Scanner) detect(b []byte, o, runEnd int64) *openFile {
	for _, d := range detectors {
		if d.weak && s.cur != nil || !d.match(b) {
			continue
		}
		r := d.open(&probe{h: b, r: s.R, off: o, limit: runEnd})
		if r.kind == "" {
			continue
		}
		k, ok := KindByExt(r.kind)
		if !ok {
			panic("carve: unknown kind " + r.kind)
		}
		f := &openFile{kind: k, start: o, knownEnd: -1, st: r.st, strict: r.strict}
		if r.size > 0 && r.size <= Kinds[k].maxSize {
			f.knownEnd = o + r.size
			f.st = nil
		} else if r.size > 0 {
			continue // implausible size: corrupt header, keep looking
		}
		return f
	}
	return nil
}

func (s *Scanner) feed(b []byte) {
	c := s.cur
	end, st := c.st.feed(b)
	switch st {
	case done:
		c.ended, c.end, c.exact = true, c.start+end, true
	case broken:
		c.ended, c.end = true, c.start+end
	}
}

func (s *Scanner) finishStream() error {
	c := s.cur
	if c.exact {
		return s.emit(c.end, FlagExact)
	}
	return s.emit(c.end, 0)
}

// closeEstimate ends the open file at o because something (a zero block, a
// new header) shows its data stopped there.
func (s *Scanner) closeEstimate(o int64) error {
	c := s.cur
	if le, ok := c.st.(lastEnder); ok {
		if end, ok := le.lastEnd(); ok && c.start+end <= o {
			return s.emit(c.start+end, FlagExact)
		}
	}
	if c.strict {
		return s.emit(o, FlagTruncated)
	}
	return s.emit(o, 0)
}

// closeAt finishes the open file at the end of a free run.
func (s *Scanner) closeAt(runEnd int64) error {
	c := s.cur
	switch {
	case c == nil:
		return nil
	case c.knownEnd >= 0 && c.knownEnd <= runEnd:
		return s.emit(c.knownEnd, FlagExact)
	case c.knownEnd >= 0:
		return s.emit(runEnd, FlagTruncated)
	case c.ended:
		return s.finishStream()
	}
	return s.closeEstimate(runEnd)
}

func (s *Scanner) emit(end int64, flags uint8) error {
	c := s.cur
	s.cur = nil
	if end <= c.start || s.OnItem == nil {
		return nil
	}
	return s.OnItem(Item{Offset: c.start, Size: end - c.start, Kind: c.kind, Flags: flags})
}
