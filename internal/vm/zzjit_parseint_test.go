//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"testing"
)

// parseIntSetup's functions call parseInt of numbers, as Octane's Box2D
// does of its counts and indices -- integers, zeros, fractions, numbers
// whose string has an exponent, NaN and the infinities, -0's and a
// negative fraction's parse to -0 -- and then of what native code leaves
// to Go: a string, a second argument, parseInt replaced.
const parseIntSetup = `
	var counts = [0, 1, 7, 255, 65536, 2147483648, 9007199254740993, -1, -4, -2147483649]
	function ints(k) { let s = 0; for (let i = 0; i < counts.length; i++) s += parseInt(counts[i] * k); return s }
	function show(x) { const r = parseInt(x); return Object.is(r, -0) ? '-0' : String(r) }
	var odd = [0.5, 0.999999, 1e-6, 1e-7, 9.5e-7, -0, -0.5, -0.0000005, 1.5, -1.5, 123.456, 1e20, 1e21, -1e21, 5e300, NaN, Infinity, -Infinity, 2 ** 53 + 0.5]
	function shows() { let s = ''; for (let i = 0; i < odd.length; i++) s += show(odd[i]) + ' '; return s }
	function strs(x) { return parseInt(x) }
	function radix(x) { return parseInt(x, 16) }
	function viaNumber(x) { return Number.parseInt(x) }
`

// parseInt of a number is its integer part natively (ssa's parseIntCall),
// as V8's JSCallReducer reduces it, where ToString writes the number with
// no exponent; any other number, a string, a radix, another parseInt, Go's
// call. Each answer is the interpreter's, -0 included, and ints's calls
// of integers neither call Go nor leave its code.
func TestJITParseIntOfNumbers(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	drivers := ""
	for _, name := range []string{"ints", "shows", "strs", "radix", "viaNumber"} {
		drivers += "function drive_" + name + "(n, a) { let r; for (let i = 0; i < n; i++) r = " + name + "(a); return r };"
	}
	// One function of each number, called with it alone, so that its
	// answer is the native code's where native code makes it: 1 / the
	// answer tells -0 from +0.
	values := []struct {
		src    string
		native bool
	}{
		{"0", true}, {"-0", true}, {"0.5", true}, {"0.999999", true}, {"1e-6", true}, {"7", true}, {"-7.75", true},
		{"1e20", true}, {"-1e20", true}, {"123.456", true}, {"9007199254740994", true},
		{"1e-7", false}, {"9.5e-7", false}, {"-0.5", false}, {"-5e-7", false}, {"1e21", false}, {"-1e21", false},
		{"5e300", false}, {"NaN", false}, {"Infinity", false}, {"-Infinity", false},
	}
	for i, v := range values {
		drivers += fmt.Sprintf("function p%d(x) { return 1 / parseInt(x) }; function drive_p%d(n) { let r; for (let i = 0; i < n; i++) r = p%d(%s); return r };", i, i, i, v.src)
	}
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `drive_ints(30, 1)`, `drive_ints(30, 3)`, `drive_shows(30)`, `drive_strs(30, 42)`,
			`drive_radix(30, 255)`, `drive_viaNumber(30, 12.75)`)
		for i := range values {
			rounds = append(rounds, fmt.Sprintf("drive_p%d(30)", i))
		}
	}
	rounds = append(rounds, `drive_strs(30, '42px')`, `drive_strs(30, 0.000001)`, `drive_strs(30, -0.25)`, `drive_viaNumber(30, -7.5)`)
	r := jitAgreesForTest(t, parseIntSetup+drivers, rounds...)
	entry := func(name string) *jitEntry {
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				return x
			}
		}
		return nil
	}
	natives := []string{"ints"}
	for i, v := range values {
		if v.native {
			natives = append(natives, fmt.Sprintf("p%d", i))
		}
	}
	for _, name := range natives {
		e := entry(name)
		if e == nil || e.ssa == nil {
			t.Errorf("%s has no native code: %+v", name, e)
			continue
		}
		if e.ssaStats.goCalls != 0 || e.ssaStats.hosts != 0 {
			t.Errorf("%s called Go %d times, left its code %d times", name, e.ssaStats.goCalls, e.ssaStats.hosts)
		}
	}
	// parseInt replaced: Go's call, the new function's answer.
	jitAgreesForTest(t, parseIntSetup+drivers, append(rounds, `(parseInt = (x) => x * 100, drive_ints(30, 1))`)...)
}
