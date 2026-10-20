// Package ir defines the pointer-free slot IR used to develop the optional
// native executor. Its Go evaluator is a correctness oracle, not a VM tier.
package ir

import (
	"math"
	"unsafe"
)

// ArrayView borrows dense storage for one bounded native entry. Each cell is
// sixteen bytes: numeric bits followed by a Go reference that native code must
// never change. Bits below NumberLimit identify existing numeric data properties.
// WritableHole, when nonzero, permits replacing that pointer-free hole marker
// with a number. The caller proves ordinary writable/extensible array storage
// and absence of inherited indexed properties before granting this permission.
// The caller owns and roots the storage, and rebuilds views after every callback.
// With NumberLimit zero, Data instead borrows an ordinary object's property
// table, DenseLength counts at most MaxProperties cells, and WritableHole is
// the numeric tag boundary. The two permissions are mutually exclusive.
// With both NumberLimit and WritableHole zero, a nonzero Length grants a
// ReferenceCell table instead: Length is its numeric tag boundary. Numeric
// operations follow Cell to live storage; reference reads additionally check
// the snapshot identity before copying its rooted handle. These permissions
// preserve the same 40-byte view and numeric native-entry ABI.
// Missing or incompatible fields exit to the host.
type ArrayView struct {
	Data         unsafe.Pointer
	DenseLength  uint64
	Length       uint64
	NumberLimit  uint64
	WritableHole uint64
}

// String handles reuse the view ABI: Data borrows flat bytes or UTF-16 units,
// DenseLength counts code units, and Length is the unit width (one or two).
// WritableHole, when nonzero, is the rooted intrinsic charCodeAt handle plus
// one, granted only after an ordinary live prototype lookup. String's distinct
// kind prevents numeric array/property operations from using this storage.
// The intrinsic's Opaque view has DenseLength=CharCodeAtBuiltin and no other
// permissions. The adapter retains all owners and refreshes after callbacks.
const CharCodeAtBuiltin = uint64(1 << 61)

// MaxProperties bounds the native linear search within one IR instruction.
const MaxProperties = 8

// PropertyCell describes borrowed own data. Only Flags' low three bits are
// ordinary attributes; bit zero grants writes. Native stores change Bits only
// after proving the existing value numeric, leaving Reference untouched.
type PropertyCell struct {
	Key       uint32
	Flags     uint8
	_         [3]byte
	Bits      uint64
	Reference unsafe.Pointer
}

// ReferenceCell grants a read of one selected own field. Cell borrows
// the live property; Bits and Reference must still match before Handle is used.
// Handle equal to MaxSlots denies a reference read. Numeric reads and writes
// use Cell's live value. Native code never writes or publishes the Go pointers.
type ReferenceCell struct {
	Key       uint32
	_         [4]byte
	Bits      uint64
	Reference unsafe.Pointer
	Cell      *PropertyCell
	Handle    uint64
}

// Kind identifies a scalar or a handle into Go-owned reference storage.
type Kind uint64

const (
	Undefined Kind = iota
	Number
	Boolean
	Null
	Uninitialized
	Opaque
	String
)

// Value is pointer-free. Opaque Bits is an index into a caller-owned root
// table; it is never a Go address. The caller keeps that table alive.
type Value struct {
	Bits uint64
	Kind Kind
}

// Float preserves IEEE bits, including signed zero. The VM adapter must use
// the VM's numeric constructor when publishing it, to canonicalize NaNs.
func Float(n float64) Value { return Value{Bits: math.Float64bits(n), Kind: Number} }

// Bool returns the scalar representation of a JavaScript boolean.
func Bool(b bool) Value {
	if b {
		return Value{Bits: 1, Kind: Boolean}
	}
	return Value{Kind: Boolean}
}

// Operand selects a slot, or Literal when Slot is -1.
type Operand struct {
	Slot    int
	Literal Value
}

// Slot returns an operand that reads slot n.
func Slot(n int) Operand { return Operand{Slot: n} }

// Literal returns an operand that reads a scalar constant.
func Literal(v Value) Operand { return Operand{Slot: -1, Literal: v} }

// Op identifies a slot instruction. Each instruction commits atomically with
// respect to guards: a guard exit retains the entire pre-instruction state.
type Op uint8

const (
	Nop Op = iota
	Copy
	CopyPair
	StoreLoad
	Swap
	Binary
	Unary
	Update
	Jump
	Branch
	Return
	ArrayRead
	ArrayWrite
	ArrayLength
	ArrayKey
	ArrayUpdate
	Insert3
	Host
	Insert2
	PropertyRead
	PropertyWrite
	BindingRead
	ReferenceRead
	Call
	StringMethod
	StringCode
	// TypeTest is typeof Left === Key's name (TypeNumber...), negated when
	// When is false, as a boolean in Dest: only the new pipeline's lowering
	// makes it, of typeof compared with a string constant.
	TypeTest
	// BindingWrite assigns Left to the global binding of the name Key, as
	// set_global does: the new pipeline's, made natively where the VM knows
	// the binding, a writable data property of the global object.
	BindingWrite
	// BindingCheck is strict mode's check_global_ref: true at Dest if the
	// name Key resolves, natively where the VM knows its binding (the global
	// object's data property), Go's otherwise.
	BindingCheck
	// Resolved is assert_resolved: Right at Dest if Left, the check's
	// answer, is true; else Go throws the ReferenceError.
	Resolved
	// ObjectLiteral is new_object, an object literal's object, at Dest:
	// natively from its site's pool (ssa.LiteralSite), Go's otherwise.
	ObjectLiteral
	// FieldDefine is define_field: Right made Left's own property Key, as
	// a literal makes it, natively along the shapes' transition
	// (ssa.PropertyAdd's Define), Go's otherwise.
	FieldDefine
	// ArrayLiteral is new_array: an array of the Extra operands from Dest
	// on, at Dest, natively from its site's pool (ssa.LiteralSite), Go's
	// otherwise. At most MaxArrayLiteral elements.
	ArrayLiteral
	// StringConst is push_const of a string, constant Key, at Dest: read
	// natively from a cell the VM keeps it in (ssa.ConstantFeedback), Go's
	// otherwise.
	StringConst
	// UpvalueRead is get_upvalue: the captured binding Key's value, at
	// Dest, read from its cell each time (with Check, an uninitialized one
	// is the interpreter's to throw). UpvalueWrite is set_upvalue: Left
	// stored into it (with Check, as set_upvalue_check: not into an
	// uninitialized one).
	UpvalueRead
	UpvalueWrite
	// StringAdd is Binary's Add where an operand may be a string (compile's
	// stringConcats): Left + Right at Dest, which the new pipeline has Go
	// make, called from native code (ssa.GoAdd), and leaves to Go
	// otherwise.
	StringAdd
	// StringConcat is concat: the Extra operands from Dest on, each
	// converted to a string and joined, at Dest, which the new pipeline
	// has Go make, called from native code (ssa.GoConcat), and leaves to
	// Go otherwise. At most MaxStringConcat parts.
	StringConcat
	// Rethrow is rethrow, the end of a finally clause, with Left the kind
	// of its completion record, the operand on top: a normal completion's
	// record (the number 0) is dropped and the code goes on; any other --
	// a throw, a return -- the interpreter reproduces, from a guard's exit.
	Rethrow
	// IterStep is iter_next_or_jump, a for...of or for...in loop's step:
	// the next value of the cursor at Left at Dest, or, the cursor done, a
	// jump to Target, the cursor kept either way. The new pipeline has Go
	// make it, called from native code (ssa's GoCall, Step), and leaves
	// it to Go otherwise.
	IterStep
	// Closure is closure, the function of constant Key made at Dest,
	// capturing the frame's locals where the frame has them (Program's
	// Captured). The new pipeline has Go make it, called from native code
	// (ssa.GoClosure), and leaves it to Go otherwise.
	Closure
	// CloseUpvalues is close_upvalues, a scope's end: what closures
	// captured of locals Key on is closed, the value kept for them. Go's,
	// called from native code (ssa.GoClose), or left to it.
	CloseUpvalues
	// IterClose is iter_close_normal, a for...of's or a destructuring's
	// end: the cursor at Left closed, which the new pipeline does natively
	// where there is nothing to close -- the cursor done, or an array's
	// walked in place -- and leaves to Go otherwise.
	IterClose
	// HasProperty is in, Left in Right, as a boolean at Dest: whether the
	// object Right, or one up its chain, has the property Left names, as
	// V8's code calls its HasProperty builtin. The new pipeline has Go
	// answer it, called from native code (ssa.GoHas), where that runs
	// nothing and throws nothing, and leaves it to Go otherwise: a
	// proxy's has trap, a key whose conversion calls a method of its own,
	// a right operand that is no object, which throws.
	HasProperty
	// DeleteProperty is delete, delete Left[Right], as a boolean at Dest
	// where Left was: whether the object Left no longer has the own
	// property Right names, as V8's code calls its DeleteProperty builtin.
	// Strict marks one in strict code, where a property that cannot be
	// deleted throws. The new pipeline has Go make it, called from native
	// code (ssa.GoDelete), where that runs nothing and throws nothing, and
	// leaves it to Go otherwise: a proxy's deleteProperty trap, a key
	// whose conversion calls a method of its own, a property strict code
	// cannot delete, a base that is no object.
	DeleteProperty
	// ArgumentRead is arguments_index, arguments[Left] at Dest, in a
	// function whose arguments object is made only on demand and never
	// pushed (bytecode's ForwardsArguments), lowered for inlining
	// (LowerSSAForward): an inlined call has the new pipeline read the
	// call's own argument for a constant index below their count that names
	// no aliased parameter, as V8 eliminates the arguments object, and
	// leaves any other to Go, which makes the object.
	ArgumentRead
	// BigIntConst is push_const of a BigInt, constant Key, at Dest: read
	// natively from a cell the VM keeps it in (ssa.BigIntConstFeedback),
	// as StringConst reads a string, Go's otherwise.
	BigIntConst
)

// RethrowNormal is the kind a normal completion's record has (Rethrow).
const RethrowNormal = 0

// MaxArrayLiteral is the most elements an ArrayLiteral has: new_array
// with more is Go's.
const MaxArrayLiteral = 16

// MaxStringConcat is the most parts a StringConcat has, as many as a call
// of Go takes (abi.MaxGoArgs): concat with more is Go's.
const MaxStringConcat = 8

// The types a TypeTest asks about.
const (
	TypeNumber = iota + 1
	TypeString
	TypeBoolean
	TypeUndefined
	TypeFunction
)

// Operator selects an arithmetic, comparison, or truthiness operation.
type Operator uint8

const (
	Add Operator = iota
	Sub
	Mul
	Div
	Lt
	Le
	Gt
	Ge
	Eq
	Ne
	Neg
	Pos
	Not
	Truth
	Int32
	BitAnd
	BitOr
	BitXor
	Shl
	Shr
	UShr
	BitNot
	// Mod is JavaScript's % on numbers, C's fmod: only the new pipeline's
	// lowering makes it, and a non-number exits to Go, as Eq does.
	Mod
	// Nullish is a Branch's condition that Left is null or undefined, as
	// ?? and ?. test it: nothing converted, and no object with Annex B's
	// [[IsHTMLDDA]], which == null alone takes for null. Only the new
	// pipeline's lowering makes it.
	Nullish
	// Pow is **, as Math.pow computes it of numbers, and a BigInt's power:
	// only the new pipeline's lowering makes it, and what it does not make
	// natively exits to Go.
	Pow
)

// Instruction reads Left and Right and writes Dest (and Extra for paired
// operations). StoreLoad stores Left into Dest before reading Right into
// Extra, so aliased locals match bytecode's sequential store/load behavior.
// Update stores the increment/decrement in Dest and, when Extra >= 0, pushes
// either the old or new value there. Branch takes Target when its condition
// equals When; otherwise execution falls through. Check guards CheckSlot
// against the temporal dead zone before any writes.
// ArrayRead/ArrayWrite use Left as the array handle and Right as the numeric
// index; Third is the stored number. ArrayUpdate commits an updated index in
// Extra only after the read succeeds. ArrayKey guards without converting a key.
// Host exits before executing the corresponding VM instruction.
// PropertyRead/PropertyWrite and BindingRead search Left's table for Key. A
// BindingRead borrows one resolved numeric binding cell. Right supplies
// the stored number; any failed permission or type check takes a host exit.
// Call reads its function from Left, its optional receiver from Right, and
// Extra arguments starting at Third.Slot. Dest receives the result. Key is
// the sequential call-site index. Ordinary evaluation exits before the call;
// a prepared native dispatch graph may transfer to a guarded callee instead.
// StringMethod reads Left's intrinsic permission into Dest. StringCode reads
// the callee from Left, the string receiver from Right and the index from Third.
// Failed string permissions take an uncommitted Host exit.
type Instruction struct {
	Op        Op
	Operator  Operator
	Left      Operand
	Right     Operand
	Third     Operand
	Dest      int
	Extra     int
	Target    int
	When      bool
	Postfix   bool
	Check     bool
	CheckSlot int
	Key       uint32
	// Bools marks, bit 0 for Left and bit 1 for Right, an operand of a
	// Binary that is no equality, or of a Unary but Not, that a comparison
	// or a Not wrote in the same block -- with only Binary, Unary, Copy,
	// CopyPair, StoreLoad and Nop instructions between -- a boolean: the operator takes its
	// number, 1 or 0, as ToNumber has it, which the new pipeline makes
	// natively (ssa.OpBoolToF64). Emscripten's code combines comparisons
	// with | and &, and Octane's zlib had left native code at two such
	// operators in every call of its hottest functions.
	Bools uint8
	// Strict marks an Eq or Ne that is === or !==. On numbers the two
	// agree; the new pipeline compares with null and undefined natively,
	// where they do not. It marks a DeleteProperty in strict code, too.
	Strict bool
	// Reference marks an ArrayRead whose result no native operation takes
	// as a number: the new pipeline reads the element's cell, whatever the
	// element holds, as it reads a property's.
	Reference bool
	// An IterStep with When is a destructuring's step, which goes on with
	// undefined once the iterator is done rather than branching to Target.
	//
	// Tail marks a Host that is a call in tail position, which gives up
	// its frame deep enough in the stack: the new pipeline makes it as a
	// call, but there, where it makes a call of the function itself a
	// jump to its start, and Go makes any other as the interpreter does.
	// Extra is its arguments' count; When marks a method's call, whose
	// receiver is below the function.
	Tail bool
	// Private marks a PropertyRead or PropertyWrite of a private member,
	// whose key -- a symbol each evaluation of its class mints -- is in
	// the binding Third names: the read or write is native where the key
	// is the one the code was compiled for. A read with When is #x in o:
	// true where the read would find the member.
	Private bool
}

// StateMap describes state immediately before a bytecode instruction. PC is
// the instruction to resume, not the VM's PC after fetching it. Depth is the
// live operand count; -1 marks unreachable code. Slots [0, Locals) hold locals
// and read-only captured bindings, receiver snapshots and binding-view handles;
// the next Depth slots hold operands.
type StateMap struct {
	PC    uint32
	Depth int
}

// Program is immutable after lowering. Code and Maps have one entry per
// bytecode instruction, including unreachable instructions. Construction is
// the responsibility of a validated bytecode adapter.
type Program struct {
	Locals    int
	StackSize int
	// This reserves a read-only receiver snapshot after captured bindings.
	This bool
	// ArrowThis marks an arrow that reads this, the one it captured: a call
	// of itself in tail position, made a jump to its start (ssa's
	// OpTailSelf), would give it the call's receiver in its place, so
	// native code makes its calls in tail position as calls, and Go those
	// deep in the stack, which give its frame up (OpTailRoom). It had been
	// refused, and AWFY's Richards ran its task functions, all such arrows,
	// in the tree tier.
	ArrowThis bool
	// ClassConstructor marks a class's constructor, which a call without
	// new throws on: a call of itself in tail position is made as a call,
	// for Go to throw, not a jump to its start (ssa's OpTailSelf).
	ClassConstructor bool
	// DerivedConstructor marks a derived class's constructor, whose this
	// slot (This) holds the uninitialized value until its super() binds
	// it, and which returns that this: native code makes a return of
	// undefined its this, Go any other, and Go a return before super()
	// with this unbound, which throws.
	DerivedConstructor bool
	// Globals contains source-name indices for reserved binding-view slots,
	// following captured bindings and the optional receiver snapshot.
	Globals []uint32
	Code    []Instruction
	Maps    []StateMap
	// Live says, for each instruction, which of the first LiveLocals
	// slots -- the function's locals -- the code from there on may read
	// before it writes them: LiveWords words of bits a PC, from Live[pc *
	// LiveWords]. A local not live there need not be written for the
	// interpreter to resume at it, as V8's frame states leave out dead
	// registers. Nil where it is not known.
	Live       []uint64
	LiveLocals int
	LiveWords  int
	// Upvalues is how many captured bindings the new pipeline reads and
	// assigns in their cells (UpvalueRead, UpvalueWrite), their slots from
	// UpvalueBase on.
	UpvalueBase, Upvalues int
	// Params is how many of the function's locals, from the first, are
	// its parameters, which a call's arguments are.
	Params int
	// Handlers are the exception handlers the function registers, and
	// HandlerAt, for each instruction, the innermost of those in force
	// there, 1 plus its index, 0 for none: the interpreter's frame has them
	// (its handlers) wherever native code leaves it. Nil where the function
	// has none.
	Handlers  []Handler
	HandlerAt []int32
	// Entries are instructions native code is entered at besides the
	// first: where a generator's or an async function's resumptions first
	// enter it, past its parameters (end_params), which run when the
	// generator is made.
	Entries []int
	// Captured marks the locals inner closures capture, by slot, or is nil
	// for none: a closure reads and assigns such a local where the frame
	// has it, so native code does too, as it does a captured binding in
	// its cell (IsCaptured).
	Captured []bool
	// Assigned marks, of those, the locals a closure assigns, or is nil for
	// none: a call may change those, which run no closure that assigns the
	// others (IsAssigned).
	Assigned []bool
	// Cells marks a program whose captured locals are in cells of their
	// own from each entry, as V8 allocates a function's context, rather
	// than read in its frame's slots (the VM's GoCells): open on the frame
	// Go entered the code with, which closes them as it is popped; closed
	// for a native callee, whose frame Go does not keep. A closure it makes
	// captures those cells, which native code may write into one it takes
	// from a pool, and no return closes anything.
	Cells bool
}

// IsCaptured reports whether local i is one an inner closure captures
// (Captured).
func (p *Program) IsCaptured(i int) bool { return i >= 0 && i < len(p.Captured) && p.Captured[i] }

// IsAssigned reports whether captured local i is one a closure assigns
// (Assigned).
func (p *Program) IsAssigned(i int) bool { return i >= 0 && i < len(p.Assigned) && p.Assigned[i] }

// Handler is an exception handler, as push_catch or push_finally registers
// it: the clause at PC, entered with the operand stack cut to Depth, a
// finally clause's if Finally, and Outer the handler in force around it, 1
// plus its index in Program.Handlers, 0 for none.
type Handler struct {
	PC      uint32
	Depth   int32
	Outer   int32
	Finally bool
}

// Footprint is the memory p holds: its instructions, state maps and
// liveness, which a runtime that keeps p counts (the VM's JIT budget).
func (p *Program) Footprint() int {
	if p == nil {
		return 0
	}
	return int(unsafe.Sizeof(*p)) + cap(p.Globals)*4 + cap(p.Code)*int(unsafe.Sizeof(Instruction{})) +
		cap(p.Maps)*int(unsafe.Sizeof(StateMap{})) + cap(p.Live)*8 + cap(p.Handlers)*int(unsafe.Sizeof(Handler{})) + cap(p.HandlerAt)*4 + cap(p.Captured) + cap(p.Assigned)
}

// LiveAt reports whether local i may be read from pc on before it is
// written (Live); true where that is not known.
func (p *Program) LiveAt(pc, i int) bool {
	if p.Live == nil || i >= p.LiveLocals {
		return true
	}
	return p.Live[pc*p.LiveWords+i/64]&(1<<(i%64)) != 0
}

// ExitKind identifies a completed return or a resumable exit.
type ExitKind uint8

const (
	Returned ExitKind = iota
	GuardExit
	BudgetExit
	HostExit
)

// MaxCallSites bounds the call instructions in one program; each has its
// index in Key.
const MaxCallSites = 16

// Exit records a result or the exact interpreter state to resume. Steps counts
// committed IR instructions; a failing guard does not consume a step.
type Exit struct {
	Kind  ExitKind
	State StateMap
	Value Value
	Steps uint64
}
