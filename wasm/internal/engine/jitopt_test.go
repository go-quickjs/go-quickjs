//go:build quickjs_jit && !android && !ios && amd64 && (linux || windows)

package engine

import (
	"testing"
)

// tierInstance instantiates a module given in the text format whose
// functions get baseline code at their first call and optimized code after
// optThreshold calls of it.
func tierInstance(t *testing.T, src string, optThreshold int) *Instance {
	t.Helper()
	inst := thresholdInstance(t, src, 0)
	inst.Code.setOSRCalls(0)
	inst.Code.SetOptThreshold(optThreshold)
	return inst
}

// optimized reports whether function name runs the optimizing tier's code.
func optimized(inst *Instance, name string) bool {
	ext, _ := inst.Export(name)
	nat := inst.Code.get(ext.Func.Index).native.Load()
	return nat != nil && nat.opt
}

// Baseline code counts its calls -- native calls from baseline code and
// from optimized code, and calls from Go -- and is replaced by optimized
// code past the threshold: a call that runs out the count runs the
// optimized code from its start, frames that began in the baseline code
// run on in it, and the results are the interpreter's.
func TestOptTierUp(t *testing.T) {
	src := `(module
  (memory 1)
  (func $leaf (export "leaf") (param i32) (result i32)
    (i32.add (i32.mul (local.get 0) (i32.const 3)) (i32.const 1)))
  (func (export "run") (param $n i32) (result i32)
    (local $i i32) (local $acc i32)
    (loop $l
      (local.set $acc (i32.xor (local.get $acc) (call $leaf (local.get $i))))
      (i32.store (i32.and (i32.mul (local.get $i) (i32.const 4)) (i32.const 0xfff)) (local.get $acc))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (i32.add (local.get $acc) (i32.load (i32.const 40))))
  (func $fact (export "fact") (param i64) (result i64)
    (if (result i64) (i64.le_u (local.get 0) (i64.const 1))
      (then (i64.const 1))
      (else (i64.mul (local.get 0) (call $fact (i64.sub (local.get 0) (i64.const 1)))))))
  (func (export "spin") (param $n i32) (result i64)
    (local $i i32) (local $x i64)
    (loop $l
      (local.set $x (i64.add (i64.rotl (local.get $x) (i64.const 7)) (i64.extend_i32_u (local.get $i))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $x)))`
	interp := interpretedInstance(t, src)
	check := func(inst *Instance, name string, args ...uint64) {
		t.Helper()
		want, err := call(t, interp, name, args...)
		if err != nil {
			t.Fatal(err)
		}
		got, err := call(t, inst, name, args...)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].N != want[0].N {
			t.Fatalf("%s(%v) = %#x, want %#x", name, args, got[0].N, want[0].N)
		}
	}

	// A leaf called natively from a loop.
	inst := tierInstance(t, src, 50)
	before := ReadJITStats().Optimized
	check(inst, "run", 20)
	if optimized(inst, "leaf") {
		t.Fatal("leaf optimized after 20 calls of a threshold of 50")
	}
	check(inst, "run", 100)
	if !optimized(inst, "leaf") {
		t.Fatal("leaf not optimized after 120 calls")
	}
	check(inst, "run", 1000)
	check(inst, "leaf", 77)
	if n := ReadJITStats().Optimized - before; n < 1 {
		t.Fatalf("%d functions optimized", n)
	}

	// Recursion: the count runs out deep in it; the frames above go on in
	// the baseline code, those below in the optimized code.
	inst = tierInstance(t, src, 5)
	check(inst, "fact", 20)
	if !optimized(inst, "fact") {
		t.Fatal("fact not optimized after 20 calls")
	}
	check(inst, "fact", 25)

	// A loop that runs long, in a function called once: its poll exits
	// count, and its next call runs the optimized code.
	inst = tierInstance(t, src, 1000)
	check(inst, "spin", 100000)
	if !optimized(inst, "spin") {
		t.Fatal("spin not optimized after a long loop")
	}
	check(inst, "spin", 100000)

	// Off, nothing is optimized.
	inst = tierInstance(t, src, -1)
	check(inst, "run", 5000)
	if optimized(inst, "leaf") || optimized(inst, "run") {
		t.Fatal("optimized with the tier off")
	}

	// At 0, every function the tier accepts is optimized at its first
	// compile.
	inst = tierInstance(t, src, 0)
	check(inst, "run", 50)
	if !optimized(inst, "leaf") || !optimized(inst, "run") {
		t.Fatal("not optimized at the first compile")
	}
}

// A call running in the interpreter enters the baseline code's loops,
// which the optimized code has none of, when the function has only
// optimized code: the engine compiles the baseline code for them.
func TestOptLoopEntries(t *testing.T) {
	src := `(module
  (func (export "spin") (param $n i32) (result i64)
    (local $i i32) (local $x i64)
    (loop $l
      (local.set $x (i64.add (i64.rotl (local.get $x) (i64.const 7)) (i64.extend_i32_u (local.get $i))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $x)))`
	interp := interpretedInstance(t, src)
	inst := thresholdInstance(t, src, 3)
	inst.Code.setOSRCalls(0)
	inst.Code.SetOSRThreshold(10)
	inst.Code.SetOptThreshold(0)
	before := osrCount()
	for k := 0; k < 6; k++ {
		want, err := call(t, interp, "spin", 5000)
		if err != nil {
			t.Fatal(err)
		}
		got, err := call(t, inst, "spin", 5000)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].N != want[0].N {
			t.Fatalf("call %d: %#x, want %#x", k, got[0].N, want[0].N)
		}
	}
	if osrCount() == before {
		t.Fatal("no call went on natively from the loop")
	}
	if !optimized(inst, "spin") {
		t.Fatal("spin not optimized")
	}
}
