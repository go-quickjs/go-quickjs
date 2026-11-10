//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"context"
	"errors"
	"math"
	"os"
	osexec "os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// arrayPoolWAT makes arrays as native code makes them from the engine's
// pools: array.new_fixed, array.new of constant lengths and array.new_default
// of any, of every storage type but v128, of lengths 0 to the most a pool's
// block has room for and past it (which exit to Go), and too large, a
// trap; summed back
// through array.len and array.get; lists of reference arrays kept in a
// global; and arrays made on both sides of a call of the host that makes
// them in the same instance.
const arrayPoolWAT = `(module
  (import "env" "back" (func $back (param i32) (result i64)))
  (type $i8s (array (mut i8)))
  (type $i16s (array (mut i16)))
  (type $i32s (array (mut i32)))
  (type $i64s (array (mut i64)))
  (type $f32s (array (mut f32)))
  (type $f64s (array (mut f64)))
  (type $refs (array (mut anyref)))
  (type $box (struct (field i32)))
  (global $keep (mut (ref null $refs)) (ref.null $refs))

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
  (func $s16 (param $a (ref $i16s)) (result i64)
    (local $i i32) (local $s i64)
    (local.set $s (i64.extend_i32_u (array.len (local.get $a))))
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (array.len (local.get $a))))
      (local.set $s (i64.add (i64.mul (local.get $s) (i64.const 3))
        (i64.extend_i32_u (array.get_u $i16s (local.get $a) (local.get $i)))))
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
  (func $s64 (param $a (ref $i64s)) (result i64)
    (local $i i32) (local $s i64)
    (local.set $s (i64.extend_i32_u (array.len (local.get $a))))
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (array.len (local.get $a))))
      (local.set $s (i64.add (i64.mul (local.get $s) (i64.const 3))
        (array.get $i64s (local.get $a) (local.get $i))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (local.get $s))
  (func $sf32 (param $a (ref $f32s)) (result i64)
    (local $i i32) (local $s i64)
    (local.set $s (i64.extend_i32_u (array.len (local.get $a))))
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (array.len (local.get $a))))
      (local.set $s (i64.add (i64.mul (local.get $s) (i64.const 3))
        (i64.extend_i32_u (i32.reinterpret_f32 (array.get $f32s (local.get $a) (local.get $i))))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (local.get $s))
  (func $sf64 (param $a (ref $f64s)) (result i64)
    (local $i i32) (local $s i64)
    (local.set $s (i64.extend_i32_u (array.len (local.get $a))))
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (array.len (local.get $a))))
      (local.set $s (i64.add (i64.mul (local.get $s) (i64.const 3))
        (i64.reinterpret_f64 (array.get $f64s (local.get $a) (local.get $i)))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (local.get $s))
  ;; A reference element counts 1 for null, its value for an i31, 7 times
  ;; its field for a box, and a reference array's sum for one.
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

  ;; array.new_fixed of every storage type, of a few lengths.
  (func (export "fixed") (param $n i32) (result i64)
    (local $i i32) (local $s i64) (local $x i32)
    (loop $l
      (local.set $x (i32.mul (local.get $i) (i32.const 0x9e3779b9)))
      (local.set $s (i64.add (i64.rotl (local.get $s) (i64.const 5))
        (i64.add
          (i64.add
            (call $s8 (array.new_fixed $i8s 3 (local.get $x) (i32.const -1) (i32.shr_u (local.get $x) (i32.const 8))))
            (call $s16 (array.new_fixed $i16s 1 (local.get $x))))
          (i64.add
            (i64.add
              (call $s32 (array.new_fixed $i32s 5 (local.get $x) (local.get $i) (i32.const 7) (i32.const -7) (i32.shl (local.get $x) (i32.const 3))))
              (call $s32 (array.new_fixed $i32s 0)))
            (i64.add
              (i64.add
                (call $s64 (array.new_fixed $i64s 2 (i64.extend_i32_s (local.get $x)) (i64.const 0x123456789abcdef0)))
                (call $sf32 (array.new_fixed $f32s 3 (f32.convert_i32_s (local.get $x)) (f32.const 0.5) (f32.const -0))))
              (i64.add
                (call $sf64 (array.new_fixed $f64s 2 (f64.convert_i32_s (local.get $x)) (f64.const -1.25)))
                (call $sr (array.new_fixed $refs 4 (ref.i31 (local.get $x)) (ref.null any)
                  (struct.new $box (local.get $i)) (array.new_fixed $refs 1 (ref.i31 (local.get $i)))))))))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $s))

  ;; array.new of constant lengths, unrolled up to 16 and exiting past
  ;; them.
  (func (export "news") (param $n i32) (result i64)
    (local $i i32) (local $s i64) (local $x i32)
    (loop $l
      (local.set $x (i32.mul (local.get $i) (i32.const 0x9e3779b9)))
      (local.set $s (i64.add (i64.rotl (local.get $s) (i64.const 5))
        (i64.add
          (i64.add
            (i64.add (call $s8 (array.new $i8s (local.get $x) (i32.const 16)))
                     (call $s16 (array.new $i16s (local.get $x) (i32.const 0))))
            (i64.add (call $s32 (array.new $i32s (local.get $x) (i32.const 17)))
                     (call $s64 (array.new $i64s (i64.extend_i32_u (local.get $x)) (i32.const 1)))))
          (i64.add
            (i64.add (call $sf32 (array.new $f32s (f32.convert_i32_u (local.get $x)) (i32.const 5)))
                     (call $sf64 (array.new $f64s (f64.convert_i32_u (local.get $x)) (i32.const 3))))
            (i64.add (call $sr (array.new $refs (struct.new $box (local.get $x)) (i32.const 16)))
                     (call $sr (array.new $refs (ref.i31 (local.get $i)) (i32.const 2))))))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $s))

  ;; array.new_default of constant lengths: the largest a pool's block has
  ;; room for, one past it, and short ones, each written and summed.
  (func (export "defaults") (param $n i32) (result i64)
    (local $i i32) (local $s i64) (local $a (ref null $i32s)) (local $r (ref null $refs)) (local $b (ref null $i8s))
    (loop $l
      (local.set $a (array.new_default $i32s (i32.const 120)))
      (array.set $i32s (local.get $a) (i32.const 119) (local.get $i))
      (local.set $r (array.new_default $refs (i32.const 30)))
      (array.set $refs (local.get $r) (i32.const 29) (ref.i31 (local.get $i)))
      (local.set $b (array.new_default $i8s (i32.const 480)))
      (array.set $i8s (local.get $b) (i32.const 479) (local.get $i))
      (local.set $s (i64.add (i64.rotl (local.get $s) (i64.const 5))
        (i64.add
          (i64.add
            (i64.add (call $s32 (ref.as_non_null (local.get $a))) (call $s32 (array.new_default $i32s (i32.const 121))))
            (i64.add (call $sr (ref.as_non_null (local.get $r))) (call $sr (array.new_default $refs (i32.const 31)))))
          (i64.add
            (i64.add (call $s8 (ref.as_non_null (local.get $b))) (call $s8 (array.new_default $i8s (i32.const 481))))
            (i64.add (call $s16 (array.new_default $i16s (i32.const 0)))
              (i64.add (call $sf64 (array.new_default $f64s (i32.const 7))) (call $sf32 (array.new_default $f32s (i32.const 1)))))))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $s))

  ;; A list of $n reference arrays kept in a global, each holding a box and
  ;; the list before it.
  (func $keep (export "keep") (param $n i32) (result i64)
    (local $i i32)
    (global.set $keep (ref.null $refs))
    (loop $l
      (global.set $keep (array.new_fixed $refs 3 (struct.new $box (local.get $i)) (global.get $keep) (ref.i31 (local.get $i))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (call $kept))
  (func $kept (export "kept") (result i64)
    (local $p (ref null $refs)) (local $s i64)
    (local.set $p (global.get $keep))
    (block $d (loop $l
      (br_if $d (ref.is_null (local.get $p)))
      (local.set $s (i64.add (i64.rotl (local.get $s) (i64.const 1))
        (i64.add (i64.extend_i32_s (struct.get $box 0 (ref.cast (ref $box) (array.get $refs (local.get $p) (i32.const 0)))))
                 (i64.extend_i32_s (i31.get_s (ref.cast (ref i31) (array.get $refs (local.get $p) (i32.const 2))))))))
      (local.set $p (ref.cast (ref null $refs) (array.get $refs (local.get $p) (i32.const 1))))
      (br $l)))
    (local.get $s))

  ;; array.new_default of lengths not constant, across every class and
  ;; past the largest, the last element written where there is one.
  (func (export "lengths") (param $n i32) (result i64)
    (local $i i32) (local $s i64) (local $k i32)
    (local $a (ref null $i32s)) (local $r (ref null $refs)) (local $b (ref null $i8s))
    (loop $l
      (local.set $k (i32.rem_u (local.get $i) (i32.const 130)))
      (local.set $a (array.new_default $i32s (local.get $k)))
      (if (local.get $k) (then (array.set $i32s (local.get $a) (i32.sub (local.get $k) (i32.const 1)) (local.get $i))))
      (local.set $k (i32.rem_u (local.get $i) (i32.const 35)))
      (local.set $r (array.new_default $refs (local.get $k)))
      (if (local.get $k) (then (array.set $refs (local.get $r) (i32.sub (local.get $k) (i32.const 1)) (ref.i31 (local.get $i)))))
      (local.set $k (i32.rem_u (i32.mul (local.get $i) (i32.const 7)) (i32.const 500)))
      (local.set $b (array.new_default $i8s (local.get $k)))
      (if (local.get $k) (then (array.set $i8s (local.get $b) (i32.sub (local.get $k) (i32.const 1)) (local.get $i))))
      (local.set $s (i64.add (i64.rotl (local.get $s) (i64.const 5))
        (i64.add
          (i64.add (call $s32 (ref.as_non_null (local.get $a))) (call $sr (ref.as_non_null (local.get $r))))
          (i64.add (call $s8 (ref.as_non_null (local.get $b)))
            (i64.add (call $s16 (array.new_default $i16s (i32.rem_u (local.get $i) (i32.const 250))))
              (call $sf64 (array.new_default $f64s (i32.rem_u (local.get $i) (i32.const 70)))))))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $s))

  ;; An array of $n i64s: past what pools hold, made by the engine, or
  ;; too large, a trap.
  (func (export "huge") (param $n i32) (result i64)
    (call $s64 (array.new_default $i64s (local.get $n))))

  (func (export "nested") (param $n i32) (result i64)
    (local $a (ref null $refs))
    (local.set $a (array.new_fixed $refs 2 (array.new_fixed $refs 1 (ref.i31 (local.get $n))) (struct.new $box (local.get $n))))
    (i64.add (call $back (local.get $n))
      (i64.add (call $sr (ref.as_non_null (local.get $a)))
        (call $sr (array.new $refs (ref.i31 (local.get $n)) (i32.const 3)))))))`

// arrayPoolCases are arrayPoolWAT's calls.
func arrayPoolCases() []jitCase {
	return []jitCase{
		{"fixed", []Val{{N: 1}}}, {"fixed", []Val{{N: 3000}}}, {"news", []Val{{N: 1}}}, {"news", []Val{{N: 2000}}},
		{"defaults", []Val{{N: 1}}}, {"defaults", []Val{{N: 300}}}, {"keep", []Val{{N: 1000}}}, {"kept", nil},
		{"nested", []Val{{N: 5}}}, {"nested", []Val{{N: 0}}},
		{"lengths", []Val{{N: 1}}}, {"lengths", []Val{{N: 1000}}},
		{"huge", []Val{{N: 100_000}}}, {"huge", []Val{{N: 0x7fff_ffff}}}, {"huge", []Val{{N: 0}}},
	}
}

// arrayPoolInstance instantiates arrayPoolWAT, its host function calling
// news back in the same instance, interpreted (threshold math.MaxInt32),
// in the baseline's code, or optimized.
func arrayPoolInstance(t testing.TB, threshold int, opt bool) *Instance {
	t.Helper()
	var inst *Instance
	host := NewHostFunction(module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}, Results: []module.ValueType{module.I64}}), "back",
		func(ctx context.Context, _ *Instance, args, results []Val) error {
			ext, _ := inst.Export("news")
			res, err := Invoke(ctx, ext.Func, []Val{{N: 4 + args[0].N}})
			if err != nil {
				return err
			}
			results[0] = res[0]
			return nil
		})
	inst = thresholdInstance(t, arrayPoolWAT, threshold, Extern{Kind: module.ExternFunc, Func: host})
	if opt {
		inst.Code.SetOptThreshold(0)
		inst.Code.SetOptBackground(false)
	}
	return inst
}

// Native code makes arrays from the engine's pools, in the baseline's code
// and optimized, idle and with the collector marking: the results are the
// interpreter's, the pools are filled a few times for many arrays, and the
// exits are the same whether the collector marks or not.
func TestJITArrayPools(t *testing.T) {
	want, werr := runCases(t, arrayPoolInstance(t, math.MaxInt32, false), arrayPoolCases())
	for _, opt := range []bool{false, true} {
		tier := map[bool]string{false: "baseline", true: "opt"}[opt]
		var idleExits [2]int64
		for _, mode := range []string{"idle", "marking", "clobbered"} {
			t.Run(tier+"/"+mode, func(t *testing.T) {
				if mode != "idle" {
					jitMarkingForTest(t, mode == "clobbered")
				}
				inst := arrayPoolInstance(t, 0, opt)
				var got [][]Val
				var gerr []error
				alloc, all := countAllocExits(func() { got, gerr = runCases(t, inst, arrayPoolCases()) })
				sameOutcomes(t, tier, arrayPoolCases(), want, werr, got, gerr)
				if mode == "idle" {
					idleExits = [2]int64{alloc, all}
				} else if [2]int64{alloc, all} != idleExits {
					t.Fatalf("%d fills and %d exits while marking, %d and %d idle", alloc, all, idleExits[0], idleExits[1])
				}
				// Some 75,000 arrays from pools of 60 types and classes:
				// the fills double up to the pools' capacity.
				if alloc == 0 || alloc > 75_000/gcPoolCap*2+60*8 {
					t.Fatalf("%d fills", alloc)
				}
				// What is not pooled exits to Go: array.new of 17
				// elements, array.new_default past the blocks, and the
				// structs' and arrays' exits for nothing else.
				for _, name := range []string{"fixed", "news", "defaults", "keep", "nested", "lengths"} {
					if opt && !optimized(inst, name) || !opt && !nativeRan(inst, name) {
						t.Errorf("%s is not %s code", name, tier)
					}
				}
			})
		}
	}
}

// The arrays of the pools are the ones the interpreter makes: of the class
// their elements take, their lengths those asked for, and what the pools
// hold is in blocks of their class, zero.
func TestJITArrayPoolBlocks(t *testing.T) {
	for _, opt := range []bool{false, true} {
		inst := arrayPoolInstance(t, 0, opt)
		if _, err := refCall(t, inst, "defaults", Val{N: 20}); err != nil {
			t.Fatal(err)
		}
		if _, err := refCall(t, inst, "fixed", Val{N: 20}); err != nil {
			t.Fatal(err)
		}
		set := inst.jit.pools.free[0].set
		pools := 0
		for _, p := range set.all {
			if p.class < 0 {
				continue
			}
			pools++
			room := int(arrayRooms[p.class])
			for _, o := range p.objs[:p.n] {
				a := (*Array)(o)
				if a.hdr.rtt != p.rt || a.hdr.kind != ObjArray || a.n != 0 {
					t.Fatalf("opt %v: class %d: a pool's array %+v", opt, p.class, *a)
				}
				if room > 0 && uintptr(a.e) != uintptr(o)+uintptr(arrayHeader) || room == 0 && a.e != nil {
					t.Fatalf("opt %v: class %d: elements at %p of %p", opt, p.class, a.e, o)
				}
				if p.each != int64(arrayHeader+room) {
					t.Fatalf("opt %v: class %d: each charged %d", opt, p.class, p.each)
				}
			}
		}
		if pools == 0 {
			t.Fatalf("opt %v: no pools of arrays", opt)
		}
	}
}

// Native code making arrays while the collector runs keeps every one of
// them and every reference their elements hold. The test runs again in a
// process of its own with GODEBUG=gccheckmark=1.
func TestJITArrayPoolsUnderCollection(t *testing.T) {
	want, _ := refCall(t, arrayPoolInstance(t, math.MaxInt32, false), "keep", Val{N: 3000})
	wantFixed, _ := refCall(t, arrayPoolInstance(t, math.MaxInt32, false), "fixed", Val{N: 500})
	for _, opt := range []bool{false, true} {
		inst := arrayPoolInstance(t, 0, opt)
		var stop atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			for !stop.Load() {
				runtime.GC()
			}
		}()
		rounds := 30
		if testing.Short() {
			rounds = 4
		}
		for r := 0; r < rounds; r++ {
			res, err := refCall(t, inst, "keep", Val{N: 3000})
			if err == nil && res[0] == want[0] {
				res, err = refCall(t, inst, "fixed", Val{N: 500})
				if err == nil && res[0] != wantFixed[0] {
					err = errors.New("fixed: wrong sum")
				}
			}
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
	if os.Getenv("QUICKJS_WASM_GCCHECK_CHILD") != "" || testing.Short() {
		return
	}
	cmd := osexec.Command(os.Args[0], "-test.run=^TestJITArrayPoolsUnderCollection$", "-test.count=1")
	cmd.Env = append(os.Environ(), "QUICKJS_WASM_GCCHECK_CHILD=1", "GODEBUG=gccheckmark=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("with gccheckmark: %v\n%s", err, out)
	}
}

// Many goroutines making arrays in one instance at once each take a table
// of pools of their own, and those calling back into the instance through
// the host share theirs with the level below: every result is right.
func TestJITArrayPoolsConcurrent(t *testing.T) {
	ref := arrayPoolInstance(t, math.MaxInt32, false)
	wantNews, _ := refCall(t, ref, "news", Val{N: 50})
	wantNested, _ := refCall(t, ref, "nested", Val{N: 3})
	for _, opt := range []bool{false, true} {
		inst := arrayPoolInstance(t, 0, opt)
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for r := 0; r < 20; r++ {
					res, err := refCall(t, inst, "news", Val{N: 50})
					if err == nil && res[0] != wantNews[0] {
						err = errors.New("news: wrong sum")
					}
					if err == nil {
						res, err = refCall(t, inst, "nested", Val{N: 3})
						if err == nil && res[0] != wantNested[0] {
							err = errors.New("nested: wrong sum")
						}
					}
					if err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("opt %v: %v", opt, err)
		}
		if n := len(inst.jit.pools.free); n == 0 || n > jitPoolsKept {
			t.Fatalf("opt %v: the instance keeps %d tables", opt, n)
		}
	}
}

// A fill of a pool of arrays charges the instance's budget for every
// array's block at once: arrays made and dropped never stop a program,
// arrays kept past the limit do, with ErrMemoryLimit; what the pools hold
// when they are collected, never reachable, is given back, unless the
// budget counted what is reachable since the fill.
func TestJITArrayPoolsBudget(t *testing.T) {
	const src = `(module
		(type $refs (array (mut anyref)))
		(type $i32s (array (mut i32)))
		(global $keep (mut (ref null $refs)) (ref.null $refs))
		(func (export "churn") (param i32)
			(loop $l
				(drop (array.new_fixed $i32s 3 (i32.const 1) (i32.const 2) (local.get 0)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1))))))
		(func (export "hoard") (param i32)
			(loop $l
				(global.set $keep (array.new_fixed $refs 2 (global.get $keep) (ref.i31 (local.get 0))))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1))))))
		(func (export "hoardLength") (param i32) (param $len i32)
			(local $a (ref null $refs))
			(loop $l
				(local.set $a (array.new_default $refs (local.get $len)))
				(array.set $refs (local.get $a) (i32.const 0) (global.get $keep))
				(global.set $keep (local.get $a))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1))))))
		(func (export "one") (drop (array.new_default $i32s (i32.const 5)))))`
	// Five i32s take a block with room for 32 bytes.
	each := int64(arrayHeader + 32)
	for _, opt := range []bool{false, true} {
		b := NewBudget(1 << 20)
		inst := jitBudgetInstance(t, src, b, opt)
		if _, err := callExport(t, inst, "churn", Val{N: 1_000_000}); err != nil {
			t.Fatalf("opt %v: churn: %v", opt, err)
		}
		if _, err := callExport(t, inst, "hoard", Val{N: 1_000_000}); !errors.Is(err, ErrMemoryLimit) {
			t.Fatalf("opt %v: hoard: got %v, want ErrMemoryLimit", opt, err)
		}
		// Arrays of a length not constant, from pools and made by the
		// engine past them.
		for _, length := range []uint64{3, 1000} {
			inst := jitBudgetInstance(t, src, NewBudget(1<<20), opt)
			if _, err := callExport(t, inst, "hoardLength", Val{N: 1_000_000}, Val{N: length}); !errors.Is(err, ErrMemoryLimit) {
				t.Fatalf("opt %v: hoardLength %d: got %v, want ErrMemoryLimit", opt, length, err)
			}
		}

		// One array made: its pool's first fill is charged whole, and
		// what is left of it given back once the instance is gone.
		for _, count := range []bool{false, true} {
			b := NewBudget(1 << 20)
			inst := jitBudgetInstance(t, src, b, opt)
			if _, err := callExport(t, inst, "one"); err != nil {
				t.Fatal(err)
			}
			if got := b.Used(); got != gcPoolFirst*each {
				t.Fatalf("opt %v: %d bytes used after one array, want %d", opt, got, gcPoolFirst*each)
			}
			if count {
				b.recount(nil, 0)
				inst.jit.pools.free[0].set.giveBack()
				if got := b.Used(); got != 0 {
					t.Fatalf("opt %v: %d bytes used after a count and the pools given back, want 0", opt, got)
				}
				continue
			}
			inst = nil
			deadline := time.Now().Add(10 * time.Second)
			for b.Used() != each && time.Now().Before(deadline) {
				runtime.GC()
				time.Sleep(time.Millisecond)
			}
			if got := b.Used(); got != each {
				t.Fatalf("opt %v: %d bytes used once the instance is gone, want %d", opt, got, each)
			}
		}
	}
}

// Arrays of many classes fill many pools, some 115, more than the bound on
// what a table's pools hold takes at their largest: what they hold is
// counted again then, so that the fills stay as large as what is left of
// the bound allows, rather than shrink to an array each as the pools'
// last fills, long taken, use it up (some 2,900 fills here without the
// count, 1,700 with it).
func TestJITArrayPoolsManyClasses(t *testing.T) {
	const n = 4000
	makes := int64(0)
	for i := range n {
		for _, past := range []bool{i%130 > 120, i%35 > 30, 7*i%500 > 480, i%250 > 240, i%70 > 60} {
			if past {
				makes++
			}
		}
	}
	for _, opt := range []bool{false, true} {
		inst := arrayPoolInstance(t, 0, opt)
		if _, err := refCall(t, inst, "lengths", Val{N: n}); err != nil {
			t.Fatal(err)
		}
		alloc, _ := countAllocExits(func() {
			if _, err := refCall(t, inst, "lengths", Val{N: n}); err != nil {
				t.Fatal(err)
			}
		})
		if fills := alloc - makes; fills < 0 || fills > 2200 {
			t.Fatalf("opt %v: %d fills for %d arrays from pools", opt, fills, 5*n-makes)
		}
	}
}
