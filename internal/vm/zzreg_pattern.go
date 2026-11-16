package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Destructuring and parameter lists in the register form
// (docs/register-interpreter.md, "Stage 2: zero refusals"): the cold
// operations of a pattern and of a rest parameter, each what the stack
// form's instruction of the same name does.

// regIterStep is iter_step.
func (r *Runtime) regIterStep(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	var v Value
	var err error
	v, err = r.iterStep(regs[w.B()])
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regIterRest is iter_rest.
func (r *Runtime) regIterRest(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	var v Value
	var err error
	v, err = r.iterRest(regs[w.B()])
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regCheckCoercible is check_coercible.
func (r *Runtime) regCheckCoercible(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	if x := regs[w.A()]; x.IsNullish() {
		return r.throwTypeError("cannot destructure %s", r.describe(x))
	}
	return nil
}

// regToPropertyKey is to_property_key.
func (r *Runtime) regToPropertyKey(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	var v Value
	var err error
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
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regObjectRest is object_rest.
func (r *Runtime) regObjectRest(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	var v Value
	var err error
	b := int(w.C())
	v, err = r.objectRest(regs[w.B()], regs[b:b+int(c.rc.Aux[pc])])
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regToString is to_string.
func (r *Runtime) regToString(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	var v Value
	var err error
	var s *String
	if s, err = r.toString(regs[w.A()]); err == nil {
		v = Str(s)
	}
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regTemplateObject is template_object.
func (r *Runtime) regTemplateObject(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	var v Value
	var err error
	v = Obj(r.templateObject(f.cl.fn, int(w.BC())))
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regRestParam is rest_param.
func (r *Runtime) regRestParam(f *frame, c *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	var v Value
	var err error
	var rest []Value
	if i := int(w.B()); i < len(f.args) {
		rest = f.args[i:]
	}
	v = Obj(r.newArrayFrom(rest))
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regCopyDataProps is copy_data_props.
func (r *Runtime) regCopyDataProps(f *frame, rc *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	if a.IsObject() && !b.IsNullish() {
		return r.copyDataProps(a.Object(), b)
	}
	return nil
}

// regDefineAccessor is define_getter, define_setter.
func (r *Runtime) regDefineAccessor(f *frame, rc *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	op := w.Op()
	if a.IsObject() && b.IsObject() {
		r.defineHalfAccessor(a.Object(), regName(f.cl, rc, pc, w.C()), b.Object(), op == bytecode.RDefineGetter, memberFlags(0))
	}
	return nil
}

// regDefineAccessorIndex is define_getter_index, define_setter_index.
func (r *Runtime) regDefineAccessorIndex(f *frame, rc *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	c := regs[w.C()]
	op := w.Op()
	k, err := r.toPropertyKey(b)
	if err != nil {
		return err
	}
	if a.IsObject() && c.IsObject() {
		return r.defineHalfAccessorChecked(a.Object(), k, c.Object(), op == bytecode.RDefineGetterIndex, memberFlags(0))
	}
	return nil
}

// regDefineIndex is define_index.
func (r *Runtime) regDefineIndex(f *frame, rc *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	c := regs[w.C()]
	k, err := r.toPropertyKey(b)
	if err != nil {
		return err
	}
	if a.IsObject() {
		return r.createDataProperty(a.Object(), k, c, memberFlags(0))
	}
	return nil
}

// regSetFuncName is set_func_name.
func (r *Runtime) regSetFuncName(f *frame, rc *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	if a.IsObject() {
		if fd := a.Object().fn(); fd != nil {
			fd.name = functionNameFromKey(b, uint32(w.C()))
		}
	}
	return nil
}

// regSetHomeObject is set_home_object.
func (r *Runtime) regSetHomeObject(f *frame, rc *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	if a.IsObject() && b.IsObject() {
		if fd := a.Object().fn(); fd != nil {
			fd.setHomeObject(b.Object())
		}
	}
	return nil
}

// regArrayPush is array_push.
func (r *Runtime) regArrayPush(f *frame, rc *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	if a.IsObject() {
		o := a.Object()
		o.extOf().elems = append(o.elemsOf(), b)
	}
	return nil
}

// regArraySpread is array_spread.
func (r *Runtime) regArraySpread(f *frame, rc *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	if a.IsObject() {
		return r.spreadInto(a.Object(), b)
	}
	return nil
}

// regCallSpread is call_spread.
func (r *Runtime) regCallSpread(f *frame, rc *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
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
	return nil
}

// regSetProtoOf is set_proto_of.
func (r *Runtime) regSetProtoOf(f *frame, rc *regCode, pc int, w bytecode.RInstr, regs *[1 << 16]Value) error {
	a := regs[w.A()]
	b := regs[w.B()]
	if a.IsObject() {
		switch {
		case b.IsObject():
			a.Object().setProto(b.Object())
		case b.IsNull():
			a.Object().setProto(nil)
		}
	}
	return nil
}
