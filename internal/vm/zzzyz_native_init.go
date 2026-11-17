package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// nativeCtorThis caches the constructor's own prototype slot in the this
// word a construction never reads as a JavaScript value: its shape in ref
// and the slot in num. The value is read afresh, so assigning .prototype
// takes effect without changing the shape.
func (r *Runtime) nativeCtorThis(e *regCallCache, fd *funcData) *Object {
	o := e.callee
	s := o.shape
	var p Value
	if s != nil && s == (*shape)(e.this.ref) {
		p = o.vals()[int(e.this.num)]
	} else {
		i := o.findOwn(atomPrototype)
		if i < 0 || s == nil || s.loose || s.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
			return r.regCtorThis(o, fd)
		}
		e.this = Value{num: float64(i), ref: unsafe.Pointer(remember(s))}
		p = o.vals()[i]
	}
	if !p.IsObject() {
		return nil
	}
	proto := p.Object()
	root := r.shapes.ctorRoot(fd, proto)
	if root != nil && !root.tracked() && fd.ctorKind != ctorDerived {
		return nil
	}
	return newOrdinary[struct{}](fd, root, proto, root.roomFor(int(fd.closure.fn.ThisProps)))
}

const (
	rlInit = rlRetUndef + 1 + iota
	rlInitZero
	rlInitCall
	rlInitArg
	rlInitConst
)

// regInitOps recognizes parameter stores, with optional falsy-to-zero
// defaults. These plans share leafOps's storage but never run as regLeafCode.
func regInitOps(fn *bytecode.Function, rc *bytecode.RegCode) []regLeafOp {
	if fn.Kind != bytecode.KindNormal && fn.Kind != bytecode.KindMethod ||
		fn.Generator || fn.Async || fn.HasRest || !fn.HasSimpleParams || fn.HasDirectEval || fn.UsesArguments ||
		rc.Params > 64 {
		return nil
	}
	if code := rc.Code; len(code) >= 5 && code[0].Op() == bytecode.RGetProp {
		return regForwardCallOps(fn, rc)
	}
	param := func(r uint16) bool { return r >= 2 && int(r) < 2+rc.Params }
	var defaults uint64
	var stores [8]regLeafOp
	n := 0
	code := rc.Code
	for pc := 0; pc < len(code); {
		w := code[pc]
		zero := false
		var p uint16
		switch {
		case w.Op() == bytecode.RJmpT && param(w.A()) && int32(w.BC()) == 1 && pc+1 < len(code):
			next := code[pc+1]
			if next.Op() != bytecode.RLoadInt || next.A() != w.A() || next.BC() != 0 {
				return nil
			}
			defaults |= 1 << (w.A() - 2)
			pc += 2
			continue
		case w.Op() == bytecode.RJmpF && param(w.A()) && int32(w.BC()) == 2 && pc+4 < len(code):
			move, jump, load, set := code[pc+1], code[pc+2], code[pc+3], code[pc+4]
			if move.Op() != bytecode.RMove || move.B() != w.A() || int(move.A()) < 2+rc.Params ||
				jump.Op() != bytecode.RJmp || int32(jump.BC()) != 1 ||
				load.Op() != bytecode.RLoadInt || load.A() != move.A() || load.BC() != 0 ||
				set.Op() != bytecode.RSetProp || set.A() != 1 || set.B() != move.A() {
				return nil
			}
			p, zero, w = code[pc].A()-2, true, set
			pc += 5
		case w.Op() == bytecode.RSetProp && w.A() == 1 && param(w.B()):
			p = w.B() - 2
			zero = defaults&(1<<p) != 0
			pc++
		case w.Op() == bytecode.RRetUndef:
			if n == 0 {
				return nil
			}
			return append([]regLeafOp(nil), stores[:n]...)
		default:
			return nil
		}
		if n == len(stores) {
			return nil
		}
		op := uint8(rlInit)
		if zero {
			op = rlInitZero
		}
		stores[n] = regLeafOp{op: op, a: w.C(), b: p}
		n++
	}
	return nil
}

// nativeInit validates every store before writing into the receiver's
// existing room. No intermediate shape can be observed: nothing calls code.
func (r *Runtime) nativeInit(cl *closure, o *Object, args []Value) bool {
	ops := cl.reg.leafOps
	if len(ops) == 0 || ops[0].op != rlInit && ops[0].op != rlInitZero {
		return false
	}
	s := o.shape
	for _, op := range ops {
		c := &cl.ic[op.a]
		if s == nil || c.shape != s || c.getter {
			return false
		}
		if c.next != nil {
			if !c.adds(o) {
				return false
			}
			s = c.next
		}
	}
	n := len(s.keys)
	if uint(n) > uint(o.valc) || n > linearScanLimit && s.index == nil {
		return false
	}
	vals := o.vals()[:n]
	for _, op := range ops {
		c := &cl.ic[op.a]
		idx := int(c.idx)
		if c.next != nil {
			idx = len(c.next.keys) - 1
		}
		v := Undefined
		if int(op.b) < len(args) {
			v = args[op.b]
		}
		if op.op == rlInitZero && !v.Truthy() {
			v = Int(0)
		}
		if verifyShapes {
			r.verifyStoreCache(c, o, cl.names[cl.reg.siteName[op.a]])
			if c.next != nil {
				o.appendTransition(v, c.next)
			} else {
				vals[idx] = v
			}
		} else {
			vals[idx] = v
		}
	}
	o.shape, o.valn = s, uint32(n)
	return true
}

// nativeForwardInit answers this.k.apply(this, arguments) and
// this.k.call(this, parameters or constants) where the target
// only initializes data properties. No script runs, so neither forwarding
// nor apply needs a frame that script could observe.
func (r *Runtime) nativeForwardInit(cl *closure, o *Object, args []Value) bool {
	var method, call uint16
	builtin := r.applyFn
	ops := cl.reg.leafOps
	if len(ops) != 0 && ops[0].op == rlInitCall {
		method, call, builtin = ops[0].a, ops[0].b, r.callFn
	} else if leafKind(cl) == bytecode.LeafForward {
		code := cl.reg.rc.Code
		method, call = code[0].C(), code[1].C()
	} else {
		return false
	}
	key := cl.names[cl.reg.siteName[method]]
	m, ok := plainOwn(o, key)
	if !ok {
		m, ok = r.cachedData(r.fwdCache(cl, int(method), o, key), o, key)
	}
	if !ok || !m.IsObject() {
		return false
	}
	fd := m.Object().fn()
	if fd == nil || fd.bound || fd.closure == nil || fd.closure.reg == nil {
		return false
	}
	if ap, ok := r.cachedData(&cl.ic[call], m.Object(), cl.names[cl.reg.siteName[call]]); !ok ||
		!ap.IsObject() || ap.Object() != builtin {
		return false
	}
	if builtin == r.callFn {
		var forwarded [8]Value
		for i, op := range ops[1:] {
			v := op.k
			if op.op == rlInitArg {
				v = Undefined
				if int(op.b) < len(args) {
					v = args[op.b]
				}
			}
			forwarded[i] = v
		}
		return r.nativeInit(fd.closure, o, forwarded[:len(ops)-1])
	}
	return r.nativeInit(fd.closure, o, args)
}

// regForwardCallOps records a complete forwarding body, including the
// reads of the method and its call property. Only argument loads follow
// those reads, so the plan can validate them all before writing anything.
func regForwardCallOps(fn *bytecode.Function, rc *bytecode.RegCode) []regLeafOp {
	code := rc.Code
	m, c, t := code[0], code[1], code[2]
	names := rc.Names
	if names == nil {
		names = fn.Names
	}
	if m.B() != 1 || c.Op() != bytecode.RGetMethodW || c.B() != m.A() ||
		names[rc.SiteName[c.C()]] != "call" ||
		t.Op() != bytecode.RMove || t.A() != c.A()+2 || t.B() != 1 {
		return nil
	}
	var ops [9]regLeafOp
	ops[0] = regLeafOp{op: rlInitCall, a: m.C(), b: c.C()}
	n := 1
	for _, w := range code[3:] {
		if w.Op() == bytecode.RCallMW {
			if w.A() != c.A() || int(w.X()) != n {
				return nil
			}
			for _, ret := range code[3+n:] {
				if ret.Op() != bytecode.RRetUndef {
					return nil
				}
			}
			return append([]regLeafOp(nil), ops[:n]...)
		}
		if n == len(ops) || w.A() != c.A()+2+uint16(n) {
			return nil
		}
		op := regLeafOp{op: rlInitArg, b: w.B() - 2}
		if w.Op() != bytecode.RMove || w.B() < 2 || int(w.B()) >= 2+rc.Params {
			v, ok := regConstOf(regConsts(fn, rc), w)
			if !ok {
				return nil
			}
			op = regLeafOp{op: rlInitConst, k: v}
		}
		ops[n] = op
		n++
	}
	return nil
}
