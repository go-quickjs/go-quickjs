//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "testing"

// regStaleInlineSetup's run inlines get and calls step, whose forEach --
// a call of Go -- calls replaceRun at the run's 30th object and its 50th,
// which acts on the second, native code going on after it; the 102nd
// object, the last, whose k is a getter, leaves get's inlined code.
const regStaleInlineSetup = `
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

// An exit inside a callee inlined by the code a run entered finds the
// callee's closure in what that code inlines (its keep's inlined), not in
// what the function's code is by then: a call of Go the run made may have
// invalidated the code and had the function compiled again, the run going
// on in the code it had, as V8 leaves an optimized frame in code it
// replaced. replaceRun, on its second call, from inside run's native code,
// invalidates run's register code (jitRegInvalidate), which inlines get;
// the run goes on in it to the getter, where it leaves get's inlined code.
// Each answer is the interpreter's.
func TestJITRegInlinedByCodeReplaced(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	want := newTestRuntime(t, Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	calls, invalidated := 0, false
	var hostsBefore uint64
	for _, rt := range []*Runtime{want, r} {
		rt.global.setOwnRaw(rt.atoms.intern("replaceRun"), Obj(rt.newNativeFunc("replaceRun", 0,
			func(rt *Runtime, this Value, args []Value) (Value, error) {
				if rt != r {
					return Undefined, nil
				}
				if calls++; calls%2 == 1 {
					return Undefined, nil
				}
				fd := rt.global.getOwn(rt.atoms.intern("run")).value().Object().fn()
				e := jitRegEntryOfForTest(rt, fd.closure)
				if e == nil || e.code == nil || e.invalid {
					return Undefined, nil
				}
				get := rt.global.getOwn(rt.atoms.intern("get")).value().Object().fn().closure
				inlines := false
				for _, x := range jitInlinedOfForTest(rt, fd.closure) {
					inlines = inlines || x.fn == get.fn
				}
				if !inlines {
					t.Errorf("run's code does not inline get: %v", jitInlinedOfForTest(rt, fd.closure))
					return Undefined, nil
				}
				hostsBefore = e.life.ssaStats.hosts + e.life.ssaStats.guards
				rt.jitRegInvalidate(fd.closure, e, "test")
				invalidated = true
				return Undefined, nil
			})), propDefault)
		if _, err := rt.Run(compileForTest(t, regStaleInlineSetup)); err != nil {
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
	if !invalidated {
		t.Fatal("run's code was never invalidated under way")
	}
	life := jitRegLifeForTest(r, "run")
	if life == nil {
		fd := r.global.getOwn(r.atoms.intern("run")).value().Object().fn()
		if e := jitRegEntryOfForTest(r, fd.closure); e != nil {
			life = e.life
		}
	}
	if life == nil || life.ssaStats.hosts+life.ssaStats.guards == hostsBefore {
		t.Fatal("run's code never left get's inlined code after it was invalidated")
	}
}
