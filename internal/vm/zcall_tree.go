package vm

import (
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A call of a compiled function asks the same questions every time --
// whether it is a closure of this realm, bound, an arrow, a derived
// constructor, made inside a with or beside an eval, which tier runs it --
// and for a given function the answers never change. planTreeCall asks
// them once, after a call, and keeps the tree in funcData.treeCall when
// they all say the function is called as runFD would call it with nothing
// to establish but the frame; callTree then makes the call on that. It is
// in the package's last files with the other calls kept out of the hot
// files' way: see zcall_tail.go.

// planTreeCall decides whether calls of fd's function may go to callTree,
// after a call that ran it, which has built its tree if the tier builds
// one. A pure body is left undecided while pureCall may still answer its
// calls, which it tries first.
func planTreeCall(fd *funcData) {
	cl := fd.closure
	fn := cl.fn
	t := (*tree)(atomic.LoadPointer(&fn.VMCode))
	switch {
	case t == nil:
		return
	case fn.Leaf == bytecode.LeafPure && cl.pureMiss < pureMissLimit:
		return
	case t == noTree || fd.native != nil || fd.bound || fd.arrow || fd.extra != nil ||
		fd.ctorKind == ctorDerived || !fn.DirectCall || fn.HasDirectEval ||
		fn.Leaf != bytecode.LeafNone && fn.Leaf != bytecode.LeafPure || fn.HasTailCall:
		// A leaf's other kinds are answered by leafCall, and a tail call
		// ends its frame in runFD's loop.
	default:
		fd.treeCall = t
	}
	fd.treePlanned = true
}

// callTree calls o, whose funcData planTreeCall has given a tree, with this
// and args, once callDirect has found it a compiled function of this
// realm and made its interrupt check: runFD's call for a function with no
// extra, run as its tree. A sloppy body's this that is a primitive is left
// to runFD, which wraps it.
func (r *Runtime) callTree(o *Object, fd *funcData, this Value, args []Value) (Value, error) {
	cl := fd.closure
	fn := cl.fn
	if fn.CoerceThis && !this.IsObject() {
		if !this.IsNullish() {
			return r.runFD(cl, this, args, Undefined, o, fd)
		}
		this = r.globalThis
	}
	if r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	base := r.stackTop
	need := fn.LocalCount + fn.MaxStack
	if base+need > len(r.stack) {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = base + need
	locals := r.stack[base : base+fn.LocalCount : base+fn.LocalCount]
	m := min(fn.ParamCount, len(locals), len(args))
	for i, a := range args[:m] {
		locals[i] = a
	}
	for i := m; i < len(locals); i++ {
		locals[i] = Undefined
	}
	if r.stackTop > r.stackHigh {
		r.stackHigh = r.stackTop
	}
	f := r.pushFrame()
	f.cl = cl
	f.locals = locals
	f.base = base + fn.LocalCount
	f.pc = 0
	f.this = this
	f.newTarget = Undefined
	f.callee = o
	f.args = args
	if len(f.openUpvalues) != 0 {
		f.openUpvalues = f.openUpvalues[:0]
	}
	if f.thisRef != nil {
		f.thisRef = nil
	}
	if f.withScopes != nil {
		f.withScopes = nil
	}
	if f.evalVars != nil {
		f.evalVars = nil
	}
	if len(f.handlers) != 0 {
		f.handlers = f.handlers[:0]
	}
	if f.native != "" {
		f.native = ""
	}
	if f.savedSP != 0 {
		f.savedSP = 0
	}
	v, err := r.runTree(f, fd.treeCall)
	r.popFrameOf(f, base)
	return v, err
}

// callRest is callDirect for a callee that is the object o, whose funcData
// is fd or nil, once a tree's call node has found it is not one
// planTreeCall has given a tree: callDirect's other cases, in its order.
func (r *Runtime) callRest(o *Object, fd *funcData, this Value, args []Value) (Value, error) {
	if fd != nil && fd.mathOp != 0 {
		if v, ok := mathCall(fd.mathOp, args); ok {
			return v, nil
		}
	}
	if o == r.callFn {
		return r.callThrough(o, this, args)
	}
	if fd != nil && fd.native == nil && !fd.bound &&
		fd.closure != nil && fd.closure.realm == r.Realm {
		if fn := fd.closure.fn; fn.DirectCall {
			if err := r.tick(); err != nil {
				return Undefined, err
			}
			if leaf := fn.Leaf; leaf == bytecode.LeafPure {
				if v, ok := r.pureCall(fd, this, args); ok {
					return v, nil
				}
			} else if leaf != bytecode.LeafNone && this.IsObject() {
				if v, ok := r.leafCall(fd.closure, this.Object(), args); ok {
					return v, nil
				}
			}
			newTarget := Undefined
			if fd.arrow {
				this, newTarget = fd.extra.lexThis, fd.extra.lexNewTarget
			}
			v, err := r.runFD(fd.closure, this, args, newTarget, o, fd)
			if err == errNoSuper {
				err = r.throwError(errReference, "%s", errNoSuper.Error())
			}
			if !fd.treePlanned {
				planTreeCall(fd)
			}
			return v, err
		}
	}
	if fd != nil && fd.native != nil && !fd.bound && (fd.realm == nil || fd.realm == r.Realm) {
		if err := r.tick(); err != nil {
			return Undefined, err
		}
		if err := r.nativeFrame(o, fd, this, args, Undefined); err != nil {
			return Undefined, err
		}
		v, err := fd.native(r, this, args)
		r.frameDepth--
		return v, err
	}
	return r.call(Obj(o), this, args)
}
