package vm

import (
	"math"
	"testing"
)

// Array(...) and new Array(...), where new.target is Array itself, are a
// native leaf every tier applies without a frame (elemArrayNew), where its
// check passes: any arguments but one number that is not an array length,
// which throws its RangeError the ordinary way, as does every construction
// by another new.target or of a subclass. Node's answers, on the trees and
// in the interpreter, from holes and lengths to elements, prototypes and
// errors; and the check's own answers.
func TestArrayConstructionLeaf(t *testing.T) {
	const src = `'use strict'
		function show(a) { return Array.isArray(a) + ':' + a.length + ':' + Object.keys(a).join('.') + ':' + Array.from(a, (v) => v === undefined ? 'u' : String(v)).join('.') + ':' + (Object.getPrototypeOf(a) === Array.prototype) }
		function err(f) { try { f(); return 'none' } catch (e) { return e.constructor.name } }
		class Sub extends Array { tag() { return 'sub' } }
		function Other() {}
		Other.prototype = { other: true }
		function run() {
			const r = []
			for (let i = 0; i < 3; i++) {
				r.push(show(new Array()), show(new Array(3)), show(new Array(0)), show(new Array(17)), new Array(70000).length)
				r.push(show(new Array('3')), show(new Array(1, 2)), show(new Array(undefined)), show(new Array(null, 'x', 3.5)))
				r.push(show(Array(2)), show(Array('a')), show(Array()), show(Array(4, 5, 6)), show(Array.call({}, 2)))
				r.push(err(() => new Array(-1)), err(() => new Array(1.5)), err(() => new Array(4294967296)), err(() => Array(NaN)), err(() => new Array(Infinity)))
				r.push(new Array(4294967295).length, show(new Array(-0)))
				const s = new Sub(2)
				r.push(s.length, s.tag(), s instanceof Sub, Reflect.construct(Array, [3], Other).other, Reflect.construct(Array, [3], Other).length, Reflect.construct(Array, [2]).length)
				const a = new Array(3); a[1] = 'm'; r.push(show(a), 0 in a, 1 in a)
			}
			return r.join(' ')
		}
		run()`
	const want = "true:0:::true true:3::u.u.u:true true:0:::true true:17::u.u.u.u.u.u.u.u.u.u.u.u.u.u.u.u.u:true 70000 true:1:0:3:true true:2:0.1:1.2:true true:1:0:u:true true:3:0.1.2:null.x.3.5:true true:2::u.u:true true:1:0:a:true true:0:::true true:3:0.1.2:4.5.6:true true:2::u.u:true RangeError RangeError RangeError RangeError RangeError 4294967295 true:0:::true 2 sub true true 3 2 true:3:1:u.m.u:true false true true:0:::true true:3::u.u.u:true true:0:::true true:17::u.u.u.u.u.u.u.u.u.u.u.u.u.u.u.u.u:true 70000 true:1:0:3:true true:2:0.1:1.2:true true:1:0:u:true true:3:0.1.2:null.x.3.5:true true:2::u.u:true true:1:0:a:true true:0:::true true:3:0.1.2:4.5.6:true true:2::u.u:true RangeError RangeError RangeError RangeError RangeError 4294967295 true:0:::true 2 sub true true 3 2 true:3:1:u.m.u:true false true true:0:::true true:3::u.u.u:true true:0:::true true:17::u.u.u.u.u.u.u.u.u.u.u.u.u.u.u.u.u:true 70000 true:1:0:3:true true:2:0.1:1.2:true true:1:0:u:true true:3:0.1.2:null.x.3.5:true true:2::u.u:true true:1:0:a:true true:0:::true true:3:0.1.2:4.5.6:true true:2::u.u:true RangeError RangeError RangeError RangeError RangeError 4294967295 true:0:::true 2 sub true true 3 2 true:3:1:u.m.u:true false true"
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			r.Close()
			treeTier.Store(previous)
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v: got %s\nwant %s", tree, got, want)
		}
		if op := r.proto.arrayCtor.fn().elemOp; op != elemArrayNew {
			t.Errorf("Array's op is %d, want elemArrayNew", op)
		}
		r.Close()
		treeTier.Store(previous)
	}
	check := leafChecks[elemArrayNew]
	for _, tc := range []struct {
		args []Value
		want bool
	}{
		{nil, true},
		{[]Value{Int(0)}, true},
		{[]Value{Int(5)}, true},
		{[]Value{Float(math.MaxUint32)}, true},
		{[]Value{Float(math.MaxUint32 + 1)}, false},
		{[]Value{Float(-1)}, false},
		{[]Value{Float(1.5)}, false},
		{[]Value{Float(math.NaN())}, false},
		{[]Value{Float(math.Inf(1))}, false},
		{[]Value{Undefined}, true},
		{[]Value{Int(1), Int(2)}, true},
		{[]Value{Float(-1), Float(1.5)}, true},
	} {
		if got := check(nil, Undefined, tc.args); got != tc.want {
			t.Errorf("check of %v: %v, want %v", tc.args, got, tc.want)
		}
	}
}
