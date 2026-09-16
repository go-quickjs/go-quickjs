package compiler

import (
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
		c.emit(bytecode.OpNewTarget, 0, 0)

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
	c.emitAt(n.Start, bytecode.OpGetGlobal, c.nameIdx(n.Name), 0)
}

func (c *compiler) compileTemplate(n *ast.TemplateLit) {
	// The parts are pushed in order and concatenated in one instruction, so a
	// template with k substitutions costs one concatenation rather than k.
	parts := 0
	for i, q := range n.Quasis {
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
			// A hole still advances the length.
			c.emit(bytecode.OpPushUndef, 0, 0)
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
			op := bytecode.OpDefineGetter
			if p.Kind == ast.PropSet {
				op = bytecode.OpDefineSetter
			}
			if p.Computed {
				c.errorf(p.Start, "computed accessor names are not yet supported")
			}
			c.compileExpr(p.Value)
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
			c.emit(bytecode.OpDefineIndex, 0, 0)
			continue
		}
		key := propKeyName(p.Key)
		c.compileExprNamed(p.Value, key)
		c.emit(bytecode.OpDefineField, c.nameIdx(key), 0)
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
	case "typeof":
		// typeof on an undeclared identifier must yield "undefined" rather
		// than throwing, which needs the non-throwing global read.
		if id, ok := n.Operand.(*ast.Ident); ok {
			if _, isLocal := c.resolveLocal(id.Name); !isLocal {
				if _, isUp := c.resolveUpvalue(id.Name); !isUp {
					c.emit(bytecode.OpGetGlobalOpt, c.nameIdx(id.Name), 0)
					c.emit(bytecode.OpTypeOf, 0, 0)
					return
				}
			}
		}
		c.compileExpr(n.Operand)
		c.emit(bytecode.OpTypeOf, 0, 0)

	case "delete":
		if m, ok := n.Operand.(*ast.Member); ok {
			c.compileExpr(m.Object)
			c.compileMemberKey(m)
			c.emitAt(n.Start, bytecode.OpDeleteProp, 0, 0)
			return
		}
		// Deleting anything that is not a property reference evaluates the
		// operand and yields true.
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
		c.compileIdentRead(target)
		// The operand must be coerced first so that `x = "1"; x++` leaves a
		// number behind and the postfix form yields the coerced value.
		c.emit(bytecode.OpToNumber, 0, 0)
		if !n.Prefix {
			c.emit(bytecode.OpDup, 0, 0)
		}
		c.emitAt(n.Start, op, 0, 0)
		c.assignTo(target, false)
		if !n.Prefix {
			// Discard the updated value, leaving the original.
			c.emit(bytecode.OpDrop, 0, 0)
		}

	case *ast.Member:
		c.compileExpr(target.Object)
		if target.Computed {
			c.compileExpr(target.Property)
			c.emit(bytecode.OpToPropertyKey, 0, 0)
			// obj key -> obj key value
			c.emit(bytecode.OpDup2, 0, 0)
			c.emit(bytecode.OpGetIndex, 0, 0)
			c.emit(bytecode.OpToNumber, 0, 0)
			if !n.Prefix {
				c.emit(bytecode.OpInsert3, 0, 0)
			}
			c.emitAt(n.Start, op, 0, 0)
			c.emit(bytecode.OpSetIndex, 0, 0)
		} else {
			name := c.nameIdx(propKeyName(target.Property))
			c.emit(bytecode.OpDup, 0, 0)
			c.emit(bytecode.OpGetProp, name, 0)
			c.emit(bytecode.OpToNumber, 0, 0)
			if !n.Prefix {
				c.emit(bytecode.OpInsert2, 0, 0)
			}
			c.emitAt(n.Start, op, 0, 0)
			c.emit(bytecode.OpSetProp, name, 0)
		}

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

	c.compileExpr(n.Callee)
	argc := c.compileArguments(n.Args)
	c.emitAt(n.Start, bytecode.OpCall, uint32(argc), 0)
}

// compileArguments pushes a call's arguments and returns how many there are.
func (c *compiler) compileArguments(args []ast.Expr) int {
	for _, a := range args {
		if _, ok := a.(*ast.Spread); ok {
			c.errorf(a.Pos(), "spread arguments are not yet supported")
		}
		c.compileExpr(a)
	}
	return len(args)
}

func (c *compiler) compileNew(n *ast.New) {
	c.compileExpr(n.Callee)
	argc := c.compileArguments(n.Args)
	c.emitAt(n.Start, bytecode.OpNew, uint32(argc), 0)
}

// compileOptionalChain compiles a chain, wiring every `?.` link to jump past
// the rest of the chain when its base is nullish.
func (c *compiler) compileOptionalChain(n *ast.OptionalChain) {
	var jumps []int
	c.compileChainLink(n.Base, &jumps)
	end := c.here()
	for _, pc := range jumps {
		c.patchJumpTo(pc, end)
	}
}

// compileChainLink compiles one link of an optional chain, collecting the
// short-circuit jumps so the caller can point them all at the chain's end.
func (c *compiler) compileChainLink(e ast.Expr, jumps *[]int) {
	switch n := e.(type) {
	case *ast.Member:
		c.compileChainLink(n.Object, jumps)
		if n.Optional {
			// The value stays on the stack for the jump, which lands with
			// undefined as the chain's result.
			*jumps = append(*jumps, c.emitJump(bytecode.OpJumpIfNullish))
		}
		if n.Computed {
			c.compileExpr(n.Property)
			c.emit(bytecode.OpGetIndex, 0, 0)
		} else {
			c.emit(bytecode.OpGetProp, c.nameIdx(propKeyName(n.Property)), 0)
		}

	case *ast.Call:
		if m, ok := n.Callee.(*ast.Member); ok {
			c.compileChainLink(m.Object, jumps)
			if m.Optional {
				*jumps = append(*jumps, c.emitJump(bytecode.OpJumpIfNullish))
			}
			if m.Computed {
				c.compileExpr(m.Property)
				c.emit(bytecode.OpGetIndexThis, 0, 0)
			} else {
				c.emit(bytecode.OpGetPropThis, c.nameIdx(propKeyName(m.Property)), 0)
			}
			if n.Optional {
				*jumps = append(*jumps, c.emitJump(bytecode.OpJumpIfNullish))
			}
			argc := c.compileArguments(n.Args)
			c.emit(bytecode.OpCallMethod, uint32(argc), 0)
			return
		}
		c.compileChainLink(n.Callee, jumps)
		if n.Optional {
			*jumps = append(*jumps, c.emitJump(bytecode.OpJumpIfNullish))
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
		c.compileExprNamed(n.Value, nameOf(n.Target))
		c.assignTo(n.Target, false)

	case "&&=", "||=", "??=":
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
		// A compound assignment reads, combines and writes back.
		c.compileReadTarget(n.Target)
		c.compileExpr(n.Value)
		c.emitAt(n.Start, compoundOpcode(n.Op), 0, 0)
		c.assignTo(n.Target, false)
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

// assignTo stores the value on top of the stack into a target, leaving the
// value on the stack as the expression's result.
func (c *compiler) assignTo(target ast.Expr, initializing bool) {
	switch t := target.(type) {
	case *ast.Ident:
		if l, ok := c.resolveLocal(t.Name); ok {
			if !initializing && l.kind == bindConst && l.initialized {
				c.errorf(t.Start, "assignment to constant variable %q", t.Name)
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
			if !c.fn.Upvalues[idx].Mutable && !initializing {
				c.errorf(t.Start, "assignment to constant variable %q", t.Name)
			}
			c.emit(bytecode.OpDup, 0, 0)
			c.emit(bytecode.OpSetUpvalue, idx, 0)
			return
		}
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpSetGlobal, c.nameIdx(t.Name), 0)

	case *ast.Member:
		// The receiver and key must be evaluated before the value was pushed,
		// but the value is already on top, so it is moved into place.
		if t.Computed {
			c.compileExpr(t.Object)
			c.compileExpr(t.Property)
			c.emit(bytecode.OpToPropertyKey, 0, 0)
			// value obj key -> value obj key value
			c.emit(bytecode.OpRot3, 0, 0)
			c.emit(bytecode.OpDup, 0, 0)
			c.emit(bytecode.OpRot4, 0, 0)
			c.emit(bytecode.OpSetIndex, 0, 0)
			return
		}
		c.compileExpr(t.Object)
		c.emit(bytecode.OpSwap, 0, 0)
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpRot3, 0, 0)
		c.emit(bytecode.OpSetProp, c.nameIdx(propKeyName(t.Property)), 0)

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
