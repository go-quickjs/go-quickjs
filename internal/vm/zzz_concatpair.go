package vm

import (
	"encoding/binary"
	"unsafe"
)

// The short concatenations' memo.
//
// A render builds the same class names, attributes, keys and rows from the
// same pieces at every pass, each a new string: a template's row of a few
// short pieces had cost an allocation at each +=, and the collector most of
// the render's time. A concatenation of two flat ASCII strings of fewer
// than ropeThreshold bytes in all looks in one set of pairMemoWays
// results, chosen by a hash of its bytes, for one with those bytes, and
// returns it: one that is there allocates nothing. A tag of 16 more bits
// of the hash spares it the comparison of its bytes with the results it
// cannot be. A new result goes in the way the count of misses picks.
//
// Before that, the last two results are checked by their pieces: a run of
// the same concatenations has the same pieces, which a hit compares by
// address and not by bytes. A piece whose bytes are its own allocation's
// (ownBytes) is kept with the result, as a String's bytes never change but
// a builder's (building), which never comes here; of another, a constant's
// or a slice of a longer text, which keeping would keep the whole of, only
// the address of its bytes is kept, and a hit compares the bytes too, as
// another string may have come to have its address.
//
// The memo holds only results it made, flat strings of their own bytes, at
// most 112 bytes each, which no append grows in place: strAppend marks only
// the ropes it makes, never a flat string, and so never one of these. It is
// the runtime's, made after pairMemoAfter short concatenations, so that a
// runtime that makes a few pays nothing; a runtime with a memory limit has
// none, as each concatenation there is charged its bytes. A runtime whose
// concatenations seldom repeat skips it (memoGate), and its check of the
// recent results the same way: a miss costs more than the concatenation it
// also makes.
type pairMemo struct {
	tags [pairMemoSets][pairMemoWays]uint16
	strs [pairMemoSets][pairMemoWays]*String
	// recent is the last two results returned, and recentNext's parity the
	// one to replace.
	recent     [2]pairRecent
	recentNext uint32
	gate       memoGate
	recentGate memoGate
}

// pairRecent is one of the memo's last two results and its pieces: a and
// b where they own their bytes, or pa and pb, their bytes' addresses.
type pairRecent struct {
	a, b, s *String
	pa, pb  uintptr
}

const (
	pairMemoBits  = 7
	pairMemoSets  = 1 << pairMemoBits
	pairMemoWays  = 4
	pairMemoAfter = 1024
)

// memoGate skips a memo that seldom hits: every memoWindow misses it
// checks the hits since the last check, and when they are fewer than one
// lookup in memoRate, the next memoSkip operations do not look in the memo.
type memoGate struct {
	hits, misses, skip uint32
}

const (
	memoWindow = 1024
	memoRate   = 8
	memoSkip   = 1 << 16
)

// miss counts a miss, ending the window after memoWindow of them.
func (g *memoGate) miss() {
	if g.misses++; g.misses == memoWindow {
		g.check()
	}
}

// check ends a window of misses.
//
//go:noinline
func (g *memoGate) check() {
	if uint64(g.hits)*(memoRate-1) < uint64(g.misses) {
		g.skip = memoSkip
	}
	g.hits, g.misses = 0, 0
}

// concatShort is a.Concat(b) for a and b whose bytes are fewer than
// ropeThreshold in all, through the memo where it applies: both flat
// ASCII, neither empty nor a builder's, and no memory limit.
func (r *Runtime) concatShort(a, b *String) *String {
	if a.ascii && b.ascii && a.length != 0 && b.length != 0 && !a.building && !b.building && r.meter == nil {
		if m := r.concats; m != nil {
			if m.gate.skip == 0 {
				return m.concat(a, b)
			}
			m.gate.skip--
		} else if r.concatCount < pairMemoAfter {
			r.concatCount++
		} else {
			r.concats = new(pairMemo)
			return r.concats.concat(a, b)
		}
	}
	return a.Concat(b)
}

// concat is a.Concat(b) through the memo: a and b flat ASCII, neither
// empty, fewer than ropeThreshold bytes in all.
func (m *pairMemo) concat(a, b *String) *String {
	n := len(a.s) + len(b.s)
	if g := &m.recentGate; g.skip == 0 {
		pa, pb := bytesAddr(a), bytesAddr(b)
		for i := range m.recent {
			c := &m.recent[i]
			if c.a == a && c.b == b && a != nil ||
				c.pa == pa && c.pb == pb && c.a == nil && len(c.s.s) == n && c.s.s[:len(a.s)] == a.s && c.s.s[len(a.s):] == b.s {
				g.hits++
				m.gate.hits++
				return c.s
			}
		}
		g.miss()
	} else {
		g.skip--
	}
	var t [ropeThreshold]byte
	copy(t[copy(t[:], a.s):], b.s)
	h := concatHash(&t, n)
	set, tag := h>>(64-pairMemoBits), uint16(h>>16)
	for i, s := range &m.strs[set] {
		if m.tags[set][i] == tag && s != nil && s.s == string(t[:n]) {
			m.gate.hits++
			m.keepRecent(a, b, s)
			return s
		}
	}
	s, room := newStringBytes(n)
	copy(room, t[:n])
	s.s = unsafe.String(unsafe.SliceData(room), n)
	s.length, s.ascii = n, true
	i := m.gate.misses % pairMemoWays
	m.tags[set][i], m.strs[set][i] = tag, s
	m.gate.miss()
	m.keepRecent(a, b, s)
	return s
}

// keepRecent makes s, the result of a and b, a recent result, in place of
// the one kept before the last.
func (m *pairMemo) keepRecent(a, b, s *String) {
	if m.recentGate.skip != 0 {
		return
	}
	c := &m.recent[m.recentNext&1]
	m.recentNext++
	if ownBytes(a) && ownBytes(b) {
		*c = pairRecent{a: a, b: b, s: s}
		return
	}
	*c = pairRecent{s: s, pa: bytesAddr(a), pb: bytesAddr(b)}
}

// ownBytes reports whether a flat string's bytes are in its own allocation,
// right after it, as newStringBytes makes them: keeping it keeps nothing
// else.
func ownBytes(s *String) bool {
	return bytesAddr(s) == uintptr(unsafe.Pointer(s))+unsafe.Sizeof(String{})
}

// bytesAddr is the address of a flat string's bytes, which keeps nothing
// alive.
func bytesAddr(s *String) uintptr {
	return uintptr(unsafe.Pointer(unsafe.StringData(s.s)))
}

// concatHash hashes the n bytes in t, the rest zero, eight at a time, seeded
// per process: its top bits pick the memo's set, bits 16 to 31 the tag.
// Strings that collide only make the memo miss, so the hash need only
// spread a render's strings, not resist an input chosen to collide.
func concatHash(t *[ropeThreshold]byte, n int) uint64 {
	h := mapSalt ^ uint64(n)
	for i := 0; i < n; i += 8 {
		// i&(ropeThreshold-8) is i, which the compiler cannot see: one
		// load of eight bytes, with no check of the bounds.
		h = (h ^ binary.LittleEndian.Uint64(t[i&(ropeThreshold-8):])) * 0x9e3779b97f4a7c15
	}
	h ^= h >> 29
	h *= 0xbf58476d1ce4e5b9
	return h ^ h>>32
}
