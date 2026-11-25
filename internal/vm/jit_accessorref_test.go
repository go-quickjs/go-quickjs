//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"
)

// accessorRefSetup's loops read getters whose values no number operation
// takes -- compared with the receiver, kept, returned -- of receivers of
// three classes, the getter on the base class's prototype.
const accessorRefSetup = `
	class P { constructor(x) { this.x = x; this.inner = { v: x } } get self() { return this } get box() { return this.inner } }
	class Q extends P {}
	class S extends Q {}
	const ps = [new P(1), new Q(2), new S(3), new P(4)]
	function selves(n) { let k = 0; for (let i = 0; i < n; i++) { const o = ps[i % ps.length]; if (o.self === o) k++ } return k }
	function boxes(n) { let s = 0, last; for (let i = 0; i < n; i++) { last = ps[i & 3].box; s += last.v } return s + ':' + last.v }`

// A register read whose value no number operation takes is a reference
// read (compile's selectNumericProperties), which writes its own register:
// a getter it finds is called, or inlined, as at any read. Taken for a
// slot program's method read, which keeps its receiver below its value,
// such a read never asked for its accessor, compiled as a data read whose
// guard failed at every compile: selves deoptimized four times and ran in
// the interpreter. Every answer is the interpreter's, and the loops run
// natively, leaving native code at none of their reads, nor calling Go.
func TestJITAccessorReferenceRead(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{}
	for range 8 {
		rounds = append(rounds, `selves(40)`, `boxes(40)`)
	}
	r := jitAgreesForTest(t, accessorRefSetup, rounds...)
	defer holdCollectorNow(t)()
	for _, c := range []struct{ name, run, want string }{{"selves", "selves(4000)", "4000"}, {"boxes", "boxes(4000)", "10000:4"}} {
		e := jitNativeForTest(t, r, c.name)
		before := e.ssaStats
		if v, err := r.Run(compileForTest(t, "''+"+c.run)); err != nil || v.String().Go() != c.want {
			t.Fatalf("%s = %s, %v; want %s", c.run, v.String().Go(), err, c.want)
		}
		st := e.ssaStats
		if st.work-before.work < 1000 || st.hosts != before.hosts || st.guards != before.guards || st.goCalls != before.goCalls || st.ended != before.ended {
			t.Errorf("%s did not run natively throughout: %+v, before %+v", c.run, st, before)
		}
	}
}
