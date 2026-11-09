package engine

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// osrInstance instantiates a module given in the text format whose
// functions are compiled after callThreshold calls, and whose calls running
// in the interpreter go on in native code after osrThreshold back edges --
// whatever WASM_JIT says -- or skips without native code.
func osrInstance(t testing.TB, src string, callThreshold, osrThreshold int, imports ...Extern) *Instance {
	t.Helper()
	inst := thresholdInstance(t, src, callThreshold, imports...)
	inst.Code.SetOSRThreshold(osrThreshold)
	inst.Code.setOSRCalls(0)
	return inst
}

// osrCount returns how many calls have gone on in native code from a loop.
func osrCount() int { return ReadJITStats().OSR }

// checkOSR calls name with args on an instance whose functions are never
// compiled at a call and enter native code from a loop at its fifth back
// edge, and on one that only interprets, and compares the results; it
// reports how many calls went on from a loop.
func checkOSR(t *testing.T, src, name string, want uint64, args ...uint64) int {
	t.Helper()
	interp := osrInstance(t, src, math.MaxInt32, -1)
	if res, err := call(t, interp, name, args...); err != nil || res[0].N != want {
		t.Fatalf("interpreted %s%v = %v, %v; want %d", name, args, res, err, want)
	}
	inst := osrInstance(t, src, math.MaxInt32, 5)
	before := osrCount()
	res, err := call(t, inst, name, args...)
	if err != nil || res[0].N != want {
		t.Fatalf("%s%v = %v, %v; want %d", name, args, res, err, want)
	}
	return osrCount() - before
}

// A loop deep in blocks, an if and a block chain goes on natively, and the
// native code runs what follows the loop in each construct out to the
// function's end.
func TestOSRNestedBlocks(t *testing.T) {
	src := `(module
  (func (export "f") (param $n i32) (result i32) (local $i i32) (local $acc i32)
    (block $outer
      (block $mid
        (if (i32.gt_s (local.get $n) (i32.const 0))
          (then
            (block $done
              (loop $l
                (local.set $acc (i32.add (local.get $acc) (i32.mul (local.get $i) (local.get $i))))
                (local.set $i (i32.add (local.get $i) (i32.const 1)))
                (br_if $l (i32.lt_s (local.get $i) (local.get $n))))
              ;; After the loop, still inside its blocks.
              (local.set $acc (i32.add (local.get $acc) (i32.const 1000))))
            (local.set $acc (i32.mul (local.get $acc) (i32.const 3)))))
        (local.set $acc (i32.add (local.get $acc) (i32.const 7))))
      (local.set $acc (i32.xor (local.get $acc) (i32.const 0x55))))
    (i32.sub (local.get $acc) (i32.const 1))))`
	var acc uint32
	for i := uint32(0); i < 100; i++ {
		acc += i * i
	}
	want := uint64(((acc+1000)*3+7)^0x55) - 1
	if n := checkOSR(t, src, "f", want, 100); n != 1 {
		t.Fatalf("%d calls went on natively, want 1", n)
	}
}

// A branch out of the loop goes on natively to the enclosing block it
// names, its value with it, and one to the function's label returns.
func TestOSRBranchesOut(t *testing.T) {
	src := `(module
  (func (export "toBlock") (param $n i32) (result i32) (local $i i32)
    (i32.add
      (block $out (result i32)
        (i32.const 100)
        (loop $l (param i32) (result i32)
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (i32.add (local.get $i))
          ;; Out of the loop, to $out, with the sum.
          (br_if $out (i32.ge_s (local.get $i) (local.get $n)))
          (br $l))
        (unreachable))
      (i32.const 1)))
  (func (export "toFunction") (param $n i32) (result i32) (local $i i32)
    (i32.const 9)
    (block $b
      (loop $l
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        ;; Out to the function's label: a return of $i * 2.
        (br_if 2 (i32.mul (local.get $i) (i32.const 2)) (i32.ge_s (local.get $i) (local.get $n)))
        (br $l)))
    (drop)
    (i32.const -1)))`
	want := uint64(100 + 50*51/2 + 1)
	if n := checkOSR(t, src, "toBlock", want, 50); n != 1 {
		t.Fatalf("toBlock: %d calls went on natively, want 1", n)
	}
	if n := checkOSR(t, src, "toFunction", 80, 40); n != 1 {
		t.Fatalf("toFunction: %d calls went on natively, want 1", n)
	}
	// Fewer back edges than the threshold: no entry.
	if n := checkOSR(t, src, "toFunction", 6, 3); n != 0 {
		t.Fatalf("toFunction(3): %d calls went on natively, want 0", n)
	}
}

// A loop with parameters and results, below other stack entries, goes on
// natively with its parameters where the back edge left them.
func TestOSRLoopParamsResults(t *testing.T) {
	src := `(module
  (func (export "f") (param $n i64) (result i64) (local $a i64)
    (i64.const 1000000)
    (f64.const 0.5)
    (i64.const 0) (i64.const 1)
    ;; fib by a loop carrying (a, b), counting $n down.
    (loop $l (param i64 i64) (result i64 i64)
      (local.set $n (i64.sub (local.get $n) (i64.const 1)))
      (if (param i64 i64) (result i64 i64) (i64.gt_s (local.get $n) (i64.const 0))
        (then
          (call $step)
          (br $l))))
    (drop)
    (local.set $a)
    (i64.trunc_f64_s (f64.mul (f64.const 4)))
    (i64.add)
    (i64.add (local.get $a)))
  (func $step (param i64 i64) (result i64 i64)
    (local.get 1) (i64.add (local.get 0) (local.get 1))))`
	// After 59 steps a is fib(59), added to 1000000 and 0.5*4 below it.
	var a, b uint64 = 0, 1
	for i := 0; i < 59; i++ {
		a, b = b, a+b
	}
	want := 1000002 + a
	if n := checkOSR(t, src, "f", want, 60); n != 1 {
		t.Fatalf("%d calls went on natively, want 1", n)
	}
}

// A function the JIT refuses -- here for a try_table -- stays in the
// interpreter, compiled once, and its loops stop asking.
func TestOSRRefusedStaysInterpreted(t *testing.T) {
	src := `(module
  (tag $e (param i32))
  (func (export "f") (param $n i32) (result i32) (local $i i32) (local $acc i32)
    (block $h (result i32)
      (try_table (catch $e $h)
        (loop $l
          (local.set $acc (i32.add (local.get $acc) (local.get $i)))
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (br_if $l (i32.lt_s (local.get $i) (local.get $n))))
        (throw $e (local.get $acc)))
      (unreachable))
    (loop $m
      (local.set $i (i32.sub (local.get $i) (i32.const 1)))
      (br_if $m (i32.gt_s (local.get $i) (i32.const 0))))
    (i32.add (local.get $i))))`
	before := ReadJITStats().Refused["exceptions"]
	for k := 0; k < 3; k++ {
		if n := checkOSR(t, src, "f", 4950, 100); n != 0 {
			t.Fatalf("%d calls went on natively, want 0", n)
		}
	}
	// One instance a check: each asked once, in the call that ran first.
	if got := ReadJITStats().Refused["exceptions"] - before; got != 3 {
		t.Fatalf("%d refusals, want 3", got)
	}
	inst := osrInstance(t, src, math.MaxInt32, 0)
	for k := 0; k < 4; k++ {
		if res, err := call(t, inst, "f", 100); err != nil || res[0].N != 4950 {
			t.Fatalf("f(100) = %v, %v", res, err)
		}
	}
	if got := ReadJITStats().Refused["exceptions"] - before; got != 4 {
		t.Fatalf("%d refusals after 4 more calls, want 4: the function was asked for again", got)
	}
	if nativeRan(inst, "f") {
		t.Fatal("f has native code")
	}
}

// A trap in the native code a loop went on in reports the frames the
// interpreter would: the trapping function at the trapping instruction,
// under its interpreted caller at its call.
func TestOSRTrapTrace(t *testing.T) {
	src := `(module
  (memory 1)
  (func $inner (param $n i32) (result i32) (local $i i32) (local $acc i32)
    (loop $l
      (local.set $acc (i32.add (local.get $acc) (i32.load (local.get $i))))
      (local.set $i (i32.add (local.get $i) (i32.const 4096)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $acc))
  (func (export "outer") (param i32) (result i32)
    (i32.add (i32.const 1) (call $inner (local.get 0)))))`
	var frames [2][]Frame
	insts := []*Instance{osrInstance(t, src, math.MaxInt32, -1), osrInstance(t, src, math.MaxInt32, 5)}
	for k, inst := range insts {
		before := osrCount()
		_, err := call(t, inst, "outer", 1<<20)
		var tr *Trap
		if !errors.As(err, &tr) || tr.Code != TrapMemOutOfBounds {
			t.Fatalf("outer(1<<20) = %v, want an out-of-bounds trap", err)
		}
		if k == 1 && osrCount()-before != 1 {
			t.Fatalf("%d calls went on natively, want 1", osrCount()-before)
		}
		frames[k] = tr.Frames
	}
	if len(frames[1]) != 2 || len(frames[0]) != 2 || frames[0][1].Offset != frames[1][1].Offset {
		t.Fatalf("trap frames %+v, interpreted %+v", frames[1], frames[0])
	}
	for _, fr := range frames {
		if fr[0].Func != 0 || fr[1].Func != 1 {
			t.Fatalf("trap frames %+v, want inner under outer", fr)
		}
	}
	// Native code knows the trapping instruction too: the load.
	if op := insts[1].Code.Module.Bytes[frames[1][0].Offset]; op != 0x28 {
		t.Fatalf("the trap is at offset %d, an instruction 0x%02x, not i32.load", frames[1][0].Offset, op)
	}
}

// A loop calling the host, which calls back into wasm, goes on natively,
// its host calls made through Go; an i32 the host returns with its upper
// half set is read as the i32 it is.
func TestOSRHostCalls(t *testing.T) {
	var inst *Instance
	calls := 0
	host := NewHostFunction(module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}, Results: []module.ValueType{module.I32}}), "h",
		func(ctx context.Context, _ *Instance, args, results []Val) error {
			calls++
			ext, _ := inst.Export("sq")
			res, err := Invoke(ctx, ext.Func, []Val{args[0]})
			if err != nil {
				return err
			}
			// The upper half is not the i32's.
			results[0] = Val{N: res[0].N | 0xdead_beef_0000_0000}
			return nil
		})
	src := `(module
  (import "env" "h" (func $h (param i32) (result i32)))
  (func (export "sq") (param i32) (result i32) (i32.mul (local.get 0) (local.get 0)))
  (func (export "f") (param $n i32) (result i64) (local $i i32) (local $acc i64)
    ;; A host result kept on the stack across the loop.
    (call $h (i32.const 3))
    (loop $l
      (local.set $acc (i64.add (local.get $acc) (i64.extend_i32_u (call $h (local.get $i)))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (i64.extend_i32_u)
    (i64.add (local.get $acc))))`
	var want uint64 = 9
	for i := uint64(0); i < 50; i++ {
		want += i * i
	}
	imp := Extern{Kind: module.ExternFunc, Func: host}
	for _, osr := range []bool{false, true} {
		if osr {
			inst = osrInstance(t, src, math.MaxInt32, 5, imp)
		} else {
			inst = osrInstance(t, src, math.MaxInt32, -1, imp)
		}
		calls = 0
		before := osrCount()
		res, err := call(t, inst, "f", 50)
		if err != nil || res[0].N != want {
			t.Fatalf("osr %v: f(50) = %#x, %v; want %d", osr, res, err, want)
		}
		if got := osrCount() - before; calls != 51 || (got == 1) != osr {
			t.Fatalf("osr %v: %d host calls, %d calls went on natively", osr, calls, got)
		}
	}
}

// A function whose loop calls it again goes on natively while copies of it
// are on the stack -- interpreted, native, and gone on natively themselves
// -- each copy in its own frame.
func TestOSRRecursion(t *testing.T) {
	src := `(module
  (func $f (export "f") (param $n i32) (result i32) (local $i i32) (local $acc i32)
    (loop $l
      (local.set $acc (i32.add (local.get $acc) (local.get $i)))
      (if (i32.and (i32.gt_s (local.get $n) (i32.const 0)) (i32.eq (local.get $i) (i32.const 7)))
        (then (local.set $acc (i32.add (local.get $acc) (call $f (i32.sub (local.get $n) (i32.const 1)))))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_s (local.get $i) (i32.const 20))))
    (local.get $acc))
  ;; The recursive call before the loop: the outer frames' loops begin
  ;; after the inner calls compiled the function.
  (func $g (export "g") (param $n i32) (result i32) (local $i i32) (local $acc i32)
    (if (i32.gt_s (local.get $n) (i32.const 0))
      (then (local.set $acc (call $g (i32.sub (local.get $n) (i32.const 1))))))
    (loop $l
      (local.set $acc (i32.add (local.get $acc) (local.get $i)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_s (local.get $i) (i32.const 20))))
    (local.get $acc)))`
	var f func(n int) uint32
	f = func(n int) uint32 {
		var acc uint32
		for i := 0; i < 20; i++ {
			acc += uint32(i)
			if n > 0 && i == 7 {
				acc += f(n - 1)
			}
		}
		return acc
	}
	for _, thresholds := range [][2]int{{math.MaxInt32, 5}, {math.MaxInt32, 0}, {3, 5}, {3, 1000}} {
		inst := osrInstance(t, src, thresholds[0], thresholds[1])
		before := osrCount()
		if res, err := call(t, inst, "f", 12); err != nil || res[0].N != uint64(f(12)) {
			t.Fatalf("thresholds %v: f(12) = %v, %v; want %d", thresholds, res, err, f(12))
		}
		if res, err := call(t, inst, "g", 12); err != nil || res[0].N != 13*190 {
			t.Fatalf("thresholds %v: g(12) = %v, %v; want %d", thresholds, res, err, 13*190)
		}
		got := osrCount() - before
		if thresholds[1] < 20 && got == 0 || thresholds[1] >= 20 && got != 0 {
			t.Fatalf("thresholds %v: %d calls went on natively", thresholds, got)
		}
	}
}

// A call running interpreted under native code -- called through Go from a
// native caller -- goes on natively at a level of its own.
func TestOSRUnderNativeCaller(t *testing.T) {
	src := `(module
  (func $sum (param $n i32) (result i32) (local $i i32) (local $acc i32)
    (loop $l
      (local.set $acc (i32.add (local.get $acc) (local.get $i)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_s (local.get $i) (local.get $n))))
    (local.get $acc))
  (func (export "g") (param $n i32) (result i32)
    (if (result i32) (i32.eqz (local.get $n))
      (then (i32.const -1))
      (else (i32.add (i32.const 1) (call $sum (local.get $n))))))) `
	inst := osrInstance(t, src, 2, 5)
	for k := 0; k < 3; k++ {
		if res, err := call(t, inst, "g", 0); err != nil || res[0].N != math.MaxUint32 {
			t.Fatalf("g(0) = %v, %v", res, err)
		}
	}
	if !nativeRan(inst, "g") {
		t.Fatal("g has no native code")
	}
	before := osrCount()
	if res, err := call(t, inst, "g", 100); err != nil || res[0].N != 4951 {
		t.Fatalf("g(100) = %v, %v; want 4951", res, err)
	}
	if got := osrCount() - before; got != 1 {
		t.Fatalf("%d calls went on natively, want 1", got)
	}
}

// A return_call out of the native code a loop went on in, to a function
// without native code, is made by the interpreter's tail call, in place of
// the frame.
func TestOSRTailCallOut(t *testing.T) {
	src := `(module
  (func $f (export "f") (param $n i32) (result i32) (local $i i32)
    (loop $l
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_s (local.get $i) (local.get $n))))
    (return_call $g (local.get $i)))
  (func $g (param i32) (result i32)
    (i32.const 1) (i32.add (local.get 0) (i32.const 5)) (return)))`
	if n := checkOSR(t, src, "f", 105, 100); n != 1 {
		t.Fatalf("%d calls went on natively, want 1", n)
	}
}

// Goroutines running the same Code's function in instances of their own go
// on natively from its loop at once: one compiles it, and each enters the
// code it placed.
func TestOSRConcurrent(t *testing.T) {
	src := `(module
  (func (export "f") (param $n i32) (result i32) (local $i i32) (local $acc i32)
    (loop $l
      (local.set $acc (i32.add (local.get $acc) (i32.mul (local.get $i) (i32.const 3))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $acc)))`
	first := osrInstance(t, src, math.MaxInt32, 50)
	const workers = 8
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func() {
			inst, err := Instantiate(context.Background(), first.Code, nil)
			if err != nil {
				errs <- err
				return
			}
			ext, _ := inst.Export("f")
			for k := 0; k < 20; k++ {
				n := uint32(1000 + k)
				res, err := Invoke(context.Background(), ext.Func, []Val{{N: uint64(n)}})
				if err != nil || uint32(res[0].N) != 3*n*(n-1)/2 {
					errs <- errors.New("wrong result")
					return
				}
			}
			errs <- nil
		}()
	}
	for w := 0; w < workers; w++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if !nativeRan(first, "f") {
		t.Fatal("f has no native code")
	}
}

// An endless loop that went on natively polls, and ends when the context
// does; a recursion from an OSR'd loop stops at the depth limit, and the
// exec runs on after both.
func TestOSRPollsAndDepth(t *testing.T) {
	src := `(module
  (func (export "spin") (loop $l (br $l)))
  (func $deep (export "deep") (param $n i32) (result i32) (local $i i32)
    (loop $l
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (i32.const 10))))
    (i32.add (call $deep (i32.add (local.get $n) (i32.const 1))) (local.get $i))))`
	inst := osrInstance(t, src, math.MaxInt32, 5)
	before := osrCount()
	ext, _ := inst.Export("spin")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Invoke(ctx, ext.Func, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("spin = %v, want the context's deadline", err)
	}
	_, err := call(t, inst, "deep", 0)
	var tr *Trap
	if !errors.As(err, &tr) || tr.Code != TrapStackOverflow {
		t.Fatalf("deep(0) = %v, want a stack overflow", err)
	}
	if got := osrCount() - before; got != 2 {
		t.Fatalf("%d calls went on natively, want 2", got)
	}
	inst2 := nativeInstance(t, benchWAT)
	if res, err := call(t, inst2, "fib", 10); err != nil || res[0].N != 55 {
		t.Fatalf("fib(10) after = %v, %v", res, err)
	}
}
