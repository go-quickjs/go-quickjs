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
	return jitNativeOfForTest(r, "call", "run")
}

// jitNativeOfForTest is how many times r's functions of the names given
// have run natively (JITReport's Native).
func jitNativeOfForTest(r *Runtime, names ...string) uint64 {
	n := uint64(0)
	for _, fn := range r.JITReport() {
		for _, name := range names {
			if strings.HasPrefix(fn.Name, name+" ") {
				n += fn.Native
			}
		}
	}
	return n
}

// A construction that has met more functions than it constructs with
// natively -- a class a loop makes at each turn, each with a prototype of
// its own, which no pool's objects have -- is Go's from native code from
// then on, whatever it constructs with (the VM's jitRegGenericNew), as
// V8's code constructs through its generic Construct builtin, and learns
// nothing more there (jitRegCallUnsettled): each such construction had
// left native code past the site's four targets, 64 times a call of 64
// classes, the exit teaching nothing. A class whose constructor does
// more, a derived one, and one whose instances' method the loop reads
// and calls, as microbench's class_factory and subclass_fresh do. Each
// answer is the interpreter's, with the collector idle and marking.
func TestJITGenericConstruction(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		class Base { constructor(a) { this.a = a } }
		function plain(n) {
			let s = 0
			for (let j = 0; j < n; j++) {
				const C = class { constructor(a) { this.a = a } get() { return this.a } }
				const o = new C(j)
				s += o.get()
			}
			return s
		}
		function derived(n) {
			let s = 0
			for (let j = 0; j < n; j++) {
				const D = class extends Base { constructor(a) { super(a); this.b = 1 } }
				const o = new D(j)
				s += o.a + o.b
			}
			return s
		}`
	for _, marking := range []bool{false, true} {
		for _, fn := range []string{"plain", "derived"} {
			t.Run(fmt.Sprintf("%s/marking=%v", fn, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				holdCollector(t)
				r, run := jitTierRuntimeForTest(t, jitInvocationsForFeedback, setup)
				src := fmt.Sprintf("%s(2000)", fn)
				for range 6 {
					run(src)
				}
				for i := range 4 {
					st, gcs, native := r.RegNativeStats(), jitCollectionsForTest(r), jitNativeOfForTest(r, fn)
					run(src)
					after := r.RegNativeStats()
					if hosts, ended := after.Hosts-st.Hosts, after.Interpreted-st.Interpreted; (hosts != 0 || ended != 0) && gcs == jitCollectionsForTest(r) {
						t.Errorf("call %d of %s: %d exits to Go (%v), %d invocations finished in the interpreter", i, src, hosts, after.HostOps, ended)
					}
					if jitNativeOfForTest(r, fn) == native {
						t.Errorf("call %d of %s did not run natively", i, src)
					}
				}
			})
		}
	}
}
