package vm

import (
	"sync/atomic"
	"testing"
)

// A runtime made with the tree tier off (newTreeTierForTest, noTree) gives
// the functions it loads no tree, as SetTreeTier(false) does for the
// process, while a runtime beside it with the tier on gives its own theirs,
// and both answer alike. A function another runtime has run already keeps
// what it was given, as it does when the process's switch changes.
func TestTreeTierPerRuntime(t *testing.T) {
	t.Parallel()
	src := `function f(n) { let s = 0; for (let i = 0; i < n; i++) s += i; return s } f(100)`
	formOf := func(r *Runtime) *tree {
		f := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure.fn
		return (*tree)(atomic.LoadPointer(&f.VMCode))
	}
	for _, on := range []bool{false, true} {
		r := newTreeTierForTest(on, Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil || v.Number() != 4950 {
			t.Fatalf("tree tier %v: %v, %v", on, v, err)
		}
		got := formOf(r)
		r.Close()
		if got == nil {
			t.Fatalf("tree tier %v: f was never settled", on)
		}
		if want := on && treeTier.Load(); (got != noTree) != want {
			t.Errorf("tree tier %v: f has a tree %v, want %v", on, got != noTree, want)
		}
	}
	if !treeTier.Load() {
		return
	}
	// Code run first with the tier on keeps its tree in a runtime without.
	fn := compileForTest(t, src)
	on, off := newTreeTierForTest(true, Config{}), newTreeTierForTest(false, Config{})
	defer on.Close()
	defer off.Close()
	if _, err := on.Run(fn); err != nil {
		t.Fatal(err)
	}
	if _, err := off.Run(fn); err != nil {
		t.Fatal(err)
	}
	if formOf(off) == noTree {
		t.Error("a function run with a tree lost it to a runtime without the tier")
	}
}

// SetOldTiers and SetNoTree, which a host's tests call on the runtime
// under a quickjs.Runtime, set what newOldTiersForTest and
// newTreeTierForTest do.
func TestTierSettersPerRuntime(t *testing.T) {
	t.Parallel()
	r := New(Config{})
	defer r.Close()
	r.SetOldTiers()
	r.SetNoTree()
	if r.RegTier() || !r.oldTiers || !r.noTree {
		t.Errorf("register tier %v, old tiers %v, no tree %v", r.RegTier(), r.oldTiers, r.noTree)
	}
	v, err := r.Run(compileForTest(t, `function f(n) { let s = 0; for (let i = 0; i < n; i++) s += i; return s } f(10)`))
	if err != nil || v.Number() != 45 {
		t.Fatalf("%v, %v", v, err)
	}
	f := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure.fn
	if (*tree)(atomic.LoadPointer(&f.VMCode)) != noTree {
		t.Error("f was given a tree")
	}
}
