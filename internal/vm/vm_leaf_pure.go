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
// any other is: nothing the body did before giving up can be seen, since a
// store, a body's one, is in the tail of it that cannot give up.
func (r *Runtime) pureCall(fd *funcData, this Value, args []Value) (Value, bool) {
	r.pureRefused = false
	return r.pureCallAt(fd, this, args, 0, true)
}

// The cases of pureCallAt's switch, which pureCases numbers the
// instructions with.
const (
	pcNone = iota
	pcPushThis
	pcGetLocal
	pcGetLocal2
	pcPushInt
	pcPushConst
	pcPushNull
	pcPushTrue
	pcPushFalse
	pcPushUndef
	pcPushEmptyString
	pcDup
	pcDrop
	pcGetProp
	pcGetPropThis
	pcCall
	pcSetProp
	pcNew
	pcGetLength
	pcGetGlobal
	pcArith
	pcBinImm
	pcBinLocal
	pcLocalBinImm
	pcNeg
	pcNot
	pcTypeOf
	pcCompare
	pcJump
	pcJumpIfFalse
	pcJumpIfTrue
	pcJumpIfCmpFalse
	pcJumpIfFalseKeep
	pcJumpIfTrueKeep
	pcGetUpvalue
	pcToNumeric
	pcIncDec
	pcSetUpvalue
	pcReturn
	pcReturnUndef
)

// pureCases gives each instruction pureCallAt evaluates its case, and every
// other instruction pcNone, its default. Switched on directly, the
// opcodes it takes are too far apart for the compiler to make the switch a
// jump table, and it becomes a search of a few comparisons for every
// instruction; numbered densely, it is one indexed jump.
var pureCases = func() (t [256]uint8) {
	t[bytecode.OpPushThis] = pcPushThis
	t[bytecode.OpGetLocal] = pcGetLocal
	t[bytecode.OpGetLocal2] = pcGetLocal2
	t[bytecode.OpPushInt] = pcPushInt
	t[bytecode.OpPushConst] = pcPushConst
	t[bytecode.OpPushNull] = pcPushNull
	t[bytecode.OpPushTrue] = pcPushTrue
	t[bytecode.OpPushFalse] = pcPushFalse
	t[bytecode.OpPushUndef] = pcPushUndef
	t[bytecode.OpPushEmptyString] = pcPushEmptyString
	t[bytecode.OpDup] = pcDup
	t[bytecode.OpDrop] = pcDrop
	t[bytecode.OpGetProp] = pcGetProp
	t[bytecode.OpGetPropThis] = pcGetPropThis
	t[bytecode.OpCall] = pcCall
	t[bytecode.OpCallMethod] = pcCall
	// A tail call is a call here: a frameless body has no frame to give
	// up, and how deep pure calls go is bounded. A method's is not taken,
	// which pcCall would have to tell from call_method's.
	t[bytecode.OpTailCall] = pcCall
	t[bytecode.OpSetProp] = pcSetProp
	t[bytecode.OpNew] = pcNew
	t[bytecode.OpGetLength] = pcGetLength
	t[bytecode.OpGetGlobal] = pcGetGlobal
	t[bytecode.OpAdd] = pcArith
	t[bytecode.OpSub] = pcArith
	t[bytecode.OpMul] = pcArith
	t[bytecode.OpDiv] = pcArith
	t[bytecode.OpBitAnd] = pcArith
	t[bytecode.OpBitOr] = pcArith
	t[bytecode.OpBitXor] = pcArith
	t[bytecode.OpShl] = pcArith
	t[bytecode.OpShr] = pcArith
	t[bytecode.OpUShr] = pcArith
	t[bytecode.OpBinImm] = pcBinImm
	t[bytecode.OpBinLocal] = pcBinLocal
	t[bytecode.OpLocalBinImm] = pcLocalBinImm
	t[bytecode.OpNeg] = pcNeg
	t[bytecode.OpNot] = pcNot
	t[bytecode.OpTypeOf] = pcTypeOf
	t[bytecode.OpLt] = pcCompare
	t[bytecode.OpLe] = pcCompare
	t[bytecode.OpGt] = pcCompare
	t[bytecode.OpGe] = pcCompare
	t[bytecode.OpEq] = pcCompare
	t[bytecode.OpNe] = pcCompare
	t[bytecode.OpStrictEq] = pcCompare
	t[bytecode.OpStrictNe] = pcCompare
	t[bytecode.OpJump] = pcJump
	t[bytecode.OpJumpIfFalse] = pcJumpIfFalse
	t[bytecode.OpJumpIfTrue] = pcJumpIfTrue
	t[bytecode.OpJumpIfCmpFalse] = pcJumpIfCmpFalse
	t[bytecode.OpJumpIfFalseKeep] = pcJumpIfFalseKeep
	t[bytecode.OpJumpIfTrueKeep] = pcJumpIfTrueKeep
	t[bytecode.OpGetUpvalue] = pcGetUpvalue
	t[bytecode.OpToNumeric] = pcToNumeric
	t[bytecode.OpInc] = pcIncDec
	t[bytecode.OpDec] = pcIncDec
	t[bytecode.OpSetUpvalue] = pcSetUpvalue
	t[bytecode.OpReturn] = pcReturn
	t[bytecode.OpReturnUndef] = pcReturnUndef
	return
}()

// pureStackSize is the most operands a pure body has; see pureBody.
const pureStackSize = 24

// pureCallDepth is how deeply pure bodies are evaluated inside one another:
// a recursive one would otherwise take the Go stack with it.
const pureCallDepth = 8

// pureCallAt is pureCall of a body depth calls in, which may store only
// where mayStore says that nothing that called it can give up afterwards.
func (r *Runtime) pureCallAt(fd *funcData, this Value, args []Value, depth int, mayStore bool) (Value, bool) {
	cl := fd.closure
	fn := cl.fn
	// An arrow is marked pure by the compiler only where its body never
	// reads this, so the this it is called with does not matter.
	if cl.pureMiss >= pureMissLimit || fd.extra != nil && len(fd.extra.lexWith) > 0 || fd.extra != nil && fd.extra.lexEvalVars != nil ||
		fn.CoerceThis && !this.IsObject() {
		return Undefined, false
	}
	// The body's operands go in the runtime's stack above every frame's,
	// each depth of pure calls above the one that called it: an array of
	// its own would be cleared at every call.
	base := r.stackTop + depth*pureStackSize
	if base+pureStackSize > len(r.stack) {
		return Undefined, false
	}
	if base+pureStackSize > r.stackHigh {
		r.stackHigh = base + pureStackSize
	}
	stack := r.stack[base : base+pureStackSize : base+pureStackSize]
	sp := 0
	code := fn.Code
	miss := func() (Value, bool) {
		cl.pureMiss++
		return Undefined, false
	}
	for pc := 0; pc < len(code); pc++ {
		in := code[pc]
		switch pureCases[in.Op] {
		case pcPushThis:
			if pc+1 < len(code) && code[pc+1].Op == bytecode.OpGetProp {
				// this.k, a method's commonest read, in one turn of the loop
				// rather than two.
				next := code[pc+1]
				if !this.IsObject() {
					return miss()
				}
				ob, ic := this.Object(), &cl.ic[next.B]
				v, ok := ic.own(ob)
				if !ok {
					if v, ok = ownScan(ic, ob, cl.names[next.A]); !ok {
						if v, ok = r.cachedData(ic, ob, cl.names[next.A]); !ok {
							return miss()
						}
					}
				}
				stack[sp] = v
				sp++
				pc++
				break
			}
			stack[sp] = this
			sp++
		case pcGetLocal:
			stack[sp] = arg(args, int(in.A))
			sp++
		case pcGetLocal2:
			stack[sp], stack[sp+1] = arg(args, int(in.A)), arg(args, int(in.B))
			sp += 2
		case pcPushInt:
			stack[sp] = Int32(int32(in.A))
			sp++
		case pcPushConst:
			stack[sp] = cl.consts[in.A]
			sp++
		case pcPushNull:
			stack[sp] = Null
			sp++
		case pcPushTrue:
			stack[sp] = True
			sp++
		case pcPushFalse:
			stack[sp] = False
			sp++
		case pcPushUndef:
			stack[sp] = Undefined
			sp++
		case pcPushEmptyString:
			stack[sp] = Str(emptyString)
			sp++
		case pcDup:
			stack[sp] = stack[sp-1]
			sp++
		case pcDrop:
			sp--
		case pcGetProp:
			o := stack[sp-1]
			if !o.IsObject() {
				return miss()
			}
			ob, ic := o.Object(), &cl.ic[in.B]
			v, ok := ic.own(ob)
			if !ok {
				if v, ok = ownScan(ic, ob, cl.names[in.A]); !ok {
					if v, ok = r.cachedData(ic, ob, cl.names[in.A]); !ok {
						return miss()
					}
				}
			}
			stack[sp-1] = v
		case pcGetPropThis:
			// A method, read as get_prop reads, above its receiver.
			o := stack[sp-1]
			if !o.IsObject() {
				return miss()
			}
			// A method is mostly the prototype's, which the cache answers
			// and a scan of the receiver's own table would not.
			ob := o.Object()
			v, ok := r.cachedData(&cl.ic[in.B], ob, cl.names[in.A])
			if !ok {
				if v, ok = plainOwn(ob, cl.names[in.A]); !ok {
					return miss()
				}
			}
			stack[sp] = v
			sp++
		case pcCall:
			n := int(in.A)
			base := sp - n - 1
			callee, recv := stack[base], Undefined
			if in.Op == bytecode.OpCallMethod {
				base--
				recv = stack[base]
			}
			v, ok := r.pureInvoke(callee, recv, stack[sp-n:sp], depth, mayStore && pc+1 >= int(fn.PureTail))
			if !ok {
				// A store refused below is the miss of the body that called
				// where it could not store, not of a body that was itself
				// called so: that one's caller is answerable.
				if r.pureRefused && !mayStore {
					return Undefined, false
				}
				r.pureRefused = false
				return miss()
			}
			sp = base
			stack[sp] = v
			sp++
		case pcSetProp:
			// A write the site's cache answers -- to a property the object
			// has, or one it adds as objects of its shape did before -- is
			// made, calling nothing; anything else gives up before writing.
			// A store where the caller could still give up is refused, but is
			// not this body's miss: called where it may store, it can.
			if !mayStore {
				r.pureRefused = true
				return Undefined, false
			}
			o := stack[sp-2]
			if !o.IsObject() {
				return miss()
			}
			ob, c := o.Object(), &cl.ic[in.B]
			// An entry for a setter calls it, which no pure body may.
			if s := ob.shape; s == nil || s != c.shape || c.getter {
				return miss()
			}
			if c.next == nil {
				if verifyShapes {
					checkShape(ob)
				}
				ob.props[c.idx].value = stack[sp-1]
			} else if c.adds(ob) {
				if verifyShapes {
					r.verifyStoreCache(c, ob, cl.names[in.A])
				}
				ob.appendTransition(Property{key: cl.names[in.A], flags: c.next.flags, value: stack[sp-1]}, c.next)
			} else {
				return miss()
			}
			sp -= 2
		case pcNew:
			n := int(in.A)
			base := sp - n - 1
			v, ok := r.pureConstruct(stack[base], stack[sp-n:sp])
			if !ok {
				return miss()
			}
			sp = base
			stack[sp] = v
			sp++
		case pcGetLength:
			switch v := stack[sp-1]; {
			case v.IsString():
				stack[sp-1] = Int(v.String().Len())
			case v.IsObject() && v.Object().class == ClassArray:
				stack[sp-1] = Uint32(v.Object().arrayLength())
			default:
				return miss()
			}
		case pcGetGlobal:
			v, ok := r.pureGlobal(cl, in)
			if !ok {
				return miss()
			}
			stack[sp] = v
			sp++
		case pcArith:
			v, ok := pureArith(in.Op, stack[sp-2], stack[sp-1])
			if !ok {
				return miss()
			}
			sp--
			stack[sp-1] = v
		case pcBinImm:
			v, ok := pureArith(bytecode.Op(in.B), stack[sp-1], Int32(int32(in.A)))
			if !ok {
				return miss()
			}
			stack[sp-1] = v
		case pcBinLocal:
			v, ok := pureArith(bytecode.Op(in.B), stack[sp-1], arg(args, int(in.A)))
			if !ok {
				return miss()
			}
			stack[sp-1] = v
		case pcLocalBinImm:
			v, ok := pureArith(bytecode.Op(in.A>>24), arg(args, int(in.A&(1<<24-1))), Int32(int32(in.B)))
			if !ok {
				return miss()
			}
			stack[sp] = v
			sp++
		case pcNeg:
			v := stack[sp-1]
			if !v.IsNumber() {
				return miss()
			}
			stack[sp-1] = Float(-v.num)
		case pcNot:
			stack[sp-1] = Bool(!truthy(stack[sp-1]))
		case pcTypeOf:
			stack[sp-1] = Str(r.typeofString(stack[sp-1]))
		case pcCompare:
			res, ok := r.pureCompare(in.Op, stack[sp-2], stack[sp-1])
			if !ok {
				return miss()
			}
			sp--
			stack[sp-1] = Bool(res)
		case pcJump:
			pc = int(in.A) - 1
		case pcJumpIfFalse:
			sp--
			if !truthy(stack[sp]) {
				pc = int(in.A) - 1
			}
		case pcJumpIfTrue:
			sp--
			if truthy(stack[sp]) {
				pc = int(in.A) - 1
			}
		case pcJumpIfCmpFalse:
			res, ok := r.pureCompare(bytecode.Op(in.B), stack[sp-2], stack[sp-1])
			if !ok {
				return miss()
			}
			sp -= 2
			if !res {
				pc = int(in.A) - 1
			}
		case pcJumpIfFalseKeep:
			if !truthy(stack[sp-1]) {
				pc = int(in.A) - 1
			} else {
				sp--
			}
		case pcJumpIfTrueKeep:
			if truthy(stack[sp-1]) {
				pc = int(in.A) - 1
			} else {
				sp--
			}
		case pcGetUpvalue:
			stack[sp] = cl.upvalues[in.A].get()
			sp++
		case pcToNumeric:
			if !stack[sp-1].IsNumber() {
				return miss()
			}
		case pcIncDec:
			v := stack[sp-1]
			if !v.IsNumber() {
				return miss()
			}
			if in.Op == bytecode.OpInc {
				stack[sp-1] = Float(v.num + 1)
			} else {
				stack[sp-1] = Float(v.num - 1)
			}
		case pcSetUpvalue:
			// Like a property's store, this is the body's last chance to give
			// up, and is refused where the caller could still give up.
			if !mayStore {
				r.pureRefused = true
				return Undefined, false
			}
			sp--
			cl.upvalues[in.A].set(stack[sp])
		case pcReturn:
			return stack[sp-1], true
		case pcReturnUndef:
			return Undefined, true
		default:
			return miss()
		}
	}
	return miss()
}

// pureConstruct is new F(...args) inside a pure body, made without a frame
// where F is a constructor of this realm whose body only stores its
// parameters in this (LeafSetThis), with nothing to install before it runs,
// and whose prototype property is a plain one holding an object. The object
// is made as construct makes it, and given the stores the sites' caches
// answer; anything else reports false. An object made and then given up with
// the rest of the evaluation was never seen by anything.
func (r *Runtime) pureConstruct(callee Value, args []Value) (Value, bool) {
	if !callee.IsObject() {
		return Undefined, false
	}
	o := callee.Object()
	fd := o.fn()
	if fd == nil || fd.ctorKind != ctorBase || fd.bound || fd.native != nil || fd.fieldInit != nil ||
		fd.closure == nil || fd.closure.realm != r.Realm || fd.closure.fn.Leaf != bytecode.LeafSetThis ||
		o.class != ClassFunction {
		return Undefined, false
	}
	p := o.getOwnVisible(atomPrototype)
	if p == nil || p.flags&propAccessor != 0 || !p.value.IsObject() {
		return Undefined, false
	}
	root := r.shapes.ctorRoot(fd)
	props := int(fd.closure.fn.ThisProps)
	if root != nil {
		props = max(props, int(root.slack))
	}
	obj := newLiteralObject(p.value.Object(), ClassObject, props)
	obj.shape = root
	if !r.leafStores(fd.closure, obj, args) {
		return Undefined, false
	}
	return Obj(obj), true
}

// The built-in array methods a pure body may call without a frame.
const (
	elemPush = iota + 1
	elemPop
)

// pureElemOp is the built-in push or pop called from a pure body, where it
// may store and the array is one the built-in's own fast path takes: a dense
// array of its own, whose push adds elements at the end and whose pop takes
// the last one off, calling nothing. It reports false for anything else.
func (r *Runtime) pureElemOp(op uint8, this Value, args []Value, mayStore bool) (Value, bool) {
	if !mayStore {
		r.pureRefused = true
		return Undefined, false
	}
	o := r.plainArray(this)
	if o == nil {
		return Undefined, false
	}
	switch op {
	case elemPush:
		if !r.noInheritedIndices(o) || o.flags&(objExtensible|objArrayLengthWritable) != objExtensible|objArrayLengthWritable ||
			int64(len(o.elems))+int64(len(args)) > maxArrayLength {
			return Undefined, false
		}
		o.elems = append(o.elems, args...)
		return Float(float64(len(o.elems))), true
	case elemPop:
		n := len(o.elems) - 1
		if n < 0 || o.flags&objArrayLengthWritable == 0 || isHole(o.elems[n]) {
			return Undefined, false
		}
		v := o.elems[n]
		o.elems[n] = Undefined
		o.elems = o.elems[:n]
		return v, true
	}
	return Undefined, false
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
	if a.IsNumber() && b.IsNumber() {
		// Two numbers are equal, loosely or strictly, as floats are: NaN
		// to nothing, and 0 to -0.
		switch op {
		case bytecode.OpEq, bytecode.OpStrictEq:
			return a.num == b.num, true
		case bytecode.OpNe, bytecode.OpStrictNe:
			return a.num != b.num, true
		}
		return compareFloats(op, a.num, b.num), true
	}
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
// be: a Math function given numbers, as a call from the interpreter
// would apply it, a body that only reads one property of this, or another
// pure one. It reports false for any other callee.
func (r *Runtime) pureInvoke(callee, this Value, args []Value, depth int, mayStore bool) (Value, bool) {
	if !callee.IsObject() {
		return Undefined, false
	}
	fd := callee.Object().fn()
	if fd == nil {
		return Undefined, false
	}
	if fd.mathOp != 0 {
		if v, ok := mathCall(fd.mathOp, args); ok {
			return v, true
		}
	}
	if fd.elemOp != 0 {
		return r.pureElemOp(fd.elemOp, this, args, mayStore)
	}
	if fd.native != nil || fd.bound || fd.closure == nil || fd.closure.realm != r.Realm ||
		!fd.closure.fn.DirectCall {
		return Undefined, false
	}
	switch fd.closure.fn.Leaf {
	case bytecode.LeafPure:
		if depth+1 >= pureCallDepth {
			return Undefined, false
		}
		return r.pureCallAt(fd, this, args, depth+1, mayStore)
	case bytecode.LeafGetThis, bytecode.LeafGetThisLength, bytecode.LeafGetThisIndex:
		if !this.IsObject() || fd.arrow {
			return Undefined, false
		}
		return r.leafCall(fd.closure, this.Object(), args)
	}
	return Undefined, false
}
