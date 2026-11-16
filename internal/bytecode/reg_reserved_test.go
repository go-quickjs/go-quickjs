package bytecode

import (
	"strings"
	"testing"
)

// The reserved operations follow every other, so that no value changes,
// and fit the 8-bit field with values to spare; they are neither cold nor
// unnamed.
func TestReservedOps(t *testing.T) {
	reserved := []ROp{RStepJLt, RStepJLe, RStepJLtI, RStepJGtI, RStepJGeI, RStepJLeU, RMulI, RAddI32, RSubI32, RAddII32}
	if RStepJLt != RFarInit+1 {
		t.Fatalf("far_init is %d and step_jlt %d: the reserved values must follow every other", RFarInit, RStepJLt)
	}
	for i, op := range reserved {
		if op != RStepJLt+ROp(i) {
			t.Errorf("%s is %d, want %d", op, op, RStepJLt+ROp(i))
		}
		if op.Cold() || strings.HasPrefix(op.String(), "rop(") {
			t.Errorf("%s (%d): cold %v, name %q", op, uint8(op), op.Cold(), op.String())
		}
	}
	// At least 12 values are left for the raw registers' regions.
	if ROpCount != RStepJLt+ROp(len(reserved)) || int(ROpCount) > 256-12 {
		t.Errorf("ROpCount is %d", ROpCount)
	}
}

// A step's X: a signed 7-bit k, and RStepAdd for += and -=.
func TestStepK(t *testing.T) {
	for _, c := range []struct {
		x uint8
		k int32
	}{{1, 1}, {0x7f, -1}, {63, 63}, {0x40, -64}, {RStepAdd | 2, 2}, {RStepAdd | 0x7e, -2}, {0, 0}} {
		if k := StepK(c.x); k != c.k {
			t.Errorf("StepK(%#x) = %d, want %d", c.x, k, c.k)
		}
	}
}

// A step-and-branch runs only with its partner after it: the matching
// compare of the same A, B and target, with an instruction past it.
func TestStepPaired(t *testing.T) {
	// 0: step_jlt r5 += 1, r2 -> 0; 1: jlt r5, r2 -> 0; 2: ret_undef.
	partner := MakeR(RJLt, 0, 5, 2, 0).WithSC(-2)
	fused := MakeR(RStepJLt, 1, 5, 2, 0).WithSC(-1)
	ret := MakeR(RRetUndef, 0, 0, 0, 0)
	if !StepPaired([]RInstr{fused, partner, ret}, 0) {
		t.Fatal("the fused pair is not paired")
	}
	add := MakeR(RStepJLt, RStepAdd|0x7d, 5, 2, 0).WithSC(-1) // += -3
	if !StepPaired([]RInstr{add, partner, ret}, 0) {
		t.Error("a step of -3 is not paired")
	}
	for name, code := range map[string][]RInstr{
		"another kind":   {fused, MakeR(RJLe, 0, 5, 2, 0).WithSC(-2), ret},
		"another A":      {fused, MakeR(RJLt, 0, 6, 2, 0).WithSC(-2), ret},
		"another B":      {fused, MakeR(RJLt, 0, 5, 3, 0).WithSC(-2), ret},
		"another target": {fused, MakeR(RJLt, 0, 5, 2, 0).WithSC(-1), ret},
		"past the end":   {fused, partner},
		"a step of 0":    {MakeR(RStepJLt, RStepAdd, 5, 2, 0).WithSC(-1), partner, ret},
		"an update of 2": {MakeR(RStepJLt, 2, 5, 2, 0).WithSC(-1), partner, ret},
		"no step":        {partner, partner, ret},
	} {
		if StepPaired(code, 0) {
			t.Errorf("%s: paired", name)
		}
	}
}
