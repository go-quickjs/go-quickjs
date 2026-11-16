package regcomp

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// Classes, private names, super, this and new.target, as the stack form's
// compileClass and its member compilers make them: cold operations of the
// stack form's instructions' names, in the same order.

func (g *gen) cold(sub bytecode.ROp, a, b, c int) int {
	return g.emit(bytecode.MakeR(sub, 0, r16(a), r16(b), r16(c)))
}

// thisExpr compiles this: register 1, or, where this is a derived
// constructor's binding, its value, checked (this_bound).
func (g *gen) thisExpr(n *ast.This, dst int) int {
	if !g.thisRef || g.thisBound {
		return g.place(regThis, dst)
	}
	d := g.dest(dst)
	g.at(n.Start)
	g.cold(bytecode.RThis, d, 0, 0)
	return d
}

// thisInto puts this in register d, as thisExpr reads it, at pos.
func (g *gen) thisInto(d, pos int) {
	if !g.thisRef || g.thisBound {
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(d), regThis, 0))
		return
	}
	g.at(pos)
	g.cold(bytecode.RThis, d, 0, 0)
}

// retReg returns register r, through derived_ret in a derived constructor.
func (g *gen) retReg(r int) {
	if g.fn.Kind == bytecode.KindDerivedConstructor {
		t := g.alloc()
		g.cold(bytecode.RDerivedRet, g.place(r, t), 0, 0)
		r = t
	}
	g.emit(bytecode.MakeR(bytecode.RRet, 0, r16(r), 0, 0))
}

// retUndef returns undefined, through derived_ret in a derived constructor.
func (g *gen) retUndef() {
	if g.fn.Kind == bytecode.KindDerivedConstructor {
		t := g.alloc()
		g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(t), 0, 0))
		g.retReg(t)
		return
	}
	g.emit(bytecode.MakeR(bytecode.RRetUndef, 0, 0, 0, 0))
}

// isSuper reports whether e is super.
func isSuper(e ast.Expr) bool {
	_, ok := e.(*ast.Super)
	return ok
}

// special reports whether m is a super property or a private member, which
// the backend reaches through cold operations.
func special(m *ast.Member) bool {
	if isSuper(m.Object) {
		return true
	}
	_, ok := m.Property.(*ast.PrivateName)
	return ok
}

// sref is a super property or a private member, its parts evaluated: for
// super[k], the base and the key; for o.#x, the object and the key's
// binding; for super.x, nothing.
type sref struct {
	m        *ast.Member
	super    bool
	obj, key int
	name     string
	// pos is where a read throws: the member's, or a called private
	// method's name.
	pos int
}

// specialRef evaluates m's parts, as the stack form does: super[k]'s base,
// then its key, converted where convert says (an update's); o.#x's object.
func (g *gen) specialRef(m *ast.Member, convert bool) sref {
	s := sref{m: m, pos: int(m.Start)}
	if isSuper(m.Object) {
		s.super = true
		if m.Computed {
			s.obj = g.alloc()
			g.at(int(m.Start))
			g.cold(bytecode.RSuperBase, s.obj, 0, 0)
			s.key = g.alloc()
			g.into(m.Property, s.key)
			if convert {
				g.at(m.Property.Pos())
				g.cold(bytecode.RToPropertyKey, s.key, 0, 0)
			}
			return s
		}
		name := memberName(m)
		s.name = name
		return s
	}
	pn := m.Property.(*ast.PrivateName)
	s.obj = g.expr(m.Object, -1)
	if g.isBinding(s.obj) {
		// What is evaluated after may assign the binding.
		s.obj = g.place(s.obj, g.alloc())
	}
	s.key = g.privateKey(pn)
	return s
}

// privateKey is the register holding private name pn's key.
func (g *gen) privateKey(pn *ast.PrivateName) int {
	// The stack compiler resolves every private name it compiles, as a
	// binding of the function's, which layout gives a register, or an
	// upvalue; anything else is its bug.
	res, ok := g.rec.Privates[pn]
	switch {
	case !ok:
		panic("regcomp: a private name the record does not resolve: #" + pn.Name)
	case res.Kind == ResLocal:
		if int(res.Index) >= len(g.slotReg) || g.slotReg[res.Index] < 0 {
			panic("regcomp: a private name's key without a register: #" + pn.Name)
		}
		return int(g.slotReg[res.Index])
	}
	d := g.alloc()
	g.emit(bytecode.MakeRBC(bytecode.RGetUpval, 0, r16(d), res.Index))
	return d
}

// specialGet reads s into dst.
func (g *gen) specialGet(s sref, dst int) int {
	d := g.dest(dst)
	m := s.m
	switch {
	case s.super && m.Computed:
		g.at(int(m.Start))
		g.cold(bytecode.RSuperGetIndex, d, s.obj, s.key)
	case s.super:
		g.at(int(m.Start))
		g.far(g.cold(bytecode.RSuperGet, d, int(g.name16(s.name)), 0), s.name)
	default:
		g.at(s.pos)
		g.privateAux(g.emit(bytecode.MakeR(bytecode.RGetPrivate, 0, r16(d), r16(s.obj), r16(s.key))), s)
	}
	return d
}

// specialSet stores v to s.
func (g *gen) specialSet(s sref, v, pos int) {
	m := s.m
	g.at(pos)
	switch {
	case s.super && m.Computed:
		g.cold(bytecode.RSuperSetIndex, s.obj, s.key, v)
	case s.super:
		g.far(g.cold(bytecode.RSuperSet, v, int(g.name16(s.name)), 0), s.name)
	default:
		g.privateAux(g.emit(bytecode.MakeR(bytecode.RSetPrivate, 0, r16(s.obj), r16(v), r16(s.key))), s)
	}
}

// privateAux is a private member's read's or write's Aux: a cache site of
// its own in the high 16 bits, the name, with its #, for the errors in
// the low.
func (g *gen) privateAux(pc int, s sref) {
	name := "#" + s.m.Property.(*ast.PrivateName).Name
	g.aux(pc, uint32(g.site16(siteKey{kind: "private", name: g.nameIdx(name)}))<<16|uint32(g.name16(name)))
	g.far(pc, name)
}

// specialMember reads super.x, super[k] or o.#x into dst.
func (g *gen) specialMember(m *ast.Member, dst int) int {
	return g.specialGet(g.specialRef(m, false), dst)
}

// specialStore compiles m = value.
func (g *gen) specialStore(m *ast.Member, value ast.Expr, dst int) int {
	s := g.specialRef(m, false)
	v := g.expr(value, -1)
	if g.isBinding(v) {
		v = g.place(v, g.alloc())
	}
	g.specialSet(s, v, int(m.Start))
	return g.place(v, dst)
}

// specialCompound compiles m op= value.
func (g *gen) specialCompound(m *ast.Member, text string, op bytecode.Op, value ast.Expr, pos, dst int) int {
	s := g.specialRef(m, true)
	cur := g.specialGet(s, g.alloc())
	v := g.operate(text, op, nil, value, cur, cur, pos)
	g.specialSet(s, v, int(m.Start))
	return g.place(v, dst)
}

// specialUpdate compiles ++m, m++, --m and m--.
func (g *gen) specialUpdate(n *ast.Update, m *ast.Member, step bytecode.ROp, keep bool, dst int) int {
	s := g.specialRef(m, true)
	cur := g.specialGet(s, g.alloc())
	return g.stepped(n, cur, step, keep, dst, func(v int) { g.specialSet(s, v, n.Start) })
}

// specialCallee puts the function m names and its receiver in window w:
// super.m's this, checked, or o.#m's object.
func (g *gen) specialCallee(m *ast.Member, w int) {
	if isSuper(m.Object) {
		g.thisInto(w+1, int(m.Start))
		g.specialGet(g.specialRef(m, false), w)
		return
	}
	s := g.specialRef(m, false)
	g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(w+1), r16(s.obj), 0))
	s.obj, s.pos = w+1, m.Property.Pos()
	g.specialGet(s, w)
}

// superCall compiles super(...): the parent constructed, this bound, and
// its value this, which a derived constructor's own call also puts in
// register 1.
func (g *gen) superCall(n *ast.Call, dst int) int {
	d := g.dest(dst)
	switch {
	case n.Forward:
		g.at(n.TracePos())
		g.cold(bytecode.RSuperForward, d, 0, 0)
		g.forwardsArgs = true
	case wideArgs(n.Args):
		a := g.alloc()
		g.spreadArray(n.Args, a)
		g.at(n.TracePos())
		g.cold(bytecode.RSuperSpread, d, a, 0)
	default:
		w := g.allocN(len(n.Args))
		g.args(n.Args, w)
		g.at(n.TracePos())
		g.cold(bytecode.RSuperCall, d, len(n.Args), w)
	}
	return d
}

// deleteSuper compiles delete super.x and delete super[k], which throw
// once the reference is made: this checked, then the key evaluated.
func (g *gen) deleteSuper(n *ast.Unary, m *ast.Member) {
	if m.Computed {
		g.cold(bytecode.RCheckThis, 0, 0, 0)
		g.discard(m.Property)
	}
	g.at(n.Start)
	g.cold(bytecode.RDeleteSuper, 0, 0, 0)
}

// privateIn compiles #x in o.
func (g *gen) privateIn(pn *ast.PrivateName, obj ast.Expr, pos, dst int) int {
	o := g.expr(obj, -1)
	k := g.privateKey(pn)
	d := g.dest(dst)
	g.at(pos)
	g.cold(bytecode.RPrivateIn, d, o, k)
	return d
}

// classSlot is the register of a class scope's binding in slot.
func (g *gen) classSlot(slot int32) int {
	if slot < 0 || int(slot) >= len(g.slotReg) || g.slotReg[slot] < 0 {
		// layout gives each of a class's bindings a register.
		panic("regcomp: a class binding without a register")
	}
	return int(g.slotReg[slot])
}

// class compiles class cls into dst, as compileClass makes it.
func (g *gen) class(cls *ast.ClassLit, dst int) int {
	cr := g.rec.Classes[cls]
	if cr == nil {
		// The stack compiler records every class it compiles.
		panic("regcomp: a class the record does not name")
	}
	var scope []int
	if cr.Self >= 0 {
		r := g.classSlot(cr.Self)
		scope = append(scope, r)
		g.emit(bytecode.MakeR(bytecode.RLoadUninit, 0, r16(r), 0, 0))
	}
	parent := -1
	if cls.Extends != nil {
		parent = g.alloc()
		g.into(cls.Extends, parent)
	}
	for _, p := range cr.Privates {
		r := g.classSlot(int32(p.Slot))
		scope = append(scope, r)
		g.at(cls.Start)
		g.far(g.cold(bytecode.RPrivateName, r, int(g.name16(p.Name)), 0), p.Name)
	}
	install := -1
	if cr.Install >= 0 {
		install = g.classSlot(cr.Install)
		scope = append(scope, install)
		g.cold(bytecode.RNewPrivateMethods, install, 0, 0)
	}
	for i, f := range cls.Fields {
		if i >= len(cr.Keys) || cr.Keys[i] < 0 {
			continue
		}
		r := g.classSlot(cr.Keys[i])
		scope = append(scope, r)
		t := g.alloc()
		g.into(f.Key, t)
		g.at(f.Key.Pos())
		g.cold(bytecode.RToPropertyKey, t, 0, 0)
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(r), r16(t), 0))
	}
	ctor := g.alloc()
	g.closureConst(int(cr.Ctor), ctor)
	if parent >= 0 {
		g.at(cls.Start)
		g.cold(bytecode.RNewClass, ctor, parent, 0)
	}
	if cr.Self >= 0 {
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(g.classSlot(cr.Self)), r16(ctor), 0))
	}
	var proto int
	prototype := func() int {
		p := g.alloc()
		g.emit(bytecode.MakeR(bytecode.RGetProp, 0, r16(p), r16(ctor), g.propSite(bytecode.OpGetProp, g.nameIdx("prototype"))))
		return p
	}
	if cr.Init >= 0 {
		proto = prototype()
		fn := g.alloc()
		g.closureConst(int(cr.Init), fn)
		g.cold(bytecode.RSetHomeObject, fn, proto, 0)
		g.cold(bytecode.RSetFieldInit, ctor, fn, 0)
	}
	for i, m := range cls.Members {
		if g.err != "" {
			break
		}
		if i >= len(cr.Members) || cr.Members[i] < 0 {
			continue
		}
		g.classMember(m, int(cr.Members[i]), ctor, install, prototype)
	}
	for _, st := range cr.Statics {
		if g.err != "" {
			break
		}
		w := g.allocN(2)
		g.closureConst(int(st.Const), w)
		g.cold(bytecode.RSetHomeObject, w, ctor, 0)
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(w+1), r16(ctor), 0))
		pos := cls.Start
		if st.Field >= 0 && int(st.Field) < len(cls.Fields) {
			pos = cls.Fields[st.Field].Start
		}
		g.at(pos)
		g.emit(bytecode.MakeR(bytecode.RCallMW, 0, r16(w), r16(w), g.callSite(bytecode.RCallMW)))
	}
	// The class scope's bindings closures keep are closed: the next
	// evaluation of the class makes new ones.
	for _, r := range scope {
		if g.capturedReg(r) {
			g.cold(bytecode.RCloseRegs, r, 1, 0)
		}
	}
	return g.place(ctor, dst)
}

// capturedReg reports whether a closure may capture the binding in register r.
func (g *gen) capturedReg(r int) bool {
	for slot, sr := range g.slotReg {
		if int(sr) == r {
			return slot < len(g.fn.Locals) && g.fn.Locals[slot].Captured
		}
	}
	return false
}

// classMember defines member m, whose function is constant k, on the class
// ctor: a method or an accessor of the prototype or of the class, keyed by
// a name, a computed key or a private name.
func (g *gen) classMember(m ast.Property, k, ctor, install int, prototype func() int) {
	key := ""
	if !m.Computed {
		key = keyName(m.Key)
	}
	nameKind := 0
	switch m.Kind {
	case ast.PropGet:
		nameKind = 1
	case ast.PropSet:
		nameKind = 2
	}
	if pn, private := m.Key.(*ast.PrivateName); private && !m.Static {
		proto := prototype()
		fn := g.alloc()
		g.closureConst(k, fn)
		g.cold(bytecode.RSetHomeObject, fn, proto, 0)
		sub := bytecode.RAddPrivateMethod
		switch m.Kind {
		case ast.PropGet:
			sub = bytecode.RAddPrivateGetter
		case ast.PropSet:
			sub = bytecode.RAddPrivateSetter
		}
		g.cold(sub, install, fn, g.privateKey(pn))
		return
	}
	target := ctor
	if !m.Static {
		target = prototype()
	}
	if m.Computed {
		kr := g.alloc()
		g.into(m.Key, kr)
		g.at(m.Key.Pos())
		g.cold(bytecode.RToPropertyKey, kr, 0, 0)
		fn := g.alloc()
		g.closureConst(k, fn)
		g.cold(bytecode.RSetFuncName, fn, kr, nameKind)
		g.cold(bytecode.RSetHomeObject, fn, target, 0)
		sub := bytecode.RClassIndex
		switch m.Kind {
		case ast.PropGet:
			sub = bytecode.RClassGetterIndex
		case ast.PropSet:
			sub = bytecode.RClassSetterIndex
		}
		g.cold(sub, target, kr, fn)
		return
	}
	fn := g.alloc()
	g.closureConst(k, fn)
	g.cold(bytecode.RSetHomeObject, fn, target, 0)
	if pn, private := m.Key.(*ast.PrivateName); private {
		sub := bytecode.RDefinePrivateMethod
		switch m.Kind {
		case ast.PropGet:
			sub = bytecode.RDefinePrivateGetter
		case ast.PropSet:
			sub = bytecode.RDefinePrivateSetter
		}
		g.cold(sub, target, fn, g.privateKey(pn))
		return
	}
	sub := bytecode.RDefineMethod
	switch m.Kind {
	case ast.PropGet:
		sub = bytecode.RClassGetter
	case ast.PropSet:
		sub = bytecode.RClassSetter
	}
	g.far(g.cold(sub, target, fn, int(g.name16(key))), key)
}

// keyName is the name a key that is not computed gives a class's member,
// as the stack form's propKeyName gives it.
func keyName(key ast.Expr) string {
	switch k := key.(type) {
	case *ast.Ident:
		return k.Name
	case *ast.StringLit:
		return k.Value
	case *ast.NumberLit:
		return jsnum.FormatFloat(k.Value)
	case *ast.PrivateName:
		return "#" + k.Name
	}
	return ""
}

// fieldInit compiles one field of a field initializer: created on this,
// a private one added, as compileFieldInit makes it.
func (g *gen) fieldInit(n *ast.FieldInit) {
	this := g.alloc()
	g.thisInto(this, n.Start)
	if pn, private := n.Key.(*ast.PrivateName); private {
		v := g.expr(n.Value, -1)
		k := g.privateKey(pn)
		g.at(n.Start)
		g.cold(bytecode.RDefinePrivate, this, v, k)
		return
	}
	k := g.alloc()
	if !n.Computed {
		g.expr(&ast.StringLit{Value: keyName(n.Key)}, k)
		v := g.expr(n.Value, -1)
		g.at(n.Start)
		g.cold(bytecode.RDefineIndex, this, k, v)
		return
	}
	g.into(n.Key, k)
	g.at(n.Key.Pos())
	g.cold(bytecode.RToPropertyKey, k, 0, 0)
	v := g.expr(n.Value, -1)
	if anonymousFn(n.Value) {
		if g.isBinding(v) {
			v = g.place(v, g.alloc())
		}
		g.cold(bytecode.RSetFuncName, v, k, 0)
	}
	g.at(n.Start)
	g.cold(bytecode.RDefineIndex, this, k, v)
}

// installPrivate compiles the installation of the class's private methods
// on this.
func (g *gen) installPrivate(n *ast.InstallPrivateMethods) {
	l := g.expr(n.Binding, -1)
	g.at(n.Start)
	g.cold(bytecode.RInstallPrivateMethods, l, 0, 0)
}

// newTarget compiles new.target.
func (g *gen) newTarget(dst int) int {
	d := g.dest(dst)
	g.cold(bytecode.RNewTarget, d, 0, 0)
	return d
}
