//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"reflect"
	"testing"
)

// TestJITSnapshotCompileWide holds the methods of no argument whose answer
// is the runtime's (snapgen's compileWide) to being asked of a compile's
// own function's snapshot alone: every snapshot of the compile, its
// callees' among them, answers each as the root does and as the feedback
// does, and no callee's table holds an answer of one -- nor records a use
// of its own, the root's effect used in its place.
func TestJITSnapshotCompileWide(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitState().unavailable = true
	if _, err := r.Run(compileForTest(t, jitSnapAskedSrc)); err != nil {
		t.Fatal(err)
	}
	r.jitState().unavailable = false
	cl := jitGlobalClosureForTest(r, "work")
	c := r.regOf(cl)
	p, err := r.jitRegLower(cl, c.rc, jitLowerSSA)
	if err != nil {
		t.Fatal(err)
	}
	r.jitRegSeenFeedback(cl, c)
	fb := r.jitRegFeedbackFor(cl, p)
	snap := jitRegSnapshotOf(fb, p)
	snap.s.derive()
	all := append([]*jitSnapshot{snap.s}, mapValues(snap.s.callees)...)
	if len(all) < 2 {
		t.Fatalf("%d snapshots, want callees' too", len(all))
	}
	methods := []string{"ArrayProtos", "GenNext", "HasCache", "KeyAtoms", "MapNexts", "TypeofCells", "WalkedCloseCell"}
	live := reflect.ValueOf(fb)
	for _, name := range methods {
		want := live.MethodByName(name).Call(nil)
		for _, s := range all {
			got := reflect.ValueOf(s.reg).MethodByName(name).Call(nil)
			for i := range got {
				if !reflect.DeepEqual(got[i].Interface(), want[i].Interface()) {
					t.Errorf("%s of a snapshot %d callees deep, result %d: %v, the feedback's %v", name, s.depth, i, got[i].Interface(), want[i].Interface())
				}
			}
		}
	}
	for _, s := range all[1:] {
		a := &s.a
		if n := len(a.ArrayProtos.e) + len(a.GenNext.e) + len(a.HasCache.e) + len(a.KeyAtoms.e) + len(a.MapNexts.e) + len(a.TypeofCells.e) + len(a.WalkedCloseCell.e); n != 0 {
			t.Errorf("a snapshot %d callees deep holds %d compile-wide answers", s.depth, n)
		}
	}
	// ArrayProtos' effect, the prototypes' shapes and objects the root's
	// keep was given, used as the root's where a callee's is asked.
	e := snap.s.a.ArrayProtos.find(0)
	if e == nil || e.eff == 0 {
		t.Fatal("the root's ArrayProtos has no effect")
	}
	for _, s := range all[1:] {
		n := len(snap.s.uses)
		s.reg.ArrayProtos()
		if len(snap.s.uses) != n+1 || snap.s.uses[n] != (jitSnapUse{snap.s, e.eff}) {
			t.Errorf("a snapshot %d callees deep used %v, want the root's effect %d", s.depth, snap.s.uses[n:], e.eff)
		}
	}
}
