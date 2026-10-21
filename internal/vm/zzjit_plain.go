//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// jitGoCallPlain makes a call of Go native code makes (ssa.GoCall), at its
// level's code's exit PC, where making it runs no script and cannot throw,
// without the frames, the pins and the operands on the VM's stack that
// jitGoCall and jitGoCallNested give a call that may -- as the tree
// tier makes these without a frame (callRest's pureElemOp, setProp's fast
// paths), and as V8's code calls a builtin that cannot call back without a
// frame of JavaScript's: a leaf's call (jitPlainCall) -- a Map's or a
// Set's get, has, set, add or delete, a WeakMap's or a WeakSet's
// operations, Date.now, String(x) of a primitive; a write a site's cache
// answers with a data property or an addition along its
// transition, or of an array's length (jitPlainWrite). Each had cost a
// call of Go its frame, its operands copied and cleared, its code pinned
// and its learning asked, more than what the existing tiers spend on the
// whole operation: microbench's map_set_int, map_set_string and
// array_length_decr ran slower with the JIT than without. It writes the
// result to the call's keep cell and reports whether it made the call;
// what it does not make it leaves to them, untouched.
//
// It counts no call of Go (ssaStats' goCalls): what such a call costs is
// a native operation's, which the code's profit (jitSSAProfit) counts as
// its work. None of these calls is one the JIT learns a native form of at
// its call of Go (jitCallSeen): push and pop, Math.random and the searches
// are not taken here.
func (r *Runtime) jitGoCallPlain(ctx *abi.Context) bool {
	// Asked first, of the operation's kind and its function's identity
	// alone: most calls of Go are of none of these, which jitGoCall makes.
	switch ctx.GoOp >> abi.GoOpKindShift & 0xff {
	case abi.GoCallSet:
	case 0, abi.GoCallMethod:
		at := 0
		if ctx.GoOp>>abi.GoOpKindShift&0xff == abi.GoCallMethod {
			at = 1
		}
		if !r.jitPlainFn(ctx.GoArgs[at].Ref) {
			return false
		}
	default:
		return false
	}
	in, n := jitGoCallInstr(ctx.GoOp)
	if n > abi.MaxGoArgs {
		return false
	}
	var v Value
	switch in.Op {
	case bytecode.OpSetProp:
		// The instruction's names and caches are its code's closure's: the
		// run's frame's, or a native callee's own.
		s, idx := r.jit, int(ctx.Level)
		if idx >= len(s.ssaCtxs) || s.ssaCtxs[idx] != ctx {
			return false
		}
		var cl *closure
		switch ctx.Live {
		case 0:
			if idx >= len(s.goRuns) || s.goRuns[idx].f == nil {
				return false
			}
			cl = s.goRuns[idx].f.cl
		case abi.LiveCall:
			cl = (*closure)(ctx.Closure)
		}
		pc := int(ctx.ExitPC)
		if cl == nil || !jitGoPropertyAt(cl.fn, pc, in) || !r.jitPlainWrite(cl, cl.fn.Code[pc], jitGoArg(ctx, 0), jitGoArg(ctx, 1)) {
			return false
		}
		v = Undefined
	case bytecode.OpCall, bytecode.OpCallMethod:
		var ok bool
		if v, ok = r.jitPlainCall(ctx, in, n); !ok {
			return false
		}
		r.jitLeafSeen(ctx, in)
	default:
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitPlainCall is the result of the call of a built-in among its operands
// (abi.GoCallMethod's receiver, the function, the arguments; a plain
// call's the function and the arguments) where it runs nothing and throws
// nothing: a leaf's (jitLeafOp), applied as every tier applies it without
// a frame (pureElemOp) -- a collection's lookup, addition or deletion on
// its own kind of collection, a weak one's, Date.now, String(x) of a
// primitive. The function is one of jitPlainSet's (jitGoCallPlain has
// asked jitPlainFn), a built-in of this realm. It reports false for any
// other call, which the built-in, called with its frame, is left to make.
func (r *Runtime) jitPlainCall(ctx *abi.Context, in bytecode.Instr, n int) (Value, bool) {
	argc := int(in.A)
	first := 1
	this := Undefined
	if in.Op == bytecode.OpCallMethod {
		this, first = jitGoArg(ctx, 0), 2
	}
	if first+argc != n {
		return Undefined, false
	}
	callee := jitGoArg(ctx, first-1)
	if !callee.IsObject() {
		return Undefined, false
	}
	fd := callee.Object().fn()
	if fd == nil || !jitLeafOp(fd.elemOp) || fd.realm != nil && fd.realm != r.Realm {
		return Undefined, false
	}
	// The arguments where native code put them, each a Value's words
	// (jitGoArg), read in place: no leaf keeps them, and each operand's
	// source holds it meanwhile. Copied into an array of this frame's, they
	// had escaped to the heap at every call -- a leaf is called through a
	// function's value (leafOps) -- which made Date.now's calls 2.5 times
	// slower; into the runtime's, a Map's calls paid the collector's write
	// barrier to clear them.
	args := unsafe.Slice((*Value)(unsafe.Pointer(&ctx.GoArgs[first])), argc)
	// Straight to what applies it, past pureElemOp's and collectionOp's
	// questions, which ask what this has asked (jitLeafOp).
	switch op := fd.elemOp; {
	case op >= elemNativeFirst:
		if r.meter != nil || !leafChecks[op](r, this, args) {
			return Undefined, false
		}
		v, err := fd.native(r, this, args)
		return v, err == nil
	case op > elemRandom:
		return leafOps[op](r, this, args, true)
	}
	return r.collectionOp(fd.elemOp, this, args, true)
}

// jitLeafOp reports whether native code's call of a built-in of frameless
// application op (elemOp) is made without a frame (jitGoCallPlain): any
// but those whose native forms the JIT learns at the calls of Go jitGoCall
// makes (jitCallSeen) -- push and pop, Math.random, a string's charCodeAt,
// its one character's substring, slice, substr and charAt, its searches,
// String.fromCharCode, and parseInt of a number -- which, made without a
// frame, it would never learn.
func jitLeafOp(op uint8) bool {
	switch op {
	case 0, elemPush, elemPop, elemRandom, elemStrCharCodeAt, elemStrCharAt, elemStrSlice, elemStrSubstring,
		elemStrSubstr, elemStrIndexOf, elemStrLastIndexOf, elemStrIncludes, elemStrFromCharCode, elemParseInt:
		return false
	}
	return true
}

// jitMaybePlain reports, of a call of Go native code makes (ssa.GoCall),
// in a few instructions, which inline where jitHostCall asks it, whether
// jitGoCallPlain might make it: a property's write; or a call, or a method
// call, whose function's address has its bit in jitPlainSet -- every leaf's,
// and a few others', which jitGoCallPlain then refuses.
func (r *Runtime) jitMaybePlain(ctx *abi.Context) bool {
	switch kind := ctx.GoOp >> abi.GoOpKindShift & 0xff; kind {
	case abi.GoCallSet:
		return true
	case 0, abi.GoCallMethod:
		// The function, the call's first operand, or a method call's
		// second, past its receiver.
		return r.jitPlainFn(ctx.GoArgs[kind].Ref)
	}
	return false
}

// jitPlainFn reports whether ref, a call's function's pointer word, may be
// one of the leaves jitPlainCall calls (jitPlainSet): true for each of
// them, and for a few others, which jitPlainCall refuses.
func (r *Runtime) jitPlainFn(ref uintptr) bool {
	s := jitPlainSlot(ref)
	return r.jitPlainSet[s>>6]&(1<<(s&63)) != 0
}

// MapNexts is ssa.MapStepFeedback's: the realm's Map and Set iterators'
// next functions (jitMapNexts), objects as long-lived as the realm, whose
// iterators native code steps itself where it can (mir's mapStep), and
// leaves the rest to Go's call (jitGoCall).
func (fb *jitFeedback) MapNexts() [2]uintptr {
	r := fb.r
	return [2]uintptr{uintptr(unsafe.Pointer(r.jitMapNexts[0])), uintptr(unsafe.Pointer(r.jitMapNexts[1]))}
}

// jitPlainWrite makes in, a property's write of val to obj, where it runs
// nothing and throws nothing: an array's own length, written with a number
// that is a length on a dense array whose length may be written, as
// setProp's fast path writes it; or what the site's cache answers for the
// object's shape with a plain data property it has or an addition along
// the cache's transition, as setPropCached makes it (storeCached) -- not a
// setter's, which runs one. It reports false for any other, which it
// leaves as it was.
func (r *Runtime) jitPlainWrite(cl *closure, in bytecode.Instr, obj, val Value) bool {
	if !obj.IsObject() || int(in.A) >= len(cl.names) {
		return false
	}
	o, key := obj.Object(), cl.names[in.A]
	if key == atomLength && o.class == ClassArray {
		if !val.IsNumber() || o.flags&(objArrayLengthWritable|objHasSparseElements) != objArrayLengthWritable || !o.noIndexKeys() {
			return false
		}
		n := uint32(val.num)
		if float64(n) != val.num {
			return false
		}
		o.setArrayLength(n)
		return true
	}
	if int(in.B) >= len(cl.ic) || o.shape == nil {
		return false
	}
	c := &cl.ic[in.B]
	if o.shape != c.shape {
		p := c.poly
		if p == nil {
			return false
		}
		c = nil
		for i := range p.n {
			if e := &p.entries[i]; e.shape == o.shape {
				c = e
				break
			}
		}
		if c == nil {
			return false
		}
	}
	if c.next == setterNext {
		return false
	}
	done, err := r.storeCached(c, o, key, val)
	return done && err == nil
}

// jitLeafCall makes a leaf's call native code makes as one (ssa.GoLeaf),
// whose function, its first operand, the code has checked it calls: as
// jitPlainCall applies it, with nothing of the call to find out first --
// its function's, its receiver's and its arguments' places are the call's
// own. It writes the result to the call's keep cell and reports whether
// it made the call.
func (r *Runtime) jitLeafCall(ctx *abi.Context) bool {
	argc := int(ctx.GoOp>>abi.GoOpArgcShift) & 0xff
	first, this := 1, Undefined
	if ctx.GoOp>>abi.GoOpKindShift&0xff == abi.GoCallMethod {
		first, this = 2, jitGoArg(ctx, 1)
	}
	if first+argc > abi.MaxGoArgs {
		return false
	}
	fd := jitGoArg(ctx, 0).Object().fn()
	args := unsafe.Slice((*Value)(unsafe.Pointer(&ctx.GoArgs[first])), argc)
	var v Value
	switch op := fd.elemOp; {
	case op >= elemNativeFirst:
		if r.meter != nil || !leafChecks[op](r, this, args) {
			return false
		}
		var err error
		if v, err = fd.native(r, this, args); err != nil {
			return false
		}
	case op > elemRandom:
		var ok bool
		if v, ok = leafOps[op](r, this, args, true); !ok {
			return false
		}
	default:
		var ok bool
		if v, ok = r.collectionOp(op, this, args, true); !ok {
			return false
		}
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitLeafSeen notes, once a call (abi.GoCallState's LeafSeen), the call of
// a leaf jitGoCallPlain has just made, at its function's PC, for its code's
// next compile (jitFeedback.Leaf), which makes it a call of Go of its own
// (ssa.GoLeaf): a call of the function the code runs, the run's frame's or
// a native callee's, not one inlined in it.
func (r *Runtime) jitLeafSeen(ctx *abi.Context, in bytecode.Instr) {
	if ctx.GoLast == 0 {
		return
	}
	st := (*abi.GoCallState)(*(*unsafe.Pointer)(unsafe.Pointer(&ctx.GoLast)))
	if st.LeafSeen {
		return
	}
	st.LeafSeen = true
	s, idx := r.jit, int(ctx.Level)
	if idx >= len(s.ssaCtxs) || s.ssaCtxs[idx] != ctx {
		return
	}
	var cl *closure
	switch ctx.Live {
	case 0:
		if idx >= len(s.goRuns) || s.goRuns[idx].f == nil {
			return
		}
		cl = s.goRuns[idx].f.cl
	case abi.LiveCall:
		cl = (*closure)(ctx.Closure)
	}
	pc := int(ctx.ExitPC)
	if cl == nil || pc >= len(cl.fn.Code) || cl.fn.Code[pc].Op != in.Op || cl.fn.Code[pc].A != in.A {
		return
	}
	e := r.jit.entryFor(cl.fn)
	first := 1
	if in.Op == bytecode.OpCallMethod {
		first = 2
	}
	callee := jitGoArg(ctx, first-1)
	if e == nil || !callee.IsObject() {
		return
	}
	o := callee.Object()
	for _, x := range jitTargets(e) {
		if int(x.pc) == pc && x.leaf {
			return
		}
	}
	e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: o, leaf: true})
	e.inlinePending = true
}
