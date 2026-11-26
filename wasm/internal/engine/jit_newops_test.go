//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"math"
	"runtime"
	"sync/atomic"
	"testing"
)

// newOpsWAT makes the objects native code does not take from the pools:
// arrays from data and element segments, in range and out of it and after
// the segments are dropped, array.new_fixed past what a pool's block holds,
// of numbers and of references, arrays of v128s, a struct with a v128
// field, and an array too large, a trap; each read back into a sum, and a
// list of them kept in a global.
const newOpsWAT = `(module
  (type $i8s (array (mut i8)))
  (type $i16s (array (mut i16)))
  (type $i32s (array (mut i32)))
  (type $f64s (array (mut f64)))
  (type $refs (array (mut anyref)))
  (type $vecs (array (mut v128)))
  (type $box (struct (field i32)))
  (type $vbox (struct (field i32) (field v128) (field anyref)))
  (global $keep (mut (ref null $refs)) (ref.null $refs))

  (data $text "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
  (data $gone "dropped")
  (elem $smalls anyref
    (item (ref.i31 (i32.const 3))) (item (ref.null any)) (item (ref.i31 (i32.const 4)))
    (item (struct.new $box (i32.const 11))) (item (ref.i31 (i32.const 5))) (item (ref.i31 (i32.const 9))))
  (elem $dead anyref (item (ref.i31 (i32.const 1))))

  ;; A reference counts 1 for null, its value for an i31, 7 times its field
  ;; for a box, and a reference array's sum for one.
  (func $sr (param $a (ref $refs)) (result i64)
    (local $i i32) (local $s i64) (local $e anyref)
    (local.set $s (i64.extend_i32_u (array.len (local.get $a))))
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (array.len (local.get $a))))
      (local.set $e (array.get $refs (local.get $a) (local.get $i)))
      (local.set $s (i64.add (i64.mul (local.get $s) (i64.const 3))
        (if (result i64) (ref.is_null (local.get $e))
          (then (i64.const 1))
          (else (if (result i64) (ref.test (ref i31) (local.get $e))
            (then (i64.extend_i32_s (i31.get_s (ref.cast (ref i31) (local.get $e)))))
            (else (if (result i64) (ref.test (ref $box) (local.get $e))
              (then (i64.mul (i64.const 7) (i64.extend_i32_s (struct.get $box 0 (ref.cast (ref $box) (local.get $e))))))
              (else (call $sr (ref.cast (ref $refs) (local.get $e)))))))))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (local.get $s))
  (func $s8 (param $a (ref $i8s)) (result i64)
    (local $i i32) (local $s i64)
    (local.set $s (i64.extend_i32_u (array.len (local.get $a))))
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (array.len (local.get $a))))
      (local.set $s (i64.add (i64.mul (local.get $s) (i64.const 3))
        (i64.extend_i32_s (array.get_s $i8s (local.get $a) (local.get $i)))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (local.get $s))
  (func $s32 (param $a (ref $i32s)) (result i64)
    (local $i i32) (local $s i64)
    (local.set $s (i64.extend_i32_u (array.len (local.get $a))))
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (array.len (local.get $a))))
      (local.set $s (i64.add (i64.mul (local.get $s) (i64.const 3))
        (i64.extend_i32_s (array.get $i32s (local.get $a) (local.get $i)))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (local.get $s))
  (func $sv (param $a (ref $vecs)) (result i64)
    (local $i i32) (local $s i64)
    (local.set $s (i64.extend_i32_u (array.len (local.get $a))))
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (array.len (local.get $a))))
      (local.set $s (i64.add (i64.mul (local.get $s) (i64.const 3))
        (i64.add (i64x2.extract_lane 0 (array.get $vecs (local.get $a) (local.get $i)))
                 (i64.rotl (i64x2.extract_lane 1 (array.get $vecs (local.get $a) (local.get $i))) (i64.const 7)))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (local.get $s))

  ;; Arrays from the data segments: of i8s, i16s, i32s and f64s, at the
  ;; offset and of the length asked, which may be out of range.
  (func (export "data") (param $off i32) (param $n i32) (result i64)
    (i64.add
      (i64.add (call $s8 (array.new_data $i8s $text (local.get $off) (local.get $n)))
        (i64.extend_i32_u (array.len (array.new_data $i16s $text (local.get $off) (i32.shr_u (local.get $n) (i32.const 1))))))
      (i64.add
        (call $s32 (array.new_data $i32s $text (local.get $off) (i32.shr_u (local.get $n) (i32.const 2))))
        (i64.reinterpret_f64 (array.get $f64s (array.new_data $f64s $text (local.get $off) (i32.const 1)) (i32.const 0))))))
  (func (export "dropData") (data.drop $gone))
  (func (export "gone") (param $off i32) (param $n i32) (result i64)
    (call $s8 (array.new_data $i8s $gone (local.get $off) (local.get $n))))

  ;; Arrays from the element segments.
  (func (export "elems") (param $off i32) (param $n i32) (result i64)
    (call $sr (array.new_elem $refs $smalls (local.get $off) (local.get $n))))
  (func (export "dropElem") (elem.drop $dead))
  (func (export "dead") (param $off i32) (param $n i32) (result i64)
    (call $sr (array.new_elem $refs $dead (local.get $off) (local.get $n))))

  ;; array.new_fixed past a pool's block: 130 i32s and 32 references.
  (func (export "fixed") (param $x i32) (result i64)
    (i64.add
      (call $s32 (array.new_fixed $i32s 130
        (local.get $x) (i32.const 1) (i32.const 2) (i32.const 3) (i32.const 4) (i32.const 5) (i32.const 6) (i32.const 7) (i32.const 8) (i32.const 9)
        (i32.const 10) (i32.const 11) (i32.const 12) (i32.const 13) (i32.const 14) (i32.const 15) (i32.const 16) (i32.const 17) (i32.const 18) (i32.const 19)
        (i32.const 20) (i32.const 21) (i32.const 22) (i32.const 23) (i32.const 24) (i32.const 25) (i32.const 26) (i32.const 27) (i32.const 28) (i32.const 29)
        (i32.const 30) (i32.const 31) (i32.const 32) (i32.const 33) (i32.const 34) (i32.const 35) (i32.const 36) (i32.const 37) (i32.const 38) (i32.const 39)
        (i32.const 40) (i32.const 41) (i32.const 42) (i32.const 43) (i32.const 44) (i32.const 45) (i32.const 46) (i32.const 47) (i32.const 48) (i32.const 49)
        (i32.const 50) (i32.const 51) (i32.const 52) (i32.const 53) (i32.const 54) (i32.const 55) (i32.const 56) (i32.const 57) (i32.const 58) (i32.const 59)
        (i32.const 60) (i32.const 61) (i32.const 62) (i32.const 63) (i32.const 64) (i32.const 65) (i32.const 66) (i32.const 67) (i32.const 68) (i32.const 69)
        (i32.const 70) (i32.const 71) (i32.const 72) (i32.const 73) (i32.const 74) (i32.const 75) (i32.const 76) (i32.const 77) (i32.const 78) (i32.const 79)
        (i32.const 80) (i32.const 81) (i32.const 82) (i32.const 83) (i32.const 84) (i32.const 85) (i32.const 86) (i32.const 87) (i32.const 88) (i32.const 89)
        (i32.const 90) (i32.const 91) (i32.const 92) (i32.const 93) (i32.const 94) (i32.const 95) (i32.const 96) (i32.const 97) (i32.const 98) (i32.const 99)
        (i32.const 100) (i32.const 101) (i32.const 102) (i32.const 103) (i32.const 104) (i32.const 105) (i32.const 106) (i32.const 107) (i32.const 108) (i32.const 109)
        (i32.const 110) (i32.const 111) (i32.const 112) (i32.const 113) (i32.const 114) (i32.const 115) (i32.const 116) (i32.const 117) (i32.const 118) (i32.const 119)
        (i32.const 120) (i32.const 121) (i32.const 122) (i32.const 123) (i32.const 124) (i32.const 125) (i32.const 126) (i32.const 127) (i32.const 128)
        (i32.mul (local.get $x) (i32.const 3))))
      (call $sr (array.new_fixed $refs 32
        (ref.i31 (local.get $x)) (ref.null any) (struct.new $box (local.get $x)) (ref.i31 (i32.const 3))
        (ref.i31 (i32.const 4)) (ref.i31 (i32.const 5)) (ref.i31 (i32.const 6)) (ref.i31 (i32.const 7))
        (ref.i31 (i32.const 8)) (ref.i31 (i32.const 9)) (ref.i31 (i32.const 10)) (ref.i31 (i32.const 11))
        (ref.i31 (i32.const 12)) (ref.i31 (i32.const 13)) (ref.i31 (i32.const 14)) (ref.i31 (i32.const 15))
        (ref.i31 (i32.const 16)) (ref.i31 (i32.const 17)) (ref.i31 (i32.const 18)) (ref.i31 (i32.const 19))
        (ref.i31 (i32.const 20)) (ref.i31 (i32.const 21)) (ref.i31 (i32.const 22)) (ref.i31 (i32.const 23))
        (ref.i31 (i32.const 24)) (ref.i31 (i32.const 25)) (ref.i31 (i32.const 26)) (ref.i31 (i32.const 27))
        (ref.i31 (i32.const 28)) (ref.i31 (i32.const 29)) (struct.new $box (i32.const 30)) (ref.i31 (local.get $x))))))

  ;; Arrays of v128s: of a constant length and of one computed, zero, of
  ;; one value and of two literal ones; and a struct with a v128 field.
  (func (export "vecs") (param $x i32) (param $n i32) (result i64)
    (local $z (ref null $vecs)) (local $b (ref null $vbox))
    (local.set $z (array.new_default $vecs (local.get $n)))
    (if (local.get $n)
      (then (array.set $vecs (local.get $z) (i32.sub (local.get $n) (i32.const 1)) (i32x4.splat (local.get $x)))))
    (local.set $b (struct.new $vbox (local.get $x) (i64x2.splat (i64.extend_i32_u (local.get $x))) (struct.new $box (local.get $x))))
    (i64.add
      (i64.add (call $sv (ref.as_non_null (local.get $z))) (call $sv (array.new_default $vecs (i32.const 3))))
      (i64.add
        (i64.add (call $sv (array.new $vecs (i32x4.splat (i32.mul (local.get $x) (i32.const 5))) (local.get $n)))
          (call $sv (array.new_fixed $vecs 2 (i64x2.splat (i64.const 7)) (i32x4.splat (local.get $x)))))
        (i64.add (i64x2.extract_lane 1 (struct.get $vbox 1 (local.get $b)))
          (i64.extend_i32_s (struct.get $box 0 (ref.cast (ref $box) (struct.get $vbox 2 (local.get $b)))))))))

  ;; An array of $n v128s: past what may be made, a trap.
  (func (export "hugeVecs") (param $n i32) (result i32)
    (array.len (array.new_default $vecs (local.get $n))))

  ;; A list of $n arrays kept in a global, each made from the element
  ;; segment, holding a box and the list before it.
  (func (export "keep") (param $n i32) (result i64)
    (local $i i32) (local $a (ref null $refs))
    (global.set $keep (ref.null $refs))
    (loop $l
      (local.set $a (array.new_elem $refs $smalls (i32.const 1) (i32.const 4)))
      (array.set $refs (local.get $a) (i32.const 0) (global.get $keep))
      (array.set $refs (local.get $a) (i32.const 1) (struct.new $box (local.get $i)))
      (global.set $keep (local.get $a))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (call $kept))
  (func $kept (export "kept") (result i64)
    (local $p (ref null $refs)) (local $s i64)
    (local.set $p (global.get $keep))
    (block $d (loop $l
      (br_if $d (ref.is_null (local.get $p)))
      (local.set $s (i64.add (i64.rotl (local.get $s) (i64.const 1))
        (i64.add (i64.extend_i32_s (struct.get $box 0 (ref.cast (ref $box) (array.get $refs (local.get $p) (i32.const 1)))))
                 (i64.extend_i32_s (i31.get_s (ref.cast (ref i31) (array.get $refs (local.get $p) (i32.const 3))))))))
      (local.set $p (ref.cast (ref null $refs) (array.get $refs (local.get $p) (i32.const 0))))
      (br $l)))
    (local.get $s)))`

// newOpsCases are newOpsWAT's calls, the segments' traps and drops among
// them.
func newOpsCases() []jitCase {
	return []jitCase{
		{"data", []Val{{N: 0}, {N: 16}}}, {"data", []Val{{N: 5}, {N: 40}}}, {"data", []Val{{N: 54}, {N: 8}}},
		{"data", []Val{{N: 55}, {N: 8}}}, {"data", []Val{{N: 0}, {N: 0}}}, {"data", []Val{{N: 62}, {N: 0}}},
		{"data", []Val{{N: 0xffff_fff0}, {N: 0x20}}},
		{"gone", []Val{{N: 0}, {N: 7}}}, {"gone", []Val{{N: 7}, {N: 0}}}, {"gone", []Val{{N: 8}, {N: 0}}},
		{"dropData", nil}, {"gone", []Val{{N: 0}, {N: 0}}}, {"gone", []Val{{N: 0}, {N: 1}}}, {"gone", []Val{{N: 1}, {N: 0}}},
		{"elems", []Val{{N: 0}, {N: 6}}}, {"elems", []Val{{N: 2}, {N: 3}}}, {"elems", []Val{{N: 6}, {N: 0}}},
		{"elems", []Val{{N: 4}, {N: 3}}}, {"elems", []Val{{N: 7}, {N: 0}}}, {"elems", []Val{{N: 0xffff_ffff}, {N: 2}}},
		{"dead", []Val{{N: 0}, {N: 1}}}, {"dropElem", nil}, {"dead", []Val{{N: 0}, {N: 0}}}, {"dead", []Val{{N: 0}, {N: 1}}},
		{"fixed", []Val{{N: 0}}}, {"fixed", []Val{{N: 0x8000_0001}}},
		{"vecs", []Val{{N: 1}, {N: 0}}}, {"vecs", []Val{{N: 7}, {N: 5}}}, {"vecs", []Val{{N: 0xdead_beef}, {N: 40}}},
		{"hugeVecs", []Val{{N: 10}}}, {"hugeVecs", []Val{{N: 0x7fff_ffff}}},
		{"keep", []Val{{N: 2000}}}, {"kept", nil},
	}
}

// Native code makes arrays from segments and of v128s, and structs with
// v128 fields, itself, and the long literal arrays through the
// allocation stub, in the
// baseline's code and optimized, idle, marking and with the registers
// clobbered: the results and the traps are the interpreter's, and only
// the traps and the segments' drops are exits.
func TestJITNewOps(t *testing.T) {
	want, werr := runCases(t, thresholdInstance(t, newOpsWAT, math.MaxInt32), newOpsCases())
	traps := 0
	for _, err := range werr {
		if err != nil {
			traps++
		}
	}
	for _, opt := range []bool{false, true} {
		tier := map[bool]string{false: "baseline", true: "opt"}[opt]
		for _, mode := range []string{"idle", "marking", "clobbered"} {
			t.Run(tier+"/"+mode, func(t *testing.T) {
				if mode != "idle" {
					jitMarkingForTest(t, mode == "clobbered")
				}
				inst := thresholdInstance(t, newOpsWAT, 0)
				if opt {
					inst.Code.SetOptThreshold(0)
					inst.Code.SetOptBackground(false)
				}
				c := &codeTestChain(t)[0].counts
				ops, stubs := c.ops.Load(), c.opStubs.Load()
				got, gerr := runCases(t, inst, newOpsCases())
				sameOutcomes(t, tier, newOpsCases(), want, werr, got, gerr)
				ops, stubs = c.ops.Load()-ops, c.opStubs.Load()-stubs
				// A trap native code makes itself is an ExitTrap; one of
				// an instruction Go makes is an ExitOp, the engine
				// declining it in the stub; data.drop and elem.drop are
				// ExitOps.
				if ops > int64(traps)+2 || stubs == 0 {
					t.Fatalf("%d ExitOps for %d traps and 2 drops, %d instructions run from the stub", ops, traps, stubs)
				}
				// The arrays from segments and of v128s, and the struct
				// with a v128 field, are native code's own.
				before := c.opStubs.Load()
				if _, err := refCall(t, inst, "data", Val{N: 3}, Val{N: 30}); err != nil {
					t.Fatal(err)
				}
				if _, err := refCall(t, inst, "elems", Val{N: 1}, Val{N: 5}); err != nil {
					t.Fatal(err)
				}
				if _, err := refCall(t, inst, "vecs", Val{N: 9}, Val{N: 20}); err != nil {
					t.Fatal(err)
				}
				if n := c.opStubs.Load() - before; n != 0 {
					t.Fatalf("%d arrays from segments or of v128s made by Go", n)
				}
				for _, name := range []string{"data", "elems", "fixed", "vecs", "keep"} {
					if opt && !optimized(inst, name) || !opt && !nativeRan(inst, name) {
						t.Errorf("%s is not %s code", name, tier)
					}
				}
			})
		}
	}
}

// The arrays the stub makes from an element segment while the collector
// runs keep every reference they hold.
func TestJITNewOpsUnderCollection(t *testing.T) {
	want, _ := refCall(t, thresholdInstance(t, newOpsWAT, math.MaxInt32), "keep", Val{N: 3000})
	for _, opt := range []bool{false, true} {
		inst := thresholdInstance(t, newOpsWAT, 0)
		if opt {
			inst.Code.SetOptThreshold(0)
			inst.Code.SetOptBackground(false)
		}
		var stop atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			for !stop.Load() {
				runtime.GC()
			}
		}()
		rounds := 20
		if testing.Short() {
			rounds = 3
		}
		for r := 0; r < rounds; r++ {
			res, err := refCall(t, inst, "keep", Val{N: 3000})
			if err == nil {
				res, err = refCall(t, inst, "kept")
			}
			if err != nil || res[0] != want[0] {
				stop.Store(true)
				<-done
				t.Fatalf("opt %v, round %d: %v, %v", opt, r, res, err)
			}
		}
		stop.Store(true)
		<-done
	}
}
