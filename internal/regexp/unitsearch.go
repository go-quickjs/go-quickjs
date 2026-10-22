package regexp

import (
	"encoding/binary"
	"unsafe"
)

// A subject past ASCII is matched by its code units, which Go has no
// bytes.IndexByte for: a search for a literal's first unit, or for the one
// unit a match can begin with, went a unit at a time. indexUnit reads four
// units at a time instead, as a word, and asks whether any of them is the
// one sought -- the test for a zero lane of the word xored with it, which
// only ever errs towards yes, the lanes then read one by one -- and so
// passes over text that does not have it four units for each step. The
// word is read in the machine's own order, in which its lanes are the
// units.

// laneOnes and laneHighs are a 1 and the high bit of each 16-bit lane of
// a word.
const (
	laneOnes  = 0x0001000100010001
	laneHighs = 0x8000800080008000
)

// indexUnit is the first position of u in s from pos on, or len(s).
func indexUnit(s []uint16, pos int, u uint16) int {
	if pos < len(s) && s[pos] == u {
		return pos
	}
	if n := len(s) - pos; n >= 8 {
		b := unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(s))), 2*len(s))
		want := uint64(u) * laneOnes
		for ; pos+4 <= len(s); pos += 4 {
			w := binary.NativeEndian.Uint64(b[2*pos:]) ^ want
			if (w-laneOnes)&^w&laneHighs != 0 {
				break
			}
		}
	}
	for ; pos < len(s); pos++ {
		if s[pos] == u {
			return pos
		}
	}
	return len(s)
}
