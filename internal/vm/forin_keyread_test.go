package vm

import "testing"

// forInKeyReadSetup reads o[k] by the keys a for...in hands out: an
// array's indices, which a key string the runtime handed out names (its
// keyAtoms), read from the array's dense elements (stringKeyRead) -- the
// key's array or another's, or another kind of object's, which reads it
// the long way: an element deleted during the loop, or cut off by a
// shorter length, which is not visited; elements added, which are not
// either, an array grown or made sparse; a hole read through
// Array.prototype; an accessor; a mapped arguments object, whose element
// a parameter's assignment changes; a typed array, a String object and a
// string; a proxy, which is given the key as a string; an index of 1024 or
// more, whose string is not one the runtime keeps; values of other kinds;
// keys kept and read after the loop, more of them than the runtime
// remembers (later), so that some are looked up again; a site that reads
// by a key string and by a number (mixed); an inherited enumerable key and
// a hole, after which the keys are a snapshot's; and an array grown during
// the loop, whose new elements are not visited. Node and QuickJS give the
// same answers.
const forInKeyReadSetup = `
		function vals(o) { var s = []; for (var k in o) s.push(o[k]); return s.join(',') }
		function cross(a, b) { var s = []; for (var k in a) s.push(String(b[k])); return s.join(',') }
		function last(a) { var x; for (var k in a) x = a[k]; return x }
		function later(a) { var ks = []; for (var k in a) ks.push(k); var s = 0; for (var j = 0; j < ks.length; j++) s += a[ks[j]]; return s }
		function at(a, k) { return a[k] }
		function mixed(a) { var s = 0; for (var k in a) s += at(a, k) * 10 + at(a, +k) + a[+k]; return s }
		function run() {
			var out = [vals([10, 20, 30]), cross([1, 2, 3], [7, 8]), cross([1, 2], { 0: 'x', 1: 'y' })]
			out.push((function () { var a = [1, 2, 3], s = []; for (var k in a) { if (k === '0') delete a[1]; s.push(k + ':' + a[k]) } return s.join(',') })())
			out.push((function () { var a = [1, 2, 3], s = []; for (var k in a) { if (k === '0') a.length = 1; s.push(k + ':' + a[k]) } return s.join(',') })())
			out.push((function () { var a = [1, 2], s = []; for (var k in a) { if (k === '0') a.push(3); s.push(k + ':' + a[k]) } return s.join(',') })())
			out.push((function () { var a = [1, 2, 3], s = []; for (var k in a) { if (k === '0') a[1000] = 9; s.push(k + ':' + a[k]) } return s.join(',') })())
			Object.defineProperty(Array.prototype, '1', { value: 'p', writable: true, configurable: true })
			out.push(cross([0, 0, 0], [1, , 3]))
			delete Array.prototype[1]
			var acc = [0, 1]; Object.defineProperty(acc, '0', { get() { return 'g' } })
			out.push(cross([0, 0], acc))
			out.push((function (x, y) { var s = []; for (var k in arguments) { if (k === '0') y = 'Y'; s.push(arguments[k]) } return s.join(',') })('a', 'b'))
			out.push(cross([0, 0], new Int8Array([5, 6])), cross([0, 0], new String('hi')), cross([0, 0], 'hi'))
			out.push(cross([0], new Proxy([9], { get(t, k) { return typeof k + k } })))
			var big = []; for (var i = 0; i < 1100; i++) big.push(i)
			out.push(last(big), cross([0, 0], null === 1 ? 0 : [undefined, null]))
			var sym = {}; out.push(cross([0, 0, 0], [sym, 'str', 1.5].map(String)))
			var many = []; for (var i = 0; i < 300; i++) many.push(i)
			out.push(later(many), later(big), mixed([1, 2, 3]))
			Array.prototype.extra = 'e'; out.push(vals([1, 2])); delete Array.prototype.extra
			out.push(vals([, 5]), vals([1, 2, 3].concat()), (function () { var a = [1, 2, 3], s = []; for (var k in a) { if (k === '0') for (var i = 0; i < 100; i++) a.push(i); s.push(a[k]) } return s.join(',') })())
			return out.join(' | ')
		}`

const forInKeyReadWant = "10,20,30 | 7,8,undefined | x,y | 0:1,2:3 | 0:1 | 0:1,1:2 | 0:1,1:2,2:3 | 1,p,3 | g,1 | a,Y | 5,6 | h,i | h,i | string0 | 1099 | undefined,null | [object Object],str,1.5 | 44850 | 604450 | 72 | 1,2,e | 5 | 1,2,3 | 1,2,3"

// TestForInKeyRead is forInKeyReadSetup on the trees and in the
// interpreter, each loop run often enough to be a tree's.
func TestForInKeyRead(t *testing.T) {
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, forInKeyReadSetup+"; var res; for (var i = 0; i < 30; i++) res = run(); res"))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != forInKeyReadWant {
			t.Errorf("tree tier %v: got %q, want %q", tree, got, forInKeyReadWant)
		}
	}
}
