package mir

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// blockLiveness is allocate's block-level liveness: for each value with a
// use in another block than its own (rec's xhead), lo, the first position
// of a block it is live into, and hi, the last position of a block it is
// live into (the block's start) or out of (its end), by value ID; -1 for
// the others. A value is live into each block from which a use of it in
// another block is reached without passing its own block, and out of each
// predecessor of such a block -- the graph's edges each laid-out block's
// predecessors that are laid out too (inOrder).
//
// It is a backward dataflow over the blocks, as the slot liveness is
// (compile's liveRegs): each block's set at its start found again from
// its successors' until none changes, the sets ir.LiveBuilder's
// persistent ones, which share what a block leaves as it is. The sets'
// extents are then read along the layout, each set against the one
// before it: a value's last position is where it first appears going
// back from the layout's end, its first where it first appears going
// forward -- what the sets change from block to block, not what they
// hold. A walk of each value's blocks back from its uses had taken as
// many steps as there are values live in each block, summed over the
// blocks: n values live across a loop of 2n blocks took 2n² steps.
func (c *core) blockLiveness(rec []useRec, xuses []xuse, inOrder []bool, start, end []int, nb int) (lo, hi []int) {
	nv := len(rec)
	lo, hi = c.ints(nv), c.ints(nv)
	for i := range lo {
		lo[i], hi[i] = -1, -1
	}
	// Each block's changes, by block ID (opAt): the values made in it
	// taken out, those used in it put in, in order of their IDs, which
	// Apply wants -- the two never of one value, whose uses in its own
	// block are no xuses.
	opAt := c.int32s(nb + 1)
	live := 0
	for id := range rec {
		r := &rec[id]
		if r.def <= 0 || r.xhead == 0 {
			continue
		}
		live++
		opAt[r.def]++
		for u := r.xhead; u != 0; u = xuses[u-1].next {
			opAt[xuses[u-1].b.ID+1]++
		}
	}
	if live == 0 {
		return lo, hi
	}
	for k := range nb {
		opAt[k+1] += opAt[k]
	}
	ops := c.liveOps(int(opAt[nb]))
	fill := c.int32s(nb)
	copy(fill, opAt[:nb])
	for id := range rec {
		r := &rec[id]
		if r.def <= 0 || r.xhead == 0 {
			continue
		}
		d := r.def - 1
		ops[fill[d]] = ir.LiveOp{Slot: int32(id), Clear: true}
		fill[d]++
		for u := r.xhead; u != 0; u = xuses[u-1].next {
			b := xuses[u-1].b.ID
			ops[fill[b]] = ir.LiveOp{Slot: int32(id)}
			fill[b]++
		}
	}
	// The successors, by block ID: the laid-out blocks each laid-out
	// block is a predecessor of.
	succAt := c.int32s(nb + 1)
	for _, b := range c.order {
		for _, p := range b.Preds {
			if inOrder[p.ID] {
				succAt[p.ID+1]++
			}
		}
	}
	for k := range nb {
		succAt[k+1] += succAt[k]
	}
	succ := c.blockList(int(succAt[nb]))
	copy(fill, succAt[:nb])
	for _, b := range c.order {
		for _, p := range b.Preds {
			if inOrder[p.ID] {
				succ[fill[p.ID]] = b
				fill[p.ID]++
			}
		}
	}
	lb := c.liveBuilder(nv)
	in, out := c.liveSets(nb), c.liveSets(nb)
	outOf := func(b *ssa.Block) ir.LiveSet {
		s := lb.Empty()
		for _, t := range succ[succAt[b.ID]:succAt[b.ID+1]] {
			s = lb.Union(s, in[t.ID])
		}
		return s
	}
	dirty := c.bools(nb)
	for _, b := range c.order {
		dirty[b.ID] = true
	}
	for changed := true; changed; {
		changed = false
		for i := len(c.order) - 1; i >= 0; i-- {
			b := c.order[i]
			if !dirty[b.ID] {
				continue
			}
			dirty[b.ID] = false
			// The edit's nodes are this set's alone, which its
			// operations change in place.
			lb.Edit()
			s := lb.Apply(outOf(b), ops[opAt[b.ID]:opAt[b.ID+1]])
			if lb.Equal(s, in[b.ID]) {
				continue
			}
			in[b.ID], changed = s, true
			for _, p := range b.Preds {
				if inOrder[p.ID] {
					dirty[p.ID] = true
				}
			}
		}
	}
	for _, b := range c.order {
		lb.Edit()
		out[b.ID] = outOf(b)
	}
	// Back from the layout's end, each block's end and then its start:
	// a value's last position is the first met where it is live.
	prev := lb.Empty()
	for i := len(c.order) - 1; i >= 0; i-- {
		b := c.order[i]
		for _, at := range [2]struct {
			s ir.LiveSet
			p int
		}{{out[b.ID], end[b.ID]}, {in[b.ID], start[b.ID]}} {
			lb.Added(prev, at.s, func(id int) bool {
				if hi[id] < 0 {
					hi[id] = at.p
				}
				return true
			})
			prev = at.s
		}
	}
	// Forward, each block's start: its first.
	prev = lb.Empty()
	for _, b := range c.order {
		p := start[b.ID]
		lb.Added(prev, in[b.ID], func(id int) bool {
			if lo[id] < 0 {
				lo[id] = p
			}
			return true
		})
		prev = in[b.ID]
	}
	return lo, hi
}
