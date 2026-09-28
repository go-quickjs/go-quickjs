package vm

import (
	"errors"
	"math"
	"math/big"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/fdlibm"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// The interpreter.
//
// One contiguous slice serves as both the local-variable storage and the
// operand stack. A frame occupies a window of it laid out as locals followed by
// operands, so entering a call costs a bounds check and a slice reslice rather
// than an allocation.
//
// The slice is allocated once at its full size and never grown. That is what
// makes it safe for an upvalue to hold a *Value pointing into a live frame: the
// backing array never moves. It also gives the sandbox its stack limit for
// free, since exhausting the slice is exactly "maximum call stack size
// exceeded".

// defaultStackSize is the number of value slots available to all frames
// together. At 24 bytes per slot this is about 6 MB, which allows a recursion
// depth in the tens of thousands for typical functions.
const defaultStackSize = 256 * 1024

// callDepthLimit bounds recursion independently of the slot count, so that a
// function with very few locals still cannot recurse without bound.
const defaultCallDepthLimit = 8192

// maxGoRecursion bounds how deep the engine recurses in Go, whatever a host
// asks for: a call takes a few kilobytes of the goroutine's stack, and one
// that grows past Go's limit -- a gigabyte on a 64-bit platform and a quarter
// of one on a 32-bit one -- ends the process, which nothing can recover
// from. It bounds the depth of calls, and separately the recursion no frame
// counts: a proxy forwarding to its target, JSON's nesting, flat's.
// It is 50,000 on a 64-bit platform and 25,000 on a 32-bit one.
const maxGoRecursion = 12500 << (unsafe.Sizeof(uintptr(0)) / 4)

// frameBlockSize is how many frames are allocated at a time. A frame is a
// couple of hundred bytes and the depth limit is thousands, so allocating the
// limit up front would cost every runtime megabytes it will almost certainly
// never use; a block is small enough to be cheap and large enough that the
// allocation is rare.
const frameBlockSize = 64

// call invokes a callable value.
func (r *Runtime) call(fn Value, this Value, args []Value) (Value, error) {
	if !fn.IsObject() {
		return Undefined, r.throwTypeError("%s is not a function", r.describe(fn))
	}
	return r.callObject(fn.Object(), this, args, Undefined)
}

// callObject invokes a function object, dispatching to a native
// implementation, a bound function or compiled bytecode.
func (r *Runtime) callObject(o *Object, this Value, args []Value, newTarget Value) (Value, error) {
	// Calls are counted as well as instructions, because a program can run for
	// a long time without either finishing a frame's instruction budget or
	// leaving one loop: deep recursion, or a loop whose every iteration calls
	// something expensive. Both pass through here, and the count is nothing
	// beside the cost of the call it is attached to.
	if err := r.tick(); err != nil {
		return Undefined, err
	}
	if p := proxyOf(o); p != nil {
		if !newTarget.IsUndefined() {
			return r.proxyConstruct(p, args, newTarget)
		}
		return r.proxyCall(p, this, args)
	}
	fd := o.fn()
	if fd == nil {
		return Undefined, r.throwTypeError("value is not a function")
	}

	// A bound function prepends its stored arguments and replaces `this`,
	// except under `new`, where the original `this` is discarded anyway.
	if fd.boundTarget != nil {
		merged := args
		if len(fd.boundArgs) > 0 {
			merged = make([]Value, 0, len(fd.boundArgs)+len(args))
			merged = append(merged, fd.boundArgs...)
			merged = append(merged, args...)
		}
		boundThis := fd.boundThis
		if !newTarget.IsUndefined() {
			boundThis = this
		}
		// Each bound function in a chain is a level of Go recursion with no
		// frame of its own, so it is counted, or a long enough chain would
		// take the call past the Go stack.
		if err := r.nest(); err != nil {
			return Undefined, err
		}
		v, err := r.callObject(fd.boundTarget, boundThis, merged, newTarget)
		r.unnest()
		return v, err
	}

	if fd.native != nil {
		if r.frameDepth >= r.maxFrames {
			return Undefined, r.throwRangeError("maximum call stack size exceeded")
		}
		// A native frame is pushed so that stack traces include it.
		f := r.pushFrame()
		f.cl = nil
		f.native = fd.name
		f.this = this
		f.newTarget = newTarget
		f.callee = o
		f.args = args
		f.handlers = f.handlers[:0]
		f.openUpvalues = f.openUpvalues[:0]
		// A built-in runs in its own realm, whoever calls it.
		if re := fd.realm; re != nil && re != r.Realm {
			prev := r.Realm
			r.Realm = re
			v, err := fd.native(r, this, args)
			r.Realm = prev
			r.frameDepth--
			return v, err
		}
		v, err := fd.native(r, this, args)
		r.frameDepth--
		return v, err
	}

	if fd.closure == nil {
		return Undefined, r.throwTypeError("function has no implementation")
	}
	// So does a compiled function.
	var v Value
	var err error
	if re := fd.closure.realm; re != r.Realm {
		prev := r.Realm
		r.Realm = re
		v, err = r.callClosure(o, fd, this, args, newTarget)
		r.Realm = prev
	} else {
		v, err = r.callClosure(o, fd, this, args, newTarget)
	}
	if err == errNoSuper {
		err = r.throwError(errReference, "%s", errNoSuper.Error())
	}
	return v, err
}

// callClosure calls a compiled function, in the realm it belongs to.
func (r *Runtime) callClosure(o *Object, fd *funcData, this Value, args []Value, newTarget Value) (Value, error) {
	// An arrow ignores the this and new.target it was called with.
	if fd.arrow {
		this = fd.lexThis
		newTarget = fd.lexNewTarget
	}

	// A generator or async function does not run its body on call. A generator
	// returns an object whose next method drives it; an async function starts
	// immediately but returns a promise at its first await.
	if fn := fd.closure.fn; isGeneratorTemplate(fn) {
		// A generator builds its frames itself, so the receiver is coerced here
		// rather than by run: a sloppy-mode async function called with no
		// receiver sees the global object like any other.
		if fn.UsesThis && !this.IsObject() && !fn.Strict {
			if this.IsNullish() {
				this = r.globalThis
			} else {
				w, err := r.toObject(this)
				if err != nil {
					return Undefined, err
				}
				this = Obj(w)
			}
		}
		gen, err := r.newGenerator(fd.closure, this, args, o, fn.Async, newTarget)
		if err != nil {
			// A generator binds its parameters at the call, so a destructuring
			// error surfaces here rather than at the first next(). An async
			// function turns it into a rejection instead, which is what makes
			// an async function never throw synchronously.
			if fn.Async && !fn.Generator {
				p := r.newPromise()
				r.rejectPromise(p, thrownValue(err))
				return Obj(p), nil
			}
			return Undefined, err
		}
		g := gen.data.(*generator)
		switch {
		case fn.Generator:
			// An async generator is still a generator: it returns an object
			// whose next method drives it, differing only in that the method
			// returns a promise.
			return Obj(gen), nil
		case fn.Async:
			return r.runAsync(g), nil
		}
		return Obj(gen), nil
	}
	// A class constructor has to be constructed. Calling one would run its
	// body with no object to initialize -- and in a derived class with no
	// `this` at all, since super() is what binds it.
	if newTarget.IsUndefined() && isClassConstructorKind(fd.closure.fn.Kind) {
		return Undefined, r.throwTypeError(
			"class constructor %s cannot be invoked without \"new\"", fd.name)
	}
	return r.run(fd.closure, this, args, newTarget, o)
}

// isClassConstructorKind reports whether a function is a class's constructor,
// which is the one kind that may only be constructed.
func isClassConstructorKind(k bytecode.FuncKind) bool {
	return k == bytecode.KindConstructor || k == bytecode.KindDerivedConstructor
}

// describe renders a value for an error message without risking a callback
// into user code, which toString would.
func (r *Runtime) describe(v Value) string {
	switch v.Kind() {
	case KindUndefined:
		return "undefined"
	case KindNull:
		return "null"
	case KindString:
		return "\"" + v.String().Go() + "\""
	case KindNumber:
		return jsnum.FormatFloat(v.Number())
	case KindBool:
		if v.BoolValue() {
			return "true"
		}
		return "false"
	}
	return v.Kind().String()
}

// run executes a compiled function, and the chain of tail calls it ends in.
//
// A frame that ends in a tail call is given up before the callee's frame is
// made, in this same call of run, so that a chain of them uses one frame and
// one level of the Go stack however long it is. A callee this cannot be done
// for -- a native function, a proxy, a generator -- is called the ordinary
// way, which ends the chain.
func (r *Runtime) run(cl *closure, this Value, args []Value, newTarget Value, callee *Object) (Value, error) {
start:
	fn := cl.fn

	// A sloppy-mode function's `this` is coerced: undefined and null become the
	// global object, and a primitive becomes its wrapper. Strict mode leaves it
	// exactly as passed, which is the difference that makes strict mode able to
	// detect a missing receiver at all.
	// The object case is tested first because it is the common one and costs a
	// single mask: a method call already has an object receiver and needs no
	// coercion at all.
	if fn.UsesThis && !this.IsObject() && !fn.Strict &&
		fn.Kind != bytecode.KindArrow && newTarget.IsUndefined() {
		if this.IsNullish() {
			this = r.globalThis
		} else {
			o, err := r.toObject(this)
			if err != nil {
				return Undefined, err
			}
			this = Obj(o)
		}
	}

	if r.frameDepth >= r.maxFrames {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}

	// Carve a window out of the shared stack: locals first, then operands.
	base := r.stackTop
	need := fn.LocalCount + fn.MaxStack
	if base+need > len(r.stack) {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = base + need

	locals := r.stack[base : base+fn.LocalCount : base+fn.LocalCount]
	// Locals must start clear, since the window was last used by an unrelated
	// frame and a stale value could be read by a binding whose declaration was
	// never reached. The parameter slots are skipped because bindParameters
	// overwrites every one of them immediately below; clearing a Value costs a
	// write barrier, so the saving is real.
	if n := fn.ParamCount; n < len(locals) {
		clear(locals[n:])
	}

	// The high-water mark is what endTurn clears back to, so that a value left
	// behind by a returning frame does not stay reachable until its slot is
	// reused.
	if r.stackTop > r.stackHigh {
		r.stackHigh = r.stackTop
	}

	f := r.pushFrame()
	// The fields are assigned rather than the struct replaced, so that the
	// handler and upvalue slices keep their backing arrays across calls. A
	// frame is large enough that copying a fresh one, and reallocating those
	// slices, showed up in the profile.
	f.cl = cl
	f.locals = locals
	f.base = base + fn.LocalCount
	f.pc = 0
	f.this = this
	// A derived constructor does not receive `this`; super() binds it. Until
	// then the object the caller made is held but unreachable, so that a
	// subclass cannot touch what the base class has not finished building.
	// The kind alone does not say which constructors are derived: a derived
	// class with no explicit constructor gets a synthesized one, and what makes
	// it derived is the heritage clause the class object records.
	f.thisRef = nil
	f.newTarget = newTarget
	f.callee = callee
	f.args = args
	f.openUpvalues = f.openUpvalues[:0]
	// The chain is inherited whole, capped so that a push inside this call
	// copies rather than writing into the creating frame's array.
	f.withScopes = nil
	f.evalVars = nil
	if callee != nil {
		if fd := callee.fn(); fd != nil {
			switch {
			case !newTarget.IsUndefined() && fd.ctorKind == ctorDerived:
				f.thisRef = &thisBinding{value: this}
			case fd.arrow && fd.lexThisRef != nil:
				// An arrow written inside a derived constructor shares its
				// binding, so calling one before super() is the same error.
				f.thisRef = fd.lexThisRef
			}
			if len(fd.lexWith) > 0 {
				f.withScopes = fd.lexWith[:len(fd.lexWith):len(fd.lexWith)]
			}
			// What an eval declared in an enclosing function is still in scope
			// here, whether or not this one has anything of its own.
			f.evalVars = fd.lexEvalVars
		}
	}
	if fn.HasDirectEval {
		// The body contains a direct eval, so it needs somewhere for the vars
		// that eval may declare. It goes on the scope chain, inside whatever
		// the enclosing functions put there: a name the evaluated code
		// declares is found the way a `with` object's properties are.
		f.evalVars = newObject(nil, ClassObject)
		f.evalVars.flags |= objEvalVars
		f.withScopes = append(f.withScopes[:len(f.withScopes):len(f.withScopes)], f.evalVars)
	}
	f.handlers = f.handlers[:0]
	f.native = ""
	f.savedSP = 0

	if err := r.bindParameters(f, fn, args); err != nil {
		r.popFrame(base)
		return Undefined, err
	}

	v, err := r.execute(f)
	r.popFrame(base)
	if err != errTailCall {
		return v, err
	}
	// The frame ended in a tail call, and is gone: the callee's is made in
	// its place, in this same call of run.
	var ok bool
	if cl, this, args, callee, ok, err = r.takeTailCall(); !ok {
		return this, err
	}
	newTarget = Undefined
	goto start
}

// takeTailCall resolves the call a frame ended in. It reports true with the
// frame to make in its place, or false with the call's result, having made it
// the ordinary way -- which a callee that is not a compiled function needs.
func (r *Runtime) takeTailCall() (*closure, Value, []Value, *Object, bool, error) {
	tc := r.pendingTail
	r.pendingTail = tailCall{}
	cl, this, args, callee, ok, err := r.tailTarget(tc)
	if err != nil {
		return nil, Undefined, nil, nil, false, err
	}
	if !ok {
		v, err := r.callObject(tc.callee.Object(), tc.this, tc.args, Undefined)
		return nil, v, nil, nil, false, err
	}
	return cl, this, args, callee, true, nil
}

// tailCall is a call a frame ended in, waiting for run to make it.
type tailCall struct {
	callee Value
	this   Value
	args   []Value
}

// tailCallDepth is how deep the call stack is before a tail call gives up its
// frame. Giving one up is observable only in the space it saves, so a shallow
// stack keeps its frames and makes the call the ordinary way, which is
// cheaper: a chain of tail calls still runs in constant space, just not in
// the least there is.
const tailCallDepth = 32

// errTailCall is how a frame tells run that it ended in a tail call, which
// pendingTail holds.
var errTailCall = errors.New("tail call")

// tailTarget resolves the callee of a tail call to a compiled function run can
// make a frame for, looking through bound functions. It reports false for one
// it cannot -- which is then called the ordinary way.
func (r *Runtime) tailTarget(tc tailCall) (*closure, Value, []Value, *Object, bool, error) {
	if !tc.callee.IsObject() {
		return nil, Undefined, nil, nil, false, r.throwTypeError("%s is not a function", r.describe(tc.callee))
	}
	o, this, args := tc.callee.Object(), tc.this, tc.args
	for {
		if err := r.tick(); err != nil {
			return nil, Undefined, nil, nil, false, err
		}
		if proxyOf(o) != nil {
			return nil, Undefined, nil, nil, false, nil
		}
		fd := o.fn()
		if fd == nil || fd.native != nil {
			return nil, Undefined, nil, nil, false, nil
		}
		if fd.boundTarget != nil {
			if len(fd.boundArgs) > 0 {
				merged := make([]Value, 0, len(fd.boundArgs)+len(args))
				merged = append(merged, fd.boundArgs...)
				args = append(merged, args...)
			}
			o, this = fd.boundTarget, fd.boundThis
			continue
		}
		// A function of another realm is called the ordinary way, which is
		// what switches to its realm.
		if fd.closure == nil || isGeneratorTemplate(fd.closure.fn) ||
			isClassConstructorKind(fd.closure.fn.Kind) || fd.closure.realm != r.Realm {
			return nil, Undefined, nil, nil, false, nil
		}
		if fd.arrow {
			this = fd.lexThis
			if !fd.lexNewTarget.IsUndefined() {
				// An arrow inside a constructor sees its new.target, which run
				// would have to be given: the ordinary call path does that.
				return nil, Undefined, nil, nil, false, nil
			}
		}
		return fd.closure, this, args, o, true, nil
	}
}

// pushFrame extends the call stack by one and returns the new frame.
//
// The slice is resliced rather than appended to. Its capacity is fixed at
// construction, so this can never reallocate -- which matters because the
// interpreter holds a *frame across nested calls, and a reallocation would
// silently leave those pointers aimed at the abandoned array.
//
// Reslicing also means the frame left behind at this depth keeps its handler
// and upvalue slices, whose backing arrays the next call reuses.
// detachSuspended lets go of the handler and upvalue slices a generator's
// frame ran with, which the generator keeps for its next resumption. The
// frame goes back to the pool, and the next call made at its depth reuses
// the slices it holds, truncating them and appending: were they still the
// generator's, that call would write over the catch handlers and captured
// variables of the generator it was suspended with.
func (f *frame) detachSuspended() {
	f.handlers = nil
	f.openUpvalues = nil
}

func (r *Runtime) pushFrame() *frame {
	// The block the next frame goes in is kept to hand, so an ordinary push is
	// a subtraction and an index. The unsigned comparison covers both ways the
	// block can be the wrong one: the depth has grown past its end, or it has
	// fallen below its start since the block was chosen.
	off := r.frameDepth - r.frameBase
	if uint(off) >= uint(len(r.cur)) {
		r.seekFrameBlock()
		off = r.frameDepth - r.frameBase
	}
	r.frameDepth++
	if r.frameDepth > r.frameHigh {
		r.frameHigh = r.frameDepth
	}
	return &r.cur[off]
}

// seekFrameBlock picks the block the current depth falls in, allocating it if
// the call stack has never been this deep.
func (r *Runtime) seekFrameBlock() {
	b := uint(r.frameDepth) / frameBlockSize
	if b == uint(len(r.frames)) {
		r.frames = append(r.frames, make([]frame, frameBlockSize))
	}
	r.cur = r.frames[b]
	r.frameBase = int(b) * frameBlockSize
}

// frameAt is the frame at a depth, counting from the bottom.
func (r *Runtime) frameAt(i int) *frame {
	u := uint(i)
	return &r.frames[u/frameBlockSize][u%frameBlockSize]
}

// topFrame is the frame being executed, or nil when nothing is.
func (r *Runtime) topFrame() *frame {
	if r.frameDepth == 0 {
		return nil
	}
	return r.frameAt(r.frameDepth - 1)
}

// popFrame releases a frame's stack window, closing any upvalues that pointed
// into its locals so that closures created inside it keep working.
//
// The window is deliberately left dirty. Clearing it on the way out was the
// single largest cost in the interpreter -- it doubled the zeroing work per
// call, since the next frame to use the region clears its locals on entry
// anyway. Nothing can read the stale values: locals are cleared before use, and
// the compiler guarantees every operand slot is written before it is read.
//
// The cost is that values stay reachable from the stack until their slots are
// reused. That delays collection, so the region above the live frames is
// cleared once per turn instead -- see endTurn, which is where a program could
// next observe the difference.
func (r *Runtime) popFrame(base int) {
	f := r.frameAt(r.frameDepth - 1)
	for _, u := range f.openUpvalues {
		u.close()
	}
	r.stackTop = base
	r.frameDepth--
}

// bindParameters copies arguments into the parameter slots.
func (r *Runtime) bindParameters(f *frame, fn *bytecode.Function, args []Value) error {
	n := fn.ParamCount
	if n > len(f.locals) {
		n = len(f.locals)
	}
	for i := 0; i < n; i++ {
		if i < len(args) {
			f.locals[i] = args[i]
		} else {
			f.locals[i] = Undefined
		}
	}
	// Anything beyond the simple positional case -- defaults, destructuring, a
	// rest parameter -- is compiled into the function prologue rather than
	// handled here, so there is nothing more to do.
	return nil
}

// pushAt stores v at sp and returns the next free slot. It takes and returns
// sp rather than holding it, so that the interpreter's sp stays a variable the
// compiler can keep in a register.
func pushAt(stack []Value, sp int, v Value) int {
	stack[sp] = v
	return sp + 1
}

// execute runs the interpreter loop for one frame.
func (r *Runtime) execute(f *frame) (Value, error) {
	return r.executeAt(f, f.base, nil)
}

// executeAt runs the interpreter loop starting from a given operand stack
// depth, optionally with an exception already pending.
//
// The extra parameters exist for generators: resuming one restores its operand
// stack, so execution starts part-way up, and generator.throw() injects an
// exception at the suspension point so that a try inside the body can catch it.
func (r *Runtime) executeAt(f *frame, startSP int, pending error) (Value, error) {
	// A script that has been stopped runs nothing more, even where the
	// interrupt was turned into a value -- a rejection, an error an iterator
	// being closed dropped -- that let the caller carry on and call again.
	if r.stopped != nil {
		return Undefined, r.stopped
	}
	cl := f.cl
	code := cl.fn.Code
	// sp is the operand stack pointer, an absolute index into r.stack.
	sp := startSP

	// The operand stack is the runtime's, which is never reallocated while
	// anything runs, and sp an index into it. sp is only ever read and
	// written directly -- never captured by a closure, never passed by its
	// address -- which is what lets the compiler keep it in a register: a
	// push or a pop is then an indexed store or load and nothing else.
	stack := r.stack

	// vmErr carries a pending exception from wherever it is raised to the
	// handler search at the bottom of the loop, and `in` is declared alongside
	// it because a goto may not jump over a declaration.
	var vmErr error
	var in bytecode.Instr

	if ret, ok := pending.(*returnSignal); ok {
		// generator.return() behaves as if a return statement ran at the
		// suspension point, so it must run the finally blocks between there
		// and the top of the body -- but must not be caught by a catch, which
		// a return statement would not trigger either.
		var ok bool
		if sp, ok = r.unwindToFinally(f, sp, ret.value); !ok {
			// Nothing owes a finally, so the body simply ends -- but a for-of
			// it was suspended inside still has to be told, and what its
			// return method reports is the result.
			if err := r.closeIteratorsReturning(f.base, sp); err != nil {
				return Undefined, err
			}
			return ret.value, nil
		}
	} else if pending != nil {
		// An exception injected at the resumption point unwinds as if it had
		// been raised by the suspended expression.
		vmErr = pending
		var ok bool
		if sp, ok = r.unwindToHandler(f, sp, vmErr); !ok {
			return Undefined, vmErr
		}
		vmErr = nil
	}

	// The program counter is kept in a local and written back to the frame at
	// each instruction, so that fetching one is a single store rather than a
	// load and a store. What the frame holds is what a stack trace taken from
	// inside a call reads, which is the instruction after the one calling --
	// the same as it ever was.
	pc := f.pc
	// The cancellation check is counted down at each backward jump, and only
	// its rare expiry is a call. A frame runs forever only by jumping back, so
	// that is where it has to be checked, and checking nowhere else keeps the
	// counter out of every other instruction -- and out of the registers the
	// dispatch needs. Each frame gets its own, so what this bounds is how many
	// times one frame may loop without a check; a program that leaves this
	// loop again and again instead of looping in it is bounded by the count of
	// calls that callObject keeps.
	budget := backEdgeCheckInterval
	for {
		in = code[pc]
		pc++
		f.pc = pc

		switch in.Op {
		case bytecode.OpNop:

		// --- Constants ----------------------------------------------------
		case bytecode.OpPushConst:
			sp = pushAt(stack, sp, cl.consts[in.A])
		case bytecode.OpPushUndef:
			sp = pushAt(stack, sp, Undefined)
		case bytecode.OpInitParam:
			// A parameter's slot is its position, so the argument that fills it
			// is at the same index.
			if int(in.A) < len(f.args) {
				f.locals[in.A] = f.args[in.A]
			} else {
				f.locals[in.A] = Undefined
			}
		case bytecode.OpParamNeedsDefault:
			sp = pushAt(stack, sp, Bool(int(in.A) >= len(f.args) || f.args[in.A].IsUndefined()))
		case bytecode.OpParamsToDeadZone:
			// The parameters of such a list are bound one at a time, so they
			// all start in the dead zone: until the prologue reaches one,
			// reading it is an error even though its argument has arrived.
			n := int(in.A)
			if n > len(f.locals) {
				n = len(f.locals)
			}
			for i := 0; i < n; i++ {
				f.locals[i] = uninitialized
			}
		case bytecode.OpPushUninitialized:
			sp = pushAt(stack, sp, uninitialized)
		case bytecode.OpPushNull:
			sp = pushAt(stack, sp, Null)
		case bytecode.OpPushTrue:
			sp = pushAt(stack, sp, True)
		case bytecode.OpPushFalse:
			sp = pushAt(stack, sp, False)
		case bytecode.OpPushInt:
			sp = pushAt(stack, sp, Int32(int32(in.A)))
		case bytecode.OpPushEmptyString:
			sp = pushAt(stack, sp, Str(emptyString))
		case bytecode.OpPushThis:
			v, bound := f.thisValue()
			if !bound {
				vmErr = r.throwError(errReference,
					"\"this\" is not bound until super() has been called")
				goto onError
			}
			sp = pushAt(stack, sp, v)

		// --- Stack --------------------------------------------------------
		case bytecode.OpDup:
			sp = pushAt(stack, sp, stack[sp-1])
		case bytecode.OpDup2:
			a, b := stack[sp-2], stack[sp-1]
			sp = pushAt(stack, sp, a)
			sp = pushAt(stack, sp, b)
		case bytecode.OpDrop:
			sp--
		case bytecode.OpSwap:
			r.stack[sp-1], r.stack[sp-2] = r.stack[sp-2], r.stack[sp-1]
		case bytecode.OpRot3:
			a := r.stack[sp-3]
			r.stack[sp-3] = r.stack[sp-2]
			r.stack[sp-2] = r.stack[sp-1]
			r.stack[sp-1] = a
		case bytecode.OpRot4:
			a := r.stack[sp-4]
			r.stack[sp-4] = r.stack[sp-3]
			r.stack[sp-3] = r.stack[sp-2]
			r.stack[sp-2] = r.stack[sp-1]
			r.stack[sp-1] = a
		case bytecode.OpInsert2:
			// a b -> b a b
			b := r.stack[sp-1]
			r.stack[sp] = b
			r.stack[sp-1] = r.stack[sp-2]
			r.stack[sp-2] = b
			sp++
		case bytecode.OpAssignConst:
			// Assigning to a const is a runtime error, not an early one: the
			// assignment may sit in a function that is never called.
			vmErr = r.throwTypeError("assignment to constant variable %q",
				r.atoms.name(cl.names[in.A]))
			goto onError
		case bytecode.OpNipUnder:
			// The top value stays; the A beneath it go.
			n := int(in.A)
			r.stack[sp-1-n] = r.stack[sp-1]
			sp -= n
		case bytecode.OpInsert3:
			// a b c -> c a b c
			c := r.stack[sp-1]
			r.stack[sp] = c
			r.stack[sp-1] = r.stack[sp-2]
			r.stack[sp-2] = r.stack[sp-3]
			r.stack[sp-3] = c
			sp++
		case bytecode.OpInsert4:
			// a b c d -> d a b c d
			d := r.stack[sp-1]
			r.stack[sp] = d
			r.stack[sp-1] = r.stack[sp-2]
			r.stack[sp-2] = r.stack[sp-3]
			r.stack[sp-3] = r.stack[sp-4]
			r.stack[sp-4] = d
			sp++

		// --- Locals -------------------------------------------------------
		case bytecode.OpGetLocal:
			sp = pushAt(stack, sp, f.locals[in.A])
		case bytecode.OpSetLocalGet:
			locals := f.locals
			locals[in.A] = stack[sp-1]
			stack[sp-1] = locals[in.B]
		case bytecode.OpClearLocal:
			f.locals[in.A] = Undefined
		case bytecode.OpBinImm:
			a, b := stack[sp-1], Int32(int32(in.A))
			op := bytecode.Op(in.B)
			if a.IsNumber() {
				x, y := a.Number(), int32(in.A)
				switch op {
				case bytecode.OpAdd:
					stack[sp-1] = Float(x + float64(y))
				case bytecode.OpSub:
					stack[sp-1] = Float(x - float64(y))
				case bytecode.OpMul:
					stack[sp-1] = Float(x * float64(y))
				case bytecode.OpBitAnd:
					stack[sp-1] = Int32(jsnum.ToInt32(x) & y)
				case bytecode.OpBitOr:
					stack[sp-1] = Int32(jsnum.ToInt32(x) | y)
				case bytecode.OpBitXor:
					stack[sp-1] = Int32(jsnum.ToInt32(x) ^ y)
				case bytecode.OpShl:
					stack[sp-1] = Int32(jsnum.ToInt32(x) << (uint32(y) & 31))
				case bytecode.OpShr:
					stack[sp-1] = Int32(jsnum.ToInt32(x) >> (uint32(y) & 31))
				default:
					stack[sp-1] = Uint32(uint32(jsnum.ToInt32(x)) >> (uint32(y) & 31))
				}
				break
			}
			v, err := r.binImm(op, a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			stack[sp-1] = v
		case bytecode.OpGetLocal2:
			locals := f.locals
			stack[sp] = locals[in.A]
			stack[sp+1] = locals[in.B]
			sp += 2
		case bytecode.OpSetLocal:
			sp--
			f.locals[in.A] = stack[sp]
		case bytecode.OpPutLocal:
			f.locals[in.A] = stack[sp-1]
		case bytecode.OpInitLocal:
			sp--
			f.locals[in.A] = stack[sp]
		case bytecode.OpGetLocalCheck:
			v := f.locals[in.A]
			if v.IsUninitialized() {
				vmErr = r.throwReferenceError(
					"cannot access %q before initialization", cl.fn.Locals[in.A].Name)
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpSetLocalCheck:
			if f.locals[in.A].IsUninitialized() {
				vmErr = r.throwReferenceError(
					"cannot access %q before initialization", cl.fn.Locals[in.A].Name)
				goto onError
			}
			if !cl.fn.Locals[in.A].Mutable {
				vmErr = r.throwTypeError(
					"assignment to constant variable %q", cl.fn.Locals[in.A].Name)
				goto onError
			}
			sp--
			f.locals[in.A] = stack[sp]

		// --- Upvalues -----------------------------------------------------
		case bytecode.OpGetUpvalue:
			sp = pushAt(stack, sp, cl.upvalues[in.A].get())
		case bytecode.OpSetUpvalue:
			sp--
			cl.upvalues[in.A].set(stack[sp])
		case bytecode.OpInitUpvalue:
			sp--
			cl.upvalues[in.A].set(stack[sp])
		case bytecode.OpGetUpvalueCheck:
			v := cl.upvalues[in.A].get()
			if v.IsUninitialized() {
				vmErr = r.throwReferenceError(
					"cannot access %q before initialization", cl.fn.Upvalues[in.A].Name)
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpSetUpvalueCheck:
			if cl.upvalues[in.A].get().IsUninitialized() {
				vmErr = r.throwReferenceError(
					"cannot access %q before initialization", cl.fn.Upvalues[in.A].Name)
				goto onError
			}
			if !cl.fn.Upvalues[in.A].Mutable {
				vmErr = r.throwTypeError(
					"assignment to constant variable %q", cl.fn.Upvalues[in.A].Name)
				goto onError
			}
			sp--
			cl.upvalues[in.A].set(stack[sp])
		case bytecode.OpCloseUpvalues:
			r.closeUpvaluesFrom(f, int(in.A))

		// --- Globals ------------------------------------------------------
		case bytecode.OpGetGlobal:
			name := cl.names[in.A]
			env := cl.scope()
			if f.evalVars != nil {
				// A name a direct eval declared in this function shadows
				// anything outside it, including a global of the same name.
				if p := evalVarProp(f.evalVars, name); p != nil {
					sp = pushAt(stack, sp, p.value)
					break
				}
			}
			if p := r.globalLexProp(env, name); p != nil {
				if p.value.IsUninitialized() {
					vmErr = r.throwReferenceError(
						"cannot access %q before it is initialized", r.atoms.name(name))
					goto onError
				}
				sp = pushAt(stack, sp, p.value)
				break
			}
			// A plain own data property of the environment -- which is what
			// every declared global is -- needs none of the machinery a
			// general read carries: no proxy trap, no exotic index, no
			// prototype walk. The environment's own one-entry cache is read
			// here rather than through findOwn, so that the common case is a
			// comparison rather than a call.
			if env.lastKey == name && int(env.lastIdx) < len(env.props) {
				if p := &env.props[env.lastIdx]; p.key == name &&
					p.flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 {
					sp = pushAt(stack, sp, p.value)
					break
				}
			}
			if i := env.findOwn(name); i >= 0 {
				if p := &env.props[i]; p.flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 {
					sp = pushAt(stack, sp, p.value)
					break
				}
			}
			if !r.isGlobalScope(env) {
				// Module code: its own bindings were looked for above, and the
				// script-level lexical ones sit between the module environment
				// and the global object it inherits from. An import is stored
				// as an accessor, which the general read below runs.
				if p := r.moduleLexProp(env, name); p != nil {
					if p.flags&propUninit != 0 {
						vmErr = r.throwReferenceError(
							"cannot access %q before it is initialized", r.atoms.name(name))
						goto onError
					}
					if p.flags&(propAccessor|propPrivate|propDeleted) == 0 {
						sp = pushAt(stack, sp, p.value)
						break
					}
				}
			}
			// The read comes first and the existence check only follows an
			// undefined result: an undeclared name is the rare case, and
			// asking twice for every global read is not worth paying for it.
			// A proxy on the global object's chain can tell the order, and is
			// asked as V8 asks it: whether it has the name, and then for it.
			if chainHasProxy(env) {
				has, err := r.hasPropErr(env, name)
				if err != nil {
					vmErr = err
					goto onError
				}
				if !has {
					vmErr = r.throwReferenceError("%s is not defined", r.atoms.name(name))
					goto onError
				}
			}
			v, err := r.getProp(env, name, Obj(env))
			if err != nil {
				vmErr = err
				goto onError
			}
			if v.IsUndefined() && !chainHasProxy(env) && !r.hasProp(env, name) {
				vmErr = r.throwReferenceError("%s is not defined", r.atoms.name(name))
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpGetGlobalOpt:
			// typeof on an undeclared name must not throw. A lexical binding
			// in its dead zone is declared, though, so that one still does.
			env := cl.scope()
			if f.evalVars != nil {
				if p := evalVarProp(f.evalVars, cl.names[in.A]); p != nil {
					sp = pushAt(stack, sp, p.value)
					break
				}
			}
			if p := r.globalLexProp(env, cl.names[in.A]); p != nil {
				if p.value.IsUninitialized() {
					vmErr = r.throwReferenceError("cannot access %q before it is initialized",
						r.atoms.name(cl.names[in.A]))
					goto onError
				}
				sp = pushAt(stack, sp, p.value)
				break
			}
			if !r.isGlobalScope(env) {
				if p := r.moduleLexProp(env, cl.names[in.A]); p != nil {
					if p.flags&propUninit != 0 {
						vmErr = r.throwReferenceError("cannot access %q before it is initialized",
							r.atoms.name(cl.names[in.A]))
						goto onError
					}
					if p.flags&(propAccessor|propPrivate|propDeleted) == 0 {
						sp = pushAt(stack, sp, p.value)
						break
					}
				}
			}
			if chainHasProxy(env) {
				// Asked whether it has the name first, as for a read; what is
				// not there is undefined.
				has, err := r.hasPropErr(env, cl.names[in.A])
				if err != nil {
					vmErr = err
					goto onError
				}
				if !has {
					sp = pushAt(stack, sp, Undefined)
					break
				}
			}
			v, err := r.getProp(env, cl.names[in.A], Obj(env))
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpCheckGlobalRef:
			// Whether the name resolves to anything is settled here, where the
			// reference is evaluated, and the answer is carried to the store:
			// a value that creates the global in the meantime does not make an
			// unresolvable reference resolvable, and a value that throws is
			// what the assignment reports rather than the reference.
			name := cl.names[in.A]
			env := cl.scope()
			found := true
			switch {
			case f.evalVars != nil && evalVarProp(f.evalVars, name) != nil:
			case r.globalLexProp(env, name) != nil:
			case !r.isGlobalScope(env) && r.moduleLexProp(env, name) != nil:
			case r.nodeQuirks && chainHasProxy(env):
				// V8 asks a proxy on the global object's chain nothing
				// before the store, which it counts as finding the name.
			default:
				var err error
				if found, err = r.hasPropErr(env, name); err != nil {
					vmErr = err
					goto onError
				}
			}
			sp = pushAt(stack, sp, Bool(found))
		case bytecode.OpAssertResolved:
			// The answer the reference gave sits beneath the value. A name
			// that resolved to nothing cannot be assigned to in strict mode,
			// and that is reported now -- after the value, which may have
			// thrown or created the global, and neither changes this.
			if !r.stack[sp-2].Truthy() {
				vmErr = r.throwReferenceError("%s is not defined", r.atoms.name(cl.names[in.A]))
				goto onError
			}
			r.stack[sp-2] = r.stack[sp-1]
			sp--
		case bytecode.OpSetGlobal:
			name := cl.names[in.A]
			env := cl.scope()
			if f.evalVars != nil {
				if p := evalVarProp(f.evalVars, name); p != nil {
					sp--
					p.value = stack[sp]
					break
				}
			}
			if p := r.globalLexProp(env, name); p != nil {
				switch {
				case p.value.IsUninitialized():
					vmErr = r.throwReferenceError(
						"cannot access %q before it is initialized", r.atoms.name(name))
					goto onError
				case p.flags&propWritable == 0:
					vmErr = r.throwTypeError("assignment to constant variable %q",
						r.atoms.name(name))
					goto onError
				}
				sp--
				p.value = stack[sp]
				break
			}
			if !r.isGlobalScope(env) {
				// Module code. Its own bindings and the script-level lexical
				// ones are properties rather than slots, so what the compiler
				// checks for a local is checked here instead.
				if p := r.moduleLexProp(env, name); p != nil {
					switch {
					case p.flags&propUninit != 0:
						vmErr = r.throwReferenceError(
							"cannot access %q before it is initialized", r.atoms.name(name))
						goto onError
					case p.flags&(propWritable|propAccessor) == 0:
						vmErr = r.throwTypeError("assignment to constant variable %q",
							r.atoms.name(name))
						goto onError
					}
					if p.flags&(propAccessor|propPrivate|propDeleted) == 0 {
						sp--
						p.value = stack[sp]
						break
					}
					// What is left is an imported binding, stored as an
					// accessor with no setter, which the assignment below
					// refuses for us.
				}
			}
			// Strict mode refuses to create a global by assignment, which is
			// the rule that catches a misspelled variable.
			if cl.fn.Strict && !(r.nodeQuirks && chainHasProxy(env)) {
				has, err := r.hasPropErr(env, name)
				if err != nil {
					vmErr = err
					goto onError
				}
				if !has {
					vmErr = r.throwReferenceError("%s is not defined", r.atoms.name(name))
					goto onError
				}
			}
			sp--
			if _, err := r.setProp(env, name, stack[sp], Obj(env), cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpDefineGlobalVar:
			name := cl.names[in.A]
			if f.evalVars != nil {
				// The evaluated code is inside a function, so what it declares
				// belongs to that function rather than to the global object.
				if evalVarProp(f.evalVars, name) == nil {
					f.evalVars.setOwnRaw(name, Undefined, propWritable|propConfigurable)
				}
				break
			}
			env := cl.scope()
			if env == r.global {
				if err := r.checkGlobalVarName(name); err != nil {
					vmErr = err
					goto onError
				}
			}
			// A binding eval creates is configurable, where a script's is not:
			// the evaluated code could have declared it anywhere, so nothing
			// should be able to rely on its being there.
			flags := propWritable | moduleBindingFlags(r.atoms.name(name))
			if in.B != 0 {
				flags |= propConfigurable
			}
			if proxyOf(env) != nil {
				// A context's global: the var is its sandbox's.
				if err := r.declareOnScope(env, name, Undefined, flags, false); err != nil {
					vmErr = err
					goto onError
				}
				break
			}
			if !r.hasOwnProp(env, name) {
				// A var can only be created where the object will accept a new
				// property, which a frozen global will not.
				if env == r.global && !env.IsExtensible() {
					vmErr = r.throwTypeError("cannot declare %q on a non-extensible global",
						r.atoms.name(name))
					goto onError
				}
				env.setOwnRaw(name, Undefined, flags)
			}
		case bytecode.OpDefineGlobalFunc:
			name := cl.names[in.A]
			if f.evalVars != nil {
				sp--
				f.evalVars.setOwnRaw(name, stack[sp], propWritable|propConfigurable)
				break
			}
			flags := propWritable | moduleBindingFlags(r.atoms.name(name))
			if in.B != 0 {
				flags |= propConfigurable
			}
			env := cl.scope()
			if proxyOf(env) != nil {
				sp--
				if err := r.declareOnScope(env, name, stack[sp], flags, true); err != nil {
					vmErr = err
					goto onError
				}
				break
			}
			if env == r.global {
				if err := r.checkGlobalVarName(name); err != nil {
					vmErr = err
					goto onError
				}
				if err := r.canDeclareGlobalFunc(name); err != nil {
					vmErr = err
					goto onError
				}
				if p := env.getOwn(name); p != nil &&
					p.flags&(propConfigurable|propAccessor) == 0 {
					// A property that cannot be redefined is updated in place
					// instead, so a non-configurable global keeps the
					// attributes it was given.
					sp--
					p.value = stack[sp]
					break
				}
			}
			sp--
			env.setOwnRaw(name, stack[sp], flags)
		case bytecode.OpDeclareGlobalLex:
			name := cl.names[in.A]
			flags := propConfigurable
			if in.B != 0 {
				flags |= propWritable
			}
			r.globalLex.setOwnRaw(name, uninitialized, flags)
		case bytecode.OpInitGlobalLex:
			sp--
			r.globalLex.setOwnRaw(cl.names[in.A], stack[sp],
				r.globalLex.getOwn(cl.names[in.A]).flags)
		case bytecode.OpDeclareModuleLex:
			name := cl.names[in.A]
			flags := propUninit | moduleBindingFlags(r.atoms.name(name))
			if in.B != 0 {
				flags |= propWritable
			}
			cl.scope().setOwnRaw(name, uninitialized, flags)
		case bytecode.OpInitModuleLex:
			name := cl.names[in.A]
			if p := cl.scope().getOwn(name); p != nil {
				sp--
				p.value = stack[sp]
				p.flags &^= propUninit
			} else {
				sp--
			}
		case bytecode.OpCheckGlobalVar:
			name := cl.names[in.A]
			if cl.scope() != r.global {
				break
			}
			if err := r.checkGlobalVarName(name); err != nil {
				vmErr = err
				goto onError
			}
			if in.B != 0 {
				if err := r.canDeclareGlobalFunc(name); err != nil {
					vmErr = err
					goto onError
				}
			} else if !r.hasOwnProp(r.global, name) && !r.global.IsExtensible() {
				vmErr = r.throwTypeError("cannot declare %q on a non-extensible global",
					r.atoms.name(name))
				goto onError
			}
		case bytecode.OpCheckGlobalLex:
			name := cl.names[in.A]
			if r.globalLex.getOwn(name) != nil {
				vmErr = r.throwError(errSyntax, "%q has already been declared",
					r.atoms.name(name))
				goto onError
			}
			// A lexical binding shadows the global object's property of the
			// same name for good, so one that cannot be deleted may not be
			// shadowed: `let undefined` would make undefined unreachable. A
			// var or function a script declared is such a property, which is
			// how the collision with one of those is caught; one eval declared
			// is configurable and so is not a collision at all.
			if p := r.global.getOwn(name); p != nil && p.flags&propConfigurable == 0 {
				vmErr = r.throwError(errSyntax,
					"%q is already a property of the global object that cannot be removed",
					r.atoms.name(name))
				goto onError
			}

		// --- Properties ---------------------------------------------------
		case bytecode.OpGetProp:
			sp--
			v, err := r.getValueProp(stack[sp], cl.names[in.A])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpGetPropThis:
			// Leave the receiver beneath the value so a method call can use it.
			recv := stack[sp-1]
			v, err := r.getValueProp(recv, cl.names[in.A])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpSetProp:
			sp--
			val := stack[sp]
			sp--
			obj := stack[sp]
			if err := r.setValueProp(obj, cl.names[in.A], val, cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpGetIndex:
			sp--
			key := stack[sp]
			sp--
			obj := stack[sp]
			v, err := r.getIndexed(obj, key)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpGetIndexThis:
			sp--
			key := stack[sp]
			recv := stack[sp-1]
			v, err := r.getIndexed(recv, key)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpSetIndex:
			sp--
			val := stack[sp]
			sp--
			key := stack[sp]
			sp--
			obj := stack[sp]
			// An element already in an array's dense storage is a writable
			// data property -- one with any other attributes has left it --
			// so a store to it is the store. A hole is not: a setter up the
			// prototype chain would be asked, so it takes the long way, as a
			// mapped arguments object's indices do.
			if obj.IsObject() && key.IsNumber() {
				o := obj.Object()
				if i := uint32(key.Number()); float64(i) == key.Number() &&
					uint(i) < uint(len(o.elems)) && o.flags&objMappedArguments == 0 && !isHole(o.elems[i]) {
					o.elems[i] = val
					break
				}
			}
			if obj.IsNullish() {
				vmErr = r.throwTypeError("cannot set property of %s", r.describe(obj))
				goto onError
			}
			k, err := r.toPropertyKey(key)
			if err != nil {
				vmErr = err
				goto onError
			}
			if err := r.setValueProp(obj, k, val, cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpDeleteVar:
			// Deleting a binding only succeeds for a configurable global
			// property, which is why a var declaration cannot be deleted.
			name := cl.names[in.A]
			// What a direct eval declared can be deleted again, unlike a var
			// the source named: the evaluated code could have declared it
			// anywhere, so nothing may rely on it being there.
			if deleteEvalVar(f.evalVars, name) {
				sp = pushAt(stack, sp, True)
				break
			}
			if r.globalLexProp(cl.scope(), name) != nil {
				// A lexical binding is not a property and cannot be removed.
				sp = pushAt(stack, sp, False)
				break
			}
			ok, err := r.deleteProp(cl.scope(), name, false)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, Bool(ok))

		case bytecode.OpDeleteProp:
			sp--
			key := stack[sp]
			sp--
			obj := stack[sp]
			if obj.IsNullish() {
				// There is nothing to delete from, which is a TypeError before
				// the key is even converted.
				vmErr = r.throwTypeError("cannot delete a property of %s", r.describe(obj))
				goto onError
			}
			n, err := r.toPropertyName(key)
			if err != nil {
				vmErr = err
				goto onError
			}
			if !obj.IsObject() {
				sp = pushAt(stack, sp, True)
				break
			}
			k, has := r.keyFor(obj.Object(), n)
			if !has {
				// Deleting what is not there succeeds.
				sp = pushAt(stack, sp, True)
				break
			}
			ok, err := r.deleteProp(obj.Object(), k, cl.fn.Strict)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, Bool(ok))
		case bytecode.OpGetLength:
			sp--
			v, err := r.getValueProp(stack[sp], atomLength)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpDefineField:
			sp--
			val := stack[sp]
			obj := stack[sp-1]
			if obj.IsObject() {
				if err := r.defineOwnProp(obj.Object(), cl.names[in.A], val, propDefault); err != nil {
					vmErr = err
					goto onError
				}
			}
		case bytecode.OpDefineIndex:
			sp--
			val := stack[sp]
			sp--
			key := stack[sp]
			obj := stack[sp-1]
			k, err := r.toPropertyKey(key)
			if err != nil {
				vmErr = err
				goto onError
			}
			if obj.IsObject() {
				// A checked define: a member named after a property the object
				// will not part with -- a class's prototype, say -- is a
				// TypeError rather than something to overwrite.
				if err := r.createDataProperty(obj.Object(), k, val,
					memberFlags(in.A)); err != nil {
					vmErr = err
					goto onError
				}
			}
		case bytecode.OpDefineGetter, bytecode.OpDefineSetter:
			sp--
			fnVal := stack[sp]
			obj := stack[sp-1]
			if obj.IsObject() && fnVal.IsObject() {
				r.defineHalfAccessor(obj.Object(), cl.names[in.A], fnVal.Object(),
					in.Op == bytecode.OpDefineGetter, memberFlags(in.B))
			}
		case bytecode.OpSetFuncName:
			if fnVal := stack[sp-1]; fnVal.IsObject() {
				if fd := fnVal.Object().fn(); fd != nil {
					fd.name = functionNameFromKey(stack[sp-2], in.A)
				}
			}
		case bytecode.OpDefineGetterIndex, bytecode.OpDefineSetterIndex:
			sp--
			fnVal := stack[sp]
			sp--
			key := stack[sp]
			obj := stack[sp-1]
			k, err := r.toPropertyKey(key)
			if err != nil {
				vmErr = err
				goto onError
			}
			if obj.IsObject() && fnVal.IsObject() {
				if err := r.defineHalfAccessorChecked(obj.Object(), k, fnVal.Object(),
					in.Op == bytecode.OpDefineGetterIndex, memberFlags(in.A)); err != nil {
					vmErr = err
					goto onError
				}
			}
		case bytecode.OpSetProtoOf:
			sp--
			val := stack[sp]
			obj := stack[sp-1]
			if obj.IsObject() {
				switch {
				case val.IsObject():
					obj.Object().proto = val.Object()
				case val.IsNull():
					obj.Object().proto = nil
				}
			}

		// --- Arithmetic ---------------------------------------------------
		case bytecode.OpAdd:
			// The two operands are read where they lie and the result written
			// over the first of them: the overwhelmingly common case is two
			// numbers, and a pop and a push for each would touch the same
			// slots twice.
			a, b := r.stack[sp-2], r.stack[sp-1]
			sp--
			if a.IsNumber() && b.IsNumber() {
				r.stack[sp-1] = Float(a.Number() + b.Number())
				break
			}
			v, err := r.add(a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			r.stack[sp-1] = v
		case bytecode.OpSub:
			a, b := r.stack[sp-2], r.stack[sp-1]
			sp--
			if a.IsNumber() && b.IsNumber() {
				r.stack[sp-1] = Float(a.Number() - b.Number())
				break
			}
			v, err := r.arith(bytecode.OpSub, a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			r.stack[sp-1] = v
		case bytecode.OpMul:
			a, b := r.stack[sp-2], r.stack[sp-1]
			sp--
			if a.IsNumber() && b.IsNumber() {
				r.stack[sp-1] = Float(a.Number() * b.Number())
				break
			}
			v, err := r.arith(bytecode.OpMul, a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			r.stack[sp-1] = v
		case bytecode.OpDiv:
			a, b := r.stack[sp-2], r.stack[sp-1]
			sp--
			if a.IsNumber() && b.IsNumber() {
				r.stack[sp-1] = Float(a.Number() / b.Number())
				break
			}
			v, err := r.arith(bytecode.OpDiv, a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			r.stack[sp-1] = v
		case bytecode.OpMod, bytecode.OpPow:
			a, b := r.stack[sp-2], r.stack[sp-1]
			sp--
			if a.IsNumber() && b.IsNumber() {
				r.stack[sp-1] = Float(numericOp(in.Op, a.Number(), b.Number()))
				break
			}
			v, err := r.arith(in.Op, a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			r.stack[sp-1] = v
		case bytecode.OpNeg:
			sp--
			a := stack[sp]
			if a.IsNumber() {
				sp = pushAt(stack, sp, Float(-a.Number()))
				break
			}
			v, err := r.negate(a)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpPos:
			sp--
			n, err := r.toNumber(stack[sp])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, Float(n))
		case bytecode.OpUpdateLocal:
			// ++ or -- on a local whose value is read, which is what the
			// separate instructions did: coerce, step, store, and push the
			// old value or the new.
			old := f.locals[in.A]
			var n, next Value
			if old.IsNumber() {
				n = old
				if in.B&bytecode.UpdateDec == 0 {
					next = Float(old.Number() + 1)
				} else {
					next = Float(old.Number() - 1)
				}
			} else {
				var err error
				if n, err = r.toNumeric(old); err != nil {
					vmErr = err
					goto onError
				}
				delta := 1.0
				if in.B&bytecode.UpdateDec != 0 {
					delta = -1
				}
				if n.IsBigInt() {
					if next, err = r.arith(bytecode.OpAdd, n, Big(NewBigInt(int64(delta)))); err != nil {
						vmErr = err
						goto onError
					}
				} else {
					next = Float(n.Number() + delta)
				}
			}
			f.locals[in.A] = next
			if in.B&bytecode.UpdatePostfix != 0 {
				sp = pushAt(stack, sp, n)
			} else {
				sp = pushAt(stack, sp, next)
			}
		case bytecode.OpIncLocal, bytecode.OpDecLocal:
			// `i++` as a statement, which is most of the increments a program
			// does. A number is the case worth having the instruction for; the
			// rest is what the separate instructions would have done.
			slot := &f.locals[in.A]
			if slot.IsNumber() {
				if in.Op == bytecode.OpIncLocal {
					*slot = Float(slot.Number() + 1)
				} else {
					*slot = Float(slot.Number() - 1)
				}
				break
			}
			n, err := r.toNumeric(*slot)
			if err != nil {
				vmErr = err
				goto onError
			}
			if n.IsBigInt() {
				delta := int64(1)
				if in.Op == bytecode.OpDecLocal {
					delta = -1
				}
				v, err := r.arith(bytecode.OpAdd, n, Big(NewBigInt(delta)))
				if err != nil {
					vmErr = err
					goto onError
				}
				f.locals[in.A] = v
				break
			}
			if in.Op == bytecode.OpIncLocal {
				f.locals[in.A] = Float(n.Number() + 1)
			} else {
				f.locals[in.A] = Float(n.Number() - 1)
			}

		case bytecode.OpInc, bytecode.OpDec:
			sp--
			a := stack[sp]
			if a.IsNumber() {
				if in.Op == bytecode.OpInc {
					sp = pushAt(stack, sp, Float(a.Number()+1))
				} else {
					sp = pushAt(stack, sp, Float(a.Number()-1))
				}
				break
			}
			delta := 1.0
			if in.Op == bytecode.OpDec {
				delta = -1
			}
			n, err := r.toNumeric(a)
			if err != nil {
				vmErr = err
				goto onError
			}
			if n.IsBigInt() {
				v, err := r.arith(bytecode.OpAdd, n, Big(NewBigInt(int64(delta))))
				if err != nil {
					vmErr = err
					goto onError
				}
				sp = pushAt(stack, sp, v)
				break
			}
			sp = pushAt(stack, sp, Float(n.Number()+delta))

		// --- Bitwise ------------------------------------------------------
		case bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr:
			b, a := stack[sp-1], stack[sp-2]
			sp -= 2
			if a.IsNumber() && b.IsNumber() {
				// Worked out here rather than in a call, which in Go costs
				// the loop every register it had.
				x := jsnum.ToInt32(a.Number())
				switch in.Op {
				case bytecode.OpBitAnd:
					sp = pushAt(stack, sp, Int32(x&jsnum.ToInt32(b.Number())))
				case bytecode.OpBitOr:
					sp = pushAt(stack, sp, Int32(x|jsnum.ToInt32(b.Number())))
				case bytecode.OpBitXor:
					sp = pushAt(stack, sp, Int32(x^jsnum.ToInt32(b.Number())))
				case bytecode.OpShl:
					// Only the low five bits of the shift count are used.
					sp = pushAt(stack, sp, Int32(x<<(jsnum.ToUint32(b.Number())&31)))
				case bytecode.OpShr:
					sp = pushAt(stack, sp, Int32(x>>(jsnum.ToUint32(b.Number())&31)))
				default:
					sp = pushAt(stack, sp, Uint32(uint32(x)>>(jsnum.ToUint32(b.Number())&31)))
				}
				break
			}
			v, err := r.bitwise(in.Op, a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpBitNot:
			sp--
			v := stack[sp]
			if !v.IsNumber() {
				n, err := r.toNumeric(v)
				if err != nil {
					vmErr = err
					goto onError
				}
				if n.IsBigInt() {
					// A BigInt has no width, so the complement is simply
					// -(x+1) -- which is what Not computes.
					out := &BigInt{}
					out.V.Not(&n.BigInt().V)
					sp = pushAt(stack, sp, Big(out))
					break
				}
				v = n
			}
			x, err := r.toInt32(v)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, Int32(^x))

		// --- Comparison ---------------------------------------------------
		case bytecode.OpEq, bytecode.OpNe:
			a, b := r.stack[sp-2], r.stack[sp-1]
			sp--
			eq, err := r.looseEquals(a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			r.stack[sp-1] = Bool(eq == (in.Op == bytecode.OpEq))
		case bytecode.OpStrictEq:
			a, b := r.stack[sp-2], r.stack[sp-1]
			sp--
			r.stack[sp-1] = Bool(a.StrictEquals(b))
		case bytecode.OpStrictNe:
			a, b := r.stack[sp-2], r.stack[sp-1]
			sp--
			r.stack[sp-1] = Bool(!a.StrictEquals(b))
		case bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe:
			a, b := r.stack[sp-2], r.stack[sp-1]
			sp--
			// Two numbers are compared directly, which also gets the NaN
			// behaviour right without going through cmpUndefined.
			if a.IsNumber() && b.IsNumber() {
				r.stack[sp-1] = Bool(compareFloats(in.Op, a.Number(), b.Number()))
				break
			}
			c, err := r.compare(a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			r.stack[sp-1] = Bool(relationalResult(in.Op, c))
		case bytecode.OpIn:
			obj, key := stack[sp-1], stack[sp-2]
			sp -= 2
			if !obj.IsObject() {
				vmErr = r.throwTypeError("the right operand of \"in\" must be an object")
				goto onError
			}
			n, err := r.toPropertyName(key)
			if err != nil {
				vmErr = err
				goto onError
			}
			k, known := r.keyFor(obj.Object(), n)
			if !known {
				sp = pushAt(stack, sp, False)
				break
			}
			has, err := r.hasPropErr(obj.Object(), k)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, Bool(has))
		case bytecode.OpInstanceOf:
			ctor, obj := stack[sp-1], stack[sp-2]
			sp -= 2
			ok, err := r.instanceOf(obj, ctor)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, Bool(ok))
		case bytecode.OpNot:
			stack[sp-1] = Bool(!stack[sp-1].Truthy())
		case bytecode.OpTypeOf:
			stack[sp-1] = Str(NewString(stack[sp-1].TypeOf()))
		case bytecode.OpIsNullish:
			sp = pushAt(stack, sp, Bool(stack[sp-1].IsNullish()))

		// --- Control flow -------------------------------------------------
		case bytecode.OpJump:
			if in.A < pc {
				if budget--; budget <= 0 {
					pc = in.A
					goto interrupted
				}
			}
			pc = in.A
		case bytecode.OpJumpIfFalse:
			sp--
			if !stack[sp].Truthy() {
				if in.A < pc {
					if budget--; budget <= 0 {
						pc = in.A
						goto interrupted
					}
				}
				pc = in.A
			}
		case bytecode.OpJumpIfTrue:
			sp--
			if stack[sp].Truthy() {
				if in.A < pc {
					if budget--; budget <= 0 {
						pc = in.A
						goto interrupted
					}
				}
				pc = in.A
			}
		case bytecode.OpJumpIfFalseKeep:
			if !stack[sp-1].Truthy() {
				if in.A < pc {
					if budget--; budget <= 0 {
						pc = in.A
						goto interrupted
					}
				}
				pc = in.A
			} else {
				sp--
			}
		case bytecode.OpJumpIfTrueKeep:
			if stack[sp-1].Truthy() {
				if in.A < pc {
					if budget--; budget <= 0 {
						pc = in.A
						goto interrupted
					}
				}
				pc = in.A
			} else {
				sp--
			}
		case bytecode.OpJumpIfCmpFalse:
			// A loop's test: the comparison and the branch that reads it,
			// without the boolean in between.
			a, b := r.stack[sp-2], r.stack[sp-1]
			sp -= 2
			cmp := bytecode.Op(in.B)
			var res bool
			switch {
			case a.IsNumber() && b.IsNumber() &&
				cmp != bytecode.OpStrictEq && cmp != bytecode.OpStrictNe &&
				cmp != bytecode.OpEq && cmp != bytecode.OpNe:
				res = compareFloats(cmp, a.Number(), b.Number())
			case cmp == bytecode.OpStrictEq:
				res = a.StrictEquals(b)
			case cmp == bytecode.OpStrictNe:
				res = !a.StrictEquals(b)
			case cmp == bytecode.OpEq, cmp == bytecode.OpNe:
				eq, err := r.looseEquals(a, b)
				if err != nil {
					vmErr = err
					goto onError
				}
				res = eq == (cmp == bytecode.OpEq)
			default:
				c, err := r.compare(a, b)
				if err != nil {
					vmErr = err
					goto onError
				}
				res = relationalResult(cmp, c)
			}
			if !res {
				if in.A < pc {
					if budget--; budget <= 0 {
						pc = in.A
						goto interrupted
					}
				}
				pc = in.A
			}

		case bytecode.OpJumpIfNullish:
			// Used by optional chaining, which keeps the value on both paths:
			// as the chain's result when short-circuiting, and as the receiver
			// of the next link otherwise.
			if stack[sp-1].IsNullish() {
				if in.A < pc {
					if budget--; budget <= 0 {
						pc = in.A
						goto interrupted
					}
				}
				pc = in.A
			}
		case bytecode.OpJumpIfNotNullish:
			if !stack[sp-1].IsNullish() {
				if in.A < pc {
					if budget--; budget <= 0 {
						pc = in.A
						goto interrupted
					}
				}
				pc = in.A
			} else {
				sp--
			}

		// --- Calls --------------------------------------------------------
		case bytecode.OpCall:
			argc := int(in.A)
			args := r.stack[sp-argc : sp]
			callee := r.stack[sp-argc-1]
			sp -= argc + 1
			v, err := r.call(callee, Undefined, args)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpTailCall, bytecode.OpTailCallMethod:
			argc := int(in.A)
			args := r.stack[sp-argc : sp]
			callee := r.stack[sp-argc-1]
			this := Undefined
			sp -= argc + 1
			if in.Op == bytecode.OpTailCallMethod {
				this = r.stack[sp-1]
				sp--
			}
			// The frame can be given up only when it is an ordinary call's:
			// a constructor's result is checked after the body returns, and
			// an arrow in a derived constructor shares its this. Anything
			// else makes an ordinary call, and the return after it the rest.
			if r.frameDepth >= tailCallDepth && f.newTarget.IsUndefined() &&
				f.thisRef == nil && len(f.handlers) == 0 && f.savedSP == 0 {
				// The arguments are copied: they live in this frame's window,
				// which the callee's frame is about to reuse.
				r.pendingTail = tailCall{callee: callee, this: this, args: append([]Value(nil), args...)}
				return Undefined, errTailCall
			}
			v, err := r.call(callee, this, args)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpDirectEval, bytecode.OpTailDirectEval:
			var args []Value
			var callee, recv Value
			if in.B == bytecode.DirectEvalSpread {
				// The arguments were gathered into an array, which a spread
				// element forces. A spread does not make the call any less
				// direct: what decides is the name the callee was written as.
				sp--
				list := stack[sp]
				sp--
				callee = stack[sp]
				sp--
				recv = stack[sp]
				if list.IsObject() {
					args = list.Object().elems
				}
			} else {
				argc := int(in.B)
				args = r.stack[sp-argc : sp]
				callee = r.stack[sp-argc-1]
				recv = r.stack[sp-argc-2]
				sp -= argc + 2
			}
			src := arg(args, 0)
			switch {
			case !callee.IsObject() || callee.Object() != r.evalFn:
				// The name resolved to something other than the intrinsic, so
				// this is an ordinary call after all -- with the receiver the
				// resolution found, which a `with` object's own eval needs --
				// and in tail position a tail call.
				if in.Op == bytecode.OpTailDirectEval && r.frameDepth >= tailCallDepth &&
					f.newTarget.IsUndefined() && f.thisRef == nil && len(f.handlers) == 0 &&
					f.savedSP == 0 {
					r.pendingTail = tailCall{callee: callee, this: recv, args: append([]Value(nil), args...)}
					return Undefined, errTailCall
				}
				v, err := r.call(callee, recv, args)
				if err != nil {
					vmErr = err
					goto onError
				}
				sp = pushAt(stack, sp, v)
			case !src.IsString():
				// eval returns anything that is not a string unchanged, which
				// is what makes eval(42) safe.
				sp = pushAt(stack, sp, src)
			default:
				v, err := r.evalDirect(f, cl.fn.EvalScopes[in.A], src.String().Go())
				if err != nil {
					vmErr = err
					goto onError
				}
				sp = pushAt(stack, sp, v)
			}
		case bytecode.OpCallMethod:
			argc := int(in.A)
			args := r.stack[sp-argc : sp]
			callee := r.stack[sp-argc-1]
			this := r.stack[sp-argc-2]
			sp -= argc + 2
			v, err := r.call(callee, this, args)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpNew:
			argc := int(in.A)
			args := r.stack[sp-argc : sp]
			callee := r.stack[sp-argc-1]
			sp -= argc + 1
			v, err := r.construct(callee, args)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpReturn:
			sp--
			v := stack[sp]
			// Anything still on the operand stack at a return is a for-of
			// cursor being abandoned, so its iterator is closed before the
			// frame goes away. An ordinary return leaves the stack empty, and
			// the comparison keeps that path free of a call.
			if sp > f.base {
				if err := r.closeIteratorsReturning(f.base, sp); err != nil {
					vmErr = err
					goto onError
				}
			}
			if f.thisRef != nil {
				v, vmErr = r.derivedResult(f, cl, v)
				if vmErr != nil {
					goto onError
				}
			}
			return v, nil
		case bytecode.OpReturnUndef:
			if sp > f.base {
				if err := r.closeIteratorsReturning(f.base, sp); err != nil {
					vmErr = err
					goto onError
				}
			}
			if f.thisRef != nil {
				v, err := r.derivedResult(f, cl, Undefined)
				if err != nil {
					vmErr = err
					goto onError
				}
				return v, nil
			}
			return Undefined, nil

		// --- Construction -------------------------------------------------
		case bytecode.OpClosure:
			sp = pushAt(stack, sp, Obj(r.makeClosure(f, cl.consts[in.A])))
		case bytecode.OpNewObject:
			// The literal says how many properties it will write, so the table
			// is sized for them -- and, when there are few enough, carried in
			// the object's own allocation.
			sp = pushAt(stack, sp, Obj(newLiteralObject(r.proto.object, ClassObject, int(in.A))))
		case bytecode.OpNewArray:
			n := int(in.A)
			arr := r.newArrayFrom(r.stack[sp-n : sp])
			sp -= n
			sp = pushAt(stack, sp, Obj(arr))
		case bytecode.OpArrayPush:
			sp--
			val := stack[sp]
			arr := stack[sp-1]
			if arr.IsObject() {
				o := arr.Object()
				o.elems = append(o.elems, val)
			}
		case bytecode.OpConcat:
			n := int(in.A)
			parts := r.stack[sp-n : sp]
			// Every part is converted before any of them is joined: a toString
			// may run user code, and when it runs is fixed. A number is left
			// as it is, having no conversion anything can observe and no need
			// of a string of its own.
			for i := range parts {
				if parts[i].IsString() || parts[i].IsNumber() {
					continue
				}
				s, err := r.toString(parts[i])
				if err != nil {
					vmErr = err
					goto onError
				}
				parts[i] = Str(s)
			}
			// The strings alone may be too long, which is known before
			// anything is written out.
			total := 0
			for _, p := range parts {
				if p.IsString() && total <= maxStringLength {
					total += p.String().length
				}
			}
			if total > maxStringLength {
				vmErr = r.throwStringLength()
				goto onError
			}
			out := joinValues(parts)
			if out.length > maxStringLength {
				vmErr = r.throwStringLength()
				goto onError
			}
			sp -= n
			sp = pushAt(stack, sp, Str(out))
		case bytecode.OpTemplateObject:
			sp = pushAt(stack, sp, Obj(r.templateObject(cl.fn, int(in.A))))
		case bytecode.OpSetName:
			if v := stack[sp-1]; v.IsObject() {
				if fd := v.Object().fn(); fd != nil && fd.name == "" {
					fd.name = r.atoms.name(cl.names[in.A])
				}
			}

		// --- Conversions --------------------------------------------------
		// --- `with` ------------------------------------------------------
		case bytecode.OpWithPush:
			sp--
			o, err := r.withObject(stack[sp])
			if err != nil {
				vmErr = err
				goto onError
			}
			// The slice is capped so that this append copies rather than
			// writing into an array a closure made under an earlier `with` is
			// still holding: two `with` statements one after the other would
			// otherwise share a slot, and the first one's closures would find
			// the second one's object in it.
			f.withScopes = append(f.withScopes[:len(f.withScopes):len(f.withScopes)], o)
		case bytecode.OpWithPop:
			f.withScopes = f.withScopes[:len(f.withScopes)-1]
		case bytecode.OpWithGet, bytecode.OpWithGetThis, bytecode.OpWithTypeof:
			name := cl.names[in.A&bytecode.WithNameMask]
			o, found, err := r.withFound(f.withScopes, name,
				int(in.A>>bytecode.WithLimitShift))
			if err != nil {
				vmErr = err
				goto onError
			}
			if !found {
				break
			}
			v, err := r.withRead(o, name, cl.fn.Strict)
			if err != nil {
				vmErr = err
				goto onError
			}
			switch in.Op {
			case bytecode.OpWithGetThis:
				// A call through a `with` object has that object as its
				// receiver, which is the whole difference between
				// `with (o) f()` and `f()`. The bindings a direct eval
				// declared are not an object a script can see, so a call
				// through one gets no receiver at all.
				if o.flags&objEvalVars != 0 {
					sp = pushAt(stack, sp, Undefined)
				} else {
					sp = pushAt(stack, sp, Obj(o))
				}
				sp = pushAt(stack, sp, v)
			case bytecode.OpWithTypeof:
				sp = pushAt(stack, sp, Str(NewString(v.TypeOf())))
			default:
				sp = pushAt(stack, sp, v)
			}
			if in.B < pc {
				if budget--; budget <= 0 {
					pc = in.B
					goto interrupted
				}
			}
			pc = in.B
		case bytecode.OpWithGetUnder:
			name := cl.names[in.A&bytecode.WithNameMask]
			o, found, err := r.withFound(f.withScopes, name,
				int(in.A>>bytecode.WithLimitShift))
			if err != nil {
				vmErr = err
				goto onError
			}
			if !found {
				break
			}
			v, err := r.withRead(o, name, cl.fn.Strict)
			if err != nil {
				vmErr = err
				goto onError
			}
			// The placeholder beneath becomes the object the name resolved to,
			// so that the write goes back where the read came from.
			r.stack[sp-1] = Obj(o)
			sp = pushAt(stack, sp, v)
			if in.B < pc {
				if budget--; budget <= 0 {
					pc = in.B
					goto interrupted
				}
			}
			pc = in.B
		case bytecode.OpWithResolve:
			name := cl.names[in.A&bytecode.WithNameMask]
			o, found, err := r.withFound(f.withScopes, name,
				int(in.A>>bytecode.WithLimitShift))
			if err != nil {
				vmErr = err
				goto onError
			}
			if found {
				// The placeholder becomes the object the name resolved to; the
				// value is evaluated next and written back to it.
				r.stack[sp-1] = Obj(o)
			}
		case bytecode.OpWithPutUnder:
			base := r.stack[sp-2]
			if !base.IsObject() {
				// The read came from a binding rather than from a `with`
				// object, so the static store follows.
				break
			}
			name := cl.names[in.A]
			v := stack[sp-1]
			// The write asks again whether the binding is there: a strict
			// reference to one that has since gone is a ReferenceError rather
			// than a property being created.
			if err := r.withWrite(base.Object(), name, v, cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
			r.stack[sp-2] = v
			sp--
			if in.B < pc {
				if budget--; budget <= 0 {
					pc = in.B
					goto interrupted
				}
			}
			pc = in.B
		case bytecode.OpWithSet:
			name := cl.names[in.A&bytecode.WithNameMask]
			o, found, err := r.withFound(f.withScopes, name,
				int(in.A>>bytecode.WithLimitShift))
			if err != nil {
				vmErr = err
				goto onError
			}
			if !found {
				break
			}
			if err := r.withWrite(o, name, stack[sp-1], cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
			if in.B < pc {
				if budget--; budget <= 0 {
					pc = in.B
					goto interrupted
				}
			}
			pc = in.B
		case bytecode.OpWithDelete:
			name := cl.names[in.A&bytecode.WithNameMask]
			o, found, err := r.withFound(f.withScopes, name,
				int(in.A>>bytecode.WithLimitShift))
			if err != nil {
				vmErr = err
				goto onError
			}
			if !found {
				break
			}
			ok, err := r.deleteProp(o, name, cl.fn.Strict)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, Bool(ok))
			if in.B < pc {
				if budget--; budget <= 0 {
					pc = in.B
					goto interrupted
				}
			}
			pc = in.B

		case bytecode.OpCheckCoercible:
			if v := stack[sp-1]; v.IsNullish() {
				vmErr = r.throwTypeError("cannot destructure %s", r.describe(v))
				goto onError
			}
		case bytecode.OpToObject:
			sp--
			o, err := r.toObject(stack[sp])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, Obj(o))
		case bytecode.OpToNumber:
			sp--
			n, err := r.toNumber(stack[sp])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, Float(n))
		case bytecode.OpToNumeric:
			sp--
			n, err := r.toNumeric(stack[sp])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, n)
		case bytecode.OpToString:
			sp--
			s, err := r.toString(stack[sp])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, Str(s))
		case bytecode.OpToPropertyKeyOfBase:
			// The object the key will be looked up on sits beneath it, and is
			// checked first: a key whose toString throws must not run at all
			// when there is nothing to look it up on.
			if base := stack[sp-2]; base.IsNullish() {
				vmErr = r.throwTypeError("cannot read property of %s", r.describe(base))
				goto onError
			}
			fallthrough
		case bytecode.OpToPropertyKey:
			// A string or symbol is already a property key and must be left
			// alone; stringifying a symbol here would turn a computed symbol
			// key into an ordinary named one.
			v := stack[sp-1]
			if v.IsString() || v.IsSymbol() {
				break
			}
			sp--
			k, err := r.toPropertyKey(stack[sp])
			if err != nil {
				vmErr = err
				goto onError
			}
			// The conversion may still produce a symbol, which an object's
			// Symbol.toPrimitive can return: the key keeps whichever kind it
			// turned out to be.
			sp = pushAt(stack, sp, r.keyToValue(k))

		// --- Exceptions ---------------------------------------------------
		case bytecode.OpThrow:
			sp--
			vmErr = r.throw(stack[sp])
			goto onError
		case bytecode.OpThrowTypeError:
			vmErr = r.throwTypeError("%s", r.atoms.name(cl.names[in.A]))
			goto onError
		case bytecode.OpThrowReferenceError:
			vmErr = r.throwError(errReference, "%s", r.atoms.name(cl.names[in.A]))
			goto onError

		case bytecode.OpNewDisposeCapability:
			o := newObject(nil, ClassObject)
			o.data = &disposeCapability{}
			sp = pushAt(stack, sp, Obj(o))
		case bytecode.OpAddDisposable:
			sp--
			c := stack[sp].Object().data.(*disposeCapability)
			if err := r.addDisposableResource(c, stack[sp-1], in.A == 1); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpDispose:
			sp--
			c := stack[sp].Object().data.(*disposeCapability)
			// A finally clause knows from its record whether an exception is
			// what is leaving the scope, which the disposal has to fold into
			// whatever it throws itself.
			var completion error
			if in.A&bytecode.DisposeRecord != 0 &&
				completionKind(stack[sp-1].Number()) == completionThrow {
				completion = r.throw(stack[sp-2])
			}
			// A scope whose `await using` was never reached disposes of what
			// it has without awaiting anything, and says so with undefined.
			if in.A&bytecode.DisposeAsync != 0 && c.hasAsync() {
				p := r.newPromise()
				r.disposeResourcesAsync(c, completion, func(err error) {
					// The exception in flight is still what the record says;
					// only a new one rejects.
					if err == nil || err == completion {
						r.resolvePromise(p, Undefined)
						return
					}
					r.rejectPromise(p, thrownValue(err))
				})
				sp = pushAt(stack, sp, Obj(p))
				break
			}
			if err := r.disposeResources(c, completion); err != nil && err != completion {
				vmErr = err
				goto onError
			}
			if in.A&bytecode.DisposeAsync != 0 {
				sp = pushAt(stack, sp, Undefined)
			}
		case bytecode.OpPushCatch:
			f.handlers = append(f.handlers, handler{pc: in.A, stackDepth: sp - f.base})
		case bytecode.OpPushFinally:
			f.handlers = append(f.handlers,
				handler{pc: in.A, stackDepth: sp - f.base, isFinally: true})
		case bytecode.OpPopCatch:
			f.handlers = f.handlers[:len(f.handlers)-1]
		case bytecode.OpRethrow:
			// The finally block's completion record is on the stack: a marker
			// saying whether the protected block fell through, returned or
			// threw, and the associated value.
			sp--
			kind := stack[sp]
			sp--
			val := stack[sp]
			switch completionKind(kind.Number()) {
			case completionThrow:
				vmErr = r.throw(val)
				goto onError
			case completionReturn:
				// An enclosing finally has its own claim on the return: the
				// value keeps unwinding until no finally is left.
				var ok bool
				if sp, ok = r.unwindToFinally(f, sp, val); ok {
					pc = f.pc
					continue
				}
				if err := r.closeIteratorsReturning(f.base, sp); err != nil {
					vmErr = err
					goto onError
				}
				if f.thisRef != nil {
					val, vmErr = r.derivedResult(f, cl, val)
					if vmErr != nil {
						goto onError
					}
				}
				return val, nil
			}

		// --- Parameters and arguments -------------------------------------
		case bytecode.OpRestParam:
			start := int(in.A)
			var rest []Value
			if start < len(f.args) {
				rest = f.args[start:]
			}
			sp = pushAt(stack, sp, Obj(r.newArrayFrom(rest)))
		case bytecode.OpGetArguments:
			sp = pushAt(stack, sp, Obj(r.newArgumentsObject(f)))
		case bytecode.OpArrayRest:
			sp--
			src := stack[sp]
			start := int(in.A)
			var rest []Value
			if src.IsObject() {
				if el := src.Object().elems; start < len(el) {
					rest = el[start:]
				}
			}
			sp = pushAt(stack, sp, Obj(r.newArrayFrom(rest)))
		case bytecode.OpObjectRest:
			// The excluded keys sit above the source on the stack.
			n := int(in.A)
			excluded := r.stack[sp-n : sp]
			src := r.stack[sp-n-1]
			sp -= n + 1
			rest, err := r.objectRest(src, excluded)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, rest)

		// --- Calls with spread --------------------------------------------
		case bytecode.OpCallSpread, bytecode.OpNewSpread:
			sp--
			argsVal := stack[sp]
			sp--
			callee := stack[sp]
			var callArgs []Value
			if argsVal.IsObject() {
				callArgs = argsVal.Object().elems
			}
			var v Value
			var err error
			if in.Op == bytecode.OpNewSpread {
				v, err = r.construct(callee, callArgs)
			} else {
				// A spread call always has an explicit receiver slot beneath
				// the callee, which the compiler pushes as undefined for a
				// plain call.
				sp--
				v, err = r.call(callee, stack[sp], callArgs)
			}
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)

		// --- Iteration ----------------------------------------------------
		case bytecode.OpForInStart:
			sp--
			cur, err := r.startForIn(stack[sp])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, cur)
		case bytecode.OpForAwaitOfStart:
			sp--
			cur, err := r.startForAwaitOf(stack[sp])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, cur)
		case bytecode.OpForOfStart:
			sp--
			cur, err := r.startForOf(stack[sp])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, cur)
		case bytecode.OpGetPropUnder:
			v, err := r.getValueProp(stack[sp-1-int(in.B)], cl.names[in.A])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpGetIndexUnder:
			v, err := r.getIndexed(stack[sp-2-int(in.A)], stack[sp-1-int(in.A)])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpIterStep:
			// The cursor sits A slots below the top: a target's reference may
			// have been pushed above it, and is evaluated before the value it
			// receives is asked for.
			v, err := r.iterStep(stack[sp-1-int(in.A)])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpIterRest:
			v, err := r.iterRest(stack[sp-1-int(in.A)])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpIterCloseNormal:
			sp--
			if err := r.iterCloseNormal(stack[sp]); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpIterNextOrJump:
			// The cursor stays on the stack so that the loop can close it.
			v, ok, err := r.iterNext(stack[sp-1])
			if err != nil {
				vmErr = err
				goto onError
			}
			if !ok {
				if in.A < pc {
					if budget--; budget <= 0 {
						pc = in.A
						goto interrupted
					}
				}
				pc = in.A
				break
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpAsyncIterNext:
			v, err := r.asyncIterNext(stack[sp-1])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpEndParams:
			// Ordinarily nothing: the body simply continues. When the frame is
			// running only the prologue, this is where it stops.
			if f.paramsOnly {
				// pc already points past this instruction, which is where the
				// body proper begins.
				return Undefined, nil
			}
		case bytecode.OpIterResume:
			// stack: cursor sent kind
			sp--
			kind := stack[sp]
			sp--
			sent := stack[sp]
			res, done, err := r.iterResume(stack[sp-1], sent, resumeMode(kind.Number()),
				in.A == 1)
			if err != nil {
				vmErr = err
				goto onError
			}
			if done {
				// A return the delegate had no method for, or took and
				// finished, ends the delegation. What that means is decided by
				// the code after it, which the value is handed to: for a
				// return it is what the outer generator returns, awaited first
				// when the generator is an async one.
				sp = pushAt(stack, sp, res)
				if in.B < pc {
					if budget--; budget <= 0 {
						pc = in.B
						goto interrupted
					}
				}
				pc = in.B
				break
			}
			sp = pushAt(stack, sp, res)
		case bytecode.OpIterUnpackDelegate:
			sp--
			res := stack[sp]
			if !res.IsObject() {
				vmErr = r.throwTypeError("an iterator result must be an object")
				goto onError
			}
			done, err := r.getValueProp(res, atomDone)
			if err != nil {
				vmErr = err
				goto onError
			}
			if !done.Truthy() {
				if in.B&1 == 1 {
					// A synchronous delegation hands the result object out as
					// it is, so its value is never read here.
					sp = pushAt(stack, sp, res)
				} else {
					val, err := r.getValueProp(res, atomValue)
					if err != nil {
						vmErr = err
						goto onError
					}
					sp = pushAt(stack, sp, val)
				}
				break
			}
			val, err := r.getValueProp(res, atomValue)
			if err != nil {
				vmErr = err
				goto onError
			}
			if st := iterStateOf(stack[sp-1]); st != nil {
				st.done = true
			}
			// The value goes to the code after the delegation, which decides
			// what it means: the delegate's return value, or -- when the outer
			// generator was the one asked to return -- what it returns.
			sp = pushAt(stack, sp, val)
			if in.A < pc {
				if budget--; budget <= 0 {
					pc = in.A
					goto interrupted
				}
			}
			pc = in.A
		case bytecode.OpIterSend, bytecode.OpIterSendAsync:
			sp--
			sent := stack[sp]
			res, err := r.iterSend(stack[sp-1], sent, in.Op == bytecode.OpIterSendAsync)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, res)
		case bytecode.OpIterUnpack:
			sp--
			res := stack[sp]
			if !res.IsObject() {
				vmErr = r.throwTypeError("an iterator result must be an object")
				goto onError
			}
			done, err := r.getValueProp(res, atomDone)
			if err != nil {
				vmErr = err
				goto onError
			}
			if in.B == 1 && !done.Truthy() {
				// A synchronous `yield*` hands the delegate's result object
				// straight out, so its value is never read here -- which a
				// getter on it can see.
				sp = pushAt(stack, sp, res)
				break
			}
			val, err := r.getValueProp(res, atomValue)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, val)
			if done.Truthy() {
				// The cursor is exhausted, so nothing is left to close.
				if st := iterStateOf(stack[sp-2]); st != nil {
					st.done = true
				}
				if in.A < pc {
					if budget--; budget <= 0 {
						pc = in.A
						goto interrupted
					}
				}
				pc = in.A
			}
		case bytecode.OpIterResultOrJump:
			sp--
			res := stack[sp]
			if !res.IsObject() {
				vmErr = r.throwTypeError("an iterator result must be an object")
				goto onError
			}
			done, err := r.getValueProp(res, atomDone)
			if err != nil {
				vmErr = err
				goto onError
			}
			if done.Truthy() {
				if in.A < pc {
					if budget--; budget <= 0 {
						pc = in.A
						goto interrupted
					}
				}
				pc = in.A
				break
			}
			val, err := r.getValueProp(res, atomValue)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, val)

		case bytecode.OpIterToArray:
			sp--
			arr, err := r.iterToArray(stack[sp], in.A)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, arr)
		case bytecode.OpIterClose:
			// Leaving a loop by break or continue is not a throw, so a failure
			// in the iterator's return method is the result rather than
			// something to swallow -- and so is a return method that hands back
			// something other than an object.
			if err := r.iterCloseNormal(stack[sp-1]); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpSpreadIter:
			sp--
			vals, err := r.spreadToStack(stack[sp])
			if err != nil {
				vmErr = err
				goto onError
			}
			target := stack[sp-1]
			if target.IsObject() {
				target.Object().elems = append(target.Object().elems, vals...)
			}
		case bytecode.OpArraySpread:
			sp--
			src := stack[sp]
			target := stack[sp-1]
			if target.IsObject() {
				if err := r.spreadInto(target.Object(), src); err != nil {
					vmErr = err
					goto onError
				}
			}
		case bytecode.OpCopyDataProps:
			sp--
			src := stack[sp]
			target := stack[sp-1]
			if target.IsObject() && !src.IsNullish() {
				if err := r.copyDataProps(target.Object(), src); err != nil {
					vmErr = err
					goto onError
				}
			}

		// --- Private class members ----------------------------------------
		case bytecode.OpPrivateName:
			sp = pushAt(stack, sp, r.newPrivateName(cl.names[in.A]))
		case bytecode.OpGetPrivate:
			sp--
			obj := stack[sp]
			if !obj.IsObject() {
				vmErr = r.throwTypeError("cannot read a private member of %s", r.describe(obj))
				goto onError
			}
			v, err := r.getPrivate(obj.Object(), privateKey(f, cl, in.B), cl.names[in.A])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpSetPrivate:
			sp--
			val := stack[sp]
			sp--
			obj := stack[sp]
			if !obj.IsObject() {
				vmErr = r.throwTypeError("cannot write a private member of %s", r.describe(obj))
				goto onError
			}
			err := r.setPrivate(obj.Object(), privateKey(f, cl, in.B), cl.names[in.A], val)
			if err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpDefinePrivate, bytecode.OpDefinePrivateMethod:
			sp--
			val := stack[sp]
			target := stack[sp-1]
			if target.IsObject() {
				key := privateKey(f, cl, in.B)
				// A private field can only be added once. A constructor that
				// returns an object it has already built would otherwise give
				// it the same field twice.
				if target.Object().getOwn(key) != nil {
					vmErr = r.throwTypeError("%s is already present on this object",
						r.atoms.name(key))
					goto onError
				}
				if !target.Object().IsExtensible() {
					// A private member is a member like any other: an object
					// that has been closed to new ones takes none.
					vmErr = r.throwTypeError(
						"cannot add %s to a non-extensible object", r.atoms.name(key))
					goto onError
				}
				flags := propFlags(propWritable | propPrivate)
				if in.Op == bytecode.OpDefinePrivateMethod {
					// A method is not a place to store anything.
					flags = propPrivate
				}
				target.Object().setOwnRaw(key, val, flags)
			}
		case bytecode.OpDefinePrivateGetter, bytecode.OpDefinePrivateSetter:
			sp--
			val := stack[sp]
			target := stack[sp-1]
			if target.IsObject() && val.IsObject() {
				r.definePrivateAccessor(target.Object(), privateKey(f, cl, in.B),
					val.Object(), in.Op == bytecode.OpDefinePrivateGetter)
			}
		case bytecode.OpPrivateIn:
			sp--
			obj := stack[sp]
			if !obj.IsObject() {
				// Only an object can have one, and asking anything else is a
				// mistake rather than a false.
				vmErr = r.throwTypeError(
					"the right operand of \"in\" must be an object, not %s", r.describe(obj))
				goto onError
			}
			// An own property and nothing else: a private member belongs to the
			// object that has it, and an object that merely inherits from an
			// instance's prototype is not an instance.
			sp = pushAt(stack, sp, Bool(obj.Object().getOwn(privateKey(f, cl, in.B)) != nil))
		case bytecode.OpNewPrivateMethods:
			sp = pushAt(stack, sp, r.newPrivateMethods())
		case bytecode.OpAddPrivateMethod, bytecode.OpAddPrivateGetter,
			bytecode.OpAddPrivateSetter:
			sp--
			fnVal := stack[sp]
			sp--
			list := stack[sp]
			r.addPrivateMethod(list, privateKey(f, cl, in.B), fnVal, in.Op)
		case bytecode.OpInstallPrivateMethods:
			this, bound := f.thisValue()
			if !bound {
				vmErr = r.throwError(errReference,
					"\"this\" is not bound until super() has been called")
				goto onError
			}
			sp--
			if err := r.installPrivateMethods(this, stack[sp]); err != nil {
				vmErr = err
				goto onError
			}

		// --- Classes ------------------------------------------------------
		case bytecode.OpNewClass:
			// stack: ctor parent
			sp--
			parent := stack[sp]
			ctorVal := stack[sp-1]
			if err := r.linkClass(ctorVal, parent); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpSetHomeObject:
			// `super` inside the method resolves against the home object,
			// which sits A slots below the function -- one normally, two when
			// a computed key was pushed in between.
			if fnVal := stack[sp-1]; fnVal.IsObject() {
				if home := stack[sp-1-int(in.A)]; fnVal.Object().fn() != nil && home.IsObject() {
					fnVal.Object().fn().homeObject = home.Object()
				}
			}
		case bytecode.OpSetFieldInit:
			sp--
			init := stack[sp]
			if ctor := stack[sp-1]; ctor.IsObject() && init.IsObject() {
				if fd := ctor.Object().fn(); fd != nil {
					fd.fieldInit = init.Object()
				}
			}
		case bytecode.OpDefineMethod:
			// A class method is not enumerable, unlike an object literal's.
			sp--
			val := stack[sp]
			target := stack[sp-1]
			if target.IsObject() {
				key := cl.names[in.A]
				// A static member named after a function's synthesized length or
				// name replaces it in place, so it has to exist first.
				r.materializeFunctionProp(target.Object(), key)
				target.Object().setOwnRaw(key, val, propWritable|propConfigurable)
			}
		case bytecode.OpSuperCall:
			var args []Value
			if in.B != 0 {
				// The spread form gathered the arguments into an array.
				sp--
				if a := stack[sp]; a.IsObject() {
					args = a.Object().elems
				}
			} else {
				argc := int(in.A)
				args = append([]Value(nil), r.stack[sp-argc:sp]...)
				sp -= argc
			}
			if err := r.superCall(f, args); err != nil {
				vmErr = err
				goto onError
			}
			// The value of a super call is the object it bound, which is what
			// makes `var x = super()` mean something.
			sp = pushAt(stack, sp, f.this)
		case bytecode.OpGetSuperProp:
			v, err := r.superGet(f, cl.names[in.A])
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)
		case bytecode.OpSetSuperProp:
			sp--
			val := stack[sp]
			if err := r.superSet(f, cl.names[in.A], val, cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpSuperBase:
			base, _, err := r.superRef(f)
			if err != nil {
				vmErr = err
				goto onError
			}
			if base == nil {
				// No prototype to reach: the reference has a null base, which
				// the read or the write below reports once it gets there.
				sp = pushAt(stack, sp, Null)
				break
			}
			sp = pushAt(stack, sp, Obj(base))
		case bytecode.OpSetSuperIndex:
			sp--
			val := stack[sp]
			sp--
			rawKey := stack[sp]
			sp--
			base := stack[sp]
			if !base.IsObject() {
				vmErr = r.throwTypeError("\"super\" has no prototype to assign to")
				goto onError
			}
			key, err := r.toPropertyKey(rawKey)
			if err != nil {
				vmErr = err
				goto onError
			}
			if err := r.superSetFrom(f, base, key, val, cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpGetSuperIndex:
			sp--
			rawKey := stack[sp]
			sp--
			base := stack[sp]
			if !base.IsObject() {
				vmErr = r.throwTypeError("\"super\" has no prototype to read from")
				goto onError
			}
			key, err := r.toPropertyKey(rawKey)
			if err != nil {
				vmErr = err
				goto onError
			}
			this, _ := f.thisValue()
			v, err := r.getProp(base.Object(), key, this)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)

		case bytecode.OpNewRegExp:
			// The constant holds the pattern text; a fresh object is built on
			// each evaluation, since a literal produces a new RegExp with its
			// own lastIndex every time it is reached.
			c := cl.fn.Constants[in.A]
			v, err := r.newRegExp(c.Str, c.Flags)
			if err != nil {
				vmErr = err
				goto onError
			}
			sp = pushAt(stack, sp, v)

		// --- Suspension ---------------------------------------------------
		case bytecode.OpYield, bytecode.OpAwait, bytecode.OpYieldStar:
			// The operand is popped before the depth is recorded, so that the
			// saved stack holds only what is still live. Recording it first
			// would save the yielded value too, and the resumption would then
			// find an extra entry beneath the sent one.
			sp--
			yielded := stack[sp]
			f.savedSP = sp
			return Undefined, &suspendSignal{
				value:    yielded,
				await:    in.Op == bytecode.OpAwait,
				delegate: in.Op == bytecode.OpYieldStar,
				raw:      in.Op == bytecode.OpYieldStar && in.A == 1,
			}
		case bytecode.OpInitialYield:
			f.savedSP = sp
			return Undefined, &suspendSignal{value: Undefined}

		case bytecode.OpCheckThisInit:
			// A super reference reads `this` before it evaluates anything
			// else, so a derived constructor that has not called super() fails
			// here rather than after the key expression has run.
			if _, bound := f.thisValue(); !bound {
				vmErr = r.throwError(errReference,
					"\"this\" is not bound until super() has been called")
				goto onError
			}
		case bytecode.OpThrowDeleteSuper:
			vmErr = r.throwError(errReference,
				"a super reference cannot be deleted")
			goto onError
		case bytecode.OpNewTarget:
			sp = pushAt(stack, sp, f.newTarget)
		case bytecode.OpImportMeta:
			sp = pushAt(stack, sp, r.importMeta(cl.env))
		case bytecode.OpPushCallee:
			if f.callee == nil {
				sp = pushAt(stack, sp, Undefined)
				break
			}
			sp = pushAt(stack, sp, Obj(f.callee))

		default:
			vmErr = r.throwTypeError("unimplemented opcode %s", in.Op)
			goto onError
		}
		continue

	interrupted:
		// A backward jump has run the budget out: the check an unbounded
		// program cannot run without reaching, since it runs no longer than
		// its code is long but through a loop, or a call, which callObject
		// counts.
		budget = backEdgeCheckInterval
		if err := r.checkInterruptNow(); err != nil {
			// An interrupt is the host stopping the script rather than a
			// JavaScript exception, so it is not catchable.
			return Undefined, err
		}
		r.sweepStaleSlots(sp)
		continue

	onError:
		// An exception unwinds to the innermost handler registered in this
		// frame. With none, it propagates to the caller, which repeats the
		// search in its own frame.
		var caught bool
		if sp, caught = r.unwindToHandler(f, sp, vmErr); !caught {
			// The exception leaves this frame entirely, so every loop it was
			// inside is being abandoned.
			r.closeIteratorsIn(f.base, sp)
			return Undefined, vmErr
		}
		pc = f.pc
		vmErr = nil
	}
}

// unwindToHandler transfers control to the innermost catch handler of a frame,
// reporting false when the frame has none and the exception must propagate.
//
// Only a JavaScript exception is catchable. A host interruption -- a cancelled
// context, or the stack limit being reached -- deliberately is not, so that a
// script cannot defeat its own sandbox with try/catch.
func (r *Runtime) unwindToHandler(f *frame, sp int, err error) (int, bool) {
	thrown, ok := err.(*Thrown)
	if !ok || len(f.handlers) == 0 {
		return sp, false
	}
	h := f.handlers[len(f.handlers)-1]
	f.handlers = f.handlers[:len(f.handlers)-1]

	// The stack may hold a partly-built expression from the point of the
	// throw, so it is cut back to the depth the handler was registered at
	// before the thrown value is pushed for the catch clause to bind.
	depth := f.base + h.stackDepth
	r.closeIteratorsIn(depth, sp)
	sp = depth
	r.stack[sp] = thrown.Value
	sp++
	if h.isFinally {
		// A finally clause reproduces the original completion after it runs,
		// so it receives a record rather than a bare value.
		r.stack[sp] = Float(float64(completionThrow))
		sp++
	}
	f.pc = h.pc
	return sp, true
}

// functionNameFromKey derives a method's name from its property key.
//
// A symbol-keyed method is named after the symbol's description in brackets,
// and one whose symbol has no description is anonymous, which is the only way
// to name a function that cannot be spelled as an identifier.
func functionNameFromKey(key Value, kind uint32) string {
	name := ""
	switch {
	case key.IsSymbol():
		sym := key.Symbol()
		if sym.HasDescription {
			name = "[" + sym.Description + "]"
		}
	case key.IsString():
		name = key.String().Go()
	}
	switch kind {
	case 1:
		return "get " + name
	case 2:
		return "set " + name
	}
	return name
}

// defineHalfAccessor installs one half of an accessor, leaving the other half
// as whatever a previous definition put there.
//
// A get/set pair written separately reaches this twice, and the second must not
// discard the first.
func (r *Runtime) defineHalfAccessor(o *Object, key Atom, fn *Object, isGetter bool,
	flags propFlags) {
	var getter, setter *Object
	if isGetter {
		getter = fn
	} else {
		setter = fn
	}
	r.defineAccessor(o, key, getter, setter, flags)
}

// memberFlags turns a define instruction's class-member operand into the
// attributes the member gets. A class's members are not enumerable, so that
// Object.keys of an instance lists its fields and not the methods it inherits.
func memberFlags(isClassMember uint32) propFlags {
	if isClassMember != 0 {
		return propWritable | propConfigurable
	}
	return propDefault
}

// moduleLexProp finds the binding a name has in module code, which is either
// one of the module's own or a script-level lexical binding: the module
// environment inherits from the global object, and those sit in front of it.
func (r *Runtime) moduleLexProp(env *Object, name Atom) *Property {
	if p := env.getOwn(name); p != nil {
		return p
	}
	if len(r.globalLex.props) == 0 {
		return nil
	}
	return r.globalLex.getOwn(name)
}

// deleteEvalVar removes a binding a direct eval declared, reporting whether
// there was one.
func deleteEvalVar(o *Object, name Atom) bool {
	for ; o != nil; o = o.proto {
		if o.getOwn(name) != nil {
			o.deleteOwn(name)
			return true
		}
	}
	return false
}

// evalVarProp finds a binding a direct eval declared in an enclosing function.
//
// The objects are chained by prototype, innermost first, so one lookup covers
// every function between here and the one that declared the name.
func evalVarProp(o *Object, name Atom) *Property {
	for ; o != nil; o = o.proto {
		if p := o.getOwn(name); p != nil {
			return p
		}
	}
	return nil
}

// globalLexProp finds a script-level lexical binding, which sits in front of
// the global object: `let x = 1` at a script's top level is reached by name but
// is not a property of globalThis.
func (r *Runtime) globalLexProp(env *Object, name Atom) *Property {
	if !r.isGlobalScope(env) || len(r.globalLex.props) == 0 {
		return nil
	}
	return r.globalLex.getOwn(name)
}

// checkGlobalVarName rejects a script-level var or function declaration whose
// name a lexical binding already has.
func (r *Runtime) checkGlobalVarName(name Atom) error {
	if r.globalLex.getOwn(name) != nil {
		return r.throwError(errSyntax, "%q has already been declared", r.atoms.name(name))
	}
	return nil
}

// canDeclareGlobalFunc reports whether a top-level function declaration may
// take a name the global object already has.
//
// A property that can be deleted may always be replaced. One that cannot must
// already look like what the declaration would create -- a writable, enumerable
// data property -- or the declaration cannot honour it.
func (r *Runtime) canDeclareGlobalFunc(name Atom) error {
	p := r.global.getOwn(name)
	if p == nil {
		if !r.global.IsExtensible() {
			return r.throwTypeError("cannot declare %q on a non-extensible global",
				r.atoms.name(name))
		}
		return nil
	}
	if p.flags&propConfigurable != 0 {
		return nil
	}
	if p.flags&propAccessor != 0 ||
		p.flags&propWritable == 0 || p.flags&propEnumerable == 0 {
		return r.throwTypeError("cannot redeclare %q as a function", r.atoms.name(name))
	}
	return nil
}

// tick advances the interrupt counter from outside the interpreter loop.
//
// A built-in that walks a long sequence without running any bytecode -- an
// Array method over an array-like with an enormous length, say -- would
// otherwise be unstoppable, which is exactly the hang the context bound exists
// to prevent.
// nest enters one level of the recursion the call stack's frames do not
// count, refusing one past maxGoRecursion as a call past the depth limit is
// refused. Each nest is matched by an unnest.
func (r *Runtime) nest() error {
	if r.nesting >= maxGoRecursion {
		return r.throwRangeError("maximum call stack size exceeded")
	}
	r.nesting++
	return nil
}

// unnest leaves a level nest entered.
func (r *Runtime) unnest() { r.nesting-- }

func (r *Runtime) tick() error {
	r.interruptCounter--
	if r.interruptCounter <= 0 {
		return r.checkInterruptNow()
	}
	return nil
}

// unwindToFinally transfers control to the innermost finally handler, carrying
// a return completion.
//
// Catch handlers are discarded on the way rather than entered: a return is not
// an exception, and `try { return } catch {}` does not run its catch.
func (r *Runtime) unwindToFinally(f *frame, sp int, value Value) (int, bool) {
	for len(f.handlers) > 0 {
		h := f.handlers[len(f.handlers)-1]
		f.handlers = f.handlers[:len(f.handlers)-1]
		if !h.isFinally {
			continue
		}
		// A for-of the return is leaving is told so, and a failure in its own
		// return method replaces the return the unwind was carrying: the
		// finally still runs, but on a throw.
		kind := completionReturn
		depth := f.base + h.stackDepth
		if err := r.closeIteratorsReturning(depth, sp); err != nil {
			value, kind = thrownValue(err), completionThrow
		}
		sp = depth
		r.stack[sp] = value
		sp++
		r.stack[sp] = Float(float64(kind))
		sp++
		f.pc = h.pc
		return sp, true
	}
	return sp, false
}

// closeUpvaluesFrom closes every upvalue pointing at or above a local slot,
// which happens when a block that captured bindings exits.
func (r *Runtime) closeUpvaluesFrom(f *frame, slot int) {
	kept := f.openUpvalues[:0]
	for _, u := range f.openUpvalues {
		// Compare addresses to decide whether the upvalue points into the
		// region being released.
		if pointsAtOrAbove(u.slot, f.locals, slot) {
			u.close()
			continue
		}
		kept = append(kept, u)
	}
	f.openUpvalues = kept
}

// pointsAtOrAbove reports whether p refers to locals[slot] or a later slot.
func pointsAtOrAbove(p *Value, locals []Value, slot int) bool {
	for i := slot; i < len(locals); i++ {
		if p == &locals[i] {
			return true
		}
	}
	return false
}

// makeClosure builds a closure from a nested function template, capturing the
// upvalues its descriptor names.
func (r *Runtime) makeClosure(f *frame, c Value) *Object {
	tmpl := c.template()

	// The object, its function data, its closure and the bindings that closure
	// captures are one allocation: a closure made in a loop is as common as
	// any object literal, and each of the four was a separate one.
	o, fd, child, upvalues := newScriptFuncObject(
		r.funcProtoFor(tmpl.fn), ClassFunction, len(tmpl.fn.Upvalues))

	// The environment is inherited, so a function declared in a module sees
	// the module's bindings rather than only the globals.
	*child = closure{
		fn: tmpl.fn, names: tmpl.names, consts: tmpl.consts,
		realm: r.Realm, env: f.cl.env, upvalues: upvalues,
	}
	for i, desc := range tmpl.fn.Upvalues {
		if desc.FromParent {
			upvalues[i] = r.captureLocal(f, int(desc.Index))
		} else {
			upvalues[i] = f.cl.upvalues[desc.Index]
		}
	}

	kind := ctorKindOf(tmpl.fn)
	*fd = funcData{
		closure:  child,
		name:     tmpl.fn.Name,
		length:   tmpl.fn.Length,
		ctorKind: kind,
		// A function created inside a `with` body keeps the objects: the names
		// in its own body resolve against them too, and the frame that pushed
		// them is gone by the time it runs. What a direct eval declared in the
		// enclosing function travels the same way.
		lexWith:     f.withScopes,
		lexEvalVars: f.evalVars,
	}
	if tmpl.fn.Kind == bytecode.KindArrow {
		// An arrow captures its surroundings rather than receiving them from
		// the call. Nesting works because an arrow created inside another has
		// already inherited them, so reading the creating frame is enough.
		fd.arrow = true
		fd.lexThis = f.this
		fd.lexThisRef = f.thisRef
		fd.lexNewTarget = f.newTarget
		fd.lexArgs = f.args
		if f.callee != nil {
			if outer := f.callee.fn(); outer != nil {
				// super resolves against the enclosing method's home object.
				fd.homeObject = outer.homeObject
				fd.superCtor = outer.superCtor
				if outer.arrow {
					fd.lexArgs = outer.lexArgs
				}
			}
		}
	}

	// A constructible function carries a fresh .prototype object, which is what
	// `new` gives the instance and where a class hangs its methods. An arrow or
	// a method is not constructible and does not get one.
	if instance := r.instanceProtoFor(tmpl.fn); instance != nil {
		// A generator function is not constructible, but it still has a
		// .prototype: it is what the generator objects it produces inherit
		// from. It carries no constructor back-reference, since nothing
		// constructs it.
		proto := newObject(instance, ClassObject)
		o.setOwnRaw(atomPrototype, Obj(proto), propWritable)
		return o
	}
	if kind != ctorNone {
		if tmpl.fn.Kind == bytecode.KindNormal {
			// An ordinary function's prototype is built when something first
			// asks for it. Most functions are called rather than constructed
			// from, and the object and its back-reference are two allocations
			// per closure that nothing ever looks at.
			fd.protoPending = true
			return o
		}
		proto := newObject(r.proto.object, ClassObject)
		proto.setOwnRaw(atomConstructor, Obj(o), propWritable|propConfigurable)
		flags := propWritable
		switch tmpl.fn.Kind {
		case bytecode.KindConstructor, bytecode.KindDerivedConstructor:
			// A class's prototype cannot be replaced, which is what makes a
			// static member named "prototype" an error.
			flags = 0
		}
		o.setOwnRaw(atomPrototype, Obj(proto), flags)
		// A constructor's home object is its own prototype, which is what
		// makes `super.m()` inside a constructor find the parent's method.
		// Nothing outside a class can name super, so setting it on every
		// constructible function is harmless.
		fd.homeObject = proto
	}
	return o
}

// materializeFunctionProto creates the .prototype an ordinary function was
// promised, as a real own property that anything reading the property table
// finds.
//
// It is deferred rather than skipped: the object's identity has to be stable,
// so the first thing to ask for it is what decides what it is, and everything
// afterwards sees the same one.
func (r *Runtime) materializeFunctionProto(o *Object) {
	fd := o.fn()
	if fd == nil || !fd.protoPending {
		return
	}
	fd.protoPending = false
	// It is the function's realm's object, whichever realm reads it first.
	objProto := r.proto.object
	if fd.closure != nil && fd.closure.realm != nil {
		objProto = fd.closure.realm.proto.object
	}
	proto := newObject(objProto, ClassObject)
	proto.setOwnRaw(atomConstructor, Obj(o), propWritable|propConfigurable)
	// It goes where it would have been had the function been built with it:
	// after length and name, which come first whenever they are created, and
	// before anything a script has added since. An ownKeys walk reports the
	// table's order, so building it late may not reorder it.
	at := 0
	for at < len(o.props) &&
		(o.props[at].key == atomLength || o.props[at].key == atomName) {
		at++
	}
	o.insertProp(at, Property{
		key: atomPrototype, value: Obj(proto), flags: propWritable,
	})
	// A constructible function's home object is its own prototype. Nothing
	// outside a class can name super, so an ordinary function's is never read,
	// but it costs nothing to keep it the same as an eagerly built one's.
	fd.homeObject = proto
}

func ctorKindOf(fn *bytecode.Function) ctorKind {
	// A generator or async function has a prototype property but is not a
	// constructor: there is no object for `new` to build, since calling one
	// produces an iterator or a promise.
	if fn.Generator || fn.Async {
		return ctorNone
	}
	switch fn.Kind {
	case bytecode.KindNormal:
		return ctorBase
	case bytecode.KindConstructor:
		return ctorBase
	case bytecode.KindDerivedConstructor:
		return ctorDerived
	}
	return ctorNone
}

// derivedResult settles what a derived constructor hands back: an object it
// returned itself, or the object super() built.
//
// The object has to exist by the time the constructor returns, whatever route
// the return took -- which is why this is at the return rather than at the
// return statement, where a finally clause may still call super() afterwards.
// An arrow written inside one shares the binding but is not the constructor,
// so its kind is what tells them apart.
func (r *Runtime) derivedResult(f *frame, cl *closure, v Value) (Value, error) {
	if v.IsObject() || cl.fn.Kind != bytecode.KindDerivedConstructor {
		return v, nil
	}
	if !v.IsUndefined() {
		// Returning anything else is a TypeError, but not one the constructor
		// can catch: it is raised by the construction, after the body has
		// finished. The value is handed out as it is so that the caller can
		// report it from there.
		return v, nil
	}
	if !f.thisRef.init {
		return Undefined, errNoSuper
	}
	return f.thisRef.value, nil
}

// declareOnScope declares a global var or function in a context, as node's
// contexts do. Both are the realm's global object's, as any script's are; a
// function is the sandbox's too, where the context's code reads it from and a
// deletion can take it away, while a var reaches the sandbox only when it is
// assigned. A var the context already has is left as it is; a function
// replaces what is there, and is assigned where it cannot be redefined.
func (r *Runtime) declareOnScope(env *Object, name Atom, v Value, flags propFlags, replace bool) error {
	p := proxyOf(env)
	if !replace {
		d, err := r.ownDescriptorOf(env, name)
		if err != nil || !d.IsUndefined() {
			return err
		}
	}
	if own := p.target.getOwn(name); own == nil || own.flags&propConfigurable != 0 {
		p.target.setOwnRaw(name, v, flags|propEnumerable)
	}
	if !replace {
		return nil
	}
	ok, err := r.defineProperty(p.sandbox, name, &propDesc{
		value: v, hasValue: true,
		writable: true, hasWritable: true,
		enumerable: true, hasEnumerable: true,
		configurable: true, hasConfigurable: true,
	})
	if err != nil || ok {
		return err
	}
	_, err = r.setProp(p.sandbox, name, v, Obj(p.sandbox), false)
	return err
}

// errNoSuper is a derived constructor finishing without having called super().
// It is not an exception the body can catch: the construction fails once the
// body is done, and the ReferenceError it becomes is the caller's realm's.
var errNoSuper = errors.New("a derived constructor must call super() before returning")

// captureLocal returns the upvalue for a local slot, reusing an existing one so
// that two closures over the same variable share it.
func (r *Runtime) captureLocal(f *frame, slot int) *upvalue {
	for _, u := range f.openUpvalues {
		if u.slot == &f.locals[slot] {
			return u
		}
	}
	u := &upvalue{slot: &f.locals[slot]}
	f.openUpvalues = append(f.openUpvalues, u)
	return u
}

// newArrayFrom builds an array holding a copy of the given values.
func (r *Runtime) newArrayFrom(vals []Value) *Object {
	o := newArrayObject(r.proto.array, len(vals))
	copy(o.elems, vals)
	return o
}

// getIndexed reads a computed property, taking a fast path for an array index
// on a dense array.
func (r *Runtime) getIndexed(obj, key Value) (Value, error) {
	if obj.IsNullish() {
		// The base is checked before the key is converted: a key whose
		// toString throws must not run at all when there is nothing to read
		// from.
		return Undefined, r.throwTypeError("cannot read property of %s", r.describe(obj))
	}
	if obj.IsObject() && key.IsNumber() {
		o := obj.Object()
		// A mapped arguments object's indices are not what its dense storage
		// says, so it has no fast path.
		if i := uint32(key.Number()); float64(i) == key.Number() &&
			uint(i) < uint(len(o.elems)) && o.flags&objMappedArguments == 0 {
			if v := o.elems[i]; !isHole(v) {
				return v, nil
			}
		}
	}
	n, err := r.toPropertyName(key)
	if err != nil {
		return Undefined, err
	}
	o := r.protoOfPrimitive(obj)
	if obj.IsObject() {
		o = obj.Object()
	}
	k, ok := r.keyFor(o, n)
	if !ok {
		return Undefined, nil
	}
	return r.getValueProp(obj, k)
}

// construct implements the `new` operator.
func (r *Runtime) construct(callee Value, args []Value) (Value, error) {
	return r.constructWithTarget(callee, args, callee)
}

// isConstructor reports whether a value responds to new.
func isConstructor(v Value) bool {
	if !v.IsObject() || v.Object() == nil {
		return false
	}
	o := v.Object()
	if p := proxyOf(o); p != nil {
		return p.constructor
	}
	fd := o.fn()
	return fd != nil && fd.ctorKind != ctorNone
}

// constructWithTarget builds an object with one constructor while another
// decides what it is.
//
// The two differ whenever a subclass is involved: `new D()` on a class derived
// from B eventually runs B's constructor, but the object is a D, so the
// prototype comes from D. Reflect.construct exposes the same split directly.
func (r *Runtime) constructWithTarget(callee Value, args []Value, newTarget Value) (Value, error) {
	if !callee.IsObject() || callee.Object() == nil {
		return Undefined, r.throwTypeError("%s is not a constructor", r.describe(callee))
	}
	o := callee.Object()
	if p := proxyOf(o); p != nil {
		return r.proxyConstruct(p, args, newTarget)
	}
	fd := o.fn()
	if fd == nil || fd.ctorKind == ctorNone {
		return Undefined, r.throwTypeError("%s is not a constructor", fd.nameOr("value"))
	}

	// A bound function that is its own new.target hands the target on, one
	// link at a time -- which is what decides both the prototype of the object
	// being built and the new.target the innermost function sees.
	for at := o; ; {
		fd := at.fn()
		if fd == nil || fd.boundTarget == nil {
			break
		}
		if newTarget.IsObject() && newTarget.Object() == at {
			newTarget = Obj(fd.boundTarget)
		}
		at = fd.boundTarget
	}

	// The new object's prototype comes from new.target's .prototype property,
	// falling back to Object.prototype when that is not an object.
	target := o
	if newTarget.IsObject() {
		target = newTarget.Object()
	}

	// A native constructor builds its own object, so there is nothing to make
	// here -- and reading new.target's prototype now would be out of order: a
	// built-in checks its arguments first, and the read is observable.
	if fd.native != nil {
		return r.constructNative(o, args, newTarget, target != o)
	}

	protoVal, err := r.getProp(target, atomPrototype, Obj(target))
	if err != nil {
		return Undefined, err
	}
	var proto *Object
	if protoVal.IsObject() {
		proto = protoVal.Object()
	} else {
		// A constructor that named no prototype falls back to the one of the
		// realm it came from -- and a revoked proxy no longer has a realm to
		// be asked about.
		if proto, err = r.protoForNewTarget(target, r.proto.object); err != nil {
			return Undefined, err
		}
	}
	// The object is made with room for what the constructor is about to put in
	// it, which the compiler counted. A bound function has no body of its own
	// to have counted, so its object starts with none.
	props := 0
	if fd.closure != nil {
		props = int(fd.closure.fn.ThisProps)
	}
	this := Obj(newLiteralObject(proto, ClassObject, props))

	// A base class gives the instance its private methods and fields before the
	// constructor body runs, so the body finds them already there. A derived one
	// has no instance yet: its super() does this once the parent hands one back.
	if fd.ctorKind != ctorDerived {
		if err := r.initInstanceElements(o, this); err != nil {
			return Undefined, err
		}
	}
	res, err := r.callObject(o, this, args, newTarget)
	if err != nil {
		return Undefined, err
	}
	// A constructor that returns an object overrides the newly created one;
	// any other return value is ignored -- except in a derived constructor,
	// where returning something that is neither an object nor undefined is a
	// TypeError. It is raised here rather than at the return so that a try
	// inside the constructor cannot catch it: the body has already finished.
	if res.IsObject() {
		return res, nil
	}
	if fd.ctorKind == ctorDerived {
		return Undefined, r.throwTypeError(
			"a derived constructor may return only an object or undefined")
	}
	return this, nil
}

// constructNative runs a built-in constructor, which makes its own object
// because an Error needs a stack and an Array needs array storage.
//
// Most of them know nothing of new.target, so the prototype it chose is put
// back afterwards -- which is what makes Reflect.construct(Array, [], F)
// produce an F. The ones that do know ask through protoFromNewTarget, and the
// read that answers them is the only one: doing it again here would run a
// prototype getter twice.
func (r *Runtime) constructNative(o *Object, args []Value, newTarget Value, derived bool) (Value, error) {
	saved := r.usedNewTargetProto
	r.usedNewTargetProto = false
	res, err := r.callObject(o, Undefined, args, newTarget)
	askedItself := r.usedNewTargetProto
	r.usedNewTargetProto = saved
	if err != nil {
		return Undefined, err
	}
	if !res.IsObject() {
		// Nothing was built, which only a constructor that refuses to be one
		// reaches. An ordinary object stands in for the `this` a scripted
		// constructor would have had.
		return Obj(newObject(r.proto.object, ClassObject)), nil
	}
	if !derived || askedItself {
		return res, nil
	}
	protoVal, err := r.getProp(newTarget.Object(), atomPrototype, newTarget)
	if err != nil {
		return Undefined, err
	}
	if protoVal.IsObject() {
		res.Object().proto = protoVal.Object()
		return res, nil
	}
	// The constructor chose its own realm's prototype; new.target's realm
	// may have another.
	from, err := r.functionRealm(o)
	if err != nil {
		return Undefined, err
	}
	to, err := r.functionRealm(newTarget.Object())
	if err != nil {
		return Undefined, err
	}
	res.Object().proto = r.counterpart(from, to, res.Object().proto)
	return res, nil
}

func (fd *funcData) nameOr(fallback string) string {
	if fd == nil || fd.name == "" {
		return fallback
	}
	return fd.name
}

// callIntrinsic1 calls a function with one argument, as call does, but for
// one of the realm's own built-ins that reads its argument before anything
// else can run and keeps nothing of it, when the argument list comes from a
// stack the runtime keeps rather than being allocated for the call. A call
// made while this one runs stacks its own above it; one that grows the stack
// leaves this call's list where it was, unchanged.
func (r *Runtime) callIntrinsic1(fn *Object, this, a Value) (Value, error) {
	i := len(r.argStack)
	r.argStack = append(r.argStack, a)
	res, err := r.call(Obj(fn), this, r.argStack[i:i+1:i+1])
	r.argStack[i] = Undefined
	r.argStack = r.argStack[:i]
	return res, err
}

// instanceOf implements the instanceof operator.
func (r *Runtime) instanceOf(obj, ctor Value) (bool, error) {
	if !ctor.IsObject() {
		return false, r.throwTypeError("the right operand of \"instanceof\" must be an object")
	}
	c := ctor.Object()

	// A Symbol.hasInstance method overrides the default behaviour.
	hasInstance, err := r.getProp(c, r.atoms.internSymbol(r.wellKnown.hasInstance), ctor)
	if err != nil {
		return false, err
	}
	if !hasInstance.IsNullish() {
		var res Value
		if hasInstance.IsObject() && hasInstance.Object() == r.hasInstanceFn {
			res, err = r.callIntrinsic1(r.hasInstanceFn, ctor, obj)
		} else {
			res, err = r.call(hasInstance, ctor, []Value{obj})
		}
		if err != nil {
			return false, err
		}
		return res.Truthy(), nil
	}

	if !c.IsCallable() {
		return false, r.throwTypeError("the right operand of \"instanceof\" is not callable")
	}
	if !obj.IsObject() {
		return false, nil
	}
	protoVal, err := r.getProp(c, atomPrototype, ctor)
	if err != nil {
		return false, err
	}
	if !protoVal.IsObject() {
		return false, r.throwTypeError("the prototype of the right operand is not an object")
	}
	target := protoVal.Object()
	// The chain is walked by asking each object for its prototype, so a proxy
	// in it runs its trap rather than being read around.
	for o := obj.Object(); ; {
		next, err := r.protoOf(o)
		if err != nil {
			return false, err
		}
		if !next.IsObject() {
			return false, nil
		}
		o = next.Object()
		if o == target {
			return true, nil
		}
	}
}

// ---------------------------------------------------------------------------
// Operator helpers
// ---------------------------------------------------------------------------

// add implements the + operator, which is the only arithmetic operator that
// also concatenates.
func (r *Runtime) add(a, b Value) (Value, error) {
	pa, err := r.toPrimitive(a, hintDefault)
	if err != nil {
		return Undefined, err
	}
	pb, err := r.toPrimitive(b, hintDefault)
	if err != nil {
		return Undefined, err
	}
	// If either side is a string after coercion, the result is a string.
	if pa.IsString() || pb.IsString() {
		sa, err := r.toString(pa)
		if err != nil {
			return Undefined, err
		}
		sb, err := r.toString(pb)
		if err != nil {
			return Undefined, err
		}
		s, err := r.concat(sa, sb)
		if err != nil {
			return Undefined, err
		}
		return Str(s), nil
	}
	na, err := r.toNumeric(pa)
	if err != nil {
		return Undefined, err
	}
	nb, err := r.toNumeric(pb)
	if err != nil {
		return Undefined, err
	}
	if na.IsBigInt() != nb.IsBigInt() {
		return Undefined, r.throwTypeError("cannot mix BigInt and other types")
	}
	if na.IsBigInt() {
		return r.bigArith(bytecode.OpAdd, na.BigInt(), nb.BigInt())
	}
	return Float(na.Number() + nb.Number()), nil
}

// arith implements the arithmetic operators other than +.
func (r *Runtime) arith(op bytecode.Op, a, b Value) (Value, error) {
	na, err := r.toNumeric(a)
	if err != nil {
		return Undefined, err
	}
	nb, err := r.toNumeric(b)
	if err != nil {
		return Undefined, err
	}
	if na.IsBigInt() != nb.IsBigInt() {
		return Undefined, r.throwTypeError("cannot mix BigInt and other types")
	}
	if na.IsBigInt() {
		return r.bigArith(op, na.BigInt(), nb.BigInt())
	}
	return Float(numericOp(op, na.Number(), nb.Number())), nil
}

// int32Op applies a bitwise or shift operator to two numbers.
func int32Op(op bytecode.Op, x, y float64) Value {
	switch op {
	case bytecode.OpBitAnd:
		return Int32(jsnum.ToInt32(x) & jsnum.ToInt32(y))
	case bytecode.OpBitOr:
		return Int32(jsnum.ToInt32(x) | jsnum.ToInt32(y))
	case bytecode.OpBitXor:
		return Int32(jsnum.ToInt32(x) ^ jsnum.ToInt32(y))
	case bytecode.OpShl:
		// Only the low five bits of the shift count are used.
		return Int32(jsnum.ToInt32(x) << (jsnum.ToUint32(y) & 31))
	case bytecode.OpShr:
		return Int32(jsnum.ToInt32(x) >> (jsnum.ToUint32(y) & 31))
	default:
		return Uint32(jsnum.ToUint32(x) >> (jsnum.ToUint32(y) & 31))
	}
}

// bitwise applies a bitwise or shift operator to operands that are not both
// numbers. Each is coerced once -- a valueOf runs once, as it does in every
// engine -- and a pair of BigInts has the BigInt operator.
func (r *Runtime) bitwise(op bytecode.Op, a, b Value) (Value, error) {
	na, err := r.toNumeric(a)
	if err != nil {
		return Undefined, err
	}
	nb, err := r.toNumeric(b)
	if err != nil {
		return Undefined, err
	}
	if na.IsBigInt() || nb.IsBigInt() {
		if op == bytecode.OpUShr {
			// Unsigned shift has no BigInt form: a BigInt has no width for
			// the sign bit to be shifted out of.
			return Undefined, r.throwTypeError("BigInt has no unsigned right shift")
		}
		v, _, err := r.bigBitwise(op, na, nb)
		return v, err
	}
	return int32Op(op, na.Number(), nb.Number()), nil
}

// binImm is OpBinImm for a left operand that is not a number: what the
// operator's own instruction does then.
func (r *Runtime) binImm(op bytecode.Op, a, b Value) (Value, error) {
	switch op {
	case bytecode.OpAdd:
		return r.add(a, b)
	case bytecode.OpSub, bytecode.OpMul:
		return r.arith(op, a, b)
	}
	return r.bitwise(op, a, b)
}

// bigBitwise applies a bitwise or shift operator when either side may be a
// BigInt, reporting whether it handled the operation.
//
// It reports false when both sides turn out to be numbers, leaving the caller
// to take its own faster path.
func (r *Runtime) bigBitwise(op bytecode.Op, a, b Value) (Value, bool, error) {
	na, err := r.toNumeric(a)
	if err != nil {
		return Undefined, false, err
	}
	nb, err := r.toNumeric(b)
	if err != nil {
		return Undefined, false, err
	}
	if !na.IsBigInt() && !nb.IsBigInt() {
		return Undefined, false, nil
	}
	if na.IsBigInt() != nb.IsBigInt() {
		return Undefined, false, r.throwTypeError("cannot mix BigInt and other types")
	}

	x, y := &na.BigInt().V, &nb.BigInt().V
	out := &BigInt{}
	switch op {
	case bytecode.OpBitAnd:
		out.V.And(x, y)
	case bytecode.OpBitOr:
		out.V.Or(x, y)
	case bytecode.OpBitXor:
		out.V.Xor(x, y)
	case bytecode.OpShl, bytecode.OpShr:
		// A BigInt shift has no width to wrap around, so the count is taken
		// whole -- and a negative one shifts the other way.
		n := y
		left := op == bytecode.OpShl
		if n.Sign() < 0 {
			neg := new(big.Int).Neg(n)
			n, left = neg, !left
		}
		if !n.IsInt64() || n.Int64() > maxBigIntBits {
			if left {
				if x.Sign() == 0 {
					break
				}
				return Undefined, false, r.throwBigIntSize()
			}
			// Shifting right past every bit leaves the sign: zero, or minus
			// one for a negative value.
			if x.Sign() < 0 {
				out.V.SetInt64(-1)
			}
			break
		}
		if left {
			if x.Sign() != 0 && int64(x.BitLen())+n.Int64() > maxBigIntBits {
				return Undefined, false, r.throwBigIntSize()
			}
			out.V.Lsh(x, uint(n.Int64()))
		} else {
			// An arithmetic shift, which big.Int's Rsh already is.
			out.V.Rsh(x, uint(n.Int64()))
		}
	default:
		return Undefined, false, r.throwTypeError("unsupported BigInt operation")
	}
	return Big(out), true, nil
}

// negate implements unary minus.
func (r *Runtime) negate(a Value) (Value, error) {
	n, err := r.toNumeric(a)
	if err != nil {
		return Undefined, err
	}
	if n.IsBigInt() {
		out := &BigInt{}
		out.V.Neg(&n.BigInt().V)
		return Big(out), nil
	}
	return Float(-n.Number()), nil
}

// numericOp applies a binary arithmetic operator to two float64 values.
func numericOp(op bytecode.Op, a, b float64) float64 {
	switch op {
	case bytecode.OpAdd:
		return a + b
	case bytecode.OpSub:
		return a - b
	case bytecode.OpMul:
		return a * b
	case bytecode.OpDiv:
		return a / b
	case bytecode.OpMod:
		// JavaScript's % keeps the sign of the dividend, which is what Go's
		// math.Mod does, unlike the integer % operator.
		return math.Mod(a, b)
	case bytecode.OpPow:
		return jsPow(a, b)
	}
	return math.NaN()
}

// jsPow implements the ** operator and Math.pow as V8 does: the C library's
// pow -- here Arm's, from its Optimized Routines, which glibc ships and so
// Node on Linux uses -- after the cases where JavaScript's differs from C's,
// and the two V8 answers without calling it.
func jsPow(a, b float64) float64 {
	switch {
	case math.IsNaN(b):
		// 1 ** NaN is 1 in C.
		return math.NaN()
	case (a == 1 || a == -1) && math.IsInf(b, 0):
		return math.NaN()
	case b == 2:
		return a * a
	case b == 0.5:
		if math.IsInf(a, 0) {
			return math.Inf(1)
		}
		// +0 for -0, where sqrt keeps the sign.
		return math.Sqrt(a + 0)
	}
	return fdlibm.PowC(a, b)
}

// compareFloats evaluates a relational operator on two numbers.
func compareFloats(op bytecode.Op, a, b float64) bool {
	switch op {
	case bytecode.OpLt:
		return a < b
	case bytecode.OpLe:
		return a <= b
	case bytecode.OpGt:
		return a > b
	default:
		return a >= b
	}
}

// relationalResult maps a three-way comparison to the boolean a relational
// operator produces. An undefined comparison, which arises from NaN, makes
// every operator false.
func relationalResult(op bytecode.Op, c cmpResult) bool {
	if c == cmpUndefined {
		return false
	}
	switch op {
	case bytecode.OpLt:
		return c == cmpLess
	case bytecode.OpLe:
		return c == cmpLess || c == cmpEqual
	case bytecode.OpGt:
		return c == cmpGreater
	default:
		return c == cmpGreater || c == cmpEqual
	}
}

// bigArith applies an arithmetic operator to two BigInts.
func (r *Runtime) bigArith(op bytecode.Op, a, b *BigInt) (Value, error) {
	out := &BigInt{}
	switch op {
	case bytecode.OpAdd, bytecode.OpSub:
		if max(a.V.BitLen(), b.V.BitLen())+1 > maxBigIntBits {
			// The one case where a sum could exceed the limit.
			if op == bytecode.OpAdd {
				out.V.Add(&a.V, &b.V)
			} else {
				out.V.Sub(&a.V, &b.V)
			}
			if out.V.BitLen() > maxBigIntBits {
				return Undefined, r.throwBigIntSize()
			}
			return Big(out), nil
		}
		if op == bytecode.OpAdd {
			out.V.Add(&a.V, &b.V)
		} else {
			out.V.Sub(&a.V, &b.V)
		}
	case bytecode.OpMul:
		// A product has at least as many bits as its factors' less one.
		if a.V.Sign() != 0 && b.V.Sign() != 0 && a.V.BitLen()+b.V.BitLen()-1 > maxBigIntBits {
			return Undefined, r.throwBigIntSize()
		}
		out.V.Mul(&a.V, &b.V)
	case bytecode.OpDiv:
		if b.IsZero() {
			return Undefined, r.throwRangeError("division by zero")
		}
		// BigInt division truncates toward zero, which is Quo rather than Div.
		out.V.Quo(&a.V, &b.V)
	case bytecode.OpMod:
		if b.IsZero() {
			return Undefined, r.throwRangeError("division by zero")
		}
		out.V.Rem(&a.V, &b.V)
	case bytecode.OpPow:
		if b.V.Sign() < 0 {
			return Undefined, r.throwRangeError("a BigInt cannot be raised to a negative power")
		}
		// Zero, one and minus one stay small whatever the power; anything
		// else has at least as many bits as the power, times its own less
		// one, which is checked before the work -- and exactly after it.
		if a.V.CmpAbs(big.NewInt(1)) > 0 && b.V.Sign() > 0 {
			if !b.V.IsInt64() || b.V.Int64() > maxBigIntBits ||
				int64(a.V.BitLen()-1)*b.V.Int64() > maxBigIntBits {
				return Undefined, r.throwBigIntSize()
			}
		}
		if !b.V.IsInt64() {
			// The base is 0, 1 or -1, and the power's parity is all that
			// matters.
			if b.V.Bit(0) == 0 {
				out.V.Exp(&a.V, big.NewInt(2), nil)
			} else {
				out.V.Set(&a.V)
			}
			return Big(out), nil
		}
		out.V.Exp(&a.V, &b.V, nil)
		if out.V.BitLen() > maxBigIntBits {
			return Undefined, r.throwBigIntSize()
		}
	default:
		return Undefined, r.throwTypeError("unsupported BigInt operation")
	}
	return Big(out), nil
}

// linkClass wires a derived class to its parent.
//
// Two chains are established. The prototypes are linked so that an instance
// inherits the parent's methods, and the constructors are linked so that a
// static method is visible on the subclass -- which is the part that surprises
// people, since it has no analogue in most class systems.
func (r *Runtime) linkClass(ctorVal, parent Value) error {
	if !ctorVal.IsObject() {
		return r.throwTypeError("a class constructor must be a function")
	}
	ctor := ctorVal.Object()
	fd := ctor.fn()
	if fd == nil {
		return r.throwTypeError("a class constructor must be a function")
	}

	// `class X extends null` produces a class whose instances have no
	// prototype, which is legal.
	if parent.IsNull() {
		if p, err := r.getProp(ctor, atomPrototype, ctorVal); err == nil && p.IsObject() {
			p.Object().proto = nil
		}
		fd.ctorKind = ctorDerived
		return nil
	}
	if !isConstructor(parent) {
		// An arrow, a method and a generator are callable but not
		// constructable, and a class has to call what it extends.
		return r.throwTypeError("a class may only extend a constructor or null")
	}
	parentObj := parent.Object()

	protoVal, err := r.getProp(ctor, atomPrototype, ctorVal)
	if err != nil {
		return err
	}
	parentProtoVal, err := r.getProp(parentObj, atomPrototype, parent)
	if err != nil {
		return err
	}
	// What the parent calls its prototype is what the instances inherit from,
	// and it has to be something they can: an object, or nothing at all.
	if !parentProtoVal.IsObject() && !parentProtoVal.IsNull() {
		return r.throwTypeError("the superclass prototype is neither an object nor null")
	}
	if protoVal.IsObject() {
		if parentProtoVal.IsObject() {
			protoVal.Object().proto = parentProtoVal.Object()
		} else {
			protoVal.Object().proto = nil
		}
	}
	// Static inheritance.
	ctor.proto = parentObj

	fd.ctorKind = ctorDerived
	fd.superCtor = ctor
	return nil
}

// superCall invokes the parent constructor on the current instance.
//
// The specification has a derived constructor receive `this` from its parent
// rather than creating it, with the binding uninitialized until super()
// returns. Here the instance is created up front and the parent runs against
// it, which gives the same result for every hierarchy that does not observe
// the difference through new.target or a base constructor that returns an
// object of its own.
func (r *Runtime) superCall(f *frame, args []Value) error {
	parent := r.parentConstructorOf(f)
	if parent == nil {
		return r.throwTypeError("\"super\" is only valid in a derived constructor")
	}
	if !isConstructor(Obj(parent)) {
		// The class's prototype may have been changed to something that cannot
		// be constructed, which is only visible now.
		return r.throwTypeError("the superclass is not a constructor")
	}
	if f.thisRef == nil {
		return r.throwError(errReference, "super() has already been called")
	}
	// new.target is forwarded rather than replaced: inside a base constructor
	// reached through super(), new.target is the derived class the caller
	// actually wrote `new` against. An abstract base distinguishes the two --
	// `new Iterator()` is an error while `new C()` for `class C extends
	// Iterator` is not -- so the difference is observable.
	newTarget := f.newTarget
	if newTarget.IsUndefined() {
		newTarget = Obj(parent)
	}
	// A base parent is entered directly rather than through construct, which
	// would build a second object for the one already made here, so its own
	// instance initializer is run on the way in. A derived parent runs its own
	// in its own super().
	if pfd := parent.fn(); pfd != nil && pfd.ctorKind != ctorDerived {
		if err := r.initInstanceElements(parent, f.this); err != nil {
			return err
		}
	}
	res, err := r.callObject(parent, f.this, args, newTarget)
	if err != nil {
		return err
	}
	// `this` is bound once, and the second super() finds it already bound. The
	// parent still ran: the check comes after the construction, not before it,
	// so a second call has the parent's side effects and none of its own.
	if f.thisRef.init {
		return r.throwError(errReference, "super() has already been called")
	}
	// A base constructor that builds its own object -- every native one does,
	// because an Error needs a stack and an Array needs array storage -- hands
	// it back instead of writing into the `this` passed in. The derived
	// constructor adopts it, keeping the prototype that newTarget chose so
	// that the result is still an instance of the derived class.
	if res.IsObject() && (!f.this.IsObject() || res.Object() != f.this.Object()) {
		if f.this.IsObject() {
			res.Object().proto = f.this.Object().proto
			// Fields the derived constructor set before super() would be lost,
			// but writing to `this` before super() is already an error, so
			// there is nothing to carry over.
		}
		f.this = res
	}
	f.thisRef.value = f.this
	f.thisRef.init = true
	// The fields belong to the class whose constructor is running, which is not
	// necessarily what is running: super() may be written inside an arrow, and
	// the arrow inherits the link to the class rather than being it. They are
	// put on the instance now: after the parent has finished with it, before
	// this constructor's body sees it.
	ctor := f.callee
	if fd := f.callee.fn(); fd != nil && fd.superCtor != nil {
		ctor = fd.superCtor
	}
	return r.initInstanceElements(ctor, f.this)
}

// initInstanceElements gives a new instance the private methods and fields of
// the class that is constructing it.
//
// A class without either has no initializer, which is the common case and costs
// nothing.
func (r *Runtime) initInstanceElements(ctor *Object, this Value) error {
	if ctor == nil {
		return nil
	}
	fd := ctor.fn()
	if fd == nil || fd.fieldInit == nil {
		return nil
	}
	_, err := r.callObject(fd.fieldInit, this, nil, Undefined)
	return err
}

// parentConstructorOf finds the constructor a frame's super() refers to.
func (r *Runtime) parentConstructorOf(f *frame) *Object {
	if f.callee == nil {
		return nil
	}
	fd := f.callee.fn()
	if fd == nil {
		return nil
	}
	if fd.superCtor != nil {
		// GetSuperConstructor reads the running class's prototype now rather
		// than remembering what it extended.
		return fd.superCtor.proto
	}
	// A method reaches the parent through its home object rather than through
	// a stored link.
	if fd.homeObject != nil && fd.homeObject.proto != nil {
		if ctor := fd.homeObject.proto.getOwn(atomConstructor); ctor != nil &&
			ctor.value.IsObject() {
			return ctor.value.Object()
		}
	}
	return nil
}

// superGet reads a property through super, resolving it on the home object's
// prototype while leaving `this` as the current receiver.
func (r *Runtime) superGet(f *frame, key Atom) (Value, error) {
	start, this, err := r.superBase(f)
	if err != nil {
		return Undefined, err
	}
	// The receiver stays the instance, so an inherited getter sees the right
	// object.
	return r.getProp(start, key, this)
}

// superBase resolves what a super reference reads from, and the receiver it
// reads with.
//
// It is settled before the key is computed, so that changing the home object's
// prototype while the key runs does not move the reference.
func (r *Runtime) superBase(f *frame) (*Object, Value, error) {
	start, this, err := r.superRef(f)
	if err != nil {
		return nil, Undefined, err
	}
	if start == nil {
		// `class C extends null` leaves the home object with no prototype, so
		// there is nothing to read from -- which the specification reports
		// rather than answering undefined.
		return nil, Undefined, r.throwTypeError("\"super\" has no prototype to read from")
	}
	return start, this, nil
}

// superRef is superBase without the requirement that there be a prototype to
// reach.
//
// A reference with no base is only an error once something is done with it, and
// that happens after the rest of the expression has run: `super[k()] = v()`
// calls both before it complains.
func (r *Runtime) superRef(f *frame) (*Object, Value, error) {
	if f.callee == nil {
		return nil, Undefined, r.throwTypeError("\"super\" is only valid inside a method")
	}
	fd := f.callee.fn()
	if fd == nil || fd.homeObject == nil {
		return nil, Undefined, r.throwTypeError("\"super\" is only valid inside a method")
	}
	this, bound := f.thisValue()
	if !bound {
		return nil, Undefined, r.throwError(errReference,
			"\"this\" is not bound until super() has been called")
	}
	return fd.homeObject.proto, this, nil
}

// superSet writes through a super reference.
//
// The lookup starts at the home object's prototype, but the receiver is the
// instance -- so an inherited setter runs with the right `this`, and an
// assignment that finds no setter lands on the instance rather than on the
// prototype.
func (r *Runtime) superSet(f *frame, key Atom, val Value, strict bool) error {
	start, this, err := r.superRef(f)
	if err != nil {
		return err
	}
	if start == nil {
		// `class C extends null`, or a home object whose prototype was taken
		// away: the reference has no base, and assigning through one is a
		// TypeError rather than a write to the instance.
		return r.throwTypeError("\"super\" has no prototype to assign to")
	}
	_, err = r.setProp(start, key, val, this, strict)
	return err
}

// superSetFrom writes through a super reference whose base was already
// resolved, which is what a computed key needs: the base is settled before the
// key runs.
func (r *Runtime) superSetFrom(f *frame, base Value, key Atom, val Value, strict bool) error {
	this, bound := f.thisValue()
	if !bound {
		return r.throwError(errReference,
			"\"this\" is not bound until super() has been called")
	}
	if !base.IsObject() {
		if !this.IsObject() {
			return r.throwTypeError("cannot assign to a super property of %s", r.describe(this))
		}
		_, err := r.setOnReceiver(this.Object(), key, val, strict)
		return err
	}
	_, err := r.setProp(base.Object(), key, val, this, strict)
	return err
}

// getPrivate reads a private class member.
//
// Unlike an ordinary property, a missing private member is an error rather than
// undefined: the field is part of the class's shape, so reaching for one on an
// object that does not have it is a bug the language reports rather than
// papering over.
// setPrivate writes a private member, running a private setter if the class
// declared one.
//
// A private accessor is stored as an ordinary accessor property carrying the
// private flag, so the write has to look for one rather than always installing
// a data property -- otherwise `set #m(v)` would be shadowed the first time
// anything assigned to #m.
func (r *Runtime) setPrivate(o *Object, key, name Atom, val Value) error {
	// An own property and nothing else: a private member belongs to the object
	// that has it, so an object that merely inherits from an instance's
	// prototype is not one and cannot be written through.
	p := o.getOwn(key)
	if p == nil {
		// A private field is added when the object is constructed and never
		// afterwards, so writing one to an object that does not have it is a
		// mistake rather than a way to add it.
		return r.throwTypeError("private member %s is not present on this object",
			r.atoms.name(name))
	}
	if p.isAccessor() {
		a := p.getterSetter()
		if a == nil || a.setter == nil {
			return r.throwTypeError("private member %s has no setter", r.atoms.name(name))
		}
		_, err := r.call(Obj(a.setter), Obj(o), []Value{val})
		return err
	}
	if p.flags&propWritable == 0 {
		// A method is installed read-only, so this is one.
		return r.throwTypeError("private method %s is read-only", r.atoms.name(name))
	}
	p.value = val
	return nil
}

func (r *Runtime) getPrivate(o *Object, key, name Atom) (Value, error) {
	p := o.getOwn(key)
	if p == nil {
		return Undefined, r.throwTypeError(
			"private member %s is not present on this object", r.atoms.name(name))
	}
	if p.isAccessor() {
		a := p.getterSetter()
		if a == nil || a.getter == nil {
			return Undefined, r.throwTypeError(
				"private member %s has no getter", r.atoms.name(name))
		}
		return r.call(Obj(a.getter), Obj(o), nil)
	}
	return p.value, nil
}

// templateObject returns the strings a tagged template site hands its tag.
//
// It is built once per site and reused, because a tag is entitled to hang state
// off the object and to compare it against one from a later call -- which is
// the usual way a tag caches whatever it derived from the text.
func (r *Runtime) templateObject(fn *bytecode.Function, idx int) *Object {
	cache := r.templateCache[fn]
	if cache == nil {
		cache = make([]*Object, len(fn.Templates))
		r.templateCache[fn] = cache
	}
	if o := cache[idx]; o != nil {
		return o
	}

	t := fn.Templates[idx]
	cooked := make([]Value, len(t.Cooked))
	raw := make([]Value, len(t.Raw))
	for i := range t.Cooked {
		// A malformed escape has no cooked meaning. That is an error in an
		// ordinary template and merely undefined here, so that a tag with its
		// own escape conventions can read the raw text instead.
		cooked[i] = Undefined
		if t.CookedValid[i] {
			cooked[i] = Str(NewString(t.Cooked[i]))
		}
		raw[i] = Str(NewString(t.Raw[i]))
	}

	o := r.newArrayFrom(cooked)
	o.setOwnRaw(atomRaw, Obj(r.freezeArray(r.newArrayFrom(raw))), 0)
	r.freezeArray(o)
	cache[idx] = o
	return o
}

// freezeArray makes an array's elements read-only, which the strings a tag
// receives have to be: the same object is handed out again on the next call, so
// a tag that wrote to it would be writing to every later call's argument.
func (r *Runtime) freezeArray(o *Object) *Object {
	for i, el := range o.elems {
		if !isHole(el) {
			o.setOwnRaw(r.atoms.indexAtom(uint32(i)), el, propEnumerable)
		}
	}
	o.markSparse()
	o.elems = nil
	// The length is synthesized rather than stored, so its writability is a
	// flag rather than a property attribute.
	o.flags &^= objExtensible | objArrayLengthWritable
	for i := range o.props {
		o.props[i].flags &^= propWritable | propConfigurable
	}
	return o
}

// endTurn releases what the turn left behind.
//
// Two things outlive a call for reasons that stop applying once nothing is
// running: the operand stack above the live frames, which popFrame leaves dirty
// because clearing it per call was the interpreter's single largest cost, and
// the values a WeakRef handed out, which have to survive the turn they were
// read in and no longer. Doing both here costs one pass over the part of the
// stack that was actually used, once per turn.
func (r *Runtime) endTurn() {
	if r.stackHigh > r.stackTop {
		clear(r.stack[r.stackTop:r.stackHigh])
		r.stackHigh = r.stackTop
	}
	r.clearReturnedFrames()
	r.releaseKeptValues()
}
