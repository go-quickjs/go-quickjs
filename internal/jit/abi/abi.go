// Package abi fixes the contract between the JIT's generated code
// (internal/jit/mir) and the Go that runs it (the VM, through internal/jit):
// the context block native code is handed, and the value encoding the VM
// uses, which the JIT is given as data rather than by importing the VM.
// See internal/jit/README.md.
package abi

import "unsafe"

// SpillSlots bounds the spill area: values the register allocator cannot
// keep in registers live there, 8 bytes each.
const SpillSlots = 256

// MaxRecords bounds an exit's records, one per slot at most, and so the
// slots of a function the JIT compiles.
const MaxRecords = 256

// MaxKeeps is how many references a function's code keeps (Context.Keep).
const MaxKeeps = 256

// Record is an exit's instruction to Go for one slot, which native code
// cannot write. Slot is the slot, and its flags say what to write there:
//   - none: the value of slot Arg, a reference;
//   - RecordScalar: the primitive whose number word is Word, over a
//     reference, whose pointer word only Go may clear;
//   - RecordMaybe: the value at address Arg, a source (ssa's origin.go), if
//     that holds a reference, and otherwise -- or if Arg is 0 -- the
//     primitive Word.
//   - RecordDirect: the value whose number word is Word and whose pointer
//     word is the context's RecordRef at the record's index, as a native
//     call read them before the callee ran (ssa's OpCall).
//
// Every slot a record reads holds its value from entry still, so Go reads
// them all before it writes any.
type Record struct {
	Slot, Arg, Word, _ uint64
}

// Record flags.
const (
	RecordScalar = 1 << 63
	RecordMaybe  = 1 << 62
	RecordDirect = 1 << 61
)

// Bounds are how deep a run's native calls go, which depends on how deep
// the VM's frames were where Go entered the code, and which Go sets before
// every entry, once for every context, all of which point here
// (Context.Bounds): rewritten in each, as they were, an entry from code the
// tree tier runs recursively, its depth another at each call, wrote the
// fields of every context there was -- 96 after a deep recursion -- and
// AWFY's Towers spent a third of its time doing it. LevelLimit is the level
// past the last a native call may run in. TailLevel is the first level
// whose code's calls in tail position give up its frame, as the
// interpreter's do from tailCallDepth frames deep: native code makes them as
// calls below it, and leaves for Go to make them at it and past
// (ssa.OpTailRoom).
type Bounds struct {
	LevelLimit, TailLevel uint64
}

// HintTable is where the VM's hint table's slots are, and how many: a
// closure's hint, the uint32 at Encoding's ClosureHint, indexes it, and the
// slot holds its function's address and its link, at HintSlotFn and
// HintSlotLink, HintSlotSize bytes a slot -- what a generic call reads to
// call a function no call site was compiled for (Context.CallTarget), as
// V8's code calls a function through the code its object points to. Go
// writes it as the table grows; native code only reads it.
type HintTable struct {
	Slots unsafe.Pointer
	Len   uint64
}

// MegaCache is a runtime's megamorphic property cache, as V8's stub cache
// is: by a hash of a shape and a key (MegaSet), a set of MegaWays entries,
// an entry naming the
// object's own data property of that key in objects of that shape -- its
// index in the shape's keys and the object's values, with MegaWritable
// set if it may be written. A property read or write whose site met more
// shapes than it was compiled for, of a receiver too large to search
// (MaxScan), looks there before it leaves for Go, which fills the entry
// when it reads or writes one; a shape it names is remembered (the VM's),
// so it never changes, and the entry holds it, so its address is never
// another's. Native code only reads it.
type MegaCache [MegaEntries]MegaEntry

// MegaEntry is an entry of a MegaCache.
type MegaEntry struct {
	Shape      unsafe.Pointer
	Key, Index uint32
}

// MegaEntries is how many entries a MegaCache has, MegaWays to a set, both
// powers of two: one entry for each hash, as the shapes of a few dozen
// constructions are, had two of them share one, each displacing the other,
// in four runs of five; MegaWritable the bit of an entry's Index set for a
// writable property.
const (
	MegaEntries  = 2048
	MegaWays     = 4
	MegaSets     = MegaEntries / MegaWays
	MegaWritable = 1 << 31
)

// Offsets of MegaEntry's fields, and its size.
const (
	OffMegaShape = int32(unsafe.Offsetof(MegaEntry{}.Shape))
	OffMegaKey   = int32(unsafe.Offsetof(MegaEntry{}.Key))
	OffMegaIndex = int32(unsafe.Offsetof(MegaEntry{}.Index))
	MegaSize     = int32(unsafe.Sizeof(MegaEntry{}))
)

// MegaKeyHash is key's part of MegaSlot's hash, which native code knows
// for a site's key as it is compiled.
func MegaKeyHash(key uint32) uint32 { return key * 0x9E3779B1 }

// MegaSet is the set of a MegaCache's entries for a shape, at address
// shape, and a key, its first entry's index: the shape's address past its
// alignment, folded, with the key's hash -- shifts and exclusive ors,
// which native code makes as cheaply.
func MegaSet(shape uintptr, key uint32) uint32 {
	s := uint64(shape)
	return (uint32(s>>4) ^ uint32(s>>15) ^ uint32(s>>26) ^ MegaKeyHash(key)) & (MegaSets - 1) * MegaWays
}

// Offsets of HintTable's fields.
const (
	OffHintSlots = int32(unsafe.Offsetof(HintTable{}.Slots))
	OffHintLen   = int32(unsafe.Offsetof(HintTable{}.Len))
)

// A link's flags (Encoding's LinkFlags): LinkCoerce, a function whose
// receiver, if not an object, is made one, which Go does; LinkTail, one
// that makes calls in tail position, whose frame they may give up
// (Context.TailFrame); LinkNotGeneric, one a generic call does not make --
// an arrow's this, an arguments object, a generator's body.
const (
	LinkCoerce = 1 << iota
	LinkTail
	LinkNotGeneric
)

// Offsets of Bounds' fields.
const (
	OffBoundsLevelLimit = int32(unsafe.Offsetof(Bounds{}.LevelLimit))
	OffBoundsTailLevel  = int32(unsafe.Offsetof(Bounds{}.TailLevel))
)

// Context is the block native code reaches through its context register.
// Go sets the frame's addresses before every entry; native code writes the
// exit record and spills, and, for a native call, the callee's context. The
// fields most code reads or writes -- the frame's, how an exit left, a
// return's and a native call's, and the constants -- come first, within a
// byte's displacement of the start.
type Context struct {
	// Locals and Stack are the frame's first local and first operand: each
	// slot is an Encoding.ValueSize-byte VM value.
	Locals unsafe.Pointer
	Stack  unsafe.Pointer
	// Upvalues is the function's first captured binding's cell: an array of
	// pointers to cells, each holding at Encoding.UpvalueSlot a pointer to
	// the binding's value. Native code only reads them.
	Upvalues unsafe.Pointer
	// ExitKind is the exit record's kind, which every exit writes; the rest
	// of the record is past the constants.
	ExitKind uint64
	// Ret is a return's number word. RetFrom is 0 when that is the result,
	// or the address of the value, a reference, that is: a source (ssa's
	// origin.go).
	Ret, RetFrom uint64
	// A native call (mir's): a caller's code sets up its callee's frame in
	// the VM's stack, and its context, the next (Next), and jumps to its
	// code. ReturnTo is where a
	// return goes in its caller's code, or 0 for Run's caller in Go;
	// RetValue is the value it returns there, both words. Next and Prev
	// are the contexts a call from this one runs in and this one's caller
	// ran in, which Go links and never moves, adding more as calls need
	// them, as V8's frames take a stack that grows. Live marks a context
	// a native call runs in; Closure, Base and Level say whose
	// frame it is -- the VM's closure, the frame's index in the VM's stack,
	// and the context's in the array -- for Go to make the VM's frames from
	// when a callee leaves native code; Level stays below Bounds'
	// LevelLimit.
	// StackBase and StackEnd are the VM's stack's first value and length,
	// StackTop and StackHigh its next free and highest used indices, which
	// a call moves. TailReturn is where the records' tail goes on, in a
	// caller's code, instead of returning to Go: 0 but during a call.
	ReturnTo uintptr
	RetValue Slot
	// Consts are the words native code makes most, the tags of Encoding's
	// that ConstWords lists, which a load from here makes in four bytes
	// where a move of the constant takes ten. Go fills them in every
	// context it makes (SetConsts), and native code only reads them.
	Consts [NumConsts]uint64
	// The rest of the exit record. ExitSite is the slot IR PC of the
	// operation whose guard or exit this was, or -1 (all ones) for a
	// block's entry or loop header state -- an entry's speculation, or a
	// poll: what a policy that stops speculating where it failed needs,
	// since ExitPC is where the interpreter resumes, often the start of
	// the statement.
	ExitPC    uint64
	ExitDepth uint64
	ExitSite  uint64
	// BackEdges is the interpreter's back-edge counter, which every native
	// back-edge decrements; at zero or below native code polls.
	BackEdges  *int
	Live       uint64
	Closure    unsafe.Pointer
	Base       uint64
	Level      uint64
	Bounds     *Bounds
	StackBase  unsafe.Pointer
	StackEnd   uint64
	StackTop   *int
	StackHigh  *int
	TailReturn uintptr
	Next, Prev unsafe.Pointer
	// Global is the object a function's global names are read from, its
	// closure's scope, and LexNames the names script-level lexical bindings
	// have, which shadow them: a slice of uint64, a bit for each atom.
	// GlobalLex is the object those bindings are, when Global is the
	// realm's global scope, and nil otherwise. Go sets all three before
	// every entry; native code only reads them.
	Global, LexNames, GlobalLex unsafe.Pointer
	// StringMethods is the realm's String.prototype, where a string's
	// methods are read (ssa.OpStringProp). Go sets it before every entry.
	StringMethods unsafe.Pointer
	// CharCodeAt is String.prototype.charCodeAt, the intrinsic, when Go has
	// found it still there before an entry of a function that reads it, and
	// otherwise nothing (a nil pointer word).
	CharCodeAt Slot
	// StringProto is String.prototype, StringShape the address of the
	// shape it had, which Go keeps alive, and CharCodeAtOffset where its
	// charCodeAt's value is in its table, in bytes, when Go found the
	// intrinsic there: native code reads the method where it is now, its
	// shape checked, and compares it with CharCodeAt, so that a method
	// replaced meanwhile -- by a call native code made -- is found.
	StringProto                   unsafe.Pointer
	StringShape, CharCodeAtOffset uint64
	// This is the receiver, laid out as a VM value, which Go sets before
	// every entry of a function that reads it; uninitialized in a derived
	// constructor before super() returns.
	This Slot
	// An inlined callee's frame, which an exit inside it wrote past its
	// caller's operands (ssa's InlineState), is in the next context, whose
	// Live is LiveInline: InlineClosure is the callee's closure's address,
	// which the caller's code keeps alive and Go finds among those it
	// inlined; InlineLocals its program's locals, past which its operands
	// are; InlineThis 1 plus its receiver's slot, or 0; InlineCallee the
	// function object called, which a construction's frame has as its
	// new.target. Base is where the frame is; the exit fields say where it
	// left.
	InlineClosure, InlineLocals, InlineThis, InlineCallee uint64
	// NewTarget is the function object a native construction called, its
	// callee's new.target, which the caller's code keeps alive: written for
	// a construction only.
	NewTarget uint64
	// EnterCallee is the function object a native call called that had no
	// native code for native callers to call (ExitEnter), which its
	// caller's code keeps alive.
	EnterCallee uintptr
	// CallCallee, CallClosure and CallUpvalues are the function object a
	// native call that checks its function (ssa.CallSite's ByCode) found,
	// its closure and its captured bindings' first cell: what the call
	// gives its callee's context, kept here between its checks and its
	// frame, while the callee's operands keep the object alive.
	CallCallee, CallClosure, CallUpvalues uintptr
	// CallTarget is which of a native call's functions its checks found,
	// by its index among them, which its frame is made for: past them, a
	// function none of them is, which the call makes through its link
	// (CallLink), from the hint table (Hints).
	CallTarget uint64
	// CallLink is the link of the function a generic call found
	// (Encoding's Link fields), kept between its checks and its frame.
	CallLink uintptr
	// CallThis is the address of the this an arrow that reads it captured
	// (Encoding's ExtraLexThis), which a native call of it gives it in
	// place of the call's receiver (ssa.CallSite's ArrowThis).
	CallThis uintptr
	// Hints is the VM's hint table, shared by every context, which a
	// generic call finds its callee's link in (HintTable).
	Hints *HintTable
	// Mega is the VM's megamorphic cache, shared by every context, which a
	// property operation looks a receiver's property up in when the shapes
	// it was compiled for are not the receiver's (MegaCache).
	Mega *MegaCache
	// Open counts what closures captured of the frame's locals and is not
	// closed yet, which Go keeps, the frame's or, for a native callee's,
	// its context's: a scope's end, and a return, call Go to close them
	// only where one is (ssa.GoClose). A native call clears it for its
	// callee.
	Open uint64
	// Records counts the Record entries an exit filled. Every other slot of
	// its state is in the frame already.
	Records uint64
	Record  [MaxRecords]Record
	// RecordRef are direct records' pointer words (RecordDirect), where
	// the collector sees them; RecordHigh bounds those written, which Go
	// clears when native code returns.
	RecordRef  [MaxRecords]unsafe.Pointer
	RecordHigh uint64
	// Spill holds what the allocator could not keep in registers.
	Spill [SpillSlots]uint64
	// Keep holds references copied out of cells a store then overwrites
	// while slots still hold them (ssa's OpKeep), read from here since: the
	// collector sees them, as it does the frame's; native code writes them
	// while it does not mark, and has Go write them while it does (a call
	// of Go). Go clears them when the code returns.
	Keep [MaxKeeps]Slot
	// ExitDesc is an ExitTable exit's description, an *ExitDescriptor,
	// which the code holds; Regs and XRegs are the registers the exit
	// saved, by number, whose words the description says where to find:
	// Go writes the frame from them (jit.ApplyExit), as V8's deoptimizer
	// reads a frame state's translation.
	ExitDesc uintptr
	Regs     [32]uint64
	XRegs    [32]uint64
	// A call of Go from native code (ssa.CallSite's Go), V8's call of a
	// runtime function: GoOp says what Go does, of GoArgs, the operands'
	// words -- integers, so that native code stores no pointer: each
	// operand is also where its source has it, which the collector sees
	// (ssa's keeps) -- and GoResume is where the code goes on. Go writes
	// the result to Keep[GoKeep], with its write barrier, and GoStatus 0,
	// or 1 when it did nothing: the code then leaves for Go to make the
	// operation. Host is the runtime the context belongs to. For a call
	// (GoOpCall), GoOp's low byte is the operation, the next its
	// arguments' count, and the next its kind (GoCallMethod, GoCallNew):
	// what Go would otherwise read from the bytecode at the call.
	GoOp, GoKeep, GoStatus uint64
	GoResume               uintptr
	// GoSite is where a call of Go that saves every register goes on, in
	// the code that made it: the routine its code shares for such calls
	// (mir's goSaving) returns there.
	GoSite uintptr
	GoArgs [MaxGoArgs]GoArg
	Host   unsafe.Pointer
	// ExitMarking is 1 when the exit was the collector's marking's: a
	// native call or a pool's object, which take no pointer while it
	// marks, in code that calls no Go, or a native call with more operands
	// than GoArgs holds; Go makes the others while it marks (GoOpMarking).
	// Go clears it before each entry.
	ExitMarking uint64
	// TailFrame is 1 when the frame is one the interpreter gives up for a
	// call in tail position -- no construction's, no arrow's sharing a
	// derived constructor's this -- and 0 otherwise: Go sets it, with
	// Closure, before every entry of a function that makes such a call,
	// and a native call of one sets it for the callee (ssa.CallSite's
	// Tail).
	TailFrame uint64
	// FrameThis is where the VM's frame keeps its receiver, which a call of
	// the function itself in tail position gives anew, for a frame Go
	// entered the code with; 0 for a native callee's, whose frame Go makes
	// from This.
	FrameThis unsafe.Pointer
}

// GoArg is an operand of a call of Go: a slot's two words, its pointer word
// as an integer (Context.GoArgs).
type GoArg struct {
	Num uint64
	Ref uintptr
}

// MaxGoArgs is how many operands a call of Go takes: a call's (ssa.GoCall)
// function, receiver and up to fourteen arguments -- a function a script
// compiled to JavaScript passes its state to, as EarleyBoyer's deriv_trees
// takes nine, had left native code at every call with eight.
const MaxGoArgs = 16

// ExitDescriptor is an exit's frame state as data (an ExitTable exit): the
// slots to write and where each one's value is, the inlined callees'
// frames to describe, and the exit's own kind, PC, depth and site, which
// Go puts in the context once it has written the frame -- the frame as an
// exit's code would have written it, records and all.
type ExitDescriptor struct {
	Kind, PC, Depth uint32
	Site            int32
	// Frame is what every description of the code shares: its slots are
	// Count of Frame's, those Frame.Index lists from Start on (Slot).
	Frame *ExitFrame
	Start uint32
	Count uint16
	// The frame's locals are at LocalsReg's address, its operands at
	// StackReg's (ExitFrame's layout).
	LocalsReg, StackReg uint8
	// Direct marks a description whose slots may be written one by one:
	// none takes a reference from another it writes, by its origin or a
	// source that may be one's address.
	Direct bool
	// Inline are the inlined callees' frames, the outermost first, each in
	// a context of its own past the code's.
	Inline []ExitInline
}

// Slot is the description's i'th slot.
func (d *ExitDescriptor) Slot(i int) *ExitSlot {
	return &d.Frame.Slots[d.Frame.Index[int(d.Start)+i]]
}

// ExitFrame is what a code's exit descriptions share: the frame's layout
// -- its locals, from a description's LocalsReg's address; the receiver's
// slot, in the context; the captured bindings, through their cells; the
// operands, from its StackReg's address -- and the values' encoding; the
// slots the descriptions write, each once, which Index lists for each
// description by its index in Slots; and the constant words their values
// are (ExitConst), each once. A function's slots repeat from exit to exit,
// the same slot at the same register or spill, and its descriptions had
// been as large as its code, each with every slot it writes and every
// field of the layout: five slots in six were another's.
type ExitFrame struct {
	FrameLocals, ThisSlot, Locals int32
	Enc                           *Encoding
	Slots                         []ExitSlot
	Index                         []uint16
	Words                         []uint64
}

// ExitSlot is a slot an exit writes: its value's number word, from Value;
// and its pointer word -- from Shadow, the source the value's reference
// was read from, if it has one; from slot Origin's as the code was entered,
// if that held the reference (Load: the value is that slot's load, so its
// word need not match); none otherwise. Where a slot is, the description's
// layout says. It is 28 bytes: a function's descriptions had been as much
// as its code, at 64 a slot, the addresses of Slot and Origin among them.
type ExitSlot struct {
	Slot, Origin  int32
	Value, Shadow ExitLoc
	Load          bool
}

// ExitAddr is where a slot is: Off bytes into the locals or the operands,
// the receiver in the context, or the cell of captured binding Off.
type ExitAddr struct {
	Base uint8
	Off  int32
}

// ExitAddr's bases.
const (
	ExitInLocals uint8 = iota
	ExitInStack
	ExitInThis
	ExitInCell
)

// ExitLoc is where an exit finds a word: in a register or a spill slot as
// it is, or boxed from a float or a boolean there; a constant, the
// description's word N (Words), or 0 for an N below 0; a slot's address;
// or none (ExitNone).
type ExitLoc struct {
	N    int32
	Kind uint8
}

// ExitLoc's kinds.
const (
	ExitNone uint8 = iota
	ExitReg
	ExitSpill
	ExitConst
	ExitF64Reg
	ExitF64Spill
	ExitBoolReg
	ExitBoolSpill
	ExitSlotAddr
)

// ExitInline is an inlined callee's frame an exit describes in a context
// (LiveInline): what an exit's code writes there.
type ExitInline struct {
	Closure, Locals, ThisSlot, Callee uint64
	Kind, PC, Depth                   uint64
	Site, Base                        int64
}

// NumConsts is how many words a context holds as constants
// (Context.Consts).
const NumConsts = 7

// ConstWords are the words a context holds as constants, in Consts'
// order: the tags native code makes most, by how many sites a corpus of
// npm packages' code makes each at.
func (e *Encoding) ConstWords() [NumConsts]uint64 {
	return [NumConsts]uint64{e.Object, e.String, e.Uninitialized, NumberLimit, e.Undefined, e.True, e.False}
}

// SetConsts fills c's constants from e, which the code run in c was
// compiled for (Context.Consts).
func (c *Context) SetConsts(e *Encoding) { c.Consts = e.ConstWords() }

// Slot is a VM value's layout -- a number word, then a pointer word -- held
// where the garbage collector sees the pointer.
type Slot struct {
	Num uint64
	Ref unsafe.Pointer
}

// Offsets of Context's fields, which generated code addresses.
var (
	OffLocals    = int32(unsafe.Offsetof(Context{}.Locals))
	OffStack     = int32(unsafe.Offsetof(Context{}.Stack))
	OffBackEdges = int32(unsafe.Offsetof(Context{}.BackEdges))
	OffUpvalues  = int32(unsafe.Offsetof(Context{}.Upvalues))
	OffConsts    = int32(unsafe.Offsetof(Context{}.Consts))
	OffThis      = int32(unsafe.Offsetof(Context{}.This))
	OffGlobal    = int32(unsafe.Offsetof(Context{}.Global))
	OffLexNames  = int32(unsafe.Offsetof(Context{}.LexNames))
	OffGlobalLex = int32(unsafe.Offsetof(Context{}.GlobalLex))
	// OffStringMethods locates Context's StringMethods.
	OffStringMethods = int32(unsafe.Offsetof(Context{}.StringMethods))
	OffCharCode      = int32(unsafe.Offsetof(Context{}.CharCodeAt))
	// OffStringProto, OffStringShape and OffCharCodeAtOffset locate what a
	// string's charCodeAt is read with (Context's StringProto).
	OffStringProto      = int32(unsafe.Offsetof(Context{}.StringProto))
	OffStringShape      = int32(unsafe.Offsetof(Context{}.StringShape))
	OffCharCodeAtOffset = int32(unsafe.Offsetof(Context{}.CharCodeAtOffset))
	OffExitKind         = int32(unsafe.Offsetof(Context{}.ExitKind))
	OffExitPC           = int32(unsafe.Offsetof(Context{}.ExitPC))
	OffExitDepth        = int32(unsafe.Offsetof(Context{}.ExitDepth))
	OffExitSite         = int32(unsafe.Offsetof(Context{}.ExitSite))
	OffRet              = int32(unsafe.Offsetof(Context{}.Ret))
	OffRetFrom          = int32(unsafe.Offsetof(Context{}.RetFrom))
	OffRecords          = int32(unsafe.Offsetof(Context{}.Records))
	OffRecord           = int32(unsafe.Offsetof(Context{}.Record))
	OffRecordRef        = int32(unsafe.Offsetof(Context{}.RecordRef))
	OffRecordHi         = int32(unsafe.Offsetof(Context{}.RecordHigh))
	OffSpill            = int32(unsafe.Offsetof(Context{}.Spill))
	OffKeep             = int32(unsafe.Offsetof(Context{}.Keep))

	OffReturnTo   = int32(unsafe.Offsetof(Context{}.ReturnTo))
	OffRetValue   = int32(unsafe.Offsetof(Context{}.RetValue))
	OffLive       = int32(unsafe.Offsetof(Context{}.Live))
	OffClosure    = int32(unsafe.Offsetof(Context{}.Closure))
	OffBase       = int32(unsafe.Offsetof(Context{}.Base))
	OffLevel      = int32(unsafe.Offsetof(Context{}.Level))
	OffBounds     = int32(unsafe.Offsetof(Context{}.Bounds))
	OffTailFrame  = int32(unsafe.Offsetof(Context{}.TailFrame))
	OffFrameThis  = int32(unsafe.Offsetof(Context{}.FrameThis))
	OffStackBase  = int32(unsafe.Offsetof(Context{}.StackBase))
	OffStackEnd   = int32(unsafe.Offsetof(Context{}.StackEnd))
	OffStackTop   = int32(unsafe.Offsetof(Context{}.StackTop))
	OffStackHigh  = int32(unsafe.Offsetof(Context{}.StackHigh))
	OffTailReturn = int32(unsafe.Offsetof(Context{}.TailReturn))
	OffNext       = int32(unsafe.Offsetof(Context{}.Next))
	OffPrev       = int32(unsafe.Offsetof(Context{}.Prev))

	OffExitDesc = int32(unsafe.Offsetof(Context{}.ExitDesc))
	OffRegs     = int32(unsafe.Offsetof(Context{}.Regs))
	OffXRegs    = int32(unsafe.Offsetof(Context{}.XRegs))

	OffInlineClosure = int32(unsafe.Offsetof(Context{}.InlineClosure))
	OffCallCallee    = int32(unsafe.Offsetof(Context{}.CallCallee))
	OffCallClosure   = int32(unsafe.Offsetof(Context{}.CallClosure))
	OffCallUpvalues  = int32(unsafe.Offsetof(Context{}.CallUpvalues))
	OffOpen          = int32(unsafe.Offsetof(Context{}.Open))
	OffCallTarget    = int32(unsafe.Offsetof(Context{}.CallTarget))
	OffCallLink      = int32(unsafe.Offsetof(Context{}.CallLink))
	OffCallThis      = int32(unsafe.Offsetof(Context{}.CallThis))
	OffHints         = int32(unsafe.Offsetof(Context{}.Hints))
	OffMega          = int32(unsafe.Offsetof(Context{}.Mega))
	OffEnterCallee   = int32(unsafe.Offsetof(Context{}.EnterCallee))
	OffInlineLocals  = int32(unsafe.Offsetof(Context{}.InlineLocals))
	OffInlineThis    = int32(unsafe.Offsetof(Context{}.InlineThis))
	OffInlineCallee  = int32(unsafe.Offsetof(Context{}.InlineCallee))
	OffNewTarget     = int32(unsafe.Offsetof(Context{}.NewTarget))

	OffGoOp     = int32(unsafe.Offsetof(Context{}.GoOp))
	OffGoKeep   = int32(unsafe.Offsetof(Context{}.GoKeep))
	OffGoStatus = int32(unsafe.Offsetof(Context{}.GoStatus))
	OffGoResume = int32(unsafe.Offsetof(Context{}.GoResume))
	OffGoSite   = int32(unsafe.Offsetof(Context{}.GoSite))
	OffGoArgs   = int32(unsafe.Offsetof(Context{}.GoArgs))

	OffExitMarking = int32(unsafe.Offsetof(Context{}.ExitMarking))
)

// ObjectPool is the objects a construction site native code makes, `new
// C(...)`, takes for C's receiver: made by Go as the VM makes them for a
// construction -- C's prototype, its root shape, room for what its body
// adds -- the last Count of Objects, each cleared as native code takes it;
// Proto is the prototype they were made with, which native code compares
// C's own with first. Go fills it when the site leaves native code for
// want of one.
type ObjectPool struct {
	Objects [PoolCapacity]unsafe.Pointer
	Count   uint64
	Proto   unsafe.Pointer
	// Size is how many objects Go makes when it fills the pool, which
	// native code does not read: PoolSize at first, twice as many each
	// time the pool runs out, up to PoolCapacity.
	Size uint64
	// Source is what Go fills the pool from, the VM's, which native code
	// does not read: one that has it is filled by a call of Go when it runs
	// out (Context's GoOp), native code going on.
	Source unsafe.Pointer
}

// PoolSize is how many objects an ObjectPool is filled with at first, and
// PoolCapacity how many it may hold. A pool that runs out has native code
// leave, and every native caller's level with it (V8 allocates inline,
// from a space the collector refills): a site that constructs much is
// given more at a time.
const (
	PoolSize     = 16
	PoolCapacity = 128
)

// ObjectPool's offsets.
const (
	OffPoolObjects = int32(unsafe.Offsetof(ObjectPool{}.Objects))
	OffPoolCount   = int32(unsafe.Offsetof(ObjectPool{}.Count))
	OffPoolProto   = int32(unsafe.Offsetof(ObjectPool{}.Proto))
)

// What a context's Live says: a native call's callee runs in it, or an
// inlined callee's frame is described in it.
const (
	LiveCall   = 1
	LiveInline = 2
)

// Exit kinds, written to Context.ExitKind.
const (
	// ExitReturn: Ret holds the result's number word, or RetFrom is where
	// the result is.
	ExitReturn uint64 = iota
	// ExitDeopt: a guard failed. The frame holds the state at ExitPC with
	// ExitDepth operands; the interpreter runs from there.
	ExitDeopt
	// ExitHost: the instruction at ExitPC is for Go to run. The frame holds
	// the state before it; native code can be entered again after it.
	ExitHost
	// ExitPoll: BackEdges ran out at a loop header. The frame holds the
	// state at ExitPC, the header, where native code can be entered again.
	ExitPoll
	// ExitTable: an exit that left its frame to Go to write, from
	// ExitDesc and the registers it saved; Go then puts the exit's own
	// kind here. No one but the code that runs native code sees it.
	ExitTable
	// ExitEnter: a native call's callee, EnterCallee, had no native code
	// its native callers may call: its frame is made, its arguments in it,
	// and Go runs it from its start, as the call would have; its caller's
	// code goes on after the call (ReturnTo), as after a callee that left.
	ExitEnter
)

// Encoding is how the VM represents a value in memory: a number word (a
// float64, or a NaN-boxed tag) and a pointer word.
type Encoding struct {
	// ValueSize is the bytes per slot; NumOffset and RefOffset locate the
	// number and pointer words in a slot.
	ValueSize, NumOffset, RefOffset int32
	// Number words: a word whose top 13 bits are not all set is a number.
	// These are the words for the primitives that are not numbers.
	Undefined, Null, True, False, Uninitialized uint64
	// CanonicalNaN is the one NaN a number word may hold.
	CanonicalNaN uint64
	// UndefinedCell is the address of a value that is undefined, which
	// nothing writes: the cell of an element a read finds absent, where
	// nothing up the chain has it (ssa.GoElemCell).
	UndefinedCell uint64
	// Object is every object's number word: its pointer word tells objects
	// apart.
	Object uint64

	// What native code reads of an object, at these offsets from the
	// pointer a slot holds (internal/jit/README.md, "Reading the heap in
	// place"). It writes
	// nothing there but the number words of elements that hold numbers.
	//
	// ObjectClass is the class byte, ClassArray an array's. ObjectFlags is
	// a flags byte, in which FlagSparse marks an array with elements kept as
	// properties past its dense ones, whose length is then the uint32 at
	// ObjectArrayLen when that is larger. ObjectElems is an array's dense
	// elements' slice: a pointer to the first value, then the count.
	//
	// What only some objects have is in side fields the pointer at
	// ObjectExt points to, or nil: an ordinary object most often has none.
	// ExtElems is the elements' slice there, and ObjectData the data. An
	// array has its own, in its allocation, for as long as it lives, which
	// is how ObjectElems is an offset from the array itself.
	ObjectClass, ObjectFlags, ObjectArrayLen, ObjectElems int32
	ObjectExt, ExtElems                                   int32
	ClassArray, FlagSparse                                uint8
	// A for...of or for...in loop's cursor is an object whose data, an
	// interface at ObjectData, is the VM's iteration state when its type
	// word is IterState (0 where native code does not step one): the
	// state's byte at IterDone says the loop is done, the pointer at IterArr
	// is the array a for...of walks in place, nil for any other iteration,
	// and the int at IterIdx the next element's index. Native code writes
	// nothing there but that index.
	ObjectData                 int32
	IterState                  uint64
	IterDone, IterArr, IterIdx int32
	// A typed array is an object of class ClassTypedArray whose data's
	// type word is TypedData: a view, whose element type's byte is at
	// TypedKind (the Elem constants), its first byte's offset in its
	// buffer at TypedOffset and its length in elements at TypedLength, as
	// ints. The pointer at TypedStorage is its buffer's storage, nil until
	// Go has looked -- and for a view that tracks a resizable buffer's
	// length, always -- and the int at TypedEnd the byte past its last
	// element there: the view is in its buffer while that is no more than
	// the length of the storage's bytes, a slice at BufferBytes, which a
	// detached buffer's are not. BufferImmutable is the storage's byte that
	// is set when the bytes may not be written.
	ClassTypedArray                                             uint8
	TypedData                                                   uint64
	TypedKind, TypedOffset, TypedLength, TypedStorage, TypedEnd int32
	BufferBytes, BufferImmutable                                int32
	// A function object made from script is one whose data's type word is
	// FuncData: the pointer at FuncClosure, in the data, is its closure,
	// whose function's address is at ClosureFn, its captured bindings'
	// first cell at ClosureUpvalues, its environment at ClosureEnv and its
	// realm at ClosureRealm -- what a native call that checks the function,
	// not the object (ssa.CallSite's ByCode), reads.
	FuncData                                                          uint64
	FuncClosure, ClosureFn, ClosureUpvalues, ClosureEnv, ClosureRealm int32
	// A closure's hint is the uint32 at ClosureHint, which indexes the hint
	// table (HintTable). A link is a function's, in its runtime: the
	// address of the code native callers call at LinkEntry, 0 for none; the
	// count of their calls at LinkIn; the function's parameters, locals,
	// and locals and stack together at LinkParams, LinkLocals and
	// LinkFrame; and its flags (LinkCoerce...) at LinkFlags -- each a
	// uint64.
	ClosureHint, HintSlotSize, HintSlotFn, HintSlotLink int32
	// An arrow's function data has, at FuncExtra, a pointer to what it
	// captured where it was made: the this at ExtraLexThis, a value, and,
	// at ExtraLexThisRef, the binding of a derived constructor's this it
	// shares, nil for none.
	FuncExtra, ExtraLexThis, ExtraLexThisRef                        int32
	LinkEntry, LinkIn, LinkParams, LinkLocals, LinkFrame, LinkFlags int32
	// Mega says contexts point to a megamorphic cache (Context.Mega).
	Mega bool
	// FlagHTMLDDA is the flags byte's bit for Annex B's [[IsHTMLDDA]],
	// which makes an object == null and == undefined.
	FlagHTMLDDA uint8
	// FlagExtensible is its bit for an object properties may be added to,
	// FlagLengthWritable an array's whose length may change.
	FlagExtensible, FlagLengthWritable uint8

	// String is every string's number word. A string's UTF-8 form's data
	// pointer is at StringData, valid only when the pointer at StringLeft is
	// nil (a rope's halves); StringLength is its length in UTF-16 code
	// units, an int; a true byte at StringASCII says each byte is a code
	// unit; and StringU16, when not nil, points to its code units.
	String                                                       uint64
	StringData, StringLeft, StringLength, StringASCII, StringU16 int32
	// Symbol is every symbol's number word, its pointer word the symbol.
	Symbol uint64

	// UpvalueSlot is the offset, in a captured binding's cell, of the
	// pointer to its value (Context.Upvalues).
	UpvalueSlot int32

	// An object's properties: ObjectShape is the offset of its shape
	// pointer, which says where each property is and what it is, and
	// ObjectProps of the slice of their values, a value each, in order. An
	// object with properties has a shape, and their keys and attributes
	// are its: the slice at ShapeKeys, an entry of KeySize bytes each, at
	// the values' positions, with the key, a uint32 atom, at PropertyKey
	// and a flags byte at PropertyFlags. ObjectProto is the offset of its
	// prototype's pointer, which the shape does not settle.
	ObjectShape, ObjectProps, ShapeKeys, KeySize, PropertyKey, PropertyFlags int32
	ObjectProto                                                              int32

	// WriteBarrier is the address of the byte compiled Go tests before it
	// stores a pointer: the runtime's write-barrier flag, set while the
	// collector marks. A store that changes a pointer word exits to Go
	// while it is set, and otherwise stores, as compiled Go does. It
	// cannot change between the test and the store: Go sets it only with
	// the world stopped, which native code, never a preemption point, is
	// not. A call of Go
	// (Context.GoOp) is one, so nothing is taken for the flag across it:
	// every store tests it again.
	WriteBarrier uint64
	// CallGo is where native code jumps to call Go (Context.GoOp): the
	// address of jit's callGo, an assembly routine that, to the Go
	// runtime, the Go function that entered the code called.
	CallGo uint64
	// Round says native code may round a double to an integer, as
	// Math.floor, Math.ceil, Math.trunc and Math.round do: always on arm64,
	// with SSE4.1's ROUNDSD on amd64. The VM asks for those only where it
	// is set (ssa's OpFloorF64...).
	Round bool
	// An ordinary object of class ClassObject, whose table has at most
	// MaxScan entries, may be searched for a key, as the VM's own small
	// objects are. A property is plain data when its flags have none of
	// PropNotData, and a plain writable one when they have PropWritable of
	// PropNotWritable. PropUninit marks a binding in its temporal dead zone.
	ClassObject, PropNotData, PropNotWritable, PropWritable, PropUninit uint8
	// ClassProxy is a proxy's class, whose prototype is its handler's to
	// say, which native code leaves to Go; ClassFunction a function's, every
	// object with a function's data.
	ClassProxy, ClassFunction uint8
}

// MaxScan bounds the table native code searches for a key.
const MaxScan = 8

// MaxEqualUnits bounds the strings, in UTF-16 code units, native code
// compares by their bytes: it does not poll, and a longer comparison is
// Go's, which the collector can stop.
const MaxEqualUnits = 256

// The element types of a typed array, as its byte at TypedKind says: the
// VM's, which native code reads and writes all but Float16's of. The two
// BigInt types follow.
const (
	ElemInt8 = iota
	ElemUint8
	ElemUint8Clamped
	ElemInt16
	ElemUint16
	ElemInt32
	ElemUint32
	ElemFloat16
	ElemFloat32
	ElemFloat64
)

// NumberLimit bounds number words: a word below it holds a number, and
// every tag is at or above it.
const NumberLimit = 0xFFF8000000000000

// IsNumber reports whether a number word holds a number.
func IsNumber(word uint64) bool { return word < NumberLimit }

// A call's GoOp (Context.GoOp): the operation in its low byte, the
// arguments' count in the next, the kind in the next -- a method call's,
// with its receiver below its function, or a construction's; or an
// iterator's step (GoCallStep), its loop's cursor the one operand, the
// result its next value or, done, the uninitialized value; or a
// destructuring's step (GoCallFill), the result undefined once done; or a
// for...of's or a destructuring's cursor made (GoCallForOf) of its one
// operand, the iterable.
// GoOpMarking
// marks a call native code makes through Go only because the collector
// marks -- a native call's, whose setup would store pointers without a
// write barrier: Go makes it, and does not count it against the code's
// profit, as it does not count an exit the marking made (ExitMarking).
//
// GoOpIndexShift is where a closure's (ssa.GoClosure) is the index of its
// function's constant, and a scope's end's (ssa.GoClose) the first local it
// closes, in the high half.
const (
	GoOpArgcShift  = 8
	GoOpKindShift  = 16
	GoOpIndexShift = 32
	GoCallMethod   = 1
	GoCallNew      = 2
	GoCallStep     = 3
	GoCallFill     = 4
	GoCallForOf    = 5
	GoOpMarking    = 1 << 24
)
