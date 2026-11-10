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

// indirectWAT calls through a table and through references, and makes tail
// calls of both kinds, with every kind of callee: the instance's own
// functions of the call's type, of another type, holding references,
// taking v128s, imports of a host function and of another instance's,
// null, and past the table.
const indirectWAT = `(module
  (type $t (func (param i32) (result i32)))
  (type $u (func (param i32) (result i64)))
  (type $v (func (param v128 i32) (result v128)))
  (type $r (func (param externref i32) (result externref i32)))
  (import "m" "host" (func $host (type $t)))
  (import "m" "other" (func $other (type $t)))
  (table $tab 12 funcref)
  (table $tab64 i64 4 funcref)
  (elem (table $tab) (i32.const 0) func $add1 $mul3 $wide $host $other $refs $deep $add1 $vec $tdeep $pass)
  (elem (table $tab64) (i64.const 0) func $add1 $mul3)
  (elem declare func $add1 $mul3 $wide $refs $deep $vec $tdeep $rdeep $pass)
  (func $add1 (type $t) (i32.add (local.get 0) (i32.const 1)))
  (func $mul3 (type $t) (i32.mul (local.get 0) (i32.const 3)))
  (func $wide (type $u) (i64.extend_i32_u (local.get 0)))
  (func $vec (type $v) (i32x4.add (local.get 0) (i32x4.splat (local.get 1))))
  (func $pass (type $r) (local.get 0) (i32.add (local.get 1) (i32.const 1)))
  (func $refs (type $t) (local externref)
    (if (ref.is_null (local.get 1)) (then (return (i32.sub (local.get 0) (i32.const 7)))))
    (i32.const 0))
  (func $deep (type $t)
    (if (result i32) (i32.eqz (local.get 0))
      (then (i32.const 0))
      (else (i32.add (i32.const 1)
        (call_indirect $tab (type $t) (i32.sub (local.get 0) (i32.const 1)) (i32.const 6))))))
  (func $tdeep (type $t)
    (if (result i32) (i32.eqz (local.get 0))
      (then (i32.const 42))
      (else (return_call_indirect $tab (type $t) (i32.sub (local.get 0) (i32.const 1)) (i32.const 9)))))
  (func $rdeep (type $t)
    (if (result i32) (i32.eqz (local.get 0))
      (then (i32.const 43))
      (else (return_call_ref $t (i32.sub (local.get 0) (i32.const 1)) (ref.func $rdeep)))))
  (func (export "dispatch") (param $i i32) (param $x i32) (result i32)
    (call_indirect $tab (type $t) (local.get $x) (local.get $i)))
  (func (export "dispatch64") (param $i i64) (param $x i32) (result i32)
    (call_indirect $tab64 (type $t) (local.get $x) (local.get $i)))
  (func (export "tail") (param $i i32) (param $x i32) (result i32)
    (return_call_indirect $tab (type $t) (local.get $x) (local.get $i)))
  (func (export "byref") (param $i i32) (param $x i32) (result i32)
    (local $r (ref null $t))
    (local.set $r (ref.cast (ref null $t) (table.get $tab (local.get $i))))
    (i32.add (call_ref $t (local.get $x) (local.get $r))
      (ref.is_null (local.get $r))))
  (func (export "tailref") (param $i i32) (param $x i32) (result i32)
    (return_call_ref $t (local.get $x) (ref.cast (ref null $t) (table.get $tab (local.get $i)))))
  (func (export "refpass") (param $x i32) (param $i i32) (result i32)
    (local $r i32)
    (call_indirect $tab (type $r) (extern.convert_any (ref.i31 (local.get $x))) (local.get $x) (local.get $i))
    local.set $r
    any.convert_extern
    ref.cast (ref i31)
    i31.get_s
    local.get $r
    i32.add)
  (func (export "refpassref") (param $x i32) (result i32)
    (local $r i32)
    (call_ref $r (extern.convert_any (ref.i31 (local.get $x))) (local.get $x) (ref.func $pass))
    local.set $r
    any.convert_extern
    ref.cast (ref i31)
    i31.get_s
    local.get $r
    i32.add)
  (func (export "vec") (param $x i32) (result i64)
    (i64x2.extract_lane 1 (call_indirect $tab (type $v) (v128.const i32x4 1 2 3 4) (local.get $x) (i32.const 8))))
  (func (export "deep") (param i32) (result i32)
    (call_indirect $tab (type $t) (local.get 0) (i32.const 6)))
  (func (export "tdeep") (param i32) (result i32)
    (call_indirect $tab (type $t) (local.get 0) (i32.const 9)))
  (func (export "rdeep") (param i32) (result i32)
    (call_ref $t (local.get 0) (ref.func $rdeep)))
  (func (export "fast") (param $n i32) (result i32)
    (local $k i32) (local $acc i32)
    (loop $l
      (local.set $acc (call_indirect $tab (type $t) (local.get $acc) (i32.and (local.get $k) (i32.const 1))))
      (local.set $acc (call_ref $t (local.get $acc) (select (result (ref $t)) (ref.func $add1) (ref.func $mul3) (i32.and (local.get $k) (i32.const 2)))))
      (br_if $l (i32.lt_u (local.tee $k (i32.add (local.get $k) (i32.const 1))) (local.get $n))))
    (local.get $acc))
  (func (export "mix") (param $n i32) (result i32)
    (local $k i32) (local $acc i32)
    (loop $l
      (local.set $acc (i32.add (local.get $acc)
        (call_indirect $tab (type $t) (i32.add (local.get $acc) (local.get $k))
          (select (i32.const 7) (i32.rem_u (local.get $k) (i32.const 6))
            (i32.eq (i32.rem_u (local.get $k) (i32.const 6)) (i32.const 2))))))
      (br_if $l (i32.lt_u (local.tee $k (i32.add (local.get $k) (i32.const 1))) (local.get $n))))
    (local.get $acc)))`

// The baseline code's calls through a table and through references, and
// its tail calls of both, are native for the instance's own functions of
// the call's type with native code, and go through Go for every other
// callee, whose traps -- with every frame's offset -- and results are the
// interpreter's, with the collector idle and marking.
func TestJITIndirectBaseline(t *testing.T) {
	for _, mode := range []string{"idle", "marking", "clobbered"} {
		t.Run(mode, func(t *testing.T) {
			if mode != "idle" {
				jitMarkingForTest(t, mode == "clobbered")
			}
			testJITIndirectBaseline(t)
		})
	}
}

func testJITIndirectBaseline(t *testing.T) {
	src := indirectWAT
	ft := module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}, Results: []module.ValueType{module.I32}})
	host := NewHostFunction(ft, "host", func(_ context.Context, _ *Instance, args, res []Val) error {
		res[0] = Val{N: uint64(uint32(args[0].N) ^ 0x55)}
		return nil
	})
	otherInst := interpretedInstance(t, `(module (func (export "f") (param i32) (result i32) (i32.sub (local.get 0) (i32.const 100))))`)
	other, _ := otherInst.Export("f")
	imports := []Extern{{Kind: module.ExternFunc, Func: host}, {Kind: module.ExternFunc, Func: other.Func}}
	interp := thresholdInstance(t, src, math.MaxInt32, imports...)
	inst := thresholdInstance(t, src, 0, imports...)
	inst.Code.setOSRCalls(0)
	inst.Code.SetOptThreshold(-1)
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
	for i := range uint64(14) {
		for _, x := range []uint64{0, 5, 1000, 0xffffffff} {
			if x == 0xffffffff && (i == 6 || i == 9) {
				// $deep and $tdeep recurse x times.
				continue
			}
			same("dispatch", i, x)
			same("tail", i, x)
			same("byref", i, x)
			same("tailref", i, x)
		}
	}
	for _, i := range []uint64{0xffffffff, 1 << 31} {
		same("dispatch", i, 1)
		same("tail", i, 1)
		same("byref", i, 1)
	}
	for _, i := range []uint64{0, 1, 2, 3, 4, 1 << 32, 1 << 63} {
		same("dispatch64", i, 9)
	}
	same("vec", 5)
	for i := range uint64(12) {
		same("refpass", 77, i)
	}
	same("refpassref", 5)
	if n := countExits(func() {
		call(t, inst, "refpass", 3, 10)
		call(t, inst, "refpassref", 3)
	}); n > 0 {
		t.Fatalf("%d exits in calls of references through a table and a reference", n)
	}
	same("mix", 5000)
	same("fast", 10)
	same("fast", 100000)
	if n := countExits(func() { call(t, inst, "fast", 100000) }); n > 100 {
		t.Fatalf("%d exits in 200,000 calls through a table and references", n)
	}
	if n := countExits(func() { call(t, inst, "vec", 7) }); n > 0 {
		t.Fatalf("%d exits in a call of v128s through a table", n)
	}
	same("deep", 1000)
	same("deep", 1_000_000)
	same("tdeep", 1_000_000)
	same("rdeep", 1_000_000)
	if n := countExits(func() { call(t, inst, "tdeep", 1_000_000) }); n > 1000 {
		t.Fatalf("%d exits in 1,000,000 tail calls through a table", n)
	}
	if n := countExits(func() { call(t, inst, "rdeep", 1_000_000) }); n > 1000 {
		t.Fatalf("%d exits in 1,000,000 tail calls through references", n)
	}
}
