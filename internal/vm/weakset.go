package vm

import "unsafe"

// WeakSet.
//
// A WeakSet holds nothing but whether each object or symbol is in it, and
// must not keep any of them alive. So it is a list of weak references to its
// members with a hash index over it, by address: a member is found by the
// address it has, and is in the set while the weak reference still names it.
// Unlike a WeakMap (weakmap.go) it has no values, so there is no ephemeron
// to make and no cleanup to run; a member the collector has taken is a dead
// entry, dropped by the next rebuild.
//
// It is a Map's index without what a WeakSet does not do: it is never
// iterated, so it needs no insertion order to keep and no cursor to repair.

// weakSetSlot is a slot of a WeakSet's index: a member's hash and its place
// in keys, plus one, so that zero is an empty slot.
type weakSetSlot struct {
	hash uint32
	at   int32
}

// weakSet is a WeakSet's internal slot.
type weakSet struct {
	// keys holds a weak reference to each member, in the order they were
	// added. A deleted member's is zero, and a collected one's names
	// nothing; either keeps its place, and its index slot, until a rebuild.
	keys []weakTarget
	// index is open addressing with linear probing over a power-of-two table
	// at most half full, as a Map's is.
	index []weakSetSlot
	// size counts the entries not deleted, collected ones among them until
	// the rebuild that finds them gone.
	size int
}

func newWeakSet() *weakSet { return &weakSet{} }

// weakHash is the hash of a member, by its address: the one thing about it a
// weak reference can be compared on. The collector does not move objects, so
// the address is the member's for as long as it lives.
func weakHash(p unsafe.Pointer) uint32 {
	return mixHash(uint64(uintptr(p)))
}

// find is where the member v, whose hash is h, is in keys, or -1. An address
// outlives its object and can come back as another's: an entry counts only
// while its weak reference still names v, and a collected member's names
// nothing, so a new object at its address does not find its entry.
func (s *weakSet) find(v Value, h uint32) int {
	if len(s.index) == 0 {
		return -1
	}
	mask := uint32(len(s.index) - 1)
	for i := h & mask; ; i = (i + 1) & mask {
		slot := s.index[i]
		if slot.at == 0 {
			return -1
		}
		if slot.hash == h {
			j := int(slot.at - 1)
			if s.keys[j].pointer() == v.ref {
				return j
			}
		}
	}
}

// place puts entry j, whose hash is h, in the first free slot from its own.
func (s *weakSet) place(h uint32, j int) {
	mask := uint32(len(s.index) - 1)
	i := h & mask
	for s.index[i].at != 0 {
		i = (i + 1) & mask
	}
	s.index[i] = weakSetSlot{hash: h, at: int32(j + 1)}
}

// rebuild drops the deleted and collected entries and makes the index again
// with room for twice what is left. It runs when the index is half full or
// the entries are half deleted, so it costs a constant per member added.
func (s *weakSet) rebuild() {
	kept := 0
	for _, k := range s.keys {
		if k.pointer() != nil {
			s.keys[kept] = k
			kept++
		}
	}
	clear(s.keys[kept:])
	s.keys = s.keys[:kept]
	// A list that has emptied gives its room back.
	if c := cap(s.keys); c > 64 && c > 4*kept {
		s.keys = append([]weakTarget(nil), s.keys...)
	}
	s.size = kept
	n := 8
	for n < 3*(kept+1) {
		n <<= 1
	}
	if n == len(s.index) {
		clear(s.index)
	} else {
		s.index = make([]weakSetSlot, n)
	}
	for j, k := range s.keys {
		// One collected since it was kept above gets no slot: it is found
		// by nothing, and the next rebuild drops it.
		if p := k.pointer(); p != nil {
			s.place(weakHash(p), j)
		}
	}
}

// add adds v, which the caller has checked can be held weakly.
func (s *weakSet) add(v Value) {
	h := weakHash(v.ref)
	if s.find(v, h) < 0 {
		if 2*(len(s.keys)+1) > len(s.index) {
			s.rebuild()
		}
		j := len(s.keys)
		s.keys = append(s.keys, makeWeak(v))
		s.place(h, j)
		s.size++
	}
}

// has reports whether v, which may be anything, is a member.
func (s *weakSet) has(v Value) bool {
	if !v.IsObject() && !v.IsSymbol() {
		return false
	}
	return s.find(v, weakHash(v.ref)) >= 0
}

// delete removes v, which may be anything, and reports whether it was a
// member.
func (s *weakSet) delete(v Value) bool {
	if !v.IsObject() && !v.IsSymbol() {
		return false
	}
	j := s.find(v, weakHash(v.ref))
	if j < 0 {
		return false
	}
	s.keys[j] = weakTarget{}
	s.size--
	if dead := len(s.keys) - s.size; dead >= 16 && dead > s.size {
		s.rebuild()
	}
	return true
}
