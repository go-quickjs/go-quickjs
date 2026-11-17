package compiler

import (
	"fmt"
	"math/big"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler/regcomp"
	"github.com/go-quickjs/go-quickjs/internal/fold"
)

// compileExprForEffect compiles an expression whose value is discarded: a
// statement, or an operand of a comma expression that is not the last. A
// postfix update is compiled as the prefix one, whose value is not kept.
func (c *compiler) compileExprForEffect(e ast.Expr) {
	// What is evaluated for its effect alone hands no name on.
	c.traceName, c.traceIn = "", ""
	switch n := e.(type) {
	case *ast.StringLit, *ast.NumberLit, *ast.BigIntLit, *ast.BoolLit, *ast.NullLit:
		// A literal has no effect -- "use strict"; is one -- so nothing is
		// pushed to be dropped.
		return
	case *ast.Update:
		if c.compileLocalUpdate(n) {
			return
		}
		if !n.Prefix {
			// The two forms differ only in which value they leave behind, and
			// neither is wanted here. The prefix form does not make the copy.
			prefix := *n
			prefix.Prefix = true
			e, n = &prefix, &prefix
		}
		if m, ok := n.Operand.(*ast.Member); ok {
			if _, private := m.Property.(*ast.PrivateName); !private {
				if _, super := m.Object.(*ast.Super); !super {
					// A property's update stores and leaves nothing behind.
					c.compileUpdateKeep(n, false)
					return
				}
			}
		}

	case *ast.Sequence:
		// The last of them too, where nothing wants the whole's value:
		// a += x, b += y makes each an append.
		for _, x := range n.Exprs {
			c.compileExprForEffect(x)
		}
		return

	case *ast.Logical:
		// A short circuit around an append whose value nothing uses: the
		// append stores, and leaves nothing for a drop.
		if n.Op != "??" && c.ownedAppend(n.Right) {
			c.compileExpr(n.Left)
			c.compileOwnedAppend(n.Right.(*ast.Assign))
			return
		}
		if n.Op != "??" && c.localAppend(n.Right) {
			c.compileExpr(n.Left)
			c.compileLocalAppend(n.Right.(*ast.Assign))
			return
		}

	case *ast.Assign:
		c.noteString(n)
		if c.ownedAppend(n) {
			c.compileOwnedAppend(n)
			return
		}
		if c.localAppend(n) {
			c.compileLocalAppend(n)
			return
		}
		// A store to a property carries its value out the same way, by copying
		// it below what the store consumes.
		if m, ok := n.Target.(*ast.Member); ok {
			switch n.Op {
			case "=":
				c.traceIn = ""
				trace := ""
				if handsName(n.Value) {
					trace = c.assignTraceName(n.Target)
				}
				c.compileMemberStore(m, func() {
					c.traceName = trace
					c.compileExprNamed(n.Value, "")
				}, false)
				return
			case "&&=", "||=", "??=":
				// A logical assignment that short-circuits leaves the value it
				// read rather than one it stored, so something still has to
				// discard it.
			default:
				c.compileMemberUpdate(m, func() {
					c.compileExpr(n.Value)
				})
				return
			}
		}
	}
	c.compileExpr(e)
}

// compileTest compiles a condition for a branch, of which only the truth
// is wanted: a constant -- a literal, or an operator that folds, !0
// (fold.Test) -- is not compiled; the operand of !x, and both of x && y and
// x || y, are compiled as tests; a comparison's as values.
func (c *compiler) compileTest(test ast.Expr) {
	c.enter(test.Pos())
	defer c.leave()
	if _, ok := fold.Test(test); ok {
		return
	}
	switch n := test.(type) {
	case *ast.BoolLit, *ast.NumberLit:
		return
	case *ast.Unary:
		if n.Op == "!" {
			c.compileTest(n.Operand)
			return
		}
	case *ast.Logical:
		if n.Op == "&&" || n.Op == "||" {
			c.compileTest(n.Left)
			c.compileTest(n.Right)
			return
		}
	case *ast.Binary:
		if _, fusable := comparisonOpcode(n.Op); fusable {
			c.compileExpr(n.Left)
			c.compileExpr(n.Right)
			return
		}
	}
	c.compileExpr(test)
}

// comparisonOpcode is the opcode of a comparison operator, and whether the
// operator is one. `in` and `instanceof` are not: they are binary operators
// that happen to produce a boolean, and each has its own instruction.
func comparisonOpcode(op string) (bytecode.Op, bool) {
	switch op {
	case "<":
		return bytecode.OpLt, true
	case "<=":
		return bytecode.OpLe, true
	case ">":
		return bytecode.OpGt, true
	case ">=":
		return bytecode.OpGe, true
	case "==":
		return bytecode.OpEq, true
	case "!=":
		return bytecode.OpNe, true
	case "===":
		return bytecode.OpStrictEq, true
	case "!==":
		return bytecode.OpStrictNe, true
	}
	return 0, false
}

// compileExpr walks an expression.
func (c *compiler) compileExpr(e ast.Expr) {
	c.compileExprNamed(e, "")
}

// compileExprNamed compiles an expression, giving an anonymous function or
// class the supplied name. `const f = () => {}` names the arrow "f", which is
// observable through Function.prototype.name.
func (c *compiler) compileExprNamed(e ast.Expr, name string) {
	c.enter(e.Pos())
	defer c.leave()
	trace := c.traceName
	c.traceName, c.traceIn = "", trace

	switch n := e.(type) {
	case *ast.NumberLit:

	case *ast.StringLit:

	case *ast.BoolLit:

	case *ast.NullLit:

	case *ast.This:

	case *ast.NewTarget:
		if c.inClassFieldInit() {
			// A field initializer is a function of its own, and nothing
			// constructs it.
			break
		}
		c.facts.reads = true

	case *ast.ImportMeta:

	case *ast.BigIntLit:

	case *ast.RegexpLit:

	case *ast.Ident:
		c.compileIdentRead(n)

	case *ast.TemplateLit:
		c.compileTemplate(n)

	case *ast.TaggedTemplate:
		c.compileTaggedTemplate(n)

	case *ast.ArrayLit:
		c.compileArrayLit(n)

	case *ast.ObjectLit:
		c.compileObjectLit(n)

	case *ast.FuncLit:
		c.traceName = trace
		c.compileFunctionLiteral(n, name)

	case *ast.ClassLit:
		c.traceName = trace
		c.compileClass(n, name)

	case *ast.Unary:
		c.compileUnary(n)

	case *ast.Update:
		c.compileUpdate(n)

	case *ast.Binary:
		// `#x in obj` asks whether an object carries a class's private field,
		// which is the only way to tell one of that class's instances from a
		// lookalike without a try/catch around a private read.
		if pn, ok := n.Left.(*ast.PrivateName); ok && n.Op == "in" {
			c.privateName(pn, n.Start)
			c.compileExpr(n.Right)
			break
		}
		if n.Op == "+" && c.compileStringChain(n) {
			break
		}
		c.compileExpr(n.Left)
		c.compileExpr(n.Right)

	case *ast.Logical:
		c.compileLogical(n)

	case *ast.Conditional:
		c.compileConditional(n)

	case *ast.Assign:
		c.compileAssign(n)

	case *ast.Sequence:
		// Every operand but the last is evaluated for its effect only.
		for i, x := range n.Exprs {
			if i < len(n.Exprs)-1 {
				c.compileExprForEffect(x)
				continue
			}
			c.traceName = trace
			c.compileExpr(x)
		}

	case *ast.Member:
		c.compileMemberRead(n)

	case *ast.Call:
		c.compileCall(n)

	case *ast.New:
		c.compileNew(n)

	case *ast.OptionalChain:
		c.compileChain(n, false)

	case *ast.Yield:
		c.compileYield(n)

	case *ast.Await:
		c.compileExpr(n.Arg)
		c.emitAwait(n.Start)

	case *ast.Super:
		c.errorf(n.Start, "\"super\" is only valid as a call or a property access")

	case *ast.Spread:
		c.errorf(n.Start, "spread is not valid here")

	default:
		c.errorf(e.Pos(), "unsupported expression %T", e)
	}
}

// foldsVoid reports whether void's operand e is a value the compile
// knows, which runs nothing and need not be evaluated: a literal, as void
// 0 always was, or an operator that folds (fold).
func foldsVoid(e ast.Expr) bool {
	switch e.(type) {
	case *ast.NumberLit, *ast.StringLit, *ast.BoolLit, *ast.NullLit, *ast.BigIntLit:
		return true
	}
	_, ok := fold.Value(e)
	return ok
}

// compileIdentRead walks a read of a variable, recording its resolution.
func (c *compiler) compileIdentRead(n *ast.Ident) {
	// Inside a `with` body the object may answer instead of the binding.
	probe := c.withProbe(bytecode.OpWithGet, n.Name)
	c.regWith(n, probe)
	c.compileIdentReadStatic(n)
}

// compileIdentReadStatic reads a name from the binding it resolves to, without
// consulting any enclosing `with` object.
func (c *compiler) compileIdentReadStatic(n *ast.Ident) {
	// undefined, NaN and Infinity are properties of the global object, not
	// keywords, so they resolve like any other name.
	if lit := c.constLiteral(n.Name); lit != nil {
		c.reg.NoteRead(n, regcomp.Res{Kind: regcomp.ResLiteral, Lit: lit})
		c.compileExpr(lit)
		return
	}
	if l, ok := c.resolveLocal(n.Name); ok {
		if l.initialized {
			c.reg.NoteRead(n, regcomp.Res{Kind: regcomp.ResLocal, Index: l.slot})
			if c.lazyArguments && l.slot == c.lazyArgumentsSlot {
				c.noteLazyArguments(n)
			}
		} else {
			c.reg.NoteRead(n, regcomp.Res{Kind: regcomp.ResLocalCheck, Index: l.slot})
			// A read in the dead zone throws at the name.
		}
		c.compiledRead(l, n)
		return
	}
	if idx, ok := c.resolveUpvalue(n.Name); ok {
		if c.fn.Upvalues[idx].TDZ {
			c.reg.NoteRead(n, regcomp.Res{Kind: regcomp.ResUpvalCheck, Index: idx})
		} else {
			c.reg.NoteRead(n, regcomp.Res{Kind: regcomp.ResUpval, Index: idx})
		}
		if l, _ := c.enclosingBinding(n.Name); l != nil {
			c.compiledRead(l, n)
		}
		return
	}
	// A named function expression's own name refers to the running closure.
	if n.Name == c.selfName {
		c.reg.NoteRead(n, regcomp.Res{Kind: regcomp.ResCallee})
		c.facts.callee = true
		return
	}
	if n.Name == "undefined" && c.withDepth == 0 && c.opts.EvalScope == nil && c.opts.EvalWithDepth == 0 &&
		!c.opts.EvalOwnVarScope && !c.opts.EvalConfigurable {
		// The global object's undefined is neither writable nor configurable,
		// and a script's lexical binding of it is an error, so where no
		// binding of the function's or its makers', no with and no direct
		// eval can stand in front of it, it is undefined: pushed, rather
		// than read from the global object at every x === undefined.
		c.reg.NoteRead(n, regcomp.Res{Kind: regcomp.ResLiteral})
		return
	}
	// A function that uses `arguments` binds it to a slot in its prologue, so
	// the searches above will have found it. Reaching here means the reference
	// is in a position with no arguments object at all, such as the top level.
	c.reg.NoteRead(n, regcomp.Res{Kind: regcomp.ResGlobal})
}

func (c *compiler) compileTemplate(n *ast.TemplateLit) {
	// The parts are pushed in order and concatenated in one instruction, so a
	// template with k substitutions costs one concatenation rather than k.
	parts := 0
	for i, q := range n.Quasis {
		// A part with a malformed escape has no cooked value. Only a tag can
		// make sense of that, by reading the raw text instead; here there is
		// nothing to build a string out of.
		if !q.Valid {
			c.errorf(n.Start, "invalid escape sequence in template literal")
			return
		}
		if q.Cooked != "" || (i == 0 && len(n.Quasis) == 1) {
			parts++
		}
		if i < len(n.Exprs) {
			c.compileExpr(n.Exprs[i])
			parts++
		}
	}
	if parts == 0 {
		return
	}
}

// maxChainParts is the most parts compileStringChain joins in one concat. A
// longer chain is joined a run at a time, each run's string the first part
// of the next.
const maxChainParts = 32

// compileStringChain compiles a + b + c + ..., once a string literal among
// its operands has made every addition from there on a concatenation, as one
// concat of the parts rather than an addition at a time: each addition but
// the last would make a string only for the next one to copy and drop.
//
// What each addition does is kept, and when. The operands before the
// literal are added as they were, and their sum converted where the literal
// would have been added to it; each operand after it is converted as soon as
// it is evaluated, before the next one is, by adding it to the empty string,
// which is ToPrimitive and then ToString -- the conversion the addition
// would have made of it. A literal, a template's string and a number need
// none: concat writes a number out itself, as ToString would.
//
// It reports false, having emitted nothing, for a chain with no string
// literal, or one that would join fewer than three parts, where a concat
// saves nothing over the additions.
func (c *compiler) compileStringChain(n *ast.Binary) bool {
	// The chain's additions, innermost first: adds[0].Left is its first
	// operand, and adds[i].Right the one after.
	var adds []*ast.Binary
	for b := n; ; {
		adds = append(adds, b)
		l, ok := b.Left.(*ast.Binary)
		if !ok || l.Op != "+" {
			break
		}
		b = l
	}
	if len(adds) < 2 {
		return false
	}
	slices.Reverse(adds)
	operand := func(i int) ast.Expr {
		if i == 0 {
			return adds[0].Left
		}
		return adds[i-1].Right
	}
	// The first string literal. A template is a string too, but only at the
	// start: after an operand, its substitutions would run before the
	// conversion of the operands it follows rather than after.
	first := -1
	for i := range len(adds) + 1 {
		if _, ok := operand(i).(*ast.StringLit); ok {
			first = i
			break
		}
		if t, ok := operand(i).(*ast.TemplateLit); ok && i == 0 && templateValid(t) {
			first = 0
			break
		}
	}
	if first < 0 {
		return false
	}
	empty := func(e ast.Expr) bool {
		s, ok := e.(*ast.StringLit)
		return ok && s.Value == ""
	}
	parts := 0
	if first > 0 {
		parts++
	}
	for i := first; i <= len(adds); i++ {
		if !empty(operand(i)) {
			parts++
		}
	}
	if parts < 3 {
		return false
	}

	parts = 0
	part := func() {
		parts++
		if parts == maxChainParts {
			parts = 1
		}
	}
	if first > 0 {
		// What comes before the literal, converted where the literal was
		// added to it.
		if first == 1 {
			c.compileExpr(operand(0))
		} else {
			c.compileExpr(adds[first-2])
		}
		part()
	}
	for i := first; i <= len(adds); i++ {
		switch e := operand(i); e.(type) {
		case *ast.StringLit:
			if empty(e) {
				continue
			}
			c.compileExpr(e)
		case *ast.TemplateLit, *ast.NumberLit:
			c.compileExpr(e)
		default:
			c.compileExpr(e)
		}
		part()
	}
	return true
}

// templateValid reports whether every part of a template has a cooked
// value, which only a tag can do without.
func templateValid(t *ast.TemplateLit) bool {
	for _, q := range t.Quasis {
		if !q.Valid {
			return false
		}
	}
	return true
}

// compileTaggedTemplate compiles tag`a${x}b`.
//
// The tag is called with the site's strings as its first argument and the
// substitutions as the rest, which is what lets a tag see the text the
// substitutions were written between -- String.raw is the plain example.
func (c *compiler) compileTaggedTemplate(n *ast.TaggedTemplate) {
	idx := c.templateIdx(n.Quasi)

	// A member tag keeps its receiver, exactly as an ordinary method call does,
	// so that String.raw`x` has String as its `this`.
	if m, ok := n.Tag.(*ast.Member); ok && !m.Optional {
		if _, isSuper := m.Object.(*ast.Super); isSuper {
			c.compileSuperMemberGet(m)
		} else {
			c.compileExpr(m.Object)
			switch {
			case m.Computed:
				c.compileExpr(m.Property)
			default:
				if pn, private := m.Property.(*ast.PrivateName); private {
					c.privateName(pn, int(m.Start))
					break
				}
			}
		}
		c.compileTemplateArguments(idx, n.Quasi)
		c.noteTail(n)
		return
	}

	c.compileExpr(n.Tag)
	c.compileTemplateArguments(idx, n.Quasi)
	c.noteTail(n)
}

// noteTail notes n, a call, as a call in tail position where it is the
// one compileTailExpr is compiling (tailCall): the function's frame is
// given up for it once the stack is deep (bytecode.Function's HasTailCall,
// regcomp.Record's Tails).
func (c *compiler) noteTail(n ast.Expr) {
	if c.tailCall != n {
		return
	}
	c.fn.HasTailCall = true
	c.reg.NoteTail(n)
}

// compileTemplateArguments walks a tagged template's substitutions.
func (c *compiler) compileTemplateArguments(idx uint32, q *ast.TemplateLit) int {
	for _, e := range q.Exprs {
		c.compileExpr(e)
	}
	return 1 + len(q.Exprs)
}

// templateIdx records one tagged template site.
//
// Each site gets its own entry even when two are spelled the same, because the
// object a site produces is reused across calls and comparing two of them is
// how a tag tells its call sites apart.
func (c *compiler) templateIdx(q *ast.TemplateLit) uint32 {
	t := bytecode.TemplateStrings{
		Cooked:      make([]string, len(q.Quasis)),
		CookedValid: make([]bool, len(q.Quasis)),
		Raw:         make([]string, len(q.Quasis)),
	}
	for i, e := range q.Quasis {
		t.Cooked[i] = e.Cooked
		t.CookedValid[i] = e.Valid
		t.Raw[i] = e.Raw
	}
	c.fn.Templates = append(c.fn.Templates, t)
	idx := uint32(len(c.fn.Templates) - 1)
	c.reg.NoteTemplate(q, idx)
	return idx
}

func (c *compiler) compileArrayLit(n *ast.ArrayLit) {
	trace := c.traceIn
	// A literal without spread is made in one operation, a hole the
	// uninitialized value, which advances the length without making a
	// property: `1 in [1,,3]` is false, and the iteration methods skip it.
	simple := true
	for _, el := range n.Elements {
		if _, isSpread := el.(*ast.Spread); isSpread {
			simple = false
			break
		}
	}
	if simple {
		for _, el := range n.Elements {
			if el == nil {
				continue
			}
			c.traceName = trace
			c.compileExpr(el)
		}
		return
	}

	for _, el := range n.Elements {
		switch el {
		case nil:
			// A hole advances the length without creating a property, which is
			// what makes `1 in [1,,3]` false and what the iteration methods
			// skip.
		default:
			if sp, ok := el.(*ast.Spread); ok {
				c.compileExpr(sp.Arg)
				continue
			}
			c.traceName = trace
			c.compileExpr(el)
		}
	}
}

func (c *compiler) compileObjectLit(n *ast.ObjectLit) {
	trace := c.traceIn
	if n.ProtoDup != 0 {
		// Two __proto__ properties in a literal are an error: each would set
		// the prototype and the second would silently win. Reaching here means
		// the text is a literal after all rather than a pattern, where two are
		// simply two places to assign to.
		c.errorf(n.ProtoDup, "an object literal may have only one __proto__ property")
	}
	// The number of properties the literal writes is known here, so the object
	// is made with room for them rather than growing three times on the way.
	// A spread contributes an unknown number, which the growth handles.
	fields := 0
	for _, p := range n.Props {
		if p.Kind != ast.PropSpread {
			fields++
		}
	}
	for _, p := range n.Props {
		switch p.Kind {
		case ast.PropSpread:
			c.compileExpr(p.Value)
			continue
		case ast.PropGet, ast.PropSet:
			// An accessor is named after its property with a "get " or "set "
			// prefix, which is what distinguishes the two halves in a stack
			// trace.
			prefix := "get "
			if p.Kind == ast.PropSet {
				prefix = "set "
			}
			if p.Computed {
				c.compileExpr(p.Key)
				c.compileExpr(p.Value)
				continue
			}
			c.compileExprNamed(p.Value, prefix+propKeyName(p.Key))
			continue
		}

		// __proto__: v in a literal sets the prototype rather than adding a
		// property, unless it is a shorthand or a method.
		if !p.Computed && !p.Shorthand && !p.Method && propKeyName(p.Key) == "__proto__" {
			c.compileExpr(p.Value)
			continue
		}

		if p.Computed {
			c.compileExpr(p.Key)
			c.compileExpr(p.Value)
			continue
		}
		key := propKeyName(p.Key)
		if trace != "" {
			if handsName(p.Value) {
				c.traceName = c.traceJoin(trace, key)
			}
		}
		c.compileExprNamed(p.Value, key)
	}
}

// propKeyName returns the string form of a non-computed property key.
func propKeyName(key ast.Expr) string {
	switch k := key.(type) {
	case *ast.Ident:
		return k.Name
	case *ast.StringLit:
		return k.Value
	case *ast.NumberLit:
		return formatKeyNumber(k.Value)
	case *ast.PrivateName:
		return "#" + k.Name
	}
	return ""
}

func (c *compiler) compileUnary(n *ast.Unary) {
	switch n.Op {
	case "!", "typeof":
		// Of a value the compile knows -- typeof 1, !void 0; the parser
		// has folded ! of a literal -- the literal of its value, as the
		// register form compiles it too (fold.Value).
		if _, ok := fold.Value(n); ok {
			return
		}
	}
	switch n.Op {
	case "typeof":
		// typeof on an undeclared identifier must yield "undefined" rather
		// than throwing, which needs the non-throwing global read.
		if id, ok := n.Operand.(*ast.Ident); ok {
			probe := c.withProbe(bytecode.OpWithTypeof, id.Name)
			c.regWith(id, probe)
			// The non-throwing global read is only right for a name that
			// resolves nowhere; a binding, or the function's own name, is
			// read the ordinary way.
			if _, isLocal := c.resolveLocal(id.Name); !isLocal && id.Name != c.selfName {
				if _, isUp := c.resolveUpvalue(id.Name); !isUp {
					return
				}
			}
			if probe >= 0 {
				// The binding past the probe, which has asked the with
				// objects already: a second probe would ask them again.
				c.compileIdentReadStatic(id)
				return
			}
		}
		c.compileExpr(n.Operand)

	case "delete":
		if ch, ok := n.Operand.(*ast.OptionalChain); ok {
			if m, ok := ch.Base.(*ast.Member); ok {
				c.compileDeleteChain(n, m)
				return
			}
		}
		if m, ok := n.Operand.(*ast.Member); ok {
			// A private name is not a property key a program may remove: there
			// is no object that could carry it afterwards, and no way to ask.
			if pn, private := m.Property.(*ast.PrivateName); private {
				c.errorf(n.Start, "a private name cannot be deleted: #%s", pn.Name)
				return
			}
			if _, isSuper := m.Object.(*ast.Super); isSuper {
				// `delete super.x` parses, and then throws: a super reference
				// names a property of the home object's prototype, and there
				// is no object it could be removed from. The reference is still
				// built first, which means `this` and then the key expression
				// -- but not the conversion of what that expression produced.
				if m.Computed {
					c.compileExpr(m.Property)
				}
				return
			}
			c.compileExpr(m.Object)
			c.compileMemberKey(m)
			return
		}
		if id, ok := n.Operand.(*ast.Ident); ok {
			probe := c.withProbe(bytecode.OpWithDelete, id.Name)
			c.regWith(id, probe)
			// A local or captured binding cannot be deleted at all; a global
			// can, but only if it is configurable, which a var declaration is
			// not. The register form is told which: the binding's
			// resolution, or a global.
			if l, isLocal := c.resolveLocal(id.Name); isLocal {
				c.reg.NoteRead(id, regcomp.Res{Kind: regcomp.ResLocal, Index: l.slot})
				return
			}
			if idx, isUpvalue := c.resolveUpvalue(id.Name); isUpvalue {
				c.reg.NoteRead(id, regcomp.Res{Kind: regcomp.ResUpval, Index: idx})
				return
			}
			c.reg.NoteRead(id, regcomp.Res{Kind: regcomp.ResGlobal})
			return
		}
		// Deleting anything else evaluates the operand and yields true.
		c.compileExpr(n.Operand)

	case "void":
		if foldsVoid(n.Operand) {
			// void 0, minifiers' undefined: a literal, or an operator
			// that folds, evaluates to nothing that could be seen, so only
			// the result is pushed, which an x === void 0 then compares as
			// it does x === undefined.
			return
		}
		c.compileExpr(n.Operand)

	case "!":
		c.compileExpr(n.Operand)

	case "-":
		// A negative numeric literal is folded, which also gets -0 right.
		if _, ok := n.Operand.(*ast.NumberLit); ok {
			return
		}
		// So is a BigInt literal, written in decimal: a constant, which a
		// sign before a hexadecimal, octal or binary one's digits could not
		// be (StringToBigInt takes none).
		if lit, ok := n.Operand.(*ast.BigIntLit); ok {
			if _, ok := new(big.Int).SetString(lit.Raw, 0); ok {
				return
			}
		}
		c.compileExpr(n.Operand)

	case "+":
		c.compileExpr(n.Operand)

	case "~":
		c.compileExpr(n.Operand)

	default:
		c.errorf(n.Start, "unsupported unary operator %q", n.Op)
	}
}

// compileLocalUpdate walks `i++` on a plain local, and reports whether it is
// one.
//
// It is what the counted loop is made of, and the four instructions it replaces
// -- read, coerce, add, store -- are four dispatches around one addition.
// Anything with more to it than that, a const, a name a `with` object may
// answer for, a binding still in its dead zone, is left to the general path.
func (c *compiler) compileLocalUpdate(n *ast.Update) bool {
	_, ok := c.updatableLocal(n)
	if !ok {
		return false
	}
	return true
}

// updatableLocal is the local an update names, when it names one that the
// local-update instructions may write: a plain one, initialized, that is not
// a constant or a named function expression's own name.
func (c *compiler) updatableLocal(n *ast.Update) (*localVar, bool) {
	id, ok := n.Operand.(*ast.Ident)
	if !ok || c.withLimit(id.Name) > 0 {
		return nil, false
	}
	l, ok := c.resolveLocal(id.Name)
	if !ok || !l.initialized || l.kind == bindConst || l.kind == bindFuncSelf {
		return nil, false
	}
	c.reg.NoteRead(id, regcomp.Res{Kind: regcomp.ResLocal, Index: l.slot})
	c.noteWrite(id, regcomp.Res{Kind: regcomp.ResLocal, Index: l.slot})
	return l, true
}

func (c *compiler) compileUpdate(n *ast.Update) { c.compileUpdateKeep(n, true) }

// compileUpdateKeep compiles ++ or --, leaving its value when keep is set.
// Without it, a property's update leaves nothing, where the other forms still
// leave a value for the caller to drop.
func (c *compiler) compileUpdateKeep(n *ast.Update, keep bool) {
	if call, ok := n.Operand.(*ast.Call); ok {
		c.compileCallTarget(call, n.Start)
		return
	}
	if _, ok := c.updatableLocal(n); ok {
		// An update of a local whose value is read is one instruction.
		return
	}

	switch target := n.Operand.(type) {
	case *ast.Ident:
		// Inside a `with` body the name is resolved once, by the read, and
		// written back to whatever that found.
		withRef := c.withLimit(target.Name) > 0
		if withRef {
			c.beginWithRef(target)
		} else {
			c.compileIdentRead(target)
		}
		if withRef {
			c.endWithRef(target)
		} else {
			c.assignTo(target, false)
		}

	case *ast.Member:
		if _, isSuper := target.Object.(*ast.Super); isSuper {
			c.noteSuper()
			// A super reference has no object: the read starts at the home
			// object's prototype and the write lands on `this`.
			if target.Computed {
				c.compileExpr(target.Property)
			} else {
				c.compileSuperMemberGet(target)
			}
			return
		}
		c.compileExpr(target.Object)
		if pn, private := target.Property.(*ast.PrivateName); private {
			// A private member is reached through its own accessors, which do
			// not consult the prototype chain the way a property does.
			c.privateName(pn, int(target.Start))
			return
		}
		if target.Computed {
			c.compileIndexForUpdate(target)
			return
		}

	default:
		c.errorf(n.Start, "invalid update target")
	}
}

// compileIndexForUpdate compiles the key of `obj[key] op= v` or
// `obj[key]++`.
func (c *compiler) compileIndexForUpdate(m *ast.Member) {
	c.compileExpr(m.Property)
}

// valueRunsNoCode reports whether a value, as the walk compiles it, can
// neither throw nor run a script's code: a read of a local or an upvalue
// without a dead zone, a literal, this where it is always bound, a
// function made. A strict assignment of one to a global resolves the
// reference at the store: nothing can happen between resolving it and
// evaluating the value, so even a proxy on the global object's chain is
// asked the same questions in the same order.
func (c *compiler) valueRunsNoCode(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.NumberLit, *ast.StringLit, *ast.BoolLit, *ast.NullLit, *ast.BigIntLit, *ast.FuncLit:
		return true
	case *ast.TemplateLit:
		return len(n.Exprs) == 0 && len(n.Quasis) == 1 && n.Quasis[0].Valid
	case *ast.This:
		// A derived constructor's this, which an arrow may share, is unbound
		// until super() binds it.
		return c.fn.Kind != bytecode.KindDerivedConstructor && c.fn.Kind != bytecode.KindArrow
	case *ast.Unary:
		switch n.Op {
		case "-":
			if _, ok := n.Operand.(*ast.NumberLit); ok {
				return true
			}
			if lit, ok := n.Operand.(*ast.BigIntLit); ok {
				_, ok := new(big.Int).SetString(lit.Raw, 0)
				return ok
			}
		case "!", "typeof":
			_, ok := fold.Value(n)
			return ok
		}
	case *ast.Ident:
		if c.withLimit(n.Name) > 0 {
			return false
		}
		if lit := c.constLiteral(n.Name); lit != nil {
			return c.valueRunsNoCode(lit)
		}
		if l, ok := c.resolveLocal(n.Name); ok {
			return l.initialized && !(c.lazyArguments && l.slot == c.lazyArgumentsSlot)
		}
		if idx, ok := c.resolveUpvalue(n.Name); ok {
			return !c.fn.Upvalues[idx].TDZ
		}
		return n.Name != c.selfName && n.Name == "undefined" && c.withDepth == 0 && c.opts.EvalScope == nil &&
			c.opts.EvalWithDepth == 0 && !c.opts.EvalOwnVarScope && !c.opts.EvalConfigurable
	}
	return false
}

func (c *compiler) compileLogical(n *ast.Logical) {
	trace := c.traceIn
	c.traceName = trace
	c.compileExpr(n.Left)
	c.traceName = trace
	c.compileExpr(n.Right)
}

func (c *compiler) compileConditional(n *ast.Conditional) {
	trace := c.traceIn
	c.compileTest(n.Test)
	c.traceName = trace
	c.compileExpr(n.Cons)
	c.traceName = trace
	c.compileExpr(n.Alt)
}

// compileMemberKey walks a member expression's key.
func (c *compiler) compileMemberKey(m *ast.Member) {
	if m.Computed {
		c.compileExpr(m.Property)
		return
	}
}

// isLazyArguments reports whether an expression is the name arguments meaning
// the function's own arguments object, made only on demand.
func (c *compiler) isLazyArguments(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	if !ok || id.Name != "arguments" {
		return false
	}
	l, ok := c.resolveLocal("arguments")
	return ok && l.slot == c.lazyArgumentsSlot
}

func (c *compiler) compileMemberRead(n *ast.Member) {
	if _, isSuper := n.Object.(*ast.Super); isSuper {
		c.compileSuperMemberGet(n)
		return
	}
	if pn, ok := n.Property.(*ast.PrivateName); ok {
		c.privateName(pn, int(n.Start))
		c.compileExpr(n.Object)
		return
	}
	if c.lazyArguments && !n.Optional && c.isLazyArguments(n.Object) {
		// arguments[i] and arguments.length in a function whose arguments
		// object is made only on demand read the arguments themselves.
		if id, ok := n.Object.(*ast.Ident); ok && c.reg != nil && (n.Computed || propKeyName(n.Property) == "length") {
			c.reg.NoteRead(id, regcomp.Res{Kind: regcomp.ResLazyArgs, Index: c.lazyArgumentsSlot})
		}
		if n.Computed {
			c.compileExpr(n.Property)
			return
		}
		if propKeyName(n.Property) == "length" {
			c.facts.argsEscape = true
			return
		}
	}
	_, named := n.Object.(*ast.Ident)
	c.lengthRead = named && !n.Computed && propKeyName(n.Property) == "length"
	c.compileExpr(n.Object)
	c.lengthRead = false
	// A failed read is reported where the property is named, as V8 reports
	// it: `a.b.c` on a line of its own says which link broke.
	if n.Computed {
		c.compileExpr(n.Property)
		return
	}
	name := propKeyName(n.Property)
	if name == "length" {
		return
	}
}

func (c *compiler) compileCall(n *ast.Call) {
	if phase, ok := importPhase(n); ok {
		c.compileImport(n, phase)
		return
	}
	// A parenthesized optional chain keeps the reference it produced, so the
	// object it read from is what the call binds `this` to.
	if chain, ok := n.Callee.(*ast.OptionalChain); ok {
		c.compileChain(chain, true)
		if hasSpread(n.Args) {
			c.compileSpreadArguments(n.Args)
			return
		}
		c.compileArguments(n.Args)
		return
	}
	// super(...) invokes the parent constructor with the current `this`.
	if _, isSuper := n.Callee.(*ast.Super); isSuper {
		c.facts.reads = true
		if n.Forward {
			c.facts.superForwards = true
			return
		}
		if hasSpread(n.Args) {
			c.compileSpreadArguments(n.Args)
			return
		}
		c.compileArguments(n.Args)
		return
	}
	// A private method call fetches through the private slot, keeping the
	// receiver for `this`.
	if m, ok := n.Callee.(*ast.Member); ok && !hasSpread(n.Args) {
		if pn, isPrivate := m.Property.(*ast.PrivateName); isPrivate {
			c.privateName(pn, int(m.Start))
			c.compileExpr(m.Object)
			c.compileArguments(n.Args)
			// A call in tail position, as any method's is.
			c.noteTail(n)
			return
		}
		if _, isSuper := m.Object.(*ast.Super); isSuper {
			c.compileSuperMemberGet(m)
			c.compileArguments(n.Args)
			c.noteTail(n)
			return
		}
	}
	// A direct eval runs its code in this scope, so the call site records what
	// is in scope for the evaluated code to reach.
	if isDirectEval(n) {
		c.compileDirectEval(n)
		return
	}
	if hasSpread(n.Args) {
		c.compileSpreadCall(n)
		return
	}
	// A method call binds `this` to the receiver without evaluating the
	// object expression twice.
	if m, ok := n.Callee.(*ast.Member); ok && !m.Optional {
		c.compileExpr(m.Object)
		if m.Computed {
			c.compileExpr(m.Property)
		}
		saved := c.facts.applyArgs
		if c.lazyArguments && c.withDepth == 0 && c.tailCall != n && isApplyOfArguments(n) {
			c.facts.applyArgs = n.Args[1].(*ast.Ident)
		}
		c.compileArguments(n.Args)
		c.facts.applyArgs = saved
		c.noteTail(n)
		return
	}

	// A call through a `with` object has that object as its receiver, which is
	// the whole difference between `with (o) f()` and `f()`. Both paths leave a
	// receiver and a callee, so the call is the same instruction either way.
	if id, ok := n.Callee.(*ast.Ident); ok && c.withDepth > 0 {
		probe := c.withProbe(bytecode.OpWithGetThis, id.Name)
		c.regWith(id, probe)
		c.compileIdentReadStatic(id)
		c.compileArguments(n.Args)
		return
	}

	c.compileExpr(n.Callee)
	c.compileArguments(n.Args)
	c.noteTail(n)
}

// importPhase reports whether n is `import(...)`, `import.defer(...)` or
// `import.source(...)`, and which. The parser writes each as a call of an
// identifier named for it, which no script can write: `import` is a reserved
// word, and the other two are not identifiers at all.
func importPhase(n *ast.Call) (uint32, bool) {
	id, ok := n.Callee.(*ast.Ident)
	if !ok {
		return 0, false
	}
	switch id.Name {
	case "import":
		return bytecode.ImportEvaluate, true
	case "import.defer":
		return bytecode.ImportDefer, true
	case "import.source":
		return bytecode.ImportSource, true
	}
	return 0, false
}

// compileImport compiles an import call as an instruction of its own. It
// names no binding, so nothing a script declares, assigns or puts in a with
// object can stand in for it. The parser has seen to the arguments: one or
// two, and no spread.
func (c *compiler) compileImport(n *ast.Call, phase uint32) {
	c.compileArguments(n.Args)
}

// compileArguments walks a call's arguments and returns how many there are.
func (c *compiler) compileArguments(args []ast.Expr) int {
	for _, a := range args {
		c.compileExpr(a)
	}
	return len(args)
}

// hasSpread reports whether an argument list contains a spread element, which
// forces the slower call form that gathers arguments into an array.
func hasSpread(args []ast.Expr) bool {
	for _, a := range args {
		if _, ok := a.(*ast.Spread); ok {
			return true
		}
	}
	return false
}

// compileSpreadArguments builds an array holding a call's arguments, which is
// how a call containing a spread element passes them.
func (c *compiler) compileSpreadArguments(args []ast.Expr) {
	for _, a := range args {
		if sp, ok := a.(*ast.Spread); ok {
			c.compileExpr(sp.Arg)
			continue
		}
		c.compileExpr(a)
	}
}

func (c *compiler) compileNew(n *ast.New) {
	if hasSpread(n.Args) {
		c.compileExpr(n.Callee)
		c.compileSpreadArguments(n.Args)
		return
	}
	c.compileExpr(n.Callee)
	c.compileArguments(n.Args)
}

// compileSpreadCall compiles a call whose arguments include a spread element.
//
// The receiver, the callee and an argument array are pushed in that order, so
// that the instruction can supply `this` correctly for a method call.
func (c *compiler) compileSpreadCall(n *ast.Call) {
	if m, ok := n.Callee.(*ast.Member); ok && !m.Optional {
		if pn, isPrivate := m.Property.(*ast.PrivateName); isPrivate {
			c.privateName(pn, int(m.Start))
			c.compileExpr(m.Object)
			c.compileSpreadArguments(n.Args)
			return
		}
		if _, isSuper := m.Object.(*ast.Super); isSuper {
			// A super reference has no object: the method comes from the
			// home object's prototype and the receiver is `this`.
			c.compileSuperMemberGet(m)
			c.compileSpreadArguments(n.Args)
			return
		}
		c.compileExpr(m.Object)
		if m.Computed {
			c.compileExpr(m.Property)
		}
		c.compileSpreadArguments(n.Args)
		return
	}
	if id, ok := n.Callee.(*ast.Ident); ok && c.withDepth > 0 {
		// Through a with object, as a call without a spread: the object
		// the name is found on is the receiver.
		probe := c.withProbe(bytecode.OpWithGetThis, id.Name)
		c.regWith(id, probe)
		c.compileIdentReadStatic(id)
		c.compileSpreadArguments(n.Args)
		return
	}
	// A plain call has no receiver, so undefined stands in for one.
	c.compileExpr(n.Callee)
	c.compileSpreadArguments(n.Args)
}

// compileChainCallArgs compiles a call's arguments inside an optional chain.
func (c *compiler) compileChainCallArgs(n *ast.Call) {
	if hasSpread(n.Args) {
		c.compileSpreadArguments(n.Args)
		return
	}
	c.compileArguments(n.Args)
}

// compileChain compiles a chain, where wantThis says what it read from is
// the receiver of a call that follows: what `(a?.b)()` calls the method on.
func (c *compiler) compileChain(n *ast.OptionalChain, wantThis bool) {
	if wantThis {
		c.compileChainLinkThis(n.Base)
	} else {
		c.compileChainLink(n.Base)
	}
}

// compileDeleteChain compiles delete of an optional chain's property, a?.b
// or a?.b.c: the property is removed where no link short-circuited, and the
// result is true where one did.
func (c *compiler) compileDeleteChain(n *ast.Unary, m *ast.Member) {
	if pn, private := m.Property.(*ast.PrivateName); private {
		c.errorf(n.Start, "a private name cannot be deleted: #%s", pn.Name)
		return
	}
	c.compileChainLink(m.Object)
	c.compileMemberKey(m)
}

// compileChainLinkThis compiles a chain's last link, what it read from kept
// as the receiver.
func (c *compiler) compileChainLinkThis(e ast.Expr) {
	m, ok := e.(*ast.Member)
	if !ok {
		c.compileChainLink(e)
		return
	}
	if _, isSuper := m.Object.(*ast.Super); isSuper {
		c.compileSuperMemberGet(m)
		return
	}
	c.compileChainLink(m.Object)
	switch {
	case m.Computed:
		c.compileExpr(m.Property)
	default:
		if pn, private := m.Property.(*ast.PrivateName); private {
			c.privateName(pn, int(m.Start))
		}
	}
}

// compileChainLink compiles one link of an optional chain.
func (c *compiler) compileChainLink(e ast.Expr) {
	switch n := e.(type) {
	case *ast.Member:
		if _, isSuper := n.Object.(*ast.Super); isSuper {
			// `super?.x` is not syntax: the read starts at the home
			// object's prototype.
			c.compileSuperMemberGet(n)
			return
		}
		c.compileChainLink(n.Object)
		switch {
		case n.Computed:
			c.compileExpr(n.Property)
		default:
			if pn, private := n.Property.(*ast.PrivateName); private {
				// A private name is reached through the class's own
				// accessors, not the prototype chain: `a?.b.#c` and
				// `a?.#c` are what the grammar allows.
				c.privateName(pn, int(n.Start))
			}
		}

	case *ast.Call:
		if _, isSuper := n.Callee.(*ast.Super); isSuper {
			// `super()?.x`: the call is the base the chain reads from, and a
			// super call is never itself optional.
			c.compileCall(n)
			return
		}
		if phase, ok := importPhase(n); ok {
			// `import(x)?.then`: never optional itself either.
			c.compileImport(n, phase)
			return
		}
		if chain, ok := n.Callee.(*ast.OptionalChain); ok {
			// `(a?.b)?.()`: the parenthesized chain is complete in itself, and
			// what it read from is the receiver of the call that follows.
			c.compileChain(chain, true)
			c.compileChainCallArgs(n)
			return
		}
		if m, ok := n.Callee.(*ast.Member); ok {
			if _, isSuper := m.Object.(*ast.Super); isSuper {
				// The method comes from the home object's prototype and the
				// receiver is `this`.
				c.compileSuperMemberGet(m)
				c.compileChainCallArgs(n)
				return
			}
			c.compileChainLink(m.Object)
			switch {
			case m.Computed:
				c.compileExpr(m.Property)
			default:
				if pn, private := m.Property.(*ast.PrivateName); private {
					c.privateName(pn, int(m.Start))
				}
			}
			c.compileChainCallArgs(n)
			return
		}
		c.compileChainLink(n.Callee)
		if hasSpread(n.Args) {
			c.compileSpreadArguments(n.Args)
			return
		}
		c.compileArguments(n.Args)

	default:
		c.compileExpr(e)
	}
}

// ---------------------------------------------------------------------------
// Assignment
// ---------------------------------------------------------------------------

func (c *compiler) compileAssign(n *ast.Assign) {
	trace := ""
	if handsName(n.Value) {
		trace = c.assignTraceName(n.Target)
	}
	value := func(name string) {
		c.traceName = trace
		c.compileExprNamed(n.Value, name)
	}
	if call, ok := n.Target.(*ast.Call); ok {
		// Sloppy code may assign to a call: the call is made, and the
		// assignment is a ReferenceError before the value is evaluated.
		c.compileCallTarget(call, int(n.Start))
		return
	}
	switch n.Op {
	case "=":
		if isPattern(n.Target) {
			c.compileExpr(n.Value)
			c.compileDestructuringAssign(n.Target)
			return
		}
		if m, ok := n.Target.(*ast.Member); ok {
			c.compileMemberStore(m, func() {
				value("")
			}, true)
			return
		}
		if id, ok := n.Target.(*ast.Ident); ok && c.withLimit(id.Name) > 0 {
			// Inside a `with` body the name is resolved before the value is
			// evaluated, so a value that removes the property still writes to
			// the object the name named.
			c.resolveWithRef(id)
			value(id.Name)
			c.endWithRef(id)
			return
		}
		if id, ok := n.Target.(*ast.Ident); ok && c.checkGlobalRef(id) {
			// In strict mode a name that resolves to nothing cannot be
			// assigned to, and it is the reference that is unresolvable rather
			// than the assignment: what the name resolves to is settled before
			// the value is evaluated and reported after it. A value that
			// creates the global does not excuse the assignment, and a value
			// that throws is what the assignment reports.
			value(nameOf(n.Target))
			if c.valueRunsNoCode(n.Value) {
				if c.reg != nil {
					c.noteWrite(id, regcomp.Res{Kind: regcomp.ResGlobal})
				}
				return
			}
			c.assignTo(n.Target, false)
			return
		}
		value(nameOf(n.Target))
		c.assignTo(n.Target, false)

	case "&&=", "||=", "??=":
		if m, ok := n.Target.(*ast.Member); ok {
			c.compileMemberUpdate(m, func() {
				value(nameOf(n.Target))
			})
			return
		}
		if id, ok := n.Target.(*ast.Ident); ok && c.withLimit(id.Name) > 0 {
			// Inside a `with` body the name is resolved once, by the read, and
			// written back to whatever that found.
			c.beginWithRef(id)
			value(id.Name)
			c.endWithRef(id)
			return
		}
		// A logical assignment only stores when the short circuit does not
		// take, so the read comes first and the store is inside the branch.
		c.compileReadTarget(n.Target)
		value(nameOf(n.Target))
		c.assignTo(n.Target, false)

	default:
		if m, ok := n.Target.(*ast.Member); ok {
			c.compileMemberUpdate(m, func() { c.compileExpr(n.Value) })
			return
		}
		if id, ok := n.Target.(*ast.Ident); ok && c.withLimit(id.Name) > 0 {
			c.beginWithRef(id)
			c.compileExpr(n.Value)
			c.endWithRef(id)
			return
		}
		// A compound assignment reads, combines and writes back.
		c.compileReadTarget(n.Target)
		c.compileExpr(n.Value)
		c.assignTo(n.Target, false)
	}
}

// compileMemberUpdate walks `obj.k op= value` and its logical relatives:
// the object and the key, evaluated once, then the value, which emitValue
// walks.
func (c *compiler) compileMemberUpdate(m *ast.Member, emitValue func()) {
	if _, isSuper := m.Object.(*ast.Super); isSuper {
		c.noteSuper()
		// The read starts at the home object's prototype and the write
		// lands on `this`.
		if m.Computed {
			c.compileExpr(m.Property)
		} else {
			c.compileSuperMemberGet(m)
		}
		emitValue()
		return
	}
	if pn, private := m.Property.(*ast.PrivateName); private {
		// A private name is not an expression: only the object is kept.
		c.privateName(pn, int(m.Start))
		c.compileExpr(m.Object)
		emitValue()
		return
	}
	c.compileExpr(m.Object)
	if m.Computed {
		c.compileIndexForUpdate(m)
	}
	emitValue()
}

// compileReadTarget walks the read of an assignment target's current value.
func (c *compiler) compileReadTarget(target ast.Expr) {
	switch t := target.(type) {
	case *ast.Ident:
		c.compileIdentRead(t)
	case *ast.Member:
		c.compileMemberRead(t)
	default:
		c.errorf(target.Pos(), "invalid assignment target")
	}
}

// assignToIdentStatic stores into the binding a name resolves to, without
// consulting any enclosing `with` object.
func (c *compiler) assignToIdentStatic(t *ast.Ident, initializing bool) {
	if l, ok := c.resolveLocal(t.Name); ok {
		if !initializing && l.kind == bindFuncSelf {
			// A function expression's own name is an immutable binding, and
			// assigning to it outside strict mode is quietly discarded rather
			// than refused.
			if c.fn.Strict {
				c.noteWrite(t, regcomp.Res{Kind: regcomp.ResConstAssign})
			} else {
				c.noteWrite(t, regcomp.Res{Kind: regcomp.ResIgnore})
			}
			return
		}
		if !initializing && l.kind == bindConst && l.initialized {
			// A runtime error rather than an early one: the assignment may
			// sit in a function that is never called.
			c.noteWrite(t, regcomp.Res{Kind: regcomp.ResConstAssign})
			return
		}
		if l.initialized || initializing {
			kind := regcomp.ResLocal
			if initializing {
				kind = regcomp.ResLocalInit
			}
			c.noteWrite(t, regcomp.Res{Kind: kind, Index: l.slot})
		} else {
			c.noteWrite(t, regcomp.Res{Kind: regcomp.ResLocalCheck, Index: l.slot})
		}
		return
	}
	if c.reg != nil {
		c.regNoteUpvalueWrite(t, initializing)
	}
	if idx, ok := c.resolveUpvalue(t.Name); ok {
		if c.fn.Upvalues[idx].TDZ && !initializing {
			// The dead zone outranks constness: writing to a binding that
			// does not exist yet is a ReferenceError whichever it is.
			return
		}
		if c.fn.Upvalues[idx].FuncSelf && !initializing {
			return
		}
		if !c.fn.Upvalues[idx].Mutable && !initializing {
			return
		}
		return
	}
	if !initializing && t.Name == c.selfName {
		return
	}
	if initializing && c.globalLex[t.Name] {
		// The binding is the script-level lexical one, and this is what takes
		// it out of its dead zone.
		c.noteWrite(t, regcomp.Res{Kind: regcomp.ResGlobalLexInit})
		return
	}
	if initializing && c.isModuleLex(t.Name) {
		// The same, for a module's top-level binding.
		c.noteWrite(t, regcomp.Res{Kind: regcomp.ResModuleLexInit})
		return
	}
	if c.reg != nil && !initializing {
		c.noteWrite(t, regcomp.Res{Kind: regcomp.ResGlobal})
	}
}

// compileCallTarget walks a call that sloppy code assigned to: the call is
// made, and then the assignment is a ReferenceError. The call's value stands
// for the expression's, on the path nothing reaches. Strict code gets here
// only as V8 parses it, and V8 makes the call there too.
func (c *compiler) compileCallTarget(call *ast.Call, pos int) {
	c.compileExpr(call)
}

// assignTo walks the store of a value into a target.
func (c *compiler) assignTo(target ast.Expr, initializing bool) {
	switch t := target.(type) {
	case *ast.Call:
		// A for-in or for-of head that is a call.
		c.compileCallTarget(t, int(t.Start))
		return
	case *ast.Ident:
		// Inside a `with` body the object may be what is written to. An
		// initializing store is a declaration, which binds in its own scope
		// whatever the object holds.
		if !initializing {
			probe := c.withProbe(bytecode.OpWithSet, t.Name)
			c.regWith(t, probe)
		}
		c.assignToIdentStatic(t, initializing)

	case *ast.Member:
		// Reached only from a compound assignment or an update, where the
		// target's subexpressions were already evaluated; a plain assignment
		// goes through compileMemberStore so that it can order them correctly.
		c.compileMemberStoreFromValue(t)

	case *ast.ArrayPattern:
		// A destructuring assignment, rather than a declaration: the leaves are
		// existing references. It arises from `[a] = b` and from a for-of head
		// whose target is a pattern.
		//
		// The pattern compilers consume the source, while assignTo leaves it
		// for its caller, so it is duplicated first.
		c.compileArrayPattern(t, ast.DeclVar, false)

	case *ast.ObjectPattern:
		c.compileObjectPattern(t, ast.DeclVar, false)

	case *ast.AssignPattern:
		// `[a = 1]` as an assignment target: the default applies to the value
		// before it reaches the reference.
		c.applyDefault(t.Default, nameOf(t.Target))
		c.assignTo(t.Target, initializing)

	default:
		c.errorf(target.Pos(), "invalid assignment target")
	}
}

// isPattern reports whether a target is a destructuring pattern.
func isPattern(e ast.Expr) bool {
	switch e.(type) {
	case *ast.ArrayPattern, *ast.ObjectPattern:
		return true
	}
	return false
}

// compileMemberStore walks `obj.p = value` and `obj[k] = value`: the object,
// then the key, then the value, the order the specification fixes.
func (c *compiler) compileMemberStore(m *ast.Member, emitValue func(), keep bool) {
	if _, isSuper := m.Object.(*ast.Super); isSuper {
		c.noteSuper()
		// A super reference has no object: the lookup starts at the home
		// object's prototype, and the receiver is `this`.
		if m.Computed {
			c.compileExpr(m.Property)
			emitValue()
			return
		}
		emitValue()
		return
	}
	if pn, ok := m.Property.(*ast.PrivateName); ok {
		c.privateName(pn, int(m.Start))
		c.compileExpr(m.Object)
		emitValue()
		return
	}
	c.compileExpr(m.Object)
	if m.Computed {
		// The key stays as it was written: `a[k] = v` converts it to a property
		// key only when the assignment happens, which is after the right-hand
		// side has run.
		c.compileExpr(m.Property)
		emitValue()
		return
	}
	emitValue()
}

// compileMemberStoreFromValue walks the store of a value made before the
// member target's parts, which is what a pattern's leaf needs.
func (c *compiler) compileMemberStoreFromValue(m *ast.Member) {
	if _, isSuper := m.Object.(*ast.Super); isSuper {
		c.noteSuper()
		if m.Computed {
			c.compileExpr(m.Property)
		}
		return
	}
	if pn, ok := m.Property.(*ast.PrivateName); ok {
		c.privateName(pn, int(m.Start))
		c.compileExpr(m.Object)
		return
	}
	c.compileExpr(m.Object)
	if m.Computed {
		c.compileExpr(m.Property)
	}
}

// compileSuperMemberGet reads a property through super, which looks it up on
// the home object's prototype rather than on the receiver.
func (c *compiler) compileSuperMemberGet(m *ast.Member) {
	c.noteSuper()
	if m.Computed {
		// The base is resolved before the key expression runs, so a derived
		// constructor that has not called super() fails before anything the
		// key might do -- and a key that changes the home object's prototype
		// does not move the reference.
		c.compileExpr(m.Property)
		return
	}
}

// compileYield walks a yield expression.
//
// `yield*` is compiled as a loop that drains the operand's iterator, forwarding
// each value out and each sent value back in, which is what delegation means.
func (c *compiler) compileYield(n *ast.Yield) {
	if !n.Delegate {
		if n.Arg != nil {
			c.compileExpr(n.Arg)
		}
		return
	}

	// yield* iterable
	//
	// The loop forwards in both directions, and in all three ways a generator
	// can be resumed. Each value the delegate produces is yielded out; each
	// value the caller sends back in is passed to the delegate's next; a throw
	// injected at the yield goes to the delegate's throw, and a return to its
	// return. Without that a generator delegating to another cannot be driven
	// at all, and closing the outer one would leave the inner one open.
	//
	// An async generator delegates over Symbol.asyncIterator, and awaits each
	// result. Falling back to Symbol.iterator when there is no async one is the
	// job of the start instruction, not of a second lookup here -- consulting
	// Symbol.iterator after an asyncIterator getter has thrown is observable.
	async := c.fn.Async
	c.compileExpr(n.Arg)
	// The first next receives undefined; after that it receives whatever the
	// caller sent to the outer generator, and the kind says which of the three
	// methods to send it to.
	c.declare(fmt.Sprintf("%%ys%d", *c.hiddenCount), bindVar, n.Start)
	*c.hiddenCount++

	// The kind is kept in a binding of its own so that it survives the
	// await an async delegation performs, which is where a return has to be
	// told apart from an exhausted delegate.
	if async {
		c.emitAwait(n.Start)
	}
	c.facts.yieldStar = true

	// The delegate has finished. Which is the end of the delegation and which
	// is the end of the outer generator depends on how it was resumed: a
	// return travels through the delegate and out of the generator that
	// delegated to it, carrying the value the delegate finished with.
	if async {
		// An async generator awaits what it returns, here as anywhere else.
		c.emitAwait(n.Start)
	}
	c.emitReturnValue(n.Start)
}

// emitAwait walks an await, marking a module's body asynchronous.
//
// A top-level await makes the module an async function: the interpreter has to
// suspend the body rather than let the await escape the frame as an error, and
// the module's completion becomes a promise.
func (c *compiler) emitAwait(pos int) {
	if c.fn.IsModule {
		c.fn.Async = true
	}
}
