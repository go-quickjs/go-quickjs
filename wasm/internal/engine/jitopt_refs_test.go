//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// optRefsWAT has references in every place the optimizing tier keeps them
// -- parameters, locals, results, the operand stack across blocks, loops,
// ifs and branches, globals, tables -- and calls and tail calls of every
// kind with them. $mk makes i31s, a GC instruction the tier refuses, so
// that its references come from baseline code.
const optRefsWAT = `(module
  (type $t (func (param i32) (result i32)))
  (type $r (func (param funcref i32) (result funcref i32)))
  (type $e (func (param eqref eqref) (result i32)))
  (import "m" "host" (func $host (type $t)))
  (table $tab 8 funcref)
  (table $tab64 i64 4 funcref)
  (elem (table $tab) (i32.const 0) func $add1 $mul3 $pass $host $refs $add1)
  (elem declare func $tdeep $rdeep $sel $ideep)
  (global $g (mut funcref) (ref.null func))
  (global $ge (mut eqref) (ref.null eq))
  (func $mk (param i32) (result eqref) (ref.i31 (local.get 0)))
  (func $add1 (type $t) (i32.add (local.get 0) (i32.const 1)))
  (func $mul3 (type $t) (i32.mul (local.get 0) (i32.const 3)))
  (func $refs (type $t) (local funcref)
    (if (result i32) (ref.is_null (local.get 1))
      (then (i32.sub (local.get 0) (i32.const 7)))
      (else (i32.const 0))))
  (func $pass (type $r) (local.get 0) (i32.add (local.get 1) (i32.const 1)))
  (func $calls (param $f funcref) (param $x i32) (result i32)
    (if (result i32) (ref.is_null (local.get $f))
      (then (i32.const -1))
      (else (call_ref $t (local.get $x) (ref.cast (ref $t) (local.get $f))))))
  (func $eq (type $e) (ref.eq (local.get 0) (local.get 1)))

  ;; Locals, globals, select and the null tests.
  (func (export "basic") (param $i i32) (result i32)
    (local $a funcref) (local $b funcref) (local $acc i32)
    (local.set $a (table.get $tab (local.get $i)))
    (local.set $b (select (result funcref) (local.get $a) (ref.func $mul3) (i32.and (local.get $i) (i32.const 1))))
    (global.set $g (local.get $b))
    (local.set $acc (i32.add (ref.is_null (local.get $a)) (i32.mul (ref.is_null (global.get $g)) (i32.const 2))))
    (local.set $acc (i32.add (local.get $acc) (i32.mul (call $calls (global.get $g) (local.get $i)) (i32.const 4))))
    (local.set $acc (i32.add (local.get $acc) (i32.mul (table.size $tab) (i32.const 1000))))
    (i32.add (local.get $acc) (i32.wrap_i64 (i64.mul (table.size $tab64) (i64.const 100000)))))

  ;; ref.eq of i31s made by baseline code, and through a call.
  (func (export "eq") (param $x i32) (param $y i32) (result i32)
    (local $a eqref)
    (local.set $a (call $mk (local.get $x)))
    (global.set $ge (call $mk (local.get $y)))
    (i32.add
      (i32.add (ref.eq (local.get $a) (global.get $ge)) (i32.mul (call $eq (local.get $a) (call $mk (local.get $x))) (i32.const 2)))
      (i32.mul (ref.eq (ref.null eq) (select (result eqref) (local.get $a) (ref.null eq) (local.get $y))) (i32.const 4))))

  ;; Branches with references: br_on_null, br_on_non_null, br_if, br_table
  ;; and a loop carrying one.
  (func (export "branches") (param $i i32) (param $n i32) (result i32)
    (local $f funcref) (local $k i32) (local $acc i32)
    (local.set $acc
      (block $b (result i32)
        (block $nn
          (local.set $f (br_on_null $nn (table.get $tab (local.get $i))))
          (br $b (i32.const 77)))
        (i32.const 5)))
    (local.set $acc (i32.add (local.get $acc) (i32.mul (ref.is_null (local.get $f)) (i32.const 10))))
    (local.set $acc (i32.add (local.get $acc)
      (block $got (result i32)
        (local.set $f
          (block $nul (result funcref)
            (br_on_non_null $nul (table.get $tab (local.get $i)))
            (br $got (i32.const 100))))
        (call $calls (local.get $f) (local.get $i)))))
    (local.set $f
      (block $out (result funcref)
        (drop (block $d0 (result funcref)
          (drop (block $d1 (result funcref)
            (ref.null func)
            (br_table $d0 $d1 $out (ref.func $add1) (local.get $i))))
          (br $out (ref.func $mul3))))
        (ref.null func)))
    (local.set $acc (i32.add (local.get $acc) (call $calls (local.get $f) (i32.const 9))))
    (table.get $tab (i32.const 0))
    (loop $l (param funcref) (result funcref)
      (local.set $k (i32.add (local.get $k) (i32.const 1)))
      (br_if $l
        (select (result funcref) (ref.func $mul3) (ref.func $add1) (i32.and (local.get $k) (i32.const 1)))
        (i32.lt_u (local.get $k) (local.get $n)))
      (drop))
    (local.set $f)
    (i32.add (local.get $acc) (call $calls (local.get $f) (i32.const 3))))

  ;; Calls with references: directly, through a table and by reference, and
  ;; one of several results; table.set traps past the end.
  (func (export "calls") (param $i i32) (result i32)
    (local $f funcref) (local $x i32)
    (call $pass (table.get $tab (local.get $i)) (local.get $i))
    (local.set $x)
    (local.set $f)
    (local.set $x (i32.add (local.get $x) (call $calls (local.get $f) (i32.const 11))))
    (call_indirect $tab (type $r) (local.get $f) (local.get $x) (i32.const 2))
    (local.set $x)
    (local.set $f)
    (local.set $x (i32.add (local.get $x) (call $calls (local.get $f) (i32.const 13))))
    (local.set $x (i32.add (local.get $x) (call_ref $t (local.get $x) (ref.func $add1))))
    (table.set $tab (local.get $i) (ref.func $mul3))
    (local.set $x (i32.add (local.get $x) (call_indirect $tab (type $t) (local.get $x) (local.get $i))))
    (table.set $tab (local.get $i) (table.get $tab (i32.const 5)))
    (local.get $x))

  ;; Tail calls of every kind, deep, natively and through Go.
  (func $tdeep (param $n i32) (param $f funcref) (result i32)
    (if (result i32) (i32.eqz (local.get $n))
      (then (call $calls (local.get $f) (i32.const 40)))
      (else (return_call $tdeep (i32.sub (local.get $n) (i32.const 1)) (local.get $f)))))
  (func $rdeep (type $t)
    (if (result i32) (i32.eqz (local.get 0))
      (then (i32.const 43))
      (else (return_call_ref $t (i32.sub (local.get 0) (i32.const 1)) (ref.func $rdeep)))))
  (func $ideep (type $t)
    (if (result i32) (i32.eqz (local.get 0))
      (then (i32.const 44))
      (else (return_call_indirect $tab (type $t) (i32.sub (local.get 0) (i32.const 1)) (i32.const 6)))))
  (func (export "tails") (param $n i32) (result i32)
    (table.set $tab (i32.const 6) (ref.func $ideep))
    (i32.add (call $tdeep (local.get $n) (ref.func $add1))
      (i32.add (call $rdeep (local.get $n)) (call $ideep (local.get $n)))))
  (func (export "tailhost") (param $x i32) (result i32)
    (return_call $host (local.get $x)))
  (func (export "tailtable") (param $i i32) (param $x i32) (result i32)
    (return_call_indirect $tab (type $t) (local.get $x) (local.get $i)))
  (func (export "tailref") (param $i i32) (param $x i32) (result i32)
    (return_call_ref $t (local.get $x)
      (select (result (ref null $t)) (ref.func $mul3) (ref.null $t) (i32.and (local.get $i) (i32.const 1)))))
  (func $sel (param $f funcref) (param $g funcref) (param $c i32) (result funcref)
    (select (result funcref) (local.get $f) (local.get $g) (local.get $c)))
  (func (export "tailrefs") (param $c i32) (result i32)
    (call $calls (call $tsel (ref.func $add1) (ref.func $mul3) (local.get $c)) (i32.const 6)))
  (func $tsel (param funcref funcref i32) (result funcref)
    (return_call $sel (local.get 1) (local.get 0) (local.get 2))))`

// The optimizing tier keeps references in their frame slots, through the
// barrier while the collector marks, and makes calls and tail calls of
// every kind with them: its results, traps -- every frame's offset -- and
// tables are the interpreter's, and the functions are optimized.
func TestOptRefs(t *testing.T) {
	for _, mode := range []string{"idle", "marking", "clobbered"} {
		t.Run(mode, func(t *testing.T) {
			if mode != "idle" {
				jitMarkingForTest(t, mode == "clobbered")
			}
			testOptRefs(t)
		})
	}
}

func testOptRefs(t *testing.T) {
	ft := module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}, Results: []module.ValueType{module.I32}})
	host := NewHostFunction(ft, "host", func(_ context.Context, _ *Instance, args, res []Val) error {
		res[0] = Val{N: uint64(uint32(args[0].N) ^ 0x55)}
		return nil
	})
	imp := Extern{Kind: module.ExternFunc, Func: host}
	interp := thresholdInstance(t, optRefsWAT, math.MaxInt32, imp)
	inst := thresholdInstance(t, optRefsWAT, 0, imp)
	inst.Code.setOSRCalls(0)
	inst.Code.SetOptThreshold(0)
	inst.Code.SetOptBackground(false)
	same := func(name string, args ...uint64) {
		t.Helper()
		want, werr := call(t, interp, name, args...)
		got, gerr := call(t, inst, name, args...)
		if fmt.Sprint(werr) != fmt.Sprint(gerr) || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s%v = %v, %v; want %v, %v", name, args, got, gerr, want, werr)
		}
		var wt, gt *Trap
		if errors.As(werr, &wt) && errors.As(gerr, &gt) {
			if len(wt.Frames) != len(gt.Frames) {
				t.Fatalf("%s%v: trap frames %+v, want %+v", name, args, gt.Frames, wt.Frames)
			}
			for k := range wt.Frames {
				if wt.Frames[k].Func != gt.Frames[k].Func || wt.Frames[k].Offset != gt.Frames[k].Offset {
					t.Fatalf("%s%v: trap frames %+v, want %+v", name, args, gt.Frames, wt.Frames)
				}
			}
		}
	}
	for i := range uint64(10) {
		same("basic", i)
		same("eq", i, i&1)
		same("eq", i, i)
		for _, n := range []uint64{0, 1, 2, 7} {
			same("branches", i, n)
		}
		same("calls", i)
		same("tailtable", i, 5)
		same("tailref", i, 5)
	}
	same("basic", 0xffffffff)
	same("calls", 1<<31)
	same("tailhost", 9)
	same("tailrefs", 0)
	same("tailrefs", 1)
	for _, n := range []uint64{0, 1, 5, 100000} {
		same("tails", n)
	}
	for _, name := range []string{"basic", "eq", "branches", "calls", "tails", "tailhost", "tailtable", "tailref", "tailrefs"} {
		if !optimized(inst, name) {
			t.Fatalf("%s not optimized", name)
		}
	}
	if n := countExits(func() { call(t, inst, "tails", 100000) }); n > 1000 {
		t.Fatalf("%d exits in 300,000 tail calls", n)
	}
}
