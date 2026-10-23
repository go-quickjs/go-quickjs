//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// jitAsyncAgreesForTest runs setup and then src, a script whose promise
// jobs leave its answer in out, in the interpreter, the tree tier, and the
// JIT under each stress setting the differential tests use, and fails
// unless each answers as the interpreter does. It returns the JIT's
// runtime at its default setting, src run there rounds times.
func jitAsyncAgreesForTest(t *testing.T, setup, src string, rounds int) *Runtime {
	t.Helper()
	run := func(r *Runtime, tree bool) string {
		previous := treeTier.Swap(tree)
		defer treeTier.Store(previous)
		if _, err := r.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
		got := ""
		for range rounds {
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			if err := r.runMicrotasks(); err != nil {
				t.Fatal(err)
			}
			v, err := r.Run(compileForTest(t, `''+out`))
			if err != nil {
				t.Fatal(err)
			}
			got += v.String().Go() + ";"
		}
		return got
	}
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	w := run(want, false)
	tr := New(Config{})
	t.Cleanup(func() { tr.Close(); tr.ReleaseClosed() })
	if got := run(tr, true); got != w {
		t.Fatalf("tree tier: %s\ninterpreter: %s", got, w)
	}
	for _, c := range []jitStressConfig{
		{threshold: true},
		{threshold: true, budget: 1},
		{threshold: true, budget: 5, deopt: 2},
		{threshold: true, budget: 3, deopt: 7},
	} {
		r := jitRuntimeForTest(t, Config{JIT: true})
		r.jitStress = c
		if got := run(r, true); got != w {
			t.Fatalf("JIT %+v: %s\ninterpreter: %s", c, got, w)
		}
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if got := run(r, true); got != w {
		t.Fatalf("JIT: %s\ninterpreter: %s", got, w)
	}
	return r
}

// Async functions and generators of calls and no loop, compiled once their
// resumptions have made them hot, as any function's calls make it
// (LowerSSAHot): each resumption -- the first, past the parameters, and
// each after an await or a yield -- enters native code at its pc. LowerSSA
// refuses them for their want of a loop, and Go had marked them refused at
// their first resumption, for good: npm's 521 async functions of calls,
// which the tree tier ran. A guard that fails (an object of another
// shape), a throw from a call caught by the body's handler, a rejection
// thrown at the await, a generator whose locals a closure captures, which
// no loop's step resumes natively, one whose every resumption is a call
// of next() native code makes, an async function that never awaits, whose
// one resumption is its call: each answer the interpreter's.
func TestJITAsyncHotResumptions(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		var out
		function get(o, k) { return o[k] }
		function add(a, b) { return a + b }
		function thrower(x) { if (x % 5 === 4) throw new RangeError('r' + x); return x }
		async function step(o, i) { const v = await get(o, 'x'); return add(add(v, i), get(o, 'y')) }
		async function guarded(o, i) { try { return add(await thrower(i), o.x) } catch (e) { return -e.message.length } }
		async function rejects(i) { const v = await (i % 3 ? i : Promise.reject(new Error('e' + i))); return add(v, 1) }
		var arrow = async (o, i) => add(await o.x, get(o, 'y') * i)
		async function plain(o, i) { return add(get(o, 'x'), i) }
		function* pair(a) { const c = () => a; yield add(c(), 1); yield add(a, get({ z: 2 }, 'z')) }
		function* steps(a) { const c = () => a; const x = yield add(c(), 1); yield add(x, get({ z: 2 }, 'z')) }
		function pump(n) { let s = 0; for (let i = 0; i < n; i++) { const g = steps(i); s += g.next().value; s += g.next(i).value; s += g.next().done ? 1 : 0 } return s }
		async function drive(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				const o = i % 7 === 6 ? { y: 2, x: 3 } : { x: 3, y: 2 }
				s += await step(o, i)
				s += await guarded(o, i)
				try { s += await rejects(i) } catch (e) { s += e.message.length * 100 }
				s += await arrow(o, i)
				s += await plain(o, i)
				for (const v of pair(i)) s += v
			}
			return s
		}`
	r := jitAsyncAgreesForTest(t, setup, `drive(1500).then(v => out = v + ':' + pump(1500))`, 3)
	for _, name := range []string{"step", "guarded", "rejects", "arrow", "plain", "pair", "steps"} {
		e := jitNativeForTest(t, r, name)
		if !e.hot {
			t.Errorf("%s compiled, but not once hot (LowerSSAHot): %+v", name, e)
		}
	}
}
