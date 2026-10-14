// Package ssa is the JIT's optimizing IR: a typed SSA form built from the
// slot IR (internal/jit/ir), whose state maps become the frame states that
// every guard and exit carries. See internal/jit/README.md.
//
// A function has several entries: the function's start, each loop header
// (where a poll returns and on-stack replacement enters), and the instruction
// after each instruction left to Go whole and each call (where Go resumes it
// after running that instruction). After a guard's exit to Go the
// interpreter goes on, unless the VM asks for an entry after every one
// (EntryFeedback). Every exit names the exact interpreter state to resume,
// so deoptimization stays exact by construction, as it is in the slot IR.
//
// Values are typed. A slot holds a Tagged value -- an ir.Value, a kind and
// its bits -- and arithmetic works on unboxed Float64, Int32 and Bool values,
// reached through guards that exit when a value is not what the operation
// needs.
package ssa

import (
	"fmt"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// Type is a value's representation.
type Type uint8

const (
	// Tagged is a slot value: an ir.Value of any kind.
	Tagged Type = iota
	Float64
	// Int32 also carries ToUint32's results, as their bits.
	Int32
	Bool
	// None is a value used only for its effect or control: a guard, a check.
	None
	// Ptr is an object's address, read from the frame slot that holds it.
	// It never reaches a slot or crosses an exit.
	Ptr
	// Source is where a tagged value came from, at run time: the address of
	// a slot's value or of the heap cell it was loaded from, or 0 for a
	// primitive (origin.go).
	Source
)

func (t Type) String() string {
	return [...]string{"tagged", "f64", "i32", "bool", "none", "ptr", "source"}[t]
}

// Op is a value's operation.
type Op uint8

const (
	OpInvalid Op = iota

	// Values.
	OpLoadSlot    // Aux: the slot, read from the frame at an entry.
	OpConst       // Const: a tagged constant.
	OpConstF64    // Const.Bits: the number's bits.
	OpConstSource // Aux: a slot's index, or -1: its source, or 0 (origin.go)
	OpPhi         // Args: one per predecessor, in Block.Preds order.

	// Guards and checks: each has a frame state and exits to it when it
	// fails. Aux is the exit kind (ir.GuardExit or ir.HostExit).
	OpUnboxF64  // tagged -> f64, if a number
	OpTruth     // tagged -> bool, if undefined, null, a boolean or a number
	OpCheckInit // tagged -> none, if not uninitialized

	// Arrays, read in place. The guards' semantics are the slot IR's
	// array views' (ir.ArrayView), less writes into holes: an array of class
	// Array, elements that hold numbers, and an index that is an integer
	// below 2**32.
	OpArrayOf   // tagged -> ptr, if an array
	OpElemKey   // f64 -> none, if an index
	OpElemRead  // ptr, f64 -> f64: the element, if the index's is a number
	OpElemWrite // ptr, f64, f64, source... -> none: stores, if the index's is a number
	// OpIndexedOf is OpArrayOf for an element read or written as a number
	// (OpElemRead, OpElemWrite), which may be a typed array's too: an object
	// of either class, which they tell apart as they run. A typed array's
	// element is in its buffer, below its length; one of a type native
	// code does not make -- Float16, a BigInt -- or a view out of its
	// buffer, is the guard's failure, or the store's call of Go.
	OpIndexedOf // tagged -> ptr, if an array or a typed array
	// OpElemCell is an element read as a reference: the element's cell, if
	// the index is an integer within the dense elements and the element is
	// not a hole, whatever it holds. OpLoadCell reads it, with the cell as
	// its shadow (origin.go).
	OpElemCell // ptr, f64 -> source
	// OpElemStore stores any value -- tagged, its pointer word found where
	// it came from, as OpPropWrite's -- in an Array's element that is
	// there and not a hole, the cells a reference live after it may have
	// been read from checked first (storeChecks); anything else is Go's
	// (GoElemStore). One whose value is a number is OpElemWrite's
	// (simplify).
	OpElemStore // ptr, f64, tagged, source... -> none
	// OpLength is x.length of an array, as the slot IR's views have it, or
	// of a string, whose length is always at hand, rope or not.
	OpLength // tagged -> f64

	// Properties, read in place. Where the VM's cache for the site knows
	// a shape (Const.Bits, the shape's address, which the VM keeps alive; 0
	// if none), an object of it has the property at Index of its table, a
	// writable one for a write. Any other ordinary object whose table is
	// small is searched for Key, as the VM searches it: the property must be
	// its own plain data, and writable for a write. Values are numbers, as
	// in the slot IR; anything else exits to Go.
	OpObjectOf  // tagged -> ptr, if an object
	OpPropRead  // ptr -> f64: the property, if the shape's and a number
	OpPropWrite // ptr, tagged, source... -> none: stores, if the shape's;
	// or, for a write whose cache adds the property (Add), adds it to an
	// object it may be added to, and stores it into one that has it.
	// A reference read from an object: the property's cell, found as a read
	// finds it, and the value there, whatever it is. The value's Shadow is
	// the cell, which an exit has Go copy from (origin.go).
	OpPropCell // ptr -> source: the property's value's address
	OpLoadCell // source -> tagged: the value at a cell
	// A global name's cell (abi.Context.Global): the binding at Index of
	// the global object's table, if it is Key's and plain, initialized
	// data, and no script-level lexical binding of the name shadows it.
	OpGlobalCell // -> source
	// A script-level lexical binding's cell (abi.Context.GlobalLex): the
	// binding at Index of their object's table, if it is Key's and
	// initialized. The bindings are never removed, nor their attributes
	// changed.
	OpLexCell // -> source
	// A binding's cell in an object the VM keeps, at Const's address: the
	// binding at Index of its table, if it is Key's and plain, initialized
	// data -- another module's binding a module imported, or a global a
	// module's code reads (ssa.GlobalSite's Holder). The first's object
	// gains and loses no binding, the second's, the global object, may: if
	// Global, no script-level lexical binding of the name shadows it.
	OpBindingCell // -> source
	// charCodeAt, as the slot IR's string kernels call it. OpStringMethod is
	// a string's charCodeAt: the context's cell (abi.Context.CharCodeAt),
	// if the receiver is a string and the cell holds the intrinsic.
	// OpStringCode is a call of it: the code unit at an index of a flat
	// string, if the callee is the intrinsic.
	OpStringMethod // tagged -> source
	// A string's property, a method, where String.prototype has it
	// (abi.Context.StringMethods): its cell at Index, if the operand is a
	// string, String.prototype's shape is Const's, the one the code was
	// compiled for, and the entry at Index is Key's.
	OpStringProp // tagged -> source
	OpStringCode // tagged callee, tagged string, f64 index -> f64

	// Boxing: a typed value as a slot value.
	OpBoxF64
	OpBoxBool

	// Arithmetic on numbers.
	OpAddF64
	OpSubF64
	OpMulF64
	OpDivF64
	// OpModF64 is JavaScript's % of two numbers, which native code divides
	// where they are integers below 2**63 in magnitude, the divisor not
	// zero; anything else Go computes, as math.Mod does -- a call of Go from
	// native code that can make one (GoMod), an exit (Aux) from code that
	// cannot.
	OpModF64
	OpNegF64 // flips the sign bit, as the slot IR does, NaN included
	OpCmpF64 // Aux: ir.Lt, ir.Le, ir.Gt, ir.Ge, ir.Eq or ir.Ne -> bool
	OpNot    // bool -> bool
	// Equality with null or undefined, whatever the other operand is, as
	// JavaScript defines it. OpStrictNullish is x === null (Aux 0) or
	// x === undefined (Aux 1): its word is that one -- or either (Aux 2),
	// as ?? and ?. test it (ir.Nullish). OpLooseNullish is
	// x == null, as == undefined: the word is either, or an object's with
	// Annex B's [[IsHTMLDDA]] (abi.Encoding.FlagHTMLDDA), which it reads
	// through the object's origin, as OpObjectOf finds it, exiting to Go
	// (Aux) where that cannot name it.
	OpStrictNullish // tagged -> bool
	OpLooseNullish  // tagged -> bool
	// OpEqTagged is x === y (Index 1) or x == y (Index 0) of any two
	// values, natively where it can be told from their words: equal words
	// are equal values -- a number but NaN, null, undefined, a boolean --
	// but for objects, whose word all share, and which are the same only if
	// their pointers, read through their origins, are; two numbers compare
	// as numbers; any other two words are strictly unequal. Two strings are
	// the same if they are one string, and unequal if their lengths differ;
	// two others of one length exit to Go (Aux). Loosely, null and undefined
	// equal each other and an object with [[IsHTMLDDA]] alone, read as
	// OpLooseNullish reads it, with nothing converted; any other two words,
	// which may convert, exit to Go (Aux).
	OpEqTagged // tagged, tagged -> bool

	// Integer conversions and bitwise operations, as JavaScript defines them.
	OpToInt32  // f64 -> i32: ToUint32's bits
	OpAndI32   // i32, i32 -> i32
	OpOrI32    //
	OpXorI32   //
	OpShlI32   // shift counts are masked to five bits
	OpSarI32   //
	OpShrU32   // i32, i32 -> i32 holding the unsigned result's bits
	OpNotI32   //
	OpI32ToF64 // signed
	OpU32ToF64 // unsigned
	// OpBoolToF64 and OpBoolToI32 are a boolean as a number, 1 or 0, as
	// ToNumber has it: a comparison an arithmetic or bitwise operator
	// takes, (a < b) | (c < d) -- what Emscripten's code does -- which
	// had left native code at the operator, its operand's unbox failing.
	OpBoolToF64 // bool -> f64
	OpBoolToI32 // bool -> i32

	// OpSameObject guards that an object is the one at Const.Bits, an
	// address the VM keeps alive: an inlined call's function (inline).
	OpSameObject // ptr -> none
	// OpFrameRoom guards that the VM's stack has Index slots past the
	// operands' first and that Const.Bits contexts follow this one: room
	// for an inlined callee's frame, and its callers' if they are inlined
	// too, which an exit inside it writes there (InlineState).
	OpFrameRoom // -> none
	// OpTailRoom guards that a call in tail position is made where the
	// stack is shallow (abi.Bounds' TailLevel): deeper, Go makes it,
	// giving up the frame, as the interpreter's tail_call does.
	OpTailRoom // -> none
	// OpTailSelf is OpTailRoom where the call may be the function's own:
	// deep in the stack, a call of the closure the frame runs (abi.Context's
	// Closure), from a frame the interpreter gives up (TailFrame), makes
	// its arguments, Args past the function, the frame's parameters --
	// Index of them, the rest of its locals, to Const's, undefined -- and
	// enters the code again at its start, as V8's code jumps to itself
	// for a call in tail position: a loop, in the frame it had. Any other
	// call there leaves for Go -- but, in a native callee's frame, a call
	// of one of the functions it has (its CallSites), which takes the
	// frame's place, its context the frame's, and goes on in its code,
	// returning where the frame would have. Key's bit 1 says the last of
	// Args is the receiver the call gives, bit 2 that the code reads it.
	OpTailSelf // function, arguments -> none
	// OpSameSymbol guards that a value is the symbol at Const's address: a
	// private member's key, which each evaluation of its class mints
	// anew, the one its code was compiled for (ir.Instruction's Private).
	OpSameSymbol // value -> none
	// OpIterClosed guards that a cursor has nothing to close: its loop or
	// destructuring done, or an array's walked in place (the VM's
	// iterCloseNormal), which no return method ends.
	OpIterClosed // cursor -> none
	// OpKeepRef is a tagged value's pointer word (Args[0]), read where it
	// came from (origin.go), or 0 for a primitive's; 0 while the collector
	// marks. Every keep at a store reads first (keep.go).
	OpKeepRef // tagged -> ptr
	// OpKeep copies a tagged value (Args[0]) into the context's keep cell
	// Index (abi.Context.Keep) -- its number word, and its pointer word
	// (Args[1], OpKeepRef) -- and is that cell's address: a value read from
	// a cell a store then overwrites is read from there since (keep.go).
	// While the collector marks Go writes it, called from native code.
	OpKeep // tagged, ptr -> source
	// OpKept is a tagged value (Args[1]) whose shadow is a keep (Args[0]).
	OpKept // source, tagged -> tagged

	// OpCall calls natively one of the functions Calls are, as V8's code
	// calls another's: the call's operands are its arguments -- the
	// receiver, for a method, the function, then the call's arguments --
	// and its result is the callee's number word, its pointer word in the
	// context's keep cell Index (OpCallCell). It fails, its state's exit,
	// Go making the call, when it is none of them or cannot be made
	// natively. Code goes on after it: what is used after it is in memory
	// (mir's allocate), and a reference read from a cell is kept before it
	// (keep.go), since the callee may write any cell; if the callee leaves
	// native code, Go makes this frame from the call's state, its records
	// (abi.RecordDirect).
	OpCall // tagged... -> tagged
	// OpCallCell is the keep cell a call's result's pointer word is in.
	OpCallCell // tagged -> source

	// OpSqrtF64 and OpAbsF64 are Math.sqrt's and Math.abs's of a number
	// (Intrinsic), as V8 computes them in its code: the square root, NaN
	// for a negative; the sign cleared, NaN included.
	OpSqrtF64 // f64 -> f64
	OpAbsF64  // f64 -> f64
	// OpInstanceOf is the instanceof operator's ordinary answer
	// (InstanceOfSite): whether a value (Args[0]) has the object Args[1]
	// among its prototypes -- false for one that is not an object -- found
	// natively along the chain. It exits, Go making the operation, at a
	// proxy, whose prototypes are its handler's.
	OpInstanceOf // tagged, ptr -> bool
	// OpTypeIs is typeof x === the type Index says (ir.TypeNumber...),
	// negated when Const.Bits is 1: told by x's word, and an object's
	// class and [[IsHTMLDDA]]; an object of another class, a proxy, exits,
	// Go telling it.
	OpTypeIs // tagged -> bool
	// OpCheckTrue exits unless its operand is true: strict mode's
	// assert_resolved, after a check_global_ref Go made.
	OpCheckTrue // tagged -> none
	// OpConstCell is the address of a cell the VM keeps a constant in, a
	// string's, at Const.Bits: what OpLoadCell reads it from, its pointer
	// word there (ir.StringConst).
	OpConstCell // -> source
	// OpUpvalueCell is the cell captured binding Index's value is in now:
	// its frame's slot while the frame runs, its own once closed -- found
	// again each time, as closing moves it (ir.UpvalueRead).
	OpUpvalueCell // -> source
	// OpUpvalueStale marks, while the builder runs, a captured binding's
	// variable where it may no longer be what its cell holds -- at an
	// entry, after a native call -- for a read to load it from its cell
	// there (builder.read). It is never in a block.
	OpUpvalueStale
	// OpLocalCell is the cell local Index, which an inner closure captures
	// (ir.Program's Captured), is in: its slot in the frame, where the
	// closure reads and assigns it too.
	OpLocalCell // -> source
	// OpPowF64 is Math.pow of two numbers (Intrinsic) where its answer is
	// exact, and so the VM's (ExactPow); a guard, which leaves the call
	// to Go otherwise -- a call of Go from native code that can make one
	// (GoPow), an exit from code that cannot.
	OpPowF64 // f64, f64 -> f64
	// OpMaxF64 and OpMinF64 are Math.max's and Math.min's of two numbers
	// (Intrinsic), as V8's Float64Max and Float64Min: NaN if either is,
	// and -0 below +0.
	OpMaxF64 // f64, f64 -> f64
	OpMinF64 // f64, f64 -> f64
	// OpFloorF64, OpCeilF64, OpTruncF64 and OpRoundF64 are Math.floor's,
	// Math.ceil's, Math.trunc's and Math.round's of a number (Intrinsic),
	// as V8's Float64RoundDown, Float64RoundUp, Float64RoundTruncate and
	// its rounding of the ceiling (Round); OpSignF64 is Math.sign's, and
	// OpFroundF64 Math.fround's, to the nearest float32 and back.
	OpFloorF64  // f64 -> f64
	OpCeilF64   // f64 -> f64
	OpTruncF64  // f64 -> f64
	OpRoundF64  // f64 -> f64
	OpSignF64   // f64 -> f64
	OpFroundF64 // f64 -> f64
	// OpMathCall is a Math function of numbers Go computes, called from
	// native code, as V8's code calls its ieee754 functions: the very
	// function the VM's Math has, and so its answer. Index is the call of
	// Go (GoMath, GoAtan2, GoHypot); Aux, for GoMath, which of the VM's
	// functions of one number (Intrinsic's Fn).
	OpMathCall // f64[, f64] -> f64
)

var opNames = [...]string{
	OpInvalid: "invalid", OpLoadSlot: "load", OpConst: "const", OpConstF64: "constf", OpConstSource: "consts", OpPhi: "phi",
	OpUnboxF64: "unbox", OpTruth: "truth", OpCheckInit: "checkinit", OpBoxF64: "boxf", OpBoxBool: "boxb",
	OpArrayOf: "arrayof", OpIndexedOf: "indexedof", OpElemKey: "elemkey", OpElemRead: "elemread", OpElemWrite: "elemwrite", OpElemCell: "elemcell", OpElemStore: "elemstore", OpLength: "length",
	OpObjectOf: "objectof", OpPropRead: "propread", OpPropWrite: "propwrite", OpPropCell: "propcell", OpLoadCell: "loadcell", OpGlobalCell: "globalcell", OpLexCell: "lexcell", OpBindingCell: "bindingcell",
	OpStringMethod: "stringmethod", OpStringProp: "stringprop", OpStringCode: "stringcode",
	OpAddF64: "addf", OpSubF64: "subf", OpMulF64: "mulf", OpDivF64: "divf", OpModF64: "modf", OpNegF64: "negf", OpCmpF64: "cmpf",
	OpNot: "not", OpStrictNullish: "strictnullish", OpLooseNullish: "loosenullish", OpEqTagged: "eqtagged", OpToInt32: "toi32", OpAndI32: "and", OpOrI32: "or", OpXorI32: "xor", OpShlI32: "shl",
	OpSarI32: "sar", OpShrU32: "shr", OpNotI32: "noti", OpI32ToF64: "i2f", OpU32ToF64: "u2f",
	OpBoolToF64: "b2f", OpBoolToI32: "b2i",
	OpSameObject: "sameobject", OpFrameRoom: "frameroom", OpTailRoom: "tailroom", OpTailSelf: "tailself", OpSameSymbol: "samesymbol", OpIterClosed: "iterclosed", OpKeepRef: "keepref", OpKeep: "keep", OpKept: "kept",
	OpCall: "call", OpCallCell: "callcell", OpSqrtF64: "sqrtf", OpAbsF64: "absf", OpInstanceOf: "instanceof", OpTypeIs: "typeis",
	OpCheckTrue: "checktrue", OpConstCell: "constcell", OpUpvalueCell: "upvaluecell", OpUpvalueStale: "upvaluestale", OpLocalCell: "localcell", OpPowF64: "powf",
	OpMaxF64: "maxf", OpMinF64: "minf", OpFloorF64: "floorf", OpCeilF64: "ceilf", OpTruncF64: "truncf", OpRoundF64: "roundf",
	OpSignF64: "signf", OpFroundF64: "froundf", OpMathCall: "mathcall",
}

func (o Op) String() string {
	if int(o) < len(opNames) {
		return opNames[o]
	}
	return fmt.Sprintf("op%d", o)
}

// isGuard reports whether an op exits when its operand is not what it needs.
func (o Op) isGuard() bool {
	switch o {
	case OpUnboxF64, OpTruth, OpCheckInit, OpModF64, OpArrayOf, OpIndexedOf, OpElemKey, OpElemRead, OpElemWrite, OpElemCell, OpElemStore, OpLength,
		OpObjectOf, OpPropRead, OpPropWrite, OpPropCell, OpGlobalCell, OpLexCell, OpBindingCell, OpStringMethod, OpStringProp, OpStringCode, OpLooseNullish, OpEqTagged,
		OpSameObject, OpSameSymbol, OpIterClosed, OpFrameRoom, OpTailRoom, OpTailSelf, OpCall, OpInstanceOf, OpTypeIs, OpCheckTrue, OpPowF64:
		return true
	}
	return false
}

// readsMemory reports whether a guard's result or exit depends on elements
// or properties, which writes change: two of them are not the same guard.
// (An array's length and an object's shape change only in Go.)
func (o Op) readsMemory() bool {
	return o == OpElemRead || o == OpElemWrite || o == OpElemCell || o == OpElemStore || o == OpPropRead || o == OpPropWrite || o == OpPropCell ||
		o == OpGlobalCell || o == OpLexCell || o == OpBindingCell || o == OpStringProp || o == OpKeep || o == OpCall
}

// Value is one SSA value.
type Value struct {
	ID    int
	Op    Op
	Type  Type
	Args  []*Value
	Aux   int
	Const ir.Value
	// Index is a property's index in tables of the shape a property
	// operation knows, and Key the property's atom; their Aux is their exit
	// kind, as every guard's is.
	Index int
	Key   uint32
	// IC is what a property access's or a call's cache saw beyond Const's
	// shape (InlineCache), or nil: most values are neither, and leave it
	// out of their size.
	IC *InlineCache
	// Upvalue marks an OpPropWrite to a captured binding (ir.UpvalueWrite):
	// its first operand is the binding's cell (OpUpvalueCell).
	Upvalue bool
	// Global marks an OpPropWrite to a global binding (ir.BindingWrite):
	// its first operand is the binding's cell (OpGlobalCell, OpLexCell),
	// not an object.
	Global bool
	// State is the frame to exit to, for guards.
	State *FrameState
	// Shadow is a tagged value's origin at run time, where no compiler can
	// name it: an ambiguous phi's is a Source phi; a value loaded from a cell
	// has the cell (origin.go).
	Shadow *Value
	Block  *Block
	// Uses counts the values, frame states and controls that use this one.
	Uses int
}

func (v *Value) String() string { return fmt.Sprintf("v%d", v.ID) }

// InlineCache is what a property access's or a call's inline cache saw,
// apart from Value, which most values -- arithmetic, guards, phis -- need
// none of.
type InlineCache struct {
	// Holders, for a property read the receiver's prototypes answered, are
	// those prototypes (PropertySite); nil otherwise. Cases, for a read or
	// a write that met objects of other shapes, are those (PropertyCase),
	// checked after Const's: a write's are own properties.
	Holders *HolderChain
	Cases   []PropertyCase
	// Add, for a write whose cache adds its property, is how (OpPropWrite);
	// Adds are more, from objects of other shapes, tried after it.
	Add  *PropertyAdd
	Adds []*PropertyAdd
	// Calls, for a call, are the functions it may call natively (OpCall).
	Calls []*CallSite
}

// Holders are v's cache's prototypes (InlineCache), or nil.
func (v *Value) Holders() *HolderChain {
	if v.IC == nil {
		return nil
	}
	return v.IC.Holders
}

// Cases are v's cache's other shapes (InlineCache), or nil.
func (v *Value) Cases() []PropertyCase {
	if v.IC == nil {
		return nil
	}
	return v.IC.Cases
}

// Add is how v's cache adds its property (InlineCache), or nil.
func (v *Value) Add() *PropertyAdd {
	if v.IC == nil {
		return nil
	}
	return v.IC.Add
}

// Adds are v's cache's other additions (InlineCache), or nil.
func (v *Value) Adds() []*PropertyAdd {
	if v.IC == nil {
		return nil
	}
	return v.IC.Adds
}

// Calls are the functions v, a call, may call natively (InlineCache), or
// nil.
func (v *Value) Calls() []*CallSite {
	if v.IC == nil {
		return nil
	}
	return v.IC.Calls
}

// setProperty sets what v's property cache saw (InlineCache), making v a
// cache only if any of it is there: a nil slice stays nil, an empty one
// empty.
func (f *Func) setProperty(v *Value, holders *HolderChain, cases []PropertyCase, add *PropertyAdd, adds []*PropertyAdd) {
	if holders == nil && cases == nil && add == nil && adds == nil && v.IC == nil {
		return
	}
	c := f.cache(v)
	c.Holders, c.Cases, c.Add, c.Adds = holders, cases, add, adds
}

// cache is v's inline cache, made, from the workspace if there is one,
// if it has none.
func (f *Func) cache(v *Value) *InlineCache {
	if v.IC == nil {
		if f.ws != nil {
			v.IC = &f.ws.caches.Make(1)[0]
		} else {
			v.IC = new(InlineCache)
		}
	}
	return v.IC
}

// FrameState is the interpreter's state at a bytecode PC: the PC to resume
// and the value of every live slot (locals, then Depth operands).
type FrameState struct {
	PC    uint32
	Depth int
	Slots []*Value
	// Site is the slot IR PC of the operation whose guards and exits use
	// this state, or -1 for a block's entry or loop header (abi.Context's
	// ExitSite).
	Site int
	// Inline, for a state inside an inlined callee, is the callee's frame,
	// whose slots are Slots' from Inline.Base on; the others are the
	// caller's at the call, which PC, Depth and Site are, and those between
	// are nil.
	Inline *InlineState
	// id numbers the function's states, for tables by state (newState).
	id int
}

// InlineState is an inlined callee's frame at an exit inside it, which,
// as V8's deoptimizer does, makes the frame the callee would have had: the
// exit writes the caller's state at the call and the callee's slots past
// it, in the caller's operand stack, and says in the next context where
// they are (abi.Context's Inline fields), for Go to make the VM's frame
// from, as for a native call's callee that left native code. Closure is
// the callee's closure's address (InlineSite); Base its first slot among
// the function's; Locals its program's locals, its receiver among them,
// at ThisSlot, or -1; PC, Depth and Site where in it the exit is. Parent is
// its caller's, at the call, when that is inlined too, and the exit makes
// both frames, each in a context of its own, the outermost first.
type InlineState struct {
	Parent   *InlineState
	Closure  uintptr
	Callee   uintptr
	Base     int
	Locals   int
	ThisSlot int
	PC       uint32
	Depth    int
	Site     int
}

// Kind is how a block ends.
type Kind uint8

const (
	BlockPlain  Kind = iota // goes to Succs[0]
	BlockIf                 // Control is a bool: Succs[0] if true, Succs[1] if false
	BlockReturn             // Control is the tagged result
	BlockExit               // leaves to State with ExitKind (a host exit or a guard)
)

// Block is a basic block.
type Block struct {
	ID      int
	Kind    Kind
	Values  []*Value
	Control *Value
	Succs   []*Block
	Preds   []*Block
	// State and ExitKind describe a BlockExit's exit.
	State    *FrameState
	ExitKind ir.ExitKind
	// Header is a loop header's state on entry, where a poll exits to, and
	// an entry block's state, where its guards exit to.
	Header *FrameState
	// PC is the bytecode PC the block starts at, or -1 for an entry block.
	PC int
	// LoopHeader marks a block a backward branch reaches; Backedge marks the
	// edges into it from inside the loop (by predecessor index).
	LoopHeader bool
	Backedge   []bool
	// Generic marks an entry block whose speculation failed before: the
	// slots it loads are not unboxed there (Feedback's EntryGeneric).
	Generic bool
}

// Entry is a place native code can be entered: an entry block, which loads
// the live slots, for a bytecode PC.
type Entry struct {
	PC    int
	Depth int
	Block *Block
}

// Func is a function in SSA form.
type Func struct {
	// Blocks are every block an entry reaches, in reverse post-order from
	// the entries -- a block after its dominator, a loop's body after its
	// header -- and numbered so (Build).
	Blocks  []*Block
	Entries []Entry
	// Locals and StackSize are the slot IR program's: the frame's shape.
	Locals, StackSize int
	// FrameLocals is how many of the Locals are the frame's own; the rest,
	// up to Locals, are captured bindings, which native code reads through
	// their cells (abi.Context.Upvalues) and never writes. Build sets it to
	// Locals; a VM whose program has captured bindings lowers it.
	FrameLocals int
	// ThisSlot is the slot the receiver is read from, one of those past
	// FrameLocals, which native code finds in the context
	// (abi.Context.This); -1 if none.
	ThisSlot int
	// Keeps counts the keep cells the code uses (OpKeep): before the
	// backend, each kept value's, or call's result's, a cell of its own,
	// numbered up to KeepLimit; the backend gives cells whose values are
	// dead to others (mir's keep cells), and Keeps is then how many the
	// context's (abi.MaxKeeps) the code uses.
	Keeps int
	// KeepLimit bounds Keeps before the backend: VirtualKeeps if 0.
	KeepLimit int
	// written marks the slots some instruction writes (Written).
	written []bool
	// assigned marks the captured locals a closure assigns (ir.Program's
	// Assigned): a call may write their cells, and no other.
	assigned []bool
	nextID   int
	// nextState numbers the frame states (FrameState.id).
	nextState int
	// shadowed is whether the phis' shadows and the stores' checks are
	// made (Finish).
	shadowed bool
	// rounds is how many rounds Optimize took.
	rounds int
	// origins is what Origins returns while originsKept: from the last
	// shadowMerges, kept once Optimize or Finish is done, as what follows
	// it changes no tagged value.
	origins     OriginMap
	originsKept bool
	// values and refs are slabs values, their arguments and frame states'
	// slots come from, a chunk at a time: a compile at run time makes
	// hundreds of each (BenchmarkJITCompile in internal/vm). Chunks double,
	// up to a limit, so a small function takes little.
	values     []Value
	refs       []*Value
	states     []FrameState
	valueChunk int
	refChunk   int
	// own is the passes' scratch for a Func built without a workspace,
	// made when first wanted (scr).
	own *scratch
	// ws is the workspace the Func was built in, or nil (Workspace).
	ws *Workspace
}

// scratch is the optimizer's tables and lists, which it reuses: the
// workspace's, kept from compile to compile, or the Func's own.
type scratch struct {
	flags                                         []bool
	vals, phis, ordered, front, boxes, rest, work []*Value
	// first and last are, by operand ID, Optimize's guards of it: the
	// indexes past their first and last in guards, 0 for none.
	first, last []int32
	guards      []guardSeen
	// liveStores, liveCands, liveFlat, live, liveFrom and liveWords are
	// liveAcrossSets's: what it returns lasts until it runs again.
	liveStores, liveCands, liveFlat []*Value
	live                            [][]*Value
	liveFrom                        []int
	liveWords                       []uint64
	// subst is Optimize's replacements, by value ID.
	subst []*Value
	// originPhis is Origins' list of the phis.
	originPhis []*Value
	// aliases is the store passes' answers (newAliases).
	aliases aliases
	// reaching is reconstruct's: the definition of each kept value that
	// reaches a block's start, by value and block, for those it has met.
	reaching map[int]*Value
}

// guardSeen is a guard Optimize has met, keyed by its op, and the index
// past the next one of the same operand, 0 for none.
type guardSeen struct {
	v    *Value
	op   Op
	next int32
}

// scr is the scratch the Func's passes use.
func (f *Func) scr() *scratch {
	if f.ws != nil {
		return &f.ws.scratch
	}
	if f.own == nil {
		f.own = new(scratch)
	}
	return f.own
}

// newState returns a new frame state like s, from a slab.
func (f *Func) newState(s FrameState) *FrameState {
	s.id = f.nextState
	f.nextState++
	if f.ws != nil {
		p := &f.ws.states.Make(1)[0]
		*p = s
		return p
	}
	if len(f.states) == 0 {
		f.states = make([]FrameState, 32)
	}
	p := &f.states[0]
	f.states = f.states[1:]
	*p = s
	return p
}

// alloc returns a new value like v, numbered next.
func (f *Func) alloc(v Value) *Value {
	p := f.slot()
	id := p.ID
	*p = v
	p.ID = id
	return p
}

// slot is a new value, zero but for its ID: alloc without a Value to
// copy, for newValue, which most values are made by.
func (f *Func) slot() *Value {
	var p *Value
	if f.ws != nil {
		p = &f.ws.values.Make(1)[0]
	} else {
		if len(f.values) == 0 {
			f.valueChunk = min(max(2*f.valueChunk, 16), 256)
			f.values = make([]Value, f.valueChunk)
		}
		p = &f.values[0]
		f.values = f.values[1:]
	}
	p.ID = f.nextID
	f.nextID++
	return p
}

// refsOf returns a slice of n values, nil for none. Its capacity is its
// length, so an append copies it rather than write over a neighbour's.
func (f *Func) refsOf(n int) []*Value {
	if n == 0 {
		return nil
	}
	if f.ws != nil {
		return f.ws.refs.Make(n)
	}
	if len(f.refs) < n {
		f.refChunk = min(max(2*f.refChunk, 64), 1024)
		f.refs = make([]*Value, max(f.refChunk, n))
	}
	s := f.refs[:n:n]
	f.refs = f.refs[n:]
	return s
}

func (f *Func) newBlock(pc int) *Block {
	var b *Block
	if f.ws != nil {
		b = &f.ws.blocks.Make(1)[0]
		*b = Block{ID: len(f.Blocks), PC: pc}
	} else {
		b = &Block{ID: len(f.Blocks), PC: pc}
	}
	f.Blocks = f.appendBlock(f.Blocks, b)
	return b
}

func (f *Func) newValue(b *Block, op Op, t Type, args ...*Value) *Value {
	v := f.slot()
	v.Op, v.Type, v.Args, v.Block = op, t, f.refsOf(len(args)), b
	copy(v.Args, args)
	for _, a := range args {
		a.Uses++
	}
	b.Values = f.appendValue(b.Values, v)
	return v
}

// NumValues bounds the function's value IDs, for tables indexed by them.
func (f *Func) NumValues() int { return f.nextID }

// NumStates bounds its frame states' IDs (FrameState.ID).
func (f *Func) NumStates() int { return f.nextState }

// ID numbers s among its function's frame states, for tables by state.
func (s *FrameState) ID() int { return s.id }

// Written reports whether any instruction writes a slot: a slot no
// instruction writes holds its value from entry in every frame state.
func (f *Func) Written(slot int) bool { return f.written[slot] }

// EntryFor returns the entry for a bytecode PC.
func (f *Func) EntryFor(pc int) (Entry, bool) {
	for _, e := range f.Entries {
		if e.PC == pc {
			return e, true
		}
	}
	return Entry{}, false
}

// String prints the function, for tests and debugging.
func (f *Func) String() string {
	var b strings.Builder
	for _, e := range f.Entries {
		fmt.Fprintf(&b, "entry pc %d depth %d: b%d\n", e.PC, e.Depth, e.Block.ID)
	}
	for _, blk := range f.Blocks {
		fmt.Fprintf(&b, "b%d (pc %d)", blk.ID, blk.PC)
		if len(blk.Preds) > 0 {
			b.WriteString(" <-")
			for _, p := range blk.Preds {
				fmt.Fprintf(&b, " b%d", p.ID)
			}
		}
		if blk.LoopHeader {
			b.WriteString(" loop")
		}
		if blk.Header != nil {
			fmt.Fprintf(&b, " state %v", blk.Header.Slots)
		}
		b.WriteString("\n")
		for _, v := range blk.Values {
			fmt.Fprintf(&b, "  %v = %v %v", v, v.Op, v.Type)
			for _, a := range v.Args {
				fmt.Fprintf(&b, " %v", a)
			}
			if v.Op == OpConst || v.Op == OpConstF64 {
				fmt.Fprintf(&b, " [%d:%#x]", v.Const.Kind, v.Const.Bits)
			}
			if v.Op == OpLoadSlot || v.Op == OpCmpF64 {
				fmt.Fprintf(&b, " aux=%d", v.Aux)
			}
			if v.State != nil {
				fmt.Fprintf(&b, " @pc%d", v.State.PC)
			}
			b.WriteString("\n")
		}
		switch blk.Kind {
		case BlockPlain:
			fmt.Fprintf(&b, "  goto b%d\n", blk.Succs[0].ID)
		case BlockIf:
			fmt.Fprintf(&b, "  if %v b%d b%d\n", blk.Control, blk.Succs[0].ID, blk.Succs[1].ID)
		case BlockReturn:
			fmt.Fprintf(&b, "  return %v\n", blk.Control)
		case BlockExit:
			fmt.Fprintf(&b, "  exit %d @pc%d\n", blk.ExitKind, blk.State.PC)
		}
	}
	return b.String()
}

// VirtualKeeps bounds the keep cells a build numbers (Func.Keeps), which the
// backend fits into the context's abi.MaxKeeps by when each is live: a
// function of hundreds of calls -- RegExp's runBlock11 -- had its calls past
// the 256th made by Go at an exit each, though nearly every result was
// dead at once.
const VirtualKeeps = 1 << 16

// keepLimit is how many keep cells a build may number (KeepLimit).
func (f *Func) keepLimit() int {
	if f.KeepLimit > 0 {
		return f.KeepLimit
	}
	return VirtualKeeps
}
