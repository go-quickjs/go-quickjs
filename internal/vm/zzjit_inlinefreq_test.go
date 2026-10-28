//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// jitFreqBothForTest is jitBothForTest with the JIT's own thresholds, so
// that the existing tiers run, and count, a function's calls while it
// warms, as in ordinary use; pick is not inlined, so that its own code is
// compiled from its counts.
func jitFreqBothForTest(t *testing.T, setup string) jitBoth {
	t.Helper()
	runtime.GC()
	t.Cleanup(func() { debug.SetGCPercent(debug.SetGCPercent(-1)) })
	debug.SetGCPercent(-1)
	jitNoInlineForTest(t, "pick")
	both := jitBothForTest(t, setup)
	jitDefaultsForTest(both.jit)
	return both
}

// jitInlinesForTest is how many times the last compile of the function
// name inlined the closure of each of srcs, the global expressions.
func jitInlinesForTest(t *testing.T, r *Runtime, inlined map[string][]uintptr, name string, srcs ...string) []int {
	t.Helper()
	var n []int
	for _, src := range srcs {
		n = append(n, jitCountForTest(inlined[name], jitClosureForTest(t, r, src)))
	}
	return n
}

// A call made less often than V8's min_inlining_frequency (0.15) for each
// invocation of the function compiled, as the existing tiers counted it
// while the function warmed, is not inlined, whatever its size, and one
// made more often is: pick calls hot at each of its invocations and cold,
// as long, once in a hundred; little, of four instructions, once in ten.
// Each answer is the interpreter's.
func TestJITInlinesFrequentCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	inlined := jitInlinedForTest(t)
	both := jitFreqBothForTest(t, `
		function hot(o) { return (o.x + 3) * (o.y - 3) + (o.x ^ o.y) - (o.y | 3) * o.x }
		function cold(o) { return (o.y + 3) * (o.x - 3) + (o.y ^ o.x) - (o.x | 3) * o.y }
		function little(o) { let t = o.x; return t }
		function pick(o, i) {
			let s = hot(o)
			if (i % 100 === 99) s += cold(o)
			if (i % 10 === 9) s += little(o)
			return s
		}
		function drive(n, o) { let s = 0; for (let i = 0; i < n; i++) s = (s + pick(o, i)) | 0; return s }
		var p = {x: 5, y: 9}`)
	both.agree(t, `drive(2000, p)`, `drive(500, p)`)
	r := both.jit
	if e := jitCallbackEntry(r, "pick"); e == nil || e.ssa == nil || e.calls == nil || e.calls.runs < 100 {
		t.Fatalf("pick has no code compiled from its counts: %+v", e)
	}
	if got := jitInlinesForTest(t, r, inlined, "pick", `hot`, `cold`, `little`); got[0] != 1 || got[1] != 0 || got[2] != 0 {
		t.Errorf("pick inlines hot, cold and little %v times, want 1, 0, 0", got)
	}
}

// Inlining's budget goes to the calls made most often, as V8's
// JSInliningHeuristic takes its candidates by frequency: pick's five calls
// of callees of 44 instructions, four of which the budget (192) holds, are
// made once at each invocation but b0's, the first, made one in four; it is
// the one not inlined. In the order of their PCs, b0 had been.
func TestJITInlinesByFrequency(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	inlined := jitInlinedForTest(t)
	body := `return (o.a + o.b * 3 - o.c) * (o.b - o.a) + (o.c ^ o.a) - (o.a | o.c) * o.b + (o.c - o.b) * (o.a + o.c)`
	both := jitFreqBothForTest(t, `
		function b0(o) { `+body+` }
		function b1(o) { `+body+` }
		function b2(o) { `+body+` }
		function b3(o) { `+body+` }
		function b4(o) { `+body+` }
		function pick(o, i) {
			let s = 0
			if (i % 4 === 0) s += b0(o)
			s += b1(o) + b2(o) + b3(o) + b4(o)
			return s | 0
		}
		function drive(n, o) { let s = 0; for (let i = 0; i < n; i++) s = (s + pick(o, i)) | 0; return s }
		var p = {a: 3, b: 2, c: 7}`)
	both.agree(t, `drive(2000, p)`, `drive(500, p)`)
	r := both.jit
	if got := jitInlinesForTest(t, r, inlined, "pick", `b0`, `b1`, `b2`, `b3`, `b4`); got[0] != 0 || got[1] != 1 || got[2] != 1 || got[3] != 1 || got[4] != 1 {
		t.Errorf("pick inlines b0 to b4 %v times, want 0, 1, 1, 1, 1", got)
	}
}

// A call inside an inlined callee is made its own frequency, in its
// callee's counts, times its callee's call's, as V8 multiplies them
// through the frames it inlines: mid calls leaf at four in ten of its
// invocations. pick calls mid at each of its invocations, so that leaf's
// call is made 0.4 times one of pick's, and mid is inlined with leaf; mid
// at three in ten, so that leaf's is 0.12, below V8's minimum, and mid,
// which inlines all of its calls or none, is not inlined -- though leaf's
// own frequency, and mid's call's, are above it. Each answer is the
// interpreter's.
func TestJITInlinedFrequenciesMultiply(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	inlined := jitInlinedForTest(t)
	setup := func(often string) string {
		return `
		function leaf(o) { let t = (o.x * 3 + o.y) | 0; return t }
		function mid(o, i) { let s = o.x + 1; if (i % 10 < 4) s += leaf(o); return s }
		function pick(o, i) { let s = o.y; if (` + often + `) s += mid(o, i); return s }
		function drive(n, o) { let s = 0; for (let i = 0; i < n; i++) s = (s + pick(o, i)) | 0; return s }
		var p = {x: 5, y: 9}`
	}
	for _, c := range []struct {
		often string
		want  int
	}{{`true`, 1}, {`i % 10 >= 7`, 0}} {
		both := jitFreqBothForTest(t, setup(c.often))
		both.agree(t, `drive(2000, p)`, `drive(500, p)`)
		r := both.jit
		e, me := jitCallbackEntry(r, "pick"), jitCallbackEntry(r, "mid")
		if e == nil || e.ssa == nil || me == nil || me.calls == nil || me.calls.runs == 0 {
			t.Fatalf("%s: pick has no code, or mid no counts: %+v, %+v", c.often, e, me)
		}
		if got := jitInlinesForTest(t, r, inlined, "pick", `mid`, `leaf`); got[0] != c.want || got[1] != c.want {
			t.Errorf("%s: pick inlines mid and leaf %v times, want %d", c.often, got, c.want)
		}
	}
}
