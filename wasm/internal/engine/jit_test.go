package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// nativeInstance instantiates a module given in the text format with its
// functions compiled at their first call, or skips without native code.
func nativeInstance(t testing.TB, src string, imports ...Extern) *Instance {
	t.Helper()
	if !JITAvailable() {
		t.Skip("no native code in this build")
	}
	b, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m, derr := decoder.Decode(b, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
		t.Fatal(derr)
	}
	code := NewCode(m)
	if !code.EnableJIT(0) {
		t.Fatal("EnableJIT refused where JITAvailable")
	}
	inst, err := Instantiate(context.Background(), code, imports)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// nativeRan reports whether function name of inst has native code.
func nativeRan(inst *Instance, name string) bool {
	ext, _ := inst.Export(name)
	return inst.Code.get(ext.Func.Index).native.Load() != nil
}

func call(t *testing.T, inst *Instance, name string, args ...uint64) ([]Val, error) {
	t.Helper()
	ext, ok := inst.Export(name)
	if !ok {
		t.Fatalf("no export %q", name)
	}
	vals := make([]Val, len(args))
	for i, a := range args {
		vals[i] = Val{N: a}
	}
	return Invoke(context.Background(), ext.Func, vals)
}

func TestJITFibAndLoop(t *testing.T) {
	inst := nativeInstance(t, benchWAT)
	res, err := call(t, inst, "fib", 20)
	if err != nil || res[0].N != 6765 {
		t.Fatalf("fib(20) = %v, %v; want 6765", res, err)
	}
	if !nativeRan(inst, "fib") {
		t.Fatal("fib has no native code")
	}
	res, err = call(t, inst, "loop", 100000)
	var want uint32
	for i := uint32(0); i < 100000; i++ {
		want += i * 3
	}
	if err != nil || res[0].N != uint64(want) {
		t.Fatalf("loop(100000) = %v, %v; want %d", res, err, want)
	}
}

func TestJITTrapTrace(t *testing.T) {
	inst := nativeInstance(t, `(module
  (memory 1)
  (func $inner (param i32) (result i32) (i32.load (local.get 0)))
  (func $middle (param i32) (result i32) (call $inner (local.get 0)))
  (func (export "outer") (param i32) (result i32) (call $middle (local.get 0))))`)
	res, err := call(t, inst, "outer", 8)
	if err != nil || res[0].N != 0 {
		t.Fatalf("outer(8) = %v, %v", res, err)
	}
	_, err = call(t, inst, "outer", 65534)
	var tr *Trap
	if !errors.As(err, &tr) || tr.Code != TrapMemOutOfBounds {
		t.Fatalf("outer(65534) = %v, want an out-of-bounds trap", err)
	}
	if len(tr.Frames) != 3 || tr.Frames[0].Func != 0 || tr.Frames[1].Func != 1 || tr.Frames[2].Func != 2 {
		t.Fatalf("trap frames %+v, want inner, middle, outer", tr.Frames)
	}
}

func TestJITStackOverflow(t *testing.T) {
	inst := nativeInstance(t, `(module
  (func $r (export "r") (param i32) (result i32) (i32.add (call $r (local.get 0)) (i32.const 1))))`)
	_, err := call(t, inst, "r", 1)
	var tr *Trap
	if !errors.As(err, &tr) || tr.Code != TrapStackOverflow {
		t.Fatalf("r(1) = %v, want a stack overflow", err)
	}
	// The exec is usable after.
	inst2 := nativeInstance(t, benchWAT)
	if res, err := call(t, inst2, "fib", 10); err != nil || res[0].N != 55 {
		t.Fatalf("fib(10) after an overflow = %v, %v", res, err)
	}
}

// The trap codes native code reports are the engine's.
func TestJITTrapCodes(t *testing.T) {
	for _, c := range []struct {
		jit  int
		trap TrapCode
	}{
		{wjit.TrapUnreachable, TrapUnreachable},
		{wjit.TrapMemOutOfBounds, TrapMemOutOfBounds},
		{wjit.TrapDivByZero, TrapDivByZero},
		{wjit.TrapDivUnrepresentable, TrapDivUnrepresentable},
		{wjit.TrapRemByZero, TrapRemByZero},
		{wjit.TrapFloatUnrepresentable, TrapFloatUnrepresentable},
		{wjit.TrapStackOverflow, TrapStackOverflow},
	} {
		if TrapCode(c.jit) != c.trap {
			t.Errorf("jit trap %d is %v, want %d", c.jit, c.trap, c.trap)
		}
	}
}
