package vm

import "testing"

// Object.prototype.hasOwnProperty is a native leaf (zzz_hasownleaf.go):
// called without a frame where its receiver is an object, no proxy nor
// module namespace, and its key no object, and as the built-in otherwise.
// A hot loop asks it of each kind -- an object's own and inherited keys, a
// number, a symbol, an array's index and length, a function's lazy name,
// through hasOwnProperty.call and as a method -- and of what the leaf
// refuses: a proxy, whose trap runs each time; a key that is an object,
// whose toString runs each time; a string, whose ToObject has its index
// and length; null, which throws a TypeError. Each answer is Node's
// (v26.12.1).
func TestHasOwnPropertyLeaf(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		const hop = Object.prototype.hasOwnProperty
		const sym = Symbol("s")
		const proto = {inherited: 1}
		const o = Object.create(proto); o.own = 1; o[7] = 2; o[sym] = 3
		let traps = 0
		const p = new Proxy({x: 1}, {getOwnPropertyDescriptor(t, k) { traps++; return Reflect.getOwnPropertyDescriptor(t, k) }})
		let strings = 0
		const key = {toString() { strings++; return "own" }}
		function f() {}
		let n = 0, out = ""
		for (let i = 0; i < 2000; i++) {
			n += hop.call(o, "own") + 2 * hop.call(o, "inherited") + 4 * o.hasOwnProperty(7) + 8 * hop.call(o, sym)
			n += 16 * [1, 2].hasOwnProperty(1) + 32 * [1, 2].hasOwnProperty("length") + 64 * hop.call(f, "name")
			n += 128 * hop.call(p, "x") + 256 * hop.call(o, key) + 512 * hop.call("abc", 1) + 1024 * hop.call("abc", "length")
			n += 2048 * hop.call(o, "missing")
		}
		try { hop.call(null, "x") } catch (e) { out = e.constructor.name }
		[n, traps, strings, out].join()`))
	if err != nil {
		t.Fatal(err)
	}
	const want = "4090000,2000,2000,TypeError"
	if got := v.String().Go(); got != want {
		t.Errorf("got %s, want Node's %s", got, want)
	}
	m := r.proto.object.getOwn(r.atoms.intern("hasOwnProperty"))
	if fd := m.value().Object().fn(); fd.elemOp != elemObjHasOwn {
		t.Errorf("hasOwnProperty's op is %d, want elemObjHasOwn", fd.elemOp)
	}
}
