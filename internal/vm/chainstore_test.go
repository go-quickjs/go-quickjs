package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// An addition to an object whose prototype chain is longer than a write
// site's entry holds -- a subclass's instance, three and four prototypes
// deep here -- is remembered by the chain's validity cell
// (fillChainStore), and made by the site's cache only while the cell is
// valid: a setter or a read-only property of the name put anywhere on the
// chain, Object.prototype included, a prototype replaced, or a receiver
// that is not extensible each takes the write the long way, and the cache
// takes it again once the chain is as before. A constructor's additions
// to its subclasses' instances are made the same way. Node's answers, on
// the trees and in the interpreter; the site's entry is the cell's, and it
// has not given up.
func TestChainStoreFollowsTheCell(t *testing.T) {
	const src = `'use strict'
		class A {} class B extends A {} class C extends B {} class D extends C {}
		function add(o, v) { o.x = v; return o }
		const out = []
		function run(label) {
			const got = []
			for (let i = 0; i < 5; i++) {
				const o = new D()
				try { add(o, i); got.push(Object.getOwnPropertyNames(o).includes('x') ? 'own' + o.x : 'none') } catch (e) { got.push(e.constructor.name) }
			}
			out.push(label + ':' + [...new Set(got)].join('/'))
		}
		run('plain'); run('again')
		let set = 0
		Object.defineProperty(B.prototype, 'x', { set(v) { set++ }, configurable: true })
		run('setter'); out.push('calls ' + set)
		delete B.prototype.x
		run('gone')
		Object.defineProperty(A.prototype, 'x', { value: 0, writable: false, configurable: true })
		run('read-only')
		delete A.prototype.x
		run('read-only gone')
		const F = function () {}
		Object.defineProperty(F.prototype, 'x', { set(v) { set++ } })
		Object.setPrototypeOf(C.prototype, F.prototype)
		run('chain replaced'); out.push('calls ' + set)
		Object.setPrototypeOf(C.prototype, B.prototype)
		run('chain back')
		Object.defineProperty(Object.prototype, 'x', { set(v) { set++ }, configurable: true })
		run('top setter'); out.push('calls ' + set)
		delete Object.prototype.x
		run('top gone')
		const fr = []
		for (let i = 0; i < 3; i++) { try { add(Object.preventExtensions(new D()), 1); fr.push('added') } catch (e) { fr.push(e.constructor.name) } }
		out.push('non-extensible:' + [...new Set(fr)].join('/'))
		class P { constructor(a) { this.a = a; this.b = a + 1 } }
		class Q extends P {} class R extends Q { constructor() { super(5); this.c = 7 } }
		const rs = []
		for (let i = 0; i < 4; i++) { const r = new R(); rs.push(Object.keys(r).join('') + r.a + r.b + r.c) }
		out.push(rs.join(','))
		out.join(', ')`
	const want = "plain:own0/own1/own2/own3/own4, again:own0/own1/own2/own3/own4, setter:none, calls 5, gone:own0/own1/own2/own3/own4, read-only:TypeError, read-only gone:own0/own1/own2/own3/own4, chain replaced:none, calls 10, chain back:own0/own1/own2/own3/own4, top setter:none, calls 15, top gone:own0/own1/own2/own3/own4, non-extensible:TypeError, abc567,abc567,abc567,abc567"
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		treeTier.Store(previous)
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v: got %s\nwant %s", tree, got, want)
		}
		cl := r.global.getOwn(r.atoms.intern("add")).value().Object().fn().closure
		for _, in := range cl.fn.Code {
			if in.Op != bytecode.OpSetProp {
				continue
			}
			if c := &cl.ic[in.B]; c.cell == nil || c.next == nil || c.fills >= maxCacheFills {
				t.Errorf("tree %v: the site's entry has cell %v, next %v, fills %d", tree, c.cell != nil, c.next != nil, c.fills)
			}
		}
		r.Close()
	}
}
