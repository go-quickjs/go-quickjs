package ssa

import (
	"iter"
	"math/bits"
)

// A frame state's slots are a persistent array, a tree of nodes of 64
// entries each, that the states share, as V8's frame states share their
// StateValues: a state made after another of its block is the other's
// with the slots written or born or dead since changed, each change a
// copy of the nodes on its path, and the rest the other's. A function of
// n values live at once, past every register, and an exit at each of its
// n instructions had states that each named every one of them: n² slots,
// 13,000 values some seven gigabytes. Shared, its states take what the
// instructions change, as the program that made them does.
//
// A node holds its entries' bits in mask, and one entry for each bit, in
// order: a leaf (height 0) its values, an inner node its children, each
// of 64 slots more for each height. A state's tree is as high as its
// highest slot needs. No slot past a state's Len is set.

const (
	slotShift = 6
	slotFan   = 1 << slotShift
)

// slotNode is a node of states' slots. id numbers it in its Func, for the
// passes' tables by node; edit is the edit that made it, which may change
// it in place while it is open (Func.openEdit): no state but the one being
// made has it yet.
type slotNode struct {
	mask uint64
	vals []*Value
	kids []*slotNode
	id   int32
	edit int32
}

// Len is how many slots s has: its frame's, its prologue's arguments
// (Args) and its homes (Virtual).
func (s *FrameState) Len() int { return s.n }

// Slot is slot i's value in s, or nil for one with none -- a local dead
// there, a captured binding's -- or past its slots.
func (s *FrameState) Slot(i int) *Value {
	if i < 0 || i >= s.n || i>>(slotShift*(s.height+1)) != 0 {
		return nil
	}
	nd := s.slots
	for h := s.height; nd != nil; h-- {
		bit := uint64(1) << (uint(i>>(slotShift*h)) & (slotFan - 1))
		if nd.mask&bit == 0 {
			return nil
		}
		j := bits.OnesCount64(nd.mask & (bit - 1))
		if h == 0 {
			return nd.vals[j]
		}
		nd = nd.kids[j]
	}
	return nil
}

// All is s's slots that have a value, and their values, in order.
func (s *FrameState) All() iter.Seq2[int, *Value] {
	return func(yield func(int, *Value) bool) {
		if s.slots != nil {
			s.slots.each(s.height, 0, yield)
		}
	}
}

// Count is how many of s's slots have a value.
func (s *FrameState) Count() int {
	if s.slots == nil {
		return 0
	}
	return s.slots.count(s.height)
}

// Dense is s's slots as a list of Len, nil where a slot has no value: for
// printing.
func (s *FrameState) Dense() []*Value {
	l := make([]*Value, s.n)
	for i, v := range s.All() {
		l[i] = v
	}
	return l
}

func (nd *slotNode) each(h, base int, yield func(int, *Value) bool) bool {
	m := nd.mask
	if h == 0 {
		for _, v := range nd.vals {
			k := bits.TrailingZeros64(m)
			m &= m - 1
			if !yield(base+k, v) {
				return false
			}
		}
		return true
	}
	for _, kid := range nd.kids {
		k := bits.TrailingZeros64(m)
		m &= m - 1
		if !kid.each(h-1, base+k<<(slotShift*h), yield) {
			return false
		}
	}
	return true
}

func (nd *slotNode) count(h int) int {
	if h == 0 {
		return len(nd.vals)
	}
	n := 0
	for _, kid := range nd.kids {
		n += kid.count(h - 1)
	}
	return n
}

// shareSlots gives s t's slots, to change from there (setSlot).
func (s *FrameState) shareSlots(t *FrameState) {
	s.slots, s.height = t.slots, t.height
}

// openEdit starts a state's edit: the nodes it makes, until the next
// edit, are the state's alone, which setSlot changes in place.
func (f *Func) openEdit() { f.slotEdit++ }

// setSlot has s's slot i hold v, or none for nil, copying the nodes on
// its path that the edit did not make, as every other state that has
// them keeps them as they are. Slot i is below s's Len.
func (f *Func) setSlot(s *FrameState, i int, v *Value) {
	for i>>(slotShift*(s.height+1)) != 0 {
		if v == nil {
			return
		}
		if s.slots != nil {
			r := f.newSlotNode(s.height+1, 1)
			r.mask, r.kids = 1, append(r.kids, s.slots)
			s.slots = r
		}
		s.height++
	}
	s.slots = f.putSlot(s.slots, s.height, i, v)
}

// putSlot is nd, of height h, with slot i holding v, or none for nil:
// nd itself where it was the edit's, or where nothing changes.
func (f *Func) putSlot(nd *slotNode, h, i int, v *Value) *slotNode {
	bit := uint64(1) << (uint(i>>(slotShift*h)) & (slotFan - 1))
	var mask uint64
	if nd != nil {
		mask = nd.mask
	}
	j := bits.OnesCount64(mask & (bit - 1))
	has := mask&bit != 0
	if h == 0 {
		switch {
		case v == nil && !has:
			return nd
		case v == nil && mask == bit:
			return nil
		case v == nil:
			m := f.openSlotNode(nd, h, 0)
			l := len(m.vals)
			copy(m.vals[j:], m.vals[j+1:])
			m.vals[l-1] = nil
			m.vals, m.mask = m.vals[:l-1], m.mask&^bit
			return m
		case has:
			if nd.vals[j] == v {
				return nd
			}
			m := f.openSlotNode(nd, h, 0)
			m.vals[j] = v
			return m
		}
		m := f.openSlotNode(nd, h, 1)
		m.vals = append(m.vals, nil)
		copy(m.vals[j+1:], m.vals[j:])
		m.vals[j], m.mask = v, m.mask|bit
		return m
	}
	var kid *slotNode
	if has {
		kid = nd.kids[j]
	}
	nk := f.putSlot(kid, h-1, i, v)
	switch {
	case nk == kid:
		return nd
	case nk == nil && mask == bit:
		return nil
	case nk == nil:
		m := f.openSlotNode(nd, h, 0)
		l := len(m.kids)
		copy(m.kids[j:], m.kids[j+1:])
		m.kids[l-1] = nil
		m.kids, m.mask = m.kids[:l-1], m.mask&^bit
		return m
	case has:
		m := f.openSlotNode(nd, h, 0)
		m.kids[j] = nk
		return m
	}
	m := f.openSlotNode(nd, h, 1)
	m.kids = append(m.kids, nil)
	copy(m.kids[j+1:], m.kids[j:])
	m.kids[j], m.mask = nk, m.mask|bit
	return m
}

// openSlotNode is nd, of height h, to change in place with room for more
// entries: nd itself if the edit made it, a copy otherwise, or a new node
// for nil.
func (f *Func) openSlotNode(nd *slotNode, h, more int) *slotNode {
	if nd != nil && nd.edit == f.slotEdit {
		if h == 0 && len(nd.vals)+more > cap(nd.vals) {
			nd.vals = append(f.slotVals(growSlots(len(nd.vals)+more)), nd.vals...)
		} else if h > 0 && len(nd.kids)+more > cap(nd.kids) {
			nd.kids = append(f.slotKids(growSlots(len(nd.kids)+more)), nd.kids...)
		}
		return nd
	}
	if nd == nil {
		return f.newSlotNode(h, more)
	}
	m := f.newSlotNode(h, len(nd.vals)+len(nd.kids)+more)
	m.mask = nd.mask
	m.vals = append(m.vals, nd.vals...)
	m.kids = append(m.kids, nd.kids...)
	return m
}

// growSlots is the room a node needing n entries is given: twice as many,
// for the slots set after, up to a node's 64.
func growSlots(n int) int { return min(slotFan, max(2*n, 4)) }

// newSlotNode is an empty node of height h, the edit's, with room for n
// entries.
func (f *Func) newSlotNode(h, n int) *slotNode {
	var nd *slotNode
	if f.ws != nil {
		nd = &f.ws.slotNodes.Make(1)[0]
	} else {
		if len(f.nodeSlab) == 0 {
			f.nodeChunk = min(max(2*f.nodeChunk, 16), 256)
			f.nodeSlab = make([]slotNode, f.nodeChunk)
		}
		nd = &f.nodeSlab[0]
		f.nodeSlab = f.nodeSlab[1:]
	}
	nd.id, nd.edit = f.nextNode, f.slotEdit
	f.nextNode++
	if h == 0 {
		nd.vals = f.slotVals(n)
	} else {
		nd.kids = f.slotKids(n)
	}
	return nd
}

// slotVals is an empty list of room n for a leaf's values.
func (f *Func) slotVals(n int) []*Value {
	if n == 0 {
		return nil
	}
	if f.ws != nil {
		return f.ws.refs.Make(n)[:0]
	}
	if len(f.valSlab) < n {
		f.valChunk = min(max(2*f.valChunk, 64), 1024)
		f.valSlab = make([]*Value, max(f.valChunk, n))
	}
	l := f.valSlab[:0:n]
	f.valSlab = f.valSlab[n:]
	return l
}

// slotKids is an empty list of room n for an inner node's children.
func (f *Func) slotKids(n int) []*slotNode {
	if n == 0 {
		return nil
	}
	if f.ws != nil {
		return f.ws.slotKids.Make(n)[:0]
	}
	if len(f.kidSlab) < n {
		f.kidChunk = min(max(2*f.kidChunk, 16), 256)
		f.kidSlab = make([]*slotNode, max(f.kidChunk, n))
	}
	l := f.kidSlab[:0:n]
	f.kidSlab = f.kidSlab[n:]
	return l
}

// NumSlotNodes bounds the states' slot nodes' IDs, for tables by node.
func (f *Func) NumSlotNodes() int { return int(f.nextNode) }

// eachSlotNode calls fn for each node of s's slots that seen, by node ID,
// does not mark yet, marking it: a pass that does what each value of a
// state asks once for every node does it once for every value however
// many states share it.
func eachSlotNode(s *FrameState, seen []bool, fn func(nd *slotNode, h int)) {
	if s.slots != nil {
		s.slots.walk(s.height, seen, fn)
	}
}

func (nd *slotNode) walk(h int, seen []bool, fn func(*slotNode, int)) {
	if seen[nd.id] {
		return
	}
	seen[nd.id] = true
	fn(nd, h)
	for _, kid := range nd.kids {
		kid.walk(h-1, seen, fn)
	}
}

// rewriteSlots replaces each value of s's nodes that seen does not mark
// yet with find's answer for it, in place: the states that share a node
// are those find's answers hold for too, as it replaces a value
// wherever it is used.
func rewriteSlots(s *FrameState, seen []bool, find func(*Value) *Value) {
	eachSlotNode(s, seen, func(nd *slotNode, h int) {
		for j, v := range nd.vals {
			nd.vals[j] = find(v)
		}
	})
}

// slotAt is a node of states' slots and its height, and next, in a walk
// of them, the first of its children not yet walked.
type slotAt struct {
	nd   *slotNode
	h    int32
	next int32
}

// slotWeights adds to each value of the states' slots, as its uses,
// weights[s.ID()] for each state s that names it, once for each slot it
// is in: through the nodes, each met once, each node's weight the sum of
// those of the nodes and states that have it, from the roots down.
func (f *Func) slotWeights(states []*FrameState, weights []int) {
	n := int(f.nextNode)
	w := f.ints(n)
	seen := f.bools(n)
	sc := f.scr()
	// The nodes in the order a walk from the roots finished them: each
	// after every node it has.
	order, walk := sc.slotOrder[:0], sc.slotWalk[:0]
	for _, s := range states {
		if s.slots == nil {
			continue
		}
		w[s.slots.id] += weights[s.id]
		if seen[s.slots.id] {
			continue
		}
		seen[s.slots.id] = true
		walk = append(walk, slotAt{nd: s.slots, h: int32(s.height)})
		for len(walk) > 0 {
			top := &walk[len(walk)-1]
			if int(top.next) < len(top.nd.kids) {
				kid := top.nd.kids[top.next]
				top.next++
				if !seen[kid.id] {
					seen[kid.id] = true
					walk = append(walk, slotAt{nd: kid, h: top.h - 1})
				}
				continue
			}
			order = append(order, *top)
			walk = walk[:len(walk)-1]
		}
	}
	for k := len(order) - 1; k >= 0; k-- {
		at := order[k]
		x := w[at.nd.id]
		if x == 0 {
			continue
		}
		if at.h == 0 {
			for _, v := range at.nd.vals {
				v.Uses += x
			}
			continue
		}
		for _, kid := range at.nd.kids {
			w[kid.id] += x
		}
	}
	clear(order)
	clear(walk[:cap(walk)])
	sc.slotOrder, sc.slotWalk = order[:0], walk[:0]
}

// checkSlots reports whether s's nodes are well formed: an entry for each
// bit, none past its Len.
func (s *FrameState) checkSlots() bool {
	ok := true
	for i := range s.All() {
		if i >= s.n {
			ok = false
		}
	}
	var check func(nd *slotNode, h int) bool
	check = func(nd *slotNode, h int) bool {
		if nd.mask == 0 {
			return false
		}
		k := bits.OnesCount64(nd.mask)
		if h == 0 {
			if len(nd.vals) != k || nd.kids != nil {
				return false
			}
			for _, v := range nd.vals {
				if v == nil {
					return false
				}
			}
			return true
		}
		if len(nd.kids) != k || len(nd.vals) != 0 {
			return false
		}
		for _, kid := range nd.kids {
			if kid == nil || !check(kid, h-1) {
				return false
			}
		}
		return true
	}
	return ok && (s.slots == nil || check(s.slots, s.height))
}

// diffSlots calls fn with each slot that a and b do not both have the same
// value in, in order: found where their trees differ, which they share
// where nothing changed between them.
func diffSlots(a, b *FrameState, fn func(int)) {
	diffSlotNodes(a.slots, a.height, b.slots, b.height, 0, fn)
}

func diffSlotNodes(x *slotNode, hx int, y *slotNode, hy int, base int, fn func(int)) {
	all := func(nd *slotNode, h, base int) {
		if nd != nil {
			nd.each(h, base, func(i int, _ *Value) bool { fn(i); return true })
		}
	}
	switch {
	case x == y && hx == hy:
	case x == nil:
		all(y, hy, base)
	case y == nil:
		all(x, hx, base)
	case hx != hy:
		// The lower tree is the higher's first child's, at its height.
		if hx < hy {
			x, hx, y, hy = y, hy, x, hx
		}
		m := x.mask
		var first *slotNode
		if m&1 != 0 {
			first = x.kids[0]
		}
		diffSlotNodes(first, hx-1, y, hy, base, fn)
		for m &^= 1; m != 0; m &= m - 1 {
			c := bits.TrailingZeros64(m)
			all(x.kids[bits.OnesCount64(x.mask&(1<<c-1))], hx-1, base+c<<(slotShift*hx))
		}
	case hx == 0:
		mx, my := x.mask, y.mask
		for m := mx | my; m != 0; m &= m - 1 {
			c := bits.TrailingZeros64(m)
			bit := uint64(1) << c
			if mx&bit != 0 && my&bit != 0 && x.vals[bits.OnesCount64(mx&(bit-1))] == y.vals[bits.OnesCount64(my&(bit-1))] {
				continue
			}
			fn(base + c)
		}
	default:
		mx, my := x.mask, y.mask
		for m := mx | my; m != 0; m &= m - 1 {
			c := bits.TrailingZeros64(m)
			bit := uint64(1) << c
			var a, b *slotNode
			if mx&bit != 0 {
				a = x.kids[bits.OnesCount64(mx&(bit-1))]
			}
			if my&bit != 0 {
				b = y.kids[bits.OnesCount64(my&(bit-1))]
			}
			diffSlotNodes(a, hx-1, b, hy-1, base+c<<(slotShift*hx), fn)
		}
	}
}

// SlotNode is a node of states' slots, which the states share: a pass of
// another package that does what each slot of a state asks once for each
// node -- mir's descriptions of exits, which share their nodes as the
// states do -- walks them (FrameState's Tree).
type SlotNode = slotNode

// SlotShift is how many bits of a slot's index each height of a tree of
// SlotNodes takes: a node at height h has 1<<SlotShift entries, each of
// 1<<(SlotShift*h) slots.
const SlotShift = slotShift

// Tree is s's slots' root and its height; nil for none.
func (s *FrameState) Tree() (*SlotNode, int) { return s.slots, s.height }

// ID numbers nd in its Func, below NumSlotNodes.
func (nd *slotNode) ID() int { return int(nd.id) }

// Mask is which of nd's entries it has.
func (nd *slotNode) Mask() uint64 { return nd.mask }

// Vals is a leaf's values, one for each bit of its Mask, in order.
func (nd *slotNode) Vals() []*Value { return nd.vals }

// Kids is an inner node's children, one for each bit of its Mask, in order.
func (nd *slotNode) Kids() []*SlotNode { return nd.kids }

// SlotWalk walks the nodes of states' slots top down (Walk), keeping its
// tables from walk to walk.
type SlotWalk struct {
	seen         []int32
	mark         int32
	order, stack []slotWalkAt
}

type slotWalkAt struct {
	nd            *slotNode
	h, base, next int32
	// rest is the mask's bits of the children not yet walked.
	rest uint64
}

// Walk calls visit with each node of the slots of states, once, after
// every node of theirs that has it -- its parents -- with its height and
// its first slot: for a pass that does for each slot of a state what the
// states asking for its node ask together, as slotWeights sums their
// weights from the roots down, each node met once however many states
// share it.
func (w *SlotWalk) Walk(f *Func, states []*FrameState, visit func(nd *SlotNode, h, base int)) {
	if n := int(f.nextNode); len(w.seen) < n {
		w.seen = make([]int32, max(n, 2*len(w.seen)))
		w.mark = 0
	}
	w.mark++
	order, stack := w.order[:0], w.stack[:0]
	for _, s := range states {
		if s.slots == nil || w.seen[s.slots.id] == w.mark {
			continue
		}
		w.seen[s.slots.id] = w.mark
		stack = append(stack, slotWalkAt{nd: s.slots, h: int32(s.height), rest: s.slots.mask})
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			if int(top.next) < len(top.nd.kids) {
				kid := top.nd.kids[top.next]
				c := int32(bits.TrailingZeros64(top.rest))
				top.rest &= top.rest - 1
				top.next++
				if w.seen[kid.id] != w.mark {
					w.seen[kid.id] = w.mark
					stack = append(stack, slotWalkAt{nd: kid, h: top.h - 1, base: top.base + c<<(slotShift*top.h), rest: kid.mask})
				}
				continue
			}
			order = append(order, *top)
			stack = stack[:len(stack)-1]
		}
	}
	for k := len(order) - 1; k >= 0; k-- {
		at := order[k]
		visit(at.nd, int(at.h), int(at.base))
	}
	clear(order)
	w.order, w.stack = order[:0], stack[:0]
}
