// Package aligned allocates buffers suitable for O_DIRECT I/O.
package aligned

import "unsafe"

// Align is the alignment O_DIRECT needs for buffers, offsets and lengths.
const Align = 4096

// Buffer returns a size-byte buffer whose start is Align-aligned.
func Buffer(size int) []byte {
	b := make([]byte, size+Align)
	off := 0
	if rem := int(uintptr(unsafe.Pointer(&b[0])) & (Align - 1)); rem != 0 {
		off = Align - rem
	}
	return b[off : off+size : off+size]
}

// AddrOf returns the address of b's first byte.
func AddrOf(b []byte) uintptr { return uintptr(unsafe.Pointer(&b[0])) }
