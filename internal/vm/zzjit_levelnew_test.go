//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"testing"
)

// A native callee's level that is a construction, whose frame a call of Go
// deeper puts off (jitLazyLevels), is made a construction's when a stack
// trace reads it: its new.target the call wrote, the frame named "new F",
// as the interpreter's is -- with the collector idle and marking.
func TestJITLazyLevelConstructs(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		function thrower(n, ...q) { if (n > 1) throw new Error('x'); return n + q.length }
		function C(a, ...r) { this.v = thrower(a) + r.length }
		class B { constructor(a, ...r) { this.v = thrower(a) + r.length } }
		class D extends B { constructor(a, ...r) { super(a, ...r); this.w = r.length } }
		const top = e => e.stack.split('\n').slice(1, 4).map(l => l.trim().split(' ')[1]).join()
		function run(n) {
			let s = 0
			for (let i = 0; i < n; i++) s += new C(0, i).v + new D(0, i).w
			let out = ''
			try { new C(5, 1) } catch (e) { out += top(e) + '|' }
			try { new D(5, 1) } catch (e) { out += top(e) }
			return s + ':' + out
		}`
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitAgreesForTest(t, src, "run(300)", "run(300)", "run(3)", "run(300)")
			for _, name := range []string{"thrower", "C"} {
				jitNativeOrCalleeForTest(t, r, name)
			}
		})
	}
}
