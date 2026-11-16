package regcomp

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Literals and the expressions of stage 2's third item
// (docs/register-interpreter.md, "Stage 2: zero refusals").

// bigInt loads a BigInt literal, a constant of the function's.
func (g *gen) bigInt(n *ast.BigIntLit, dst int) int {
	d := g.dest(dst)
	g.emit(bytecode.MakeRBC(bytecode.RLoadConst, 0, r16(d), g.constIdx(bytecode.Constant{Kind: bytecode.ConstBigInt, Str: n.Raw})))
	return d
}

// template compiles a template literal as the stack form does: its parts in
// a window, concatenated by one concat, which converts each with ToString.
// A substitution is converted as soon as it is made (to_string) where what
// follows it may run code, which would see the conversion's order.
func (g *gen) template(n *ast.TemplateLit, dst int) int {
	type part struct {
		text string
		expr ast.Expr
	}
	var parts []part
	for i, q := range n.Quasis {
		if !q.Valid {
			// The parser accepts a malformed escape only in a tagged template.
			panic("regcomp: a template's part without its cooked text")
		}
		if q.Cooked != "" || i == 0 && len(n.Quasis) == 1 {
			parts = append(parts, part{text: q.Cooked})
		}
		if i < len(n.Exprs) {
			parts = append(parts, part{expr: n.Exprs[i]})
		}
	}
	if len(parts) == 0 {
		d := g.dest(dst)
		g.emit(bytecode.MakeR(bytecode.RLoadEmpty, 0, r16(d), 0, 0))
		return d
	}
	if len(parts) == 1 && parts[0].expr == nil {
		return g.expr(&ast.StringLit{Value: parts[0].text}, dst)
	}
	if len(parts) == 1 {
		// `${x}`: x converted, in a register of the template's own.
		v := g.expr(parts[0].expr, -1)
		d := g.dest(dst)
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(d), r16(v), 0))
		g.at(parts[0].expr.Pos())
		g.emit(bytecode.MakeR(bytecode.RToString, 0, r16(d), 0, 0))
		return d
	}
	// What runs code after each part: a conversion is observable only
	// before something that runs code.
	later := make([]bool, len(parts))
	for i := len(parts) - 2; i >= 0; i-- {
		e := parts[i+1].expr
		later[i] = later[i+1] || e != nil && !g.runsNoCode(e)
	}
	w := g.allocN(len(parts))
	for i, p := range parts {
		if p.expr == nil {
			g.expr(&ast.StringLit{Value: p.text}, w+i)
			continue
		}
		g.into(p.expr, w+i)
		if later[i] {
			g.at(p.expr.Pos())
			g.emit(bytecode.MakeR(bytecode.RToString, 0, r16(w+i), 0, 0))
		}
	}
	d := g.dest(dst)
	g.at(n.Start)
	g.emit(bytecode.MakeR(bytecode.RConcat, 0, r16(d), r16(w), uint16(len(parts))))
	return d
}

// taggedTemplate compiles tag`...`: a call of the tag, a method's with its
// receiver, given the site's template object and the substitutions.
func (g *gen) taggedTemplate(n *ast.TaggedTemplate, dst int) int {
	idx, ok := g.rec.Templates[n.Quasi]
	if !ok {
		panic("regcomp: a tagged template the record does not name")
	}
	argc := 1 + len(n.Quasi.Exprs)
	wide := argc > 255
	if wide {
		// The template object and the substitutions in an array, as a
		// spread call's.
		argc = 1
	}
	w := g.allocN(2 + argc)
	op := bytecode.RCallW
	if m, ok := n.Tag.(*ast.Member); ok && !m.Optional && special(m) {
		g.specialCallee(m, w)
		op = bytecode.RCallMW
	} else if ok && !m.Optional {
		g.into(m.Object, w+1)
		if m.Computed {
			k := g.expr(m.Property, -1)
			g.at(m.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetIndex, 0, r16(w), r16(w+1), r16(k)))
		} else {
			name := memberName(m)
			g.at(m.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetMethod, 0, r16(w), r16(w+1), g.propSite(bytecode.OpGetPropThis, g.nameIdx(name))))
		}
		op = bytecode.RCallMW
	} else {
		g.into(n.Tag, w)
	}
	g.emit(bytecode.MakeRBC(bytecode.RTemplateObject, 0, r16(w+2), idx))
	d := dst
	if d < 0 {
		d = w
	}
	if wide {
		g.arrayFrom(w+2, n.Quasi.Exprs, w+2)
		g.at(n.Start)
		g.cold(bytecode.RCallSpread, w, d, 0)
		return d
	}
	g.args(n.Quasi.Exprs, w+3)
	g.at(n.Start)
	if g.rec.Tails[n] {
		op = tailOp(op)
	}
	g.emit(bytecode.MakeR(op, uint8(argc), r16(w), r16(d), g.callSite(op)))
	return d
}

// property defines object literal d's property p, as the stack form's
// compileObjectLit does: a spread's properties copied, an accessor's half
// defined, __proto__: v setting the prototype, a computed key converted
// before its value is made, a function named after its key, and a method
// given the literal as its home object.
func (g *gen) property(d int, p ast.Property) {
	cold := func(sub bytecode.ROp, a, b, c int) {
		g.emit(bytecode.MakeR(sub, 0, r16(a), r16(b), r16(c)))
	}
	at := p.Start
	if at == 0 {
		at = p.Value.Pos()
	}
	if p.Kind == ast.PropSpread {
		v := g.expr(p.Value, -1)
		g.at(at)
		cold(bytecode.RCopyDataProps, d, v, 0)
		return
	}
	var name string
	numKey := false
	if !p.Computed {
		switch k := p.Key.(type) {
		case *ast.Ident:
			name = k.Name
		case *ast.StringLit:
			name = k.Value
		case *ast.NumberLit:
			numKey = true
		default:
			g.fail("an object literal's key the backend does not compile")
			return
		}
	}
	if !p.Computed && !numKey && !p.Shorthand && !p.Method && p.Kind == ast.PropInit && name == "__proto__" {
		v := g.expr(p.Value, -1)
		g.at(at)
		cold(bytecode.RSetProtoOf, d, v, 0)
		return
	}
	if p.Computed || numKey {
		// The key converted first, then the value made; a function is
		// named after the key, which is only known now.
		k := g.alloc()
		g.into(p.Key, k)
		g.at(p.Key.Pos())
		cold(bytecode.RToPropertyKey, k, 0, 0)
		v := g.expr(p.Value, -1)
		if g.isBinding(v) {
			v = g.place(v, g.alloc())
		}
		accessor := p.Kind == ast.PropGet || p.Kind == ast.PropSet
		g.at(at)
		switch {
		case accessor:
			kind := 1
			if p.Kind == ast.PropSet {
				kind = 2
			}
			cold(bytecode.RSetFuncName, v, k, kind)
			cold(bytecode.RSetHomeObject, v, d, 0)
			sub := bytecode.RDefineGetterIndex
			if p.Kind == ast.PropSet {
				sub = bytecode.RDefineSetterIndex
			}
			cold(sub, d, k, v)
			return
		case p.Method || anonymousFn(p.Value):
			if !numKey || p.Method {
				cold(bytecode.RSetFuncName, v, k, 0)
			}
		}
		if p.Method {
			cold(bytecode.RSetHomeObject, v, d, 0)
		}
		cold(bytecode.RDefineIndex, d, k, v)
		return
	}
	v := g.expr(p.Value, -1)
	g.at(at)
	switch p.Kind {
	case ast.PropGet, ast.PropSet:
		cold(bytecode.RSetHomeObject, v, d, 0)
		sub := bytecode.RDefineGetter
		if p.Kind == ast.PropSet {
			sub = bytecode.RDefineSetter
		}
		g.far(g.emit(bytecode.MakeR(sub, 0, r16(d), r16(v), g.name16(name))), name)
		return
	}
	if p.Method {
		cold(bytecode.RSetHomeObject, v, d, 0)
	}
	g.far(g.emit(bytecode.MakeR(bytecode.RDefineField, 0, r16(d), r16(v), g.name16(name))), name)
}

// anonymousFn reports whether e is an anonymous function definition, which
// takes the name of what it is assigned to.
func anonymousFn(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.FuncLit:
		return n.Name == nil
	case *ast.ClassLit:
		return n.Name == nil
	}
	return false
}

// hasSpread reports whether a list of elements or arguments has a spread.
func hasSpread(list []ast.Expr) bool {
	for _, e := range list {
		if _, ok := e.(*ast.Spread); ok {
			return true
		}
	}
	return false
}

// arrayChunk is how many elements an array made at once takes at most,
// each in a register: a literal of more, or arguments past 255, are made
// as an array of the first arrayChunk and the rest pushed onto it.
const arrayChunk = 1024

// wideArgs reports whether a call's arguments are passed as an array: one
// is a spread, or they are more than an instruction's 8 bits count.
func wideArgs(args []ast.Expr) bool {
	return len(args) > 255 || hasSpread(args)
}

// spreadArray makes into dst an array of els, which has a spread or more
// elements than arrayChunk: the elements before the first spread, up to
// arrayChunk of them, made at once, then each pushed or spread onto it in
// order, a hole pushed as the marker.
func (g *gen) spreadArray(els []ast.Expr, dst int) int {
	return g.arrayFrom(-1, els, dst)
}

// arrayFrom is spreadArray of the value in register pre, where pre is not
// negative, and then els.
func (g *gen) arrayFrom(pre int, els []ast.Expr, dst int) int {
	lead := 0
	if pre >= 0 {
		lead = 1
	}
	first := 0
	for first < len(els) && lead+first < arrayChunk {
		if _, ok := els[first].(*ast.Spread); ok {
			break
		}
		first++
	}
	base := g.allocN(lead + first)
	if pre >= 0 {
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(base), r16(pre), 0))
	}
	for i, e := range els[:first] {
		if e == nil {
			g.emit(bytecode.MakeR(bytecode.RLoadUninit, 0, r16(base+lead+i), 0, 0))
			continue
		}
		g.into(e, base+lead+i)
	}
	d := dst
	if d < 0 || g.isBinding(d) {
		// The array is made in a register of its own: an element may read
		// the binding it goes to.
		d = g.alloc()
	}
	g.emit(bytecode.MakeR(bytecode.RNewArray, 0, r16(d), r16(base), uint16(lead+first)))
	for _, e := range els[first:] {
		mark := g.top
		switch x := e.(type) {
		case nil:
			t := g.alloc()
			g.emit(bytecode.MakeR(bytecode.RLoadUninit, 0, r16(t), 0, 0))
			g.emit(bytecode.MakeR(bytecode.RArrayPush, 0, r16(d), r16(t), 0))
		case *ast.Spread:
			v := g.expr(x.Arg, -1)
			g.emit(bytecode.MakeR(bytecode.RArraySpread, 0, r16(d), r16(v), 0))
		default:
			v := g.expr(x, -1)
			g.emit(bytecode.MakeR(bytecode.RArrayPush, 0, r16(d), r16(v), 0))
		}
		g.top = mark
	}
	return g.place(d, dst)
}

// spreadCall compiles a call with a spread among its arguments, as the
// stack form's call_spread: the function, its this -- a method's receiver,
// or undefined -- and an array of the arguments in a window.
func (g *gen) spreadCall(n *ast.Call, dst int) int {
	w := g.allocN(3)
	if m, ok := n.Callee.(*ast.Member); ok && special(m) {
		mm := *m
		if m.Optional {
			mm.Object, mm.Optional = g.optionalLink(m.Object), false
		}
		g.specialCallee(&mm, w)
	} else if ok {
		obj := m.Object
		if m.Optional {
			obj = g.optionalLink(obj)
		}
		g.into(obj, w+1)
		if m.Computed {
			k := g.expr(m.Property, -1)
			g.at(m.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetIndex, 0, r16(w), r16(w+1), r16(k)))
		} else {
			name := memberName(m)
			// At the stack form's position: its get_prop has none of its own.
			g.emit(bytecode.MakeR(bytecode.RGetProp, 0, r16(w), r16(w+1), g.propSite(bytecode.OpGetProp, g.nameIdx(name))))
		}
	} else if id, ok := n.Callee.(*ast.Ident); ok && g.withs[id] != nil {
		// Through a with object, which is the receiver where it has the
		// name (with_get_this), as a call's without a spread.
		g.probeOp(id, bytecode.OpWithGetThis)
		g.probed(id, bytecode.RWithGetThis, w, func() {
			g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(w+1), 0, 0))
			g.into(n.Callee, w)
		})
	} else {
		g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(w+1), 0, 0))
		g.into(n.Callee, w)
	}
	if n.Optional {
		if g.chain == nil {
			panic("regcomp: an optional call outside its chain")
		}
		g.nullishJump(w, g.chain.nullish)
	}
	g.spreadArray(n.Args, w+2)
	d := g.dest(dst)
	g.at(n.TracePos())
	g.emit(bytecode.MakeR(bytecode.RCallSpread, 0, r16(w), r16(d), 0))
	return d
}

// regExpr is a value already in a register, as an operand of an expression
// compiled around it: an optional link's object, checked before the link.
type regExpr struct {
	*ast.Ident
	reg int
}

// chainCtx is an optional chain being compiled: where a link whose object is
// undefined or null goes.
type chainCtx struct {
	nullish *mark
}

// nullishJump jumps to target where register r is undefined or null -- not
// document.all's stand-in, which == null but is no nullish value.
func (g *gen) nullishJump(r int, target *mark) {
	cont := g.newMark()
	g.jump(bytecode.MakeR(bytecode.RJNotNull, 0, r16(r), 0, 0), cont, true)
	u := g.alloc()
	g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(u), 0, 0))
	g.jump(bytecode.MakeR(bytecode.RJSEq, 0, r16(r), r16(u), 0), target, true)
	g.emit(bytecode.MakeR(bytecode.RLoadNull, 0, r16(u), 0, 0))
	g.jump(bytecode.MakeR(bytecode.RJSEq, 0, r16(r), r16(u), 0), target, true)
	g.bind(cont)
}

// optionalLink evaluates the object of an optional link, n's, and jumps to
// the chain's end where it is nullish: the object, as a register operand.
func (g *gen) optionalLink(object ast.Expr) ast.Expr {
	if g.chain == nil {
		panic("regcomp: an optional link outside its chain")
	}
	o := g.expr(object, -1)
	if g.isBinding(o) {
		// What the link reads is the value checked.
		o = g.place(o, g.alloc())
	}
	g.nullishJump(o, g.chain.nullish)
	return &regExpr{Ident: &ast.Ident{Start: int32(object.Pos())}, reg: o}
}

// optionalChain compiles an optional chain: its links, each nullish check
// going to the end, where the chain is undefined.
func (g *gen) optionalChain(n *ast.OptionalChain, dst int) int {
	saved := g.chain
	c := &chainCtx{nullish: g.newMark()}
	g.chain = c
	r := g.expr(n.Base, -1)
	g.chain = saved
	d := dst
	if d < 0 || g.isBinding(d) && d != r {
		d = g.alloc()
	}
	if r != d {
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(d), r16(r), 0))
	}
	end := g.newMark()
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
	g.bind(c.nullish)
	g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(d), 0, 0))
	g.bind(end)
	return g.place(d, dst)
}

// nullish compiles a ?? b: b where a is undefined or null.
func (g *gen) nullish(n *ast.Logical, dst int) int {
	d := dst
	if d < 0 || g.isBinding(d) {
		d = g.alloc()
	}
	g.into(n.Left, d)
	rhs, end := g.newMark(), g.newMark()
	g.nullishJump(d, rhs)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
	g.bind(rhs)
	g.into(n.Right, d)
	g.bind(end)
	return g.place(d, dst)
}

// isChainCall reports whether n's function comes from an optional link:
// o?.m(), or (o?.m)(), whose this is the object read from.
func isChainCall(n *ast.Call) bool {
	switch c := n.Callee.(type) {
	case *ast.Member:
		return c.Optional
	case *ast.OptionalChain:
		return true
	}
	return false
}

// chainCall compiles a call in or of an optional chain: f?.(), o?.m(),
// o.m?.() and (o?.m)(). A method's receiver is its object; a nullish link
// before the call skips it, to the chain's end, and a parenthesized chain
// that was nullish calls undefined, which throws, as it does anywhere.
func (g *gen) chainCall(n *ast.Call, dst int) int {
	argc := len(n.Args)
	wide := wideArgs(n.Args)
	if wide {
		// The arguments in an array, as a spread call's.
		argc = 1
	}
	w := g.allocN(2 + argc)
	op := bytecode.RCallW
	callee := n.Callee
	var outer *chainCtx
	saved := g.chain
	if c, ok := callee.(*ast.OptionalChain); ok {
		// (chain)(): the chain's own links end at the call, with the
		// function and its this undefined.
		outer = &chainCtx{nullish: g.newMark()}
		g.chain = outer
		defer func() { g.chain = saved }()
		callee = c.Base
	}
	if m, ok := callee.(*ast.Member); ok && special(m) {
		mm := *m
		if m.Optional {
			mm.Object, mm.Optional = g.optionalLink(m.Object), false
		}
		g.specialCallee(&mm, w)
		op = bytecode.RCallMW
	} else if ok {
		obj := m.Object
		if m.Optional {
			obj = g.optionalLink(obj)
		}
		g.into(obj, w+1)
		if m.Computed {
			k := g.expr(m.Property, -1)
			g.at(m.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetIndex, 0, r16(w), r16(w+1), r16(k)))
		} else {
			name := memberName(m)
			g.at(m.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetMethod, 0, r16(w), r16(w+1), g.propSite(bytecode.OpGetPropThis, g.nameIdx(name))))
		}
		op = bytecode.RCallMW
	} else {
		g.into(callee, w)
	}
	if outer != nil {
		// The call's own ?. belongs to the chain around it.
		g.chain = saved
		skip := g.newMark()
		g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), skip, false)
		g.bind(outer.nullish)
		g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(w), 0, 0))
		g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(w+1), 0, 0))
		g.bind(skip)
	}
	if n.Optional {
		if g.chain == nil {
			panic("regcomp: an optional call outside its chain")
		}
		g.nullishJump(w, g.chain.nullish)
	}
	d := dst
	if d < 0 {
		d = w
	}
	if wide {
		g.spreadArray(n.Args, w+2)
		g.at(n.TracePos())
		g.cold(bytecode.RCallSpread, w, d, 0)
		return d
	}
	g.args(n.Args, w+2)
	g.at(n.TracePos())
	if g.rec.Tails[n] {
		op = tailOp(op)
	}
	g.emit(bytecode.MakeR(op, uint8(argc), r16(w), r16(d), g.callSite(op)))
	return d
}

// logicalAssign compiles a ||= b, a &&= b and a ??= b: the target's
// reference evaluated, read, and b made and stored only where the read
// does not decide: the expression's value is what was read, or b's.
func (g *gen) logicalAssign(n *ast.Assign, dst int) int {
	d := g.alloc()
	end := g.newMark()
	short := func() {
		switch n.Op {
		case "||=":
			g.jump(bytecode.MakeR(bytecode.RJmpT, 0, r16(d), 0, 0), end, false)
		case "&&=":
			g.jump(bytecode.MakeR(bytecode.RJmpF, 0, r16(d), 0, 0), end, false)
		default:
			assign := g.newMark()
			g.nullishJump(d, assign)
			g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
			g.bind(assign)
		}
	}
	switch t := n.Target.(type) {
	case *ast.Ident:
		g.place(g.readForWrite(t, d), d)
		short()
		v := g.expr(n.Value, -1)
		g.storeValue(t, v)
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(d), r16(v), 0))
	case *ast.Member:
		if special(t) {
			s := g.specialRef(t, true)
			g.specialGet(s, d)
			short()
			v := g.expr(n.Value, -1)
			g.specialSet(s, v, int(n.Start))
			g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(d), r16(v), 0))
			break
		}
		o := g.expr(t.Object, -1)
		if g.isBinding(o) {
			o = g.place(o, g.alloc())
		}
		if t.Computed {
			// The key converted once, for the read and the write.
			k := g.alloc()
			g.into(t.Property, k)
			g.at(t.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RKeyOfBase, 0, r16(k), r16(o), r16(k)))
			g.emit(bytecode.MakeR(bytecode.RGetIndex, 0, r16(d), r16(o), r16(k)))
			short()
			v := g.expr(n.Value, -1)
			g.at(int(n.Start))
			g.emit(bytecode.MakeR(bytecode.RSetIndex, 0, r16(o), r16(k), r16(v)))
			g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(d), r16(v), 0))
			break
		}
		name := memberName(t)
		g.at(t.Property.Pos())
		g.emit(bytecode.MakeR(bytecode.RGetProp, 0, r16(d), r16(o), g.propSite(bytecode.OpGetProp, g.nameIdx(name))))
		short()
		v := g.expr(n.Value, -1)
		g.at(int(n.Start))
		g.emit(bytecode.MakeR(bytecode.RSetProp, 0, r16(o), r16(v), g.propSite(bytecode.OpSetProp, g.nameIdx(name))))
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(d), r16(v), 0))
	default:
		// A call is assign's; the parser allows nothing else.
		panic(fmt.Sprintf("regcomp: a logical assignment's target %T", n.Target))
	}
	g.bind(end)
	return g.place(d, dst)
}
