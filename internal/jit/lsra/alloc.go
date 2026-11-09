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

// union merges two sorted lists of ranges, joining those that meet.
func union(a, b []rng) []rng {
	out := make([]rng, 0, len(a)+len(b))
	add := func(r rng) {
		if l := len(out) - 1; l >= 0 && r.from <= out[l].to {
			out[l].to = max(out[l].to, r.to)
			return
		}
		out = append(out, r)
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		if j == len(b) || (i < len(a) && a[i].from < b[j].from) {
			add(a[i])
			i++
		} else {
			add(b[j])
			j++
		}
	}
	return out
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

// Allocate allocates f's virtual registers.
func Allocate(f *Func, cfg *Config) (*Result, error) {
	n := len(f.Class)
	nb := len(f.Blocks)
	alloc := func(v VReg) bool { return v >= 0 && f.Class[v] != NoClass }
	lap := func(p Phase) {
		if cfg.Lap != nil {
			cfg.Lap(p)
		}
	}

	// predIdx is, for each block's successor k, the block's index among
	// that successor's predecessors.
	predIdx := make([][]int, nb)
	for i := range f.Blocks {
		for _, s := range f.Blocks[i].Succs {
			predIdx[i] = append(predIdx[i], slices.Index(f.Blocks[s].Preds, i))
		}
	}

	// Liveness, of the registers that may be live across a block's
	// boundary -- a phi's, a phi's argument, one used in a block other
	// than its definition's -- numbered densely (global): the registers
	// live into and out of each block, over the blocks backwards until
	// nothing changes. A successor's phis read their arguments on the
	// edge, at the predecessor's end. The others are live within their
	// blocks alone, which the ranges' walk finds.
	defBlock := make([]int32, n)
	for i := range defBlock {
		defBlock[i] = -1
	}
	global := make([]int32, n)
	var globals []VReg
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
	// gi is v's global number, or -1.
	gi := func(v VReg) int { return int(global[v]) - 1 }
	words := (len(globals) + 63) / 64
	slab := make([]uint64, 4*nb*words)
	liveIn := make([][]uint64, nb)
	liveOut := make([][]uint64, nb)
	gen := make([][]uint64, nb)
	kill := make([][]uint64, nb)
	for i := range f.Blocks {
		b := &f.Blocks[i]
		s := slab[4*i*words : 4*(i+1)*words]
		liveIn[i], liveOut[i], gen[i], kill[i] = s[:words], s[words:2*words], s[2*words:3*words], s[3*words:]
		g, k := gen[i], kill[i]
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
			out := liveOut[i]
			for k, s := range b.Succs {
				for w := range out {
					nw := out[w] | liveIn[s][w]
					if nw != out[w] {
						out[w] = nw
						changed = true
					}
				}
				pi := predIdx[i][k]
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
			in := liveIn[i]
			for w := range in {
				nw := gen[i][w] | out[w]&^kill[i][w]
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
	ranges := make([][]rng, n)
	weight := make([]float64, n)
	defAt := make([]int32, n)
	var clobbers []clobber
	starts := make([]int32, nb)
	ends := make([]int32, nb)
	pos := int32(0)
	for i := range f.Blocks {
		starts[i] = pos
		ends[i] = pos + 2 + 2*int32(len(f.Blocks[i].Insts))
		pos = ends[i] + 2
	}
	// open is, for a register live in the block being walked, the index
	// of its range there plus one.
	open := make([]int32, n)
	var opened []VReg
	def := func(d VReg, at int32) {
		if o := open[d]; o > 0 {
			ranges[d][o-1].from = at
			open[d] = 0
		} else {
			ranges[d] = append(ranges[d], rng{at, at + 1})
		}
		defAt[d] = at
	}
	for i := nb - 1; i >= 0; i-- {
		b := &f.Blocks[i]
		start, end := starts[i], ends[i]
		w := depthWeight(b.Depth)
		opened = opened[:0]
		for wi, word := range liveOut[i] {
			for word != 0 {
				v := globals[wi*64+bits.TrailingZeros64(word)]
				word &= word - 1
				ranges[v] = append(ranges[v], rng{start, end + 1})
				open[v] = int32(len(ranges[v]))
				opened = append(opened, v)
			}
		}
		for k, s := range b.Succs {
			pi := predIdx[i][k]
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
					ranges[v] = append(ranges[v], rng{start, u + 1})
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
	slices.SortFunc(clobbers, func(a, b clobber) int { return int(a.at - b.at) })
	for v, r := range ranges {
		if len(r) < 2 {
			continue
		}
		slices.SortFunc(r, func(a, b rng) int { return int(a.from - b.from) })
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
	// crossed visits the clobbers a list of ranges lives across: those at
	// a position p where it is live at p and p+1.
	crossed := func(rs []rng, visit func(c *clobber)) {
		for _, r := range rs {
			k, _ := slices.BinarySearchFunc(clobbers, r.from, func(c clobber, p int32) int { return int(c.at - p) })
			for ; k < len(clobbers) && clobbers[k].at < r.to-1; k++ {
				visit(&clobbers[k])
			}
		}
	}

	// Webs: each register its own, then a phi's and an argument's joined
	// where their lifetimes do not meet and their parameters' slots allow,
	// the edges of the deepest blocks first.
	// single is v's web of its own.
	single := func(v VReg) *web {
		w := &web{members: []VReg{v}, cls: f.Class[v], ranges: ranges[v], weight: weight[v], fixed: -1, reg: -1, slot: -1}
		if f.FixedSlot[v] >= 0 {
			w.fixed, w.arrive = f.FixedSlot[v], defAt[v]
		}
		cls := w.cls
		crossed(w.ranges, func(c *clobber) {
			if c.call {
				w.calls = true
				w.callWeight += c.weight
			}
			if int(cls) < len(c.masks) {
				w.avoid |= c.masks[cls]
			}
		})
		return w
	}
	lap(PhaseRanges)
	webs := make([]*web, n)
	for v := range n {
		if alloc(VReg(v)) && len(ranges[v]) > 0 {
			webs[v] = single(VReg(v))
		}
	}
	type pair struct {
		a, b   VReg
		weight float64
	}
	var pairs []pair
	for i := range f.Blocks {
		b := &f.Blocks[i]
		for _, p := range b.Phis {
			if webs[p.Def] == nil {
				continue
			}
			for k, a := range p.Args {
				if a != p.Def && alloc(a) && webs[a] != nil && f.Class[a] == f.Class[p.Def] {
					pairs = append(pairs, pair{p.Def, a, depthWeight(f.Blocks[b.Preds[k]].Depth)})
				}
			}
		}
	}
	slices.SortStableFunc(pairs, func(x, y pair) int {
		switch {
		case x.weight > y.weight:
			return -1
		case x.weight < y.weight:
			return 1
		}
		return 0
	})
	for _, pr := range pairs {
		x, y := webs[pr.a], webs[pr.b]
		if x == y || (x.fixed >= 0 && y.fixed >= 0) || overlap(x.ranges, y.ranges) {
			continue
		}
		if len(y.members) > len(x.members) {
			x, y = y, x
		}
		x.members = append(x.members, y.members...)
		if !x.owned {
			x.ranges, x.owned = slices.Clone(x.ranges), true
		}
		x.ranges = unionInto(x.ranges, y.ranges)
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
					x.hints = append(x.hints, y)
					y.hints = append(y.hints, x)
				}
			}
		}
		for j := range b.Insts {
			in := &b.Insts[j]
			if in.Hint == None || len(in.Defs) == 0 {
				continue
			}
			if x, y := webOf(in.Defs[0]), webOf(in.Hint); x != nil && y != nil && x != y && x.cls == y.cls {
				x.hints = append(x.hints, y)
			}
		}
	}
	var list []*web
	for v, w := range webs {
		if w != nil && w.members[0] == VReg(v) {
			list = append(list, w)
		}
	}
	slices.SortFunc(list, func(a, b *web) int {
		if a.start() != b.start() {
			return int(a.start() - b.start())
		}
		return int(a.members[0] - b.members[0])
	})

	lap(PhaseWebs)

	// Registers: a linear scan of each class's webs, in order of their
	// starts. The active webs are live at the scan's position, the
	// inactive ones in a hole there: a web takes a register no active web
	// has and no inactive one meeting it has.
	// A web it would spill of more than one register is taken apart
	// instead, its registers scanned again each on its own, each hinted to
	// the others' registers: they may find registers in its holes.
	for cls, regs := range cfg.Classes {
		var active, inactive []*web
		var queue webQueue
		for _, w := range list {
			if int(w.cls) == cls {
				queue = append(queue, w)
			}
		}
		heap.Init(&queue)
		apart := func(w *web) bool {
			if len(w.members) < 2 {
				return false
			}
			parts := make([]*web, len(w.members))
			for i, m := range w.members {
				parts[i] = single(m)
				webs[m] = parts[i]
			}
			for _, p := range parts {
				p.hints = append(slices.Clone(w.hints), parts...)
				heap.Push(&queue, p)
			}
			return true
		}
		for queue.Len() > 0 {
			cur := heap.Pop(&queue).(*web)
			if cur.calls && cur.callWeight >= cur.weight {
				// Its reloads after the calls it lives across would cost
				// more than its uses gain: it lives in its slot.
				if !apart(cur) {
					cur.spilled = true
				}
				continue
			}
			p := cur.start()
			var back []*web
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
	}

	lap(PhaseScan)

	// The webs as the scan left them, some taken apart.
	list = list[:0]
	for v, w := range webs {
		if w != nil && w.members[0] == VReg(v) {
			list = append(list, w)
		}
	}
	slices.SortFunc(list, func(a, b *web) int {
		if a.start() != b.start() {
			return int(a.start() - b.start())
		}
		return int(a.members[0] - b.members[0])
	})

	// Slots: for the webs spilled, and those in registers that live across
	// a call. A web with a parameter has the slot the parameter arrives in,
	// which holds nothing else from the function's start until then.
	var slots [][]rng
	take := func(s int32, rs []rng) {
		for int(s) >= len(slots) {
			slots = append(slots, nil)
		}
		slots[s] = unionInto(slots[s], rs)
	}
	for _, w := range list {
		if w.fixed < 0 {
			continue
		}
		rs := []rng{{0, w.arrive + 1}}
		if w.spilled || w.calls {
			rs = union(rs, w.ranges)
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
			return nil, ErrSlots
		}
		w.slot = s
		take(s, w.ranges)
	}

	res := &Result{Reg: make([]int16, n), Slot: make([]int32, n), Across: map[[2]int32][]VReg{}, NumSlots: len(slots)}
	for i := range res.Reg {
		res.Reg[i] = -1
		res.Slot[i] = -1
	}
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
			crossed(ranges[m], func(c *clobber) {
				if c.call {
					res.Across[c.key] = append(res.Across[c.key], m)
				}
			})
		}
	}
	lap(PhaseSlots)
	return res, nil
}

// depthWeight is the weight of a use at loop depth d.
func depthWeight(d int) float64 {
	w := 1.0
	for ; d > 0 && w < 1e6; d-- {
		w *= 8
	}
	return w
}

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
// array where it has room.
func unionInto(a, b []rng) []rng {
	if len(b) == 0 {
		return a
	}
	n := len(a)
	a = slices.Grow(a, len(b))[:n+len(b)]
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
