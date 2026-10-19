//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
)

// A native callee that leaves native code while the collector marks --
// here c, deoptimizing at a string after a host function set the flag
// native code tests -- has Go finish it, and its native callers go on
// natively after their calls, as they do when it does not mark: the only
// pointer native code stores there before it tests the flag again is the
// call's result, which Go wrote with its barrier (runSSALoop,
// jitUnwindNative, mir's resumeCall). They had finished their
// invocations in the interpreter, and what they called with them.
func TestJITResumesWhileMarking(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	flag := jitMarkingForTest(t)
	*flag = 0
	setup := `
		function c(x) { if (typeof x === 'string') mark(); return x * 2 }
		function b(x) { let s = 0; for (let j = 0; j < 2; j++) s += c(x); return s }
		function a(n, mk) { let s = 0; for (let i = 0; i < n; i++) s += b(i === mk ? '' + i : i); unmark(); return s }
	`
	r := jitRuntimeForTest(t, Config{JIT: true})
	host := func(name string, v uint8) {
		r.global.setOwnRaw(r.atoms.intern(name), Obj(r.newNativeFunc(name, 0, func(rt *Runtime, _ Value, _ []Value) (Value, error) {
			*flag = v
			return Undefined, nil
		})), propWritable|propConfigurable)
	}
	host("mark", 1)
	host("unmark", 0)
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	want.global.setOwnRaw(want.atoms.intern("mark"), Obj(want.newNativeFunc("mark", 0, func(*Runtime, Value, []Value) (Value, error) { return Undefined, nil })), propWritable|propConfigurable)
	want.global.setOwnRaw(want.atoms.intern("unmark"), Obj(want.newNativeFunc("unmark", 0, func(*Runtime, Value, []Value) (Value, error) { return Undefined, nil })), propWritable|propConfigurable)
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	var resumed, levels uint64
	for i := range 12 {
		src := `''+a(400, -1)`
		if i >= 8 {
			// c leaves while the collector marks, b and a native callers of
			// it.
			src = `''+a(400, 300)`
		}
		if i == 8 {
			resumed, levels = r.jit.resumed, r.jit.resumedLevels
		}
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		gv, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d: got %s, interpreter %s", i, got, want)
		}
	}
	if r.jit.resumed+r.jit.resumedLevels == resumed+levels {
		t.Errorf("no native caller went on natively after a callee left while the collector marked: resumed %d, levels %d", r.jit.resumed-resumed, r.jit.resumedLevels-levels)
	}
}
