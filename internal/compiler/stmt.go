package compiler

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// compileStatements compiles a statement list, hoisting the function
// declarations it contains so that they are callable before their definition.
func (c *compiler) compileStatements(body []ast.Stmt) {
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
	for _, s := range body {
		if fd, ok := hoistableFunction(s); ok {
			c.predeclareFunction(fd)
		}
	}
	for _, s := range body {
		c.compileStatement(s)
	}
}

// predeclareFunction creates the binding for a hoisted function declaration
// and emits its definition up front.
func (c *compiler) predeclareFunction(fd *ast.FuncDecl) {
	name := fd.Fn.Name.Name
	c.compileFunctionLiteral(fd.Fn, name)
	if c.parent == nil && c.depth == 0 && !c.evalVarsAreLocal() {
		c.emit(bytecode.OpDefineGlobalFunc, c.nameIdx(name), boolBit(c.opts.EvalConfigurable))
		return
	}
	kind := bindFunction
	if fd.Fn.Async || fd.Fn.Generator {
		kind = bindFunctionLexical
	}
	slot := c.declare(name, kind, fd.Start)

	// Annex B: a plain function declared inside a block is also assigned to a
	// var-scoped binding of the same name, which is what makes
	//
	//	function outer() { { function f() {} } return f; }
	//
	// work in sloppy mode. The binding is created by hoisting like any other
	// var; only the assignment happens here, when the declaration is reached.
	if !c.fn.Strict && c.depth > 0 && kind == bindFunction {
		c.emitAnnexBFunctionAlias(name)
	}
	c.emit(bytecode.OpSetLocal, slot, 0)
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
	// rather than a slot, and hoistGlobals has already created it.
	if c.parent == nil {
		c.emit(bytecode.OpDup, 0, 0)
		c.emit(bytecode.OpSetGlobal, c.nameIdx(name), 0)
		c.emit(bytecode.OpDrop, 0, 0)
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
		if n.Arg != nil {
			c.compileExpr(n.Arg)
		} else {
			c.emit(bytecode.OpPushUndef, 0, 0)
		}
		if len(c.finallys) > 0 {
			// The finally clause must run before the function actually
			// returns, so the return becomes a completion record it consumes.
			c.emit(bytecode.OpPopCatch, 0, 0)
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
		c.compileTry(n)

	case *ast.SwitchStmt:
		c.compileSwitch(n)

	case *ast.LabeledStmt:
		c.compileLabeled(n)

	case *ast.DebuggerStmt:
		// No debugger is attached, so this is a no-op.

	case *ast.ImportDecl:
		c.compileImportDecl(n)

	case *ast.ExportDecl:
		c.compileExportDecl(n)

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
		if d.Init == nil {
			// `var x;` leaves an existing binding alone; a lexical binding
			// without an initializer becomes undefined.
			if n.Kind != ast.DeclVar {
				c.emit(bytecode.OpPushUndef, 0, 0)
				c.initBinding(d.Target, n.Kind)
			}
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
	probe := c.withProbe(bytecode.OpWithSet, name)
	defer c.patchWithProbe(probe)

	if l, ok := c.resolveLocal(name); ok {
		c.emit(bytecode.OpSetLocal, l.slot, 0)
		return
	}
	if idx, ok := c.resolveUpvalue(name); ok {
		c.emit(bytecode.OpSetUpvalue, idx, 0)
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
	c.loops = append(c.loops, loopCtx{label: label, isLoop: isLoop, scopeDepth: c.depth})
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
	c.compileExpr(n.Right)
	c.emit(bytecode.OpForInStart, 0, 0)
	c.compileForBody(n.Left, n.Body)
}

func (c *compiler) compileForOf(n *ast.ForOfStmt) {
	c.compileExpr(n.Right)
	if n.Await {
		c.emit(bytecode.OpForAwaitOfStart, 0, 0)
		c.compileForAwaitBody(n.Left, n.Body)
		return
	}
	c.emit(bytecode.OpForOfStart, 0, 0)
	c.compileForBody(n.Left, n.Body)
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
	c.finallys = append(c.finallys, finallyCtx{body: n.Finally})

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

	c.beginScope()
	c.compileStatements(n.Finally)
	c.endScope()

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
	c.beginScope()
	c.compileStatements(n.Block)
	c.endScope()
	c.emit(bytecode.OpPopCatch, 0, 0)
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
	c.beginScope()
	c.pushLoop("", false)

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
		c.compileStatements(cs.Body)
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
	// Remove the discriminant.
	c.emit(bytecode.OpDrop, 0, 0)
}

// nameOf returns the name a declaration target implies, used to give an
// anonymous function the name of the variable it is assigned to.
func nameOf(target ast.Expr) string {
	if id, ok := target.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// emitPendingFinallys inlines the body of every enclosing finally clause before
// a break or continue leaves it.
//
// A completion record cannot express "jump to that label", so the clause is
// compiled a second time at the jump site rather than being routed through.
// Finally clauses are small and rarely nested, so the duplication is bounded.
func (c *compiler) emitPendingFinallys() {
	for i := len(c.finallys) - 1; i >= 0; i-- {
		c.emit(bytecode.OpPopCatch, 0, 0)
		c.beginScope()
		c.compileStatements(c.finallys[i].body)
		c.endScope()
	}
}

// atModuleTopLevel reports whether the compiler is in a module's outermost
// statement list, where bindings live in the module environment rather than in
// frame slots.
func (c *compiler) atModuleTopLevel() bool {
	return c.module != nil && c.parent == nil && c.depth == 0
}

// hoistableFunction returns the function declaration a statement contains,
// looking through an export wrapper.
func hoistableFunction(s ast.Stmt) (*ast.FuncDecl, bool) {
	switch n := s.(type) {
	case *ast.FuncDecl:
		if n.Fn.Name != nil {
			return n, true
		}
	case *ast.ExportDecl:
		if n.Decl != nil {
			if fd, ok := n.Decl.(*ast.FuncDecl); ok && fd.Fn.Name != nil {
				return fd, true
			}
		}
	}
	return nil, false
}

// compileFieldInit creates one instance field.
//
// A field is created on the instance rather than written through it, so a
// setter the prototype happens to have for the same name is not called, and a
// private field is added rather than requiring one to be there already.
func (c *compiler) compileFieldInit(n *ast.FieldInit) {
	if pn, private := n.Key.(*ast.PrivateName); private {
		c.emit(bytecode.OpPushThis, 0, 0)
		c.compileExpr(n.Value)
		c.emitAt(n.Start, bytecode.OpDefinePrivate, c.nameIdx("#"+pn.Name), 0)
		c.emit(bytecode.OpDrop, 0, 0)
		return
	}
	c.emit(bytecode.OpPushThis, 0, 0)
	if n.Computed {
		c.compileExpr(n.Key)
		c.emit(bytecode.OpToPropertyKey, 0, 0)
	} else {
		c.emit(bytecode.OpPushConst, c.stringConst(propKeyName(n.Key)), 0)
	}
	c.compileExpr(n.Value)
	c.emitAt(n.Start, bytecode.OpDefineIndex, 0, 0)
	c.emit(bytecode.OpDrop, 0, 0)
}
