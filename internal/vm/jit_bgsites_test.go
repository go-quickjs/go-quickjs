//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"strings"
	"testing"
)

// The call sites' pass of a snapshot's gather (jitCallSiteFacts) keeps its
// table by register in the runtime (jitState's siteRegs), reused from one
// closure's pass to the next: what a snapshot hands to the worker must not
// point into it, as the next compile's gather writes it while the first's
// derivation runs. Six functions of calls grow hot in one loop, their
// compiles handed over back to back with none waited for; the answers are
// a foreground runtime's, and under the race detector a worker's read of
// the table is a race with the next gather's writes.
func TestJITBackgroundGathersBackToBack(t *testing.T) {
	t.Parallel()
	var src strings.Builder
	src.WriteString(`
function add(a, b) { return a + b }
function Pair(a, b) { this.a = a; this.b = b }
const o = {m(x) { return x * 2 }}
`)
	names := []string{"f0", "f1", "f2", "f3", "f4", "f5"}
	for _, n := range names {
		src.WriteString(`function ` + n + `(n) {
	let s = 0, t = 1, u = 2, v = 3
	for (let i = 0; i < n; i++) {
		const p = new Pair(i, t)
		s = add(s, p.a) % 1000003
		t = o.m(t) % 1009
		u = add(u, v); v = add(t, u) % 997
	}
	return s + t + u + v
}
`)
	}
	src.WriteString(`function all(n) { let s = 0; for (let k = 0; k < 4; k++) { s += f0(n) + f1(n) + f2(n) + f3(n) + f4(n) + f5(n) } return s }`)
	prog := compileForTest(t, src.String())
	run := func(background bool) (string, *Runtime) {
		r := newTestRuntime(t, Config{JIT: true})
		t.Cleanup(r.Close)
		r.SetJITWarmUp(1, 1)
		r.SetJITBackground(background)
		r.jitStress.background = false
		if _, err := r.Run(prog); err != nil {
			t.Fatal(err)
		}
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern("all"))
		var out string
		for range 8 {
			v, err := r.Call(fn, Undefined, []Value{Float(300)})
			out += regNativeShow(r, v, err) + ";"
		}
		return out, r
	}
	want, _ := run(false)
	// A few runtimes, each its compiles' overlap with the workers anew:
	// one read of the table on a worker shows in most.
	for range 4 {
		got, r := run(true)
		if got != want {
			t.Fatalf("in the background %q, in the foreground %q", got, want)
		}
		if r.jit.bgSubmitted < 2 {
			t.Fatalf("%d compiles handed to the background, want two or more", r.jit.bgSubmitted)
		}
	}
}
