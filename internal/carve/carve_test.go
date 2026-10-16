package carve

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"zeroout/internal/ext4"
	"zeroout/internal/ext4/ext4test"
)

const bs = ext4test.BlockSize

func noiseImage(w, h int, seed uint64) *image.RGBA {
	r := rand.New(rand.NewPCG(seed, 1))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(r.Uint32())
	}
	return img
}

func encode(t *testing.T, f func(*bytes.Buffer) error) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := f(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func randomBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, 2))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func testFiles(t *testing.T) map[string][]byte {
	files := map[string][]byte{}
	files["jpg"] = encode(t, func(b *bytes.Buffer) error {
		return jpeg.Encode(b, noiseImage(200, 160, 1), &jpeg.Options{Quality: 90})
	})
	files["png"] = encode(t, func(b *bytes.Buffer) error { return png.Encode(b, noiseImage(64, 64, 2)) })
	files["gif"] = encode(t, func(b *bytes.Buffer) error {
		p := image.NewPaletted(image.Rect(0, 0, 120, 90), []color.Color{color.Black, color.White, color.RGBA{200, 10, 10, 255}})
		r := rand.New(rand.NewPCG(3, 3))
		for i := range p.Pix {
			p.Pix[i] = uint8(r.IntN(3))
		}
		return gif.Encode(b, p, nil)
	})
	files["docx"] = encode(t, func(b *bytes.Buffer) error {
		z := zip.NewWriter(b)
		for name, data := range map[string][]byte{"[Content_Types].xml": []byte("<Types/>"), "word/document.xml": randomBytes(9000, 4)} {
			w, _ := z.Create(name)
			w.Write(data)
		}
		z.SetComment("zeroout test")
		return z.Close()
	})
	files["pdf"] = append(append([]byte("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n"), randomBytes(7000, 5)...), "\ntrailer\n%%EOF\n"...)
	wav := make([]byte, 44+9000)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	copy(wav[44:], randomBytes(9000, 6))
	files["wav"] = wav
	mp4 := []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2")
	mdat := make([]byte, 8+10000)
	binary.BigEndian.PutUint32(mdat, uint32(len(mdat)))
	copy(mdat[4:], "mdat")
	copy(mdat[8:], randomBytes(10000, 7))
	moov := make([]byte, 108)
	binary.BigEndian.PutUint32(moov, 108)
	copy(moov[4:], "moov")
	files["mp4"] = append(append(mp4, mdat...), moov...)
	files["txt"] = []byte(strings.Repeat("The quick brown fox jumps over the lazy dog.\n", 120))
	return files
}

type layout struct {
	img   ext4test.Image
	where map[string]int // kind -> start block
}

func blocks(n int) int { return (n + bs - 1) / bs }

func TestScanFindsDeletedFiles(t *testing.T) {
	files := testFiles(t)
	order := []string{"jpg", "png", "gif", "docx", "pdf", "wav", "mp4", "txt"}
	im := ext4test.Image{Blocks: 200, Data: map[int][]byte{}}
	where := map[string]int{}
	next := 5
	for i, k := range order {
		where[k] = next
		im.Data[next] = files[k]
		next += blocks(len(files[k]))
		if i%2 == 0 {
			next++ // zero gap after some files; others are packed back to back
		}
	}
	// A live JPEG in allocated blocks must not be reported.
	live := files["jpg"]
	liveAt := next + 2
	im.Data[liveAt] = live
	for b := liveAt; b < liveAt+blocks(len(live)); b++ {
		im.Used = append(im.Used, b)
	}
	// A deleted PNG whose second half was reused by a newer (allocated) file.
	partAt := liveAt + blocks(len(live)) + 2
	im.Data[partAt] = files["png"]
	im.Used = append(im.Used, partAt+2)

	items := scanImage(t, im.Build(), im.Blocks)
	byStart := map[int64]Item{}
	for _, it := range items {
		byStart[it.Offset] = it
	}
	for _, k := range order {
		it, ok := byStart[int64(where[k])*bs]
		if !ok {
			t.Errorf("%s at block %d not found; items: %v", k, where[k], describe(items))
			continue
		}
		if got := Kinds[it.Kind].Ext; got != k {
			t.Errorf("block %d: kind %s, want %s", where[k], got, k)
		}
		if it.Size != int64(len(files[k])) {
			t.Errorf("%s: size %d, want %d (flags %b)", k, it.Size, len(files[k]), it.Flags)
		}
		if k != "txt" && it.Flags&FlagExact == 0 {
			t.Errorf("%s: expected exact end, flags %b", k, it.Flags)
		}
	}
	if _, ok := byStart[int64(liveAt)*bs]; ok {
		t.Error("reported a file in allocated blocks")
	}
	part, ok := byStart[int64(partAt)*bs]
	if !ok || part.Flags&FlagTruncated == 0 || part.Size != 2*bs {
		t.Errorf("partly overwritten png = %+v, ok=%v; want truncated, 2 blocks", part, ok)
	}
}

// Real filesystems pack files back to back with no zero block between them.
// A container format must not swallow the files that follow it.
func TestPackedAfterContainer(t *testing.T) {
	files := testFiles(t)
	order := []string{"pdf", "jpg", "docx", "txt", "wav"}
	im := ext4test.Image{Blocks: 100, Data: map[int][]byte{}}
	where := map[string]int{}
	next := 5
	for _, k := range order {
		where[k] = next
		im.Data[next] = files[k]
		next += blocks(len(files[k]))
	}
	items := scanImage(t, im.Build(), im.Blocks)
	byStart := map[int64]Item{}
	for _, it := range items {
		byStart[it.Offset] = it
	}
	for _, k := range order {
		it, ok := byStart[int64(where[k])*bs]
		if !ok || Kinds[it.Kind].Ext != k || it.Size != int64(len(files[k])) {
			t.Errorf("%s: got %+v (ok=%v), want size %d; all: %s", k, it, ok, len(files[k]), describe(items))
		}
	}
}

func scanImage(t *testing.T, img []byte, nblocks int) []Item {
	t.Helper()
	fs, err := ext4.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	var items []Item
	s := &Scanner{R: bytes.NewReader(img), BlockSize: fs.BlockSize, TotalBlocks: uint64(nblocks), HeatCells: 10,
		OnItem: func(it Item) error { items = append(items, it); return nil }}
	err = fs.FreeExtents(context.Background(), func(e ext4.Extent) error {
		return s.Scan(context.Background(), Extent{e.Start, e.Count})
	})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func describe(items []Item) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString(Kinds[it.Kind].Ext)
		b.WriteString("@")
		b.WriteString(strconv.FormatInt(it.Offset/bs, 10))
		b.WriteString(" ")
	}
	return b.String()
}

func TestStreamersAcrossBlockSplits(t *testing.T) {
	files := testFiles(t)
	cases := map[string]func() streamer{
		"jpg":  func() streamer { return newJPEG() },
		"png":  newPNG,
		"gif":  func() streamer { return newGIF() },
		"docx": newZIP,
	}
	for k, mk := range cases {
		data := append(append([]byte{}, files[k]...), randomBytes(5000, 9)...)
		for _, chunk := range []int{1, 7, 512, 4096} {
			st := mk()
			var end int64 = -1
			for off := 0; off < len(data); off += chunk {
				e, s := st.feed(data[off:min(off+chunk, len(data))])
				if s == done {
					end = e
					break
				}
				if s == broken {
					t.Fatalf("%s chunk %d: broken at %d", k, chunk, e)
				}
			}
			if end != int64(len(files[k])) {
				t.Errorf("%s chunk %d: end %d, want %d", k, chunk, end, len(files[k]))
			}
		}
	}
}

func TestOggPageWalk(t *testing.T) {
	page := func(flags byte, payload int) []byte {
		p := []byte("OggS\x00")
		p = append(p, flags)
		p = append(p, make([]byte, 20)...) // granule, serial, seq, crc
		segs := []byte{}
		for n := payload; n > 0; n -= 255 {
			segs = append(segs, byte(min(n, 255)))
		}
		p = append(p, byte(len(segs)))
		p = append(p, segs...)
		return append(p, randomBytes(payload, 10)...)
	}
	file := append(append(page(0x02, 300), page(0, 1000)...), page(0x04, 50)...)
	data := append(append([]byte{}, file...), randomBytes(100, 11)...)
	st := newOgg()
	end, s := st.feed(data)
	if s != done || end != int64(len(file)) {
		t.Fatalf("ogg: end %d status %d, want %d done", end, s, len(file))
	}
}

// A minimal Compound File (Word 97) with 512-byte sectors: FAT in sector 0,
// directory in sector 1, data in sectors 2-9, so the file is 11 sectors.
func TestOLESize(t *testing.T) {
	f := make([]byte, 11*512)
	copy(f, "\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1")
	le := binary.LittleEndian
	le.PutUint16(f[30:], 9)          // sector shift
	le.PutUint32(f[44:], 1)          // one FAT sector
	le.PutUint32(f[48:], 1)          // directory starts at sector 1
	le.PutUint32(f[68:], 0xFFFFFFFE) // no DIFAT chain
	le.PutUint32(f[76:], 0)          // FAT is sector 0
	for i := 1; i < 109; i++ {
		le.PutUint32(f[76+4*i:], 0xFFFFFFFF)
	}
	fat := f[512:1024]
	for i := 0; i < 128; i++ {
		le.PutUint32(fat[4*i:], 0xFFFFFFFF) // free
	}
	le.PutUint32(fat[0:], 0xFFFFFFFD) // FAT sector
	le.PutUint32(fat[4:], 0xFFFFFFFE) // directory
	for s := 2; s < 9; s++ {
		le.PutUint32(fat[4*s:], uint32(s+1))
	}
	le.PutUint32(fat[4*9:], 0xFFFFFFFE)
	copy(f[1024:], []byte("W\x00o\x00r\x00d\x00D\x00o\x00c\x00u\x00m\x00e\x00n\x00t\x00"))

	img := ext4test.Image{Blocks: 20, Data: map[int][]byte{5: f, 7: f[:0]}}
	items := scanImage(t, img.Build(), 20)
	if len(items) != 1 || Kinds[items[0].Kind].Ext != "doc" || items[0].Size != int64(len(f)) || items[0].Flags&FlagExact == 0 {
		t.Fatalf("items = %+v (%s)", items, describe(items))
	}
}
