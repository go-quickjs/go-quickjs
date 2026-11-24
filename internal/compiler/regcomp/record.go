// Package regcomp is the register form's backend: it compiles a function's
// AST into register code (docs/register-interpreter.md). The compiler's walk
// of the AST, which emits no code, resolves the function's scopes and hands
// them to it as a Record of what each identifier of the function resolved
// to; regcomp does the code generation and register allocation.
package regcomp

import (
	"iter"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// ResKind is what an identifier resolved to.
type ResKind uint8

const (
	// ResNone is an identifier the walk resolved some way the backend does
	// not compile.
	ResNone ResKind = iota
	// ResLocal is a binding of the function's, in slot Index, whose
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
	// ResLiteral is a name the walk resolved to the literal Lit:
	// undefined, or a constant initialized with a literal.
	ResLiteral
	// ResConstAssign is a write that throws, to a constant.
	ResConstAssign
	// ResIgnore is a sloppy write to a function expression's own name,
	// which does nothing.
	ResIgnore
	// ResLazyArgs is arguments read as arguments.length or arguments[i] in
	// a function whose arguments object is made only on demand, into local
	// Index.
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
	// dead zone.
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

// Record is the walk's resolution of one function's
// identifiers: of its reads, of its writes (ReadOf, WriteOf), and of the
// bindings its hoisted function declarations are stored in.
type Record struct {
	// slots is each identifier's resolutions, read and written, at the
	// number the parser gave it in its function (ast.Ident.ID), and more
	// is those of an identifier whose slot another has: an arrow's
	// parameter, numbered in the code around the arrow, or a name the
	// compiler made, which has no number.
	slots []identSlot
	more  map[*ast.Ident]*identSlot
	// lits is the literal of each read the walk resolved to one
	// (Res.Lit).
	lits  map[*ast.Ident]ast.Expr
	Funcs map[*ast.FuncDecl]uint32
	// Reused are the function expressions a var declaration in a loop
	// makes only the first time (the compiler's reusedClosures).
	Reused map[*ast.FuncLit]bool
	// Appends are the additions the walk found appends to a local
	// (localAppend) or to an upvalue (ownedAppend), and Freezes the reads
	// of a binding an append grows that a str_freeze follows: what reads
	// one and may keep it must not see it grow after.
	Appends map[*ast.Assign]bool
	Freezes map[*ast.Ident]bool
	// Catches is the slot of each catch clause's binding, where it
	// is an identifier.
	Catches map[*ast.CatchClause]uint32
	// Heads is, for a for-of or for-in loop's lexical declaration, the
	// slots of the bindings its head's expression sees in their
	// dead zone (ForIn/OfHeadEvaluation's TDZ environment).
	Heads map[*ast.VarDecl][]uint32
	// Tails is each call in tail position, where tail calls are allowed
	// (the walk's compileTailExpr).
	Tails map[ast.Expr]bool
	// Evals is each call that is a direct eval, with the index of its
	// scope in the function's EvalScopes.
	Evals map[*ast.Call]uint32
	// Withs is each name that probes the with objects and the eval's
	// variables in scope before its static access.
	Withs map[*ast.Ident]WithProbe
	// Templates is each tagged template's index in the function's
	// Templates, its site, whose object every evaluation of it gives.
	Templates map[*ast.TemplateLit]uint32
	// Top is what top-level code does before its first statement, in
	// order: the checks and declarations of the
	// names it binds in the global object and the global lexical
	// environment, and its function declarations' definitions.
	Top []TopOp
	// Completion is the slot of top-level code's completion value,
	// which an expression statement sets, or -1 for a function.
	Completion int32
	// ThisRef says top-level code's this is the call site's, which may be
	// a derived constructor's binding, unbound until super() binds it:
	// eval code's, and a debugger's at a frame (the walk's thisIsValue).
	ThisRef bool
	// Bail says the record saw one name resolved, or probed, two ways,
	// which it cannot express.
	Bail bool
	// Classes is each class the function defines, as the walk laid it out
	// (ClassRec).
	Classes map[*ast.ClassLit]*ClassRec
	// Privates is where each private name's key is, as the walk resolved
	// it: a binding of the function's (ResLocal) or an upvalue
	// (ResUpval).
	Privates map[*ast.PrivateName]Res
	// Debugs is, in code compiled for a debugger, each place it can stop
	// -- a statement, a loop's test or update, a for-in or for-of head --
	// by its position: the index of the first statement there in the
	// function's Debug.Statements, and whether it is a debugger statement
	// (DebugMark).
	Debugs map[int32]DebugMark
	// Unmarked are the lexical bindings the walk leaves out of their dead
	// zone, nothing being able to see them there (its
	// declareLexicalNameMarked). In a debugger's code, where every binding
	// is captured for the debugger, the register form leaves them so too.
	Unmarked map[uint32]bool
	// rare is what few functions have, apart, so that every function's
	// record stays in its size class.
	rare *recordRare

	// finallies are the finally clauses the walk has walked a copy of,
	// and quiet how many copies other than a clause's first are being
	// walked: the notes made there are dropped (BeginFinally).
	finallies map[ast.Stmt]bool
	quiet     int
}

// BeginFinally is called as the walk walks a copy of the finally clause of
// statements list, and the function it returns as the copy ends. The walk
// walks a clause once for each way out of it,
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

// recordRare is the part of a Record few functions have.
type recordRare struct {
	// paramCopies are the body's vars that shadow a binding of the
	// parameter list's, each with that binding's slot first: the body's
	// starts with its value, copied at the end of the prologue (the
	// walk's bodyVar).
	paramCopies [][2]uint32
	// self is, where a body's var of the function's own name shadows the
	// binding the parameter list's closures see, that binding's slot,
	// which holds the function from the start.
	self []uint32
	// arguments holds the walk's notes of possible arguments aliases, for
	// a function with a candidate declaration (NoteArgumentsAliases).
	arguments *argumentsNotes
	// bodyScope says the body has a scope of its own for what a direct
	// eval in it declares (the walk's bodyEvalScope).
	bodyScope bool
	// forInInits are the Annex B initializers, for (var a = 0 in o), of
	// a name a with object or an eval's variable may answer, each the
	// declaration of an identifier of its own that the walk walked in
	// its place (ForInInit).
	forInInits map[*ast.VarDecl]*ast.VarDecl
}

// rareOf is r's rare part, made on its first use.
func (r *Record) rareOf() *recordRare {
	if r.rare == nil {
		r.rare = &recordRare{}
	}
	return r.rare
}

// ParamCopies are the body's vars that shadow a binding of the parameter
// list's (recordRare's paramCopies).
func (r *Record) ParamCopies() [][2]uint32 {
	if r.rare == nil {
		return nil
	}
	return r.rare.paramCopies
}

// Self is the binding of the function's own name that a body's var
// shadows (recordRare's self).
func (r *Record) Self() []uint32 {
	if r.rare == nil {
		return nil
	}
	return r.rare.self
}

// NoteForInInit records that the walk walked init, a declaration of an
// identifier of its own, in place of for-in head d's Annex B
// initializer: the initializer resolves its name before its value is
// made and writes through that reference once, each turn's store
// resolves the name anew, and a probed name's two writes, the same
// identifier's, would be one name probed two ways.
func (r *Record) NoteForInInit(d, init *ast.VarDecl) {
	if r == nil || r.quiet > 0 {
		return
	}
	rare := r.rareOf()
	if rare.forInInits == nil {
		rare.forInInits = map[*ast.VarDecl]*ast.VarDecl{}
	}
	rare.forInInits[d] = init
}

// ForInInit is the declaration the walk walked in place of for-in head
// d's initializer (NoteForInInit), or d where it walked d's own.
func (r *Record) ForInInit(d *ast.VarDecl) *ast.VarDecl {
	if r.rare != nil {
		if init := r.rare.forInInits[d]; init != nil {
			return init
		}
	}
	return d
}

// NoteParamCopy records that the body's var in slot to starts with the
// value of the parameter list's binding in slot from (ParamCopies).
func (r *Record) NoteParamCopy(from, to uint32) {
	if r == nil {
		return
	}
	x := r.rareOf()
	x.paramCopies = append(x.paramCopies, [2]uint32{from, to})
}

// BodyScope says the body has a scope of its own for what a direct eval
// in it declares, pushed at the end of the prologue (recordRare's
// bodyScope).
func (r *Record) BodyScope() bool {
	return r.rare != nil && r.rare.bodyScope
}

// NoteBodyScope records that the body has a scope of its own for what a
// direct eval in it declares (BodyScope).
func (r *Record) NoteBodyScope() {
	if r == nil {
		return
	}
	r.rareOf().bodyScope = true
}

// NoteSelf records slot as the function's own name's binding that a
// body's var shadows (Self).
func (r *Record) NoteSelf(slot uint32) {
	if r == nil {
		return
	}
	x := r.rareOf()
	x.self = append(x.self, slot)
}

// NewRecord makes an empty record of a function with idents identifiers
// (ast.FuncLit.Idents).
func NewRecord(idents uint32) *Record {
	return &Record{slots: make([]identSlot, idents), Funcs: map[*ast.FuncDecl]uint32{}, Completion: -1}
}

// identSlot is an identifier's resolutions, read and written.
type identSlot struct {
	id          *ast.Ident
	read, write slotRes
}

// slotRes is a Res in a slot, but for its Lit (Record.lits), and whether
// there is one.
type slotRes struct {
	kind  ResKind
	set   bool
	index uint32
}

// slot is id's slot, or nil where it has none and add is false.
func (r *Record) slot(id *ast.Ident, add bool) *identSlot {
	if n := id.ID(); n > 0 && int(n) <= len(r.slots) {
		s := &r.slots[n-1]
		switch s.id {
		case id:
			return s
		case nil:
			// An identifier takes its slot as it is first noted, so one
			// in more has found its slot taken.
			if !add {
				return nil
			}
			s.id = id
			return s
		}
	}
	if s := r.more[id]; s != nil || !add {
		return s
	}
	if r.more == nil {
		r.more = map[*ast.Ident]*identSlot{}
	}
	s := &identSlot{id: id}
	r.more[id] = s
	return s
}

// res is the Res in v, of identifier id: its read's, with its literal,
// where read is true.
func (r *Record) res(id *ast.Ident, v slotRes, read bool) Res {
	res := Res{Kind: v.kind, Index: v.index}
	if read && v.kind == ResLiteral && r.lits != nil {
		res.Lit = r.lits[id]
	}
	return res
}

// put puts res in v, of identifier id, its read's where read is true,
// whose literal goes to lits: a write has none.
func (r *Record) put(id *ast.Ident, v *slotRes, res Res, read bool) {
	*v = slotRes{kind: res.Kind, set: true, index: res.Index}
	if !read {
		return
	}
	switch {
	case res.Lit != nil:
		if r.lits == nil {
			r.lits = map[*ast.Ident]ast.Expr{}
		}
		r.lits[id] = res.Lit
	case r.lits != nil:
		delete(r.lits, id)
	}
}

// ReadOf is the resolution of read id, and whether the record has one.
func (r *Record) ReadOf(id *ast.Ident) (Res, bool) {
	if s := r.slot(id, false); s != nil && s.read.set {
		return r.res(id, s.read, true), true
	}
	return Res{}, false
}

// WriteOf is the resolution of write id, and whether the record has one.
func (r *Record) WriteOf(id *ast.Ident) (Res, bool) {
	if s := r.slot(id, false); s != nil && s.write.set {
		return r.res(id, s.write, false), true
	}
	return Res{}, false
}

// readRes is ReadOf's Res alone.
func (r *Record) readRes(id *ast.Ident) Res {
	res, _ := r.ReadOf(id)
	return res
}

// writeRes is WriteOf's Res alone.
func (r *Record) writeRes(id *ast.Ident) Res {
	res, _ := r.WriteOf(id)
	return res
}

// setRead makes res the resolution of read id, or drops it where has is
// false.
func (r *Record) setRead(id *ast.Ident, res Res, has bool) {
	s := r.slot(id, has)
	switch {
	case has:
		r.put(id, &s.read, res, true)
	case s != nil:
		s.read = slotRes{}
	}
}

// setWrite makes res the resolution of write id, or drops it where has is
// false.
func (r *Record) setWrite(id *ast.Ident, res Res, has bool) {
	s := r.slot(id, has)
	switch {
	case has:
		r.put(id, &s.write, res, false)
	case s != nil:
		s.write = slotRes{}
	}
}

// Reads is the resolutions of the reads the record has, in no order.
func (r *Record) Reads() iter.Seq[Res] {
	return func(yield func(Res) bool) {
		r.each(func(s *identSlot) bool { return !s.read.set || yield(r.res(s.id, s.read, true)) })
	}
}

// Writes is the resolutions of the writes the record has, in no order.
func (r *Record) Writes() iter.Seq[Res] {
	return func(yield func(Res) bool) {
		r.each(func(s *identSlot) bool { return !s.write.set || yield(r.res(s.id, s.write, false)) })
	}
}

// each calls f with each slot that is an identifier's, until f returns
// false.
func (r *Record) each(f func(*identSlot) bool) {
	for i := range r.slots {
		if s := &r.slots[i]; s.id != nil && !f(s) {
			return
		}
	}
	for _, s := range r.more {
		if !f(s) {
			return
		}
	}
}

// TopOp is one of top-level code's declarations: Op of name Name and flag
// Flag, at source position Pos, and
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

// NoteAppend records that n is an append to a local or an upvalue.
func (r *Record) NoteAppend(n *ast.Assign) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Appends == nil {
		r.Appends = map[*ast.Assign]bool{}
	}
	r.Appends[n] = true
}

// NoteTail records that call n is in tail position.
func (r *Record) NoteTail(n ast.Expr) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Tails == nil {
		r.Tails = map[ast.Expr]bool{}
	}
	r.Tails[n] = true
}

// NoteEval records that call n is a direct eval whose scope is the
// function's EvalScopes[idx].
func (r *Record) NoteEval(n *ast.Call, idx uint32) {
	if r == nil || r.quiet > 0 {
		return
	}
	if r.Evals == nil {
		r.Evals = map[*ast.Call]uint32{}
	}
	r.Evals[n] = idx
}

// WithProbe is a name's probe (Record.Withs): Op, with_get, with_get_this, with_typeof, with_delete, with_set,
// with_get_under or with_resolve, of the innermost Limit scopes.
type WithProbe struct {
	Op    bytecode.Op
	Limit uint16
}

// NoteWith records that name id probes as p says.
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
	s := r.slot(id, true)
	if s.read.set && r.res(id, s.read, true) != res {
		r.Bail = true
	}
	r.put(id, &s.read, res, true)
}

// NoteWrite records a write of id.
func (r *Record) NoteWrite(id *ast.Ident, res Res) {
	if r == nil || r.quiet > 0 {
		return
	}
	s := r.slot(id, true)
	if s.write.set && r.res(id, s.write, false) != res {
		r.Bail = true
	}
	r.put(id, &s.write, res, false)
}

// DebugMark is a place code compiled for a debugger can stop: its index
// in the function's Debug.Statements, and whether it is a debugger
// statement.
type DebugMark struct {
	Index    uint32
	Debugger bool
}

// NoteDebug records the debugger's statement of index idx at pos, the
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

// NoteUnmarked records that the walk leaves the lexical binding of slot
// out of its dead zone.
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

// ClassRec is what the walk made of one class the function defines:
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

// NoteClass records what the walk made of class cls.
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
