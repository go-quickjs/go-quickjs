// Package compiler turns an abstract syntax tree into bytecode.
//
// Compilation is a single pass over the tree, with one preliminary walk per
// function to hoist `var` and function declarations. The preliminary walk is
// unavoidable: `var` bindings and function declarations are visible from the
// top of the function regardless of where they appear, so their slots must
// exist before any statement is compiled.
//
// Variable references resolve in three steps -- a local of the current
// function, an upvalue captured from an enclosing one, or a global. The first
// two are resolved at compile time to a slot index, so the interpreter never
// looks a name up in a map.
package compiler

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Error is a compile-time error.
type Error struct {
	Msg  string
	Line int
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("SyntaxError: %s (line %d)", e.Msg, e.Line)
	}
	return "SyntaxError: " + e.Msg
}

// Options configures a compilation.
type Options struct {
	// Source names the origin of the code in stack traces.
	Source string
	// Text is the original source, retained so that Function.prototype.toString
	// can reproduce a function's text.
	Text string
	// EvalScope is set when compiling the code of a direct eval. It names the
	// bindings of the calling function, which the code may read and write: each
	// one it actually uses becomes an upvalue, which the interpreter binds to
	// the calling frame.
	EvalScope []bytecode.EvalBinding
	// PrivateNames are the private names of the classes enclosing a direct
	// eval's call site, which its code may refer to as the surrounding code
	// may.
	PrivateNames []string
	// EvalOwnVarScope marks eval code, whose top-level vars belong to the
	// evaluated code rather than to the global object when it is strict.
	EvalOwnVarScope bool
	// EvalConfigurable marks the bindings eval creates on the global object,
	// which are configurable where a script's are not: the evaluated code could
	// have declared them anywhere, so nothing should be able to rely on them.
	EvalConfigurable bool
}

// bindKind classifies a binding, which decides its initialization and
// assignment rules.
type bindKind uint8

const (
	bindVar bindKind = iota
	bindLet
	bindConst
	bindParam
	bindFunction
	// bindFunctionLexical is an async or generator function declared inside a
	// block. Annex B's tolerance of a repeated function declaration covers only
	// plain ones, so these collide like any other lexical binding.
	bindFunctionLexical
	// bindCatch is a catch clause's parameter, which behaves like let but may
	// be shadowed by a var of the same name.
	bindCatch
)

// localVar is a variable in the function being compiled.
type localVar struct {
	name  string
	kind  bindKind
	slot  uint32
	depth int
	// withDepth is how many `with` bodies enclosed the declaration. A binding
	// declared inside one is inner to it, so a reference to it is not probed
	// against that object.
	withDepth int
	// captured marks a local that an inner function closes over.
	captured bool
	// initialized is false while a let or const binding is in its temporal
	// dead zone, which lets the compiler emit the checked accessors only where
	// they are actually needed.
	initialized bool
}

// loopCtx tracks the jumps a break or continue must patch.
type loopCtx struct {
	// label names the statement, or is empty for an unlabelled loop.
	label string
	// isLoop distinguishes a loop, which continue may target, from a labelled
	// block or switch, which it may not.
	isLoop bool
	// breaks and continues hold the program counters of jumps awaiting a
	// target, since the target is not known until the loop is fully compiled.
	breaks    []int
	continues []int
	// scopeDepth is the depth to unwind to when the jump is taken.
	scopeDepth int
}

// finallyCtx tracks an enclosing finally clause.
type finallyCtx struct {
	// body is kept so that a break or continue leaving the clause can compile
	// it inline, which a completion record cannot express.
	body []ast.Stmt
	// returns holds the program counters of returns routed through the clause,
	// awaiting its start address.
	returns []int
}

// completionKind mirrors the runtime's encoding of how a protected block
// finished. The values must match those in the vm package.
const (
	completionNormal = 0
	completionThrow  = 1
	completionReturn = 2
)

// compiler holds the state for one function.
type compiler struct {
	fn     *bytecode.Function
	parent *compiler
	opts   Options

	locals   []localVar
	nextSlot uint32
	// hiddenCount names the compiler-generated bindings, which are spelled
	// with a character no identifier may contain so that nothing a script
	// writes can collide with one.
	hiddenCount int
	depth       int

	// inFieldInit marks the initializer of a class field, which is a function
	// of its own even though it is compiled into the constructor.
	inFieldInit bool

	// withDepth counts the `with` bodies enclosing the code being compiled.
	// While it is non-zero every name is probed against the objects before the
	// binding it would otherwise resolve to.
	withDepth int

	// privateScopes is the stack of class bodies enclosing the code being
	// compiled, innermost last, each holding the private names it declares. A
	// reference to a name in none of them is a syntax error rather than a
	// runtime one, which is why it is tracked here and not in the runtime.
	privateScopes [][]string

	// nameIndex and constIndex deduplicate the tables, so that a name or
	// constant used many times costs one entry.
	nameIndex  map[string]uint32
	constIndex map[constKey]uint32

	loops []loopCtx
	// finallys is the stack of enclosing finally clauses, which return, break
	// and continue all have to account for.
	finallys []finallyCtx
	// pendingLabel carries a label from a labelled statement to the loop it
	// labels, which is the next context pushed.
	pendingLabel string

	// module is non-nil while compiling module code, collecting the shape the
	// linker needs.
	module *ModuleInfo

	// selfName is the name a named function expression uses to refer to
	// itself, which resolves to the running closure rather than to a binding.
	selfName string

	// completionSlot holds the local that accumulates the program's completion
	// value -- what eval returns -- or -1 inside a function, where the
	// completion value is whatever `return` produces instead.
	//
	// Each expression statement stores into it rather than discarding its
	// value, so the last one evaluated wins even when it sits inside a block or
	// a loop.
	completionSlot int32

	// stackDepth tracks the operand stack so that MaxStack can be recorded and
	// the frame sized exactly once per function.
	stackDepth int
	maxStack   int

	// recursionDepth bounds how deeply the compiler descends into the tree. The
	// parser caps nesting too, but a function body nests independently of its
	// caller's, so the limit is enforced on both sides.
	recursionDepth int

	// lastLine avoids emitting a duplicate line entry for every instruction.
	lastLine int32
	// lineOf maps a byte offset to a line, supplied by the caller.
	lineOf func(pos int) int32
}

// constKey identifies a constant for deduplication. Functions are never
// deduplicated, since each occurrence is a distinct template.
type constKey struct {
	kind bytecode.ConstKind
	num  float64
	str  string
}

// Compile compiles a parsed program into a top-level function.
func Compile(prog *ast.Program, opts Options) (fn *bytecode.Function, err error) {
	c := newCompiler(nil, opts)
	c.fn.Name = "<main>"
	c.fn.Strict = prog.Strict
	c.fn.IsModule = prog.Module
	c.fn.Source = opts.Source
	c.lineOf = lineMapper(opts.Text)

	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*Error); ok {
				fn, err = nil, e
				return
			}
			panic(r)
		}
	}()

	// Reserve the completion-value slot before anything else, so that its index
	// is stable, and seed it with undefined for a program whose last statement
	// produces no value.
	c.completionSlot = int32(c.nextSlot)
	c.nextSlot++
	c.emit(bytecode.OpPushUndef, 0, 0)
	c.emit(bytecode.OpSetLocal, uint32(c.completionSlot), 0)

	// A direct eval's code is inside the class bodies its call site was inside,
	// so their private names are in scope for it.
	if len(opts.PrivateNames) > 0 {
		c.privateScopes = append(c.privateScopes, opts.PrivateNames)
	}

	// Top-level var and function declarations become properties of the global
	// object rather than locals, which is what makes them visible to other
	// scripts in the same realm.
	c.checkScopes(prog.Body)
	c.hoistGlobals(prog.Body)
	c.compileStatements(prog.Body)

	c.emit(bytecode.OpGetLocal, uint32(c.completionSlot), 0)
	c.emit(bytecode.OpReturn, 0, 0)
	c.finish()
	return c.fn, nil
}

func newCompiler(parent *compiler, opts Options) *compiler {
	c := &compiler{
		fn:         &bytecode.Function{Source: opts.Source},
		parent:     parent,
		opts:       opts,
		nameIndex:  make(map[string]uint32, 8),
		constIndex: make(map[constKey]uint32, 8),
		// A function has no completion value of its own; only the top-level
		// program tracks one, and Compile overwrites this.
		completionSlot: -1,
	}
	if parent != nil {
		c.lineOf = parent.lineOf
		// A function written inside a `with` body resolves the names in its own
		// body against the objects too, so it is compiled the same way.
		c.withDepth = parent.withDepth
	}
	return c
}

// finish records the tables the interpreter needs.
func (c *compiler) finish() {
	c.fn.LocalCount = int(c.nextSlot)
	c.fn.MaxStack = c.maxStack + 4 // headroom for the interpreter's own pushes
	c.fn.Locals = make([]bytecode.LocalDesc, c.nextSlot)
	for _, l := range c.locals {
		if int(l.slot) < len(c.fn.Locals) {
			c.fn.Locals[l.slot] = bytecode.LocalDesc{
				Name:     l.name,
				Mutable:  l.kind != bindConst,
				TDZ:      l.kind == bindLet || l.kind == bindConst,
				Captured: l.captured,
			}
		}
	}
}

// lineMapper returns a function mapping a byte offset to a 1-based line.
//
// The line starts are precomputed once so that mapping a position is a binary
// search rather than a scan, which matters because the compiler maps a position
// for nearly every statement.
func lineMapper(text string) func(int) int32 {
	if text == "" {
		return func(int) int32 { return 0 }
	}
	starts := make([]int, 1, 64)
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return func(pos int) int32 {
		lo, hi := 0, len(starts)-1
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if starts[mid] <= pos {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		return int32(lo + 1)
	}
}

// ---------------------------------------------------------------------------
// Emitting
// ---------------------------------------------------------------------------

// emit appends an instruction and returns its program counter.
func (c *compiler) emit(op bytecode.Op, a, b uint32) int {
	c.fn.Code = append(c.fn.Code, bytecode.Instr{Op: op, A: a, B: b})
	c.adjustStack(op, a, b)
	return len(c.fn.Code) - 1
}

// emitAt emits an instruction and records its source line.
func (c *compiler) emitAt(pos int, op bytecode.Op, a, b uint32) int {
	if c.lineOf != nil {
		if line := c.lineOf(pos); line != c.lastLine {
			c.fn.Lines = append(c.fn.Lines, bytecode.SourceLoc{
				PC: uint32(len(c.fn.Code)), Line: line,
			})
			c.lastLine = line
		}
	}
	return c.emit(op, a, b)
}

// adjustStack tracks the operand stack depth so the frame can be sized.
func (c *compiler) adjustStack(op bytecode.Op, a, b uint32) {
	c.stackDepth += stackEffect(op, a, b)
	if c.stackDepth > c.maxStack {
		c.maxStack = c.stackDepth
	}
	if c.stackDepth < 0 {
		// A negative depth means the compiler emitted an unbalanced sequence,
		// which is a bug here rather than in the input.
		c.stackDepth = 0
	}
}

// maxCompileDepth bounds recursion over the syntax tree. A goroutine stack
// overflow cannot be caught, so deeply nested input is rejected instead.
const maxCompileDepth = 1000

// enter increases the recursion depth, failing if the tree is too deep.
func (c *compiler) enter(pos int) {
	c.recursionDepth++
	if c.recursionDepth > maxCompileDepth {
		c.errorf(pos, "the expression nests too deeply")
	}
}

func (c *compiler) leave() { c.recursionDepth-- }

// emitJump emits a jump with a placeholder target and returns its program
// counter so that patchJump can fill it in.
func (c *compiler) emitJump(op bytecode.Op) int {
	return c.emit(op, 0xFFFFFFFF, 0)
}

// patchJump points a previously emitted jump at the current position.
func (c *compiler) patchJump(pc int) {
	c.fn.Code[pc].A = uint32(len(c.fn.Code))
}

// patchJumpTo points a jump at a specific position.
func (c *compiler) patchJumpTo(pc, target int) {
	c.fn.Code[pc].A = uint32(target)
}

// here returns the current program counter, for backward jumps.
func (c *compiler) here() int { return len(c.fn.Code) }

// errorf reports a compile error.
func (c *compiler) errorf(pos int, format string, args ...any) {
	line := 0
	if c.lineOf != nil {
		line = int(c.lineOf(pos))
	}
	panic(&Error{Msg: fmt.Sprintf(format, args...), Line: line})
}

// ---------------------------------------------------------------------------
// Names and constants
// ---------------------------------------------------------------------------

// nameIdx returns the index of a name in the function's name table.
func (c *compiler) nameIdx(name string) uint32 {
	if i, ok := c.nameIndex[name]; ok {
		return i
	}
	c.fn.Names = append(c.fn.Names, name)
	i := uint32(len(c.fn.Names) - 1)
	c.nameIndex[name] = i
	return i
}

// numberConst returns the index of a numeric constant.
func (c *compiler) numberConst(v float64) uint32 {
	return c.constIdx(constKey{kind: bytecode.ConstNumber, num: v},
		bytecode.Constant{Kind: bytecode.ConstNumber, Num: v})
}

// stringConst returns the index of a string constant.
func (c *compiler) stringConst(s string) uint32 {
	return c.constIdx(constKey{kind: bytecode.ConstString, str: s},
		bytecode.Constant{Kind: bytecode.ConstString, Str: s})
}

func (c *compiler) constIdx(key constKey, val bytecode.Constant) uint32 {
	if i, ok := c.constIndex[key]; ok {
		return i
	}
	c.fn.Constants = append(c.fn.Constants, val)
	i := uint32(len(c.fn.Constants) - 1)
	c.constIndex[key] = i
	return i
}

// addConst appends a constant without deduplication, for function templates
// and regular expressions where each occurrence is distinct.
func (c *compiler) addConst(val bytecode.Constant) uint32 {
	c.fn.Constants = append(c.fn.Constants, val)
	return uint32(len(c.fn.Constants) - 1)
}

// ---------------------------------------------------------------------------
// Scopes and bindings
// ---------------------------------------------------------------------------

func (c *compiler) beginScope() { c.depth++ }

// endScope discards the bindings of the innermost scope, closing any upvalues
// that captured them.
func (c *compiler) endScope() {
	c.depth--
	// Find the first local belonging to the scope being closed.
	i := len(c.locals)
	needsClose := false
	for i > 0 && c.locals[i-1].depth > c.depth {
		if c.locals[i-1].captured {
			needsClose = true
		}
		i--
	}
	if needsClose {
		c.emit(bytecode.OpCloseUpvalues, c.locals[i].slot, 0)
	}
	// Slots are not reclaimed: reusing them would let a closure created in one
	// iteration of a loop observe a later iteration's value.
	c.locals = c.locals[:i]
}

// declare adds a binding to the current scope and returns its slot.
func (c *compiler) declare(name string, kind bindKind, pos int) uint32 {
	// A redeclaration in the same scope is an error for lexical bindings.
	// A function declaration inside a block is a lexical binding and collides
	// like one. At a function's own top level it is var-scoped instead, which
	// is why `function f(){} function f(){}` is legal there and not in a block.
	lexicalFn := (kind == bindFunction || kind == bindFunctionLexical) && c.depth > 0
	if (kind != bindVar && kind != bindFunction && kind != bindFunctionLexical) || lexicalFn {
		for i := len(c.locals) - 1; i >= 0 && c.locals[i].depth == c.depth; i-- {
			if c.locals[i].name != name {
				continue
			}
			// Two plain function declarations in one sloppy-mode block name the
			// same binding rather than colliding, which Annex B requires and
			// the web depends on.
			if !c.fn.Strict && kind == bindFunction && c.locals[i].kind == bindFunction {
				return c.locals[i].slot
			}
			c.errorf(pos, "identifier %q has already been declared", name)
		}
	}
	// A `var` that names an existing binding in the same function reuses it.
	// A var colliding with a lexical binding is rejected by checkScopes before
	// compilation begins, since the rule is about the shape of the source
	// rather than the order bindings happen to be created in.
	if kind == bindVar {
		for i := range c.locals {
			if c.locals[i].name == name && c.locals[i].kind == bindVar {
				return c.locals[i].slot
			}
		}
	}

	slot := c.nextSlot
	c.nextSlot++
	c.locals = append(c.locals, localVar{
		name:      name,
		kind:      kind,
		slot:      slot,
		depth:     c.depth,
		withDepth: c.withDepth,
		initialized: kind == bindVar || kind == bindParam ||
			kind == bindFunction || kind == bindFunctionLexical,
	})
	return slot
}

// markInitialized clears a lexical binding's dead zone.
func (c *compiler) markInitialized(name string) {
	for i := len(c.locals) - 1; i >= 0; i-- {
		if c.locals[i].name == name {
			c.locals[i].initialized = true
			return
		}
	}
}

// resolveLocal finds a binding in the current function.
func (c *compiler) resolveLocal(name string) (*localVar, bool) {
	for i := len(c.locals) - 1; i >= 0; i-- {
		if c.locals[i].name == name {
			return &c.locals[i], true
		}
	}
	return nil, false
}

// resolveUpvalue finds a binding in an enclosing function, adding the upvalue
// descriptors needed to thread it down through every intervening function.
func (c *compiler) resolveUpvalue(name string) (uint32, bool) {
	if c.parent == nil {
		// The code of a direct eval has no enclosing compiler, but it does have
		// an enclosing frame. Its bindings are declared as upvalues by name,
		// and the interpreter matches them to the frame's slots.
		for _, b := range c.opts.EvalScope {
			if b.Name != name {
				continue
			}
			return c.addUpvalue(name, b.Index, b.FromLocal, b.Mutable, b.TDZ, 0), true
		}
		return 0, false
	}
	if l, ok := c.parent.resolveLocal(name); ok {
		l.captured = true
		return c.addUpvalue(name, l.slot, true, l.kind != bindConst,
			l.kind == bindLet || l.kind == bindConst, l.withDepth), true
	}
	// Not a local of the parent, so look further out and forward the result.
	if idx, ok := c.parent.resolveUpvalue(name); ok {
		desc := c.parent.fn.Upvalues[idx]
		return c.addUpvalue(name, idx, false, desc.Mutable, desc.TDZ, desc.WithDepth), true
	}
	return 0, false
}

// addUpvalue appends an upvalue descriptor, reusing an existing one for the
// same source so that a name captured twice shares a slot.
func (c *compiler) addUpvalue(name string, index uint32, fromParent, mutable, tdz bool,
	withDepth int) uint32 {
	for i, u := range c.fn.Upvalues {
		if u.Index == index && u.FromParent == fromParent && u.Name == name {
			return uint32(i)
		}
	}
	c.fn.Upvalues = append(c.fn.Upvalues, bytecode.UpvalueDesc{
		FromParent: fromParent,
		Index:      index,
		Name:       name,
		Mutable:    mutable,
		TDZ:        tdz,
		WithDepth:  withDepth,
	})
	return uint32(len(c.fn.Upvalues) - 1)
}

// ---------------------------------------------------------------------------
// Hoisting
// ---------------------------------------------------------------------------

// hoistGlobals declares top-level var and function bindings on the global
// object, which is where script-level declarations live.
func (c *compiler) hoistGlobals(body []ast.Stmt) {
	var names []string
	collectVarNamesIn(body, &names, c.fn.Strict)
	for _, n := range names {
		if c.evalVarsAreLocal() {
			// Strict eval code gets a variable environment of its own, so its
			// vars are bindings of the evaluated code and vanish with it.
			// Which is the point: strict mode is where `eval` stops being able
			// to reach into its surroundings.
			c.declare(n, bindVar, 0)
			continue
		}
		// A var declared by eval is configurable, unlike one a script declares,
		// because the evaluated code could have declared it anywhere.
		c.emit(bytecode.OpDefineGlobalVar, c.nameIdx(n), boolBit(c.opts.EvalConfigurable))
	}
}

// evalVarsAreLocal reports whether a top-level var belongs to the evaluated
// code rather than to the global object.
func (c *compiler) evalVarsAreLocal() bool {
	return c.opts.EvalOwnVarScope && c.fn.Strict
}

func boolBit(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// collectVarNames gathers the names bound by `var` and by function
// declarations, descending through every construct that does not introduce a
// new function scope.
func collectVarNames(body []ast.Stmt, out *[]string) {
	collectVarNamesIn(body, out, false)
}

// collectVarNamesIn gathers the names a function's var-scoped bindings cover.
//
// Real `var` declarations and top-level function declarations are
// unconditional. A function declared inside a block is included only because of
// Annex B, which gives it a var-scoped alias -- and only where that alias would
// be legal: a `let` of the same name anywhere between the block and the
// function top level suppresses it, and strict mode suppresses it outright.
func collectVarNamesIn(body []ast.Stmt, out *[]string, strict bool) {
	c := &varCollector{out: *out, strict: strict}
	c.stmts(body)
	*out = c.out
}

// varCollector walks a function body tracking the lexical scopes in effect, so
// that Annex B's alias can be suppressed where an enclosing block already binds
// the name.
type varCollector struct {
	out    []string
	strict bool
	// lexical is the stack of enclosing block scopes, innermost last. It is
	// empty at the function's own top level, where a function declaration is
	// var-scoped outright rather than by Annex B.
	lexical [][]lexName
}

func (c *varCollector) add(name string) { c.out = append(c.out, name) }

// stmts walks a statement list as one lexical scope.
func (c *varCollector) stmts(list []ast.Stmt) {
	for _, s := range list {
		c.stmt(s)
	}
}

// block walks a nested statement list, which is a scope of its own.
func (c *varCollector) block(list []ast.Stmt) {
	var names []lexName
	for _, s := range list {
		names = lexicalNamesOf(s, names, true)
	}
	c.lexical = append(c.lexical, names)
	c.stmts(list)
	c.lexical = c.lexical[:len(c.lexical)-1]
}

// nested walks a single statement that stands where a block could.
func (c *varCollector) nested(s ast.Stmt) {
	if s == nil {
		return
	}
	if b, ok := s.(*ast.BlockStmt); ok {
		c.block(b.Body)
		return
	}
	c.stmt(s)
}

// shadowed reports whether an enclosing block binds the name lexically.
func (c *varCollector) shadowed(name string) bool {
	for _, scope := range c.lexical {
		for i := range scope {
			if scope[i].name == name {
				return true
			}
		}
	}
	return false
}

func (c *varCollector) stmt(s ast.Stmt) {
	switch n := s.(type) {
	case *ast.VarDecl:
		if n.Kind != ast.DeclVar {
			return
		}
		var names []string
		for _, d := range n.Decls {
			collectPatternNames(d.Target, &names)
		}
		c.out = append(c.out, names...)

	case *ast.FuncDecl:
		if n.Fn.Name == nil {
			return
		}
		name := n.Fn.Name.Name
		if len(c.lexical) == 0 {
			// At the function's own top level the binding is var-scoped
			// outright, whatever the mode.
			c.add(name)
			return
		}
		// Inside a block it exists only by Annex B, which strict mode does not
		// grant and which an async or generator declaration never gets.
		if c.strict || n.Fn.Async || n.Fn.Generator || c.shadowed(name) {
			return
		}
		c.add(name)

	case *ast.BlockStmt:
		c.block(n.Body)
	case *ast.IfStmt:
		c.nested(n.Cons)
		c.nested(n.Alt)
	case *ast.ForStmt:
		c.forScope(n.Init, n.Body)
	case *ast.ForInStmt:
		c.forScope(stmtOrNil(n.Left), n.Body)
	case *ast.ForOfStmt:
		c.forScope(stmtOrNil(n.Left), n.Body)
	case *ast.WhileStmt:
		c.nested(n.Body)
	case *ast.DoWhileStmt:
		c.nested(n.Body)
	case *ast.TryStmt:
		c.block(n.Block)
		if n.Catch != nil {
			c.block(n.Catch.Body)
		}
		if n.Finally != nil {
			c.block(n.Finally)
		}
	case *ast.SwitchStmt:
		// All the clauses share one block scope.
		var body []ast.Stmt
		for _, cs := range n.Cases {
			body = append(body, cs.Body...)
		}
		c.block(body)
	case *ast.LabeledStmt:
		c.nested(n.Body)
	case *ast.WithStmt:
		c.nested(n.Body)
	case *ast.ExportDecl:
		// A declaration wrapped in an export still binds its names.
		if n.Decl != nil {
			c.stmt(n.Decl)
		}
	}
}

// forScope walks a loop head and body as one scope, which is what a `let` in
// the head is scoped to.
func (c *varCollector) forScope(init ast.Stmt, body ast.Stmt) {
	var names []lexName
	if vd, ok := init.(*ast.VarDecl); ok && vd.Kind != ast.DeclVar {
		for _, d := range vd.Decls {
			names = patternNames(d.Target, names)
		}
	}
	c.lexical = append(c.lexical, names)
	if init != nil {
		c.stmt(init)
	}
	c.nested(body)
	c.lexical = c.lexical[:len(c.lexical)-1]
}

// stmtOrNil unwraps a for-in or for-of head that may not be a statement.
func stmtOrNil(n ast.Node) ast.Stmt {
	if s, ok := n.(ast.Stmt); ok {
		return s
	}
	return nil
}

// collectPatternNames gathers the identifiers a binding pattern introduces.
func collectPatternNames(target ast.Expr, out *[]string) {
	switch n := target.(type) {
	case *ast.Ident:
		*out = append(*out, n.Name)
	case *ast.ArrayPattern:
		for _, el := range n.Elements {
			if el != nil {
				collectPatternNames(el, out)
			}
		}
		if n.Rest != nil {
			collectPatternNames(n.Rest, out)
		}
	case *ast.ObjectPattern:
		for _, p := range n.Props {
			collectPatternNames(p.Value, out)
		}
		if n.Rest != nil {
			collectPatternNames(n.Rest, out)
		}
	case *ast.AssignPattern:
		collectPatternNames(n.Target, out)
	case *ast.RestElement:
		collectPatternNames(n.Arg, out)
	}
}

// stackEffect returns how much an opcode changes the operand stack depth.
//
// It exists so that MaxStack can be computed without simulating execution. An
// overestimate merely wastes a few slots per frame; an underestimate would
// corrupt the stack, so anything uncertain is rounded up.
func stackEffect(op bytecode.Op, a, b uint32) int {
	switch op {
	case bytecode.OpPushConst, bytecode.OpPushUndef, bytecode.OpPushNull,
		bytecode.OpPushTrue, bytecode.OpPushFalse, bytecode.OpPushThis,
		bytecode.OpPushInt, bytecode.OpPushEmptyString,
		bytecode.OpPushUninitialized, bytecode.OpGetLocal,
		bytecode.OpGetLocalCheck, bytecode.OpGetUpvalue,
		bytecode.OpGetUpvalueCheck, bytecode.OpGetGlobal,
		bytecode.OpGetGlobalOpt, bytecode.OpDup, bytecode.OpClosure,
		bytecode.OpNewObject, bytecode.OpNewTarget, bytecode.OpImportMeta,
		bytecode.OpPushCallee,
		bytecode.OpGetArguments, bytecode.OpRestParam, bytecode.OpDeleteVar,
		bytecode.OpGetSuperProp,
		bytecode.OpGetPropThis,
		bytecode.OpIsNullish:
		return 1

	case bytecode.OpDup2:
		return 2

	case bytecode.OpDrop, bytecode.OpSetLocal, bytecode.OpSetLocalCheck,
		bytecode.OpInitLocal, bytecode.OpSetUpvalue, bytecode.OpSetUpvalueCheck,
		bytecode.OpInitUpvalue, bytecode.OpSetGlobal, bytecode.OpThrow,
		bytecode.OpReturn, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue,
		bytecode.OpDefineGlobalFunc, bytecode.OpArrayPush,
		bytecode.OpDefineField, bytecode.OpDefineGetter,
		bytecode.OpDefineSetter, bytecode.OpSetProtoOf,
		bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep,
		bytecode.OpJumpIfNotNullish:
		return -1

	case bytecode.OpJumpIfNullish:
		// Optional chaining keeps the tested value on both paths.
		return 0

	case bytecode.OpAsyncIterNext:
		return 1
	case bytecode.OpIterResultOrJump:
		// Pops the result and pushes the value when it continues.
		return 0

	case bytecode.OpIterNextOrJump:
		// Pushes the next value when it continues and nothing when it stops;
		// the larger figure is the one MaxStack needs.
		return 1

	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv,
		bytecode.OpMod, bytecode.OpPow, bytecode.OpBitAnd, bytecode.OpBitOr,
		bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr,
		bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe,
		bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe,
		bytecode.OpIn, bytecode.OpInstanceOf, bytecode.OpGetIndex,
		bytecode.OpDeleteProp, bytecode.OpDefineIndex:
		return -1

	case bytecode.OpSetProp, bytecode.OpSetIndex:
		// SetProp pops the object and the value; SetIndex also pops the key.
		if op == bytecode.OpSetIndex {
			return -3
		}
		return -2

	case bytecode.OpGetIndexThis:
		return 0

	case bytecode.OpCall:
		// Pops the callee and its arguments, pushes the result.
		return -int(a)
	case bytecode.OpCallMethod:
		return -int(a) - 1
	case bytecode.OpNew:
		return -int(a)
	case bytecode.OpNewArray, bytecode.OpConcat:
		return -int(a) + 1

	case bytecode.OpCallSpread:
		// Pops the receiver, the callee and the argument array.
		return -2
	case bytecode.OpNewSpread:
		return -1
	case bytecode.OpSpreadIter:
		return -1
	case bytecode.OpObjectRest:
		return -int(a)
	case bytecode.OpSuperCall:
		// Pops the arguments, or the argument array in the spread form.
		if b != 0 {
			return -1
		}
		return -int(a) + 1
	case bytecode.OpNewClass:
		// Pops the parent, leaving the constructor.
		return -1
	case bytecode.OpYield, bytecode.OpAwait:
		// Pops the operand and pushes the resumption value.
		return 0

	case bytecode.OpDefineMethod, bytecode.OpDefinePrivate:
		return -1
	case bytecode.OpSetHomeObject, bytecode.OpSetFuncName:
		// Both read the stack without changing it.
		return 0
	case bytecode.OpDefineGetterIndex, bytecode.OpDefineSetterIndex:
		// Pops the key and the function, leaving the target.
		return -2
	case bytecode.OpSetPrivate:
		return -2
	case bytecode.OpGetPrivate:
		return 0
	case bytecode.OpRethrow:
		// Pops the completion record.
		return -2

	case bytecode.OpInsert2, bytecode.OpInsert3, bytecode.OpInsert4:
		return 1
	}
	// Everything else leaves the depth unchanged.
	return 0
}
