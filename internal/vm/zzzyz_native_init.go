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
)

// regInitOps recognizes parameter stores, with optional falsy-to-zero
// defaults. These plans share leafOps's storage but never run as regLeafCode.
func regInitOps(fn *bytecode.Function, rc *bytecode.RegCode) []regLeafOp {
	if fn.Kind != bytecode.KindNormal && fn.Kind != bytecode.KindMethod ||
		fn.Generator || fn.Async || fn.HasRest || !fn.HasSimpleParams || fn.HasDirectEval || fn.UsesArguments ||
		rc.Params > 64 {
		return nil
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
	if len(ops) == 0 || ops[0].op < rlInit {
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
