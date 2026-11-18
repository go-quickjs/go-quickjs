package engine

import (
	"testing"
)

// nativeGCSrc is a module of the GC operations native code runs itself:
// struct fields and array elements of every storage kind, read signed and
// unsigned, arrays' lengths, i31s, the tests and casts to type indices,
// br_on_cast both ways, call_ref, the reference tests and selects --
// each in a loop of n turns after the objects' allocations, which leave to
// Go, and with traps for null, out-of-bounds indices and failed casts.
const nativeGCSrc = `(module
  (type $s (sub (struct (field (mut i8)) (field (mut i16)) (field (mut i32)) (field (mut i64))
    (field (mut f32)) (field (mut f64)) (field (mut v128)) (field (mut (ref null $s))) (field (mut eqref)))))
  (type $t (sub $s (struct (field (mut i8)) (field (mut i16)) (field (mut i32)) (field (mut i64))
    (field (mut f32)) (field (mut f64)) (field (mut v128)) (field (mut (ref null $s))) (field (mut eqref)) (field i32))))
  (type $a8 (array (mut i8)))
  (type $a16 (array (mut i16)))
  (type $a32 (array (mut i32)))
  (type $a64 (array (mut i64)))
  (type $af (array (mut f64)))
  (type $av (array (mut v128)))
  (type $ar (array (mut (ref null eq))))
  (type $fn (func (param i32) (result i32)))
  (elem declare func $twice)
  (func $twice (type $fn) (i32.add (local.get 0) (local.get 0)))
  (func (export "structs") (param $n i32) (param $x i64) (result i64)
    (local $s (ref null $s)) (local $o (ref null $s)) (local $i i32) (local $acc i64)
    (local.set $o (struct.new_default $s))
    (local.set $s (struct.new_default $s))
    (loop $l
      (struct.set $s 0 (local.get $s) (i32.wrap_i64 (local.get $x)))
      (struct.set $s 1 (local.get $s) (i32.wrap_i64 (i64.shr_u (local.get $x) (i64.const 8))))
      (struct.set $s 2 (local.get $s) (i32.add (local.get $i) (i32.wrap_i64 (local.get $x))))
      (struct.set $s 3 (local.get $s) (i64.mul (local.get $x) (i64.extend_i32_u (local.get $i))))
      (struct.set $s 4 (local.get $s) (f32.convert_i32_s (local.get $i)))
      (struct.set $s 5 (local.get $s) (f64.reinterpret_i64 (local.get $x)))
      (struct.set $s 6 (local.get $s) (i64x2.splat (local.get $x)))
      (struct.set $s 7 (local.get $s) (select (result (ref null $s)) (local.get $o) (ref.null $s) (i32.and (local.get $i) (i32.const 1))))
      (struct.set $s 8 (local.get $s) (ref.i31 (local.get $i)))
      (local.set $acc (i64.add (local.get $acc)
        (i64.add
          (i64.add (i64.extend_i32_u (struct.get_s $s 0 (local.get $s))) (i64.extend_i32_u (struct.get_u $s 0 (local.get $s))))
          (i64.add (i64.extend_i32_u (struct.get_s $s 1 (local.get $s))) (i64.extend_i32_u (struct.get_u $s 1 (local.get $s)))))))
      (local.set $acc (i64.add (local.get $acc)
        (i64.add (i64.add (i64.extend_i32_u (struct.get $s 2 (local.get $s))) (struct.get $s 3 (local.get $s)))
          (i64.add (i64.extend_i32_u (i32.reinterpret_f32 (struct.get $s 4 (local.get $s)))) (i64.reinterpret_f64 (struct.get $s 5 (local.get $s)))))))
      (local.set $acc (i64.add (local.get $acc)
        (i64.add (i64x2.extract_lane 1 (struct.get $s 6 (local.get $s)))
          (i64.add (i64.extend_i32_u (ref.is_null (struct.get $s 7 (local.get $s))))
            (i64.extend_i32_u (i31.get_s (ref.cast (ref i31) (struct.get $s 8 (local.get $s)))))))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $acc))
  (func (export "arrays") (param $n i32) (param $x i64) (result i64)
    (local $b (ref null $a8)) (local $h (ref null $a16)) (local $w (ref null $a32)) (local $d (ref null $a64))
    (local $f (ref null $af)) (local $v (ref null $av)) (local $r (ref null $ar)) (local $i i32) (local $acc i64)
    (local.set $b (array.new_default $a8 (local.get $n)))
    (local.set $h (array.new_default $a16 (local.get $n)))
    (local.set $w (array.new_default $a32 (local.get $n)))
    (local.set $d (array.new_default $a64 (local.get $n)))
    (local.set $f (array.new_default $af (local.get $n)))
    (local.set $v (array.new_default $av (local.get $n)))
    (local.set $r (array.new_default $ar (local.get $n)))
    (loop $l
      (array.set $a8 (local.get $b) (local.get $i) (i32.wrap_i64 (i64.add (local.get $x) (i64.extend_i32_u (local.get $i)))))
      (array.set $a16 (local.get $h) (local.get $i) (i32.wrap_i64 (i64.mul (local.get $x) (i64.extend_i32_u (local.get $i)))))
      (array.set $a32 (local.get $w) (local.get $i) (i32.xor (local.get $i) (i32.wrap_i64 (local.get $x))))
      (array.set $a64 (local.get $d) (local.get $i) (i64.rotl (local.get $x) (i64.extend_i32_u (local.get $i))))
      (array.set $af (local.get $f) (local.get $i) (f64.convert_i32_s (i32.sub (i32.const 7) (local.get $i))))
      (array.set $av (local.get $v) (local.get $i) (i64x2.splat (i64.extend_i32_u (local.get $i))))
      (array.set $ar (local.get $r) (local.get $i) (select (result eqref) (ref.i31 (local.get $i)) (local.get $b) (i32.and (local.get $i) (i32.const 1))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (array.len (local.get $b)))))
    (local.set $i (i32.const 0))
    (loop $l
      (local.set $acc (i64.add (local.get $acc)
        (i64.add
          (i64.add (i64.extend_i32_u (array.get_s $a8 (local.get $b) (local.get $i))) (i64.extend_i32_u (array.get_u $a8 (local.get $b) (local.get $i))))
          (i64.add (i64.extend_i32_u (array.get_s $a16 (local.get $h) (local.get $i))) (i64.extend_i32_u (array.get_u $a16 (local.get $h) (local.get $i)))))))
      (local.set $acc (i64.add (local.get $acc)
        (i64.add (i64.add (i64.extend_i32_u (array.get $a32 (local.get $w) (local.get $i))) (array.get $a64 (local.get $d) (local.get $i)))
          (i64.add (i64.reinterpret_f64 (array.get $af (local.get $f) (local.get $i)))
            (i64x2.extract_lane 0 (array.get $av (local.get $v) (local.get $i)))))))
      (local.set $acc (i64.add (local.get $acc) (i64.extend_i32_u (ref.test (ref i31) (array.get $ar (local.get $r) (local.get $i))))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (array.len (local.get $w)))))
    (local.get $acc))
  (func (export "oob") (param $n i32) (param $i i32) (result i32)
    (array.get $a32 (array.new_default $a32 (local.get $n)) (local.get $i)))
  (func (export "oobset") (param $n i32) (param $i i32)
    (array.set $a16 (array.new_default $a16 (local.get $n)) (local.get $i) (i32.const 1)))
  (func (export "null") (param $k i32) (result i32)
    (local $s (ref null $s)) (local $a (ref null $a8))
    (if (i32.eq (local.get $k) (i32.const 1)) (then (return (struct.get $s 2 (local.get $s)))))
    (if (i32.eq (local.get $k) (i32.const 2)) (then (struct.set $s 2 (local.get $s) (i32.const 1))))
    (if (i32.eq (local.get $k) (i32.const 3)) (then (return (array.len (local.get $a)))))
    (if (i32.eq (local.get $k) (i32.const 4)) (then (return (array.get_u $a8 (local.get $a) (i32.const 0)))))
    (if (i32.eq (local.get $k) (i32.const 5)) (then (return (i31.get_u (ref.null i31)))))
    (if (i32.eq (local.get $k) (i32.const 6)) (then (return (ref.is_null (ref.as_non_null (local.get $s))))))
    (i32.const -1))
  (func (export "casts") (param $n i32) (param $k i32) (result i32)
    (local $r eqref) (local $i i32) (local $acc i32)
    (local.set $r
      (if (result eqref) (i32.eq (local.get $k) (i32.const 0)) (then (struct.new_default $s))
        (else (if (result eqref) (i32.eq (local.get $k) (i32.const 1)) (then (struct.new_default $t))
          (else (if (result eqref) (i32.eq (local.get $k) (i32.const 2)) (then (ref.i31 (i32.const 5)))
            (else (if (result eqref) (i32.eq (local.get $k) (i32.const 3)) (then (array.new_default $a8 (i32.const 1)))
              (else (ref.null eq))))))))))
    (loop $l
      (local.set $acc (i32.add (local.get $acc)
        (i32.add (i32.add (ref.test (ref $s) (local.get $r)) (i32.shl (ref.test (ref $t) (local.get $r)) (i32.const 1)))
          (i32.add (i32.shl (ref.test (ref null $s) (local.get $r)) (i32.const 2)) (i32.shl (ref.test (ref $a8) (local.get $r)) (i32.const 3))))))
      (local.set $acc (i32.add (local.get $acc)
        (block $done (result i32)
          (drop (block $y (result (ref $s)) (br_on_cast $y eqref (ref $s) (local.get $r)) (drop) (br $done (i32.const 100))))
          (i32.const 1000))))
      (local.set $acc (i32.add (local.get $acc)
        (block $done (result i32)
          (drop (block $y (result eqref) (br_on_cast_fail $y eqref (ref null $t) (local.get $r)) (drop) (br $done (i32.const 7))))
          (i32.const 70000))))
      (local.set $acc (i32.add (local.get $acc) (ref.eq (local.get $r) (local.get $r))))
      (local.set $acc (i32.add (local.get $acc)
        (block $done (result i32)
          (drop (block $nn (result (ref eq)) (br_on_non_null $nn (local.get $r)) (br $done (i32.const 3))))
          (i32.const 9))))
      (local.set $acc (i32.add (local.get $acc)
        (block $done (result i32)
          (block $nl (br_on_null $nl (local.get $r)) (drop) (br $done (i32.const 11)))
          (i32.const 13))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $acc))
  (func (export "cast") (param $k i32) (result i32)
    (local $r eqref)
    (local.set $r (if (result eqref) (i32.eqz (local.get $k)) (then (struct.new_default $t)) (else (ref.i31 (i32.const 1)))))
    (struct.get $t 9 (ref.cast (ref $t) (local.get $r))))
  (func (export "callref") (param $n i32) (param $null i32) (result i32)
    (local $f (ref null $fn)) (local $i i32) (local $acc i32)
    (local.set $f (select (result (ref null $fn)) (ref.null $fn) (ref.func $twice) (local.get $null)))
    (loop $l
      (local.set $acc (i32.add (local.get $acc) (call_ref $fn (local.get $i) (local.get $f))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $acc))
  (func (export "sel") (param $c i32) (result i64)
    (local $a v128) (local $b v128) (local $x eqref) (local $y eqref)
    (local.set $a (v128.const i64x2 1 2)) (local.set $b (v128.const i64x2 3 4))
    (local.set $x (ref.i31 (i32.const 4))) (local.set $y (ref.i31 (i32.const 6)))
    (i64.add (i64x2.extract_lane 1 (select (local.get $a) (local.get $b) (local.get $c)))
      (i64.extend_i32_u (i31.get_u (ref.cast (ref i31) (select (result eqref) (local.get $x) (local.get $y) (local.get $c))))))))`

// TestNativeGC checks the GC operations native code runs against the Go
// table, with the collector's marking flag read set and clear, and that a
// loop of them leaves to Go only for the allocations before it.
func TestNativeGC(t *testing.T) {
	nativeOnly(t)
	var calls []regCall
	for _, x := range []uint64{0, 0x80, 0xff80fe7f81, 1<<63 | 0x7fff, 0x7ff8000000000001} {
		for _, n := range []uint64{1, 7, 40} {
			calls = append(calls, regCall{"structs", []Val{{N: n}, {N: x}}}, regCall{"arrays", []Val{{N: n}, {N: x}}})
		}
	}
	for _, c := range [][2]uint64{{4, 0}, {4, 3}, {4, 4}, {4, 0xffffffff}, {0, 0}, {1, 1}} {
		calls = append(calls, regCall{"oob", []Val{{N: c[0]}, {N: c[1]}}}, regCall{"oobset", []Val{{N: c[0]}, {N: c[1]}}})
	}
	for k := uint64(0); k <= 6; k++ {
		calls = append(calls, regCall{"null", []Val{{N: k}}})
	}
	for k := uint64(0); k <= 4; k++ {
		calls = append(calls, regCall{"casts", []Val{{N: 5}, {N: k}}})
	}
	calls = append(calls, regCall{"cast", []Val{{N: 0}}}, regCall{"cast", []Val{{N: 1}}},
		regCall{"callref", []Val{{N: 9}, {N: 0}}}, regCall{"callref", []Val{{N: 9}, {N: 1}}},
		regCall{"sel", []Val{{N: 0}}}, regCall{"sel", []Val{{N: 1}}})
	marked, unmarked := matchNative(t, nativeGCSrc, calls)
	if marked == 0 || unmarked != 0 {
		t.Fatalf("the barrier routine stored %d times marking and %d not, want some and none", marked, unmarked)
	}
	// The loops run natively: a call of n turns leaves to Go for its
	// allocations and its first calls alone.
	inst := instantiateMode(t, nativeGCSrc, true)
	for _, c := range []struct {
		name  string
		args  []uint64
		exits int64
	}{{"structs", []uint64{200, 5}, 2}, {"arrays", []uint64{200, 5}, 7}, {"casts", []uint64{200, 0}, 1},
		{"casts", []uint64{200, 1}, 1}, {"casts", []uint64{200, 2}, 1}, {"callref", []uint64{200, 0}, 1}} {
		withNative(true, func() {
			if _, err := call(t, inst, c.name, c.args...); err != nil {
				t.Fatal(err)
			}
		})
		var exits int64
		withNative(true, func() {
			exits = countNativeExits(func() {
				if _, err := call(t, inst, c.name, c.args...); err != nil {
					t.Fatal(err)
				}
			})
		})
		if exits > c.exits {
			t.Errorf("%s%v left native code %d times, want at most %d", c.name, c.args, exits, c.exits)
		}
	}
}
