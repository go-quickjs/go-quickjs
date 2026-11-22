package ssa

import (
	"testing"

	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

// A register loop's step writes its register in the block where the loop
// goes on (stepOn), before that block's first instruction; a captured
// one's store to its cell exits to the state there. It had taken the state
// the last instruction translated left -- the code past the loop, which is
// filled before the loop's body -- whose values do not dominate the body:
// a frame state in the loop naming values made after it, which the
// allocator's spill classes, taking two values live at once only where
// one's definition dominates the other's, gave one slot with a value of
// the body (test262's intl402/NumberFormat/constructor-unit.js, under the
// allocation checker).
func TestBuildStepStoreOfCapturedLoopVariable(t *testing.T) {
	fn := compiledFunctionForTest(t, `function f(a, g) {
		var t = "";
		for (var k = 0; k < 3; k++) {
			for (const x of a) {
				g(function () { return x; });
			}
			t = t + "-" + g(k);
		}
		return t;
	}`)
	p, err := jitcompile.LowerReg(fn.RegCode(), fn)
	if err != nil {
		t.Fatal(err)
	}
	f, err := BuildWith(p, goCallsFeedback{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(f); err != nil {
		t.Fatal(err)
	}
	// The step's store: in the block the loop goes on in, its state that
	// block's, at its first instruction.
	stores := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpPropWrite || !v.Upvalue || v.State == nil {
				continue
			}
			stores++
			if v.State.Site != b.PC {
				t.Errorf("b%d (pc %d): %v exits to the state at pc %d", b.ID, b.PC, v, v.State.Site)
			}
		}
	}
	if stores == 0 {
		t.Fatal("no store of the loop's captured variable")
	}
	Optimize(f)
	if err := Check(f); err != nil {
		t.Fatal(err)
	}
}
