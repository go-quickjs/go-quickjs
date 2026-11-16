//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// regStepCallSrc's loops step iterators written in script: a plain one,
// one left early, whose return is called, one whose results are of other
// layouts -- getters, a done that is no boolean -- and at last no object,
// one whose next throws, whose return is not called, a step that meets an
// array and an iterator in turn, and one whose next leaves native code.
const regStepCallSrc = `
var log = [];
function Range(n) { this.i = 0; this.n = n; }
Range.prototype.next = function () {
    if (this.i < this.n) return { value: this.i++, done: false };
    return { value: undefined, done: true };
};
Range.prototype[Symbol.iterator] = function () { return this; };
Range.prototype.return = function () { log.push("ret" + this.i); return { done: true }; };
function sum(n) { var s = 0; for (var k = 0; k < n; k++) for (var v of new Range(50)) s += v; return s; }
function early(n) { var s = 0; for (var k = 0; k < n; k++) for (var v of new Range(50)) { if (v === 30) break; s += v; } return s + ":" + log.length; }
function Odd(n) { this.i = 0; this.n = n; }
Odd.prototype.next = function () {
    this.i++;
    if (this.i === this.n) return 42;
    if (this.i % 7 === 0) return { get done() { return false; }, get value() { return this.i * 2; }, i: this.i };
    if (this.i % 11 === 0) return { done: 0, value: "s" + this.i };
    if (this.i > this.n + 5) return { value: 1, done: true };
    return { value: this.i, done: false };
};
Odd.prototype[Symbol.iterator] = function () { return this; };
function odd(n) { var s = 0, t = ""; try { for (var v of new Odd(n)) { if (typeof v === "string") t += v; else s += v; } } catch (e) { return s + t + ":" + e.constructor.name; } return s + t; }
function Thrower(n) { this.i = 0; this.n = n; }
Thrower.prototype.next = function () { if (++this.i === this.n) throw new RangeError("at " + this.i); return { value: this.i, done: false }; };
Thrower.prototype[Symbol.iterator] = function () { return this; };
Thrower.prototype.return = function () { log.push("bad"); return {}; };
function thrown(n) { var s = 0; try { for (var v of new Thrower(n)) s += v; } catch (e) { return s + ":" + e.message + ":" + log.length; } return s; }
function mixed(n) { var s = 0, a = [1, 2, 3]; for (var k = 0; k < n; k++) { var it = k % 3 === 0 ? a : new Range(4); for (var v of it) s += v; } return s; }
function Deopt(n) { this.i = 0; this.n = n; }
Deopt.prototype.next = function () { this.i++; var x = this.i < 400 ? this.i : "x" + this.i; if (this.i > this.n) return { value: 0, done: true }; return { value: x, done: false }; };
Deopt.prototype[Symbol.iterator] = function () { return this; };
function deopt(n) { var s = 0, c = 0; for (var v of new Deopt(n)) { if (typeof v === "string") c++; else s += v; } return s + ":" + c; }
function round() { return [sum(200), early(100), odd(40), thrown(30), mixed(300), deopt(1000)].join(";"); }
`

// TestJITRegStepCall: register code's loop's step of an iterator written
// in script calls its next natively (ssa's userStep), through the next's
// link, and reads its result natively where it is of the layout the step
// learned, done false or true (ssa.CallSite's StepResult), Go making the
// iterator protocol of any other -- getters, a done no boolean, no object
// -- and a cursor of anything else, an array's, stepped by Go: each step
// had been a call of Go that called the next in the interpreter
// (iterator_for_of 1.4 times the stack form's code). A next that throws
// leaves its iterator unclosed, a loop left early closes it, and a next
// that leaves native code is finished by Go, the step made of what it
// returned, the next never called again. Each answer is Node's, with the
// collector idle and marking, and the steps of the plain iterator are
// native.
func TestJITRegStepCall(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, regStepCallSrc)
			rts[0].SetJITReport(true)
			var got [2][]string
			for k, r := range rts {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("round"))
				for range 40 {
					v, err := r.Call(fn, Undefined, nil)
					got[k] = append(got[k], regNativeShow(r, v, err))
				}
			}
			// Node's first three rounds: the log grows by the loops left
			// early, a hundred a round, and never by a thrower's return.
			want := []string{
				"245000;43500:100;819s11s22s33:TypeError;435:at 30:100;1800;79800:601",
				"245000;43500:200;819s11s22s33:TypeError;435:at 30:200;1800;79800:601",
				"245000;43500:300;819s11s22s33:TypeError;435:at 30:300;1800;79800:601",
			}
			for i, w := range want {
				if got[0][i] != w || got[1][i] != w {
					t.Errorf("round %d: with the JIT %q, without %q, want %q", i, got[0][i], got[1][i], w)
				}
			}
			for i := range got[0] {
				if got[0][i] != got[1][i] {
					t.Errorf("round %d: with the JIT %q, without %q", i, got[0][i], got[1][i])
				}
			}
			if !strings.HasSuffix(got[0][39], ":4000;1800;79800:601") {
				t.Errorf("last round %q", got[0][39])
			}
			st := rts[0].RegNativeStats()
			if st.Entries == 0 {
				t.Fatal("no native code entered")
			}
			// sum's steps alone are 40 x 200 x 51: nearly all native.
			if n := st.GoCallOps["iter_next"]; n > 40*200*51/20 {
				t.Errorf("%d steps made by Go, of more than %d: %v", n, 40*200*51, st.GoCallOps)
			}
		})
	}
}
