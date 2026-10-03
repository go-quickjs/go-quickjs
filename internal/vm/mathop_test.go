package vm

import "testing"

// The one-argument Math functions are numbered below the two that mathOp
// gives max and min.
func TestMathOpNumbers(t *testing.T) {
	if len(unaryMath) >= mathMax {
		t.Fatalf("%d unary Math functions reach mathMax (%d)", len(unaryMath), mathMax)
	}
}
