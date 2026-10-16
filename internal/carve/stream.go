package carve

import (
	"bytes"
	"encoding/binary"
)

type status int

const (
	more   status = iota
	done          // file ends exactly at the returned offset
	broken        // data stopped making sense; returned offset is a best guess
)

// A streamer is fed a file's data block by block, in order, starting at the
// file's first byte, and reports where the file ends. Offsets are relative to
// the start of the file.
type streamer interface {
	feed(p []byte) (end int64, st status)
}

// lastEnder is implemented by streamers that can't detect the end on their
// own but know the last plausible end seen (e.g. a PDF's final %%EOF).
type lastEnder interface {
	lastEnd() (int64, bool)
}

// ---------- JPEG: walk marker segments, skip entropy-coded data ----------

const (
	jsMarkerFF = iota
	jsMarker
	jsLen1
	jsLen2
	jsSkip
	jsEntropy
	jsEntropyFF
)

type jpegStream struct {
	pos   int64
	state int
	after int
	skip  int
	lenHi byte
}

func newJPEG() *jpegStream { return &jpegStream{state: jsSkip, skip: 2, after: jsMarkerFF} }

func (s *jpegStream) feed(p []byte) (int64, status) {
	base := s.pos
	s.pos += int64(len(p))
	for i := 0; i < len(p); {
		switch s.state {
		case jsSkip:
			n := min(s.skip, len(p)-i)
			i += n
			s.skip -= n
			if s.skip == 0 {
				s.state = s.after
			}
		case jsMarkerFF:
			if p[i] != 0xFF {
				return base + int64(i), broken
			}
			s.state = jsMarker
			i++
		case jsMarker:
			m := p[i]
			i++
			switch {
			case m == 0xFF: // fill byte
			case m == 0xD9:
				return base + int64(i), done
			case m >= 0xD0 && m <= 0xD7, m == 0x01:
				s.state = jsMarkerFF
			case m == 0x00, m == 0xD8:
				return base + int64(i-2), broken
			default:
				s.after = jsMarkerFF
				if m == 0xDA {
					s.after = jsEntropy
				}
				s.state = jsLen1
			}
		case jsLen1:
			s.lenHi = p[i]
			i++
			s.state = jsLen2
		case jsLen2:
			l := int(s.lenHi)<<8 | int(p[i])
			i++
			if l < 2 {
				return base + int64(i), broken
			}
			s.skip, s.state = l-2, jsSkip
			if s.skip == 0 {
				s.state = s.after
			}
		case jsEntropy:
			k := bytes.IndexByte(p[i:], 0xFF)
			if k < 0 {
				i = len(p)
				continue
			}
			i += k + 1
			s.state = jsEntropyFF
		case jsEntropyFF:
			switch m := p[i]; {
			case m == 0x00, m >= 0xD0 && m <= 0xD7:
				s.state = jsEntropy
				i++
			case m == 0xFF:
				i++
			default:
				s.state = jsMarker // let jsMarker consume m
			}
		}
	}
	return s.pos, more
}

// ---------- signature-terminated formats (PNG, ZIP) ----------

// tailStream searches the stream for a terminator, carrying the last few
// bytes between feeds so a terminator split across blocks is still found.
type tailStream struct {
	pos   int64 // stream offset of data[len(carry)] on the next feed
	carry []byte
	keep  int
	// find returns the end of the file as an index into data, or -1. It may
	// return an index beyond len(data) when the terminator says more follows.
	find    func(data []byte) int
	pending int64 // >0: file ends at this stream offset
}

func (s *tailStream) feed(p []byte) (int64, status) {
	base := s.pos
	s.pos += int64(len(p))
	if s.pending > 0 {
		if s.pending <= s.pos {
			return s.pending, done
		}
		return s.pos, more
	}
	data := append(append(make([]byte, 0, len(s.carry)+len(p)), s.carry...), p...)
	dataStart := base - int64(len(s.carry))
	if end := s.find(data); end >= 0 {
		abs := dataStart + int64(end)
		if abs <= s.pos {
			return abs, done
		}
		s.pending = abs
		return s.pos, more
	}
	s.carry = append(s.carry[:0], data[max(0, len(data)-s.keep):]...)
	return s.pos, more
}

var pngEnd = []byte("IEND\xAE\x42\x60\x82")

func newPNG() streamer {
	return &tailStream{keep: len(pngEnd) - 1, find: func(d []byte) int {
		if i := bytes.Index(d, pngEnd); i >= 0 {
			return i + len(pngEnd)
		}
		return -1
	}}
}

var zipEOCD = []byte("PK\x05\x06")

func newZIP() streamer {
	return &tailStream{keep: 21, find: func(d []byte) int {
		for from := 0; ; {
			i := bytes.Index(d[from:], zipEOCD)
			if i < 0 {
				return -1
			}
			i += from
			if i+22 > len(d) {
				return -1 // header incomplete; it is in the carry for next time
			}
			return i + 22 + int(binary.LittleEndian.Uint16(d[i+20:]))
		}
	}}
}

// ---------- PDF: remember the last %%EOF ----------

type pdfStream struct {
	pos   int64
	carry []byte
	last  int64
}

var pdfEOF = []byte("%%EOF")

func (s *pdfStream) feed(p []byte) (int64, status) {
	data := append(append(make([]byte, 0, len(s.carry)+len(p)), s.carry...), p...)
	dataStart := s.pos - int64(len(s.carry))
	s.pos += int64(len(p))
	for from := 0; ; {
		i := bytes.Index(data[from:], pdfEOF)
		if i < 0 {
			break
		}
		end := from + i + len(pdfEOF)
		for end < len(data) && end < from+i+len(pdfEOF)+2 && (data[end] == '\r' || data[end] == '\n') {
			end++
		}
		if e := dataStart + int64(end); e > s.last {
			s.last = e
		}
		from += i + 1
	}
	s.carry = append(s.carry[:0], data[max(0, len(data)-len(pdfEOF)-1):]...)
	return s.pos, more
}

func (s *pdfStream) lastEnd() (int64, bool) { return s.last, s.last > 0 }

// ---------- GIF: walk blocks to the trailer ----------

const (
	gsHeader = iota
	gsSkip
	gsIntro
	gsExtLabel
	gsSubLen
	gsImgDesc
)

type gifStream struct {
	pos   int64
	state int
	after int
	skip  int
	buf   []byte
	need  int
}

func newGIF() *gifStream { return &gifStream{state: gsHeader, need: 13} }

func (s *gifStream) feed(p []byte) (int64, status) {
	base := s.pos
	s.pos += int64(len(p))
	for i := 0; i < len(p); {
		switch s.state {
		case gsHeader, gsImgDesc:
			n := min(s.need-len(s.buf), len(p)-i)
			s.buf = append(s.buf, p[i:i+n]...)
			i += n
			if len(s.buf) < s.need {
				continue
			}
			flags := s.buf[len(s.buf)-1]
			if s.state == gsHeader {
				flags = s.buf[10]
			}
			s.skip = 0
			if flags&0x80 != 0 {
				s.skip = 3 << ((flags & 7) + 1)
			}
			s.after = gsIntro
			if s.state == gsImgDesc {
				s.skip++ // LZW minimum code size
				s.after = gsSubLen
			}
			s.buf, s.state = s.buf[:0], gsSkip
		case gsSkip:
			n := min(s.skip, len(p)-i)
			i += n
			s.skip -= n
			if s.skip == 0 {
				s.state = s.after
			}
		case gsIntro:
			switch p[i] {
			case 0x21:
				s.state = gsExtLabel
			case 0x2C:
				s.state, s.need = gsImgDesc, 9
			case 0x3B:
				return base + int64(i) + 1, done
			default:
				return base + int64(i), broken
			}
			i++
		case gsExtLabel:
			s.state = gsSubLen
			i++
		case gsSubLen:
			if n := int(p[i]); n == 0 {
				s.state = gsIntro
			} else {
				s.skip, s.after, s.state = n, gsSubLen, gsSkip
			}
			i++
		}
	}
	return s.pos, more
}

// ---------- Ogg: walk pages until one doesn't start with OggS ----------

type oggStream struct {
	pos      int64
	pageAt   int64 // stream offset where the current page header started
	hdr      []byte
	segs     int
	skip     int64
	lastEOS  bool
	inHeader bool
}

func newOgg() *oggStream { return &oggStream{inHeader: true} }

func (s *oggStream) feed(p []byte) (int64, status) {
	base := s.pos
	s.pos += int64(len(p))
	for i := 0; i < len(p); {
		if !s.inHeader {
			n := min(s.skip, int64(len(p)-i))
			i += int(n)
			s.skip -= n
			if s.skip == 0 {
				s.inHeader, s.pageAt = true, base+int64(i)
			}
			continue
		}
		s.hdr = append(s.hdr, p[i])
		i++
		k := len(s.hdr)
		if k <= 4 && s.hdr[k-1] != "OggS"[k-1] {
			// No further page: the file ended at the previous page boundary.
			if s.lastEOS {
				return s.pageAt, done
			}
			return s.pageAt, broken
		}
		if k == 27 {
			s.segs = int(s.hdr[26])
			s.lastEOS = s.hdr[5]&0x04 != 0
		}
		if k == 27+s.segs && k >= 27 {
			var sum int64
			for _, l := range s.hdr[27:] {
				sum += int64(l)
			}
			s.hdr, s.skip, s.inHeader = s.hdr[:0], sum, false
			if sum == 0 {
				s.inHeader, s.pageAt = true, base+int64(i)
			}
		}
	}
	return s.pos, more
}

// ---------- text: ends at the first NUL (ext4 zero-fills the last block) ----------

type textStream struct{ pos int64 }

func (s *textStream) feed(p []byte) (int64, status) {
	base := s.pos
	s.pos += int64(len(p))
	n := len(p)
	if i := bytes.IndexByte(p, 0); i >= 0 {
		n = i
	}
	if !looksText(p[:n], 0.02) {
		return base, broken
	}
	if n < len(p) {
		return base + int64(n), done
	}
	return s.pos, more
}

// looksText reports whether b is mostly printable text, allowing up to
// maxCtl (fraction) of control bytes. Bytes >= 0x80 count as text (UTF-8).
func looksText(b []byte, maxCtl float64) bool {
	ctl := 0
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' && c != '\f' || c == 0x7F {
			ctl++
		}
	}
	return float64(ctl) <= maxCtl*float64(len(b))
}
