//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// mathFamilySetup's loops call Math.imul of every kind of number -- past
// an int32's range, fractions, -0, NaN, the infinities -- Math.clz32, and
// Math.pow, Math.atan2 and Math.hypot of two numbers; oddLoop gives them a
// string too, and constLoop constants, which the code folds.
const mathFamilySetup = `
var ints = [0, -0, 1, -1, 3, 7.9, -7.9, 0x7fffffff, -0x80000000, 0xffffffff, 2 ** 32 + 5, 2 ** 53 + 2, -(2 ** 60), 1e300, NaN, Infinity, -Infinity, 0.5, -1e-300];
function imulLoop(n) { let s = 0x811c9dc5 | 0; for (let i = 0; i < n; i++) s = Math.imul(s ^ i, 16777619) + Math.imul(ints[i % ints.length], ints[(i * 7 + 3) % ints.length]) | 0; return s }
function clzLoop(n) { let t = 0; for (let i = 0; i < n; i++) t += Math.clz32(ints[i % ints.length]) * (i & 3) + Math.clz32(i); return t }
function twoLoop(n) { let t = 0; for (let i = 0; i < n; i++) { const a = (i * 37) % 1000 - 500, b = i % 17 - 8; t += Math.pow(i % 5 + 1, b) + Math.atan2(a, b) + Math.hypot(a, b) } return String(t) }
var odd = [3, "5", 2];
function constLoop(n) { let s = 0; for (let i = 0; i < n; i++) s = s + Math.imul(0x7fffffff, 3) + Math.imul(-5, 2 ** 32 + 7) + Math.clz32(1) + (i & 1) | 0; return s }
function oddLoop(n) { let s = 0; for (let i = 0; i < n; i++) s = s + Math.imul(odd[i % 3], 3) + Math.clz32(odd[(i + 1) % 3]) + Math.pow(odd[(i + 2) % 3], 2) | 0; return s }
`

// Math.imul is an instruction of native code's (ssa.OpMulI32), each
// argument's ToInt32 and their product's low half; Math.clz32 is a call of
// Go of one number's (GoMath), as Math's others are; Math.pow, Math.atan2
// and Math.hypot of two numbers as before. Each loop's answer is Node's
// (v26.10.0) and the interpreter's, and, warm, it makes no call of Go with
// a frame and never leaves its code, which it runs. A string argument has Go make the call,
// and Math.imul replaced is called.
func TestJITMathFamily(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	rounds := [][2]string{
		{"imulLoop(300)", "-783171719"}, {"clzLoop(300)", "17731"},
		{"twoLoop(300)", "1919301.6775609707"}, {"oddLoop(300)", "15700"},
		{"constLoop(300)", "-1950"},
	}
	var srcs []string
	for range 6 {
		for _, rd := range rounds {
			srcs = append(srcs, rd[0])
		}
	}
	r := jitAgreesForTest(t, mathFamilySetup, srcs...)
	for _, rd := range rounds {
		v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", rd[0], err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %s, want Node's %s", rd[0], got, rd[1])
		}
	}
	for _, name := range []string{"imulLoop", "clzLoop", "twoLoop", "constLoop"} {
		e := jitNativeForTest(t, r, name)
		calls, hosts, entries := e.ssaStats.goCalls, e.ssaStats.hosts, e.ssaStats.entries
		if _, err := r.Run(compileForTest(t, name+"(300)")); err != nil {
			t.Fatal(err)
		}
		calls, hosts, entries = e.ssaStats.goCalls-calls, e.ssaStats.hosts-hosts, e.ssaStats.entries-entries
		if entries == 0 || calls != 0 || hosts != 0 {
			t.Errorf("%s ran natively %d times, made %d calls of Go with frames and left its code %d times", name, entries, calls, hosts)
		}
	}
	v, err := r.Run(compileForTest(t, "Math.imul = (a, b) => (a | 0) - (b | 0); '' + imulLoop(300)"))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != "-719671039" {
		t.Errorf("imulLoop(300) with Math.imul replaced = %s, want Node's -719671039", got)
	}
}
