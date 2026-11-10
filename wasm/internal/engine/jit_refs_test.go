//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"runtime"
	"sync/atomic"
	"testing"
	"unsafe"

	ijit "github.com/go-quickjs/go-quickjs/internal/jit"
)

// refsWAT exercises the reference instructions native code compiles:
// locals, globals and tables of references, select, the null tests and
// branches, ref.func, calls passing references natively and through Go,
// call_ref and return_call_ref.
const refsWAT = `(module
  (type $ii (func (param i32) (result i32)))
  (table $t 4 (ref null $ii))
  (table $e (export "e") 8 externref)
  (global $g (mut (ref null $ii)) (ref.null $ii))
  (global $x (mut externref) (ref.null extern))
  (elem declare func $inc $dbl)
  (func $inc (type $ii) (i32.add (local.get 0) (i32.const 1)))
  (func $dbl (type $ii) (i32.mul (local.get 0) (i32.const 2)))
  (func $pick (param $sel i32) (result (ref null $ii))
    (select (result (ref null $ii)) (ref.func $inc) (ref.func $dbl) (local.get $sel)))
  (func (export "apply") (param $sel i32) (param $v i32) (result i32)
    (local $f (ref null $ii))
    (local.set $f (call $pick (local.get $sel)))
    (global.set $g (local.get $f))
    (table.set $t (i32.const 2) (global.get $g))
    (call_ref $ii (local.get $v) (table.get $t (i32.const 2))))
  (func (export "tail") (param $sel i32) (param $v i32) (result i32)
    (return_call_ref $ii (local.get $v) (call $pick (local.get $sel))))
  (func (export "loop") (param $n i32) (result i32)
    (local $i i32) (local $acc i32) (local $f (ref null $ii))
    (local.set $acc (i32.const 1))
    (loop $l
      (local.set $f (call $pick (i32.and (local.get $i) (i32.const 1))))
      (table.set $t (i32.rem_u (local.get $i) (i32.const 4)) (local.get $f))
      (local.set $acc (i32.and (call_ref $ii (local.get $acc)
        (table.get $t (i32.rem_u (local.get $i) (i32.const 4)))) (i32.const 0xffff)))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $acc))
  (func (export "isnull") (param i32) (result i32)
    (ref.is_null (select (result funcref) (ref.null func) (ref.func $inc) (local.get 0))))
  (func (export "brnull") (param i32) (result i32)
    (block $b (result i32)
      (block $n
        (br_on_null $n (select (result funcref) (ref.null func) (ref.func $inc) (local.get 0)))
        (drop)
        (br $b (i32.const 1)))
      (i32.const 2)))
  (func (export "brnonnull") (param i32) (result i32)
    (block $b (result funcref)
      (br_on_non_null $b (select (result funcref) (ref.null func) (ref.func $inc) (local.get 0)))
      (return (i32.const 3)))
    (drop)
    (i32.const 4))
  (func (export "asnonnull") (param i32) (result i32)
    (drop (ref.as_non_null (select (result funcref) (ref.null func) (ref.func $inc) (local.get 0))))
    (i32.const 7))
  (func (export "eq") (param eqref eqref) (result i32)
    (ref.eq (local.get 0) (local.get 1)))
  (func $id (param externref) (result externref) (local.get 0))
  (func (export "swap") (param externref) (result externref)
    (global.get $x)
    (global.set $x (call $id (local.get 0))))
  (func (export "grow") (param externref i32) (result i32)
    (table.grow $e (local.get 0) (local.get 1)))
  (func (export "fill") (param i32 externref i32)
    (table.fill $e (local.get 0) (local.get 1) (local.get 2)))
  (func (export "get") (param i32) (result externref)
    (table.get $e (local.get 0)))
  (func (export "shuffle") (param $n i32) (param $k i32)
    (local $i i32) (local $a externref) (local $b externref)
    (loop $l
      (local.set $a (table.get $e (i32.rem_u (local.get $i) (local.get $k))))
      (local.set $b (table.get $e (i32.rem_u (i32.mul (local.get $i) (i32.const 7)) (local.get $k))))
      (global.set $x (local.get $a))
      (table.set $e (i32.rem_u (local.get $i) (local.get $k)) (local.get $b))
      (table.set $e (i32.rem_u (i32.mul (local.get $i) (i32.const 7)) (local.get $k)) (call $id (global.get $x)))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))))`

// refCall calls export name of inst with args.
func refCall(t *testing.T, inst *Instance, name string, args ...Val) ([]Val, error) {
	t.Helper()
	ext, ok := inst.Export(name)
	if !ok {
		t.Fatalf("no export %q", name)
	}
	return Invoke(context.Background(), ext.Func, args)
}

// sameRef reports whether two results are alike: the same object, or
// functions of the same index.
func sameRef(a, b Ref) bool {
	if fa, fb := a.Func(), b.Func(); fa != nil || fb != nil {
		return fa != nil && fb != nil && fa.Index == fb.Index
	}
	return a == b
}

// refHost1 and refHost2 are the host values refCases passes, the same in
// every run.
var refHost1, refHost2 = HostRef("one"), HostRef("two")

// jitCase is a call of an export, with its arguments.
type jitCase struct {
	name string
	args []Val
}

// refCases are refsWAT's calls.
func refCases() []jitCase {
	h1, h2 := refHost1, refHost2
	return []jitCase{
		{"apply", []Val{{N: 1}, {N: 10}}}, {"apply", []Val{{N: 0}, {N: 10}}},
		{"tail", []Val{{N: 1}, {N: 5}}}, {"tail", []Val{{N: 0}, {N: 5}}},
		{"loop", []Val{{N: 1000}}},
		{"isnull", []Val{{N: 0}}}, {"isnull", []Val{{N: 1}}},
		{"brnull", []Val{{N: 0}}}, {"brnull", []Val{{N: 1}}},
		{"brnonnull", []Val{{N: 0}}}, {"brnonnull", []Val{{N: 1}}},
		{"asnonnull", []Val{{N: 0}}}, {"asnonnull", []Val{{N: 1}}},
		{"eq", []Val{{R: I31(5)}, {R: I31(5)}}}, {"eq", []Val{{R: I31(5)}, {R: I31(6)}}},
		{"eq", []Val{{R: Null}, {R: Null}}}, {"eq", []Val{{R: I31(0)}, {R: Null}}},
		{"swap", []Val{{R: h1}}}, {"swap", []Val{{R: h2}}}, {"swap", []Val{{R: Null}}},
		{"grow", []Val{{R: h1}, {N: 3}}}, {"fill", []Val{{N: 1}, {R: h2}, {N: 5}}},
		{"fill", []Val{{N: 9}, {R: h2}, {N: 5}}},
		{"get", []Val{{N: 0}}}, {"get", []Val{{N: 1}}}, {"get", []Val{{N: 10}}}, {"get", []Val{{N: 1000}}},
		{"shuffle", []Val{{N: 100}, {N: 11}}}, {"get", []Val{{N: 3}}}, {"get", []Val{{N: 6}}},
	}
}

// runCases runs cases on inst, returning each call's results and error.
func runCases(t *testing.T, inst *Instance, cases []jitCase) (out [][]Val, errs []error) {
	for _, c := range cases {
		res, err := refCall(t, inst, c.name, c.args...)
		out = append(out, res)
		errs = append(errs, err)
	}
	return out, errs
}

func sameOutcomes(t *testing.T, what string, cases []jitCase, ri [][]Val, ei []error, rj [][]Val, ej []error) {
	t.Helper()
	for i := range ri {
		var ti, tj *Trap
		if errors.As(ei[i], &ti) != errors.As(ej[i], &tj) || (ti != nil && ti.Code != tj.Code) || (ei[i] == nil) != (ej[i] == nil) {
			t.Fatalf("%s: %s%v: %v, %v", what, cases[i].name, cases[i].args, ei[i], ej[i])
		}
		for k := range ri[i] {
			if ri[i][k].N != rj[i][k].N || !sameRef(ri[i][k].R, rj[i][k].R) {
				t.Fatalf("%s: %s%v = %v, %v", what, cases[i].name, cases[i].args, ri[i], rj[i])
			}
		}
	}
}

// Native code that holds references computes what the interpreter does.
func TestJITReferences(t *testing.T) {
	want, werr := runCases(t, interpretedInstance(t, refsWAT), refCases())
	inst := nativeInstance(t, refsWAT)
	got, gerr := runCases(t, inst, refCases())
	sameOutcomes(t, "native", refCases(), want, werr, got, gerr)
	for _, name := range []string{"apply", "tail", "loop", "isnull", "brnull", "brnonnull", "asnonnull", "eq", "swap", "grow", "fill", "get", "shuffle"} {
		if !nativeRan(inst, name) {
			t.Errorf("%s has no native code", name)
		}
	}
}

// jitMarkingForTest has the code compiled from here on see the collector
// marking at every store of a pointer word -- through the runtime's
// barrier store, or through clobber's, which first fills the registers
// native code may keep with junk -- until the test ends.
func jitMarkingForTest(t *testing.T, clobber bool) {
	flag := new(uint8)
	*flag = 1
	wb, sr := jitWriteBarrier, jitStoreRef
	jitWriteBarrier = uint64(uintptr(unsafe.Pointer(flag)))
	if clobber {
		jitStoreRef = ijit.StoreRefClobber()
	}
	t.Cleanup(func() {
		jitWriteBarrier, jitStoreRef = wb, sr
		runtime.KeepAlive(flag)
	})
}

// countExits returns the exits native code takes while f runs.
func countExits(f func()) int64 {
	jitCountExits = true
	defer func() { jitCountExits = false }()
	before := jitExits.Load()
	f()
	return jitExits.Load() - before
}

// Native code does the same work whether the collector marks or not: it
// stores its pointer words through the barrier routine while the collector
// marks, which neither leaves native code nor calls Go for the marking's
// sake, and its results are the same.
func TestJITSameWorkWhileMarking(t *testing.T) {
	for _, mod := range []struct {
		name, src string
		cases     func() []jitCase
	}{{"refs", refsWAT, refCases}, {"gc", gcWAT, gcCases}} {
		// The baseline's code, and the optimizing tier's.
		for _, tier := range []struct {
			name string
			inst func(testing.TB, string) *Instance
		}{{"", func(t testing.TB, src string) *Instance { return nativeInstance(t, src) }}, {"/opt", optInstance}} {
			var idle [][]Val
			var idleErr []error
			idleExits := countExits(func() { idle, idleErr = runCases(t, tier.inst(t, mod.src), mod.cases()) })
			for _, clobber := range []bool{false, true} {
				t.Run(mod.name+tier.name+map[bool]string{false: "/marking", true: "/clobbered"}[clobber], func(t *testing.T) {
					jitMarkingForTest(t, clobber)
					var got [][]Val
					var gotErr []error
					exits := countExits(func() { got, gotErr = runCases(t, tier.inst(t, mod.src), mod.cases()) })
					sameOutcomes(t, "marking", mod.cases(), idle, idleErr, got, gotErr)
					if exits != idleExits {
						t.Fatalf("%d exits while marking, %d idle", exits, idleExits)
					}
				})
			}
		}
	}
}

// Native code moving references between a table, a global and its locals
// while the collector runs keeps every object they reach: its stores of
// pointer words go through the write barrier while the collector marks.
// The test runs again in a process of its own with GODEBUG=gccheckmark=1,
// where the collector checks after each cycle that it marked everything
// reachable.
func TestJITReferencesUnderCollection(t *testing.T) {
	if !JITAvailable() {
		t.Skip("no native code in this build")
	}
	for _, opt := range []bool{false, true} {
		referencesUnderCollection(t, opt)
	}
	if os.Getenv("QUICKJS_WASM_GCCHECK_CHILD") != "" || testing.Short() {
		return
	}
	cmd := osexec.Command(os.Args[0], "-test.run=^TestJITReferencesUnderCollection$", "-test.count=1")
	cmd.Env = append(os.Environ(), "QUICKJS_WASM_GCCHECK_CHILD=1", "GODEBUG=gccheckmark=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("with gccheckmark: %v\n%s", err, out)
	}
}

// referencesUnderCollection moves references between a table, a global,
// locals and a call while the collector runs, in the baseline code or, opt,
// in the optimizing tier's.
func referencesUnderCollection(t *testing.T, opt bool) {
	const k = 64
	inst := nativeInstance(t, refsWAT)
	if opt {
		inst.Code.SetOptThreshold(0)
		inst.Code.SetOptBackground(false)
	}
	if _, err := refCall(t, inst, "grow", Val{R: Null}, Val{N: k - 8}); err != nil {
		t.Fatal(err)
	}
	type box struct {
		id  int
		pad [6]int
	}
	for i := 0; i < k; i++ {
		inst.Tables[1].Elems[i] = HostRef(&box{id: i})
	}
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		var sink [][]byte
		for !stop.Load() {
			for range 200 {
				sink = append(sink, make([]byte, 128))
			}
			sink = sink[:0]
			runtime.GC()
		}
	}()
	rounds := 200
	if testing.Short() {
		rounds = 20
	}
	for r := 0; r < rounds; r++ {
		if _, err := refCall(t, inst, "shuffle", Val{N: 997}, Val{N: k}); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	<-done
	runtime.GC()
	seen := map[int]bool{}
	for i, r := range inst.Tables[1].Elems {
		v, ok := r.Host()
		b, isBox := v.(*box)
		if !ok || !isBox || b.id < 0 || b.id >= k {
			t.Fatalf("element %d is %v", i, v)
		}
		seen[b.id] = true
	}
	if len(seen) == 0 {
		t.Fatal("no element survived")
	}
	if opt && !optimized(inst, "shuffle") {
		t.Fatal("shuffle not optimized")
	}
}
