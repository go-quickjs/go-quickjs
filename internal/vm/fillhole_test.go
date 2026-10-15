package vm

import "testing"

// An element stored where new Array(n) left a hole is stored in place
// (fillHole), by the tree tier's set_index and by setIndexed, where the
// store is a new element's: in order and out of order, on a subclass's
// array and on one whose length is not writable; and not where it is not
// -- a frozen, a sealed or a non-extensible array, which a strict store
// throws on, a setter or a read-only element on Array.prototype or
// Object.prototype, which is asked instead. Each answer is Node's, three
// rounds alike, on the trees and in the interpreter.
func TestFillHoles(t *testing.T) {
	const src = `
		const out = []
		function fill(a, order) { for (const i of order) a[i] = i * 10; return a }
		function strictFill(a, i) { 'use strict'; try { a[i] = 1; return 'ok' } catch (e) { return e.name } }
		for (let round = 0; round < 3; round++) {
			const r = []
			r.push(fill(new Array(4), [0, 1, 2, 3]).join())
			r.push(fill(new Array(4), [3, 1]).join() + '/' + Object.keys(fill(new Array(4), [3, 1])).join())
			const fz = Object.freeze(new Array(3))
			r.push(fill(fz, [0, 1]).length + ':' + Object.keys(fz).length + ':' + strictFill(fz, 0))
			const sl = Object.seal(new Array(3))
			r.push(fill(sl, [1]).join() + ':' + strictFill(sl, 0))
			const ne = Object.preventExtensions(new Array(2))
			r.push(fill(ne, [0]).join() + ':' + strictFill(ne, 1))
			const nl = new Array(3)
			Object.defineProperty(nl, 'length', { writable: false })
			r.push(fill(nl, [0, 2]).join() + ':' + nl.length + ':' + strictFill(nl, 1) + ':' + strictFill(nl, 3))
			class Sub extends Array {}
			r.push(fill(new Sub(3), [2, 0]).join())
			let log = ''
			Object.defineProperty(Array.prototype, 2, { set(v) { log += 'A' + v }, configurable: true })
			r.push(fill(new Array(4), [0, 1, 2, 3]).join() + ':' + log)
			delete Array.prototype[2]
			Object.defineProperty(Object.prototype, 1, { set(v) { log += 'O' + v }, configurable: true })
			r.push(fill(new Array(3), [0, 1, 2]).join() + ':' + log)
			delete Object.prototype[1]
			Object.defineProperty(Array.prototype, 0, { value: 'ro', writable: false, configurable: true })
			r.push(fill(new Array(2), [0, 1]).join() + ':' + strictFill(new Array(2), 0))
			delete Array.prototype[0]
			r.push(fill(new Array(3), [0, 1, 2]).join())
			out.push(r.join(' | '))
		}
		out[0] + '\n' + (out[0] === out[1] && out[1] === out[2])`
	const want = "0,10,20,30 | ,10,,30/1,3 | 3:0:TypeError | ,,:TypeError | ,:TypeError | 0,,20:3:ok:TypeError | 0,,20 | 0,10,,30:A20 | 0,,20:A20O10 | ro,10:TypeError | 0,10,20\ntrue"
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree tier %v: got\n%s\nwant\n%s", tree, got, want)
		}
	}
	SetTreeTier(true)
}

// new Array(n) is n holes, in storage made for n -- the object's own up to
// sixteen elements -- for each size of that storage and past it, past a
// run of holes and past the length dense storage is made for, filled and
// grown as any array is.
func TestNewArrayLengths(t *testing.T) {
	const src = `
		const out = []
		for (const n of [0, 1, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 16, 17, 1024, 1025, 65536, 65537]) {
			const a = new Array(n)
			const holes = n === 0 || (!(0 in a) && !((n - 1) in a))
			a[n >> 1] = 'x'
			a.push('y')
			out.push(n + ':' + a.length + ':' + holes + ':' + Object.keys(a).join('.') + ':' + Object.isExtensible(a) + ':' + a.indexOf('y'))
		}
		out.join(' ')`
	const want = "0:2:true:0.1:true:1 1:2:true:0.1:true:1 4:5:true:2.4:true:4 5:6:true:2.5:true:5 " +
		"6:7:true:3.6:true:6 7:8:true:3.7:true:7 8:9:true:4.8:true:8 9:10:true:4.9:true:9 " +
		"10:11:true:5.10:true:10 11:12:true:5.11:true:11 12:13:true:6.12:true:12 13:14:true:6.13:true:13 " +
		"16:17:true:8.16:true:16 17:18:true:8.17:true:17 1024:1025:true:512.1024:true:1024 " +
		"1025:1026:true:512.1025:true:1025 65536:65537:true:32768.65536:true:65536 65537:65538:true:32768.65537:true:65537"
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
