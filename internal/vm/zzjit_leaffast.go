//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
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

// jitGoFast is where native code jumps for any other call of Go whose site
// saves its live registers (abi.Encoding's GoFast): as jitLeafFast, it
// calls jitGoDirect, and goes straight back where it says.
func jitGoFast()

// jitGoFastAddr is jitGoFast's address.
func jitGoFastAddr() uintptr

// jitGoDirect makes the call of Go st describes, as jit.goCall would --
// the description read into ctx (jit.ExpandGoCall), the call made by
// jitHostCall, or a +'s by jitGoAppend or jitGoAdd, its status -- and
// returns back, where the call goes on, or where its refusal does
// (GoCallState's Fail): what jit's callGo, its call of jit.HostCall's
// function value and the code's routine of calls of Go, saved and resumed
// through the context, had cost every call of Go besides.
func jitGoDirect(ctx *abi.Context, st *abi.GoCallState, back uintptr) uintptr {
	ctx.GoState = uintptr(unsafe.Pointer(st))
	fail := jit.ExpandGoCall(ctx)
	ctx.GoStatus = 1
	r := (*Runtime)(ctx.Host)
	if r != nil && r.jit != nil {
		r.jit.goFast++
	}
	ok := false
	switch op := ssa.GoOp(ctx.GoOp & 0xff); {
	case r == nil || ctx.GoKeep >= abi.MaxKeeps || jitGoCallWork != nil:
		ok = jitHostCall(ctx)
	case op == ssa.GoAppend:
		// A loop's string builder's s += x, the most frequent of them in
		// a loop, and below any other +, made here rather than through
		// jitHostCall's dispatch of every kind of call.
		ok = r.jitGoAppend(ctx)
	case op == ssa.GoAdd:
		ok = r.jitGoAdd(ctx)
	default:
		ok = jitHostCall(ctx)
	}
	if ok {
		ctx.GoStatus = 0
		return back
	}
	return back + uintptr(int64(fail))
}

// jitGoAppend makes a loop's string builder's s += x (ssa.GoAppend), of
// the operands in ctx's GoArgs, its result to the call's keep cell, and
// reports whether it did: what GoAdd takes, a string grown in place or
// made a builder of (jitAppend). jitGoDirect calls it first, and
// jitHostCall for a call made otherwise.
func (r *Runtime) jitGoAppend(ctx *abi.Context) bool {
	// A loop's string builder's s += x (jitAppend): what GoAdd takes,
	// a string's grown in place or made a builder of.
	a, b := jitGoArg(ctx, 0), jitGoArg(ctx, 1)
	if a.IsBigInt() && b.IsBigInt() {
		// A loop's sum of BigInts, which it takes for a string's.
		return r.jitGoBigAdd(ctx, a, b)
	}
	if a.IsBigInt() || b.IsBigInt() {
		if !jitAddOperand(a, b) || !jitAddOperand(b, a) {
			return false
		}
	} else if !jitPlainOperand(a) || !jitPlainOperand(b) {
		return false
	}
	if r.meter != nil {
		return false
	}
	var v Value
	if a.IsString() {
		x := b
		if !b.IsString() {
			s, err := r.toString(b)
			if err != nil {
				return false
			}
			x = Str(s)
		}
		s, ok := r.jitAppend(a.String(), x.String())
		if !ok {
			return false
		}
		v = Str(s)
	} else {
		var err error
		if v, err = r.add(a, b); err != nil {
			return false
		}
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitGoAdd makes a + of Go's (ssa.GoAdd) that is no string builder's
// append -- a string's concatenation, s = x + s among them, a number's
// conversion to a string by + "", a sum of BigInts -- of the operands in
// ctx's GoArgs, its result to the call's keep cell, and reports whether it
// did. jitGoDirect calls it first, and jitHostCall for a call made
// otherwise.
func (r *Runtime) jitGoAdd(ctx *abi.Context) bool {
	a, b := jitGoArg(ctx, 0), jitGoArg(ctx, 1)
	if r.meter == nil {
		if v, ok := r.jitConcatAdd(a, b); ok {
			*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
			return true
		}
	}
	// Primitives, and nothing that throws: an object's conversion may
	// call anything, and making an error may too (a getter of
	// Error.stackTraceLimit) -- a symbol's, a BigInt's with a number,
	// a string too long, a memory limit reached. Go makes those after
	// the code leaves.
	if a.IsBigInt() || b.IsBigInt() {
		if a.IsBigInt() && b.IsBigInt() {
			return r.jitGoBigAdd(ctx, a, b)
		}
		if !jitAddOperand(a, b) || !jitAddOperand(b, a) {
			return false
		}
	} else if !jitPlainOperand(a) || !jitPlainOperand(b) {
		return false
	}
	if r.meter != nil || a.IsString() && b.IsString() && a.String().length+b.String().length > maxStringLength-64 {
		return false
	}
	v, err := r.add(a, b)
	if err != nil {
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

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
