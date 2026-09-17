package compiler

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// compileExpr emits code leaving the expression's value on the stack.
func (c *compiler) compileExpr(e ast.Expr) {
	c.compileExprNamed(e, "")
}

// compileExprNamed compiles an expression, giving an anonymous function or
// class the supplied name. `const f = () => {}` names the arrow "f", which is
// observable through Function.prototype.name.
func (c *compiler) compileExprNamed(e ast.Expr, name string) {
	c.enter(e.Pos())
	defer c.leave()

	switch n := e.(type) {
	case *ast.NumberLit:
		c.compileNumber(n.Value)

	case *ast.StringLit:
		if n.Value == "" {
			c.emit(bytecode.OpPushEmptyString, 0, 0)
		} else {
			c.emit(bytecode.OpPushConst, c.stringConst(n.Value), 0)
		}

	case *ast.BoolLit:
		if n.Value {
			c.emit(bytecode.OpPushTrue, 0, 0)
		} else {
			c.emit(bytecode.OpPushFalse, 0, 0)
		}

	case *ast.NullLit:
		c.emit(bytecode.OpPushNull, 0, 0)

	case *ast.This:
		c.emit(bytecode.OpPushThis, 0, 0)

	case *ast.NewTarget:
		if c.inClassFieldInit() {
			// A field initializer is a function of its own, and nothing
			// constructs it.
			c.emit(bytecode.OpPushUndef, 0, 0)
			break
		}
		c.emit(bytecode.OpNewTarget, 0, 0)

	case *ast.ImportMeta:
		c.emit(bytecode.OpImportMeta, 0, 0)

	case *ast.BigIntLit:
		c.emit(bytecode.OpPushConst, c.addConst(bytecode.Constant{
			Kind: bytecode.ConstBigInt, Str: n.Raw,
		}), 0)

	case *ast.RegexpLit:
		c.emit(bytecode.OpNewRegExp, c.addConst(bytecode.Constant{
			Kind: bytecode.ConstRegExp, Str: n.Pattern, Flags: n.Flags,
		}), 0)

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
		c.compileFunctionLiteral(n, name)

	case *ast.ClassLit:
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
			name, ref := c.privateName(pn, n.Start)
			c.compileExpr(n.Right)
			c.emitAt(n.Start, bytecode.OpPrivateIn, name, ref)
			break
		}
		c.compileExpr(n.Left)
		c.compileExpr(n.Right)
		c.emitAt(n.Start, binaryOpcode(n.Op), 0, 0)

	case *ast.Logical:
		c.compileLogical(n)

	case *ast.Conditional:
		c.compileConditional(n)

	case *ast.Assign:
		c.compileAssign(n)

	case *ast.Sequence:
		// Every operand but the last is evaluated for its effect only.
		for i, x := range n.Exprs {
			c.compileExpr(x)
			if i < len(n.Exprs)-1 {
				c.emit(bytecode.OpDrop, 0, 0)
			}
		}

	case *ast.Member:
		c.compileMemberRead(n)

	case *ast.Call:
		c.compileCall(n)

	case *ast.New:
		c.compileNew(n)

	case *ast.OptionalChain:
		c.compileOptionalChain(n)

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

// compileNumber emits the cheapest push for a numeric literal.
func (c *compiler) compileNumber(v float64) {
	// Small integers are encoded in the instruction, avoiding a constant-pool
	// entry and the indirection to read it.
	if i := int32(v); float64(i) == v && !isNegZero(v) {
		c.emit(bytecode.OpPushInt, uint32(i), 0)
		return
	}
	c.emit(bytecode.OpPushConst, c.numberConst(v), 0)
}

// isNegZero reports whether v is negative zero, which must not take the integer
// path because it would be re-materialized as +0.
func isNegZero(v float64) bool { return v == 0 && 1/v < 0 }

// compileIdentRead emits a read of a variable.
func (c *compiler) compileIdentRead(n *ast.Ident) {
	// Inside a `with` body the object may answer instead of the binding.
	probe := c.withProbe(bytecode.OpWithGet, n.Name)
	c.compileIdentReadStatic(n)
	c.patchWithProbe(probe)
}

// compileIdentReadStatic reads a name from the binding it resolves to, without
// consulting any enclosing `with` object.
func (c *compiler) compileIdentReadStatic(n *ast.Ident) {
	// undefined, NaN and Infinity are properties of the global object, not
	// keywords, so they resolve like any other name.
	if l, ok := c.resolveLocal(n.Name); ok {
		if l.initialized {
			c.emit(bytecode.OpGetLocal, l.slot, 0)
		} else {
			c.emit(bytecode.OpGetLocalCheck, l.slot, 0)
		}
		return
	}
	if idx, ok := c.resolveUpvalue(n.Name); ok {
		if c.fn.Upvalues[idx].TDZ {
			c.emit(bytecode.OpGetUpvalueCheck, idx, 0)
		} else {
			c.emit(bytecode.OpGetUpvalue, idx, 0)
		}
		return
	}
	// A named function expression's own name refers to the running closure.
	if n.Name == c.selfName {
		c.emit(bytecode.OpPushCallee, 0, 0)
		return
	}
	// A function that uses `arguments` binds it to a slot in its prologue, so
	// the searches above will have found it. Reaching here means the reference
	// is in a position with no arguments object at all, such as the top level.
	c.emitAt(n.Start, bytecode.OpGetGlobal, c.nameIdx(n.Name), 0)
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
			c.emit(bytecode.OpPushConst, c.stringConst(q.Cooked), 0)
			parts++
		}
		if i < len(n.Exprs) {
			c.compileExpr(n.Exprs[i])
			c.emit(bytecode.OpToString, 0, 0)
			parts++
		}
	}
	if parts == 0 {
		c.emit(bytecode.OpPushEmptyString, 0, 0)
		return
	}
	if parts > 1 {
		c.emit(bytecode.OpConcat, uint32(parts), 0)
	}
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
			c.emit(bytecode.OpPushThis, 0, 0)
			c.compileSuperMemberGet(m)
		} else {
			c.compileExpr(m.Object)
			switch {
			case m.Computed:
				c.compileExpr(m.Property)
				c.emit(bytecode.OpGetIndexThis, 0, 0)
			default:
				if pn, private := m.Property.(*ast.PrivateName); private {
					// The receiver stays beneath the method, as
					// OpGetPropThis leaves it.
					name, ref := c.privateName(pn, m.Start)
					c.emit(bytecode.OpDup, 0, 0)
					c.emitAt(m.Start, bytecode.OpGetPrivate, name, ref)
					break
				}
				c.emit(bytecode.OpGetPropThis, c.nameIdx(propKeyName(m.Property)), 0)
			}
		}
		argc := c.compileTemplateArguments(idx, n.Quasi)
		c.emitAt(n.Start, bytecode.OpCallMethod, uint32(argc), 0)
		return
	}

	c.compileExpr(n.Tag)
	argc := c.compileTemplateArguments(idx, n.Quasi)
	c.emitAt(n.Start, bytecode.OpCall, uint32(argc), 0)
}

// compileTemplateArguments pushes the strings object and the substitutions.
func (c *compiler) compileTemplateArguments(idx uint32, q *ast.TemplateLit) int {
	c.emit(bytecode.OpTemplateObject, idx, 0)
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
	return uint32(len(c.fn.Templates) - 1)
}

func (c *compiler) compileArrayLit(n *ast.ArrayLit) {
	// Without spread or holes the whole literal is built in one instruction
	// from values already on the stack.
	simple := true
	for _, el := range n.Elements {
		if el == nil {
			simple = false
			break
		}
		if _, isSpread := el.(*ast.Spread); isSpread {
			simple = false
			break
		}
	}
	if simple {
		for _, el := range n.Elements {
			c.compileExpr(el)
		}
		c.emit(bytecode.OpNewArray, uint32(len(n.Elements)), 0)
		return
	}

	c.emit(bytecode.OpNewArray, 0, 0)
	for _, el := range n.Elements {
		switch {
		case el == nil:
			// A hole advances the length without creating a property, which is
			// what makes `1 in [1,,3]` false and what the iteration methods
			// skip.
			c.emit(bytecode.OpPushUninitialized, 0, 0)
			c.emit(bytecode.OpArrayPush, 0, 0)
		default:
			if sp, ok := el.(*ast.Spread); ok {
				c.compileExpr(sp.Arg)
				c.emit(bytecode.OpArraySpread, 0, 0)
				continue
			}
			c.compileExpr(el)
			c.emit(bytecode.OpArrayPush, 0, 0)
		}
	}
}

func (c *compiler) compileObjectLit(n *ast.ObjectLit) {
	c.emit(bytecode.OpNewObject, 0, 0)
	for _, p := range n.Props {
		switch p.Kind {
		case ast.PropSpread:
			c.compileExpr(p.Value)
			c.emit(bytecode.OpCopyDataProps, 0, 0)
			continue
		case ast.PropGet, ast.PropSet:
			// An accessor is named after its property with a "get " or "set "
			// prefix, which is what distinguishes the two halves in a stack
			// trace.
			nameKind, prefix := uint32(1), "get "
			if p.Kind == ast.PropSet {
				nameKind, prefix = 2, "set "
			}
			if p.Computed {
				op := bytecode.OpDefineGetterIndex
				if p.Kind == ast.PropSet {
					op = bytecode.OpDefineSetterIndex
				}
				c.compileExpr(p.Key)
				c.emit(bytecode.OpToPropertyKey, 0, 0)
				c.compileExpr(p.Value)
				c.emit(bytecode.OpSetFuncName, nameKind, 0)
				c.emit(bytecode.OpSetHomeObject, 2, 0)
				c.emit(op, 0, 0)
				continue
			}
			op := bytecode.OpDefineGetter
			if p.Kind == ast.PropSet {
				op = bytecode.OpDefineSetter
			}
			c.compileExprNamed(p.Value, prefix+propKeyName(p.Key))
			c.emit(bytecode.OpSetHomeObject, 1, 0)
			c.emit(op, c.nameIdx(propKeyName(p.Key)), 0)
			continue
		}

		// __proto__: v in a literal sets the prototype rather than adding a
		// property, unless it is a shorthand or a method.
		if !p.Computed && !p.Shorthand && !p.Method && propKeyName(p.Key) == "__proto__" {
			c.compileExpr(p.Value)
			c.emit(bytecode.OpSetProtoOf, 0, 0)
			continue
		}

		if p.Computed {
			c.compileExpr(p.Key)
			c.emit(bytecode.OpToPropertyKey, 0, 0)
			c.compileExpr(p.Value)
			// The name comes from the key, which is only known now. A value
			// that is not an anonymous definition keeps whatever name it has:
			// `{[k]: f}` must not rename f.
			if p.Method || isAnonymousFnDef(p.Value) {
				c.emit(bytecode.OpSetFuncName, 0, 0)
			}
			if p.Method {
				// A shorthand method may use super, which resolves against the
				// literal it is defined in.
				c.emit(bytecode.OpSetHomeObject, 2, 0)
			}
			c.emit(bytecode.OpDefineIndex, 0, 0)
			continue
		}
		key := propKeyName(p.Key)
		c.compileExprNamed(p.Value, key)
		if p.Method {
			// A shorthand method may use super, which resolves against the
			// literal it is defined in. A plain `k: function(){}` may not, and
			// deliberately does not get a home object.
			c.emit(bytecode.OpSetHomeObject, 1, 0)
		}
		c.emit(bytecode.OpDefineField, c.nameIdx(key), 0)
	}
}

// isAnonymousFnDef reports whether an expression is an AnonymousFunctionDefinition,
// which is what decides whether the surrounding syntax gets to name it.
//
// A named function keeps its own name, and an expression that merely evaluates
// to a function is not a definition at all -- `{[k]: f}` leaves f alone.
func isAnonymousFnDef(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.FuncLit:
		return n.Name == nil
	case *ast.ClassLit:
		return n.Name == nil
	}
	return false
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
	case "typeof":
		// typeof on an undeclared identifier must yield "undefined" rather
		// than throwing, which needs the non-throwing global read.
		if id, ok := n.Operand.(*ast.Ident); ok {
			probe := c.withProbe(bytecode.OpWithTypeof, id.Name)
			// The non-throwing global read is only right for a name that
			// resolves nowhere; a binding, or the function's own name, is
			// read the ordinary way.
			if _, isLocal := c.resolveLocal(id.Name); !isLocal && id.Name != c.selfName {
				if _, isUp := c.resolveUpvalue(id.Name); !isUp {
					c.emit(bytecode.OpGetGlobalOpt, c.nameIdx(id.Name), 0)
					c.emit(bytecode.OpTypeOf, 0, 0)
					c.patchWithProbe(probe)
					return
				}
			}
			if probe >= 0 {
				c.compileExpr(n.Operand)
				c.emit(bytecode.OpTypeOf, 0, 0)
				c.patchWithProbe(probe)
				return
			}
		}
		c.compileExpr(n.Operand)
		c.emit(bytecode.OpTypeOf, 0, 0)

	case "delete":
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
					c.emit(bytecode.OpCheckThisInit, 0, 0)
					c.compileExpr(m.Property)
					c.emit(bytecode.OpDrop, 0, 0)
				}
				c.emitAt(n.Start, bytecode.OpThrowDeleteSuper, 0, 0)
				return
			}
			c.compileExpr(m.Object)
			c.compileMemberKey(m)
			c.emitAt(n.Start, bytecode.OpDeleteProp, 0, 0)
			return
		}
		if id, ok := n.Operand.(*ast.Ident); ok {
			probe := c.withProbe(bytecode.OpWithDelete, id.Name)
			defer c.patchWithProbe(probe)
			// A local or captured binding cannot be deleted at all; a global
			// can, but only if it is configurable, which a var declaration is
			// not.
			if _, isLocal := c.resolveLocal(id.Name); isLocal {
				c.emit(bytecode.OpPushFalse, 0, 0)
				return
			}
			if _, isUpvalue := c.resolveUpvalue(id.Name); isUpvalue {
				c.emit(bytecode.OpPushFalse, 0, 0)
				return
			}
			c.emitAt(n.Start, bytecode.OpDeleteVar, c.nameIdx(id.Name), 0)
			return
		}
		// Deleting anything else evaluates the operand and yields true.
		c.compileExpr(n.Operand)
		c.emit(bytecode.OpDrop, 0, 0)
		c.emit(bytecode.OpPushTrue, 0, 0)

	case "void":
		c.compileExpr(n.Operand)
		c.emit(bytecode.OpDrop, 0, 0)
		c.emit(bytecode.OpPushUndef, 0, 0)

	case "!":
		c.compileExpr(n.Operand)
		c.emit(bytecode.OpNot, 0, 0)

	case "-":
		// A negative numeric literal is folded, which also gets -0 right.
		if lit, ok := n.Operand.(*ast.NumberLit); ok {
			c.emit(bytecode.OpPushConst, c.numberConst(-lit.Value), 0)
			return
		}
		c.compileExpr(n.Operand)
		c.emitAt(n.Start, bytecode.OpNeg, 0, 0)

	case "+":
		c.compileExpr(n.Operand)
		c.emitAt(n.Start, bytecode.OpPos, 0, 0)

	case "~":
		c.compileExpr(n.Operand)
		c.emitAt(n.Start, bytecode.OpBitNot, 0, 0)

	default:
		c.errorf(n.Start, "unsupported unary operator %q", n.Op)
	}
}

func (c *compiler) compileUpdate(n *ast.Update) {
	op := bytecode.OpInc
	if n.Op == "--" {
		op = bytecode.OpDec
	}

	switch target := n.Operand.(type) {
	case *ast.Ident:
		// Inside a `with` body the name is resolved once, by the read, and
		// written back to whatever that found. The base it leaves under the
		// value is why the postfix copy goes beneath it rather than on top.
		withRef := c.withLimit(target.Name) > 0
		if withRef {
			c.beginWithRef(target)
		} else {
			c.compileIdentRead(target)
		}
		// The operand is coerced first, so that `x = "1"; x++` leaves a number
		// behind and the postfix form yields the coerced value rather than the
		// original string. To a numeric, not a number: a BigInt increments as
		// a BigInt.
		c.emit(bytecode.OpToNumeric, 0, 0)
		if !n.Prefix {
			if withRef {
				c.emit(bytecode.OpInsert2, 0, 0)
			} else {
				c.emit(bytecode.OpDup, 0, 0)
			}
		}
		c.emitAt(n.Start, op, 0, 0)
		if withRef {
			c.endWithRef(target)
		} else {
			c.assignTo(target, false)
		}
		if !n.Prefix {
			// Discard the updated value, leaving the original as the result.
			c.emit(bytecode.OpDrop, 0, 0)
		}

	case *ast.Member:
		if _, isSuper := target.Object.(*ast.Super); isSuper {
			// A super reference has no object on the stack: the read starts at
			// the home object's prototype and the write lands on `this`.
			// The key, when there is one, stays beneath the value for the
			// store; without one there is nothing under it, so the copy that
			// carries the expression's result is a plain duplicate.
			keep := bytecode.OpDup
			if target.Computed {
				c.emit(bytecode.OpSuperBase, 0, 0)
				c.compileExpr(target.Property)
				c.emit(bytecode.OpToPropertyKey, 0, 0)
				c.emit(bytecode.OpDup2, 0, 0)
				c.emit(bytecode.OpGetSuperIndex, 0, 0)
				keep = bytecode.OpInsert3
			} else {
				c.compileSuperMemberGet(target)
			}
			c.emit(bytecode.OpToNumeric, 0, 0)
			// Postfix yields the old value, so it is copied down before the
			// increment; prefix yields the new one, so the copy comes after.
			if !n.Prefix {
				c.emit(keep, 0, 0)
				c.emitAt(n.Start, op, 0, 0)
			} else {
				c.emitAt(n.Start, op, 0, 0)
				c.emit(keep, 0, 0)
			}
			if target.Computed {
				c.emitAt(n.Start, bytecode.OpSetSuperIndex, 0, 0)
			} else {
				c.emitAt(n.Start, bytecode.OpSetSuperProp,
					c.nameIdx(propKeyName(target.Property)), 0)
			}
			return
		}
		c.compileExpr(target.Object)
		if pn, private := target.Property.(*ast.PrivateName); private {
			// A private member is reached through its own accessors, which do
			// not consult the prototype chain the way a property does.
			name, ref := c.privateName(pn, target.Start)
			c.emit(bytecode.OpDup, 0, 0)
			c.emit(bytecode.OpGetPrivate, name, ref)
			c.emit(bytecode.OpToNumeric, 0, 0)
			if !n.Prefix {
				c.emit(bytecode.OpInsert2, 0, 0)
				c.emitAt(n.Start, op, 0, 0)
			} else {
				c.emitAt(n.Start, op, 0, 0)
				c.emit(bytecode.OpInsert2, 0, 0)
			}
			c.emit(bytecode.OpSetPrivate, name, ref)
			return
		}
		if target.Computed {
			c.compileExpr(target.Property)
			c.emit(bytecode.OpToPropertyKeyOfBase, 0, 0)
			c.emit(bytecode.OpDup2, 0, 0)
			c.emit(bytecode.OpGetIndex, 0, 0)
			c.emit(bytecode.OpToNumeric, 0, 0)
			// Postfix yields the old value, so it is copied down before the
			// increment; prefix yields the new one, so the copy comes after.
			if !n.Prefix {
				c.emit(bytecode.OpInsert3, 0, 0)
				c.emitAt(n.Start, op, 0, 0)
			} else {
				c.emitAt(n.Start, op, 0, 0)
				c.emit(bytecode.OpInsert3, 0, 0)
			}
			c.emit(bytecode.OpSetIndex, 0, 0)
			return
		}
		name := c.nameIdx(propKeyName(target.Property))
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpGetProp, name, 0)
		c.emit(bytecode.OpToNumeric, 0, 0)
		if !n.Prefix {
			c.emit(bytecode.OpInsert2, 0, 0)
			c.emitAt(n.Start, op, 0, 0)
		} else {
			c.emitAt(n.Start, op, 0, 0)
			c.emit(bytecode.OpInsert2, 0, 0)
		}
		c.emit(bytecode.OpSetProp, name, 0)

	default:
		c.errorf(n.Start, "invalid update target")
	}
}

func (c *compiler) compileLogical(n *ast.Logical) {
	c.compileExpr(n.Left)
	var jump int
	switch n.Op {
	case "&&":
		jump = c.emitJump(bytecode.OpJumpIfFalseKeep)
	case "||":
		jump = c.emitJump(bytecode.OpJumpIfTrueKeep)
	default: // "??"
		jump = c.emitJump(bytecode.OpJumpIfNotNullish)
	}
	c.compileExpr(n.Right)
	c.patchJump(jump)
}

func (c *compiler) compileConditional(n *ast.Conditional) {
	c.compileExpr(n.Test)
	elseJump := c.emitJump(bytecode.OpJumpIfFalse)
	c.compileExpr(n.Cons)
	endJump := c.emitJump(bytecode.OpJump)
	c.patchJump(elseJump)
	// Both branches leave one value, so the depth tracked after the jump is
	// one too high; correct it so MaxStack is not inflated.
	c.stackDepth--
	c.compileExpr(n.Alt)
	c.patchJump(endJump)
}

// compileMemberKey pushes a member expression's key.
func (c *compiler) compileMemberKey(m *ast.Member) {
	if m.Computed {
		c.compileExpr(m.Property)
		return
	}
	c.emit(bytecode.OpPushConst, c.stringConst(propKeyName(m.Property)), 0)
}

func (c *compiler) compileMemberRead(n *ast.Member) {
	if _, isSuper := n.Object.(*ast.Super); isSuper {
		c.compileSuperMemberGet(n)
		return
	}
	if pn, ok := n.Property.(*ast.PrivateName); ok {
		name, ref := c.privateName(pn, n.Start)
		c.compileExpr(n.Object)
		c.emitAt(n.Start, bytecode.OpGetPrivate, name, ref)
		return
	}
	c.compileExpr(n.Object)
	if n.Computed {
		c.compileExpr(n.Property)
		c.emitAt(n.Start, bytecode.OpGetIndex, 0, 0)
		return
	}
	name := propKeyName(n.Property)
	if name == "length" {
		c.emitAt(n.Start, bytecode.OpGetLength, 0, 0)
		return
	}
	c.emitAt(n.Start, bytecode.OpGetProp, c.nameIdx(name), 0)
}

func (c *compiler) compileCall(n *ast.Call) {
	// super(...) invokes the parent constructor with the current `this`.
	if _, isSuper := n.Callee.(*ast.Super); isSuper {
		if hasSpread(n.Args) {
			c.compileSpreadArguments(n.Args)
			c.emitAt(n.Start, bytecode.OpSuperCall, 0, 1)
			return
		}
		argc := c.compileArguments(n.Args)
		c.emitAt(n.Start, bytecode.OpSuperCall, uint32(argc), 0)
		return
	}
	// A private method call fetches through the private slot, keeping the
	// receiver for `this`.
	if m, ok := n.Callee.(*ast.Member); ok && !hasSpread(n.Args) {
		if pn, isPrivate := m.Property.(*ast.PrivateName); isPrivate {
			name, ref := c.privateName(pn, m.Start)
			c.compileExpr(m.Object)
			c.emit(bytecode.OpDup, 0, 0)
			c.emit(bytecode.OpGetPrivate, name, ref)
			argc := c.compileArguments(n.Args)
			c.emitAt(n.Start, bytecode.OpCallMethod, uint32(argc), 0)
			return
		}
		if _, isSuper := m.Object.(*ast.Super); isSuper {
			c.emit(bytecode.OpPushThis, 0, 0)
			c.compileSuperMemberGet(m)
			argc := c.compileArguments(n.Args)
			c.emitAt(n.Start, bytecode.OpCallMethod, uint32(argc), 0)
			return
		}
	}
	// A direct eval runs its code in this scope, so the call site records what
	// is in scope for the evaluated code to reach.
	if isDirectEval(n) && !hasSpread(n.Args) && c.withDepth == 0 {
		c.compileDirectEval(n)
		return
	}
	if hasSpread(n.Args) {
		c.compileSpreadCall(n)
		return
	}
	// A method call keeps the receiver on the stack so that `this` binds to it
	// without evaluating the object expression twice.
	if m, ok := n.Callee.(*ast.Member); ok && !m.Optional {
		c.compileExpr(m.Object)
		if m.Computed {
			c.compileExpr(m.Property)
			c.emit(bytecode.OpGetIndexThis, 0, 0)
		} else {
			c.emit(bytecode.OpGetPropThis, c.nameIdx(propKeyName(m.Property)), 0)
		}
		argc := c.compileArguments(n.Args)
		c.emitAt(n.Start, bytecode.OpCallMethod, uint32(argc), 0)
		return
	}

	// A call through a `with` object has that object as its receiver, which is
	// the whole difference between `with (o) f()` and `f()`. Both paths leave a
	// receiver and a callee, so the call is the same instruction either way.
	if id, ok := n.Callee.(*ast.Ident); ok && c.withDepth > 0 {
		probe := c.withProbe(bytecode.OpWithGetThis, id.Name)
		c.emit(bytecode.OpPushUndef, 0, 0)
		c.compileIdentReadStatic(id)
		c.patchWithProbe(probe)
		argc := c.compileArguments(n.Args)
		c.emitAt(n.Start, bytecode.OpCallMethod, uint32(argc), 0)
		return
	}

	c.compileExpr(n.Callee)
	argc := c.compileArguments(n.Args)
	c.emitAt(n.Start, bytecode.OpCall, uint32(argc), 0)
}

// compileArguments pushes a call's arguments and returns how many there are.
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
	c.emit(bytecode.OpNewArray, 0, 0)
	for _, a := range args {
		if sp, ok := a.(*ast.Spread); ok {
			c.compileExpr(sp.Arg)
			c.emit(bytecode.OpSpreadIter, 0, 0)
			continue
		}
		c.compileExpr(a)
		c.emit(bytecode.OpArrayPush, 0, 0)
	}
}

func (c *compiler) compileNew(n *ast.New) {
	if hasSpread(n.Args) {
		c.compileExpr(n.Callee)
		c.compileSpreadArguments(n.Args)
		c.emitAt(n.Start, bytecode.OpNewSpread, 0, 0)
		return
	}
	c.compileExpr(n.Callee)
	argc := c.compileArguments(n.Args)
	c.emitAt(n.Start, bytecode.OpNew, uint32(argc), 0)
}

// compileSpreadCall compiles a call whose arguments include a spread element.
//
// The receiver, the callee and an argument array are pushed in that order, so
// that the instruction can supply `this` correctly for a method call.
func (c *compiler) compileSpreadCall(n *ast.Call) {
	if m, ok := n.Callee.(*ast.Member); ok && !m.Optional {
		if pn, isPrivate := m.Property.(*ast.PrivateName); isPrivate {
			name, ref := c.privateName(pn, m.Start)
			c.compileExpr(m.Object)
			c.emit(bytecode.OpDup, 0, 0)
			c.emit(bytecode.OpGetPrivate, name, ref)
			c.compileSpreadArguments(n.Args)
			c.emitAt(n.Start, bytecode.OpCallSpread, 0, 0)
			return
		}
		if _, isSuper := m.Object.(*ast.Super); isSuper {
			// A super reference has no object on the stack: the method comes
			// from the home object's prototype and the receiver is `this`.
			c.emit(bytecode.OpPushThis, 0, 0)
			c.compileSuperMemberGet(m)
			c.compileSpreadArguments(n.Args)
			c.emitAt(n.Start, bytecode.OpCallSpread, 0, 0)
			return
		}
		c.compileExpr(m.Object)
		c.emit(bytecode.OpDup, 0, 0)
		if m.Computed {
			c.compileExpr(m.Property)
			c.emit(bytecode.OpGetIndex, 0, 0)
		} else {
			c.emit(bytecode.OpGetProp, c.nameIdx(propKeyName(m.Property)), 0)
		}
		c.compileSpreadArguments(n.Args)
		c.emitAt(n.Start, bytecode.OpCallSpread, 0, 0)
		return
	}
	// A plain call has no receiver, so undefined stands in for one.
	c.emit(bytecode.OpPushUndef, 0, 0)
	c.compileExpr(n.Callee)
	c.compileSpreadArguments(n.Args)
	c.emitAt(n.Start, bytecode.OpCallSpread, 0, 0)
}

// chainJump is a pending short-circuit from an optional link.
//
// live records how many stack slots the link had built up when it tested, so
// that the landing pad can clear exactly those. A property access has one (the
// object); a method call has two (the receiver and the function).
type chainJump struct {
	pc   int
	live int
}

// compileOptionalChain compiles a chain, wiring every `?.` link to jump past
// the rest of the chain when its base is nullish.
func (c *compiler) compileOptionalChain(n *ast.OptionalChain) {
	var jumps []chainJump
	c.compileChainLink(n.Base, &jumps)
	if len(jumps) == 0 {
		return
	}
	done := c.emitJump(bytecode.OpJump)

	// Each short circuit gets its own landing pad, because links differ in how
	// much they left on the stack. Every pad produces undefined, which is the
	// chain's value regardless of whether null or undefined triggered it.
	var exits []int
	for i, j := range jumps {
		c.patchJumpTo(j.pc, c.here())
		for range j.live {
			c.emit(bytecode.OpDrop, 0, 0)
		}
		c.emit(bytecode.OpPushUndef, 0, 0)
		// Every pad but the last has to jump over the ones that follow it.
		if i < len(jumps)-1 {
			exits = append(exits, c.emitJump(bytecode.OpJump))
		}
	}
	c.patchJump(done)
	for _, pc := range exits {
		c.patchJump(pc)
	}
}

// compileChainLink compiles one link of an optional chain, collecting the
// short-circuit jumps so the caller can give each a landing pad.
func (c *compiler) compileChainLink(e ast.Expr, jumps *[]chainJump) {
	switch n := e.(type) {
	case *ast.Member:
		if _, isSuper := n.Object.(*ast.Super); isSuper {
			// A super reference has no object on the stack: the read starts at
			// the home object's prototype. `super?.x` is not syntax, so there
			// is never a short circuit to record here.
			c.compileSuperMemberGet(n)
			return
		}
		c.compileChainLink(n.Object, jumps)
		if n.Optional {
			*jumps = append(*jumps, chainJump{pc: c.emitJump(bytecode.OpJumpIfNullish), live: 1})
		}
		switch {
		case n.Computed:
			c.compileExpr(n.Property)
			c.emit(bytecode.OpGetIndex, 0, 0)
		default:
			if pn, private := n.Property.(*ast.PrivateName); private {
				// A private name is not a property: it is reached through the
				// class's own accessors, which do not walk the prototype
				// chain. `a?.b.#c` is written this way and is what the
				// grammar allows -- `a?.#c` too.
				name, ref := c.privateName(pn, n.Start)
				c.emitAt(n.Start, bytecode.OpGetPrivate, name, ref)
				break
			}
			c.emit(bytecode.OpGetProp, c.nameIdx(propKeyName(n.Property)), 0)
		}

	case *ast.Call:
		if m, ok := n.Callee.(*ast.Member); ok {
			if _, isSuper := m.Object.(*ast.Super); isSuper {
				// The method comes from the home object's prototype and the
				// receiver is `this`, which goes underneath it.
				c.emit(bytecode.OpPushThis, 0, 0)
				c.compileSuperMemberGet(m)
				if n.Optional {
					*jumps = append(*jumps, chainJump{
						pc: c.emitJump(bytecode.OpJumpIfNullish), live: 2,
					})
				}
				argc := c.compileArguments(n.Args)
				c.emit(bytecode.OpCallMethod, uint32(argc), 0)
				return
			}
			c.compileChainLink(m.Object, jumps)
			if m.Optional {
				*jumps = append(*jumps, chainJump{pc: c.emitJump(bytecode.OpJumpIfNullish), live: 1})
			}
			switch {
			case m.Computed:
				c.compileExpr(m.Property)
				c.emit(bytecode.OpGetIndexThis, 0, 0)
			default:
				if pn, private := m.Property.(*ast.PrivateName); private {
					// The receiver stays beneath the method, as
					// OpGetPropThis leaves it.
					name, ref := c.privateName(pn, m.Start)
					c.emit(bytecode.OpDup, 0, 0)
					c.emitAt(m.Start, bytecode.OpGetPrivate, name, ref)
					break
				}
				c.emit(bytecode.OpGetPropThis, c.nameIdx(propKeyName(m.Property)), 0)
			}
			if n.Optional {
				// The receiver is beneath the function here, so a short
				// circuit has two slots to clear.
				*jumps = append(*jumps, chainJump{pc: c.emitJump(bytecode.OpJumpIfNullish), live: 2})
			}
			argc := c.compileArguments(n.Args)
			c.emit(bytecode.OpCallMethod, uint32(argc), 0)
			return
		}
		c.compileChainLink(n.Callee, jumps)
		if n.Optional {
			*jumps = append(*jumps, chainJump{pc: c.emitJump(bytecode.OpJumpIfNullish), live: 1})
		}
		argc := c.compileArguments(n.Args)
		c.emit(bytecode.OpCall, uint32(argc), 0)

	default:
		c.compileExpr(e)
	}
}

// ---------------------------------------------------------------------------
// Assignment
// ---------------------------------------------------------------------------

func (c *compiler) compileAssign(n *ast.Assign) {
	switch n.Op {
	case "=":
		if isPattern(n.Target) {
			c.compileExpr(n.Value)
			c.compileDestructuringAssign(n.Target)
			return
		}
		if m, ok := n.Target.(*ast.Member); ok {
			c.compileMemberStore(m, func() {
				c.compileExprNamed(n.Value, "")
			})
			return
		}
		if id, ok := n.Target.(*ast.Ident); ok && c.withLimit(id.Name) > 0 {
			// Inside a `with` body the name is resolved before the value is
			// evaluated, so a value that removes the property still writes to
			// the object the name named.
			c.resolveWithRef(id)
			c.compileExprNamed(n.Value, id.Name)
			c.endWithRef(id)
			return
		}
		c.compileExprNamed(n.Value, nameOf(n.Target))
		c.assignTo(n.Target, false)

	case "&&=", "||=", "??=":
		if m, ok := n.Target.(*ast.Member); ok {
			c.compileMemberUpdate(m, n.Op, func() {
				c.compileExprNamed(n.Value, nameOf(n.Target))
			}, n.Start)
			return
		}
		if id, ok := n.Target.(*ast.Ident); ok && c.withLimit(id.Name) > 0 {
			// Inside a `with` body the name is resolved once, by the read, and
			// written back to whatever that found.
			c.beginWithRef(id)
			c.finishUpdate(n.Op, func() {
				c.compileExprNamed(n.Value, id.Name)
			}, n.Start, 1, func() { c.endWithRef(id) })
			return
		}
		// A logical assignment only stores when the short circuit does not
		// take, so the read comes first and the store is inside the branch.
		c.compileReadTarget(n.Target)
		var jump int
		switch n.Op {
		case "&&=":
			jump = c.emitJump(bytecode.OpJumpIfFalseKeep)
		case "||=":
			jump = c.emitJump(bytecode.OpJumpIfTrueKeep)
		default:
			jump = c.emitJump(bytecode.OpJumpIfNotNullish)
		}
		c.compileExprNamed(n.Value, nameOf(n.Target))
		c.assignTo(n.Target, false)
		c.patchJump(jump)

	default:
		if m, ok := n.Target.(*ast.Member); ok {
			c.compileMemberUpdate(m, n.Op, func() { c.compileExpr(n.Value) }, n.Start)
			return
		}
		if id, ok := n.Target.(*ast.Ident); ok && c.withLimit(id.Name) > 0 {
			c.beginWithRef(id)
			c.compileExpr(n.Value)
			c.emitAt(n.Start, compoundOpcode(n.Op), 0, 0)
			c.endWithRef(id)
			return
		}
		// A compound assignment reads, combines and writes back.
		c.compileReadTarget(n.Target)
		c.compileExpr(n.Value)
		c.emitAt(n.Start, compoundOpcode(n.Op), 0, 0)
		c.assignTo(n.Target, false)
	}
}

// compileMemberUpdate compiles `obj.k op= value` and its logical relatives.
//
// The object and the key are evaluated once and kept on the stack, because they
// are expressions: `base[prop] *= f()` must call prop.toString once, and
// reading the property and writing it back have to address the same place even
// if the read changed what is there.
func (c *compiler) compileMemberUpdate(m *ast.Member, op string, emitValue func(), pos int) {
	if _, isSuper := m.Object.(*ast.Super); isSuper {
		// A super reference has no object on the stack: the read starts at the
		// home object's prototype and the write lands on `this`.
		if m.Computed {
			c.emit(bytecode.OpSuperBase, 0, 0)
			c.compileExpr(m.Property)
			c.emit(bytecode.OpToPropertyKey, 0, 0)
			c.emit(bytecode.OpDup2, 0, 0)
			c.emitAt(m.Start, bytecode.OpGetSuperIndex, 0, 0)
			c.finishUpdate(op, emitValue, pos, 2, func() {
				c.emit(bytecode.OpInsert3, 0, 0)
				c.emitAt(m.Start, bytecode.OpSetSuperIndex, 0, 0)
			})
			return
		}
		name := c.nameIdx(propKeyName(m.Property))
		c.compileSuperMemberGet(m)
		c.finishUpdate(op, emitValue, pos, 0, func() {
			c.emit(bytecode.OpDup, 0, 0)
			c.emitAt(m.Start, bytecode.OpSetSuperProp, name, 0)
		})
		return
	}
	if pn, private := m.Property.(*ast.PrivateName); private {
		// A private name is not an expression, so there is nothing to evaluate
		// twice; only the object is kept.
		name, ref := c.privateName(pn, m.Start)
		c.compileExpr(m.Object)
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpGetPrivate, name, ref)
		c.finishUpdate(op, emitValue, pos, 1, func() {
			c.emit(bytecode.OpInsert2, 0, 0)
			c.emit(bytecode.OpSetPrivate, name, ref)
		})
		return
	}

	c.compileExpr(m.Object)
	if m.Computed {
		c.compileExpr(m.Property)
		c.emit(bytecode.OpToPropertyKeyOfBase, 0, 0)
		c.emit(bytecode.OpDup2, 0, 0)
		c.emitAt(m.Start, bytecode.OpGetIndex, 0, 0)
		c.finishUpdate(op, emitValue, pos, 2, func() {
			c.emit(bytecode.OpInsert3, 0, 0)
			c.emitAt(m.Start, bytecode.OpSetIndex, 0, 0)
		})
		return
	}
	name := c.nameIdx(propKeyName(m.Property))
	c.emit(bytecode.OpDup, 0, 0)
	c.emitAt(m.Start, bytecode.OpGetProp, name, 0)
	c.finishUpdate(op, emitValue, pos, 1, func() {
		c.emit(bytecode.OpInsert2, 0, 0)
		c.emitAt(m.Start, bytecode.OpSetProp, name, 0)
	})
}

// finishUpdate combines the read value with the new one and stores it.
//
// A logical assignment stores only when the short circuit does not take, and
// leaves the read value as the result when it does -- so the base and key
// beneath it have to be dropped on that path.
func (c *compiler) finishUpdate(op string, emitValue func(), pos, under int, store func()) {
	switch op {
	case "&&=", "||=", "??=":
		var jump int
		switch op {
		case "&&=":
			jump = c.emitJump(bytecode.OpJumpIfFalseKeep)
		case "||=":
			jump = c.emitJump(bytecode.OpJumpIfTrueKeep)
		default:
			jump = c.emitJump(bytecode.OpJumpIfNotNullish)
		}
		emitValue()
		store()
		done := c.emitJump(bytecode.OpJump)
		c.patchJump(jump)
		// The short circuit leaves the read value on top of the base and key,
		// which have to go.
		c.emit(bytecode.OpNipUnder, uint32(under), 0)
		c.patchJump(done)
	default:
		emitValue()
		c.emitAt(pos, compoundOpcode(op), 0, 0)
		store()
	}
}

// compileReadTarget pushes the current value of an assignment target.
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
				c.emitAt(t.Start, bytecode.OpAssignConst, c.nameIdx(t.Name), 0)
			}
			return
		}
		if !initializing && l.kind == bindConst && l.initialized {
			// A runtime error rather than an early one: the assignment may
			// sit in a function that is never called.
			c.emitAt(t.Start, bytecode.OpAssignConst, c.nameIdx(t.Name), 0)
			return
		}
		if l.initialized || initializing {
			c.emit(bytecode.OpPutLocal, l.slot, 0)
		} else {
			c.emit(bytecode.OpDup, 0, 0)
			c.emit(bytecode.OpSetLocalCheck, l.slot, 0)
		}
		return
	}
	if idx, ok := c.resolveUpvalue(t.Name); ok {
		if c.fn.Upvalues[idx].TDZ && !initializing {
			// The dead zone outranks constness: writing to a binding that
			// does not exist yet is a ReferenceError whichever it is.
			c.emit(bytecode.OpDup, 0, 0)
			c.emitAt(t.Start, bytecode.OpSetUpvalueCheck, idx, 0)
			return
		}
		if c.fn.Upvalues[idx].FuncSelf && !initializing {
			if c.fn.Strict {
				c.emitAt(t.Start, bytecode.OpAssignConst, c.nameIdx(t.Name), 0)
			}
			return
		}
		if !c.fn.Upvalues[idx].Mutable && !initializing {
			c.emitAt(t.Start, bytecode.OpAssignConst, c.nameIdx(t.Name), 0)
			return
		}
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpSetUpvalue, idx, 0)
		return
	}
	if !initializing && t.Name == c.selfName {
		// The name has no slot, so nothing nested refers to it: the assignment
		// still has to be refused in strict mode and discarded otherwise.
		if c.fn.Strict {
			c.emitAt(t.Start, bytecode.OpAssignConst, c.nameIdx(t.Name), 0)
		}
		return
	}
	if initializing && c.globalLex[t.Name] {
		// The binding is the script-level lexical one, and this is what takes
		// it out of its dead zone.
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpInitGlobalLex, c.nameIdx(t.Name), 0)
		return
	}
	c.emit(bytecode.OpDup, 0, 0)
	c.emit(bytecode.OpSetGlobal, c.nameIdx(t.Name), 0)
}

// assignTo stores the value on top of the stack into a target, leaving the
// value on the stack as the expression's result.
func (c *compiler) assignTo(target ast.Expr, initializing bool) {
	switch t := target.(type) {
	case *ast.Ident:
		// Inside a `with` body the object may be what is written to. An
		// initializing store is a declaration, which binds in its own scope
		// whatever the object holds.
		if !initializing {
			probe := c.withProbe(bytecode.OpWithSet, t.Name)
			defer c.patchWithProbe(probe)
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
		c.emit(bytecode.OpDup, 0, 0)
		c.compileArrayPattern(t, ast.DeclVar, false)

	case *ast.ObjectPattern:
		c.emit(bytecode.OpDup, 0, 0)
		c.compileObjectPattern(t, ast.DeclVar, false)

	case *ast.AssignPattern:
		// `[a = 1]` as an assignment target: the default applies to the value
		// on the stack before it reaches the reference underneath.
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

// binaryOpcode maps a binary operator to its instruction.
func binaryOpcode(op string) bytecode.Op {
	switch op {
	case "+":
		return bytecode.OpAdd
	case "-":
		return bytecode.OpSub
	case "*":
		return bytecode.OpMul
	case "/":
		return bytecode.OpDiv
	case "%":
		return bytecode.OpMod
	case "**":
		return bytecode.OpPow
	case "&":
		return bytecode.OpBitAnd
	case "|":
		return bytecode.OpBitOr
	case "^":
		return bytecode.OpBitXor
	case "<<":
		return bytecode.OpShl
	case ">>":
		return bytecode.OpShr
	case ">>>":
		return bytecode.OpUShr
	case "==":
		return bytecode.OpEq
	case "!=":
		return bytecode.OpNe
	case "===":
		return bytecode.OpStrictEq
	case "!==":
		return bytecode.OpStrictNe
	case "<":
		return bytecode.OpLt
	case "<=":
		return bytecode.OpLe
	case ">":
		return bytecode.OpGt
	case ">=":
		return bytecode.OpGe
	case "in":
		return bytecode.OpIn
	case "instanceof":
		return bytecode.OpInstanceOf
	}
	return bytecode.OpNop
}

// compoundOpcode maps a compound assignment operator to the binary instruction
// that computes its value.
func compoundOpcode(op string) bytecode.Op {
	// Strip the trailing '=' and reuse the binary table.
	return binaryOpcode(op[:len(op)-1])
}

// compileMemberStore compiles `obj.p = value` and `obj[k] = value`, evaluating
// the object, then the key, then the value, and leaving the assigned value on
// the stack as the expression's result.
//
// The order matters: each part may have side effects, and the specification
// fixes the order in which they happen.
func (c *compiler) compileMemberStore(m *ast.Member, emitValue func()) {
	if _, isSuper := m.Object.(*ast.Super); isSuper {
		// A super reference has no object on the stack: the lookup starts at
		// the home object's prototype, and the receiver is `this`.
		if m.Computed {
			c.emit(bytecode.OpSuperBase, 0, 0)
			c.compileExpr(m.Property)
			emitValue()
			// base key value -> value base key value, so the store consumes
			// three and the result is left behind.
			c.emit(bytecode.OpInsert3, 0, 0)
			c.emitAt(m.Start, bytecode.OpSetSuperIndex, 0, 0)
			return
		}
		emitValue()
		c.emit(bytecode.OpDup, 0, 0)
		c.emitAt(m.Start, bytecode.OpSetSuperProp,
			c.nameIdx(propKeyName(m.Property)), 0)
		return
	}
	if pn, ok := m.Property.(*ast.PrivateName); ok {
		name, ref := c.privateName(pn, m.Start)
		c.compileExpr(m.Object)
		emitValue()
		c.emit(bytecode.OpInsert2, 0, 0)
		c.emitAt(m.Start, bytecode.OpSetPrivate, name, ref)
		return
	}
	c.compileExpr(m.Object)
	if m.Computed {
		c.compileExpr(m.Property)
		c.emit(bytecode.OpToPropertyKeyOfBase, 0, 0)
		emitValue()
		// obj key value -> value obj key value, so the store consumes three
		// and the result is left behind.
		c.emit(bytecode.OpInsert3, 0, 0)
		c.emitAt(m.Start, bytecode.OpSetIndex, 0, 0)
		return
	}
	emitValue()
	// obj value -> value obj value
	c.emit(bytecode.OpInsert2, 0, 0)
	c.emitAt(m.Start, bytecode.OpSetProp, c.nameIdx(propKeyName(m.Property)), 0)
}

// compileMemberStoreFromValue stores a value that is already on the stack into
// a member target, which is what a compound assignment needs after combining.
func (c *compiler) compileMemberStoreFromValue(m *ast.Member) {
	if _, isSuper := m.Object.(*ast.Super); isSuper {
		// The value is on top and the key, if computed, has to go under it.
		if m.Computed {
			// The value is already on the stack, so the base and the key go
			// above it and it is swapped back on top.
			c.emit(bytecode.OpSuperBase, 0, 0)
			c.emit(bytecode.OpSwap, 0, 0)
			c.compileExpr(m.Property)
			c.emit(bytecode.OpSwap, 0, 0)
			c.emit(bytecode.OpInsert3, 0, 0)
			c.emitAt(m.Start, bytecode.OpSetSuperIndex, 0, 0)
			return
		}
		c.emit(bytecode.OpDup, 0, 0)
		c.emitAt(m.Start, bytecode.OpSetSuperProp,
			c.nameIdx(propKeyName(m.Property)), 0)
		return
	}
	if pn, ok := m.Property.(*ast.PrivateName); ok {
		name, ref := c.privateName(pn, m.Start)
		c.compileExpr(m.Object)
		c.emit(bytecode.OpSwap, 0, 0)
		c.emit(bytecode.OpInsert2, 0, 0)
		c.emit(bytecode.OpSetPrivate, name, ref)
		return
	}
	// The value is on top; the object and key have to go beneath it.
	if m.Computed {
		c.compileExpr(m.Object)
		c.compileExpr(m.Property)
		c.emit(bytecode.OpToPropertyKeyOfBase, 0, 0)
		// value obj key -> obj key value
		c.emit(bytecode.OpRot3, 0, 0)
		c.emit(bytecode.OpInsert3, 0, 0)
		c.emit(bytecode.OpSetIndex, 0, 0)
		return
	}
	c.compileExpr(m.Object)
	// value obj -> obj value
	c.emit(bytecode.OpSwap, 0, 0)
	c.emit(bytecode.OpInsert2, 0, 0)
	c.emit(bytecode.OpSetProp, c.nameIdx(propKeyName(m.Property)), 0)
}

// compileSuperMemberGet reads a property through super, which looks it up on
// the home object's prototype rather than on the receiver.
func (c *compiler) compileSuperMemberGet(m *ast.Member) {
	if m.Computed {
		// The base is resolved before the key expression runs, so a derived
		// constructor that has not called super() fails before anything the
		// key might do -- and a key that changes the home object's prototype
		// does not move the reference.
		c.emit(bytecode.OpSuperBase, 0, 0)
		c.compileExpr(m.Property)
		c.emit(bytecode.OpGetSuperIndex, 0, 0)
		return
	}
	c.emit(bytecode.OpGetSuperProp, c.nameIdx(propKeyName(m.Property)), 0)
}

// compileYield emits a yield expression.
//
// `yield*` is compiled as a loop that drains the operand's iterator, forwarding
// each value out and each sent value back in, which is what delegation means.
func (c *compiler) compileYield(n *ast.Yield) {
	if !n.Delegate {
		if n.Arg != nil {
			c.compileExpr(n.Arg)
		} else {
			c.emit(bytecode.OpPushUndef, 0, 0)
		}
		c.emitAt(n.Start, bytecode.OpYield, 0, 0)
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
	if async {
		c.emit(bytecode.OpForAwaitOfStart, 0, 0)
	} else {
		c.emit(bytecode.OpForOfStart, 0, 0)
	}
	// The first next receives undefined; after that it receives whatever the
	// caller sent to the outer generator, and the kind says which of the three
	// methods to send it to.
	kindSlot := c.declare(fmt.Sprintf("%%ys%d", *c.hiddenCount), bindVar, n.Start)
	*c.hiddenCount++
	c.emit(bytecode.OpPushUndef, 0, 0)
	c.emit(bytecode.OpPushInt, 0, 0)

	start := c.here()
	// stack: cursor sent kind. The kind is kept in a binding of its own so
	// that it survives the await an async delegation performs, which is where
	// a return has to be told apart from an exhausted delegate.
	c.emit(bytecode.OpDup, 0, 0)
	c.emit(bytecode.OpSetLocal, kindSlot, 0)
	if async {
		c.emit(bytecode.OpIterResume, 1, 0)
		c.emitAwait(n.Start)
	} else {
		c.emit(bytecode.OpIterResume, 0, 0)
	}
	raw := uint32(0)
	if !async {
		// A synchronous delegation yields the delegate's result object as it
		// is, so nothing reads its value on the way past.
		raw = 1
	}
	exit := c.emitJumpB(bytecode.OpIterUnpackDelegate, kindSlot<<1|raw)
	c.emit(bytecode.OpYieldStar, raw, 0)
	c.emit(bytecode.OpJump, uint32(start), 0)

	c.patchJump(exit)
	// The delegate's return value is the value of the whole expression, so the
	// cursor beneath it is removed rather than the value.
	c.emit(bytecode.OpSwap, 0, 0)
	c.emit(bytecode.OpDrop, 0, 0)
}

// emitAwait emits an await, marking a module's body asynchronous.
//
// A top-level await makes the module an async function: the interpreter has to
// suspend the body rather than let the await escape the frame as an error, and
// the module's completion becomes a promise.
func (c *compiler) emitAwait(pos int) {
	if c.fn.IsModule {
		c.fn.Async = true
	}
	c.emitAt(pos, bytecode.OpAwait, 0, 0)
}
