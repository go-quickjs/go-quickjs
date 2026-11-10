//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"os"
	osexec "os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// optInstance is nativeInstance with every function the optimizing tier
// accepts optimized at its first compile.
func optInstance(t testing.TB, src string) *Instance {
	t.Helper()
	inst := nativeInstance(t, src)
	inst.Code.SetOptThreshold(0)
	inst.Code.SetOptBackground(false)
	return inst
}

// The optimizing tier's GC instructions compute what the interpreter does,
// trap alike and allocate through Go, idle and with the collector marking.
func TestOptGC(t *testing.T) {
	want, werr := runCases(t, interpretedInstance(t, gcWAT), gcCases())
	for _, mode := range []string{"idle", "marking", "clobbered"} {
		t.Run(mode, func(t *testing.T) {
			if mode != "idle" {
				jitMarkingForTest(t, mode == "clobbered")
			}
			inst := optInstance(t, gcWAT)
			got, gerr := runCases(t, inst, gcCases())
			sameOutcomes(t, "optimized", gcCases(), want, werr, got, gerr)
			for _, name := range []string{"fields", "fieldref", "defaults", "arrays", "refarray", "fixed", "elems", "i31", "tests", "cast", "brcast", "nulls", "list", "relink"} {
				if !optimized(inst, name) {
					t.Errorf("%s is not optimized", name)
				}
			}
		})
	}
}

// The optimizing tier's code relinking a list of structs while the
// collector runs keeps every node, as the baseline's does
// (TestJITGCUnderCollection). The test runs again in a process of its own
// with GODEBUG=gccheckmark=1.
func TestOptGCUnderCollection(t *testing.T) {
	inst := optInstance(t, gcWAT)
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
	if !optimized(inst, "list") || !optimized(inst, "relink") {
		t.Fatal("list or relink is not optimized")
	}
	if os.Getenv("QUICKJS_WASM_GCCHECK_CHILD") != "" || testing.Short() {
		return
	}
	cmd := osexec.Command(os.Args[0], "-test.run=^TestOptGCUnderCollection$", "-test.count=1")
	cmd.Env = append(os.Environ(), "QUICKJS_WASM_GCCHECK_CHILD=1", "GODEBUG=gccheckmark=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("with gccheckmark: %v\n%s", err, out)
	}
}

// The optimizing tier's field and element accesses with more values live
// than registers -- indices, values stored and values read in registers,
// spilled and constant -- and across the allocations Go makes, which every
// register crosses in its slot, compute what the interpreter does.
func TestOptGCPressure(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`(module
  (type $s (struct (field (mut i8)) (field (mut i16)) (field (mut i32)) (field (mut i64)) (field (mut f32)) (field (mut f64))))
  (type $a8 (array (mut i8)))
  (type $a64 (array (mut i64)))
  (type $af32 (array (mut f32)))
  (func (export "f") (param $x i32) (param $i i32) (result i64)
    (local $s (ref null $s)) (local $b (ref null $a8)) (local $c (ref null $a64)) (local $d (ref null $af32))
`)
	const n = 20
	for k := 0; k < n; k++ {
		fmt.Fprintf(&sb, "    (local $v%d i64) (local $w%d f64)\n", k, k)
	}
	for k := 0; k < n; k++ {
		fmt.Fprintf(&sb, "    (local.set $v%d (i64.mul (i64.extend_i32_u (local.get $x)) (i64.const %d)))\n", k, 3*k+1)
		fmt.Fprintf(&sb, "    (local.set $w%d (f64.add (f64.convert_i32_s (local.get $x)) (f64.const %d.5)))\n", k, k)
	}
	sb.WriteString(`    (local.set $s (struct.new $s (local.get $x) (local.get $x) (local.get $x) (local.get $v1) (f32.demote_f64 (local.get $w2)) (local.get $w3)))
    (local.set $b (array.new $a8 (local.get $x) (i32.const 16)))
    (local.set $c (array.new_default $a64 (i32.const 16)))
    (local.set $d (array.new $af32 (f32.const 0.25) (i32.const 16)))
`)
	for k := 0; k < n; k++ {
		fmt.Fprintf(&sb, "    (array.set $a64 (local.get $c) (i32.and (i32.add (local.get $i) (i32.const %d)) (i32.const 15)) (local.get $v%d))\n", k, k)
		fmt.Fprintf(&sb, "    (array.set $af32 (local.get $d) (i32.const %d) (f32.demote_f64 (local.get $w%d)))\n", k%16, k)
		fmt.Fprintf(&sb, "    (struct.set $s %d (local.get $s) (i32.wrap_i64 (local.get $v%d)))\n", k%3, k)
		fmt.Fprintf(&sb, "    (local.set $v%d (i64.add (local.get $v%d) (i64.extend_i32_s (array.get_s $a8 (local.get $b) (i32.and (local.get $i) (i32.const 15))))))\n", k, k)
	}
	sb.WriteString("    (i64.add (i64.add (i64.add (i64.extend_i32_u (struct.get_u $s 0 (local.get $s))) (i64.extend_i32_s (struct.get_s $s 1 (local.get $s))))\n")
	sb.WriteString("      (i64.add (i64.extend_i32_u (struct.get $s 2 (local.get $s))) (struct.get $s 3 (local.get $s))))\n")
	sb.WriteString("      (i64.add (i64.add (i64.trunc_f32_s (struct.get $s 4 (local.get $s))) (i64.trunc_f64_s (struct.get $s 5 (local.get $s))))\n")
	sb.WriteString("        (i64.add (array.get $a64 (local.get $c) (i32.and (local.get $i) (i32.const 15))) (i64.trunc_f32_s (array.get $af32 (local.get $d) (i32.const 3))))))\n")
	expr := "(i64.const 0)"
	for k := 0; k < n; k++ {
		expr = fmt.Sprintf("(i64.add %s (i64.add (local.get $v%d) (i64.trunc_f64_s (local.get $w%d))))", expr, k, k)
	}
	sb.WriteString("    " + expr + "\n    i64.add))")
	src := sb.String()
	var calls []jitCall
	for _, x := range []uint64{0, 1, 0x7f, 0x80, 0xff, 0x7fff, 0x8000, 0x12345678, 0xffffffff} {
		for _, i := range []uint64{0, 5, 15, 16, 0xffffffff} {
			calls = append(calls, jitCall{name: "f", args: []uint64{x, i}})
		}
	}
	inst := optInstance(t, src)
	sameAsInterpreterIn(t, inst, src, calls, optimized)
}
