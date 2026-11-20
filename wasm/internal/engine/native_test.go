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

// testOf is inst's test switches (Instance's test), made at the first.
func testOf(inst *Instance) *instanceTest {
	if inst.test == nil {
		inst.test = new(instanceTest)
	}
	return inst.test
}

// countNativeExits runs f with inst's native code's exits counted,
// returning their number.
func countNativeExits(inst *Instance, f func()) int64 {
	var counts [256]int64
	t := testOf(inst)
	was := t.exits
	t.exits = &counts
	defer func() { t.exits = was }()
	f()
	total := int64(0)
	for _, n := range counts {
		total += n
	}
	return total
}

// withNative runs f with the register code of inst's Code run by the
// native handlers, where they are built, or by the Go table's loop: the
// code compiled before and the code compiled meanwhile.
func withNative(inst *Instance, on bool, f func()) {
	c := inst.Code
	was := c.native
	setNative(c, on && nativeBuilt)
	defer setNative(c, was)
	f()
}

// setNative has c's register code run by the native handlers or not,
// that compiled already and that compiled from now on.
func setNative(c *Code, on bool) {
	c.native = on
	for i := range c.fns {
		if cd := c.fns[i].Load(); cd != nil && cd.reg != nil {
			cd.reg.native = on
		}
	}
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
	var x exec
	var cs callSite
	var fn Function
	var cd compiled
	var rc regCode
	var st regSite
	var mv regMove
	var inst Instance
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
		{"r", unsafe.Offsetof(c.r), regdef.CtxR},
		{"x", unsafe.Offsetof(c.x), regdef.CtxX},
		{"inst", unsafe.Offsetof(c.inst), regdef.CtxInst},
		{"funcs", unsafe.Offsetof(c.funcs), regdef.CtxFuncs},
		{"fns", unsafe.Offsetof(c.fns), regdef.CtxFns},
		{"sites", unsafe.Offsetof(c.sites), regdef.CtxSites},
		{"calls", unsafe.Offsetof(c.calls), regdef.CtxCalls},
		{"maxDepth", unsafe.Offsetof(c.maxDepth), regdef.CtxMaxDepth},
		{"storeRef", unsafe.Offsetof(c.storeRef), regdef.CtxStoreRef},
		{"wb", unsafe.Offsetof(c.wb), regdef.CtxWB},
		{"caller", unsafe.Offsetof(c.caller), regdef.CtxCaller},
		{"retAddr", unsafe.Offsetof(c.retAddr), regdef.CtxRetAddr},
		{"site", unsafe.Offsetof(c.site), regdef.CtxSite},
		{"nc", unsafe.Offsetof(c.nc), regdef.CtxNc},
		{"nsp", unsafe.Offsetof(c.nsp), regdef.CtxNsp},
		{"fn", unsafe.Offsetof(c.fn), regdef.CtxFn},
		{"cd", unsafe.Offsetof(c.cd), regdef.CtxCd},
		{"rc", unsafe.Offsetof(c.rc), regdef.CtxRc},
		{"extTable", unsafe.Offsetof(c.extTable), regdef.CtxExtTable},
		{"rch", unsafe.Offsetof(c.rch), regdef.CtxRch},
		{"rsp", unsafe.Offsetof(c.rsp), regdef.CtxRsp},
		{"simdTable", unsafe.Offsetof(c.simdTable), regdef.CtxSIMDTable},
		{"gc", unsafe.Offsetof(c.gc), regdef.CtxGC},
		{"i31", unsafe.Offsetof(c.i31), regdef.CtxI31},
		{"regCode.gc", unsafe.Offsetof(rc.gc), regdef.RcGC},
		{"a regGC", unsafe.Sizeof(regGC{}), regdef.GCSize},
		{"regGC.nullable", unsafe.Offsetof(regGC{}.nullable), regdef.GCNullable},
		{"regGC.mode", unsafe.Offsetof(regGC{}.mode), regdef.GCMode},
		{"header.kind", unsafe.Offsetof(header{}.kind), regdef.HeaderKind},
		{"ObjStruct", uintptr(ObjStruct), regdef.ObjStruct},
		{"ObjArray", uintptr(ObjArray), regdef.ObjArray},
		{"regGC.depth", unsafe.Offsetof(regGC{}.depth), regdef.GCDepth},
		{"regGC.canon", unsafe.Offsetof(regGC{}.canon), regdef.GCCanon},
		{"rtt.supers", unsafe.Offsetof(rtt{}.supers), regdef.RttSupers},
		{"Array.e", unsafe.Offsetof(Array{}.e), regdef.ArrayElems},
		{"Array.n", unsafe.Offsetof(Array{}.n), regdef.ArrayLen},
		{"header.rtt", unsafe.Offsetof(header{}.rtt), 0},
		{"xCallRef", uintptr(xCallRef), regdef.XCallRef},
		{"xSelectRef", uintptr(xSelectRef), regdef.XSelectRef},
		{"xSelect128", uintptr(xSelect128), regdef.XSelect128},
		{"xRefIsNull", uintptr(xRefIsNull), regdef.XRefIsNull},
		{"xRefEq", uintptr(xRefEq), regdef.XRefEq},
		{"xRefAsNonNull", uintptr(xRefAsNonNull), regdef.XRefAsNonNull},
		{"xBrNull", uintptr(xBrNull), regdef.XBrNull},
		{"xBrNonNull", uintptr(xBrNonNull), regdef.XBrNonNull},
		{"xBrOnCast", uintptr(xBrOnCast), regdef.XBrOnCast},
		{"xBrOnCastFail", uintptr(xBrOnCastFail), regdef.XBrOnCastFail},
		{"frame.l", unsafe.Offsetof(f.l), regdef.FrameL},
		{"frame.r", unsafe.Offsetof(f.r), regdef.FrameR},
		{"frame.inst", unsafe.Offsetof(f.inst), regdef.FrameInst},
		{"frame.osr", unsafe.Offsetof(f.osr), regdef.FrameOsr},
		{"frame.rc", unsafe.Offsetof(f.rc), regdef.FrameRc},
		{"frame.tickSet", unsafe.Offsetof(f.tickSet), regdef.FrameTickSet},
		{"frame.sig", unsafe.Offsetof(f.sig), regdef.FrameSig},
		{"frame.nctx", unsafe.Offsetof(f.nctx), regdef.FrameNctx},
		{"exec.nchunks", unsafe.Offsetof(x.nchunks), regdef.XNchunks},
		{"exec.nc", unsafe.Offsetof(x.nc), regdef.XNc},
		{"exec.rchunks", unsafe.Offsetof(x.rchunks), regdef.XRchunks},
		{"exec.rc", unsafe.Offsetof(x.rc), regdef.XRc},
		{"exec.rsp", unsafe.Offsetof(x.rsp), regdef.XRsp},
		{"exec.nsp", unsafe.Offsetof(x.nsp), regdef.XNsp},
		{"exec.depth", unsafe.Offsetof(x.depth), regdef.XDepth},
		{"exec.calls", unsafe.Offsetof(x.calls), regdef.XCalls},
		{"exec.poll", unsafe.Offsetof(x.poll), regdef.XPoll},
		{"exec.frames", unsafe.Offsetof(x.frames), regdef.XFrames},
		{"callSite.offset", unsafe.Offsetof(cs.offset), regdef.CallSiteOffset},
		{"a callSite", unsafe.Sizeof(cs), regdef.CallSiteSize},
		{"Function.Inst", unsafe.Offsetof(fn.Inst), regdef.FnInst},
		{"Function.Index", unsafe.Offsetof(fn.Index), regdef.FnIndex},
		{"compiled.nslots", unsafe.Offsetof(cd.nslots), regdef.CdNslots},
		{"compiled.rslots", unsafe.Offsetof(cd.rslots), regdef.CdRslots},
		{"compiled.refs", unsafe.Offsetof(cd.refs), regdef.CdRefs},
		{"compiled.reg", unsafe.Offsetof(cd.reg), regdef.CdReg},
		{"regCode.code", unsafe.Offsetof(rc.code), regdef.RcCode},
		{"regCode.sites", unsafe.Offsetof(rc.sites), regdef.RcSites},
		{"regCode.tables", unsafe.Offsetof(rc.tables), regdef.RcTables},
		{"regCode.callee", unsafe.Offsetof(rc.callee), regdef.RcCallee},
		{"regSite.fn", unsafe.Offsetof(st.fn), regdef.SiteFn},
		{"regSite.at", unsafe.Offsetof(st.at), regdef.SiteAt},
		{"regSite.args", unsafe.Offsetof(st.args), regdef.SiteArgs},
		{"regSite.rets", unsafe.Offsetof(st.rets), regdef.SiteRets},
		{"regSite.table", unsafe.Offsetof(st.table), regdef.SiteTable},
		{"regSite.checked", unsafe.Offsetof(st.checked), regdef.SiteChecked},
		{"regSite.target", unsafe.Offsetof(st.target), regdef.SiteTarget},
		{"regSite.want", unsafe.Offsetof(st.want), regdef.SiteWant},
		{"a regSite", unsafe.Sizeof(st), regdef.SiteSize},
		{"Function.Type", unsafe.Offsetof(fn.Type), regdef.FnType},
		{"Instance.Tables", unsafe.Offsetof(inst.Tables), regdef.InstTables},
		{"Table.Elems", unsafe.Offsetof(Table{}.Elems), regdef.TableElems},
		{"xCallIndirect", uintptr(xCallIndirect), regdef.XCallIndirect},
		{"regMove.from", unsafe.Offsetof(mv.from), regdef.MoveFrom},
		{"regMove.to", unsafe.Offsetof(mv.to), regdef.MoveTo},
		{"regMove.k", unsafe.Offsetof(mv.k), regdef.MoveK},
		{"a regMove", unsafe.Sizeof(mv), regdef.MoveSize},
		{"vkV128", uintptr(vkV128), regdef.MoveV128},
		{"vkRef", uintptr(vkRef), regdef.MoveRef},
		{"a Ref", unsafe.Sizeof(Ref{}), regdef.RefSize},
		{"Ref.p", unsafe.Offsetof(Ref{}.p), 0},
		{"frame.tick", unsafe.Offsetof(f.tick), regdef.FrameTick},
		{"Memory.buf", unsafe.Offsetof(m.buf), regdef.MemoryBuf},
		{"Memory.Is64", unsafe.Offsetof(m.Is64), regdef.MemoryIs64},
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
	checkGenerated(t, "./internal/nativegen", "native_handlers_amd64.s", "native_handlers_arm64.s")
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
	op := rExt
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
	inst := instantiateReg(t, regKernels)
	withNative(inst, true, func() {
		for i := uint64(0); i < 120; i++ {
			got, err := call(t, inst, "pop", i)
			if err != nil || got[0].N != uint64(bitsOnes(i)) {
				t.Fatalf("pop(%d) = %v, %v", i, got, err)
			}
		}
	})
	if calls == 0 {
		t.Fatal("no call left native code")
	}
	if len(bad) > 0 {
		t.Fatalf("%d calls; %s", calls, strings.Join(bad[:min(len(bad), 5)], "; "))
	}
}

func bitsOnes(x uint64) int {
	n := 0
	for ; x != 0; x &= x - 1 {
		n++
	}
	return n
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
	calls = nativeSIMDCases(&b, calls, vals)
	b.WriteString(")")
	return b.String(), calls
}

// nativeSIMDCases adds to the module of nativeCases a function for each
// SIMD operator with a native handler, and calls of it over vectors of
// edge cases of every lane type: zeros, ones, the signed extremes, NaNs
// quiet and signalling with payloads and signs, infinities, zeros of
// either sign, subnormals and the truncations' bounds.
func nativeSIMDCases(b *strings.Builder, calls []regCall, vals map[string][]uint64) []regCall {
	vecs := [][2]uint64{
		{0, 0}, {^uint64(0), ^uint64(0)}, {1 << 63, 1<<63 - 1}, {0x80ff7f0001fe8081, 0x0123456789abcdef},
		{0x7fc00000ffc00001, 0x7fa0000000000001}, {0x3f800000bf800000, 0x7f800000ff800000},
		{0x7ff8000000000001, 0xfff4000000000000}, {1 << 63, 0x3ff0000000000000},
		{0x4f0000004effffff, 0xcf0000003fc00000}, {0x400c000000000000, 0xc004000000000000},
		{0x0001000200030004, 0xfffefffd80008001}, {0x7fff80007fffffff, 0x8000000000000001},
	}
	v := func(x [2]uint64) Val { return Val{N: x[0], Hi: x[1]} }
	bin := []string{"v128.and", "v128.or", "v128.xor", "v128.andnot", "i8x16.swizzle"}
	for _, ty := range []string{"i8x16", "i16x8", "i32x4", "i64x2"} {
		ops := []string{"add", "sub", "eq", "ne", "lt_s", "gt_s", "le_s", "ge_s"}
		switch ty {
		case "i8x16":
			ops = append(ops, "add_sat_s", "add_sat_u", "sub_sat_s", "sub_sat_u", "min_s", "min_u", "max_s", "max_u",
				"avgr_u", "narrow_i16x8_s", "narrow_i16x8_u", "lt_u", "gt_u", "le_u", "ge_u")
		case "i16x8":
			ops = append(ops, "add_sat_s", "add_sat_u", "sub_sat_s", "sub_sat_u", "mul", "min_s", "min_u", "max_s", "max_u",
				"avgr_u", "narrow_i32x4_s", "narrow_i32x4_u", "lt_u", "gt_u", "le_u", "ge_u")
		case "i32x4":
			ops = append(ops, "mul", "min_s", "min_u", "max_s", "max_u", "dot_i16x8_s", "lt_u", "gt_u", "le_u", "ge_u")
		}
		for _, op := range ops {
			bin = append(bin, ty+"."+op)
		}
	}
	for _, ty := range []string{"f32x4", "f64x2"} {
		for _, op := range []string{"add", "sub", "mul", "div", "pmin", "pmax", "eq", "ne", "lt", "gt", "le", "ge"} {
			bin = append(bin, ty+"."+op)
		}
	}
	for _, op := range bin {
		name := "v." + op
		fmt.Fprintf(b, "(func (export %q) (param v128 v128) (result v128) (%s (local.get 0) (local.get 1)))\n", name, op)
		for _, x := range vecs {
			for _, y := range vecs {
				calls = append(calls, regCall{name, []Val{v(x), v(y)}})
			}
		}
	}
	un := []string{"v128.not", "i8x16.abs", "i8x16.neg", "i16x8.abs", "i16x8.neg", "i32x4.abs", "i32x4.neg", "i64x2.neg"}
	for _, e := range []string{"i16x8.extend_%s_i8x16_%s", "i32x4.extend_%s_i16x8_%s", "i64x2.extend_%s_i32x4_%s"} {
		for _, half := range []string{"low", "high"} {
			for _, sg := range []string{"s", "u"} {
				un = append(un, fmt.Sprintf(e, half, sg))
			}
		}
	}
	for _, ty := range []string{"f32x4", "f64x2"} {
		for _, op := range []string{"abs", "neg", "sqrt", "ceil", "floor", "trunc", "nearest"} {
			un = append(un, ty+"."+op)
		}
	}
	for _, op := range un {
		name := "v." + op
		fmt.Fprintf(b, "(func (export %q) (param v128) (result v128) (%s (local.get 0)))\n", name, op)
		for _, x := range vecs {
			calls = append(calls, regCall{name, []Val{v(x)}})
		}
	}
	tests := []string{"v128.any_true"}
	for _, ty := range []string{"i8x16", "i16x8", "i32x4", "i64x2"} {
		tests = append(tests, ty+".all_true", ty+".bitmask")
	}
	for _, op := range tests {
		name := "v." + op
		fmt.Fprintf(b, "(func (export %q) (param v128) (result i32) (%s (local.get 0)))\n", name, op)
		for _, x := range append(vecs, [2]uint64{0x0101010101010101, 0x0101010101010100}, [2]uint64{0x0001000100010001, 0x0001000100000001}) {
			calls = append(calls, regCall{name, []Val{v(x)}})
		}
	}
	for _, op := range []string{"i16x8.shl", "i16x8.shr_s", "i16x8.shr_u", "i32x4.shl", "i32x4.shr_s", "i32x4.shr_u", "i64x2.shl", "i64x2.shr_u"} {
		name := "v." + op
		fmt.Fprintf(b, "(func (export %q) (param v128 i32) (result v128) (%s (local.get 0) (local.get 1)))\n", name, op)
		for _, x := range vecs {
			for _, n := range []uint64{0, 1, 7, 8, 15, 16, 31, 32, 33, 63, 64, 65, 0xffffffff} {
				calls = append(calls, regCall{name, []Val{v(x), {N: n}}})
			}
		}
	}
	b.WriteString("(func (export \"v.bitselect\") (param v128 v128 v128) (result v128) (v128.bitselect (local.get 0) (local.get 1) (local.get 2)))\n")
	for _, x := range vecs[:6] {
		for _, y := range vecs[6:] {
			for _, c := range vecs {
				calls = append(calls, regCall{"v.bitselect", []Val{v(x), v(y), v(c)}})
			}
		}
	}
	for i, lanes := range []string{
		"0 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15", "16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31",
		"31 30 29 28 27 26 25 24 23 22 21 20 19 18 17 16", "0 16 1 17 2 18 3 19 4 20 5 21 6 22 7 23",
		"4 5 6 7 8 9 10 11 12 13 14 15 0 1 2 3", "31 0 31 0 15 16 15 16 7 7 7 7 24 24 24 24",
	} {
		name := fmt.Sprintf("v.shuffle%d", i)
		fmt.Fprintf(b, "(func (export %q) (param v128 v128) (result v128) (i8x16.shuffle %s (local.get 0) (local.get 1)))\n", name, lanes)
		for _, x := range vecs[:4] {
			for _, y := range vecs[8:] {
				calls = append(calls, regCall{name, []Val{v(x), v(y)}})
			}
		}
	}
	for _, e := range []struct {
		op, res string
		lanes   []int
	}{
		{"i8x16.extract_lane_s", "i32", []int{0, 7, 8, 15}}, {"i8x16.extract_lane_u", "i32", []int{0, 7, 8, 15}},
		{"i16x8.extract_lane_s", "i32", []int{0, 3, 4, 7}}, {"i16x8.extract_lane_u", "i32", []int{0, 3, 4, 7}},
		{"i32x4.extract_lane", "i32", []int{0, 1, 2, 3}}, {"f32x4.extract_lane", "f32", []int{0, 3}},
		{"i64x2.extract_lane", "i64", []int{0, 1}}, {"f64x2.extract_lane", "f64", []int{0, 1}},
	} {
		for _, lane := range e.lanes {
			name := fmt.Sprintf("v.%s%d", e.op, lane)
			fmt.Fprintf(b, "(func (export %q) (param v128) (result %s) (%s %d (local.get 0)))\n", name, e.res, e.op, lane)
			for _, x := range vecs {
				calls = append(calls, regCall{name, []Val{v(x)}})
			}
		}
	}
	for _, r := range []struct {
		ty, arg string
		lanes   []int
	}{
		{"i8x16", "i32", []int{0, 9, 15}}, {"i16x8", "i32", []int{0, 5, 7}}, {"i32x4", "i32", []int{0, 3}},
		{"f32x4", "f32", []int{1, 2}}, {"i64x2", "i64", []int{0, 1}}, {"f64x2", "f64", []int{0, 1}},
	} {
		arg := vals["i32"]
		if r.arg == "i64" || r.arg == "f64" {
			arg = vals["i64"]
		}
		for _, lane := range r.lanes {
			name := fmt.Sprintf("v.%s.replace_lane%d", r.ty, lane)
			fmt.Fprintf(b, "(func (export %q) (param v128 %s) (result v128) (%s.replace_lane %d (local.get 0) (local.get 1)))\n", name, r.arg, r.ty, lane)
			for _, x := range vecs[:4] {
				for _, n := range arg {
					calls = append(calls, regCall{name, []Val{v(x), {N: n}}})
				}
			}
		}
		name := "v." + r.ty + ".splat"
		fmt.Fprintf(b, "(func (export %q) (param %s) (result v128) (%s.splat (local.get 0)))\n", name, r.arg, r.ty)
		for _, n := range arg {
			calls = append(calls, regCall{name, []Val{{N: n}}})
		}
	}
	// The operators arm64 has native handlers of and amd64 has not
	// ("v.x."), relaxed ones among them, which must give what the Go
	// handlers give on the machine running them.
	for _, op := range []string{"f32x4.min", "f32x4.max", "f64x2.min", "f64x2.max", "i64x2.mul", "i16x8.q15mulr_sat_s",
		"i16x8.extmul_low_i8x16_s", "i16x8.extmul_high_i8x16_s", "i16x8.extmul_low_i8x16_u", "i16x8.extmul_high_i8x16_u",
		"i32x4.extmul_low_i16x8_s", "i32x4.extmul_high_i16x8_s", "i32x4.extmul_low_i16x8_u", "i32x4.extmul_high_i16x8_u",
		"i64x2.extmul_low_i32x4_s", "i64x2.extmul_high_i32x4_s", "i64x2.extmul_low_i32x4_u", "i64x2.extmul_high_i32x4_u",
		"i16x8.relaxed_q15mulr_s", "f32x4.relaxed_min", "f32x4.relaxed_max", "f64x2.relaxed_min", "f64x2.relaxed_max",
		"i8x16.relaxed_swizzle", "i16x8.relaxed_dot_i8x16_i7x16_s"} {
		name := "v.x." + op
		fmt.Fprintf(b, "(func (export %q) (param v128 v128) (result v128) (%s (local.get 0) (local.get 1)))\n", name, op)
		for _, x := range vecs {
			for _, y := range vecs {
				calls = append(calls, regCall{name, []Val{v(x), v(y)}})
			}
		}
	}
	for _, op := range []string{"i8x16.popcnt", "i64x2.abs", "i16x8.extadd_pairwise_i8x16_s", "i16x8.extadd_pairwise_i8x16_u",
		"i32x4.extadd_pairwise_i16x8_s", "i32x4.extadd_pairwise_i16x8_u", "f32x4.demote_f64x2_zero", "f64x2.promote_low_f32x4",
		"f32x4.convert_i32x4_s", "f32x4.convert_i32x4_u", "f64x2.convert_low_i32x4_s", "f64x2.convert_low_i32x4_u",
		"i32x4.trunc_sat_f32x4_s", "i32x4.trunc_sat_f32x4_u", "i32x4.trunc_sat_f64x2_s_zero", "i32x4.trunc_sat_f64x2_u_zero",
		"i32x4.relaxed_trunc_f32x4_s", "i32x4.relaxed_trunc_f32x4_u", "i32x4.relaxed_trunc_f64x2_s_zero", "i32x4.relaxed_trunc_f64x2_u_zero"} {
		name := "v.x." + op
		fmt.Fprintf(b, "(func (export %q) (param v128) (result v128) (%s (local.get 0)))\n", name, op)
		for _, x := range vecs {
			calls = append(calls, regCall{name, []Val{v(x)}})
		}
	}
	for _, op := range []string{"i8x16.shl", "i8x16.shr_s", "i8x16.shr_u", "i64x2.shr_s"} {
		name := "v.x." + op
		fmt.Fprintf(b, "(func (export %q) (param v128 i32) (result v128) (%s (local.get 0) (local.get 1)))\n", name, op)
		for _, x := range vecs {
			for _, n := range []uint64{0, 1, 7, 8, 9, 63, 64, 65, 0xffffffff} {
				calls = append(calls, regCall{name, []Val{v(x), {N: n}}})
			}
		}
	}
	for _, op := range []string{"i8x16.relaxed_laneselect", "i16x8.relaxed_laneselect", "i32x4.relaxed_laneselect", "i64x2.relaxed_laneselect",
		"f32x4.relaxed_madd", "f32x4.relaxed_nmadd", "f64x2.relaxed_madd", "f64x2.relaxed_nmadd", "i32x4.relaxed_dot_i8x16_i7x16_add_s"} {
		name := "v.x." + op
		fmt.Fprintf(b, "(func (export %q) (param v128 v128 v128) (result v128) (%s (local.get 0) (local.get 1) (local.get 2)))\n", name, op)
		for _, x := range vecs {
			for _, y := range vecs[3:9] {
				for _, z := range vecs[4:8] {
					calls = append(calls, regCall{name, []Val{v(x), v(y), v(z)}})
				}
			}
		}
	}
	b.WriteString(`(func (export "v.mem.load") (param i32) (result v128) (v128.load offset=7 (local.get 0)))
(func (export "v.mem.store") (param i32 v128) (result v128) (v128.store offset=9 (local.get 0) (local.get 1)) (v128.load offset=9 (local.get 0)))
(func (export "v.mem.load8x8") (param i32) (result v128) (v128.load8x8_s (local.get 0)))
`)
	for _, a := range []uint64{0, 100, 65536 - 23, 65536 - 22, 0xffffffff} {
		calls = append(calls, regCall{"v.mem.load", []Val{{N: a}}}, regCall{"v.mem.load8x8", []Val{{N: a}}})
		calls = append(calls, regCall{"v.mem.store", []Val{{N: a}, v(vecs[3])}})
	}
	return calls
}

// nativeMayExit reports whether a call of export name may leave native
// code where it returns: a division (by 0 or -1), a loop's poll, and the
// operations in the second table -- i64 comparisons made booleans with an
// immediate.
func nativeMayExit(name string) bool {
	if strings.HasPrefix(name, "v.") {
		// Of the SIMD operators, the accesses out of bounds and the loads
		// that extend leave to Go, and on amd64 the operators only arm64
		// has native handlers of.
		return strings.HasPrefix(name, "v.mem.") || strings.HasPrefix(name, "v.x.") && runtime.GOARCH != "arm64"
	}
	if strings.Contains(name, "div") || strings.Contains(name, "rem") || name == "count" || strings.Contains(name, "trunc_") || strings.Contains(name, "convert_i64_u") {
		return true
	}
	return false
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
	native := instantiateReg(t, src)
	goTable := instantiateReg(t, src)
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
		withNative(native, true, func() { exits = countNativeExits(native, func() { got, gerr = run(native, c) }) })
		withNative(goTable, false, func() { want, werr = run(goTable, c) })
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
	inst := instantiateReg(t, regKernels)
	var exits int64
	withNative(inst, false, func() {
		exits = countNativeExits(inst, func() {
			if _, err := call(t, inst, "pop", 12); err != nil {
				t.Fatal(err)
			}
		})
	})
	if exits != 0 {
		t.Fatalf("%d native exits with the native handlers off", exits)
	}
	withNative(inst, true, func() {
		exits = countNativeExits(inst, func() {
			if _, err := call(t, inst, "pop", 12); err != nil {
				t.Fatal(err)
			}
		})
	})
	if exits == 0 {
		t.Fatal("no native exit for popcnt's escape with the native handlers on")
	}
}

// nativeHandles reports whether op has a native handler of its own: its
// table entry is not the exit stub's, which rInvalid's is.
func nativeHandles(op regOp) bool {
	t := (*[256]uintptr)(wasmNativeTablePtr())
	return t[op] != t[rInvalid]
}

// matchNative runs calls on the module of src natively, with the
// collector's marking flag read as set and as clear, and on the Go table,
// and fails where a result, an error or a trap's frames differ. It
// returns the barrier routine's stores natively with the flag set and
// clear.
func matchNative(t *testing.T, src string, calls []regCall) (marked, unmarked int64) {
	t.Helper()
	return matchNativeWith(t, src, calls, func(*Instance) {})
}

// matchNativeWith is matchNative, its native instances made so by set.
func matchNativeWith(t *testing.T, src string, calls []regCall, set func(*Instance)) (marked, unmarked int64) {
	t.Helper()
	insts := [3]*Instance{instantiateReg(t, src), instantiateReg(t, src), instantiateReg(t, src)}
	// The first reads the collector's flag as clear, the second as set;
	// the third runs on the Go table.
	clear, marking := uint8(0), uint8(1)
	testOf(insts[0]).mark, testOf(insts[1]).mark = &clear, &marking
	set(insts[0])
	set(insts[1])
	for _, c := range calls {
		var res [3][]Val
		var errs [3]error
		for k, inst := range insts {
			ext, ok := inst.Export(c.name)
			if !ok {
				t.Fatalf("no export %q", c.name)
			}
			before := nativeStoreRefs
			withNative(inst, k < 2, func() { res[k], errs[k] = Invoke(context.Background(), ext.Func, c.args) })
			switch k {
			case 0:
				unmarked += nativeStoreRefs - before
			case 1:
				marked += nativeStoreRefs - before
			}
		}
		for k := 0; k < 2; k++ {
			got, gerr := res[k], errs[k]
			want, werr := res[2], errs[2]
			if (gerr == nil) != (werr == nil) || gerr != nil && gerr.Error() != werr.Error() {
				t.Errorf("%s%v (marking %v): native %v, the Go table's %v", c.name, c.args, k == 1, gerr, werr)
				continue
			}
			var gt, wt *Trap
			if errors.As(gerr, &gt) && errors.As(werr, &wt) {
				if len(gt.Frames) != len(wt.Frames) {
					t.Errorf("%s%v: native trap of %d frames, the Go table's %d", c.name, c.args, len(gt.Frames), len(wt.Frames))
				} else {
					for i := range gt.Frames {
						if gt.Frames[i].Func != wt.Frames[i].Func || gt.Frames[i].Offset != wt.Frames[i].Offset {
							t.Errorf("%s%v: native frame %d %+v, the Go table's %+v", c.name, c.args, i, gt.Frames[i], wt.Frames[i])
							break
						}
					}
				}
			}
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("%s%v (marking %v) = %v, the Go table's %v", c.name, c.args, k == 1, got, want)
					break
				}
			}
		}
	}
	return marked, unmarked
}

// TestNativeCalls checks the calls native code makes itself, and those it
// leaves to Go, against the Go table: recursion, many arguments of each
// kind and several results, the stack's exhaustion, traps and exceptions
// in a callee, callees with references and try regions, host functions,
// the poll of a recursion.
func TestNativeCalls(t *testing.T) {
	nativeOnly(t)
	src := `(module
	  (tag $e (param i32))
	  (memory 1)
	  (func $fib (export "fib") (param i32) (result i64)
	    (if (result i64) (i32.lt_u (local.get 0) (i32.const 2))
	      (then (i64.extend_i32_u (local.get 0)))
	      (else (i64.add (call $fib (i32.sub (local.get 0) (i32.const 1))) (call $fib (i32.sub (local.get 0) (i32.const 2)))))))
	  (func $even (export "even") (param i32) (result i32)
	    (if (result i32) (i32.eqz (local.get 0)) (then (i32.const 1)) (else (call $odd (i32.sub (local.get 0) (i32.const 1))))))
	  (func $odd (param i32) (result i32)
	    (if (result i32) (i32.eqz (local.get 0)) (then (i32.const 0)) (else (call $even (i32.sub (local.get 0) (i32.const 1))))))
	  (func $many (param i32 i64 f32 f64 v128 i32) (result i64 f64 v128)
	    (i64.add (local.get 1) (i64.extend_i32_u (i32.add (local.get 0) (local.get 5))))
	    (f64.add (local.get 3) (f64.promote_f32 (local.get 2)))
	    (local.get 4))
	  (func (export "many") (param i32) (result i64)
	    (local i64 f64 v128)
	    (call $many (local.get 0) (i64.const 7) (f32.const 1.5) (f64.const 2.25) (v128.const i64x2 3 4) (i32.const 9))
	    (local.set 3) (local.set 2) (local.set 1)
	    (i64.add (i64.add (local.get 1) (i64.trunc_f64_s (local.get 2))) (i64x2.extract_lane 1 (local.get 3))))
	  (func $deep (export "deep") (param i32) (result i32)
	    (if (result i32) (i32.eqz (local.get 0)) (then (i32.const 0))
	      (else (i32.add (call $deep (i32.sub (local.get 0) (i32.const 1))) (i32.const 1)))))
	  (func $trap (param i32) (result i32) (i32.div_u (i32.const 1) (local.get 0)))
	  (func (export "trapdeep") (param i32) (result i32) (i32.add (call $trap (local.get 0)) (call $trap (i32.const 0))))
	  (func $throw (param i32) (result i32) (if (local.get 0) (then (throw $e (local.get 0)))) (i32.const 5))
	  (func (export "catch") (param i32) (result i32)
	    (block $h (result i32)
	      (try_table (catch $e $h) (drop (call $throw (local.get 0))))
	      (i32.const -1)))
	  (func $withref (param i32) (result i32) (local externref) (local.get 0))
	  (func $withtry (param i32) (result i32) (block (try_table (drop (call $throw (i32.const 0))))) (local.get 0))
	  (func $loads (param i32) (result i32) (i32.store (local.get 0) (i32.const 77)) (i32.load (local.get 0)))
	  (func (export "mixed") (param i32) (result i32)
	    (i32.add (i32.add (call $withref (local.get 0)) (call $withtry (local.get 0))) (call $loads (i32.const 64))))
	  (type $node (struct (field (ref null $node)) (field (ref null $node))))
	  (func $make (param i32) (result (ref $node))
	    (if (result (ref $node)) (i32.eqz (local.get 0))
	      (then (struct.new $node (ref.null $node) (ref.null $node)))
	      (else (struct.new $node (call $make (i32.sub (local.get 0) (i32.const 1)))
	                              (call $make (i32.sub (local.get 0) (i32.const 1)))))))
	  (func $check (param (ref null $node)) (result i32)
	    (if (result i32) (ref.is_null (struct.get $node 0 (local.get 0)))
	      (then (i32.const 1))
	      (else (i32.add (i32.const 1) (i32.add (call $check (struct.get $node 0 (local.get 0)))
	                                            (call $check (struct.get $node 1 (local.get 0))))))))
	  (func (export "tree") (param i32) (result i32) (call $check (call $make (local.get 0))))
	  (func $pass (param externref i32 externref) (result externref i32 externref)
	    (local externref) (local.set 3 (local.get 0))
	    (local.get 2) (i32.add (local.get 1) (i32.const 1)) (local.get 3))
	  (func (export "swap") (param externref externref) (result externref i32 externref)
	    (call $pass (local.get 0) (i32.const 6) (local.get 1)))
	  (func $lane (param v128) (result i32) (i32x4.extract_lane 2 (local.get 0)))
	  (type $ii (func (param i32) (result i32)))
	  (type $none (func))
	  (table $t 5 funcref)
	  (elem (table $t) (i32.const 0) func $sq $inc $nop)
	  (func $sq (type $ii) (i32.mul (local.get 0) (local.get 0)))
	  (func $inc (type $ii) (i32.add (local.get 0) (i32.const 1)))
	  (func $nop (type $none))
	  (func (export "ind") (param i32 i32) (result i32) (call_indirect $t (type $ii) (local.get 0) (local.get 1)))
	  (func (export "indloop") (param i32) (result i32) (local i32 i32)
	    (loop $l
	      (local.set 1 (call_indirect $t (type $ii) (local.get 1) (i32.and (local.get 2) (i32.const 1))))
	      (local.set 2 (i32.add (local.get 2) (i32.const 1)))
	      (br_if $l (i32.lt_u (local.get 2) (local.get 0))))
	    (local.get 1))
	  (func (export "lane") (param i32) (result i32) (call $lane (i32x4.splat (local.get 0))))
	  (func (export "refs") (param externref externref i32) (result externref)
	    (local externref)
	    (local.set 3 (local.get 0))
	    (if (local.get 2) (then (local.set 3 (ref.null extern))))
	    (select (result externref) (local.get 3) (local.get 1) (local.get 2))))`
	host := HostRef("h")
	calls := []regCall{
		{"fib", []Val{{N: 0}}}, {"fib", []Val{{N: 1}}}, {"fib", []Val{{N: 15}}},
		{"even", []Val{{N: 10001}}}, {"even", []Val{{N: 10000}}},
		{"many", []Val{{N: 1}}}, {"many", []Val{{N: 0xffffffff}}},
		{"deep", []Val{{N: 1000}}}, {"deep", []Val{{N: uint64(MaxCallDepth) + 10}}},
		{"trapdeep", []Val{{N: 3}}},
		{"catch", []Val{{N: 0}}}, {"catch", []Val{{N: 4}}},
		{"mixed", []Val{{N: 3}}},
		{"refs", []Val{{R: host}, {R: Null}, {N: 0}}}, {"refs", []Val{{R: host}, {R: Null}, {N: 1}}},
		{"tree", []Val{{N: 0}}}, {"tree", []Val{{N: 10}}},
		{"lane", []Val{{N: 7}}}, {"lane", []Val{{N: 9}}}, {"lane", []Val{{N: 11}}},
		{"ind", []Val{{N: 5}, {N: 0}}}, {"ind", []Val{{N: 5}, {N: 1}}}, {"ind", []Val{{N: 5}, {N: 0}}},
		{"ind", []Val{{N: 5}, {N: 2}}}, {"ind", []Val{{N: 5}, {N: 3}}}, {"ind", []Val{{N: 5}, {N: 5}}},
		{"ind", []Val{{N: 5}, {N: 0xffffffff}}}, {"indloop", []Val{{N: 1000}}},
		{"swap", []Val{{R: host}, {R: HostRef("k")}}}, {"swap", []Val{{R: Null}, {R: host}}},
	}
	marked, unmarked := matchNativeWithHost(t, src, calls)
	t.Logf("the barrier routine's stores: %d marking, %d not", marked, unmarked)
	if marked == 0 || unmarked != 0 {
		t.Fatalf("the barrier routine stored %d times marking and %d not, want some and none", marked, unmarked)
	}
	// Callees with reference slots are called natively: once a first run
	// has compiled them and made the frames, no call leaves to Go.
	inst := instantiateReg(t, src)
	withNative(inst, true, func() {
		if _, err := call(t, inst, "tree", 8); err != nil {
			t.Fatal(err)
		}
	})
	// An exec fresh from the pool makes its frames, call records and
	// chunks as Go's calls first need them; those exits are not counted.
	var exits []nativeExit
	withNative(inst, true, func() {
		exits = logNativeExits(inst, func() {
			if got, err := call(t, inst, "tree", 8); err != nil || got[0].N != 511 {
				t.Errorf("tree(8) = %v, %v", got, err)
			}
		})
	})
	if n, all := exitsBut(callExits(exits), exitRoom); n != 0 {
		t.Errorf("%d of tree(8)'s calls left to Go:%s", n, all)
	}
	// So are call_indirect's callees whose entry and type are right, once
	// compiled.
	withNative(inst, true, func() {
		if _, err := call(t, inst, "indloop", 10); err != nil {
			t.Fatal(err)
		}
	})
	withNative(inst, true, func() {
		exits = logNativeExits(inst, func() {
			if _, err := call(t, inst, "indloop", 100); err != nil {
				t.Error(err)
			}
		})
	})
	if n, all := exitsBut(callExits(exits), exitRoom); n != 0 {
		t.Errorf("%d of indloop(100)'s indirect calls left to Go:%s", n, all)
	}
}

// matchNativeWithHost is matchNative with the spectest's print_i32 to
// import.
func matchNativeWithHost(t *testing.T, src string, calls []regCall) (int64, int64) {
	return matchNative(t, src, calls)
}

// TestNativeRefsMarking checks the reference moves natively with the
// collector's marking flag read as set and clear: the same results, the
// pointer words stored through the barrier routine only while it is set.
func TestNativeRefsMarking(t *testing.T) {
	nativeOnly(t)
	if !nativeHandles(rRefMov) || !nativeHandles(rRefNull) {
		t.Fatal("ref.mov or ref.null has no native handler")
	}
	for _, op := range []regOp{rFar, rReturnCall} {
		if nativeHandles(op) {
			t.Errorf("operation %d has a native handler", op)
		}
	}
	host, other := HostRef("a"), HostRef("b")
	marked, unmarked := matchNative(t, `(module
	  (func (export "f") (param externref externref i32) (result externref) (local externref externref)
	    (local.set 3 (local.get 0))
	    (local.set 4 (local.get 1))
	    (if (local.get 2) (then (local.set 3 (ref.null extern)) (local.set 4 (local.get 3))))
	    (local.get 4)))`, []regCall{
		{"f", []Val{{R: host}, {R: other}, {N: 0}}},
		{"f", []Val{{R: host}, {R: other}, {N: 1}}},
		{"f", []Val{{R: Null}, {R: other}, {N: 0}}},
	})
	if marked == 0 || unmarked != 0 {
		t.Fatalf("the barrier routine stored %d times marking and %d not, want some and none", marked, unmarked)
	}
}

// TestNativeSIMDWithout runs SIMD operators natively with rSIMD's table
// the exit stub's alone, as on a CPU without SSE4.2: every operator leaves
// to Go, with the Go table's results.
func TestNativeSIMDWithout(t *testing.T) {
	nativeOnly(t)
	src := `(module
	  (func (export "f") (param v128 v128 i32) (result v128)
	    (i32x4.shl (i8x16.shuffle 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19
	      (i32x4.mul (local.get 0) (local.get 1)) (f64x2.sqrt (local.get 1))) (local.get 2))))`
	calls := []regCall{
		{"f", []Val{{N: 0x0123456789abcdef, Hi: 0xfedcba9876543210}, {N: 0x7ff8000000000001, Hi: 0x4010000000000000}, {N: 3}}},
		{"f", []Val{{N: ^uint64(0), Hi: 1}, {N: 1 << 63, Hi: 0xfff4000000000000}, {N: 33}}},
	}
	var counts [256]int64
	matchNativeWith(t, src, calls, func(inst *Instance) {
		testOf(inst).simdTable = uintptr(unsafe.Pointer(&nativeSIMDExits))
		testOf(inst).exits = &counts
	})
	exits := int64(0)
	for _, n := range counts {
		exits += n
	}
	if exits == 0 {
		t.Fatal("no SIMD operator left to Go with rSIMD's table the exit stub's")
	}
}
