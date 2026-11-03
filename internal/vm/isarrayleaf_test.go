package vm

import "testing"

// Array.isArray is a native leaf (zzz_isarrayleaf.go): called without a
// frame where its argument is no proxy, and as the built-in otherwise. A
// hot loop asks it, plainly through a binding and as a method, of arrays,
// a subclass's, an array-like, primitives and no argument, and of what
// the leaf refuses: a proxy of an array, which is one, and a revoked
// proxy, which throws a TypeError. Each answer is Node's (v26.12.1).
func TestIsArrayLeaf(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		const isArr = Array.isArray
		class Sub extends Array {}
		const vals = [[], new Sub(), {length: 0}, "abc", 1, null, undefined, new Proxy([], {}), new Proxy({}, {})]
		let n = 0, out = ""
		for (let i = 0; i < 2000; i++) {
			for (let k = 0; k < vals.length; k++) n += (isArr(vals[k]) ? 1 : 0) << k
			n += Array.isArray() ? 1000 : 0
		}
		const { proxy, revoke } = Proxy.revocable([], {})
		revoke()
		try { isArr(proxy) } catch (e) { out = e.constructor.name }
		[n, out].join()`))
	if err != nil {
		t.Fatal(err)
	}
	const want = "262000,TypeError"
	if got := v.String().Go(); got != want {
		t.Errorf("got %s, want Node's %s", got, want)
	}
	m := r.proto.arrayCtor.getOwn(r.atoms.intern("isArray"))
	if fd := m.value().Object().fn(); fd.elemOp != elemArrIsArray {
		t.Errorf("Array.isArray's op is %d, want elemArrIsArray", fd.elemOp)
	}
}
