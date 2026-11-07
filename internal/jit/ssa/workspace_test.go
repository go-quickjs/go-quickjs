package ssa

import "testing"

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
