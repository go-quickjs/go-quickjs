//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"testing"
)

// argsRecursionSetup is EarleyBoyer's shape: lst and app read their
// arguments -- lst through `var a = arguments`, a's length and elements
// read as an object's, app by index and count -- and tree makes, at each
// level of its recursion, a closure that calls them, called natively from
// deeper in the recursion while its outer runs are in its code.
const argsRecursionSetup = `
	function lst() { var a = arguments; var r = 0; for (var i = a.length - 1; i >= 0; i--) r += a[i]; return r }
	function app() { if (arguments.length === 0) return 0; var r = arguments[arguments.length - 1]; for (var i = arguments.length - 2; i >= 0; i--) r += arguments[i]; return r }
	function tree(d) {
		if (d === 0) return lst(1, 2, 3)
		const step = function (k) { let s = 0; for (let i = 0; i < 2; i++) s += app(k, i) + lst(k); return s }
		return step(d) + tree(d - 1) + step(d)
	}
	function run(n) { let s = 0; for (let i = 0; i < n; i++) s += tree(12); return s }`

// Neither an arguments object's length and elements, nor a recursion's
// calls of Go, leave native code at every call:
//   - a.length of an object that is no array, once its speculation has
//     failed, is Go's read from native code (ssa.GoHost); and at an exit Go
//     reads it, and an element by a local's key, and goes on natively, where
//     jitHost had made neither, the interpreter finishing the invocation;
//   - a[i] of an arguments object that aliases nothing is Go's read from
//     native code (jitKeyedClass, ssa.GoIndex), as an ordinary object's;
//   - a call of Go leaves code due to be compiled again only where the
//     compile can be made then (jitCompileDueNow): step's, called natively
//     from deeper in its own recursion, had left at each call for a compile
//     refused while those calls were in its code.
//
// EarleyBoyer's sc_list, sc_append and loop3 had so left native code some
// 3,000 times a run, the interpreter finishing over 200 invocations. Each
// mechanism alone undone has this leave 16 to 130 times.
func TestJITArgumentsInRecursion(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	both := jitBothForTest(t, argsRecursionSetup)
	for range 30 {
		both.agree(t, `run(20)`)
	}
	// A few, each speculation's first failure: lst's and app's code too,
	// native callees of their own.
	if s := both.jit.jit; s.interpreted > 8 || s.hosts > 8 {
		t.Errorf("finished in the interpreter %d times, left for Go %d times", s.interpreted, s.hosts)
	}
	jitAgreesForTest(t, argsRecursionSetup, `run(2)`, `run(5)`)
}
