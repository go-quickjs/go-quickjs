package ir

import (
	"iter"
	"math/bits"
	"slices"
)

// Liveness says, for each instruction, which of the first N slots the code
// from there may read before it writes them (Program's Live). Each
// instruction's set is a persistent bitset, a tree that the instructions'
// sets share, as frame states share their slots: an instruction's set is
// the next one's with the slots it writes taken out and those it reads put
// in, which copies only the nodes on their paths. A dense row of n bits at
// each of m instructions is n·m bits however few change from one to the
// next -- a function of 50,000 values hundreds of megabytes -- where the
// shared sets take what the instructions change. A Liveness keeps the
// rows where they are the smaller (LiveBuilder's Finish), as they are for
// most functions of a few hundred slots or fewer.
//
// The tree's leaves are words of 64 slots; an inner node has up to 16
// children, a mask of those it has, and one entry for each, in order. A
// set's tree is as high as N needs: at height 0 a set is its one word,
// which is its row.
type Liveness struct {
	// N is how many slots the sets are of: the function's locals, or its
	// registers in register code's program.
	N int
	// rows, where not nil, are the sets as dense rows, words words a PC.
	rows  []uint64
	words int
	// Else roots holds each instruction's set, its root's offset in nodes,
	// of a tree height high: nodes holds the inner nodes, each its mask
	// and then its children's entries -- leaves' indices in leaves at
	// height 1, nodes' offsets above. nodes[0] is the empty node.
	height int
	roots  []uint32
	nodes  []uint32
	leaves []uint64
}

const maxInt = int(^uint(0) >> 1)

const (
	liveFanShift = 4
	liveFan      = 1 << liveFanShift
)

// liveHeight is the height of a tree of n slots.
func liveHeight(n int) int {
	h := 0
	for span := 64; span < n; span <<= liveFanShift {
		h++
	}
	return h
}

// liveChunk is which child of a node at height h slot i is under.
func liveChunk(h, i int) int { return i >> (6 + liveFanShift*(h-1)) & (liveFan - 1) }

// Has reports whether slot i is in pc's set; false for i past N.
func (l *Liveness) Has(pc, i int) bool {
	if i < 0 || i >= l.N {
		return false
	}
	if l.rows != nil {
		return l.rows[pc*l.words+i>>6]&(1<<(i&63)) != 0
	}
	return liveHas(l.nodes, l.leaves, l.height, uint64(l.roots[pc]), i)
}

func liveHas(nodes []uint32, leaves []uint64, h int, s uint64, i int) bool {
	for ; h > 0; h-- {
		off := int(s)
		bit := uint32(1) << liveChunk(h, i)
		m := nodes[off]
		if m&bit == 0 {
			return false
		}
		s = uint64(nodes[off+1+bits.OnesCount32(m&(bit-1))])
		if h == 1 {
			s = leaves[s]
		}
	}
	return s&(1<<(i&63)) != 0
}

// Slots is pc's set, in order.
func (l *Liveness) Slots(pc int) iter.Seq[int] {
	return func(yield func(int) bool) {
		if l.rows != nil {
			for w, x := range l.rows[pc*l.words : (pc+1)*l.words] {
				if !liveEach(nil, nil, 0, x, w*64, yield) {
					return
				}
			}
			return
		}
		liveEach(l.nodes, l.leaves, l.height, uint64(l.roots[pc]), 0, yield)
	}
}

func liveEach(nodes []uint32, leaves []uint64, h int, s uint64, base int, yield func(int) bool) bool {
	if h == 0 {
		for w := s; w != 0; w &= w - 1 {
			if !yield(base + bits.TrailingZeros64(w)) {
				return false
			}
		}
		return true
	}
	off := int(s)
	m := nodes[off]
	for k := 1; m != 0; k++ {
		c := bits.TrailingZeros32(m)
		m &= m - 1
		kid := uint64(nodes[off+k])
		if h == 1 {
			kid = leaves[kid]
		}
		if !liveEach(nodes, leaves, h-1, kid, base+c<<(6+liveFanShift*(h-1)), yield) {
			return false
		}
	}
	return true
}

// Changed is the slots in one of pc0's and pc1's sets and not the other's,
// in order: found where the two sets' trees differ, which they share
// where nothing changed between them.
func (l *Liveness) Changed(pc0, pc1 int) iter.Seq[int] {
	return func(yield func(int) bool) {
		if l.rows != nil {
			r0, r1 := l.rows[pc0*l.words:(pc0+1)*l.words], l.rows[pc1*l.words:]
			for w, x := range r0 {
				if !liveEach(nil, nil, 0, x^r1[w], w*64, yield) {
					return
				}
			}
			return
		}
		liveDiff(l.nodes, l.leaves, l.height, uint64(l.roots[pc0]), uint64(l.roots[pc1]), 0, yield)
	}
}

func liveDiff(nodes []uint32, leaves []uint64, h int, a, b uint64, base int, yield func(int) bool) bool {
	if a == b {
		return true
	}
	if h == 0 {
		return liveEach(nil, nil, 0, a^b, base, yield)
	}
	ma, mb := nodes[a], nodes[b]
	ka, kb := int(a)+1, int(b)+1
	for m := ma | mb; m != 0; m &= m - 1 {
		c := bits.TrailingZeros32(m)
		bit := uint32(1) << c
		// A child one of them has not is the empty set: word 0, or the
		// empty node at offset 0.
		var x, y uint64
		if ma&bit != 0 {
			x = uint64(nodes[ka])
			ka++
			if h == 1 {
				x = leaves[x]
			}
		}
		if mb&bit != 0 {
			y = uint64(nodes[kb])
			kb++
			if h == 1 {
				y = leaves[y]
			}
		}
		if !liveDiff(nodes, leaves, h-1, x, y, base+c<<(6+liveFanShift*(h-1)), yield) {
			return false
		}
	}
	return true
}

// Size is the memory l holds, in bytes.
func (l *Liveness) Size() int {
	if l == nil {
		return 0
	}
	return 80 + cap(l.rows)*8 + cap(l.roots)*4 + cap(l.nodes)*4 + cap(l.leaves)*8
}

// LiveSet is a set a LiveBuilder makes: at height 0 the set's word, above it
// its root node's offset in the builder's nodes.
type LiveSet uint64

// LiveBuilder makes a Liveness's sets. Its operations return the set they
// are given wherever they change nothing, so that a set that ends where it
// began is the same set, and Equal is quick on sets that share their
// nodes. A node made since the edit began (Edit) is the edit's own, which
// its operations change in place: a set made of several operations copies
// a node on their paths once.
type LiveBuilder struct {
	n, height int
	nodes     []uint32
	leaves    []uint64
	// The first node and leaf the edit made.
	editNode, editLeaf int
	full               LiveSet
	kids               []uint32
}

// NewLiveBuilder is a builder of sets of n slots.
func NewLiveBuilder(n int) *LiveBuilder {
	// No edit is open until one begins: every set is kept.
	return &LiveBuilder{n: n, height: liveHeight(n), nodes: []uint32{0}, editNode: maxInt, editLeaf: maxInt}
}

// Reset makes b a builder of sets of n slots again, as NewLiveBuilder's,
// keeping the room it has: every set made before is gone.
func (b *LiveBuilder) Reset(n int) {
	*b = LiveBuilder{n: n, height: liveHeight(n), nodes: append(b.nodes[:0], 0), leaves: b.leaves[:0], kids: b.kids[:0], editNode: maxInt, editLeaf: maxInt}
}

// Bytes is the memory b holds.
func (b *LiveBuilder) Bytes() int {
	return cap(b.nodes)*4 + cap(b.leaves)*8 + cap(b.kids)*4
}

// Added calls yield with the slots of y that x has not, in order, until it
// returns false: found where the two sets' trees differ, which they share
// where nothing changed between them.
func (b *LiveBuilder) Added(x, y LiveSet, yield func(int) bool) {
	liveAdded(b.nodes, b.leaves, b.height, uint64(x), uint64(y), 0, yield)
}

func liveAdded(nodes []uint32, leaves []uint64, h int, a, b uint64, base int, yield func(int) bool) bool {
	if a == b || b == 0 {
		return true
	}
	if h == 0 {
		return liveEach(nil, nil, 0, b&^a, base, yield)
	}
	var ma uint32
	if a != 0 {
		ma = nodes[a]
	}
	mb := nodes[b]
	ka, kb := int(a)+1, int(b)+1
	for m := ma | mb; m != 0; m &= m - 1 {
		c := bits.TrailingZeros32(m)
		bit := uint32(1) << c
		var x, y uint64
		if ma&bit != 0 {
			x = uint64(nodes[ka])
			ka++
			if h == 1 {
				x = leaves[x]
			}
		}
		if mb&bit == 0 {
			continue
		}
		y = uint64(nodes[kb])
		kb++
		if h == 1 {
			y = leaves[y]
		}
		if !liveAdded(nodes, leaves, h-1, x, y, base+c<<(6+liveFanShift*(h-1)), yield) {
			return false
		}
	}
	return true
}

// Edit begins an edit: the sets made until the next one may share the
// nodes it makes, which they change in place, and no set made before
// does. A set made in an edit is one that the edit's next operations may
// change; a set to keep is made in an edit of its own.
func (b *LiveBuilder) Edit() { b.editNode, b.editLeaf = len(b.nodes), len(b.leaves) }

// Empty is the empty set.
func (b *LiveBuilder) Empty() LiveSet { return 0 }

// LiveOp sets slot Slot in a set (Apply) or, with Clear, takes it out.
type LiveOp struct {
	Slot  int32
	Clear bool
}

// Apply is s with ops done, in order of their slots, those of one slot in
// the order given. Ops of slots outside [0, N) are ignored. ops is sorted
// by slot in place, stably.
func (b *LiveBuilder) Apply(s LiveSet, ops []LiveOp) LiveSet {
	k := 0
	for _, o := range ops {
		if o.Slot >= 0 && int(o.Slot) < b.n {
			ops[k] = o
			k++
		}
	}
	ops = ops[:k]
	if len(ops) == 0 {
		return s
	}
	if !slices.IsSortedFunc(ops, func(x, y LiveOp) int { return int(x.Slot) - int(y.Slot) }) {
		slices.SortStableFunc(ops, func(x, y LiveOp) int { return int(x.Slot) - int(y.Slot) })
	}
	return LiveSet(b.apply(b.height, uint64(s), ops))
}

func (b *LiveBuilder) apply(h int, s uint64, ops []LiveOp) uint64 {
	if h == 0 {
		for _, o := range ops {
			if bit := uint64(1) << (o.Slot & 63); o.Clear {
				s &^= bit
			} else {
				s |= bit
			}
		}
		return s
	}
	off := int(s)
	m := b.nodes[off]
	own := off >= b.editNode
	// The node's children, changed where ops reach them.
	start := len(b.kids)
	changed := false
	var nm uint32
	j, k := 0, off+1
	for c := 0; c < liveFan && j < len(ops) || m>>c != 0; c++ {
		bit := uint32(1) << c
		var kid uint64
		has := m&bit != 0
		if has {
			kid = uint64(b.nodes[k])
			k++
		}
		e := j
		for e < len(ops) && liveChunk(h, int(ops[e].Slot)) == c {
			e++
		}
		if e == j {
			if has {
				nm |= bit
				b.kids = append(b.kids, uint32(kid))
			}
			continue
		}
		var x uint64
		if has {
			x = kid
			if h == 1 {
				x = b.leaves[kid]
			}
		}
		y := b.apply(h-1, x, ops[j:e])
		j = e
		switch {
		case y == x && !has:
			continue
		case y == x:
		case y == 0:
			changed = true
			continue
		case h == 1 && has && int(kid) >= b.editLeaf:
			b.leaves[kid] = y
		case h == 1:
			b.leaves = append(b.leaves, y)
			kid = uint64(len(b.leaves) - 1)
			changed = true
		default:
			kid = y
			changed = true
		}
		nm |= bit
		b.kids = append(b.kids, uint32(kid))
	}
	r := s
	switch {
	case !changed:
	case own && nm == m:
		copy(b.nodes[off+1:], b.kids[start:])
	default:
		r = b.node(nm, b.kids[start:])
	}
	b.kids = b.kids[:start]
	return r
}

// node is a new node of mask m and children kids, or the empty node.
func (b *LiveBuilder) node(m uint32, kids []uint32) uint64 {
	if m == 0 {
		return 0
	}
	off := len(b.nodes)
	b.nodes = append(b.nodes, m)
	b.nodes = append(b.nodes, kids...)
	return uint64(off)
}

// Union is the slots of x and y: x itself where y adds nothing to it.
func (b *LiveBuilder) Union(x, y LiveSet) LiveSet {
	return LiveSet(b.union(b.height, uint64(x), uint64(y)))
}

func (b *LiveBuilder) union(h int, x, y uint64) uint64 {
	if x == y || y == 0 {
		return x
	}
	if x == 0 {
		return y
	}
	if h == 0 {
		return x | y
	}
	mx, my := b.nodes[x], b.nodes[y]
	own := int(x) >= b.editNode
	start := len(b.kids)
	changed := my&^mx != 0
	kx, ky := int(x)+1, int(y)+1
	for m := mx | my; m != 0; m &= m - 1 {
		bit := uint32(1) << bits.TrailingZeros32(m)
		switch {
		case mx&bit == 0:
			b.kids = append(b.kids, b.nodes[ky])
			ky++
		case my&bit == 0:
			b.kids = append(b.kids, b.nodes[kx])
			kx++
		default:
			a, c := b.nodes[kx], b.nodes[ky]
			kx++
			ky++
			if h == 1 {
				wa, wc := b.leaves[a], b.leaves[c]
				switch {
				case wa|wc == wa:
				case int(a) >= b.editLeaf:
					b.leaves[a] = wa | wc
				default:
					b.leaves = append(b.leaves, wa|wc)
					a = uint32(len(b.leaves) - 1)
					changed = true
				}
			} else if u := b.union(h-1, uint64(a), uint64(c)); u != uint64(a) {
				a = uint32(u)
				changed = true
			}
			b.kids = append(b.kids, a)
		}
	}
	r := x
	switch {
	case !changed:
	case own && mx|my == mx:
		copy(b.nodes[int(x)+1:], b.kids[start:])
	default:
		r = b.node(mx|my, b.kids[start:])
	}
	b.kids = b.kids[:start]
	return r
}

// Full is the set of every slot.
func (b *LiveBuilder) Full() LiveSet {
	if b.full == 0 && b.n > 0 {
		ops := make([]LiveOp, b.n)
		for i := range ops {
			ops[i].Slot = int32(i)
		}
		// Made in no edit, as no other set's may change it.
		en, el := b.editNode, b.editLeaf
		b.editNode, b.editLeaf = maxInt, maxInt
		b.full = b.Apply(0, ops)
		b.editNode, b.editLeaf = max(en, len(b.nodes)), max(el, len(b.leaves))
	}
	return b.full
}

// Equal reports whether x and y have the same slots.
func (b *LiveBuilder) Equal(x, y LiveSet) bool {
	return b.equal(b.height, uint64(x), uint64(y))
}

func (b *LiveBuilder) equal(h int, x, y uint64) bool {
	if x == y {
		return true
	}
	if h == 0 || x == 0 || y == 0 {
		return false
	}
	m := b.nodes[x]
	if m != b.nodes[y] {
		return false
	}
	for k := 1; k <= bits.OnesCount32(m); k++ {
		a, c := b.nodes[int(x)+k], b.nodes[int(y)+k]
		if h == 1 {
			if b.leaves[a] != b.leaves[c] {
				return false
			}
		} else if !b.equal(h-1, uint64(a), uint64(c)) {
			return false
		}
	}
	return true
}

// Has reports whether slot i is in s.
func (b *LiveBuilder) Has(s LiveSet, i int) bool {
	if i < 0 || i >= b.n {
		return false
	}
	return liveHas(b.nodes, b.leaves, b.height, uint64(s), i)
}

// Finish is the Liveness whose instruction pc has set roots[pc]: its tree,
// each node the sets have copied once, without those the builder made on
// its way there; or its dense rows, where they are the smaller.
func (b *LiveBuilder) Finish(roots []LiveSet) *Liveness {
	words := (b.n + 63) / 64
	l := &Liveness{N: b.n, height: b.height}
	if b.height == 0 {
		l.rows, l.words = make([]uint64, len(roots)), words
		for i, r := range roots {
			l.rows[i] = uint64(r)
		}
		return l
	}
	l.roots = make([]uint32, len(roots))
	l.nodes = []uint32{0}
	nodeAt := make([]uint32, len(b.nodes))
	leafAt := make([]uint32, len(b.leaves))
	for i := range leafAt {
		leafAt[i] = ^uint32(0)
	}
	var copyNode func(h int, off uint32) uint32
	copyNode = func(h int, off uint32) uint32 {
		if off == 0 || nodeAt[off] != 0 {
			return nodeAt[off]
		}
		m := b.nodes[off]
		n := bits.OnesCount32(m)
		at := len(l.nodes)
		l.nodes = append(l.nodes, m)
		l.nodes = append(l.nodes, make([]uint32, n)...)
		for k := 1; k <= n; k++ {
			kid := b.nodes[int(off)+k]
			if h == 1 {
				if leafAt[kid] == ^uint32(0) {
					leafAt[kid] = uint32(len(l.leaves))
					l.leaves = append(l.leaves, b.leaves[kid])
				}
				kid = leafAt[kid]
			} else {
				kid = copyNode(h-1, kid)
			}
			l.nodes[at+k] = kid
		}
		nodeAt[off] = uint32(at)
		return uint32(at)
	}
	for i, r := range roots {
		l.roots[i] = copyNode(b.height, uint32(r))
	}
	if dense := len(roots) * words * 8; dense <= len(l.roots)*4+len(l.nodes)*4+len(l.leaves)*8 {
		rows := make([]uint64, len(roots)*words)
		for pc := range roots {
			for i := range l.Slots(pc) {
				rows[pc*words+i>>6] |= 1 << (i & 63)
			}
		}
		return &Liveness{N: b.n, rows: rows, words: words}
	}
	l.nodes = slices.Clip(l.nodes)
	l.leaves = slices.Clip(l.leaves)
	return l
}

// LivenessFromRows is the Liveness of dense rows: words words of bits for
// each of m instructions, row pc from rows[pc*words], of n slots.
func LivenessFromRows(rows []uint64, words, n, m int) *Liveness {
	b := NewLiveBuilder(n)
	roots := make([]LiveSet, m)
	var ops []LiveOp
	prev, cur := LiveSet(0), make([]uint64, words)
	for pc := range m {
		row := rows[pc*words : (pc+1)*words]
		ops = ops[:0]
		for w := range words {
			for x := row[w] ^ cur[w]; x != 0; x &= x - 1 {
				i := w*64 + bits.TrailingZeros64(x)
				ops = append(ops, LiveOp{Slot: int32(i), Clear: row[w]&(1<<(i&63)) == 0})
			}
		}
		copy(cur, row)
		b.Edit()
		prev = b.Apply(prev, ops)
		roots[pc] = prev
	}
	return b.Finish(roots)
}
