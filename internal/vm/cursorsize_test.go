package vm

import (
	"testing"
	"unsafe"
)

// A for-of's cursor and its object -- every for-of's, spread's and array
// destructuring's -- are 112 bytes, a size class: the object has no ext,
// its class saying where the state is, and what only for-in walks is kept
// apart in its own cursor's allocation, which fits 192 bytes.
func TestCursorSizes(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("sizes of a 64-bit build")
	}
	if size := unsafe.Sizeof(iterObject{}); size != 112 {
		t.Errorf("iterObject is %d bytes, want 112", size)
	}
	if size := unsafe.Sizeof(forInObject{}); size > 192 {
		t.Errorf("forInObject is %d bytes, want at most 192", size)
	}
}

// TestCursorOneAllocation pins that a for-of's cursor is one allocation of
// 112 bytes, and a for-in's one of at most 192, with nothing else made for
// either: a loop over an array and one over an object's keys.
func TestCursorOneAllocation(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("sizes of a 64-bit build")
	}
	r := New(Config{})
	defer r.Close()
	arr := Obj(r.newArrayFrom([]Value{Int(1), Int(2), Int(3)}))
	if n := testing.AllocsPerRun(100, func() {
		cur, err := r.startForOf(arr)
		if err != nil || cursorOf(cur) == nil || cursorOf(cur).arr == nil {
			t.Fatal("no cursor over the array")
		}
	}); n != 1 {
		t.Errorf("a for-of's cursor over an array: %v allocations, want 1", n)
	}
	for _, tt := range []struct{ src, want string }{
		{`var s = 0; for (const x of [1, 2, 3]) s += x; s`, "6"},
		{`var s = ''; for (const k in {a: 1, b: 2}) s += k; s`, "ab"},
		{`var s = ''; for (const k in [5, 6]) s += k; for (const k in 'xy') s += k; s`, "0101"},
		{`var o = {a: 1, b: 2, c: 3}, s = ''; for (const k in o) { delete o.c; s += k } s`, "ab"},
		{`var [a, , b] = new Set([1, 2, 3]); a + b`, "4"},
		{`function* g() { yield 1; yield 2 } [...g(), ...'ab'].join()`, "1,2,a,b"},
	} {
		for _, on := range []bool{false, true} {
			SetTreeTier(on)
			v, err := r.Run(compileForTest(t, tt.src))
			if err != nil {
				t.Fatalf("%s: %v", tt.src, err)
			}
			if got, _ := r.ToString(v); got.Go() != tt.want {
				t.Errorf("tree %v: %s = %s, want %s", on, tt.src, got.Go(), tt.want)
			}
		}
	}
	SetTreeTier(true)
}

// A tree's for-in walks its keys in its loop's step itself
// (ztree_a_gen.go's iterNextTree, a copy of iterNext's walk, so that the
// step is no call deeper than iterNext's): each answer is the
// interpreter's, in both tiers, and Node v26.10.0's -- an object's keys,
// an array's with holes and an index its prototype has, keys deleted and
// added and an array shortened during the walk, a string's indices, an
// inherited key, an array's index and named keys, twenty keys -- but for
// a proxy's, whose has trap the walk calls before each key once the
// target's layout changed (Node asks for its own property descriptor
// instead, and calls no has trap: "a|" and "a"), and whose throw ends it.
func TestTreeForInAsInterpreter(t *testing.T) {
	defer SetTreeTier(true)
	for _, tc := range []struct{ src, want string }{
		{"var o = { a: 1, b: 2, c: 3 }, s = ''; for (var k in o) s += k + o[k]; return s", "a1b2c3"},
		{"var a = [1, , 3, 4], s = ''; for (var k in a) s += k + ':' + a[k] + ','; return s", "0:1,2:3,3:4,"},
		{"Array.prototype[1] = 'p'; var a = [1, , 3], s = ''; for (var k in a) s += k; delete Array.prototype[1]; return s", "021"},
		{"var o = { a: 1, b: 2, c: 3, d: 4 }, s = ''; for (var k in o) { s += k; if (k == 'a') delete o.c } return s", "abd"},
		{"var o = { a: 1, b: 2 }, s = ''; for (var k in o) { s += k; o.z = 1 } return s", "ab"},
		{"var a = [0, 1, 2, 3, 4], s = ''; for (var k in a) { s += k; if (k == '1') a.length = 3 } return s", "012"},
		{"var s = ''; for (var k in 'abc') s += k; return s", "012"},
		{"var p = Object.create({ inh: 1 }); p.own = 2; var s = ''; for (var k in p) s += k; return s", "owninh"},
		{"var log = [], t = { a: 1, b: 2 }; var px = new Proxy(t, { has(o, k) { log.push('has ' + String(k)); return k in o } }); var s = ''; for (var k in px) { s += k; delete t.b } return s + '|' + log.join()", "a|has a,has b"},
		{"var t = { a: 1, b: 2 }; var px = new Proxy(t, { has() { throw new RangeError('h') } }); var s = ''; try { for (var k in px) { s += k; delete t.b } } catch (e) { s += e.name } return s", "RangeError"},
		{"var o = {}; for (var i = 0; i < 20; i++) o['k' + i] = i; var s = 0; for (var k in o) s += o[k]; return s", "190"},
		{"var a = []; a[5] = 5; a[2] = 2; a.x = 'x'; var s = ''; for (var k in a) s += k + ','; return s", "2,5,x,"},
	} {
		for _, on := range []bool{false, true} {
			SetTreeTier(on)
			r := New(Config{})
			v, err := r.Run(compileForTest(t, "(function () { var r; for (var j = 0; j < 3; j++) r = (function () { "+tc.src+" })(); return r })()"))
			if err != nil {
				t.Fatalf("tree %v: %s: %v", on, tc.src, err)
			}
			if got, _ := r.ToString(v); got.Go() != tc.want {
				t.Errorf("tree %v: %s = %q, want %q", on, tc.src, got.Go(), tc.want)
			}
			r.Close()
		}
	}
}

// A tree's loop steps a Map's or a Set's iterator of its realm itself
// (ztree_a_gen.go's iterNextTree, as stepIter's case steps one), its
// generator test made once: each answer is the interpreter's, in both
// tiers, and Node v26.10.0's -- a Map's values, keys and entries, a Set's,
// entries deleted, added and cleared during the walk, the iterators'
// next replaced on their prototype and on one iterator, which Go's step
// calls, and a walk left by break and taken up again.
func TestTreeMapIterAsInterpreter(t *testing.T) {
	defer SetTreeTier(true)
	for _, tc := range []struct{ src, want string }{
		{"var m = new Map([[1, 'a'], [2, 'b'], [3, 'c']]), s = ''; for (var v of m.values()) s += v; return s", "abc"},
		{"var m = new Map([[1, 'a'], [2, 'b']]), s = ''; for (var k of m.keys()) s += k; for (var e of m) s += e[0] + e[1]; for (var e of m.entries()) s += e.join('='); return s", "121a2b1=a2=b"},
		{"var t = new Set([3, 1, 2]), s = ''; for (var v of t) s += v; for (var v of t.values()) s += v; for (var e of t.entries()) s += e.join(); return s", "3123123,31,12,2"},
		{"var m = new Map([[1, 1], [2, 2], [3, 3], [4, 4]]), s = ''; for (var [k] of m) { s += k; if (k == 1) { m.delete(2); m.set(5, 5) } } return s", "1345"},
		{"var m = new Map([[1, 1], [2, 2]]), s = ''; for (var v of m.values()) { s += v; if (v == 1) m.clear() } return s + m.size", "10"},
		{"var m = new Map([[1, 1], [2, 2]]); var it = m.values(); var P = Object.getPrototypeOf(it), orig = P.next; P.next = function () { var r = orig.call(this); if (!r.done) r.value *= 10; return r }; var s = ''; for (var v of it) s += v + ','; P.next = orig; return s", "10,20,"},
		{"var m = new Map([[1, 1], [2, 2]]); var it = m.values(); it.next = function () { return { done: true } }; var s = 'x'; for (var v of it) s += v; return s", "x"},
		{"var t = new Set(); for (var i = 0; i < 50; i++) t.add(i); var s = 0; for (var v of t) { s += v; if (v % 10 == 0) t.delete(v + 1) } return s", "1120"},
		{"var m = new Map([[1, 'a'], [2, 'b'], [3, 'c']]), s = ''; var it = m.values(); for (var v of it) { s += v; break } for (var v of it) s += v; return s", "abc"},
	} {
		for _, on := range []bool{false, true} {
			SetTreeTier(on)
			r := New(Config{})
			v, err := r.Run(compileForTest(t, "(function () { var r; for (var j = 0; j < 3; j++) r = (function () { "+tc.src+" })(); return r })()"))
			if err != nil {
				t.Fatalf("tree %v: %s: %v", on, tc.src, err)
			}
			if got, _ := r.ToString(v); got.Go() != tc.want {
				t.Errorf("tree %v: %s = %q, want %q", on, tc.src, got.Go(), tc.want)
			}
			r.Close()
		}
	}
}
