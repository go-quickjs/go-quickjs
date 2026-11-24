package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// nativeAllocSrc makes structs of every field kind by struct.new, and
// arrays of every element kind but v128 by array.new, array.new_default
// and array.new_fixed, each read back whole: of lengths given at run time,
// from the pools' classes and past them, and too long, a trap.
var nativeAllocSrc = func() string {
	var b strings.Builder
	b.WriteString(`(module
  (type $box (struct (field i32)))
  (type $s (struct (field i8) (field i16) (field i32) (field i64) (field f32) (field f64)
    (field v128) (field (ref null $box)) (field eqref)))
  (global $keep (mut anyref) (ref.null any))
  (func (export "structs") (param $n i32) (param $x i64) (result i64)
    (local $s (ref null $s)) (local $i i32) (local $acc i64)
    (loop $l
      (local.set $s (struct.new $s
        (i32.wrap_i64 (local.get $x)) (i32.wrap_i64 (i64.shr_u (local.get $x) (i64.const 8)))
        (i32.add (local.get $i) (i32.wrap_i64 (local.get $x))) (i64.mul (local.get $x) (i64.extend_i32_u (local.get $i)))
        (f32.convert_i32_s (local.get $i)) (f64.reinterpret_i64 (local.get $x))
        (i64x2.splat (i64.add (local.get $x) (i64.const 3)))
        (if (result (ref null $box)) (i32.and (local.get $i) (i32.const 1)) (then (struct.new $box (local.get $i))) (else (ref.null $box)))
        (ref.i31 (local.get $i))))
      (global.set $keep (local.get $s))
      (local.set $acc (i64.add (i64.mul (local.get $acc) (i64.const 31))
        (i64.add
          (i64.add (i64.extend_i32_s (struct.get_s $s 0 (local.get $s))) (i64.extend_i32_u (struct.get_u $s 1 (local.get $s))))
          (i64.add (i64.extend_i32_u (struct.get $s 2 (local.get $s))) (struct.get $s 3 (local.get $s))))))
      (local.set $acc (i64.add (local.get $acc)
        (i64.add (i64.add (i64.extend_i32_u (i32.reinterpret_f32 (struct.get $s 4 (local.get $s)))) (i64.reinterpret_f64 (struct.get $s 5 (local.get $s))))
          (i64.add (i64x2.extract_lane 1 (struct.get $s 6 (local.get $s)))
            (i64.add (i64.extend_i32_u (ref.is_null (struct.get $s 7 (local.get $s))))
              (i64.extend_i32_u (i31.get_s (ref.cast (ref i31) (struct.get $s 8 (local.get $s))))))))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $acc))
  (func (export "defaults") (param $n i32) (result i32)
    (local $i i32) (local $acc i32)
    (loop $l
      (local.set $acc (i32.add (local.get $acc) (struct.get $s 2 (struct.new_default $s))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $acc))
`)
	for _, t := range []struct{ name, storage, val, get, ext string }{
		{"i8", "i8", "i32", "array.get_s", "(i64.extend_i32_s %s)"},
		{"i16", "i16", "i32", "array.get_u", "(i64.extend_i32_u %s)"},
		{"i32", "i32", "i32", "array.get", "(i64.extend_i32_s %s)"},
		{"i64", "i64", "i64", "array.get", "%s"},
		{"f32", "f32", "f32", "array.get", "(i64.extend_i32_u (i32.reinterpret_f32 %s))"},
		{"f64", "f64", "f64", "array.get", "(i64.reinterpret_f64 %s)"},
		{"ref", "(ref null $box)", "(ref null $box)", "array.get", "(i64.extend_i32_u (if (result i32) (ref.is_null %[1]s) (then (i32.const 7)) (else (struct.get $box 0 %[1]s))))"},
	} {
		get := fmt.Sprintf(t.ext, fmt.Sprintf("(%s $%s (local.get $a) (local.get $i))", t.get, t.name))
		fmt.Fprintf(&b, "  (type $%s (array (mut %s)))\n", t.name, t.storage)
		val := "(local.get $v)"
		if t.name == "ref" {
			val = "(struct.new $box (i32.wrap_i64 (local.get $v)))"
		}
		param := t.val
		if t.name == "ref" {
			param = "i64"
		}
		fixed := strings.TrimSpace(strings.Repeat(val+" ", 5))
		for _, f := range []struct{ suffix, make string }{
			{"", fmt.Sprintf("(array.new $%s %s (local.get $n))", t.name, val)},
			{"_default", fmt.Sprintf("(array.new_default $%s (local.get $n))", t.name)},
			{"_fixed", fmt.Sprintf("(array.new_fixed $%s 5 %s)", t.name, fixed)},
		} {
			fmt.Fprintf(&b, `  (func (export "%[1]s%[2]s") (param $n i32) (param $v %[3]s) (result i64)
    (local $a (ref null $%[1]s)) (local $i i32) (local $s i64)
    (local.set $a %[4]s)
    (global.set $keep (local.get $a))
    (local.set $s (i64.extend_i32_u (array.len (local.get $a))))
    (block $d (loop $l
      (br_if $d (i32.ge_u (local.get $i) (array.len (local.get $a))))
      (local.set $s (i64.add (i64.mul (local.get $s) (i64.const 31)) %[5]s))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $l)))
    (local.get $s))
`, t.name, f.suffix, param, f.make, get)
		}
	}
	b.WriteString(")")
	return b.String()
}()

// nativeAllocCalls are nativeAllocSrc's calls: lengths 0, 1, 16, 17, each
// side of the largest class, past it, and too long.
func nativeAllocCalls() []regCall {
	calls := []regCall{{"structs", []Val{{N: 1}, {N: 0x8123456789abcdef}}}, {"structs", []Val{{N: 300}, {N: 0xfedc}}},
		{"defaults", []Val{{N: 1000}}}}
	sizes := map[string]uint64{"i8": 1, "i16": 2, "i32": 4, "i64": 8, "f32": 4, "f64": 8, "ref": 16}
	vals := map[string]uint64{"i8": 0xfffffff3, "i16": 0xffff8001, "i32": 0x89abcdef, "i64": 0x8123456789abcdef,
		"f32": 0x7fc00001, "f64": 0x7ff8000000000003, "ref": 0x1234}
	for _, name := range []string{"i8", "i16", "i32", "i64", "f32", "f64", "ref"} {
		edge := 480 / sizes[name]
		for _, n := range []uint64{0, 1, 2, 16, 17, edge - 1, edge, edge + 1, 1000, 0x7fffffff, 0xffffffff} {
			for _, f := range []string{"", "_default"} {
				calls = append(calls, regCall{name + f, []Val{{N: n}, {N: vals[name]}}})
			}
		}
		calls = append(calls, regCall{name + "_fixed", []Val{{N: 0}, {N: vals[name]}}})
	}
	return calls
}

// TestNativeAlloc checks the native handlers' allocations against the Go
// table, with the collector's flag read set and clear, and that a loop of
// them leaves native code for no allocation but a trapping one.
func TestNativeAlloc(t *testing.T) {
	nativeOnly(t)
	calls := nativeAllocCalls()
	marked, unmarked := matchNative(t, nativeAllocSrc, calls)
	if marked == 0 || unmarked != 0 {
		t.Fatalf("the barrier routine stored %d times marking and %d not, want some and none", marked, unmarked)
	}
	inst := instantiateReg(t, nativeAllocSrc)
	for _, c := range calls {
		var err error
		var exits []nativeExit
		withNative(inst, true, func() {
			exits = logNativeExits(inst, func() {
				ext, _ := inst.Export(c.name)
				_, err = Invoke(context.Background(), ext.Func, c.args)
			})
		})
		want := 0
		var tr *Trap
		if errors.As(err, &tr) {
			want = 1
		}
		var allocs []nativeExit
		for _, e := range exits {
			if e.op == uint8(rExt) && e.x >= uint8(xStructNew) && e.x <= uint8(xArrayNewSegment) {
				allocs = append(allocs, e)
			}
		}
		if n, all := exitsBut(allocs, exitRoom); n > want {
			t.Errorf("%s%v left native code %d times for allocations, want at most %d:%s", c.name, c.args, n, want, all)
		}
	}
}

// TestNativeAllocBudget checks allocations past the instance's budget, from
// the pools and past them, kept in a list: they stop with ErrMemoryLimit
// natively as on the Go table.
func TestNativeAllocBudget(t *testing.T) {
	nativeOnly(t)
	const src = `(module
  (type $a (array (mut i64)))
  (type $node (struct (field (ref null $node)) (field (ref null $a))))
  (global $keep (mut (ref null $node)) (ref.null $node))
  (func (export "hoard") (param $n i32) (param $len i32)
    (loop $l
      (global.set $keep (struct.new $node (global.get $keep) (array.new $a (i64.const 1) (local.get $len))))
      (br_if $l (local.tee $n (i32.sub (local.get $n) (i32.const 1)))))))`
	for _, native := range []bool{true, false} {
		for _, n := range []uint64{8, 30, 100, 5000} {
			inst, err := budgetInstance(t, src, NewBudget(256<<10))
			if err != nil {
				t.Fatal(err)
			}
			withNative(inst, native, func() {
				if _, err := callExport(t, inst, "hoard", Val{N: 4}, Val{N: n}); err != nil {
					t.Fatalf("native %v: a few arrays of %d: %v", native, n, err)
				}
				if _, err := callExport(t, inst, "hoard", Val{N: 100_000}, Val{N: n}); !errors.Is(err, ErrMemoryLimit) {
					t.Fatalf("native %v: arrays of %d: got %v, want ErrMemoryLimit", native, n, err)
				}
			})
		}
	}
}

// TestNativeAllocConvention checks the allocation stub's frame: its Go
// runs on the goroutine that entered the code, from a frame the collector
// and the unwinder walk through to the Go that entered it -- which may
// grow the stack and collect -- and the results are the Go table's, the
// handlers having kept what they need across it.
func TestNativeAllocConvention(t *testing.T) {
	nativeOnly(t)
	inst := instantiateReg(t, nativeAllocSrc)
	var calls int
	var bad []string
	g := goid()
	testOf(inst).allocHook = func() {
		calls++
		pcs := make([]uintptr, 64)
		frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
		after, via := false, false
		for {
			fr, more := frames.Next()
			if after {
				if !strings.HasSuffix(fr.Function, ".wasmNativeEnter") && !strings.HasSuffix(fr.Function, ".nativeRun") {
					bad = append(bad, "wasmNativeAlloc's caller is "+fr.Function)
				}
				after = false
			}
			if strings.HasSuffix(fr.Function, ".wasmNativeAlloc") {
				via, after = true, true
			}
			if !more {
				break
			}
		}
		if !via {
			bad = append(bad, "not called through wasmNativeAlloc")
		}
		if got := goid(); got != g {
			bad = append(bad, "goroutine "+got+", want "+g)
		}
		if deep(2000) != 2000*2001/2 {
			bad = append(bad, "the deep recursion's sum")
		}
		if calls%8 == 1 {
			runtime.GC()
		}
	}
	ref := instantiateReg(t, nativeAllocSrc)
	for _, c := range nativeAllocCalls() {
		var want, got []Val
		var werr, gerr error
		withNative(ref, false, func() {
			ext, _ := ref.Export(c.name)
			want, werr = Invoke(context.Background(), ext.Func, c.args)
		})
		withNative(inst, true, func() {
			ext, _ := inst.Export(c.name)
			got, gerr = Invoke(context.Background(), ext.Func, c.args)
		})
		if (werr == nil) != (gerr == nil) || len(want) != len(got) || len(want) > 0 && want[0].N != got[0].N {
			t.Fatalf("%s%v = %v, %v; want %v, %v", c.name, c.args, got, gerr, want, werr)
		}
	}
	if calls == 0 {
		t.Fatal("the allocation stub was not called")
	}
	if len(bad) > 0 {
		t.Fatalf("%d calls; %s", calls, strings.Join(bad[:min(len(bad), 5)], "; "))
	}
}
