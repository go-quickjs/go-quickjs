package vm

import "testing"

// The one-argument Math functions are numbered below the two that mathOp
// gives max and min, and below BigInt, asIntN and asUintN.
func TestMathOpNumbers(t *testing.T) {
	if len(unaryMath) >= min(mathBigInt, mathAsIntN, mathAsUintN, mathMax) {
		t.Fatalf("%d unary Math functions reach mathBigInt (%d)", len(unaryMath), mathBigInt)
	}
}
