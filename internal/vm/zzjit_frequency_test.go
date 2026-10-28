//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"slices"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// frequencySetup's caller makes, at each invocation, three calls of g in a
// loop, one method call, none of g where k is negative, one construction,
// one call of a pure body, which the call path answers without a frame,
// one of g through Function.prototype.call, whose frame is the built-in's,
// and one call in tail position; tail's call of g is in tail position too.
const frequencySetup = `
	function C(x) { let t = x + 1; this.v = t }
	function g(o) { let t = o.n + 1; o.n = t & 1023; return t }
	var obj = { m(o) { let t = o.n + 2; o.n = t & 1023; return t } }
	function pure(o) { return o.n * 2 }
	function tail(o) { return g(o) }
	function caller(o, k) {
		let s = 0
		for (let i = 0; i < 3; i++) s += g(o)
		s += obj.m(o)
		if (k < 0) s += g(o)
		const c = new C(s)
		s += c.v + pure(o)
		s += g.call(null, o)
		return tail(o) + s
	}
	var o = {n: 0}`

// frequencyRuntime is a runtime running frequencySetup with the JIT's own
// thresholds, the tree tier on or off for the functions it runs.
func frequencyRuntime(t *testing.T, tree bool) *Runtime {
	t.Helper()
	was := treeTier.Load()
	SetTreeTier(tree)
	t.Cleanup(func() { SetTreeTier(was) })
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitDefaultsForTest(r)
	if _, err := r.Run(compileForTest(t, frequencySetup)); err != nil {
		t.Fatal(err)
	}
	return r
}

// frequencyCounts is name's function's counts: its runs, and each call's
// count, in the order of their PCs.
func frequencyCounts(t *testing.T, r *Runtime, name string) (uint32, []uint32) {
	t.Helper()
	e := jitCallbackEntry(r, name)
	if e == nil || e.calls == nil {
		t.Fatalf("%s counts nothing: %+v", name, e)
	}
	return e.calls.runs, slices.Clone(e.calls.calls)
}

// The existing tiers count each call site's calls, and each function's
// invocations, from the one that gives it an entry (the eighth,
// jitInvocationsForFeedback) on, as V8's feedback counts them from its
// vector's allocation: exactly, in the interpreter and in the tree tier
// alike. caller, invoked 50 times, counts 43 runs; g's call in the loop
// 129 calls, three for each, the method call and the construction 43, the
// call never made none; the pure body's, answered without a frame, 43
// (jitCountPure), and g's through Function.prototype.call, made in call's
// frame, 43, at the call of call. A call in tail position, not deep in the
// stack, is counted as any call is: caller's of tail 43, tail's of g 43 of
// its own 43 runs. Their counts over the runs (jitCallCount) are 3, 1, 0
// and 1, known from 19 runs on (jitCountsEnough).
func TestJITCountsCallsBySite(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, trees := range []bool{true, false} {
		t.Run(fmt.Sprintf("tree=%v", trees), func(t *testing.T) {
			r := frequencyRuntime(t, trees)
			if _, err := r.Run(compileForTest(t, `for (let i = 0; i < 50; i++) caller(o, i)`)); err != nil {
				t.Fatal(err)
			}
			if e := jitCallbackEntry(r, "caller"); e == nil || !e.warming {
				t.Fatalf("caller's entry %+v, not warming: the test is of the existing tiers", e)
			}
			runs, calls := frequencyCounts(t, r, "caller")
			if want := []uint32{129, 43, 0, 43, 43, 43, 43}; runs != 43 || !slices.Equal(calls, want) {
				t.Errorf("caller counts %d runs, calls %v; want 43, %v", runs, calls, want)
			}
			runs, calls = frequencyCounts(t, r, "tail")
			if want := []uint32{43}; runs != 43 || !slices.Equal(calls, want) {
				t.Errorf("tail counts %d runs, calls %v; want 43, %v", runs, calls, want)
			}
			e, fn := jitCallbackEntry(r, "caller"), jitFnForTest(t, r, "caller")
			if got := (*tree)(fn.VMCode) != noTree; got != trees {
				t.Fatalf("caller runs as a tree: %v", got)
			}
			for i, want := range []float64{3, 1, 0, 1} {
				pc := int(e.calls.pcs[i])
				if f, ok := jitCallCount(e, pc, 1); ok != (want >= 0) || ok && f != want {
					t.Errorf("caller's call at %d: frequency %v, %v; want %v", pc, f, ok, want)
				}
				if _, ok := jitCallCount(e, pc, 44); ok || jitCountsEnough != 19 {
					t.Errorf("caller's call at %d: frequency known of 43 runs, 44 enough (jitCountsEnough %d)", pc, jitCountsEnough)
				}
			}
		})
	}
}

// Every form of a call the compiler makes -- of a global, a local, a
// captured binding, a method by name, by key, on this, optional, a
// construction, calls in another's arguments, and of 0 to 5 arguments, of
// pure bodies and of a constructor forwarding its arguments, which the call
// path answers without the callee's frame, and through
// Function.prototype.call and apply, too -- is
// counted at its own site, in the interpreter and in the tree tier: each
// of forms' calls, made once at each invocation, counts its runs.
func TestJITCountsEveryCallForm(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, trees := range []bool{true, false} {
		t.Run(fmt.Sprintf("tree=%v", trees), func(t *testing.T) {
			r := frequencyRuntime(t, trees)
			src := `
				function h(a, b, c, d, e) { let t = (a | 0) + 1; return t }
				function K(a) { let t = a; this.a = t }
				var box = { h, k: 'h', p: { q: { h } }, n: 3, pm(i) { return this.n + i } }
				function W() { this.init.apply(this, arguments) }
				W.prototype.init = function (a) { let t = a; this.a = t }
				const p0 = () => 0, p1 = (x) => x * 2
				function forms(o, i) {
					const loc = h, up = (x) => { let z = x + 1; return z }
					let s = h() + h(1) + h(1, 2) + h(1, 2, 3) + h(1, 2, 3, 4) + h(1, 2, 3, 4, 5)
					s += loc(i) + box.h(i) + box[box.k](i) + box.p.q.h(i) + (box.h?.(i) ?? 0)
					s += new K(i).a + h(h(i), h(i + 1))
					s += o.self(i) + box.pm(i) + p0() + p1(i) + new W(i).a + h.call(null, i) + h.apply(null, [i])
					function inner() { return loc(s) }
					return s + inner() + up(i)
				}
				var self = { h, self(i) { return this.h(i) } }
				for (let i = 0; i < 40; i++) forms(self, i)`
			if _, err := r.Run(compileForTest(t, src)); err != nil {
				t.Fatal(err)
			}
			runs, calls := frequencyCounts(t, r, "forms")
			if runs != 33 || len(calls) == 0 {
				t.Fatalf("forms counts %d runs, calls %v", runs, calls)
			}
			for i, n := range calls {
				if n != runs {
					t.Errorf("forms' call %d (pc %d) counts %d, want %d: %v", i, jitCallbackEntry(r, "forms").calls.pcs[i], n, runs, calls)
				}
			}
			if runs, calls := frequencyCounts(t, r, "self"); runs != 33 || !slices.Equal(calls, []uint32{33}) {
				t.Errorf("self counts %d runs, calls %v", runs, calls)
			}
		})
	}
}

// Native code counts nothing, as V8's optimized code counts no feedback:
// once caller and c2 are compiled, from their budgets, their invocations
// and their calls -- natively, inlined, or through Go, as c2's of gw,
// which is not compiled -- are not counted, and the counts the existing
// tiers made while they warmed are their code's entries'. Each answer is
// the interpreter's.
func TestJITCountsNotNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := frequencyRuntime(t, true)
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	if _, err := want.Run(compileForTest(t, frequencySetup)); err != nil {
		t.Fatal(err)
	}
	both := jitBoth{jit: r, want: want}
	both.script(t, `
		function gw(o) { with (o) { return n } }
		function c2(o, i) { let s = g(o) + gw(o); if (i < 0) s += gw(o); for (let j = 0; j < 40; j++) s = (s * 31 + j) | 0; return s }
		function drive(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + caller(o, i) + c2(o, i)) | 0; return s }`)
	for range 10 {
		both.agree(t, `drive(100)`)
	}
	for _, name := range []string{"caller", "c2"} {
		if e := jitCallbackEntry(r, name); e == nil || e.ssa == nil || e.invalid || e.calls == nil {
			t.Fatalf("%s has no code: %+v", name, e)
		}
	}
	// The last run counted may have gone on in native code from its loop,
	// by on-stack replacement, its calls after it uncounted.
	runs, calls := frequencyCounts(t, r, "caller")
	within := func(n, per uint32) bool { return n <= per*runs && n+per >= per*runs }
	if runs < 43 || runs > 600 || !within(calls[0], 3) || !within(calls[1], 1) || calls[2] != 0 || !within(calls[3], 1) || !within(calls[6], 1) {
		t.Fatalf("caller counts %d runs, calls %v", runs, calls)
	}
	runs2, calls2 := frequencyCounts(t, r, "c2")
	if runs2 < 8 || runs2 > 600 || !slices.Equal(calls2, []uint32{runs2, runs2, 0}) {
		t.Fatalf("c2 counts %d runs, calls %v", runs2, calls2)
	}
	goCalls := r.jit.goCalls
	both.agree(t, `drive(1000)`)
	if r.jit.goCalls-goCalls < 1000 {
		t.Errorf("c2's code made %d calls of Go in 1,000 runs", r.jit.goCalls-goCalls)
	}
	for _, c := range []struct {
		name  string
		runs  uint32
		calls []uint32
	}{{"caller", runs, calls}, {"c2", runs2, calls2}} {
		if n, got := frequencyCounts(t, r, c.name); n != c.runs || !slices.Equal(got, c.calls) {
			t.Errorf("%s's code counted: %d runs, %v, after %d, %v", c.name, n, got, c.runs, c.calls)
		}
	}
}

// The counts outlive the code the cache drops, as V8's feedback outlives
// the code optimized from it (jitGhost): caller's, flushed unused, are its
// next entry's, which goes on counting from them.
func TestJITCountsOutliveCode(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setEpoch := jitFlushForTest(t)
	r := frequencyRuntime(t, true)
	if _, err := r.Run(compileForTest(t, `for (let i = 0; i < 700; i++) caller(o, i)`)); err != nil {
		t.Fatal(err)
	}
	e := jitCallbackEntry(r, "caller")
	if e == nil || e.ssa == nil || e.calls == nil {
		t.Fatalf("caller has no code: %+v", e)
	}
	c, runs := e.calls, e.calls.runs
	setEpoch(1000 + jitFlushAge)
	r.jit.flush()
	setEpoch(1000 + 2*jitFlushAge)
	r.jit.flush()
	if e := jitCallbackEntry(r, "caller"); e != nil {
		t.Fatalf("caller's unused code was not flushed: %+v", e)
	}
	if _, err := r.Run(compileForTest(t, `for (let i = 0; i < 20; i++) caller(o, i)`)); err != nil {
		t.Fatal(err)
	}
	e = jitCallbackEntry(r, "caller")
	if e == nil || e.calls != c || c.runs < runs {
		t.Fatalf("caller's entry %+v counts %+v, not %+v from %d runs", e, e.calls, c, runs)
	}
}

// jitFnForTest is the function of the global name.
func jitFnForTest(t *testing.T, r *Runtime, name string) *bytecode.Function {
	t.Helper()
	return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn
}
