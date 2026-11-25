//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A method read whose site has met objects -- of many shapes, so its
// cases are past what a cache holds, or along a chain deeper than a cache
// holds -- and primitives, of one kind or several, in either order, has
// each primitive's kind as a case beside the objects', as V8's load ICs
// keep the HeapNumber map or the String map among their maps: the code
// compiled again reads the method off the primitive's prototype natively,
// and the objects' as before; and the call of the primitive's method, a
// built-in no native call calls, is Go's from native code (ssa.CallSite's
// Generic). The read had learned a number once and the code had never been
// compiled with it: every number's read left native code, 1,737 times in
// the first call of 5,000 numbers and 128 in every other call after it,
// the interpreter finishing each invocation; with the read made natively,
// the call left at each number, 64 times a call, the exit teaching
// nothing. Each answer is the interpreter's. The collector is held, and
// a call beside which the collector ran -- another test forced it, or the
// process's memory limit (heavyMemoryLimit) -- is not counted: a
// collection empties caches the code reads, which a few reads then learn
// again.
func TestJITPrimitiveCaseAtObjectSite(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		class A { toString() { return "a" } }
		class B extends A {}
		class C extends B {}
		class D extends C {}
		function objects(Ctor) {
			const out = []
			for (let k = 0; k < 12; k++) { const o = new Ctor; o["p" + k] = k; out.push(o) }
			return out
		}
		const shallow = objects(A), deep = objects(D)
		const strs = ["x", "yy", "zzz"]
		function f(x) { return x.toString() }
		function run(kind, objs) {
			let n = 0
			for (let i = 0; i < 5000; i++) {
				let x
				switch (kind === 4 ? i % 4 : kind) {
				case 0: x = objs[i % 12]; break
				case 1: x = i; break
				case 2: x = strs[i % 3]; break
				case 3: x = i % 2 === 0; break
				}
				n += f(x).length
			}
			return n
		}`
	// first's receivers, then then's: 0 objects, 1 numbers, 2 strings, 3
	// booleans, 4 all four in turn.
	for _, tc := range []struct{ first, then int }{{0, 1}, {0, 2}, {0, 3}, {1, 0}, {2, 0}, {0, 4}} {
		for _, deep := range []bool{false, true} {
			for _, inline := range []bool{true, false} {
				t.Run(fmt.Sprintf("%d-then-%d/deep=%v/inline=%v", tc.first, tc.then, deep, inline), func(t *testing.T) {
					if !inline {
						jitTestSwitch(t, func(jc *jitTestConfig) { jc.noInline = func(fn *bytecode.Function) bool { return fn.Name == "f" } })
					}
					holdCollector(t)
					r, run := jitTierRuntimeForTest(t, jitInvocationsForFeedback, setup)
					objs := "shallow"
					if deep {
						objs = "deep"
					}
					for range 3 {
						run(fmt.Sprintf("run(%d, %s)", tc.first, objs))
					}
					src := fmt.Sprintf("run(%d, %s)", tc.then, objs)
					// Two calls to learn and be compiled again in, then none
					// leaves native code.
					run(src)
					run(src)
					for i := range 4 {
						st, gcs := r.RegNativeStats(), jitCollectionsForTest(r)
						run(src)
						after := r.RegNativeStats()
						if hosts, ended := after.Hosts-st.Hosts, after.Interpreted-st.Interpreted; (hosts != 0 || ended != 0) && gcs == jitCollectionsForTest(r) {
							t.Errorf("warm call %d of %s: %d exits to Go (%v), %d invocations finished in the interpreter", i, src, hosts, after.HostOps, ended)
						}
					}
					native := uint64(0)
					for _, fn := range r.JITReport() {
						if strings.HasPrefix(fn.Name, "f ") || strings.HasPrefix(fn.Name, "run ") {
							native += fn.Native
						}
					}
					if native == 0 {
						t.Errorf("%s never ran natively", src)
					}
				})
			}
		}
	}
}

// A primitive's method replaced, or deleted, at a read whose code reads
// it off its prototype beside the objects' cases, is the one the code
// calls: the prototype's shape it checks changed. Each answer is the
// interpreter's.
func TestJITPrimitiveCaseReplaced(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		const objs = []
		for (let k = 0; k < 12; k++) { const o = {toString() { return "o" }}; o["p" + k] = k; objs.push(o) }
		function str(x) { return x.toString() }
		function run(prim) {
			let n = 0
			for (let i = 0; i < 5000; i++) n += str(prim ? i : objs[i % 12]).length
			return n
		}`
	holdCollector(t)
	r, run := jitTierRuntimeForTest(t, jitInvocationsForFeedback, setup)
	for range 3 {
		run(`run(false)`)
	}
	run(`run(true)`)
	run(`run(true)`)
	st, gcs := r.RegNativeStats(), jitCollectionsForTest(r)
	run(`run(true)`)
	if after := r.RegNativeStats(); after.Hosts != st.Hosts && gcs == jitCollectionsForTest(r) {
		t.Errorf("warm, run(true) left native code %d times (%v)", after.Hosts-st.Hosts, after.HostOps)
	}
	run(`Number.prototype.toString = function () { return "nn" }, run(true)`)
	run(`run(true) + run(false)`)
	run(`delete Number.prototype.toString, run(true)`)
}

// jitCollectionsForTest is how many collections the process has made, and
// the one r's megamorphic cache was last emptied at (jitState.megaGC): the
// cache is emptied at r's next look (flush) after a collection ends, which
// for one that ended between two calls is in the second, where the
// process's count does not change.
func jitCollectionsForTest(r *Runtime) [2]uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return [2]uint64{uint64(m.NumGC), r.jit.megaGC}
}
