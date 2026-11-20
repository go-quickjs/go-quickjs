//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"
)

// regDeepSrc recurses natively far past the contexts a runtime makes at
// first: a sum 10,000 deep; a recursion without end, whose RangeError is
// the VM's depth limit's; an error thrown at the bottom of one 10,000 deep
// and caught at its top, and one caught halfway, whose stack is the
// interpreter's; and an Error's stack taken at the bottom.
const regDeepSrc = `
function sum(n) { return n == 0 ? 0 : n + sum(n - 1) }
var deepest = 0;
function forever(n) { deepest = n; return forever(n + 1) + 1 }
function overflow() {
	try { return "returned " + forever(0) } catch (e) { return e.name + ": " + e.message + " at " + deepest }
}
function thrower(n) { if (n == 0) throw new Error("bottom"); return thrower(n - 1) + 1 }
function throws(n) { try { return thrower(n) } catch (e) { return e.message + "|" + e.stack } }
function half(n, k) { if (n == k) { try { return thrower(k) } catch (e) { return -n } } return half(n - 1, k) + 1 }
function stackAt(n) { return n == 0 ? new Error("deep").stack : stackAt(n - 1) }
`

// regDeepRuntimes is regNativeRuntimes for regDeepSrc, both runtimes
// allowing frames deeper than the default limit, as Config.MaxCallDepth
// does.
func regDeepRuntimes(t *testing.T, frames int) [2]*Runtime {
	t.Helper()
	regNoInline(t)
	rts := regNativeRuntimes(t, regDeepSrc)
	for _, r := range rts {
		r.maxFrames = frames
	}
	rts[0].jitStress = jitStressConfig{}
	rts[0].SetJITWarmUp(1, 1)
	rts[0].SetJITReport(true)
	return rts
}

// regDeepCall calls name in both runtimes and fails unless they answer
// alike, returning the JIT's answer.
func regDeepCall(t *testing.T, rts [2]*Runtime, name string, args ...Value) string {
	t.Helper()
	var got [2]string
	for i, r := range rts {
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
		v, err := r.Call(fn, Undefined, args)
		got[i] = regNativeShow(r, v, err)
	}
	if got[0] != got[1] {
		t.Fatalf("%s%v: with the JIT %.300q, without %.300q", name, args, got[0], got[1])
	}
	return got[0]
}

// regDeepMarking runs f with the collector idle and then marking: native
// code seeing it mark (jitMarkingForTest), and the collector collecting
// all the while, so that contexts are made, filled and given back while
// it marks.
func regDeepMarking(t *testing.T, f func(t *testing.T)) {
	t.Run("idle", f)
	t.Run("marking", func(t *testing.T) {
		jitMarkingForTest(t)
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					runtime.GC()
				}
			}
		}()
		t.Cleanup(func() { close(done); wg.Wait() })
		f(t)
	})
}

// TestJITRegDeepRecursion runs regDeepSrc's recursions with the JIT's
// register code and without the JIT: a recursion 10,000 deep runs in
// native code at every level once the contexts have grown to its depth,
// in chunks, none of them moved -- no level finished in Go, no run left
// for want of a context -- with the exact sum; one without end throws the
// interpreter's RangeError at the interpreter's depth; an error thrown
// 10,000 deep is caught where the interpreter catches it, its stack the
// interpreter's, as is a stack taken at that depth. The contexts a
// recursion took are given back once two looks find them unused
// (jitShrinkContexts), and made again by the next. The collector idle and
// marking.
func TestJITRegDeepRecursion(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const depth, frames = 10000, 12000
	regDeepMarking(t, func(t *testing.T) {
		rts := regDeepRuntimes(t, frames)
		r := rts[0]
		for range 4 {
			regDeepCall(t, rts, "sum", Float(depth))
		}
		if n := len(r.jit.ssaCtxs); n < depth+1 {
			t.Fatalf("%d contexts for a recursion %d deep", n, depth)
		}
		before := r.RegNativeStats()
		for range 3 {
			if got := regDeepCall(t, rts, "sum", Float(depth)); got != "50005000" {
				t.Fatalf("sum(%d) = %s, want 50005000", depth, got)
			}
		}
		after := r.RegNativeStats()
		t.Logf("%d contexts; sum's runs: %d entries, %d unwound, %d calls of Go", len(r.jit.ssaCtxs),
			after.Entries-before.Entries, after.Unwound-before.Unwound, after.GoCalls-before.GoCalls)
		if n := after.Entries - before.Entries; n > 3*10 {
			t.Errorf("%d entries into native code for 3 sums", n)
		}
		if n := after.Unwound - before.Unwound; n != 0 {
			t.Errorf("Go finished %d levels of sum", n)
		}
		if n := after.GoCalls - before.GoCalls; n != 0 {
			t.Errorf("%d calls of Go in sum's recursion", n)
		}
		for k, n := range after.Ended {
			if strings.HasPrefix(k, "no context") {
				t.Errorf("%d runs ended for want of a context: %s", n, k)
			}
		}
		// The contexts, chunk after chunk, each linked to the next and
		// numbered by its level.
		for i, c := range r.jit.ssaCtxs {
			if c.Level != uint64(i) || i > 0 && c.Prev != unsafe.Pointer(r.jit.ssaCtxs[i-1]) {
				t.Fatalf("context %d: level %d, not linked to the one before", i, c.Level)
			}
			if i+1 < len(r.jit.ssaCtxs) && c.Next != unsafe.Pointer(r.jit.ssaCtxs[i+1]) {
				t.Fatalf("context %d not linked to the next", i)
			}
		}

		// The depth limit's RangeError, at its depth.
		overflow := regDeepCall(t, rts, "overflow")
		t.Logf("overflow: %s; %d contexts", overflow, len(r.jit.ssaCtxs))
		if !strings.HasPrefix(overflow, "RangeError: ") {
			t.Fatalf("overflow() = %q, want a RangeError", overflow)
		}
		if n := len(r.jit.ssaCtxs); n > r.jitContextsMost() {
			t.Errorf("%d contexts, past the %d the frames allow", n, r.jitContextsMost())
		}

		// Thrown at the bottom, caught at the top and halfway.
		for range 3 {
			got := regDeepCall(t, rts, "throws", Float(depth))
			if !strings.HasPrefix(got, "bottom|Error: bottom") || strings.Count(got, "thrower") < 2 {
				t.Fatalf("throws(%d) = %.200q", depth, got)
			}
			if got := regDeepCall(t, rts, "half", Float(depth), Float(depth/2)); got != "0" {
				t.Fatalf("half(%d, %d) = %s, want 0", depth, depth/2, got)
			}
			if got := regDeepCall(t, rts, "stackAt", Float(depth)); strings.Count(got, "stackAt") < 2 {
				t.Fatalf("stackAt(%d) = %.200q", depth, got)
			}
		}

		// Given back: the first look finds them used since the last, the
		// second unused.
		s := r.jit
		shrunk := s.ctxShrunk
		s.jitShrinkContexts()
		s.jitShrinkContexts()
		if n := len(s.ssaCtxs); n != jitContexts || s.ctxShrunk == shrunk {
			t.Fatalf("%d contexts after two looks found them unused, want %d", n, jitContexts)
		}
		if c := s.ssaCtxs[len(s.ssaCtxs)-1]; c.Next != nil {
			t.Fatal("the last context kept links past it")
		}
		if got := regDeepCall(t, rts, "sum", Float(depth)); got != "50005000" {
			t.Fatalf("sum(%d) after the contexts were given back = %s", depth, got)
		}
		if n := len(s.ssaCtxs); n < depth+1 {
			t.Fatalf("%d contexts made again for a recursion %d deep", n, depth)
		}
		// Used since: kept by the next look.
		s.jitShrinkContexts()
		if n := len(s.ssaCtxs); n < depth+1 {
			t.Fatalf("%d contexts kept by a look after a recursion %d deep", n, depth)
		}
	})
}

// TestJITRegShrinkKeepsRunning gives back contexts while native code runs
// below Go: a recursion of native calls calls Go at its bottom, from where
// script looks at the contexts (jitShrinkContexts); those its levels run
// in are kept, as is every one past them a level is in, and the
// recursion's answer is the interpreter's. Those past it that a deeper
// recursion took are given back.
func TestJITRegShrinkKeepsRunning(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
function down(n) { return n == 0 ? look() : down(n - 1) + 1 }
function deep(n) { return n == 0 ? 0 : deep(n - 1) + 1 }
`
	regDeepMarking(t, func(t *testing.T) {
		regNoInline(t)
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(1, 1)
		// look, called by Go from the bottom of down's levels, gives the
		// contexts back, past those down runs in, once it is told to.
		looks, deepest, shrink := 0, 0, false
		for i, rt := range rts {
			hook := rt.NewFunction("look", 0, func(r *Runtime, this Value, args []Value) (Value, error) {
				if i != 0 || !shrink {
					return Float(1), nil
				}
				looks++
				for k, c := range r.jit.ssaCtxs {
					if c.Live != 0 {
						deepest = max(deepest, k)
					}
				}
				r.jit.jitShrinkContexts()
				r.jit.jitShrinkContexts()
				return Float(1), nil
			})
			rt.global.setOwnRaw(rt.atoms.intern("look"), hook, propDefault)
		}
		call := func(name string, n int) string {
			t.Helper()
			var got [2]string
			for i, rt := range rts {
				fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern(name))
				v, err := rt.Call(fn, Undefined, []Value{Float(float64(n))})
				got[i] = regNativeShow(rt, v, err)
			}
			if got[0] != got[1] {
				t.Fatalf("%s(%d): with the JIT %q, without %q", name, n, got[0], got[1])
			}
			return got[0]
		}
		for range 4 {
			call("deep", 3000)
			call("down", 100)
		}
		if n := len(r.jit.ssaCtxs); n < 3000 {
			t.Fatalf("%d contexts for a recursion 3000 deep", n)
		}
		shrink = true
		for range 3 {
			if got := call("down", 100); got != "101" {
				t.Fatalf("down(100) = %q, want 101", got)
			}
		}
		if looks == 0 {
			t.Fatal("look never called")
		}
		if n := len(r.jit.ssaCtxs); n <= deepest || n >= 3000 {
			t.Errorf("%d contexts after the looks, the deepest running %d", n, deepest)
		}
		if got := call("deep", 3000); got != "3000" {
			t.Fatalf("deep(3000) = %q", got)
		}
	})
}

// TestJITRegConsumedOperands has native callees leave native code under
// register code's calls whose states leave out the window registers the
// calls consume (ssa.Value's Consumes): a guard failing at the bottom of a
// recursion, the levels above it finished by Go from their states and
// going on past their calls with what was kept across them (a result of
// an earlier call, a local the call does not consume), and a throw caught
// by a handler a level up, which reads its frame's own registers. Each
// answer is the interpreter's.
func TestJITRegConsumedOperands(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
function leaf(o) { return o.v }
function down(n, o) { if (n == 0) return leaf(o); var k = n * 3; var a = down(n - 1, o); return a + down(0, o) + k }
function guarded(m) { var o = { v: 1 }, s = 0; for (var i = 0; i < m; i++) { if (i == m - 3) o = { w: 0, v: 2.5 }; s += down(20, o) } return s }
function thrower(n, o) { if (n == 0) { if (o.bad) throw new Error("bad " + o.v); return o.v } return thrower(n - 1, o) + n }
function catcher(n, o) { var mine = n * 7; try { return thrower(n, o) + mine } catch (e) { return e.message + " " + mine } }
function throws(m) { var s = ""; for (var i = 0; i < m; i++) s += catcher(15, i % 5 == 4 ? { bad: 1, v: i } : { v: i }) + ","; return s }
`
	regBothMarking(t, func(t *testing.T) {
		regNoInline(t)
		rts := regNativeRuntimes(t, src)
		r := rts[0]
		r.jitStress = jitStressConfig{}
		r.SetJITWarmUp(1, 1)
		for range 3 {
			regDeepCall(t, rts, "guarded", Float(40))
			regDeepCall(t, rts, "throws", Float(30))
		}
		// down(20, o) is 21 o.v + 630: 37 of 651, then 3 of 682.5.
		if got := regDeepCall(t, rts, "guarded", Float(40)); got != "26134.5" {
			t.Fatalf("guarded(40) = %s, want 26134.5", got)
		}
		regDeepCall(t, rts, "throws", Float(30))
		if st := r.RegNativeStats(); st.Unwound == 0 {
			t.Errorf("no callee left native code: %+v", st)
		}
	})
}
