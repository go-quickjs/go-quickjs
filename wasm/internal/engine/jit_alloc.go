//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"os"
	"unsafe"

	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The allocation stub. Where a pool of new objects is empty, or an array is
// past what the pools hold, native code calls Go without leaving: it stores
// what an ExitAlloc stores -- the exit's record, its frame's L, the
// registers it keeps -- and jumps to wasmJITAlloc, which calls jitAllocStub
// from a frame of its own, as a function the Go function that entered the
// code called (internal/jit's storeRef is one so). Go fills the pool, or
// makes the array into its reference slot, and native code goes on as from
// the exit's resumption. What Go cannot do without leaving native code --
// a trap, a charge the budget holds only after counting what is reachable,
// which reads the frames as an exit makes them -- it leaves to the exit:
// native code takes the exit whose record it stored.

// wasmJITAlloc is where native code jumps to make a GC object for its
// ExitAlloc record (wjit.Request.AllocStub): its context in DI (X0), where
// to go on in R11 (X16). It leaves 1 in AX (X17) where Go made the object,
// else 0, and keeps the context (and, on arm64, the link register):
// nothing else.
func wasmJITAlloc()

// wasmJITAllocClobber is wasmJITAlloc with junk left in every register but
// those it keeps and gives, for tests (WASM_JIT_BARRIER=clobber).
func wasmJITAllocClobber()

func wasmJITAllocAddr() uintptr
func wasmJITAllocClobberAddr() uintptr

// jitAllocStubAddr is where native code is compiled to jump to allocate.
var jitAllocStubAddr = func() uint64 {
	if os.Getenv("WASM_JIT_BARRIER") == "clobber" {
		return uint64(wasmJITAllocClobberAddr())
	}
	return uint64(wasmJITAllocAddr())
}()

// jitAllocStub makes the object of the ExitAlloc record ctx's level holds,
// or runs the instruction of its ExitOp record, one that makes an object
// (wjit.StubOp), reporting whether it did: else the code takes the exit.
func jitAllocStub(ctx *wjit.Ctx) uintptr {
	lv := (*jitLevel)(unsafe.Pointer(ctx))
	if t := lv.cj.test; t != nil {
		if c := t.counts; c != nil && ctx.Exit == wjit.ExitOp {
			c.opStubs.Add(1)
		} else if c != nil {
			c.stubs.Add(1)
		}
		if t.jit.allocHook != nil {
			t.jit.allocHook()
		}
	}
	if ctx.Exit == wjit.ExitOp {
		// Where its references are, as the exit reads them (jitExit), and
		// cleared where the code goes on: an exit after it sets its own.
		lv.rargs = ctx.RArgs
		f := lv.x.exitFrame(lv)
		if !lv.x.jitNew(lv.inst, opcode.Opcode(ctx.Arg0), uint32(ctx.Arg1), uint32(ctx.Arg2), &f, int(ctx.Arg3), uint32(ctx.Arg4), true) {
			return 0
		}
		ctx.RArgs = 0
		return 1
	}
	f := lv.x.exitFrame(lv)
	if lv.x.allocFor(lv, ctx.Arg0, ctx.Arg2, &f, uint32(ctx.Arg1), true) {
		return 1
	}
	return 0
}

// The level's first field is its context, which native code holds.
var _ [unsafe.Offsetof(jitLevel{}.ctx)]struct{}
