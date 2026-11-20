//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regPatternSrc's array patterns destructure, in loops, an array with a
// rest element, as microbench's local_destruct does; a Set, a Map and a
// generator, whose cursors native code makes no step of its own for; an
// iterator of its own, which counts its next and return calls; one with
// no return method, which every pattern takes to its end; one whose next
// throws at its fifth value, caught; and a pattern longer than what it
// destructures, undefined past the end.
const regPatternSrc = `
var array = [1, 2, 3, 4, 5]
function arrays(n) { var j, a, b, c, d, s = 0; for (j = 0; j < n; j++) { [a, b, , c, ...d] = array; s += a + b * 2 + c * 3 + d.length * 4 + d[0] } return s }
var set = new Set([3, 1, 4, 1, 5, 9])
var map = new Map([["a", 1], ["b", 2], ["c", 3]])
function* gen(k) { yield k; yield k + 1; yield k + 2 }
function others(n) {
	var j, s = 0
	for (j = 0; j < n; j++) {
		var [x, y, ...z] = set
		var [[k1, v1], [k2, v2]] = map
		var [g1, , g3, g4] = gen(j & 7)
		s += x + y * 2 + z.length + z[2] + v1 + v2 + k1.length + k2.charCodeAt(0) + g1 + g3 + (g4 === undefined ? 1 : 0)
	}
	return s
}
var calls = 0, closes = 0
var counted = { [Symbol.iterator]() { var i = 0; return { next() { calls++; return { value: i++, done: i > 6 } }, return() { closes++; return {} } } } }
function custom(n) { var j, s = 0; for (j = 0; j < n; j++) { var [a, b, ...r] = counted; var [c] = counted; s += a + b + r.length + c } return s + ":" + calls + ":" + closes }
var plain = { [Symbol.iterator]() { var i = 0; return { next() { return { value: i * 3, done: ++i > 5 } } } } }
function exhausted(n) { var j, s = 0; for (j = 0; j < n; j++) { var [a, , b, ...r] = plain; s += a + b + r.length * 7 + r[1] } return s }
var throwing = { [Symbol.iterator]() { var i = 0; return { next() { if (++i === 5) throw new RangeError("next " + i); return { value: i, done: false } } } } }
function thrown(n) {
	var j, s = 0, caught = 0
	for (j = 0; j < n; j++) {
		try { var [a, b, ...r] = throwing; s += r.length } catch (e) { caught++; s += e.message.length + (e instanceof RangeError ? 1 : 0) }
		var [p, q, t, u] = [j, j + 1]
		s += p + q + (t === undefined) + (u === undefined)
	}
	return s + ":" + caught
}
`

// TestJITRegPatternSteps: register code's array pattern's step of a cursor
// native code makes none of its own for (abi.GoCallFill) and its rest
// element (abi.GoCallRest) are made by Go from native code, their results
// to the call's keep cell (jitRegIterMade). The step had left native code
// at every turn, and the rest had run whole (jitRegWhole). Each answer is
// the interpreter's, with the collector idle and marking -- the iterators'
// next and return calls counted, a next's RangeError caught -- and once
// warm no destructuring leaves native code or runs whole.
func TestJITRegPatternSteps(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	want := map[string]string{ // Node's
		"arrays":    "52000",
		"others":    "258000",
		"custom":    "10000:25600:3200",
		"exhausted": "64000",
		"thrown":    "4018000:2000",
	}
	// Those whose code goes on natively at every turn: custom's and
	// thrown's close their cursors through Go (ir.IterClose's exit), and
	// their loops are left to the interpreter.
	counted := map[string]bool{"arrays": true, "others": true, "exhausted": true}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regPatternSrc)
			r := rts[0]
			r.jitStress = jitStressConfig{}
			r.SetJITWarmUp(1, 1)
			run := func(name string, n float64) string {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					v, err := r.Call(fn, Undefined, []Value{Float(n)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] {
					t.Errorf("%s(%v): with the JIT %q, without %q", name, n, got[0], got[1])
				}
				return got[0]
			}
			for _, name := range []string{"arrays", "others", "custom", "exhausted", "thrown"} {
				st0 := r.RegNativeStats()
				for range 6 {
					run(name, 200)
				}
				s := r.jitState()
				s.reporting = true
				whole := s.regGoOps[jitRegGoKey{op: bytecode.RIterRest, whole: true}] + s.regGoOps[jitRegGoKey{op: bytecode.RIterStep, whole: true}]
				st := r.RegNativeStats()
				got := run(name, 2000)
				s.reporting = false
				if n := s.regGoOps[jitRegGoKey{op: bytecode.RIterRest, whole: true}] + s.regGoOps[jitRegGoKey{op: bytecode.RIterStep, whole: true}] - whole; n != 0 {
					t.Errorf("%s: %d steps and rest elements run whole", name, n)
				}
				if got != want[name] {
					t.Errorf("%s(2000) = %q, want %q", name, got, want[name])
				}
				if !counted[name] {
					continue
				}
				st2 := r.RegNativeStats()
				for _, op := range []string{"iter_step", "iter_rest"} {
					if n := st2.HostOps[op] - st.HostOps[op]; n != 0 {
						t.Errorf("%s: %d of its %s left native code", name, n, op)
					}
				}
				if n := st2.WholeLeft - st.WholeLeft; n != 0 {
					t.Errorf("%s: %d runs whole left native code", name, n)
				}
				// Native code ran in this call; exhausted's, whose rest
				// element Go makes at each turn (jitRegIterMade), in the
				// warm-up's calls or this one's: it is judged not to pay
				// (jitRegStretchWork) and left to the interpreter, in this
				// call or before, as its compile comes later or earlier.
				if from := map[bool]RegNativeStats{false: st, true: st0}[name == "exhausted"]; st2.Entries == from.Entries {
					t.Errorf("%s: no native code ran", name)
				}
			}
		})
	}
}
