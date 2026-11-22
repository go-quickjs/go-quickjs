package ssa

import "testing"

// Arena-backed metadata must not also allocate a heap copy of the input.
// Keep two results live to check that each request owns its storage, with
// and without a workspace, and repeat after rewinding the workspace.
func TestWorkspaceMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(*Func) bool
	}{
		{"holder", func(f *Func) bool {
			a := f.holderChain(HolderChain{Holders: [2]Holder{{Object: 1, Shape: 2}}})
			b := f.holderChain(HolderChain{Holders: [2]Holder{{Object: 3, Shape: 4}}})
			return a != b && a.Holders[0] == (Holder{1, 2}) && b.Holders[0] == (Holder{3, 4})
		}},
		{"inline", func(f *Func) bool {
			a := f.inlineState(InlineState{Base: 1})
			b := f.inlineState(InlineState{Parent: a, Base: 2})
			return a != b && a.Base == 1 && b.Base == 2 && b.Parent == a
		}},
		{"accessor", func(f *Func) bool {
			a := f.accessorSite(AccessorSite{Function: 1})
			b := f.accessorSite(AccessorSite{Function: 2})
			return a != b && a.Function == 1 && b.Function == 2
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.make(&Func{}) {
				t.Fatal("metadata without a workspace was lost or aliased")
			}
			var w Workspace
			w.init()
			f := &Func{ws: &w}
			valid := true
			allocs := testing.AllocsPerRun(100, func() {
				valid = tc.make(f) && valid
				w.Rewind()
			})
			if !valid {
				t.Fatal("workspace metadata was lost or aliased")
			}
			if allocs != 0 {
				t.Fatalf("metadata in a reused workspace allocated %g objects", allocs)
			}
		})
	}
}

// A workspace outlives its compile in the process's pool, so the tables that
// hold what points into a compile's frames, and through them into its
// runtime, are emptied with the rest.
func TestRewindLeavesNoFrames(t *testing.T) {
	var w Workspace
	w.init()
	sc := &w.scratch
	sc.frameOf = append(sc.frameOf, &frame{}, &frame{})[:1]
	sc.incomplete = append(sc.incomplete, []slotPhi{{}}, []slotPhi{{}})[:1]
	w.Rewind()
	for i, f := range sc.frameOf[:cap(sc.frameOf)] {
		if f != nil {
			t.Errorf("frameOf[%d] is left", i)
		}
	}
	for i, l := range sc.incomplete[:cap(sc.incomplete)] {
		if l != nil {
			t.Errorf("incomplete[%d] is left", i)
		}
	}
}

// Tables a block's number grows are made with room to grow in, so the
// bigger of a compile's blocks are not copied a block at a time.
func TestBlockTablesHaveRoom(t *testing.T) {
	var w Workspace
	f := &Func{ws: &w}
	w.init()
	got := f.refLists(blockRoom * 8)[:8]
	if cap(got) != blockRoom*8 {
		t.Errorf("capacity %d", cap(got))
	}
	for _, l := range got[:cap(got)] {
		if l != nil {
			t.Error("room that is not empty")
		}
	}
}
