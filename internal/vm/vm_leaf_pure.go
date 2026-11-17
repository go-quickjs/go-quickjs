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

// pureTreeCode is the shortest pure body, in register instructions, that
// is called as its register code rather than evaluated here when it calls
// nothing. A comparison's value is three instructions of register code
// (its branch, the two loads) where the stack bytecode's had been one, so
// that x => x <= last is six: at 6 the JIT ran BigInt64Array's every of it
// at the interpreter's speed (typed_i64_every, 2.4 -> 46 ns), its calls
// made with frames, where 9 keeps the bodies of seven stack instructions
// or fewer evaluated here as they had been.
const pureTreeCode = 9

// pureByTree reports whether calls of a pure body go to its register code,
// as planCall gives them (regPlan), rather than to pureCall: its first call
// is made with a frame, after which planCall gives it its register code (see
// pureFirst). pureCallAt pays an indirect jump of its one switch for every
// instruction, which is often mispredicted, as the bodies it evaluates are
// all different; its register code pays a frame once. A body that calls
// nothing gains nothing else from being frameless, and from about seven
// evaluated instructions on a frame is the cheaper; one that calls keeps its calls
// frameless here, which its register code would make with frames. A call
// from inside another pure body (pureInvoke) is still evaluated here.
func pureByTree(rc *bytecode.RegCode) bool {
	return !rc.PureCalls && len(rc.Code) >= pureTreeCode
}

// pureFirst reports whether a call of fd's pure body, of code rc, tries
// pureCall: the body is not one pureByTree gives its register code, or
// planCall has decided about it and found it none. A short body, the
// commonest, is answered by the first test.
func pureFirst(fd *funcData, rc *bytecode.RegCode) bool {
	return len(rc.Code) < pureTreeCode || rc.PureCalls || fd.planned
}

// pureCallDepth is how deeply pure bodies are evaluated inside one another:
// a recursive one would otherwise take the Go stack with it. A recursion
// deeper than this is given up on whole, and after pureMissLimit of those is
// made with frames, so the limit is set above what recursion over a small
// input -- factorial(10), fib(30), a balanced tree's height -- reaches: a
// level here takes less of the Go stack than a framed call does.
const pureCallDepth = 32

// pureCallAt is pureCall of a body depth calls in, which may store only
// where mayStore says that nothing that called it can give up afterwards.
// It evaluates the body's register code (the register compiler's pureBody), its
// registers a window of the runtime's stack above every frame's, each
// depth of pure calls above the one that called it: register 1 the this,
// the parameters' from 2, the arguments read where the caller has them.
func (r *Runtime) pureCallAt(fd *funcData, this Value, args []Value, depth int, mayStore bool) (Value, bool) {
	cl := fd.closure
	c := cl.reg
	// An arrow is marked pure by the compiler only where its body never
	// reads this, so the this it is called with does not matter.
	if c == nil || c.rc.Leaf != bytecode.LeafPure || cl.pureMiss >= pureMissLimit || len(fd.extra.with()) > 0 || fd.extra.evalVars() != nil {
		return Undefined, false
	}
	if cl.fn.CoerceThis && !this.IsObject() {
		// A sloppy body called without an object for this -- a plain
		// call, f() -- whose this a frame makes: a miss, so that its calls
		// are planned (planCall), the register code's entry making the
		// global object its this, rather than each tried here and then made
		// by runFD.
		cl.pureMiss++
		return Undefined, false
	}
	rc := c.rc
	base := r.stackTop + depth*bytecode.PureRegs
	if base+bytecode.PureRegs > len(r.stack) {
		return Undefined, false
	}
	if base+bytecode.PureRegs > r.stackHigh {
		r.stackHigh = base + bytecode.PureRegs
	}
	regs := r.stack[base : base+bytecode.PureRegs : base+bytecode.PureRegs]
	regs[1] = this
	for i := range cl.fn.ParamCount {
		regs[2+i] = arg(args, i)
	}
	code := rc.Code
	miss := func() (Value, bool) {
		cl.pureMiss++
		return Undefined, false
	}
	// A store, or a call of a body that may store, only where nothing
	// after it can give up (PureTail).
	tail := int(rc.PureTail)
	for pc := 0; pc < len(code); pc++ {
		w := code[pc]
		switch op := w.Op(); op {
		case bytecode.RMove:
			regs[w.A()] = regs[w.B()]
		case bytecode.RLoadInt:
			regs[w.A()] = Int32(int32(w.BC()))
		case bytecode.RLoadConst:
			regs[w.A()] = cl.consts[w.BC()]
		case bytecode.RLoadUndef:
			regs[w.A()] = Undefined
		case bytecode.RLoadNull:
			regs[w.A()] = Null
		case bytecode.RLoadTrue:
			regs[w.A()] = True
		case bytecode.RLoadFalse:
			regs[w.A()] = False
		case bytecode.RLoadEmpty:
			regs[w.A()] = Str(emptyString)
		case bytecode.RGetProp:
			o := regs[w.B()]
			if !o.IsObject() {
				return miss()
			}
			ob, ic := o.Object(), &cl.ic[w.C()]
			v, ok := ic.own(ob)
			if !ok {
				name := cl.names[c.siteName[w.C()]]
				if v, ok = ownScan(ic, ob, name); !ok {
					if v, ok = r.cachedData(ic, ob, name); !ok {
						return miss()
					}
				}
			}
			regs[w.A()] = v
		case bytecode.RGetMethodW:
			// A method, read above its receiver: mostly the prototype's,
			// which the cache answers and a scan of the receiver's own
			// table would not.
			o := regs[w.B()]
			if !o.IsObject() {
				return miss()
			}
			ob, name := o.Object(), cl.names[c.siteName[w.C()]]
			v, ok := r.cachedData(&cl.ic[w.C()], ob, name)
			if !ok {
				if v, ok = plainOwn(ob, name); !ok {
					return miss()
				}
			}
			regs[w.A()], regs[w.A()+1] = v, o
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT:
			a, n := int(w.A()), int(w.X())
			recv := Undefined
			if op == bytecode.RCallMW {
				recv = regs[a+1]
			}
			// A tail call is a call here: a frameless body has no frame to
			// give up, and how deep pure calls go is bounded.
			v, ok := r.pureInvoke(regs[a], recv, regs[a+2:a+2+n], depth, mayStore && pc+1 >= tail)
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
			regs[w.B()] = v
		case bytecode.RNewW:
			a, n := int(w.A()), int(w.X())
			v, ok := r.pureConstruct(regs[a], regs[a+2:a+2+n])
			if !ok {
				return miss()
			}
			regs[w.B()] = v
		case bytecode.RSetProp:
			// A write the site's cache answers -- to a property the object
			// has, or one it adds as objects of its shape did before -- is
			// made, calling nothing; anything else gives up before writing.
			// A store where the caller could still give up is refused, but is
			// not this body's miss: called where it may store, it can.
			if !mayStore {
				r.pureRefused = true
				return Undefined, false
			}
			o := regs[w.A()]
			if !o.IsObject() {
				return miss()
			}
			ob, e := o.Object(), &cl.ic[w.C()]
			// An entry for a setter calls it, which no pure body may.
			if s := ob.shape; s == nil || s != e.shape || e.getter {
				return miss()
			}
			if e.next == nil {
				if verifyShapes {
					checkShape(ob)
				}
				ob.vals()[e.idx] = regs[w.B()]
			} else if e.adds(ob) {
				if verifyShapes {
					r.verifyStoreCache(e, ob, cl.names[c.siteName[w.C()]])
				}
				ob.appendTransition(regs[w.B()], e.next)
			} else {
				return miss()
			}
		case bytecode.RAddProp, bytecode.RAddPropI:
			// o.k += b and o.k++ of a number the object has, as their
			// handlers' fast paths make them: a store, refused as set_prop
			// refuses one.
			if !mayStore {
				r.pureRefused = true
				return Undefined, false
			}
			b := Int32(w.SB())
			if op == bytecode.RAddProp {
				b = regs[w.B()]
			}
			o := regs[w.A()]
			if !o.IsObject() || !b.IsNumber() {
				return miss()
			}
			ob := o.Object()
			e := &cl.ic[w.C()]
			if ob.shape != e.ownShape {
				return miss()
			}
			p := &ob.vals()[e.idx]
			if !p.IsNumber() {
				return miss()
			}
			p.num = regNum(p.Number() + b.Number()).num
		case bytecode.RGetLength:
			switch v := regs[w.B()]; {
			case v.IsString():
				regs[w.A()] = Int(v.String().Len())
			case v.IsObject() && v.Object().class == ClassArray:
				regs[w.A()] = Uint32(v.Object().arrayLength())
			default:
				return miss()
			}
		case bytecode.RGetGlobal:
			v, ok := r.pureGlobal(cl, uint32(w.C()), cl.names[regNameIdx(c, pc, w.B())])
			if !ok {
				return miss()
			}
			regs[w.A()] = v
		case bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor,
			bytecode.RShl, bytecode.RShr, bytecode.RUShr:
			v, ok := pureArith(regStackOp[op], regs[w.B()], regs[w.C()])
			if !ok {
				return miss()
			}
			regs[w.A()] = v
		case bytecode.RBin:
			v, ok := pureArith(bytecode.Op(w.X()), regs[w.B()], regs[w.C()])
			if !ok {
				return miss()
			}
			regs[w.A()] = v
		case bytecode.RAddI, bytecode.RSubI, bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI,
			bytecode.RShrI, bytecode.RUShrI, bytecode.RMulI:
			o, k := regStackOp[op], w.SC()
			switch op {
			case bytecode.RAddI:
				o = bytecode.OpAdd
			case bytecode.RSubI:
				o = bytecode.OpSub
			}
			v, ok := pureArith(o, regs[w.B()], Int32(k))
			if !ok {
				return miss()
			}
			regs[w.A()] = v
		case bytecode.RAddI32, bytecode.RSubI32, bytecode.RAddII32:
			// (x + y) | 0, (x - y) | 0, (x + k) | 0: the operator, then
			// the | 0, as the int32 forms' slow path makes them.
			var v Value
			var ok bool
			switch {
			case op == bytecode.RAddI32:
				v, ok = pureArith(bytecode.OpAdd, regs[w.B()], regs[w.C()])
			case op == bytecode.RSubI32:
				v, ok = pureArith(bytecode.OpSub, regs[w.B()], regs[w.C()])
			case w.X() == bytecode.RAddII32Sub:
				v, ok = pureArith(bytecode.OpSub, regs[w.B()], Int32(-w.SC()))
			default:
				v, ok = pureArith(bytecode.OpAdd, regs[w.B()], Int32(w.SC()))
			}
			if ok {
				v, ok = pureArith(bytecode.OpBitOr, v, Int32(0))
			}
			if !ok {
				return miss()
			}
			regs[w.A()] = v
		case bytecode.RInc, bytecode.RDec:
			v := regs[w.B()]
			if !v.IsNumber() {
				return miss()
			}
			if op == bytecode.RInc {
				regs[w.A()] = Float(v.num + 1)
			} else {
				regs[w.A()] = Float(v.num - 1)
			}
		case bytecode.RUnary:
			v := regs[w.B()]
			switch bytecode.Op(w.X()) {
			case bytecode.OpNeg:
				if !v.IsNumber() {
					return miss()
				}
				regs[w.A()] = Float(-v.num)
			case bytecode.OpNot:
				regs[w.A()] = Bool(!truthy(v))
			default:
				return miss()
			}
		case bytecode.RTypeof:
			regs[w.A()] = Str(r.typeofString(regs[w.B()]))
		case bytecode.RGetUpval:
			regs[w.A()] = cl.upvalues[w.BC()].get()
		case bytecode.RSetUpval:
			// Like a property's store, this is the body's last chance to give
			// up, and is refused where the caller could still give up.
			if !mayStore {
				r.pureRefused = true
				return Undefined, false
			}
			cl.upvalues[w.BC()].set(regs[w.A()])
		case bytecode.RStrFreeze:
			if v := regs[w.A()]; v.IsString() {
				if s := v.String(); s.building {
					s.building = false
				}
			}
		case bytecode.RJmp:
			pc += int(int32(w.BC()))
		case bytecode.RJmpF:
			if !truthy(regs[w.A()]) {
				pc += int(int32(w.BC()))
			}
		case bytecode.RJmpT:
			if truthy(regs[w.A()]) {
				pc += int(int32(w.BC()))
			}
		case bytecode.RJSEq, bytecode.RJSNe, bytecode.RJSEqI, bytecode.RJSNeI:
			b := Int32(int32(w.SB()))
			if op == bytecode.RJSEq || op == bytecode.RJSNe {
				b = regs[w.B()]
			}
			if regs[w.A()].StrictEquals(b) == (op == bytecode.RJSEq || op == bytecode.RJSEqI) {
				pc += int(w.SC())
			}
		case bytecode.RJNull, bytecode.RJNotNull:
			v := regs[w.A()]
			if v.IsObject() && v.object().flags&objHTMLDDA != 0 {
				return miss()
			}
			if (v.IsNull() || v.IsUndefined()) == (op == bytecode.RJNull) {
				pc += int(w.SC())
			}
		case bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt,
			bytecode.RJGe, bytecode.RJNGe, bytecode.RJEq, bytecode.RJNe, bytecode.RJLtI, bytecode.RJNLtI,
			bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI, bytecode.RJGeI, bytecode.RJNGeI,
			bytecode.RJEqI, bytecode.RJNeI, bytecode.RJLtU, bytecode.RJNLtU, bytecode.RJLeU, bytecode.RJNLeU:
			j := regJumpCmp[op]
			b := Int32(int32(w.SB()))
			switch {
			case j.reg:
				b = regs[w.B()]
			case j.upval:
				b = cl.upvalues[w.B()].get()
			}
			res, ok := r.pureCompare(j.op, regs[w.A()], b)
			if !ok {
				return miss()
			}
			if res != j.not {
				pc += int(w.SC())
			}
		case bytecode.RSwitch:
			if to := c.switches[w.BC()].lookup(regs[w.A()], cl.consts); to >= 0 {
				pc = int(to) - 1
			}
		case bytecode.RSwitchTypeof:
			if to := c.switches[w.BC()].typeTo[typeofKind[struct{}](regs[w.A()])]; to >= 0 {
				pc = int(to) - 1
			}
		case bytecode.RJTypeof:
			x := w.X()
			if (typeofKind[struct{}](regs[w.A()]) == int(x&^bytecode.RCmpTrue)) == (x&bytecode.RCmpTrue != 0) {
				pc += int(w.SC())
			}
		case bytecode.RRet:
			return regs[w.A()], true
		case bytecode.RRetUndef:
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
	if fd == nil || fd.ctorKind != ctorBase || fd.bound || fd.native != nil || fd.fieldInit() != nil ||
		fd.closure == nil || fd.closure.realm != r.Realm || leafKind(fd.closure) != bytecode.LeafSetThis ||
		o.class != ClassFunction {
		return Undefined, false
	}
	p := o.getOwnVisible(atomPrototype)
	if !p.ok() || p.flags()&propAccessor != 0 || !p.value().IsObject() {
		return Undefined, false
	}
	proto := p.value().Object()
	root := r.shapes.ctorRoot(fd, proto)
	obj := newOrdinary[struct{}](fd, root, proto, root.roomFor(int(fd.closure.fn.ThisProps)))
	if !r.leafStores(fd.closure, obj, args) {
		return Undefined, false
	}
	root.noteBuilt(int(obj.valn))
	return Obj(obj), true
}

// The built-ins the interpreter and native code call without a frame, as
// they call nothing and cannot throw where they are applied (elemOp): the
// array methods a pure body may call, the collections' lookups and additions
// (collectionOp), and the leaves past elemRandom (leafOps) -- the
// collections' deletions, the weak ones' operations, Date.now, String and
// Number of a primitive, Boolean of any value. Each declares its own where
// it is made (setElemOp).
const (
	elemPush = iota + 1
	elemPop
	elemMapGet
	elemMapHas
	elemMapSet
	elemSetHas
	elemSetAdd
	elemRandom
	elemMapDelete
	elemSetDelete
	elemDateNow
	elemWeakMapGet
	elemWeakMapHas
	elemWeakMapSet
	elemWeakMapDelete
	elemWeakSetHas
	elemWeakSetAdd
	elemWeakSetDelete
	elemStringOf
	// Number(x) and Boolean(x) called, of a primitive and of any value.
	elemNumberOf
	elemBooleanOf

	// Native leaves: the built-in's own function, called without a frame
	// where its op's check says it runs no script, throws nothing and keeps
	// nothing (leafChecks). String.prototype's methods on a string, and
	// String's of numbers; Number's; Array.prototype's searches.
	elemStrAt
	elemStrCharAt
	elemStrCharCodeAt
	elemStrCodePointAt
	elemStrIndexOf
	elemStrLastIndexOf
	elemStrIncludes
	elemStrStartsWith
	elemStrEndsWith
	elemStrSlice
	elemStrSubstring
	elemStrSubstr
	elemStrToUpperCase
	elemStrToLowerCase
	elemStrTrim
	elemStrTrimStart
	elemStrTrimEnd
	elemStrIsWellFormed
	elemStrToWellFormed
	elemStrConcat
	elemStrRepeat
	elemStrPadStart
	elemStrPadEnd
	elemStrNormalize
	elemStrLocaleCompare
	elemStrFromCharCode
	elemStrFromCodePoint
	// Number's: Number.isInteger, isSafeInteger, isFinite and isNaN, of any
	// value; the global isNaN, isFinite, parseInt and parseFloat, of
	// primitives; Number.prototype's toString, valueOf, toFixed,
	// toExponential and toPrecision, on a number.
	elemNumIsInteger
	elemNumIsSafeInteger
	elemNumIsFinite
	elemNumIsNaN
	elemIsNaN
	elemIsFinite
	elemParseInt
	elemParseFloat
	elemNumToString
	elemNumValueOf
	elemNumToFixed
	elemNumToExponential
	elemNumToPrecision
	// Array.prototype's indexOf, lastIndexOf and includes, on an array
	// whose elements are all there is to its indices (searchElems).
	elemArrIndexOf
	elemArrLastIndexOf
	elemArrIncludes
	// RegExp.prototype's exec and test, on a RegExp neither global nor
	// sticky, of a primitive, where the match calls nothing and cannot fail
	// (zzz_regexpleaf.go). Next to the searches, which native code's calls
	// of Go do not make without a frame either (jitLeafOp).
	elemRegExpExec
	elemRegExpTest
	// DataView.prototype's getters and setters, by the bytes they move, a
	// setter of a BigInt apart: an index in the view, its buffer there
	// (zzz_dataview.go).
	elemViewGet1
	elemViewGet2
	elemViewGet4
	elemViewGet8
	elemViewSet1
	elemViewSet2
	elemViewSet4
	elemViewSet8
	elemViewSetBig
	// Object.prototype.hasOwnProperty, on an object no proxy nor module
	// namespace, of a key no object (zzz_hasownleaf.go).
	elemObjHasOwn
	// Array.isArray, of anything but a proxy (zzz_isarrayleaf.go).
	elemArrIsArray
	// Native leaves a construction applies too, where new.target is the
	// built-in itself (constructWithTarget): its call and its construction
	// are the same, as Array's are. Array(...) and new Array(...).
	elemArrayNew
	elemLeafEnd
)

// elemNewFirst is the first native leaf a construction applies.
const elemNewFirst = elemArrayNew

// elemNativeFirst is the first native leaf's op. A call applies a native
// leaf itself (leafChecks), never by pureElemOp.
const elemNativeFirst = elemStrAt

// pureElemOp is the built-in push or pop called from a pure body, where it
// may store and the array is one the built-in's own fast path takes: a dense
// array of its own, whose push adds elements at the end and whose pop takes
// the last one off, calling nothing. It reports false for anything else.
func (r *Runtime) pureElemOp(op uint8, this Value, args []Value, mayStore bool) (Value, bool) {
	if op > elemPop {
		return r.collectionOp(op, this, args, mayStore)
	}
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
		o.ownElems()
		els := o.elemsOf()
		if !r.noInheritedIndices(o) || o.flags&(objExtensible|objArrayLengthWritable) != objExtensible|objArrayLengthWritable ||
			int64(len(els))+int64(len(args)) > maxArrayLength {
			return Undefined, false
		}
		// Each element stored where it goes: a slice appended is a call
		// of the runtime's copy, made for any length, where most pushes
		// are of one.
		end := len(els)
		if end+len(args) > cap(els) {
			els = growElems[struct{}](els, len(args))
		}
		els = els[:end+len(args)]
		for i, v := range args {
			els[end+i] = v
		}
		o.extOf().elems = els
		return Float(float64(len(els))), true
	case elemPop:
		els := o.elemsOf()
		n := len(els) - 1
		if n < 0 || o.flags&objArrayLengthWritable == 0 || isHole(els[n]) {
			return Undefined, false
		}
		v := els[n]
		o.ownElems()
		o.extOf().elems[n] = Undefined
		o.extOf().elems = o.elemsOf()[:n]
		return v, true
	}
	return Undefined, false
}

// pureGlobal is get_global's fast paths: a script-level lexical binding
// already initialized, or a plain data property of the global environment's
// own table, which the site may already know the place of.
func (r *Runtime) pureGlobal(cl *closure, at uint32, name Atom) (Value, bool) {
	const plain = propAccessor | propPrivate | propDeleted | propUninit
	site := &cl.ic[at]
	// The environment the site found the name in, as the interpreter's read
	// takes it (see propCache): no scope to ask for, no key to compare.
	if env := site.p1; env != nil && (r.globalLex.valn == 0 || !r.lexShadows(name)) {
		if i := uint(site.idx); i < uint(int(env.valn)) {
			if p := env.shape.keys[i]; p.flags&plain == 0 {
				return env.vals()[i], true
			}
		}
	}
	return r.pureGlobalSlow(cl, site, name)
}

// pureGlobalSlow is pureGlobal where the site has not got the slot.
func (r *Runtime) pureGlobalSlow(cl *closure, site *propCache, name Atom) (Value, bool) {
	const plain = propAccessor | propPrivate | propDeleted | propUninit
	env := cl.scope()
	if r.globalLex.valn != 0 {
		if p := r.globalLexProp(env, name); p.ok() {
			if p.value().IsUninitialized() {
				return Undefined, false
			}
			return p.value(), true
		}
	}
	if i := uint(site.idx); i < uint(int(env.valn)) {
		if p := env.shape.keys[i]; p.key == name && p.flags&plain == 0 {
			if site.p1 != env {
				site.p1 = env
			}
			return env.vals()[i], true
		}
	}
	if i := env.findOwn(name); i >= 0 {
		if p := env.shape.keys[i]; p.flags&plain == 0 {
			noteGlobalSlot(site, env, i)
			return env.vals()[i], true
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
		return pureBigCompare[struct{}](op, a, b)
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
	if op := fd.elemOp; op >= elemNativeFirst {
		// Its own function, where its check passes (leafChecks) -- but a
		// DataView's setter, which writes, only where the body may store:
		// a body that gave up after it is evaluated again with a frame,
		// and would write again.
		if r.meter != nil || !mayStore && op >= elemViewSet1 && op <= elemViewSetBig || !leafChecks[op](r, this, args) {
			return Undefined, false
		}
		v, err := fd.native(r, this, args)
		return v, err == nil
	} else if op != 0 {
		return r.pureElemOp(op, this, args, mayStore)
	}
	if fd.native != nil || fd.bound || fd.closure == nil || fd.closure.realm != r.Realm ||
		!fd.closure.fn.DirectCall {
		return Undefined, false
	}
	switch leafKind(fd.closure) {
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
