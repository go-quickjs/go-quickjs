//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// megaAddSrc's variant makes objects with Object.create of a prototype made
// at each call, and adds y to each: the write's site meets a layout more at
// every call, and is megamorphic once warm, its additions native code's by
// the megamorphic cache's entries (jitMegaFillAdd, mir's megaAdd), the
// calls of Object.create its leaf's. Its modes change what an addition may
// meet halfway through a call: a setter for y on the prototype, y made
// read-only there, a setter on Object.prototype, y a data property of the
// prototype; objects closed to new properties, given other prototypes, one
// with a setter; a prototype made at each turn; and enough properties
// added that the objects outgrow their room. megaAddWant is Node's answer
// for each mode (vm.runInThisContext).
const megaAddSrc = `
function variant(n, mode) {
	var proto = { x: 1 }, s = 0, hits = 0, o, keys = "";
	for (var j = 0; j < n; j++) {
		if (j == n >> 1) {
			if (mode == 1) Object.defineProperty(proto, "y", { set(v) { hits += v }, configurable: true });
			if (mode == 2) Object.defineProperty(proto, "y", { value: -1, writable: false });
			if (mode == 3) Object.defineProperty(Object.prototype, "y", { set(v) { hits += v }, configurable: true });
			if (mode == 4) proto.y = 5;
		}
		o = Object.create(mode == 7 ? { x: j } : proto);
		if (mode == 5 && j % 7 == 0) Object.preventExtensions(o);
		if (mode == 6 && j % 5 == 0) Object.setPrototypeOf(o, j % 10 ? { x: 2 } : { x: 3, set y(v) { hits++ } });
		o.y = j;
		if (mode == 8) { o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6; o.g = 7; o.h = 8; o.i = 9; o.k = j; s += o.k + o.a + o.i }
		s += o.x + (o.y === undefined ? 100 : o.y);
		if (j == n - 1) keys = Object.keys(o).join();
	}
	if (mode == 3) delete Object.prototype.y;
	return s + ":" + hits + ":" + keys;
}
`

var megaAddWant = [...]string{"80200:0:y", "40300:59900:", "20100:0:", "40300:59900:", "80200:0:y", "74429:0:", "76520:40:y", "159600:0:y", "164000:0:y,a,b,c,d,e,f,g,h,i,k"}

// TestJITMegaAdd runs megaAddSrc's variant with the JIT and without, the
// collector idle and as while it marks: Node's answers, the loop run by
// native code once warm -- never left as slow -- and its additions made
// there, Go writing only the first of each of a call's two layouts, which
// fills the megamorphic cache's entry: the first object made on the call's
// prototype keeps it in its table (polyproto.go), its entry's chain that
// prototype, and the others are of the prototype's root.
func TestJITMegaAdd(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			rts := regNativeRuntimes(t, megaAddSrc)
			rts[0].SetJITReport(true)
			call := func(r *Runtime, mode int) string {
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("variant"))
				v, err := r.Call(fn, Undefined, []Value{Float(400), Float(float64(mode))})
				return regNativeShow(r, v, err)
			}
			for i, r := range rts {
				for k := range 20 {
					if got := call(r, 0); got != megaAddWant[0] {
						t.Fatalf("runtime %d, call %d: %s, want %s", i, k, got, megaAddWant[0])
					}
				}
			}
			// A collection's end empties the megamorphic cache (jitState's
			// flush), whose entries the next additions are Go's to fill
			// again: the calls counted are ones no collection ends in, the
			// collector held off (jitCollectorHeldForTest) and the cache
			// filled again by a call first. A checkptr build, slower, had
			// a collection end in them.
			jitCollectorHeldForTest(t)
			call(rts[0], 0)
			before := rts[0].RegNativeStats()
			const calls = 10
			for range calls {
				call(rts[0], 0)
			}
			after := rts[0].RegNativeStats()
			if after.Entries == before.Entries {
				t.Error("variant not run natively once warm")
			}
			if n := after.PlainWrites - before.PlainWrites; n > 2*calls {
				t.Errorf("%d additions written by Go in %d calls; want two a call, the first of each layout", n, calls)
			}
			for k, n := range after.Ended {
				if strings.HasSuffix(k, " in variant") && n != before.Ended[k] {
					t.Errorf("variant ended in the interpreter %d times once warm: %s", n-before.Ended[k], k)
				}
			}
			for mode := 1; mode < len(megaAddWant); mode++ {
				for i, r := range rts {
					for k := range 3 {
						if got := call(r, mode); got != megaAddWant[mode] {
							t.Errorf("runtime %d, mode %d, call %d: %s, want %s", i, mode, k, got, megaAddWant[mode])
						}
					}
				}
			}
		})
	}
}
