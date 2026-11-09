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
	"slices"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Error is a compile-time error.
type Error struct {
	Msg  string
	Line int
	// Col is the column on Line, when the error has a position. Line and Col
	// are placed in the file as Options.LineOffset and ColumnOffset place the
	// source.
	Col int
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
	// LineOffset and ColumnOffset place the source within a larger file, for
	// the positions a stack trace reports.
	LineOffset, ColumnOffset int
	// NodeQuirks compiles as V8 does where it departs from the standard.
	// Annex B then hoists a block-level function named arguments over the
	// arguments object, and one whose name a function in an enclosing block
	// already binds.
	NodeQuirks bool
	// Text is the original source, retained so that Function.prototype.toString
	// can reproduce a function's text.
	Text string
	// EvalScope is set when compiling the code of a direct eval. It names the
	// bindings of the calling function, which the code may read and write: each
	// one it actually uses becomes an upvalue, which the interpreter binds to
	// the calling frame.
	EvalScope []bytecode.EvalBinding
	// EvalWithDepth is how many `with` bodies the call site is inside. The
	// evaluated code is inside them too, so a name it mentions is looked for
	// in their objects before the binding it would otherwise mean.
	EvalWithDepth int
	// PrivateNames are the private names of the classes enclosing a direct
	// eval's call site, which its code may refer to as the surrounding code
	// may, each with the hidden binding that holds its key.
	PrivateNames []bytecode.EvalPrivateName
	// ArgumentNames is what the parameter scope binds, when the eval's call
	// site is a parameter default. A var the evaluated code declares may not
	// collide with one of them.
	ArgumentNames []string
	// EvalOwnVarScope marks eval code, whose top-level vars belong to the
	// evaluated code rather than to the global object when it is strict.
	EvalOwnVarScope bool
	// InFieldInit marks a direct eval whose call site is a class field
	// initializer, where `arguments` has no meaning and new.target is
	// undefined -- the initializer is a function of its own, even though it is
	// compiled into the constructor.
	InFieldInit bool
	// EvalVarScopeIsGlobal says whether the call site's own vars are properties
	// of the global object, which decides where the evaluated code's go.
	EvalVarScopeIsGlobal bool
	// EvalConfigurable marks the bindings eval creates on the global object,
	// which are configurable where a script's are not: the evaluated code could
	// have declared them anywhere, so nothing should be able to rely on them.
	EvalConfigurable bool
	// The contexts a direct eval inherits from its call site, which an eval
	// nested inside it inherits in turn. The parser is told the same thing;
	// these are here so that the compiler can pass them on.
	AllowSuperProp bool
	AllowSuperCall bool
	AllowNewTarget bool
	// Debug compiles for a debugger: each statement begins with an
	// OpDebugStmt, a place a debugger can stop, and the function records
	// what is in scope there, as a direct eval's call site does. The code is
	// for one runtime: the scopes capture what a debugger may read.
	Debug bool
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
	// bindFuncSelf is the name a function expression gives itself. It is
	// immutable like a const, but an assignment to it outside strict mode is
	// quietly discarded rather than refused -- which is the one place the two
	// kinds of immutable binding differ.
	bindFuncSelf
)

// localVar is a variable in the function being compiled.
type localVar struct {
	name  string
	kind  bindKind
	slot  uint32
	depth int
	// previous is the preceding binding of this name, as an index plus one,
	// when localIndex is in use. Scope exit restores that binding.
	previous int
	// withDepth is how many `with` bodies enclosed the declaration. A binding
	// declared inside one is inner to it, so a reference to it is not probed
	// against that object.
	withDepth int
	// captured marks a local that an inner function closes over.
	captured bool
	// read says a read of the binding has been compiled, and appended that
	// an addition to it may grow what it holds in place, so that every read
	// compiled from then on is followed by a str_freeze (ownedAppend,
	// localAppend). copied says each iteration of a loop has a copy of the
	// binding, which holds the same string.
	read, appended, copied bool
	// stringy says the binding has been given a string by what the source
	// shows (stringly): what a local must have been to be appended to in
	// place, which spares a sum its str_freezes.
	stringy bool
	// paramScoped marks a binding that belongs to the parameter scope rather
	// than to the body. A function with parameter expressions has two, and its
	// body may declare a name the parameter scope already binds: the arguments
	// object is in the parameter scope, so `function f(p = 0) { let arguments }`
	// shadows it rather than colliding with it.
	paramScoped bool
	// initialized is false while a let or const binding is in its temporal
	// dead zone, which lets the compiler emit the checked accessors only where
	// they are actually needed.
	initialized bool
	// jumpedOver marks a binding of a switch's case block, which a case can
	// be entered past the declaration of: it stays in its dead zone to the
	// compiler, whatever has been compiled, so every access is checked.
	jumpedOver bool
	// lit is a const's initializer where it is a literal -- null, a boolean,
	// a number or a string -- which every read of it once it is
	// initialized gives (constLiteral).
	lit ast.Expr
}

// varScoped reports whether a binding belongs to its function's variable scope
// rather than to a block inside it, which is what a var declared by a direct
// eval can share and what a lexical one refuses.
func (l *localVar) varScoped() bool {
	switch l.kind {
	case bindVar, bindParam:
		return true
	case bindFunction:
		// A function declared at a function's top level is var-scoped; one in
		// a block is a lexical binding that Annex B also aliases to a var.
		return l.depth == 0
	}
	return false
}

// lexical reports whether a binding is one a var may not be hoisted over.
func (l *localVar) lexical() bool {
	switch l.kind {
	case bindLet, bindConst, bindFunctionLexical:
		return true
	case bindFunction:
		return l.depth > 0
	}
	return false
}

// loopCtx tracks the jumps a break or continue must patch.
type loopCtx struct {
	// label names the statement, or is empty for an unlabelled loop.
	label string
	// isLoop distinguishes a loop, which continue may target, from a labelled
	// block or switch, which it may not.
	isLoop bool
	// labelOnly marks a labelled statement other than a loop -- a block, an
	// if, a switch's label -- which only a break naming it may target: a
	// break without a label goes to the innermost loop or switch, past it.
	labelOnly bool
	// breaks and continues hold the program counters of jumps awaiting a
	// target, since the target is not known until the loop is fully compiled.
	breaks    []int
	continues []int
	// scopeDepth is the depth to unwind to when the jump is taken.
	scopeDepth int
	// exits is how much of the enclosing exit stack is still in force here, so
	// that a jump out of an inner statement undoes what it left behind.
	exits int
	// finallys and handlers are how much of the enclosing finally stack and
	// handler stack were in force when the statement began. A jump to this
	// target unwinds down to these and no further: a finally clause whose try
	// block contains the target is not owed a run, and an exception handler
	// that encloses the target is not one the jump is leaving.
	finallys int
	handlers int
}

// pendingExit is something a statement left in place for the duration of its
// body, which a jump out of that body has to undo.
//
// A `break` or `continue` leaves by jumping, so the instructions the statement
// would have run on its way out are skipped -- and an iterator would go
// unclosed, an operand would be stranded on the stack, or a `with` object would
// stay on the scope chain.
type pendingExit uint8

const (
	// exitCursor is a for-in or for-of cursor: closed, then dropped.
	exitCursor pendingExit = iota
	// exitDrop is an operand, which is only a switch's discriminant.
	exitDrop
	// exitWith is an object on the frame's `with` chain.
	exitWith
	// exitCompletion is the record a finally clause is running for: the value
	// and the kind of the completion it interrupted. A break or continue out
	// of the clause abandons that completion, and its two operands with it.
	exitCompletion
)

// finallyCtx tracks an enclosing finally clause.
type finallyCtx struct {
	// body is kept so that a break or continue leaving the clause can compile
	// it inline, which a completion record cannot express. emit, where body
	// is nil, emits the clause instead: a for await's close.
	body []ast.Stmt
	emit func()
	// returns holds the program counters of returns routed through the clause,
	// awaiting its start address.
	returns []int
	// handlers is how deep the handler stack was once this clause's own was
	// pushed. A jump into the clause has to pop everything above that, and the
	// clause's own handler with it -- the clause is not protected by itself.
	handlers int
	// exits is how many exits were in force when the clause's try began:
	// those above were pushed by statements inside it, which a jump leaving
	// them undoes before the clause runs.
	exits int
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
	// lit is the function being compiled, where it has one, and
	// mentionsEval whether it or anything in it mentions eval, once asked:
	// 0 not yet, 1 no, 2 yes.
	lit          *ast.FuncLit
	mentionsEval uint8
	// lengthRead says the name being compiled is x of x.length (compiledRead).
	lengthRead bool
	// mentioned is, of the names asked about, whether a function or a
	// class inside this one mentions them.
	mentioned map[string]bool

	// traceName is the name V8's inferrer would hand the next function or
	// class literal compiled (tracename.go), and traceIn the one the
	// expression being compiled was handed.
	traceName, traceIn string

	// lazyArguments is set for a function whose arguments object is made
	// only where a call of apply needs it, in lazyArgumentsSlot.
	lazyArguments     bool
	lazyArgumentsSlot uint32

	locals     []localVar
	localIndex map[string]int
	nextSlot   uint32
	// paramCaptures are the closures' captures of this function's
	// parameters that may take the value (UpvalueDesc's ByValue), which
	// finish settles once every store to them is compiled.
	paramCaptures []paramCapture
	// hiddenCount names the compiler-generated bindings, which are spelled
	// with a character no identifier may contain so that nothing a script
	// writes can collide with one.
	//
	// It is shared by every compiler of one compilation, because a nested
	// function resolves such a name against its enclosing functions like any
	// other: two of them numbered independently would collide, and the inner
	// one would shadow the outer one it was meant to reach.
	hiddenCount *int
	depth       int

	// paramScopeNames is what the parameter scope binds while a parameter
	// default is being compiled, and nil elsewhere. A direct eval there cannot
	// declare a var named by one of them.
	paramScopeNames []string

	// inFieldInit marks the initializer of a class field, which is a function
	// of its own even though it is compiled into the constructor.
	inFieldInit bool

	// moduleBindingsDone marks the compilation of a module's body, whose
	// declarations were compiled into the separate function the linker runs.
	moduleBindingsDone bool
	// withDepth counts the `with` bodies enclosing the code being compiled.
	// While it is non-zero every name is probed against the objects before the
	// binding it would otherwise resolve to.
	withDepth int

	// privateScopes is the stack of class bodies enclosing the code being
	// compiled, innermost last, each holding the private names it declares. A
	// reference to a name in none of them is a syntax error rather than a
	// runtime one, which is why it is tracked here and not in the runtime.
	privateScopes [][]privateBinding

	// nameIndex and constIndex deduplicate the tables, so that a name or
	// constant used many times costs one entry.
	nameIndex  map[string]uint32
	constIndex map[constKey]uint32

	loops []loopCtx
	// reused are the function expressions of the function's var declarations
	// in a loop that are made once per invocation (reusedClosures).
	reused map[*ast.FuncLit]bool
	// finallys is the stack of enclosing finally clauses, which return, break
	// and continue all have to account for.
	finallys []finallyCtx
	// finallyRecord is set while a finally clause is compiled as the clause
	// proper, with its completion record beneath it, rather than inlined at a
	// break or continue.
	finallyRecord bool
	// finallyBodies is how many finally clauses are being compiled around
	// the code being compiled, which compileTryFinally inlines no more.
	finallyBodies int
	// disposeScopes are the enclosing scopes with using declarations,
	// innermost last.
	disposeScopes []disposeScope
	// tailCall is the call being compiled in tail position, which is emitted
	// as a tail call. Only that node is: a call nested in its arguments is an
	// ordinary one.
	tailCall ast.Expr
	// handlerDepth is how many exception handlers enclose the position being
	// compiled. A jump out of them has to pop each one, since the instruction
	// it lands on is not the one that would have.
	handlerDepth int
	// globalLex names the script-level lexical bindings this compilation
	// declared, which live in the global lexical environment rather than in a
	// frame slot.
	globalLex map[string]bool
	// moduleLex names a module's top-level lexical bindings, which live in the
	// module environment. They are declared in their dead zone, so the
	// declaration compiles to an initialization rather than an assignment.
	moduleLex map[string]bool
	// pendingLocals holds what is known about each slot, by slot, gathered
	// as scopes close so that a binding is still described after it has gone
	// out of scope.
	pendingLocals []bytecode.LocalDesc
	// withStatements counts the enclosing `with` statements, which is not the
	// same as withDepth: a function containing a direct eval has a scope of
	// its own on that chain without any `with` being written.
	withStatements int
	// litNames names the consts given a literal (localVar.lit) in this
	// function and its makers before it was made, shared with the
	// functions made after them (constLiteral); nil while there are none.
	litNames map[string]bool
	// evalOwnVars names the vars this eval's code declares in the calling
	// function, which have no slot there. A reference to one resolves to the
	// binding the declaration made rather than to whatever the name meant
	// outside the function.
	evalOwnVars map[string]bool
	// annexB holds the block-level function declarations Annex B also assigns
	// to a var of the same name, decided once when the vars are hoisted so
	// that the assignment is made exactly where the binding was.
	annexB map[*ast.FuncDecl]bool
	// exits records what the statements currently being compiled left in place
	// for the duration of their bodies, innermost last, and exitHandlers how
	// deep the handler stack was when each was pushed: a handler above that
	// was registered inside the statement, and goes before it is undone.
	exits        []pendingExit
	exitHandlers []int
	// returnSlot is a hidden local a return routed through a finally clause
	// parks its value in while the statements it leaves are undone, or -1.
	returnSlot int32
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

	// targets marks, by position, what something jumps to, which is what
	// says whether the instruction last emitted is the end of one path.
	targets []bool

	// localsUsed is the most locals has held when a scope left it: what
	// release clears.
	localsUsed int
	// localNames has a bit for each name locals has held (nameBit): most
	// lookups are of names a function does not bind, a global's or an
	// enclosing function's, which it spares a scan of the list.
	localNames uint64

	// scratch is where the tables above, the code and the function's own
	// tables are built, taken from pool and given back when the function is
	// done.
	scratch *scratch
	pool    *scratchPool

	// recursionDepth bounds how deeply the compiler descends into the tree. The
	// parser caps nesting too, but a function body nests independently of its
	// caller's, so the limit is enforced on both sides.
	recursionDepth int

	// script is the source being compiled, shared by every function in it.
	script *bytecode.Script
	// lastPos avoids emitting a duplicate position entry for every
	// instruction.
	lastPos int32
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
	c.script.Comments = (*bytecode.SourceComments)(prog.Comments)
	c.fn.Name = "<main>"
	c.fn.TopLevel = true
	c.fn.Strict = prog.Strict
	c.fn.IsModule = prog.Module
	c.fn.Source = opts.Source
	c.inFieldInit = opts.InFieldInit
	// Eval code is inside whatever `with` bodies its call site is inside, so a
	// name it mentions is probed against their objects first.
	c.withDepth = opts.EvalWithDepth
	if opts.InFieldInit && containsArgumentsInStmts(prog.Body) {
		return nil, &Error{Msg: "\"arguments\" is not allowed in a class field initializer"}
	}

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

	c.checkEvalVarNames(prog.Body)

	// Top-level var and function declarations become properties of the global
	// object rather than locals, which is what makes them visible to other
	// scripts in the same realm.
	c.checkScopes(prog.Body)
	// Every declaration is checked against the global environment before any
	// of them is created, so that a script whose declarations cannot all be
	// honoured leaves none of them behind.
	c.checkGlobalLexicals(prog.Body)
	c.checkGlobalVars(prog.Body)
	c.hoistGlobals(prog.Body)
	c.compileStatements(prog.Body)

	c.emit(bytecode.OpGetLocal, uint32(c.completionSlot), 0)
	c.emit(bytecode.OpReturn, 0, 0)
	c.finish()
	return c.done(), nil
}

// scratch is what compiling one function grows as it goes: its code and
// tables, before they are copied out at their size, and the maps and lists
// that only the compiling needs. A script's functions are compiled one inside
// another, so a few of these serve them all, in turn.
type scratch struct {
	state      compiler
	code       []bytecode.Instr
	lines      []bytecode.SourceLoc
	names      []string
	consts     []bytecode.Constant
	upvalues   []bytecode.UpvalueDesc
	locals     []localVar
	pending    []bytecode.LocalDesc
	targets    []bool
	captures   []paramCapture
	nameIndex  map[string]uint32
	constIndex map[constKey]uint32
	localIndex map[string]int
}

// scratchPool holds the scratch no function being compiled has, and the
// lists of jumps no condition being compiled has.
type scratchPool struct {
	free  []*scratch
	jumps [][]int
}

// jumpList is an empty list for a condition's jumps (emitTestJumps), which
// putJumps gives back. It cannot be kept on a caller's stack: the compiler's
// functions all call one another, and the escape analysis takes what any
// of them does with a result as what all of them do.
func (p *scratchPool) jumpList() []int {
	if n := len(p.jumps); n > 0 {
		s := p.jumps[n-1]
		p.jumps = p.jumps[:n-1]
		return s
	}
	return make([]int, 0, 4)
}

func (p *scratchPool) putJumps(s []int) { p.jumps = append(p.jumps, s[:0]) }

// scratchPools keep a compile's scratch for the next, so that each script
// does not grow its buffers from nothing. A compile takes a pool of its own:
// separate runtimes compile at once.
var scratchPools = sync.Pool{New: func() any { return &scratchPool{} }}

func (p *scratchPool) get() *scratch {
	if n := len(p.free); n > 0 {
		s := p.free[n-1]
		p.free = p.free[:n-1]
		return s
	}
	return &scratch{
		// Room for a small function's whole body, which saves growing the
		// code from nothing a doubling at a time.
		code:       make([]bytecode.Instr, 0, 32),
		nameIndex:  make(map[string]uint32, 8),
		constIndex: make(map[constKey]uint32, 8),
	}
}

func newCompiler(parent *compiler, opts Options) *compiler {
	var pool *scratchPool
	if parent != nil {
		pool = parent.pool
	} else {
		pool = scratchPools.Get().(*scratchPool)
	}
	s := pool.get()
	c := &s.state
	*c = compiler{
		fn: &bytecode.Function{
			Source:    opts.Source,
			Code:      s.code[:0],
			Lines:     s.lines[:0],
			Names:     s.names[:0],
			Constants: s.consts[:0],
			Upvalues:  s.upvalues[:0],
		},
		parent:        parent,
		opts:          opts,
		lastPos:       -1,
		locals:        s.locals[:0],
		pendingLocals: s.pending[:0],
		targets:       s.targets[:0],
		paramCaptures: s.captures[:0],
		nameIndex:     s.nameIndex,
		constIndex:    s.constIndex,
		scratch:       s,
		pool:          pool,
		// A function has no completion value of its own; only the top-level
		// program tracks one, and Compile overwrites this.
		completionSlot: -1,
		returnSlot:     -1,
	}
	if parent != nil {
		c.hiddenCount = parent.hiddenCount
		c.script = parent.script
		// A function written inside a `with` body resolves the names in its own
		// body against the objects too, so it is compiled the same way.
		c.withDepth = parent.withDepth
		c.withStatements = parent.withStatements
		c.litNames = parent.litNames
	} else {
		c.hiddenCount = new(int)
		c.script = bytecode.NewScript(opts.Source, opts.Text)
		c.script.SetOffset(int32(opts.LineOffset), int32(opts.ColumnOffset))
	}
	c.fn.Script = c.script
	if opts.Debug {
		c.fn.Debug = &bytecode.FunctionDebug{}
	}
	return c
}

// Source bytes a function takes for each instruction of its code, and for
// each position it records, at the least the code a program has seen does.
const (
	bytesPerInstr = 4
	bytesPerLine  = 8
	// reserveMin is the source a function must span for its tables to be
	// sized from it: a smaller one is served by the scratch as it is.
	reserveMin = 1 << 10
)

// reserve gives the function's code and positions room for what n bytes of
// its own source compile to, in the one step a large function would
// otherwise grow to it in many, its whole code copied at each. A function
// written inside it is no part of n: the wrapper of a bundle has the whole
// bundle in its span and little code of its own.
func (c *compiler) reserve(n int) {
	if n < reserveMin {
		return
	}
	c.fn.Code = slices.Grow(c.fn.Code, n/bytesPerInstr)
	if c.script.HasText() {
		c.fn.Lines = slices.Grow(c.fn.Lines, n/bytesPerLine)
	}
}

// finish records the tables the interpreter needs, and what the JIT and
// the VM's native calls ask of the code (recordFacts).
func (c *compiler) finish() {
	c.fn.LocalCount = int(c.nextSlot)
	c.fn.MaxStack = c.maxStack + 4 // headroom for the interpreter's own pushes
	c.fn.Locals = make([]bytecode.LocalDesc, c.nextSlot)
	for _, l := range c.locals {
		c.recordLocal(l)
	}
	copy(c.fn.Locals, c.pendingLocals)
	facts := c.recordFacts()
	if len(c.paramCaptures) != 0 && !facts.eval {
		c.settleParamCaptures(facts)
	}
	if c.opts.Debug {
		c.finishDebugScopes()
	}
}

// codeFacts is what recordFacts found of a function's finished code that
// settleParamCaptures asks: the slots of its first 64 locals an
// instruction stores to (bytecode.WrittenLocals), those a closure it makes
// may assign (UpvalueDesc's Assigned), and whether it has a direct eval.
type codeFacts struct {
	written, closures uint64
	eval              bool
}

// What recordFacts notes of an instruction, by its opcode (opFacts): one
// of these, and, for an opcode whose instruction stores to a local
// (bytecode.WrittenLocals), factStoresA, for a store to the slot in A
// alone, or factStores.
const (
	factPrivate    = 1 + iota // a private member's read or write, which has a cache
	factSuperRef              // a super reference, through the home object
	factPushCallee            // the function object pushed
	factSuperCall             // super(), which constructs with new.target
	factNewTarget             // new.target
	factSetUpvalue            // a store to an upvalue
	factEval                  // a direct eval
	factArguments             // the arguments object made, or handed to apply
	factYieldStar             // a yield*

	factStores  = 0x40
	factStoresA = 0x80
)

// opFacts is what recordFacts notes of an instruction of each opcode, 0
// for nothing, as most are, for which a load is all the walk costs: a
// switch of each instruction's opcode, and WrittenLocals' of each, had
// made compiling Three.js's and Babel's bundles 2.2% and 3.6% slower.
var opFacts = [256]uint8{
	bytecode.OpGetPrivate:      factPrivate,
	bytecode.OpSetPrivate:      factPrivate,
	bytecode.OpGetSuperProp:    factSuperRef,
	bytecode.OpGetSuperIndex:   factSuperRef,
	bytecode.OpSetSuperProp:    factSuperRef,
	bytecode.OpSetSuperIndex:   factSuperRef,
	bytecode.OpSuperBase:       factSuperRef,
	bytecode.OpPushCallee:      factPushCallee,
	bytecode.OpSuperCall:       factSuperCall,
	bytecode.OpNewTarget:       factNewTarget,
	bytecode.OpSetUpvalue:      factSetUpvalue,
	bytecode.OpSetUpvalueCheck: factSetUpvalue,
	bytecode.OpInitUpvalue:     factSetUpvalue,
	bytecode.OpDirectEval:      factEval,
	bytecode.OpTailDirectEval:  factEval,
	bytecode.OpGetArguments:    factArguments,
	bytecode.OpLazyArguments:   factArguments | factStoresA,
	bytecode.OpApplyArguments:  factArguments | factStoresA,
	bytecode.OpYieldStar:       factYieldStar,

	bytecode.OpSetLocal:            factStoresA,
	bytecode.OpPutLocal:            factStoresA,
	bytecode.OpSetLocalCheck:       factStoresA,
	bytecode.OpInitLocal:           factStoresA,
	bytecode.OpClearLocal:          factStoresA,
	bytecode.OpSetLocalGet:         factStoresA,
	bytecode.OpIncLocal:            factStoresA,
	bytecode.OpDecLocal:            factStoresA,
	bytecode.OpUpdateLocal:         factStoresA,
	bytecode.OpInitParam:           factStoresA,
	bytecode.OpArgumentsIndex:      factStoresA,
	bytecode.OpGetLocalIndexUpdate: factStores,
	bytecode.OpParamsToDeadZone:    factStores,
}

// recordFacts records, in one walk of the function's finished code and
// one of its closures', what the JIT and the VM's native calls ask of it
// (bytecode.Function's FrameCallee, ReadsMaker, AssignsParams,
// MakesArguments, SuperForwards and HasYieldStar, and its upvalues'
// Assigned), and counts its private members' caches: each read and write
// of one has a cache of its own, as a property's has. Its closures' were
// recorded as their compiles ended, before its own.
func (c *compiler) recordFacts() (f codeFacts) {
	fn := c.fn
	arrow := fn.Kind == bytecode.KindArrow
	callee := fn.MappedArguments || arrow && fn.UsesNewTarget
	reads := fn.UsesNewTarget
	// No parameter for a store to be to, and no closure's capture of one
	// to settle: what the code stores to is not asked.
	stores := fn.ParamCount != 0 || len(c.paramCaptures) != 0
	params, written := uint32(fn.ParamCount), uint64(0)
	assigns := false
	for _, in := range fn.Code {
		k := opFacts[in.Op]
		if k == 0 {
			continue
		}
		switch {
		case !stores:
		case k&factStoresA != 0:
			assigns = assigns || in.A < params
			written |= 1 << in.A // none for a slot past 63
		case k&factStores != 0:
			lo, hi := bytecode.WrittenLocals(in)
			assigns = assigns || lo < hi && lo < fn.ParamCount
			for s := lo; s < hi && s < 64; s++ {
				written |= 1 << s
			}
		}
		switch k &^ (factStores | factStoresA) {
		case factPrivate:
			fn.PrivateSites++
		case factSuperRef:
			callee, reads = true, true
		case factPushCallee:
			callee = true
		case factSuperCall:
			reads = true
			fn.SuperForwards = fn.SuperForwards || in.B == bytecode.SuperForward
		case factNewTarget:
			reads = true
		case factSetUpvalue:
			if int(in.A) < len(fn.Upvalues) {
				fn.Upvalues[in.A].Assigned = true
			}
		case factEval:
			f.eval = true
		case factArguments:
			fn.MakesArguments = true
		case factYieldStar:
			fn.HasYieldStar = true
		}
	}
	fn.AssignsParams, f.written = assigns, written
	if f.eval || fn.HasDirectEval {
		// A direct eval may assign any binding in scope, a strict one's
		// too, which declares nothing in it but assigns what is there.
		for i := range fn.Upvalues {
			fn.Upvalues[i].Assigned = true
		}
	}
	// What the closures it makes take of it: an arrow's reads of its
	// maker's function object, through every arrow between, and the
	// bindings each may assign, its own locals' and those it forwards of
	// the function's upvalues.
	makes := false
	for i := range fn.Constants {
		k := &fn.Constants[i]
		if k.Kind != bytecode.ConstFunction || k.Fn == nil {
			continue
		}
		makes = makes || k.Fn.ReadsMaker
		ups := k.Fn.Upvalues
		for j := range ups {
			switch d := &ups[j]; {
			case !d.Assigned:
			case d.FromParent:
				if d.Index < 64 {
					f.closures |= 1 << d.Index
				}
			case int(d.Index) < len(fn.Upvalues):
				fn.Upvalues[d.Index].Assigned = true
			}
		}
	}
	fn.FrameCallee = callee || makes
	fn.ReadsMaker = arrow && (reads || makes)
	return f
}

// paramCapture is a closure's capture of a parameter of the function
// that makes it: fn's upvalue up, of the parameter in slot.
type paramCapture struct {
	fn   *bytecode.Function
	up   uint32
	slot uint32
}

// settleParamCaptures marks by value (UpvalueDesc's ByValue) each of the
// function's paramCaptures whose parameter nothing assigns: no instruction
// of the function's stores to its slot -- a parameter list with a
// default, a pattern or a rest binds every slot in its prologue, so none
// of its parameters is -- and none of its closures assigns it, nor one a
// closure makes (UpvalueDesc's Assigned), the one capturing it by value
// or any other. A direct eval in the function, strict or not, may assign
// any, and it is not asked; one in a closure assigns what it captures, as
// every name in scope at the eval is (evalScope). Only a function some
// closure of which captures a parameter is looked at, once its code is all
// compiled, its closures' with it, from what recordFacts found of them,
// for the first 64 parameters, which are all a closure's captures are
// taken from.
func (c *compiler) settleParamCaptures(f codeFacts) {
	if c.fn.HasDirectEval {
		return
	}
	for _, p := range c.paramCaptures {
		if s := p.slot; s < 64 && (f.written|f.closures)&(1<<s) == 0 {
			markByValue(p.fn, p.up)
		}
	}
}

// markByValue marks fn's upvalue up as taken by value, which a closure of
// fn then knows before it is allocated (bytecode.Function's
// CapturesByValue).
func markByValue(fn *bytecode.Function, up uint32) {
	fn.Upvalues[up].ByValue = true
	fn.CapturesByValue = true
}

// release copies the function's tables out of the scratch they were built
// in, at their size, and gives the scratch back for the next function. It is
// the last thing done with a function's compiler.
func (c *compiler) release() *bytecode.Function {
	s := c.scratch
	fn := c.fn
	pool := c.pool
	s.code, fn.Code = fn.Code[:0], exact(fn.Code)
	s.lines, fn.Lines = fn.Lines[:0], exact(fn.Lines)
	// What points into the script -- a name is a slice of its text, and a
	// constant may hold a nested function -- is cleared, so that a pool kept
	// for the next compile does not keep this one's script alive: what this
	// function wrote, not all a bigger function before it had room for,
	// which each of its siblings would pay for again.
	names, consts, upvalues := fn.Names, fn.Constants, fn.Upvalues
	fn.Names, fn.Constants, fn.Upvalues = exact(names), exact(consts), exact(upvalues)
	s.names, s.consts, s.upvalues = clearUsed(names), clearUsed(consts), clearUsed(upvalues)
	s.locals = clearUsed(c.locals[:max(c.localsUsed, len(c.locals))])
	s.pending = clearUsed(c.pendingLocals)
	s.targets = c.targets[:0]
	s.captures = clearUsed(c.paramCaptures)
	resetIndex(&s.nameIndex)
	resetIndex(&s.constIndex)
	resetIndex(&s.localIndex)
	// The state is reused too; drop its source, enclosing compiler and all
	// function-specific settings before making it available to a sibling.
	*c = compiler{}
	pool.free = append(pool.free, s)
	return fn
}

// done releases a script's own compiler, the last of a compile, and hands
// its pool on to the next compile. One that fails keeps neither.
func (c *compiler) done() *bytecode.Function {
	pool := c.pool
	fn := c.release()
	scratchPools.Put(pool)
	return fn
}

// clearUsed is s emptied for reuse, its elements cleared.
func clearUsed[T any](s []T) []T {
	clear(s)
	return s[:0]
}

// bigIndex is the most entries a function's map is kept with: clearing a
// map takes the time of all the room it has, and a function that grew one
// would have each of its siblings clear that.
const bigIndex = 256

// resetIndex empties a map for the next function.
func resetIndex[K comparable, V any](m *map[K]V) {
	if len(*m) > bigIndex {
		*m = make(map[K]V, 8)
		return
	}
	clear(*m)
}

// exact is a copy of s no larger than it, or nil for an empty s.
func exact[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return append(make([]T, 0, len(s)), s...)
}

// isTarget reports whether something jumps to pc.
func (c *compiler) isTarget(pc int) bool {
	return pc < len(c.targets) && c.targets[pc]
}

// ---------------------------------------------------------------------------
// Emitting
// ---------------------------------------------------------------------------

// emit appends an instruction and returns its program counter.
func (c *compiler) emit(op bytecode.Op, a, b uint32) int {
	if (op == bytecode.OpGetProp || op == bytecode.OpGetPropThis || op == bytecode.OpSetProp ||
		op == bytecode.OpNewObject || op == bytecode.OpInstanceOf || op == bytecode.OpGetGlobal ||
		op == bytecode.OpSetGlobal || op == bytecode.OpCheckGlobalRef) && b == 0 {
		// Each property read and write has a cache of its own, numbered
		// here, and so does each object literal, which keeps the shape its
		// objects start from in it, each instanceof, for its read of the
		// constructor's Symbol.hasInstance, and each global read, write and
		// strict mode's check before a write, for where in the global object
		// its name was.
		c.fn.PropSites++
		b = c.fn.PropSites
	}
	if op == bytecode.OpGetLocal && c.lazyArguments && a == c.lazyArgumentsSlot {
		// Every read of an arguments object made on demand makes it, but for
		// the one a call of apply fuses with.
		op = bytecode.OpLazyArguments
	}
	if fused, ok := c.fuse(op, a); ok {
		c.adjustStack(op, a, b)
		return fused
	}
	c.fn.Code = append(c.fn.Code, bytecode.Instr{Op: op, A: a, B: b})
	c.adjustStack(op, a, b)
	return len(c.fn.Code) - 1
}

// fuse makes the instruction about to be emitted part of the last one, where
// the two are a pair a single instruction does, and reports whether it did.
// The instruction each pair dispatches is the one a program runs most:
// fewer of them is less of the loop's own cost.
//
// The position the second would have had must be one nothing jumps to. A
// source position recorded for it is left where it is, for the instruction
// emitted next, when the second cannot throw -- nothing would ever report
// it -- and moved onto the pair when it can.
func (c *compiler) fuse(op bytecode.Op, a uint32) (int, bool) {
	n := len(c.fn.Code)
	if n == 0 || c.isTarget(n) {
		return 0, false
	}
	last := &c.fn.Code[n-1]
	if immediateOp(op) && last.Op == bytecode.OpPushConst {
		// A negative int32 literal, which compileUnary folds into a
		// constant, taken by an operator as an immediate as a positive one
		// is: sp + -8, x & -4. Elsewhere -1 stays the constant it was.
		if k := c.fn.Constants[last.A]; k.Kind == bytecode.ConstNumber {
			if i := int32(k.Num); float64(i) == k.Num && !isNegZero(k.Num) {
				last.Op, last.A = bytecode.OpPushInt, uint32(i)
			}
		}
	}
	switch {
	case op == bytecode.OpGetLocal && last.Op == bytecode.OpGetLocal:
		last.Op, last.B = bytecode.OpGetLocal2, a
	case op == bytecode.OpGetLocal && last.Op == bytecode.OpSetLocal:
		last.Op, last.B = bytecode.OpSetLocalGet, a
	case op == bytecode.OpSetLocal && last.Op == bytecode.OpPushUndef:
		last.Op, last.A = bytecode.OpClearLocal, a
	case immediateOp(op) && last.Op == bytecode.OpPushInt && n >= 2 && !c.isTarget(n-1) &&
		c.fn.Code[n-2].Op == bytecode.OpGetLocal && c.fn.Code[n-2].A < 1<<24:
		// A local, an integer and the operator: `x & 0xff`.
		first := &c.fn.Code[n-2]
		first.Op, first.A, first.B = bytecode.OpLocalBinImm, first.A|uint32(op)<<24, last.A
		c.fn.Code = c.fn.Code[:n-1]
		c.collapsePositions(n - 2)
		return n - 2, true
	case immediateOp(op) && last.Op == bytecode.OpPushInt:
		last.Op, last.B = bytecode.OpBinImm, uint32(op)
		c.movePosition(n)
	case (op == bytecode.OpAdd || op == bytecode.OpSub || op == bytecode.OpMul) && last.Op == bytecode.OpGetLocal:
		last.Op, last.B = bytecode.OpBinLocal, uint32(op)
		c.movePosition(n)
	case op == bytecode.OpGetIndex && last.Op == bytecode.OpGetLocal2:
		last.Op = bytecode.OpGetLocalIndex
		c.movePosition(n)
	case op == bytecode.OpInitLocal && last.Op == bytecode.OpSetLocal && last.A == a && n >= 2 &&
		c.fn.Code[n-2].Op == bytecode.OpPushUninitialized && !c.isTarget(n-1):
		// A binding marked uninitialized and initialized straight after, as
		// a for-of's is each iteration: nothing can see the mark.
		first := &c.fn.Code[n-2]
		first.Op, first.A = bytecode.OpInitLocal, a
		c.fn.Code = c.fn.Code[:n-1]
		c.collapsePositions(n - 2)
		return n - 2, true
	case op == bytecode.OpCallMethod && a == 2 && last.Op == bytecode.OpLazyArguments:
		// f.apply(x, arguments), with the arguments object never made.
		last.Op = bytecode.OpApplyArguments
		c.movePosition(n)
	case op == bytecode.OpGetIndex && last.Op == bytecode.OpUpdateLocal && n >= 2 &&
		c.fn.Code[n-2].Op == bytecode.OpGetLocal && !c.isTarget(n-1) && !c.hasPosition(n) &&
		last.A < 1<<29:
		// The update and the read can both throw, and report the same
		// position, the update's; the read of the object before them cannot.
		// So the three are one where nothing jumps between them, with the
		// update's position.
		first := &c.fn.Code[n-2]
		first.Op, first.B = bytecode.OpGetLocalIndexUpdate, last.A<<2|last.B
		c.fn.Code = c.fn.Code[:n-1]
		c.movePosition(n - 1)
		return n - 2, true
	default:
		return 0, false
	}
	return n - 1, true
}

// collapsePositions gives the instruction at n every source position recorded
// past it, which instructions fused into it had: the last of them, which is
// the one that can throw, is the one it keeps.
func (c *compiler) collapsePositions(n int) {
	for i := len(c.fn.Lines) - 1; i >= 0 && c.fn.Lines[i].PC > uint32(n); i-- {
		c.fn.Lines[i].PC = uint32(n)
	}
}

// hasPosition reports whether a source position has been recorded for the
// instruction at n.
func (c *compiler) hasPosition(n int) bool {
	l := len(c.fn.Lines)
	return l > 0 && c.fn.Lines[l-1].PC == uint32(n)
}

// movePosition gives the instruction before n the source position recorded
// for n, which a pair fused from the two needs when it is the second of them
// that can throw.
func (c *compiler) movePosition(n int) {
	if l := len(c.fn.Lines); l > 0 && c.fn.Lines[l-1].PC == uint32(n) {
		c.fn.Lines[l-1].PC = uint32(n - 1)
	}
}

// immediateOp reports whether a binary operator has a form taking its right
// operand as an immediate, OpBinImm.
func immediateOp(op bytecode.Op) bool {
	switch op {
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpBitAnd, bytecode.OpBitOr,
		bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr:
		return true
	}
	return false
}

// emitAt emits an instruction and records its source line.
func (c *compiler) emitAt(pos int, op bytecode.Op, a, b uint32) int {
	c.recordLine(pos)
	return c.emit(op, a, b)
}

// recordLine attributes the next instruction emitted to a source position.
func (c *compiler) recordLine(pos int) {
	if !c.script.HasText() {
		return
	}
	if p := int32(pos); p != c.lastPos {
		c.fn.Lines = append(c.fn.Lines, bytecode.SourceLoc{
			PC: uint32(len(c.fn.Code)), Pos: p,
		})
		c.lastPos = p
	}
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

// emitJumpB is emitJump for an instruction that carries a second operand.
func (c *compiler) emitJumpB(op bytecode.Op, b uint32) int {
	return c.emit(op, 0xFFFFFFFF, b)
}

// patchJump points a previously emitted jump at the current position.
func (c *compiler) patchJump(pc int) {
	c.markTarget(len(c.fn.Code))
	c.fn.Code[pc].A = uint32(len(c.fn.Code))
}

// patchJumpTo points a jump at a specific position.
func (c *compiler) patchJumpTo(pc, target int) {
	c.markTarget(target)
	c.fn.Code[pc].A = uint32(target)
}

// here returns the current program counter, for backward jumps.
func (c *compiler) here() int {
	c.markTarget(len(c.fn.Code))
	return len(c.fn.Code)
}

// markTarget records that something jumps to a position.
//
// What this is for is deciding whether the last instruction emitted is the end
// of one path or the meeting point of several, which is what says whether it
// may be rewritten -- see compileExprForEffect. Marking a position that nothing
// actually jumps to only gives up a rewrite, so the callers mark freely.
func (c *compiler) markTarget(pc int) {
	for len(c.targets) <= pc {
		c.targets = append(c.targets, false)
	}
	c.targets[pc] = true
}

// errorf reports a compile error.
func (c *compiler) errorf(pos int, format string, args ...any) {
	l, col := c.script.Position(int32(pos))
	panic(&Error{Msg: fmt.Sprintf(format, args...), Line: int(l), Col: int(col)})
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
	// What the interpreter needs to know about a binding is recorded before it
	// goes out of scope, since the slot outlives the scope and an instruction
	// compiled earlier may still name it.
	for _, l := range c.locals[i:] {
		c.recordLocal(l)
	}
	if c.localIndex != nil {
		for j := len(c.locals) - 1; j >= i; j-- {
			l := &c.locals[j]
			if l.previous == 0 {
				delete(c.localIndex, l.name)
			} else {
				c.localIndex[l.name] = l.previous
			}
		}
	}
	// Slots are not reclaimed: reusing them would let a closure created in one
	// iteration of a loop observe a later iteration's value.
	c.localsUsed = max(c.localsUsed, len(c.locals))
	c.locals = c.locals[:i]
}

// recordLocal notes a binding's name and kind for the interpreter, which reads
// them when reporting an assignment to a constant or a read before
// initialization.
func (c *compiler) recordLocal(l localVar) {
	for uint32(len(c.pendingLocals)) <= l.slot {
		c.pendingLocals = append(c.pendingLocals, bytecode.LocalDesc{})
	}
	c.pendingLocals[l.slot] = bytecode.LocalDesc{
		Name:     l.name,
		Mutable:  l.kind != bindConst,
		TDZ:      l.kind == bindLet || l.kind == bindConst,
		Captured: l.captured,
	}
}

// declare adds a binding to the current scope and returns its slot.
func (c *compiler) declare(name string, kind bindKind, pos int) uint32 {
	// A redeclaration in the same scope is an error for lexical bindings.
	// A function declaration inside a block is a lexical binding and collides
	// like one. At a function's own top level it is var-scoped instead, which
	// is why `function f(){} function f(){}` is legal there and not in a block.
	lexicalFn := (kind == bindFunction || kind == bindFunctionLexical) && c.depth > 0
	// Two parameters may share a name where the list is simple and the code is
	// sloppy, which the parser decides; each still has its own slot, because
	// the interpreter fills them positionally, and the later one is what the
	// name resolves to.
	if kind != bindParam &&
		((kind != bindVar && kind != bindFunction && kind != bindFunctionLexical) || lexicalFn) {
		for i := len(c.locals) - 1; i >= 0 && c.locals[i].depth == c.depth; i-- {
			if c.locals[i].name != name || c.locals[i].paramScoped {
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
		if c.localIndex != nil {
			for i := c.localIndex[name] - 1; i >= 0; i = c.locals[i].previous - 1 {
				if c.locals[i].kind == bindVar {
					return c.locals[i].slot
				}
			}
		} else {
			for i := range c.locals {
				if c.locals[i].name == name && c.locals[i].kind == bindVar {
					return c.locals[i].slot
				}
			}
		}
	}

	slot := c.nextSlot
	c.nextSlot++
	c.addLocal(localVar{
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
	if l, ok := c.resolveLocal(name); ok {
		l.initialized = !l.jumpedOver
	}
}

// Small functions need no map. Bundles with thousands of bindings otherwise
// scan the whole enclosing function for each identifier in a nested function.
const indexedLocals = 32

// nameBit is a name's bit in a compiler's localNames.
func nameBit(name string) uint64 {
	if name == "" {
		return 1
	}
	return 1 << ((uint(len(name))*37 + uint(name[0])*5 + uint(name[len(name)-1])) & 63)
}

func (c *compiler) addLocal(l localVar) {
	c.localNames |= nameBit(l.name)
	if c.localIndex != nil {
		l.previous = c.localIndex[l.name]
		c.localIndex[l.name] = len(c.locals) + 1
	}
	c.locals = append(c.locals, l)
	if c.localIndex == nil && len(c.locals) == indexedLocals {
		if c.scratch.localIndex == nil {
			c.scratch.localIndex = make(map[string]int, indexedLocals)
		}
		c.localIndex = c.scratch.localIndex
		for i := range c.locals {
			l := &c.locals[i]
			l.previous = c.localIndex[l.name]
			c.localIndex[l.name] = i + 1
		}
	}
}

// constLiteral is the literal a read of name gives, where the name is a
// const of this function or of one enclosing it, initialized with a
// literal -- const NO_WORK = null, const DATA_SIZE = 4 -- and out of its
// dead zone where the read is: nil for anything else. A const's value is
// its initializer's from its initialization on, whichever closure reads
// it, so the read is the literal, and a function made after it captures
// nothing for it. That a binding of an enclosing function is out of its
// dead zone where a function is compiled means it is whenever the function
// runs, as resolveUpvalue's checks have it: a function is compiled where it
// is made, a hoisted one at its block's start, and a switch's case block's
// bindings are never out of it to the compiler. A with body or a direct
// eval that could answer for the name is probed before the read, as it is
// before any. Code compiled for a debugger keeps its reads, so that the
// binding is in every scope that names it.
//
// litNames, the names of the consts given a literal that this function or
// its makers had compiled when it was made, answers for the rest without
// the search: identifiers are read everywhere, and a search of each
// enclosing function's locals for a global's name cost loading code 5%.
func (c *compiler) constLiteral(name string) ast.Expr {
	if c.opts.Debug || !c.litNames[name] {
		return nil
	}
	for f := c; f != nil; f = f.parent {
		if l, ok := f.resolveLocal(name); ok {
			if l.kind == bindConst && l.initialized && !l.jumpedOver {
				return l.lit
			}
			return nil
		}
	}
	return nil
}

// callerBinding finds what the call site of this eval binds a name to.
func (c *compiler) callerBinding(name string) (bytecode.EvalBinding, bool) {
	for _, b := range c.opts.EvalScope {
		if b.Name == name {
			return b, true
		}
	}
	return bytecode.EvalBinding{}, false
}

// resolveLocal finds a binding in the current function.
func (c *compiler) resolveLocal(name string) (*localVar, bool) {
	if c.localIndex != nil {
		if i := c.localIndex[name]; i != 0 {
			return &c.locals[i-1], true
		}
		return nil, false
	}
	if c.localNames&nameBit(name) == 0 {
		return nil, false
	}
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
			if b.Name != name || c.evalOwnVars[name] {
				continue
			}
			idx := c.addUpvalue(name, b.Index, b.FromLocal, b.Mutable, b.TDZ, b.WithDepth)
			c.fn.Upvalues[idx].FuncSelf = b.FuncSelf
			return idx, true
		}
		return 0, false
	}
	if l, ok := c.parent.resolveLocal(name); ok {
		l.captured = true
		immutable := l.kind == bindConst || l.kind == bindFuncSelf
		// A function is compiled where it is made, so a binding its maker
		// has initialized by then -- as the maker's own reads would find it
		// -- is initialized whenever the function runs, and its reads need
		// no check. A hoisted declaration is made at its block's start,
		// before the block's own bindings are.
		idx := c.addUpvalue(name, l.slot, true, !immutable,
			(l.kind == bindLet || l.kind == bindConst) && !l.initialized, l.withDepth)
		c.fn.Upvalues[idx].FuncSelf = l.kind == bindFuncSelf
		if !c.opts.Debug {
			switch {
			case l.kind == bindConst && l.initialized:
				// A const no assignment can change -- one through a with
				// or an eval throws as any does -- that holds its value
				// already, as the closure is made where it is compiled.
				markByValue(c.fn, idx)
			case l.kind == bindParam && c.parent.fn.Strict:
				// A strict function's parameter, which no arguments object
				// aliases: by value if nothing stores to it, which only the
				// end of its function's compile can say.
				c.parent.paramCaptures = append(c.parent.paramCaptures, paramCapture{c.fn, idx, l.slot})
			}
		}
		return idx, true
	}
	// Not a local of the parent, so look further out and forward the result.
	if idx, ok := c.parent.resolveUpvalue(name); ok {
		desc := c.parent.fn.Upvalues[idx]
		out := c.addUpvalue(name, idx, false, desc.Mutable, desc.TDZ, desc.WithDepth)
		c.fn.Upvalues[out].FuncSelf = desc.FuncSelf
		return out, true
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

// checkGlobalLexicals rejects a top-level let, const or class whose name the
// global object already holds in a property that cannot be removed.
//
// A lexical binding shadows such a property for good, which is why the
// specification refuses it rather than letting `let undefined` make undefined
// unreachable for the rest of the realm. The check is a runtime one because
// only then is it known what the global object holds.
func (c *compiler) checkGlobalLexicals(body []ast.Stmt) {
	if !c.atScriptTopLevel() {
		// A module's bindings are its own, and so are eval code's: neither
		// reaches the global environment, whatever the strictness.
		return
	}
	for _, s := range body {
		switch n := s.(type) {
		case *ast.VarDecl:
			if n.Kind == ast.DeclVar {
				continue
			}
			var room [8]string
			names := room[:0]
			for _, d := range n.Decls {
				names = appendPatternNames(names, d.Target)
			}
			for _, name := range names {
				c.emitAt(n.Start, bytecode.OpCheckGlobalLex, c.nameIdx(name), 0)
			}
		case *ast.ClassDecl:
			if n.Class.Name != nil {
				c.emitAt(n.Start, bytecode.OpCheckGlobalLex,
					c.nameIdx(n.Class.Name.Name), 0)
			}
		}
	}
}

// checkGlobalVars rejects a top-level var or function declaration the global
// environment will not accept, before any of them is created.
func (c *compiler) checkGlobalVars(body []ast.Stmt) {
	if !c.functionsAreGlobal() {
		return
	}
	funcs := map[string]bool{}
	for _, s := range body {
		if _, name, ok := hoistableFunction(s); ok {
			funcs[name] = true
			c.emit(bytecode.OpCheckGlobalVar, c.nameIdx(name), 1)
		}
	}
	for _, n := range c.varNames(body) {
		if funcs[n] {
			continue
		}
		c.emit(bytecode.OpCheckGlobalVar, c.nameIdx(n), 0)
	}
}

// hoistGlobals declares top-level var and function bindings on the global
// object, which is where script-level declarations live.
func (c *compiler) hoistGlobals(body []ast.Stmt) {
	names, annexB := collectVarDecls(body, nil, c.fn.Strict, c.opts.NodeQuirks)
	c.annexB = annexB
	// Only Annex B asks for some of the names, and a lexical binding of the
	// calling function between the eval and its variable scope stops the
	// hoist rather than failing the eval.
	if len(annexB) > 0 && c.opts.EvalOwnVarScope && !c.varScopeIsGlobal() {
		// Collected as strict code, the names are the ones without Annex B.
		plain, _ := collectVarDecls(body, nil, true, false)
		real := map[string]bool{}
		for _, n := range plain {
			real[n] = true
		}
		kept := names[:0]
		for _, n := range names {
			if b, known := c.callerBinding(n); !real[n] && known && b.Lexical && b.FromLocal {
				for fd := range annexB {
					if fd.Fn.Name.Name == n {
						delete(annexB, fd)
					}
				}
				continue
			}
			kept = append(kept, n)
		}
		names = kept
	}
	// V8 creates them in the order they are written instead.
	if !c.opts.NodeQuirks {
		names = functionsFirst(body, names)
	}
	for _, n := range names {
		if c.evalVarsAreLocal() {
			// Strict eval code gets a variable environment of its own, so its
			// vars are bindings of the evaluated code and vanish with it.
			// Which is the point: strict mode is where `eval` stops being able
			// to reach into its surroundings.
			c.declare(n, bindVar, 0)
			continue
		}
		if c.opts.EvalOwnVarScope && !c.varScopeIsGlobal() {
			// Sloppy eval code inside a function: what it declares belongs to
			// that function's variable scope.
			b, known := c.callerBinding(n)
			switch {
			case known && b.VarScoped:
				// The function already binds the name, so the declaration
				// creates nothing and a reference means that binding.
				continue
			case known && b.Lexical && b.FromLocal:
				// A lexical binding of the calling function stands between the
				// evaluated code and the variable scope, and a var may not be
				// hoisted over one.
				c.errorf(0, "%q is already declared as a lexical binding", n)
			}
			if c.evalOwnVars == nil {
				c.evalOwnVars = map[string]bool{}
			}
			c.evalOwnVars[n] = true
		}
		// A var declared by eval is configurable, unlike one a script declares,
		// because the evaluated code could have declared it anywhere.
		c.emit(bytecode.OpDefineGlobalVar, c.nameIdx(n), boolBit(c.opts.EvalConfigurable))
	}
}

// functionsFirst orders the names a script or eval code declares as its
// declaration instantiation creates them: the functions first -- each where
// its last declaration puts it -- and then the vars, in the order written.
func functionsFirst(body []ast.Stmt, names []string) []string {
	var fns []string
	isFn := map[string]bool{}
	for i := len(body) - 1; i >= 0; i-- {
		if _, name, ok := hoistableFunction(body[i]); ok && !isFn[name] {
			isFn[name] = true
			fns = append(fns, name)
		}
	}
	if len(fns) == 0 {
		return names
	}
	out := make([]string, 0, len(names))
	for i := len(fns) - 1; i >= 0; i-- {
		out = append(out, fns[i])
	}
	for _, n := range names {
		if !isFn[n] {
			out = append(out, n)
		}
	}
	return out
}

// atScriptTopLevel reports whether the compiler is in the outermost statement
// list of a script -- not a module, and not code eval is compiling, both of
// which keep their lexical bindings to themselves.
func (c *compiler) atScriptTopLevel() bool {
	return c.parent == nil && c.depth == 0 && c.module == nil && !c.opts.EvalOwnVarScope
}

// evalVarsAreLocal reports whether a top-level var belongs to the evaluated
// code rather than to the global object.
func (c *compiler) evalVarsAreLocal() bool {
	return c.opts.EvalOwnVarScope && c.fn.Strict
}

// varScopeIsGlobal reports whether a var declared where the compiler stands
// becomes a property of the global object.
//
// It is false inside any function and at a module's top level, and an eval
// inherits the answer from its call site: what its code declares belongs
// wherever the caller's own vars do.
func (c *compiler) varScopeIsGlobal() bool {
	if c.parent != nil || c.module != nil {
		return false
	}
	if c.opts.EvalOwnVarScope {
		return c.opts.EvalVarScopeIsGlobal
	}
	return true
}

func boolBit(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// collectVarNamesIn gathers the names a function's var-scoped bindings cover.
//
// Real `var` declarations and top-level function declarations are
// unconditional. A function declared inside a block is included only because of
// Annex B, which gives it a var-scoped alias -- and only where that alias would
// be legal: a `let` of the same name anywhere between the block and the
// function top level suppresses it, and strict mode suppresses it outright.
func collectVarNamesIn(body []ast.Stmt, out *[]string, strict bool) {
	*out, _ = collectVarDecls(body, *out, strict, false)
}

// varNames collects the names a body's var-scoped bindings cover, as the code
// being compiled hoists them.
func (c *compiler) varNames(body []ast.Stmt) []string {
	names, _ := collectVarDecls(body, nil, c.fn.Strict, c.opts.NodeQuirks)
	return names
}

// collectVarDecls is collectVarNamesIn that also reports the block-level
// function declarations whose names it added for Annex B. With nodeQuirks it
// hoists as V8 does.
func collectVarDecls(body []ast.Stmt, out []string, strict, nodeQuirks bool) ([]string, map[*ast.FuncDecl]bool) {
	c := &varCollector{out: out, strict: strict, nodeQuirks: nodeQuirks}
	// A declaration Annex B would hoist over a lexical binding of the top
	// level is not hoisted: `let f; { function f() {} }` keeps its let.
	// Strict code hoists nothing, so needs no list.
	if !strict {
		for _, s := range body {
			c.top = lexicalNamesOf(s, c.top, false)
		}
	}
	c.stmts(body)
	return c.out, c.granted
}

// varCollector walks a function body tracking the lexical scopes in effect, so
// that Annex B's alias can be suppressed where an enclosing block already binds
// the name.
type varCollector struct {
	out    []string
	strict bool
	// nodeQuirks hoists over an enclosing block's function of the same name,
	// as V8 does.
	nodeQuirks bool
	// lexical is the stack of enclosing block scopes, innermost last. It is
	// empty at the function's own top level, where a function declaration is
	// var-scoped outright rather than by Annex B.
	lexical [][]lexName
	// top is what the top level binds lexically.
	top []lexName
	// granted holds the block-level declarations Annex B hoists.
	granted map[*ast.FuncDecl]bool
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
	if _, ok := s.(*ast.FuncDecl); ok {
		// `if (x) function f() {}` is read as though the declaration were in
		// a block of its own.
		c.block([]ast.Stmt{s})
		return
	}
	c.stmt(s)
}

// shadowed reports whether an enclosing block, or the top level, binds the
// name lexically.
func (c *varCollector) shadowed(name string) bool {
	for i := range c.top {
		if c.top[i].name == name {
			return true
		}
	}
	for depth, scope := range c.lexical {
		for i := range scope {
			// A block function declaration binds the name lexically too. In
			// the declaration's own block it is the declaration being hoisted
			// rather than something in its way; in an enclosing block it is
			// in the way like anything else -- except to V8.
			enclosing := depth < len(c.lexical)-1 && !c.nodeQuirks
			if scope[i].name == name && (!scope[i].fromFunc || enclosing) {
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
		for _, d := range n.Decls {
			c.out = appendPatternNames(c.out, d.Target)
		}

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
		if c.granted == nil {
			c.granted = map[*ast.FuncDecl]bool{}
		}
		c.granted[n] = true

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
			// A destructured catch parameter is in the way of a hoist, as a
			// let would be. A simple one is not: B.3.4 lets a var in the catch
			// block share its name, and so a hoisted function too.
			var names []lexName
			if _, simple := n.Catch.Param.(*ast.Ident); !simple && n.Catch.Param != nil {
				names = patternNames(n.Catch.Param, nil)
			}
			c.lexical = append(c.lexical, names)
			c.block(n.Catch.Body)
			c.lexical = c.lexical[:len(c.lexical)-1]
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

// appendPatternNames appends the identifiers a binding pattern introduces.
func appendPatternNames(out []string, target ast.Expr) []string {
	switch n := target.(type) {
	case *ast.Ident:
		out = append(out, n.Name)
	case *ast.ArrayPattern:
		for _, el := range n.Elements {
			if el != nil {
				out = appendPatternNames(out, el)
			}
		}
		if n.Rest != nil {
			out = appendPatternNames(out, n.Rest)
		}
	case *ast.ObjectPattern:
		for _, p := range n.Props {
			out = appendPatternNames(out, p.Value)
		}
		if n.Rest != nil {
			out = appendPatternNames(out, n.Rest)
		}
	case *ast.AssignPattern:
		out = appendPatternNames(out, n.Target)
	case *ast.RestElement:
		out = appendPatternNames(out, n.Arg)
	}
	return out
}

// stackEffect returns how much an opcode changes the operand stack depth.
//
// It exists so that MaxStack can be computed without simulating execution. An
// overestimate merely wastes a few slots per frame; an underestimate would
// corrupt the stack, so anything uncertain is rounded up.
func stackEffect(op bytecode.Op, a, b uint32) int {
	switch op {
	case bytecode.OpGetLocal2:
		return 2
	case bytecode.OpGetLocalIndex:
		return 1
	case bytecode.OpSetLocalGet, bytecode.OpBinImm, bytecode.OpBinLocal:
		return 0
	case bytecode.OpLocalBinImm:
		return 1
	case bytecode.OpApplyArguments:
		return -2
	case bytecode.OpLazyArguments, bytecode.OpArgumentsLength:
		return 1
	case bytecode.OpArgumentsIndex:
		return 0
	case bytecode.OpClearLocal:
		return 0
	case bytecode.OpUpdateLocal, bytecode.OpGetLocalIndexUpdate:
		return 1
	case bytecode.OpPushConst, bytecode.OpPushUndef, bytecode.OpPushNull,
		bytecode.OpPushTrue, bytecode.OpPushFalse, bytecode.OpPushThis,
		bytecode.OpPushInt, bytecode.OpPushEmptyString,
		bytecode.OpPushUninitialized, bytecode.OpGetLocal,
		bytecode.OpGetLocalCheck, bytecode.OpGetUpvalue,
		bytecode.OpGetUpvalueCheck, bytecode.OpGetGlobal,
		bytecode.OpGetGlobalOpt, bytecode.OpDup, bytecode.OpClosure,
		bytecode.OpNewObject, bytecode.OpNewTarget, bytecode.OpImportMeta,
		bytecode.OpPushCallee, bytecode.OpNewRegExp, bytecode.OpTemplateObject,
		bytecode.OpPrivateName, bytecode.OpNewDisposeCapability,
		bytecode.OpGetArguments, bytecode.OpRestParam, bytecode.OpDeleteVar,
		bytecode.OpIterStep, bytecode.OpIterRest,
		bytecode.OpGetPropUnder, bytecode.OpGetIndexUnder,
		bytecode.OpGetSuperProp,
		bytecode.OpGetPropThis,
		bytecode.OpParamNeedsDefault,
		bytecode.OpIsNullish:
		return 1

	case bytecode.OpCheckGlobalRef:
		// Pushes what the reference resolved to, which the store consumes.
		return 1
	case bytecode.OpAssertResolved:
		return -1

	case bytecode.OpDup2:
		return 2

	case bytecode.OpIterCloseNormal,
		bytecode.OpDrop, bytecode.OpSetLocal, bytecode.OpSetLocalCheck,
		bytecode.OpInitLocal, bytecode.OpSetUpvalue, bytecode.OpSetUpvalueCheck,
		bytecode.OpInitUpvalue, bytecode.OpSetGlobal, bytecode.OpSetGlobalStrict, bytecode.OpThrow,
		bytecode.OpReturn, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue,
		bytecode.OpDefineGlobalFunc, bytecode.OpArrayPush,
		bytecode.OpDefineField, bytecode.OpDefineGetter,
		bytecode.OpDefineSetter, bytecode.OpSetProtoOf,
		bytecode.OpSetFieldInit,
		bytecode.OpSetSuperProp,
		bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep,
		bytecode.OpJumpIfNotNullish:
		return -1

	case bytecode.OpJumpIfCmpFalse:
		// Pops both operands of the comparison it carries.
		return -2

	case bytecode.OpJumpIfNullish:
		// Optional chaining keeps the tested value on both paths.
		return 0

	case bytecode.OpAsyncIterNext:
		return 1
	case bytecode.OpAsyncIterClose:
		// Pops the cursor, and pushes what its return method returned where
		// it goes on; pops the result it checks, or the cursor it marks.
		if b == 0 {
			return 0
		}
		return -1
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
	case bytecode.OpImport:
		return 1 - int(b)
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
		// Pops the arguments, or the argument array in the spread form, and
		// pushes this; the forwarding form pops nothing.
		switch b {
		case bytecode.SuperSpread:
			// It had been -1, which counted the array popped and not this
			// pushed: one slot under, where an estimate is to be over.
			return 0
		case bytecode.SuperForward:
			return 1
		}
		return -int(a) + 1
	case bytecode.OpNewClass:
		// Pops the parent, leaving the constructor.
		return -1
	case bytecode.OpYield, bytecode.OpAwait:
		// Pops the operand and pushes the resumption value.
		return 0

	case bytecode.OpYieldStar:
		// Pops the operand and pushes the resumption value and its kind.
		return 1

	case bytecode.OpIterUnpackDelegate:
		// Pops the result and pushes either it or its value.
		return 0

	case bytecode.OpDefineMethod, bytecode.OpDefinePrivate,
		bytecode.OpDefinePrivateMethod:
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
	case bytecode.OpSetSuperIndex:
		// Pops the base, the key and the value.
		return -3
	case bytecode.OpGetSuperIndex:
		// Pops the base and the key, leaving the value.
		return -1
	case bytecode.OpSuperBase:
		return 1

	case bytecode.OpRethrow:
		// Pops the completion record.
		return -2

	case bytecode.OpInsert2, bytecode.OpInsert3, bytecode.OpInsert4:
		return 1
	}
	// Everything else leaves the depth unchanged.
	return 0
}

// checkEvalVarNames rejects a var the evaluated code cannot create.
//
// A direct eval in a parameter default runs in the parameter scope, which sits
// between it and the function's variable scope. A var it declares belongs to
// the variable scope, so a name the parameter scope already binds would be
// shadowed by that binding and never reachable -- which the specification makes
// an error rather than a surprise.
func (c *compiler) checkEvalVarNames(body []ast.Stmt) {
	if len(c.opts.ArgumentNames) == 0 {
		return
	}
	names := c.varNames(body)
	var lexical []lexicalName
	for _, s := range body {
		collectLexicalNames(s, &lexical)
	}
	for _, l := range lexical {
		names = append(names, l.name)
	}
	for _, n := range names {
		for _, arg := range c.opts.ArgumentNames {
			if n == arg {
				c.errorf(0, "%q is already bound by the parameter list", n)
			}
		}
	}
}
