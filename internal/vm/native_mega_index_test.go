//go:build amd64 || arm64

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// nativeMegaIndexDefs reads and writes the own properties of objects of
// 24 prototypes' layouts at sites that give up their caches, objects of
// twenty properties fill adds, whose tables have an index (more than
// linearScanLimit): rd's twenty reads, more keys than the index's recent
// entries, so that some share one and are found in its slots, and a
// prototype's x; wr's two writes and an addition. round's tags then make
// the objects otherwise: of six properties read and then grown past the
// limit, three deleted, a getter and setter for b on some, a read-only a on
// others, twelve with six deleted. nativeMegaIndexWant is Node's answer
// (vm.runInThisContext).
const nativeMegaIndexDefs = `
var protos = [], hits = 0;
for (var i = 0; i < 24; i++) { var p = { x: i }; p["k" + i] = i; protos.push(p); }
function fill(o, n, i) {
	if (n > 0) o.a = i + 0;
	if (n > 1) o.b = i + 1;
	if (n > 2) o.c = i + 2;
	if (n > 3) o.d = i + 3;
	if (n > 4) o.e = i + 4;
	if (n > 5) o.f = i + 5;
	if (n > 6) o.g = i + 6;
	if (n > 7) o.h = i + 7;
	if (n > 8) o.j = i + 8;
	if (n > 9) o.l = i + 9;
	if (n > 10) o.m = i + 10;
	if (n > 11) o.n = i + 11;
	if (n > 12) o.q = i + 12;
	if (n > 13) o.r = i + 13;
	if (n > 14) o.s = i + 14;
	if (n > 15) o.t = i + 15;
	if (n > 16) o.u = i + 16;
	if (n > 17) o.v = i + 17;
	if (n > 18) o.z = i + 18;
	if (n > 19) o.y = i + 19;
	return o
}
function make(i, n) { return fill(Object.create(protos[i % 24]), n, i) }
function rd(o) {
	return (o.a | 0) + (o.b | 0) + (o.c | 0) + (o.d | 0) + (o.e | 0) + (o.f | 0) + (o.g | 0) + (o.h | 0) + (o.j | 0) + (o.l | 0) +
		(o.m | 0) + (o.n | 0) + (o.q | 0) + (o.r | 0) + (o.s | 0) + (o.t | 0) + (o.u | 0) + (o.v | 0) + (o.z | 0) + (o.y | 0) + o.x
}
function wr(o, v) { o.a = v; o.t = v + 1; o.w = v + 2 }
function round(n, tag, m) {
	var s = 0, o;
	for (var i = 0; i < n; i++) {
		var k = i % (m || 24);
		switch (tag) {
		case 0: o = make(k, 20); break;
		case 1: o = make(k, 6); s += rd(o); o.h = 7; o.j = 8; o.l = 9; o.m = 10; o.n = 11; o.q = 12; break;
		case 2: o = make(k, 20); delete o.c; delete o.t; if (i % 2) delete o.a; break;
		case 3: o = make(k, 20); if (i % 5 == 0) Object.defineProperty(o, "b", { get() { hits++; return 1000 }, set(v) { hits += v }, configurable: true }); break;
		case 4: o = make(k, 20); if (i % 3 == 0) Object.defineProperty(o, "a", { value: -5, writable: false }); break;
		case 5: o = make(k, 12); delete o.a; delete o.b; delete o.c; delete o.d; delete o.e; delete o.f; break;
		}
		wr(o, i);
		if (tag == 3) o.b = 3;
		s += rd(o) + o.w;
	}
	return s + "/" + Object.keys(o).length;
}
`

const nativeMegaIndexSrc = nativeMegaIndexDefs + `
var out = []; for (var k = 0; k < 4; k++) out.push(round(240, 0)); for (var t = 1; t <= 5; t++) out.push(round(240, t)); out.push(hits); out.join(' ')`

// nativeMegaIndexUse's use writes, adds and reads at wr's and rd's sites
// the properties of objects objs makes of four of the prototypes, apart
// from it.
const nativeMegaIndexUse = `
function objs(n) { var a = []; for (var i = 0; i < n; i++) a.push(make(i % 4, 20)); return a }
function use(a) { var s = 0; for (var i = 0; i < a.length; i++) { wr(a[i], i); s += rd(a[i]) + a[i].w } return s }
`

const nativeMegaIndexUseWant = 135600

const nativeMegaIndexWant = "181200/21 181200/21 181200/21 181200/21 143520/14 177960/20 226776/21 171320/21 118320/9 192"

// TestRegNativeMegaIndexed runs nativeMegaIndexSrc by the Go table and by
// the native handlers, marking and not: Node's answer. And once rd's and
// wr's sites have given up and the realm's caches hold what they need,
// use's reads and writes of own properties of tables with an index, and
// its additions to them, are made with no exit to Go, marking and not,
// over objects of four of the prototypes: found as findOwn finds them, by
// the index's recent entries and its slots (nativegen's indexFind).
func TestRegNativeMegaIndexed(t *testing.T) {
	if got := nativeRunSrc(t, nativeMegaIndexSrc, false); got != nativeMegaIndexWant {
		t.Fatalf("Go table: %s, want %s", got, nativeMegaIndexWant)
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			nativeCounting(t)
			nativeMarking(t, marking)
			if got := nativeRunSrc(t, nativeMegaIndexSrc, true); got != nativeMegaIndexWant {
				t.Errorf("native: %s, want %s", got, nativeMegaIndexWant)
			}
			r := newTestRuntime(t, Config{}.withNative(true))
			defer r.Close()
			if _, err := r.Run(compileForTest(t, nativeMegaIndexDefs+nativeMegaIndexUse+"for (var k = 0; k < 4; k++) round(240, 0); use(objs(240))")); err != nil {
				t.Fatal(err)
			}
			mk, _ := r.GetProp(Obj(r.Global()), r.Intern("objs"))
			use, _ := r.GetProp(Obj(r.Global()), r.Intern("use"))
			// The objects are made apart from use, and all before it: their
			// additions past the limit build their layouts' indexes, which Go
			// makes, and fill the realm's cache of additions, whose sets of
			// four entries the shapes' addresses pick -- made between two of
			// use's calls, they could take the set of one of its additions.
			var batches [5]Value
			for i := range batches {
				os, err := r.Call(mk, Undefined, []Value{Float(240)})
				if err != nil {
					t.Fatal(err)
				}
				batches[i] = os
			}
			var exits [256]int64
			for k, os := range batches {
				before := nativeExitOps(t)
				v, err := r.Call(use, Undefined, []Value{os})
				if err != nil {
					t.Fatal(err)
				}
				after := nativeExitOps(t)
				if k != 0 {
					// The first gives the cache use's additions.
					for i := range exits {
						exits[i] += after[i] - before[i]
					}
				}
				if !v.IsNumber() || v.num != nativeMegaIndexUseWant {
					t.Fatalf("use = %v, want %v", v, nativeMegaIndexUseWant)
				}
			}
			for _, op := range []bytecode.ROp{bytecode.RGetProp, bytecode.RSetProp} {
				if n := exits[op]; n != 0 {
					t.Errorf("%v exited to Go %d times in 960 turns once its caches were filled", op, n)
				}
			}
		})
	}
}

// nativeMegaCallsDefs calls a prototype's method and an own one, and reads
// two properties of an object an object holds, at sites that meet objects
// of 24 prototypes' layouts and give up: get_method_w's reads and
// get_prop_next's pair, answered natively as get_prop's are (megaRead,
// by megaReadSlot), the objects' tables of more than linearScanLimit
// properties found by their indexes. nativeMegaCallsWant is Node's answer
// (vm.runInThisContext).
const nativeMegaCallsDefs = nativeMegaIndexDefs + `
var cprotos = [];
for (var i = 0; i < 24; i++) { var p = { x: i, mth: function () { return this.y } }; p["k" + i] = i; cprotos.push(p); }
function ownm() { return this.a + 1 }
function cmake(i) {
	var o = fill(Object.create(cprotos[i % 24]), 12, i);
	o.y = i; o.own = ownm;
	var inner = { v: i };
	inner["w" + (i % 24)] = i;
	inner.w = 2;
	o.inner = inner;
	return o
}
function call(o) { return o.mth() + o.own() }
function chain(o) { return o.inner.v + o.inner.w }
function cround(n, m) { var s = 0; for (var i = 0; i < n; i++) { var o = cmake(i % (m || 24)); s += call(o) + chain(o) } return s }
function cobjs(n) { var a = []; for (var i = 0; i < n; i++) a.push(cmake(i % 4)); return a }
function cuse(a) { var s = 0; for (var i = 0; i < a.length; i++) s += call(a[i]) + chain(a[i]); return s }
`

const nativeMegaCallsWant = "9000 9000 9000 9000 1800"

// TestRegNativeMegaCalls runs nativeMegaCallsDefs's rounds by the Go table
// and by the native handlers, marking and not: Node's answer. And once
// call's and chain's sites have given up and the realm's shared cache
// holds the prototypes' methods, cuse's method reads and paired reads over
// objects of four of the prototypes make no exit to Go, marking and not.
func TestRegNativeMegaCalls(t *testing.T) {
	const run = "var out = []; for (var k = 0; k < 4; k++) out.push(cround(240)); out.push(cround(240, 4)); out.join(' ')"
	if got := nativeRunSrc(t, nativeMegaCallsDefs+run, false); got != nativeMegaCallsWant {
		t.Fatalf("Go table: %s, want %s", got, nativeMegaCallsWant)
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			nativeCounting(t)
			nativeMarking(t, marking)
			r := newTestRuntime(t, Config{}.withNative(true))
			defer r.Close()
			if v, err := r.Run(compileForTest(t, nativeMegaCallsDefs+run)); err != nil || v.String().Go() != nativeMegaCallsWant {
				t.Fatalf("native: %v, %v; want %s", v, err, nativeMegaCallsWant)
			}
			mk, _ := r.GetProp(Obj(r.Global()), r.Intern("cobjs"))
			use, _ := r.GetProp(Obj(r.Global()), r.Intern("cuse"))
			var batches [5]Value
			for i := range batches {
				os, err := r.Call(mk, Undefined, []Value{Float(240)})
				if err != nil {
					t.Fatal(err)
				}
				batches[i] = os
			}
			var exits [256]int64
			for k, os := range batches {
				before := nativeExitOps(t)
				v, err := r.Call(use, Undefined, []Value{os})
				if err != nil {
					t.Fatal(err)
				}
				if !v.IsNumber() || v.num != 1800 {
					t.Fatalf("cuse = %v, want 1800 (Node's)", v)
				}
				if k == 0 {
					// The first warms the shared cache for cobjs' layouts.
					continue
				}
				after := nativeExitOps(t)
				for i := range exits {
					exits[i] += after[i] - before[i]
				}
			}
			for _, op := range []bytecode.ROp{bytecode.RGetMethodW, bytecode.RGetPropNext, bytecode.RGetProp} {
				if n := exits[op]; n != 0 {
					t.Errorf("%v exited to Go %d times in 960 turns once its caches were filled", op, n)
				}
			}
		})
	}
}
