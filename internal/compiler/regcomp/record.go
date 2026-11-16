// Package regcomp is the register form's backend: it compiles a function's
// AST into register code (docs/register-interpreter.md). It shares the stack
// compiler's scope resolution, which the stack compiler hands it as a Record
// of what each identifier of the function resolved to, and does its own code
// generation and register allocation.
package regcomp

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// ResKind is what an identifier resolved to.
type ResKind uint8

const (
	// ResNone is an identifier the stack compiler resolved some way the
	// backend does not compile.
	ResNone ResKind = iota
	// ResLocal is a binding of the function's, in stack slot Index, whose
	// reads need no dead-zone check.
	ResLocal
	// ResLocalCheck is one whose reads, or writes, check the dead zone.
	ResLocalCheck
	// ResLocalInit is a declaration's store, which initializes the binding.
	ResLocalInit
	// ResUpval is upvalue Index, read or written without a check.
	ResUpval
	// ResGlobal is a global read or a sloppy global write.
	ResGlobal
	// ResGlobalStrict is a strict global write.
	ResGlobalStrict
	// ResCallee is a named function expression's own name, read.
	ResCallee
	// ResLiteral is a name the stack compiler compiled as the literal Lit:
	// undefined, or a constant initialized with a literal.
	ResLiteral
	// ResConstAssign is a write that throws, to a constant.
	ResConstAssign
	// ResIgnore is a sloppy write to a function expression's own name,
	// which does nothing.
	ResIgnore
	// ResLazyArgs is arguments read as arguments.length or arguments[i] in
	// a function whose arguments object is made only on demand, into local
	// Index (the stack form's arguments_length and arguments_index).
	ResLazyArgs
	// ResGlobalVar is a var declaration's store to a global, which top-level
	// code makes with set_global whatever its strictness: the var exists.
	ResGlobalVar
	// ResGlobalLexInit is the initialization of a script's top-level let,
	// const or class binding (init_global_lex).
	ResGlobalLexInit
	// ResModuleLexInit is the initialization of a module's top-level let,
	// const or class binding (init_module_lex).
	ResModuleLexInit
	// ResUpvalCheck is upvalue Index, read or written with a check of its
	// dead zone (the stack form's get_upvalue_check, set_upvalue_check).
	ResUpvalCheck
	// ResWith is a name a with object or an eval's variable may answer:
	// a probe of them (Record.Withs) before its static access, whose
	// resolution the backend keeps aside (gen.withs).
	ResWith
)

// Res is an identifier's resolution.
type Res struct {
	Kind  ResKind
	Index uint32
	Lit   ast.Expr
}

// Record is the stack compiler's resolution of one function's
// identifiers: of its reads, of its writes, and of the bindings its
// hoisted function declarations are stored in.
type Record struct {
	Read  map[*ast.Ident]Res
	Write map[*ast.Ident]Res
	Funcs map[*ast.FuncDecl]uint32
	// Reused are the function expressions a var declaration in a loop
	// makes only the first time (the compiler's reusedClosures).
	Reused map[*ast.FuncLit]bool
	// Appends are the additions the stack compiler made appends to a
	// local (localAppend), and Freezes the reads it followed by a
	// str_freeze, of a binding an append grows: what reads one and may
	// keep it must not see it grow after.
	Appends map[*ast.Assign]bool
	Freezes map[*ast.Ident]bool
	// Catches is the stack slot of each catch clause's binding, where it
	// is an identifier.
	Catches map[*ast.CatchClause]uint32
	// Heads is, for a for-of or for-in loop's lexical declaration, the
	// stack slots of the bindings its head's expression sees in their
	// dead zone (ForIn/OfHeadEvaluation's TDZ environment).
	Heads map[*ast.VarDecl][]uint32
	// Tails is each call the stack form compiled as a tail call
	// (tail_call, tail_call_method, tail_direct_eval).
	Tails map[ast.Expr]bool
	// Evals is each call the stack form compiled as a direct eval
	// (direct_eval), with the index of its scope in the function's
	// EvalScopes.
	Evals map[*ast.Call]uint32
	// Withs is each name the stack form compiled with a probe of the with
	// objects and the eval's variables in scope before its static access.
	Withs map[*ast.Ident]WithProbe
	// Templates is each tagged template's index in the function's
	// Templates, its site, whose object every evaluation of it gives.
	Templates map[*ast.TemplateLit]uint32
	// Top is what top-level code does before its first statement, as the
	// stack form does it, in order: the checks and declarations of the
	// names it binds in the global object and the global lexical
	// environment, and its function declarations' definitions.
	Top []TopOp
	// Completion is the stack slot of top-level code's completion value,
	// which an expression statement sets, or -1 for a function.
	Completion int32
	// ThisRef says top-level code's this is the call site's, which may be
	// a derived constructor's binding, unbound until super() binds it:
	// eval code's, and a debugger's at a frame (the stack compiler's
	// thisIsValue).
	ThisRef bool
	// Classes is each class the function defines, as the stack form
	// defines it (ClassRec).
	Classes map[*ast.ClassLit]*ClassRec
	// Privates is where each private name's key is, as the stack form
	// resolved it: a binding of the function's (ResLocal) or an upvalue
	// (ResUpval).
	Privates map[*ast.PrivateName]Res
	// Debugs is, in code compiled for a debugger, each place it can stop
	// -- a statement, a loop's test or update, a for-in or for-of head --
	// by its position: the index of the stack form's first debug_stmt
	// there in the function's Debug.Statements, and whether it is a
	// debugger statement (DebugMark).
	Debugs map[int32]DebugMark
	// Unmarked are the lexical bindings the stack form leaves out of
	// their dead zone, nothing being able to see them there (its
	// declareLexicalNameMarked). In a debugger's code, where every
	// binding is captured for the debugger, the register form leaves them
	// so too: a debugger's evaluation sees what the stack form's sees.
	Unmarked map[uint32]bool
	// Bail says the record saw one name resolved, or probed, two ways,
	// which it cannot express.
	Bail bool

	// finallies are the finally clauses the stack form has compiled a
	// copy of, and quiet how many copies other than a clause's first are
	// being compiled: the notes made there are dropped (BeginFinally).
	finallies map[ast.Stmt]bool
	quiet     int
}

// BeginFinally is called as the stack form compiles a copy of the finally
// clause of statements list, and the function it returns as the copy
// ends. The stack form compiles a clause once for each way out of it,
// each copy's bindings in slots of their own, as slots are never reused;
// regcomp compiles its copies from one resolution, the first copy's, in
// which each binding of the clause's is a slot no other binding has --
// the slot whose function constant regcomp's closure finds first. The
// notes of every later copy are dropped.
func (r *Record) BeginFinally(list []ast.Stmt) func() {
	if r == nil || len(list) == 0 {
		return func() {}
	}
	if !r.finallies[list[0]] {
		if r.finallies == nil {
			r.finallies = map[ast.Stmt]bool{}
		}
		r.finallies[list[0]] = true
		return func() {}
	}
	r.quiet++
	return func() { r.quiet-- }
}

// NoteFunc records that hoisted function declaration fd is stored in
// slot.
func (r *Record) NoteFunc(fd *ast.FuncDecl, slot uint32) {
	if r == nil || r.quiet > 0 {
		return
	}
	r.Funcs[fd] = slot
}

// NewRecord makes an empty record.
func NewRecord() *Record {
	return &Record{Read: map[*ast.Ident]Res{}, Write: map[*ast.Ident]Res{}, Funcs: map[*ast.FuncDecl]uint32{}, Completion: -1}
}

// TopOp is one of top-level code's declarations: the stack form's
// instruction Op of name Name and flag Flag, at source position Pos, and
// for define_global_func the function, Fn, whose closure it defines.
// OpSetUpvalue is eval code's function declaration of a name its caller
// binds as a var: Fn's closure stored as the record resolves the write of
// Id, the declaration's name.
type TopOp struct {
	Op   bytecode.Op
	Name string
	Flag uint32
	Pos  int32
	Fn   *ast.FuncLit
	Id   *ast.Ident
}

// NoteTop records top-level code's declaration op.
func (r *Record) NoteTop(op TopOp) {
	if r != nil {
		r.Top = append(r.Top, op)
	}
}

// NoteAppend records that n is an append to a local.
func (r *Record) NoteAppend(n *ast.Assign) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Appends == nil {
		r.Appends = map[*ast.Assign]bool{}
	}
	r.Appends[n] = true
}

// NoteTail records that the stack form compiled call n as a tail call.
func (r *Record) NoteTail(n ast.Expr) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Tails == nil {
		r.Tails = map[ast.Expr]bool{}
	}
	r.Tails[n] = true
}

// NoteEval records that the stack form compiled call n as a direct eval
// whose scope is the function's EvalScopes[idx].
func (r *Record) NoteEval(n *ast.Call, idx uint32) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Evals == nil {
		r.Evals = map[*ast.Call]uint32{}
	}
	r.Evals[n] = idx
}

// WithProbe is a name's probe (Record.Withs): Op, the stack form's
// with_get, with_get_this, with_typeof, with_delete, with_set,
// with_get_under or with_resolve, of the innermost Limit scopes.
type WithProbe struct {
	Op    bytecode.Op
	Limit uint16
}

// NoteWith records that the stack form compiled name id with probe p.
func (r *Record) NoteWith(id *ast.Ident, p WithProbe) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Withs == nil {
		r.Withs = map[*ast.Ident]WithProbe{}
	}
	if old, ok := r.Withs[id]; ok && old != p {
		r.Bail = true
	}
	r.Withs[id] = p
}

// NoteCatch records that the binding of catch clause c is in slot.
// NoteHead records slot as a binding of the dead-zone environment a for-of
// or for-in loop's head d makes for its expression.
func (r *Record) NoteHead(d *ast.VarDecl, slot uint32) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Heads == nil {
		r.Heads = map[*ast.VarDecl][]uint32{}
	}
	r.Heads[d] = append(r.Heads[d], slot)
}

func (r *Record) NoteCatch(c *ast.CatchClause, slot uint32) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Catches == nil {
		r.Catches = map[*ast.CatchClause]uint32{}
	}
	r.Catches[c] = slot
}

// NoteFreeze records that the read id is followed by a str_freeze.
func (r *Record) NoteFreeze(id *ast.Ident) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Freezes == nil {
		r.Freezes = map[*ast.Ident]bool{}
	}
	r.Freezes[id] = true
}

// NoteRead records a read of id.
func (r *Record) NoteRead(id *ast.Ident, res Res) {
	if r == nil || r.quiet > 0 {
		return
	}
	if old, ok := r.Read[id]; ok && old != res {
		r.Bail = true
	}
	r.Read[id] = res
}

// NoteWrite records a write of id.
func (r *Record) NoteWrite(id *ast.Ident, res Res) {
	if r == nil || r.quiet > 0 {
		return
	}
	if old, ok := r.Write[id]; ok && old != res {
		r.Bail = true
	}
	r.Write[id] = res
}

// DebugMark is a place code compiled for a debugger can stop: its index
// in the function's Debug.Statements, and whether it is a debugger
// statement.
type DebugMark struct {
	Index    uint32
	Debugger bool
}

// NoteDebug records the stack form's debug_stmt of index idx at pos, the
// first there: a finally clause's later copies, compiled from the first's
// resolution, stop where it does.
func (r *Record) NoteDebug(pos int32, idx uint32, debugger bool) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Debugs == nil {
		r.Debugs = map[int32]DebugMark{}
	}
	if _, ok := r.Debugs[pos]; !ok {
		r.Debugs[pos] = DebugMark{idx, debugger}
	}
}

// NoteUnmarked records that the stack form leaves the lexical binding of
// slot out of its dead zone.
func (r *Record) NoteUnmarked(slot uint32) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Unmarked == nil {
		r.Unmarked = map[uint32]bool{}
	}
	r.Unmarked[slot] = true
}

// NoteTemplate records that tagged template q's site is idx.
func (r *Record) NoteTemplate(q *ast.TemplateLit, idx uint32) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Templates == nil {
		r.Templates = map[*ast.TemplateLit]uint32{}
	}
	r.Templates[q] = idx
}

// ClassRec is what the stack form made of one class the function defines:
// the slots of the class scope's bindings and the constants of the
// functions it made, which the register form makes in the same order.
type ClassRec struct {
	// Self is the slot of the class's inner name, or -1 for a class
	// without a name of its own.
	Self int32
	// Privates are the class's private names, each minted into its slot.
	Privates []ClassPrivate
	// Install is the slot of the list of the private methods each instance
	// is given, or -1.
	Install int32
	// Keys is, for each of the class's fields, the slot its computed key
	// is kept in, or -1.
	Keys []int32
	// Ctor is the constant of the constructor's function, and Init that of
	// the instances' field initializer, or -1.
	Ctor, Init int32
	// Members is, for each of the class's members, the constant of its
	// function, or -1 for the constructor.
	Members []int32
	// Statics are the static fields' and blocks' functions, in the order
	// they run.
	Statics []ClassStatic
}

// ClassPrivate is one of a class's private names: its key's slot and its
// name, with its #.
type ClassPrivate struct {
	Slot uint32
	Name string
}

// ClassStatic is a static field's or block's function, the constant
// Const: the field Field of the class's fields, or a block for -1.
type ClassStatic struct {
	Const uint32
	Field int32
}

// NoteClass records what the stack form made of class cls.
func (r *Record) NoteClass(cls *ast.ClassLit, cr *ClassRec) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Classes == nil {
		r.Classes = map[*ast.ClassLit]*ClassRec{}
	}
	r.Classes[cls] = cr
}

// NotePrivate records where private name pn's key is.
func (r *Record) NotePrivate(pn *ast.PrivateName, res Res) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Privates == nil {
		r.Privates = map[*ast.PrivateName]Res{}
	}
	if old, ok := r.Privates[pn]; ok && old != res {
		r.Bail = true
	}
	r.Privates[pn] = res
}
