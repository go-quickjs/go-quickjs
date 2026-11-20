//go:build amd64 || arm64

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// nativeMegaSrc reads and writes properties at sites that meet objects of
// 24 prototypes' layouts and give up their caches (maxCacheFills): rd's
// read of a prototype's x and of the object's own y, wr's addition of y
// and its write of it, answered by the realm's caches of such sites
// (sharedProp's, deepStore's), natively by the handlers (amd64_mega.go).
// Its rounds then change what an answer rests on after the caches have
// it: a setter for y on a prototype, y made read-only on Object.prototype,
// both taken off again, objects closed to new properties, objects given
// other prototypes, a getter for x on a prototype, x written on another
// and deleted from a third. nativeMegaWant is Node's answer
// (vm.runInThisContext).
const nativeMegaDefs = `
var protos = [], out = [], hits = 0;
for (var i = 0; i < 24; i++) { var p = { x: i }; p["k" + i] = i; protos.push(p); }
function rd(o) { return o.x + o.y }
function wr(o, v) { o.y = v }
function round(n, tag, m) {
	var s = 0, last;
	for (var i = 0; i < n; i++) {
		var o = Object.create(protos[i % (m || 24)]);
		if (tag == 4 && i % 5 == 0) Object.preventExtensions(o);
		if (tag == 5 && i % 7 == 0) Object.setPrototypeOf(o, i % 2 ? protos[(i + 1) % 24] : { x: 100 });
		wr(o, i);
		wr(o, i + 1);
		s += rd(o) | 0;
		last = o;
	}
	return s + "/" + Object.keys(last).join();
}
`

const nativeMegaSrc = nativeMegaDefs + `
for (var k = 0; k < 4; k++) out.push(round(240, 0));
Object.defineProperty(protos[3], "y", { set(v) { hits += v }, configurable: true });
out.push(round(240, 1), hits);
Object.defineProperty(Object.prototype, "y", { value: -1, writable: false, configurable: true });
out.push(round(240, 2));
delete Object.prototype.y;
delete protos[3].y;
out.push(round(240, 3));
out.push(round(240, 4), round(240, 5));
Object.defineProperty(protos[6], "x", { get() { return 1000 }, configurable: true });
protos[7].x = 77;
delete protos[8].x;
out.push(round(240, 6));
out.join(" ")
`

// nativeMegaOwnSrc's rd reads a property objects of 24 layouts have
// themselves, nothing a prototype has: the realm's shared cache, which
// only a read of a prototype's fills, is never made, and the native
// handlers answer from the object's own table (megaRead's own).
const nativeMegaOwnSrc = `
var objs = [];
for (var i = 0; i < 24; i++) { var o = {}; o["k" + i] = i; o.v = i; objs.push(o); }
function rd(o) { return o.v }
function round(n) { var s = 0; for (var i = 0; i < n; i++) s += rd(objs[i % 24]); return s }
`

// nativeMegaPolyWarm has wr write y of objects of two layouts that have
// it before its site meets the prototypes' objects: the site keeps the
// entry of the first among its others' (propCache.poly) when it gives up,
// which a write of an object of none of them looks among before the
// realm's caches.
const nativeMegaPolyWarm = `var pa = { y: 0 }, pb = { z: 0, y: 0 }; wr(pa, 1); wr(pb, 2); wr(pa, 3);
`

const nativeMegaWant = "31680/y 31680/y 31680/y 31680/y 30530/y 2230 2500/ 31680/y 25440/y 33275/y 41070/y"

// TestRegNativeMegaProps runs nativeMegaSrc by the Go table and by the
// native handlers, marking and not: Node's answer. And once its sites have
// given up and the realm's caches hold their answers, its reads and writes
// are made with no exit to Go, marking and not, over objects of four of
// the prototypes: the caches are found by the shapes' addresses, and more
// entries than a set holds could share one, which a run of 24 prototypes'
// does now and then -- as nativeMegaSrc's exits, which this test does not
// compare from one run to another.
func TestRegNativeMegaProps(t *testing.T) {
	if got := nativeRunSrc(t, nativeMegaSrc, false); got != nativeMegaWant {
		t.Fatalf("Go table: %s, want %s", got, nativeMegaWant)
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			nativeCounting(t)
			nativeMarking(t, marking)
			if got := nativeRunSrc(t, nativeMegaSrc, true); got != nativeMegaWant {
				t.Errorf("native: %s, want %s", got, nativeMegaWant)
			}
			for _, warm := range []string{"", nativeMegaPolyWarm} {
				src := nativeMegaDefs + warm + "for (var k = 0; k < 4; k++) round(240, 0); round(240, 0, 4)"
				r := newTestRuntime(t, Config{}.withNative(true))
				defer r.Close()
				if _, err := r.Run(compileForTest(t, src)); err != nil {
					t.Fatal(err)
				}
				if warm != "" {
					w, _ := r.GetProp(Obj(r.Global()), r.Intern("wr"))
					if ic := w.Object().fn().closure.ic; ic[1].fills < maxCacheFills || ic[1].poly == nil || ic[1].poly.n == 0 {
						t.Fatalf("wr's site: %d fills, entries of other shapes %v; want it given up with some", ic[1].fills, ic[1].poly)
					}
				}
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("round"))
				before := nativeExitOps(t)
				for range 4 {
					v, err := r.Call(fn, Undefined, []Value{Float(240), Float(0), Float(4)})
					if err != nil {
						t.Fatal(err)
					}
					if got := v.String().Go(); got != "29280/y" {
						t.Fatalf("warm %q: round(240, 0, 4) = %s, want 29280/y (Node's)", warm, got)
					}
				}
				after := nativeExitOps(t)
				for _, op := range []bytecode.ROp{bytecode.RGetProp, bytecode.RSetProp} {
					if n := after[op] - before[op]; n != 0 {
						t.Errorf("warm %q: %v exited to Go %d times in 960 turns once its caches were filled", warm, op, n)
					}
				}
			}
			// Own reads alone, no shared cache made.
			o := newTestRuntime(t, Config{}.withNative(true))
			defer o.Close()
			if _, err := o.Run(compileForTest(t, nativeMegaOwnSrc+"for (var k = 0; k < 4; k++) round(240)")); err != nil {
				t.Fatal(err)
			}
			fn, _ := o.GetProp(Obj(o.Global()), o.Intern("round"))
			before := nativeExitOps(t)
			v, err := o.Call(fn, Undefined, []Value{Float(240)})
			if err != nil || !v.IsNumber() || v.num != 2760 {
				t.Errorf("own reads: %v, %v; want 2760", v, err)
			}
			if n := nativeExitOps(t)[bytecode.RGetProp] - before[bytecode.RGetProp]; n != 0 || o.shared != nil {
				t.Errorf("own reads exited to Go %d times in 240, shared cache made %v", n, o.shared != nil)
			}
		})
	}
}
