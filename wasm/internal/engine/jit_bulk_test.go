//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"bytes"
	"fmt"
	"testing"
)

// bulkWAT has memory.size, memory.copy and memory.fill of an i32 memory,
// an i64 memory and a shared memory, and copies between them; table.size
// and table.fill of an i32 table and an i64 one; and a function holding
// references around a copy and a fill, whose references stay where they
// were.
const bulkWAT = `(module
  (memory $a 1 4)
  (memory $b i64 1 4)
  (memory $c 1 4 shared)
  (table $t 4 20 funcref)
  (table $u i64 4 20 funcref)
  (func $f (result i32) (i32.const 7))
  (func $g (result i32) (i32.const 9))
  (elem declare func $f $g)
  (func (export "init") (param $n i32)
    (local $i i32)
    (loop $l
      (i32.store8 $a (local.get $i) (i32.add (i32.mul (local.get $i) (i32.const 7)) (i32.const 1)))
      (i64.store8 $b (i64.extend_i32_u (local.get $i)) (i64.extend_i32_u (i32.mul (local.get $i) (i32.const 13))))
      (i32.store8 $c (local.get $i) (i32.mul (local.get $i) (i32.const 5)))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n)))))
  (func (export "copyA") (param i32 i32 i32) (memory.copy $a $a (local.get 0) (local.get 1) (local.get 2)))
  (func (export "copyB") (param i64 i64 i64) (memory.copy $b $b (local.get 0) (local.get 1) (local.get 2)))
  (func (export "copyC") (param i32 i32 i32) (memory.copy $c $c (local.get 0) (local.get 1) (local.get 2)))
  (func (export "copyAC") (param i32 i32 i32) (memory.copy $a $c (local.get 0) (local.get 1) (local.get 2)))
  (func (export "copyCA") (param i32 i32 i32) (memory.copy $c $a (local.get 0) (local.get 1) (local.get 2)))
  (func (export "copyBA") (param i64 i32 i32) (memory.copy $b $a (local.get 0) (local.get 1) (local.get 2)))
  (func (export "copyAB") (param i32 i64 i32) (memory.copy $a $b (local.get 0) (local.get 1) (local.get 2)))
  (func (export "fillA") (param i32 i32 i32) (memory.fill $a (local.get 0) (local.get 1) (local.get 2)))
  (func (export "fillB") (param i64 i32 i64) (memory.fill $b (local.get 0) (local.get 1) (local.get 2)))
  (func (export "fillC") (param i32 i32 i32) (memory.fill $c (local.get 0) (local.get 1) (local.get 2)))
  (func (export "sizes") (result i64)
    (i64.add
      (i64.add (i64.extend_i32_u (memory.size $a)) (i64.shl (memory.size $b) (i64.const 8)))
      (i64.add (i64.shl (i64.extend_i32_u (memory.size $c)) (i64.const 16))
        (i64.add (i64.shl (i64.extend_i32_u (table.size $t)) (i64.const 24))
          (i64.shl (table.size $u) (i64.const 32))))))
  (func (export "grow") (param i32) (result i32)
    (i32.add
      (i32.add (memory.grow $a (local.get 0)) (i32.wrap_i64 (memory.grow $b (i64.extend_i32_u (local.get 0)))))
      (i32.add (memory.grow $c (local.get 0))
        (i32.add (table.grow $t (ref.null func) (local.get 0))
          (i32.wrap_i64 (table.grow $u (ref.func $f) (i64.extend_i32_u (local.get 0))))))))
  (func (export "tfill") (param i32 i32 i32)
    (table.fill $t (local.get 0)
      (select (result funcref) (ref.func $f) (ref.null func) (i32.eqz (local.get 1))) (local.get 2)))
  (func (export "tfill64") (param i64 i32 i64)
    (table.fill $u (local.get 0)
      (select (result funcref) (ref.func $g) (ref.null func) (i32.eqz (local.get 1))) (local.get 2)))
  (func (export "tsum") (result i32)
    (local $i i32) (local $acc i32)
    (block $done
      (loop $l
        (br_if $done (i32.ge_u (local.get $i) (table.size $t)))
        (local.set $acc (i32.add (i32.mul (local.get $acc) (i32.const 3))
          (if (result i32) (ref.is_null (table.get $t (local.get $i)))
            (then (i32.const 1))
            (else (i32.add (i32.const 2) (call_ref $ft (ref.cast (ref $ft) (table.get $t (local.get $i)))))))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $l)))
    (local.set $i (i32.const 0))
    (block $done
      (loop $l
        (br_if $done (i64.ge_u (i64.extend_i32_u (local.get $i)) (table.size $u)))
        (local.set $acc (i32.add (i32.mul (local.get $acc) (i32.const 5))
          (if (result i32) (ref.is_null (table.get $u (i64.extend_i32_u (local.get $i))))
            (then (i32.const 1))
            (else (i32.const 4)))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $l)))
    (local.get $acc))
  (type $ft (func (result i32)))
  (func (export "refs") (param $d i32) (param $s i32) (param $n i32) (result i32)
    (local $r funcref) (local $e externref)
    (local.set $r (ref.func $g))
    (memory.copy $a $a (local.get $d) (local.get $s) (local.get $n))
    (memory.fill $c (local.get $s) (local.get $d) (local.get $n))
    (i32.add (i32.add (ref.is_null (local.get $e)) (call_ref $ft (ref.cast (ref $ft) (local.get $r))))
      (i32.load8_u $a (local.get $d)))))`

// The baseline code's memory.size, memory.copy, memory.fill, table.size
// and table.fill, of every kind of memory and table, make no exit, and
// leave results, traps and memories as the interpreter's, with the
// collector idle and marking.
func TestJITBulkBaseline(t *testing.T) {
	for _, mode := range []string{"idle", "marking", "clobbered"} {
		t.Run(mode, func(t *testing.T) {
			if mode != "idle" {
				jitMarkingForTest(t, mode == "clobbered")
			}
			testJITBulkBaseline(t)
		})
	}
}

func testJITBulkBaseline(t *testing.T) {
	interp := interpretedInstance(t, bulkWAT)
	inst := thresholdInstance(t, bulkWAT, 0)
	inst.Code.setOSRCalls(0)
	inst.Code.SetOptThreshold(-1)
	same := func(name string, args ...uint64) {
		t.Helper()
		want, werr := call(t, interp, name, args...)
		got, gerr := call(t, inst, name, args...)
		if fmt.Sprint(werr) != fmt.Sprint(gerr) || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s%v = %v, %v; want %v, %v", name, args, got, gerr, want, werr)
		}
		for i := range inst.Memories {
			if !bytes.Equal(inst.Memories[i].bytes(), interp.Memories[i].bytes()) {
				t.Fatalf("%s%v: memory %d differs", name, args, i)
			}
		}
	}
	same("init", 65536)
	same("sizes")
	const size = 65536
	for _, n := range []uint64{0, 1, 3, 7, 8, 9, 15, 16, 17, 31, 32, 33, 100, 4097} {
		for _, d := range []int64{-33, -9, -8, -1, 0, 1, 7, 8, 9, 32, 64} {
			for _, f := range []string{"copyA", "copyB", "copyC", "copyAC", "copyCA", "copyBA", "copyAB"} {
				same(f, uint64(1000+d), 1000, n)
				same(f, 1000, uint64(1000+d), n)
			}
		}
		for _, f := range []string{"copyA", "copyB", "copyC", "copyAC", "copyBA"} {
			same(f, size-n, 0, n)
			same(f, 0, size-n, n)
			same(f, size-n+1, 0, n)
			same(f, 0, size-n+1, n)
		}
		for _, f := range []string{"fillA", "fillB", "fillC"} {
			same(f, 333, n*5+1, n)
			same(f, size-n, 0xab, n)
			same(f, size-n+1, 0xab, n)
		}
		same("refs", 2000, 2003, n)
		same("refs", size-n, 8, n)
		same("refs", size-n+1, 8, n)
	}
	for _, f := range []string{"copyA", "copyC", "copyAC"} {
		same(f, 0, 0, 0xffffffff)
		same(f, 0xffffffff, 0, 1)
		same(f, 0, 0xffffffff, 1)
		same(f, size, size, 0)
		same(f, size+1, size, 0)
	}
	for _, f := range []string{"copyB", "copyBA", "copyAB"} {
		same(f, 0, 0, 0xffffffff)
		same(f, 1<<63, 0, 1)
		same(f, 0xffffffffffffffff, 1, 1)
		same(f, 1, 0xffffffffffffffff, 2)
		same(f, size, size, 0)
	}
	same("fillA", 0xffffffff, 0, 1)
	same("fillB", 0xffffffffffffffff, 0, 2)
	same("fillB", 2, 0, 0xffffffffffffffff)
	same("fillC", 0, 0, 0xffffffff)
	same("fillC", size, 1, 0)
	for _, tf := range []string{"tfill", "tfill64"} {
		for _, d := range []uint64{0, 1, 3, 4, 5} {
			for _, n := range []uint64{0, 1, 3, 4} {
				same(tf, d, d&1, n)
				same("tsum")
			}
		}
	}
	same("tfill64", 0xffffffffffffffff, 0, 2)
	same("tfill", 0xffffffff, 0, 2)
	same("grow", 1)
	same("sizes")
	for _, f := range []string{"copyA", "copyB", "copyC"} {
		same(f, size, 2*size-100, 100)
		same(f, 2*size-50, 2*size-100, 51)
	}
	same("fillC", 2*size-10, 3, 10)
	same("tfill", 3, 1, 3)
	same("tsum")
	same("grow", 0)
	same("sizes")
	for _, f := range []string{"init", "copyA", "fillB", "sizes", "tfill", "refs"} {
		if !nativeRan(inst, f) {
			t.Fatalf("%s has no native code", f)
		}
	}
	if n := countExits(func() {
		for range 100 {
			call(t, inst, "copyA", 100, 300, 64)
			call(t, inst, "copyBA", 100, 300, 64)
			call(t, inst, "fillC", 100, 300, 64)
			call(t, inst, "sizes")
			call(t, inst, "tfill", 1, 0, 2)
		}
	}); n > 0 {
		t.Fatalf("%d exits in calls of memory.size, memory.copy, memory.fill, table.size and table.fill", n)
	}
}
