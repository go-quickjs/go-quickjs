package vm

import "testing"

// An addition at a site whose cache has given up, past a prototype chain
// deeper than a site's cache holds, is made as the runtime's cache of
// additions remembers it (deepStore) only while every prototype is the one
// it was, with the layout it had: a setter, a read-only property, a
// prototype replaced or an object made non-extensible after the addition
// was remembered each takes the write the long way.
func TestDeepStoreFollowsTheChain(t *testing.T) {
	const src = `
		'use strict'
		class A {} class B extends A {} class C extends B {} class D extends C {} class E extends D {}
		const shapes = []
		for (let i = 0; i < 40; i++) {
			// Objects of 40 shapes, so that the site below gives up.
			const o = new E()
			o['k' + i] = i
			shapes.push(o)
		}
		function add(o, v) { o.x = v; return o }
		const out = []
		function run(label) {
			const got = []
			for (const s of shapes) {
				const o = Object.create(Object.getPrototypeOf(s))
				for (const k of Object.keys(s)) o[k] = s[k]
				try { add(o, 1); got.push(Object.getOwnPropertyNames(o).includes('x') ? 'own' : 'none') }
				catch (e) { got.push(e.constructor.name) }
			}
			out.push(label + ':' + [...new Set(got)].join('/'))
		}
		run('plain'); run('plain again')
		let set = 0
		Object.defineProperty(C.prototype, 'x', { set(v) { set++ }, configurable: true })
		run('setter'); out.push('setter calls ' + set)
		delete C.prototype.x
		run('setter gone')
		Object.defineProperty(B.prototype, 'x', { value: 0, writable: false, configurable: true })
		run('read-only')
		delete B.prototype.x
		run('read-only gone')
		const F = function () {}
		F.prototype.x = 0
		Object.defineProperty(F.prototype, 'x', { set(v) { set++ } })
		Object.setPrototypeOf(D.prototype, F.prototype)
		run('chain replaced'); out.push('setter calls ' + set)
		Object.setPrototypeOf(D.prototype, C.prototype)
		run('chain back')
		const frozen = shapes.map(s => { const o = Object.create(Object.getPrototypeOf(s)); for (const k of Object.keys(s)) o[k] = s[k]; return Object.preventExtensions(o) })
		const fr = []
		for (const o of frozen) { try { add(o, 1); fr.push('added') } catch (e) { fr.push(e.constructor.name) } }
		out.push('non-extensible:' + [...new Set(fr)].join('/'))
		out.join(', ')`
	const want = "plain:own, plain again:own, setter:none, setter calls 40, setter gone:own, read-only:TypeError, " +
		"read-only gone:own, chain replaced:none, setter calls 80, chain back:own, non-extensible:TypeError"
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		var counts string
		if err == nil && r.shapes.deep != nil {
			n := 0
			for _, e := range r.shapes.deep {
				if e.n > 2 {
					n++
				}
			}
			if n == 0 {
				counts = " (no deep addition remembered)"
			}
		}
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want || counts != "" {
			t.Errorf("tree %v: %s%s\nwant %s", tree, got, counts, want)
		}
	}
}
