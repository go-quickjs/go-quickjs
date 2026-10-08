// Package graph is a directed graph over dense node numbers, 0 to n-1, and
// what the tree tier's structuring asks of one: reverse postorder, natural
// loops and how they nest -- one way of finding each, over arrays indexed
// by node -- and a DOT rendering to look at one.
//
// A DiGraph keeps everything it works in -- its edges, the predecessor
// view it builds from them, its order, the walks' marks and loop bodies --
// across Reset, so a graph used again for a graph no bigger allocates
// nothing. Its successor lists are rows of one list, each node's start and
// length kept beside it; its predecessors are offsets into another; its
// loop bodies are bit sets cut from one []uint64; a walk marks the nodes it
// has seen with its own generation number, so that starting one clears
// nothing. A DiGraph is not safe for concurrent use.
package graph

import (
	"fmt"
	"math/bits"
	"slices"
	"strings"
)

// Bits is a set of node numbers, a bit for each.
type Bits []uint64

// Has reports whether the set holds i.
func (b Bits) Has(i int32) bool { return b[i>>6]&(1<<(uint(i)&63)) != 0 }

// Set adds i to the set.
func (b Bits) Set(i int32) { b[i>>6] |= 1 << (uint(i) & 63) }

// Len is how many the set holds.
func (b Bits) Len() int {
	n := 0
	for _, w := range b {
		n += bits.OnesCount64(w)
	}
	return n
}

// Words is how many words a set of n numbers takes.
func Words(n int) int { return (n + 63) >> 6 }

// Loop is a natural loop: its header and the nodes its back edges come
// round through, the header among them.
type Loop struct {
	Head int32
	Body Bits
	Size int
}

// DiGraph is a directed graph of n nodes.
type DiGraph struct {
	n int
	// v's successors are flat[start[v] : start[v]+count[v]]: the rows in
	// one list, in the order they were given, which Reset empties.
	start, count, flat []int32

	// The predecessor view, built from succ when first asked for, of
	// predN, each node's predecessors counted as its edges are given.
	predOK            bool
	predN             []int32
	predOff, predFlat []int32

	order []int32 // reverse postorder from the last entry asked about
	// mark is where the last depth-first walk left each node: 2*mgen while
	// the walk is in it, 2*mgen+1 once it has left it -- reached from
	// entry -- and anything less for a node it has not reached, so that a
	// walk starts by moving mgen on, clearing nothing.
	mark []uint32
	mgen uint32
	work []int32
	back []int32 // the edges the walk found going back, tail and head
	// hidx is, for a node heading a loop NaturalLoops is building, its
	// place in loops, and -1 for any other node: -1 throughout between
	// calls.
	hidx   []int32
	bitBuf []uint64
	loops  []Loop
	level  []int32
	parent []int32
	header []bool
	size   []int32 // a header's loop's size, for LoopNest
}

// Reset makes g a graph of n nodes and no edges, keeping what it works in.
func (g *DiGraph) Reset(n int) {
	g.n = n
	g.start, g.count, g.predN = grow(g.start, n), grow(g.count, n), grow(g.predN, n)
	clear(g.count)
	clear(g.predN)
	g.flat = g.flat[:0]
	g.predOK = false
	g.order = g.order[:0]
	g.loops = g.loops[:0]
}

// Len is how many nodes g has.
func (g *DiGraph) Len() int { return g.n }

// SetSuccessors gives v the successors s, in that order, replacing any it
// had. s is copied.
func (g *DiGraph) SetSuccessors(v int32, s []int32) {
	for _, y := range g.Successors(v) {
		g.predN[y]--
	}
	g.start[v], g.count[v] = int32(len(g.flat)), int32(len(s))
	g.flat = append(g.flat, s...)
	for _, y := range s {
		g.predN[y]++
	}
	g.predOK = false
}

// Successors is v's successors, in the order they were given.
func (g *DiGraph) Successors(v int32) []int32 {
	return g.flat[g.start[v] : g.start[v]+g.count[v]]
}

// Predecessors is the nodes with v among their successors, each once for
// each edge, in node order.
func (g *DiGraph) Predecessors(v int32) []int32 {
	if !g.predOK {
		g.buildPreds()
	}
	return g.predFlat[g.predOff[v]:g.predOff[v+1]]
}

// buildPreds builds the predecessor view, as offsets into one list. Each
// row's offset is first the end of its row, and goes down as the row is
// filled from its end, the nodes taken last first, so that it ends at the
// row's start with the row in node order.
func (g *DiGraph) buildPreds() {
	off := grow(g.predOff, g.n+1)
	sum := int32(0)
	for i, c := range g.predN[:g.n] {
		sum += c
		off[i] = sum
	}
	edges := sum
	off[g.n] = edges
	flat := grow(g.predFlat, int(edges))
	for v := g.n - 1; v >= 0; v-- {
		for _, y := range g.Successors(int32(v)) {
			off[y]--
			flat[off[y]] = int32(v)
		}
	}
	g.predOff, g.predFlat, g.predOK = off, flat, true
}

// grow is s with length n, its contents unspecified.
func grow[T any](s []T, n int) []T {
	if cap(s) < n {
		return make([]T, n, n+n/2)
	}
	return s[:n]
}

// ReversePostorder is the nodes reachable from entry in reverse postorder
// of a depth-first walk that takes each node's successors in order. It is
// g's to reuse: the next call may change it.
//
// The walk also keeps the edges it finds going back to a node it is still
// in -- the retreating edges, of which every back edge of a natural loop is
// one, a dominator being an ancestor in every depth-first walk -- for
// NaturalLoops, which so need not look at every edge again.
func (g *DiGraph) ReversePostorder(entry int32) []int32 {
	if len(g.mark) < g.n || g.mgen >= 1<<30 {
		g.mark = make([]uint32, g.n, g.n+g.n/2)
		g.mgen = 0
	}
	g.mgen++
	g.order, g.back = g.order[:0], g.back[:0]
	g.walk(entry, 2*g.mgen)
	slices.Reverse(g.order)
	return g.order
}

// walk is the depth-first walk from b, whose nodes are marked in while it
// is in them and in+1 once it has left them. It recurses, as deep as the
// longest path it takes, which keeps a node's place in its successors in
// registers rather than in a stack of its own; Go grows the goroutine's
// stack as it needs.
func (g *DiGraph) walk(b int32, in uint32) {
	g.mark[b] = in
	for _, y := range g.flat[g.start[b] : g.start[b]+g.count[b]] {
		if m := g.mark[y]; m < in {
			g.walk(y, in)
		} else if m == in {
			g.back = append(g.back, b, y)
		}
	}
	g.mark[b] = in + 1
	g.order = append(g.order, b)
}

// Order is the reverse postorder ReversePostorder found last.
func (g *DiGraph) Order() []int32 { return g.order }

// NaturalLoops is g's natural loops from entry, one for each header, the
// back edges into it together, in the order the depth-first walk first
// finds an edge back into each, and true; or false where g is not
// reducible. A back edge goes to a node that dominates where it comes from;
// its loop is the nodes that reach its tail without going through its head.
// It is g's to reuse.
//
// The dominators are not computed. In a reducible graph -- which code
// without a goto, as compiled JavaScript is, always makes -- the edges the
// depth-first walk finds going back to a node it is still in are exactly
// the back edges, and a loop's body is what the walk back from each tail
// reaches without going through the head, which is all a body is made of
// here, its bits the walk's marks. A walk that reaches entry instead shows
// that the head does not dominate the tail: the graph is not reducible, and
// there are then no loops to give.
func (g *DiGraph) NaturalLoops(entry int32) ([]Loop, bool) {
	g.ReversePostorder(entry)
	back, mark, reached := g.back, g.mark, 2*g.mgen+1
	// Each node an edge goes back to heads a loop: the bodies for them come
	// from one buffer, cut once.
	if len(g.hidx) < g.n {
		g.hidx = make([]int32, g.n, g.n+g.n/2)
		for i := range g.hidx {
			g.hidx[i] = -1
		}
	}
	hidx := g.hidx
	loops := g.loops[:0]
	for e := 1; e < len(back); e += 2 {
		if h := back[e]; hidx[h] < 0 {
			hidx[h] = int32(len(loops))
			loops = append(loops, Loop{Head: h, Size: 1})
		}
	}
	w := Words(g.n)
	g.bitBuf = grow(g.bitBuf, len(loops)*w)
	clear(g.bitBuf)
	for i := range loops {
		loops[i].Body = Bits(g.bitBuf[i*w : (i+1)*w : (i+1)*w])
		loops[i].Body.Set(loops[i].Head)
	}
	ok := true
	work := g.work[:0]
	for e := 0; e < len(back) && ok; e += 2 {
		l := &loops[hidx[back[e+1]]]
		body := l.Body
		work = append(work[:0], back[e])
		for len(work) > 0 {
			x := work[len(work)-1]
			work = work[:len(work)-1]
			if body.Has(x) {
				continue
			}
			if x == entry {
				ok = false
				break
			}
			body.Set(x)
			l.Size++
			for _, p := range g.Predecessors(x) {
				// What entry does not reach is in no loop.
				if mark[p] == reached {
					work = append(work, p)
				}
			}
		}
	}
	for _, l := range loops {
		hidx[l.Head] = -1
	}
	g.work, g.loops = work[:0], loops
	if !ok {
		g.loops = g.loops[:0]
		return nil, false
	}
	return loops, true
}

// LoopNest is, for each node, the header of the innermost natural loop it
// is in, or -1; for each header, the header of the loop it is inside, or
// -1; which nodes are headers; and true -- or false where g is not
// reducible (see NaturalLoops). They are g's to reuse.
//
// Two natural loops are disjoint or one is inside the other, which is then
// the smaller, so a node's innermost loop is the smallest that holds it, and
// a header's parent the smallest other one: no order of the loops is needed.
func (g *DiGraph) LoopNest(entry int32) (level, parent []int32, header []bool, ok bool) {
	loops, ok := g.NaturalLoops(entry)
	if !ok {
		return nil, nil, nil, false
	}
	level, parent = grow(g.level, g.n), grow(g.parent, g.n)
	header, size := grow(g.header, g.n), grow(g.size, g.n)
	for i := range level {
		level[i], parent[i], header[i] = -1, -1, false
	}
	for _, l := range loops {
		header[l.Head], size[l.Head] = true, int32(l.Size)
	}
	for _, l := range loops {
		ls := int32(l.Size)
		for k, w := range l.Body {
			for ; w != 0; w &= w - 1 {
				x := int32(k<<6 + bits.TrailingZeros64(w))
				if in := level[x]; in < 0 || ls < size[in] {
					level[x] = l.Head
				}
				if x != l.Head && header[x] {
					if p := parent[x]; p < 0 || ls < size[p] {
						parent[x] = l.Head
					}
				}
			}
		}
	}
	g.level, g.parent, g.header, g.size = level, parent, header, size
	return level, parent, header, true
}

// ToDot is g in Graphviz's DOT language, to look at: each node by number,
// its edges in the order they were given, entry marked, and, where marked
// is true, the loops NaturalLoops finds from entry -- their headers boxed
// and the edges back into them dashed. Its output depends only on g, so
// that two dumps diff cleanly.
func (g *DiGraph) ToDot(entry int32, marked bool) string {
	var header []bool
	var order []int32
	if marked {
		order = slices.Clone(g.ReversePostorder(entry))
		header = make([]bool, g.n)
		loops, _ := g.NaturalLoops(entry)
		for _, l := range loops {
			header[l.Head] = true
		}
	}
	rpo := func(v int32) int {
		if i := slices.Index(order, v); i >= 0 {
			return i
		}
		return -1
	}
	var b strings.Builder
	b.WriteString("digraph g {\n")
	for v := int32(0); v < int32(g.n); v++ {
		var attrs []string
		if v == entry {
			attrs = append(attrs, "style=bold")
		}
		if marked && header[v] {
			attrs = append(attrs, "shape=box")
		}
		if len(attrs) > 0 {
			fmt.Fprintf(&b, "  %d [%s];\n", v, strings.Join(attrs, ", "))
		} else {
			fmt.Fprintf(&b, "  %d;\n", v)
		}
	}
	for v := int32(0); v < int32(g.n); v++ {
		for _, y := range g.Successors(v) {
			// An edge into a header that goes back in reverse postorder,
			// from a node in the header's loop, is a back edge.
			if marked && header[y] && rpo(v) >= 0 && rpo(y) <= rpo(v) && g.inLoop(y, v) {
				fmt.Fprintf(&b, "  %d -> %d [style=dashed];\n", v, y)
			} else {
				fmt.Fprintf(&b, "  %d -> %d;\n", v, y)
			}
		}
	}
	b.WriteString("}\n")
	return b.String()
}

// inLoop reports whether v is in the natural loop headed by h, as
// NaturalLoops found it last.
func (g *DiGraph) inLoop(h, v int32) bool {
	for _, l := range g.loops {
		if l.Head == h {
			return l.Body.Has(v)
		}
	}
	return false
}
