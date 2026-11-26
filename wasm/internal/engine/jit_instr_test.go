//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"

	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
)

// exitInstance instantiates src with every function compiled at its first
// call -- and optimized there where opt says so -- its native code compiled
// with set's switches.
func exitInstance(t *testing.T, src string, opt bool, set func(*codeJITTest)) *Instance {
	t.Helper()
	if set != nil {
		codeSwitch(t, func(c *codeTest) { set(&c.jit) })
	}
	inst := nativeInstance(t, src)
	inst.Code.setOSRCalls(0)
	if opt {
		inst.Code.SetOptThreshold(0)
		inst.Code.SetOptBackground(false)
	} else {
		inst.Code.SetOptThreshold(-1)
	}
	return inst
}

// baseCPU is the CPU without the features the code may do without:
// POPCNT, SSE4.1, and the SIMD its vectors take.
func baseCPU() *wjit.Features {
	f := wjit.CPU
	f.Popcnt, f.Round, f.SIMD = false, false, false
	return &f
}

// sameOutcome calls name with args on want and got and requires the same
// results, or the same trap at the same frames.
func sameOutcome(t *testing.T, want, got *Instance, name string, args ...uint64) {
	t.Helper()
	w, werr := call(t, want, name, args...)
	g, gerr := call(t, got, name, args...)
	if (werr == nil) != (gerr == nil) {
		t.Fatalf("%s%v = %v, %v natively; %v, %v interpreted", name, args, g, gerr, w, werr)
	}
	if werr != nil {
		var wt, gt *Trap
		if !errors.As(werr, &wt) || !errors.As(gerr, &gt) {
			t.Fatalf("%s%v: %v natively, %v interpreted", name, args, gerr, werr)
		}
		if wt.Code != gt.Code || len(wt.Frames) != len(gt.Frames) {
			t.Fatalf("%s%v: trap %v at %v natively, %v at %v interpreted", name, args, gt.Code, gt.Frames, wt.Code, wt.Frames)
		}
		for i := range wt.Frames {
			if wt.Frames[i].Func != gt.Frames[i].Func || wt.Frames[i].Offset != gt.Frames[i].Offset {
				t.Fatalf("%s%v: trap %v at %v natively, %v at %v interpreted", name, args, gt.Code, gt.Frames, wt.Code, wt.Frames)
			}
		}
		return
	}
	if fmt.Sprint(w) != fmt.Sprint(g) {
		t.Fatalf("%s%v = %v natively, %v interpreted", name, args, g, w)
	}
}

// The instructions native code runs in Go (ExitInstr) trap as the
// interpreter does, at the instruction, under the same frames: every
// trapping instruction of trapCases with every instruction run so, in the
// baseline code and the optimizing tier's, and the v128 accesses of GC
// objects, which the code runs so, under a call.
func TestJITInstrExitTraps(t *testing.T) {
	cases := append([]trapCase(nil), trapCases...)
	cases = append(cases,
		trapCase{name: "struct.get of a v128 field of null", src: `(module (type $v (struct (field i32) (field (mut v128))))
		(func (export "f") (result v128) (struct.get $v 1 (ref.null $v))))`, code: TrapNullDereference},
		trapCase{name: "struct.set of a v128 field of null", src: `(module (type $v (struct (field (mut v128))))
		(func (export "f") (struct.set $v 0 (ref.null $v) (v128.const i64x2 1 2))))`, code: TrapNullDereference},
		trapCase{name: "array.get of v128s past the length", src: `(module (type $a (array (mut v128)))
		(func (export "f") (result v128) (array.get $a (array.new_default $a (i32.const 3)) (i32.const 3))))`, code: TrapArrayOutOfBounds},
		trapCase{name: "array.set of v128s past the length", src: `(module (type $a (array (mut v128)))
		(func (export "f") (array.set $a (array.new_default $a (i32.const 3)) (i32.const -1) (v128.const i64x2 1 2))))`, code: TrapArrayOutOfBounds},
		trapCase{name: "v128.load past the memory", src: `(module (memory 1)
		(func (export "f") (result v128) (v128.load offset=8 (i32.const 65528))))`, code: TrapMemOutOfBounds},
		trapCase{name: "v128.store64_lane past the memory", src: `(module (memory 1)
		(func (export "f") (v128.store64_lane 1 (i32.const 65530) (v128.const i64x2 1 2))))`, code: TrapMemOutOfBounds},
	)
	modes := []struct {
		name string
		opt  bool
		set  func(*codeJITTest)
	}{
		{"exits", false, func(c *codeJITTest) { c.exitAll = true }},
		{"opt exits", true, func(c *codeJITTest) { c.exitAll = true }},
		{"base cpu", false, func(c *codeJITTest) { c.features = baseCPU() }},
		{"baseline", false, nil},
		{"opt", true, nil},
	}
	for _, c := range cases {
		for _, m := range modes {
			t.Run(c.name+"/"+m.name, func(t *testing.T) {
				interp := interpretedInstance(t, c.src)
				inst := exitInstance(t, c.src, m.opt, m.set)
				sameOutcome(t, interp, inst, "f")
				if !nativeRan(inst, "f") {
					t.Fatal("f has no native code")
				}
				if m.opt && !optimized(inst, "f") {
					t.Fatal("f is not optimized")
				}
			})
		}
	}
}

// An instruction run in Go in a body the optimizing tier inlined reports
// its trap at the instruction in the callee, under the call's frame, and
// gives the interpreter's results.
func TestJITInstrExitInlined(t *testing.T) {
	src := `(module (memory 1)
  (func $g (param i32) (result i32) (i32.div_u (i32.const 7) (local.get 0)))
  (func $h (param i32) (result i32) (i32.load offset=4 (local.get 0)))
  (func (export "f") (param i32) (result i32)
    (i32.add (call $g (local.get 0)) (call $h (local.get 0)))))`
	interp := interpretedInstance(t, src)
	for _, opt := range []bool{false, true} {
		inst := exitInstance(t, src, opt, func(c *codeJITTest) { c.exitAll = true })
		var n int64
		for _, arg := range []uint64{3, 0, 65534, 65532, 1} {
			n += countInstrs(t, func() { sameOutcome(t, interp, inst, "f", arg) })
		}
		if n == 0 {
			t.Fatalf("opt %v: no instruction ran in Go", opt)
		}
		if opt && !optimized(inst, "f") {
			t.Fatal("f is not optimized")
		}
	}
}

// The v128 fields and elements of GC objects are read and written natively
// in both tiers, and on a CPU without the features the code takes popcnt,
// the roundings and the SIMD instructions run in Go, the rest of each
// function natively: no function is refused, and the results are the
// interpreter's.
func TestJITInstrExitResults(t *testing.T) {
	src := `(module
  (type $v (struct (field (mut v128)) (field i32) (field (mut v128))))
  (type $a (array (mut v128)))
  (memory 1)
  (func (export "fields") (param i32 i64) (result i64) (local $s (ref null $v))
    (local.set $s (struct.new $v (i64x2.splat (local.get 1)) (local.get 0) (i32x4.splat (local.get 0))))
    (struct.set $v 2 (local.get $s) (i64x2.replace_lane 1 (struct.get $v 2 (local.get $s)) (local.get 1)))
    (i64.add (i64x2.extract_lane 0 (struct.get $v 0 (local.get $s)))
      (i64.add (i64x2.extract_lane 1 (struct.get $v 2 (local.get $s)))
        (i64.extend_i32_u (i32.add (struct.get $v 1 (local.get $s)) (i32x4.extract_lane 0 (struct.get $v 2 (local.get $s))))))))
  (func (export "elems") (param i32 i64) (result i64) (local $a (ref null $a)) (local $i i32) (local $acc v128)
    (local.set $a (array.new $a (i64x2.splat (local.get 1)) (local.get 0)))
    (block $done (loop $l
      (br_if $done (i32.ge_u (local.get $i) (local.get 0)))
      (array.set $a (local.get $a) (local.get $i)
        (i64x2.add (array.get $a (local.get $a) (local.get $i)) (i64x2.splat (i64.extend_i32_u (local.get $i)))))
      (local.set $acc (i64x2.add (local.get $acc) (array.get $a (local.get $a) (local.get $i))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (i64.add (i64x2.extract_lane 0 (local.get $acc)) (i64x2.extract_lane 1 (local.get $acc))))
  (func (export "bits") (param i32 i64) (result i64)
    (i64.add (i64.extend_i32_u (i32.popcnt (local.get 0))) (i64.popcnt (local.get 1))))
  (func (export "rounds") (param i32 i64) (result i64) (local $f f64)
    (local.set $f (f64.div (f64.convert_i64_s (local.get 1)) (f64.const 7)))
    (i64.add
      (i64.add (i64.reinterpret_f64 (f64.ceil (local.get $f))) (i64.reinterpret_f64 (f64.floor (local.get $f))))
      (i64.add (i64.reinterpret_f64 (f64.nearest (local.get $f)))
        (i64.extend_i32_u (i32.reinterpret_f32 (f32.trunc (f32.demote_f64 (local.get $f))))))))
  (func (export "vectors") (param i32 i64) (result i64)
    (v128.store (i32.const 16) (i64x2.splat (local.get 1)))
    (v128.store (i32.const 32) (i32x4.mul (v128.load (i32.const 16)) (i32x4.splat (local.get 0))))
    (i64.add (i64x2.extract_lane 1 (v128.load (i32.const 32)))
      (i64.extend_i32_u (i8x16.extract_lane_u 3 (i8x16.popcnt (v128.load offset=8 (i32.const 24))))))))`
	interp := interpretedInstance(t, src)
	names := []string{"fields", "elems", "bits", "rounds", "vectors"}
	args := [][2]uint64{{0, 0}, {5, 0x0123456789abcdef}, {0xffffffff, 1 << 63}, {17, 1e9}}
	for _, m := range []struct {
		name string
		opt  bool
		feat *wjit.Features
	}{{"baseline", false, nil}, {"opt", true, nil}, {"baseline base cpu", false, baseCPU()}, {"opt base cpu", true, baseCPU()}} {
		t.Run(m.name, func(t *testing.T) {
			inst := exitInstance(t, src, m.opt, func(c *codeJITTest) { c.features = m.feat })
			n := countInstrs(t, func() {
				for _, name := range names {
					for _, a := range args {
						sameOutcome(t, interp, inst, name, a[0], a[1])
					}
				}
			})
			// The v128 accesses are native; on the base CPU the vectors'
			// instructions, popcnt and the roundings run in Go.
			if base := m.feat != nil; base != (n > 0) && !(m.opt && !base) {
				t.Fatalf("%d instructions ran in Go", n)
			}
			for _, name := range names {
				if !nativeRan(inst, name) {
					t.Fatalf("%s has no native code", name)
				}
				// The optimizing tier leaves a function with v128 values
				// on a CPU without its SIMD to the baseline.
				v128 := name == "fields" || name == "elems" || name == "vectors"
				if m.opt && optimized(inst, name) != !(m.feat != nil && v128) {
					t.Fatalf("%s optimized: %v", name, optimized(inst, name))
				}
			}
		})
	}
}

// bigFunctionWAT is a module whose export "f" is one function of n turns
// of memory accesses, a call, a trap's site, a v128 constant and a branch
// over a call through Go, whose code is past a megabyte for n of some
// thousands: its v128 constants, after its code, are far from their
// loads, and its exits' and traps' stubs far from their sites.
func bigFunctionWAT(n int) string {
	var b strings.Builder
	b.WriteString(`(module
  (import "env" "g" (func $host (param i32) (result i32)))
  (memory 1)
  (func $g (param i32) (result i32) (i32.add (local.get 0) (i32.const 3)))
  (func (export "f") (param $p i32) (result i32) (local $a i32) (local $v v128)
`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `    (local.set $a (i32.add (local.get $a) (i32.load offset=%d (local.get $p))))
    (i32.store offset=%d (local.get $p) (i32.div_u (local.get $a) (i32.const %d)))
    (local.set $v (i32x4.add (local.get $v) (v128.const i32x4 %d 1 2 3)))
    (if (i32.eqz (local.get $a)) (then (local.set $a (call $host (call $g (local.get $a))))))
`, 4*(i%1000), 4*(i%1000+1), i+1, i)
	}
	b.WriteString(`    (i32.add (local.get $a) (i32.add (i32x4.extract_lane 0 (local.get $v)) (i32x4.extract_lane 3 (local.get $v))))))`)
	return b.String()
}

// A function whose code is past a megabyte is compiled, in both tiers,
// and runs as the interpreter does.
func TestJITLargeFunction(t *testing.T) {
	if testing.Short() {
		t.Skip("a large function's compiles")
	}
	src := bigFunctionWAT(8000)
	host := NewHostFunction(module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}, Results: []module.ValueType{module.I32}}), "g",
		func(_ context.Context, _ *Instance, args, results []Val) error {
			results[0] = Val{N: uint64(uint32(args[0].N) + 1)}
			return nil
		})
	interp := thresholdInstance(t, src, math.MaxInt32, Extern{Kind: module.ExternFunc, Func: host})
	for _, opt := range []bool{false, true} {
		inst := thresholdInstance(t, src, 0, Extern{Kind: module.ExternFunc, Func: host})
		inst.Code.setOSRCalls(0)
		if opt {
			inst.Code.SetOptThreshold(0)
			inst.Code.SetOptBackground(false)
		} else {
			inst.Code.SetOptThreshold(-1)
		}
		for _, arg := range []uint64{0, 64, 65536 - 4004, 65536} {
			sameOutcome(t, interp, inst, "f", arg)
		}
		ext, _ := inst.Export("f")
		nat := inst.Code.get(ext.Func.Index).native.Load()
		if nat == nil || nat.opt != opt {
			t.Fatalf("opt %v: f's native code %v", opt, nat)
		}
		size := nat.end - nat.base
		t.Logf("opt %v: %d bytes of code", opt, size)
		if size <= 1<<20 {
			t.Fatalf("f's code is %d bytes, want past a megabyte", size)
		}
	}
}
