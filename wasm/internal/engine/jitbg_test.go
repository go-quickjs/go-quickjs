//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"
)

// bgInstance instantiates a module given in the text format whose
// functions get baseline code at their first call and optimized code,
// compiled in the background, after optThreshold calls.
func bgInstance(t *testing.T, src string, optThreshold int) *Instance {
	t.Helper()
	inst := thresholdInstance(t, src, 0)
	inst.Code.setOSRCalls(0)
	inst.Code.SetOptThreshold(optThreshold)
	inst.Code.SetOptBackground(true)
	return inst
}

// waitOpt waits for the code's compiles in the background, failing the
// test past a deadline.
func waitOpt(t *testing.T, c *Code) {
	t.Helper()
	done := make(chan struct{})
	go func() { c.waitOptBackground(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the compiles in the background did not finish")
	}
}

// A function whose baseline code counted enough calls is compiled in the
// background: the call that ran the count out, and those after it, run
// the baseline code until the optimized code is published, which later
// calls run, with the interpreter's results throughout.
func TestOptBackground(t *testing.T) {
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
      (else (i64.mul (local.get 0) (call $fact (i64.sub (local.get 0) (i64.const 1))))))))`
	interp := interpretedInstance(t, src)
	inst := bgInstance(t, src, 50)
	check := func(name string, args ...uint64) {
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
	before := ReadJITStats().Optimized
	check("run", 20)
	if optimized(inst, "leaf") {
		t.Fatal("leaf optimized after 20 calls of a threshold of 50")
	}
	for range 20 {
		check("run", 100)
		check("fact", 20)
	}
	waitOpt(t, inst.Code)
	if !optimized(inst, "leaf") || !optimized(inst, "fact") {
		t.Fatalf("not optimized once the background is done: leaf %v, fact %v", optimized(inst, "leaf"), optimized(inst, "fact"))
	}
	if n := ReadJITStats().Optimized - before; n < 2 {
		t.Fatalf("%d functions optimized", n)
	}
	// The optimized code runs, and the baseline code it replaced is kept
	// for the frames that began in it.
	ext, _ := inst.Export("leaf")
	if nat := inst.Code.get(ext.Func.Index).native.Load(); nat.prev == nil || nat.prev.opt {
		t.Fatal("the optimized code does not keep the baseline code it replaced")
	}
	check("run", 1000)
	check("fact", 25)
}

// The optimized code is placed and published while native code of the
// Code runs -- here a read of the run lock held throughout, as a call
// running a long loop holds it -- since it goes on pages no running code
// is in.
func TestOptBackgroundPlacesWhileCodeRuns(t *testing.T) {
	src := `(module
  (func $sq (export "sq") (param i64) (result i64) (i64.mul (local.get 0) (local.get 0)))
  (func (export "sum") (param $n i64) (result i64)
    (local $acc i64)
    (loop $l
      (local.set $acc (i64.add (local.get $acc) (call $sq (local.get $n))))
      (br_if $l (i64.ne (local.tee $n (i64.sub (local.get $n) (i64.const 1))) (i64.const 0))))
    (local.get $acc)))`
	inst := bgInstance(t, src, 100)
	// Every function has its baseline code: no compile on the spot is left.
	if _, err := call(t, inst, "sum", 1); err != nil {
		t.Fatal(err)
	}
	cj := inst.Code.jit.Load()
	cj.run.RLock()
	defer cj.run.RUnlock()
	got, err := call(t, inst, "sum", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(1000 * 1001 * 2001 / 6); got[0].N != want {
		t.Fatalf("sum(1000) = %d, want %d", got[0].N, want)
	}
	waitOpt(t, inst.Code)
	if !optimized(inst, "sq") {
		t.Fatal("sq not optimized")
	}
	if got, _ := call(t, inst, "sum", 10); got[0].N != 385 {
		t.Fatalf("sum(10) = %d, want 385", got[0].N)
	}
}

// stressModule is a module of n functions that call each other through a
// table and directly, with loops and memory, for many functions to cross
// the threshold at different times on different goroutines.
func stressModule(n int) string {
	var b strings.Builder
	b.WriteString("(module\n  (type $f (func (param i64) (result i64)))\n  (memory 1)\n")
	fmt.Fprintf(&b, "  (table %d funcref)\n  (elem (i32.const 0)", n)
	for i := range n {
		fmt.Fprintf(&b, " $f%d", i)
	}
	b.WriteString(")\n")
	for i := range n {
		fmt.Fprintf(&b, "  (func $f%d (type $f) (local $i i32) (local $x i64)\n", i)
		fmt.Fprintf(&b, "    (local.set $x (i64.add (local.get 0) (i64.const %d)))\n", i*7919+1)
		// A short loop of its own, a different length for each.
		fmt.Fprintf(&b, "    (loop $l\n      (local.set $x (i64.xor (i64.rotl (local.get $x) (i64.const %d)) (i64.extend_i32_u (local.get $i))))\n", i%61+1)
		fmt.Fprintf(&b, "      (i64.store (i32.and (i32.mul (local.get $i) (i32.const 8)) (i32.const 0x3ff8)) (local.get $x))\n")
		fmt.Fprintf(&b, "      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (i32.const %d))))\n", i%5+1)
		if i > 0 && i%3 != 0 {
			fmt.Fprintf(&b, "    (local.set $x (i64.add (local.get $x) (call $f%d (i64.shr_u (local.get $x) (i64.const 3)))))\n", i-1)
		}
		b.WriteString("    (i64.add (local.get $x) (i64.load (i32.const 64))))\n")
	}
	b.WriteString(`  (func (export "run") (param $n i32) (param $seed i64) (result i64)
    (local $acc i64)
    (loop $l
      (local.set $seed (i64.add (i64.mul (local.get $seed) (i64.const 6364136223846793005)) (i64.const 1442695040888963407)))
      (local.set $acc (i64.xor (local.get $acc)
        (call_indirect (type $f) (local.get $acc)
          (i32.wrap_i64 (i64.rem_u (i64.shr_u (local.get $seed) (i64.const 33)) (i64.const `)
	fmt.Fprintf(&b, "%d", n)
	b.WriteString(`))))))
      (br_if $l (i32.ne (local.tee $n (i32.sub (local.get $n) (i32.const 1))) (i32.const 0))))
    (local.get $acc)))`)
	return b.String()
}

// Many functions cross the threshold on several goroutines at once, each
// running an instance of one of two Codes, while the collector runs: their
// compiles finish in the background and their code is published while the
// others run, and every call's result is the interpreter's.
func TestOptBackgroundStress(t *testing.T) {
	src := stressModule(48)
	interp := interpretedInstance(t, src)
	const goroutines, rounds, calls = 6, 150, 200
	want := make([][]uint64, goroutines)
	for g := range want {
		for r := range rounds {
			res, err := call(t, interp, "run", calls, uint64(g*1000+r))
			if err != nil {
				t.Fatal(err)
			}
			want[g] = append(want[g], res[0].N)
		}
	}
	first, second := bgInstance(t, src, 25), bgInstance(t, src, 40)
	before := ReadJITStats().Optimized
	stop := make(chan struct{})
	var gcs atomic.Int64
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
				gcs.Add(1)
			}
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := range goroutines {
		inst := first
		if g%2 == 1 {
			inst = second
		}
		if g > 1 {
			var err error
			if inst, err = Instantiate(context.Background(), inst.Code, nil); err != nil {
				t.Fatal(err)
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ext, _ := inst.Export("run")
			for r := range rounds {
				res, err := Invoke(context.Background(), ext.Func, []Val{{N: calls}, {N: uint64(g*1000 + r)}})
				if err != nil {
					errs <- err
					return
				}
				if res[0].N != want[g][r] {
					errs <- fmt.Errorf("goroutine %d, round %d: %#x, want %#x", g, r, res[0].N, want[g][r])
					return
				}
			}
		}()
	}
	wg.Wait()
	waitOpt(t, first.Code)
	waitOpt(t, second.Code)
	close(stop)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	n := ReadJITStats().Optimized - before
	t.Logf("%d functions optimized in the background, %d collections", n, gcs.Load())
	if n < 20 {
		t.Fatalf("only %d functions optimized", n)
	}
	// Once published, the optimized code gives the same results.
	ext, _ := first.Export("run")
	for r := range rounds {
		res, err := Invoke(context.Background(), ext.Func, []Val{{N: calls}, {N: uint64(r)}})
		if err != nil {
			t.Fatal(err)
		}
		if res[0].N != want[0][r] {
			t.Fatalf("round %d after the compiles: %#x, want %#x", r, res[0].N, want[0][r])
		}
	}
}

// A Code collected while its functions wait for a worker is compiled for no
// more: its requests are dropped, its arena given back, and nothing is
// placed in it after.
func TestOptBackgroundCodeCollected(t *testing.T) {
	waitOptWorkers()
	gate := make(chan struct{})
	optWorkerHook = func() { <-gate }
	defer func() { optWorkerHook = nil }()
	opened := false
	defer func() {
		if !opened {
			close(gate)
		}
	}()

	src := stressModule(16)
	cj, owner := func() (*codeJIT, weak.Pointer[Code]) {
		inst := bgInstance(t, src, 5)
		for r := range 10 {
			if _, err := call(t, inst, "run", 50, uint64(r)); err != nil {
				t.Fatal(err)
			}
		}
		return inst.Code.jit.Load(), weak.Make(inst.Code)
	}()
	if cj.bg.pending.Load() == 0 {
		t.Fatal("no function waits for a worker")
	}
	for i := 0; owner.Value() != nil; i++ {
		if i == 100 {
			t.Fatal("the Code is not collected while its functions wait for a worker")
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
	// Its cleanup gives the arena back and drops the queued requests.
	for i := 0; ; i++ {
		cj.mu.Lock()
		closed := cj.bg.closed
		cj.mu.Unlock()
		if closed {
			break
		}
		if i == 1000 {
			t.Fatal("the Code's cleanup did not run")
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	opened = true
	waitOptWorkers()
	if n := cj.bg.pending.Load(); n != 0 {
		t.Fatalf("%d requests left of a collected Code", n)
	}
	optWorkers.mu.Lock()
	for _, q := range optWorkers.ring {
		if q == cj {
			t.Error("a collected Code is still in the workers' ring")
		}
	}
	optWorkers.mu.Unlock()
	if mapped, live := cj.arena.Mapped(); mapped != 0 || live != 0 {
		t.Fatalf("a collected Code's arena maps %d bytes, %d live", mapped, live)
	}
}
