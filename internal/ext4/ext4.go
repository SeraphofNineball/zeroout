// Package ext4 reads just enough of an ext2/3/4 filesystem — superblock,
// group descriptors and block bitmaps — to enumerate its free blocks.
package ext4

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	superblockOffset = 1024
	magic            = 0xEF53

	compatSparseSuper2 = 0x200
	incompatMetaBG     = 0x10
	incompat64Bit      = 0x80
	roCompatSparse     = 0x1
	roCompatGDTCsum    = 0x10
	roCompatBigalloc   = 0x200
	roCompatMetaCsum   = 0x400

	bgBlockUninit = 0x2
)

// FS is an opened filesystem. All block numbers are filesystem block numbers;
// byte offsets are relative to the start of the filesystem.
type FS struct {
	r io.ReaderAt

	BlockSize      int64
	Blocks         uint64
	FirstDataBlock uint64
	BlocksPerGroup uint64
	Groups         uint64
	UUID           [16]byte

	descs       []desc
	uninitValid bool
	descSize    uint64
	perBlock    uint64
	tableLoc    []uint64 // block holding each block of descriptors
}

type desc struct {
	bitmap uint64
	free   uint64
	flags  uint16
}

// Probe reports whether an ext2/3/4 superblock starts at byte off of r.
func Probe(r io.ReaderAt, off int64) bool {
	var b [2]byte
	_, err := r.ReadAt(b[:], off+superblockOffset+0x38)
	return err == nil && binary.LittleEndian.Uint16(b[:]) == magic
}

// Open parses the superblock and group descriptors.
func Open(r io.ReaderAt) (*FS, error) {
	sb := make([]byte, 1024)
	if _, err := r.ReadAt(sb, superblockOffset); err != nil {
		return nil, fmt.Errorf("reading superblock: %w", err)
	}
	le := binary.LittleEndian
	if le.Uint16(sb[0x38:]) != magic {
		return nil, errors.New("no ext2/3/4 filesystem found")
	}
	compat, incompat, roCompat := le.Uint32(sb[0x5C:]), le.Uint32(sb[0x60:]), le.Uint32(sb[0x64:])
	if roCompat&roCompatBigalloc != 0 {
		return nil, errors.New("ext4 bigalloc filesystems are not supported")
	}
	logBS := le.Uint32(sb[0x18:])
	if logBS > 6 {
		return nil, fmt.Errorf("implausible block size 2^%d KiB", logBS)
	}
	fs := &FS{
		r:              r,
		BlockSize:      1024 << logBS,
		Blocks:         uint64(le.Uint32(sb[0x04:])),
		FirstDataBlock: uint64(le.Uint32(sb[0x14:])),
		BlocksPerGroup: uint64(le.Uint32(sb[0x20:])),
		uninitValid:    roCompat&(roCompatGDTCsum|roCompatMetaCsum) != 0,
	}
	copy(fs.UUID[:], sb[0x68:0x78])
	descSize := uint64(32)
	if incompat&incompat64Bit != 0 {
		fs.Blocks |= uint64(le.Uint32(sb[0x150:])) << 32
		if ds := uint64(le.Uint16(sb[0xFE:])); ds >= 64 {
			descSize = ds
		}
	}
	if fs.BlocksPerGroup == 0 || fs.BlocksPerGroup > uint64(fs.BlockSize)*8 || fs.Blocks <= fs.FirstDataBlock {
		return nil, errors.New("corrupt superblock")
	}
	fs.Groups = (fs.Blocks - fs.FirstDataBlock + fs.BlocksPerGroup - 1) / fs.BlocksPerGroup

	// Locate the descriptor table: contiguous after the superblock, or with
	// meta_bg, one block at the start of each meta group.
	perBlock := uint64(fs.BlockSize) / descSize
	tableBlocks := (fs.Groups + perBlock - 1) / perBlock
	firstMetaBG := uint64(tableBlocks)
	if incompat&incompatMetaBG != 0 {
		firstMetaBG = uint64(le.Uint32(sb[0x104:]))
	}
	backups := [2]uint64{uint64(le.Uint32(sb[0x24C:])), uint64(le.Uint32(sb[0x250:]))}
	hasSuper := func(g uint64) bool {
		switch {
		case g == 0:
			return true
		case compat&compatSparseSuper2 != 0:
			return g == backups[0] || g == backups[1]
		case roCompat&roCompatSparse == 0 || g == 1:
			return true
		}
		return isPow(g, 3) || isPow(g, 5) || isPow(g, 7)
	}

	fs.descSize, fs.perBlock = descSize, perBlock
	fs.tableLoc = make([]uint64, tableBlocks)
	raw := make([]byte, tableBlocks*uint64(fs.BlockSize))
	normal := min(firstMetaBG, tableBlocks)
	for i := uint64(0); i < normal; i++ {
		fs.tableLoc[i] = fs.FirstDataBlock + 1 + i
	}
	if normal > 0 {
		at := int64(fs.FirstDataBlock+1) * fs.BlockSize
		if _, err := r.ReadAt(raw[:normal*uint64(fs.BlockSize)], at); err != nil {
			return nil, fmt.Errorf("reading group descriptors: %w", err)
		}
	}
	for mg := normal; mg < tableBlocks; mg++ {
		g := mg * perBlock
		blk := fs.FirstDataBlock + g*fs.BlocksPerGroup
		if hasSuper(g) {
			blk++
		}
		fs.tableLoc[mg] = blk
		dst := raw[mg*uint64(fs.BlockSize) : (mg+1)*uint64(fs.BlockSize)]
		if _, err := r.ReadAt(dst, int64(blk)*fs.BlockSize); err != nil {
			return nil, fmt.Errorf("reading meta_bg descriptors: %w", err)
		}
	}

	fs.descs = make([]desc, fs.Groups)
	for g := range fs.descs {
		fs.descs[g] = fs.parseDesc(raw[uint64(g)*descSize:])
		if fs.descs[g].bitmap >= fs.Blocks && !fs.uninit(uint64(g)) {
			return nil, fmt.Errorf("group %d: block bitmap %d is outside the filesystem", g, fs.descs[g].bitmap)
		}
	}
	return fs, nil
}

func (fs *FS) parseDesc(d []byte) desc {
	le := binary.LittleEndian
	r := desc{
		bitmap: uint64(le.Uint32(d[0x00:])),
		free:   uint64(le.Uint16(d[0x0C:])),
		flags:  le.Uint16(d[0x12:]),
	}
	if fs.descSize >= 64 {
		r.bitmap |= uint64(le.Uint32(d[0x20:])) << 32
		r.free |= uint64(le.Uint16(d[0x2C:])) << 16
	}
	return r
}

// readDesc reads group g's descriptor fresh from disk.
func (fs *FS) readDesc(g uint64) (desc, error) {
	b := make([]byte, fs.descSize)
	off := int64(fs.tableLoc[g/fs.perBlock])*fs.BlockSize + int64(g%fs.perBlock*fs.descSize)
	if _, err := fs.r.ReadAt(b, off); err != nil {
		return desc{}, err
	}
	return fs.parseDesc(b), nil
}

func isPow(n, base uint64) bool {
	for n > 1 && n%base == 0 {
		n /= base
	}
	return n == 1
}

// Size is the filesystem size in bytes.
func (fs *FS) Size() int64 { return int64(fs.Blocks) * fs.BlockSize }

func (fs *FS) uninit(g uint64) bool {
	return fs.uninitValid && fs.descs[g].flags&bgBlockUninit != 0
}

func (fs *FS) groupStart(g uint64) uint64 { return fs.FirstDataBlock + g*fs.BlocksPerGroup }

func (fs *FS) groupLen(g uint64) uint64 {
	return min(fs.BlocksPerGroup, fs.Blocks-fs.groupStart(g))
}

// FreeCounts returns the free block totals from the group descriptors.
// uninit counts free blocks in groups whose bitmap was never initialised —
// space that has never held data since the filesystem was created.
func (fs *FS) FreeCounts() (free, uninit uint64) {
	for g, d := range fs.descs {
		free += d.free
		if fs.uninit(uint64(g)) {
			uninit += d.free
		}
	}
	return free, uninit
}

// Extent is a run of consecutive blocks.
type Extent struct {
	Start, Count uint64
}

// FreeExtents calls fn for every maximal run of free blocks in initialised
// groups, in ascending order. Runs spanning group boundaries are merged.
func (fs *FS) FreeExtents(ctx context.Context, fn func(Extent) error) error {
	var run Extent
	flush := func() error {
		if run.Count == 0 {
			return nil
		}
		e := run
		run = Extent{}
		return fn(e)
	}
	bm := &bitmapReader{fs: fs}
	for g := uint64(0); g < fs.Groups; g++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fs.uninit(g) {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		bits, err := bm.get(g)
		if err != nil {
			return err
		}
		base, n := fs.groupStart(g), fs.groupLen(g)
		for i := uint64(0); i < n; {
			if i%8 == 0 && i+8 <= n {
				switch bits[i/8] {
				case 0x00:
					if run.Count == 0 {
						run.Start = base + i
					}
					run.Count += 8
					i += 8
					continue
				case 0xFF:
					if err := flush(); err != nil {
						return err
					}
					i += 8
					continue
				}
			}
			if bits[i/8]&(1<<(i%8)) == 0 {
				if run.Count == 0 {
					run.Start = base + i
				}
				run.Count++
			} else if err := flush(); err != nil {
				return err
			}
			i++
		}
	}
	return flush()
}

// IsFree re-reads the on-disk descriptors and bitmaps and reports whether
// every block in [start, start+count) is still free.
func (fs *FS) IsFree(start, count uint64) (bool, error) {
	if start < fs.FirstDataBlock || start+count > fs.Blocks {
		return false, nil
	}
	bs := uint64(fs.BlockSize)
	bits := make([]byte, bs)
	for b := start; b < start+count; {
		g := (b - fs.FirstDataBlock) / fs.BlocksPerGroup
		end := min(start+count, fs.groupStart(g)+fs.groupLen(g))
		d, err := fs.readDesc(g)
		if err != nil {
			return false, err
		}
		if !(fs.uninitValid && d.flags&bgBlockUninit != 0) {
			if _, err := fs.r.ReadAt(bits, int64(d.bitmap*bs)); err != nil {
				return false, err
			}
			for i := b - fs.groupStart(g); i < end-fs.groupStart(g); i++ {
				if bits[i/8]&(1<<(i%8)) != 0 {
					return false, nil
				}
			}
		}
		b = end
	}
	return true, nil
}

// bitmapReader reads block bitmaps, coalescing runs of adjacent bitmap blocks
// (flex_bg places them together) into single reads.
type bitmapReader struct {
	fs    *FS
	batch int
	first uint64
	buf   []byte
	n     uint64
}

func (b *bitmapReader) get(g uint64) ([]byte, error) {
	bs := uint64(b.fs.BlockSize)
	if g >= b.first && g < b.first+b.n {
		return b.buf[(g-b.first)*bs : (g-b.first+1)*bs], nil
	}
	limit := uint64(256)
	if b.batch > 0 {
		limit = uint64(b.batch)
	}
	start := b.fs.descs[g].bitmap
	n := uint64(1)
	for g+n < b.fs.Groups && n < limit && !b.fs.uninit(g+n) && b.fs.descs[g+n].bitmap == start+n {
		n++
	}
	if uint64(len(b.buf)) < n*bs {
		b.buf = make([]byte, n*bs)
	}
	if _, err := b.fs.r.ReadAt(b.buf[:n*bs], int64(start*bs)); err != nil {
		return nil, fmt.Errorf("reading bitmap of group %d: %w", g, err)
	}
	b.first, b.n = g, n
	return b.buf[:bs], nil
}
