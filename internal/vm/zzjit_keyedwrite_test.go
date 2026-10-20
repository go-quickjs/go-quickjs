//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// keyedWriteSetup's functions write objects' properties by computed keys,
// as microbench's prop_create does, obj[i] = i: indices and names, new and
// existing, on objects made by literals and by classes, with no
// prototype, and then what Go must store after an exit: a setter or a
// read-only property up the chain, __proto__, an object that may not be
// extended, in sloppy code and in strict, an accessor of its own, a
// typed array, and a site that also meets arrays.
const keyedWriteSetup = `
	function fill(n) { let o; for (let j = 0; j < n; j++) { o = {}; o.a = 1; o.b = 2; o.c = 3; for (let i = 0; i < 10; i++) o[i] = i * j } return o }
	function show(o) { return JSON.stringify(o) + ':' + Object.keys(o).join(',') }
	function filled(n) { return show(fill(n)) }
	function keys(o, ks, v) { for (let i = 0; i < ks.length; i++) o[ks[i]] = v + i; return o }
	var sym = Symbol('s')
	var mixed = ['x', 5, '7', 1.5, -1, 4294967295, 4294967294, sym, 'x', '05', -0]
	function named(n) { let o; for (let j = 0; j < n; j++) o = keys({ x: 0 }, mixed, j); return show(o) + ':' + o[sym] + ':' + o[4294967294] + ':' + o['-0'] + ':' + o[0] }
	class Point { constructor() { this.x = 1 } }
	function classes(n) { let p; for (let j = 0; j < n; j++) p = keys(new Point(), ['y', 0, 'x'], j); return show(p) + ':' + (p instanceof Point) }
	function bare(n) { let o; for (let j = 0; j < n; j++) o = keys(Object.create(null), ['a', 1, 'toString'], j); return Object.keys(o).join(',') + ':' + o.toString + ':' + Object.getPrototypeOf(o) }
	var seen = []
	var withSetter = { set k(v) { seen.push(v) } }
	function setters(n) { seen = []; let o; for (let j = 0; j < n; j++) o = keys(Object.create(withSetter), ['k', 'm'], j); return show(o) + ':' + seen.length + ':' + seen[seen.length - 1] }
	var readOnly = Object.defineProperty({}, 'r', { value: 'ro', writable: false })
	function readOnlys(n) { let o; for (let j = 0; j < n; j++) o = keys(Object.create(readOnly), ['r', 's'], j); return show(o) + ':' + o.r }
	function strictReadOnly() { 'use strict'; try { const o = Object.create(readOnly); o['r'] = 1; return 'no' } catch (e) { return e.constructor.name } }
	function protos(n) { let o; for (let j = 0; j < n; j++) o = keys({}, ['__proto__', 'p'], j); return show(o) + ':' + Object.getPrototypeOf(o) + ':' + o.__proto__ }
	function sealed(n) { let o; for (let j = 0; j < n; j++) o = keys(Object.preventExtensions({ a: 1 }), ['a', 'b', 0], j); return show(o) }
	function strictSealed() { 'use strict'; try { const o = Object.preventExtensions({}); o[0] = 1; return 'no' } catch (e) { return e.constructor.name } }
	function frozen(n) { let o; for (let j = 0; j < n; j++) o = keys(Object.freeze({ a: 1, 0: 2 }), ['a', 0], j); return show(o) }
	function accessors(n) { let got = 0; const o = { get g() { return got }, set g(v) { got = v * 3 } }; for (let j = 0; j < n; j++) keys(o, ['g'], j); return got + ':' + show(o) }
	function typed(n) { let t; for (let j = 0; j < n; j++) t = keys(new Int8Array(3), [0, 1, 5, 'x'], j); return Array.from(t).join(',') + ':' + t.x + ':' + t[5] }
	function both(n) { let a, o; for (let j = 0; j < n; j++) { a = keys([], [0, 1, 2], j); o = keys({}, [0, 1, 2], j) } return show(a) + ':' + show(o) + ':' + Array.isArray(a) }
	function holes(n) { let o; for (let j = 0; j < n; j++) { o = { 0: 'a', 1: 'b', 2: 'c' }; delete o[1]; keys(o, [1, 3], j) } return show(o) }
`

// keyedWriteRounds are keyedWriteSetup's calls and their answers, Node's
// (v26.10.0).
var keyedWriteRounds = [][2]string{
	{`filled(30)`, `{"0":0,"1":29,"2":58,"3":87,"4":116,"5":145,"6":174,"7":203,"8":232,"9":261,"a":1,"b":2,"c":3}:0,1,2,3,4,5,6,7,8,9,a,b,c`},
	{`named(30)`, `{"0":39,"5":30,"7":31,"4294967294":35,"x":37,"1.5":32,"-1":33,"4294967295":34,"05":38}:0,5,7,4294967294,x,1.5,-1,4294967295,05:36:35:undefined:39`},
	{`classes(30)`, `{"0":30,"x":31,"y":29}:0,x,y:true`},
	{`bare(30)`, `1,a,toString:31:null`},
	{`setters(30)`, `{"m":30}:m:30:29`},
	{`readOnlys(30)`, `{"s":30}:s:ro`},
	{`strictReadOnly()`, `TypeError`},
	{`protos(30)`, `{"p":30}:p:[object Object]:[object Object]`},
	{`sealed(30)`, `{"a":29}:a`},
	{`strictSealed()`, `TypeError`},
	{`frozen(30)`, `{"0":2,"a":1}:0,a`},
	{`accessors(30)`, `87:{"g":87}:g`},
	{`typed(30)`, `29,30,0:32:undefined`},
	{`both(30)`, `[29,30,31]:0,1,2:{"0":29,"1":30,"2":31}:0,1,2:true`},
	{`holes(30)`, `{"0":"a","1":29,"2":"c","3":30}:0,1,2,3`},
}

// An element's write to an object native code does not store to -- an
// ordinary object's property by an index or a name -- is made by Go from
// native code (ssa.GoKeyedWrite), where it had left native code at every
// store, as microbench's prop_create did: each answer is Node's and the
// interpreter's, and fill, whose stores Go makes so, never leaves its
// code.
func TestJITKeyedWrites(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 6 {
		for _, r := range keyedWriteRounds {
			rounds = append(rounds, r[0])
		}
	}
	r := jitAgreesForTest(t, keyedWriteSetup, rounds...)
	for _, round := range keyedWriteRounds {
		v, err := r.Run(compileForTest(t, "''+("+round[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", round[0], err)
		}
		if got := v.String().Go(); got != round[1] {
			t.Errorf("%s = %s, want %s", round[0], got, round[1])
		}
	}
	e := jitNativeForTest(t, r, "fill")
	if e.ssaStats.hosts > 2 || len(e.keyedWrites) == 0 {
		t.Errorf("fill left its code %d times, its keyed writes %v", e.ssaStats.hosts, e.keyedWrites)
	}
}
