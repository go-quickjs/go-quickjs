// Package regcomp is the register form's backend: it compiles a function's
// AST into register code (docs/register-interpreter.md). It shares the stack
// compiler's scope resolution, which the stack compiler hands it as a Record
// of what each identifier of the function resolved to, and does its own code
// generation and register allocation.
package regcomp

import "github.com/go-quickjs/go-quickjs/internal/ast"

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
	// Tails is each call the stack form compiled as a tail call
	// (tail_call, tail_call_method, tail_direct_eval).
	Tails map[ast.Expr]bool
	// Bail says the function has something whose resolution the record
	// cannot express -- a with, an eval's variables, an Annex B function --
	// and is left to the stack form.
	Bail bool
}

// NewRecord makes an empty record.
func NewRecord() *Record {
	return &Record{Read: map[*ast.Ident]Res{}, Write: map[*ast.Ident]Res{}, Funcs: map[*ast.FuncDecl]uint32{}}
}

// NoteAppend records that n is an append to a local.
func (r *Record) NoteAppend(n *ast.Assign) {
	if r == nil {
		return
	}
	if r.Appends == nil {
		r.Appends = map[*ast.Assign]bool{}
	}
	r.Appends[n] = true
}

// NoteTail records that the stack form compiled call n as a tail call.
func (r *Record) NoteTail(n ast.Expr) {
	if r == nil {
		return
	}
	if r.Tails == nil {
		r.Tails = map[ast.Expr]bool{}
	}
	r.Tails[n] = true
}

// NoteCatch records that the binding of catch clause c is in slot.
func (r *Record) NoteCatch(c *ast.CatchClause, slot uint32) {
	if r == nil {
		return
	}
	if r.Catches == nil {
		r.Catches = map[*ast.CatchClause]uint32{}
	}
	r.Catches[c] = slot
}

// NoteFreeze records that the read id is followed by a str_freeze.
func (r *Record) NoteFreeze(id *ast.Ident) {
	if r == nil {
		return
	}
	if r.Freezes == nil {
		r.Freezes = map[*ast.Ident]bool{}
	}
	r.Freezes[id] = true
}

// NoteRead records a read of id.
func (r *Record) NoteRead(id *ast.Ident, res Res) {
	if r == nil {
		return
	}
	if old, ok := r.Read[id]; ok && old != res {
		r.Bail = true
	}
	r.Read[id] = res
}

// NoteWrite records a write of id.
func (r *Record) NoteWrite(id *ast.Ident, res Res) {
	if r == nil {
		return
	}
	if old, ok := r.Write[id]; ok && old != res {
		r.Bail = true
	}
	r.Write[id] = res
}
