//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// staleInlineSetup's run inlines get and calls step natively, whose
// forEach -- a call of Go -- calls replaceRun at the run's 30th object and
// its 50th, which acts on the second, native code going on after it; the
// 102nd object, the last, whose k is a getter, leaves get's inlined code.
const staleInlineSetup = `
	function get(o) { return o.k }
	function step(k) { if (k) [0].forEach(replaceRun); return 1 }
	var plain = [], mixed = []
	for (let i = 0; i < 102; i++) {
		plain.push({ k: i })
		mixed.push(i < 101 ? { k: i } : { get k() { return 7 } })
	}
	function run(os, at) {
		let s = 0
		for (let i = 0; i < os.length; i++) {
			s += step(i === at || i === at - 20)
			s += get(os[i])
		}
		return s
	}
`

// An exit inside a callee inlined by the code a run entered with finds
// the callee's closure in what that code inlines, which the run keeps
// (jitGoRun's inlined), not in what the function's entry says its code
// inlines: a call of Go the run made may have had the function compiled
// again meanwhile (otherwiseInUse), the run going on in the code it had,
// as V8 leaves an optimized frame in code it replaced. replaceRun gives
// run's entry such lists, those of code that inlines nothing, on its
// second call (the first has the run resume after step, whose call of Go
// it was, which a run compiled again does not). Go had panicked ("an
// inlined callee's closure is not its caller's"), which Octane's
// Typescript met in one run of some ten: a native callee's call of Go had
// entered the run's function again, and that entry compiled it again
// (jitRecompileLearned).
func TestJITInlinedByCodeReplaced(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	var e *jitEntry
	var had []*closure
	calls := 0
	for _, rt := range []*Runtime{want, r} {
		rt.global.setOwnRaw(rt.atoms.intern("replaceRun"), Obj(rt.newNativeFunc("replaceRun", 0,
			func(rt *Runtime, this Value, args []Value) (Value, error) {
				if rt != r {
					return Undefined, nil
				}
				if calls++; calls%2 == 1 {
					return Undefined, nil
				}
				for k, x := range rt.jit.cache {
					if fn := k.Value(); fn != nil && fn.Name == "run" {
						e = x
					}
				}
				if e == nil || e.ssa == nil || len(e.ssaInlined) == 0 {
					t.Errorf("run's code inlines nothing: %+v", e)
					return Undefined, nil
				}
				had, e.ssaInlined = e.ssaInlined, nil
				return Undefined, nil
			})), propDefault)
		if _, err := rt.Run(compileForTest(t, staleInlineSetup)); err != nil {
			t.Fatal(err)
		}
	}
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `run(plain, -1)`)
	}
	rounds = append(rounds, `run(mixed, 50)`)
	for i, src := range rounds {
		src = "''+(" + src + ")"
		wv, werr := want.Run(compileForTest(t, src))
		gv, gerr := r.Run(compileForTest(t, src))
		if werr != nil || gerr != nil || gv.String().Go() != wv.String().Go() {
			t.Fatalf("round %d (%s): %v, %v; interpreter %v, %v", i, src, gv, gerr, wv, werr)
		}
	}
	if had == nil {
		t.Fatal("run's entry was never given other code's lists")
	}
	e.ssaInlined = had
	if e.ssaStats.hosts == 0 {
		t.Fatal("run's code never left get's inlined code")
	}
}
