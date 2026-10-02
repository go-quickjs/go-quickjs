package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// pureMissLimit is how many calls of a closure may find its body's fast
// paths closed before pureCall stops trying them: a body whose property is
// a getter, or whose operand is an object, would otherwise be tried and
// given up on at every call.
const pureMissLimit = 64

// pureCall answers a call of a function whose body LeafPure marks without a
// frame: the body is evaluated here, over a small stack of its own, with the
// arguments read where the caller has them. Each instruction takes only its
// fast path -- a plain data property, a global held in the global object's
// table, numbers for an operator, primitives for == -- where nothing can run
// code or throw. Where one cannot, the call reports false, and is made as
// any other is: the body stores nothing and calls nothing, so nothing it did
// before giving up can be seen.
func (r *Runtime) pureCall(fd *funcData, this Value, args []Value) (Value, bool) {
	return r.pureCallAt(fd, this, args, 0)
}

// pureCallDepth is how deeply pure bodies are evaluated inside one another:
// a recursive one would otherwise take the Go stack with it.
const pureCallDepth = 8

func (r *Runtime) pureCallAt(fd *funcData, this Value, args []Value, depth int) (Value, bool) {
	cl := fd.closure
	fn := cl.fn
	if cl.pureMiss >= pureMissLimit || fd.arrow || len(fd.lexWith) > 0 || fd.lexEvalVars != nil ||
		fn.CoerceThis && !this.IsObject() {
		return Undefined, false
	}
	var stack [24]Value
	sp := 0
	code := fn.Code
	miss := func() (Value, bool) {
		cl.pureMiss++
		return Undefined, false
	}
	for pc := 0; pc < len(code); pc++ {
		in := code[pc]
		switch in.Op {
		case bytecode.OpPushThis:
			stack[sp] = this
			sp++
		case bytecode.OpGetLocal:
			stack[sp] = arg(args, int(in.A))
			sp++
		case bytecode.OpGetLocal2:
			stack[sp], stack[sp+1] = arg(args, int(in.A)), arg(args, int(in.B))
			sp += 2
		case bytecode.OpPushInt:
			stack[sp] = Int32(int32(in.A))
			sp++
		case bytecode.OpPushConst:
			stack[sp] = cl.consts[in.A]
			sp++
		case bytecode.OpPushNull:
			stack[sp] = Null
			sp++
		case bytecode.OpPushTrue:
			stack[sp] = True
			sp++
		case bytecode.OpPushFalse:
			stack[sp] = False
			sp++
		case bytecode.OpPushUndef:
			stack[sp] = Undefined
			sp++
		case bytecode.OpPushEmptyString:
			stack[sp] = Str(emptyString)
			sp++
		case bytecode.OpDup:
			stack[sp] = stack[sp-1]
			sp++
		case bytecode.OpDrop:
			sp--
		case bytecode.OpGetProp:
			o := stack[sp-1]
			if !o.IsObject() {
				return miss()
			}
			name := cl.names[in.A]
			v, ok := plainOwn(o.Object(), name)
			if !ok {
				if v, ok = r.cachedData(&cl.ic[in.B], o.Object(), name); !ok {
					return miss()
				}
			}
			stack[sp-1] = v
		case bytecode.OpGetPropThis:
			// A method, read as get_prop reads, above its receiver.
			o := stack[sp-1]
			if !o.IsObject() {
				return miss()
			}
			name := cl.names[in.A]
			v, ok := plainOwn(o.Object(), name)
			if !ok {
				if v, ok = r.cachedData(&cl.ic[in.B], o.Object(), name); !ok {
					return miss()
				}
			}
			stack[sp] = v
			sp++
		case bytecode.OpCall, bytecode.OpCallMethod:
			n := int(in.A)
			base := sp - n - 1
			callee, recv := stack[base], Undefined
			if in.Op == bytecode.OpCallMethod {
				base--
				recv = stack[base]
			}
			v, ok := r.pureInvoke(callee, recv, stack[sp-n:sp], depth)
			if !ok {
				return miss()
			}
			sp = base
			stack[sp] = v
			sp++
		case bytecode.OpGetLength:
			switch v := stack[sp-1]; {
			case v.IsString():
				stack[sp-1] = Int(v.String().Len())
			case v.IsObject() && v.Object().class == ClassArray:
				stack[sp-1] = Uint32(v.Object().arrayLength())
			default:
				return miss()
			}
		case bytecode.OpGetGlobal:
			v, ok := r.pureGlobal(cl, in)
			if !ok {
				return miss()
			}
			stack[sp] = v
			sp++
		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv,
			bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr:
			v, ok := pureArith(in.Op, stack[sp-2], stack[sp-1])
			if !ok {
				return miss()
			}
			sp--
			stack[sp-1] = v
		case bytecode.OpBinImm:
			v, ok := pureArith(bytecode.Op(in.B), stack[sp-1], Int32(int32(in.A)))
			if !ok {
				return miss()
			}
			stack[sp-1] = v
		case bytecode.OpBinLocal:
			v, ok := pureArith(bytecode.Op(in.B), stack[sp-1], arg(args, int(in.A)))
			if !ok {
				return miss()
			}
			stack[sp-1] = v
		case bytecode.OpLocalBinImm:
			v, ok := pureArith(bytecode.Op(in.A>>24), arg(args, int(in.A&(1<<24-1))), Int32(int32(in.B)))
			if !ok {
				return miss()
			}
			stack[sp] = v
			sp++
		case bytecode.OpNeg:
			v := stack[sp-1]
			if !v.IsNumber() {
				return miss()
			}
			stack[sp-1] = Float(-v.num)
		case bytecode.OpNot:
			stack[sp-1] = Bool(!truthy(stack[sp-1]))
		case bytecode.OpTypeOf:
			stack[sp-1] = Str(r.typeofString(stack[sp-1]))
		case bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe,
			bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe:
			res, ok := r.pureCompare(in.Op, stack[sp-2], stack[sp-1])
			if !ok {
				return miss()
			}
			sp--
			stack[sp-1] = Bool(res)
		case bytecode.OpJump:
			pc = int(in.A) - 1
		case bytecode.OpJumpIfFalse:
			sp--
			if !truthy(stack[sp]) {
				pc = int(in.A) - 1
			}
		case bytecode.OpJumpIfTrue:
			sp--
			if truthy(stack[sp]) {
				pc = int(in.A) - 1
			}
		case bytecode.OpJumpIfCmpFalse:
			res, ok := r.pureCompare(bytecode.Op(in.B), stack[sp-2], stack[sp-1])
			if !ok {
				return miss()
			}
			sp -= 2
			if !res {
				pc = int(in.A) - 1
			}
		case bytecode.OpJumpIfFalseKeep:
			if !truthy(stack[sp-1]) {
				pc = int(in.A) - 1
			} else {
				sp--
			}
		case bytecode.OpJumpIfTrueKeep:
			if truthy(stack[sp-1]) {
				pc = int(in.A) - 1
			} else {
				sp--
			}
		case bytecode.OpReturn:
			return stack[sp-1], true
		case bytecode.OpReturnUndef:
			return Undefined, true
		default:
			return miss()
		}
	}
	return miss()
}

// pureGlobal is get_global's fast paths: a script-level lexical binding
// already initialized, or a plain data property of the global environment's
// own table, which the site may already know the place of.
func (r *Runtime) pureGlobal(cl *closure, in bytecode.Instr) (Value, bool) {
	name := cl.names[in.A]
	env := cl.scope()
	if len(r.globalLex.props) != 0 {
		if p := r.globalLexProp(env, name); p != nil {
			if p.value.IsUninitialized() {
				return Undefined, false
			}
			return p.value, true
		}
	}
	const plain = propAccessor | propPrivate | propDeleted | propUninit
	site := &cl.ic[in.B]
	if i := uint(site.idx); i < uint(len(env.props)) {
		if p := &env.props[i]; p.key == name && p.flags&plain == 0 {
			return p.value, true
		}
	}
	if i := env.findOwn(name); i >= 0 {
		if p := &env.props[i]; p.flags&plain == 0 {
			site.idx = i
			return p.value, true
		}
	}
	return Undefined, false
}

// pureArith is an arithmetic or bitwise operator on two numbers, and false
// for anything else.
func pureArith(op bytecode.Op, a, b Value) (Value, bool) {
	if !a.IsNumber() || !b.IsNumber() {
		return Undefined, false
	}
	x, y := a.num, b.num
	switch op {
	case bytecode.OpAdd:
		return Float(x + y), true
	case bytecode.OpSub:
		return Float(x - y), true
	case bytecode.OpMul:
		return Float(x * y), true
	case bytecode.OpDiv:
		return Float(x / y), true
	case bytecode.OpBitAnd:
		return Int32(toInt32(x) & toInt32(y)), true
	case bytecode.OpBitOr:
		return Int32(toInt32(x) | toInt32(y)), true
	case bytecode.OpBitXor:
		return Int32(toInt32(x) ^ toInt32(y)), true
	case bytecode.OpShl:
		return Int32(toInt32(x) << (uint32(toInt32(y)) & 31)), true
	case bytecode.OpShr:
		return Int32(toInt32(x) >> (uint32(toInt32(y)) & 31)), true
	case bytecode.OpUShr:
		return Uint32(uint32(toInt32(x)) >> (uint32(toInt32(y)) & 31)), true
	}
	return Undefined, false
}

// pureCompare is a comparison that runs nothing: two numbers ordered,
// strict equality of anything, and == of two primitives. It reports false
// in ok for anything else -- an object, whose valueOf could be called, or
// strings ordered.
func (r *Runtime) pureCompare(op bytecode.Op, a, b Value) (res, ok bool) {
	switch op {
	case bytecode.OpStrictEq:
		return a.StrictEquals(b), true
	case bytecode.OpStrictNe:
		return !a.StrictEquals(b), true
	case bytecode.OpEq, bytecode.OpNe:
		if a.IsObject() || b.IsObject() {
			return false, false
		}
		eq, err := r.looseEquals(a, b)
		if err != nil {
			return false, false
		}
		return eq == (op == bytecode.OpEq), true
	}
	if !a.IsNumber() || !b.IsNumber() {
		return false, false
	}
	return compareFloats(op, a.num, b.num), true
}

// pureInvoke is a call inside a pure body, made without a frame where it can
// be: a unary Math function given a number, as a call from the interpreter
// would apply it, a body that only reads one property of this, or another
// pure one. It reports false for any other callee.
func (r *Runtime) pureInvoke(callee, this Value, args []Value, depth int) (Value, bool) {
	if !callee.IsObject() {
		return Undefined, false
	}
	fd := callee.Object().fn()
	if fd == nil {
		return Undefined, false
	}
	if fd.unary != 0 && len(args) != 0 && args[0].IsNumber() {
		return Float(unaryMath[fd.unary](args[0].Number())), true
	}
	if fd.native != nil || fd.boundTarget != nil || fd.closure == nil || fd.closure.realm != r.Realm ||
		!fd.closure.fn.DirectCall {
		return Undefined, false
	}
	switch fd.closure.fn.Leaf {
	case bytecode.LeafPure:
		if depth+1 >= pureCallDepth {
			return Undefined, false
		}
		return r.pureCallAt(fd, this, args, depth+1)
	case bytecode.LeafGetThis, bytecode.LeafGetThisLength, bytecode.LeafGetThisIndex:
		if !this.IsObject() || fd.arrow {
			return Undefined, false
		}
		return r.leafCall(fd.closure, this.Object(), args)
	}
	return Undefined, false
}
