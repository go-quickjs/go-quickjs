package ssa

import "testing"

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
