//go:build quickjs_jit && !android && !ios && amd64 && (linux || windows)

package engine

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
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

// The optimizing tier's call_indirect calls natively a function of the
// call's type that its instance defined and that has native code, and
// leaves every other element -- null, past the table, of another type, a
// host function, another instance's, one that holds references -- to Go,
// whose traps and calls are the interpreter's.
func TestOptCallIndirect(t *testing.T) {
	src := `(module
  (type $t (func (param i32) (result i32)))
  (type $u (func (param i32) (result i64)))
  (import "m" "host" (func $host (type $t)))
  (import "m" "other" (func $other (type $t)))
  (table 12 funcref)
  (elem (i32.const 0) $add1 $mul3 $wide $host $other $refs $deep $add1)
  (func $add1 (type $t) (i32.add (local.get 0) (i32.const 1)))
  (func $mul3 (type $t) (i32.mul (local.get 0) (i32.const 3)))
  (func $wide (type $u) (i64.extend_i32_u (local.get 0)))
  (func $refs (type $t) (local externref)
    (if (ref.is_null (local.get 1)) (then (return (i32.sub (local.get 0) (i32.const 7)))))
    (i32.const 0))
  (func $deep (type $t)
    (if (result i32) (i32.eqz (local.get 0))
      (then (i32.const 0))
      (else (i32.add (i32.const 1)
        (call_indirect (type $t) (i32.sub (local.get 0) (i32.const 1)) (i32.const 6))))))
  (func (export "dispatch") (param $i i32) (param $x i32) (result i32)
    (call_indirect (type $t) (local.get $x) (local.get $i)))
  (func (export "deep") (param i32) (result i32)
    (call_indirect (type $t) (local.get 0) (i32.const 6)))
  (func (export "fast") (param $n i32) (result i32)
    (local $k i32) (local $acc i32)
    (loop $l
      (local.set $acc (call_indirect (type $t) (local.get $acc) (i32.and (local.get $k) (i32.const 1))))
      (br_if $l (i32.lt_u (local.tee $k (i32.add (local.get $k) (i32.const 1))) (local.get $n))))
    (local.get $acc))
  (func (export "mix") (param $n i32) (result i32)
    (local $k i32) (local $acc i32)
    (loop $l
      (local.set $acc (i32.add (local.get $acc)
        (call_indirect (type $t) (i32.add (local.get $acc) (local.get $k))
          (select (i32.const 7) (i32.rem_u (local.get $k) (i32.const 6))
            (i32.eq (i32.rem_u (local.get $k) (i32.const 6)) (i32.const 2))))))
      (br_if $l (i32.lt_u (local.tee $k (i32.add (local.get $k) (i32.const 1))) (local.get $n))))
    (local.get $acc)))`
	ft := module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}, Results: []module.ValueType{module.I32}})
	host := NewHostFunction(ft, "host", func(_ context.Context, _ *Instance, args, res []Val) error {
		res[0] = Val{N: uint64(uint32(args[0].N) ^ 0x55)}
		return nil
	})
	otherInst := interpretedInstance(t, `(module (func (export "f") (param i32) (result i32) (i32.sub (local.get 0) (i32.const 100))))`)
	other, _ := otherInst.Export("f")
	imports := []Extern{{Kind: module.ExternFunc, Func: host}, {Kind: module.ExternFunc, Func: other.Func}}
	interp := thresholdInstance(t, src, math.MaxInt32, imports...)
	inst := thresholdInstance(t, src, 0, imports...)
	inst.Code.setOSRCalls(0)
	inst.Code.SetOptThreshold(0)
	same := func(name string, args ...uint64) {
		t.Helper()
		want, werr := call(t, interp, name, args...)
		got, gerr := call(t, inst, name, args...)
		if fmt.Sprint(werr) != fmt.Sprint(gerr) || (werr == nil && got[0].N != want[0].N) {
			t.Fatalf("%s%v = %v, %v; want %v, %v", name, args, got, gerr, want, werr)
		}
	}
	for i := range uint64(14) {
		for _, x := range []uint64{0, 5, 0xffffffff} {
			same("dispatch", i, x)
		}
	}
	same("dispatch", 0xffffffff, 1)
	same("dispatch", 1<<31, 1)
	if !optimized(inst, "dispatch") {
		t.Fatal("dispatch not optimized")
	}
	same("mix", 5000)
	same("fast", 10)
	same("fast", 100000)
	if n := countExits(func() { call(t, inst, "fast", 100000) }); n > 100 {
		t.Fatalf("%d exits in 100,000 calls through the table", n)
	}
	same("deep", 1000)
	same("deep", 1_000_000)
}

// The optimizing tier's memory.copy and memory.fill of memory 0 run in its
// code: both ends checked before a byte is written, a copy to a higher
// address overlapping its source made from the end down.
func TestOptBulkMemory(t *testing.T) {
	src := `(module
  (memory 1)
  (func (export "init") (param $n i32)
    (local $i i32)
    (loop $l
      (i32.store8 (local.get $i) (i32.add (i32.mul (local.get $i) (i32.const 7)) (i32.const 1)))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n)))))
  (func (export "copy") (param i32 i32 i32) (memory.copy (local.get 0) (local.get 1) (local.get 2)))
  (func (export "fill") (param i32 i32 i32) (memory.fill (local.get 0) (local.get 1) (local.get 2)))
  (func (export "both") (param $d i32) (param $s i32) (param $n i32) (result i32)
    (local $live i32)
    (local.set $live (i32.mul (local.get $d) (i32.const 3)))
    (memory.copy (local.get $d) (local.get $s) (local.get $n))
    (memory.fill (local.get $s) (local.get $live) (local.get $n))
    (i32.add (local.get $live) (i32.load (local.get $d)))))`
	interp := interpretedInstance(t, src)
	inst := tierInstance(t, src, 0)
	same := func(name string, args ...uint64) {
		t.Helper()
		want, werr := call(t, interp, name, args...)
		got, gerr := call(t, inst, name, args...)
		if fmt.Sprint(werr) != fmt.Sprint(gerr) || len(got) != len(want) || (len(got) > 0 && got[0].N != want[0].N) {
			t.Fatalf("%s%v = %v, %v; want %v, %v", name, args, got, gerr, want, werr)
		}
		if !bytes.Equal(inst.Memories[0].bytes(), interp.Memories[0].bytes()) {
			t.Fatalf("%s%v: the memories differ", name, args)
		}
	}
	same("init", 65536)
	const size = 65536
	for _, n := range []uint64{0, 1, 3, 7, 8, 9, 15, 16, 17, 100, 1000, 4097} {
		for _, d := range []int64{-9, -8, -1, 0, 1, 7, 8, 9, 64} {
			same("copy", uint64(1000+d), 1000, n)
			same("copy", 1000, uint64(1000+d), n)
		}
		same("copy", size-n, 0, n)
		same("copy", 0, size-n, n)
		same("copy", size-n+1, 0, n)
		same("copy", 0, size-n+1, n)
		same("fill", 333, n*5+1, n)
		same("fill", size-n, 0xab, n)
		same("fill", size-n+1, 0xab, n)
		same("both", 2000, 2003, n)
		same("both", size-n, 8, n)
	}
	same("copy", 0, 0, 0xffffffff)
	same("copy", 0xffffffff, 0, 1)
	same("copy", 0, 0xffffffff, 1)
	same("fill", 0xffffffff, 0, 1)
	same("fill", 0, 0, 0xffffffff)
	same("copy", size, size, 0)
	same("fill", size, 1, 0)
	if n := countExits(func() {
		for range 1000 {
			call(t, inst, "both", 100, 300, 64)
		}
	}); n > 0 {
		t.Fatalf("%d exits in 1,000 calls of memory.copy and memory.fill", n)
	}
	if !optimized(inst, "copy") || !optimized(inst, "fill") || !optimized(inst, "both") {
		t.Fatal("not optimized")
	}
}

// Jump threading takes each back edge of a dispatch loop -- a loop whose
// header switches on a local its back edges set to constants, as Go's code
// for WebAssembly has -- to its case, and looks the locals up again. Random
// such loops, with locals copied into each other, cases that fall into the
// next, conditional and computed dispatch, compute what the interpreter
// computes.
func TestOptThreadDispatch(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for iter := range 1500 {
		src := dispatchLoop(rng)
		interp := interpretedInstance(t, src)
		inst := tierInstance(t, src, 0)
		for _, n := range []uint64{0, 1, 5, 40, 300} {
			s := rng.Uint64()
			want, werr := call(t, interp, "run", n, s)
			got, gerr := call(t, inst, "run", n, s)
			if fmt.Sprint(werr) != fmt.Sprint(gerr) || (werr == nil && got[0].N != want[0].N) {
				t.Fatalf("iteration %d, run(%d, %#x) = %v, %v; want %v, %v\n%s", iter, n, s, got, gerr, want, werr, src)
			}
		}
		if !optimized(inst, "run") {
			t.Fatalf("iteration %d: not optimized\n%s", iter, src)
		}
	}
}

// dispatchLoop is a random function of a dispatch loop over i64 locals.
func dispatchLoop(rng *rand.Rand) string {
	nv := 2 + rng.IntN(4)
	nc := 2 + rng.IntN(5)
	var sb strings.Builder
	sb.WriteString("(module (func (export \"run\") (param $n i32) (param $s i64) (result i64)\n (local $pc i32) (local $t i64)")
	for v := range nv {
		fmt.Fprintf(&sb, " (local $v%d i64)", v)
	}
	sb.WriteString("\n")
	for v := range nv {
		fmt.Fprintf(&sb, " (local.set $v%d (i64.add (local.get $s) (i64.const %d)))\n", v, rng.IntN(100))
	}
	sb.WriteString(" (loop $L\n  (block $exit\n")
	for c := nc - 1; c >= 0; c-- {
		fmt.Fprintf(&sb, "  (block $c%d\n", c)
	}
	sb.WriteString("   (br_table")
	for c := range nc {
		fmt.Fprintf(&sb, " $c%d", c)
	}
	sb.WriteString(" $exit (local.get $pc)))\n")
	v := func() string { return fmt.Sprintf("$v%d", rng.IntN(nv)) }
	stmt := func() string {
		switch rng.IntN(7) {
		case 0:
			return fmt.Sprintf("(local.set %s (local.get %s))", v(), v())
		case 1:
			return fmt.Sprintf("(local.set %s (i64.add (local.get %s) (i64.const %d)))", v(), v(), rng.IntN(9))
		case 2:
			return fmt.Sprintf("(local.set %s (i64.xor (local.get %s) (i64.rotl (local.get %s) (i64.const %d))))", v(), v(), v(), 1+rng.IntN(60))
		case 3:
			return fmt.Sprintf("(if (i64.lt_u (local.get %s) (local.get %s)) (then (local.set %s (i64.mul (local.get %s) (i64.const 3)))))", v(), v(), v(), v())
		case 4:
			a, b := v(), v()
			return fmt.Sprintf("(local.set $t (local.get %s)) (local.set %s (local.get %s)) (local.set %s (local.get $t))", a, a, b, b)
		case 5:
			return fmt.Sprintf("(local.set $pc (i32.wrap_i64 (i64.rem_u (local.get %s) (i64.const %d))))", v(), nc+1)
		}
		return fmt.Sprintf("(local.set %s (i64.sub (local.get %s) (local.get %s)))", v(), v(), v())
	}
	for c := range nc {
		sb.WriteString("   (br_if $exit (i32.eqz (local.get $n)))\n   (local.set $n (i32.sub (local.get $n) (i32.const 1)))\n")
		for k := rng.IntN(4); k > 0; k-- {
			sb.WriteString("   " + stmt() + "\n")
		}
		if rng.IntN(2) == 0 {
			fmt.Fprintf(&sb, "   (if (i64.gt_u (local.get %s) (local.get %s)) (then (local.set $pc (i32.const %d)) (br $L)))\n", v(), v(), rng.IntN(nc))
		}
		for k := rng.IntN(2); k > 0; k-- {
			sb.WriteString("   " + stmt() + "\n")
		}
		switch rng.IntN(5) {
		case 0:
			// Into the next case.
		case 2:
			fmt.Fprintf(&sb, "   (local.set $pc (i32.const %d)) (br_table $L $exit $L (i32.wrap_i64 (i64.and (local.get %s) (i64.const 3))))\n", rng.IntN(nc+1), v())
		case 1:
			fmt.Fprintf(&sb, "   (local.set $pc (i32.const %d)) (br_if $L (i64.lt_s (local.get %s) (i64.const 0)))\n", rng.IntN(nc+1), v())
		default:
			fmt.Fprintf(&sb, "   (local.set $pc (i32.const %d)) (br $L)\n", rng.IntN(nc+1))
		}
		fmt.Fprintf(&sb, "  ) ;; the end of case %d\n", c)
	}
	sb.WriteString("  ) ;; the loop\n")
	res := "(local.get $v0)"
	for v := 1; v < nv; v++ {
		res = fmt.Sprintf("(i64.add (i64.mul %s (i64.const 31)) (local.get $v%d))", res, v)
	}
	sb.WriteString(" " + res + "))\n")
	return sb.String()
}

// A function called once whose loop calls through Go -- a host function,
// here, through a table -- counts those calls toward the optimizing tier,
// as the polls are then Go's: its next call runs the optimized code.
func TestOptTierUpByExits(t *testing.T) {
	src := `(module
  (type $t (func (param i32) (result i32)))
  (import "m" "h" (func $h (type $t)))
  (table 1 funcref)
  (elem (i32.const 0) $h)
  (func (export "loop") (param $n i32) (result i32)
    (local $i i32) (local $acc i32)
    (loop $l
      (local.set $acc (i32.add (local.get $acc) (call_indirect (type $t) (local.get $i) (i32.const 0))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n))))
    (local.get $acc)))`
	ft := module.CanonicalFunc(&module.FuncType{Params: []module.ValueType{module.I32}, Results: []module.ValueType{module.I32}})
	h := NewHostFunction(ft, "h", func(_ context.Context, _ *Instance, args, res []Val) error {
		res[0] = Val{N: uint64(uint32(args[0].N) * 3)}
		return nil
	})
	inst := thresholdInstance(t, src, 0, Extern{Kind: module.ExternFunc, Func: h})
	inst.Code.setOSRCalls(0)
	inst.Code.SetOptThreshold(1000)
	for _, n := range []uint64{10, 5000} {
		got, err := call(t, inst, "loop", n)
		if err != nil {
			t.Fatal(err)
		}
		want := uint64(0)
		for i := range n {
			want += 3 * i
		}
		if got[0].N != uint64(uint32(want)) {
			t.Fatalf("loop(%d) = %d, want %d", n, got[0].N, uint32(want))
		}
	}
	if !optimized(inst, "loop") {
		t.Fatal("loop not optimized after 5,000 calls through Go")
	}
}
