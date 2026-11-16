package regcomp

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Destructuring (docs/register-interpreter.md, "Stage 2: zero refusals"):
// a pattern unpacks a value in a register into its leaves, as the stack
// form's compileArrayPattern and compileObjectPattern do, in the same order.
// An array pattern steps a cursor, which for_of_start makes, with iter_step,
// and closes it at the end; its steps are in the ranges of a handler of the
// cursor's, which closes it as a throw does and throws again, as a for-of
// loop's body is. A leaf that is a property is evaluated -- its object and
// key -- before the value it receives is asked for. A default replaces a
// value that is undefined.

// leaf is one leaf of a pattern: its target, its default, and for an
// assignment's property the registers of its object and key.
type leaf struct {
	target ast.Expr
	def    ast.Expr
	member *ast.Member
	obj    int
	key    int
	// spec is a super property's or a private member's reference.
	spec *sref
}

// pattern unpacks the value in register v into target: a declaration's
// bindings where declaring, an assignment's references otherwise.
func (g *gen) pattern(target ast.Expr, v int, declaring bool) {
	if g.err != "" {
		return
	}
	switch t := target.(type) {
	case *ast.Ident:
		g.storeLeaf(t, v, declaring)
	case *ast.AssignPattern:
		g.pattern(t.Target, g.withDefault(v, t.Default), declaring)
	case *ast.ArrayPattern:
		g.arrayPattern(t, v, declaring)
	case *ast.ObjectPattern:
		g.objectPattern(t, v, declaring)
	case *ast.Member:
		if declaring {
			// The parser allows a property only as an assignment's target.
			panic("regcomp: a declaration's property")
		}
		l := g.prepareLeaf(t, false)
		g.storeRef(l, v, false)
	case *ast.Call:
		// A call, which sloppy code may name as a for-in or for-of
		// head's target: the call is made, and then a ReferenceError.
		g.callTarget(t, int(t.Start))
	default:
		panic(fmt.Sprintf("regcomp: a pattern's target %T", target))
	}
}

// withDefault is v, or def's value where v is undefined, in a register of
// its own where v is a binding's.
func (g *gen) withDefault(v int, def ast.Expr) int {
	d := v
	if g.isBinding(v) {
		d = g.alloc()
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(d), r16(v), 0))
	}
	u := g.alloc()
	g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(u), 0, 0))
	skip := g.newMark()
	g.jump(bytecode.MakeR(bytecode.RJSNe, 0, r16(d), r16(u), 0), skip, true)
	g.into(def, d)
	g.bind(skip)
	return d
}

// prepareLeaf evaluates what of el, an element or a property's value, comes
// before the value it receives: an assignment's property's object and key.
func (g *gen) prepareLeaf(el ast.Expr, declaring bool) leaf {
	l := leaf{target: el, obj: -1, key: -1}
	if ap, ok := el.(*ast.AssignPattern); ok {
		l.target, l.def = ap.Target, ap.Default
	}
	if id, ok := l.target.(*ast.Ident); ok && g.withs[id] != nil && g.withs[id].probe.Op == bytecode.OpWithResolve {
		// The name is resolved before the value it receives is read,
		// as the stack form's prepareRef resolves it.
		g.withResolve(id)
		return l
	}
	m, ok := l.target.(*ast.Member)
	if !ok || declaring {
		return l
	}
	if special(m) {
		s := g.specialRef(m, false)
		l.spec = &s
		return l
	}
	l.member = m
	l.obj = g.alloc()
	g.into(m.Object, l.obj)
	if m.Computed {
		l.key = g.alloc()
		g.into(m.Property, l.key)
	} else {
		memberName(m)
	}
	return l
}

// storeRef stores v, its default applied, into the leaf l.
func (g *gen) storeRef(l leaf, v int, declaring bool) {
	if l.def != nil {
		v = g.withDefault(v, l.def)
	}
	if l.spec != nil {
		g.specialSet(*l.spec, v, int(l.spec.m.Start))
		return
	}
	if m := l.member; m != nil {
		g.at(int(m.Start))
		if m.Computed {
			g.emit(bytecode.MakeR(bytecode.RSetIndex, 0, r16(l.obj), r16(l.key), r16(v)))
			return
		}
		name := memberName(m)
		g.emit(bytecode.MakeR(bytecode.RSetProp, 0, r16(l.obj), r16(v), g.propSite(bytecode.OpSetProp, g.nameIdx(name))))
		return
	}
	g.pattern(l.target, v, declaring)
}

// storeLeaf stores v into the binding id names, as the record resolved the
// stack form's store of it.
func (g *gen) storeLeaf(id *ast.Ident, v int, declaring bool) {
	res, ok := g.rec.Write[id]
	if !ok {
		g.fail("a store the record does not resolve")
		return
	}
	switch res.Kind {
	case ResLocal, ResLocalInit:
		if k := g.farOf(res.Index); k >= 0 {
			g.movToFar(k, v)
		} else if r := int(g.slotReg[res.Index]); r != v {
			g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(r), r16(v), 0))
		}
	case ResGlobalVar:
		g.at(int(id.Start))
		g.far(g.emit(bytecode.MakeR(bytecode.RSetGlobal, 0, r16(v), g.name16(id.Name), g.globalSite("RSetGlobal", id.Name))), id.Name)
	case ResGlobalLexInit, ResModuleLexInit:
		g.lexInit(res.Kind, id.Name, v, int(id.Start))
	case ResIgnore:
	case ResUpval, ResUpvalCheck, ResLocalCheck, ResGlobal, ResConstAssign, ResWith:
		// ResLocalCheck and ResUpvalCheck: a let's own initializer
		// assigning it, in its dead zone (let y = [y] = []).
		g.storeValue(id, v)
	default:
		g.fail("a store the backend does not compile")
	}
}

// arrayPattern unpacks v through the iterator protocol.
func (g *gen) arrayPattern(pat *ast.ArrayPattern, v int, declaring bool) {
	cur := g.alloc()
	g.at(pat.Start)
	g.emit(bytecode.MakeR(bytecode.RForOfStart, 0, r16(cur), r16(v), 0))
	p := &prot{to: g.newMark(), reg: g.alloc(), cursor: cur}
	release := g.hold()
	g.protect(p)
	step := func(sub bytecode.ROp, el ast.Expr) {
		mark := g.top
		var l leaf
		if el != nil {
			l = g.prepareLeaf(el, declaring)
		}
		val := g.alloc()
		g.at(pat.Start)
		g.emit(bytecode.MakeR(sub, 0, r16(val), r16(cur), 0))
		if el != nil {
			g.storeRef(l, val, declaring)
		}
		g.top = mark
	}
	for _, el := range pat.Elements {
		if g.err != "" {
			break
		}
		step(bytecode.RIterStep, el)
	}
	if pat.Rest != nil {
		step(bytecode.RIterRest, pat.Rest)
	}
	g.unprotect(p)
	release()
	// The pattern has what it needs, so an iterator it stopped short of is
	// told so; unlike a close for a throw, its failure is the result.
	g.at(pat.Start)
	g.emit(bytecode.MakeR(bytecode.RIterClose, 0, 0, r16(cur), 0))
	if len(p.ranges) == 0 {
		return
	}
	end := g.newMark()
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
	g.bind(p.to)
	g.emit(bytecode.MakeR(bytecode.RIterCloseThrow, 0, 0, r16(cur), 0))
	g.emit(bytecode.MakeR(bytecode.RThrow, 0, r16(p.reg), 0, 0))
	g.bind(end)
}

// objectPattern unpacks v's properties: v checked to be something
// properties can be read from first, each property's key evaluated, and
// converted, before its leaf's reference, and a rest object made of the
// properties the pattern did not name.
func (g *gen) objectPattern(pat *ast.ObjectPattern, v int, declaring bool) {
	g.at(pat.Start)
	g.emit(bytecode.MakeR(bytecode.RCheckCoercible, 0, r16(v), 0, 0))
	keys := -1
	if pat.Rest != nil {
		// The keys the rest leaves out, each as the property read it.
		keys = g.allocN(len(pat.Props))
	}
	release := g.hold()
	defer release()
	for i, p := range pat.Props {
		if g.err != "" {
			return
		}
		mark := g.top
		key := -1
		name := ""
		switch k := p.Key.(type) {
		case *ast.Ident:
			if !p.Computed {
				name = k.Name
			}
		case *ast.StringLit:
			if !p.Computed {
				name = k.Value
			}
		}
		if name == "" {
			// A computed key, or a number's, converted before the leaf's
			// reference is evaluated.
			key = g.alloc()
			if keys >= 0 {
				key = keys + i
			}
			g.into(p.Key, key)
			g.at(p.Key.Pos())
			g.emit(bytecode.MakeR(bytecode.RToPropertyKey, 0, r16(key), 0, 0))
		} else if keys >= 0 {
			g.emit(bytecode.MakeRBC(bytecode.RLoadConst, 0, r16(keys+i), g.constIdx(bytecode.Constant{Kind: bytecode.ConstString, Str: name})))
		}
		l := g.prepareLeaf(p.Value, declaring)
		val := g.alloc()
		g.at(pat.Start)
		if key >= 0 {
			g.emit(bytecode.MakeR(bytecode.RGetIndex, 0, r16(val), r16(v), r16(key)))
		} else {
			g.emit(bytecode.MakeR(bytecode.RGetProp, 0, r16(val), r16(v), g.propSite(bytecode.OpGetProp, g.nameIdx(name))))
		}
		g.storeRef(l, val, declaring)
		g.top = mark
	}
	if pat.Rest == nil {
		return
	}
	mark := g.top
	l := g.prepareLeaf(pat.Rest, declaring)
	rest := g.alloc()
	g.at(pat.Start)
	at := g.emit(bytecode.MakeR(bytecode.RObjectRest, 0, r16(rest), r16(v), r16(keys)))
	g.aux(at, uint32(len(pat.Props)))
	g.storeRef(l, rest, declaring)
	g.top = mark
}

// patternNames lists the bindings a pattern declares.
func patternNames(target ast.Expr, out []*ast.Ident) []*ast.Ident {
	switch t := target.(type) {
	case *ast.Ident:
		out = append(out, t)
	case *ast.AssignPattern:
		out = patternNames(t.Target, out)
	case *ast.RestElement:
		out = patternNames(t.Arg, out)
	case *ast.ArrayPattern:
		for _, el := range t.Elements {
			if el != nil {
				out = patternNames(el, out)
			}
		}
		if t.Rest != nil {
			out = patternNames(t.Rest, out)
		}
	case *ast.ObjectPattern:
		for _, p := range t.Props {
			out = patternNames(p.Value, out)
		}
		if t.Rest != nil {
			out = patternNames(t.Rest, out)
		}
	}
	return out
}

// paramsAside moves the argument of each parameter a read checks or a
// closure may see to a register of its own, where it waits while the
// parameter is in its dead zone (the stack form's params_to_dead_zone);
// -1 for the others, whose arguments stay in place.
func (g *gen) paramsAside() []int {
	args := make([]int, len(g.lit.Params))
	for i := range args {
		args[i] = -1
		if i < len(g.tdz) && (g.tdz[i] || g.fn.Locals[i].Captured) {
			args[i] = g.alloc()
			g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(args[i]), r16(regParams+i), 0))
			if !g.tdz[i] {
				// Only a closure sees it: in its dead zone all the same.
				g.emit(bytecode.MakeR(bytecode.RLoadUninit, 0, r16(regParams+i), 0, 0))
			}
		}
	}
	return args
}

// params binds a list of parameters with a default, a pattern or a rest,
// one at a time, in order, as the stack form's bindParameters: each from
// its argument, args[i] where paramsAside took it aside, a default where it
// is undefined, the rest an array of the arguments from its place, and a
// pattern unpacked.
func (g *gen) params(args []int) {
	for i, p := range g.lit.Params {
		if g.err != "" {
			return
		}
		reg := regParams + i
		arg := reg
		if args[i] >= 0 {
			arg = args[i]
		}
		mark := g.top
		switch t := p.(type) {
		case *ast.Ident:
			if arg != reg {
				g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(reg), r16(arg), 0))
			}
		case *ast.AssignPattern:
			// The default runs with the parameter still in its dead zone:
			// f(a = a) reads it there.
			u := g.alloc()
			g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(u), 0, 0))
			passed, done := g.newMark(), g.newMark()
			g.jump(bytecode.MakeR(bytecode.RJSNe, 0, r16(arg), r16(u), 0), passed, true)
			g.into(t.Default, reg)
			if arg != reg {
				g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), done, false)
				g.bind(passed)
				g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(reg), r16(arg), 0))
			} else {
				g.bind(passed)
			}
			g.bind(done)
			if _, ok := t.Target.(*ast.Ident); !ok {
				g.pattern(t.Target, reg, true)
			}
		case *ast.RestElement:
			g.at(t.Start)
			g.emit(bytecode.MakeR(bytecode.RRestParam, 0, r16(reg), uint16(i), 0))
			if _, ok := t.Arg.(*ast.Ident); !ok {
				g.pattern(t.Arg, reg, true)
			}
		default:
			if arg != reg {
				g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(reg), r16(arg), 0))
			}
			g.pattern(p, reg, true)
		}
		g.top = mark
	}
}

// patternSource is the register of e's value, which a pattern unpacks: one
// of its own where it is a binding's, which the pattern's stores may
// assign before it is done reading it.
func (g *gen) patternSource(e ast.Expr) int {
	v := g.expr(e, -1)
	if g.isBinding(v) {
		d := g.alloc()
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(d), r16(v), 0))
		return d
	}
	return v
}

// patternBindings puts the lexical bindings target declares in their dead
// zone where a read checks it, before what runs ahead of their
// initialization; in a loop, where each turn's binding is a new one, the
// ones a closure keeps are closed first, which leaves the closures of the
// turns before with theirs (close_regs).
func (g *gen) patternBindings(target ast.Expr) {
	for _, id := range patternNames(target, nil) {
		res, ok := g.rec.Write[id]
		if !ok || res.Kind != ResLocalInit {
			continue
		}
		slot := int(res.Index)
		if g.fn.Locals[slot].Captured && g.inLoop() {
			g.cold(bytecode.RCloseRegs, int(g.slotReg[slot]), 1, 0)
		}
		if g.tdz[slot] || g.fn.Locals[slot].Captured {
			g.emit(bytecode.MakeR(bytecode.RLoadUninit, 0, r16(int(g.slotReg[slot])), 0, 0))
		}
	}
}
