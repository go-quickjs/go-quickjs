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
// Leaf), which the code checked the call calls, as the call's form says
// (jitLeafForm), which it makes at the call's first. It writes the result
// to the call's keep cell, as any call of Go does, and returns back, where
// the call goes on, and the result's number word; where the leaf does not
// make the call, it returns where the call's refusal goes on
// (GoCallState's Fail), Go leaving the call to an exit, ctx as jit.goCall
// would have left it: the call's operation and keep cell, its status.
// Nothing reads those of a call made: they are written only for one that
// leaves.
func jitLeafGo(ctx *abi.Context, st *abi.GoCallState, back uintptr) (uintptr, uint64) {
	if jitGoCallWork != nil {
		jitGoCallWork()
	}
	r := (*Runtime)(ctx.Host)
	form := (*jitLeafForm)(st.LeafForm)
	if form == nil && r != nil {
		form = r.jitLeafFormOf(st)
	}
	if form != nil {
		this := Undefined
		if form.method {
			this = jitGoArg(ctx, 1)
		}
		args := unsafe.Slice((*Value)(unsafe.Pointer(&ctx.GoArgs[form.first])), form.argc)
		var v Value
		ok := false
		if form.check != nil {
			if r.meter == nil && form.check(r, this, args) {
				var err error
				v, err = form.native(r, this, args)
				ok = err == nil
			}
		} else {
			v, ok = form.apply(r, this, args)
		}
		if ok {
			*(*Value)(unsafe.Pointer(&ctx.Keep[form.keep])) = v
			ctx.GoStatus = 0
			r.jit.leafFast++
			return back, math.Float64bits(v.num)
		}
		r.jit.leafRefused++
	}
	ctx.GoOp, ctx.GoKeep, ctx.GoLast, ctx.GoStatus = st.Op, st.Keep, uintptr(unsafe.Pointer(st)), 1
	return back + uintptr(int64(st.Fail)), 0
}

// jitLeafForm is a leaf's call from native code as the VM makes it: its
// operands' places in the context's GoArgs -- the receiver, a method
// call's, at 1, the argc arguments from first -- its keep cell, and the
// leaf: a native leaf's check and function (leafChecks, funcData's
// native), which a memory meter refuses, or the function that applies a
// hand-made one or a collection's operation (leafOps, collectionOp). Each
// call's description holds its own (abi.GoCallState's LeafForm), made at
// its first call (jitLeafFormOf): what jitLeafOf finds out at every call
// -- the operands' count and kind, the function's data, its kind of leaf
// -- had cost more than the routine that calls it.
type jitLeafForm struct {
	check  func(r *Runtime, this Value, args []Value) bool
	native NativeFunc
	apply  func(r *Runtime, this Value, args []Value) (Value, bool)
	first  int
	argc   int
	keep   uint64
	method bool
}

// jitLeafFormOf makes the form of the leaf's call st describes, keeping it
// in st for the calls after, or nil for a call it cannot make: no leaf, a
// function that is none, a keep cell past the context's.
func (r *Runtime) jitLeafFormOf(st *abi.GoCallState) *jitLeafForm {
	op := st.Op
	argc := int(op>>abi.GoOpArgcShift) & 0xff
	method := op>>abi.GoOpKindShift&0xff == abi.GoCallMethod
	first := 1
	if method {
		first = 2
	}
	if st.Leaf == 0 || st.Keep >= abi.MaxKeeps || first+argc > abi.MaxGoArgs {
		return nil
	}
	fd := (*Object)(*(*unsafe.Pointer)(unsafe.Pointer(&st.Leaf))).fn()
	if fd == nil || fd.elemOp == 0 {
		return nil
	}
	form := &jitLeafForm{first: first, argc: argc, keep: st.Keep, method: method}
	switch op := fd.elemOp; {
	case op >= elemNativeFirst:
		form.check, form.native = leafChecks[op], fd.native
	case op > elemRandom:
		f := leafOps[op]
		form.apply = func(r *Runtime, this Value, args []Value) (Value, bool) { return f(r, this, args, true) }
	default:
		form.apply = func(r *Runtime, this Value, args []Value) (Value, bool) { return r.collectionOp(op, this, args, true) }
	}
	st.LeafForm = unsafe.Pointer(form)
	return form
}

// jitLeafOf is the result of the leaf call op (abi.Context's GoOp, of a
// GoLeaf) of fn, of the receiver and the arguments in ctx's GoArgs past
// the function's, read in place, as every tier applies the leaf without a
// frame (jitLeafApply), the memory meter allowing it: false where it does
// not make it. jitLeafCall, callGo's path, makes it so.
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
