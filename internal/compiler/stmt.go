package compiler

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler/regcomp"
	"github.com/go-quickjs/go-quickjs/internal/fold"
)

// compileStatements compiles a statement list, hoisting the function
// declarations it contains so that they are callable before their definition.
func (c *compiler) compileStatements(body []ast.Stmt) {
	c.hoistBlockDeclarations(body, true)
	if has, async := usingKind(body); has {
		c.compileDisposeScope(body[0].Pos(), async, func() { c.compileList(body) })
		return
	}
	c.compileList(body)
}

// compileList compiles a list's statements in turn, those after one that
// always leaves the list -- a return, a throw, a break, a continue, as
// its compile notes (c.leaves, fold.Leaves) -- for what compiling them
// checks alone: nothing reaches them, and their code is dropped
// (compileDropped), as V8 drops what follows such a statement. It leaves
// c.leaves saying whether the list is always left so.
func (c *compiler) compileList(body []ast.Stmt) {
	dead := false
	for _, s := range body {
		if dead {
			c.compileDropped(func() { c.compileStatement(s) })
			continue
		}
		c.compileStatement(s)
		dead = c.leaves && fold.On(fold.Unreachable)
	}
	c.leaves = dead
}

// compileDropped compiles f, code nothing reaches -- a statement after a
// return, the branch a constant test never takes -- for what compiling it
// does besides its code: its early errors, which the compiler reports as
// it goes, and the bindings, captures and constants it makes, which are
// the function's whatever runs. Its eval scopes and the places a debugger
// stops at are dropped, as the register form compiles none of it.
func (c *compiler) compileDropped(f func()) {
	scopes := len(c.fn.EvalScopes)
	stmts := 0
	if c.fn.Debug != nil {
		stmts = len(c.fn.Debug.Statements)
	}
	f()
	c.fn.EvalScopes = c.fn.EvalScopes[:scopes]
	if c.fn.Debug != nil {
		c.fn.Debug.Statements = c.fn.Debug.Statements[:stmts]
	}
}

// hoistBlockDeclarations creates the bindings a statement list declares, before
// any of it runs.
//
// It is separate from compiling the list because a switch's bindings belong to
// the whole case block rather than to one clause: they are created once, before
// the first case expression is evaluated, and the clauses are compiled one
// after another afterwards. inOrder says the list runs from its first
// statement, which a switch's does not: a case can be jumped to past a
// declaration.
func (c *compiler) hoistBlockDeclarations(body []ast.Stmt, inOrder bool) {
	// Every binding of a scope exists before any of its code runs, so the
	// lexical ones are created first. They must precede the function
	// declarations: a hoisted function is compiled here, and if a let it
	// refers to had no slot yet, the reference would wrongly resolve to a
	// global that shadows it forever.
	//
	// The lexical bindings start in their dead zone, so a read before the
	// declaration is a ReferenceError rather than undefined.
	// At a module's top level the lexical bindings already live in the module
	// environment, installed by hoistModuleBindings, so declaring slots for
	// them here would shadow the very properties the linker forwards to.
	if !c.atModuleTopLevel() {
		closures := -1
		for i, s := range body {
			if vd, ok := s.(*ast.VarDecl); ok && vd.Kind != ast.DeclVar {
				// A let or const is marked as in its dead zone only where
				// something could read it there: a closure or an eval in the
				// list, which may run at any time, or a mention of the name
				// before the declaration is done, which is all that runs
				// before it in a list that runs in order. Otherwise nothing
				// reads the slot before it is initialized.
				mark := !inOrder || vd.Kind != ast.DeclLet && vd.Kind != ast.DeclConst
				if !mark {
					if closures < 0 {
						closures = 0
						if containsClosure(stmtNodes(body)...) {
							closures = 1
						}
					}
					// Each declaration's search is of the statements before
					// it, so in a long list a late one is marked rather than
					// searched for: compiling stays linear in the list.
					mark = closures == 1 || i > maxDeadZoneSearch || mentionsDeclared(vd, body[:i])
				}
				c.predeclareLexicalMarked(vd, mark)
			}
			if cd, ok := s.(*ast.ClassDecl); ok && cd.Class.Name != nil {
				c.declareLexicalName(cd.Class.Name.Name, bindLet, cd.Start)
			}
		}
	}
	// Function declarations are hoisted and initialized immediately, so a call
	// may precede the declaration textually. An exported one is hoisted too,
	// which means looking through the export wrapper.
	//
	// Every binding is created before any of the bodies is compiled, because a
	// body that refers to a function declared after it has to find the binding
	// rather than fall through to a global that shadows it forever.
	var hoisted []hoistedFunc
	for _, s := range body {
		fd, name, ok := hoistableFunction(s)
		if !ok {
			continue
		}
		if c.moduleBindingsDone && c.atModuleTopLevel() {
			// A module's top-level functions were defined when it was linked,
			// which is before any of this runs. A block's are its own, and
			// are made when the block is entered, as anywhere else.
			continue
		}
		h := hoistedFunc{fd: fd, name: name}
		if !c.functionsAreGlobal() {
			kind := bindFunction
			if fd.Fn.Async || fd.Fn.Generator {
				kind = bindFunctionLexical
			}
			h.slot, h.local = c.declare(name, kind, fd.Start), true
		}
		hoisted = append(hoisted, h)
	}
	for _, h := range hoisted {
		c.predeclareFunction(h)
	}
}

// hoistedFunc is a function declaration whose binding has been created and
// whose definition is still to be emitted.
type hoistedFunc struct {
	fd    *ast.FuncDecl
	name  string
	slot  uint32
	local bool
}

// functionsAreGlobal reports whether a hoisted function declaration becomes a
// property of the global object rather than a binding in a frame slot.
func (c *compiler) functionsAreGlobal() bool {
	return c.parent == nil && c.depth == 0 && !c.evalVarsAreLocal()
}

// predeclareFunction walks a hoisted function declaration's definition into
// the binding that was already created for it.
func (c *compiler) predeclareFunction(h hoistedFunc) {
	// `export default function () {}` is hoisted like any other function
	// declaration, but the binding it creates has a name no identifier can
	// spell -- while the function itself is called "default".
	fnName := h.name
	if h.fd.Fn.Name == nil {
		fnName = "default"
	}
	// A declaration's name is a binding of the enclosing scope, not of the
	// function: clearing it here is what keeps the function from binding it
	// again, immutably, inside itself.
	lit := *h.fd.Fn
	lit.Name = nil
	c.compileFunctionLiteral(&lit, fnName)
	if !h.local {
		// Sloppy eval code declaring a function the calling function already
		// binds assigns to that binding rather than making one of its own,
		// which is what lets the evaluated code replace a var the caller
		// declared -- and what a reference to the name finds afterwards,
		// whether it is written inside the eval or outside it.
		if b, known := c.callerBinding(h.name); known && b.VarScoped {
			// The register form makes the store as the write of an
			// identifier of its own (regcomp.TopOp's Id): the name's is
			// an Annex B alias's where it has one.
			id := &ast.Ident{Name: h.name, Start: h.fd.Fn.Name.Start}
			saved := c.regIdent
			c.regIdent = id
			c.storeVar(h.name)
			c.regIdent = saved
			if c.reg != nil && c.parent == nil {
				c.reg.NoteTop(regcomp.TopOp{Op: bytecode.OpSetUpvalue, Name: h.name, Pos: int32(h.fd.Start), Fn: h.fd.Fn, Id: id})
			}
			return
		}
		c.regTop(bytecode.OpDefineGlobalFunc, h.name, boolBit(c.opts.EvalConfigurable), h.fd.Fn, h.fd.Start)
		return
	}

	c.reg.NoteFunc(h.fd, h.slot)
	c.noteStore(h.slot)
}

// emitAnnexBFunctionAlias copies a block-scoped function into the var binding
// that shares its name, id's: the register form is told the var's store
// as id's write.
func (c *compiler) emitAnnexBFunctionAlias(id *ast.Ident) {
	name := id.Name
	// Hoisting made sure nothing lexical stands in the way, other than a simple
	// catch parameter, which the alias passes over to reach the var -- or the
	// function declared at the top level, whose binding the var is. The
	// function's own block-scoped binding is skipped too, since it is the
	// thing being aliased.
	for i := len(c.locals) - 1; i >= 0; i-- {
		l := &c.locals[i]
		if l.name != name || l.depth >= c.depth || !l.varScoped() {
			continue
		}
		c.noteWrite(id, regcomp.Res{Kind: regcomp.ResLocal, Index: l.slot})
		return
	}
	if c.parent != nil {
		return
	}
	// At a script's top level the var is a property of the global object
	// rather than a slot, and hoistGlobals has already created it. The store
	// consumes the copy, leaving the function for the binding it belongs to.
	// Eval code's var is the calling function's: a binding it already had, or
	// the one the eval made in its variable scope.
	if b, known := c.callerBinding(name); known && b.VarScoped && !c.evalOwnVars[name] {
		idx, _ := c.resolveUpvalue(name)
		c.noteWrite(id, regcomp.Res{Kind: regcomp.ResUpval, Index: idx})
		return
	}
	c.noteWrite(id, regcomp.Res{Kind: regcomp.ResGlobalVar})
}

// predeclareLexical creates the bindings of a let or const declaration in
// their uninitialized state.
func (c *compiler) predeclareLexical(vd *ast.VarDecl) {
	c.predeclareLexicalMarked(vd, true)
}

// predeclareLexicalMarked is predeclareLexical, storing the dead zone's mark
// in the bindings' slots only with mark: without it they hold whatever they
// held, which nothing reads before they are initialized.
func (c *compiler) predeclareLexicalMarked(vd *ast.VarDecl, mark bool) {
	kind := bindLet
	if vd.Kind.IsConst() {
		kind = bindConst
	}
	var room [8]string
	names := room[:0]
	for _, d := range vd.Decls {
		names = appendPatternNames(names, d.Target)
	}
	for _, n := range names {
		c.declareLexicalNameMarked(n, kind, vd.Start, mark)
	}
}

// declareLexicalName declares a lexical binding and leaves it uninitialized.
func (c *compiler) declareLexicalName(name string, kind bindKind, pos int) {
	c.declareLexicalNameMarked(name, kind, pos, true)
}

// declareLexicalNameMarked is declareLexicalName, storing the dead zone's
// mark in a frame slot only with mark.
func (c *compiler) declareLexicalNameMarked(name string, kind bindKind, pos int, mark bool) {
	if c.atScriptTopLevel() {
		// A script's top-level lexical bindings outlive it: the next script in
		// the same realm sees them, and so does eval, which a frame slot could
		// not manage.
		if c.globalLex == nil {
			c.globalLex = map[string]bool{}
		}
		if c.globalLex[name] {
			// Two lexical declarations of one name in the same script, which
			// is an error the script never gets to run.
			c.errorf(pos, "%q has already been declared", name)
		}
		c.globalLex[name] = true
		mutable := uint32(0)
		if kind != bindConst {
			mutable = 1
		}
		c.regTop(bytecode.OpDeclareGlobalLex, name, mutable, nil, pos)
		return
	}
	slot := c.declare(name, kind, pos)
	// The slot starts as the uninitialized marker, which the checked accessors
	// test for. Frame locals are cleared on entry, and the zero Value is the
	// number 0, so the marker has to be stored explicitly.
	if mark {
	} else {
		c.reg.NoteUnmarked(slot)
	}
	c.markUninitialized(name)
}

// maxDeadZoneSearch is how many statements before a let or const are
// searched for a mention of it, beyond which it is marked as in its dead
// zone whether or not anything could see the mark.
const maxDeadZoneSearch = 64

// stmtNodes is a statement list as nodes, for the walkers that take them.
func stmtNodes(list []ast.Stmt) []ast.Node {
	nodes := make([]ast.Node, len(list))
	for i, s := range list {
		nodes[i] = s
	}
	return nodes
}

// mentionsDeclared reports whether a declaration's bindings could be read
// before it initializes them: whether any of their names is mentioned in the
// statements before it, or in its own initializers. A pattern counts as a
// mention, since a default in it could read one.
func mentionsDeclared(vd *ast.VarDecl, before []ast.Stmt) bool {
	var names []string
	for _, d := range vd.Decls {
		id, ok := d.Target.(*ast.Ident)
		if !ok {
			return true
		}
		names = append(names, id.Name)
	}
	for _, n := range names {
		w := &argumentsScanner{seekName: n}
		w.stmts(before)
		for _, d := range vd.Decls {
			w.expr(d.Init)
		}
		if w.found {
			return true
		}
	}
	return false
}

// markUninitialized records that a binding is still in its dead zone.
func (c *compiler) markUninitialized(name string) {
	if l, ok := c.resolveLocal(name); ok {
		l.initialized = false
	}
}

func (c *compiler) compileStatement(s ast.Stmt) {
	c.enter(s.Pos())
	defer c.leave()
	// What its list needs of it once it is compiled: whether it always
	// leaves the list, which a block's or an if's compile sets.
	defer func() { c.leaves = fold.Leaves(s, c.leaves) }()
	if c.opts.Debug {
		c.debugStatement(s)
	}

	switch n := s.(type) {
	case *ast.ExprStmt:
		if c.completionSlot >= 0 {
			// At the top level an expression statement's value is the
			// program's completion value, which eval returns.
			c.compileExpr(n.X)
		} else {
			c.compileExprForEffect(n.X)
		}

	case *ast.VarDecl:
		c.compileVarDecl(n)

	case *ast.FuncDecl:
		// Made by predeclareFunction as its block was entered. Annex B: a
		// plain function declared inside a block is also assigned to a
		// var-scoped binding of the same name, which is what makes
		//
		//	function outer() { { function f() {} } return f; }
		//
		// work in sloppy mode. The binding is created by hoisting like any
		// other var; the assignment, of what the block's binding holds,
		// happens here, when the declaration is evaluated (B.3.3.1).
		if c.annexB[n] {
			c.compileIdentReadStatic(n.Fn.Name)
			c.emitAnnexBFunctionAlias(n.Fn.Name)
		}

	case *ast.ClassDecl:
		c.compileClass(n.Class, n.Class.Name.Name)
		c.assignTo(n.Class.Name, true)

	case *ast.BlockStmt:
		c.beginScope()
		c.compileStatements(n.Body)
		c.endScope()

	case *ast.EmptyStmt:

	case *ast.IfStmt:
		c.compileIf(n)

	case *ast.WhileStmt:
		c.compileWhile(n)

	case *ast.DoWhileStmt:
		c.compileDoWhile(n)

	case *ast.ForStmt:
		c.compileFor(n)

	case *ast.ForInStmt:
		c.compileForIn(n)

	case *ast.ForOfStmt:
		c.compileForOf(n)

	case *ast.ReturnStmt:
		if _, this := n.Arg.(*ast.This); !this {
			c.fn.ReturnsReceiver = false
		}
		if n.Arg != nil {
			if c.tailCallsAllowed() {
				c.compileTailExpr(n.Arg)
			} else {
				c.compileExpr(n.Arg)
			}
		}
		c.emitReturnValue(n.Start)

	case *ast.BreakStmt:
		c.compileBreak(n)

	case *ast.ContinueStmt:
		c.compileContinue(n)

	case *ast.ThrowStmt:
		c.compileExpr(n.Arg)

	case *ast.TryStmt:
		c.compileTry(n)

	case *ast.SwitchStmt:
		c.compileSwitch(n)

	case *ast.LabeledStmt:
		c.compileLabeled(n)

	case *ast.DebuggerStmt:
		// Only code compiled for a debugger stops here, at the debug instruction
		// debugStatement gave it; anything else runs on.

	case *ast.ImportDecl:
		c.compileImportDecl(n)

	case *ast.ExportDecl:
		c.compileExportDecl(n)

	case *ast.DisposeStmt:
		c.compileDispose(n)

	case *ast.InstallPrivateMethods:
		c.compileIdentRead(n.Binding)

	case *ast.FieldInit:
		c.compileFieldInit(n)

	case *ast.WithStmt:
		c.compileWith(n)

	default:
		c.errorf(s.Pos(), "unsupported statement %T", s)
	}
}

// compileVarDecl compiles a var, let or const declaration.
func (c *compiler) compileVarDecl(n *ast.VarDecl) {
	for _, d := range n.Decls {
		if id, ok := d.Target.(*ast.Ident); ok && stringly(d.Init) {
			if l, ok := c.resolveLocal(id.Name); ok {
				l.stringy = true
			}
		}
		if d.Init == nil {
			// `var x;` leaves an existing binding alone; a lexical binding
			// without an initializer becomes undefined.
			if n.Kind != ast.DeclVar {
				c.initBinding(d.Target, n.Kind)
			}
			continue
		}
		if id, ok := d.Target.(*ast.Ident); ok && n.Kind == ast.DeclVar &&
			!c.opts.NodeQuirks && c.withLimit(id.Name) > 0 {
			// A var's initializer is an assignment, and inside a `with` body
			// an assignment resolves its name before it evaluates its value:
			// an initializer that deletes the property still writes to the
			// object the name named. The value the store leaves is the
			// declaration's, which nothing reads. With NodeQuirks the name
			// is resolved after the value is made, as V8 does: the store
			// below, a binding's.
			c.resolveWithRef(id)
			if handsName(d.Init) {
				c.traceName = c.traceJoin("", id.Name)
			}
			c.compileExprNamed(d.Init, id.Name)
			c.endWithRef(id)
			continue
		}
		if fn, ok := d.Init.(*ast.FuncLit); ok && c.reused[fn] && c.inLoop() {
			// A function nothing can tell from the one the last iteration
			// made, which the var holds then: made the first time, and
			// left after that (reusedClosures).
			c.compileIdentRead(d.Target.(*ast.Ident))
			if handsName(d.Init) {
				c.traceName = c.traceJoin("", nameOf(d.Target))
			}
			c.compileExprNamed(d.Init, nameOf(d.Target))
			c.initBinding(d.Target, n.Kind)
			continue
		}
		if handsName(d.Init) {
			c.traceName = c.traceJoin("", nameOf(d.Target))
		}
		c.compileExprNamed(d.Init, nameOf(d.Target))
		if n.Kind.IsUsing() {
			// The value is added to the scope's resources first, and a value
			// that cannot be disposed of leaves the binding in its dead zone.
			c.addDisposable(n.Kind, d.Target.Pos())
		}
		c.initBinding(d.Target, n.Kind)
		if id, ok := d.Target.(*ast.Ident); ok && n.Kind == ast.DeclConst {
			// What its reads give from here on (constLiteral).
			switch d.Init.(type) {
			case *ast.NullLit, *ast.BoolLit, *ast.NumberLit, *ast.StringLit:
				if l, ok := c.resolveLocal(id.Name); ok {
					l.lit = d.Init
					if c.litNames == nil {
						// Shared with every function made from here on, the
						// only ones that may read it without the dead zone's
						// check (constLiteral).
						c.litNames = map[string]bool{}
					}
					c.litNames[id.Name] = true
				}
			}
		}
	}
}

// inLoop reports whether the code being compiled is in a loop's body.
func (c *compiler) inLoop() bool {
	for _, l := range c.loops {
		if l.isLoop {
			return true
		}
	}
	return false
}

// initBinding walks the store of a value into a declaration's target.
func (c *compiler) initBinding(target ast.Expr, kind ast.DeclKind) {
	if id, ok := target.(*ast.Ident); ok {
		if kind == ast.DeclVar {
			c.regIdent = id
			c.storeVar(id.Name)
			c.regIdent = nil
			return
		}
		// A lexical binding is initialized rather than assigned, which also
		// clears its dead zone.
		if l, ok := c.resolveLocal(id.Name); ok {
			c.noteWrite(id, regcomp.Res{Kind: regcomp.ResLocalInit, Index: l.slot})
			c.markInitialized(id.Name)
			return
		}
		// A block of its own would have declared a local, so reaching here
		// means the binding is the top-level one.
		if c.globalLex[id.Name] {
			c.noteWrite(id, regcomp.Res{Kind: regcomp.ResGlobalLexInit})
			return
		}
		if c.isModuleLex(id.Name) {
			c.noteWrite(id, regcomp.Res{Kind: regcomp.ResModuleLexInit})
			return
		}
		return
	}
	c.compileDestructuring(target, kind)
}

// emitReturnValue walks a return, routed through any finally clause that
// owes a run first.
func (c *compiler) emitReturnValue(pos int) {
	if len(c.finallys) > 0 {
		// The finally clause must run before the function actually returns, so
		// the return becomes a completion record it consumes. Every handler
		// between here and the clause is left behind by the jump, including
		// the clause's own: a clause that was still protected by itself would
		// run a second time.
		ctx := &c.finallys[len(c.finallys)-1]
		savedDepth := c.handlerDepth
		if len(c.exits) > ctx.exits {
			// What the statements inside the clause's try left in place is
			// undone first, as they are left before the try is: a for...of
			// there closes its iterator before the clause runs. The value
			// waits in a hidden local meanwhile.
			if c.returnSlot < 0 {
				c.returnSlot = int32(c.declare(fmt.Sprintf("%%rv%d", *c.hiddenCount), bindVar, pos))
				*c.hiddenCount++
			}
			c.unwindExits(len(c.exits), ctx.exits)
		}
		c.handlerDepth = savedDepth
		return
	}
}

// storeVar assigns to a var binding or a global.
func (c *compiler) storeVar(name string) {
	// A var's initializer is an assignment rather than a binding
	// initialization: the declaration hoists out of a `with` body, but the
	// store happens inside it, where the object may be what is written to.
	// The probe answers, or the static store runs.
	probe := c.withProbe(bytecode.OpWithSet, name)
	switch {
	case c.regIdent != nil:
		c.regWith(c.regIdent, probe)
	case probe >= 0 && c.reg != nil:
		// Eval code's function declaration, which the register form
		// does not compile.
		c.reg.Bail = true
	}
	if probe >= 0 {
		defer func() {
		}()
	}

	if l, ok := c.resolveLocal(name); ok {
		if c.regIdent != nil {
			c.noteWrite(c.regIdent, regcomp.Res{Kind: regcomp.ResLocalInit, Index: l.slot})
		}
		return
	}
	if idx, ok := c.resolveUpvalue(name); ok {
		if c.regIdent != nil {
			res := regcomp.Res{Kind: regcomp.ResUpval, Index: idx}
			if c.fn.Upvalues[idx].TDZ {
				res = regcomp.Res{}
			}
			c.noteWrite(c.regIdent, res)
		}
		return
	}
	if c.regIdent != nil {
		c.noteWrite(c.regIdent, regcomp.Res{Kind: regcomp.ResGlobalVar})
	}
}

func (c *compiler) compileIf(n *ast.IfStmt) {
	// A test that folds runs one branch, and the other is never reached
	// (fold.Dead): as V8's ToBooleanIsTrue, ToBooleanIsFalse. The if is
	// always left where the branch that runs is.
	if truthy, ok := fold.Dead(n.Test); ok {
		leaves := false
		if truthy {
			c.compileStatement(n.Cons)
			leaves = c.leaves
		} else {
			c.compileDropped(func() { c.compileStatement(n.Cons) })
		}
		if n.Alt != nil {
			if truthy {
				c.compileDropped(func() { c.compileStatement(n.Alt) })
			} else {
				c.compileStatement(n.Alt)
				leaves = c.leaves
			}
		}
		c.leaves = leaves
		return
	}
	// if (test) break; is the condition's own jumps, to where the break goes,
	// when the break has nothing to undo on its way: so is a continue.
	if n.Alt == nil {
		if i, _ := c.bareJumpTarget(n.Cons); i >= 0 {
			c.compileTest(n.Test)
			c.leaves = false
			return
		}
	}
	c.compileTest(n.Test)
	c.compileStatement(n.Cons)

	if n.Alt == nil {
		c.leaves = false
		return
	}
	// The if is always left where both branches are.
	leaves := c.leaves
	c.compileStatement(n.Alt)
	c.leaves = leaves && c.leaves
}

// pushLoop begins a loop context for break and continue.
//
// A pending label set by an enclosing labelled statement is consumed here, so
// that `outer: for (...)` gives the loop itself the label and `continue outer`
// reaches its update clause rather than its exit.
func (c *compiler) pushLoop(labels []string, isLoop bool) *loopCtx {
	if labels == nil {
		labels = c.pendingLabels
		c.pendingLabels = nil
	}
	c.loops = append(c.loops, loopCtx{
		labels: labels, isLoop: isLoop, scopeDepth: c.depth, exits: len(c.exits),
		finallys: len(c.finallys), handlers: c.handlerDepth, firstSlot: c.nextSlot,
	})
	return &c.loops[len(c.loops)-1]
}

// popLoop ends a loop context.
func (c *compiler) popLoop() {
	c.loops = c.loops[:len(c.loops)-1]
}

func (c *compiler) compileWhile(n *ast.WhileStmt) {
	if truthy, ok := fold.Dead(n.Test); ok && !truthy {
		// A loop whose test is false runs nothing (fold.Dead).
		c.compileDropped(func() { c.compileLoopWhile(n) })
		return
	}
	c.compileLoopWhile(n)
}

// compileLoopWhile compiles a while loop.
func (c *compiler) compileLoopWhile(n *ast.WhileStmt) {
	c.pushLoop(nil, true)
	c.debugExpr(n.Test)
	c.compileTest(n.Test)
	c.compileStatement(n.Body)
	c.popLoop()
}

func (c *compiler) compileDoWhile(n *ast.DoWhileStmt) {
	c.pushLoop(nil, true)
	c.compileStatement(n.Body)
	c.debugExpr(n.Test)
	c.compileTest(n.Test)
	c.popLoop()
}

func (c *compiler) compileFor(n *ast.ForStmt) {
	// The init clause's bindings live in a scope enclosing the loop.
	c.beginScope()

	// A let or const in the head is a fresh binding each iteration, so a
	// closure created in one iteration must not see a later iteration's value.
	// The slot where those bindings start is recorded here so that the end of
	// each iteration can detach any closure that captured them.
	firstSlot := c.nextSlot
	perIteration := false

	if vd, ok := n.Init.(*ast.VarDecl); ok && vd.Kind.IsUsing() {
		// What the head declares is disposed of when the loop is done with,
		// not each iteration: the bindings are not copied from one to the
		// next, since nothing can assign to them.
		c.predeclareLexical(vd)
		c.compileDisposeScope(n.Start, vd.Kind == ast.DeclAwaitUsing, func() {
			c.compileVarDecl(vd)
			c.compileForLoop(n, firstSlot, false)
		})
		c.endScope()
		return
	}

	if n.Init != nil {
		switch init := n.Init.(type) {
		case *ast.VarDecl:
			if init.Kind != ast.DeclVar {
				c.predeclareLexical(init)
				// Only a closure could tell one iteration's binding from
				// the next's.
				perIteration = containsClosure(init, n.Test, n.Update, n.Body)
				if perIteration {
					for i := range c.locals {
						if c.locals[i].slot >= firstSlot {
							c.locals[i].copied = true
						}
					}
				}
			}
			c.compileVarDecl(init)
		case *ast.ExprStmt:
			c.compileExpr(init.X)
		}
	}
	if truthy, ok := fold.Dead(n.Test); ok && !truthy {
		// A loop whose test is false runs its init clause alone
		// (fold.Dead).
		c.compileDropped(func() { c.compileForLoop(n, firstSlot, perIteration) })
	} else {
		c.compileForLoop(n, firstSlot, perIteration)
	}
	c.endScope()
}

// compileForLoop compiles a three-clause for once its init clause has run.
func (c *compiler) compileForLoop(n *ast.ForStmt, firstSlot uint32, perIteration bool) {
	c.pushLoop(nil, true)

	if n.Test != nil {
		c.debugExpr(n.Test)
		c.compileTest(n.Test)
	}
	c.compileStatement(n.Body)

	if n.Update != nil {
		c.debugExpr(n.Update)
		c.compileExprForEffect(n.Update)
	}
	// `continue` jumps to the update clause, not the test.
	c.popLoop()
}

func (c *compiler) compileForIn(n *ast.ForInStmt) {
	if c.beginHeadScope(n.Left) {
		defer c.endScope()
	}
	// Annex B's `for (var a = 0 in o)` assigns its initializer once, before
	// the object is evaluated.
	if vd, ok := n.Left.(*ast.VarDecl); ok && vd.Kind == ast.DeclVar && vd.Decls[0].Init != nil {
		c.compileForInInit(vd)
	}
	c.compileExpr(n.Right)
	c.pushExit(exitCursor)
	c.compileForBody(n.Left, n.Body)
	c.popExit()
}

// compileForInInit walks for-in head vd's Annex B initializer. Where a
// with object or an eval's variable may answer the name, the initializer
// resolves its reference before its value is made (B.3.5), and each turn's
// store resolves it after: the two are probed and stored differently, so
// the initializer is the write of an identifier of its own, which the
// register form is told of (regcomp.Record.NoteForInInit). With
// NodeQuirks a var's initializer resolves its name after its value is
// made, as V8 does (compileVarDecl), and so is stored as each turn's
// value is, the identifier's one write.
func (c *compiler) compileForInInit(vd *ast.VarDecl) {
	id, ok := vd.Decls[0].Target.(*ast.Ident)
	if !ok || c.reg == nil || c.opts.NodeQuirks || c.withLimit(id.Name) == 0 {
		c.compileVarDecl(vd)
		return
	}
	init := &ast.VarDecl{Kind: vd.Kind, Start: vd.Start, Decls: []ast.Declarator{{
		Target: &ast.Ident{Name: id.Name, Start: id.Start},
		Init:   vd.Decls[0].Init,
	}}}
	c.reg.NoteForInInit(vd, init)
	c.compileVarDecl(init)
}

// beginHeadScope puts a for-in or for-of head's lexical bindings in scope, in
// their dead zone, for the expression that follows.
//
// `for (let x of [x])` names the binding being declared rather than one of the
// same name outside it, which is a scope of its own -- distinct again from the
// one each iteration gets.
func (c *compiler) beginHeadScope(left ast.Node) bool {
	vd, ok := left.(*ast.VarDecl)
	if !ok || vd.Kind == ast.DeclVar {
		return false
	}
	c.beginScope()
	c.predeclareLexical(vd)
	if c.reg != nil {
		var names []string
		for _, d := range vd.Decls {
			names = appendPatternNames(names, d.Target)
		}
		for _, n := range names {
			if l, ok := c.resolveLocal(n); ok {
				c.reg.NoteHead(vd, uint32(l.slot))
			}
		}
	}
	return true
}

func (c *compiler) compileForOf(n *ast.ForOfStmt) {
	if c.beginHeadScope(n.Left) {
		defer c.endScope()
	}
	c.compileExpr(n.Right)
	if n.Await {
		c.pushExit(exitCursor)
		c.compileForAwaitBody(n.Left, n.Body, n.Right.Pos())
		c.popExit()
		return
	}
	c.pushExit(exitCursor)
	c.compileForBody(n.Left, n.Body)
	c.popExit()
}

// compileForAwaitBody walks the loop of a `for await`, which differs from the
// synchronous form in that each step awaits the promise the iterator returns
// before unpacking the result, and in how it is left.
//
// A loop its body leaves -- a break, a continue of an outer loop, a return,
// a throw, or a return or a throw injected where the body is suspended --
// closes its iterator as AsyncIteratorClose does: its return method's
// result is awaited, and checked to be an object unless what leaves is a
// throw, which a failure in closing does not replace. The runtime's
// unwinding cannot await, so the loop is protected by a finally clause of
// its own that does: inlined where a break or a continue leaves through it,
// as a finally clause is, and the handler a return, or a throw, is routed
// to. A loop the iterator ends, or whose next, or the result it awaited,
// fails, is not closed. The cursor is kept in a hidden local, where the
// close finds it, and another says whether the body is running.
//
// Each turn's await is at pos, the loop's expression's: a stack trace of
// what throws after it -- the iterator's return method closing the loop --
// names that line.
func (c *compiler) compileForAwaitBody(left ast.Node, body ast.Stmt, pos int) {
	cur := c.declare(fmt.Sprintf("%%fa%d", *c.hiddenCount), bindVar, left.Pos())
	busy := c.declare(fmt.Sprintf("%%fb%d", *c.hiddenCount), bindVar, left.Pos())
	*c.hiddenCount++
	c.handlerDepth++
	c.finallys = append(c.finallys, finallyCtx{
		emit:     func() { c.emitAsyncClose(cur, busy, false) },
		handlers: c.handlerDepth, exits: len(c.exits),
	})

	c.pushLoop(nil, true)
	c.debugAt(left.Pos())

	c.emitAwait(pos)

	c.beginScope()
	c.compileIterationBody(left, body)
	c.endScope()

	c.popLoop()
	// A break of this loop arrives here, its handler still in force.
	c.handlerDepth--
	c.emitAsyncClose(cur, busy, false)
	// So does a loop the iterator ended, which is not closed.

	// The clause's handler, and the returns routed to it, with the
	// completion's record above the cursor.
	c.finallys = c.finallys[:len(c.finallys)-1]
	c.emitAsyncClose(cur, busy, true)
	c.emitAsyncClose(cur, busy, false)
}

// emitAsyncClose walks a for await's AsyncIteratorClose of the cursor in
// local cur, where local busy says the body is running: its return method
// is called, and what that returns awaited and, unless quiet, checked to be
// an object. quiet is a throw's: whatever goes wrong closing is dropped,
// and the throw goes on. Where the body is not running, the cursor is only
// marked done, so that nothing closes it after.
func (c *compiler) emitAsyncClose(cur, busy uint32, quiet bool) {
	if !quiet {
		return
	}
	c.handlerDepth++
	c.handlerDepth--
}

// compileForBody walks the shared loop structure of for-in and for-of, its
// iterator made.
func (c *compiler) compileForBody(left ast.Node, body ast.Stmt) {
	c.pushLoop(nil, true)
	c.debugAt(left.Pos())

	// IterNextOrJump advances the iterator, pushing the next value, or jumps
	// to the exit when it is exhausted.

	c.beginScope()
	c.compileIterationBody(left, body)
	c.endScope()

	c.popLoop()
}

// bareJumpTarget is the index in c.loops of the statement a break or
// continue goes to, alone or alone in a block, and whether it is a continue,
// if it is one that is a jump and nothing else: it leaves no finally clause,
// exception handler, iterator, operand or with object behind. It is -1 for
// anything else.
func (c *compiler) bareJumpTarget(s ast.Stmt) (int, bool) {
	if b, ok := s.(*ast.BlockStmt); ok && len(b.Body) == 1 {
		s = b.Body[0]
	}
	label, cont := "", false
	switch n := s.(type) {
	case *ast.BreakStmt:
		label = n.Label
	case *ast.ContinueStmt:
		label, cont = n.Label, true
	default:
		return -1, false
	}
	for i := len(c.loops) - 1; i >= 0; i-- {
		l := &c.loops[i]
		if cont && !l.isLoop || label == "" && l.labelOnly {
			continue
		}
		if label == "" || l.named(label) {
			if len(c.finallys) > l.finallys || c.handlerDepth > l.handlers || len(c.exits) > l.exits {
				return -1, false
			}
			return i, cont
		}
	}
	return -1, false
}

func (c *compiler) compileBreak(n *ast.BreakStmt) {
	for i := len(c.loops) - 1; i >= 0; i-- {
		if n.Label == "" && !c.loops[i].labelOnly || n.Label != "" && c.loops[i].named(n.Label) {
			c.unwindTo(&c.loops[i])
			return
		}
	}
	c.errorf(n.Start, "\"break\" has no enclosing target")
}

func (c *compiler) compileContinue(n *ast.ContinueStmt) {
	for i := len(c.loops) - 1; i >= 0; i-- {
		if !c.loops[i].isLoop {
			continue
		}
		if n.Label == "" || c.loops[i].named(n.Label) {
			c.unwindTo(&c.loops[i])
			return
		}
	}
	c.errorf(n.Start, "\"continue\" has no enclosing loop")
}

func (c *compiler) compileLabeled(n *ast.LabeledStmt) {
	// A labelled loop reuses the loop's own context so that `continue label`
	// reaches the loop's update clause rather than its exit. The labels in
	// front of a statement, `a: b: while (...)`, are all its own.
	labels := []string{n.Label}
	body := n.Body
	for {
		inner, ok := body.(*ast.LabeledStmt)
		if !ok {
			break
		}
		labels = append(labels, inner.Label)
		body = inner.Body
	}
	switch body.(type) {
	case *ast.WhileStmt, *ast.DoWhileStmt, *ast.ForStmt, *ast.ForInStmt, *ast.ForOfStmt:
		c.compileLabeledLoop(labels, body)
	default:
		c.pushLoop(labels, false).labelOnly = true
		c.compileStatement(body)
		c.popLoop()
	}
}

// compileLabeledLoop compiles a loop whose own context carries the labels.
func (c *compiler) compileLabeledLoop(labels []string, body ast.Stmt) {
	// The loop walkers push their context themselves, so the labels are
	// recorded in a pending field the next pushLoop consumes.
	c.pendingLabels = labels
	c.compileStatement(body)
}

// named reports whether label is one of the statement's labels.
func (l *loopCtx) named(label string) bool {
	for _, x := range l.labels {
		if x == label {
			return true
		}
	}
	return false
}

// compileTry compiles a try statement.
//
// A finally clause has to run however the protected block finished, so the
// block's outcome is a completion record -- a value and a kind -- the finally
// code reproduces afterwards. That is what makes `return` inside a try still
// run the finally before returning.
func (c *compiler) compileTry(n *ast.TryStmt) {
	if n.Finally == nil {
		c.compileTryCatch(n)
		return
	}
	c.compileTryFinally(n.Start, func() {
		// The catch clause, when present, sits inside the finally's
		// protection so that a throw from the catch body still runs the
		// finally.
		if n.Catch != nil {
			c.compileTryCatch(&ast.TryStmt{Block: n.Block, Catch: n.Catch, Start: n.Start})
		} else {
			c.beginScope()
			c.compileStatements(n.Block)
			c.endScope()
		}
	}, n.Finally)
}

// compileTryFinally compiles body protected by a finally clause.
//
// The clause is compiled twice where it can be: inline, for the body's
// normal completion, which goes on past the try after it; and as the
// handler's, which a throw and the returns routed through the clause enter
// with their completion record, and its rethrow carries on. A
// loop whose body is a try statement then runs no record and no rethrow at
// each turn, as it does none for a try with only a catch. Code compiled for a debugger has the clause
// once, so that a breakpoint in it is one statement; and a clause inside a
// clause has it once, so that nested ones are not copied again for each
// copy of the one outside.
func (c *compiler) compileTryFinally(pos int, body func(), finally []ast.Stmt) {
	c.handlerDepth++
	c.finallys = append(c.finallys, finallyCtx{body: finally, handlers: c.handlerDepth, exits: len(c.exits)})

	body()

	// Normal completion: drop the finally handler and run the clause, inline
	// or with a record saying nothing unusual happened.
	c.handlerDepth--
	c.finallys = c.finallys[:len(c.finallys)-1]
	if !c.opts.Debug && c.finallyBodies == 0 {
		c.compileFinallyClause(pos, finally, false)
	}

	// The handler jumps here, having pushed its own record, and so do any
	// returns that were routed through the clause.
	c.compileFinallyClause(pos, finally, true)
}

// compileFinallyClause walks a finally clause's statements, with the
// completion record of the try where record says so, or with none, inlined
// after the try's normal completion.
func (c *compiler) compileFinallyClause(pos int, finally []ast.Stmt, record bool) {
	// What the finally clause produces is not the statement's value: the try
	// block's is, so `try { 1 } finally { 2 }` is 1, which a binding of its
	// own keeps across the clause.
	if c.completionSlot >= 0 {
		name := fmt.Sprintf("%%cv%d", *c.hiddenCount)
		*c.hiddenCount++
		c.declare(name, bindVar, pos)
	}
	// A break or continue leaving the clause drops the completion record.
	if record {
		c.pushExit(exitCompletion)
	}
	defer c.reg.BeginFinally(finally)()
	c.beginScope()
	savedRecord := c.finallyRecord
	c.finallyRecord = record
	c.finallyBodies++
	c.compileStatements(finally)
	c.finallyBodies--
	c.finallyRecord = savedRecord
	c.endScope()
	if record {
		c.popExit()
	}
}

// compileTryCatch compiles a try statement that has no finally clause.
func (c *compiler) compileTryCatch(n *ast.TryStmt) {
	if n.Catch == nil {
		c.beginScope()
		c.compileStatements(n.Block)
		c.endScope()
		return
	}
	c.handlerDepth++
	c.beginScope()
	c.compileStatements(n.Block)
	c.endScope()
	// The handler is gone from here on: the catch body runs after it fired,
	// and the code after the statement after it was popped.
	c.handlerDepth--

	c.beginScope()
	if n.Catch.Param != nil {
		c.bindCatchParam(n.Catch, n.Catch.Param)
	}
	c.compileStatements(n.Catch.Body)
	c.endScope()
}

// bindCatchParam binds the caught value to the catch clause's parameter.
func (c *compiler) bindCatchParam(clause *ast.CatchClause, param ast.Expr) {
	if id, ok := param.(*ast.Ident); ok {
		slot := c.declare(id.Name, bindCatch, int(id.Start))
		c.reg.NoteCatch(clause, slot)
		// The store is what binds it, so the clause's body assigns to an
		// ordinary mutable binding rather than one still in a dead zone.
		c.markInitialized(id.Name)
		return
	}
	var names []string
	names = appendPatternNames(names, param)
	for _, n := range names {
		c.declare(n, bindCatch, param.Pos())
	}
	c.compileDestructuring(param, ast.DeclLet)
}

func (c *compiler) compileSwitch(n *ast.SwitchStmt) {
	c.compileExpr(n.Disc)
	c.pushExit(exitDrop)
	c.beginScope()
	c.pushLoop(nil, false)

	// The bindings of every clause belong to the case block as a whole, and
	// they are created before the first case expression is evaluated: a
	// selector may close over one, and finds the block's binding rather than
	// whatever the same name means outside.
	var all []ast.Stmt
	for _, cs := range n.Cases {
		all = append(all, cs.Body...)
	}
	first := len(c.locals)
	c.hoistBlockDeclarations(all, false)
	for i := first; i < len(c.locals); i++ {
		if k := c.locals[i].kind; k == bindLet || k == bindConst {
			c.locals[i].jumpedOver = true
		}
	}

	// Each case's test, then the clause bodies, in order.
	for _, cs := range n.Cases {
		if cs.Test != nil {
			c.compileExpr(cs.Test)
		}
	}
	for _, cs := range n.Cases {
		c.compileList(cs.Body)
	}

	c.popLoop()
	c.endScope()
	c.popExit()
	// Remove the discriminant.
}

// nameOf returns the name a declaration target implies, used to give an
// anonymous function the name of the variable it is assigned to.
func nameOf(target ast.Expr) string {
	if id, ok := target.(*ast.Ident); ok && !id.Paren() {
		return id.Name
	}
	return ""
}

// pushExit records something the statement being compiled leaves in place for
// the duration of its body.
func (c *compiler) pushExit(e pendingExit) {
	c.exits = append(c.exits, e)
	c.exitHandlers = append(c.exitHandlers, c.handlerDepth)
}

// popExit ends the statement that pushed the innermost exit.
func (c *compiler) popExit() {
	c.exits = c.exits[:len(c.exits)-1]
	c.exitHandlers = c.exitHandlers[:len(c.exitHandlers)-1]
}

// unwindExits undoes what the statements from exit top-1 down to exit down
// left in place, innermost first, and reports down. Each is undone once the
// handlers registered inside its statement are popped, so that an error
// closing an iterator is caught by a handler around the loop, as the loop
// is left before the handler's try is, and not by one inside it.
func (c *compiler) unwindExits(top, down int) int {
	for i := top - 1; i >= down; i-- {
		for c.handlerDepth > c.exitHandlers[i] {
			c.handlerDepth--
		}
	}
	return min(top, down)
}

// unwindTo walks what a break or continue owes on its way to target: the
// finally clauses it leaves, the exception handlers it jumps out from, and the
// operands and iterators the statements in between put there.
//
// Only what lies between the jump and the target is unwound. A finally clause
// whose try block contains the target is not being left at all -- in
//
//	try { for (;;) break } finally { ... }
//
// the break goes to the end of the loop, which is still inside the try, so the
// clause keeps its handler and does not run. Getting that wrong runs the clause
// early and pops a handler that is still in use.
func (c *compiler) unwindTo(target *loopCtx) {
	saved, savedDepth := c.finallys, c.handlerDepth
	savedExits, savedExitHandlers := c.exits, c.exitHandlers
	defer func() {
		c.finallys, c.handlerDepth = saved, savedDepth
		c.exits, c.exitHandlers = savedExits, savedExitHandlers
	}()
	top := len(c.exits)
	for i := len(saved) - 1; i >= target.finallys; i-- {
		// What statements inside the clause's try left in place is undone
		// before the clause runs, as they are left before the try is: a
		// for...of there closes its iterator first.
		top = c.unwindExits(top, max(saved[i].exits, target.exits))
		// Everything between here and the clause goes, the clause's own
		// handler included.
		for c.handlerDepth >= saved[i].handlers {
			c.handlerDepth--
		}
		// While a clause's body is being inlined it is no longer pending: a
		// break or continue written inside it leaves through the clauses
		// outside it, not through itself again.
		//
		// A completion record cannot express "jump to that label", so the
		// clause is compiled a second time at the jump site rather than being
		// routed through. Clauses are small and rarely nested, so the
		// duplication is bounded.
		//
		// Capped, so that a try statement inside the clause appends its
		// own entry to a copy rather than over saved[i], the next
		// clause outward, which this jump, or one compiled around it,
		// still has to inline.
		c.finallys = saved[:i:i]
		if saved[i].emit != nil {
			saved[i].emit()
			continue
		}
		// The clause's own statements see only the exits still in force,
		// so that a jump inside it does not undo again what this one has.
		c.exits = append([]pendingExit(nil), savedExits[:top]...)
		c.exitHandlers = append([]int(nil), savedExitHandlers[:top]...)
		// What the clause produces is not the jump's value, as for the
		// clause compileFinallyClause compiles: the try block's is, unless
		// the clause itself leaves abruptly.
		if c.completionSlot >= 0 {
			c.declare(fmt.Sprintf("%%cv%d", *c.hiddenCount), bindVar, 0)
			*c.hiddenCount++
		}
		end := c.reg.BeginFinally(saved[i].body)
		c.beginScope()
		savedRecord := c.finallyRecord
		c.finallyRecord = false
		c.compileStatements(saved[i].body)
		c.finallyRecord = savedRecord
		c.endScope()
		end()
		c.exits, c.exitHandlers = savedExits, savedExitHandlers
	}
	// What is left is the handler of a catch clause the jump leaves without
	// passing through a finally: a try whose block breaks out of a loop around
	// it. Nothing runs, but the handler still has to go, or a later throw
	// lands in a catch clause that was finished with.
	c.unwindExits(top, target.exits)
	for c.handlerDepth > target.handlers {
		c.handlerDepth--
	}
}

// atModuleTopLevel reports whether the compiler is in a module's outermost
// statement list, where bindings live in the module environment rather than in
// frame slots.
func (c *compiler) atModuleTopLevel() bool {
	return c.module != nil && c.parent == nil && c.depth == 0
}

// isModuleLex reports whether a name is one of the module's own top-level
// lexical bindings, declaring it here rather than assigning to it.
func (c *compiler) isModuleLex(name string) bool {
	return c.moduleLex != nil && c.moduleLex[name] && c.atModuleTopLevel()
}

// hoistableFunction returns the function declaration a statement contains and
// the name it binds, looking through an export wrapper and labels.
//
// `export default function () {}` is hoisted too, under the name the linker
// looks for rather than under one of its own. A labelled function declaration,
// `L: function f() {}`, which sloppy code may write, is the list's as an
// unlabelled one is.
func hoistableFunction(s ast.Stmt) (*ast.FuncDecl, string, bool) {
	switch n := s.(type) {
	case *ast.FuncDecl:
		if n.Fn.Name != nil {
			return n, n.Fn.Name.Name, true
		}
	case *ast.LabeledStmt:
		return hoistableFunction(n.Body)
	case *ast.ExportDecl:
		fd, ok := n.Decl.(*ast.FuncDecl)
		switch {
		case !ok:
		case fd.Fn.Name != nil:
			return fd, fd.Fn.Name.Name, true
		case n.Default:
			return fd, defaultBindingName, true
		}
	}
	return nil, "", false
}

// compileFieldInit creates one instance field.
//
// A field is created on the instance rather than written through it, so a
// setter the prototype happens to have for the same name is not called, and a
// private field is added rather than requiring one to be there already.
func (c *compiler) compileFieldInit(n *ast.FieldInit) {
	// A field initializer is a function of its own, even though it is compiled
	// into the constructor: it may read `super.x`, because its home object is
	// the class, but it may not call super() -- there is only one constructor
	// and it is not this.
	saved := c.inFieldInit
	c.inFieldInit = true
	defer func() { c.inFieldInit = saved }()

	if pn, private := n.Key.(*ast.PrivateName); private {
		c.privateName(pn, n.Start)
		c.compileExprNamed(n.Value, "#"+pn.Name)
		return
	}
	if !n.Computed {
		key := propKeyName(n.Key)
		c.compileExprNamed(n.Value, key)
		return
	}
	c.compileExpr(n.Key)
	c.compileExpr(n.Value)
}

// compileIterationBody walks the binding of a for-in/of head to each value
// and the body, in the scope the iteration has begun.
//
// A using head's value is disposed of at the end of each iteration, before
// the loop goes on or, if it is leaving, closes its iterator.
func (c *compiler) compileIterationBody(left ast.Node, body ast.Stmt) {
	switch l := left.(type) {
	case *ast.VarDecl:
		if l.Kind != ast.DeclVar {
			c.predeclareLexical(l)
		}
		if l.Kind.IsUsing() {
			c.compileDisposeScope(l.Start, l.Kind == ast.DeclAwaitUsing, func() {
				c.addDisposable(l.Kind, l.Start)
				c.initBinding(l.Decls[0].Target, l.Kind)
				c.compileStatement(body)
			})
			return
		}
		c.initBinding(l.Decls[0].Target, l.Kind)
	case ast.Expr:
		c.assignTo(l, false)
	}
	c.compileStatement(body)
}
