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
// frame of JavaScript's: a Map's or a Set's get, has, set, add or delete
// on a Map or a Set (jitCollectionCall); String(x) of a primitive; a write
// a site's cache answers with a data property or an addition along its
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
	default:
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitPlainCall is the result of the call of a built-in among its operands
// (abi.GoCallMethod's receiver, the function, the arguments; a plain
// call's the function and the arguments) where it runs nothing and throws
// nothing: a collection's lookup, addition or deletion on its own kind of
// collection (jitCollectionCall), or String(x) of a primitive -- a
// symbol's description included, which only String's call converts. The
// function is one of jitPlainFns (jitGoCallPlain has asked jitPlainFn),
// the realm's own built-ins, of none of which anything is to be asked. It
// reports false for any other call.
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
	o := callee.Object()
	if o == r.jitStringCtor {
		// As the constructor's call converts it, of a primitive: nothing
		// runs, nothing throws.
		if r.meter != nil {
			return Undefined, false
		}
		if argc == 0 {
			return Str(emptyString), true
		}
		x := jitGoArg(ctx, first)
		switch {
		case x.IsObject():
			return Undefined, false
		case x.IsSymbol():
			return Str(NewString(x.Symbol().String())), true
		}
		s, err := r.toString(x)
		if err != nil {
			return Undefined, false
		}
		return Str(s), true
	}
	if in.Op != bytecode.OpCallMethod || argc > 2 {
		return Undefined, false
	}
	var args [2]Value
	for i := range argc {
		args[i] = jitGoArg(ctx, first+i)
	}
	return r.jitCollectionCall(o, this, args[:argc])
}

// jitCollectionCall is Map.prototype's get, has, set and delete, or
// Set.prototype's has, add and delete, the realm's, o, called on this with
// args, applied as the tree tier applies them without a frame
// (collectionOp), on a Map or a Set as each asks its receiver to be: a
// lookup, an addition or a deletion calls nothing a script wrote and
// cannot throw. It reports false for any other function or receiver,
// which the built-in is left to refuse.
func (r *Runtime) jitCollectionCall(o *Object, this Value, args []Value) (Value, bool) {
	if o != r.jitMapDelete && o != r.jitSetDelete {
		if fd := o.fn(); fd != nil && fd.elemOp >= elemMapGet && fd.elemOp <= elemSetAdd {
			return r.collectionOp(fd.elemOp, this, args, true)
		}
		return Undefined, false
	}
	if !this.IsObject() {
		return Undefined, false
	}
	c := this.Object()
	m, ok := c.dataOf().(*jsMap)
	if !ok || o == r.jitMapDelete && c.class != ClassMap || o == r.jitSetDelete && c.class != ClassSet {
		return Undefined, false
	}
	return Bool(m.delete(arg(args, 0))), true
}

// jitMaybePlain reports, of a call of Go native code makes (ssa.GoCall),
// in a few instructions, which inline where jitHostCall asks it, whether
// jitGoCallPlain might make it: a property's write; or a call, or a method
// call, whose function's address starts its search in jitPlainSet at a
// slot that holds one -- a quarter of the others, which jitGoCallPlain
// then refuses.
func (r *Runtime) jitMaybePlain(ctx *abi.Context) bool {
	switch kind := ctx.GoOp >> abi.GoOpKindShift & 0xff; kind {
	case abi.GoCallSet:
		return true
	case 0, abi.GoCallMethod:
		// The function, the call's first operand, or a method call's
		// second, past its receiver.
		return r.jitPlainSet[jitPlainSlot(ctx.GoArgs[kind].Ref)] != 0
	}
	return false
}

// jitPlainFn reports whether ref, a call's function's pointer word, is one
// of jitPlainFns, the functions jitPlainCall calls (jitPlainSet).
func (r *Runtime) jitPlainFn(ref uintptr) bool {
	set := &r.jitPlainSet
	for i := jitPlainSlot(ref); set[i] != 0; i = (i + 1) % len(set) {
		if set[i] == ref {
			return true
		}
	}
	return false
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
