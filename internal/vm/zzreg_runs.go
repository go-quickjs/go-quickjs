package vm

import (
	"encoding/binary"
	"math"
	"os"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// What the register interpreter's handlers call out of line: the
// operators' slow paths, the miss paths and what an iteration runs
// (docs/register-interpreter.md, "The loop").

// regBinImm is bin_imm's slow path.
func (r *Runtime) regBinImm(op bytecode.Op, a Value, imm int32) (Value, error) {
	if a.IsNumber() {
		x, y := a.Number(), imm
		switch op {
		case bytecode.OpAdd:
			return Float(x + float64(y)), nil
		case bytecode.OpSub:
			return Float(x - float64(y)), nil
		case bytecode.OpMul:
			return Float(x * float64(y)), nil
		}
		xi := int32(x)
		if float64(xi) != x {
			xi = jsnum.ToInt32(x)
		}
		switch op {
		case bytecode.OpBitAnd:
			return Int32(xi & y), nil
		case bytecode.OpBitOr:
			return Int32(xi | y), nil
		case bytecode.OpBitXor:
			return Int32(xi ^ y), nil
		case bytecode.OpShl:
			return Int32(xi << (uint32(y) & 31)), nil
		case bytecode.OpShr:
			return Int32(xi >> (uint32(y) & 31)), nil
		default:
			return Uint32(uint32(xi) >> (uint32(y) & 31)), nil
		}
	}
	return r.binImm(op, a, Int32(imm))
}

// regBinary is a binary operator the loop has no case of its own for.
func (r *Runtime) regBinary(op bytecode.Op, a, b Value) (Value, error) {
	switch op {
	case bytecode.OpDiv, bytecode.OpMod, bytecode.OpPow:
		if a.IsNumber() && b.IsNumber() {
			if op == bytecode.OpDiv {
				return Float(a.Number() / b.Number()), nil
			}
			return Float(numericOp(op, a.Number(), b.Number())), nil
		}
		return r.arith(op, a, b)
	case bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr:
		if a.IsNumber() && b.IsNumber() {
			fa, fb := a.Number(), b.Number()
			x, y := int32(fa), int32(fb)
			if float64(x) != fa {
				x = jsnum.ToInt32(fa)
			}
			if float64(y) != fb {
				y = jsnum.ToInt32(fb)
			}
			switch op {
			case bytecode.OpBitAnd:
				return Int32(x & y), nil
			case bytecode.OpBitOr:
				return Int32(x | y), nil
			case bytecode.OpBitXor:
				return Int32(x ^ y), nil
			case bytecode.OpShl:
				return Int32(x << (uint32(y) & 31)), nil
			case bytecode.OpShr:
				return Int32(x >> (uint32(y) & 31)), nil
			}
			return Uint32(uint32(x) >> (uint32(y) & 31)), nil
		}
		return r.bitwise(op, a, b)
	}
	res, err := r.regCompare(op, a, b)
	return Bool(res), err
}

// regCompare is a comparison: an equality or a relational operator.
func (r *Runtime) regCompare(op bytecode.Op, a, b Value) (bool, error) {
	switch op {
	case bytecode.OpStrictEq:
		return a.StrictEquals(b), nil
	case bytecode.OpStrictNe:
		return !a.StrictEquals(b), nil
	case bytecode.OpEq, bytecode.OpNe:
		eq, err := r.looseEquals(a, b)
		return eq == (op == bytecode.OpEq), err
	}
	if a.IsNumber() && b.IsNumber() {
		return compareFloats(op, a.Number(), b.Number()), nil
	}
	if res, ok := pureBigCompare[struct{}](op, a, b); ok {
		// Two BigInts an int64 holds, as the tree's compare nodes order
		// them.
		return res, nil
	}
	c, err := r.compare(a, b)
	if err != nil {
		return false, err
	}
	return relationalResult(op, c), nil
}

// regStep is ++ or -- of a value that is not a number.
func (r *Runtime) regStep(a Value, delta float64) (Value, error) {
	if a.IsBigInt() {
		if v, ok := shortArith(bytecode.OpAdd, a, shortBig(int64(delta))); ok {
			return v, nil
		}
		return r.bigOp(bytecode.OpAdd, a, shortBig(int64(delta)))
	}
	n, err := r.toNumeric(a)
	if err != nil {
		return Undefined, err
	}
	if n.IsBigInt() {
		return r.arith(bytecode.OpAdd, n, shortBig(int64(delta)))
	}
	return Float(n.Number() + delta), nil
}

// regUnary is a unary operator.
func (r *Runtime) regUnary(op bytecode.Op, a Value) (Value, error) {
	switch op {
	case bytecode.OpNeg:
		if a.IsNumber() {
			return Float(-a.Number()), nil
		}
		return r.negate(a)
	case bytecode.OpPos:
		n, err := r.toNumber(a)
		return Float(n), err
	case bytecode.OpNot:
		return Bool(!regTruthy(a)), nil
	case bytecode.OpTypeOf:
		return Str(r.typeofString(a)), nil
	case bytecode.OpToNumeric:
		return r.toNumeric(a)
	}
	// bit_not.
	if !a.IsNumber() {
		n, err := r.toNumeric(a)
		if err != nil {
			return Undefined, err
		}
		if n.IsBigInt() {
			return bigNot(n), nil
		}
		a = n
	}
	x, err := r.toInt32(a)
	return Int32(^x), err
}

// regElemMiss is the element get_index, set_index and their immediate
// forms read or write where their fast path has found no dense element, as
// the tree's getIndexMiss and setIndexMiss do: a typed array's, from or to
// its buffer as elemAt and setElem read and write it, typedRead and
// typedStore the rest they can; an element appended at an array's length,
// or a hole filled (appendElem, fillHole). False sends what is left -- a
// hole read, a value a typed array converts with code, a getter, a proxy
// -- to the slow path. None of it can throw or run code.
//
// The slow functions of get_index, set_index and their immediate forms
// call it first; it tells the operations apart by the instruction.
//
//go:noinline
func regElemMiss(c *regCode, k *regKept, regs *[1 << 16]Value) bool {
	w := regAt(c.code, k.pc-1)
	var obj, key Value
	switch w.Op() {
	case bytecode.RGetIndex, bytecode.RGetIndexI:
		obj, key = regs[w.B()], Value{num: float64(w.C())}
		if w.Op() == bytecode.RGetIndex {
			key = regs[w.C()]
		}
		if obj.IsObject() && obj.object().class == ClassTypedArray {
			// The element read here, as elemAt reads it, rather than
			// through a second call.
			t, ok := obj.object().dataOf().(*typedArrayData)
			i := uint32(key.num)
			if ok && float64(i) == key.num && uint(i) < uint(t.fixedLength) {
				if st := t.st; st != nil && t.end <= len(st.bytes) {
					b, j := st.bytes[t.byteOffset:t.end], int(i)
					var v Value
					switch t.kind {
					case elemInt8:
						v = Int(int(int8(b[j])))
					case elemUint8, elemUint8Clamped:
						v = Int(int(b[j]))
					case elemInt16:
						v = Int(int(int16(binary.LittleEndian.Uint16(b[j<<1:]))))
					case elemUint16:
						v = Int(int(binary.LittleEndian.Uint16(b[j<<1:])))
					case elemInt32:
						v = Int32(int32(binary.LittleEndian.Uint32(b[j<<2:])))
					case elemUint32:
						v = Uint32(binary.LittleEndian.Uint32(b[j<<2:]))
					case elemFloat32:
						v = Float(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[j<<2:]))))
					case elemFloat64:
						v = Float(math.Float64frombits(binary.LittleEndian.Uint64(b[j<<3:])))
					case elemBigInt64:
						// A BigInt kind's here too, rather than through
						// elemAt, typedRead and the slow path's read.
						v = shortBig(int64(binary.LittleEndian.Uint64(b[j<<3:])))
					case elemBigUint64:
						v = bigFromUint64(binary.LittleEndian.Uint64(b[j<<3:]))
					default:
						goto long
					}
					regs[w.A()] = v
					return true
				}
			}
		}
	long:
		if v, ok := elemAt(obj, key); ok {
			regs[w.A()] = v
			return true
		}
		if v, ok := typedRead(obj, key.num); ok {
			regs[w.A()] = v
			return true
		}
		return false
	case bytecode.RSetIndex, bytecode.RSetIndexI:
		obj, key = regs[w.A()], Value{num: float64(w.B())}
		if w.Op() == bytecode.RSetIndex {
			key = regs[w.B()]
		}
		v := regs[w.C()]
		if obj.IsObject() && obj.object().class == ClassTypedArray && v.IsNumber() {
			// A number stored here, as setElem stores it.
			t, ok := obj.object().dataOf().(*typedArrayData)
			i := uint32(key.num)
			if ok && float64(i) == key.num && uint(i) < uint(t.fixedLength) {
				if st := t.st; st != nil && t.end <= len(st.bytes) && !st.immutable {
					b, n, j := st.bytes[t.byteOffset:t.end], v.num, int(i)
					switch t.kind {
					case elemInt8, elemUint8:
						b[j] = byte(toInt32Wrap(n))
					case elemUint8Clamped:
						b[j] = clampUint8(n)
					case elemInt16, elemUint16:
						binary.LittleEndian.PutUint16(b[j<<1:], uint16(toInt32Wrap(n)))
					case elemInt32, elemUint32:
						binary.LittleEndian.PutUint32(b[j<<2:], uint32(toInt32Wrap(n)))
					case elemFloat32:
						binary.LittleEndian.PutUint32(b[j<<2:], math.Float32bits(float32(n)))
					case elemFloat64:
						binary.LittleEndian.PutUint64(b[j<<3:], math.Float64bits(n))
					default:
						return typedStore(obj, key.num, v)
					}
					return true
				}
			}
		}
		if setElem(obj, key, v) || typedStore(obj, key.num, v) {
			return true
		}
		// An element appended at an array's length, or a hole filled,
		// as the tree's setIndexMiss stores them.
		return obj.IsObject() && (appendElem(obj.object(), key.num, v) || fillHole(obj.object(), key.num, v))
	}
	return false
}

// regSetMiss is set_index's miss: on an array, an element appended at its
// length or a hole filled, as the tree's setIndexMiss stores them, where
// setElem would store nothing the loop's fast path has not; on anything
// else, regElemMiss's typed array's element. False sends what is left --
// a shared literal's element, a setter, a proxy -- to the slow path. None
// of it can throw or run code.
//
//go:noinline
func (r *Runtime) regSetMiss(c *regCode, k *regKept, regs *[1 << 16]Value) bool {
	w := regAt(c.code, k.pc-1)
	if obj := regs[w.A()]; obj.IsObject() && obj.object().class == ClassArray {
		key, v := regs[w.B()], regs[w.C()]
		o := obj.object()
		return appendElem(o, key.num, v) || fillHole(o, key.num, v)
	} else if v := regs[w.C()]; obj.IsObject() && v.IsNumber() {
		// A typed array's element stored as native code stores it, before
		// regElemMiss's call: in set_index's case, the store made the
		// loop's appends 6 to 9% slower at every placement.
		if t, p := regTypedAt(r, obj.object(), regs[w.B()].num); t != nil && regTypedStore(t, p, v.num) {
			return true
		}
	}
	return regElemMiss(c, k, regs)
}

// regBackEdge is a backward jump's interrupt check, when the budget is
// spent.
func (r *Runtime) regBackEdge(f *frame, c *regCode, k *regKept) error {
	r.backEdges = backEdgeCheckInterval
	f.pc = c.src[k.pc-1] + 1
	if err := r.checkInterruptNow(); err != nil {
		return err
	}
	// The temporaries past what the loop's head has live are stale: a
	// value a call's window was given stays there until the window is
	// used again.
	r.sweepStaleSlots(f.base - int(c.locals) + c.rc.LiveAt(k.pc-1))
	return nil
}

// regAppend is append, a case of runReg's: an append to a local, its
// string grown in place where it alone holds it, as the tree's
// strAppendNode grows it. The case keeps nothing of the instruction across
// the call; this reads it back.
//
//go:noinline
func (r *Runtime) regAppend(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	f.pc = c.src[pc] + 1
	v, err := strAppendOp[struct{}](r, regs[w.A()], regs[w.B()])
	if err == nil {
		regs[w.A()] = v
	}
	return err
}

// regCharCodeAt is char_code_at, a case of runReg's: s.charCodeAt(i) of a
// string, its method the built-in read from the cache primProp fills and
// its argument a number, read here as the tree's charCodeAtCall reads it,
// without the method's read on the slow path, the call's site and the
// built-in's conversions; anything else is regCharCodeAtSlow's. The case
// keeps nothing of the instruction across the call; this reads it back.
//
//go:noinline
func (r *Runtime) regCharCodeAt(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	if o := regs[w.B()]; o.IsString() {
		key := f.cl.names[c.siteName[w.C()]]
		p := r.proto.str
		if e := &r.primProps[key&63]; e.key == key && e.proto == p && e.shape == p.shape {
			if m := p.vals()[e.idx]; m.IsObject() {
				if fd := m.Object().fn(); fd != nil && fd.elemOp == elemStrCharCodeAt && !fd.bound {
					if a := regs[w.X()]; a.IsNumber() {
						x := a.Number()
						if x != x {
							x = 0
						}
						// ToIntegerOrInfinity truncates toward zero, so any
						// position above -1 and below the length is a code
						// unit's.
						v := Float(math.NaN())
						if s := o.String(); x > -1 && x < float64(s.Len()) {
							v = Int(s.CharCodeAt(int(x)))
						}
						regs[w.A()] = v
						return nil
					}
				}
			}
		}
	}
	return r.regCharCodeAtSlow(f, c, pc, regs)
}

// regIterNext is iter_next: R[A] = the next value of cursor R[B], or a
// jump by SC where it has none, the step at the head of each turn of a
// for-of or a for-in loop. An array's element in its dense storage is
// read here, as the tree's iterNextTree reads it; a generator's cursor is
// stepped as the tree steps it (regIterNextTree); anything else as
// iterNext steps it.
//
//go:noinline
func (r *Runtime) regIterNext(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	cur := regs[w.B()]
	var v Value
	var more bool
	var err error
	if st := cursorOf(cur); st != nil && !st.done {
		if a := st.arr; a != nil {
			if i := st.idx; i < int(a.arrayLength()) && i < len(a.elemsOf()) {
				if x := a.elemsOf()[i]; !isHole(x) {
					st.idx++
					regs[w.A()] = x
					return nil
				}
			}
		} else if !st.forIn && r.genNextFn != nil && st.next.ref == unsafe.Pointer(r.genNextFn) {
			f.pc = c.src[pc] + 1
			if g := regGenCursor(st); g != nil {
				v, more, err = r.regGenStep(st, g)
			} else {
				v, more, err = r.regIterNextTree(f, cur)
			}
			goto stepped
		} else if !st.forIn {
			f.pc = c.src[pc] + 1
			v, more, err = r.regStepNext(st)
			goto stepped
		}
	}
	f.pc = c.src[pc] + 1
	v, more, err = r.iterNext(cur)
stepped:
	if err != nil {
		return err
	}
	if !more {
		k.pc += int(w.SC())
		return nil
	}
	regs[w.A()] = v
	return nil
}

// regLit is regexp_lit: the literal's RegExp made or given back in the
// window's receiver and its method in the callee's register -- the
// built-in, unread, where reading it would find it running nothing to
// find it (regexpLitMethod), as the tree's literal call does, and read
// otherwise, at the instruction's position or, for one that calls, at the
// stack form's instruction of the literal (Aux) -- and, with RLitCall,
// the method called with the window's one argument as a call_method_w
// marked RMarkLitRecv calls it.
//
//go:noinline
func (r *Runtime) regLit(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	f.pc = c.src[pc] + 1
	a, x, kc := w.A(), w.X(), uint32(w.C())
	lit, err := r.newRegExpLiteral(regConst(f.cl.fn, c, kc))
	if err != nil {
		return err
	}
	regs[a+1] = lit
	test := x&bytecode.RLitTest != 0
	if m := r.regexpLitMethod(test); m != nil {
		if x&(bytecode.RLitCall|bytecode.RLitDead) == bytecode.RLitCall && !regs[a+2].IsObject() {
			// The built-in on the literal given a primitive, called
			// at once, and the RegExp given back (RMarkLitRecv).
			v, err := r.callRest(m, m.fn(), lit, regs[a+2:a+3])
			if err != nil {
				return err
			}
			if !test || r.regexpLitMethod(false) != nil {
				// test calls exec, which must be the built-in too.
				r.regexpLiteralDone(regConst(f.cl.fn, c, kc), lit.Object())
			}
			regs[w.B()] = v
			return nil
		}
		regs[a] = Obj(m)
	} else {
		name := atomExec
		if test {
			name = r.atoms.intern("test")
		}
		if x&bytecode.RLitCall != 0 {
			f.pc = c.rc.Aux[pc] + 1
		}
		v, err := r.getValueProp(lit, name)
		if err != nil {
			return err
		}
		regs[a] = v
		f.pc = c.src[pc] + 1
	}
	if x&bytecode.RLitCall == 0 {
		return nil
	}
	m := uint8(bytecode.RMarkLitRecv)
	if x&bytecode.RLitDead != 0 {
		m |= bytecode.RMarkDead
	}
	v, err := r.regMarkedCall(f, c, f.base-int(c.locals)+int(a), 1, pc, m, kc)
	if err != nil {
		return err
	}
	regs[w.B()] = v
	return nil
}

// regCallSlow is the call of instruction w at pc, its window at base,
// that its site's cache does not answer.
// regMarkedCall is the call of call_method_w pc, its window at base, of
// argc arguments, which its Aux, aux, marks, and what the tree's nodes of
// such calls do around it, x its operand (Aux). RMarkLitArg makes the
// call's first argument,
// the literal; a literal's RegExp is given back where the call keeps
// nothing of it and runs nothing of the script's (regexpLiteralDone):
// exec or test, the realm's own, given a primitive, of the literal's
// RegExp (RMarkLitRecv); replace, split or match searching with it
// directly (regexpArgCall). A call of the realm's exec or replace whose
// value nothing reads but its truth (RMarkDead) tells the function so, as
// deadExec's node does (regDeadCall); one of exec whose value goes to a
// local that keeps it only as execReuseOf says (RMarkReuse) has its array
// made in the memory of the last (reuseCallRest).
func (r *Runtime) regMarkedCall(f *frame, c *regCode, base, argc, pc int, m uint8, x uint32) (Value, error) {
	k := x
	args := r.stack[base+2 : base+2+argc]
	if m&bytecode.RMarkLitArg != 0 {
		lit, err := r.newRegExpLiteral(regConst(f.cl.fn, c, k))
		if err != nil {
			return Undefined, err
		}
		args[0] = lit
	}
	fv, recv := r.stack[base], r.stack[base+1]
	if !fv.IsObject() {
		return r.regCall(fv, recv, args)
	}
	o := fv.Object()
	var rx *Object
	if m&bytecode.RMarkLitArg != 0 {
		if r.regexpArgCall(o, recv, args) {
			rx = args[0].Object()
		}
	} else if m&bytecode.RMarkLitRecv != 0 && recv.IsObject() && (argc == 0 || !args[0].IsObject()) {
		if exec := r.regexpLitMethod(false); exec != nil && (o == exec || o == r.regexpLitMethod(true)) {
			rx = recv.Object()
		}
	}
	var v Value
	var err error
	switch {
	case m&bytecode.RMarkReuse != 0:
		if site := regReuseAt(c, pc); site != nil && o == r.regexpExecFn {
			wb := f.base - int(c.locals)
			v, err = reuseCallRest[struct{}](r, site, r.stack[wb:wb+int(c.numRegs)], o, o.fn(), recv, args)
		} else {
			v, err = r.regMarkedRest(o, recv, args)
		}
	case m&bytecode.RMarkDead != 0 && (o == r.regexpExecFn || o == r.strReplaceFn):
		v, err = r.regDeadCall(o, recv, args)
	default:
		v, err = r.regMarkedRest(o, recv, args)
	}
	if err != nil {
		return Undefined, err
	}
	if rx != nil {
		r.regexpLiteralDone(regConst(f.cl.fn, c, k), rx)
	}
	return v, nil
}

// regApplyArgs is apply_args: target.apply(this, the frame's arguments),
// which the built-in apply makes with the arguments as passed, no object
// made; any other apply is given one, made once and kept in the arguments
// binding's register, slot, so that every call is given the same one.
func (r *Runtime) regApplyArgs(f *frame, target, apply, this Value, slot *Value) (Value, error) {
	if !apply.IsObject() || apply.Object() != r.applyFn {
		o := *slot
		if !o.IsObject() {
			o = Obj(r.newArgumentsObject(f))
			*slot = o
		}
		return r.call(apply, target, []Value{this, o})
	}
	args := f.args
	if f.cl.fn.MappedArguments {
		n := min(f.cl.fn.ParamCount, len(args), len(f.locals))
		for i := 0; i < n; i++ {
			if !f.locals[i].sameBits(args[i]) {
				// A parameter has been assigned, which a mapped object
				// would show.
				args = append([]Value(nil), args...)
				copy(args, f.locals[:n])
				break
			}
		}
	}
	return r.applyCall(target, this, args)
}

// regWindow is the frame's registers from base. The register form's
// verifier proves that every register an instruction names is below the
// frame's count, which the frame has, so the array's length is never
// reached past them.
//
//go:nocheckptr
func regWindow(stack []Value, base int) *[1 << 16]Value {
	return (*[1 << 16]Value)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(stack)), uintptr(base)*unsafe.Sizeof(Value{})))
}

// regAt is the instruction at pc, which the verifier proves is in the code.
func regAt(code unsafe.Pointer, pc int) bytecode.RInstr {
	return *(*bytecode.RInstr)(unsafe.Add(code, uintptr(pc)*8))
}

// The kinds of regCode's leaf.
const (
	regLeafNone = iota
	regLeafPure
	regLeafThis
	// regLeafCode is run as its register code by regLeafRun, with no
	// frame, at the call site.
	regLeafCode
	// regLeafArg returns its parameter leafArg, and regLeafConst the
	// constant leafVal, undefined for nothing: rhCallW answers them from
	// the window and the code.
	regLeafArg
	regLeafConst
)

// regTrace prints why a function has no register code.
var regTrace = os.Getenv("QJS_REGTRACE") != ""

// regDump prints each function's register code as it is made.
var regDump = os.Getenv("QJS_REGDUMP") != ""

// regWordKind reports whether a value of tag bits is one that is === only
// to a value of the same words: undefined, null, a boolean, a symbol or an
// object.
func regWordKind(bits uint64) bool {
	return bits-tagBase < heapBigBits-tagBase &&
		1<<(bits&0xFF)&(1<<KindUndefined|1<<KindNull|1<<KindBool|1<<KindSymbol|1<<KindObject) != 0
}

// regTruthy is ToBoolean, a boolean's without the call.
func regTruthy(v Value) bool {
	return math.Float64bits(v.num) == trueBits || math.Float64bits(v.num) != falseBits && v.Truthy()
}

// regJumpOff is a jump's offset: BC for a jump of a register's
// truthiness or none, SC for a compare-and-branch.
func regJumpOff(w bytecode.RInstr) int32 {
	switch w.Op() {
	case bytecode.RJmp, bytecode.RJmpF, bytecode.RJmpT:
		return int32(w.BC())
	}
	return w.SC()
}

// regNaN is the canonical NaN as a Value, in a variable rather than a
// constant: Go's loop-invariant pass takes a constant out of the loop into a
// register of its own, one for each use, which the loop then saves and
// restores around every case that calls anything.
var regNaN = Value{num: math.Float64frombits(canonicalNaN)}

// regNum is Float, its NaN read from regNaN.
func regNum(f float64) Value {
	if f != f {
		return regNaN
	}
	return Value{num: f}
}

// regIsInt32 reports whether f, a number's, is an int32 exactly: what a
// bitwise operator's fast path takes without converting, once its case
// has seen the operand is a number (IsNumber). The tag test is not
// redundant in time: without it (a NaN fails the round trip's compare as
// well), loop_int32 was 8.1% slower at every placement. A bool, so that
// the case converts once more for the value and no phi of it and 0 is
// left to Go's branch elimination, which made it conditional moves.
//
// It and regTagged are runReg's own helpers, each of an inlining cost of
// 20 or less, which Go inlines into a big function as well
// (docs/register-interpreter.md, "The loop"): what costs more is written
// out in the loop.
func regIsInt32(f float64) bool {
	return float64(int32(f)) == f
}

// regVerifyAdd is storeCached's check of an addition the site's cache
// answers, in a build with the quickjs_verify tag, for set_prop's case:
// out of line, so that the case calls nothing Go would inline into it.
//
//go:noinline
func regVerifyAdd(r *Runtime, s *propCache, obj *Object, cl *closure, c *regCode, w bytecode.RInstr) {
	r.verifyStoreCache(s, obj, cl.names[c.siteName[w.C()]])
}

// regTagged reports whether bits, a value's, are a tagged value of kind k:
// Value.isTag of bits the caller has read.
func regTagged(bits uint64, k Kind) bool {
	return bits&tagMask == tagBase && Kind(bits&0xFF) == k
}

// regKept is what the loop keeps in memory across a call: a struct Go does
// not keep in registers (it has an array of more than one element), whose
// address every call the loop makes is given, so that Go cannot take a load
// of it after the call for the value stored before: the loop's pc is then
// not live across the call, and is not saved at every dispatch.
type regKept struct {
	pc  int
	pad [2]int
}

// regTypedAt is the address of a typed array's element at k, read and
// written as native code does (cacheView): from the view's data pointer,
// below its length, while its epoch is the runtime's. Nil -- another
// object, a key that is no index in the view, a stale view, a BigInt
// kind's -- leaves the element to regElemMiss. regSetMiss calls it once a
// set_index's dense element has missed, so that asm.js-style heap writes
// make no call; runReg's typed tail has its reads written out, as it,
// regTypedStore and a load by the element's kind would cost past the 20
// Go inlines into a big function. It and regTypedStore are two so that
// each stays below Go's inlining budget, which TestIntendedInlining pins.
func regTypedAt(r *Runtime, o *Object, k float64) (*typedArrayData, unsafe.Pointer) {
	if o.class == ClassTypedArray {
		if t, ok := o.dataOf().(*typedArrayData); ok && t.epoch == r.typedEpoch && t.kind <= elemFloat64 {
			if i := uint32(k); float64(i) == k && uint(i) < uint(t.length) {
				return t, unsafe.Add(t.data, uintptr(i)<<elemShifts[t.kind&15])
			}
		}
	}
	return nil, nil
}

// regTypedStore stores n as the element at p: a float kind's any number,
// an integer kind's (Uint8ClampedArray's aside) ToInt32(n) wrapped to its
// width, for n below 2^63 in magnitude, as regIsInt32 takes it. False -- a
// view of an immutable buffer, another kind, a NaN, an infinity or a
// larger number -- leaves the store to regSetMiss.
func regTypedStore(t *typedArrayData, p unsafe.Pointer, n float64) bool {
	if t.readonly {
		return false
	}
	switch t.kind {
	case elemFloat64:
		*(*float64)(p) = n
		return true
	case elemFloat32:
		*(*float32)(p) = float32(n)
		return true
	}
	if math.Float64bits(n)&(1<<63-1) < regInt64Bound {
		switch x := int64(n); t.kind {
		case elemInt32, elemUint32:
			*(*int32)(p) = int32(x)
			return true
		case elemInt8, elemUint8:
			*(*int8)(p) = int8(x)
			return true
		case elemInt16, elemUint16:
			*(*int16)(p) = int16(x)
			return true
		}
	}
	return false
}

// regInt64Bound is 2^63's bits: a float64 whose bits without the sign are
// below it converts to an int64 exactly, and a NaN's, an infinity's and a
// tagged value's are not.
const regInt64Bound = 0x43e0000000000000
