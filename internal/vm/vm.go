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
		r.frames = append(r.frames, frame{native: fd.name, this: this})
		v, err := fd.native(r, this, args)
		r.frames = r.frames[:len(r.frames)-1]
		return v, err
	}

	if fd.closure == nil {
		return Undefined, r.throwTypeError("function has no implementation")
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
	// Locals must start clear: the window was last used by an unrelated frame.
	clear(locals)

	r.frames = append(r.frames, frame{
		cl:        cl,
		locals:    locals,
		base:      base + fn.LocalCount,
		this:      this,
		newTarget: newTarget,
		callee:    callee,
		argc:      len(args),
		args:      args,
	})
	f := &r.frames[len(r.frames)-1]

	if err := r.bindParameters(f, fn, args); err != nil {
		r.popFrame(base)
		return Undefined, err
	}

	v, err := r.execute(f)
	r.popFrame(base)
	return v, err
}

// popFrame releases a frame's stack window, closing any upvalues that pointed
// into its locals so that closures created inside it keep working.
func (r *Runtime) popFrame(base int) {
	f := &r.frames[len(r.frames)-1]
	for _, u := range f.openUpvalues {
		u.close()
	}
	// Clear the window so that the values it held can be collected rather than
	// being pinned until the slots are reused.
	clear(r.stack[base:r.stackTop])
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
	cl := f.cl
	code := cl.fn.Code
	// sp is the operand stack pointer, an absolute index into r.stack.
	sp := f.base

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

	for {
		if err := r.checkInterrupt(); err != nil {
			// An interrupt is the host stopping the script rather than a
			// JavaScript exception, so it is not catchable.
			return Undefined, err
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
			if !r.hasProp(r.global, name) {
				vmErr = r.throwReferenceError("%s is not defined", r.atoms.name(name))
				goto onError
			}
			v, err := r.getProp(r.global, name, Obj(r.global))
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpGetGlobalOpt:
			// typeof on an undeclared name must not throw.
			v, err := r.getProp(r.global, cl.names[in.A], Obj(r.global))
			if err != nil {
				vmErr = err
				goto onError
			}
			push(v)
		case bytecode.OpSetGlobal:
			if err := r.setProp(r.global, cl.names[in.A], pop(), Obj(r.global), cl.fn.Strict); err != nil {
				vmErr = err
				goto onError
			}
		case bytecode.OpDefineGlobalVar:
			name := cl.names[in.A]
			if !r.hasOwnProp(r.global, name) {
				r.global.setOwnRaw(name, Undefined, propWritable|propEnumerable)
			}
		case bytecode.OpDefineGlobalFunc:
			r.global.setOwnRaw(cl.names[in.A], pop(), propWritable|propEnumerable)

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
				var getter, setter *Object
				if in.Op == bytecode.OpDefineGetter {
					getter = fnVal.Object()
				} else {
					setter = fnVal.Object()
				}
				r.defineAccessor(obj.Object(), cl.names[in.A], getter, setter,
					propEnumerable|propConfigurable)
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
			return pop(), nil
		case bytecode.OpReturnUndef:
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
		case bytecode.OpPopCatch:
			f.handlers = f.handlers[:len(f.handlers)-1]

		// --- Iteration ----------------------------------------------------
		case bytecode.OpForInStart:
			cur, err := r.startForIn(pop())
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
		case bytecode.OpIterClose:
			r.closeIter(peek(0))
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
	*sp = h.stackDepth
	r.stack[*sp] = thrown.Value
	*sp++
	f.pc = h.pc
	return true
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
	child := &closure{fn: tmpl.fn, names: tmpl.names, consts: tmpl.consts, realm: r}

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
