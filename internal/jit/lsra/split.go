package lsra

import "slices"

// Splitting. Where the scan takes a register from webs that meet the web
// it scans (an eviction), it punches a hole in each instead of spilling it
// whole, where its holes with the new one cost less than its uses (and,
// without Config.Displace, the new holes less than spilling the web
// scanned): the
// evicted web keeps its register but over its holes, where it lives in its
// slot. A hole covers the positions where the two are live, and may be
// widened, between the web's last use or definition before and its first
// after, to where a block of a lower loop depth begins, or a loop's header
// -- a loop's entry or exit -- or to a hole it has already, so that the
// moves into and out of it go where they run least. Its cost is its web's
// uses and definitions in it, each now a slot's, and those moves: a store
// where the web goes into it in its register -- none for a web stored
// where it is defined (one that lives across a call) -- and a load where
// it comes out, at a position within a block or on each edge that crosses
// its bounds, each weighed by its loop depth.
//
// A hole's bounds are positions where moves can go: a block's start --
// the moves on the edges into it -- or an instruction's start, before it
// reads its operands. Within a block at most one of a web's registers
// lives across a bound, its members never meeting.

// spillWhole spills w for its whole life, its holes gone.
func (w *web) spillWhole() {
	w.reg, w.spilled = -1, true
	if w.full != nil {
		w.ranges, w.holes, w.full = w.full, nil, nil
	}
}

// blockOf is the block position x is in, once events has made its table.
func (al *Allocator) blockOf(x int32) int { return int(al.posBlock[x>>1]) }

// The bounds within a block are its start and the even positions from its
// start plus two to its end less two, its instructions' starts.

// floorBound is the last bound at or before x.
func (al *Allocator) floorBound(x int32) int32 {
	b := al.blockOf(x)
	if x >= al.ends[b]-1 {
		return al.ends[b] - 2
	}
	return x &^ 1
}

// ceilBound is the first bound at or after x, or maxPos.
func (al *Allocator) ceilBound(x int32) int32 {
	if x >= al.maxPos {
		return al.maxPos
	}
	b := al.blockOf(x)
	if c := (x + 1) &^ 1; c <= al.ends[b]-2 {
		return c
	}
	if b+1 < len(al.starts) {
		return al.starts[b+1]
	}
	return al.maxPos
}

// isGap reports whether bound x is an instruction's start, not a block's.
func (al *Allocator) isGap(x int32) bool {
	return x > 0 && x < al.maxPos && x != al.starts[al.blockOf(x)]
}

// gapKey is the block and index of the instruction that starts at x.
func (al *Allocator) gapKey(x int32) [2]int32 {
	b := al.blockOf(x)
	return [2]int32{int32(b), (x - al.starts[b] - 2) / 2}
}

// liveAt reports whether a list of ranges covers x.
func liveAt(rs []rng, x int32) bool {
	i := endsAfter(rs, 0, x)
	return i < len(rs) && rs[i].from <= x
}

// An event is a use or a definition of a register: its position times
// four, plus two for a definition, plus one where it weighs nothing -- a
// phi's argument of its phi's web or a phi all of whose arguments are, which
// move nowhere, or a definition of a web stored where its registers are
// defined (Allocate's weights).
const (
	evDef  = 2
	evFree = 1
)

// evPos is event e's position.
func evPos(e int32) int32 { return e >> 2 }

// firstEvent is the index of the first of evs at or past position x.
func firstEvent(evs []int32, x int32) int {
	k := x << 2
	lo, hi := 0, len(evs)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if evs[m] < k {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

// events finds each register's uses and definitions by position, and the
// block of each position (blockOf), the first time an allocation needs
// them.
func (al *Allocator) events() {
	if al.evKnown {
		return
	}
	al.evKnown = true
	f := al.f
	n := len(f.Class)
	pb := grown(al.posBlock, int(al.maxPos/2)+1)
	bw := grown(al.blockW, len(f.Blocks))
	for i := range f.Blocks {
		for x := al.starts[i] >> 1; x <= al.ends[i]>>1; x++ {
			pb[x] = int32(i)
		}
		bw[i] = depthWeight(f.Blocks[i].Depth)
	}
	pb[len(pb)-1] = int32(len(f.Blocks) - 1)
	al.posBlock, al.blockW = pb, bw
	off := grown(al.evOff, n+1)
	clear(off)
	al.eventPass(off, nil)
	for v := range n {
		off[v+1] += off[v]
	}
	ev := grown(al.ev, int(off[n]))
	next := grown(al.evNext, n)
	copy(next, off[:n])
	al.eventPass(next, ev)
	al.ev, al.evOff, al.evNext = ev, off, next
}

// eventPass goes over each use and definition of a register in order of
// position, as events -- a phi's at its block's start, an argument's use
// at its edge's predecessor's end, an instruction's uses at its start and
// its definitions one after -- free as the webs are when it runs. With ev
// nil, it counts each register's in at[v+1]; else it puts each in ev at
// at[v], and moves at[v] past it.
func (al *Allocator) eventPass(at []int32, ev []int32) {
	f := al.f
	webs := al.webs
	// phiFree reports whether every argument of phi p is its web's.
	phiFree := func(p *Phi) bool {
		x := webs[p.Def]
		for _, a := range p.Args {
			if a >= 0 && f.Class[a] != NoClass && webs[a] != x {
				return false
			}
		}
		return x != nil && len(x.members) > 1
	}
	put := func(v VReg, p int32) {
		if ev == nil {
			at[v+1]++
			return
		}
		ev[at[v]] = p
		at[v]++
	}
	for i := range f.Blocks {
		b := &f.Blocks[i]
		start := al.starts[i]
		for k := range b.Phis {
			if d := b.Phis[k].Def; d >= 0 && f.Class[d] != NoClass {
				e := start<<2 | evDef
				if ev != nil && phiFree(&b.Phis[k]) {
					e |= evFree
				}
				put(d, e)
			}
		}
		for j := range b.Insts {
			in := &b.Insts[j]
			u := start + 2 + 2*int32(j)
			for _, v := range in.Uses {
				if v >= 0 && f.Class[v] != NoClass {
					put(v, u<<2)
				}
			}
			for _, d := range in.Defs {
				if d >= 0 && f.Class[d] != NoClass {
					e := (u+1)<<2 | evDef
					if ev != nil && webs[d] != nil && webs[d].calls {
						e |= evFree
					}
					put(d, e)
				}
			}
		}
		for k, s := range b.Succs {
			pi := al.predIdx[int(al.predOff[i])+k]
			for _, ph := range f.Blocks[s].Phis {
				if a := ph.Args[pi]; a >= 0 && f.Class[a] != NoClass {
					e := al.ends[i] << 2
					if ev != nil && webs[a] != nil && webs[a] == webs[ph.Def] {
						e |= evFree
					}
					put(a, e)
				}
			}
		}
	}
}

// posWeight is the weight of a use or a move at position x.
func (al *Allocator) posWeight(x int32) float64 { return al.blockW[al.blockOf(x)] }

// webEvents makes w's uses and definitions, its members', in order of
// position, as events numbers them, and evW, the sums of the weights of
// those before each and of all, the first time w needs them.
func (al *Allocator) webEvents(w *web) {
	if w.evW != nil {
		return
	}
	if len(w.members) == 1 {
		m := w.members[0]
		w.ev = al.ev[al.evOff[m]:al.evOff[m+1]]
	} else {
		n := 0
		for _, m := range w.members {
			n += int(al.evOff[m+1] - al.evOff[m])
		}
		ev := al.i32s.take(n)
		for _, m := range w.members {
			ev = append(ev, al.ev[al.evOff[m]:al.evOff[m+1]]...)
		}
		slices.Sort(ev)
		w.ev = ev
	}
	evW := append(al.f64s.take(len(w.ev)+1), 0)
	for _, e := range w.ev {
		x := 0.0
		if e&evFree == 0 {
			x = al.posWeight(evPos(e))
		}
		evW = append(evW, evW[len(evW)-1]+x)
	}
	w.evW = evW
}

// meetSpan is from the first position two lists of ranges both cover to
// past the last, from > to where they never meet.
func meetSpan(a, b []rng) (from, to int32) {
	from, to = 1, 0
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		lo, hi := max(a[i].from, b[j].from), min(a[i].to, b[j].to)
		if lo < hi {
			if from > to {
				from = lo
			}
			to = hi
		}
		if a[i].to < b[j].to {
			i++
		} else {
			j++
		}
	}
	return from, to
}

// inHoles reports whether x is in one of the holes.
func inHoles(holes []rng, x int32) bool {
	return liveAt(holes, x)
}

// maxBoundSteps bounds the blocks a hole's bound is moved over to find a
// cheaper one.
const maxBoundSteps = 64

// hole is a hole to punch in a web over [s, e), and what it costs. Nothing
// compares holes: the field keeps the compiler from making an equality
// function for them.
type hole struct {
	_    [0]func()
	s, e int32
	cost float64
}

// plan finds the hole to punch in a's register where cur meets it, or no
// hole (an empty one) where its holes with it would cost as much as
// spilling a whole.
func (al *Allocator) plan(a, cur *web) hole {
	al.events()
	f := al.f
	full := a.ranges
	if a.full != nil {
		full = a.full
	}
	c0, c1 := meetSpan(a.ranges, cur.ranges)
	if c0 >= c1 {
		return hole{}
	}
	// The holes it has before and after the span, and its last use or
	// definition before the span and its first after.
	hp, hn := int32(-1), al.maxPos
	for _, h := range a.holes {
		if h.to <= c0 {
			hp = h.to
		} else if h.from >= c1 {
			hn = h.from
			break
		}
	}
	al.webEvents(a)
	last, next := int32(-1), al.maxPos
	i := firstEvent(a.ev, c0)
	if i > 0 {
		last = evPos(a.ev[i-1])
	}
	if j := i + firstEvent(a.ev[i:], c1); j < len(a.ev) {
		next = evPos(a.ev[j])
	}
	// The bounds: where the span begins and ends, or as far as the hole
	// before or after where nothing of a's is between, else a block's
	// start of a lower depth, or a loop's header, between a's uses.
	s, e := al.floorBound(c0), al.ceilBound(c1)
	if hp >= 0 && last < hp {
		s = hp
	}
	depth := func(x int32) int { return f.Blocks[al.blockOf(x)].Depth }
	cost := al.holeCost(a, full, s, e)
	if s != hp {
		md := depth(s)
		bi := al.blockOf(s)
		if al.starts[bi] == s {
			bi--
		}
		for steps := 0; bi >= 0 && steps < maxBoundSteps; bi, steps = bi-1, steps+1 {
			x := al.starts[bi]
			if x <= last || x < hp {
				break
			}
			d := f.Blocks[bi].Depth
			header := d <= md && slices.ContainsFunc(f.Blocks[bi].Preds, func(p int) bool { return p >= bi })
			if d >= md && !header {
				continue
			}
			md = min(md, d)
			if c := al.holeCost(a, full, x, e); c < cost {
				s, cost = x, c
			}
		}
	}
	if hn < al.maxPos && next >= hn {
		if c := al.holeCost(a, full, s, hn); c <= cost {
			e, cost = hn, c
		}
	} else if e < al.maxPos {
		md := depth(e)
		bi := al.blockOf(e) + 1
		for steps := 0; steps < maxBoundSteps; bi, steps = bi+1, steps+1 {
			x := al.maxPos
			if bi < len(al.starts) {
				x = al.starts[bi]
			}
			if x > next || x > hn {
				break
			}
			if x < al.maxPos {
				d := f.Blocks[bi].Depth
				if d >= md {
					continue
				}
				md = d
			}
			if c := al.holeCost(a, full, s, x); c < cost {
				e, cost = x, c
			}
			if x == al.maxPos {
				break
			}
		}
	}
	if al.mutateHoles && c1-c0 > 2 {
		// The hole leaves the span's last position out.
		e = c1 - 1
	}
	// Spilled whole, a's uses outside its holes go to its slot too, and
	// its holes' moves go: its holes, this one with them, must cost less
	// than all of its uses. A hole over all that is left of its register
	// is a spill too.
	if s <= a.start() && e >= a.end() || a.holeW+a.moveW+cost >= a.weight && !al.stress {
		return hole{}
	}
	return hole{s: s, e: e, cost: cost}
}

// punch punches hole h in a's register.
func (al *Allocator) punch(a *web, h hole) {
	full := a.ranges
	if a.full != nil {
		full = a.full
	}
	evW := al.eventWeight(a, h.s, h.e)
	// A web punched before owns its holes and its ranges, which change in
	// their arrays where those have room.
	var holes, out []rng
	if a.full == nil {
		holes = al.rngs.take(len(a.holes) + 1)
		holes = append(holes, a.holes...)
	} else {
		holes, out = a.holes, a.ranges
	}
	holes = unionInto(&al.rngs, holes, []rng{{h.s, h.e}})
	if cap(out) < len(full)+len(holes) {
		out = al.rngs.take(len(full) + len(holes))
	}
	a.full, a.holes, a.ranges = full, holes, subtractInto(out[:0], full, holes)
	a.holeW += evW
	a.moveW += h.cost - evW
	a.dense = 0
}

// eventWeight is the weight of a's uses and definitions in [s, e) but in
// its holes.
func (al *Allocator) eventWeight(a *web, s, e int32) float64 {
	sum := func(s, e int32) float64 { return a.evW[firstEvent(a.ev, e)] - a.evW[firstEvent(a.ev, s)] }
	w := sum(s, e)
	for _, h := range a.holes {
		if h.from < e && h.to > s {
			w -= sum(max(h.from, s), min(h.to, e))
		}
	}
	return w
}

// holeCost is what a hole over [s, e) in a, whose ranges are full, adds:
// a's uses and definitions in it, and the moves into and out of a's holes
// it adds, less those it takes away where it meets one.
func (al *Allocator) holeCost(a *web, full []rng, s, e int32) float64 {
	f := al.f
	in := func(x int32) bool { return x >= s && x < e }
	old := func(x int32) bool { return inHoles(a.holes, x) }
	// move is the cost of a move from where a is at x0 to where it is at
	// x1, w each.
	move := func(in0, in1 bool, w float64) float64 {
		switch {
		case in0 == in1:
			return 0
		case in1 && a.calls:
			// Its slot holds it already.
			return 0
		}
		return w
	}
	cost := al.eventWeight(a, s, e)
	cross := func(x0, x1 int32, w float64) {
		if liveAt(full, x0) && liveAt(full, x1) {
			cost += move(in(x0) || old(x0), in(x1) || old(x1), w) - move(old(x0), old(x1), w)
		}
	}
	for _, x := range [2]int32{s, e} {
		if al.isGap(x) {
			cross(x-1, x, al.posWeight(x))
		}
	}
	edge := func(p, b int) {
		bp := &f.Blocks[p]
		d := bp.Depth
		if len(bp.Succs) > 1 {
			d = f.Blocks[b].Depth
		}
		cross(al.ends[p], al.starts[b], depthWeight(d))
	}
	last := al.blockOf(min(e, al.maxPos) - 1)
	for b := al.blockOf(s); b <= last; b++ {
		if in(al.starts[b]) {
			for _, p := range f.Blocks[b].Preds {
				if !in(al.ends[p]) {
					edge(p, b)
				}
			}
		}
		if in(al.ends[b]) {
			for _, sc := range f.Blocks[b].Succs {
				if !in(al.starts[sc]) {
					edge(b, sc)
				}
			}
		}
	}
	return cost
}

// subtractInto appends to out the ranges of a not in b, both in order:
// out has room for len(a)+len(b) more, and is neither's.
func subtractInto(out, a, b []rng) []rng {
	j := 0
	for _, r := range a {
		for j < len(b) && b[j].to <= r.from {
			j++
		}
		from := r.from
		for k := j; k < len(b) && b[k].from < r.to; k++ {
			if b[k].from > from {
				out = append(out, rng{from, b[k].from})
			}
			from = max(from, b[k].to)
		}
		if from < r.to {
			out = append(out, rng{from, r.to})
		}
	}
	return out
}

// slotRanges are where w needs its slot: its whole life where it is
// spilled or stored where it is defined, else its holes -- from the
// position before each, where it is stored, to the one past, where it is
// loaded.
func (al *Allocator) slotRanges(w *web) []rng {
	switch {
	case w.full == nil:
		return w.ranges
	case w.calls:
		return w.full
	}
	out := al.rngs.take(len(w.full) + len(w.holes))
	for _, r := range w.full {
		for _, h := range w.holes {
			lo, hi := max(r.from, h.from-1), min(r.to, h.to+1)
			if lo >= hi {
				continue
			}
			if n := len(out); n > 0 && out[n-1].to >= lo {
				out[n-1].to = max(out[n-1].to, hi)
				continue
			}
			out = append(out, rng{lo, hi})
		}
	}
	return out
}

// splitMoves makes the split registers' moves (Result.Moves, Entry and
// Edge) from the webs with holes in list, of registers whose ranges are
// ranges.
func (al *Allocator) splitMoves(res *Result, list []*web, ranges [][]rng) {
	f := al.f
	nb := len(f.Blocks)
	keyed := al.keyed[:0]
	// Into and out of the holes within blocks, by the instruction's start
	// times two, then the definitions, by it times two plus one.
	for _, w := range list {
		if len(w.holes) == 0 {
			continue
		}
		// At a bound within a block one member at most lives across it:
		// the web's ranges say whether one does.
		across := func(x int32) VReg {
			if !al.isGap(x) || !liveAt(w.full, x-1) || !liveAt(w.full, x) {
				return None
			}
			for _, m := range w.members {
				if rs := ranges[m]; liveAt(rs, x-1) && liveAt(rs, x) {
					return m
				}
			}
			return None
		}
		for _, h := range w.holes {
			if m := across(h.from); m != None {
				keyed = append(keyed, keyedMove{[2]int32{2 * h.from}, Move{V: m, Reg: -1, Copy: !w.calls}})
			}
			if m := across(h.to); m != None {
				keyed = append(keyed, keyedMove{[2]int32{2 * h.to}, Move{V: m, Reg: int16(w.reg), Copy: true}})
			}
		}
		for _, m := range w.members {
			for _, ev := range al.ev[al.evOff[m]:al.evOff[m+1]] {
				// An instruction's definition, at an odd position: a phi's
				// is in Entry.
				if x := evPos(ev); ev&evDef != 0 && x&1 != 0 {
					keyed = append(keyed, keyedMove{[2]int32{2*(x-1) + 1}, Move{V: m, Reg: w.where(x)}})
				}
			}
		}
	}
	slices.SortFunc(keyed, func(x, y keyedMove) int { return int(x.key[0] - y.key[0]) })
	for i := range keyed {
		x := keyed[i].key[0] >> 1
		b := al.blockOf(x)
		keyed[i].key = [2]int32{int32(b), (x - al.starts[b] - 2) / 2}
	}
	res.gap.fill(keyed, nb)
	// Where each is as its blocks begin, from its ranges -- one a block --
	// that begin where a block does, and its moves on the edges, from
	// those that end past a block's end.
	keyed, entries := keyed[:0], al.entries[:0]
	for _, w := range list {
		if len(w.holes) == 0 {
			continue
		}
		for _, m := range w.members {
			rs := ranges[m]
			for _, r := range rs {
				b := al.blockOf(r.from)
				if r.from == al.starts[b] {
					entries = append(entries, keyedMove{[2]int32{int32(b)}, Move{V: m, Reg: w.where(r.from)}})
				}
				if r.to != al.ends[b]+1 {
					continue
				}
				in0 := inHoles(w.holes, al.ends[b])
				for k, sc := range f.Blocks[b].Succs {
					x := al.starts[sc]
					if in0 == inHoles(w.holes, x) || !liveAt(rs, x) || slices.ContainsFunc(f.Blocks[sc].Phis, func(p Phi) bool { return p.Def == m }) {
						continue
					}
					switch {
					case !in0:
						if !w.calls {
							keyed = append(keyed, keyedMove{[2]int32{int32(b), int32(k)}, Move{V: m, Reg: -1, Copy: true}})
						}
					default:
						keyed = append(keyed, keyedMove{[2]int32{int32(b), int32(k)}, Move{V: m, Reg: int16(w.reg), Copy: true}})
					}
				}
			}
		}
	}
	slices.SortFunc(keyed, func(x, y keyedMove) int { return cmpKey(x.key, y.key) })
	res.edge.fill(keyed, nb)
	// The entries by block: counted, then put.
	off := grown(res.entryOff, nb+1)
	clear(off)
	for _, e := range entries {
		off[e.key[0]+1]++
	}
	for b := range nb {
		off[b+1] += off[b]
	}
	mv := grown(res.entryMv, len(entries))
	next := grown(al.evNext, nb)
	copy(next, off[:nb])
	for _, e := range entries {
		mv[next[e.key[0]]] = e.mv
		next[e.key[0]]++
	}
	res.entryOff, res.entryMv, al.evNext = off, mv, next
	clear(keyed)
	al.keyed, al.entries = keyed[:0], entries[:0]
}

// where is w's register at x, or -1 where x is in a hole.
func (w *web) where(x int32) int16 {
	if inHoles(w.holes, x) {
		return -1
	}
	return int16(w.reg)
}

// fill sets the table to the moves keyed, in order of their keys, whose
// first numbers are blocks below nb.
func (t *moveTable) fill(keyed []keyedMove, nb int) {
	t.key, t.off, t.mv = t.key[:0], t.off[:0], t.mv[:0]
	t.block = grown(t.block, nb+1)
	b := 0
	for i, km := range keyed {
		if i == 0 || km.key != keyed[i-1].key {
			for ; b <= int(km.key[0]); b++ {
				t.block[b] = int32(len(t.key))
			}
			t.key = append(t.key, km.key)
			t.off = append(t.off, int32(len(t.mv)))
		}
		t.mv = append(t.mv, km.mv)
	}
	t.off = append(t.off, int32(len(t.mv)))
	for ; b <= nb; b++ {
		t.block[b] = int32(len(t.key))
	}
}
