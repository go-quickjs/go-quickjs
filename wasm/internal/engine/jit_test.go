package engine

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
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
	} {
		if TrapCode(c.jit) != c.trap {
			t.Errorf("jit trap %d is %v, want %d", c.jit, c.trap, c.trap)
		}
	}
}

// Locals living in registers keep their values across native calls, calls
// through Go and indirect calls, and in loops.
func TestJITLocalsAcrossCalls(t *testing.T) {
	var hostCalls int
	host := NewHostFunction(module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}, Results: []module.ValueType{module.I32}}), "twice",
		func(_ context.Context, _ *Instance, args, results []Val) error {
			hostCalls++
			results[0] = Val{N: uint64(uint32(args[0].N) * 2)}
			return nil
		})
	inst := nativeInstance(t, `(module
  (import "env" "twice" (func $twice (param i32) (result i32)))
  (type $t (func (param i32) (result i32)))
  (table funcref (elem $inc $twice))
  (func $inc (param i32) (result i32) (i32.add (local.get 0) (i32.const 1)))
  (func (export "run") (param $n i32) (result f64)
    (local $i i32) (local $acc i32) (local $f f64) (local $g f32)
    (local.set $g (f32.const 0.5))
    (block $done
      (loop $l
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $acc (i32.add (local.get $acc) (call $inc (local.get $i))))
        (local.set $acc (i32.add (local.get $acc) (call $twice (local.get $i))))
        (local.set $acc (i32.add (local.get $acc)
          (call_indirect (type $t) (local.get $i) (i32.and (local.get $i) (i32.const 1)))))
        (local.set $f (f64.add (local.get $f) (f64.convert_i32_u (local.get $acc))))
        (local.set $g (f32.mul (local.get $g) (f32.const 1.5)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $l)))
    (f64.add (local.get $f) (f64.promote_f32 (local.get $g)))))`,
		Extern{Kind: module.ExternFunc, Func: host})
	const n = 100
	var acc uint32
	var f float64
	g := float32(0.5)
	for i := uint32(0); i < n; i++ {
		acc += i + 1
		acc += i * 2
		if i&1 == 0 {
			acc += i + 1
		} else {
			acc += i * 2
		}
		f += float64(acc)
		g *= 1.5
	}
	want := f + float64(g)
	res, err := call(t, inst, "run", n)
	if err != nil || math.Float64frombits(res[0].N) != want {
		t.Fatalf("run(%d) = %v, %v; want %v", n, res, err, want)
	}
	if hostCalls != n+n/2 || !nativeRan(inst, "run") {
		t.Fatalf("%d host calls, native %v", hostCalls, nativeRan(inst, "run"))
	}
}

// Tail calls run in constant stack across the tiers: between native
// functions, and between a native one and one the JIT refuses.
func TestJITTailCalls(t *testing.T) {
	inst := nativeInstance(t, `(module
  (type $t (func (param i32) (result i32)))
  (table funcref (elem $even $odd $odd_interp))
  (func $even (export "even") (param i32) (result i32)
    (if (result i32) (i32.eqz (local.get 0)) (then (i32.const 1))
      (else (return_call $odd (i32.sub (local.get 0) (i32.const 1))))))
  (func $odd (param i32) (result i32)
    (if (result i32) (i32.eqz (local.get 0)) (then (i32.const 0))
      (else (return_call_indirect (type $t) (i32.sub (local.get 0) (i32.const 1)) (i32.const 0)))))
  (func $even2 (export "even2") (param i32) (result i32)
    (if (result i32) (i32.eqz (local.get 0)) (then (i32.const 1))
      (else (return_call $odd_interp (i32.sub (local.get 0) (i32.const 1))))))
  ;; A reference value keeps this one interpreted.
  (func $odd_interp (param i32) (result i32)
    (drop (ref.null func))
    (if (result i32) (i32.eqz (local.get 0)) (then (i32.const 0))
      (else (return_call $even2 (i32.sub (local.get 0) (i32.const 1)))))))`)
	for _, name := range []string{"even", "even2"} {
		for _, c := range []struct{ n, want uint64 }{{0, 1}, {7, 0}, {1_000_000, 1}, {1_000_001, 0}} {
			res, err := call(t, inst, name, c.n)
			if err != nil || res[0].N != c.want {
				t.Fatalf("%s(%d) = %v, %v; want %d", name, c.n, res, err, c.want)
			}
		}
	}
	if !nativeRan(inst, "even") || !nativeRan(inst, "even2") {
		t.Fatal("even or even2 has no native code")
	}
}

// A host function called from native code may call back into wasm, which
// runs natively at a level of its own, and grow the memory, which the
// native caller sees.
func TestJITHostReentry(t *testing.T) {
	var inst *Instance
	host := NewHostFunction(module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}, Results: []module.ValueType{module.I32}}), "back",
		func(ctx context.Context, _ *Instance, args, results []Val) error {
			inst.Memories[0].Grow(1)
			ext, _ := inst.Export("fib")
			res, err := Invoke(ctx, ext.Func, []Val{args[0]})
			if err != nil {
				return err
			}
			results[0] = res[0]
			return nil
		})
	inst = nativeInstance(t, `(module
  (import "env" "back" (func $back (param i32) (result i32)))
  (memory (export "mem") 1)
  (func $fib (export "fib") (param i32) (result i32)
    (if (result i32) (i32.lt_u (local.get 0) (i32.const 2))
      (then (local.get 0))
      (else (i32.add (call $fib (i32.sub (local.get 0) (i32.const 1)))
                     (call $fib (i32.sub (local.get 0) (i32.const 2)))))))
  (func (export "run") (param i32) (result i32)
    (local $r i32)
    (local.set $r (call $back (local.get 0)))
    ;; The page the host added is there.
    (i32.store (i32.const 70000) (local.get $r))
    (i32.add (i32.load (i32.const 70000)) (memory.size))))`,
		Extern{Kind: module.ExternFunc, Func: host})
	res, err := call(t, inst, "run", 15)
	if err != nil || res[0].N != 610+2 {
		t.Fatalf("run(15) = %v, %v; want 612", res, err)
	}
}

// An endless loop in native code polls, and ends when the context does.
func TestJITCancel(t *testing.T) {
	inst := nativeInstance(t, `(module (func (export "spin") (loop $l (br $l))))`)
	ext, _ := inst.Export("spin")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Invoke(ctx, ext.Func, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("spin = %v, want the context's deadline", err)
	}
}
