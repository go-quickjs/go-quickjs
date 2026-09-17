package compiler

import (
	"fmt"
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
	case ast.FuncDerivedConstructor:
		return bytecode.KindDerivedConstructor
	}
	return bytecode.KindNormal
}

// compileFunctionBody compiles a function's parameters and body into the
// receiver, which is a fresh compiler for that function.
func (c *compiler) compileFunctionBody(fn *ast.FuncLit) {
	// Function.prototype.toString returns the source as written, so the span
	// is recorded rather than the text reconstructed -- a reconstruction would
	// lose comments, spacing and the exact parameter syntax, all of which some
	// code inspects.
	if fn.End > fn.Start && fn.End <= len(c.opts.Text) {
		c.fn.Text = c.opts.Text[fn.Start:fn.End]
	}

	// Parameters must occupy slots 0..n-1, because the interpreter copies
	// arguments into those slots positionally. Nothing may be declared before
	// them.
	c.fn.UsesThis = referencesThis(fn)

	// A function that mentions `arguments`, directly or through an arrow that
	// captures it, materializes the object into a slot. It exists before the
	// parameters are initialized, so a default may refer to it, and the slot
	// has to exist before the body is compiled, because an arrow can only
	// capture a binding that is already there.
	wantArguments := c.fn.Kind != bytecode.KindArrow &&
		(referencesArguments(fn.Body) || referencesArgumentsInParams(fn.Params))
	c.bindParameters(fn, func() {
		if !wantArguments {
			return
		}
		c.fn.UsesArguments = true
		slot := c.declare("arguments", bindVar, fn.Start)
		c.emit(bytecode.OpGetArguments, 0, 0)
		c.emit(bytecode.OpSetLocal, slot, 0)
	})
	if fn.Generator || fn.Async {
		// A generator's parameters are bound when it is called, so the
		// prologue has to be separable from the body.
		c.emit(bytecode.OpEndParams, 0, 0)
		c.fn.ParamEnd = uint32(c.here())
	}

	// A named function expression can refer to itself by name. That reference
	// resolves to the running closure rather than to a local, so no slot is
	// allocated for it: recording the name is enough for compileIdentRead to
	// emit OpPushCallee instead of a variable read.
	if fn.Name != nil {
		c.selfName = fn.Name.Name
	}

	// Hoist var declarations and nested function declarations to the top of
	// the function, as their scope requires.
	var varNames []string
	collectVarNamesIn(fn.Body, &varNames, c.fn.Strict)
	for _, n := range varNames {
		if _, exists := c.resolveLocal(n); !exists {
			slot := c.declare(n, bindVar, fn.Start)
			// A hoisted var starts as undefined.
			c.emit(bytecode.OpPushUndef, 0, 0)
			c.emit(bytecode.OpSetLocal, slot, 0)
		}
	}

	c.compileStatements(fn.Body)
	if fn.Kind == ast.FuncConstructor || fn.Kind == ast.FuncDerivedConstructor {
		// A constructor returns its `this` rather than undefined. That matters
		// for a derived class, where super() may replace `this` with the
		// object the base constructor built -- which is how `class E extends
		// Error` ends up with a real Error, and `class A extends Array` with a
		// real array.
		c.emit(bytecode.OpPushThis, 0, 0)
		c.emit(bytecode.OpReturn, 0, 0)
	}
	c.emit(bytecode.OpReturnUndef, 0, 0)
	c.finish()
}

// bindParameters declares the parameter slots and emits the prologue for
// defaults, destructuring and the rest parameter.
func (c *compiler) bindParameters(fn *ast.FuncLit, materialize func()) {
	simple := true
	for _, p := range fn.Params {
		if _, ok := p.(*ast.Ident); !ok {
			simple = false
			break
		}
	}
	c.fn.HasSimpleParams = simple
	// A default, a pattern or a rest element makes the parameters lexical: they
	// are bound one at a time, in order, and one that has not been reached yet
	// may not be read.
	c.fn.ParamsAreLexical = !simple

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

	// Every parameter takes exactly one slot, and those slots have to be
	// 0..n-1 because the interpreter fills them positionally. So they are all
	// reserved before any initializing code is emitted: a pattern parameter
	// introduces names of its own, and declaring those as it went would push
	// the parameters after it out of position.
	slots := make([]uint32, len(fn.Params))
	names := make([]string, len(fn.Params))
	declareParam := func(id *ast.Ident) uint32 {
		slot := c.declare(id.Name, bindParam, id.Start)
		if c.fn.ParamsAreLexical {
			c.markUninitialized(id.Name)
		}
		return slot
	}
	for i, p := range fn.Params {
		switch param := p.(type) {
		case *ast.Ident:
			names[i] = param.Name
			slots[i] = declareParam(param)
		case *ast.AssignPattern:
			if id, ok := param.Target.(*ast.Ident); ok {
				names[i] = id.Name
				slots[i] = declareParam(id)
				continue
			}
			slots[i] = c.anonymousParamSlot()
		case *ast.RestElement:
			if id, ok := param.Arg.(*ast.Ident); ok {
				names[i] = id.Name
				slots[i] = declareParam(id)
				continue
			}
			slots[i] = c.anonymousParamSlot()
		default:
			slots[i] = c.anonymousParamSlot()
		}
	}

	if c.fn.ParamsAreLexical {
		// The parameters are bound one at a time from here on, so they all
		// start in the dead zone. An argument that was passed is already
		// bound; undefined, passed or missing, is what makes a default run.
		c.emit(bytecode.OpParamsToDeadZone, uint32(len(fn.Params)), 0)
	}

	// The arguments object is created before the parameters are initialized,
	// so a default may refer to it.
	materialize()

	for i, p := range fn.Params {
		slot := slots[i]
		switch param := p.(type) {
		case *ast.Ident:
			// Nothing is evaluated for it: the argument arrived in the slot,
			// or the marker is still there and stands for undefined. A list
			// with no defaults and no patterns never put it there.
			if c.fn.ParamsAreLexical {
				c.emit(bytecode.OpInitParam, slot, 0)
			}

		case *ast.AssignPattern:
			// A parameter slot always exists; the default only applies when
			// the argument was undefined. The binding is not initialized until
			// the default has run, which is what makes `f(x = x)` an error.
			if !isIdent(param.Target) {
				c.declarePatternNames(param.Target)
			}
			c.emit(bytecode.OpParamNeedsDefault, slot, 0)
			passed := c.emitJump(bytecode.OpJumpIfFalse)
			c.compileExprNamed(param.Default, nameOf(param.Target))
			c.emit(bytecode.OpSetLocal, slot, 0)
			done := c.emitJump(bytecode.OpJump)
			c.patchJump(passed)
			c.emit(bytecode.OpInitParam, slot, 0)
			c.patchJump(done)
			if !isIdent(param.Target) {
				c.emit(bytecode.OpGetLocal, slot, 0)
				c.compileDestructuring(param.Target, ast.DeclLet)
			}

		case *ast.RestElement:
			// A rest parameter binds an array of whatever was passed beyond
			// the declared ones, so its slot is filled from the frame's
			// argument list rather than positionally.
			c.fn.HasRest = true
			c.emit(bytecode.OpRestParam, uint32(i), 0)
			c.emit(bytecode.OpSetLocal, slot, 0)
			if !isIdent(param.Arg) {
				c.declarePatternNames(param.Arg)
				c.emit(bytecode.OpGetLocal, slot, 0)
				c.compileDestructuring(param.Arg, ast.DeclLet)
			}

		default:
			if c.fn.ParamsAreLexical {
				c.emit(bytecode.OpInitParam, slot, 0)
			}
			c.declarePatternNames(p)
			c.emit(bytecode.OpGetLocal, slot, 0)
			c.compileDestructuring(p, ast.DeclLet)
		}
		if names[i] != "" {
			c.markInitialized(names[i])
		}
	}
	// ParamCount is how many arguments the interpreter copies positionally, so
	// it excludes a rest parameter, which is filled from the argument list.
	c.fn.ParamCount = len(fn.Params)
	if c.fn.HasRest {
		c.fn.ParamCount--
	}
	c.fn.Length = length
}

// anonymousParamSlot reserves a parameter's positional slot for a pattern,
// which has no name of its own to bind it to.
func (c *compiler) anonymousParamSlot() uint32 {
	slot := c.nextSlot
	c.nextSlot++
	c.locals = append(c.locals, localVar{
		name: "", kind: bindParam, slot: slot, depth: c.depth, initialized: true,
	})
	return slot
}

// declarePatternNames declares the bindings a parameter pattern introduces,
// which the destructuring code then initializes.
func (c *compiler) declarePatternNames(target ast.Expr) {
	var names []string
	collectPatternNames(target, &names)
	for _, n := range names {
		c.declare(n, bindLet, target.Pos())
		c.markInitialized(n)
	}
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
	// An array pattern unpacks through the iterator protocol, not through
	// indexed access, so that `const [a] = new Set([1])` works and so that a
	// generator only produces as many values as the pattern names. The source
	// is drained into a dense array first, which keeps the unpacking below
	// simple and makes holes and defaults fall out naturally.
	want := uint32(len(pat.Elements))
	if pat.Rest != nil {
		want = bytecode.IterAll
	}
	c.emit(bytecode.OpIterToArray, want, 0)

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
	if pat.Rest != nil {
		// The rest element takes everything from the first index the named
		// elements did not consume.
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpArrayRest, uint32(len(pat.Elements)), 0)
		c.bindPatternLeaf(pat.Rest, kind, declaring)
	}
	c.emit(bytecode.OpDrop, 0, 0)
}

// compileObjectPattern unpacks an object pattern.
func (c *compiler) compileObjectPattern(pat *ast.ObjectPattern, kind ast.DeclKind, declaring bool) {
	// The source has to be something properties can be read from, and that is
	// checked before any of them are -- which is the only thing an empty
	// pattern does, and why `var {} = null` is an error at all.
	c.emit(bytecode.OpCheckCoercible, 0, 0)
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
	if pat.Rest != nil {
		// The rest object holds every own enumerable property except the ones
		// the pattern already bound, so those keys are pushed for the
		// instruction to exclude.
		c.emit(bytecode.OpDup, 0, 0)
		for _, p := range pat.Props {
			if p.Computed {
				c.compileExpr(p.Key)
				c.emit(bytecode.OpToPropertyKey, 0, 0)
				continue
			}
			c.emit(bytecode.OpPushConst, c.stringConst(propKeyName(p.Key)), 0)
		}
		c.emit(bytecode.OpObjectRest, uint32(len(pat.Props)), 0)
		c.bindPatternLeaf(pat.Rest, kind, declaring)
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
// A class is assembled from ordinary parts: the constructor is a function whose
// .prototype carries the methods, static members are properties of the
// constructor itself, and instance fields are compiled into the top of the
// constructor body. Inheritance links both chains -- prototypes for instance
// members, constructors for static ones -- which is what makes a static method
// visible on a subclass.
func (c *compiler) compileClass(cls *ast.ClassLit, inferredName string) {
	name := inferredName
	if cls.Name != nil {
		name = cls.Name.Name
	}

	// A computed field key is evaluated once, when the class is defined, not
	// once per instance -- `class C { [log()] = 1 }` calls log once however
	// many instances are made. The key is stashed in a hidden binding of the
	// enclosing scope, which the constructor then reads as an upvalue; the
	// names begin with a character no identifier may contain, so nothing a
	// script writes can collide with one.
	keyNames := make([]string, len(cls.Fields))
	for i, f := range cls.Fields {
		if !f.Computed {
			continue
		}
		keyNames[i] = fmt.Sprintf("%%key%d", *c.hiddenCount)
		*c.hiddenCount++
	}

	// The private names are visible throughout the body, including to a method
	// written above the field it reads, so they are all collected before any of
	// it is compiled. Each gets a hidden binding to hold the key this
	// evaluation of the class mints for it.
	privates := collectPrivateNames(cls)
	c.nameHiddenBindings(privates)
	installName := ""
	for _, b := range privates {
		if b.installed() {
			installName = fmt.Sprintf("%%pm%d", *c.hiddenCount)
			*c.hiddenCount++
			break
		}
	}

	ctor := c.synthesizeConstructor(cls, keyNames, installName)
	if cls.Extends != nil {
		// The parent is evaluated before the constructor is built, as the
		// heritage clause is an expression that may have side effects -- and
		// before the class's own private names come into scope, since the
		// heritage clause is outside the body that declares them.
		c.compileExpr(cls.Extends)
	}

	// The body is a scope of its own, holding the class's inner name binding,
	// its computed keys and its private names. Opening it per evaluation is
	// what keeps two evaluations of the same class apart: each closes its
	// bindings on the way out, so the methods of one do not share a cell with
	// the methods of another.
	c.beginScope()
	c.pushPrivateScope(privates)
	defer c.popPrivateScope()
	c.emitPrivateKeys(privates, cls.Start)
	if installName != "" {
		// The list is created before the constructor is compiled and filled as
		// the body is evaluated, so that a static block that constructs an
		// instance finds the methods already there.
		slot := c.declare(installName, bindConst, cls.Start)
		c.emit(bytecode.OpNewPrivateMethods, 0, 0)
		c.emit(bytecode.OpSetLocal, slot, 0)
		c.markInitialized(installName)
	}

	c.evalComputedFieldKeys(cls, keyNames)
	c.compileFunctionLiteral(ctor, name)
	if cls.Extends != nil {
		c.emit(bytecode.OpSwap, 0, 0)
		// stack: ctor parent
		c.emitAt(cls.Start, bytecode.OpNewClass, 0, 0)
	}

	// A class has an inner binding for its own name, in scope throughout the
	// body. It is what lets a static block or a method refer to the class
	// before the outer binding is initialized, and it is a separate, immutable
	// binding that shadows the outer one.
	if name != "" {
		c.emit(bytecode.OpDup, 0, 0)
		slot := c.declare(name, bindConst, cls.Start)
		c.emit(bytecode.OpSetLocal, slot, 0)
		c.markInitialized(name)
	}

	for _, m := range cls.Members {
		fn, ok := m.Value.(*ast.FuncLit)
		if !ok || fn.Kind == ast.FuncConstructor {
			continue
		}
		c.compileClassMember(m, fn, installName)
	}

	// Static fields are assigned after the class object exists, with the
	// constructor as `this`.
	for i, f := range cls.Fields {
		if !f.Static {
			continue
		}
		c.emit(bytecode.OpDup, 0, 0)
		if f.Computed {
			// The key was evaluated when the class was defined; only the value
			// is produced here.
			c.compileIdentRead(&ast.Ident{Name: keyNames[i], Start: f.Start})
		}
		switch {
		case f.Value == nil:
			c.emit(bytecode.OpPushUndef, 0, 0)
		case needsHomeObject(f.Value):
			// A static initializer that mentions super is an immediately
			// invoked method of the class, which is what gives it a home
			// object to resolve super against. Only one that needs it pays for
			// the call.
			c.emit(bytecode.OpDup, 0, 0)
			c.compileFunctionLiteral(&ast.FuncLit{
				Kind:  ast.FuncMethod,
				Body:  []ast.Stmt{&ast.ReturnStmt{Arg: f.Value, Start: f.Start}},
				Start: f.Start,
				End:   f.Start,
			}, "")
			c.emit(bytecode.OpSetHomeObject, 1, 0)
			c.emit(bytecode.OpCallMethod, 0, 0)
		default:
			c.compileExprNamed(f.Value, classFieldName(f.Key, f.Computed))
		}
		switch {
		case f.Computed:
			c.emit(bytecode.OpDefineIndex, 0, 0)
		default:
			if pn, private := f.Key.(*ast.PrivateName); private {
				// A private field is hidden from every reflective operation,
				// static or not, which the define instruction records rather
				// than the attributes.
				name, ref := c.privateName(pn, f.Start)
				c.emit(bytecode.OpDefinePrivate, name, ref)
			} else {
				c.emit(bytecode.OpDefineField, c.nameIdx(propKeyName(f.Key)), 0)
			}
		}
		c.emit(bytecode.OpDrop, 0, 0)
	}

	for _, block := range cls.StaticBlocks {
		// A static block is an immediately invoked method whose `this` is the
		// class. OpCallMethod takes the receiver from beneath the callee, so
		// the constructor is duplicated into that slot.
		c.emit(bytecode.OpDup, 0, 0)
		c.compileFunctionLiteral(&ast.FuncLit{
			Kind:  ast.FuncMethod,
			Body:  block,
			Start: cls.Start,
		}, "")
		// Its home object is the class, so `super.x` there reads from the
		// parent class rather than from the parent's prototype.
		c.emit(bytecode.OpSetHomeObject, 1, 0)
		c.emit(bytecode.OpCallMethod, 0, 0)
		c.emit(bytecode.OpDrop, 0, 0)
	}
	c.endScope()
}

// compileClassMember attaches one method or accessor to the class.
func (c *compiler) compileClassMember(m ast.Property, fn *ast.FuncLit, installName string) {
	key := ""
	if !m.Computed {
		key = propKeyName(m.Key)
	}
	// An accessor is named after its property with a "get " or "set " prefix.
	nameKind := uint32(0)
	methodName := key
	switch m.Kind {
	case ast.PropGet:
		nameKind, methodName = 1, "get "+key
	case ast.PropSet:
		nameKind, methodName = 2, "set "+key
	}

	if pn, private := m.Key.(*ast.PrivateName); private && !m.Static {
		c.compilePrivateMethod(m, fn, pn, methodName, installName)
		return
	}

	if m.Static {
		// The constructor is the target and stays on the stack.
		c.emit(bytecode.OpDup, 0, 0)
	} else {
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpGetProp, c.nameIdx("prototype"), 0)
	}
	// A computed key is evaluated before the method it names, which is
	// observable when it has a side effect, and leaves the stack in the
	// [target, key, value] order the define instructions expect.
	homeDepth := uint32(1)
	if m.Computed {
		c.compileExpr(m.Key)
		c.emit(bytecode.OpToPropertyKey, 0, 0)
		// The key now sits between the target and the function.
		homeDepth = 2
	}
	c.compileMethodValue(fn, methodName)
	if m.Computed {
		// The name is only knowable once the key has been evaluated.
		c.emit(bytecode.OpSetFuncName, nameKind, 0)
	}
	// The home object is what `super` resolves against, so a method has to
	// remember the object it was defined on.
	c.emit(bytecode.OpSetHomeObject, homeDepth, 0)
	c.emitClassMemberDefine(m, key)
	c.emit(bytecode.OpDrop, 0, 0)
}

// emitClassMemberDefine installs a member whose value is on the stack above
// its target.
func (c *compiler) emitClassMemberDefine(m ast.Property, key string) {
	if m.Computed {
		// The key is already on the stack, beneath the value.
		switch m.Kind {
		case ast.PropGet:
			c.emit(bytecode.OpDefineGetterIndex, 0, 0)
		case ast.PropSet:
			c.emit(bytecode.OpDefineSetterIndex, 0, 0)
		default:
			c.emit(bytecode.OpDefineIndex, 0, 0)
		}
		return
	}
	// A class method is not enumerable, unlike an object literal's, and a
	// private one is additionally hidden from every reflective operation. Only
	// a static private member reaches here; an instance one belongs to the
	// instance and is handled above.
	if pn, private := m.Key.(*ast.PrivateName); private {
		name, ref := c.privateName(pn, m.Start)
		switch m.Kind {
		case ast.PropGet:
			c.emit(bytecode.OpDefinePrivateGetter, name, ref)
		case ast.PropSet:
			c.emit(bytecode.OpDefinePrivateSetter, name, ref)
		default:
			c.emit(bytecode.OpDefinePrivate, name, ref)
		}
		return
	}
	switch m.Kind {
	case ast.PropGet:
		c.emit(bytecode.OpDefineGetter, c.nameIdx(key), 0)
	case ast.PropSet:
		c.emit(bytecode.OpDefineSetter, c.nameIdx(key), 0)
	default:
		c.emit(bytecode.OpDefineMethod, c.nameIdx(key), 0)
	}
}

// compilePrivateMethod adds one private instance method or accessor to the list
// each instance of the class is given.
//
// It is not defined on the prototype: an object that merely inherits from the
// prototype is not an instance, and asking it for a private member has to fail.
// The function is made once per class evaluation and shared by every instance,
// so it is built here rather than in the constructor.
func (c *compiler) compilePrivateMethod(m ast.Property, fn *ast.FuncLit,
	pn *ast.PrivateName, methodName, installName string) {
	name, ref := c.privateName(pn, m.Start)

	// `super.x` in the method resolves against the prototype's prototype, so
	// the prototype is its home object even though it is not defined there.
	c.emit(bytecode.OpDup, 0, 0)
	c.emit(bytecode.OpGetProp, c.nameIdx("prototype"), 0)
	c.compileIdentRead(&ast.Ident{Name: installName, Start: m.Start})
	c.compileMethodValue(fn, methodName)
	c.emit(bytecode.OpSetHomeObject, 2, 0)
	op := bytecode.OpAddPrivateMethod
	switch m.Kind {
	case ast.PropGet:
		op = bytecode.OpAddPrivateGetter
	case ast.PropSet:
		op = bytecode.OpAddPrivateSetter
	}
	c.emit(op, name, ref)
	c.emit(bytecode.OpDrop, 0, 0)
}

// synthesizeConstructor builds the function that `new` will call, prefixing the
// instance field initializers to whatever body the class declared.
//
// Compiling fields as statements rather than as separate initializer functions
// means they see the constructor's scope and `this` for free.
// evalComputedFieldKeys evaluates each computed field key and stores it in its
// hidden binding, leaving the operand stack as it found it.
func (c *compiler) evalComputedFieldKeys(cls *ast.ClassLit, keyNames []string) {
	for i, f := range cls.Fields {
		if !f.Computed {
			continue
		}
		slot := c.declare(keyNames[i], bindConst, f.Start)
		c.compileExpr(f.Key)
		c.emit(bytecode.OpToPropertyKey, 0, 0)
		c.emit(bytecode.OpSetLocal, slot, 0)
		c.markInitialized(keyNames[i])
	}
}

func (c *compiler) synthesizeConstructor(cls *ast.ClassLit, keyNames []string,
	installName string) *ast.FuncLit {
	var declared *ast.FuncLit
	for _, m := range cls.Members {
		if fn, ok := m.Value.(*ast.FuncLit); ok && fn.Kind == ast.FuncConstructor {
			declared = fn
		}
	}

	var fieldInit []ast.Stmt
	if installName != "" {
		// A private method belongs to the instance, and is put there where the
		// specification puts it: once `this` exists, before any field runs.
		fieldInit = append(fieldInit, &ast.InstallPrivateMethods{
			Binding: installName, Start: cls.Start,
		})
	}
	for i, f := range cls.Fields {
		if f.Static {
			continue
		}
		key, computed := f.Key, f.Computed
		if computed {
			// Read the key the class definition already computed rather than
			// evaluating the expression again for every instance.
			key = &ast.Ident{Name: keyNames[i], Start: f.Start}
		}
		value := f.Value
		if value == nil {
			// A field with no initializer is still created, holding undefined.
			value = &ast.Ident{Name: "undefined", Start: f.Start}
		}
		fieldInit = append(fieldInit, &ast.FieldInit{
			Key:      key,
			Value:    value,
			Computed: computed,
			Start:    f.Start,
		})
	}

	if declared == nil {
		// A class with no explicit constructor still has one. A derived class
		// forwards its arguments to the parent, which is what the implicit
		// `constructor(...args) { super(...args); }` does.
		body := fieldInit
		if cls.Extends != nil {
			body = append([]ast.Stmt{implicitSuperCall(cls.Start)}, body...)
		}
		// The synthesized constructor stands in for the class as a whole, so
		// its source span is the class's: `C.toString()` is the class text.
		return &ast.FuncLit{
			Kind: constructorKind(cls), Body: body,
			Start: cls.Start, End: cls.End,
		}
	}

	lit := *declared
	lit.Name = nil
	lit.Kind = constructorKind(cls)
	lit.Start, lit.End = cls.Start, cls.End
	if len(fieldInit) > 0 {
		// Fields are initialized before the constructor body runs. In a derived
		// class they must follow super(), which the body itself calls, so they
		// are placed after the first statement when that statement is a super
		// call.
		if cls.Extends != nil && startsWithSuperCall(lit.Body) {
			merged := append([]ast.Stmt{lit.Body[0]}, fieldInit...)
			lit.Body = append(merged, lit.Body[1:]...)
		} else {
			lit.Body = append(append([]ast.Stmt{}, fieldInit...), lit.Body...)
		}
	}
	return &lit
}

// constructorKind says whether a class's constructor is a derived one, which
// decides what it may return and when its `this` is bound.
func constructorKind(cls *ast.ClassLit) ast.FuncKind {
	if cls.Extends != nil {
		return ast.FuncDerivedConstructor
	}
	return ast.FuncConstructor
}

// implicitSuperCall builds `super(...arguments)` for a derived class that
// declares no constructor.
func implicitSuperCall(pos int) ast.Stmt {
	return &ast.ExprStmt{
		X: &ast.Call{
			Callee: &ast.Super{Start: pos},
			Args: []ast.Expr{&ast.Spread{
				Arg:   &ast.Ident{Name: "arguments", Start: pos},
				Start: pos,
			}},
			Start: pos,
		},
		Start: pos,
	}
}

// startsWithSuperCall reports whether a constructor body begins with super().
func startsWithSuperCall(body []ast.Stmt) bool {
	if len(body) == 0 {
		return false
	}
	es, ok := body[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.Call)
	if !ok {
		return false
	}
	_, isSuper := call.Callee.(*ast.Super)
	return isSuper
}

// classFieldName returns the name to infer for an anonymous function assigned
// to a class field.
func classFieldName(key ast.Expr, computed bool) string {
	if computed {
		return ""
	}
	return propKeyName(key)
}

// compileMethodValue emits a method's closure.
func (c *compiler) compileMethodValue(fn *ast.FuncLit, name string) {
	lit := *fn
	lit.Name = nil
	c.compileFunctionLiteral(&lit, name)
}

// formatKeyNumber renders a numeric property key the way ToString would, so
// that {1: x} and {"1": x} name the same property.
func formatKeyNumber(v float64) string { return jsnum.FormatFloat(v) }
