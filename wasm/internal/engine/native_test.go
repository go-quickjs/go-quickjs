package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
)

// nativeOnly skips a test where the native handlers are not built.
func nativeOnly(t *testing.T) {
	t.Helper()
	if !nativeBuilt {
		t.Skip("no native handlers on this architecture")
	}
}

// countNativeExits runs f with native code's exits counted, returning
// their number.
func countNativeExits(f func()) int64 {
	var counts [256]int64
	nativeExitCounts = &counts
	defer func() { nativeExitCounts = nil }()
	f()
	total := int64(0)
	for _, n := range counts {
		total += n
	}
	return total
}

// withNative runs f with the native handlers on or off.
func withNative(on bool, f func()) {
	was := SetRegNative(on)
	defer SetRegNative(was)
	f()
}

// TestNativeLayout checks internal/regdef's offsets, which the generator
// writes the handlers with, against the engine's structures.
func TestNativeLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the native handlers' layout is a 64-bit one")
	}
	var c nativeCtx
	var f frame
	var m Memory
	var g Global
	for _, x := range []struct {
		what      string
		got, want uintptr
	}{
		{"l", unsafe.Offsetof(c.l), regdef.CtxL},
		{"code", unsafe.Offsetof(c.code), regdef.CtxCode},
		{"table", unsafe.Offsetof(c.table), regdef.CtxTable},
		{"pc", unsafe.Offsetof(c.pc), regdef.CtxPC},
		{"f", unsafe.Offsetof(c.f), regdef.CtxF},
		{"goTable", unsafe.Offsetof(c.goTable), regdef.CtxGoTable},
		{"slowTable", unsafe.Offsetof(c.slowTable), regdef.CtxSlowTable},
		{"callH", unsafe.Offsetof(c.callH), regdef.CtxCallH},
		{"after", unsafe.Offsetof(c.after), regdef.CtxAfter},
		{"mem0", unsafe.Offsetof(c.mem0), regdef.CtxMem0},
		{"globals", unsafe.Offsetof(c.globals), regdef.CtxGlobals},
		{"tables", unsafe.Offsetof(c.tables), regdef.CtxTables},
		{"exits", unsafe.Offsetof(c.exits), regdef.CtxExits},
		{"the context's size", unsafe.Sizeof(c), regdef.CtxSize},
		{"frame.tick", unsafe.Offsetof(f.tick), regdef.FrameTick},
		{"Memory.buf", unsafe.Offsetof(m.buf), regdef.MemoryBuf},
		{"Global.Num", unsafe.Offsetof(g.Num), regdef.GlobalNum},
		{"a slice", unsafe.Sizeof([]int32(nil)), regdef.SliceSize},
	} {
		if x.got != x.want {
			t.Errorf("%s at %d, internal/regdef says %d", x.what, x.got, x.want)
		}
	}
	if unsafe.Sizeof(f.tick) != 4 {
		t.Errorf("frame.tick is %d bytes, the handlers take 4", unsafe.Sizeof(f.tick))
	}
}

// TestNativeGenerated fails while native_handlers_amd64.s differs from what
// internal/nativegen writes; go generate ./wasm/internal/engine rewrites it.
func TestNativeGenerated(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the generator")
	}
	checkGenerated(t, "./internal/nativegen", "native_handlers_amd64.s")
}

// TestNativeCallHConvention checks the convention the exit stub calls Go
// in, written by hand: the Go handler gets the frame's slots, the
// instruction, its pc and the frame in the registers Go passes them in, on
// the goroutine that entered the code, with a stack it can grow, X15 zero,
// and a frame the collector and the unwinder walk through to the function
// that entered the code. Where the toolchain's convention is not this one,
// it says which part differs.
func TestNativeCallHConvention(t *testing.T) {
	nativeOnly(t)
	op := rCall
	orig := regTable[op]
	defer func() { regTable[op] = orig }()
	want := goid()
	var calls int
	var bad []string
	regTable[op] = func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
		calls++
		pcs := make([]uintptr, 32)
		frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
		afterCallH, viaCallH := false, false
		for {
			fr, more := frames.Next()
			if afterCallH {
				if !strings.HasSuffix(fr.Function, ".wasmNativeEnter") && !strings.HasSuffix(fr.Function, ".nativeRun") {
					bad = append(bad, "wasmNativeCallH's caller is "+fr.Function)
				}
				afterCallH = false
			}
			if strings.HasSuffix(fr.Function, ".wasmNativeCallH") {
				viaCallH, afterCallH = true, true
			}
			if !more {
				break
			}
		}
		switch {
		case !viaCallH:
			bad = append(bad, "not called through wasmNativeCallH")
		case f == nil || f.rc == nil:
			bad = append(bad, "no frame (the fourth argument, SI to DI)")
		case l != unsafe.Pointer(&f.l[0]):
			bad = append(bad, "the slots (the first argument, AX) are not the frame's")
		case pc < 0 || int(pc) >= len(f.rc.code):
			bad = append(bad, fmt.Sprintf("pc (the third argument, CX) %d out of the code", pc))
		case f.rc.code[pc] != w || uint8(w) != op:
			bad = append(bad, fmt.Sprintf("the instruction (the second argument, BX) %#x is not the code's at pc %d", w, pc))
		}
		if g := goid(); g != want {
			bad = append(bad, fmt.Sprintf("goroutine (R14) %q, want %q", g, want))
		}
		if deep(2000) != 2000*2001/2 {
			bad = append(bad, "the deep recursion's sum")
		}
		if !zeroed() {
			bad = append(bad, "a fresh array not zero (X15)")
		}
		if calls%50 == 1 {
			runtime.GC()
		}
		return orig(l, w, pc, f)
	}
	withNative(true, func() {
		inst := instantiateMode(t, regKernels, true)
		got, err := call(t, inst, "fib", 15)
		if err != nil || got[0].N != 610 {
			t.Fatalf("fib(15) = %v, %v", got, err)
		}
	})
	if calls == 0 {
		t.Fatal("no call left native code")
	}
	if len(bad) > 0 {
		t.Fatalf("%d calls; %s", calls, strings.Join(bad[:min(len(bad), 5)], "; "))
	}
}

// goid returns the goroutine's header line, its id.
func goid() string {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	s, _, _ := strings.Cut(string(b), "[")
	return s
}

//go:noinline
func deep(n int) int {
	var pad [64]byte
	if n == 0 {
		return int(pad[0])
	}
	return n + deep(n-1)
}

//go:noinline
func zeroed() bool {
	var a [16]uint64
	for _, x := range a {
		if x != 0 {
			return false
		}
	}
	return true
}

// nativeCases returns a module and calls of every operation the native
// handlers do, over its edge cases.
func nativeCases() (string, []regCall) {
	var b strings.Builder
	var calls []regCall
	b.WriteString("(module (memory 1) (global $g32 (mut i32) (i32.const 7)) (global $g64 (mut i64) (i64.const -9))\n")
	vals := map[string][]uint64{
		"i32": {0, 1, 0xffffffff, 2, 31, 32, 33, 0x7fffffff, 0x80000000, 12345, 0xfffe7961},
		"i64": {0, 1, 0xffffffffffffffff, 63, 64, 65, 0x7fffffffffffffff, 0x8000000000000000, 0x123456789, 0xfffffff6789abcdf, 0xffffffff},
	}
	imms := []int64{-32768, -1, 0, 1, 31, 32, 63, 64, 32767}
	bin := []string{"add", "sub", "mul", "and", "or", "xor", "shl", "shr_s", "shr_u", "rotl", "rotr",
		"div_s", "div_u", "rem_s", "rem_u", "eq", "ne", "lt_s", "lt_u", "gt_s", "gt_u", "le_s", "le_u", "ge_s", "ge_u"}
	cmps := []string{"eq", "ne", "lt_s", "lt_u", "gt_s", "gt_u", "le_s", "le_u", "ge_s", "ge_u"}
	for _, ty := range []string{"i32", "i64"} {
		for _, op := range bin {
			r := ty
			if contains(cmps, op) {
				r = "i32"
			}
			name := ty + "." + op
			fmt.Fprintf(&b, "(func (export %q) (param %s %s) (result %s) (%s.%s (local.get 0) (local.get 1)))\n", name, ty, ty, r, ty, op)
			for _, x := range vals[ty] {
				for _, y := range vals[ty] {
					calls = append(calls, regCall{name, []Val{{N: x}, {N: y}}})
				}
			}
			for i, k := range imms {
				in := fmt.Sprintf("%s.%s.imm%d", ty, op, i)
				fmt.Fprintf(&b, "(func (export %q) (param %s) (result %s) (%s.%s (local.get 0) (%s.const %d)))\n", in, ty, r, ty, op, ty, k)
				for _, x := range vals[ty] {
					calls = append(calls, regCall{in, []Val{{N: x}}})
				}
			}
		}
		for _, op := range cmps {
			name := ty + ".br." + op
			fmt.Fprintf(&b, "(func (export %q) (param %s %s) (result i32) (if (result i32) (%s.%s (local.get 0) (local.get 1)) (then (i32.const 1)) (else (i32.const 0))))\n", name, ty, ty, ty, op)
			for _, x := range vals[ty] {
				for _, y := range vals[ty] {
					calls = append(calls, regCall{name, []Val{{N: x}, {N: y}}})
				}
			}
			for i, k := range imms {
				in := fmt.Sprintf("%s.br.%s.imm%d", ty, op, i)
				fmt.Fprintf(&b, "(func (export %q) (param %s) (result i32) (block (br_if 0 (%s.%s (local.get 0) (%s.const %d))) (return (i32.const 0))) (i32.const 1))\n", in, ty, ty, op, ty, k)
				for _, x := range vals[ty] {
					calls = append(calls, regCall{in, []Val{{N: x}}})
				}
			}
		}
		un := []string{"eqz", "clz", "ctz", "extend8_s", "extend16_s"}
		if ty == "i64" {
			un = append(un, "extend32_s")
		}
		for _, op := range un {
			r := ty
			if op == "eqz" {
				r = "i32"
			}
			name := ty + "." + op
			fmt.Fprintf(&b, "(func (export %q) (param %s) (result %s) (%s.%s (local.get 0)))\n", name, ty, r, ty, op)
			for _, x := range vals[ty] {
				calls = append(calls, regCall{name, []Val{{N: x}}})
			}
		}
	}
	fvals := map[string][]uint64{
		"f32": {0, 0x80000000, 0x3f800000, 0xbf800000, 0x7f800000, 0xff800000, 0x7fc00000, 0x7fa00001, 0xffc00123,
			1, 0x4f000000, 0xcf000000, 0x4f800000, 0x5f000000, 0x3f000000, 0xbf7fffff, 0x4effffff, 0xcf000001},
		"f64": {0, 1 << 63, 0x3ff0000000000000, 0xbff0000000000000, 0x7ff0000000000000, 0xfff0000000000000,
			0x7ff8000000000000, 0x7ff0000000000001, 0xfff8000000000123, 1, 0x41dfffffffe00000, 0xc1e0000000100000,
			0xc1e0000000200000, 0x41f0000000000000, 0x43e0000000000000, 0x43f0000000000000, 0xbfe0000000000000,
			0x41efffffffffffff, 0xbfefffffffffffff},
	}
	for _, ty := range []string{"f32", "f64"} {
		for _, op := range []string{"add", "sub", "mul", "div", "min", "max", "eq", "ne", "lt", "gt", "le", "ge"} {
			r := ty
			if contains([]string{"eq", "ne", "lt", "gt", "le", "ge"}, op) {
				r = "i32"
				for _, form := range []string{"if", "br"} {
					name := ty + "." + form + "." + op
					if form == "if" {
						fmt.Fprintf(&b, "(func (export %q) (param %s %s) (result i32) (if (result i32) (%s.%s (local.get 0) (local.get 1)) (then (i32.const 1)) (else (i32.const 0))))\n", name, ty, ty, ty, op)
					} else {
						fmt.Fprintf(&b, "(func (export %q) (param %s %s) (result i32) (block (br_if 0 (%s.%s (local.get 0) (local.get 1))) (return (i32.const 0))) (i32.const 1))\n", name, ty, ty, ty, op)
					}
					for _, x := range fvals[ty] {
						for _, y := range fvals[ty] {
							calls = append(calls, regCall{name, []Val{{N: x}, {N: y}}})
						}
					}
				}
			}
			name := ty + "." + op
			fmt.Fprintf(&b, "(func (export %q) (param %s %s) (result %s) (%s.%s (local.get 0) (local.get 1)))\n", name, ty, ty, r, ty, op)
			for _, x := range fvals[ty] {
				for _, y := range fvals[ty] {
					calls = append(calls, regCall{name, []Val{{N: x}, {N: y}}})
				}
			}
		}
		for _, op := range []string{"abs", "neg", "sqrt"} {
			name := ty + "." + op
			fmt.Fprintf(&b, "(func (export %q) (param %s) (result %s) (%s.%s (local.get 0)))\n", name, ty, ty, ty, op)
			for _, x := range fvals[ty] {
				calls = append(calls, regCall{name, []Val{{N: x}}})
			}
		}
		for _, it := range []string{"i32", "i64"} {
			for _, sg := range []string{"s", "u"} {
				name := it + ".trunc_" + ty + "_" + sg
				fmt.Fprintf(&b, "(func (export %q) (param %s) (result %s) (%s (local.get 0)))\n", name, ty, it, name)
				for _, x := range fvals[ty] {
					calls = append(calls, regCall{name, []Val{{N: x}}})
				}
				cn := ty + ".convert_" + it + "_" + sg
				fmt.Fprintf(&b, "(func (export %q) (param %s) (result %s) (%s (local.get 0)))\n", cn, it, ty, cn)
				for _, x := range vals[it] {
					calls = append(calls, regCall{cn, []Val{{N: x}}})
				}
			}
		}
	}
	b.WriteString(`(func (export "demote") (param f64) (result f32) (f32.demote_f64 (local.get 0)))
(func (export "promote") (param f32) (result f64) (f64.promote_f32 (local.get 0)))
`)
	for _, x := range fvals["f64"] {
		calls = append(calls, regCall{"demote", []Val{{N: x}}})
	}
	for _, x := range fvals["f32"] {
		calls = append(calls, regCall{"promote", []Val{{N: x}}})
	}
	b.WriteString(`(func (export "wrap") (param i64) (result i32) (i32.wrap_i64 (local.get 0)))
(func (export "ext_s") (param i32) (result i64) (i64.extend_i32_s (local.get 0)))
(func (export "consts") (result i64) (i64.add (i64.add (i64.const 0x123456789abcdef) (i64.const -5)) (i64.extend_i32_u (i32.const 0xfffffff0))))
(func (export "sel") (param i64 i64 i32) (result i64) (select (local.get 0) (local.get 1) (local.get 2)))
(func (export "globals") (param i32 i64) (result i64)
  (global.set $g32 (i32.add (global.get $g32) (local.get 0)))
  (global.set $g64 (i64.mul (global.get $g64) (local.get 1)))
  (i64.add (i64.extend_i32_u (global.get $g32)) (global.get $g64)))
(func (export "table") (param i32) (result i32)
  (block $d (block $c (block $b (block $a (br_table $a $b $c $d (local.get 0)))
    (return (i32.const 10))) (return (i32.const 11))) (return (i32.const 12))) (i32.const 13))
(func (export "count") (param i32) (result i32) (local i32)
  (loop $l (local.set 1 (i32.add (local.get 1) (i32.const 3)))
    (br_if $l (i32.ne (local.tee 0 (i32.sub (local.get 0) (i32.const 1))) (i32.const 0))))
  (local.get 1))
(func (export "v128") (param i64) (result i64) (local v128 v128)
  (local.set 1 (v128.const i64x2 1 2)) (local.set 2 (local.get 1)) (i64x2.extract_lane 1 (local.get 2)))
`)
	calls = append(calls, regCall{"wrap", []Val{{N: 0x1234567890}}}, regCall{"ext_s", []Val{{N: 0x80000001}}},
		regCall{"consts", nil}, regCall{"sel", []Val{{N: 1}, {N: 2}, {N: 0}}}, regCall{"sel", []Val{{N: 1}, {N: 2}, {N: 5}}},
		regCall{"globals", []Val{{N: 5}, {N: 3}}}, regCall{"globals", []Val{{N: 0xffffffff}, {N: 1 << 40}}},
		regCall{"count", []Val{{N: 10000}}})
	for i := uint64(0); i < 6; i++ {
		calls = append(calls, regCall{"table", []Val{{N: i}}})
	}
	calls = append(calls, regCall{"table", []Val{{N: 0xffffffff}}})
	type mem struct {
		ty, op string
		width  uint64
	}
	for _, m := range []mem{{"i32", "load", 4}, {"i64", "load", 8}, {"f32", "load", 4}, {"f64", "load", 8},
		{"i32", "load8_s", 1}, {"i32", "load8_u", 1}, {"i32", "load16_s", 2}, {"i32", "load16_u", 2},
		{"i64", "load8_s", 1}, {"i64", "load8_u", 1}, {"i64", "load16_s", 2}, {"i64", "load16_u", 2},
		{"i64", "load32_s", 4}, {"i64", "load32_u", 4}} {
		name := m.ty + "." + m.op
		fmt.Fprintf(&b, "(func (export %q) (param i32) (result %s) (%s.%s offset=3 (local.get 0)))\n", name, m.ty, m.ty, m.op)
		for _, a := range []uint64{0, 100, 65536 - 3 - m.width, 65536 - 2 - m.width, 0xffffffff} {
			calls = append(calls, regCall{name, []Val{{N: a}}})
		}
	}
	for _, m := range []mem{{"i32", "store", 4}, {"i64", "store", 8}, {"f32", "store", 4}, {"f64", "store", 8},
		{"i32", "store8", 1}, {"i32", "store16", 2}, {"i64", "store8", 1}, {"i64", "store16", 2}, {"i64", "store32", 4}} {
		name := m.ty + "." + m.op
		fmt.Fprintf(&b, "(func (export %q) (param i32 %s) (result i64) (%s.%s offset=5 (local.get 0) (local.get 1)) (i64.load offset=5 (local.get 0)))\n", name, m.ty, m.ty, m.op)
		v := uint64(0xfedcba9876543210)
		if m.ty == "i32" || m.ty == "f32" {
			v = 0x87654321
		}
		for _, a := range []uint64{0, 64, 65536 - 13, 65536 - 5 - m.width, 65536 - 4 - m.width} {
			calls = append(calls, regCall{name, []Val{{N: a}, {N: v}}})
		}
	}
	b.WriteString(")")
	return b.String(), calls
}

// nativeMayExit reports whether a call of export name may leave native
// code where it returns: a division (by 0 or -1), a loop's poll, and the
// operations in the second table -- i64 comparisons made booleans with an
// immediate.
func nativeMayExit(name string) bool {
	if strings.Contains(name, "div") || strings.Contains(name, "rem") || name == "count" || strings.Contains(name, "trunc_") || strings.Contains(name, "convert_i64_u") {
		return true
	}
	return strings.HasPrefix(name, "i64.") && strings.Contains(name, ".imm") && !strings.Contains(name, ".br.") &&
		contains([]string{"eq", "ne", "lt_s", "lt_u", "gt_s", "gt_u", "le_s", "le_u", "ge_s", "ge_u"}, strings.Split(name, ".")[1])
}

func contains(s []string, x string) bool {
	for _, y := range s {
		if y == x {
			return true
		}
	}
	return false
}

// TestNativeMatchesGoTable runs every operation the native handlers do, over
// its edge cases, natively and on the Go table, and fails on a different
// result, a different trap or trap frame; and, where the call returned, on
// an exit to Go but a division's of 0 or -1 and a loop's poll.
func TestNativeMatchesGoTable(t *testing.T) {
	nativeOnly(t)
	src, calls := nativeCases()
	native := instantiateMode(t, src, true)
	goTable := instantiateMode(t, src, true)
	run := func(inst *Instance, c regCall) ([]Val, error) {
		ext, ok := inst.Export(c.name)
		if !ok {
			t.Fatalf("no export %q", c.name)
		}
		return Invoke(context.Background(), ext.Func, c.args)
	}
	exited := 0
	for _, c := range calls {
		var got, want []Val
		var gerr, werr error
		var exits int64
		withNative(true, func() { exits = countNativeExits(func() { got, gerr = run(native, c) }) })
		withNative(false, func() { want, werr = run(goTable, c) })
		if (gerr == nil) != (werr == nil) || gerr != nil && gerr.Error() != werr.Error() {
			t.Errorf("%s%v: native %v, the Go table's %v", c.name, c.args, gerr, werr)
			continue
		}
		var gt, wt *Trap
		if errors.As(gerr, &gt) && errors.As(werr, &wt) && (len(gt.Frames) != len(wt.Frames) || len(gt.Frames) > 0 && gt.Frames[0].Offset != wt.Frames[0].Offset) {
			t.Errorf("%s%v: native trap frames %+v, the Go table's %+v", c.name, c.args, gt.Frames, wt.Frames)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s%v = %#x, the Go table's %#x", c.name, c.args, got[i].N, want[i].N)
				break
			}
		}
		if gerr == nil && exits > 0 && !nativeMayExit(c.name) {
			t.Errorf("%s%v: %d exits", c.name, c.args, exits)
		}
		if exits > 0 {
			exited++
		}
	}
	t.Logf("%d calls compared, %d exited to Go", len(calls), exited)
}

// TestNativeOff checks the switch: with the native handlers off, native
// code never runs.
func TestNativeOff(t *testing.T) {
	nativeOnly(t)
	inst := instantiateMode(t, regKernels, true)
	var exits int64
	withNative(false, func() {
		exits = countNativeExits(func() {
			if _, err := call(t, inst, "fib", 12); err != nil {
				t.Fatal(err)
			}
		})
	})
	if exits != 0 {
		t.Fatalf("%d native exits with the native handlers off", exits)
	}
	withNative(true, func() {
		exits = countNativeExits(func() {
			if _, err := call(t, inst, "fib", 12); err != nil {
				t.Fatal(err)
			}
		})
	})
	if exits == 0 {
		t.Fatal("no native exits for fib's calls with the native handlers on")
	}
}

// nativeHandles reports whether op has a native handler of its own: its
// table entry is not the exit stub's, which rInvalid's is.
func nativeHandles(op regOp) bool {
	t := (*[256]uintptr)(wasmNativeTablePtr())
	return t[op] != t[rInvalid]
}

// TestNativeStoresNoReference checks the rule the native handlers keep for
// the collector: no operation that writes a reference slot, a reference
// global or anything else a reference may be stored in has a native
// handler, so that native code stores no pointer and does the same work
// whether the collector marks or not; every such operation leaves to its
// Go handler, whose stores have Go's write barrier.
func TestNativeStoresNoReference(t *testing.T) {
	nativeOnly(t)
	for _, op := range []regOp{rRefMov, rRefNull, rExt, rSIMD, rFar, rCall, rReturnCall} {
		if nativeHandles(op) {
			t.Errorf("operation %d has a native handler", op)
		}
	}
	n := 0
	for op := 0; op < 256; op++ {
		if nativeHandles(regOp(op)) {
			n++
		}
	}
	t.Logf("%d operations have native handlers", n)
}
