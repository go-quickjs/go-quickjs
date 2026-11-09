package opt

import "slices"

// removeUnreachable removes the blocks the entry does not reach, and their
// edges to the blocks it does, with the phis' arguments for those edges.
// The blocks left are in reverse postorder (order).
func removeUnreachable(f *Func) {
	reach := make([]bool, f.nextBlock)
	reach[f.Entry.ID] = true
	work := []*Block{f.Entry}
	for len(work) > 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]
		for _, s := range b.Succs {
			if !reach[s.ID] {
				reach[s.ID] = true
				work = append(work, s)
			}
		}
	}
	for _, b := range f.Blocks {
		if !reach[b.ID] {
			b.dead = true
			continue
		}
		for i := 0; i < len(b.Preds); i++ {
			if !reach[b.Preds[i].ID] {
				removePred(b, i)
				i--
			}
		}
	}
	order(f)
}

// removePred removes b's predecessor i, and its phis' argument i.
func removePred(b *Block, i int) {
	b.Preds = slices.Delete(b.Preds, i, i+1)
	for _, p := range b.Phis {
		p.Args = slices.Delete(p.Args, i, i+1)
	}
}

// removeEdge removes the edge from b to its successor i.
func removeEdge(b *Block, i int) {
	s := b.Succs[i]
	b.Succs = slices.Delete(b.Succs, i, i+1)
	if j := s.predIndex(b); j >= 0 {
		removePred(s, j)
	}
}

// order lays the reachable blocks out in reverse postorder, an if's then
// branch before its else: every block after its dominators, a loop's body
// after its header.
func order(f *Func) {
	seen := make([]bool, f.nextBlock)
	post := make([]*Block, 0, len(f.Blocks))
	type frame struct {
		b *Block
		i int
	}
	stack := []frame{{f.Entry, len(f.Entry.Succs)}}
	seen[f.Entry.ID] = true
	for len(stack) > 0 {
		t := &stack[len(stack)-1]
		if t.i == 0 {
			post = append(post, t.b)
			stack = stack[:len(stack)-1]
			continue
		}
		t.i--
		// The successors are visited last first, so that the first comes
		// first in reverse postorder.
		s := t.b.Succs[t.i]
		if !seen[s.ID] {
			seen[s.ID] = true
			stack = append(stack, frame{s, len(s.Succs)})
		}
	}
	slices.Reverse(post)
	f.Blocks = post
}

// splitCriticalEdges puts a block on every edge from a block with more
// than one successor to a block with more than one predecessor, so that
// the moves of an edge's phis have a block of their own.
func splitCriticalEdges(f *Func) {
	for _, b := range slices.Clone(f.Blocks) {
		if len(b.Succs) < 2 {
			continue
		}
		for i, s := range b.Succs {
			if len(s.Preds) < 2 {
				continue
			}
			e := f.newBlock()
			e.sealed = true
			e.Kind = BPlain
			e.Succs = []*Block{s}
			e.Preds = []*Block{b}
			b.Succs[i] = e
			s.Preds[s.predIndex(b)] = e
		}
	}
	order(f)
}

// dominators computes each block's immediate dominator, by Cooper, Harvey
// and Kennedy's "A Simple, Fast Dominance Algorithm", over f's blocks in
// reverse postorder. idom is indexed by block ID; the entry's is itself.
type domTree struct {
	idom     []*Block
	rpo      []int32
	children [][]*Block
}

func dominators(f *Func) *domTree {
	n := f.nextBlock
	d := &domTree{idom: make([]*Block, n), rpo: make([]int32, n), children: make([][]*Block, n)}
	for i, b := range f.Blocks {
		d.rpo[b.ID] = int32(i)
	}
	d.idom[f.Entry.ID] = f.Entry
	for changed := true; changed; {
		changed = false
		for _, b := range f.Blocks[1:] {
			var nd *Block
			for _, p := range b.Preds {
				if d.idom[p.ID] == nil {
					continue
				}
				if nd == nil {
					nd = p
				} else {
					nd = d.intersect(p, nd)
				}
			}
			if d.idom[b.ID] != nd {
				d.idom[b.ID] = nd
				changed = true
			}
		}
	}
	for _, b := range f.Blocks[1:] {
		p := d.idom[b.ID]
		d.children[p.ID] = append(d.children[p.ID], b)
	}
	return d
}

func (d *domTree) intersect(a, b *Block) *Block {
	for a != b {
		for d.rpo[a.ID] > d.rpo[b.ID] {
			a = d.idom[a.ID]
		}
		for d.rpo[b.ID] > d.rpo[a.ID] {
			b = d.idom[b.ID]
		}
	}
	return a
}

// dominates reports whether a dominates b.
func (d *domTree) dominates(a, b *Block) bool {
	for {
		if a == b {
			return true
		}
		p := d.idom[b.ID]
		if p == b {
			return false
		}
		b = p
	}
}

// findLoops sets each block's innermost natural loop and its depth: a
// loop is a header and the blocks that reach a back edge to it -- an edge
// from a block it dominates -- without passing through it.
func findLoops(f *Func, d *domTree) {
	type loop struct {
		header *Block
		blocks []*Block
	}
	var loops []loop
	for _, h := range f.Blocks {
		h.Loop, h.Depth, h.outer = nil, 0, nil
	}
	for _, h := range f.Blocks {
		var body []*Block
		in := map[*Block]bool{h: true}
		for _, p := range h.Preds {
			if !d.dominates(h, p) {
				continue
			}
			work := []*Block{p}
			for len(work) > 0 {
				b := work[len(work)-1]
				work = work[:len(work)-1]
				if in[b] {
					continue
				}
				in[b] = true
				body = append(body, b)
				work = append(work, b.Preds...)
			}
		}
		if len(in) > 1 || slices.Contains(h.Preds, h) {
			loops = append(loops, loop{h, append(body, h)})
		}
	}
	// Outer loops first, so that an inner loop's header is its blocks'
	// innermost.
	slices.SortStableFunc(loops, func(a, b loop) int { return len(b.blocks) - len(a.blocks) })
	for _, l := range loops {
		l.header.outer = l.header.Loop
		for _, b := range l.blocks {
			b.Loop = l.header
			b.Depth++
		}
	}
}

// computeUses counts each value's uses.
func computeUses(f *Func) {
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			v.Uses = 0
		}
		for _, v := range b.Values {
			v.Uses = 0
		}
	}
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			for _, a := range v.Args {
				a.Uses++
			}
		}
		for _, v := range b.Values {
			for _, a := range v.Args {
				a.Uses++
			}
		}
		if b.Control != nil {
			b.Control.Uses++
		}
		for _, a := range b.Results {
			a.Uses++
		}
	}
}

// loopDepths sets each block's depth to the number of loops it is in, by
// a loop nesting forest as Steensgaard's: a loop is a strongly connected
// component of the control flow graph, its entries the blocks of it that
// an edge from outside reaches, and the loops within it those of the
// component without its edges to the entries. Unlike natural loops it
// counts the loops of an irreducible graph, which jump threading makes of
// a dispatch loop; the allocator weighs a use by its block's depth.
//
// The edges of a switch of more than two targets enter a loop only where
// nothing else does: a dispatch loop's switch, threaded past, enters each
// case, and a loop of cases is entered too from the cases before it.
func loopDepths(f *Func) {
	for _, b := range f.Blocks {
		b.Depth = 0
	}
	type edge struct{ from, to int32 }
	cut := map[edge]bool{}
	member := make([]int32, f.nextBlock)
	gen := int32(0)
	var nest func(blocks []*Block)
	nest = func(blocks []*Block) {
		gen++
		for _, b := range blocks {
			member[b.ID] = gen
		}
		g := gen
		in := func(from, to *Block) bool {
			return member[to.ID] == g && !cut[edge{from.ID, to.ID}]
		}
		for _, scc := range components(blocks, in) {
			if len(scc) == 1 && !slices.ContainsFunc(scc[0].Succs, func(s *Block) bool { return s == scc[0] && in(s, s) }) {
				continue
			}
			inSCC := make(map[*Block]bool, len(scc))
			for _, b := range scc {
				inSCC[b] = true
				b.Depth++
			}
			var entries, switched []*Block
			for _, b := range scc {
				plain, sw := false, false
				for _, p := range b.Preds {
					switch {
					case inSCC[p]:
					case fromSwitch(p):
						sw = true
					default:
						plain = true
					}
				}
				if plain {
					entries = append(entries, b)
				} else if sw {
					switched = append(switched, b)
				}
			}
			if len(entries) == 0 {
				entries = switched
			}
			for _, b := range entries {
				for _, p := range b.Preds {
					if inSCC[p] {
						cut[edge{p.ID, b.ID}] = true
					}
				}
			}
			nest(scc)
		}
	}
	nest(f.Blocks)
}

// fromSwitch reports whether an edge from p is a switch's of more than two
// targets: p is the switch, or the block an edge of it was split by.
func fromSwitch(p *Block) bool {
	if p.Kind == BPlain && len(p.Preds) == 1 && len(p.Values) == 0 && len(p.Phis) == 0 {
		p = p.Preds[0]
	}
	return p.Kind == BSwitch && len(p.Succs) > 2
}

// components are the strongly connected components of the graph of blocks
// and the edges in allows, by Tarjan's algorithm.
func components(blocks []*Block, in func(from, to *Block) bool) [][]*Block {
	index := map[*Block]int{}
	low := map[*Block]int{}
	onStack := map[*Block]bool{}
	var stack []*Block
	var out [][]*Block
	next := 0
	var visit func(b *Block)
	visit = func(b *Block) {
		index[b], low[b] = next, next
		next++
		stack = append(stack, b)
		onStack[b] = true
		for _, s := range b.Succs {
			if !in(b, s) {
				continue
			}
			if _, seen := index[s]; !seen {
				visit(s)
				low[b] = min(low[b], low[s])
			} else if onStack[s] {
				low[b] = min(low[b], index[s])
			}
		}
		if low[b] == index[b] {
			var scc []*Block
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == b {
					break
				}
			}
			out = append(out, scc)
		}
	}
	for _, b := range blocks {
		if _, seen := index[b]; !seen {
			visit(b)
		}
	}
	return out
}
