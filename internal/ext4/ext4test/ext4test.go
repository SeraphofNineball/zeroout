// Package ext4test builds tiny synthetic ext4 images for tests.
package ext4test

import "encoding/binary"

const BlockSize = 4096

// Image describes a one-group filesystem with 4 KiB blocks. Blocks 0-2 hold
// the superblock, descriptor table and bitmap and are always in use.
type Image struct {
	Blocks int
	Used   []int          // additional allocated blocks
	Data   map[int][]byte // contents written starting at a block
}

// Build returns the raw image bytes.
func (im Image) Build() []byte {
	img := make([]byte, im.Blocks*BlockSize)
	le := binary.LittleEndian
	sb := img[1024:]
	le.PutUint32(sb[0x04:], uint32(im.Blocks))
	le.PutUint32(sb[0x14:], 0)     // first data block
	le.PutUint32(sb[0x18:], 2)     // 4 KiB blocks
	le.PutUint32(sb[0x20:], 32768) // blocks per group
	le.PutUint16(sb[0x38:], 0xEF53)
	copy(sb[0x68:], "zeroout-test-fs!")

	gd := img[BlockSize:]
	le.PutUint32(gd[0x00:], 2) // block bitmap

	bitmap := img[2*BlockSize : 3*BlockSize]
	used := append([]int{0, 1, 2}, im.Used...)
	for _, b := range used {
		bitmap[b/8] |= 1 << (b % 8)
	}
	for b := im.Blocks; b < BlockSize*8; b++ {
		bitmap[b/8] |= 1 << (b % 8) // padding past the end
	}
	free := im.Blocks - len(used)
	le.PutUint16(gd[0x0C:], uint16(free))
	le.PutUint32(sb[0x0C:], uint32(free))

	for blk, data := range im.Data {
		copy(img[blk*BlockSize:], data)
	}
	return img
}
