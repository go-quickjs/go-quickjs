//go:build amd64 || arm64

package vm

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// nativeAbsentSrc reads a key that objects of some of a site's shapes
// have and others have not (hasKey: an array, two objects with it, one
// without), a key no object of 24 prototypes' layouts has at a site that
// gives up (ref), and functions' prototypes (proto); edits then puts ref
// on a prototype and on Object.prototype and takes it off, and replaces
// a function's prototype, writably and not. nativeAbsentWant is Node's
// answer (vm.runInThisContext).
const nativeAbsentSrc = `
var aprotos = [];
for (var i = 0; i < 24; i++) { var p = { x: i }; p["k" + i] = i; aprotos.push(p); }
function hasKey(o) { return o.key !== undefined ? o.key : -1 }
function ref(o) { return o.ref === undefined ? 1 : 2 }
function proto(f) { return f.prototype.v }
var fns = [];
for (var i = 0; i < 8; i++) { var F = function () {}; F.prototype.v = i; fns.push(F); }
function els(n, m) {
	var s = 0, a = [[1], { key: 3 }, { key: 4, b: 1 }, { c: 1 }];
	for (var i = 0; i < n; i++) {
		s += hasKey(a[i % 4]);
		s += ref(Object.create(aprotos[i % (m || 24)]));
		s += proto(fns[i % 8]);
	}
	return s;
}
function edits() {
	var out = [], o = Object.create(aprotos[0]), F = function () {};
	for (var i = 0; i < 30; i++) ref(Object.create(aprotos[i % 24]));
	out.push(ref(o));
	aprotos[0].ref = 5;
	out.push(ref(o), ref(Object.create(aprotos[0])));
	delete aprotos[0].ref;
	Object.prototype.ref = 7;
	out.push(ref(o), ref(Object.create(aprotos[3])));
	delete Object.prototype.ref;
	out.push(ref(o));
	out.push(proto(F) === undefined, typeof F.prototype);
	F.prototype = { v: 9 };
	out.push(proto(F));
	Object.defineProperty(F, "prototype", { value: { v: 10 }, writable: false });
	out.push(proto(F));
	class C { }
	C.prototype.v = 11;
	out.push(proto(C), proto(function () {}) === undefined);
	return out.join();
}
`

const nativeAbsentWant = "1380 1380 1380 1380 1380 1,2,2,2,2,1,true,object,9,10,11,true"

// TestRegNativeAbsentReads runs nativeAbsentSrc by the Go table and by the
// native handlers, marking and not: Node's answer. And once warm, els's
// reads make no exit to Go, marking and not: the absent key's entry kept
// beside the others' at hasKey's site, the realm's shared cache's entry
// for ref's by the chain's cell, proto's of the prototype the functions
// have made (fillChainCache, sharedProp, ownProto).
func TestRegNativeAbsentReads(t *testing.T) {
	const run = "var out = []; for (var k = 0; k < 4; k++) out.push(els(240)); out.push(els(240, 4), edits()); out.join(' ')"
	if got := nativeRunSrc(t, nativeAbsentSrc+run, false); got != nativeAbsentWant {
		t.Fatalf("Go table: %s, want %s", got, nativeAbsentWant)
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			nativeCounting(t)
			nativeMarking(t, marking)
			if got := nativeRunSrc(t, nativeAbsentSrc+run, true); got != nativeAbsentWant {
				t.Fatalf("native: %s, want %s", got, nativeAbsentWant)
			}
			r := newTestRuntime(t, Config{}.withNative(true))
			defer r.Close()
			if _, err := r.Run(compileForTest(t, nativeAbsentSrc+"for (var k = 0; k < 4; k++) els(240); els(240, 4)")); err != nil {
				t.Fatal(err)
			}
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("els"))
			before := nativeExitOps(t)
			for range 4 {
				v, err := r.Call(fn, Undefined, []Value{Float(240), Float(4)})
				if err != nil {
					t.Fatal(err)
				}
				if !v.IsNumber() || v.num != 1380 {
					t.Fatalf("els(240, 4) = %v, want 1380 (Node's)", v)
				}
			}
			if n := nativeExitOps(t)[bytecode.RGetProp] - before[bytecode.RGetProp]; n != 0 {
				t.Errorf("get_prop exited to Go %d times in 960 turns once warm", n)
			}
		})
	}
}
