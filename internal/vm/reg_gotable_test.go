package vm

import "testing"

// regGoTableRuntime runs src as register code by the Go table's loop
// (QJS_NONATIVE) or the native handlers, and returns the runtime and its
// answer.
func regGoTableRuntime(t *testing.T, src string, native bool) (*Runtime, string) {
	t.Helper()
	fn := compileForTest(t, src)
	was := SetRegNative(native)
	t.Cleanup(func() { SetRegNative(was) })
	r := New(Config{})
	t.Cleanup(r.Close)
	v, err := r.Run(fn)
	if err != nil {
		t.Fatalf("native %v: %v", native, err)
	}
	return r, v.String().Go()
}

func regGlobalFn(t *testing.T, r *Runtime, name string) Value {
	t.Helper()
	v, err := r.getProp(r.global, r.atoms.intern(name), Obj(r.global))
	if err != nil || !v.IsObject() || v.Object().fn() == nil {
		t.Fatalf("%s = %v, %v", name, v, err)
	}
	return v
}

// TestRegGoTableLeafCallbackKept: a callback whose body is a leaf the
// frameless evaluator gives up on -- a sum of BigInts -- is the built-in's
// kept callback in the Go table's loop once the evaluator has given up on
// it (pureMiss), rather than callDirect's at each call, which tried the
// evaluator and then entered a frame. Its answers are the same.
func TestRegGoTableLeafCallbackKept(t *testing.T) {
	src := `
		var f = (acc, x) => acc + x
		var a = new BigInt64Array(10)
		for (var i = 0; i < 10; i++) a[i] = BigInt(i) * 3n
		var s = 0n
		for (var j = 0; j < 100; j++) s += a.reduce(f, 0n) + [1n, 2n].reduce(f, 5n)
		String(s)`
	r, got := regGoTableRuntime(t, src, false)
	if got != "14300" {
		t.Fatalf("got %s, want 14300", got)
	}
	if jitBuilt && r.jitOn() {
		t.Skip("a runtime with the JIT keeps its leaves on callDirect, which counts them")
	}
	fv := regGlobalFn(t, r, "f")
	if m := fv.Object().fn().closure.pureMiss; m < pureMissLimit {
		t.Fatalf("pureMiss %d: the evaluator has not given up on the body", m)
	}
	cb := r.regCallbackOf(fv, Undefined)
	if cb == nil {
		t.Fatal("no kept callback for a leaf the evaluator gave up on, in the Go table")
	}
	cb.end()
}
