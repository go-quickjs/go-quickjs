package vm

import "testing"

// The one-argument Math functions, clz32 last among them, are numbered
// below the two-number functions mathCall applies (hypot, atan2, pow,
// imul), BigInt's, and max and min.
func TestMathOpNumbers(t *testing.T) {
	if len(unaryMath) > int(mathHypot) {
		t.Fatalf("%d unary Math functions reach mathHypot (%d)", len(unaryMath), mathHypot)
	}
	if int(mathClz32) != len(unaryMath)-1 || unaryMathName(mathClz32) != "clz32" {
		t.Fatalf("mathClz32 is %d of %d unary functions, named %q", mathClz32, len(unaryMath), unaryMathName(mathClz32))
	}
	if mathHypot >= mathAtan2 || mathAtan2 >= mathPow || mathPow >= mathImul || mathImul >= mathBigInt {
		t.Fatal("the two-number functions are not below BigInt's")
	}
}
