package ssa

import (
	"math/rand"
	"testing"
)

// A phi may be a cell if any phi it reaches through phis and keeps has an
// argument that may be: over graphs of phis and keeps that make loops
// within loops, of cells of every kind, every answer of every query is
// what a search of each cell would find, whichever phi is asked first.
func TestAliasesLoops(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 600; round++ {
		f := &Func{ThisSlot: -1, assigned: []bool{true, false}}
		blk := f.newBlock(-1)
		calls := []*Value{f.newValue(blk, OpCall, Tagged), f.newValue(blk, OpCall, Tagged)}
		var leaves []*Value
		for i := 0; i < 3; i++ {
			c := f.newValue(blk, OpPropCell, Source)
			c.Key = uint32(i)
			leaves = append(leaves, c)
		}
		for _, c := range calls {
			leaves = append(leaves, f.newValue(blk, OpCallCell, Source, c))
		}
		for i := 0; i < 2; i++ {
			c := f.newValue(blk, OpLocalCell, Source)
			c.Index = i
			leaves = append(leaves, c)
		}
		leaves = append(leaves, f.newValue(blk, OpElemCell, Source), f.newValue(blk, OpUpvalueCell, Source),
			f.newValue(blk, OpConstSource, Source), f.newValue(blk, OpConstF64, Float64))
		n := 2 + rng.Intn(30)
		nodes := make([]*Value, n)
		for i := range nodes {
			if rng.Intn(5) == 0 {
				// A keep, of a value whose shadow is set below.
				nodes[i] = f.newValue(blk, OpKeep, Tagged, f.newValue(blk, OpConst, Tagged))
			} else {
				nodes[i] = f.newValue(blk, OpPhi, Source)
			}
		}
		pick := func() *Value {
			if rng.Intn(10) < 8 {
				return nodes[rng.Intn(n)]
			}
			return leaves[rng.Intn(len(leaves))]
		}
		for _, v := range nodes {
			if v.Op == OpKeep {
				if rng.Intn(6) != 0 {
					v.Args[0].Shadow = pick()
				}
				continue
			}
			for j := 1 + rng.Intn(3); j > 0; j-- {
				v.Args = append(v.Args, pick())
			}
		}
		// reaches is whether a search from v, through phis and keeps, meets
		// a cell yes says yes of.
		reaches := func(v *Value, yes func(*Value) bool) bool {
			seen := map[*Value]bool{v: true}
			work := []*Value{v}
			for len(work) > 0 {
				v := work[len(work)-1]
				work = work[:len(work)-1]
				next := v.Args
				if v.Op == OpKeep {
					next = nil
					if sh := v.Args[0].Shadow; sh != nil {
						next = []*Value{sh}
					}
				}
				for _, x := range next {
					switch {
					case x.Op == OpPhi || x.Op == OpKeep:
						if !seen[x] {
							seen[x] = true
							work = append(work, x)
						}
					case yes(x):
						return true
					}
				}
			}
			return false
		}
		a := newAliases(f)
		for _, i := range rng.Perm(n) {
			v := nodes[i]
			for _, key := range []uint32{0, 1, 2, anyKey, upvalueKey, localKey(0), localKey(1)} {
				if got, want := a.may(key, v), reaches(v, func(x *Value) bool { return a.may(key, x) }); got != want {
					t.Fatalf("round %d: %v, key %#x: %v, want %v", round, v, key, got, want)
				}
			}
			for _, c := range calls {
				for _, q := range []struct {
					name string
					ask  func(call, s *Value) bool
				}{{"mayCall", a.mayCall}, {"mayOwnCell", a.mayOwnCell}, {"mayElemCell", a.mayElemCell}} {
					if got, want := q.ask(c, v), reaches(v, func(x *Value) bool { return q.ask(c, x) }); got != want {
						t.Fatalf("round %d: %s(%v, %v): %v, want %v", round, q.name, c, v, got, want)
					}
				}
			}
		}
		if len(a.stack) != 0 {
			t.Fatalf("round %d: %d values left on the search's stack", round, len(a.stack))
		}
	}
}

// A phi round a loop may be what any phi of the loop may be. Round a loop,
// b's shadow at the header, bh, merges a constant and its value at the end
// of the body, bb, which merges bh and a property's cell: bh may be that
// cell, through bb, from the iteration before. Asked first of bb, a search
// had looked at bh while bb was being searched, taken bb for no there, and
// kept the no for bh, which a store of the cell's key then did not check.
func TestAliasesProvisionalNo(t *testing.T) {
	f := &Func{ThisSlot: -1}
	entry, header, body := f.newBlock(-1), f.newBlock(0), f.newBlock(1)
	link := func(from, to *Block) {
		from.Succs = append(from.Succs, to)
		to.Preds = append(to.Preds, from)
	}
	link(entry, header)
	link(header, body)
	link(body, header)
	object := f.newValue(entry, OpConstSource, Source)
	cell := f.newValue(header, OpPropCell, Source, object)
	cell.Key = 7
	none := f.newValue(entry, OpConstSource, Source)
	none.Aux = -1
	bh := f.newValue(header, OpPhi, Source)
	bb := f.newValue(body, OpPhi, Source)
	bh.Args = []*Value{none, bb}
	bb.Args = []*Value{bh, cell}
	a := newAliases(f)
	for _, v := range []*Value{bb, bh} {
		if !a.may(7, v) || !a.may(anyKey, v) {
			t.Errorf("%v: a store of key 7 may not write it", v)
		}
		if a.may(8, v) {
			t.Errorf("%v: a store of key 8 may write it", v)
		}
	}
	if a.may(7, none) || a.mayElemCell(bb, bh) || a.mayOwnCell(bb, bh) {
		t.Error("a constant source, or an element's, may be written")
	}
	// The loop's phis have one summary, found once.
	a = newAliases(f)
	if a.may(8, bb) || a.of[bh.ID] == 0 || a.of[bh.ID] != a.of[bb.ID] || len(a.sums) != 1 {
		t.Error("the loop's phis have no summary, or two")
	}
}
