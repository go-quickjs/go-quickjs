//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// A read whose cache has filled more often than its entry and its others
// hold -- it has met a fifth shape, as Typescript's reads of a syntax
// tree's nodes do in the interpreter between their compiles, after the
// code learned four at its deoptimizations -- is megamorphic, as V8's
// inline cache is from its fifth map on (jitCacheMegamorphic), not only
// once the cache gives up itself (maxCacheFills): its code is compiled to
// read the receiver's own table and the megamorphic cache, and objects of
// classes it never met leave it native, deoptimizing nothing. A cache of
// four fills is still polymorphic. Each answer is the interpreter's, an
// inherited property's, a getter's, a Proxy's and an absent one's too.
func TestJITMegamorphicPastFourShapes(t *testing.T) {
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			var classes strings.Builder
			for k := range 10 {
				fmt.Fprintf(&classes, "function C%d() { Base.call(this, %d); this.k%d = 0 }\nC%d.prototype = Object.create(Base.prototype)\n", k, k, k, k)
			}
			r, run := jitTierRuntimeForTest(t, 8, "function Base(x) { this.x = x; this.y = 0 }\n"+classes.String()+`
var Cs = [C0, C1, C2, C3, C4, C5, C6, C7, C8, C9];
function make(n) { var os = []; for (var i = 0; i < n; i++) os.push(new Cs[i % 10]()); return os }
function read(os, n) { var v; for (var i = 0; i < n; i++) v = os[i].x; return v }
var one = make(1), all = make(40);
`)
			for range 16 {
				run(`read(one, 1)`)
			}
			v, _ := r.GetProp(Obj(r.Global()), r.Intern("read"))
			cl := v.Object().fn().closure
			var site *propCache
			for i := range cl.ic {
				if c := &cl.ic[i]; c.shape != nil && c.shape != noShape && !c.getter {
					site = c
				}
			}
			if site == nil {
				t.Fatal("read's site has no entry")
			}
			fills := site.fills
			t.Cleanup(func() { site.fills = fills })
			site.fills = 1 + maxPolyEntries
			if regLearnSite(t, r, "read", ir.ReferenceRead).Megamorphic {
				t.Fatal("a cache of four fills is megamorphic")
			}
			site.fills = 2 + maxPolyEntries
			if !regLearnSite(t, r, "read", ir.ReferenceRead).Megamorphic {
				t.Fatal("a cache that met a fifth shape is not megamorphic")
			}
			if !r.jitRegSiteMegamorphic(cl, cl.reg, r.jit.regLearnedOf(r.jitRegKeyOf(cl)), jitReadPCForTest(t, cl)) {
				t.Fatal("the exits do not take the site for megamorphic")
			}
			e := jitNativeForTest(t, r, "read")
			r.jitRegInvalidate(cl, r.jitRegFor(cl, cl.reg), "test")
			for range 16 {
				run(`read(one, 1)`)
			}
			before := e.deopts
			for _, src := range []string{
				`read(all, 40)`,
				`read([{y: 1, x: 2}], 1)`,
				`read([Object.create({x: 3})], 1)`,
				`read([{get x() { return 4 }}], 1)`,
				`read([new Proxy({x: 5}, {})], 1)`,
				`read([{}], 1)`,
			} {
				run(src)
			}
			if e.deopts != before {
				t.Fatalf("the megamorphic read deoptimized %d times", e.deopts-before)
			}
		})
	}
}

// jitReadPCForTest is the register pc of cl's first get_prop.
func jitReadPCForTest(t *testing.T, cl *closure) int {
	t.Helper()
	for pc, w := range cl.reg.rc.Code {
		if int(w.C()) < len(cl.ic) && w.Op().String() == "get_prop" {
			return pc
		}
	}
	t.Fatal("no get_prop")
	return -1
}
