//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regMadeArgsSrc's functions make closures that read their arguments by
// index and count them -- sloppy and strict, a captured binding read --
// and call them, the closure made anew each turn: inlined where called and
// never made (jitRegFeedback's madeInline, InlineSite's ReadsArguments);
// one reads past the call's count and one by a key the build does not
// know, Go's, which stops the closure being inlined at its call once it
// has left native code there too often (jitInlineLeft); one throws
// once from inside, where Go makes the closure and its frame is given the
// call's arguments (jitRegInlineArgs); and one with a rest parameter,
// called, not inlined.
const regMadeArgsSrc = `
function reads(n) {
	var s = 0, k = 3;
	for (var i = 0; i < n; i++) {
		var f = function () { return arguments[0] * k + (arguments[1] | 0) + arguments.length };
		s += f(i, 2);
		var g = function () { "use strict"; return arguments[1] - arguments[0] * arguments.length };
		s += g(i, 1, 2);
		s = s % 1000000007;
	}
	return s;
}
function past(n) {
	var s = 0;
	for (var i = 0; i < n; i++) {
		var g = function () { "use strict"; return (arguments[2] === undefined ? 7 : arguments[2]) - arguments[0] };
		s += g(i, 1);
	}
	return s;
}
function keyed(n) {
	var s = 0;
	for (var i = 0; i < n; i++) {
		var f = function (j) { "use strict"; return arguments[j] + arguments.length };
		s += f(i & 1, 5, 6);
	}
	return s;
}
function throws(n) {
	var s = 0, m = "";
	for (var i = 0; i < n; i++) {
		var f = function () { if (arguments[0] === 9000) throw new RangeError("at " + arguments[0]); return arguments[0] + arguments.length };
		try { s += f(i, 1) } catch (e) { m = e.message }
	}
	return s + ":" + m;
}
function rests(n) {
	var s = 0;
	for (var i = 0; i < n; i++) {
		var f = function (a, ...more) { return a + more.length + (more[0] | 0) };
		s += f(i, 4, 5) + f(i);
	}
	return s;
}
function main(n) { return reads(n) + ";" + past(n) + ";" + keyed(n) + ";" + throws(n) + ";" + rests(n) }
`

// TestJITRegMadeClosuresReadArguments runs regMadeArgsSrc's main natively
// and with no JIT, the collector idle and marking, and compares them with
// the exact answer: once warm, the closures that only read their arguments
// are inlined where they are called and not made, their reads the call's
// arguments, no exit at their reads and counts; and Go makes them where
// one throws inside.
func TestJITRegMadeClosuresReadArguments(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const want = "50000;-49925000;55000;50005998:at 9000;100050000"
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			RecordJITCompiles(true)
			defer RecordJITCompiles(false)
			rts := regNativeRuntimes(t, regMadeArgsSrc)
			r := rts[0]
			r.SetJITWarmUp(1, 1)
			run := func() {
				t.Helper()
				var got [2]string
				for i, r := range rts {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern("main"))
					v, err := r.Call(fn, Undefined, []Value{Float(10000)})
					got[i] = regNativeShow(r, v, err)
				}
				if got[0] != got[1] || got[0] != want {
					t.Fatalf("main: with the JIT %q, without %q, want %q", got[0], got[1], want)
				}
			}
			for range 4 {
				run()
			}
			// What follows counts what native code did, which the stress
			// modes' compiles, deoptimizations and budgets change.
			r.jitStress = jitStressConfig{}
			for range 4 {
				run()
			}
			before := r.RegNativeStats().HostOps
			run()
			after := r.RegNativeStats().HostOps
			for _, op := range []string{"args_index", "args_len", "arguments"} {
				if n := after[op] - before[op]; n != 0 {
					t.Errorf("%d exits at %s once warm: %v", n, op, after)
				}
			}
			unmade := map[string]int{}
			for key, e := range r.jit.regCache {
				if fn := key.Value(); fn != nil && e != nil && e.keep != nil {
					unmade[fn.Name] += e.keep.called + e.keep.unmade
				}
			}
			for _, name := range []string{"reads", "throws"} {
				if unmade[name] == 0 {
					t.Errorf("%s makes the closures it calls: %v", name, unmade)
				}
			}
			if r.jit.materialized == 0 {
				t.Error("Go made no closure the code did not make")
			}
			if fns, _, n := regCompileARM64(t, r); fns == 0 || n == 0 {
				t.Errorf("arm64: %d functions compiled, %d callees inlined", fns, n)
			}
		})
	}
}
