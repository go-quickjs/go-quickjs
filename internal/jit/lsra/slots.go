package lsra

import (
	"math"
	"math/bits"
)

// The slots' scan gives each web that needs a slot, in the order of the
// webs' starts, the lowest slot from which as many as it is wide hold
// nothing where it needs them: whose lists of ranges do not meet its.
// Testing each slot from the first in turn, a web is tested against every
// slot of the webs in slots where it begins, which makes the scan
// quadratic in the slots a function needs at once. Past slotTreeMin
// slots, a slot that certainly meets the web is passed over without a
// test, by what is known of each at the scan's position, the furthest any
// web's needs have begun: the first of its ranges that ends past it, its
// key the range's start. A web whose needs begin at the position, or
// cover it, meets every slot whose key lies before the end of its range
// there; the slots' keys are in a tree of their maxima, which finds the
// first slot at or past another whose key is at least that end. The
// slots the tree finds are tested as before, and the answer is the same:
// only slots that would fail are passed over. A web whose needs begin
// past the position moves it; one whose ranges lie wholly before it, or
// that has none over it, is tested slot by slot.
//
// Where the position passes the end of the range that gives a slot its
// key, the key moves to the slot's next range: the ends are kept by
// position, a list of them at each position and a bit for each position
// that has any, which the position's moves sweep. Every end kept lies past
// the position where it is kept, and each slot's key, once the position
// has passed what it may, is that of its first range that ends past the
// position, whatever the order its ends are met in.

// slotTreeMin is the number of slots past which the slots' scan keeps
// their keys in a tree: below it, a test of each in turn costs less.
const slotTreeMin = 32

// slotsMode is how the slots' scan finds a web's first slot, for tests:
// past slotTreeMin slots by the keys (slotsAuto), each slot tested in turn
// (slotsLinear), or by the keys from the first (slotsTree).
type slotsMode int8

const (
	slotsAuto slotsMode = iota
	slotsLinear
	slotsTree
)

// noKey is the key of a slot that holds nothing past the scan's position.
const noKey = math.MaxInt32

// slotScan is the slots' scan's state past slotTreeMin slots.
type slotScan struct {
	// on says the keys are kept, from min slots; p is the scan's position.
	on  bool
	min int
	p   int32
	// n is the tree's leaves, a power of two past the slots and the widest
	// web's; key and cur are each slot's key and the index of its range
	// that is, and keys the tree, keys[n+s] slot s's, keys[i] the larger
	// of keys[2i] and keys[2i+1].
	n    int
	key  []int32
	cur  []int32
	keys []int32
	// The ends of the slots' ranges that give their keys, each with its
	// slot: where the position passes one, the slot's key moves to its
	// next range. A slot's earlier ones may be left, and are passed over.
	// By position, at[x] is plus one the index in byPos of the last kept
	// at x, each's next the one kept before it there, and full has a bit
	// for each position that has any; swept is the position up to which
	// they have been met, and npos the positions' number. With
	// Config.slotHeap, they are a heap instead, ends, the first first
	// (heap).
	npos  int32
	at    []int32
	byPos []slotAt
	full  []uint64
	swept int32
	heap  bool
	ends  []slotEnd
}

type slotEnd struct{ to, s int32 }

// slotAt is an end kept at its position: its slot, and plus one the index
// of the one kept before it there, or 0.
type slotAt struct{ s, next int32 }

// reset readies the scan for an allocation's slots, whose ranges end by
// maxPos; heap is Config.slotHeap.
func (sc *slotScan) reset(mode slotsMode, heap bool, maxPos int32) {
	sc.on, sc.p, sc.ends, sc.heap, sc.npos = false, -1, sc.ends[:0], heap, maxPos+1
	switch mode {
	case slotsLinear:
		sc.min = math.MaxInt
	case slotsTree:
		sc.min = 0
	default:
		sc.min = slotTreeMin
	}
}

// first is the first slot from which width slots of slots meet none of
// rs, as a test of each from the first finds it.
func (sc *slotScan) first(slots [][]rng, rs []rng, width int32) int32 {
	if len(rs) > 0 && rs[0].from > sc.p {
		sc.advance(slots, rs[0].from)
	}
	if !sc.on {
		if len(slots) < sc.min {
			return firstFree(slots, rs, width, 0)
		}
		sc.start(slots)
	}
	// The slots this web may add have keys too.
	if len(slots)+int(width) >= sc.n {
		sc.grow(len(slots) + int(width))
	}
	// k is the first of rs's ranges that ends past the position: one that
	// does not cover it, or none, leaves the keys nothing to tell.
	k := endsAfter(rs, 0, sc.p)
	if k == len(rs) || rs[k].from > sc.p {
		return firstFree(slots, rs, width, 0)
	}
	x := rs[k].to
	// From the position on, a slot meets rs where its key lies before x;
	// where rs begins at the position, its other ranges are all that may
	// meet the slot's from its key on, and nothing of the slot's before.
	from := rs[0].from == sc.p
	fits := func(t int) bool {
		switch {
		case t >= len(slots):
			return true
		case sc.key[t] < x:
			return false
		case from:
			return len(rs) == 1 || !overlap(slots[t][sc.cur[t]:], rs[1:])
		}
		return !overlap(slots[t], rs)
	}
	lo := 0
	for {
		s := sc.find(lo, x)
		k := 0
		for k < int(width) && fits(s+k) {
			k++
		}
		if k == int(width) {
			return int32(s)
		}
		lo = s + k + 1
	}
}

// firstFree is the first slot from lo from which width slots meet none of
// rs, each tested in turn.
func firstFree(slots [][]rng, rs []rng, width, lo int32) int32 {
	s := lo
	for k := int32(0); k < width; {
		if int(s+k) < len(slots) && overlap(slots[s+k], rs) {
			s, k = s+k+1, 0
			continue
		}
		k++
	}
	return s
}

// taken updates slot s's key after a web's ranges are added to it.
func (sc *slotScan) taken(slots [][]rng, s int32) {
	if !sc.on {
		return
	}
	sc.cur[s] = int32(endsAfter(slots[s], 0, sc.p))
	sc.setKey(slots, s)
}

// setKey sets slot s's key from its range cur, and notes where the range
// ends.
func (sc *slotScan) setKey(slots [][]rng, s int32) {
	k := int32(noKey)
	if c := sc.cur[s]; int(c) < len(slots[s]) {
		r := slots[s][c]
		k = r.from
		sc.push(slotEnd{r.to, s})
	}
	sc.key[s] = k
	i := sc.n + int(s)
	sc.keys[i] = k
	for i > 1 {
		i >>= 1
		m := max(sc.keys[2*i], sc.keys[2*i+1])
		if sc.keys[i] == m {
			break
		}
		sc.keys[i] = m
	}
}

// start keeps the keys of slots, from here on.
func (sc *slotScan) start(slots [][]rng) {
	sc.on = true
	sc.n = 0
	if !sc.heap {
		sc.at = grown(sc.at, int(sc.npos))
		clear(sc.at)
		sc.full = grown(sc.full, int(sc.npos+63)/64)
		clear(sc.full)
		sc.byPos, sc.swept = sc.byPos[:0], sc.p
	}
	sc.grow(2*len(slots) + 1)
	for s := range slots {
		sc.cur[s] = int32(endsAfter(slots[s], 0, sc.p))
		sc.setKey(slots, int32(s))
	}
}

// grow makes the tree's leaves more than need, the keys kept.
func (sc *slotScan) grow(need int) {
	n := max(sc.n, 64)
	for n <= need {
		n *= 2
	}
	old := sc.n
	key, cur := sc.key[:old], sc.cur[:old]
	for range n - old {
		key, cur = append(key, noKey), append(cur, 0)
	}
	keys := grown(sc.keys, 2*n)
	copy(keys[n:], key)
	for i := n - 1; i >= 1; i-- {
		keys[i] = max(keys[2*i], keys[2*i+1])
	}
	sc.n, sc.key, sc.cur, sc.keys = n, key, cur, keys
}

// advance moves the scan's position to p, and the keys of the slots whose
// ranges giving them end at or before it to their next ranges.
func (sc *slotScan) advance(slots [][]rng, p int32) {
	sc.p = p
	if !sc.on {
		return
	}
	if sc.heap {
		for len(sc.ends) > 0 && sc.ends[0].to <= p {
			sc.passed(slots, sc.pop().s, p)
		}
		return
	}
	// The positions with ends, from the one after the last swept to p: the
	// ends a slot's move keeps lie past p.
	full, at := sc.full, sc.at
	for x := sc.swept + 1; x <= p; {
		word := full[x>>6] >> (x & 63)
		if word == 0 {
			x = (x | 63) + 1
			continue
		}
		x += int32(bits.TrailingZeros64(word))
		if x > p {
			break
		}
		for k := at[x]; k != 0; {
			e := sc.byPos[k-1]
			k = e.next
			sc.passed(slots, e.s, p)
		}
		at[x] = 0
		full[x>>6] &^= 1 << (x & 63)
		x++
	}
	sc.swept = p
}

// passed moves slot s's key past p, where the range that gives it ends by
// p.
func (sc *slotScan) passed(slots [][]rng, s, p int32) {
	c := sc.cur[s]
	rs := slots[s]
	if int(c) >= len(rs) || rs[c].to > p {
		// A range it no longer has, or not past yet.
		return
	}
	for int(c) < len(rs) && rs[c].to <= p {
		c++
	}
	sc.cur[s] = c
	sc.setKey(slots, s)
}

// find is the first slot at or past lo whose key is at least x: there is
// one, past the slots.
func (sc *slotScan) find(lo int, x int32) int {
	i := sc.n + lo
	for sc.keys[i] < x {
		// Up past the subtrees i is the last of, to the next one.
		for i&1 == 1 {
			i >>= 1
		}
		i++
	}
	for i < sc.n {
		i *= 2
		if sc.keys[i] < x {
			i++
		}
	}
	return i - sc.n
}

// push keeps end e: at its position, or with Config.slotHeap in the heap,
// which pop takes the first end of.
func (sc *slotScan) push(e slotEnd) {
	if !sc.heap {
		sc.byPos = append(sc.byPos, slotAt{e.s, sc.at[e.to]})
		sc.at[e.to] = int32(len(sc.byPos))
		sc.full[e.to>>6] |= 1 << (e.to & 63)
		return
	}
	h := append(sc.ends, e)
	i := len(h) - 1
	for i > 0 {
		p := (i - 1) / 2
		if h[p].to <= e.to {
			break
		}
		h[i] = h[p]
		i = p
	}
	h[i] = e
	sc.ends = h
}

func (sc *slotScan) pop() slotEnd {
	h := sc.ends
	top := h[0]
	last := h[len(h)-1]
	h = h[:len(h)-1]
	i := 0
	for {
		c := 2*i + 1
		if c >= len(h) {
			break
		}
		if c+1 < len(h) && h[c+1].to < h[c].to {
			c++
		}
		if last.to <= h[c].to {
			break
		}
		h[i] = h[c]
		i = c
	}
	if len(h) > 0 {
		h[i] = last
	}
	sc.ends = h
	return top
}
