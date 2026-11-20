//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// regLoopSrc's loops test at their end in register code, entered by a
// jump to the test: a sum stored once the loop is done, a float carried
// round, a test of two comparisons, one that reads a property and calls
// a method, a loop never entered, and nested loops.
const regLoopSrc = `
var total = 0;
var box = { n: 7, more(i) { return i < this.n * 40 } };
function sumTo(n) { var j, s = 0; for (j = 0; j < n; j++) s += j; total = s; return n }
function floats(n) { var a = 0.1, s = 0; for (var i = 0; i < n; i++) { s += a * a; a += 1.1 } return s }
function both(a, n, k) { var i = 0; while (i < n && a[i & 7] !== k) i++; return i }
function calls(o) { var s = 0; for (var i = 0; o.more(i); i++) s += i & 3; return s }
function never(n) { var s = 1.5; for (var i = n; i < 0; i++) s += i; return s + n }
function nested(n) { var s = 0; for (var i = 0; i < n; i++) for (var j = 0; j < 8; j++) s += i * j; return s }
function main(n) {
	return sumTo(n) + ":" + total + ":" + floats(n) + ":" + both([1, 2, 3, 4, 5, 6, 7, 8], n, 9) + ":" + both([1, 2, 3], n, 3) + ":" +
		calls(box) + ":" + never(n) + ":" + nested(n);
}
`

// A register loop is built with its body's first instruction its one
// header, its test copied before it (the compile package's loopTest): the
// numbers it carries are not boxed inside it -- a sum stored once it is
// done had been boxed at every turn, the test's block no loop's header --
// and every answer is the interpreter's, with the collector idle and
// marking.
func TestJITRegLoopsOneHeader(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			listings := map[string]string{}
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.annotate = switchOn })
			jitTestSwitch(t, func(jc *jitTestConfig) {
				jc.codeHook = func(f JITFunction, _ []byte, c *mir.Code) { listings[f.Name] = c.Listing }
			})
			rts := regNativeRuntimes(t, regLoopSrc)
			// Compiled as the code asks, not as stress configures it.
			rts[0].jitStress = jitStressConfig{}
			var got [2]string
			for k, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("main"))
				for i := range 30 {
					v, err := r.Call(fn, Undefined, []Value{Float(float64(200 + i%3))})
					got[k] = regNativeShow(r, v, err)
				}
			}
			const want = "202:20301:3304260.449999981:202:2:420:203.5:568428" // Node's
			if got[0] != got[1] || got[1] != want {
				t.Errorf("with the JIT %q, without %q, want %q", got[0], got[1], want)
			}
			for _, name := range []string{"sumTo", "floats", "nested"} {
				l, ok := listings[name]
				if !ok {
					t.Errorf("%s was not compiled", name)
					continue
				}
				if n := innermostLoopBoxes(l); n != 0 {
					t.Errorf("%s makes %d boxes inside its innermost loops:\n%s", name, n, l)
				}
			}
		})
	}
}

// A loop that carries an object escape analysis keeps unmade has no entry
// in its function's code (ssa.Func's Unentered), and a frame that waits
// there long goes on in code compiled for it apart (jitRegLoopCode), its
// entry kept (ssa.Func's FrameAt): both named by the header's register pc,
// as frame states name it. The register form had read Unentered's as the
// program's instructions and given FrameAt one, and so had never compiled
// code with the entry: a long loop called from Go ran in the interpreter
// to its end, every call.
func TestJITRegLoopEntryUnmade(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, `var res;
function props(n) { var j, o = { x: 1, y: 2 }, s = 0; for (j = 0; j < n; j++) s += o.x + o.y; res = s; return n }`)
			// Compiled as the code asks, not as stress configures it.
			rts[0].jitStress = jitStressConfig{}
			var got [2]string
			for k, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("props"))
				for range 4 {
					v, err := r.Call(fn, Undefined, []Value{Float(300000)})
					res, _ := r.GetProp(Obj(r.Global()), r.Intern("res"))
					got[k] = regNativeShow(r, v, err) + ":" + regNativeShow(r, res, nil)
				}
			}
			if want := "300000:900000"; got[0] != got[1] || got[1] != want {
				t.Errorf("with the JIT %q, without %q, want %q", got[0], got[1], want)
			}
			r := rts[0]
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("props"))
			cl := fn.Object().fn().closure
			e := r.jit.regCache[r.jitKeyOf(cl.fn)]
			if e == nil || e.code == nil {
				t.Fatal("props has no register code")
			}
			// The loop's entry kept: in the function's code, compiled for
			// the frame waiting at the loop (jitRegFrameAt), or, where that
			// code dropped it (Unentered), in code compiled for the frame
			// apart (osr) -- and entered.
			code := e
			if len(e.unentered) != 0 {
				if code = e.osr; code == nil || code.code == nil {
					t.Fatalf("no code compiled for the frame waiting at the loop at %d", e.unentered[0])
				}
			}
			if len(code.loops) == 0 || code.life.ssaStats.entries == 0 {
				t.Errorf("the loop's code has entries %v, entered %d times", code.loops, code.life.ssaStats.entries)
			}
		})
	}
}

// regFramesSrc's callees are called natively: fib's plain calls, whose
// receiver register the call writes, and bindings that start undefined
// beside temporaries a callee before them filled.
const regFramesSrc = `
function fib(k) { return k < 2 ? k : fib(k - 1) + fib(k - 2) }
function fill(a) { var p = a * 3, q = [a, p], r = q.length + p; return r + q[1] }
function late(a) { var u, w; let v; if (a > 5) { u = a * 2; v = u + 1 } else w = 'w'; return String(u) + ':' + v + ':' + w + ':' + typeof w }
function drive(n) { var s = ''; for (var i = 0; i < n; i++) { s = fill(i) + '|' + late(i & 7) + '|' + fib(i & 7) } return s }
`

// A plain call's receiver register is the call's to write (compile's
// RegAccess): no longer read by it, it is not live from the function's
// start, where fib's code had loaded it at its entry and kept it in every
// frame state. And a callee's call entry makes only its bindings
// undefined (ssa.Func's InitEnd), as the interpreter's call does, not its
// temporaries, which it writes before it reads them: answers exact with
// what an earlier callee left in the window, idle and marking.
func TestJITRegCallFrames(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			listings := map[string]string{}
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.annotate = switchOn })
			jitTestSwitch(t, func(jc *jitTestConfig) {
				jc.codeHook = func(f JITFunction, _ []byte, c *mir.Code) { listings[f.Name] = c.Listing }
			})
			rts := regNativeRuntimes(t, regFramesSrc)
			// Compiled as the code asks, not as stress configures it.
			rts[0].jitStress = jitStressConfig{}
			var got [2]string
			for k, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("drive"))
				for i := range 40 {
					v, err := r.Call(fn, Undefined, []Value{Float(float64(60 + i%5))})
					got[k] = regNativeShow(r, v, err)
				}
			}
			// Node's.
			if want := "380|14:15:undefined:undefined|13"; got[0] != got[1] || got[1] != want {
				t.Errorf("with the JIT %q, without %q, want %q", got[0], got[1], want)
			}
			l, ok := listings["fib"]
			if !ok {
				t.Fatal("fib was not compiled")
			}
			start := strings.SplitN(l, "\nb1 ", 2)[0]
			if n := strings.Count(start, "= load "); n != 1 {
				t.Errorf("fib's entry loads %d registers, want its parameter alone:\n%s", n, start)
			}
		})
	}
}

// regCarriedSrc's loops carry an object escape analysis keeps unmade, d,
// which an exit inside them -- o a number at one turn -- and the polls at
// their headers must make before the interpreter reads the frame: a
// rotated for loop and a do-while.
const regCarriedSrc = `
function Vec(x, y) { this.x = x; this.y = y }
Vec.prototype.add = function (o) { return new Vec(this.x + o.x, this.y + o.y) };
var plain = { a: 1 };
function forLoop(n, k) {
	let s = new Vec(0, 0);
	const d = new Vec(1, 2);
	for (let i = 0; i < n; i++) { const o = i === k ? 1 : plain; o.a = 1; s = s.add(d) }
	return s.x + s.y;
}
function doLoop(n, k) {
	let s = new Vec(0, 0), i = 0;
	const d = new Vec(1, 2);
	do { const o = i === k ? 1 : plain; o.a = 1; s = s.add(d) } while (++i < n);
	return s.x + s.y;
}
function driveFor(n, m) { var t = 0; for (var j = 0; j < n; j++) t += forLoop(m, 5); return t }
function driveDo(n, m) { var t = 0; for (var j = 0; j < n; j++) t += doLoop(m, 5); return t }
`

// The objects native code did not make are made at a poll's exit too, as
// at every other (jitRegLeft): a loop carrying one has no entry at its
// header, so the frame goes on in the interpreter, or in code compiled for
// it, which read the home's word left in the register as the object -- a
// null, and s.add(d) threw. Exact answers, idle and marking.
func TestJITRegPollMakesObjects(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regCarriedSrc)
			for k, r := range rts {
				for _, name := range []string{"driveFor", "driveDo"} {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					for i := range 30 {
						m := float64(20)
						if i >= 20 {
							m = []float64{500, 3}[i%2]
						}
						v, err := r.Call(fn, Undefined, []Value{Float(100), Float(m)})
						want := fmt.Sprint(100 * 3 * m)
						if got := regNativeShow(r, v, err); got != want {
							t.Fatalf("runtime %d, %s call %d (%v turns): %s, want %s", k, name, i, m, got, want)
						}
					}
				}
			}
		})
	}
}

// regCompareSrc's functions return comparisons, which register code
// compiles to a compare-and-branch and the two booleans' loads (the
// compile package's boolBranch): of numbers, NaN among them, strings,
// mixed values, objects whose valueOf runs, null and undefined.
const regCompareSrc = `
function lt(a, b) { return a < b }
function le(a, b) { return a <= b }
function gt(a, b) { return a > b }
function ge(a, b) { return a >= b }
function eq(a, b) { return a == b }
function ne(a, b) { return a != b }
function seq(a, b) { return a === b }
function sne(a, b) { return a !== b }
function nle(a, b) { return !(a <= b) }
function lti(a) { return a < 3 }
function isNull(a) { return a == null }
function notNull(a) { return a != null }
var calls = 0;
var vo = { valueOf() { calls++; return 2 } };
var values = [1, 2, 3, -0, 0, NaN, Infinity, "2", "10", "a", null, undefined, true, vo];
function table() {
	var out = [];
	for (var i = 0; i < values.length; i++) {
		var a = values[i];
		out.push(lti(a) ? 1 : 0, isNull(a) ? 1 : 0, notNull(a) ? 1 : 0);
		for (var j = 0; j < values.length; j++) {
			var b = values[j];
			out.push(+lt(a, b), +le(a, b), +gt(a, b), +ge(a, b), +eq(a, b), +ne(a, b), +seq(a, b), +sne(a, b), +nle(a, b));
		}
	}
	return out.join("") + ":" + calls;
}
function count(n, k) { var s = 0; for (var i = 0; i < n; i++) if (seq(i & 15, k)) s++; return s }
`

// A comparison's boolean is lowered as the comparison (boolBranch), not
// as a branch to two constants: every answer the interpreter's, valueOf
// run as often, idle and marking; and a test of the returned boolean,
// inlined, branches on the comparison itself, no phi of the constants.
func TestJITRegComparisonValues(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			listings := map[string]string{}
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.annotate = switchOn })
			jitTestSwitch(t, func(jc *jitTestConfig) {
				jc.codeHook = func(f JITFunction, _ []byte, c *mir.Code) { listings[f.Name] = c.Listing }
			})
			rts := regNativeRuntimes(t, regCompareSrc)
			// Compiled as the code asks, not as stress configures it.
			rts[0].jitStress = jitStressConfig{}
			var got [2][2]string
			for k, r := range rts {
				tbl, _ := r.GetProp(Obj(r.Global()), r.Intern("table"))
				cnt, _ := r.GetProp(Obj(r.Global()), r.Intern("count"))
				for range 30 {
					v, err := r.Call(tbl, Undefined, nil)
					got[k][0] = regNativeShow(r, v, err)
					v, err = r.Call(cnt, Undefined, []Value{Float(3000), Float(5)})
					got[k][1] = regNativeShow(r, v, err)
				}
			}
			if got[0] != got[1] {
				t.Fatalf("with the JIT %q, without %q", got[0], got[1])
			}
			if got[1][1] != "188" {
				t.Errorf("count = %s, want 188", got[1][1])
			}
			for _, name := range []string{"lt", "seq", "nle"} {
				if l, ok := listings[name]; ok && strings.Contains(l, " = phi ") {
					t.Errorf("%s's boolean is a phi:\n%s", name, l)
				}
			}
			if l := listings["count"]; strings.Contains(l, "const tagged [2:") {
				t.Errorf("count branches on a boolean's constants:\n%s", l)
			}
		})
	}
}

// regAccumulateSrc's loop replaces its accumulator each turn through a
// method that constructs the next, which native code inlines with the
// construction: escape analysis keeps the objects unmade round the loop,
// their fields in registers.
const regAccumulateSrc = `
function V2(x, y) { this.x = x; this.y = y }
V2.prototype.add = function (o) { return new V2(this.x + o.x, this.y + o.y) };
var vs = [];
for (var i = 0; i < 16; i++) vs.push(new V2(i, i & 7));
function sum(a, m) { var i, s = new V2(0, 0); for (i = 0; i < m; i++) s = s.add(a[i % a.length]); return s.x + s.y }
function drive(n) { var t = 0; for (var j = 0; j < n; j++) t += sum(vs, 100); return t }
`

// An object a register loop begins with, merged at its header with the
// one each turn makes, is used past the loop's end only from the copy of
// the test before the loop (compile's loopTest), at the end of that
// predecessor: escape analysis takes a phi's use there (liveAt), not in
// the phi's block, which the loop reaches, and keeps the objects unmade,
// where it had made one each turn. Exact answers, idle and marking.
func TestJITRegLoopObjectsUnmade(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			var last string
			jitTestSwitch(t, func(jc *jitTestConfig) { jc.annotate = switchOn })
			jitTestSwitch(t, func(jc *jitTestConfig) {
				jc.codeHook = func(f JITFunction, _ []byte, c *mir.Code) {
					if f.Name == "sum" {
						last = c.Listing
					}
				}
			})
			rts := regNativeRuntimes(t, regAccumulateSrc)
			// Compiled as the code asks, not as stress configures it: the
			// listing is the code a run without stress keeps.
			rts[0].jitStress = jitStressConfig{}
			for k, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("drive"))
				for i := range 40 {
					v, err := r.Call(fn, Undefined, []Value{Float(200)})
					// Node's: 200 calls, each the sum of 100 vectors' x and y.
					if got := regNativeShow(r, v, err); got != "213600" {
						t.Fatalf("runtime %d, call %d: %s", k, i, got)
					}
				}
			}
			if !strings.Contains(last, "propread") || strings.Contains(last, "= call ") {
				t.Errorf("sum makes an object each turn:\n%s", last)
			}
		})
	}
}
