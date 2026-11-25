//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"
)

// madeCallSetup's mk makes a function at each of its invocations and calls
// it at two sites, around a recursion of its own, as EarleyBoyer's and
// argsRecursionSetup's tree do; the test keeps g out of line.
const madeCallSetup = `
	function mk(d) {
		const g = function (k) { let s = 0; for (let i = 0; i < 3; i++) s += k * i + (s & 1); return s }
		return g(d) + (d > 0 ? mk(d - 1) : 0) + g(d + 1)
	}
	function run(n) { let s = 0; for (let i = 0; i < n; i++) s += mk(12); return s }`

// A call of a closure the calling function's own code makes is checked by
// the closure's code from the first compile (jitRegMadeBy): each
// invocation makes a new object, which a call checked by the object it
// learned misses at every invocation but that one's. Each of mk's sites
// had learned the code at a compile of its own, the first left native code
// at each level of the recursion under way, the interpreter finishing
// every one. Every answer is the interpreter's, and from the start no run
// leaves native code at a call of g, nor finishes in the interpreter.
func TestJITMadeClosureCalledByCode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer holdCollectorNow(t)()
	both := jitBothForTest(t, madeCallSetup)
	r := both.jit
	jitNoInlineForTest(t, "g")
	for range 10 {
		both.agree(t, `run(5)`)
	}
	m := jitNativeForTest(t, r, "mk")
	if m.ssaStats.hosts != 0 || r.jit.interpreted != 0 {
		t.Errorf("mk left native code %d times, the interpreter finished %d runs; want none", m.ssaStats.hosts, r.jit.interpreted)
	}
}
