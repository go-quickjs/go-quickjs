package compiler

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// compileFunctionLiteral compiles a nested function and emits the instruction
// that builds a closure from it.
func (c *compiler) compileFunctionLiteral(fn *ast.FuncLit, inferredName string) {
	sub := newCompiler(c, c.opts)
	sub.fn.Strict = fn.Strict || c.fn.Strict
	sub.fn.Async = fn.Async
	sub.fn.Generator = fn.Generator
	sub.fn.Kind = funcKindOf(fn)
	sub.fn.IsExprBody = fn.ExprBody

	switch {
	case fn.Name != nil:
		sub.fn.Name = fn.Name.Name
	default:
		sub.fn.Name = inferredName
	}

	sub.compileFunctionBody(fn)

	idx := c.addConst(bytecode.Constant{Kind: bytecode.ConstFunction, Fn: sub.fn})
	c.emit(bytecode.OpClosure, idx, 0)
}

func funcKindOf(fn *ast.FuncLit) bytecode.FuncKind {
	switch fn.Kind {
	case ast.FuncArrow:
		return bytecode.KindArrow
	case ast.FuncMethod:
		return bytecode.KindMethod
	case ast.FuncGetter:
		return bytecode.KindGetter
	case ast.FuncSetter:
		return bytecode.KindSetter
	case ast.FuncConstructor:
		return bytecode.KindConstructor
	}
	return bytecode.KindNormal
}

// compileFunctionBody compiles a function's parameters and body into the
// receiver, which is a fresh compiler for that function.
func (c *compiler) compileFunctionBody(fn *ast.FuncLit) {
	// A named function expression can refer to itself, so its own name is
	// bound inside the body. A declaration's name binds in the enclosing scope
	// instead and must not be redeclared here.
	if fn.Name != nil && fn.Kind == ast.FuncNormal {
		slot := c.declare(fn.Name.Name, bindConst, fn.Start)
		// The binding is filled in by the caller through the closure itself;
		// until then it reads as undefined rather than throwing.
		_ = slot
	}

	c.bindParameters(fn)

	// Hoist var declarations and nested function declarations to the top of
	// the function, as their scope requires.
	var varNames []string
	collectVarNames(fn.Body, &varNames)
	for _, n := range varNames {
		if _, exists := c.resolveLocal(n); !exists {
			slot := c.declare(n, bindVar, fn.Start)
			// A hoisted var starts as undefined.
			c.emit(bytecode.OpPushUndef, 0, 0)
			c.emit(bytecode.OpSetLocal, slot, 0)
		}
	}

	c.compileStatements(fn.Body)
	c.emit(bytecode.OpReturnUndef, 0, 0)
	c.finish()
}

// bindParameters declares the parameter slots and emits the prologue for
// defaults, destructuring and the rest parameter.
func (c *compiler) bindParameters(fn *ast.FuncLit) {
	simple := true
	for _, p := range fn.Params {
		if _, ok := p.(*ast.Ident); !ok {
			simple = false
			break
		}
	}
	c.fn.HasSimpleParams = simple

	// Function.prototype.length counts the parameters before the first one
	// with a default or a rest element.
	length := 0
	counting := true
	for _, p := range fn.Params {
		switch p.(type) {
		case *ast.AssignPattern, *ast.RestElement:
			counting = false
		}
		if counting {
			length++
		}
	}
	c.fn.ParamCount = len(fn.Params)

	for i, p := range fn.Params {
		switch param := p.(type) {
		case *ast.Ident:
			c.declare(param.Name, bindParam, param.Start)

		case *ast.AssignPattern:
			// A parameter slot always exists; the default only applies when
			// the argument was undefined.
			slot := c.declareParamTarget(param.Target, i)
			c.emit(bytecode.OpGetLocal, slot, 0)
			c.emit(bytecode.OpPushUndef, 0, 0)
			c.emit(bytecode.OpStrictEq, 0, 0)
			skip := c.emitJump(bytecode.OpJumpIfFalse)
			c.compileExprNamed(param.Default, nameOf(param.Target))
			c.emit(bytecode.OpSetLocal, slot, 0)
			c.patchJump(skip)
			if !isIdent(param.Target) {
				c.emit(bytecode.OpGetLocal, slot, 0)
				c.compileDestructuring(param.Target, ast.DeclLet)
			}

		case *ast.RestElement:
			c.fn.HasRest = true
			c.declareParamTarget(param.Arg, i)
			c.errorf(param.Start, "rest parameters are not yet supported")

		default:
			// A destructuring parameter takes an anonymous slot, which the
			// pattern then unpacks.
			slot := c.nextSlot
			c.nextSlot++
			c.locals = append(c.locals, localVar{
				name: "", kind: bindParam, slot: slot, depth: c.depth, initialized: true,
			})
			var names []string
			collectPatternNames(p, &names)
			for _, n := range names {
				c.declare(n, bindLet, p.Pos())
				c.markInitialized(n)
			}
			c.emit(bytecode.OpGetLocal, slot, 0)
			c.compileDestructuring(p, ast.DeclLet)
		}
	}
	c.fn.ParamCount = len(fn.Params)
	_ = length
}

// declareParamTarget declares the binding a parameter introduces and returns
// its slot, which must be the positional slot i.
func (c *compiler) declareParamTarget(target ast.Expr, i int) uint32 {
	if id, ok := target.(*ast.Ident); ok {
		return c.declare(id.Name, bindParam, id.Start)
	}
	// A pattern parameter still occupies its positional slot; the names it
	// introduces are declared separately.
	slot := c.nextSlot
	c.nextSlot++
	c.locals = append(c.locals, localVar{
		name: "", kind: bindParam, slot: slot, depth: c.depth, initialized: true,
	})
	var names []string
	collectPatternNames(target, &names)
	for _, n := range names {
		c.declare(n, bindLet, target.Pos())
		c.markInitialized(n)
	}
	return slot
}

func isIdent(e ast.Expr) bool {
	_, ok := e.(*ast.Ident)
	return ok
}

// ---------------------------------------------------------------------------
// Destructuring
// ---------------------------------------------------------------------------

// compileDestructuring unpacks the value on top of the stack into a binding
// pattern, consuming it.
func (c *compiler) compileDestructuring(target ast.Expr, kind ast.DeclKind) {
	switch pat := target.(type) {
	case *ast.Ident:
		c.initBinding(pat, kind)

	case *ast.ArrayPattern:
		c.compileArrayPattern(pat, kind, true)

	case *ast.ObjectPattern:
		c.compileObjectPattern(pat, kind, true)

	case *ast.AssignPattern:
		c.applyDefault(pat.Default, nameOf(pat.Target))
		c.compileDestructuring(pat.Target, kind)

	default:
		c.errorf(target.Pos(), "unsupported destructuring target %T", target)
	}
}

// compileDestructuringAssign unpacks into an assignment pattern, whose leaves
// are references rather than new bindings, and leaves the source value on the
// stack as the expression's result.
func (c *compiler) compileDestructuringAssign(target ast.Expr) {
	c.emit(bytecode.OpDup, 0, 0)
	switch pat := target.(type) {
	case *ast.ArrayPattern:
		c.compileArrayPattern(pat, ast.DeclVar, false)
	case *ast.ObjectPattern:
		c.compileObjectPattern(pat, ast.DeclVar, false)
	default:
		c.errorf(target.Pos(), "unsupported destructuring target %T", target)
	}
}

// compileArrayPattern unpacks an array pattern. declaring selects between
// creating bindings and assigning to existing references.
func (c *compiler) compileArrayPattern(pat *ast.ArrayPattern, kind ast.DeclKind, declaring bool) {
	if pat.Rest != nil {
		c.errorf(pat.Start, "rest elements in destructuring are not yet supported")
	}
	for i, el := range pat.Elements {
		if el == nil {
			continue
		}
		// Index the source, which stays on the stack for the next element.
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpPushInt, uint32(i), 0)
		c.emit(bytecode.OpGetIndex, 0, 0)
		c.bindPatternLeaf(el, kind, declaring)
	}
	c.emit(bytecode.OpDrop, 0, 0)
}

// compileObjectPattern unpacks an object pattern.
func (c *compiler) compileObjectPattern(pat *ast.ObjectPattern, kind ast.DeclKind, declaring bool) {
	if pat.Rest != nil {
		c.errorf(pat.Start, "rest elements in destructuring are not yet supported")
	}
	for _, p := range pat.Props {
		c.emit(bytecode.OpDup, 0, 0)
		if p.Computed {
			c.compileExpr(p.Key)
			c.emit(bytecode.OpGetIndex, 0, 0)
		} else {
			c.emit(bytecode.OpGetProp, c.nameIdx(propKeyName(p.Key)), 0)
		}
		c.bindPatternLeaf(p.Value, kind, declaring)
	}
	c.emit(bytecode.OpDrop, 0, 0)
}

// bindPatternLeaf stores the extracted value into one leaf of a pattern.
func (c *compiler) bindPatternLeaf(leaf ast.Expr, kind ast.DeclKind, declaring bool) {
	if ap, ok := leaf.(*ast.AssignPattern); ok {
		c.applyDefault(ap.Default, nameOf(ap.Target))
		leaf = ap.Target
	}
	switch l := leaf.(type) {
	case *ast.Ident:
		if declaring {
			c.initBinding(l, kind)
			return
		}
		c.assignTo(l, false)
		c.emit(bytecode.OpDrop, 0, 0)
	case *ast.Member:
		c.assignTo(l, false)
		c.emit(bytecode.OpDrop, 0, 0)
	case *ast.ArrayPattern:
		c.compileArrayPattern(l, kind, declaring)
	case *ast.ObjectPattern:
		c.compileObjectPattern(l, kind, declaring)
	default:
		c.errorf(leaf.Pos(), "unsupported destructuring target %T", leaf)
	}
}

// applyDefault replaces the value on the stack with a default when it is
// undefined.
func (c *compiler) applyDefault(def ast.Expr, name string) {
	c.emit(bytecode.OpDup, 0, 0)
	c.emit(bytecode.OpPushUndef, 0, 0)
	c.emit(bytecode.OpStrictEq, 0, 0)
	skip := c.emitJump(bytecode.OpJumpIfFalse)
	c.emit(bytecode.OpDrop, 0, 0)
	c.compileExprNamed(def, name)
	c.patchJump(skip)
}

// ---------------------------------------------------------------------------
// Classes
// ---------------------------------------------------------------------------

// compileClass compiles a class definition, leaving the constructor on the
// stack.
//
// A class is built out of ordinary pieces: the constructor is a function whose
// .prototype carries the methods, and the static members are properties of the
// constructor itself.
func (c *compiler) compileClass(cls *ast.ClassLit, inferredName string) {
	if cls.Extends != nil {
		c.errorf(cls.Start, "class inheritance is not yet supported")
	}
	if len(cls.Fields) > 0 {
		c.errorf(cls.Start, "class fields are not yet supported")
	}
	if len(cls.StaticBlocks) > 0 {
		c.errorf(cls.Start, "static blocks are not yet supported")
	}

	name := inferredName
	if cls.Name != nil {
		name = cls.Name.Name
	}

	// Find the constructor, or synthesize an empty one.
	var ctor *ast.FuncLit
	for _, m := range cls.Members {
		if fn, ok := m.Value.(*ast.FuncLit); ok && fn.Kind == ast.FuncConstructor {
			ctor = fn
		}
	}
	if ctor == nil {
		ctor = &ast.FuncLit{Kind: ast.FuncConstructor, Start: cls.Start}
	}
	ctorLit := *ctor
	ctorLit.Name = nil
	c.compileFunctionLiteral(&ctorLit, name)

	// Attach the methods to the constructor's prototype, and the static
	// members to the constructor itself.
	for _, m := range cls.Members {
		fn, ok := m.Value.(*ast.FuncLit)
		if !ok || fn.Kind == ast.FuncConstructor {
			continue
		}
		if m.Computed {
			c.errorf(m.Start, "computed class member names are not yet supported")
		}
		key := propKeyName(m.Key)

		if m.Static {
			// The constructor is the target, so it is duplicated for the
			// define and left in place afterwards.
			c.compileMethodValue(fn, key)
			c.emitDefineMember(m.Kind, key)
			continue
		}
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpGetProp, c.nameIdx("prototype"), 0)
		c.compileMethodValue(fn, key)
		c.emitDefineMember(m.Kind, key)
		c.emit(bytecode.OpDrop, 0, 0)
	}
}

// compileMethodValue emits a method's closure.
func (c *compiler) compileMethodValue(fn *ast.FuncLit, name string) {
	lit := *fn
	lit.Name = nil
	c.compileFunctionLiteral(&lit, name)
}

// emitDefineMember installs a class member on the object beneath it.
func (c *compiler) emitDefineMember(kind ast.PropKind, key string) {
	switch kind {
	case ast.PropGet:
		c.emit(bytecode.OpDefineGetter, c.nameIdx(key), 0)
	case ast.PropSet:
		c.emit(bytecode.OpDefineSetter, c.nameIdx(key), 0)
	default:
		c.emit(bytecode.OpDefineField, c.nameIdx(key), 0)
	}
}

// formatKeyNumber renders a numeric property key the way ToString would, so
// that {1: x} and {"1": x} name the same property.
func formatKeyNumber(v float64) string { return jsnum.FormatFloat(v) }
