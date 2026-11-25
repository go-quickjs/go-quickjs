//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A megamorphic site's method read, get_method_w -- o.m() of objects of
// many shapes, the method on their prototype -- whose receiver neither its
// own table nor the megamorphic cache has the method for is read by Go
// from native code at the miss, on a branch of the read's own, the code
// going on after it, as V8's megamorphic loads call the runtime on a miss
// of the stub cache: a shape the site never met, or one whose entry
// another has displaced. The call fills the megamorphic cache as an exit
// did. A getter's method, which the prototypes of half the receivers
// have, Go calls there, and the site's cache is left as the interpreter's
// method read leaves it (cachedProp's, data alone: the getter's entry had
// been taken for the method, its pair called). Before, every such miss
// left native code, the interpreter finishing the invocation, the
// exit teaching nothing the next shape could use: Octane's Typescript, of
// AST nodes of hundreds of shapes, about 330 times a run at its
// typeCheck, emit and isClass reads. Each answer is the interpreter's.
func TestJITMegamorphicMethodMissByGo(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		const own = { m(k) { return this.v + k } }
		const getter = { get m() { return function (k) { return this.v + k + 100 } } }
		let made = 0
		function make(withGetter) {
			const o = Object.create(withGetter && made % 2 ? getter : own)
			o.v = made % 7
			o["q" + made] = made
			made++
			return o
		}
		function batch(n, withGetter) { const out = []; for (let k = 0; k < n; k++) out.push(make(withGetter)); return out }
		const warm = batch(24, false)
		function call(o, k) { return o.m(k) }
		function run(objs) { let t = 0; for (let i = 0; i < objs.length; i++) t += call(objs[i], i & 3); return t }`
	for _, getter := range []bool{false, true} {
		for _, inline := range []bool{true, false} {
			t.Run(fmt.Sprintf("getter=%v/inline=%v", getter, inline), func(t *testing.T) {
				if !inline {
					jitTestSwitch(t, func(jc *jitTestConfig) { jc.noInline = func(fn *bytecode.Function) bool { return fn.Name == "call" } })
				}
				holdCollector(t)
				r, run := jitTierRuntimeForTest(t, jitInvocationsForFeedback, setup)
				for range 4 {
					run(`run(warm)`)
				}
				cl := jitGlobalClosure(r, "call")
				if !jitSiteMegamorphicForTest(t, r, cl, r.jit.regLearnedOf(r.jitRegKeyOf(cl)), bytecode.OpGetPropThis, bytecode.RGetMethodW) {
					t.Fatal("call's method read is not megamorphic: no test of its misses")
				}
				// Receivers of shapes the site never met, in each call: the
				// first read of each misses the megamorphic cache. Two calls
				// to learn and be compiled again in -- batch's write of a
				// new key, and with getters, the callee inlined no more (the
				// VM's jitMegaGetter) and the getters' functions called
				// generically -- then none leaves native code.
				src := fmt.Sprintf("run(batch(64, %v))", getter)
				run(src)
				run(src)
				for i := range 4 {
					st, gcs, native := r.RegNativeStats(), jitCollectionsForTest(r), jitMegaMethodNative(r)
					run(src)
					after := r.RegNativeStats()
					if hosts, ended := after.Hosts-st.Hosts, after.Interpreted-st.Interpreted; (hosts != 0 || ended != 0) && gcs == jitCollectionsForTest(r) {
						t.Errorf("call %d of new shapes: %d exits to Go (%v), %d invocations finished in the interpreter", i, hosts, after.HostOps, ended)
					}
					if jitMegaMethodNative(r) == native {
						// Code judged slow for its exits is entered no more,
						// and leaves no more.
						t.Errorf("call %d of new shapes did not run natively", i)
					}
				}
				if !getter {
					return
				}
				// What the site's cache answers for each receiver is what a
				// method read finds.
				site, name := jitMethodSiteForTest(t, r, cl)
				run(`(globalThis.objs = batch(8, true), run(objs))`)
				objs := r.global.getOwn(r.atoms.intern("objs")).value().Object()
				for i, o := range objs.elemsOf() {
					got, ok := r.cachedProp(&cl.ic[site], o.Object(), name)
					want, err := r.getValueProp(o, name)
					if err != nil {
						t.Fatal(err)
					}
					if ok && !got.sameBits(want) {
						t.Errorf("receiver %d: the cache of call's method read answers other than a read", i)
					}
				}
				run(`run(batch(16, true))`)
			})
		}
	}
}

// jitMegaMethodNative is how many times TestJITMegamorphicMethodMissByGo's
// functions have run natively.
func jitMegaMethodNative(r *Runtime) uint64 {
	n := uint64(0)
	for _, fn := range r.JITReport() {
		if strings.HasPrefix(fn.Name, "call ") || strings.HasPrefix(fn.Name, "run ") {
			n += fn.Native
		}
	}
	return n
}
