//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// A leaf's call from native code (ssa.GoLeaf) -- a built-in that runs no
// script and throws nothing where its check passes, the function the code
// has checked it calls -- is made by jitLeafGo, as V8's optimized code
// calls a fast API function, with no frame of JavaScript's and with
// nothing of the call to find out: native code jumps to jitLeafFast
// (zzjit_leaffast_*.s, abi.Encoding's LeafFast), which calls it with the
// call's description and where the call goes on, and goes straight back
// there with the result's number word in a register. A call of Go had
// gone through jit's callGo, goCall, ExpandGoCall, jit.HostCall and
// jitHostCall's dispatch before it reached the leaf, and back through the
// code's routine for calls of Go: more than the leaf's own work, for
// String's methods, Number's predicates, a Map's get.
//
// jitLeafFast is, as callGo is, to the Go runtime a function the Go
// function that entered the code called -- native code touches neither
// the stack nor the link register -- so that jitLeafGo is ordinary Go,
// which may grow the stack, be preempted and allocate; native code has
// saved what it keeps, as for any call of Go.

// jitLeafFast is where native code jumps for a leaf's call (abi.Encoding's
// LeafFast).
func jitLeafFast()

// jitLeafFastAddr is jitLeafFast's address.
func jitLeafFastAddr() uintptr

// jitLeafGo makes the leaf's call st describes, of the operands native code
// wrote to ctx's GoArgs past the function's -- the receiver of a method
// call, then the arguments -- its function the one st names (GoCallState's
// Leaf), which the code checked the call calls. It writes the result to
// the call's keep cell, as any call of Go does, and returns back, where
// the call goes on, and the result's number word; where the leaf does not
// make the call, it returns where the call's refusal goes on (GoCallState's
// Fail), Go leaving the call to an exit. It leaves ctx as jit.goCall would
// have left it: the call's operation and keep cell, its status.
func jitLeafGo(ctx *abi.Context, st *abi.GoCallState, back uintptr) (uintptr, uint64) {
	if jitGoCallWork != nil {
		jitGoCallWork()
	}
	ctx.GoOp, ctx.GoKeep, ctx.GoLast = st.Op, st.Keep, uintptr(unsafe.Pointer(st))
	r := (*Runtime)(ctx.Host)
	if r == nil {
		ctx.GoStatus = 1
		return back + uintptr(int64(st.Fail)), 0
	}
	if st.Keep < abi.MaxKeeps && st.Leaf != 0 {
		if v, ok := r.jitLeafOf(ctx, st.Op, (*Object)(*(*unsafe.Pointer)(unsafe.Pointer(&st.Leaf)))); ok {
			*(*Value)(unsafe.Pointer(&ctx.Keep[st.Keep])) = v
			ctx.GoStatus = 0
			r.jit.leafFast++
			return back, math.Float64bits(v.num)
		}
	}
	ctx.GoStatus = 1
	r.jit.leafRefused++
	return back + uintptr(int64(st.Fail)), 0
}

// jitLeafOf is the result of the leaf call op (abi.Context's GoOp, of a
// GoLeaf) of fn, of the receiver and the arguments in ctx's GoArgs past
// the function's, read in place, as every tier applies the leaf without a
// frame (jitLeafApply), the memory meter allowing it: false where it does
// not make it.
func (r *Runtime) jitLeafOf(ctx *abi.Context, op uint64, fn *Object) (Value, bool) {
	argc := int(op>>abi.GoOpArgcShift) & 0xff
	first, this := 1, Undefined
	if op>>abi.GoOpKindShift&0xff == abi.GoCallMethod {
		first, this = 2, jitGoArg(ctx, 1)
	}
	fd := fn.fn()
	if first+argc > abi.MaxGoArgs || fd == nil || fd.elemOp == 0 || fd.elemOp >= elemNativeFirst && r.meter != nil {
		return Undefined, false
	}
	return r.jitLeafApply(fd, this, unsafe.Slice((*Value)(unsafe.Pointer(&ctx.GoArgs[first])), argc))
}
