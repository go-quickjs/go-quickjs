package lsra

import (
	"container/heap"
	"math/bits"
	"slices"
)

// rng is a range of positions, from inclusive, to exclusive.
type rng struct{ from, to int32 }

// overlap reports whether two sorted lists of ranges meet.
func overlap(a, b []rng) bool {
	if len(a) < len(b) {
		a, b = b, a
	}
	if len(b)*8 < len(a) {
		// Few ranges against many: each found in the many by a search,
		// from where the last was.
		lo := 0
		for _, r := range b {
			i := endsAfter(a, lo, r.from)
			if i == len(a) {
				return false
			}
			if a[i].from < r.to {
				return true
			}
			lo = i
		}
		return false
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i].to <= b[j].from:
			i++
		case b[j].to <= a[i].from:
			j++
		default:
			return true
		}
	}
	return false
}

// endsAfter is the index of the first of a's ranges from lo on that ends
// after p, or len(a): a's ranges are in order and apart.
func endsAfter(a []rng, lo int, p int32) int {
	hi := len(a)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if a[m].to <= p {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

// web is coalesced virtual registers, which share a location.
type web struct {
	members []VReg
	cls     int8
	ranges  []rng
	// weight is its uses' and definitions' weight, callWeight that of the
	// reloads after the calls it lives across.
	weight, callWeight float64
	calls              bool
	// avoid is the registers of its class an instruction it lives across
	// clobbers.
	avoid uint64
	// fixed is the slot a member arrives in, or -1, and arrive the
	// position that member is defined at.
	fixed  int32
	arrive int32
	hints  []*web
	reg    int
	slot   int32
	// spilled says it lives in its slot; cur is the scan's index into
	// ranges: of the first range that does not end before the scan's
	// position.
	spilled bool
	cur     int
	// dense is its density, once known.
	dense float64
	// owned says ranges is its own, not a register's, and may grow in
	// place.
	owned bool
}

func (w *web) start() int32 { return w.ranges[0].from }
func (w *web) end() int32   { return w.ranges[len(w.ranges)-1].to }

// covers reports whether w is live at p, at or past the scan's last
// position.
func (w *web) covers(p int32) bool {
	for w.cur < len(w.ranges) && w.ranges[w.cur].to <= p {
		w.cur++
	}
	return w.cur < len(w.ranges) && w.ranges[w.cur].from <= p
}

type clobber struct {
	at     int32
	masks  []uint64
	call   bool
	key    [2]int32
	weight float64
}

// pair is a phi and an argument to coalesce, weighed by the class of
// their edge's depth's weight.
type pair struct {
	a, b  VReg
	class int
}

// vacross is what a register lives across: whether calls, their weight,
// and the registers of its class clobbers write.
type vacross struct {
	calls  bool
	weight float64
	avoid  uint64
}

// An Allocator allocates the registers of one function after another,
// keeping what it allocates for the next: its result, and the lists and
// tables an allocation works in. It is not safe for concurrent use.
type Allocator struct {
	res Result

	predIdx  []int32
	predOff  []int32
	defBlock []int32
	global   []int32
	globals  []VReg
	live     []uint64
	ranges   [][]rng
	weight   []float64
	defAt    []int32
	open     []int32
	opened   []VReg
	starts   []int32
	ends     []int32
	clobbers []clobber
	webs     []*web
	pairs    []pair
	list     []*web
	active   []*web
	inactive []*web
	back     []*web
	queue    []*web
	partQ    webQueue
	slots    [][]rng
	across   []acrossEntry
	parts    []*web
	next     []int32
	clobAt   []int32
	vacross  []vacross
	sorted   []pair

	rngs  arena[rng]
	vregs arena[VReg]
	hints arena[*web]
	pool  chunks[web]
}

// acrossEntry is a register a call, by its index among the clobbers,
// reloads.
type acrossEntry struct {
	call int32
	v    VReg
}

// Allocate allocates f's virtual registers.
func Allocate(f *Func, cfg *Config) (*Result, error) {
	return new(Allocator).Allocate(f, cfg)
}

// grown returns s with length n, its contents not kept, reusing its array
// where it is large enough.
func grown[T any](s []T, n int) []T {
	if cap(s) < n {
		return make([]T, n, n+n/4)
	}
	return s[:n]
}

// Allocate allocates f's virtual registers. The result is the
// Allocator's, valid until its next allocation.
func (al *Allocator) Allocate(f *Func, cfg *Config) (*Result, error) {
	n := len(f.Class)
	nb := len(f.Blocks)
	alloc := func(v VReg) bool { return v >= 0 && f.Class[v] != NoClass }
	lap := func(p Phase) {
		if cfg.Lap != nil {
			cfg.Lap(p)
		}
	}
	al.rngs.reset()
	al.vregs.reset()
	al.hints.reset()
	al.pool.reset()

	// predIdx is, for each block's successor k, the block's index among
	// that successor's predecessors: predIdx[predOff[i]+k].
	al.predOff = grown(al.predOff, nb+1)
	predIdx := al.predIdx[:0]
	for i := range f.Blocks {
		al.predOff[i] = int32(len(predIdx))
		for _, s := range f.Blocks[i].Succs {
			predIdx = append(predIdx, int32(slices.Index(f.Blocks[s].Preds, i)))
		}
	}
	al.predOff[nb] = int32(len(predIdx))
	al.predIdx = predIdx
	predOff := al.predOff

	// Liveness, of the registers that may be live across a block's
	// boundary -- a phi's, a phi's argument, one used in a block other
	// than its definition's -- numbered densely (global): the registers
	// live into and out of each block, over the blocks backwards until
	// nothing changes. A successor's phis read their arguments on the
	// edge, at the predecessor's end. The others are live within their
	// blocks alone, which the ranges' walk finds.
	defBlock := grown(al.defBlock, n)
	global := grown(al.global, n)
	for i := range defBlock {
		defBlock[i] = -1
	}
	clear(global)
	al.defBlock, al.global = defBlock, global
	globals := al.globals[:0]
	mark := func(v VReg) {
		if alloc(v) && global[v] == 0 {
			globals = append(globals, v)
			global[v] = int32(len(globals))
		}
	}
	for i := range f.Blocks {
		b := &f.Blocks[i]
		for _, p := range b.Phis {
			if alloc(p.Def) {
				defBlock[p.Def] = int32(i)
				mark(p.Def)
			}
			for _, a := range p.Args {
				mark(a)
			}
		}
		for j := range b.Insts {
			for _, d := range b.Insts[j].Defs {
				if alloc(d) {
					defBlock[d] = int32(i)
				}
			}
		}
	}
	for i := range f.Blocks {
		b := &f.Blocks[i]
		for j := range b.Insts {
			for _, u := range b.Insts[j].Uses {
				if alloc(u) && defBlock[u] != int32(i) {
					mark(u)
				}
			}
		}
	}
	al.globals = globals
	// gi is v's global number, or -1.
	gi := func(v VReg) int { return int(global[v]) - 1 }
	words := (len(globals) + 63) / 64
	// Each block's sets: live in, live out, gen and kill, words each.
	live := grown(al.live, 4*nb*words)
	clear(live)
	al.live = live
	liveIn := func(i int) []uint64 { return live[4*i*words : (4*i+1)*words] }
	liveOut := func(i int) []uint64 { return live[(4*i+1)*words : (4*i+2)*words] }
	for i := range f.Blocks {
		b := &f.Blocks[i]
		g, k := live[(4*i+2)*words:(4*i+3)*words], live[(4*i+3)*words:(4*i+4)*words]
		for j := len(b.Insts) - 1; j >= 0; j-- {
			in := &b.Insts[j]
			for _, d := range in.Defs {
				if alloc(d) {
					if x := gi(d); x >= 0 {
						k[x/64] |= 1 << (x % 64)
						g[x/64] &^= 1 << (x % 64)
					}
				}
			}
			for _, u := range in.Uses {
				if alloc(u) {
					if x := gi(u); x >= 0 {
						g[x/64] |= 1 << (x % 64)
					}
				}
			}
		}
		for _, p := range b.Phis {
			if alloc(p.Def) {
				x := gi(p.Def)
				k[x/64] |= 1 << (x % 64)
				g[x/64] &^= 1 << (x % 64)
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for i := nb - 1; i >= 0; i-- {
			b := &f.Blocks[i]
			out := liveOut(i)
			for k, s := range b.Succs {
				sin := liveIn(s)
				for w := range out {
					nw := out[w] | sin[w]
					if nw != out[w] {
						out[w] = nw
						changed = true
					}
				}
				pi := predIdx[int(predOff[i])+k]
				for _, p := range f.Blocks[s].Phis {
					if a := p.Args[pi]; alloc(a) {
						x := gi(a)
						if out[x/64]&(1<<(x%64)) == 0 {
							out[x/64] |= 1 << (x % 64)
							changed = true
						}
					}
				}
			}
			in := liveIn(i)
			g, k := live[(4*i+2)*words:(4*i+3)*words], live[(4*i+3)*words:(4*i+4)*words]
			for w := range in {
				nw := g[w] | out[w]&^k[w]
				if nw != in[w] {
					in[w] = nw
					changed = true
				}
			}
		}
	}
	lap(PhaseLiveness)

	// The ranges, over positions: a block's phis are defined at its start;
	// instruction j reads at start+2+2j and writes one after; the block
	// ends at start+2+2*len(Insts), where its successors' phis read their
	// arguments. Each register's ranges are made backwards through each
	// block: what is live out, from the block's start to its end, cut at
	// its definition; what is used and not live after, from the start to
	// the use, cut likewise.
	ranges := grown(al.ranges, n)
	clear(ranges)
	al.ranges = ranges
	weight := grown(al.weight, n)
	clear(weight)
	al.weight = weight
	defAt := grown(al.defAt, n)
	clear(defAt)
	al.defAt = defAt
	clobbers := al.clobbers[:0]
	starts := grown(al.starts, nb)
	ends := grown(al.ends, nb)
	al.starts, al.ends = starts, ends
	pos := int32(0)
	for i := range f.Blocks {
		starts[i] = pos
		ends[i] = pos + 2 + 2*int32(len(f.Blocks[i].Insts))
		pos = ends[i] + 2
	}
	// open is, for a register live in the block being walked, the index
	// of its range there plus one.
	open := grown(al.open, n)
	clear(open)
	al.open = open
	opened := al.opened[:0]
	rngs := &al.rngs
	def := func(d VReg, at int32) {
		if o := open[d]; o > 0 {
			ranges[d][o-1].from = at
			open[d] = 0
		} else {
			ranges[d] = appendA(rngs, ranges[d], rng{at, at + 1})
		}
		defAt[d] = at
	}
	for i := nb - 1; i >= 0; i-- {
		b := &f.Blocks[i]
		start, end := starts[i], ends[i]
		w := depthWeight(b.Depth)
		opened = opened[:0]
		for wi, word := range liveOut(i) {
			for word != 0 {
				v := globals[wi*64+bits.TrailingZeros64(word)]
				word &= word - 1
				ranges[v] = appendA(rngs, ranges[v], rng{start, end + 1})
				open[v] = int32(len(ranges[v]))
				opened = append(opened, v)
			}
		}
		for k, s := range b.Succs {
			pi := predIdx[int(predOff[i])+k]
			for _, p := range f.Blocks[s].Phis {
				if a := p.Args[pi]; alloc(a) {
					weight[a] += w
				}
			}
		}
		for j := len(b.Insts) - 1; j >= 0; j-- {
			in := &b.Insts[j]
			u := start + 2 + 2*int32(j)
			for _, d := range in.Defs {
				if alloc(d) {
					weight[d] += w
					def(d, u+1)
				}
			}
			for _, v := range in.Uses {
				if !alloc(v) {
					continue
				}
				weight[v] += w
				if open[v] == 0 {
					ranges[v] = appendA(rngs, ranges[v], rng{start, u + 1})
					open[v] = int32(len(ranges[v]))
					opened = append(opened, v)
				}
			}
			if in.Call || len(in.Clobber) > 0 {
				clobbers = append(clobbers, clobber{at: u, masks: in.Clobber, call: in.Call, key: [2]int32{int32(i), int32(j)}, weight: w})
			}
		}
		for _, p := range b.Phis {
			if alloc(p.Def) {
				weight[p.Def] += w
				def(p.Def, start)
			}
		}
		for _, v := range opened {
			open[v] = 0
		}
	}
	al.opened = opened
	// The clobbers were found backwards: their positions are distinct.
	slices.Reverse(clobbers)
	al.clobbers = clobbers
	for v, r := range ranges {
		if len(r) < 2 {
			continue
		}
		// Found backwards, a range a block, a register's ranges are in
		// the reverse of their order.
		slices.Reverse(r)
		out := r[:1]
		for _, x := range r[1:] {
			if l := &out[len(out)-1]; x.from <= l.to {
				l.to = max(l.to, x.to)
			} else {
				out = append(out, x)
			}
		}
		ranges[v] = out
	}
	// clobAt is each clobber's position, in order.
	clobAt := al.clobAt[:0]
	for k := range clobbers {
		clobAt = append(clobAt, clobbers[k].at)
	}
	al.clobAt = clobAt
	// crossed visits the clobbers a list of ranges lives across: those at
	// a position p where it is live at p and p+1.
	crossed := func(rs []rng, visit func(k int)) {
		if len(clobAt) == 0 {
			return
		}
		for _, r := range rs {
			// The first clobber at or past the range's start.
			lo, hi := 0, len(clobAt)
			for lo < hi {
				m := int(uint(lo+hi) >> 1)
				if clobAt[m] < r.from {
					lo = m + 1
				} else {
					hi = m
				}
			}
			for k := lo; k < len(clobAt) && clobAt[k] < r.to-1; k++ {
				visit(k)
			}
		}
	}
	lap(PhaseRanges)

	// Webs: each register its own, then a phi's and an argument's joined
	// where their lifetimes do not meet and their parameters' slots allow,
	// the edges of the deepest blocks first.
	// single is v's web of its own: the clobbers it lives across are
	// found once, for its first, and kept for a web taken apart.
	vacr := grown(al.vacross, n)
	al.vacross = vacr
	single := func(v VReg) *web {
		w := al.pool.new()
		*w = web{members: append(al.vregs.take(1), v), cls: f.Class[v], ranges: ranges[v], weight: weight[v], fixed: -1, reg: -1, slot: -1}
		if f.FixedSlot[v] >= 0 {
			w.fixed, w.arrive = f.FixedSlot[v], defAt[v]
		}
		w.calls, w.callWeight, w.avoid = vacr[v].calls, vacr[v].weight, vacr[v].avoid
		return w
	}
	webs := grown(al.webs, n)
	clear(webs)
	al.webs = webs
	for v := range n {
		if alloc(VReg(v)) && len(ranges[v]) > 0 {
			a := vacross{}
			cls := f.Class[v]
			crossed(ranges[v], func(k int) {
				c := &clobbers[k]
				if c.call {
					a.calls = true
					a.weight += c.weight
				}
				if int(cls) < len(c.masks) {
					a.avoid |= c.masks[cls]
				}
			})
			vacr[v] = a
			webs[v] = single(VReg(v))
		}
	}
	pairs := al.pairs[:0]
	for i := range f.Blocks {
		b := &f.Blocks[i]
		for _, p := range b.Phis {
			if webs[p.Def] == nil {
				continue
			}
			for k, a := range p.Args {
				if a != p.Def && alloc(a) && webs[a] != nil && f.Class[a] == f.Class[p.Def] {
					pairs = append(pairs, pair{p.Def, a, weightClass(f.Blocks[b.Preds[k]].Depth)})
				}
			}
		}
	}
	// The pairs by their edges' weight, the heaviest first, in order
	// within a weight: a counting sort by the weight's class.
	var count [numWeightClasses + 1]int
	for _, pr := range pairs {
		count[numWeightClasses-pr.class]++
	}
	for c := 1; c <= numWeightClasses; c++ {
		count[c] += count[c-1]
	}
	sorted := grown(al.sorted, len(pairs))
	for _, pr := range pairs {
		c := numWeightClasses - 1 - pr.class
		sorted[count[c]] = pr
		count[c]++
	}
	al.pairs, al.sorted = sorted, pairs
	pairs = sorted
	for _, pr := range pairs {
		x, y := webs[pr.a], webs[pr.b]
		if x == y || (x.fixed >= 0 && y.fixed >= 0) || overlap(x.ranges, y.ranges) {
			continue
		}
		if len(y.members) > len(x.members) {
			x, y = y, x
		}
		x.members = appendA(&al.vregs, x.members, y.members...)
		if !x.owned {
			x.ranges, x.owned = appendA(rngs, rngs.take(len(x.ranges)), x.ranges...), true
		}
		x.ranges = unionInto(rngs, x.ranges, y.ranges)
		x.weight += y.weight
		x.callWeight += y.callWeight
		x.calls = x.calls || y.calls
		x.avoid |= y.avoid
		if y.fixed >= 0 {
			x.fixed, x.arrive = y.fixed, y.arrive
		}
		for _, m := range y.members {
			webs[m] = x
		}
	}
	// Hints: a two-address definition wants its operand's register, a phi
	// its arguments' where they are not one web, and they its.
	webOf := func(v VReg) *web {
		if v < 0 {
			return nil
		}
		return webs[v]
	}
	for i := range f.Blocks {
		b := &f.Blocks[i]
		for _, p := range b.Phis {
			for _, a := range p.Args {
				if x, y := webOf(p.Def), webOf(a); x != nil && y != nil && x != y && x.cls == y.cls {
					x.hints = appendA(&al.hints, x.hints, y)
					y.hints = appendA(&al.hints, y.hints, x)
				}
			}
		}
		for j := range b.Insts {
			in := &b.Insts[j]
			if in.Hint == None || len(in.Defs) == 0 {
				continue
			}
			if x, y := webOf(in.Defs[0]), webOf(in.Hint); x != nil && y != nil && x != y && x.cls == y.cls {
				x.hints = appendA(&al.hints, x.hints, y)
			}
		}
	}
	list := al.list[:0]
	for v, w := range webs {
		if w != nil && w.members[0] == VReg(v) {
			list = append(list, w)
		}
	}
	slices.SortFunc(list, byStart)
	lap(PhaseWebs)

	// Registers: a linear scan of each class's webs, in order of their
	// starts. The active webs are live at the scan's position, the
	// inactive ones in a hole there: a web takes a register no active web
	// has and no inactive one meeting it has.
	// A web it would spill of more than one register is taken apart
	// instead, its registers scanned again each on its own, each hinted to
	// the others' registers: they may find registers in its holes.
	for cls, regs := range cfg.Classes {
		active, inactive := al.active[:0], al.inactive[:0]
		// The webs to scan: the class's, in order of their starts, and
		// the parts of webs taken apart, which start at or after the web
		// they were part of, in a heap. Each is next where it is the less.
		queue, parts := al.queue[:0], al.partQ[:0]
		for _, w := range list {
			if int(w.cls) == cls {
				queue = append(queue, w)
			}
		}
		qi := 0
		apart := func(w *web) bool {
			if len(w.members) < 2 {
				return false
			}
			ps := al.parts[:0]
			for _, m := range w.members {
				p := single(m)
				webs[m] = p
				ps = append(ps, p)
			}
			for _, p := range ps {
				p.hints = append(append(al.hints.take(len(w.hints)+len(ps)), w.hints...), ps...)
				heap.Push(&parts, p)
			}
			clear(ps)
			al.parts = ps[:0]
			return true
		}
		for qi < len(queue) || len(parts) > 0 {
			var cur *web
			if len(parts) > 0 && (qi == len(queue) || byStart(parts[0], queue[qi]) < 0) {
				cur = heap.Pop(&parts).(*web)
			} else {
				cur = queue[qi]
				qi++
			}
			if cur.calls && cur.callWeight >= cur.weight {
				// Its reloads after the calls it lives across would cost
				// more than its uses gain: it lives in its slot.
				if !apart(cur) {
					cur.spilled = true
				}
				continue
			}
			p := cur.start()
			back := al.back[:0]
			keep := active[:0]
			for _, a := range active {
				switch {
				case a.end() <= p:
				case a.covers(p):
					keep = append(keep, a)
				default:
					back = append(back, a)
				}
			}
			active = keep
			keepIn := inactive[:0]
			for _, a := range inactive {
				switch {
				case a.end() <= p:
				case a.covers(p):
					active = append(active, a)
				default:
					keepIn = append(keepIn, a)
				}
			}
			inactive = append(keepIn, back...)
			al.back = back
			used := cur.avoid
			for _, a := range active {
				used |= 1 << a.reg
			}
			for _, a := range inactive {
				if used&(1<<a.reg) == 0 && overlap(a.ranges[a.cur:], cur.ranges) {
					used |= 1 << a.reg
				}
			}
			reg := -1
			for _, h := range cur.hints {
				if h.reg >= 0 && used&(1<<h.reg) == 0 {
					reg = h.reg
					break
				}
			}
			if reg < 0 {
				for _, r := range regs {
					if used&(1<<r) == 0 {
						reg = r
						break
					}
				}
			}
			if reg < 0 {
				// No register: take the one whose webs in cur's way are
				// the least dense -- their weight over the positions they
				// cover -- if they are less dense than cur, spilling them.
				best, bestCost := -1, cur.density()
				for _, r := range regs {
					if cur.avoid&(1<<r) != 0 {
						continue
					}
					cost := 0.0
					for _, a := range active {
						if a.reg == r {
							cost = max(cost, a.density())
						}
					}
					for _, a := range inactive {
						if a.reg == r && overlap(a.ranges[a.cur:], cur.ranges) {
							cost = max(cost, a.density())
						}
					}
					if cost < bestCost {
						best, bestCost = r, cost
					}
				}
				if best < 0 {
					if !apart(cur) {
						cur.spilled = true
					}
					continue
				}
				evict := func(a *web) bool {
					if a.reg == best && overlap(a.ranges[a.cur:], cur.ranges) {
						a.reg, a.spilled = -1, true
						return true
					}
					return false
				}
				active = slices.DeleteFunc(active, evict)
				inactive = slices.DeleteFunc(inactive, evict)
				reg = best
			}
			cur.reg = reg
			active = append(active, cur)
		}
		clear(active)
		clear(inactive)
		clear(queue)
		al.active, al.inactive, al.queue, al.partQ = active[:0], inactive[:0], queue[:0], parts[:0]
	}
	lap(PhaseScan)

	// The webs as the scan left them, some taken apart.
	list = list[:0]
	for v, w := range webs {
		if w != nil && w.members[0] == VReg(v) {
			list = append(list, w)
		}
	}
	slices.SortFunc(list, byStart)
	al.list = list

	// Slots: for the webs spilled, and those in registers that live across
	// a call. A web with a parameter has the slot the parameter arrives in,
	// which holds nothing else from the function's start until then.
	slots := al.slots[:0]
	take := func(s int32, rs []rng) {
		for int(s) >= len(slots) {
			slots = append(slots, nil)
		}
		slots[s] = unionInto(rngs, slots[s], rs)
	}
	for _, w := range list {
		if w.fixed < 0 {
			continue
		}
		rs := append(rngs.take(1), rng{0, w.arrive + 1})
		if w.spilled || w.calls {
			rs = unionInto(rngs, rs, w.ranges)
			w.slot = w.fixed
		}
		take(w.fixed, rs)
	}
	for _, w := range list {
		if !(w.spilled || w.calls) || w.slot >= 0 {
			continue
		}
		s := int32(0)
		for int(s) < len(slots) && overlap(slots[s], w.ranges) {
			s++
		}
		if int(s) >= cfg.NumSlots {
			clear(slots)
			al.slots = slots[:0]
			return nil, ErrSlots
		}
		w.slot = s
		take(s, w.ranges)
	}
	nslots := len(slots)
	clear(slots)
	al.slots = slots[:0]

	res := &al.res
	res.Reg = grown(res.Reg, n)
	res.Slot = grown(res.Slot, n)
	res.NumSlots = nslots
	for i := range res.Reg {
		res.Reg[i] = -1
		res.Slot[i] = -1
	}
	across := al.across[:0]
	for _, w := range list {
		for _, m := range w.members {
			res.Reg[m] = int16(w.reg)
			res.Slot[m] = w.slot
		}
		if !w.calls || w.reg < 0 {
			continue
		}
		// The calls each of its registers lives across, which reload it.
		for _, m := range w.members {
			crossed(ranges[m], func(k int) {
				if clobbers[k].call {
					across = append(across, acrossEntry{int32(k), m})
				}
			})
		}
	}
	al.across = across
	// Each call's registers, in the order found, by the call's index
	// among the clobbers.
	res.acrossOff = grown(res.acrossOff, len(clobbers)+1)
	clear(res.acrossOff)
	for _, e := range across {
		res.acrossOff[e.call+1]++
	}
	for k := range clobbers {
		res.acrossOff[k+1] += res.acrossOff[k]
	}
	res.acrossV = grown(res.acrossV, len(across))
	next := al.next[:0]
	for k := range clobbers {
		next = append(next, res.acrossOff[k])
	}
	for _, e := range across {
		res.acrossV[next[e.call]] = e.v
		next[e.call]++
	}
	al.next = next[:0]
	res.acrossKey = res.acrossKey[:0]
	for k := range clobbers {
		res.acrossKey = append(res.acrossKey, clobbers[k].key)
	}
	clear(webs)
	clear(list)
	al.list = list[:0]
	lap(PhaseSlots)
	return res, nil
}

// byStart orders webs by their starts, then their first members.
func byStart(a, b *web) int {
	if a.start() != b.start() {
		return int(a.start() - b.start())
	}
	return int(a.members[0] - b.members[0])
}

// depthWeight is the weight of a use at loop depth d: 8 to the depth's
// power, up to 8 to the 7th.
func depthWeight(d int) float64 {
	w := 1.0
	for ; d > 0 && w < 1e6; d-- {
		w *= 8
	}
	return w
}

// numWeightClasses is the number of depthWeight's weights.
const numWeightClasses = 8

// weightClass is the class of depthWeight(d), which orders the weights.
func weightClass(d int) int { return min(max(d, 0), numWeightClasses-1) }

// webQueue is webs by their starts, the first first.
type webQueue []*web

func (q webQueue) Len() int { return len(q) }
func (q webQueue) Less(i, j int) bool {
	if a, b := q[i].start(), q[j].start(); a != b {
		return a < b
	}
	return q[i].members[0] < q[j].members[0]
}
func (q webQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *webQueue) Push(x any)   { *q = append(*q, x.(*web)) }
func (q *webQueue) Pop() any {
	old := *q
	w := old[len(old)-1]
	*q = old[:len(old)-1]
	return w
}

// density is w's weight over the positions it covers: a long web of few
// uses gains less from a register than a short one of as many.
func (w *web) density() float64 {
	if w.dense == 0 {
		n := int32(0)
		for _, r := range w.ranges {
			n += r.to - r.from
		}
		w.dense = w.weight / float64(n)
	}
	return w.dense
}

// unionInto merges b into a, both sorted, joining ranges that meet, in a's
// array where it has room, else in one of the arena's.
func unionInto(ar *arena[rng], a, b []rng) []rng {
	if len(b) == 0 {
		return a
	}
	if len(b)*8 < len(a) {
		// Few ranges into many: each merged where a search finds it, the
		// ranges it meets -- a's are apart, none meeting the next --
		// joined with it.
		lo := 0
		for _, r := range b {
			i := endsAfter(a, lo, r.from-1)
			j := i
			for j < len(a) && a[j].from <= r.to {
				j++
			}
			if i < j {
				a[i] = rng{min(a[i].from, r.from), max(a[j-1].to, r.to)}
				a = append(a[:i+1], a[j:]...)
			} else {
				if len(a) == cap(a) {
					a = append(ar.take(2*cap(a)+1), a...)
				}
				a = a[:len(a)+1]
				copy(a[i+1:], a[i:])
				a[i] = r
			}
			lo = i
		}
		return a
	}
	n := len(a)
	if n+len(b) > cap(a) {
		a = append(ar.take(max(2*cap(a), n+len(b))), a...)
	}
	a = a[:n+len(b)]
	i, j := n-1, len(b)-1
	for k := len(a) - 1; j >= 0; k-- {
		if i >= 0 && a[i].from > b[j].from {
			a[k] = a[i]
			i--
		} else {
			a[k] = b[j]
			j--
		}
	}
	out := a[:1]
	for _, r := range a[1:] {
		if l := &out[len(out)-1]; r.from <= l.to {
			l.to = max(l.to, r.to)
		} else {
			out = append(out, r)
		}
	}
	return out
}
