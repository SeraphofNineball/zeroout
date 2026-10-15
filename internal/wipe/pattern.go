package wipe

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
)

// Pattern is the data written during one pass.
type Pattern struct {
	name   string
	fill   byte
	random bool
}

var (
	Zero   = Pattern{name: "zero"}
	One    = Pattern{name: "one", fill: 0xFF}
	Random = Pattern{name: "random", random: true}
)

// ParsePattern accepts "zero", "one", "random" or a byte value like "0xAA".
func ParsePattern(s string) (Pattern, error) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "zero", "zeros", "0x00":
		return Zero, nil
	case "one", "ones", "0xff":
		return One, nil
	case "random":
		return Random, nil
	default:
		if hex, ok := strings.CutPrefix(v, "0x"); ok && len(hex) == 2 {
			if b, err := strconv.ParseUint(hex, 16, 8); err == nil {
				return Pattern{name: fmt.Sprintf("0x%02X", b), fill: byte(b)}, nil
			}
		}
	}
	return Pattern{}, fmt.Errorf("unknown pattern %q (use zero, one, random or 0xNN)", s)
}

func (p Pattern) String() string { return p.name }

func (p Pattern) MarshalText() ([]byte, error) { return []byte(p.name), nil }

func (p *Pattern) UnmarshalText(b []byte) error {
	v, err := ParsePattern(string(b))
	if err == nil {
		*p = v
	}
	return err
}

// randomUnit is the granularity of the random stream. Each unit is generated
// from its own seed, so any byte range can be regenerated for verification
// without replaying everything before it.
const randomUnit = 1 << 20

// source produces the bytes for one fill file.
type source struct {
	p       Pattern
	seed    [32]byte
	scratch []byte
}

func newSource(p Pattern, passSeed [32]byte, chunk int) *source {
	s := &source{p: p, seed: passSeed}
	binary.LittleEndian.PutUint32(s.seed[28:], uint32(chunk))
	return s
}

// constant reports whether every byte of the stream is the same, in which
// case a buffer only has to be filled once.
func (s *source) constant() bool { return !s.p.random }

// fillAt writes the stream bytes for file offset off into buf.
func (s *source) fillAt(buf []byte, off int64) {
	if !s.p.random {
		if s.p.fill == 0 {
			clear(buf)
		} else {
			for i := range buf {
				buf[i] = s.p.fill
			}
		}
		return
	}
	for len(buf) > 0 {
		in := int(off % randomUnit)
		n := min(randomUnit-in, len(buf))
		seed := s.seed
		binary.LittleEndian.PutUint64(seed[16:24], uint64(off/randomUnit))
		r := rand.NewChaCha8(seed)
		if in == 0 {
			r.Read(buf[:n])
		} else {
			if s.scratch == nil {
				s.scratch = make([]byte, randomUnit)
			}
			r.Read(s.scratch[:in+n])
			copy(buf, s.scratch[in:in+n])
		}
		buf = buf[n:]
		off += int64(n)
	}
}
