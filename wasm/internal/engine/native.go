package engine

import (
	"os"
	"unsafe"
)

//go:generate go run ./internal/nativegen native_handlers_amd64.s

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
// which TestNativeLayout checks.
type nativeCtx struct {
	l         unsafe.Pointer
	code      unsafe.Pointer
	table     uintptr
	pc        uintptr
	f         *frame
	goTable   uintptr
	slowTable uintptr
	callH     uintptr
	after     uintptr
	mem0      unsafe.Pointer
	globals   unsafe.Pointer
	tables    unsafe.Pointer
	exits     unsafe.Pointer
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
	c.l = unsafe.Pointer(&f.l[0])
	c.code = unsafe.Pointer(&rc.code[0])
	c.pc = uintptr(pc)
	c.f = f
	c.mem0 = nil
	if m := f.inst.mem0; m != nil && !m.Shared {
		c.mem0 = unsafe.Pointer(m)
	}
	c.globals = nil
	if len(f.inst.Globals) > 0 {
		c.globals = unsafe.Pointer(&f.inst.Globals[0])
	}
	c.tables = nil
	if len(rc.tables) > 0 {
		c.tables = unsafe.Pointer(&rc.tables[0])
	}
	c.exits = unsafe.Pointer(nativeExitCounts)
	return c
}
