package vm

import (
	"encoding/json"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"os"
	"testing"
)

// keyedStoreCases are keyed stores -- o[k] = v, { [k]: v }, a class's
// computed and plain fields -- whose sites' caches (reg_table_keyed.go)
// fill and then meet what an entry must not answer: a setter or a
// read-only property put on the prototype chain after, a prototype
// swapped, objects made non-extensible, sealed or frozen, accessors of the
// object's own, proxies, arrays' and typed arrays' keys, numeric strings,
// __proto__ as a computed key, a key whose conversion runs code, a define
// over a property of other attributes, and stores past an object's room.
// Each answer is Node's.
var keyedStoreCases = []struct{ name, src, want string }{
	{"setterAfter", `(function () {
	var proto = {}, r = [], o
	function put(o, k, v) { o[k] = v }
	for (var i = 0; i < 20; i++) { o = Object.create(proto); put(o, 'a', i) }
	r.push(o.a, Object.keys(o).length)
	Object.defineProperty(proto, 'a', { set(v) { r.push('set' + v) }, configurable: true })
	for (var i = 0; i < 3; i++) { o = Object.create(proto); put(o, 'a', i) }
	r.push(Object.keys(o).length, o.hasOwnProperty('a'))
	return r.join()
})()`, "19,1,set0,set1,set2,0,false"},
	{"readOnlyAfter", `(function () {
	var proto = {}, r = [], o
	function put(o, k, v) { o[k] = v }
	function sput(o, k, v) { 'use strict'; o[k] = v }
	for (var i = 0; i < 20; i++) { o = Object.create(proto); put(o, 'b', i); o = Object.create(proto); sput(o, 'b', i) }
	Object.defineProperty(proto, 'b', { value: 'ro', writable: false })
	o = Object.create(proto); put(o, 'b', 5); r.push(o.b, Object.keys(o).length)
	try { o = Object.create(proto); sput(o, 'b', 5); r.push('no throw') } catch (e) { r.push(e.constructor.name, Object.keys(o).length) }
	return r.join()
})()`, "ro,0,TypeError,0"},
	{"deepChain", `(function () {
	class A {} class B extends A {} class C extends B {}
	var r = [], o
	function put(o, k, v) { o[k] = v }
	for (var i = 0; i < 20; i++) { o = new C(); put(o, 'q', i); put(o, 'w', i) }
	r.push(o.q, o.w)
	Object.defineProperty(A.prototype, 'q', { set(v) { r.push('A.q' + v) }, configurable: true })
	Object.defineProperty(Object.prototype, 'w', { set(v) { r.push('O.w' + v) }, configurable: true })
	for (var i = 0; i < 2; i++) { o = new C(); put(o, 'q', i); put(o, 'w', i) }
	r.push(Object.keys(o).length)
	delete Object.prototype.w
	return r.join()
})()`, "19,19,A.q0,O.w0,A.q1,O.w1,0"},
	{"protoSwap", `(function () {
	var r = [], o, p1 = { x: 1 }, p2 = { set k(v) { r.push('p2:' + v) } }
	function put(o, k, v) { o[k] = v }
	for (var i = 0; i < 20; i++) { o = Object.create(p1); put(o, 'k', i) }
	Object.setPrototypeOf(p1, p2)
	o = Object.create(p1); put(o, 'k', 'v')
	r.push(o.hasOwnProperty('k'))
	return r.join()
})()`, "p2:v,false"},
	{"extensibility", `(function () {
	var r = [], o
	function put(o, k, v) { o[k] = v }
	function sput(o, k, v) { 'use strict'; o[k] = v }
	for (var i = 0; i < 20; i++) { o = { x: 1 }; put(o, 'y', i); put(o, 'x', i); sput(o, 'x', i); sput(o, 'z', i) }
	o = { x: 1 }; Object.preventExtensions(o); put(o, 'y', 1); put(o, 'x', 2); r.push('y' in o, o.x)
	try { sput(o, 'z', 1) } catch (e) { r.push(e.constructor.name) }
	o = { x: 1 }; Object.seal(o); put(o, 'x', 3); put(o, 'y', 3); r.push(o.x, 'y' in o)
	o = { x: 1 }; Object.freeze(o); put(o, 'x', 4); r.push(o.x)
	try { sput(o, 'x', 4) } catch (e) { r.push(e.constructor.name) }
	var k = 'x'
	function lit(o) { return class extends (function () { return o }) { [k] = 9 } }
	var F = lit(Object.freeze({ x: 1 }))
	try { new F(); r.push('no throw') } catch (e) { r.push(e.constructor.name) }
	var G = lit(Object.preventExtensions({ y: 1 }))
	try { new G(); r.push('no throw') } catch (e) { r.push(e.constructor.name) }
	return r.join()
})()`, "false,2,TypeError,3,false,1,TypeError,TypeError,TypeError"},
	{"accessorsOwn", `(function () {
	var r = [], o
	function put(o, k, v) { o[k] = v }
	for (var i = 0; i < 20; i++) { o = { a: 0 }; put(o, 'a', i) }
	o = { get a() { return 'g' }, set a(v) { r.push('own' + v) } }
	put(o, 'a', 1); put(o, 'a', 2)
	o = { get a() { return 'g' } }
	put(o, 'a', 3); r.push(o.a)
	var k = 'a'
	for (var i = 0; i < 20; i++) o = { a: 0, [k]: i }
	r.push(o.a)
	o = { get a() { return 'g' }, [k]: 'data' }
	var d = Object.getOwnPropertyDescriptor(o, 'a')
	r.push(d.value, d.writable, d.enumerable, d.configurable, typeof d.get)
	return r.join()
})()`, "own1,own2,g,19,data,true,true,true,undefined"},
	{"proxies", `(function () {
	var r = [], o
	function put(o, k, v) { o[k] = v }
	var t = {}
	var p = new Proxy(t, {
		set(t, k, v) { r.push('set:' + String(k)); t[k] = v; return true },
		defineProperty(t, k, d) { r.push('def:' + String(k)); return Reflect.defineProperty(t, k, d) }
	})
	for (var i = 0; i < 20; i++) { o = {}; put(o, 'a', i) }
	put(p, 'a', 1); put(p, 'a', 2)
	class Base { constructor() { return p } }
	var k = 'b'
	class D extends Base { a = 1; [k] = 2 }
	for (var i = 0; i < 3; i++) new D()
	var o2 = Object.create(p)
	put(o2, 'c', 3)
	r.push(t.a, t.b, o2.hasOwnProperty('c'))
	return r.join()
})()`, "set:a,set:a,def:a,def:b,def:a,def:b,def:a,def:b,set:c,1,2,false"},
	{"arrays", `(function () {
	var r = [], a
	function put(o, k, v) { o[k] = v }
	for (var i = 0; i < 20; i++) { a = []; put(a, 'foo', i); put(a, '1', i); put(a, 'length', 5) }
	r.push(a.length, a[1], a.foo)
	for (var i = 0; i < 20; i++) { a = [1, 2, 3]; put(a, 'length', 1) }
	r.push(a.length)
	for (var i = 0; i < 20; i++) { a = []; put(a, '4294967294', i) }
	r.push(a.length, a[4294967294])
	for (var i = 0; i < 20; i++) { a = []; put(a, '4294967295', i) }
	r.push(a.length, a['4294967295'])
	for (var i = 0; i < 20; i++) { a = []; put(a, '2147483648', i) }
	r.push(a.length)
	var o
	for (var i = 0; i < 20; i++) o = { ['4294967294']: i, ['length']: 3 }
	r.push(Object.keys(o).join('/'))
	return r.join()
})()`, "5,19,19,1,4294967295,19,0,19,2147483649,4294967294/length"},
	{"typed", `(function () {
	var r = [], t
	function put(o, k, v) { o[k] = v }
	for (var i = 0; i < 20; i++) { t = new Uint8Array(4); put(t, 'foo', i); put(t, '1', 7); put(t, '-0', 9); put(t, '1.5', 9); put(t, '9', 9); put(t, 'length', 9) }
	r.push(t.foo, t[1], '-0' in t, '1.5' in t, '9' in t, t.length, Object.keys(t).join('/'))
	function sput(o, k, v) { 'use strict'; o[k] = v }
	try { sput(t, '-0', 1); r.push('ok') } catch (e) { r.push(e.constructor.name) }
	return r.join()
})()`, "19,7,false,false,false,4,0/1/2/3/foo,ok"},
	{"numericStrings", `(function () {
	var r = [], o
	function put(o, k, v) { o[k] = v }
	for (var i = 0; i < 20; i++) { o = {}; put(o, 'b', 1); put(o, '1', 2); put(o, '-0', 3); put(o, '4294967295', 4); put(o, '0', 5); put(o, '4294967294', 6) }
	r.push(Object.keys(o).join('/'), o[-0], o['-0'], o[0])
	for (var i = 0; i < 20; i++) o = { b: 1, ['1']: 2, ['-0']: 3, ['4294967295']: 4, ['0']: 5 }
	r.push(Object.keys(o).join('/'))
	return r.join()
})()`, "0/1/4294967294/b/-0/4294967295,5,3,5,0/1/b/-0/4294967295"},
	{"protoKey", `(function () {
	var r = [], o, k = '__proto__', px = { tag: 'px' }
	for (var i = 0; i < 20; i++) o = { [k]: i }
	r.push(Object.getPrototypeOf(o) === Object.prototype, o.hasOwnProperty('__proto__'), Object.keys(o).join('/'), o.__proto__)
	function put(o, k, v) { o[k] = v }
	for (var i = 0; i < 20; i++) { o = {}; put(o, k, px) }
	r.push(Object.getPrototypeOf(o) === px, o.hasOwnProperty('__proto__'), o.tag)
	return r.join()
})()`, "true,true,__proto__,19,true,false,px"},
	{"classFields", `(function () {
	var r = [], keys = ['f0', 'f1', 'f2', 'f3']
	function mk(k) { return class { [k] = 1; ['x' + k] = 2; static [k] = 3; y = 4 } }
	var out = []
	for (var i = 0; i < 20; i++) { var C = mk(keys[i & 3]); var o = new C(); out.push(Object.keys(o).join('/'), C[keys[i & 3]]) }
	r.push(out.slice(-8).join(';'))
	class N { ['name'] = 1; static ['name'] = 'n'; static ['length'] = 9; static ['prototype2'] = 0 }
	r.push(N.name, N.length, new N().name)
	return r.join()
})()`, "f0/xf0/y;3;f1/xf1/y;3;f2/xf2/y;3;f3/xf3/y;3,n,9,1"},
	{"keyConversion", `(function () {
	var log = [], o = {}
	var key = { toString() { log.push('key'); return 'kk' } }
	function put(o, k, v) { o[k] = v }
	for (var i = 0; i < 20; i++) put(o, key, i)
	log.push(o.kk)
	var x
	for (var i = 0; i < 3; i++) x = { [key]: (log.push('val'), i) }
	log.push(x.kk)
	var c = 0
	var key2 = { toString() { c++; return 's' + (c & 1) } }
	for (var i = 0; i < 10; i++) put(o, key2, i)
	log.push(o.s0, o.s1, c)
	return log.length + ':' + log.slice(-12).join()
})()`, "31:key,19,key,val,key,val,key,val,2,9,8,10"},
	{"redefine", `(function () {
	var r = []
	class B { constructor(cfg) { Object.defineProperty(this, 'a', { value: 0, writable: true, enumerable: false, configurable: cfg }) } }
	var k = 'a'
	class D extends B { [k] = 5 }
	for (var i = 0; i < 20; i++) { var d = new D(true) }
	r.push(d.a, d.propertyIsEnumerable('a'))
	var n = 0
	for (var i = 0; i < 20; i++) { try { new D(false) } catch (e) { n++; var name = e.constructor.name } }
	r.push(n, name)
	var o
	for (var i = 0; i < 20; i++) o = { a: 1, [k]: i }
	var dd = Object.getOwnPropertyDescriptor(o, 'a')
	r.push(dd.value, dd.writable, dd.enumerable, dd.configurable)
	class E extends B { a = 7 }
	var e = new E(true)
	r.push(e.a, e.propertyIsEnumerable('a'))
	return r.join()
})()`, "5,true,20,TypeError,19,true,true,true,7,true"},
	{"growth", `(function () {
	var r = [], keys = []
	for (var j = 0; j < 24; j++) keys.push('g' + j)
	function put(o, k, v) { o[k] = v }
	function fill(o) { for (var j = 0; j < 24; j++) put(o, keys[j], j); return o }
	var objs = []
	for (var i = 0; i < 30; i++) objs.push(fill({ a: 1 }))
	var o = objs[29]
	r.push(Object.keys(o).length, o.g23, o.g7, o.g8, 'g15' in o, o[keys[16]])
	var lits = []
	for (var i = 0; i < 30; i++) {
		var p = { a: 1, b: 2, c: 3, d: 4, e: 5, f: 6, g: 7 }
		put(p, 'h', i)
		p['i' + ''] = 9
		p[keys[0]] = 10; p[keys[1]] = 11; p[keys[2]] = 12
		lits.push(p)
	}
	var p = lits[29]
	r.push(Object.keys(p).join('/'), p.h, p.i, p.g2, p[keys[1]], lits[3].h)
	return r.join()
})()`, "25,23,7,8,true,16,a/b/c/d/e/f/g/h/i/g0/g1/g2,29,9,12,11,3"},
	{"symbols", `(function () {
	var r = [], s = Symbol('s'), t = Symbol.iterator, o
	function put(o, k, v) { o[k] = v }
	for (var i = 0; i < 20; i++) { o = {}; put(o, s, i); put(o, t, i) }
	r.push(o[s], o[t], Object.getOwnPropertySymbols(o).length)
	for (var i = 0; i < 20; i++) o = { [s]: i }
	r.push(o[s])
	Object.defineProperty(Object.prototype, s, { set(v) { r.push('sym-set' + v) }, configurable: true })
	o = {}; put(o, s, 'x')
	r.push(Object.getOwnPropertySymbols(o).length)
	delete Object.prototype[s]
	return r.join()
})()`, "19,19,2,19,sym-setx,0"},
	{"polyKeys", `(function () {
	var keys = ['ka', 'kb', 'kc', 'kd'], o, sum = 0
	for (var j = 0; j < 400; j++) { o = { [keys[j & 3]]: j, v: 1 }; sum += o[keys[j & 3]] + o.v }
	var p, sum2 = 0
	for (var j = 0; j < 400; j++) { p = {}; p[keys[j & 3]] = j; p[keys[(j + 1) & 3]] = 1; sum2 += p[keys[j & 3]] }
	return [sum, sum2, Object.keys(o).join('/'), Object.keys(p).join('/')].join()
})()`, "80200,79800,kd/v,kd/ka"},
}

// TestKeyedStoreSemantics runs keyedStoreCases, each answer Node's.
// KEYED_STORE_DUMP names a file to write the cases to, as JSON, for
// running them in Node.
func TestKeyedStoreSemantics(t *testing.T) {
	if f := os.Getenv("KEYED_STORE_DUMP"); f != "" {
		m := map[string]string{}
		for _, c := range keyedStoreCases {
			m[c.name] = c.src
		}
		b, _ := json.Marshal(m)
		if err := os.WriteFile(f, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Parallel()
	for _, c := range keyedStoreCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := New(Config{})
			defer r.Close()
			v, err := r.Run(compileForTest(t, c.src))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("got\n%s\nwant\n%s", got, c.want)
			}
		})
	}
}

// TestKeyedStoreCacheHits checks that a keyed store's site remembers each
// pair of a shape and a key it meets, up to the four its entries hold, and
// answers them from then on: after 400 stores of four keys, by a literal's
// computed key and by an assignment, each site has been filled four times,
// once a pair, and holds all four.
func TestKeyedStoreCacheHits(t *testing.T) {
	t.Parallel()
	const src = `
		var keys = ['ka', 'kb', 'kc', 'kd']
		function lit(n) { var o; for (var j = 0; j < n; j++) o = { [keys[j & 3]]: j, v: 1 }; return o }
		function set(n) { var o; for (var j = 0; j < n; j++) { o = {}; o[keys[j & 3]] = j }; return o }
		lit(400); set(400)`
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lit", "set"} {
		fn, err := r.getValueProp(Obj(r.global), r.atoms.intern(name))
		if err != nil {
			t.Fatal(err)
		}
		cl := fn.Object().fn().closure
		c := r.regOf(cl)
		sites := 0
		for pc, w := range c.rc.Code {
			if op := w.Op(); op != bytecode.RDefineIndex && op != bytecode.RSetIndex {
				continue
			}
			sites++
			e := regKeyedSite(c, cl, pc)
			if e == nil {
				t.Fatalf("%s: the keyed store at %d has no site", name, pc)
			}
			if e.fills != 4 {
				t.Errorf("%s: the site was filled %d times, want 4", name, e.fills)
			}
			seen := map[Atom]bool{}
			for _, x := range append([]propCache{*e}, polyEntries(e)...) {
				if x.next == nil {
					t.Errorf("%s: an entry is no addition", name)
					continue
				}
				seen[x.next.key] = true
			}
			if len(seen) != 4 {
				t.Errorf("%s: the site holds %d keys, want 4", name, len(seen))
			}
		}
		if sites != 1 {
			t.Errorf("%s has %d keyed stores, want 1", name, sites)
		}
	}
}

// polyEntries is c's poly's entries.
func polyEntries(c *propCache) []propCache {
	if c.poly == nil {
		return nil
	}
	return c.poly.entries[:c.poly.n]
}
