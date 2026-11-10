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

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// poolWAT makes structs as native code makes them, from the engine's
// pools: trees of two reference fields, chains of structs of every storage
// type, structs of their defaults, and trees made on both sides of a call
// of the host that makes trees in the same instance.
const poolWAT = `(module
  (import "env" "back" (func $back (param i32) (result i32)))
  (type $node (struct (field $l (ref null $node)) (field $r (ref null $node))))
  (type $all (struct (field (mut i8)) (field (mut i16)) (field (mut i32)) (field (mut i64))
    (field (mut f32)) (field (mut f64)) (field (mut anyref)) (field (mut (ref null $all)))))
  (global $keep (mut (ref null $node)) (ref.null $node))
  (func $make (param $d i32) (result (ref $node))
    (if (result (ref $node)) (i32.eqz (local.get $d))
      (then (struct.new $node (ref.null $node) (ref.null $node)))
      (else (struct.new $node
        (call $make (i32.sub (local.get $d) (i32.const 1)))
        (call $make (i32.sub (local.get $d) (i32.const 1)))))))
  (func $check (param $t (ref null $node)) (result i32)
    (if (result i32) (ref.is_null (struct.get $node $l (local.get $t)))
      (then (i32.const 1))
      (else (i32.add (i32.const 1)
        (i32.add (call $check (struct.get $node $l (local.get $t)))
                 (call $check (struct.get $node $r (local.get $t))))))))
  (func $trees (export "trees") (param $d i32) (param $n i32) (result i32)
    (local $i i32) (local $s i32)
    (loop $l
      (local.set $s (i32.add (local.get $s) (call $check (call $make (local.get $d)))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $s))
  (func (export "keep") (param $d i32) (result i32)
    (global.set $keep (call $make (local.get $d)))
    (call $check (global.get $keep)))
  (func (export "kept") (result i32) (call $check (global.get $keep)))
  (func (export "alls") (param $n i32) (result i64)
    (local $i i32) (local $p (ref null $all)) (local $s i64)
    (loop $l
      (local.set $p (struct.new $all (local.get $i) (i32.mul (local.get $i) (i32.const 3))
        (i32.mul (local.get $i) (i32.const 7)) (i64.mul (i64.extend_i32_u (local.get $i)) (i64.extend_i32_u (local.get $i)))
        (f32.convert_i32_s (local.get $i)) (f64.mul (f64.convert_i32_s (local.get $i)) (f64.const 0.5))
        (ref.i31 (local.get $i)) (local.get $p)))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (block $done
      (loop $w
        (br_if $done (ref.is_null (local.get $p)))
        (local.set $s (i64.add (local.get $s)
          (i64.add
            (i64.add (i64.extend_i32_s (struct.get_s $all 0 (local.get $p))) (i64.extend_i32_u (struct.get_u $all 1 (local.get $p))))
            (i64.add
              (i64.add (i64.extend_i32_u (struct.get $all 2 (local.get $p))) (struct.get $all 3 (local.get $p)))
              (i64.add
                (i64.add (i64.trunc_f32_s (struct.get $all 4 (local.get $p))) (i64.trunc_f64_s (struct.get $all 5 (local.get $p))))
                (i64.extend_i32_s (i31.get_s (ref.cast (ref i31) (struct.get $all 6 (local.get $p))))))))))
        (local.set $p (struct.get $all 7 (local.get $p)))
        (br $w)))
    (local.get $s))
  (func (export "defaults") (param $n i32) (result i32)
    (local $i i32) (local $p (ref null $all)) (local $s i32)
    (loop $l
      (local.set $p (struct.new_default $all))
      (local.set $s (i32.add (local.get $s)
        (i32.add (i32.add (struct.get_u $all 0 (local.get $p)) (struct.get $all 2 (local.get $p)))
          (i32.add (ref.is_null (struct.get $all 6 (local.get $p))) (ref.is_null (struct.get $all 7 (local.get $p)))))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $s))
  (func (export "nested") (param $d i32) (result i32)
    (local $a (ref null $node))
    (local.set $a (call $make (local.get $d)))
    (i32.add (call $back (local.get $d))
      (i32.add (call $check (local.get $a)) (call $check (call $make (local.get $d)))))))`

// poolCases are poolWAT's calls.
func poolCases() []jitCase {
	return []jitCase{
		{"trees", []Val{{N: 0}, {N: 1}}}, {"trees", []Val{{N: 6}, {N: 50}}}, {"trees", []Val{{N: 10}, {N: 3}}},
		{"keep", []Val{{N: 8}}}, {"trees", []Val{{N: 4}, {N: 100}}}, {"kept", nil},
		{"alls", []Val{{N: 1}}}, {"alls", []Val{{N: 1000}}}, {"defaults", []Val{{N: 1000}}},
		{"nested", []Val{{N: 5}}}, {"nested", []Val{{N: 0}}},
	}
}

// poolInstance instantiates poolWAT, its host function calling trees back
// in the same instance, interpreted (threshold math.MaxInt32), in the
// baseline's code, or optimized.
func poolInstance(t testing.TB, threshold int, opt bool) *Instance {
	t.Helper()
	var inst *Instance
	host := NewHostFunction(module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}, Results: []module.ValueType{module.I32}}), "back",
		func(ctx context.Context, _ *Instance, args, results []Val) error {
			ext, _ := inst.Export("trees")
			res, err := Invoke(ctx, ext.Func, []Val{args[0], {N: 3}})
			if err != nil {
				return err
			}
			results[0] = res[0]
			return nil
		})
	inst = thresholdInstance(t, poolWAT, threshold, Extern{Kind: module.ExternFunc, Func: host})
	if opt {
		inst.Code.SetOptThreshold(0)
		inst.Code.SetOptBackground(false)
	}
	return inst
}

// countAllocExits returns the ExitAllocs native code takes while f runs,
// and all its exits.
func countAllocExits(f func()) (alloc, all int64) {
	a := jitAllocExits.Load()
	all = countExits(f)
	return jitAllocExits.Load() - a, all
}

// Native code makes structs from the engine's pools, in the baseline's code
// and optimized, idle and with the collector marking: the results are the
// interpreter's, the pools are filled a few times for many structs, and the
// exits are the same whether the collector marks or not.
func TestJITPools(t *testing.T) {
	want, werr := runCases(t, poolInstance(t, math.MaxInt32, false), poolCases())
	for _, opt := range []bool{false, true} {
		tier := map[bool]string{false: "baseline", true: "opt"}[opt]
		var idleExits [2]int64
		for _, mode := range []string{"idle", "marking", "clobbered"} {
			t.Run(tier+"/"+mode, func(t *testing.T) {
				if mode != "idle" {
					jitMarkingForTest(t, mode == "clobbered")
				}
				inst := poolInstance(t, 0, opt)
				var got [][]Val
				var gerr []error
				alloc, all := countAllocExits(func() { got, gerr = runCases(t, inst, poolCases()) })
				sameOutcomes(t, tier, poolCases(), want, werr, got, gerr)
				if mode == "idle" {
					idleExits = [2]int64{alloc, all}
				} else if [2]int64{alloc, all} != idleExits {
					t.Fatalf("%d fills and %d exits while marking, %d and %d idle", alloc, all, idleExits[0], idleExits[1])
				}
				// Some 41,000 structs: the fills double up to the
				// pools' capacity, and each level takes the instance's
				// table once.
				if alloc == 0 || alloc > 41_000/gcPoolCap*2 {
					t.Fatalf("%d fills", alloc)
				}
				for _, name := range []string{"trees", "alls", "defaults", "nested"} {
					if opt && !optimized(inst, name) || !opt && !nativeRan(inst, name) {
						t.Errorf("%s is not %s code", name, tier)
					}
				}
			})
		}
	}
}

// Native code making structs while the collector runs keeps every one of
// them: a struct is in its pool until it is in its slot. The test runs
// again in a process of its own with GODEBUG=gccheckmark=1.
func TestJITPoolsUnderCollection(t *testing.T) {
	for _, opt := range []bool{false, true} {
		inst := poolInstance(t, 0, opt)
		var stop atomic.Bool
		done := make(chan struct{})
		go func() {
			defer close(done)
			for !stop.Load() {
				runtime.GC()
			}
		}()
		rounds := 50
		if testing.Short() {
			rounds = 5
		}
		for r := 0; r < rounds; r++ {
			res, err := refCall(t, inst, "keep", Val{N: 10})
			if err == nil && res[0].N == 2047 {
				res, err = refCall(t, inst, "trees", Val{N: 6}, Val{N: 20})
			}
			if err == nil && res[0].N == 20*127 {
				res, err = refCall(t, inst, "kept")
			}
			if err != nil || res[0].N != 2047 {
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
	cmd := osexec.Command(os.Args[0], "-test.run=^TestJITPoolsUnderCollection$", "-test.count=1")
	cmd.Env = append(os.Environ(), "QUICKJS_WASM_GCCHECK_CHILD=1", "GODEBUG=gccheckmark=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("with gccheckmark: %v\n%s", err, out)
	}
}

// Many goroutines making structs in one instance at once each take a table
// of pools of their own, and those calling back into the instance through
// the host share theirs with the level below: every result is right.
func TestJITPoolsConcurrent(t *testing.T) {
	for _, opt := range []bool{false, true} {
		inst := poolInstance(t, 0, opt)
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for r := 0; r < 20; r++ {
					res, err := refCall(t, inst, "trees", Val{N: 7}, Val{N: 10})
					if err == nil && res[0].N != 10*255 {
						err = errors.New("trees: wrong count")
					}
					if err == nil {
						res, err = refCall(t, inst, "nested", Val{N: 4})
						if err == nil && res[0].N != 3*31+31+31 {
							err = errors.New("nested: wrong count")
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

// jitBudgetInstance is budgetInstance with every function compiled at its
// first call, optimized where opt says.
func jitBudgetInstance(t *testing.T, src string, b *Budget, opt bool) *Instance {
	t.Helper()
	if !JITAvailable() {
		t.Skip("no native code in this build")
	}
	bin, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m, derr := decoder.Decode(bin, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
		t.Fatal(derr)
	}
	code := NewCode(m)
	code.EnableJIT(0)
	if opt {
		code.SetOptThreshold(0)
		code.SetOptBackground(false)
	}
	inst, err := InstantiateBudget(context.Background(), code, nil, b)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// A pool's fill charges the instance's budget for every struct it makes:
// structs made and dropped never stop a program, structs kept past the
// limit do, with ErrMemoryLimit; what a table of pools holds when it is
// collected, never reachable, is given back, unless the budget counted
// what is reachable since the fill.
func TestJITPoolsBudget(t *testing.T) {
	const src = `(module
		(type $node (struct (field (ref null $node)) (field i64)))
		(global $keep (mut (ref null $node)) (ref.null $node))
		(func (export "churn") (param i32)
			(loop $l
				(drop (struct.new $node (ref.null $node) (i64.const 0)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1))))))
		(func (export "hoard") (param i32)
			(loop $l
				(global.set $keep (struct.new $node (global.get $keep) (i64.const 0)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1))))))
		(func (export "one") (drop (struct.new_default $node))))`
	each := structSize(rttOf(mustCanonical(t, src, 0)))
	for _, opt := range []bool{false, true} {
		b := NewBudget(1 << 20)
		inst := jitBudgetInstance(t, src, b, opt)
		if _, err := callExport(t, inst, "churn", Val{N: 1_000_000}); err != nil {
			t.Fatalf("opt %v: churn: %v", opt, err)
		}
		if _, err := callExport(t, inst, "hoard", Val{N: 1_000_000}); !errors.Is(err, ErrMemoryLimit) {
			t.Fatalf("opt %v: hoard: got %v, want ErrMemoryLimit", opt, err)
		}

		// One struct made: its pool's first fill is charged whole, and
		// what is left of it given back once the instance is gone.
		for _, count := range []bool{false, true} {
			b := NewBudget(1 << 20)
			inst := jitBudgetInstance(t, src, b, opt)
			if _, err := callExport(t, inst, "one"); err != nil {
				t.Fatal(err)
			}
			if got := b.Used(); got != gcPoolFirst*each {
				t.Fatalf("opt %v: %d bytes used after one struct, want %d", opt, got, gcPoolFirst*each)
			}
			if count {
				// A count since the fill: the pool's structs are not in
				// it, and what is left of them is not given back.
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

// mustCanonical returns the canonical index of type ti of the module src.
func mustCanonical(t *testing.T, src string, ti uint32) module.CanonicalIndex {
	t.Helper()
	bin, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m, derr := decoder.Decode(bin, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	return m.Canonical[ti]
}
