package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestPureByTree pins which pure bodies are called as their register code:
// a closure register code makes is given its code's call at once (regPlan),
// a pure body's however short, as the register tier gave it before the old
// tiers went -- the tree tier's choice (pureByTree), which left a short one
// or one that calls frameless (pureCall), was the tree's alone. A call from
// inside another pure body is still evaluated frameless (pureInvoke).
func TestPureByTree(t *testing.T) {
	t.Parallel()
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
	r := New(Config{})
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	res := v.Object().elemsOf()
	if got := res[0].Number(); got != 300+149*3 {
		t.Errorf("n = %v, want %v", got, 300+149*3)
	}
	// Which of them are given their register code's call.
	for i, want := range []bool{true, true, true, true} {
		fd := res[i+1].Object().fn()
		name := []string{"held", "mark", "one", "viaHeld"}[i]
		if fd.closure.fn.RegCompiled().Leaf != bytecode.LeafPure {
			t.Fatalf("%s is not a pure body", name)
		}
		if got := fd.regCall != nil; got != want {
			t.Errorf("%s: given its register code's call = %v, want %v", name, got, want)
		}
	}
	r.Close()
}

// TestPureByTreeComparison pins that a pure body returning a comparison --
// six register instructions, the branch and the two loads its value takes
// besides the return -- is evaluated frameless (pureByTree false), as it
// was when the stack bytecode's four instructions were counted: called with
// a frame, BigInt64Array's every of x => x <= last ran natively at the
// interpreter's speed. A body of a dozen instructions goes to its code.
func TestPureByTreeComparison(t *testing.T) {
	t.Parallel()
	top := compileForTest(t, `function f(last) { return [(x) => x <= last, (x, y) => (x * 3 + y * 5 + last) * (x - y) - (y + 7) * (x + 9) * last] }`)
	var fs []*bytecode.Function
	for _, k := range top.Constants {
		if k.Fn != nil && k.Fn.Name == "f" {
			for _, c := range k.Fn.Constants {
				if c.Fn != nil {
					fs = append(fs, c.Fn)
				}
			}
		}
	}
	if len(fs) != 2 {
		t.Fatalf("%d arrows", len(fs))
	}
	for i, want := range []bool{false, true} {
		rc := fs[i].RegCode()
		if rc == nil || rc.Leaf != bytecode.LeafPure || rc.PureCalls {
			t.Fatalf("arrow %d is not a pure body that calls nothing: %v", i, rc)
		}
		if got := pureByTree(rc); got != want {
			t.Errorf("arrow %d, %d instructions: pureByTree %v, want %v", i, len(rc.Code), got, want)
		}
	}
}

// TestPureInt32Forms pins pure bodies of the int32 forms -- (x + y) | 0,
// (x - y) | 0, (x + k) | 0 and (x - k) | 0 -- evaluated frameless with the
// answers their register code gives: past 2^31, of a string, whose
// operator is concatenation or subtraction as the source wrote it, and of
// a BigInt, which throws. A pure body calling itself in tail position, as
// a loop of tail_self_loop's, is one too.
func TestPureInt32Forms(t *testing.T) {
	t.Parallel()
	src := `
		function add(a, b) { return (a + b) | 0 }
		function sub(a, b) { return (a - b) | 0 }
		function addK(a) { return (a + 7) | 0 }
		function subK(a) { return (a - 1) | 0 }
		function loop(n, acc) { "use strict"; if (n === 0) return acc; return loop(n - 1, (acc + n) | 0) }
		var out = [];
		for (var i = 0; i < 3; i++) {
			out.push(add(0x7fffffff, 1), add("5", 1), sub(-0x80000000, 1), addK(0x7ffffffa), addK("5"), subK("5"), subK(-0x80000000));
			try { add(1n, 2n) } catch (e) { out.push(e.name) }
			out.push(loop(100, 0));
		}
		[out.join(), add, sub, addK, subK, loop]`
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	res := v.Object().elemsOf()
	const once = "-2147483648,51,2147483647,-2147483647,57,4,2147483647,TypeError,5050"
	if got := res[0].String().Go(); got != once+","+once+","+once {
		t.Errorf("got %s", got)
	}
	for i, name := range []string{"add", "sub", "addK", "subK", "loop"} {
		rc := res[i+1].Object().fn().closure.fn.RegCompiled()
		if rc == nil || rc.Leaf != bytecode.LeafPure {
			t.Errorf("%s is not a pure body: %v", name, rc)
		}
	}
}
