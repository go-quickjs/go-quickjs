package ssa

import (
	"math/rand"
	"testing"
)

// A phi may be a cell if any phi it reaches through phis has an argument
// that may be: over graphs of phis that make loops within loops, every
// answer is reachability's, whichever phi is asked first, with the
// answers kept from query to query.
func TestAliasesLoops(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 400; round++ {
		f := &Func{ThisSlot: -1}
		blk := f.newBlock(-1)
		n := 2 + rng.Intn(30)
		phis := make([]*Value, n)
		for i := range phis {
			phis[i] = f.newValue(blk, OpPhi, Source)
		}
		var cells []*Value
		for i := 0; i < 3; i++ {
			c := f.newValue(blk, OpPropCell, Source)
			c.Key = uint32(i)
			cells = append(cells, c)
		}
		none := f.newValue(blk, OpConstSource, Source)
		none.Aux = -1
		for _, p := range phis {
			for j := 1 + rng.Intn(3); j > 0; j-- {
				switch r := rng.Intn(40); {
				case r < 30:
					p.Args = append(p.Args, phis[rng.Intn(n)])
				case r < 32:
					p.Args = append(p.Args, cells[rng.Intn(len(cells))])
				default:
					p.Args = append(p.Args, none)
				}
			}
		}
		reaches := func(p *Value, key uint32) bool {
			seen := map[*Value]bool{p: true}
			work := []*Value{p}
			for len(work) > 0 {
				v := work[len(work)-1]
				work = work[:len(work)-1]
				for _, x := range v.Args {
					switch {
					case x.Op == OpPhi:
						if !seen[x] {
							seen[x] = true
							work = append(work, x)
						}
					case x.Op == OpPropCell && x.Key == key:
						return true
					}
				}
			}
			return false
		}
		a := newAliases(f)
		for _, i := range rng.Perm(n) {
			for key := uint32(0); key < 3; key++ {
				if got, want := a.may(key, phis[i]), reaches(phis[i], key); got != want {
					t.Fatalf("round %d: phi %d, key %d: %v, want %v", round, i, key, got, want)
				}
			}
		}
		if a.depth != 0 || len(a.provisional) != 0 {
			t.Fatalf("round %d: a search left unfinished", round)
		}
	}
}

// An answer of no found for a phi while a phi of its loop was being
// searched is kept only if the search ends in no. Round a loop, b's shadow
// at the header, bh, merges a constant and its value at the end of the
// body, bb, which merges bh and a property's cell: bh may be that cell,
// through bb, from the iteration before. Asked first of bb, the search
// looked at bh while bb was being searched, took bb for no there, and had
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
	// A no the search ended in is kept, for each phi it met.
	a = newAliases(f)
	if a.may(8, bb) || a.key[aliasKey{bh, 8}] != 0 || a.key[aliasKey{bb, 8}] != 0 {
		t.Error("the search's no is not kept")
	}
}
