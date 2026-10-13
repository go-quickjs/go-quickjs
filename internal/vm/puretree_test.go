package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestPureByTree pins which pure bodies a call from a tree runs as their
// tree (pureByTree): one of pureTreeCode instructions or more that calls
// nothing is given its tree's call after its first, framed, call; one that
// calls, or a short one, stays frameless (pureCall), as does every call
// from inside another pure body. Each answers the same in both tiers.
func TestPureByTree(t *testing.T) {
	const src = `
		var HELD = 4, RUN = 2;
		function T() { this.state = 0; this.v = 1 }
		T.prototype.held = function () { return (this.state & HELD) != 0 || this.state == RUN };
		T.prototype.mark = function () { this.state = this.state | HELD };
		T.prototype.one = function () { return 1 };
		T.prototype.viaHeld = function () { return this.held() };
		var t = new T(), n = 0;
		for (var i = 0; i < 300; i++) {
			if (t.held()) n++;
			if (t.viaHeld()) n += 2;
			n += t.one();
			if (i == 150) t.mark();
		}
		[n, T.prototype.held, T.prototype.mark, T.prototype.one, T.prototype.viaHeld]`
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatalf("tree=%v: %v", tree, err)
		}
		res := v.Object().elemsOf()
		if got := res[0].Number(); got != 300+149*3 {
			t.Errorf("tree=%v: n = %v, want %v", tree, got, 300+149*3)
		}
		if tree {
			for i, want := range []bool{true, true, false, false} {
				fd := res[i+1].Object().fn()
				name := []string{"held", "mark", "one", "viaHeld"}[i]
				if fd.closure.fn.Leaf != bytecode.LeafPure {
					t.Fatalf("%s is not a pure body", name)
				}
				if got := fd.treeCall != nil; got != want {
					t.Errorf("%s: given its tree's call = %v, want %v", name, got, want)
				}
			}
		}
		r.Close()
	}
	SetTreeTier(true)
}
