//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// digitsSetup's loops convert strings to int32s (s | 0) and to numbers
// (+s): those of one to nine ASCII bytes, decimal digits with at most one
// point and a digit after it, native code converts itself (mir's digits)
// -- leading zeros, zero, nine digits, a fraction, trailing zeros, a
// fraction of eight digits; every other Go does -- ten digits, a sign, a
// space, an exponent, a hexadecimal prefix, the empty string, a point
// alone or last, two points, other scripts' digits, a rope -- and an
// object's, which Go refuses, by its valueOf after an exit.
const digitsSetup = `
var conv = 0, rope = '1234' + 'x'.slice(1) + '5678'.repeat(1);
function toInts(a, n) { var r = 0, f = 0; for (var j = 0; j < n; j++) { var s = a[j % a.length]; r = (r + (s | 0)) | 0; f += +s } return r + ':' + f }
function plain(n) { return toInts(['12345', '7', '007', '0', '999999999', '000000001'], n) }
function fractions(n) { return toInts(['.5', '0.1', '1.0000000', '2.50', '1234567.8', '1.2345678', '0.0000001', '3.14159'], n) }
function others(n) { return toInts(['1234567890', ' 42', '-5', '1e3', '1e5', '-1.5', '0x10', '', '.', '1.', '1.2.3', '1..2', '١٢', '12a', 'NaN', '1.5x', rope, '4294967296', '123456789.5'], n) }
function odd(n) { conv = 0; return toInts(['12', { valueOf() { conv++; return 5 } }, '3'], n) + ':' + conv }
`

// A string of one to nine ASCII decimal digits, converted to a number
// (OpOddballF64), is converted natively, its digits read where the string
// is: each answer the interpreter's, the strings Go converts and an object
// Go refuses too, with the collector idle and marking; and a loop over
// such strings makes no call of Go, where each conversion had been one.
func TestJITDigitStringsConvertNatively(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			var rounds []string
			for range 6 {
				rounds = append(rounds, "plain(60)", "fractions(80)", "others(80)", "odd(30)")
			}
			r := jitAgreesForTest(t, digitsSetup, rounds...)
			jitNativeForTest(t, r, "toInts")
			v, calls := jitCountGoCalls(t, r, "''+plain(6000)")
			if got, want := v.String().Go(), "-715020968:1000012359000"; got != want {
				t.Fatalf("plain(6000) = %s, want %s", got, want)
			}
			if calls > 6000/100 {
				t.Errorf("6000 turns of two conversions made %d calls of Go", calls)
			}
			// fractions' strings, natively too.
			if _, calls := jitCountGoCalls(t, r, "''+fractions(8000)"); calls > 8000/100 {
				t.Errorf("8000 turns of fractions made %d calls of Go", calls)
			}
		})
	}
}
