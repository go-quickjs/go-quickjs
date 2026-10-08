package graph

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// build is a graph of n nodes with the given successors.
func build(g *DiGraph, n int, succ map[int32][]int32) {
	g.Reset(n)
	for v, s := range succ {
		g.SetSuccessors(v, s)
	}
}

// nested is two loops, one in the other, and an exit:
//
//	0 -> 1; 1 -> 2, 5; 2 -> 3; 3 -> 3, 4; 4 -> 1; 5
var nested = map[int32][]int32{0: {1}, 1: {2, 5}, 2: {3}, 3: {3, 4}, 4: {1}}

func TestOrder(t *testing.T) {
	var g DiGraph
	build(&g, 6, nested)
	if got := g.ReversePostorder(0); !slices.Equal(got, []int32{0, 1, 5, 2, 3, 4}) {
		t.Errorf("reverse postorder %v", got)
	}
	if got := g.Predecessors(1); !slices.Equal(got, []int32{0, 4}) {
		t.Errorf("predecessors of 1: %v", got)
	}
}

func TestLoopNest(t *testing.T) {
	var g DiGraph
	build(&g, 6, nested)
	level, parent, header, ok := g.LoopNest(0)
	if !ok {
		t.Fatal("not reducible")
	}
	if !slices.Equal(level, []int32{-1, 1, 1, 3, 1, -1}) {
		t.Errorf("level %v", level)
	}
	if parent[1] != -1 || parent[3] != 1 {
		t.Errorf("parent %v", parent)
	}
	if !header[1] || !header[3] || header[2] {
		t.Errorf("header %v", header)
	}
}

// naiveDominators is each node's dominator set, by iterating sets to a
// fixed point: the definition natural loops are checked against.
func naiveDominators(n int, succ map[int32][]int32, reach []bool) [][]bool {
	dom := make([][]bool, n)
	for i := range dom {
		dom[i] = make([]bool, n)
		for j := range dom[i] {
			dom[i][j] = i != 0
		}
	}
	dom[0][0] = true
	for changed := true; changed; {
		changed = false
		for v := 1; v < n; v++ {
			if !reach[v] {
				continue
			}
			next := make([]bool, n)
			first := true
			for p := 0; p < n; p++ {
				if !reach[p] || !slices.Contains(succ[int32(p)], int32(v)) {
					continue
				}
				for j := range next {
					if first {
						next[j] = dom[p][j]
					} else {
						next[j] = next[j] && dom[p][j]
					}
				}
				first = false
			}
			next[v] = true
			if !slices.Equal(next, dom[v]) {
				dom[v], changed = next, true
			}
		}
	}
	return dom
}

// TestRandomGraphs checks natural loops against their definition, from
// naive dominator sets, over random graphs, irreducible ones among them,
// one DiGraph reused.
func TestRandomGraphs(t *testing.T) {
	var g DiGraph
	reducible, irreducible := 0, 0
	defer func() {
		// Both kinds are made, or the test checks less than it says.
		if !t.Failed() && (reducible < 1000 || irreducible < 50) {
			t.Errorf("%d reducible graphs and %d not", reducible, irreducible)
		}
	}()
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 2000; iter++ {
		n := 1 + rng.Intn(70)
		succ := map[int32][]int32{}
		for v := 0; v < n; v++ {
			for k := rng.Intn(3); k > 0; k-- {
				succ[int32(v)] = append(succ[int32(v)], int32(rng.Intn(n)))
			}
		}
		build(&g, n, succ)
		order := g.ReversePostorder(0)
		reach := make([]bool, n)
		for _, v := range order {
			reach[v] = true
		}
		dom := naiveDominators(n, succ, reach)
		// The loops by their definition, from the naive dominators: for
		// each edge b -> h where h dominates b, h and what reaches b without
		// going through h.
		want := map[int32]map[int32]bool{}
		for b := int32(0); b < int32(n); b++ {
			if !reach[b] {
				continue
			}
			for _, h := range succ[b] {
				if !dom[b][h] {
					continue
				}
				body := want[h]
				if body == nil {
					body = map[int32]bool{h: true}
					want[h] = body
				}
				work := []int32{b}
				for len(work) > 0 {
					x := work[len(work)-1]
					work = work[:len(work)-1]
					if body[x] {
						continue
					}
					body[x] = true
					for p := int32(0); p < int32(n); p++ {
						if reach[p] && slices.Contains(succ[p], x) {
							work = append(work, p)
						}
					}
				}
			}
		}
		loops, ok := g.NaturalLoops(0)
		// Reducible exactly where every edge the walk found going back to
		// a node it was in goes to a node that dominates its tail.
		wantOK := true
		for e := 0; e < len(g.back); e += 2 {
			if !dom[g.back[e]][g.back[e+1]] {
				wantOK = false
			}
		}
		if ok != wantOK {
			t.Fatalf("graph %d: reducible %v, want %v", iter, ok, wantOK)
		}
		if !ok {
			irreducible++
			continue
		}
		reducible++
		if len(loops) != len(want) {
			t.Fatalf("graph %d: %d loops, want %d", iter, len(loops), len(want))
		}
		for _, l := range loops {
			body := want[l.Head]
			if body == nil || len(body) != l.Size {
				t.Fatalf("graph %d: loop at %d of %d nodes, want %d", iter, l.Head, l.Size, len(body))
			}
			for x := range body {
				if !l.Body.Has(x) {
					t.Fatalf("graph %d: loop at %d lacks %d", iter, l.Head, x)
				}
			}
		}
		for _, l := range loops {
			if l.Size != l.Body.Len() || !l.Body.Has(l.Head) {
				t.Fatalf("graph %d: loop at %d: size %d, body %d", iter, l.Head, l.Size, l.Body.Len())
			}
			// Every node in the body other than the head has its
			// reachable predecessors in the body too.
			for x := int32(0); x < int32(n); x++ {
				if !l.Body.Has(x) || x == l.Head {
					continue
				}
				for _, p := range g.Predecessors(x) {
					if reach[p] && !l.Body.Has(p) {
						t.Fatalf("graph %d: loop at %d holds %d but not its predecessor %d", iter, l.Head, x, p)
					}
				}
			}
		}
	}
}

// TestWarmGraphAllocatesNothing pins that a graph reused for one no bigger
// allocates nothing.
func TestWarmGraphAllocatesNothing(t *testing.T) {
	var g DiGraph
	succ := [][]int32{{1}, {2, 5}, {3}, {3, 4}, {1}, nil}
	run := func() {
		g.Reset(len(succ))
		for v, s := range succ {
			g.SetSuccessors(int32(v), s)
		}
		g.LoopNest(0)
	}
	run()
	if n := testing.AllocsPerRun(100, run); n != 0 {
		t.Errorf("a warm graph allocates %v times", n)
	}
}

// TestToDot pins the DOT rendering of nested, loops marked.
func TestToDot(t *testing.T) {
	var g DiGraph
	build(&g, 6, nested)
	want := `digraph g {
  0 [style=bold];
  1 [shape=box];
  2;
  3 [shape=box];
  4;
  5;
  0 -> 1;
  1 -> 2;
  1 -> 5;
  2 -> 3;
  3 -> 3 [style=dashed];
  3 -> 4;
  4 -> 1 [style=dashed];
}
`
	if got := g.ToDot(0, true); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := g.ToDot(0, false); strings.Contains(got, "dashed") || strings.Contains(got, "box") {
		t.Errorf("unmarked rendering marks loops:\n%s", got)
	}
}
