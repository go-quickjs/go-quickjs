package vm

import "testing"

// The one-argument Math functions are numbered below the two that mathOp
// gives max and min, and below BigInt's asIntN and asUintN.
func TestMathOpNumbers(t *testing.T) {
	if len(unaryMath) >= min(mathAsIntN, mathAsUintN, mathMax) {
		t.Fatalf("%d unary Math functions reach mathAsIntN (%d)", len(unaryMath), mathAsIntN)
	}
}
