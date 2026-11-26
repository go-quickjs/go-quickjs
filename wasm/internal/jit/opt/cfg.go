package opt

import "slices"

// removeUnreachable removes the blocks the entry does not reach, and their
// edges to the blocks it does, with the phis' arguments for those edges.
// The blocks left are in reverse postorder (order).
func removeUnreachable(f *Func) {
	ws := f.ws
	reach := ws.blockMarks(f)
	reach[f.Entry.ID] = 1
	work := append(ws.work[:0], f.Entry)
	for len(work) > 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]
		for _, s := range b.Succs {
			if reach[s.ID] == 0 {
				reach[s.ID] = 1
				work = append(work, s)
			}
		}
	}
	ws.work = work
	for _, b := range f.Blocks {
		if reach[b.ID] == 0 {
			b.dead = true
			continue
		}
		for i := 0; i < len(b.Preds); i++ {
			if reach[b.Preds[i].ID] == 0 {
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

// orderFrame is a block order's walk: a block and the successors it has
// left to visit.
type orderFrame struct {
	b *Block
	i int
}

// order lays the reachable blocks out in reverse postorder, an if's then
// branch before its else: every block after its dominators, a loop's body
// after its header. The control flow graph has changed: the dominators
// known are no longer.
func order(f *Func) { orderBy(f, false) }

// layout lays the blocks out in reverse postorder once their depths are
// known (loopDepths), as order does but that, of an if's two successors,
// one deeper than the other comes first: a loop's body follows the test
// that leaves the loop, and the code that leaves it comes after the loop.
// A turn of the loop then takes no branch but its back edge's.
func layout(f *Func) { orderBy(f, true) }

// orderBy is order, or, with deeper, layout.
func orderBy(f *Func, deeper bool) {
	ws := f.ws
	f.dom = nil
	seen := ws.blockMarks(f)
	post := ws.blks.take(len(f.Blocks))
	stack := append(ws.frames[:0], orderFrame{f.Entry, len(f.Entry.Succs)})
	seen[f.Entry.ID] = 1
	for len(stack) > 0 {
		t := &stack[len(stack)-1]
		if t.i == 0 {
			post = appendA(&ws.blks, post, t.b)
			stack = stack[:len(stack)-1]
			continue
		}
		t.i--
		// The successors are visited last first, so that the first comes
		// first in reverse postorder.
		k := t.i
		if b := t.b; deeper && b.Kind == BIf && b.Succs[1].Depth > b.Succs[0].Depth {
			k = 1 - k
		}
		s := t.b.Succs[k]
		if seen[s.ID] == 0 {
			seen[s.ID] = 1
			stack = append(stack, orderFrame{s, len(s.Succs)})
		}
	}
	clear(stack[:cap(stack)])
	ws.frames = stack[:0]
	slices.Reverse(post)
	f.Blocks = post
}

// splitCriticalEdges puts a block on every edge from a block with more
// than one successor to a block with more than one predecessor, so that
// the moves of an edge's phis have a block of their own.
func splitCriticalEdges(f *Func) {
	split := false
	for _, b := range f.Blocks[:len(f.Blocks):len(f.Blocks)] {
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
			e.Succs = append(f.ws.blks.take(1), s)
			e.Preds = append(f.ws.blks.take(1), b)
			b.Succs[i] = e
			s.Preds[s.predIndex(b)] = e
			split = true
		}
	}
	// Without a split the blocks are in the order order would give them:
	// the graph has not changed since it last laid them out.
	if split {
		order(f)
	}
}

// domTree is each block's immediate dominator, by Cooper, Harvey and
// Kennedy's "A Simple, Fast Dominance Algorithm", over f's blocks in
// reverse postorder. idom is indexed by block ID; the entry's is itself.
// A block's children in the tree, in the blocks' order, are
// children[childOff[id]:childOff[id+1]]; pre and post number the blocks
// in a walk of the tree, which dominates compares.
type domTree struct {
	idom     []*Block
	rpo      []int32
	childOff []int32
	children []*Block
	pre      []int32
	post     []int32
}

// dominators is f's dominator tree, kept in f's workspace until the
// control flow graph changes (order).
func dominators(f *Func) *domTree {
	if f.dom == nil {
		f.dom = &f.ws.dom
		f.dom.compute(f)
	}
	return f.dom
}

// newDominators is f's dominator tree, made anew.
func newDominators(f *Func) *domTree {
	d := new(domTree)
	d.compute(f)
	return d
}

func (d *domTree) compute(f *Func) {
	n := int(f.nextBlock)
	d.idom = grown(d.idom, n)
	clear(d.idom)
	d.rpo = grown(d.rpo, n)
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
	// The children, counted by parent, then placed in the blocks' order.
	d.childOff = grown(d.childOff, n+1)
	clear(d.childOff)
	for _, b := range f.Blocks[1:] {
		d.childOff[d.idom[b.ID].ID+1]++
	}
	for i := range n {
		d.childOff[i+1] += d.childOff[i]
	}
	d.children = grown(d.children, len(f.Blocks))
	next := grown(d.post, n)
	copy(next, d.childOff[:n])
	for _, b := range f.Blocks[1:] {
		p := d.idom[b.ID].ID
		d.children[next[p]] = b
		next[p]++
	}
	d.pre = grown(d.pre, n)
	d.post = next
	clock := int32(0)
	var walk func(b *Block)
	walk = func(b *Block) {
		d.pre[b.ID] = clock
		clock++
		for _, c := range d.kids(b) {
			walk(c)
		}
		d.post[b.ID] = clock
		clock++
	}
	walk(f.Entry)
}

// kids are b's children in the tree.
func (d *domTree) kids(b *Block) []*Block { return d.children[d.childOff[b.ID]:d.childOff[b.ID+1]] }

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

// dominates reports whether a dominates b: b is within a's subtree.
func (d *domTree) dominates(a, b *Block) bool {
	return d.pre[a.ID] <= d.pre[b.ID] && d.post[b.ID] <= d.post[a.ID]
}

// natLoop is a natural loop: its header and its blocks.
type natLoop struct {
	header *Block
	blocks []*Block
}

// findLoops sets each block's innermost natural loop and its depth: a
// loop is a header and the blocks that reach a back edge to it -- an edge
// from a block it dominates -- without passing through it.
func findLoops(f *Func, d *domTree) {
	ws := f.ws
	loops := ws.loops[:0]
	for _, h := range f.Blocks {
		h.Loop, h.Depth, h.outer = nil, 0, nil
	}
	// in marks a header's loop's blocks with the header's ID plus one.
	in := ws.blockMarks(f)
	for _, h := range f.Blocks {
		mark := h.ID + 1
		var body []*Block
		for _, p := range h.Preds {
			if !d.dominates(h, p) {
				continue
			}
			if in[h.ID] != mark {
				in[h.ID] = mark
				body = ws.blks.take(0)
			}
			work := append(ws.work[:0], p)
			for len(work) > 0 {
				b := work[len(work)-1]
				work = work[:len(work)-1]
				if in[b.ID] == mark {
					continue
				}
				in[b.ID] = mark
				body = appendA(&ws.blks, body, b)
				work = append(work, b.Preds...)
			}
			ws.work = work
		}
		if len(body) > 0 || slices.Contains(h.Preds, h) {
			loops = append(loops, natLoop{h, appendA(&ws.blks, body, h)})
		}
	}
	// Outer loops first, so that an inner loop's header is its blocks'
	// innermost.
	slices.SortStableFunc(loops, func(a, b natLoop) int { return len(b.blocks) - len(a.blocks) })
	for _, l := range loops {
		l.header.outer = l.header.Loop
		for _, b := range l.blocks {
			b.Loop = l.header
			b.Depth++
		}
	}
	clear(loops)
	ws.loops = loops[:0]
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
//
// It reports whether an if's second successor is deeper than its first,
// which is where layout lays the blocks out other than order does.
func loopDepths(f *Func) bool {
	ws := f.ws
	for _, b := range f.Blocks {
		b.Depth = 0
	}
	// A graph whose every edge goes forward in the blocks' order has no
	// loop: each block's depth is 0.
	at := ws.blockMarks(f)
	for i, b := range f.Blocks {
		at[b.ID] = int32(i)
	}
	cyclic := false
	for i, b := range f.Blocks {
		for _, s := range b.Succs {
			if int(at[s.ID]) <= i {
				cyclic = true
				break
			}
		}
		if cyclic {
			break
		}
	}
	if !cyclic {
		return false
	}
	// cut marks the edges cut, by edgeOff[b.ID]+k for b's k-th successor:
	// every edge from one block to another, where a block has several.
	edgeOff := grown(ws.edgeOff, int(f.nextBlock))
	nedges := 0
	for _, b := range f.Blocks {
		edgeOff[b.ID] = int32(nedges)
		nedges += len(b.Succs)
	}
	cut := grown(ws.cut, nedges)
	clear(cut)
	ws.edgeOff, ws.cut = edgeOff, cut
	// member marks the blocks of the component being nested; inSCC those
	// of a component found within it.
	member := ws.blockMarks(f)
	inSCC := grown(ws.inSCC, int(f.nextBlock))
	clear(inSCC)
	ws.inSCC = inSCC
	sc := &ws.scc
	sc.reset(f)
	sc.member, sc.cut, sc.edgeOff = member, cut, edgeOff
	sc.nest(f.Blocks)
	sc.member, sc.cut, sc.edgeOff = nil, nil, nil
	for _, b := range f.Blocks {
		if b.Kind == BIf && b.Succs[1].Depth > b.Succs[0].Depth {
			return true
		}
	}
	return false
}

// nest counts the loops of the component of blocks, and of those within
// them, in their blocks' depths (loopDepths).
func (s *sccs) nest(blocks []*Block) {
	ws := s.ws
	member, cut, edgeOff, inSCC := s.member, s.cut, s.edgeOff, ws.inSCC
	s.gen++
	for _, b := range blocks {
		member[b.ID] = s.gen
	}
	for _, scc := range s.components(blocks) {
		s.sccGen++
		sccGen := s.sccGen
		for _, b := range scc {
			inSCC[b.ID] = sccGen
			b.Depth++
		}
		entries, switched := ws.blks.take(0), ws.blks.take(0)
		for _, b := range scc {
			plain, sw := false, false
			for _, p := range b.Preds {
				switch {
				case inSCC[p.ID] == sccGen:
				case fromSwitch(p):
					sw = true
				default:
					plain = true
				}
			}
			if plain {
				entries = appendA(&ws.blks, entries, b)
			} else if sw {
				switched = appendA(&ws.blks, switched, b)
			}
		}
		if len(entries) == 0 {
			entries = switched
		}
		for _, b := range entries {
			for _, p := range b.Preds {
				if inSCC[p.ID] == sccGen {
					for k, c := range p.Succs {
						if c == b {
							cut[int(edgeOff[p.ID])+k] = true
							s.cutAny = true
						}
					}
				}
			}
		}
		s.nest(scc)
	}
}

// fromSwitch reports whether an edge from p is a switch's of more than two
// targets: p is the switch, or the block an edge of it was split by.
func fromSwitch(p *Block) bool {
	if p.Kind == BPlain && len(p.Preds) == 1 && len(p.Values) == 0 && len(p.Phis) == 0 {
		p = p.Preds[0]
	}
	return p.Kind == BSwitch && len(p.Succs) > 2
}

// sccs is Tarjan's algorithm's state, by block ID, kept in the workspace,
// and loopDepths' nesting's: the component being nested is the blocks
// whose member is gen, its edges those not cut.
type sccs struct {
	ws      *workspace
	index   []int32
	low     []int32
	onStack []bool
	stack   []*Block
	frames  []sccFrame
	next    int32

	member      []int32
	cut         []bool
	edgeOff     []int32
	cutAny      bool
	gen, sccGen int32
}

// sccFrame is the walk's visit of block b, at its successor k.
type sccFrame struct {
	b *Block
	k int
}

// reset readies s for f's blocks.
func (s *sccs) reset(f *Func) {
	n := int(f.nextBlock)
	s.ws = f.ws
	s.index = grown(s.index, n)
	s.low = grown(s.low, n)
	s.onStack = grown(s.onStack, n)
	clear(s.onStack)
	clear(s.stack[:cap(s.stack)])
	s.stack = s.stack[:0]
	s.cutAny, s.gen, s.sccGen = false, 0, 0
}

// in reports whether from's k-th successor's edge is within the component
// being nested.
func (s *sccs) in(from *Block, k int) bool {
	return s.member[from.Succs[k].ID] == s.gen && (!s.cutAny || !s.cut[int(s.edgeOff[from.ID])+k])
}

// components are the strongly connected components of the component being
// nested, of blocks, by Tarjan's algorithm, but for those of a block alone
// without an edge to itself, which are no loop.
func (s *sccs) components(blocks []*Block) [][]*Block {
	ws := s.ws
	for _, b := range blocks {
		s.index[b.ID] = -1
	}
	s.next = 0
	out := ws.sccLists.take(0)
	frames := s.frames[:0]
	for _, root := range blocks {
		if s.index[root.ID] >= 0 {
			continue
		}
		s.open(root)
		frames = append(frames, sccFrame{root, 0})
		for len(frames) > 0 {
			t := &frames[len(frames)-1]
			b := t.b
			if k := t.k; k < len(b.Succs) {
				t.k++
				if !s.in(b, k) {
					continue
				}
				c := b.Succs[k]
				if s.index[c.ID] < 0 {
					s.open(c)
					frames = append(frames, sccFrame{c, 0})
				} else if s.onStack[c.ID] {
					s.low[b.ID] = min(s.low[b.ID], s.index[c.ID])
				}
				continue
			}
			// b's walk is done: it closes its component if it is its
			// root, and its caller takes its low.
			frames = frames[:len(frames)-1]
			if s.low[b.ID] == s.index[b.ID] {
				out = s.close(b, out)
			}
			if len(frames) > 0 {
				p := frames[len(frames)-1].b
				s.low[p.ID] = min(s.low[p.ID], s.low[b.ID])
			}
		}
	}
	clear(frames[:cap(frames)])
	s.frames = frames[:0]
	return out
}

// open numbers b, found by the walk, and pushes it.
func (s *sccs) open(b *Block) {
	s.index[b.ID], s.low[b.ID] = s.next, s.next
	s.next++
	s.stack = append(s.stack, b)
	s.onStack[b.ID] = true
}

// close pops the component b is the root of, and appends it to out if it
// is a loop.
func (s *sccs) close(b *Block, out [][]*Block) [][]*Block {
	ws := s.ws
	if w := s.stack[len(s.stack)-1]; w == b {
		s.stack = s.stack[:len(s.stack)-1]
		s.onStack[b.ID] = false
		if k := slices.Index(b.Succs, b); k < 0 || !s.in(b, k) {
			return out
		}
		return appendA(&ws.sccLists, out, append(ws.blks.take(1), b))
	}
	scc := ws.blks.take(0)
	for {
		w := s.stack[len(s.stack)-1]
		s.stack = s.stack[:len(s.stack)-1]
		s.onStack[w.ID] = false
		scc = appendA(&ws.blks, scc, w)
		if w == b {
			break
		}
	}
	return appendA(&ws.sccLists, out, scc)
}

// grown returns s with length n, its contents not kept, reusing its array
// where it is large enough.
func grown[T any](s []T, n int) []T {
	if cap(s) < n {
		return make([]T, n, n+n/4)
	}
	return s[:n]
}
