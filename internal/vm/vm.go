package vm

import (
	"math"

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
		if len(r.frames) >= r.maxFrames {
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
		r.frames = r.frames[:len(r.frames)-1]
		return v, err
	}

	if fd.closure == nil {
		return Undefined, r.throwTypeError("function has no implementation")
	}

	// A generator or async function does not run its body on call. A generator
	// returns an object whose next method drives it; an async function starts
	// immediately but returns a promise at its first await.
	if fn := fd.closure.fn; isGeneratorTemplate(fn) {
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
	return r.run(fd.closure, this, args, newTarget, o)
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

	if len(r.frames) >= r.maxFrames {
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
	f.newTarget = newTarget
	f.callee = callee
	f.args = args
	f.openUpvalues = f.openUpvalues[:0]
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
	r.frames = r.frames[:len(r.frames)+1]
	return &r.frames[len(r.frames)-1]
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
// reused, which delays collection but is bounded by the stack size.
func (r *Runtime) popFrame(base int) {
	f := &r.frames[len(r.frames)-1]
	for _, u := range f.openUpvalues {
		u.close()
	}
	r.stackTop = base
	r.frames = r.frames[:len(r.frames)-1]
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
			// it was suspended inside still has to be told.
			r.closeIteratorsIn(f.base, sp)
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
			push(f.this)

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
				return Undefined, r.throwReferenceError(
					"cannot access %q before initialization", cl.fn.Locals[in.A].Name)
			}
			push(v)
		case bytecode.OpSetLocalCheck:
			if f.locals[in.A].IsUninitialized() {
				return Undefined, r.throwReferenceError(
					"cannot access %q before initialization", cl.fn.Locals[in.A].Name)
			}
			if !cl.fn.Locals[in.A].Mutable {
				return Undefined, r.throwTypeError(
					"assignment to constant variable %q", cl.fn.Locals[in.A].Name)
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
				return Undefined, r.throwReferenceError(
					"cannot access %q before initialization", cl.fn.Upvalues[in.A].Name)
			}
			push(v)
		case bytecode.OpSetUpvalueCheck:
			if !cl.fn.Upvalues[in.A].Mutable {
				return Undefined, r.throwTypeError(
					"assignment to constant variable %q", cl.fn.Upvalues[in.A].Name)
			}
			cl.upvalues[in.A].set(pop())
		case bytecode.OpCloseUpvalues:
			r.closeUpvaluesFrom(f, int(in.A))

		// --- Globals ------------------------------------------------------
		case bytecode.OpGetGlobal:
			name := cl.names[in.A]
			env := cl.scope()
			if !r.hasProp(env, name) {
				vmErr = r.throwReferenceError("%s is not defined", r.atoms.name(name))
				goto onError
			}
			v, err := r.getProp(env, name, Obj(env))
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpGetGlobalOpt:
			// typeof on an undeclared name must not throw.
			env := cl.scope()
			v, err := r.getProp(env, cl.names[in.A], Obj(env))
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpSetGlobal:
			name := cl.names[in.A]
			env := cl.scope()
			// Strict mode refuses to create a global by assignment, which is
			// the rule that catches a misspelled variable.
			if cl.fn.Strict && !r.hasProp(env, name) {
				vmErr = r.throwReferenceError("%s is not defined", r.atoms.name(name))
				goto onError
			}
			if err := r.setProp(env, name, pop(), Obj(env), cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpDefineGlobalVar:
			name := cl.names[in.A]
			env := cl.scope()
			if !r.hasOwnProp(env, name) {
				env.setOwnRaw(name, Undefined,
					propWritable|moduleBindingFlags(r.atoms.name(name)))
			}
		case bytecode.OpDefineGlobalFunc:
			name := cl.names[in.A]
			cl.scope().setOwnRaw(name, pop(),
				propWritable|moduleBindingFlags(r.atoms.name(name)))

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
			ok, err := r.deleteProp(cl.scope(), name, false)
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Bool(ok))

		case bytecode.OpDeleteProp:
			key := pop()
			obj := pop()
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
				if err := r.defineOwnProp(obj.Object(), k, val, propDefault); err != nil {
					vmErr = err
					goto onError
				}
			}
		case bytecode.OpDefineGetter, bytecode.OpDefineSetter:
			fnVal := pop()
			obj := peek(0)
			if obj.IsObject() && fnVal.IsObject() {
				r.defineHalfAccessor(obj.Object(), cl.names[in.A], fnVal.Object(),
					in.Op == bytecode.OpDefineGetter)
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
				r.defineHalfAccessor(obj.Object(), k, fnVal.Object(),
					in.Op == bytecode.OpDefineGetterIndex)
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
			x, err := r.toInt32(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Int32(^x))
		case bytecode.OpShl, bytecode.OpShr:
			b, a := pop(), pop()
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
			push(Bool(r.hasProp(obj.Object(), k)))
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
				r.closeIteratorsIn(f.base, sp)
			}
			return v, nil
		case bytecode.OpReturnUndef:
			if sp > f.base {
				r.closeIteratorsIn(f.base, sp)
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
		case bytecode.OpSetName:
			if v := peek(0); v.IsObject() {
				if fd := v.Object().fn(); fd != nil && fd.name == "" {
					fd.name = r.atoms.name(cl.names[in.A])
				}
			}

		// --- Conversions --------------------------------------------------
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
		case bytecode.OpToString:
			s, err := r.toString(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			push(Str(s))
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
			push(Str(NewString(r.atoms.name(k))))

		// --- Exceptions ---------------------------------------------------
		case bytecode.OpThrow:
			vmErr = r.throw(pop())
			goto onError
		case bytecode.OpThrowTypeError:
			vmErr = r.throwTypeError("%s", r.atoms.name(cl.names[in.A]))
			goto onError
		case bytecode.OpPushCatch:
			f.handlers = append(f.handlers, handler{pc: in.A, stackDepth: sp})
		case bytecode.OpPushFinally:
			f.handlers = append(f.handlers, handler{pc: in.A, stackDepth: sp, isFinally: true})
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
				r.closeIteratorsIn(f.base, sp)
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
			r.closeIter(peek(0))
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
		case bytecode.OpGetPrivate:
			obj := pop()
			if !obj.IsObject() {
				vmErr = r.throwTypeError("cannot read a private member of %s", r.describe(obj))
				goto onError
			}
			v, err := r.getPrivate(obj.Object(), cl.names[in.A])
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
			if err := r.setPrivate(obj.Object(), cl.names[in.A], val); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpDefinePrivate:
			val := pop()
			target := peek(0)
			if target.IsObject() {
				target.Object().setOwnRaw(cl.names[in.A], val, propWritable|propPrivate)
			}
		case bytecode.OpPrivateIn:
			obj := pop()
			// The prototype chain is walked because a private method lives on
			// the prototype rather than the instance, and `#m in obj` has to
			// find it there just as reading this.#m does.
			found := false
			if obj.IsObject() {
				for cur := obj.Object(); cur != nil; cur = cur.proto {
					if cur.getOwn(cl.names[in.A]) != nil {
						found = true
						break
					}
				}
			}
			push(Bool(found))

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
		case bytecode.OpDefineMethod:
			// A class method is not enumerable, unlike an object literal's.
			val := pop()
			target := peek(0)
			if target.IsObject() {
				target.Object().setOwnRaw(cl.names[in.A], val,
					propWritable|propConfigurable)
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
			push(Undefined)
		case bytecode.OpGetSuperProp:
			v, err := r.superGet(f, cl.names[in.A])
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpGetSuperIndex:
			key, err := r.toPropertyKey(pop())
			if err != nil {
				vmErr = err
				goto onError
			}
			v, err := r.superGet(f, key)
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
		case bytecode.OpYield, bytecode.OpAwait:
			// The operand is popped before the depth is recorded, so that the
			// saved stack holds only what is still live. Recording it first
			// would save the yielded value too, and the resumption would then
			// find an extra entry beneath the sent one.
			yielded := pop()
			f.savedSP = sp
			return Undefined, &suspendSignal{
				value: yielded,
				await: in.Op == bytecode.OpAwait,
			}
		case bytecode.OpInitialYield:
			f.savedSP = sp
			return Undefined, &suspendSignal{value: Undefined}

		case bytecode.OpNewTarget:
			push(f.newTarget)
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
	r.closeIteratorsIn(h.stackDepth, *sp)
	*sp = h.stackDepth
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
func (r *Runtime) defineHalfAccessor(o *Object, key Atom, fn *Object, isGetter bool) {
	var getter, setter *Object
	if isGetter {
		getter = fn
	} else {
		setter = fn
	}
	r.defineAccessor(o, key, getter, setter, propEnumerable|propConfigurable)
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
		r.closeIteratorsIn(h.stackDepth, *sp)
		*sp = h.stackDepth
		r.stack[*sp] = value
		*sp++
		r.stack[*sp] = Float(float64(completionReturn))
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

	o := newObject(r.proto.function, ClassFunction)
	kind := ctorKindOf(tmpl.fn)
	o.data = &funcData{
		closure:  child,
		name:     tmpl.fn.Name,
		length:   tmpl.fn.ParamCount,
		ctorKind: kind,
	}

	// A constructible function carries a fresh .prototype object, which is what
	// `new` gives the instance and where a class hangs its methods. An arrow or
	// a method is not constructible and does not get one.
	if kind != ctorNone {
		proto := newObject(r.proto.object, ClassObject)
		proto.setOwnRaw(atomConstructor, Obj(o), propWritable|propConfigurable)
		o.setOwnRaw(atomPrototype, Obj(proto), propWritable)
	}
	return o
}

func ctorKindOf(fn *bytecode.Function) ctorKind {
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
	o.flags |= objArrayLengthWritable
	if len(vals) > 0 {
		o.elems = make([]Value, len(vals))
		copy(o.elems, vals)
	}
	return o
}

// getIndexed reads a computed property, taking a fast path for an array index
// on a dense array.
func (r *Runtime) getIndexed(obj, key Value) (Value, error) {
	if obj.IsObject() && key.IsNumber() {
		o := obj.Object()
		if i := uint32(key.Number()); float64(i) == key.Number() && int(i) < len(o.elems) {
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
	if !callee.IsObject() {
		return Undefined, r.throwTypeError("%s is not a constructor", r.describe(callee))
	}
	o := callee.Object()
	if p := proxyOf(o); p != nil {
		return r.proxyConstruct(p, args, callee)
	}
	fd := o.fn()
	if fd == nil || fd.ctorKind == ctorNone {
		return Undefined, r.throwTypeError("%s is not a constructor", fd.nameOr("value"))
	}

	// The new object's prototype comes from the constructor's .prototype
	// property, falling back to Object.prototype when that is not an object.
	protoVal, err := r.getProp(o, atomPrototype, callee)
	if err != nil {
		return Undefined, err
	}
	proto := r.proto.object
	if protoVal.IsObject() {
		proto = protoVal.Object()
	}
	this := Obj(newObject(proto, ClassObject))

	res, err := r.callObject(o, this, args, callee)
	if err != nil {
		return Undefined, err
	}
	// A constructor that returns an object overrides the newly created one;
	// any other return value is ignored.
	if res.IsObject() {
		return res, nil
	}
	return this, nil
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
	for p := obj.Object().proto; p != nil; p = p.proto {
		if p == target {
			return true, nil
		}
	}
	return false, nil
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
	if !parent.IsObject() || !parent.Object().IsCallable() {
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
	if protoVal.IsObject() && parentProtoVal.IsObject() {
		protoVal.Object().proto = parentProtoVal.Object()
	}
	// Static inheritance.
	ctor.proto = parentObj

	fd.ctorKind = ctorDerived
	fd.parentCtor = parentObj
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
	// new.target is forwarded rather than replaced: inside a base constructor
	// reached through super(), new.target is the derived class the caller
	// actually wrote `new` against. An abstract base distinguishes the two --
	// `new Iterator()` is an error while `new C()` for `class C extends
	// Iterator` is not -- so the difference is observable.
	newTarget := f.newTarget
	if newTarget.IsUndefined() {
		newTarget = Obj(parent)
	}
	res, err := r.callObject(parent, f.this, args, newTarget)
	if err != nil {
		return err
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
	return nil
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
	if fd.parentCtor != nil {
		return fd.parentCtor
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
	if f.callee == nil {
		return Undefined, r.throwTypeError("\"super\" is only valid inside a method")
	}
	fd := f.callee.fn()
	if fd == nil || fd.homeObject == nil {
		return Undefined, r.throwTypeError("\"super\" is only valid inside a method")
	}
	start := fd.homeObject.proto
	if start == nil {
		return Undefined, nil
	}
	// The receiver stays the instance, so an inherited getter sees the right
	// object.
	return r.getProp(start, key, f.this)
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
func (r *Runtime) setPrivate(o *Object, key Atom, val Value) error {
	for cur := o; cur != nil; cur = cur.proto {
		p := cur.getOwn(key)
		if p == nil {
			continue
		}
		if p.isAccessor() {
			a := p.getterSetter()
			if a == nil || a.setter == nil {
				return r.throwTypeError("private member %s has no setter", r.atoms.name(key))
			}
			_, err := r.call(Obj(a.setter), Obj(o), []Value{val})
			return err
		}
		if cur == o {
			p.value = val
			return nil
		}
		// A private method found on a prototype is not writable through an
		// instance; only a field, which lives on the instance itself, is.
		return r.throwTypeError("private method %s is read-only", r.atoms.name(key))
	}
	// A private member is invisible to every reflective operation, which the
	// flag rather than the attributes expresses.
	o.setOwnRaw(key, val, propWritable|propPrivate)
	return nil
}

func (r *Runtime) getPrivate(o *Object, key Atom) (Value, error) {
	for cur := o; cur != nil; cur = cur.proto {
		if p := cur.getOwn(key); p != nil {
			if p.isAccessor() {
				a := p.getterSetter()
				if a == nil || a.getter == nil {
					return Undefined, r.throwTypeError(
						"private member %s has no getter", r.atoms.name(key))
				}
				return r.call(Obj(a.getter), Obj(o), nil)
			}
			return p.value, nil
		}
	}
	return Undefined, r.throwTypeError(
		"private member %s is not present on this object", r.atoms.name(key))
}
