package compiler

import (
	"fmt"
	"sort"

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

	// A direct eval in the body may declare a var here, and it would belong to
	// this function. The answer is needed before anything is compiled: a frame
	// needs somewhere to put such a binding, and every reference to a name
	// this function does not bind has to be compiled to look there first.
	c.fn.HasDirectEval = !c.fn.Strict && !c.varScopeIsGlobal() &&
		(containsDirectEval(fn.Body) || containsDirectEvalInParams(fn.Params))
	if c.fn.HasDirectEval {
		// Those bindings are reached the way a `with` object's properties are:
		// they have no slot, they shadow whatever the name meant outside the
		// function, and whether they are there at all is only known when the
		// name is evaluated. Counting the frame's own as a scope is what makes
		// every reference to a name this function does not bind probe it --
		// and what leaves the names it does bind alone, since they are
		// declared inside it.
		c.withDepth++
	}

	// A function that mentions `arguments`, directly or through an arrow that
	// captures it, materializes the object into a slot. It exists before the
	// parameters are initialized, so a default may refer to it, and the slot
	// has to exist before the body is compiled, because an arrow can only
	// capture a binding that is already there.
	wantArguments := c.fn.Kind != bytecode.KindArrow &&
		(referencesArguments(fn.Body) || referencesArgumentsInParams(fn.Params)) &&
		!bindsArguments(fn.Params) &&
		!(simpleParams(fn.Params) && declaresArguments(fn.Body))
	// A constructible function's object is made with room for the properties
	// the body assigns to `this`, so that a constructor of three fields does
	// not grow its table three times. Nothing else builds an object, so nothing
	// else pays for the walk.
	switch c.fn.Kind {
	case bytecode.KindNormal, bytecode.KindConstructor, bytecode.KindDerivedConstructor:
		c.fn.ThisProps = uint8(thisPropertyCount(fn.Body))
	}

	// A named function expression can refer to itself by name.
	if fn.Name != nil {
		c.selfName = fn.Name.Name
	}

	c.bindParameters(fn, func() {
		// The binding for the function's own name encloses the parameter
		// scope, so a default may refer to it -- and it is made here, after
		// the parameter slots are reserved, because those have to come first.
		defer c.bindSelfName(fn)
		if !wantArguments {
			return
		}
		c.fn.UsesArguments = true
		// A sloppy function with plain parameters gets the mapped arguments
		// object, whose indices alias the parameters. Anything more elaborate
		// than plain parameters, or strict mode, gets the snapshot instead.
		c.fn.MappedArguments = !c.fn.Strict && c.fn.HasSimpleParams
		slot := c.declare("arguments", bindVar, fn.Start)
		if !c.fn.HasSimpleParams {
			// With parameter expressions the arguments object belongs to the
			// parameter scope, which the body may shadow.
			c.locals[len(c.locals)-1].paramScoped = true
		}
		c.emit(bytecode.OpGetArguments, 0, 0)
		c.emit(bytecode.OpSetLocal, slot, 0)
	}, wantArguments)
	if fn.Generator || fn.Async {
		// A generator's parameters are bound when it is called, so the
		// prologue has to be separable from the body.
		c.emit(bytecode.OpEndParams, 0, 0)
		c.fn.ParamEnd = uint32(c.here())
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

// bindSelfName gives a named function expression a binding for its own name.
//
// The slot is made only when something nested could need it: a reference in the
// function's own body is answered from the running closure, but a nested
// function can capture nothing else. A parameter or a body-level binding of the
// same name shadows it, in which case there is nothing left for the name to
// reach.
func (c *compiler) bindSelfName(fn *ast.FuncLit) {
	if c.selfName == "" {
		return
	}
	if _, shadowed := c.resolveLocal(c.selfName); shadowed {
		return
	}
	for _, n := range topLevelLexicalNames(fn.Body) {
		if n == c.selfName {
			return
		}
	}
	// A var of the same name is a binding of the function's own scope, and it
	// shadows the name the function was written with -- `function n() { var n }`
	// reads undefined, not itself.
	var varNames []string
	collectVarNamesIn(fn.Body, &varNames, c.fn.Strict)
	for _, n := range varNames {
		if n == c.selfName {
			return
		}
	}
	if !referencesName(fn, c.selfName) {
		return
	}
	slot := c.declare(c.selfName, bindFuncSelf, fn.Start)
	c.emit(bytecode.OpPushCallee, 0, 0)
	c.emit(bytecode.OpInitLocal, slot, 0)
	c.markInitialized(c.selfName)
}

// bindParameters declares the parameter slots and emits the prologue for
// defaults, destructuring and the rest parameter.
func (c *compiler) bindParameters(fn *ast.FuncLit, materialize func(), wantArgs bool) {
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

	// A direct eval in one of the defaults below runs in the parameter scope,
	// which binds these names.
	if !c.fn.Strict {
		saved := c.paramScopeNames
		c.paramScopeNames = append([]string(nil), names...)
		if wantArgs {
			c.paramScopeNames = append(c.paramScopeNames, "arguments")
		}
		defer func() { c.paramScopeNames = saved }()
	}

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
//
// An array pattern unpacks through the iterator protocol, not through indexed
// access, so that `const [a] = new Set([1])` works and so that a generator
// produces only as many values as the pattern names. The cursor stays on the
// operand stack for the whole pattern: that is what makes an abrupt exit close
// it, by the same rule that closes a for-of's, and what keeps each element's
// step interleaved with the target that receives it -- which is observable,
// since evaluating a target can run a getter or a generator's yield.
func (c *compiler) compileArrayPattern(pat *ast.ArrayPattern, kind ast.DeclKind, declaring bool) {
	c.emitAt(pat.Start, bytecode.OpForOfStart, 0, 0)

	for _, el := range pat.Elements {
		if el == nil {
			// A hole asks for a value and throws it away.
			c.emit(bytecode.OpIterStep, 0, 0)
			c.emit(bytecode.OpDrop, 0, 0)
			continue
		}
		ref := c.prepareRef(el, declaring, kind)
		c.emit(bytecode.OpIterStep, uint32(ref.slots), 0)
		if ref.def != nil {
			c.applyDefault(ref.def, nameOf(ref.target))
		}
		c.storeRef(ref, kind, declaring)
	}

	if pat.Rest != nil {
		ref := c.prepareRef(pat.Rest, declaring, kind)
		c.emit(bytecode.OpIterRest, uint32(ref.slots), 0)
		c.storeRef(ref, kind, declaring)
	}

	// The pattern has what it needs, so an iterator it stopped short of is told
	// so -- and unlike a close during an abrupt completion, a failure there is
	// the result.
	c.emit(bytecode.OpIterCloseNormal, 0, 0)
}

// patternRef is one leaf of a destructuring pattern, with whatever part of its
// target had to be evaluated before the value arrives.
//
// A target that is a property access is evaluated first: `[a.b] = it` reads a
// before it asks the iterator for anything, which a getter on the object it
// comes from can see.
type patternRef struct {
	target ast.Expr
	// def is the leaf's default value, applied when the value is undefined.
	def ast.Expr
	// slots is how many stack slots the reference occupies, which is how far
	// below the top the pattern's own cursor has moved.
	slots int
	// member is the property access the reference belongs to, when it is one.
	member *ast.Member
	// private marks a member whose key is a private name.
	private bool
	// withRef marks a name resolved against the `with` objects in scope
	// before the value was read, which is observable: the objects are asked
	// whether they have the name, and asked in that order.
	withRef *ast.Ident
}

// prepareRef evaluates the part of a target that comes before the value.
func (c *compiler) prepareRef(leaf ast.Expr, declaring bool, kind ast.DeclKind) patternRef {
	ref := patternRef{target: leaf}
	if ap, ok := leaf.(*ast.AssignPattern); ok {
		ref.target, ref.def = ap.Target, ap.Default
	}
	if id, ok := ref.target.(*ast.Ident); ok {
		// A name is resolved before the value it receives is read, which
		// nothing can see unless a `with` object is asked about it.
		if !declaring || kind == ast.DeclVar {
			if c.withLimit(id.Name) > 0 {
				c.resolveWithRef(id)
				ref.withRef, ref.slots = id, 1
			}
		}
		return ref
	}
	m, ok := ref.target.(*ast.Member)
	if !ok || declaring {
		return ref
	}
	if _, isSuper := m.Object.(*ast.Super); isSuper {
		// super.x is resolved against the home object rather than a value on
		// the stack, so there is nothing to evaluate ahead of time.
		return ref
	}
	ref.member = m
	if pn, isPrivate := m.Property.(*ast.PrivateName); isPrivate {
		c.checkPrivateName(pn, m.Start)
		c.compileExpr(m.Object)
		ref.private, ref.slots = true, 1
		return ref
	}
	c.compileExpr(m.Object)
	ref.slots = 1
	if m.Computed {
		// The key expression is evaluated here; converting it to a property
		// key waits until the store, which is where the specification puts it.
		c.compileExpr(m.Property)
		ref.slots = 2
	}
	return ref
}

// storeRef puts the value on top of the stack into a pattern's leaf, consuming
// both it and whatever prepareRef pushed.
func (c *compiler) storeRef(ref patternRef, kind ast.DeclKind, declaring bool) {
	if ref.member != nil {
		switch {
		case ref.private:
			pn := ref.member.Property.(*ast.PrivateName)
			name, key := c.privateName(pn, ref.member.Start)
			c.emitAt(ref.member.Start, bytecode.OpSetPrivate, name, key)
		case ref.member.Computed:
			c.emitAt(ref.member.Start, bytecode.OpSetIndex, 0, 0)
		default:
			c.emitAt(ref.member.Start, bytecode.OpSetProp,
				c.nameIdx(propKeyName(ref.member.Property)), 0)
		}
		return
	}
	switch l := ref.target.(type) {
	case *ast.Ident:
		if ref.withRef != nil {
			// The reference was settled before the read; the write goes to
			// whatever it found.
			c.endWithRef(ref.withRef)
			c.emit(bytecode.OpDrop, 0, 0)
			return
		}
		if declaring {
			c.initBinding(l, kind)
			return
		}
		c.assignTo(l, false)
		c.emit(bytecode.OpDrop, 0, 0)
	case *ast.Member:
		// A super property, which resolves against the home object.
		c.assignTo(l, false)
		c.emit(bytecode.OpDrop, 0, 0)
	case *ast.ArrayPattern:
		c.compileArrayPattern(l, kind, declaring)
	case *ast.ObjectPattern:
		c.compileObjectPattern(l, kind, declaring)
	default:
		c.errorf(ref.target.Pos(), "unsupported destructuring target %T", ref.target)
	}
}

// compileObjectPattern unpacks an object pattern.
//
// Each property's target is evaluated before the property is read, which a
// getter on the source can see: `({a: obj[key()]} = src)` calls key before it
// reads src.a.
func (c *compiler) compileObjectPattern(pat *ast.ObjectPattern, kind ast.DeclKind, declaring bool) {
	// The source has to be something properties can be read from, and that is
	// checked before any of them are -- which is the only thing an empty
	// pattern does, and why `var {} = null` is an error at all.
	c.emit(bytecode.OpCheckCoercible, 0, 0)
	for _, p := range pat.Props {
		if p.Computed {
			// The source key is evaluated and converted first, before the
			// target it will be read into.
			c.compileExpr(p.Key)
			c.emit(bytecode.OpToPropertyKey, 0, 0)
			ref := c.prepareRef(p.Value, declaring, kind)
			c.emit(bytecode.OpGetIndexUnder, uint32(ref.slots), 0)
			if ref.def != nil {
				c.applyDefault(ref.def, nameOf(ref.target))
			}
			c.storeRef(ref, kind, declaring)
			// The key has done its work.
			c.emit(bytecode.OpDrop, 0, 0)
			continue
		}
		ref := c.prepareRef(p.Value, declaring, kind)
		c.emit(bytecode.OpGetPropUnder, c.nameIdx(propKeyName(p.Key)), uint32(ref.slots))
		if ref.def != nil {
			c.applyDefault(ref.def, nameOf(ref.target))
		}
		c.storeRef(ref, kind, declaring)
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

	ctor := c.synthesizeConstructor(cls)
	instanceInit := c.instanceInitializer(cls, keyNames, installName)

	// The body is a scope of its own, holding the class's inner name binding,
	// its computed keys and its private names. Opening it per evaluation is
	// what keeps two evaluations of the same class apart: each closes its
	// bindings on the way out, so the methods of one do not share a cell with
	// the methods of another.
	c.beginScope()

	// A class written with a name has an inner binding for it, in scope
	// throughout the body and the heritage clause. It is immutable, and it is
	// in its dead zone until the class exists: `class x extends x {}` is a
	// ReferenceError rather than a lookup of whatever x is outside. A name the
	// class only inferred -- `var C = class {}` -- binds nothing.
	selfSlot, hasSelf := uint32(0), cls.Name != nil
	if hasSelf {
		selfSlot = c.declare(cls.Name.Name, bindConst, cls.Start)
		c.emit(bytecode.OpPushUninitialized, 0, 0)
		c.emit(bytecode.OpSetLocal, selfSlot, 0)
		c.markUninitialized(cls.Name.Name)
	}

	if cls.Extends != nil {
		// The parent is evaluated before the constructor is built, as the
		// heritage clause is an expression that may have side effects -- and
		// before the class's own private names come into scope, since the
		// heritage clause is outside the body that declares them.
		c.compileExpr(cls.Extends)
	}

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

	// The class now exists, which is what takes its own name out of the dead
	// zone. That binding is what lets a static block or a method refer to the
	// class before the outer binding is initialized, and it shadows the outer
	// one rather than being it: assigning to it is a TypeError.
	if hasSelf {
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpInitLocal, selfSlot, 0)
		c.markInitialized(cls.Name.Name)
	}

	if instanceInit != nil {
		// The initializer's home object is the prototype, so `super.x` in a
		// field reads from the parent's prototype, as it does in a method.
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpGetProp, c.nameIdx("prototype"), 0)
		c.compileFunctionLiteral(instanceInit, "")
		c.emit(bytecode.OpSetHomeObject, 1, 0)
		c.emit(bytecode.OpSwap, 0, 0)
		c.emit(bytecode.OpDrop, 0, 0)
		c.emit(bytecode.OpSetFieldInit, 0, 0)
	}

	for _, m := range cls.Members {
		fn, ok := m.Value.(*ast.FuncLit)
		if !ok || fn.Kind == ast.FuncConstructor {
			continue
		}
		c.compileClassMember(m, fn, installName)
	}

	// The static elements run once the class object exists, in the order they
	// were written: a field and a block are the same kind of thing here, and a
	// block between two fields runs between them.
	c.compileStaticElements(cls, keyNames)
	c.endScope()
}

// compileStaticElements runs a class's static fields and static blocks, in
// source order.
//
// Each is an immediately invoked method of the class. That is what makes `this`
// the constructor -- an initializer may read one static field to compute the
// next -- and what gives it a home object for `super.x`. OpCallMethod takes the
// receiver from beneath the callee, so the constructor is duplicated into that
// slot each time.
func (c *compiler) compileStaticElements(cls *ast.ClassLit, keyNames []string) {
	type element struct {
		pos   int
		field int // index into cls.Fields, or -1 for a block
		block int // index into cls.StaticBlocks
	}
	var elements []element
	for i, f := range cls.Fields {
		if f.Static {
			elements = append(elements, element{pos: f.Start, field: i})
		}
	}
	for i, b := range cls.StaticBlocks {
		elements = append(elements, element{pos: b.Start, field: -1, block: i})
	}
	sort.SliceStable(elements, func(i, j int) bool {
		return elements[i].pos < elements[j].pos
	})

	for _, el := range elements {
		c.emit(bytecode.OpDup, 0, 0)
		if el.field < 0 {
			c.compileFunctionLiteral(&ast.FuncLit{
				Kind:  ast.FuncMethod,
				Body:  cls.StaticBlocks[el.block].Body,
				Start: cls.Start,
			}, "")
			// Its home object is the class, so `super.x` there reads from the
			// parent class rather than from the parent's prototype.
			c.emit(bytecode.OpSetHomeObject, 1, 0)
			c.emit(bytecode.OpCallMethod, 0, 0)
			c.emit(bytecode.OpDrop, 0, 0)
			continue
		}
		f := cls.Fields[el.field]
		key, computed := f.Key, f.Computed
		if computed {
			// Read the key the class definition already computed rather than
			// evaluating the expression a second time.
			key = &ast.Ident{Name: keyNames[el.field], Start: f.Start}
		}
		value := f.Value
		if value == nil {
			// A field with no initializer is still created, holding undefined.
			value = &ast.Ident{Name: "undefined", Start: f.Start}
		}
		c.compileFunctionLiteral(&ast.FuncLit{
			Kind: ast.FuncMethod,
			Body: []ast.Stmt{&ast.FieldInit{
				Key: key, Value: value, Computed: computed, Start: f.Start,
			}},
			Start: f.Start,
			End:   f.Start,
		}, "")
		c.emit(bytecode.OpSetHomeObject, 1, 0)
		c.emit(bytecode.OpCallMethod, 0, 0)
		c.emit(bytecode.OpDrop, 0, 0)
	}
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

// classMember marks a define instruction as a class's, which the interpreter
// reads as "not enumerable". An object literal's members are enumerable and a
// class's are not, and the two share these instructions.
const classMember = 1

// emitClassMemberDefine installs a member whose value is on the stack above
// its target.
func (c *compiler) emitClassMemberDefine(m ast.Property, key string) {
	if m.Computed {
		// The key is already on the stack, beneath the value. The operand marks
		// the member as a class's rather than an object literal's, which is what
		// makes it non-enumerable.
		switch m.Kind {
		case ast.PropGet:
			c.emit(bytecode.OpDefineGetterIndex, classMember, 0)
		case ast.PropSet:
			c.emit(bytecode.OpDefineSetterIndex, classMember, 0)
		default:
			c.emit(bytecode.OpDefineIndex, classMember, 0)
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
			c.emit(bytecode.OpDefinePrivateMethod, name, ref)
		}
		return
	}
	switch m.Kind {
	case ast.PropGet:
		c.emit(bytecode.OpDefineGetter, c.nameIdx(key), classMember)
	case ast.PropSet:
		c.emit(bytecode.OpDefineSetter, c.nameIdx(key), classMember)
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

// instanceInitializer builds the function that gives a new instance what the
// class body gives it: its private methods, then its fields, in the order they
// were written. It returns nil for a class that has neither.
//
// It is a function of its own rather than a prefix of the constructor because
// of when it has to run. A base class runs it before the constructor body, but
// a derived one runs it inside super(), which may be written anywhere in the
// body -- in a branch, in a loop, inside an arrow -- and only the call that
// binds `this` runs it, so a second super() adds no second set of fields.
// Making it a function also keeps the constructor's `arguments` and new.target
// out of the initializers, which are not theirs to see.
func (c *compiler) instanceInitializer(cls *ast.ClassLit, keyNames []string,
	installName string) *ast.FuncLit {
	var body []ast.Stmt
	if installName != "" {
		// A private method belongs to the instance, and is put there where the
		// specification puts it: once `this` exists, before any field runs.
		body = append(body, &ast.InstallPrivateMethods{
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
		body = append(body, &ast.FieldInit{
			Key:      key,
			Value:    value,
			Computed: computed,
			Start:    f.Start,
		})
	}
	if len(body) == 0 {
		return nil
	}
	return &ast.FuncLit{
		Kind: ast.FuncMethod, Body: body, Start: cls.Start, End: cls.Start,
	}
}

// synthesizeConstructor builds the function that `new` will call: the one the
// class declared, or the one a class without a constructor is given.
func (c *compiler) synthesizeConstructor(cls *ast.ClassLit) *ast.FuncLit {
	var declared *ast.FuncLit
	for _, m := range cls.Members {
		if fn, ok := m.Value.(*ast.FuncLit); ok && fn.Kind == ast.FuncConstructor {
			declared = fn
		}
	}

	if declared == nil {
		// A class with no explicit constructor still has one. A derived class
		// forwards its arguments to the parent, which is what the implicit
		// `constructor(...args) { super(...args); }` does.
		var body []ast.Stmt
		if cls.Extends != nil {
			body = []ast.Stmt{implicitSuperCall(cls.Start)}
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

// topLevelLexicalNames lists the let, const and class bindings a statement list
// declares directly, which is what a name in an enclosing scope is shadowed by.
func topLevelLexicalNames(body []ast.Stmt) []string {
	var out []string
	for _, s := range body {
		switch n := s.(type) {
		case *ast.VarDecl:
			if n.Kind == ast.DeclVar {
				continue
			}
			for _, d := range n.Decls {
				collectPatternNames(d.Target, &out)
			}
		case *ast.ClassDecl:
			if n.Class.Name != nil {
				out = append(out, n.Class.Name.Name)
			}
		}
	}
	return out
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

// simpleParams reports whether every parameter is a plain identifier, which is
// what decides whether the parameter list is a scope of its own.
func simpleParams(params []ast.Expr) bool {
	for _, p := range params {
		if _, ok := p.(*ast.Ident); !ok {
			return false
		}
	}
	return true
}

// bindsArguments reports whether a parameter is named `arguments`, which leaves
// no arguments object: the parameter is what the name means.
func bindsArguments(params []ast.Expr) bool {
	var names []string
	for _, p := range params {
		collectPatternNames(p, &names)
	}
	for _, n := range names {
		if n == "arguments" {
			return true
		}
	}
	return false
}

// declaresArguments reports whether a function body declares `arguments` at its
// top level. With a plain parameter list the body and the parameters share a
// scope, so such a declaration is what the name means and no object is made.
func declaresArguments(body []ast.Stmt) bool {
	var lex []lexName
	for _, st := range body {
		lex = lexicalNamesOf(st, lex, true)
	}
	for _, n := range lex {
		if n.name == "arguments" {
			return true
		}
	}
	return false
}
