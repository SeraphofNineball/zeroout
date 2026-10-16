package carve

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
)

var (
	le = binary.LittleEndian
	be = binary.BigEndian
)

// probe gives a detector access to a candidate file. h is its first block;
// further bytes can be read with read() up to the end of the free run, since
// anything past that belongs to allocated blocks.
type probe struct {
	h     []byte
	r     io.ReaderAt
	off   int64 // file start, filesystem byte offset
	limit int64 // end of the free run, filesystem byte offset
}

// read fills b from file offset rel; false if that crosses the free run end.
func (c *probe) read(b []byte, rel int64) bool {
	if rel < 0 || c.off+rel+int64(len(b)) > c.limit {
		return false
	}
	if rel+int64(len(b)) <= int64(len(c.h)) {
		copy(b, c.h[rel:])
		return true
	}
	_, err := c.r.ReadAt(b, c.off+rel)
	return err == nil
}

// avail is how many bytes of the file lie inside the free run.
func (c *probe) avail() int64 { return c.limit - c.off }

// result of a detection. kind "" rejects the candidate.
type result struct {
	kind   string
	size   int64 // exact size from the format, or -1 if unknown
	st     streamer
	strict bool // st has a definite terminator; not finding it means truncated
}

func unknown(kind string) result { return result{kind: kind, size: -1} }
func sized(kind string, n int64) result {
	if n <= 0 {
		return unknown(kind)
	}
	return result{kind: kind, size: n}
}
func streamed(kind string, st streamer, strict bool) result {
	return result{kind: kind, size: -1, st: st, strict: strict}
}

type detector struct {
	match func(h []byte) bool
	open  func(c *probe) result
	// weak detectors (text) only apply when no other file is open, because
	// their "signature" also occurs inside other files.
	weak bool
}

func prefix(s string) func([]byte) bool {
	return func(h []byte) bool { return bytes.HasPrefix(h, []byte(s)) }
}

func fixed(kind string) func(*probe) result { return func(*probe) result { return unknown(kind) } }

var detectors = []detector{
	// ----- images -----
	{func(h []byte) bool {
		return h[0] == 0xFF && h[1] == 0xD8 && h[2] == 0xFF && (h[3] >= 0xE0 && h[3] <= 0xEF || h[3] == 0xDB || h[3] == 0xC0 || h[3] == 0xC4 || h[3] == 0xFE)
	}, func(*probe) result { return streamed("jpg", newJPEG(), true) }, false},
	{prefix("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"), func(*probe) result { return streamed("png", newPNG(), true) }, false},
	{func(h []byte) bool {
		return bytes.HasPrefix(h, []byte("GIF87a")) || bytes.HasPrefix(h, []byte("GIF89a"))
	},
		func(*probe) result { return streamed("gif", newGIF(), true) }, false},
	{prefix("BM"), openBMP, false},
	{prefix("RIFF"), openRIFF, false},
	{prefix("FORM"), openAIFF, false},
	{func(h []byte) bool {
		return bytes.HasPrefix(h, []byte("II*\x00")) || bytes.HasPrefix(h, []byte("MM\x00*")) ||
			bytes.HasPrefix(h, []byte("IIRO")) || bytes.HasPrefix(h, []byte("IIRS")) || bytes.HasPrefix(h, []byte("IIU\x00"))
	}, openTIFF, false},
	{func(h []byte) bool {
		return bytes.HasPrefix(h, []byte("8BPS\x00\x01")) || bytes.HasPrefix(h, []byte("8BPS\x00\x02"))
	}, fixed("psd"), false},

	// ----- video / audio -----
	{func(h []byte) bool { return string(h[4:8]) == "ftyp" && be.Uint32(h) >= 8 && be.Uint32(h) <= 512 }, openFtyp, false},
	{func(h []byte) bool {
		t := string(h[4:8])
		return (t == "moov" || t == "mdat" || t == "wide") && be.Uint32(h) >= 8
	}, openQuickTime, false},
	{prefix("\x1A\x45\xDF\xA3"), openEBML, false},
	{prefix("\x30\x26\xB2\x75\x8E\x66\xCF\x11\xA6\xD9\x00\xAA\x00\x62\xCE\x6C"), openASF, false},
	{func(h []byte) bool {
		return bytes.HasPrefix(h, []byte("FLV\x01")) && h[4] <= 5 && be.Uint32(h[5:]) == 9
	}, fixed("flv"), false},
	{func(h []byte) bool {
		return bytes.HasPrefix(h, []byte("\x00\x00\x01\xBA")) && (h[4]&0xC4 == 0x44 || h[4]&0xF1 == 0x21)
	}, fixed("mpg"), false},
	{func(h []byte) bool {
		for i := 0; i+188 <= len(h) && i < 188*21; i += 188 {
			if h[i] != 0x47 {
				return false
			}
		}
		return len(h) >= 188*21
	}, fixed("ts"), false},
	{func(h []byte) bool {
		return bytes.HasPrefix(h, []byte("ID3")) && h[3] >= 2 && h[3] <= 4 && h[5]&0x0F == 0
	},
		fixed("mp3"), false},
	{func(h []byte) bool { return bytes.HasPrefix(h, []byte("fLaC")) && h[4]&0x7F == 0 }, fixed("flac"), false},
	{func(h []byte) bool { return bytes.HasPrefix(h, []byte("OggS\x00\x02")) }, openOgg, false},
	{prefix("MThd\x00\x00\x00\x06"), openMIDI, false},

	// ----- documents / archives -----
	{func(h []byte) bool {
		return bytes.HasPrefix(h, []byte("%PDF-1.")) || bytes.HasPrefix(h, []byte("%PDF-2."))
	},
		func(*probe) result { return streamed("pdf", &pdfStream{}, false) }, false},
	{func(h []byte) bool { return bytes.HasPrefix(h, []byte("PK\x03\x04")) && le.Uint16(h[4:]) <= 63 }, openZIP, false},
	{prefix("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1"), openOLE, false},
	{prefix("{\\rtf1"), func(*probe) result { return streamed("rtf", &textStream{}, false) }, false},
	{func(h []byte) bool {
		return bytes.HasPrefix(h, []byte("Rar!\x1A\x07\x00")) || bytes.HasPrefix(h, []byte("Rar!\x1A\x07\x01\x00"))
	},
		fixed("rar"), false},
	{prefix("7z\xBC\xAF\x27\x1C\x00"), open7z, false},
	{func(h []byte) bool {
		return h[0] == 0x1F && h[1] == 0x8B && h[2] == 0x08 && h[3] < 0x20 && h[9] <= 13 || h[0] == 0x1F && h[1] == 0x8B && h[2] == 0x08 && h[3] < 0x20 && h[9] == 255
	},
		fixed("gz"), false},
	{func(h []byte) bool {
		return bytes.HasPrefix(h, []byte("BZh")) && h[3] >= '1' && h[3] <= '9' && bytes.Equal(h[4:10], []byte("1AY&SY"))
	}, fixed("bz2"), false},
	{prefix("\xFD7zXZ\x00"), fixed("xz"), false},
	{func(h []byte) bool { return len(h) > 263 && bytes.HasPrefix(h[257:], []byte("ustar")) }, fixed("tar"), false},
	{func(h []byte) bool { return bytes.HasPrefix(h, []byte("MSCF")) && le.Uint32(h[4:]) == 0 }, func(c *probe) result {
		return sized("cab", int64(le.Uint32(c.h[8:])))
	}, false},

	// ----- databases / programs -----
	{prefix("SQLite format 3\x00"), openSQLite, false},
	{func(h []byte) bool {
		return bytes.HasPrefix(h, []byte("\x7FELF")) && (h[4] == 1 || h[4] == 2) && (h[5] == 1 || h[5] == 2) && h[6] == 1
	},
		openELF, false},
	{prefix("MZ"), openPE, false},

	// ----- text (weak) -----
	{isTextStart, openText, true},
}

// ---------- known-length formats ----------

func openBMP(c *probe) result {
	h := c.h
	size, dataOff, dib := int64(le.Uint32(h[2:])), le.Uint32(h[10:]), le.Uint32(h[14:])
	if le.Uint32(h[6:]) != 0 || dataOff < 26 || dataOff > 8192 {
		return result{}
	}
	switch dib {
	case 12, 40, 52, 56, 64, 108, 124:
	default:
		return result{}
	}
	if dib >= 40 {
		if le.Uint16(h[26:]) != 1 {
			return result{}
		}
		switch le.Uint16(h[28:]) {
		case 1, 4, 8, 16, 24, 32:
		default:
			return result{}
		}
	}
	if size < int64(dataOff) {
		return unknown("bmp")
	}
	return sized("bmp", size)
}

func openRIFF(c *probe) result {
	size := int64(le.Uint32(c.h[4:])) + 8
	var kind string
	switch string(c.h[8:12]) {
	case "WAVE":
		kind = "wav"
	case "AVI ":
		kind = "avi"
	case "WEBP":
		kind = "webp"
	default:
		return result{}
	}
	if size < 20 {
		return result{}
	}
	return sized(kind, size+size%2)
}

func openAIFF(c *probe) result {
	if t := string(c.h[8:12]); t != "AIFF" && t != "AIFC" {
		return result{}
	}
	return sized("aiff", int64(be.Uint32(c.h[4:]))+8)
}

func openTIFF(c *probe) result {
	h := c.h
	switch {
	case bytes.HasPrefix(h, []byte("IIRO")), bytes.HasPrefix(h, []byte("IIRS")):
		return unknown("orf")
	case bytes.HasPrefix(h, []byte("IIU\x00")):
		return unknown("rw2")
	case len(h) > 10 && string(h[8:10]) == "CR":
		return unknown("cr2")
	case bytes.Contains(h, []byte("NIKON")):
		return unknown("nef")
	case bytes.Contains(h, []byte("SONY")):
		return unknown("arw")
	case bytes.Contains(h, []byte("PENTAX")):
		return unknown("pef")
	}
	// Plain TIFF: the first IFD offset must point inside the file.
	var ifd uint32
	if h[0] == 'I' {
		ifd = le.Uint32(h[4:])
	} else {
		ifd = be.Uint32(h[4:])
	}
	if ifd < 8 {
		return result{}
	}
	return unknown("tif")
}

// bmffWalk walks ISO base media (MP4/MOV/HEIC) top-level boxes and returns
// the end of the last box, or -1 if the layout is unusable.
func bmffWalk(c *probe) int64 {
	var hdr [16]byte
	off, boxes := int64(0), 0
	for ; boxes < 100000; boxes++ {
		if !c.read(hdr[:8], off) {
			break
		}
		size := int64(be.Uint32(hdr[:4]))
		typ := hdr[4:8]
		if !printable(typ) || boxes > 0 && string(typ) == "ftyp" {
			break
		}
		hl := int64(8)
		switch size {
		case 0:
			return -1 // box extends to end of file: length unknown
		case 1:
			if !c.read(hdr[8:16], off+8) {
				return off + 16 // can't even read the size: truncated here
			}
			size, hl = int64(be.Uint64(hdr[8:16])), 16
		}
		if size < hl {
			break
		}
		off += size
	}
	if boxes < 2 {
		return -1
	}
	return off
}

func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}

func openFtyp(c *probe) result {
	brand := string(c.h[8:12])
	kind := "mp4"
	switch {
	case brand == "qt  ":
		kind = "mov"
	case brand == "M4A " || brand == "M4B " || brand == "M4P ":
		kind = "m4a"
	case brand == "M4V " || brand == "M4VH" || brand == "M4VP":
		kind = "m4v"
	case brand == "heic" || brand == "heix" || brand == "heim" || brand == "heis" || brand == "hevc" || brand == "mif1" || brand == "msf1":
		kind = "heic"
	case brand == "avif" || brand == "avis":
		kind = "avif"
	case brand == "crx ":
		kind = "cr3"
	case strings.HasPrefix(brand, "3g"):
		kind = "3gp"
	}
	return sized(kind, bmffWalk(c))
}

func openQuickTime(c *probe) result {
	n := bmffWalk(c)
	if n <= 0 {
		return result{} // a lone moov/mdat-looking block is too weak a signal
	}
	return sized("mov", n)
}

// vint reads an EBML variable-length integer at b[0]. It returns the value
// (marker bit removed when strip is set), its length, and whether all value
// bits were 1 (the "unknown size" marker).
func vint(b []byte, strip bool) (v uint64, n int, allOnes bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	for n = 1; b[0]&(0x80>>(n-1)) == 0; n++ {
	}
	if n > len(b) {
		return 0, 0, false
	}
	first := uint64(b[0])
	if strip {
		first &^= 0x80 >> (n - 1)
	}
	v = first
	allOnes = first == uint64(0xFF>>n)
	for _, x := range b[1:n] {
		v = v<<8 | uint64(x)
		allOnes = allOnes && x == 0xFF
	}
	return v, n, allOnes
}

func openEBML(c *probe) result {
	h := c.h
	size, n, _ := vint(h[4:], true)
	hdrEnd := 4 + n + int(size)
	if n == 0 || hdrEnd > 512 {
		return result{}
	}
	kind := "mkv"
	if bytes.Contains(h[:hdrEnd], []byte("webm")) {
		kind = "webm"
	}
	if hdrEnd+4 > len(h) || !bytes.Equal(h[hdrEnd:hdrEnd+4], []byte{0x18, 0x53, 0x80, 0x67}) {
		return unknown(kind)
	}
	segSize, sn, unk := vint(h[hdrEnd+4:], true)
	if sn == 0 || unk {
		return unknown(kind)
	}
	return sized(kind, int64(hdrEnd+4+sn)+int64(segSize))
}

var (
	asfObjects = [][]byte{
		[]byte("\x30\x26\xB2\x75\x8E\x66\xCF\x11\xA6\xD9\x00\xAA\x00\x62\xCE\x6C"), // header
		[]byte("\x36\x26\xB2\x75\x8E\x66\xCF\x11\xA6\xD9\x00\xAA\x00\x62\xCE\x6C"), // data
		[]byte("\x90\x08\x00\x33\xB1\xE5\xCF\x11\x89\xF4\x00\xA0\xC9\x03\x49\xCB"), // simple index
		[]byte("\xD3\x29\xE2\xD6\xDA\x35\xD1\x11\x90\x34\x00\xA0\xC9\x03\x49\xBE"), // index
	}
	asfVideoMedia = []byte("\xC0\xEF\x19\xBC\x4D\x5B\xCF\x11\xA8\xFD\x00\x80\x5F\x5C\x44\x2B")
)

func openASF(c *probe) result {
	kind := "wma"
	if bytes.Contains(c.h, asfVideoMedia) {
		kind = "wmv"
	}
	var hdr [24]byte
	off := int64(0)
	for i := 0; i < 64; i++ {
		if !c.read(hdr[:], off) {
			break
		}
		known := false
		for _, g := range asfObjects {
			known = known || bytes.Equal(hdr[:16], g)
		}
		size := int64(le.Uint64(hdr[16:]))
		if !known || size < 24 {
			break
		}
		off += size
	}
	return sized(kind, off)
}

func openOgg(c *probe) result {
	kind := "ogg"
	switch {
	case bytes.Contains(c.h[:min(len(c.h), 512)], []byte("\x80theora")):
		kind = "ogv"
	case bytes.Contains(c.h[:min(len(c.h), 512)], []byte("OpusHead")):
		kind = "opus"
	}
	return streamed(kind, newOgg(), true)
}

func openMIDI(c *probe) result {
	var hdr [8]byte
	off := int64(14)
	tracks := int(be.Uint16(c.h[10:]))
	for i := 0; i < tracks; i++ {
		if !c.read(hdr[:], off) {
			return sized("mid", off+1<<20) // runs past the free space
		}
		if string(hdr[:4]) != "MTrk" {
			break
		}
		off += 8 + int64(be.Uint32(hdr[4:]))
	}
	return sized("mid", off)
}

func openZIP(c *probe) result {
	h := c.h
	kind := "zip"
	nameLen, extraLen := int(le.Uint16(h[26:])), int(le.Uint16(h[28:]))
	name := ""
	if 30+nameLen <= len(h) {
		name = string(h[30 : 30+nameLen])
	}
	look := make([]byte, min(64*KB, c.avail()))
	if !c.read(look, 0) {
		look = h
	}
	switch {
	case name == "mimetype":
		data := look[min(len(look), 30+nameLen+extraLen):]
		switch {
		case bytes.HasPrefix(data, []byte("application/epub+zip")):
			kind = "epub"
		case bytes.HasPrefix(data, []byte("application/vnd.oasis.opendocument.text")):
			kind = "odt"
		case bytes.HasPrefix(data, []byte("application/vnd.oasis.opendocument.spreadsheet")):
			kind = "ods"
		case bytes.HasPrefix(data, []byte("application/vnd.oasis.opendocument.presentation")):
			kind = "odp"
		}
	case bytes.Contains(look, []byte("AndroidManifest.xml")):
		kind = "apk"
	case bytes.Contains(look, []byte("word/")):
		kind = "docx"
	case bytes.Contains(look, []byte("xl/")):
		kind = "xlsx"
	case bytes.Contains(look, []byte("ppt/")):
		kind = "pptx"
	case strings.HasPrefix(name, "META-INF/"):
		kind = "jar"
	}
	return streamed(kind, newZIP(), true)
}

var (
	oleWord  = utf16("WordDocument")
	oleExcel = utf16("Workbook")
	oleBook  = utf16("Book")
	olePPT   = utf16("PowerPoint Document")
	oleMsg   = utf16("__substg1.0_")
)

func utf16(s string) []byte {
	b := make([]byte, 0, 2*len(s))
	for _, r := range s {
		b = append(b, byte(r), 0)
	}
	return b
}

func openOLE(c *probe) result {
	h := c.h
	shift := le.Uint16(h[30:])
	if shift != 9 && shift != 12 {
		return result{}
	}
	ss := int64(1) << shift
	nFAT := int64(le.Uint32(h[44:]))
	dirStart := int64(le.Uint32(h[48:]))

	kind := "ole"
	dir := make([]byte, ss)
	if c.read(dir, (dirStart+1)*ss) {
		switch {
		case bytes.Contains(dir, oleWord):
			kind = "doc"
		case bytes.Contains(dir, oleExcel), bytes.Contains(dir, oleBook):
			kind = "xls"
		case bytes.Contains(dir, olePPT):
			kind = "ppt"
		case bytes.Contains(dir, oleMsg):
			kind = "msg"
		}
	}
	if nFAT == 0 || nFAT > 109 {
		return unknown(kind) // FAT continues in DIFAT sectors; not followed
	}
	// The file ends after the highest sector any FAT entry marks as used.
	fat := make([]byte, ss)
	last := int64(-1)
	for i := int64(0); i < nFAT; i++ {
		sec := int64(le.Uint32(h[76+4*i:]))
		if !c.read(fat, (sec+1)*ss) {
			return sized(kind, c.avail()+1) // FAT lies beyond the free space
		}
		for k := ss/4 - 1; k >= 0; k-- {
			if le.Uint32(fat[4*k:]) != 0xFFFFFFFF {
				last = max(last, i*(ss/4)+k)
				break
			}
		}
	}
	if last < 0 {
		return unknown(kind)
	}
	return sized(kind, (last+2)*ss)
}

func open7z(c *probe) result {
	next, size := le.Uint64(c.h[12:]), le.Uint64(c.h[20:])
	if next > 1<<46 || size > 1<<32 {
		return result{}
	}
	return sized("7z", 32+int64(next)+int64(size))
}

func openSQLite(c *probe) result {
	h := c.h
	ps := int64(be.Uint16(h[16:]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps&(ps-1) != 0 {
		return result{}
	}
	pages := int64(be.Uint32(h[28:]))
	if pages == 0 || be.Uint32(h[24:]) != be.Uint32(h[92:]) {
		return unknown("sqlite")
	}
	return sized("sqlite", ps*pages)
}

func openELF(c *probe) result {
	h := c.h
	var o binary.ByteOrder = binary.LittleEndian
	if h[5] == 2 {
		o = binary.BigEndian
	}
	var sh, ph int64
	if h[4] == 2 {
		sh = int64(o.Uint64(h[0x28:])) + int64(o.Uint16(h[0x3A:]))*int64(o.Uint16(h[0x3C:]))
		ph = int64(o.Uint64(h[0x20:])) + int64(o.Uint16(h[0x36:]))*int64(o.Uint16(h[0x38:]))
	} else {
		sh = int64(o.Uint32(h[0x20:])) + int64(o.Uint16(h[0x2E:]))*int64(o.Uint16(h[0x30:]))
		ph = int64(o.Uint32(h[0x1C:])) + int64(o.Uint16(h[0x2A:]))*int64(o.Uint16(h[0x2C:]))
	}
	return sized("elf", max(sh, ph))
}

func openPE(c *probe) result {
	h := c.h
	lfanew := int64(le.Uint32(h[0x3C:]))
	if lfanew < 0x40 || lfanew+24 > int64(len(h)) || string(h[lfanew:lfanew+4]) != "PE\x00\x00" {
		return result{}
	}
	nsec := int64(le.Uint16(h[lfanew+6:]))
	optSize := int64(le.Uint16(h[lfanew+20:]))
	chars := le.Uint16(h[lfanew+22:])
	if nsec == 0 || nsec > 96 {
		return result{}
	}
	kind := "exe"
	if chars&0x2000 != 0 {
		kind = "dll"
	}
	opt := lfanew + 24
	end := int64(0)
	sec := make([]byte, 40*nsec)
	if !c.read(sec, opt+optSize) {
		return unknown(kind)
	}
	for i := int64(0); i < nsec; i++ {
		s := sec[40*i:]
		end = max(end, int64(le.Uint32(s[20:]))+int64(le.Uint32(s[16:])))
	}
	// An Authenticode signature is appended after the sections.
	var dd [8]byte
	ddBase := opt + 96
	if c.read(dd[:2], opt) && le.Uint16(dd[:2]) == 0x20b {
		ddBase = opt + 112
	}
	if c.read(dd[:], ddBase+4*8) {
		if off, size := int64(le.Uint32(dd[:4])), int64(le.Uint32(dd[4:])); off > 0 && size > 0 {
			end = max(end, off+size)
		}
	}
	return sized(kind, end)
}

// ---------- text ----------

func isTextStart(h []byte) bool {
	n := min(len(h), 512)
	if bytes.IndexByte(h[:n], 0) >= 0 || !looksText(h[:n], 0) {
		return false
	}
	ascii := 0
	for _, c := range h[:n] {
		if c >= 0x20 && c < 0x7F || c == '\n' || c == '\r' || c == '\t' {
			ascii++
		}
	}
	return ascii*4 >= n*3
}

func openText(c *probe) result {
	head := strings.ToLower(string(c.h[:min(len(c.h), 1024)]))
	trimmed := strings.TrimLeft(head, " \t\r\n"+string(rune(0xFEFF)))
	kind := "txt"
	switch {
	case strings.HasPrefix(trimmed, "<?xml"):
		kind = "xml"
		if strings.Contains(head, "<svg") {
			kind = "svg"
		}
	case strings.HasPrefix(trimmed, "<svg"):
		kind = "svg"
	case strings.HasPrefix(trimmed, "<!doctype html"), strings.HasPrefix(trimmed, "<html"):
		kind = "html"
	case strings.HasPrefix(trimmed, "{\""), strings.HasPrefix(trimmed, "[{"), strings.HasPrefix(trimmed, "[\""):
		kind = "json"
	case strings.HasPrefix(trimmed, "begin:vcard"):
		kind = "vcf"
	case strings.HasPrefix(trimmed, "begin:vcalendar"):
		kind = "ics"
	case strings.HasPrefix(trimmed, "#!"):
		kind = "sh"
	case hasMailHeaders(head):
		kind = "eml"
	}
	return streamed(kind, &textStream{}, false)
}

func hasMailHeaders(s string) bool {
	n := 0
	for _, h := range []string{"return-path:", "received:", "from:", "to:", "subject:", "date:", "message-id:", "mime-version:", "delivered-to:"} {
		if strings.Contains(s, "\n"+h) || strings.HasPrefix(s, h) {
			n++
		}
	}
	return n >= 3
}
