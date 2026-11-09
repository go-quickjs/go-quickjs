package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
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
	return thresholdInstance(t, src, 0, imports...)
}

// interpretedInstance is nativeInstance with every function left to the
// interpreter, whatever WASM_JIT says.
func interpretedInstance(t testing.TB, src string) *Instance {
	t.Helper()
	return thresholdInstance(t, src, math.MaxInt32)
}

// thresholdInstance instantiates a module given in the text format with its
// functions compiled after threshold calls, or skips without native code.
func thresholdInstance(t testing.TB, src string, threshold int, imports ...Extern) *Instance {
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
	if !code.EnableJIT(threshold) {
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

// Native code reaches what its instructions' short forms do not: slots past
// 32 KiB of a frame and a callee's frame past them, a declared local
// zeroed again at every call, operand stacks deeper than the registers
// (every entry spilled and reloaded), memory offsets past 64 KiB, and a
// br_table of 300 entries.
func TestJITLargeFramesAndSpills(t *testing.T) {
	var src strings.Builder
	src.WriteString(`(module
  (memory 2)
  (func $inc (param i64) (result i64) (i64.add (local.get 0) (i64.const 1)))
  (func (export "big") (param i64) (result i64) (local`)
	src.WriteString(strings.Repeat(" i64", 4999))
	src.WriteString(`)
    (local.set 4000 (local.get 0))
    (local.set 4998 (i64.mul (local.get 4000) (i64.const 3)))
    (local.set 4500 (call $inc (local.get 4998)))
    (i64.add (local.get 4500) (local.get 4999))
    (local.set 4999 (local.get 0)))
  (func (export "deepi") (param i32) (result i32)
`)
	for k := 1; k <= 40; k++ {
		fmt.Fprintf(&src, "    (i32.add (local.get 0) (i32.const %d))\n", k)
	}
	src.WriteString(strings.Repeat("    i32.add\n", 39))
	src.WriteString(`  )
  (func (export "deepf") (param f64) (result f64)
`)
	for k := 1; k <= 40; k++ {
		fmt.Fprintf(&src, "    (f64.add (local.get 0) (f64.const %d))\n", k)
	}
	src.WriteString(strings.Repeat("    f64.add\n", 39))
	src.WriteString(`  )
  (func (export "mem") (param i32) (result i64)
    (i64.store offset=70000 (local.get 0) (i64.const 0x123456789))
    (i64.add (i64.load offset=70000 (local.get 0))
             (i64.load8_u offset=131071 (i32.const 0))))
  (func (export "table") (param i32) (result i32)
    (block $b2 (block $b1 (block $b0
      (br_table`)
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&src, " %d", i%3)
	}
	src.WriteString(` 2 (local.get 0)))
      (return (i32.const 10)))
     (return (i32.const 11)))
    (i32.const 12)))`)
	inst := nativeInstance(t, src.String())
	for _, c := range []struct {
		name string
		arg  uint64
		want uint64
	}{
		// The local written at the end is zero again at the next call.
		{"big", 5, 16},
		{"big", 7, 22},
		{"deepi", 1, 40 + 820},
		{"deepi", 0xFFFFFFFF, uint64(uint32(820 - 40))},
		{"deepf", math.Float64bits(0.5), math.Float64bits(840)},
		{"mem", 8, 0x123456789},
		{"table", 0, 10},
		{"table", 1, 11},
		{"table", 2, 12},
		{"table", 298, 11},
		{"table", 299, 12},
		{"table", 300, 12},
		{"table", 0xFFFFFFFF, 12},
	} {
		res, err := call(t, inst, c.name, c.arg)
		if err != nil || res[0].N != c.want {
			t.Errorf("%s(%#x) = %v, %v; want %#x", c.name, c.arg, res, err, c.want)
		}
		if !nativeRan(inst, c.name) {
			t.Errorf("%s has no native code", c.name)
		}
	}
	// 131072 - 8 - 70000 is the last address an i64 at offset 70000 fits.
	if _, err := call(t, inst, "mem", 131072-70000-8); err != nil {
		t.Errorf("mem at the last word: %v", err)
	}
	_, err := call(t, inst, "mem", 131072-70000-7)
	var tr *Trap
	if !errors.As(err, &tr) || tr.Code != TrapMemOutOfBounds {
		t.Errorf("mem past the end = %v, want an out-of-bounds trap", err)
	}

	// A shared memory's size is read at each access, as other threads grow
	// it: a grow is seen by the code that made it.
	shared := nativeInstance(t, `(module
  (memory 1 4 shared)
  (func (export "at") (param i32) (result i32)
    (i32.store (local.get 0) (i32.const 7))
    (i32.load (local.get 0)))
  (func (export "grow") (param i32) (result i32)
    (drop (memory.grow (i32.const 1)))
    (i32.store (local.get 0) (i32.const 9))
    (i32.load (local.get 0))))`)
	if res, err := call(t, shared, "at", 65532); err != nil || res[0].N != 7 {
		t.Errorf("at(65532) = %v, %v; want 7", res, err)
	}
	if _, err := call(t, shared, "at", 65533); !errors.As(err, &tr) || tr.Code != TrapMemOutOfBounds {
		t.Errorf("at(65533) = %v, want an out-of-bounds trap", err)
	}
	if res, err := call(t, shared, "grow", 65536); err != nil || res[0].N != 9 {
		t.Errorf("grow(65536) = %v, %v; want 9", res, err)
	}
	if !nativeRan(shared, "at") || !nativeRan(shared, "grow") {
		t.Error("the shared memory's functions have no native code")
	}
}

// Native code's floats are the interpreter's bit for bit on the same
// machine, NaNs' payloads and signs included -- which the differential
// test, taking any NaN for a NaN, does not check: arithmetic with signaling
// and quiet NaNs in either operand, min and max of NaNs and of zeros, the
// roundings, abs, neg and copysign of NaNs, and the conversions between f32
// and f64. Each result's whole word is compared, an f32's upper half
// included. Of two NaN operands, which one's payload an arithmetic
// operation keeps is the order of its instruction's operands, which the Go
// compiler chooses for the interpreter's commutative operators and the
// spec leaves open: there both results must be NaNs.
func TestJITFloatBits(t *testing.T) {
	f32s := []uint64{0, 0x80000000, 0x3f800000, 0xbfc00000, 0x40200000, 0xbf000000, 0x3f000000,
		0x7f800000, 0xff800000, 0x7fc00000, 0xffc00000, 0x7f800001, 0x7fa00005, 0xff812345,
		0x7fc12345, 0x00000001, 0x80000001, 0x7f7fffff, 0x4b000001, 0x4f000000}
	f64s := []uint64{0, 1 << 63, 0x3ff0000000000000, 0xbff8000000000000, 0x4004000000000000,
		0xbfe0000000000000, 0x3fe0000000000000, 0x7ff0000000000000, 0xfff0000000000000,
		0x7ff8000000000000, 0xfff8000000000000, 0x7ff0000000000001, 0x7ff4000000000005,
		0xfff0000000012345, 0x7ff8000000012345, 1, 0x8000000000000001, 0x7fefffffffffffff,
		0x4330000000000001, 0x47efffffe0000000, 0x36a0000000000000}
	var src strings.Builder
	src.WriteString("(module\n")
	type op struct {
		name   string
		binary bool
		// arith says which NaN of two it gives is open.
		arith bool
		in    []uint64
	}
	var ops []op
	for _, ty := range []string{"f32", "f64"} {
		in := f32s
		if ty == "f64" {
			in = f64s
		}
		for _, name := range []string{"add", "sub", "mul", "div", "min", "max", "copysign"} {
			fmt.Fprintf(&src, "(func (export %q) (param %s %s) (result %s) (%s.%s (local.get 0) (local.get 1)))\n",
				ty+"."+name, ty, ty, ty, ty, name)
			ops = append(ops, op{ty + "." + name, true, name != "copysign", in})
		}
		for _, name := range []string{"abs", "neg", "sqrt", "ceil", "floor", "trunc", "nearest"} {
			fmt.Fprintf(&src, "(func (export %q) (param %s) (result %s) (%s.%s (local.get 0)))\n",
				ty+"."+name, ty, ty, ty, name)
			ops = append(ops, op{ty + "." + name, false, false, in})
		}
	}
	src.WriteString(`(func (export "f32.demote_f64") (param f64) (result f32) (f32.demote_f64 (local.get 0)))
(func (export "f64.promote_f32") (param f32) (result f64) (f64.promote_f32 (local.get 0)))
)`)
	ops = append(ops, op{"f32.demote_f64", false, false, f64s}, op{"f64.promote_f32", false, false, f32s})
	isNaN := func(bits uint64, f64 bool) bool {
		if f64 {
			return bits&(1<<63-1) > 0x7ff0000000000000
		}
		return bits&(1<<31-1) > 0x7f800000 && bits>>32 == 0
	}
	interp := interpretedInstance(t, src.String())
	native := nativeInstance(t, src.String())
	for _, o := range ops {
		var argss [][]uint64
		for _, a := range o.in {
			if !o.binary {
				argss = append(argss, []uint64{a})
				continue
			}
			for _, b := range o.in {
				argss = append(argss, []uint64{a, b})
			}
		}
		for _, args := range argss {
			want, err := call(t, interp, o.name, args...)
			if err != nil {
				t.Fatal(err)
			}
			got, err := call(t, native, o.name, args...)
			wide := strings.HasPrefix(o.name, "f64")
			if err == nil && o.arith && isNaN(args[0], wide) && isNaN(args[1], wide) &&
				isNaN(want[0].N, wide) && isNaN(got[0].N, wide) {
				continue
			}
			if err != nil || got[0].N != want[0].N {
				t.Errorf("%s(%#x) = %#x, %v natively, %#x interpreted", o.name, args, got, err, want[0].N)
			}
		}
		if nativeRan(interp, o.name) || !nativeRan(native, o.name) {
			t.Fatalf("%s: the interpreted instance has native code, or the native one none", o.name)
		}
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
  ;; A vector keeps this one interpreted.
  (func $odd_interp (param i32) (result i32)
    (drop (v128.const i64x2 0 0))
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

// An endless chain of tail calls ends with its context, interpreted and
// native, direct and through a table.
func TestTailCallsPoll(t *testing.T) {
	src := `(module
  (type $t (func (param i32) (result i32)))
  (table funcref (elem $ind))
  (func $direct (export "direct") (param i32) (result i32)
    (return_call $direct (i32.add (local.get 0) (i32.const 1))))
  (func $ind (export "indirect") (param i32) (result i32)
    (return_call_indirect (type $t) (i32.add (local.get 0) (i32.const 1)) (i32.const 0))))`
	b, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, native := range []bool{false, true} {
		m, derr := decoder.Decode(b, decoder.CoreLimits())
		if derr != nil {
			t.Fatal(derr)
		}
		code := NewCode(m)
		if native && !code.EnableJIT(0) {
			continue
		}
		inst, err := Instantiate(context.Background(), code, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"direct", "indirect"} {
			ext, _ := inst.Export(name)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			_, err := Invoke(ctx, ext.Func, []Val{{N: 0}})
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s (native %v) = %v, want the context's deadline", name, native, err)
			}
		}
	}
}
