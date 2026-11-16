package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Destructuring and parameter lists in the register form
// (docs/register-interpreter.md, "Stage 2: zero refusals"): the cold
// operations of a pattern and of a rest parameter, each what the stack
// form's instruction of the same name does.

// regColdPattern runs the pattern's cold operation w in frame f.
func (r *Runtime) regColdPattern(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	var v Value
	var err error
	switch bytecode.RColdOp(w.X()) {
	case bytecode.RColdIterStep:
		v, err = r.iterStep(regs[w.B()])
	case bytecode.RColdIterRest:
		v, err = r.iterRest(regs[w.B()])
	case bytecode.RColdCheckCoercible:
		if x := regs[w.A()]; x.IsNullish() {
			return r.throwTypeError("cannot destructure %s", r.describe(x))
		}
		return nil
	case bytecode.RColdToPropertyKey:
		// A string or a symbol is a key already.
		x := regs[w.A()]
		if x.IsString() || x.IsSymbol() {
			return nil
		}
		k, err := r.toPropertyKey(x)
		if err != nil {
			return err
		}
		v = r.keyToValue(k)
	case bytecode.RColdObjectRest:
		b := int(w.C())
		v, err = r.objectRest(regs[w.B()], regs[b:b+int(c.rc.Aux[pc])])
	case bytecode.RColdToString:
		var s *String
		if s, err = r.toString(regs[w.A()]); err == nil {
			v = Str(s)
		}
	case bytecode.RColdTemplateObject:
		v = Obj(r.templateObject(f.cl.fn, int(w.BC())))
	case bytecode.RColdRestParam:
		var rest []Value
		if i := int(w.B()); i < len(f.args) {
			rest = f.args[i:]
		}
		v = Obj(r.newArrayFrom(rest))
	}
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regColdObject runs an object literal's cold operation w in frame f, each
// what the stack form's instruction of the same name does.
func (r *Runtime) regColdObject(f *frame, rc *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a, b, c := regs[w.A()], regs[w.B()], regs[w.C()]
	switch op := bytecode.RColdOp(w.X()); op {
	case bytecode.RColdCopyDataProps:
		if a.IsObject() && !b.IsNullish() {
			return r.copyDataProps(a.Object(), b)
		}
	case bytecode.RColdDefineGetter, bytecode.RColdDefineSetter:
		if a.IsObject() && b.IsObject() {
			r.defineHalfAccessor(a.Object(), regName(f.cl, rc, pc, w.C()), b.Object(), op == bytecode.RColdDefineGetter, memberFlags(0))
		}
	case bytecode.RColdDefineGetterIndex, bytecode.RColdDefineSetterIndex:
		k, err := r.toPropertyKey(b)
		if err != nil {
			return err
		}
		if a.IsObject() && c.IsObject() {
			return r.defineHalfAccessorChecked(a.Object(), k, c.Object(), op == bytecode.RColdDefineGetterIndex, memberFlags(0))
		}
	case bytecode.RColdDefineIndex:
		k, err := r.toPropertyKey(b)
		if err != nil {
			return err
		}
		if a.IsObject() {
			return r.createDataProperty(a.Object(), k, c, memberFlags(0))
		}
	case bytecode.RColdSetFuncName:
		if a.IsObject() {
			if fd := a.Object().fn(); fd != nil {
				fd.name = functionNameFromKey(b, uint32(w.C()))
			}
		}
	case bytecode.RColdSetHomeObject:
		if a.IsObject() && b.IsObject() {
			if fd := a.Object().fn(); fd != nil {
				fd.setHomeObject(b.Object())
			}
		}
	case bytecode.RColdArrayPush:
		if a.IsObject() {
			o := a.Object()
			o.extOf().elems = append(o.elemsOf(), b)
		}
	case bytecode.RColdArraySpread:
		if a.IsObject() {
			return r.spreadInto(a.Object(), b)
		}
	case bytecode.RColdCallSpread:
		base := int(w.A())
		callee, args := regs[base], regs[base+2]
		var callArgs []Value
		if args.IsObject() {
			callArgs = args.Object().elemsOf()
		}
		var v Value
		var err error
		if w.C() == 1 {
			v, err = r.construct(callee, callArgs)
		} else {
			v, err = r.call(callee, regs[base+1], callArgs)
		}
		if err != nil {
			return err
		}
		regs[w.B()] = v
	case bytecode.RColdSetProtoOf:
		if a.IsObject() {
			switch {
			case b.IsObject():
				a.Object().setProto(b.Object())
			case b.IsNull():
				a.Object().setProto(nil)
			}
		}
	}
	return nil
}
