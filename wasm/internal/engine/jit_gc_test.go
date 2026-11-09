//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"os"
	osexec "os/exec"
	"runtime"
	"sync/atomic"
	"testing"
)

// gcWAT exercises the GC instructions: native field and element reads and
// writes of every storage type, length, i31s, tests, casts and branches on
// casts against abstract and defined types, and the allocations and range
// instructions Go runs.
const gcWAT = `(module
  (type $pt (sub (struct (field (mut i32)))))
  (type $pt3 (sub $pt (struct (field (mut i32)) (field (mut i64)))))
  (type $all (struct (field (mut i8)) (field (mut i16)) (field (mut i32)) (field (mut i64))
    (field (mut f32)) (field (mut f64)) (field (mut anyref)) (field (mut (ref null $all)))))
  (type $node (struct (field $next (mut (ref null $node))) (field $v (mut i32))))
  (type $a8 (array (mut i8)))
  (type $a16 (array (mut i16)))
  (type $a32 (array (mut i32)))
  (type $a64 (array (mut i64)))
  (type $af32 (array (mut f32)))
  (type $af64 (array (mut f64)))
  (type $aref (array (mut anyref)))
  (type $f (func (param i32) (result i32)))
  (data $d "\01\02\03\04\05\06\07\08\ff\fe")
  (elem $e anyref (ref.i31 (i32.const 7)) (ref.null any) (ref.i31 (i32.const 9)))
  (global $keep (mut (ref null $node)) (ref.null $node))
  (func (export "fields") (param $x i32) (result i32)
    (local $s (ref null $all))
    (local.set $s (struct.new $all (local.get $x) (local.get $x) (local.get $x)
      (i64.extend_i32_s (local.get $x)) (f32.convert_i32_s (local.get $x)) (f64.convert_i32_s (local.get $x))
      (ref.i31 (local.get $x)) (ref.null $all)))
    (struct.set $all 7 (local.get $s) (local.get $s))
    (struct.set $all 0 (local.get $s) (i32.add (struct.get_u $all 0 (local.get $s)) (i32.const 1)))
    (i32.add (i32.add (i32.add (struct.get_s $all 0 (local.get $s)) (struct.get_u $all 0 (local.get $s)))
      (i32.add (struct.get_s $all 1 (local.get $s)) (struct.get_u $all 1 (struct.get $all 7 (local.get $s)))))
      (i32.add (i32.add (struct.get $all 2 (local.get $s)) (i32.wrap_i64 (struct.get $all 3 (local.get $s))))
        (i32.add (i32.trunc_f32_s (struct.get $all 4 (local.get $s))) (i32.trunc_f64_s (struct.get $all 5 (local.get $s)))))))
  (func (export "fieldref") (param $x i32) (result i32)
    (i31.get_s (ref.cast (ref i31) (struct.get $all 6
      (struct.new $all (i32.const 0) (i32.const 0) (i32.const 0) (i64.const 0) (f32.const 0) (f64.const 0)
        (ref.i31 (local.get $x)) (ref.null $all))))))
  (func (export "defaults") (result i32)
    (local $s (ref $all))
    (local.set $s (struct.new_default $all))
    (i32.add (struct.get $all 2 (local.get $s)) (ref.is_null (struct.get $all 6 (local.get $s)))))
  (func (export "arrays") (param $n i32) (param $i i32) (result i64)
    (local $a8 (ref $a8)) (local $a16 (ref $a16)) (local $a32 (ref $a32)) (local $a64 (ref $a64))
    (local $f32 (ref $af32)) (local $f64 (ref $af64))
    (local.set $a8 (array.new $a8 (i32.const -3) (local.get $n)))
    (local.set $a16 (array.new $a16 (i32.const -300) (local.get $n)))
    (local.set $a32 (array.new_default $a32 (local.get $n)))
    (local.set $a64 (array.new $a64 (i64.const -5) (local.get $n)))
    (local.set $f32 (array.new $af32 (f32.const 1.5) (local.get $n)))
    (local.set $f64 (array.new $af64 (f64.const 2.25) (local.get $n)))
    (array.set $a8 (local.get $a8) (local.get $i) (i32.const 200))
    (array.set $a16 (local.get $a16) (local.get $i) (i32.const 40000))
    (array.set $a32 (local.get $a32) (local.get $i) (i32.const -7))
    (array.set $a64 (local.get $a64) (local.get $i) (i64.const 0x1234567890))
    (array.set $af32 (local.get $f32) (local.get $i) (f32.const -2.5))
    (array.set $af64 (local.get $f64) (local.get $i) (f64.const 8.5))
    (i64.add
      (i64.add (i64.extend_i32_u (array.get_u $a8 (local.get $a8) (local.get $i)))
        (i64.extend_i32_s (array.get_s $a8 (local.get $a8) (i32.const 0))))
      (i64.add
        (i64.add (i64.extend_i32_u (array.get_u $a16 (local.get $a16) (local.get $i)))
          (i64.extend_i32_s (array.get_s $a16 (local.get $a16) (i32.const 0))))
        (i64.add
          (i64.add (i64.extend_i32_s (array.get $a32 (local.get $a32) (local.get $i))) (array.get $a64 (local.get $a64) (local.get $i)))
          (i64.add (i64.extend_i32_u (array.len (local.get $a64)))
            (i64.add (i64.trunc_f32_s (array.get $af32 (local.get $f32) (local.get $i)))
              (i64.trunc_f64_s (array.get $af64 (local.get $f64) (local.get $i)))))))))
  (func (export "refarray") (param $n i32) (param $i i32) (result i32)
    (local $a (ref $aref))
    (local.set $a (array.new_default $aref (local.get $n)))
    (array.set $aref (local.get $a) (local.get $i) (ref.i31 (local.get $n)))
    (array.fill $aref (local.get $a) (i32.const 0) (ref.i31 (i32.const 3)) (local.get $i))
    (i32.add (i31.get_u (ref.cast (ref i31) (array.get $aref (local.get $a) (local.get $i))))
      (ref.test (ref i31) (array.get $aref (local.get $a) (i32.const 0)))))
  (func (export "fixed") (param $x i32) (result i32)
    (local $a (ref $a32)) (local $b (ref $a32))
    (local.set $a (array.new_fixed $a32 4 (local.get $x) (i32.const 2) (i32.const 3) (i32.const 4)))
    (local.set $b (array.new_data $a32 $d (i32.const 0) (i32.const 2)))
    (array.copy $a32 $a32 (local.get $b) (i32.const 0) (local.get $a) (i32.const 1) (i32.const 2))
    (array.init_data $a32 $d (local.get $a) (i32.const 2) (i32.const 4) (i32.const 1))
    (i32.add (i32.add (array.get $a32 (local.get $b) (i32.const 0)) (array.get $a32 (local.get $b) (i32.const 1)))
      (i32.add (array.get $a32 (local.get $a) (i32.const 0)) (array.get $a32 (local.get $a) (i32.const 2)))))
  (func (export "elems") (param $i i32) (result i32)
    (local $a (ref $aref))
    (local.set $a (array.new_elem $aref $e (i32.const 0) (i32.const 3)))
    (array.init_elem $aref $e (local.get $a) (i32.const 1) (i32.const 2) (i32.const 1))
    (i31.get_s (ref.cast (ref i31) (array.get $aref (local.get $a) (local.get $i)))))
  (func (export "i31") (param $x i32) (result i32)
    (i32.add (i31.get_s (ref.i31 (local.get $x))) (i31.get_u (ref.i31 (local.get $x)))))
  (func $obj (param $k i32) (result anyref)
    (block $b (result anyref)
      (drop (br_if $b (ref.null any) (i32.eq (local.get $k) (i32.const 0))))
      (drop (br_if $b (ref.i31 (i32.const 1)) (i32.eq (local.get $k) (i32.const 1))))
      (drop (br_if $b (struct.new $pt (i32.const 2)) (i32.eq (local.get $k) (i32.const 2))))
      (drop (br_if $b (struct.new $pt3 (i32.const 3) (i64.const 3)) (i32.eq (local.get $k) (i32.const 3))))
      (drop (br_if $b (array.new_default $a32 (i32.const 1)) (i32.eq (local.get $k) (i32.const 4))))
      (any.convert_extern (extern.convert_any (struct.new_default $node)))))
  (func (export "tests") (param $k i32) (result i32)
    (local $o anyref)
    (local.set $o (call $obj (local.get $k)))
    (i32.or (i32.or (i32.or (ref.test anyref (local.get $o)) (i32.shl (ref.test (ref any) (local.get $o)) (i32.const 1)))
      (i32.or (i32.shl (ref.test (ref eq) (local.get $o)) (i32.const 2)) (i32.shl (ref.test (ref i31) (local.get $o)) (i32.const 3))))
      (i32.or (i32.or (i32.shl (ref.test (ref struct) (local.get $o)) (i32.const 4)) (i32.shl (ref.test (ref array) (local.get $o)) (i32.const 5)))
        (i32.or (i32.or (i32.shl (ref.test (ref $pt) (local.get $o)) (i32.const 6)) (i32.shl (ref.test (ref $pt3) (local.get $o)) (i32.const 7)))
          (i32.or (i32.shl (ref.test (ref null $pt3) (local.get $o)) (i32.const 8)) (i32.shl (ref.test (ref none) (local.get $o)) (i32.const 9)))))))
  (func (export "cast") (param $k i32) (result i32)
    (struct.get $pt 0 (ref.cast (ref $pt) (call $obj (local.get $k)))))
  (func (export "brcast") (param $k i32) (result i32)
    (drop (block $s (result (ref $pt))
      (drop (block $f (result anyref)
        (br_on_cast_fail $f anyref (ref $pt3) (call $obj (local.get $k)))
        (return (i32.wrap_i64 (struct.get $pt3 1)))))
      (br_on_cast $s anyref (ref $pt) (call $obj (local.get $k)))
      (return (i32.const -1))))
    (i32.const 100))
  (func (export "nulls") (param $k i32) (result i32)
    (if (i32.eq (local.get $k) (i32.const 0)) (then (return (struct.get $pt 0 (ref.null $pt)))))
    (if (i32.eq (local.get $k) (i32.const 1)) (then (return (array.len (ref.null $a32)))))
    (if (i32.eq (local.get $k) (i32.const 2)) (then (return (i31.get_s (ref.null i31)))))
    (if (i32.eq (local.get $k) (i32.const 3)) (then (struct.set $pt 0 (ref.null $pt) (i32.const 1))))
    (if (i32.eq (local.get $k) (i32.const 4)) (then (array.fill $a32 (ref.null $a32) (i32.const 0) (i32.const 0) (i32.const 0))))
    (if (i32.eq (local.get $k) (i32.const 5)) (then (return (array.get $a32 (array.new_default $a32 (i32.const 3)) (i32.const 3)))))
    (if (i32.eq (local.get $k) (i32.const 6)) (then (array.set $a32 (array.new_default $a32 (i32.const 3)) (i32.const -1) (i32.const 0))))
    (if (i32.eq (local.get $k) (i32.const 7)) (then (drop (array.new_default $a32 (i32.const -1)))))
    (i32.const 0))
  (func (export "list") (param $n i32) (result i32)
    (local $i i32) (local $head (ref null $node)) (local $sum i32)
    (loop $l
      (local.set $head (struct.new $node (local.get $head) (local.get $i)))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (global.set $keep (local.get $head))
    (block $done
      (loop $w
        (br_if $done (ref.is_null (local.get $head)))
        (local.set $sum (i32.add (local.get $sum) (struct.get $node $v (local.get $head))))
        (local.set $head (struct.get $node $next (local.get $head)))
        (br $w)))
    (local.get $sum))
  (func (export "relink") (param $n i32) (result i32)
    (local $i i32) (local $a (ref null $node)) (local $b (ref null $node)) (local $sum i32)
    ;; Reverses the kept list n times, through struct.set of the links.
    (loop $l
      (local.set $a (global.get $keep))
      (local.set $b (ref.null $node))
      (block $done
        (loop $w
          (br_if $done (ref.is_null (local.get $a)))
          (global.set $keep (struct.get $node $next (local.get $a)))
          (struct.set $node $next (local.get $a) (local.get $b))
          (local.set $b (local.get $a))
          (local.set $a (global.get $keep))
          (br $w)))
      (global.set $keep (local.get $b))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.set $a (global.get $keep))
    (block $done
      (loop $w
        (br_if $done (ref.is_null (local.get $a)))
        (local.set $sum (i32.add (local.get $sum) (struct.get $node $v (local.get $a))))
        (local.set $a (struct.get $node $next (local.get $a)))
        (br $w)))
    (local.get $sum)))`

// gcCases are gcWAT's calls.
func gcCases() []jitCase {
	var cases []jitCase
	for _, x := range []uint64{0, 1, 127, 128, 255, 0x7fff, 0x8000, 0xffff, 0x40000000, 0x7fffffff, 0xffffffff} {
		cases = append(cases, jitCase{"fields", []Val{{N: x}}}, jitCase{"fieldref", []Val{{N: x}}}, jitCase{"i31", []Val{{N: x}}}, jitCase{"fixed", []Val{{N: x}}})
	}
	cases = append(cases, jitCase{"defaults", nil})
	for _, ni := range [][2]uint64{{3, 0}, {3, 2}, {3, 3}, {0, 0}, {5, 0xffffffff}} {
		cases = append(cases, jitCase{"arrays", []Val{{N: ni[0]}, {N: ni[1]}}}, jitCase{"refarray", []Val{{N: ni[0]}, {N: ni[1]}}})
	}
	for k := uint64(0); k < 8; k++ {
		cases = append(cases, jitCase{"tests", []Val{{N: k}}}, jitCase{"cast", []Val{{N: k}}}, jitCase{"brcast", []Val{{N: k}}},
			jitCase{"nulls", []Val{{N: k}}}, jitCase{"elems", []Val{{N: k}}})
	}
	return append(cases, jitCase{"list", []Val{{N: 1000}}}, jitCase{"relink", []Val{{N: 5}}})
}

// Native code of the GC instructions computes what the interpreter does,
// traps alike, and allocates through Go.
func TestJITGC(t *testing.T) {
	want, werr := runCases(t, interpretedInstance(t, gcWAT), gcCases())
	inst := nativeInstance(t, gcWAT)
	got, gerr := runCases(t, inst, gcCases())
	sameOutcomes(t, "native", gcCases(), want, werr, got, gerr)
	for _, name := range []string{"fields", "fieldref", "defaults", "arrays", "refarray", "fixed", "elems", "i31", "tests", "cast", "brcast", "nulls", "list", "relink"} {
		if !nativeRan(inst, name) {
			t.Errorf("%s has no native code", name)
		}
	}
}

// Native code relinking a list of structs while the collector runs keeps
// every node: its stores into the structs' reference fields and into the
// global go through the write barrier while the collector marks. The test
// runs again in a process of its own with GODEBUG=gccheckmark=1.
func TestJITGCUnderCollection(t *testing.T) {
	inst := nativeInstance(t, gcWAT)
	const n = 2000
	res, err := refCall(t, inst, "list", Val{N: n})
	if err != nil || res[0].N != n*(n-1)/2 {
		t.Fatalf("list(%d) = %v, %v", n, res, err)
	}
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			runtime.GC()
		}
	}()
	rounds := 100
	if testing.Short() {
		rounds = 10
	}
	for r := 0; r < rounds; r++ {
		res, err := refCall(t, inst, "relink", Val{N: 3})
		if err != nil || res[0].N != n*(n-1)/2 {
			stop.Store(true)
			<-done
			t.Fatalf("relink round %d = %v, %v", r, res, err)
		}
	}
	stop.Store(true)
	<-done
	if os.Getenv("QUICKJS_WASM_GCCHECK_CHILD") != "" || testing.Short() {
		return
	}
	cmd := osexec.Command(os.Args[0], "-test.run=^TestJITGCUnderCollection$", "-test.count=1")
	cmd.Env = append(os.Environ(), "QUICKJS_WASM_GCCHECK_CHILD=1", "GODEBUG=gccheckmark=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("with gccheckmark: %v\n%s", err, out)
	}
}
