package vm

import "testing"

// A method read off a string or a number in a method call node by count
// (methodCallN) is read from its prototype by the site's cache: string and
// number methods, a string's length and an unknown name, which go the long
// way (methodMiss) and are no functions, booleans and symbols, which go
// the long way too, a method put on String.prototype and Number.prototype
// and then replaced, a getter there called with the primitive as its this,
// and a method deleted. Node's answers, on the trees and in the
// interpreter; on the trees, the strings' and numbers' methods are read
// without methodMiss.
func TestPrimitiveMethodReadsCached(t *testing.T) {
	const src = `'use strict'
		function err(f) { try { return String(f()) } catch (e) { return e.constructor.name } }
		function run(round) {
			const r = []
			const s = 'Hello, World', n = 3.14159, b = true, sym = Symbol('q')
			let k = 0
			for (let i = 0; i < 4; i++) {
				r.push(s.indexOf('o'), s.charAt(i), s.slice(i, i + 3), s.toUpperCase().length, n.toFixed(i), n.toString(), b.toString(), sym.toString())
				r.push(err(() => s.length()), err(() => s.nope()), err(() => n.nope()))
				k += s.indexOf(s.charAt(i + 1))
			}
			r.push(k)
			if (round === 1) {
				String.prototype.who = function () { return typeof this }
				Number.prototype.who = function () { return typeof this }
				Object.defineProperty(String.prototype, 'g', { get() { const t = typeof this; return () => t + ':' + this.length }, configurable: true })
			}
			if (round === 2) {
				String.prototype.who = function () { 'use strict'; return 'strict ' + typeof this }
				String.prototype.indexOf = function () { return -42 }
			}
			if (round === 3) delete Number.prototype.toFixed
			r.push(err(() => s.who()), err(() => n.who()), err(() => s.g()), err(() => s.indexOf('l')), err(() => n.toFixed(1)))
			return r.join(',')
		}
		const out = []
		for (let round = 0; round < 4; round++) out.push(run(round))
		out.join(' | ')`
	const want = "4,H,Hel,12,3,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,4,e,ell,12,3.1,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,4,l,llo,12,3.14,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,4,l,lo,,12,3.142,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,9,TypeError,TypeError,TypeError,2,3.1 | 4,H,Hel,12,3,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,4,e,ell,12,3.1,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,4,l,llo,12,3.14,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,4,l,lo,,12,3.142,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,9,string,number,string:12,2,3.1 | 4,H,Hel,12,3,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,4,e,ell,12,3.1,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,4,l,llo,12,3.14,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,4,l,lo,,12,3.142,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,9,strict string,number,string:12,-42,3.1 | -42,H,Hel,12,3,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,-42,e,ell,12,3.1,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,-42,l,llo,12,3.14,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,-42,l,lo,,12,3.142,3.14159,true,Symbol(q),TypeError,TypeError,TypeError,-168,strict string,number,string:12,-42,TypeError"
	misses := 0
	orig := methodMiss
	methodMiss = func(c *tctx, o Value, key Atom, gpc int) Value {
		misses++
		return orig(c, o, key, gpc)
	}
	defer func() { methodMiss = orig }()
	for _, tree := range []bool{true, false} {
		misses = 0
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v: got %s\nwant %s", tree, got, want)
		}
		// Each round's loop reads 6 methods of strings and numbers 4
		// times, and 2 of a boolean and a symbol, which miss.
		// Each of 4 rounds' loops reads 6 methods of strings and numbers 4
		// times, and 2 of a boolean and a symbol, which miss (32); the
		// rest are each site's first read, its first after a round changes
		// a prototype, and keys sharing primProps's slot (key&63), which
		// atoms numbered differently from one runtime to the next make
		// vary (58 to 70). Read the long way, they all miss (134).
		if tree && misses > 100 {
			t.Errorf("%d method reads missed on the trees", misses)
		}
	}
}
