package compiler

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// compileStatements compiles a statement list, hoisting the function
// declarations it contains so that they are callable before their definition.
func (c *compiler) compileStatements(body []ast.Stmt) {
	c.hoistBlockDeclarations(body)
	for _, s := range body {
		c.compileStatement(s)
	}
}

// hoistBlockDeclarations creates the bindings a statement list declares, before
// any of it runs.
//
// It is separate from compiling the list because a switch's bindings belong to
// the whole case block rather than to one clause: they are created once, before
// the first case expression is evaluated, and the clauses are compiled one
// after another afterwards.
func (c *compiler) hoistBlockDeclarations(body []ast.Stmt) {
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
		for _, s := range body {
			if vd, ok := s.(*ast.VarDecl); ok && vd.Kind != ast.DeclVar {
				c.predeclareLexical(vd)
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
		if c.moduleBindingsDone {
			// A module's top-level functions were defined when it was linked,
			// which is before any of this runs.
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

// predeclareFunction emits a hoisted function declaration's definition into the
// binding that was already created for it.
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
			c.storeVar(h.name, h.fd.Start)
			return
		}
		c.emit(bytecode.OpDefineGlobalFunc, c.nameIdx(h.name),
			boolBit(c.opts.EvalConfigurable))
		return
	}

	// Annex B: a plain function declared inside a block is also assigned to a
	// var-scoped binding of the same name, which is what makes
	//
	//	function outer() { { function f() {} } return f; }
	//
	// work in sloppy mode. The binding is created by hoisting like any other
	// var; only the assignment happens here, when the declaration is reached.
	if !c.fn.Strict && c.depth > 0 && !h.fd.Fn.Async && !h.fd.Fn.Generator {
		c.emitAnnexBFunctionAlias(h.name)
	}
	c.emit(bytecode.OpSetLocal, h.slot, 0)
}

// emitAnnexBFunctionAlias copies a block-scoped function into the var binding
// that shares its name.
func (c *compiler) emitAnnexBFunctionAlias(name string) {
	// The innermost enclosing binding decides: a var of the same name is what
	// the alias writes to, and anything lexical suppresses it. The function's
	// own block-scoped binding is skipped, since it is the thing being aliased.
	for i := len(c.locals) - 1; i >= 0; i-- {
		if c.locals[i].name != name || c.locals[i].depth >= c.depth {
			continue
		}
		if c.locals[i].kind == bindVar {
			c.emit(bytecode.OpDup, 0, 0)
			c.emit(bytecode.OpSetLocal, c.locals[i].slot, 0)
		}
		return
	}
	// At a script's top level the var is a property of the global object
	// rather than a slot, and hoistGlobals has already created it. The store
	// consumes the copy, leaving the function for the binding it belongs to.
	if c.parent == nil {
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpSetGlobal, c.nameIdx(name), 0)
	}
}

// predeclareLexical creates the bindings of a let or const declaration in
// their uninitialized state.
func (c *compiler) predeclareLexical(vd *ast.VarDecl) {
	kind := bindLet
	if vd.Kind == ast.DeclConst {
		kind = bindConst
	}
	var names []string
	for _, d := range vd.Decls {
		collectPatternNames(d.Target, &names)
	}
	for _, n := range names {
		c.declareLexicalName(n, kind, vd.Start)
	}
}

// declareLexicalName declares a lexical binding and leaves it uninitialized.
func (c *compiler) declareLexicalName(name string, kind bindKind, pos int) {
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
		c.emitAt(pos, bytecode.OpDeclareGlobalLex, c.nameIdx(name), mutable)
		return
	}
	slot := c.declare(name, kind, pos)
	// The slot starts as the uninitialized marker, which the checked accessors
	// test for. Frame locals are cleared on entry, and the zero Value is the
	// number 0, so the marker has to be stored explicitly.
	c.emit(bytecode.OpPushUninitialized, 0, 0)
	c.emit(bytecode.OpSetLocal, slot, 0)
	c.markUninitialized(name)
}

// markUninitialized records that a binding is still in its dead zone.
func (c *compiler) markUninitialized(name string) {
	for i := len(c.locals) - 1; i >= 0; i-- {
		if c.locals[i].name == name {
			c.locals[i].initialized = false
			return
		}
	}
}

// resetCompletion sets the program's completion value to undefined, which the
// control-flow statements do before they run.
//
// They report undefined rather than nothing when their body produces no value,
// which is what makes `1; if (false) {}` undefined rather than 1 -- unlike a
// block or an empty statement, which produce nothing and leave the value alone.
func (c *compiler) resetCompletion() {
	if c.completionSlot < 0 {
		return
	}
	c.emit(bytecode.OpPushUndef, 0, 0)
	c.emit(bytecode.OpSetLocal, uint32(c.completionSlot), 0)
}

func (c *compiler) compileStatement(s ast.Stmt) {
	c.enter(s.Pos())
	defer c.leave()

	switch n := s.(type) {
	case *ast.ExprStmt:
		c.compileExpr(n.X)
		if c.completionSlot >= 0 {
			// At the top level an expression statement's value is the
			// program's completion value, which eval returns.
			c.emit(bytecode.OpSetLocal, uint32(c.completionSlot), 0)
		} else {
			c.emit(bytecode.OpDrop, 0, 0)
		}

	case *ast.VarDecl:
		c.compileVarDecl(n)

	case *ast.FuncDecl:
		// Already emitted by predeclareFunction.

	case *ast.ClassDecl:
		c.compileClass(n.Class, n.Class.Name.Name)
		c.assignTo(n.Class.Name, true)
		c.emit(bytecode.OpDrop, 0, 0)

	case *ast.BlockStmt:
		c.beginScope()
		c.compileStatements(n.Body)
		c.endScope()

	case *ast.EmptyStmt:

	case *ast.IfStmt:
		c.resetCompletion()
		c.compileIf(n)

	case *ast.WhileStmt:
		c.resetCompletion()
		c.compileWhile(n)

	case *ast.DoWhileStmt:
		c.resetCompletion()
		c.compileDoWhile(n)

	case *ast.ForStmt:
		c.resetCompletion()
		c.compileFor(n)

	case *ast.ForInStmt:
		c.resetCompletion()
		c.compileForIn(n)

	case *ast.ForOfStmt:
		c.resetCompletion()
		c.compileForOf(n)

	case *ast.ReturnStmt:
		if n.Arg != nil {
			c.compileExpr(n.Arg)
		} else {
			c.emit(bytecode.OpPushUndef, 0, 0)
		}
		if len(c.finallys) > 0 {
			// The finally clause must run before the function actually
			// returns, so the return becomes a completion record it consumes.
			// Every handler between here and the clause is left behind by the
			// jump, including the clause's own: a clause that was still
			// protected by itself would run a second time.
			ctx := &c.finallys[len(c.finallys)-1]
			for d := c.handlerDepth; d >= ctx.handlers; d-- {
				c.emit(bytecode.OpPopCatch, 0, 0)
			}
			c.emit(bytecode.OpPushInt, uint32(completionReturn), 0)
			c.emitAt(n.Start, bytecode.OpJump, 0, 0)
			c.finallys[len(c.finallys)-1].returns = append(
				c.finallys[len(c.finallys)-1].returns, c.here()-1)
			break
		}
		c.emitAt(n.Start, bytecode.OpReturn, 0, 0)

	case *ast.BreakStmt:
		c.compileBreak(n)

	case *ast.ContinueStmt:
		c.compileContinue(n)

	case *ast.ThrowStmt:
		c.compileExpr(n.Arg)
		c.emitAt(n.Start, bytecode.OpThrow, 0, 0)

	case *ast.TryStmt:
		c.resetCompletion()
		c.compileTry(n)

	case *ast.SwitchStmt:
		c.resetCompletion()
		c.compileSwitch(n)

	case *ast.LabeledStmt:
		c.compileLabeled(n)

	case *ast.DebuggerStmt:
		// No debugger is attached, so this is a no-op.

	case *ast.ImportDecl:
		c.compileImportDecl(n)

	case *ast.ExportDecl:
		c.compileExportDecl(n)

	case *ast.InstallPrivateMethods:
		c.compileIdentRead(&ast.Ident{Name: n.Binding, Start: n.Start})
		c.emitAt(n.Start, bytecode.OpInstallPrivateMethods, 0, 0)

	case *ast.FieldInit:
		c.compileFieldInit(n)

	case *ast.WithStmt:
		c.resetCompletion()
		c.compileWith(n)

	default:
		c.errorf(s.Pos(), "unsupported statement %T", s)
	}
}

// compileVarDecl compiles a var, let or const declaration.
func (c *compiler) compileVarDecl(n *ast.VarDecl) {
	for _, d := range n.Decls {
		if d.Init == nil {
			// `var x;` leaves an existing binding alone; a lexical binding
			// without an initializer becomes undefined.
			if n.Kind != ast.DeclVar {
				c.emit(bytecode.OpPushUndef, 0, 0)
				c.initBinding(d.Target, n.Kind)
			}
			continue
		}
		if id, ok := d.Target.(*ast.Ident); ok && n.Kind == ast.DeclVar &&
			c.withLimit(id.Name) > 0 {
			// A var's initializer is an assignment, and inside a `with` body
			// an assignment resolves its name before it evaluates its value:
			// an initializer that deletes the property still writes to the
			// object the name named. The value the store leaves is the
			// declaration's, which nothing reads.
			c.resolveWithRef(id)
			c.compileExprNamed(d.Init, id.Name)
			c.endWithRef(id)
			c.emit(bytecode.OpDrop, 0, 0)
			continue
		}
		c.compileExprNamed(d.Init, nameOf(d.Target))
		c.initBinding(d.Target, n.Kind)
	}
}

// initBinding stores the value on the stack into a declaration's target.
func (c *compiler) initBinding(target ast.Expr, kind ast.DeclKind) {
	if id, ok := target.(*ast.Ident); ok {
		if kind == ast.DeclVar {
			c.storeVar(id.Name, id.Start)
			return
		}
		// A lexical binding is initialized rather than assigned, which also
		// clears its dead zone.
		if l, ok := c.resolveLocal(id.Name); ok {
			c.emit(bytecode.OpInitLocal, l.slot, 0)
			c.markInitialized(id.Name)
			return
		}
		// A block of its own would have declared a local, so reaching here
		// means the binding is the top-level one.
		if c.globalLex[id.Name] {
			c.emit(bytecode.OpInitGlobalLex, c.nameIdx(id.Name), 0)
			return
		}
		if c.isModuleLex(id.Name) {
			c.emit(bytecode.OpInitModuleLex, c.nameIdx(id.Name), 0)
			return
		}
		c.emit(bytecode.OpSetGlobal, c.nameIdx(id.Name), 0)
		return
	}
	c.compileDestructuring(target, kind)
}

// storeVar assigns to a var binding or a global.
func (c *compiler) storeVar(name string, pos int) {
	// A var's initializer is an assignment rather than a binding
	// initialization: the declaration hoists out of a `with` body, but the
	// store happens inside it, where the object may be what is written to.
	// The probe peeks at the value and jumps over the static store, while the
	// store itself consumes it. A copy for the store and a drop after both
	// paths is what leaves the stack the same either way -- and a store that
	// quietly left its value behind would push the loop it sits in off its own
	// operands.
	probe := c.withProbe(bytecode.OpWithSet, name)
	if probe >= 0 {
		c.emit(bytecode.OpDup, 0, 0)
		defer func() {
			c.patchWithProbe(probe)
			c.emit(bytecode.OpDrop, 0, 0)
		}()
	}

	if l, ok := c.resolveLocal(name); ok {
		c.emit(bytecode.OpSetLocal, l.slot, 0)
		return
	}
	if idx, ok := c.resolveUpvalue(name); ok {
		if c.fn.Upvalues[idx].TDZ {
			c.emit(bytecode.OpSetUpvalueCheck, idx, 0)
		} else {
			c.emit(bytecode.OpSetUpvalue, idx, 0)
		}
		return
	}
	c.emit(bytecode.OpSetGlobal, c.nameIdx(name), 0)
}

func (c *compiler) compileIf(n *ast.IfStmt) {
	c.compileExpr(n.Test)
	elseJump := c.emitJump(bytecode.OpJumpIfFalse)
	c.compileStatement(n.Cons)

	if n.Alt == nil {
		c.patchJump(elseJump)
		return
	}
	endJump := c.emitJump(bytecode.OpJump)
	c.patchJump(elseJump)
	c.compileStatement(n.Alt)
	c.patchJump(endJump)
}

// pushLoop begins a loop context for break and continue.
//
// A pending label set by an enclosing labelled statement is consumed here, so
// that `outer: for (...)` gives the loop itself the label and `continue outer`
// reaches its update clause rather than its exit.
func (c *compiler) pushLoop(label string, isLoop bool) *loopCtx {
	if label == "" && c.pendingLabel != "" {
		label = c.pendingLabel
		c.pendingLabel = ""
	}
	c.loops = append(c.loops, loopCtx{
		label: label, isLoop: isLoop, scopeDepth: c.depth, exits: len(c.exits),
	})
	return &c.loops[len(c.loops)-1]
}

// popLoop ends a loop context, patching its break jumps to the current
// position and its continue jumps to continueTarget.
func (c *compiler) popLoop(continueTarget int) {
	l := c.loops[len(c.loops)-1]
	c.loops = c.loops[:len(c.loops)-1]
	for _, pc := range l.breaks {
		c.patchJump(pc)
	}
	for _, pc := range l.continues {
		c.patchJumpTo(pc, continueTarget)
	}
}

func (c *compiler) compileWhile(n *ast.WhileStmt) {
	start := c.here()
	c.pushLoop("", true)
	c.compileExpr(n.Test)
	exit := c.emitJump(bytecode.OpJumpIfFalse)
	c.compileStatement(n.Body)
	c.emit(bytecode.OpJump, uint32(start), 0)
	c.patchJump(exit)
	c.popLoop(start)
}

func (c *compiler) compileDoWhile(n *ast.DoWhileStmt) {
	start := c.here()
	c.pushLoop("", true)
	c.compileStatement(n.Body)
	testAt := c.here()
	c.compileExpr(n.Test)
	c.emit(bytecode.OpJumpIfTrue, uint32(start), 0)
	c.popLoop(testAt)
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

	if n.Init != nil {
		switch init := n.Init.(type) {
		case *ast.VarDecl:
			if init.Kind != ast.DeclVar {
				c.predeclareLexical(init)
				perIteration = true
			}
			c.compileVarDecl(init)
		case *ast.ExprStmt:
			c.compileExpr(init.X)
			c.emit(bytecode.OpDrop, 0, 0)
		}
	}

	if perIteration {
		// The first iteration gets its own copy too, before the test runs: a
		// closure made in the init clause keeps what the init left there,
		// rather than following what the loop goes on to do with it.
		c.emit(bytecode.OpCloseUpvalues, firstSlot, 0)
	}

	start := c.here()
	c.pushLoop("", true)

	exit := -1
	if n.Test != nil {
		c.compileExpr(n.Test)
		exit = c.emitJump(bytecode.OpJumpIfFalse)
	}
	c.compileStatement(n.Body)

	updateAt := c.here()
	if perIteration {
		// Snapshot the loop variable into any closure that captured it, before
		// the update clause changes it. This runs on the `continue` path too,
		// because that is also the end of an iteration.
		c.emit(bytecode.OpCloseUpvalues, firstSlot, 0)
	}
	if n.Update != nil {
		c.compileExpr(n.Update)
		c.emit(bytecode.OpDrop, 0, 0)
	}
	c.emit(bytecode.OpJump, uint32(start), 0)
	if exit >= 0 {
		c.patchJump(exit)
	}
	// `continue` jumps to the update clause, not the test.
	c.popLoop(updateAt)
	c.endScope()
}

func (c *compiler) compileForIn(n *ast.ForInStmt) {
	if c.beginHeadScope(n.Left) {
		defer c.endScope()
	}
	c.compileExpr(n.Right)
	c.emit(bytecode.OpForInStart, 0, 0)
	c.pushExit(exitCursor)
	c.compileForBody(n.Left, n.Body)
	c.popExit()
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
	return true
}

func (c *compiler) compileForOf(n *ast.ForOfStmt) {
	if c.beginHeadScope(n.Left) {
		defer c.endScope()
	}
	c.compileExpr(n.Right)
	if n.Await {
		c.emit(bytecode.OpForAwaitOfStart, 0, 0)
		c.pushExit(exitCursor)
		c.compileForAwaitBody(n.Left, n.Body)
		c.popExit()
		return
	}
	c.emit(bytecode.OpForOfStart, 0, 0)
	c.pushExit(exitCursor)
	c.compileForBody(n.Left, n.Body)
	c.popExit()
}

// compileForAwaitBody emits the loop of a `for await`, which differs from the
// synchronous form in that each step awaits the promise the iterator returns
// before unpacking the result.
func (c *compiler) compileForAwaitBody(left ast.Node, body ast.Stmt) {
	start := c.here()
	c.pushLoop("", true)

	c.emit(bytecode.OpAsyncIterNext, 0, 0)
	c.emitAwait(start)
	exit := c.emitJump(bytecode.OpIterResultOrJump)

	c.beginScope()
	switch l := left.(type) {
	case *ast.VarDecl:
		if l.Kind != ast.DeclVar {
			c.predeclareLexical(l)
		}
		c.initBinding(l.Decls[0].Target, l.Kind)
	case ast.Expr:
		c.assignTo(l, false)
		c.emit(bytecode.OpDrop, 0, 0)
	}
	c.compileStatement(body)
	c.endScope()

	c.emit(bytecode.OpJump, uint32(start), 0)
	c.patchJump(exit)
	c.popLoop(start)
	// A break arrives here with the iterator still open, so it is closed
	// before the cursor is dropped. An exhausted one is already closed and the
	// instruction leaves it alone.
	c.emit(bytecode.OpIterClose, 0, 0)
	c.emit(bytecode.OpDrop, 0, 0)
}

// compileForBody emits the shared loop structure of for-in and for-of, with the
// iterator already on the stack.
func (c *compiler) compileForBody(left ast.Node, body ast.Stmt) {
	start := c.here()
	c.pushLoop("", true)

	// IterNextOrJump advances the iterator, pushing the next value, or jumps
	// to the exit when it is exhausted.
	exit := c.emitJump(bytecode.OpIterNextOrJump)

	c.beginScope()
	switch l := left.(type) {
	case *ast.VarDecl:
		if l.Kind != ast.DeclVar {
			c.predeclareLexical(l)
		}
		c.initBinding(l.Decls[0].Target, l.Kind)
	case ast.Expr:
		c.assignTo(l, false)
		c.emit(bytecode.OpDrop, 0, 0)
	}
	c.compileStatement(body)
	c.endScope()

	c.emit(bytecode.OpJump, uint32(start), 0)
	c.patchJump(exit)
	c.popLoop(start)
	// ForOfStart left the cursor on the stack. A break arrives here with the
	// iterator still open, so it is closed before the cursor is dropped; an
	// exhausted one is already closed and the instruction leaves it alone.
	c.emit(bytecode.OpIterClose, 0, 0)
	c.emit(bytecode.OpDrop, 0, 0)
}

func (c *compiler) compileBreak(n *ast.BreakStmt) {
	for i := len(c.loops) - 1; i >= 0; i-- {
		if n.Label == "" || c.loops[i].label == n.Label {
			c.emitPendingFinallys()
			c.emitPendingExits(c.loops[i].exits)
			pc := c.emitJump(bytecode.OpJump)
			c.loops[i].breaks = append(c.loops[i].breaks, pc)
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
		if n.Label == "" || c.loops[i].label == n.Label {
			c.emitPendingFinallys()
			c.emitPendingExits(c.loops[i].exits)
			pc := c.emitJump(bytecode.OpJump)
			c.loops[i].continues = append(c.loops[i].continues, pc)
			return
		}
	}
	c.errorf(n.Start, "\"continue\" has no enclosing loop")
}

func (c *compiler) compileLabeled(n *ast.LabeledStmt) {
	// A labelled loop reuses the loop's own context so that `continue label`
	// reaches the loop's update clause rather than its exit.
	switch body := n.Body.(type) {
	case *ast.WhileStmt, *ast.DoWhileStmt, *ast.ForStmt, *ast.ForInStmt, *ast.ForOfStmt:
		c.compileLabeledLoop(n.Label, body)
	default:
		c.pushLoop(n.Label, false)
		c.compileStatement(n.Body)
		c.popLoop(c.here())
	}
}

// compileLabeledLoop compiles a loop whose own context carries the label.
func (c *compiler) compileLabeledLoop(label string, body ast.Stmt) {
	// The loop compilers push their context themselves, so the label is
	// applied by patching it immediately afterwards. To keep that simple the
	// label is recorded in a pending field the next pushLoop consumes.
	c.pendingLabel = label
	c.compileStatement(body)
}

// compileTry compiles a try statement.
//
// A finally clause has to run however the protected block finished, so the
// block's outcome is encoded as a completion record -- a value and a kind --
// left on the stack for the finally code to reproduce afterwards. That is what
// makes `return` inside a try still run the finally before returning.
func (c *compiler) compileTry(n *ast.TryStmt) {
	if n.Finally == nil {
		c.compileTryCatch(n)
		return
	}

	finallyHandler := c.emitJump(bytecode.OpPushFinally)
	c.handlerDepth++
	c.finallys = append(c.finallys, finallyCtx{body: n.Finally, handlers: c.handlerDepth})

	// finallyStart is filled in once the clause's first instruction is known.
	finallyStart := 0

	// The catch clause, when present, sits inside the finally's protection so
	// that a throw from the catch body still runs the finally.
	if n.Catch != nil {
		c.compileTryCatch(&ast.TryStmt{Block: n.Block, Catch: n.Catch, Start: n.Start})
	} else {
		c.beginScope()
		c.compileStatements(n.Block)
		c.endScope()
	}

	// Normal completion: drop the finally handler and fall into the clause
	// with a record saying nothing unusual happened.
	c.emit(bytecode.OpPopCatch, 0, 0)
	c.handlerDepth--
	c.emit(bytecode.OpPushUndef, 0, 0)
	c.emit(bytecode.OpPushInt, uint32(completionNormal), 0)
	finallyStart = c.here()

	// The handler jumps here too, having pushed its own record, and so do any
	// returns that were routed through the clause.
	c.patchJump(finallyHandler)
	ctx := c.finallys[len(c.finallys)-1]
	c.finallys = c.finallys[:len(c.finallys)-1]
	for _, pc := range ctx.returns {
		c.patchJumpTo(pc, finallyStart)
	}

	// What the finally clause produces is not the statement's value: the try
	// block's is, so `try { 1 } finally { 2 }` is 1.
	save := int32(-1)
	if c.completionSlot >= 0 {
		name := fmt.Sprintf("%%cv%d", *c.hiddenCount)
		*c.hiddenCount++
		save = int32(c.declare(name, bindVar, n.Start))
		c.emit(bytecode.OpGetLocal, uint32(c.completionSlot), 0)
		c.emit(bytecode.OpSetLocal, uint32(save), 0)
		// The clause starts with no value of its own. It matters only when it
		// leaves abruptly -- a break or a continue written inside it carries
		// what the clause itself produced, and undefined when it produced
		// nothing, rather than what the try block had produced.
		c.emit(bytecode.OpPushUndef, 0, 0)
		c.emit(bytecode.OpSetLocal, uint32(c.completionSlot), 0)
	}
	// The completion record sits beneath the clause's own operands, so a break
	// or continue leaving the clause has to drop it: what it jumps to is not
	// expecting two values it never pushed.
	c.pushExit(exitCompletion)
	c.beginScope()
	c.compileStatements(n.Finally)
	c.endScope()
	c.popExit()
	if save >= 0 {
		c.emit(bytecode.OpGetLocal, uint32(save), 0)
		c.emit(bytecode.OpSetLocal, uint32(c.completionSlot), 0)
	}

	// Reproduce the original completion: rethrow, return, or carry on.
	c.emit(bytecode.OpRethrow, 0, 0)
}

// compileTryCatch compiles a try statement that has no finally clause.
func (c *compiler) compileTryCatch(n *ast.TryStmt) {
	if n.Catch == nil {
		c.beginScope()
		c.compileStatements(n.Block)
		c.endScope()
		return
	}
	catchPC := c.emitJump(bytecode.OpPushCatch)
	c.handlerDepth++
	c.beginScope()
	c.compileStatements(n.Block)
	c.endScope()
	c.emit(bytecode.OpPopCatch, 0, 0)
	// The handler is gone from here on: the catch body runs after it fired,
	// and the code after the statement after it was popped.
	c.handlerDepth--
	skip := c.emitJump(bytecode.OpJump)

	c.patchJump(catchPC)
	c.beginScope()
	if n.Catch.Param != nil {
		// The thrown value is on the stack when the handler runs.
		c.bindCatchParam(n.Catch.Param)
	} else {
		c.emit(bytecode.OpDrop, 0, 0)
	}
	c.compileStatements(n.Catch.Body)
	c.endScope()
	c.patchJump(skip)
}

// bindCatchParam binds the caught value to the catch clause's parameter.
func (c *compiler) bindCatchParam(param ast.Expr) {
	if id, ok := param.(*ast.Ident); ok {
		slot := c.declare(id.Name, bindCatch, id.Start)
		c.emit(bytecode.OpSetLocal, slot, 0)
		// The store is what binds it, so the clause's body assigns to an
		// ordinary mutable binding rather than one still in a dead zone.
		c.markInitialized(id.Name)
		return
	}
	var names []string
	collectPatternNames(param, &names)
	for _, n := range names {
		c.declare(n, bindCatch, param.Pos())
	}
	c.compileDestructuring(param, ast.DeclLet)
}

func (c *compiler) compileSwitch(n *ast.SwitchStmt) {
	c.compileExpr(n.Disc)
	c.pushExit(exitDrop)
	c.beginScope()
	c.pushLoop("", false)

	// The bindings of every clause belong to the case block as a whole, and
	// they are created before the first case expression is evaluated: a
	// selector may close over one, and finds the block's binding rather than
	// whatever the same name means outside.
	var all []ast.Stmt
	for _, cs := range n.Cases {
		all = append(all, cs.Body...)
	}
	c.hoistBlockDeclarations(all)

	// Each case's test is compared against the discriminant, which stays on the
	// stack for the duration.
	bodyJumps := make([]int, len(n.Cases))
	defaultIdx := -1
	for i, cs := range n.Cases {
		if cs.Test == nil {
			defaultIdx = i
			bodyJumps[i] = -1
			continue
		}
		c.emit(bytecode.OpDup, 0, 0)
		c.compileExpr(cs.Test)
		c.emit(bytecode.OpStrictEq, 0, 0)
		bodyJumps[i] = c.emitJump(bytecode.OpJumpIfTrue)
	}

	// No case matched: go to the default clause if there is one, else past the
	// whole statement.
	var defaultJump int
	if defaultIdx >= 0 {
		defaultJump = c.emitJump(bytecode.OpJump)
	} else {
		defaultJump = c.emitJump(bytecode.OpJump)
	}

	// Clause bodies fall through to one another, which is why they are emitted
	// in order with no jumps between them.
	starts := make([]int, len(n.Cases))
	for i, cs := range n.Cases {
		starts[i] = c.here()
		for _, s := range cs.Body {
			c.compileStatement(s)
		}
	}
	end := c.here()

	for i := range n.Cases {
		if bodyJumps[i] >= 0 {
			c.patchJumpTo(bodyJumps[i], starts[i])
		}
	}
	if defaultIdx >= 0 {
		c.patchJumpTo(defaultJump, starts[defaultIdx])
	} else {
		c.patchJumpTo(defaultJump, end)
	}

	c.popLoop(end)
	c.endScope()
	c.popExit()
	// Remove the discriminant.
	c.emit(bytecode.OpDrop, 0, 0)
}

// nameOf returns the name a declaration target implies, used to give an
// anonymous function the name of the variable it is assigned to.
func nameOf(target ast.Expr) string {
	if id, ok := target.(*ast.Ident); ok && !id.Paren {
		return id.Name
	}
	return ""
}

// pushExit records something the statement being compiled leaves in place for
// the duration of its body.
func (c *compiler) pushExit(e pendingExit) { c.exits = append(c.exits, e) }

// popExit ends the statement that pushed the innermost exit, which emits its
// own undo on the path that falls out of the bottom.
func (c *compiler) popExit() { c.exits = c.exits[:len(c.exits)-1] }

// emitPendingExits undoes everything the statements between here and the jump's
// target left in place, innermost first.
func (c *compiler) emitPendingExits(down int) {
	for i := len(c.exits) - 1; i >= down; i-- {
		switch c.exits[i] {
		case exitCursor:
			c.emit(bytecode.OpIterClose, 0, 0)
			c.emit(bytecode.OpDrop, 0, 0)
		case exitDrop:
			c.emit(bytecode.OpDrop, 0, 0)
		case exitWith:
			c.emit(bytecode.OpWithPop, 0, 0)
		case exitCompletion:
			c.emit(bytecode.OpDrop, 0, 0)
			c.emit(bytecode.OpDrop, 0, 0)
		}
	}
}

// emitPendingFinallys inlines the body of every enclosing finally clause before
// a break or continue leaves it.
//
// A completion record cannot express "jump to that label", so the clause is
// compiled a second time at the jump site rather than being routed through.
// Finally clauses are small and rarely nested, so the duplication is bounded.
func (c *compiler) emitPendingFinallys() {
	saved, savedDepth := c.finallys, c.handlerDepth
	defer func() { c.finallys, c.handlerDepth = saved, savedDepth }()
	for i := len(saved) - 1; i >= 0; i-- {
		// Everything between here and the clause goes, the clause's own
		// handler included.
		for c.handlerDepth >= saved[i].handlers {
			c.emit(bytecode.OpPopCatch, 0, 0)
			c.handlerDepth--
		}
		// While a clause's body is being inlined it is no longer pending: a
		// break or continue written inside it leaves through the clauses
		// outside it, not through itself again.
		c.finallys = saved[:i]
		c.beginScope()
		c.compileStatements(saved[i].body)
		c.endScope()
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
// the name it binds, looking through an export wrapper.
//
// `export default function () {}` is hoisted too, under the name the linker
// looks for rather than under one of its own.
func hoistableFunction(s ast.Stmt) (*ast.FuncDecl, string, bool) {
	switch n := s.(type) {
	case *ast.FuncDecl:
		if n.Fn.Name != nil {
			return n, n.Fn.Name.Name, true
		}
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
		name, ref := c.privateName(pn, n.Start)
		c.emit(bytecode.OpPushThis, 0, 0)
		c.compileExprNamed(n.Value, "#"+pn.Name)
		c.emitAt(n.Start, bytecode.OpDefinePrivate, name, ref)
		c.emit(bytecode.OpDrop, 0, 0)
		return
	}
	c.emit(bytecode.OpPushThis, 0, 0)
	if !n.Computed {
		key := propKeyName(n.Key)
		c.emit(bytecode.OpPushConst, c.stringConst(key), 0)
		c.compileExprNamed(n.Value, key)
		c.emitAt(n.Start, bytecode.OpDefineIndex, 0, 0)
		c.emit(bytecode.OpDrop, 0, 0)
		return
	}
	c.compileExpr(n.Key)
	c.emit(bytecode.OpToPropertyKey, 0, 0)
	c.compileExpr(n.Value)
	// A field named by a computed key names its initializer too, but only once
	// the key is known.
	if isAnonymousFnDef(n.Value) {
		c.emit(bytecode.OpSetFuncName, 0, 0)
	}
	c.emitAt(n.Start, bytecode.OpDefineIndex, 0, 0)
	c.emit(bytecode.OpDrop, 0, 0)
}
