//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// regDepthSrc makes calls of Go from register code's native callees deep
// in recursions of native calls (jitRegGoCallNested): a built-in's call at
// the bottom of down's recursion; a stack trace taken there, and an error a
// built-in throws there, caught by the run or by a level between, whose
// stacks read the levels' frames, which the call put off (jitLazyLevels);
// and a reviver that runs script from the call, which changes what the
// levels below read and recurses natively again, the levels' code found
// wanting and its function, called by Go, asked to be compiled again while
// they wait for the call -- which their contexts hold off
// (jitRegLevelsOf).
const regDepthSrc = `
function bottom(k) { return JSON.stringify([k]).length }
function down(n, k) { return n == 0 ? bottom(k) : down(n - 1, k) + 1 }
function calls(n, m) { var s = 0; for (var i = 0; i < m; i++) s += down(n, i); return s }
function stackAt(k) { return new Error("at " + k).stack }
function downStack(n, k) { return n == 0 ? stackAt(k) : downStack(n - 1, k) }
function stacks(n, m) { var s = ""; for (var i = 0; i < m; i++) s = downStack(n, i); return s }
function parses(k) { return JSON.parse(k % 3 == 2 ? "{" : "[" + k + "]")[0] }
function downThrow(n, k) { return n == 0 ? parses(k) : downThrow(n - 1, k) + 1 }
function catchAt(n, k) { try { return downThrow(n, k) } catch (e) { return -1000 - n } }
function midThrow(n, k) { return n == 0 ? catchAt(10, k) : midThrow(n - 1, k) + 1 }
function throws(n, m) {
	var s = 0, msg = "";
	for (var i = 0; i < m; i++) {
		try { s += downThrow(n, i) } catch (e) { msg = e.name + ": " + e.message + " " + e.stack }
		s += midThrow(n, i);
	}
	return s + " " + msg;
}
var o = { v: 1 };
function revive(k) {
	return JSON.parse("[" + k + "]", function (key, v) {
		if (key === "0" && v % 4 == 3) {
			o = v % 8 == 3 ? { w: 0, v: 2 } : { v: "s" };
			var t = 0;
			for (var j = 0; j < 2000; j++) t += downRe(3, 0.5);
			return v + t + downRe(5, v + 1);
		}
		return v;
	})[0];
}
function downRe(n, k) { return n == 0 ? revive(k) : downRe(n - 1, k) + o.v }
function reenters(n, m) { var s = ""; for (var i = 0; i < m; i++) s += downRe(n, i) + ","; return s }
`

// TestJITRegGoCallDepth makes regDepthSrc's calls of Go from native code
// one level below the run's and thirty below it, and compares the answers
// with a runtime's with no JIT: a call of Go finds the code of its calling
// level alone, whatever the depth (RegNativeStats' Levels), putting off the
// levels below until one is read, which none is (LevelsMade); stack traces
// and errors thrown there are the interpreter's, to the text; and the
// levels' code, compiled again while they wait, is not let go from under
// them. The collector idle and marking.
func TestJITRegGoCallDepth(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			// The levels called, none inlined: the native calls are what it
			// is about.
			regNoInline(t)
			rts := regNativeRuntimes(t, regDepthSrc)
			rts[0].SetJITWarmUp(1, 1)
			call := func(name string, args ...Value) string {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					v, err := r.Call(fn, Undefined, args)
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Errorf("%s%v: with the JIT %q, without %q", name, args, got[0], got[1])
				}
				return got[0]
			}
			r := rts[0]
			// Counted where the code runs as compiled: no stress's
			// thresholds, deoptimizations or compiles in the background
			// (QJS_JIT_STRESS), which the answers below are checked under.
			stress := r.jitStress
			r.jitStress = jitStressConfig{}
			const m = 200
			for _, depth := range []int{1, 30} {
				// Compiled, its contexts grown and what it calls learned.
				for range 6 {
					call("calls", Float(float64(depth)), Float(m))
				}
				before := r.RegNativeStats()
				got := call("calls", Float(float64(depth)), Float(m))
				// Each down(depth, i) answers depth plus the length of
				// "[i]".
				want := 0
				for i := range m {
					want += depth + 2 + len(fmt.Sprint(i))
				}
				if got != fmt.Sprint(want) {
					t.Errorf("calls(%d, %d) = %s, want %d", depth, m, got, want)
				}
				after := r.RegNativeStats()
				nested, levels := after.NestedGoCalls-before.NestedGoCalls, after.Levels-before.Levels
				putOff, made := after.LevelsPutOff-before.LevelsPutOff, after.LevelsMade-before.LevelsMade
				// One level found for each call of Go, its calling one's, at
				// either depth; none unwound, none made.
				if nested < m/2 || levels != nested || made != 0 || after.Unwound != before.Unwound {
					t.Errorf("depth %d: %d calls of Go from native callees, %d levels found, %d made for a read, %d unwound; want %d or more, as many, 0, 0",
						depth, nested, levels, made, after.Unwound-before.Unwound, m/2)
				}
				// The levels below each, down's, put off: at least depth of
				// them, by whichever level the run is entered at.
				if putOff < nested*uint64(depth) {
					t.Errorf("depth %d: %d levels put off by %d calls", depth, putOff, nested)
				}
			}
			// Stack traces taken deep in native recursion: the levels' frames
			// made, each of its function, at its call.
			for _, depth := range []int{1, 30} {
				for range 3 {
					call("stacks", Float(float64(depth)), Float(20))
				}
				before := r.RegNativeStats()
				st := call("stacks", Float(float64(depth)), Float(20))
				if after := r.RegNativeStats(); after.LevelsMade == before.LevelsMade && depth > 1 {
					t.Errorf("depth %d: no levels made for the stack traces", depth)
				}
				// The error's line, stackAt's, downStack's depth+1, stacks':
				// ten frames at most.
				if n := strings.Count(st, "downStack"); n != min(depth+1, 9) {
					t.Errorf("depth %d: downStack %d times in %q", depth, n, st)
				}
			}
			r.jitStress = stress
			for _, depth := range []int{1, 30} {
				for range 4 {
					call("throws", Float(float64(depth)), Float(30))
				}
				call("reenters", Float(float64(depth)), Float(60))
				call("reenters", Float(float64(depth)), Float(60))
			}
		})
	}
}
