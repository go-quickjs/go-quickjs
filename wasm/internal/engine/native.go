package engine

import (
	"os"
	"unsafe"
)

// The native handlers (README.md, "Native handlers"): machine code for the
// hot operations of register code, made offline by internal/nativegen and
// assembled into the binary (native_handlers_<arch>.s), one block for every
// build, never patched, every address it needs given by the context. What
// the code does not do itself goes to the exit stub, which calls the
// instruction's Go handler -- or its slow function -- in place, through a
// trampoline of the architecture's own (native_<arch>.s), and goes on at
// the pc it answers.

// nativeCtx is what native code reads beyond its pinned registers, a
// frame's own (frame.nctx): internal/regdef gives each field's offset,
// which TestNativeLayout checks. Its fields are plain words, which native
// code writes as it calls a function without a write barrier: what each
// addresses is kept by the exec, the frame, the instance or the Code
// besides, and no Go object moves.
type nativeCtx struct {
	l, code, table, pc, f            uintptr
	goTable, slowTable, callH, after uintptr
	mem0, globals, tables, exits     uintptr
	// r is the frame's reference slots; x the exec; inst the instance;
	// funcs, fns and sites the first of the instance's functions, the
	// Code's records and the code's sites; calls says native code may
	// call a function itself (nativeCall); maxDepth is MaxCallDepth;
	// storeRef the barrier routine and wb the collector's marking flag's
	// address (nativeStoreRef).
	r, x, inst, funcs, fns, sites uintptr
	calls, maxDepth, storeRef, wb uintptr
	// caller is the context of the frame that called this one natively,
	// or 0; retAddr its call's address, site its site, nc and nsp the
	// exec's chunk and slot to give back as it returns; fn, cd and rc are
	// the call's scratch.
	caller, retAddr, site, nc, nsp uintptr
	fn, cd, rc                     uintptr
	// extTable is the native second table, rExt's.
	extTable uintptr
	// rch and rsp are the exec's reference chunk and slot to give back
	// as a frame native code called returns.
	rch, rsp uintptr
}

// regNativeOn runs register code by the native handlers where they are
// built. WASM_NONATIVE set in the environment, read once, leaves it to the
// Go table's loop, to compare the two and as an escape hatch.
var regNativeOn = nativeBuilt && os.Getenv("WASM_NONATIVE") == ""

// SetRegNative runs register code by the native handlers or not, where they
// are built, and reports whether it did: a test's switch, as WASM_NONATIVE
// is the environment's.
func SetRegNative(on bool) bool {
	was := regNativeOn
	regNativeOn = on && nativeBuilt
	return was
}

// nativeExitCounts, set by a test, counts native code's exits to Go by
// operation.
var nativeExitCounts *[256]int64

// regRun runs register code rc in frame f from pc until the frame returns:
// natively where the handlers are on, else by the Go table's loop.
func regRun(f *frame, rc *regCode, pc int32) {
	if regNativeOn {
		nativeRun(f, rc, pc)
		return
	}
	regLoop(unsafe.Pointer(&f.l[0]), unsafe.Pointer(&rc.code[0]), pc, f)
}

// nativeSet makes frame f's context for running rc from pc.
func nativeSet(f *frame, rc *regCode, pc int32) *nativeCtx {
	c := &f.nctx
	inst := f.inst
	c.l = uintptr(unsafe.Pointer(&f.l[0]))
	c.code = uintptr(unsafe.Pointer(&rc.code[0]))
	c.pc = uintptr(pc)
	c.f = uintptr(unsafe.Pointer(f))
	c.mem0 = 0
	if m := inst.mem0; m != nil && !m.Shared {
		c.mem0 = uintptr(unsafe.Pointer(m))
	}
	c.globals = sliceAddr(inst.Globals)
	c.tables = sliceAddr(rc.tables)
	c.exits = uintptr(unsafe.Pointer(nativeExitCounts))
	c.r = sliceAddr(f.r)
	c.x = uintptr(unsafe.Pointer(f.x))
	c.inst = uintptr(unsafe.Pointer(inst))
	c.funcs = sliceAddr(inst.Funcs)
	c.fns = sliceAddr(inst.Code.fns)
	c.sites = sliceAddr(rc.sites)
	c.calls = 0
	if nativeCall(inst.Code) {
		c.calls = 1
	}
	c.maxDepth = uintptr(MaxCallDepth)
	c.caller = 0
	return c
}

// sliceAddr is the address of s's first element, or 0.
func sliceAddr[T any](s []T) uintptr {
	if len(s) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&s[0]))
}

// nativeMarkFlag, set by a test, is the flag the contexts made after it
// read for the collector's, so that native code stores as it does while
// the collector marks.
var nativeMarkFlag *uint8

// nativeStoreRefGo is the barrier routine's store (nativeStoreRef): a
// pointer stored with Go's write barrier.
func nativeStoreRefGo(p *unsafe.Pointer, v unsafe.Pointer) {
	nativeStoreRefs++
	*p = v
}

// nativeStoreRefs counts the barrier routine's stores, for the tests.
var nativeStoreRefs int64

// nativeCall reports whether native code may call code's functions itself:
// not where the JIT runs code, whose calls Go's run makes, entering native
// code where a callee has it.
func nativeCall(code *Code) bool { return code.jit.Load() == nil }
