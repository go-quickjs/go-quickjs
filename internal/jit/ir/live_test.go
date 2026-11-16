package ir

import (
	"math/rand"
	"slices"
	"testing"
)

// A Liveness's sets are what dense rows of the same changes are, at every
// height of tree: each slot's bit, each set's slots in order, the slots two
// sets tell apart, and a builder's unions, equality and the slots one set
// adds to another (Added).
func TestLivenessSets(t *testing.T) {
	for _, n := range []int{1, 63, 64, 65, 200, 1024, 1025, 5000, 70000} {
		rng := rand.New(rand.NewSource(int64(n)))
		b := NewLiveBuilder(n)
		const m = 300
		words := (n + 63) / 64
		rows := make([][]uint64, m)
		roots := make([]LiveSet, m)
		cur := make([]uint64, words)
		s := b.Empty()
		for pc := range m {
			var ops []LiveOp
			for range rng.Intn(6) {
				i := rng.Intn(n)
				if pc%50 == 0 {
					i = rng.Intn(min(n, 3))
				}
				ops = append(ops, LiveOp{Slot: int32(i), Clear: rng.Intn(3) == 0})
			}
			for _, o := range ops {
				if o.Clear {
					cur[o.Slot/64] &^= 1 << (o.Slot % 64)
				} else {
					cur[o.Slot/64] |= 1 << (o.Slot % 64)
				}
			}
			b.Edit()
			s = b.Apply(s, slices.Clone(ops))
			if pc%37 == 0 && pc > 0 {
				// A union with an earlier set, as a block's end is its
				// successors'.
				k := rng.Intn(pc)
				s = b.Union(s, roots[k])
				for w := range cur {
					cur[w] |= rows[k][w]
				}
			}
			rows[pc], roots[pc] = slices.Clone(cur), s
		}
		dense := func(pc, i int) bool { return rows[pc][i/64]&(1<<(i%64)) != 0 }
		for _, l := range []*Liveness{b.Finish(roots), LivenessFromRows(slices.Concat(rows...), words, n, m)} {
			for pc := range m {
				var want []int
				for i := range n {
					if dense(pc, i) {
						want = append(want, i)
					}
					if pc%29 == 0 && l.Has(pc, i) != dense(pc, i) {
						t.Fatalf("n %d pc %d slot %d: has %v", n, pc, i, l.Has(pc, i))
					}
				}
				if got := slices.Collect(l.Slots(pc)); !slices.Equal(got, want) {
					t.Fatalf("n %d pc %d: slots %v, want %v", n, pc, got, want)
				}
				for _, i := range want {
					if !l.Has(pc, i) || !b.Has(roots[pc], i) {
						t.Fatalf("n %d pc %d: slot %d not had", n, pc, i)
					}
				}
				q := rng.Intn(m)
				var diff []int
				for i := range n {
					if dense(pc, i) != dense(q, i) {
						diff = append(diff, i)
					}
				}
				if got := slices.Collect(l.Changed(pc, q)); !slices.Equal(got, diff) {
					t.Fatalf("n %d pcs %d, %d: changed %v, want %v", n, pc, q, got, diff)
				}
				var added, got []int
				for i := range n {
					if dense(pc, i) && !dense(q, i) {
						added = append(added, i)
					}
				}
				b.Added(roots[q], roots[pc], func(i int) bool { got = append(got, i); return true })
				if !slices.Equal(got, added) {
					t.Fatalf("n %d pcs %d, %d: added %v, want %v", n, pc, q, got, added)
				}
				if eq := b.Equal(roots[pc], roots[q]); eq != (len(diff) == 0) {
					t.Fatalf("n %d pcs %d, %d: equal %v, %d differ", n, pc, q, eq, len(diff))
				}
			}
			if l.Has(0, n) || l.Has(0, -1) {
				t.Fatalf("n %d: a slot past the sets is had", n)
			}
		}
	}
}

// An edit's operations change its own nodes in place, and no set made
// before it: a set made of several operations copies a node once.
func TestLivenessEdits(t *testing.T) {
	b := NewLiveBuilder(5000)
	s := b.Apply(b.Empty(), []LiveOp{{Slot: 7}, {Slot: 4000}})
	b.Edit()
	t1 := b.Apply(s, []LiveOp{{Slot: 8}})
	nodes, leaves := len(b.nodes), len(b.leaves)
	t2 := b.Apply(t1, []LiveOp{{Slot: 9}, {Slot: 4001}})
	t2 = b.Union(t2, b.Apply(b.Empty(), []LiveOp{{Slot: 10}}))
	// What the edit adds: the leaf of 4001 and the path to it, and the set of
	// 10, its root, its node and its leaf.
	if t2 != t1 || len(b.leaves) != leaves+2 || len(b.nodes) != nodes+liveHeight(5000)*2+2 {
		t.Errorf("an edit's set copied its own nodes: %d of %d nodes' words, %d of %d leaves", len(b.nodes), nodes, len(b.leaves), leaves)
	}
	for _, i := range []int{7, 8, 9, 10, 4000, 4001} {
		if !b.Has(t2, i) {
			t.Errorf("slot %d not in the edit's set", i)
		}
	}
	for _, i := range []int{8, 9, 10, 4001} {
		if b.Has(s, i) {
			t.Errorf("slot %d in the set made before the edit", i)
		}
	}
}

// A set that changes nothing is the set it was: Apply, Union and Full
// give back their own where nothing changes, so that the sets of
// instructions that change little share their trees.
func TestLivenessShares(t *testing.T) {
	b := NewLiveBuilder(5000)
	s := b.Apply(b.Empty(), []LiveOp{{Slot: 7}, {Slot: 4000}})
	if b.Apply(s, []LiveOp{{Slot: 7}}) != s || b.Apply(s, []LiveOp{{Slot: 8, Clear: true}}) != s {
		t.Error("an Apply that changes nothing made a set")
	}
	if b.Union(s, b.Apply(b.Empty(), []LiveOp{{Slot: 4000}})) != s || b.Union(s, s) != s {
		t.Error("a Union that adds nothing made a set")
	}
	if f := b.Full(); b.Union(f, s) != f || b.Full() != f {
		t.Error("the full set is made again")
	}
	if e := b.Apply(s, []LiveOp{{Slot: 7, Clear: true}, {Slot: 4000, Clear: true}}); e != b.Empty() {
		t.Errorf("a set emptied is %d, not the empty set", e)
	}
	before := len(b.nodes)
	t2 := b.Apply(s, []LiveOp{{Slot: 4001}})
	if grown := len(b.nodes) - before; grown > 2*(1+liveFan) {
		t.Errorf("a change of one slot made %d words of nodes", grown)
	}
	l := b.Finish([]LiveSet{s, s, t2})
	if l.roots[0] != l.roots[1] {
		t.Error("two instructions of one set have two")
	}
}

// A builder Reset makes sets as a new one does, of the slots it is given.
func TestLivenessReset(t *testing.T) {
	b := NewLiveBuilder(5000)
	b.Apply(b.Empty(), []LiveOp{{Slot: 7}, {Slot: 4000}})
	b.Reset(100)
	s := b.Apply(b.Empty(), []LiveOp{{Slot: 3}, {Slot: 99}, {Slot: 4000}})
	var got []int
	b.Added(b.Empty(), s, func(i int) bool { got = append(got, i); return true })
	if !slices.Equal(got, []int{3, 99}) || b.Has(s, 7) || len(b.nodes) > 1+2*(1+liveFan) {
		t.Errorf("after Reset: %v, %d words of nodes", got, len(b.nodes))
	}
}
