package vm

import (
	"math"
	"math/big"

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
		return r.callObject(fd.boundTarget, boundThis, merged, newTarget)
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
		v, err := fd.native(r, this, args)
		r.frameDepth--
		return v, err
	}

	if fd.closure == nil {
		return Undefined, r.throwTypeError("function has no implementation")
	}

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
		gen, err := r.newGenerator(fd.closure, this, args, o, fn.Async)
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

// run executes a compiled function.
func (r *Runtime) run(cl *closure, this Value, args []Value, newTarget Value, callee *Object) (Value, error) {
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
	if top := base + fn.LocalCount + fn.MaxStack; top > r.stackHigh {
		r.stackHigh = top
	}

	// The high-water mark is what endTurn clears back to, so that a value left
	// behind by a returning frame does not stay reachable until its slot is
	// reused.
	if top := base + fn.LocalCount + fn.MaxStack; top > r.stackHigh {
		r.stackHigh = top
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
		}
	}
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
	return v, err
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

// bindLexicalParameters fills the slots of a parameter list whose bindings are
// initialized one at a time.
//
// Such a parameter is not bound until the prologue reaches it, so one that has
// not been reached has to be distinguishable from one that has:
// `function f(a = b, b) {}` is a reference error, and so is
// `function f(a = a) {}`. What makes a default run is the argument being
// undefined -- passed or missing, which nothing can tell apart -- so both leave
// the marker in place, and the prologue replaces it.
func bindLexicalParameters(slots []Value, args []Value) {
	for i := range slots {
		if i < len(args) && !args[i].IsUndefined() {
			slots[i] = args[i]
		} else {
			slots[i] = uninitialized
		}
	}
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
	cl := f.cl
	code := cl.fn.Code
	// sp is the operand stack pointer, an absolute index into r.stack.
	sp := startSP

	// push and pop are written against the local sp so that the compiler keeps
	// it in a register; f.base and r.stack do not change during the loop.
	push := func(v Value) {
		r.stack[sp] = v
		sp++
	}
	pop := func() Value {
		sp--
		return r.stack[sp]
	}
	peek := func(n int) Value { return r.stack[sp-1-n] }

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
		if !r.unwindToFinally(f, &sp, ret.value) {
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
		if !r.unwindToHandler(f, &sp, vmErr) {
			return Undefined, vmErr
		}
		vmErr = nil
	}

	for {
		// The cancellation check happens on every instruction, so the counter
		// is decremented inline and only the rare expiry is a call. Reading a
		// context's Done channel per instruction would dominate the loop, and
		// so, measurably, did calling even a trivial helper.
		r.interruptCounter--
		if r.interruptCounter <= 0 {
			if err := r.checkInterruptNow(); err != nil {
				// An interrupt is the host stopping the script rather than a
				// JavaScript exception, so it is not catchable.
				return Undefined, err
			}
		}

		in = code[f.pc]
		f.pc++

		switch in.Op {
		case bytecode.OpNop:

		// --- Constants ----------------------------------------------------
		case bytecode.OpPushConst:
			push(cl.consts[in.A])
		case bytecode.OpPushUndef:
			push(Undefined)
		case bytecode.OpInitParam:
			// A parameter's slot is its position, so the argument that fills it
			// is at the same index.
			if int(in.A) < len(f.args) {
				f.locals[in.A] = f.args[in.A]
			} else {
				f.locals[in.A] = Undefined
			}
		case bytecode.OpParamNeedsDefault:
			push(Bool(int(in.A) >= len(f.args) || f.args[in.A].IsUndefined()))
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
			push(uninitialized)
		case bytecode.OpPushNull:
			push(Null)
		case bytecode.OpPushTrue:
			push(True)
		case bytecode.OpPushFalse:
			push(False)
		case bytecode.OpPushInt:
			push(Int32(int32(in.A)))
		case bytecode.OpPushEmptyString:
			push(Str(emptyString))
		case bytecode.OpPushThis:
			v, bound := f.thisValue()
			if !bound {
				vmErr = r.throwError(errReference,
					"\"this\" is not bound until super() has been called")
				goto onError
			}
			push(v)

		// --- Stack --------------------------------------------------------
		case bytecode.OpDup:
			push(peek(0))
		case bytecode.OpDup2:
			a, b := peek(1), peek(0)
			push(a)
			push(b)
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
			push(f.locals[in.A])
		case bytecode.OpSetLocal:
			f.locals[in.A] = pop()
		case bytecode.OpPutLocal:
			f.locals[in.A] = peek(0)
		case bytecode.OpInitLocal:
			f.locals[in.A] = pop()
		case bytecode.OpGetLocalCheck:
			v := f.locals[in.A]
			if v.IsUninitialized() {
				vmErr = r.throwReferenceError(
					"cannot access %q before initialization", cl.fn.Locals[in.A].Name)
				goto onError
			}
			push(v)
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
			f.locals[in.A] = pop()

		// --- Upvalues -----------------------------------------------------
		case bytecode.OpGetUpvalue:
			push(cl.upvalues[in.A].get())
		case bytecode.OpSetUpvalue:
			cl.upvalues[in.A].set(pop())
		case bytecode.OpInitUpvalue:
			cl.upvalues[in.A].set(pop())
		case bytecode.OpGetUpvalueCheck:
			v := cl.upvalues[in.A].get()
			if v.IsUninitialized() {
				vmErr = r.throwReferenceError(
					"cannot access %q before initialization", cl.fn.Upvalues[in.A].Name)
				goto onError
			}
			push(v)
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
			cl.upvalues[in.A].set(pop())
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
					push(p.value)
					break
				}
			}
			if p := r.globalLexProp(env, name); p != nil {
				if p.value.IsUninitialized() {
					vmErr = r.throwReferenceError(
						"cannot access %q before it is initialized", r.atoms.name(name))
					goto onError
				}
				push(p.value)
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
					push(p.value)
					break
				}
			}
			if i := env.findOwn(name); i >= 0 {
				if p := &env.props[i]; p.flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 {
					push(p.value)
					break
				}
			}
			if env != r.global {
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
						push(p.value)
						break
					}
				}
			}
			// The read comes first and the existence check only follows an
			// undefined result: an undeclared name is the rare case, and
			// asking twice for every global read is not worth paying for it.
			v, err := r.getProp(env, name, Obj(env))
			if err != nil {
				vmErr = err
				goto onError
			}
			if v.IsUndefined() && !r.hasProp(env, name) {
				vmErr = r.throwReferenceError("%s is not defined", r.atoms.name(name))
				goto onError
			}
			push(v)
		case bytecode.OpGetGlobalOpt:
			// typeof on an undeclared name must not throw. A lexical binding
			// in its dead zone is declared, though, so that one still does.
			env := cl.scope()
			if f.evalVars != nil {
				if p := evalVarProp(f.evalVars, cl.names[in.A]); p != nil {
					push(p.value)
					break
				}
			}
			if p := r.globalLexProp(env, cl.names[in.A]); p != nil {
				if p.value.IsUninitialized() {
					vmErr = r.throwReferenceError("cannot access %q before it is initialized",
						r.atoms.name(cl.names[in.A]))
					goto onError
				}
				push(p.value)
				break
			}
			if env != r.global {
				if p := r.moduleLexProp(env, cl.names[in.A]); p != nil {
					if p.flags&propUninit != 0 {
						vmErr = r.throwReferenceError("cannot access %q before it is initialized",
							r.atoms.name(cl.names[in.A]))
						goto onError
					}
					if p.flags&(propAccessor|propPrivate|propDeleted) == 0 {
						push(p.value)
						break
					}
				}
			}
			v, err := r.getProp(env, cl.names[in.A], Obj(env))
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
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
			case env != r.global && r.moduleLexProp(env, name) != nil:
			default:
				found = r.hasProp(env, name)
			}
			push(Bool(found))
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
					p.value = pop()
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
				p.value = pop()
				break
			}
			if env != r.global {
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
						p.value = pop()
						break
					}
					// What is left is an imported binding, stored as an
					// accessor with no setter, which the assignment below
					// refuses for us.
				}
			}
			// Strict mode refuses to create a global by assignment, which is
			// the rule that catches a misspelled variable.
			if cl.fn.Strict && !r.hasProp(env, name) {
				vmErr = r.throwReferenceError("%s is not defined", r.atoms.name(name))
				goto onError
			}
			if _, err := r.setProp(env, name, pop(), Obj(env), cl.fn.Strict); err != nil {
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
				f.evalVars.setOwnRaw(name, pop(), propWritable|propConfigurable)
				break
			}
			flags := propWritable | moduleBindingFlags(r.atoms.name(name))
			if in.B != 0 {
				flags |= propConfigurable
			}
			env := cl.scope()
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
					p.value = pop()
					break
				}
			}
			env.setOwnRaw(name, pop(), flags)
		case bytecode.OpDeclareGlobalLex:
			name := cl.names[in.A]
			flags := propConfigurable
			if in.B != 0 {
				flags |= propWritable
			}
			r.globalLex.setOwnRaw(name, uninitialized, flags)
		case bytecode.OpInitGlobalLex:
			r.globalLex.setOwnRaw(cl.names[in.A], pop(),
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
				p.value = pop()
				p.flags &^= propUninit
			} else {
				pop()
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
			v, err := r.getValueProp(pop(), cl.names[in.A])
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpGetPropThis:
			// Leave the receiver beneath the value so a method call can use it.
			recv := peek(0)
			v, err := r.getValueProp(recv, cl.names[in.A])
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpSetProp:
			val := pop()
			obj := pop()
			if err := r.setValueProp(obj, cl.names[in.A], val, cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpGetIndex:
			key := pop()
			obj := pop()
			v, err := r.getIndexed(obj, key)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpGetIndexThis:
			key := pop()
			recv := peek(0)
			v, err := r.getIndexed(recv, key)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpSetIndex:
			val := pop()
			key := pop()
			obj := pop()
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
				push(True)
				break
			}
			if r.globalLexProp(cl.scope(), name) != nil {
				// A lexical binding is not a property and cannot be removed.
				push(False)
				break
			}
			ok, err := r.deleteProp(cl.scope(), name, false)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Bool(ok))

		case bytecode.OpDeleteProp:
			key := pop()
			obj := pop()
			if obj.IsNullish() {
				// There is nothing to delete from, which is a TypeError before
				// the key is even converted.
				vmErr = r.throwTypeError("cannot delete a property of %s", r.describe(obj))
				goto onError
			}
			k, err := r.toPropertyKey(key)
			if err != nil {
				vmErr = err
				goto onError
			}
			if !obj.IsObject() {
				push(True)
				break
			}
			ok, err := r.deleteProp(obj.Object(), k, cl.fn.Strict)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Bool(ok))
		case bytecode.OpGetLength:
			v, err := r.getValueProp(pop(), atomLength)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpDefineField:
			val := pop()
			obj := peek(0)
			if obj.IsObject() {
				if err := r.defineOwnProp(obj.Object(), cl.names[in.A], val, propDefault); err != nil {
					vmErr = err
					goto onError
				}
			}
		case bytecode.OpDefineIndex:
			val := pop()
			key := pop()
			obj := peek(0)
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
			fnVal := pop()
			obj := peek(0)
			if obj.IsObject() && fnVal.IsObject() {
				r.defineHalfAccessor(obj.Object(), cl.names[in.A], fnVal.Object(),
					in.Op == bytecode.OpDefineGetter, memberFlags(in.B))
			}
		case bytecode.OpSetFuncName:
			if fnVal := peek(0); fnVal.IsObject() {
				if fd := fnVal.Object().fn(); fd != nil {
					fd.name = functionNameFromKey(peek(1), in.A)
				}
			}
		case bytecode.OpDefineGetterIndex, bytecode.OpDefineSetterIndex:
			fnVal := pop()
			key := pop()
			obj := peek(0)
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
			val := pop()
			obj := peek(0)
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
			b, a := pop(), pop()
			// The overwhelmingly common case is two numbers, so it is tested
			// before the general algorithm.
			if a.IsNumber() && b.IsNumber() {
				push(Float(a.Number() + b.Number()))
				break
			}
			v, err := r.add(a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod,
			bytecode.OpPow:
			b, a := pop(), pop()
			if a.IsNumber() && b.IsNumber() {
				push(Float(numericOp(in.Op, a.Number(), b.Number())))
				break
			}
			v, err := r.arith(in.Op, a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpNeg:
			a := pop()
			if a.IsNumber() {
				push(Float(-a.Number()))
				break
			}
			v, err := r.negate(a)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpPos:
			n, err := r.toNumber(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Float(n))
		case bytecode.OpInc, bytecode.OpDec:
			a := pop()
			if a.IsNumber() {
				if in.Op == bytecode.OpInc {
					push(Float(a.Number() + 1))
				} else {
					push(Float(a.Number() - 1))
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
				push(v)
				break
			}
			push(Float(n.Number() + delta))

		// --- Bitwise ------------------------------------------------------
		case bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor:
			b, a := pop(), pop()
			if !a.IsNumber() || !b.IsNumber() {
				v, done, err := r.bigBitwise(in.Op, a, b)
				if err != nil {
					vmErr = err
					goto onError
				}
				if done {
					push(v)
					break
				}
			}
			x, err := r.toInt32(a)
			if err != nil {
				vmErr = err
				goto onError
			}
			y, err := r.toInt32(b)
			if err != nil {
				vmErr = err
				goto onError
			}
			switch in.Op {
			case bytecode.OpBitAnd:
				push(Int32(x & y))
			case bytecode.OpBitOr:
				push(Int32(x | y))
			default:
				push(Int32(x ^ y))
			}
		case bytecode.OpBitNot:
			v := pop()
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
					push(Big(out))
					break
				}
				v = n
			}
			x, err := r.toInt32(v)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Int32(^x))
		case bytecode.OpShl, bytecode.OpShr:
			b, a := pop(), pop()
			if !a.IsNumber() || !b.IsNumber() {
				v, done, err := r.bigBitwise(in.Op, a, b)
				if err != nil {
					vmErr = err
					goto onError
				}
				if done {
					push(v)
					break
				}
			}
			x, err := r.toInt32(a)
			if err != nil {
				vmErr = err
				goto onError
			}
			y, err := r.toUint32(b)
			if err != nil {
				vmErr = err
				goto onError
			}
			// Only the low five bits of the shift count are used.
			if in.Op == bytecode.OpShl {
				push(Int32(x << (y & 31)))
			} else {
				push(Int32(x >> (y & 31)))
			}
		case bytecode.OpUShr:
			b, a := pop(), pop()
			if !a.IsNumber() || !b.IsNumber() {
				// Unsigned shift has no BigInt form: a BigInt has no width for
				// the sign bit to be shifted out of.
				na, err := r.toNumeric(a)
				if err != nil {
					vmErr = err
					goto onError
				}
				nb, err := r.toNumeric(b)
				if err != nil {
					vmErr = err
					goto onError
				}
				if na.IsBigInt() || nb.IsBigInt() {
					vmErr = r.throwTypeError("BigInt has no unsigned right shift")
					goto onError
				}
				a, b = na, nb
			}
			x, err := r.toUint32(a)
			if err != nil {
				vmErr = err
				goto onError
			}
			y, err := r.toUint32(b)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Uint32(x >> (y & 31)))

		// --- Comparison ---------------------------------------------------
		case bytecode.OpEq, bytecode.OpNe:
			b, a := pop(), pop()
			eq, err := r.looseEquals(a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Bool(eq == (in.Op == bytecode.OpEq)))
		case bytecode.OpStrictEq:
			b, a := pop(), pop()
			push(Bool(a.StrictEquals(b)))
		case bytecode.OpStrictNe:
			b, a := pop(), pop()
			push(Bool(!a.StrictEquals(b)))
		case bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe:
			b, a := pop(), pop()
			// Two numbers are compared directly, which also gets the NaN
			// behaviour right without going through cmpUndefined.
			if a.IsNumber() && b.IsNumber() {
				push(Bool(compareFloats(in.Op, a.Number(), b.Number())))
				break
			}
			c, err := r.compare(a, b)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Bool(relationalResult(in.Op, c)))
		case bytecode.OpIn:
			obj, key := pop(), pop()
			if !obj.IsObject() {
				vmErr = r.throwTypeError("the right operand of \"in\" must be an object")
				goto onError
			}
			k, err := r.toPropertyKey(key)
			if err != nil {
				vmErr = err
				goto onError
			}
			has, err := r.hasPropErr(obj.Object(), k)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Bool(has))
		case bytecode.OpInstanceOf:
			ctor, obj := pop(), pop()
			ok, err := r.instanceOf(obj, ctor)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Bool(ok))
		case bytecode.OpNot:
			push(Bool(!pop().Truthy()))
		case bytecode.OpTypeOf:
			push(Str(NewString(pop().TypeOf())))
		case bytecode.OpIsNullish:
			push(Bool(peek(0).IsNullish()))

		// --- Control flow -------------------------------------------------
		case bytecode.OpJump:
			f.pc = in.A
		case bytecode.OpJumpIfFalse:
			if !pop().Truthy() {
				f.pc = in.A
			}
		case bytecode.OpJumpIfTrue:
			if pop().Truthy() {
				f.pc = in.A
			}
		case bytecode.OpJumpIfFalseKeep:
			if !peek(0).Truthy() {
				f.pc = in.A
			} else {
				sp--
			}
		case bytecode.OpJumpIfTrueKeep:
			if peek(0).Truthy() {
				f.pc = in.A
			} else {
				sp--
			}
		case bytecode.OpJumpIfNullish:
			// Used by optional chaining, which keeps the value on both paths:
			// as the chain's result when short-circuiting, and as the receiver
			// of the next link otherwise.
			if peek(0).IsNullish() {
				f.pc = in.A
			}
		case bytecode.OpJumpIfNotNullish:
			if !peek(0).IsNullish() {
				f.pc = in.A
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
			push(v)
		case bytecode.OpDirectEval:
			var args []Value
			var callee, recv Value
			if in.B == bytecode.DirectEvalSpread {
				// The arguments were gathered into an array, which a spread
				// element forces. A spread does not make the call any less
				// direct: what decides is the name the callee was written as.
				list := pop()
				callee = pop()
				recv = pop()
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
				// resolution found, which a `with` object's own eval needs.
				v, err := r.call(callee, recv, args)
				if err != nil {
					vmErr = err
					goto onError
				}
				push(v)
			case !src.IsString():
				// eval returns anything that is not a string unchanged, which
				// is what makes eval(42) safe.
				push(src)
			default:
				v, err := r.evalDirect(f, cl.fn.EvalScopes[in.A], src.String().Go())
				if err != nil {
					vmErr = err
					goto onError
				}
				push(v)
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
			push(v)
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
			push(v)
		case bytecode.OpReturn:
			v := pop()
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
			push(Obj(r.makeClosure(f, cl.consts[in.A])))
		case bytecode.OpNewObject:
			push(Obj(newObject(r.proto.object, ClassObject)))
		case bytecode.OpNewArray:
			n := int(in.A)
			arr := r.newArrayFrom(r.stack[sp-n : sp])
			sp -= n
			push(Obj(arr))
		case bytecode.OpArrayPush:
			val := pop()
			arr := peek(0)
			if arr.IsObject() {
				o := arr.Object()
				o.elems = append(o.elems, val)
			}
		case bytecode.OpConcat:
			n := int(in.A)
			parts := r.stack[sp-n : sp]
			var out *String
			for i, p := range parts {
				s, err := r.toString(p)
				if err != nil {
					vmErr = err
					goto onError
				}
				if i == 0 {
					out = s
				} else {
					out = out.Concat(s)
				}
			}
			sp -= n
			if out == nil {
				out = emptyString
			}
			push(Str(out))
		case bytecode.OpTemplateObject:
			push(Obj(r.templateObject(cl.fn, int(in.A))))
		case bytecode.OpSetName:
			if v := peek(0); v.IsObject() {
				if fd := v.Object().fn(); fd != nil && fd.name == "" {
					fd.name = r.atoms.name(cl.names[in.A])
				}
			}

		// --- Conversions --------------------------------------------------
		// --- `with` ------------------------------------------------------
		case bytecode.OpWithPush:
			o, err := r.withObject(pop())
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
					push(Undefined)
				} else {
					push(Obj(o))
				}
				push(v)
			case bytecode.OpWithTypeof:
				push(Str(NewString(v.TypeOf())))
			default:
				push(v)
			}
			f.pc = in.B
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
			push(v)
			f.pc = in.B
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
			v := peek(0)
			// The write asks again whether the binding is there: a strict
			// reference to one that has since gone is a ReferenceError rather
			// than a property being created.
			if err := r.withWrite(base.Object(), name, v, cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
			r.stack[sp-2] = v
			sp--
			f.pc = in.B
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
			if err := r.withWrite(o, name, peek(0), cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
			f.pc = in.B
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
			push(Bool(ok))
			f.pc = in.B

		case bytecode.OpCheckCoercible:
			if v := peek(0); v.IsNullish() {
				vmErr = r.throwTypeError("cannot destructure %s", r.describe(v))
				goto onError
			}
		case bytecode.OpToObject:
			o, err := r.toObject(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Obj(o))
		case bytecode.OpToNumber:
			n, err := r.toNumber(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Float(n))
		case bytecode.OpToNumeric:
			n, err := r.toNumeric(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			push(n)
		case bytecode.OpToString:
			s, err := r.toString(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Str(s))
		case bytecode.OpToPropertyKeyOfBase:
			// The object the key will be looked up on sits beneath it, and is
			// checked first: a key whose toString throws must not run at all
			// when there is nothing to look it up on.
			if base := peek(1); base.IsNullish() {
				vmErr = r.throwTypeError("cannot read property of %s", r.describe(base))
				goto onError
			}
			fallthrough
		case bytecode.OpToPropertyKey:
			// A string or symbol is already a property key and must be left
			// alone; stringifying a symbol here would turn a computed symbol
			// key into an ordinary named one.
			v := peek(0)
			if v.IsString() || v.IsSymbol() {
				break
			}
			k, err := r.toPropertyKey(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			// The conversion may still produce a symbol, which an object's
			// Symbol.toPrimitive can return: the key keeps whichever kind it
			// turned out to be.
			push(r.keyToValue(k))

		// --- Exceptions ---------------------------------------------------
		case bytecode.OpThrow:
			vmErr = r.throw(pop())
			goto onError
		case bytecode.OpThrowTypeError:
			vmErr = r.throwTypeError("%s", r.atoms.name(cl.names[in.A]))
			goto onError
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
			kind := pop()
			val := pop()
			switch completionKind(kind.Number()) {
			case completionThrow:
				vmErr = r.throw(val)
				goto onError
			case completionReturn:
				// An enclosing finally has its own claim on the return: the
				// value keeps unwinding until no finally is left.
				if r.unwindToFinally(f, &sp, val) {
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
			push(Obj(r.newArrayFrom(rest)))
		case bytecode.OpGetArguments:
			push(Obj(r.newArgumentsObject(f)))
		case bytecode.OpArrayRest:
			src := pop()
			start := int(in.A)
			var rest []Value
			if src.IsObject() {
				if el := src.Object().elems; start < len(el) {
					rest = el[start:]
				}
			}
			push(Obj(r.newArrayFrom(rest)))
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
			push(rest)

		// --- Calls with spread --------------------------------------------
		case bytecode.OpCallSpread, bytecode.OpNewSpread:
			argsVal := pop()
			callee := pop()
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
				v, err = r.call(callee, pop(), callArgs)
			}
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)

		// --- Iteration ----------------------------------------------------
		case bytecode.OpForInStart:
			cur, err := r.startForIn(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			push(cur)
		case bytecode.OpForAwaitOfStart:
			cur, err := r.startForAwaitOf(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			push(cur)
		case bytecode.OpForOfStart:
			cur, err := r.startForOf(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			push(cur)
		case bytecode.OpGetPropUnder:
			v, err := r.getValueProp(peek(int(in.B)), cl.names[in.A])
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpGetIndexUnder:
			v, err := r.getIndexed(peek(int(in.A)+1), peek(int(in.A)))
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpIterStep:
			// The cursor sits A slots below the top: a target's reference may
			// have been pushed above it, and is evaluated before the value it
			// receives is asked for.
			v, err := r.iterStep(peek(int(in.A)))
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpIterRest:
			v, err := r.iterRest(peek(int(in.A)))
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpIterCloseNormal:
			if err := r.iterCloseNormal(pop()); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpIterNextOrJump:
			// The cursor stays on the stack so that the loop can close it.
			v, ok, err := r.iterNext(peek(0))
			if err != nil {
				vmErr = err
				goto onError
			}
			if !ok {
				f.pc = in.A
				break
			}
			push(v)
		case bytecode.OpAsyncIterNext:
			v, err := r.asyncIterNext(peek(0))
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
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
			kind := pop()
			sent := pop()
			res, done, err := r.iterResume(peek(0), sent, resumeMode(kind.Number()),
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
				push(res)
				f.pc = in.B
				break
			}
			push(res)
		case bytecode.OpIterUnpackDelegate:
			res := pop()
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
					push(res)
				} else {
					val, err := r.getValueProp(res, atomValue)
					if err != nil {
						vmErr = err
						goto onError
					}
					push(val)
				}
				break
			}
			val, err := r.getValueProp(res, atomValue)
			if err != nil {
				vmErr = err
				goto onError
			}
			if st := iterStateOf(peek(0)); st != nil {
				st.done = true
			}
			// The value goes to the code after the delegation, which decides
			// what it means: the delegate's return value, or -- when the outer
			// generator was the one asked to return -- what it returns.
			push(val)
			f.pc = in.A
		case bytecode.OpIterSend, bytecode.OpIterSendAsync:
			sent := pop()
			res, err := r.iterSend(peek(0), sent, in.Op == bytecode.OpIterSendAsync)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(res)
		case bytecode.OpIterUnpack:
			res := pop()
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
				push(res)
				break
			}
			val, err := r.getValueProp(res, atomValue)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(val)
			if done.Truthy() {
				// The cursor is exhausted, so nothing is left to close.
				if st := iterStateOf(peek(1)); st != nil {
					st.done = true
				}
				f.pc = in.A
			}
		case bytecode.OpIterResultOrJump:
			res := pop()
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
				f.pc = in.A
				break
			}
			val, err := r.getValueProp(res, atomValue)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(val)

		case bytecode.OpIterToArray:
			arr, err := r.iterToArray(pop(), in.A)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(arr)
		case bytecode.OpIterClose:
			// Leaving a loop by break or continue is not a throw, so a failure
			// in the iterator's return method is the result rather than
			// something to swallow -- and so is a return method that hands back
			// something other than an object.
			if err := r.iterCloseNormal(peek(0)); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpSpreadIter:
			vals, err := r.spreadToStack(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			target := peek(0)
			if target.IsObject() {
				target.Object().elems = append(target.Object().elems, vals...)
			}
		case bytecode.OpArraySpread:
			src := pop()
			target := peek(0)
			if target.IsObject() {
				if err := r.spreadInto(target.Object(), src); err != nil {
					vmErr = err
					goto onError
				}
			}
		case bytecode.OpCopyDataProps:
			src := pop()
			target := peek(0)
			if target.IsObject() && !src.IsNullish() {
				if err := r.copyDataProps(target.Object(), src); err != nil {
					vmErr = err
					goto onError
				}
			}

		// --- Private class members ----------------------------------------
		case bytecode.OpPrivateName:
			push(r.newPrivateName(cl.names[in.A]))
		case bytecode.OpGetPrivate:
			obj := pop()
			if !obj.IsObject() {
				vmErr = r.throwTypeError("cannot read a private member of %s", r.describe(obj))
				goto onError
			}
			v, err := r.getPrivate(obj.Object(), privateKey(f, cl, in.B), cl.names[in.A])
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpSetPrivate:
			val := pop()
			obj := pop()
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
			val := pop()
			target := peek(0)
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
			val := pop()
			target := peek(0)
			if target.IsObject() && val.IsObject() {
				r.definePrivateAccessor(target.Object(), privateKey(f, cl, in.B),
					val.Object(), in.Op == bytecode.OpDefinePrivateGetter)
			}
		case bytecode.OpPrivateIn:
			obj := pop()
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
			push(Bool(obj.Object().getOwn(privateKey(f, cl, in.B)) != nil))
		case bytecode.OpNewPrivateMethods:
			push(r.newPrivateMethods())
		case bytecode.OpAddPrivateMethod, bytecode.OpAddPrivateGetter,
			bytecode.OpAddPrivateSetter:
			fnVal := pop()
			list := pop()
			r.addPrivateMethod(list, privateKey(f, cl, in.B), fnVal, in.Op)
		case bytecode.OpInstallPrivateMethods:
			this, bound := f.thisValue()
			if !bound {
				vmErr = r.throwError(errReference,
					"\"this\" is not bound until super() has been called")
				goto onError
			}
			if err := r.installPrivateMethods(this, pop()); err != nil {
				vmErr = err
				goto onError
			}

		// --- Classes ------------------------------------------------------
		case bytecode.OpNewClass:
			// stack: ctor parent
			parent := pop()
			ctorVal := peek(0)
			if err := r.linkClass(ctorVal, parent); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpSetHomeObject:
			// `super` inside the method resolves against the home object,
			// which sits A slots below the function -- one normally, two when
			// a computed key was pushed in between.
			if fnVal := peek(0); fnVal.IsObject() {
				if home := peek(int(in.A)); fnVal.Object().fn() != nil && home.IsObject() {
					fnVal.Object().fn().homeObject = home.Object()
				}
			}
		case bytecode.OpSetFieldInit:
			init := pop()
			if ctor := peek(0); ctor.IsObject() && init.IsObject() {
				if fd := ctor.Object().fn(); fd != nil {
					fd.fieldInit = init.Object()
				}
			}
		case bytecode.OpDefineMethod:
			// A class method is not enumerable, unlike an object literal's.
			val := pop()
			target := peek(0)
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
				if a := pop(); a.IsObject() {
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
			push(f.this)
		case bytecode.OpGetSuperProp:
			v, err := r.superGet(f, cl.names[in.A])
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpSetSuperProp:
			val := pop()
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
				push(Null)
				break
			}
			push(Obj(base))
		case bytecode.OpSetSuperIndex:
			val := pop()
			rawKey := pop()
			base := pop()
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
			rawKey := pop()
			base := pop()
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
			push(v)

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
			push(v)

		// --- Suspension ---------------------------------------------------
		case bytecode.OpYield, bytecode.OpAwait, bytecode.OpYieldStar:
			// The operand is popped before the depth is recorded, so that the
			// saved stack holds only what is still live. Recording it first
			// would save the yielded value too, and the resumption would then
			// find an extra entry beneath the sent one.
			yielded := pop()
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
			push(f.newTarget)
		case bytecode.OpImportMeta:
			push(r.importMeta(cl.env))
		case bytecode.OpPushCallee:
			if f.callee == nil {
				push(Undefined)
				break
			}
			push(Obj(f.callee))

		default:
			vmErr = r.throwTypeError("unimplemented opcode %s", in.Op)
			goto onError
		}
		continue

	onError:
		// An exception unwinds to the innermost handler registered in this
		// frame. With none, it propagates to the caller, which repeats the
		// search in its own frame.
		if !r.unwindToHandler(f, &sp, vmErr) {
			// The exception leaves this frame entirely, so every loop it was
			// inside is being abandoned.
			r.closeIteratorsIn(f.base, sp)
			return Undefined, vmErr
		}
		vmErr = nil
	}
}

// unwindToHandler transfers control to the innermost catch handler of a frame,
// reporting false when the frame has none and the exception must propagate.
//
// Only a JavaScript exception is catchable. A host interruption -- a cancelled
// context, or the stack limit being reached -- deliberately is not, so that a
// script cannot defeat its own sandbox with try/catch.
func (r *Runtime) unwindToHandler(f *frame, sp *int, err error) bool {
	thrown, ok := err.(*Thrown)
	if !ok || len(f.handlers) == 0 {
		return false
	}
	h := f.handlers[len(f.handlers)-1]
	f.handlers = f.handlers[:len(f.handlers)-1]

	// The stack may hold a partly-built expression from the point of the
	// throw, so it is cut back to the depth the handler was registered at
	// before the thrown value is pushed for the catch clause to bind.
	depth := f.base + h.stackDepth
	r.closeIteratorsIn(depth, *sp)
	*sp = depth
	r.stack[*sp] = thrown.Value
	*sp++
	if h.isFinally {
		// A finally clause reproduces the original completion after it runs,
		// so it receives a record rather than a bare value.
		r.stack[*sp] = Float(float64(completionThrow))
		*sp++
	}
	f.pc = h.pc
	return true
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
	if env != r.global || len(r.globalLex.props) == 0 {
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
func (r *Runtime) unwindToFinally(f *frame, sp *int, value Value) bool {
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
		if err := r.closeIteratorsReturning(depth, *sp); err != nil {
			value, kind = thrownValue(err), completionThrow
		}
		*sp = depth
		r.stack[*sp] = value
		*sp++
		r.stack[*sp] = Float(float64(kind))
		*sp++
		f.pc = h.pc
		return true
	}
	return false
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
	tmpl, _ := c.ref.(*closure)
	// The environment is inherited, so a function declared in a module sees
	// the module's bindings rather than only the globals.
	child := &closure{
		fn: tmpl.fn, names: tmpl.names, consts: tmpl.consts,
		realm: r, env: f.cl.env,
	}

	child.upvalues = make([]*upvalue, len(tmpl.fn.Upvalues))
	for i, desc := range tmpl.fn.Upvalues {
		if desc.FromParent {
			child.upvalues[i] = r.captureLocal(f, int(desc.Index))
		} else {
			child.upvalues[i] = f.cl.upvalues[desc.Index]
		}
	}

	o := newObject(r.funcProtoFor(tmpl.fn), ClassFunction)
	kind := ctorKindOf(tmpl.fn)
	fd := &funcData{
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
	o.data = fd

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
		return Undefined, r.throwError(errReference,
			"a derived constructor must call super() before returning")
	}
	return f.thisRef.value, nil
}

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
	o := newObject(r.proto.array, ClassArray)
	if len(vals) > 0 {
		o.elems = make([]Value, len(vals))
		copy(o.elems, vals)
	}
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
			int(i) < len(o.elems) && o.flags&objMappedArguments == 0 {
			if v := o.elems[i]; !isHole(v) {
				return v, nil
			}
		}
	}
	k, err := r.toPropertyKey(key)
	if err != nil {
		return Undefined, err
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
		return p.target != nil && isConstructor(Obj(p.target))
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
		return r.constructNative(o, fd, args, newTarget, target != o)
	}

	protoVal, err := r.getProp(target, atomPrototype, Obj(target))
	if err != nil {
		return Undefined, err
	}
	proto := r.proto.object
	if protoVal.IsObject() {
		proto = protoVal.Object()
	} else if p := proxyOf(target); p != nil && p.revoked {
		// A constructor that named no prototype falls back to the one of the
		// realm it came from, and a revoked proxy no longer has a realm to be
		// asked about.
		return Undefined, r.throwTypeError("cannot perform an operation on a revoked proxy")
	}
	this := Obj(newObject(proto, ClassObject))

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
func (r *Runtime) constructNative(o *Object, fd *funcData, args []Value,
	newTarget Value, derived bool) (Value, error) {
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
	}
	return res, nil
}

func (fd *funcData) nameOr(fallback string) string {
	if fd == nil || fd.name == "" {
		return fallback
	}
	return fd.name
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
		res, err := r.call(hasInstance, ctor, []Value{obj})
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
		return Str(sa.Concat(sb)), nil
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
		if !n.IsInt64() || n.Int64() > 1<<24 {
			if left {
				return Undefined, false, r.throwRangeError("BigInt shift is too large")
			}
			// Shifting right past every bit leaves the sign: zero, or minus
			// one for a negative value.
			if x.Sign() < 0 {
				out.V.SetInt64(-1)
			}
			break
		}
		if left {
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

// jsPow implements the ** operator, which differs from math.Pow in one case:
// any base raised to a NaN exponent is NaN, including 1 ** NaN.
func jsPow(a, b float64) float64 {
	if math.IsNaN(b) {
		return math.NaN()
	}
	// math.Pow(1, ±Inf) is 1 in Go but NaN in JavaScript.
	if (a == 1 || a == -1) && math.IsInf(b, 0) {
		return math.NaN()
	}
	return math.Pow(a, b)
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
	case bytecode.OpAdd:
		out.V.Add(&a.V, &b.V)
	case bytecode.OpSub:
		out.V.Sub(&a.V, &b.V)
	case bytecode.OpMul:
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
		if !b.V.IsInt64() {
			return Undefined, r.throwRangeError("BigInt exponent is too large")
		}
		out.V.Exp(&a.V, &b.V, nil)
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
	r.releaseKeptValues()
}
