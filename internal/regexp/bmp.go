package regexp

import (
	"sync"
	"unsafe"
)

// A class of many ranges -- \p{L} has hundreds -- is a binary search over
// them at every character it is asked about, which on text past ASCII is
// most of what matching it costs. Such a class answers for the Basic
// Multilingual Plane from a table instead: a bit for each code point, in
// blocks of 256, each distinct block kept once, so that a property whose
// code points come in long runs, as most do, takes a few kilobytes. The
// table is made the first time the class is asked about a code point it
// answers for, and a class that is never asked makes none; once made it is
// never changed, and so is shared by every goroutine matching the pattern
// at once, as two that made it at the same moment each use their own.

// bmpMinRanges is the fewest ranges a class has before it answers for the
// Basic Multilingual Plane from a table: below it, the search is a few
// comparisons.
const bmpMinRanges = 16

// bmpTable is which code points below U+10000 a class's ranges hold:
// bit c&63 of word (c>>6)&3 of blocks[index[c>>8]]. There are 256 blocks
// at most, one for each index, so an index fits a byte.
type bmpTable struct {
	index  [256]uint8
	blocks [][4]uint64
}

// has reports whether the table holds c, which is below U+10000.
func (t *bmpTable) has(c rune) bool {
	return t.blocks[t.index[c>>8]][(c>>6)&3]&(1<<(c&63)) != 0
}

// bmpKey names a shared property table's ranges, whose table every class
// taking them whole shares (propertyBMP).
type bmpKey struct {
	ranges *charRange
	n      int
}

// propertyBMP holds the tables of the shared property tables' ranges, made
// as patterns first need them: a pattern compiled again, or by another
// runtime, finds its table made. Like the ranges, they are the package's.
var propertyBMP sync.Map // bmpKey -> *bmpTable

// bmpOf is the set's table, made if it has none yet.
func (s *charSet) bmpOf() *bmpTable {
	if t := s.bmp.Load(); t != nil {
		return t
	}
	shared := len(s.ranges) > 0 && unsafe.SliceData(s.ranges) == s.table
	var key bmpKey
	if shared {
		key = bmpKey{s.table, len(s.ranges)}
		if t, ok := propertyBMP.Load(key); ok {
			s.bmp.Store(t.(*bmpTable))
			return t.(*bmpTable)
		}
	}
	t := makeBMPTable(s.ranges)
	if shared {
		if was, loaded := propertyBMP.LoadOrStore(key, t); loaded {
			t = was.(*bmpTable)
		}
	}
	s.bmp.Store(t)
	return t
}

// makeBMPTable is the table of the code points below U+10000 that ranges
// hold.
func makeBMPTable(ranges []charRange) *bmpTable {
	var bits [1 << 10]uint64
	for _, rg := range ranges {
		lo, hi := max(rg.lo, 0), min(rg.hi, 0xFFFF)
		for c := lo; c <= hi; {
			w := c >> 6
			// The bits of word w from c to hi, or to the word's end.
			end := min(hi, w<<6|63)
			n := uint(end - c + 1)
			mask := ^uint64(0)
			if n < 64 {
				mask = (1<<n - 1)
			}
			bits[w] |= mask << uint(c&63)
			c = end + 1
		}
	}
	t := &bmpTable{}
	seen := make(map[[4]uint64]uint8)
	for b := 0; b < 256; b++ {
		blk := [4]uint64(bits[4*b : 4*b+4])
		i, ok := seen[blk]
		if !ok {
			i = uint8(len(t.blocks))
			seen[blk] = i
			t.blocks = append(t.blocks, blk)
		}
		t.index[b] = i
	}
	t.blocks = t.blocks[:len(t.blocks):len(t.blocks)]
	return t
}

// bmpFootprint is what the set's table takes, where it is the set's own.
func (s *charSet) bmpFootprint() int {
	t := s.bmp.Load()
	if t == nil || len(s.ranges) > 0 && unsafe.SliceData(s.ranges) == s.table {
		return 0
	}
	return int(unsafe.Sizeof(*t)) + cap(t.blocks)*int(unsafe.Sizeof([4]uint64{}))
}
