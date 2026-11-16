package bytecode

import (
	"fmt"
	"strings"
)

// The register form of a function's code: what the register interpreter
// runs (docs/register-interpreter.md).
//
// A frame is a window of registers in the runtime's value stack. The first
// LocalCount are the function's locals, in the slots the stack form gives
// them, so that an upvalue, a stack trace and the debugger find a local
// where they always did; the temporaries the stack form keeps on its operand
// stack follow, one register for each depth that stack reaches, and then the
// few registers the register form adds of its own.
//
// An instruction is one 64-bit word, read as a struct of an operation and
// four operands:
//
//	bits  0- 7  Op  the operation
//	bits  8-15  X   a small operand: an argument count, a comparison, a flag
//	bits 16-31  A   a register, most often the destination
//	bits 32-47  B   a register, an index or a small immediate
//	bits 48-63  C   a register, an index or a small immediate
//
// B and C read together are BC, a 32-bit index or a signed jump offset. A
// register is 16 bits, so a function has up to 65,536 of them without any
// form of its own for more; a function that needs more registers runs in the
// stack form until the far moves of the design's later stage exist.

// RInstr is one register instruction.
type RInstr uint64

// ROp is a register operation.
type ROp uint8

// MakeR builds an instruction from its fields.
func MakeR(op ROp, x uint8, a, b, c uint16) RInstr {
	return RInstr(uint64(op) | uint64(x)<<8 | uint64(a)<<16 | uint64(b)<<32 | uint64(c)<<48)
}

// MakeRBC builds an instruction whose B and C are one 32-bit operand.
func MakeRBC(op ROp, x uint8, a uint16, bc uint32) RInstr {
	return RInstr(uint64(op) | uint64(x)<<8 | uint64(a)<<16 | uint64(bc)<<32)
}

// Op is the instruction's operation.
func (w RInstr) Op() ROp { return ROp(w) }

// X is the instruction's 8-bit operand.
func (w RInstr) X() uint8 { return uint8(w >> 8) }

// A is the instruction's first 16-bit operand.
func (w RInstr) A() uint16 { return uint16(w >> 16) }

// B is the instruction's second 16-bit operand.
func (w RInstr) B() uint16 { return uint16(w >> 32) }

// C is the instruction's third 16-bit operand.
func (w RInstr) C() uint16 { return uint16(w >> 48) }

// BC is B and C read as one 32-bit operand.
func (w RInstr) BC() uint32 { return uint32(w >> 32) }

// SC is C read as a signed 16-bit operand: an immediate or a jump offset.
func (w RInstr) SC() int32 { return int32(int16(w >> 48)) }

// SB is B read as a signed 16-bit immediate.
func (w RInstr) SB() int32 { return int32(int16(w >> 32)) }

// WithSC is w with C replaced by the signed offset off, which the caller
// has checked fits in 16 bits.
func (w RInstr) WithSC(off int32) RInstr {
	return w&^(0xffff<<48) | RInstr(uint64(uint16(int16(off)))<<48)
}

// WithBC is w with BC replaced by bc.
func (w RInstr) WithBC(bc uint32) RInstr {
	return w&^(0xffffffff<<32) | RInstr(uint64(bc)<<32)
}

// WithA is w with A replaced by a.
func (w RInstr) WithA(a uint16) RInstr {
	return w&^(0xffff<<16) | RInstr(uint64(a)<<16)
}

// The register operations. R[n] is register n of the frame; an offset is
// relative to the instruction after the jump. Each operation that reads a
// property, a global or a name keeps the stack form's cache site, so that
// the two forms of a function share its caches.
const (
	RNop ROp = iota
	// RMove: R[A] = R[B].
	RMove
	// RLoadInt: R[A] = int32(BC).
	RLoadInt
	// RLoadConst: R[A] = Constants[BC].
	RLoadConst
	// RLoadUndef, RLoadNull, RLoadTrue, RLoadFalse, RLoadEmpty and
	// RLoadUninit load what they name into R[A]: RLoadEmpty the empty
	// string, RLoadUninit the dead zone's marker.
	RLoadUndef
	RLoadNull
	RLoadTrue
	RLoadFalse
	RLoadEmpty
	RLoadUninit
	// RLoadThis: R[A] = this, which a frame the register form runs has
	// bound from its start.
	RLoadThis
	// RCheckRef throws the ReferenceError of a binding in its dead zone if
	// R[A] is the dead zone's marker; the message names Locals[B].
	RCheckRef
	// RGetUpval: R[A] = upvalue BC. RSetUpval: upvalue BC = R[A].
	RGetUpval
	RSetUpval
	// RGetGlobal: R[A] = the global Names[B], site C.
	RGetGlobal
	// RSetGlobal: the global Names[B], site C, = R[A], as set_global.
	RSetGlobal
	// RGetProp: R[A] = R[B].name, site C, as get_prop; the site's name is
	// RegCode.SiteName[C].
	RGetProp
	// RGetMethod: R[A] = R[B].name, site C, as get_prop_this, the read of a
	// method a call is about to make.
	RGetMethod
	// RSetProp: R[A].name = R[B], site C, as set_prop.
	RSetProp
	// RGetIndex: R[A] = R[B][R[C]].
	RGetIndex
	// RSetIndex: R[A][R[B]] = R[C].
	RSetIndex
	// RDefineField: defines R[A].Names[C] = R[B], as define_field.
	RDefineField
	// RNewObject: R[A] = a new object literal of B properties, site C.
	RNewObject
	// RNewArray: R[A] = an array of the C values from R[B].
	RNewArray
	// RIterNext: R[A] = the next value of cursor R[B], or a jump by SC
	// where it has none: a for-of or for-in loop's step, at the head of
	// each turn (value 24, out of the free list).
	RIterNext
	// RAdd, RSub, RMul: R[A] = R[B] op R[C].
	RAdd
	RSub
	RMul
	// RBin: R[A] = R[B] op R[C], where X is the stack form's Op of the
	// operator: a division, a remainder, a power, a bitwise operator or a
	// shift, an equality or a relational comparison.
	RBin
	// RLit is a RegExp literal's exec or test, a per-turn step of the
	// loops that match (value 29, out of the free list): R[A+1] = the
	// literal of Constants[C], made or given back, and R[A] its method,
	// exec or test (RLitTest), not read where it is the realm's built-in,
	// which reading runs nothing to find, and read otherwise -- at the
	// instruction's position, or with RLitCall at the stack form's
	// instruction Aux. With RLitCall the method is then called with the
	// window's one argument, R[A+2], which nothing can change by the read,
	// R[B] = its value, the literal given back as RMarkLitRecv's call
	// gives it; RLitDead says nothing reads the value but its truth.
	// Without RLitCall a call_method_w marked RMarkLitRecv follows, after
	// the arguments.
	RLit
	// RInc and RDec: R[A] = ToNumeric(R[B]) + 1 or - 1.
	RInc
	RDec
	// RUnary: R[A] = op R[B], where X is the stack form's Op: neg, pos,
	// not, bit_not or typeof.
	RUnary
	// RJmp: jump by int32(BC).
	RJmp
	// RJmpF and RJmpT: jump by int32(BC) if R[A] is falsy or truthy.
	RJmpF
	RJmpT
	// RCallWT and RCallMWT are RCallW and RCallMW in tail position, as the
	// stack form makes tail_call (values 36 and 37, out of the free list):
	// once the stack is deep, the frame is given to the callee in place or
	// given up. Ops of their own, so that the ordinary call checks nothing
	// of it.
	RCallWT
	RCallMWT
	// RRet returns R[A]; RRetUndef returns undefined.
	RRet
	RRetUndef
	_ // free: 40, the translation reference's call, removed
	_ // free: 41, the translation reference's call_method, removed
	_ // free: 42, the translation reference's new, removed
	_ // free: 43, the translation reference's close_upvals, removed

	// The bitwise operators and shifts, each an operation of its own so
	// that no instruction decides at run time which operator it is: R[A] =
	// R[B] op R[C], or R[B] op SC for the immediate forms.
	RBitAnd
	RBitOr
	RBitXor
	RShl
	RShr
	RUShr
	RAndI
	ROrI
	RXorI
	RShlI
	RShrI
	RUShrI
	// RCallW: R[B] = R[A](R[A+2] ... R[A+1+X]) with this R[A+1], the callee's
	// frame starting at R[A]: its register 0 is the function, 1 its this,
	// and its parameters are the arguments where they lie (the register
	// form's own frame layout; docs/register-interpreter.md).
	RCallW
	// RAddI and RSubI: R[A] = R[B] + SC or - SC.
	RAddI
	RSubI
	// The compare-and-branch operations, one for each comparison and each
	// way it can decide: RJLt jumps by SC if R[A] < R[B], RJNLt if not --
	// which a NaN makes different things. The I forms compare with the
	// immediate SB. RJSEq and RJSNe are === and !==.
	RJLt
	RJNLt
	RJLe
	RJNLe
	RJGt
	RJNGt
	RJGe
	RJNGe
	RJLtI
	RJNLtI
	RJLeI
	RJNLeI
	RJGtI
	RJNGtI
	RJGeI
	RJNGeI
	RJSEq
	RJSNe
	RJSEqI
	RJSNeI
	// RGetIndexI: R[A] = R[B][C], and RSetIndexI: R[A][B] = R[C], with the
	// index the immediate C or B -- tab[3] = j with no load of the 3.
	RGetIndexI
	RSetIndexI
	// RCallMW is RCallW with this R[A+1]: a method call, its window
	// R[A] the method and R[A+1] the receiver get_method wrote.
	RCallMW
	// RNewW: R[B] = new R[A](R[A+2] ... R[A+1+X]).
	RNewW
	// RClosureW: R[A] = a closure of Constants[BC], each upvalue it takes
	// from its maker the register RegCode.Caps[BC] names.
	RClosureW
	// RPostInc and RPostDec: R[A] = ToNumeric(R[B]), then R[B] = R[A] + 1
	// or - 1 -- a postfix update whose value is used, as in a[i++].
	RPostInc
	RPostDec
	// RGetLength: R[A] = R[B].length, an array's and a string's answered
	// in the case.
	RGetLength
	// RJEq, RJNe and their immediate forms are == and != that branch;
	// RJNull and RJNotNull jump by SC if R[A] == null, or != null.
	RJEq
	RJNe
	RJEqI
	RJNeI
	RJNull
	RJNotNull
	// RThrow throws R[A].
	RThrow
	// RInstanceOf: R[A] = R[B] instanceof R[C], its cache site Aux[pc].
	RInstanceOf
	// RIn: R[A] = R[B] in R[C].
	RIn
	// RDelete: R[A] = delete R[B][R[C]].
	RDelete
	// RArguments: R[A] = the frame's arguments object, made anew.
	RArguments
	// RNewArrayK: R[A] = an array literal of the B constants of
	// Literals[Aux[pc]], whose arrays share them until one is written, its
	// cache site C keeping them (the VM's shared literals).
	RNewArrayK
	// RAddProp: R[A].name += R[B], and RAddPropI: R[A].name += SB, site C
	// -- an update of a place as one operation, its value unused: o.a += j,
	// o.n++. The site is the store's, whose entry for an own writable data
	// property says where the value to add to is too.
	RAddProp
	RAddPropI
	// RApplyArgs: R[A] = R[B+1].call(R[B], R[B+2], ...the frame's
	// arguments): f.apply(x, arguments) of a function whose arguments object
	// is never made (Function.ForwardsArguments), the arguments as passed
	// handed on without it; R[B] is f and R[B+1] what f.apply read. R[C]
	// is the arguments binding, where an apply other than the built-in is
	// given the object, made once.
	RApplyArgs
	// RGetIndexPre: R[C] += SX, then R[A] = R[B][R[C]] -- x[++i] and
	// x[--i] of a local i, its step X a signed 8-bit immediate.
	RGetIndexPre
	// RJLtU, RJNLtU, RJLeU, RJNLeU: the compare-and-branches of R[A] with
	// upvalue B, by SC -- a loop's bound that a closure captures.
	RJLtU
	RJNLtU
	RJLeU
	RJNLeU
	// RGetMethodW: R[A] = R[B].name, site C, as get_prop_this, and
	// R[A+1] = R[B]: a method's read that puts its receiver in the call's
	// window, so that a receiver in a binding needs no move.
	RGetMethodW
	// RArgsLen: R[A] = arguments.length, and RArgsIndex: R[A] =
	// arguments[R[C]], in a function whose arguments object R[B] is made
	// only on demand: until it is, the frame's arguments are read where
	// they lie (arguments_length, arguments_index).
	RArgsLen
	RArgsIndex
	// RConcat: R[A] = the string of R[B] ... R[B+C-1] joined, each a
	// string or a number, or converted by ToString: a chain of + after a
	// string literal (the stack form's concat), one string made rather than
	// one an addition.
	RConcat
	// RStrPart: R[A] = "" + R[A] where R[A] is neither a string nor a
	// number, a part of a chain converted where its addition converted it.
	RStrPart
	// RKeyOfBase: R[A] = ToPropertyKey(R[C]), after a TypeError if R[B],
	// the object it is for, is null or undefined: a computed member's key
	// both read and written, converted once (to_property_key_of_base). A
	// string, a symbol or a number is the key as it is.
	RKeyOfBase
	// RGetIndexPost: R[A] = R[B][R[C]], then R[C] += SX -- x[i++] and
	// x[i--] of a local i, the read's key the step's old value.
	RGetIndexPost
	// RNewArrayE: R[A] = [], an empty literal made with the room its arrays
	// came to need: B is its site, which learns it (the tree's
	// arrayEmptyNode's).
	RNewArrayE
	// RAddIndex: R[A][R[B]] += R[C]; RAddIndexK: R[A][C] += R[B], a key
	// that is an index; RAddIndexI: R[A][R[B]] += SC. Each is a statement's
	// element update, its value unused, read where the value's evaluation
	// cannot change what the read sees, as RAddProp's. X, in K and I, says
	// an update, ++ or --, which converts by ToNumeric where += adds as +
	// does. The key is converted once.
	RAddIndex
	RAddIndexK
	RAddIndexI
	// RSwitch: a switch's leading cases whose selectors are literals, looked
	// up as one: R[A] goes to the body of the first case of Switches[BC]
	// it strictly equals, and to the next instruction where it equals
	// none -- the chain of the switch's other cases, or its default.
	RSwitch
	// RJTypeof: jump by SC if typeof R[A] is the type of index X&^RCmpTrue
	// in the VM's typeofNames -- undefined, object, boolean, number,
	// string, symbol, bigint, function -- and X's RCmpTrue bit says it is,
	// or neither: typeof x === "name" and its kind as one compare.
	RJTypeof
	// RTypeof: R[A] = typeof R[B].
	RTypeof
	// RSwitchTypeof: switch (typeof R[A]) of string cases alone: the body
	// of the first case of Switches[BC] that is the type's name, or the
	// next instruction.
	RSwitchTypeof
	// RNewObjectLit: R[A] = an object literal of the X properties named
	// ObjKeys[Aux[pc]], their values R[B] on, site C: the literal made at
	// once, its values evaluated before it, which nothing can tell apart.
	RNewObjectLit
	// RAppend: R[A] = R[A] + R[B], an addition to a local the stack
	// compiler made an append (bytecode.AddOwned): the string the local
	// alone holds grown in place where it can be (the VM's strAppend),
	// anything else added as RAdd adds it. Its value is the local's.
	RAppend
	// RStrFreeze: R[A], a read of a binding an append grows, is no longer
	// the binding's alone (the stack form's str_freeze, StrFreezeA).
	RStrFreeze
	// RCharCodeAt: R[A] = R[B].charCodeAt(R[X]), the method read at site
	// C and called with the argument in register X, a local no one else
	// writes: a string's code unit read where the method is the built-in
	// and the argument a number, anything else the read and the call
	// get_method_w and call_method_w make. It is placed at the call; Aux
	// is the stack instruction of the read, where the read's errors are
	// placed.
	RCharCodeAt
	// RCold is an operation that does not run per turn of a hot loop, its
	// sub-operation (RColdOp) in X: one opcode value for all of them,
	// dispatched a second time out of the loop.
	RCold

	// RInvalid is the last operation there can be, which no code holds:
	// the interpreter's switch has a case for it so that its jump table
	// spans every value of an ROp and needs no range check.
	RInvalid ROp = 255
)

// RCmpTrue is the bit of a compare-and-branch's X (jtypeof's, a fused
// set_upval's) that says it jumps when the comparison holds, rather than
// when it does not.
const RCmpTrue = 0x80

// RColdOp is cold's sub-operation, in its X.
type RColdOp uint8

const (
	// RColdNewRegExp: R[A] = the RegExp literal of Constants[BC].
	RColdNewRegExp RColdOp = iota
	// RColdForOfStart and RColdForInStart: R[A] = a cursor over R[B], a
	// for-of loop's or a for-in loop's (startForOf, startForIn).
	RColdForOfStart
	RColdForInStart
	// RColdIterClose closes cursor R[B] as a break, a continue or a
	// return that leaves its loop does: an error of its return method's is
	// thrown. RColdIterCloseThrow closes it as a throw that leaves the loop
	// does: an error of its return method's is dropped.
	RColdIterClose
	RColdIterCloseThrow
)

// The flags of regexp_lit's X.
const (
	RLitTest = 1 << iota
	RLitCall
	RLitDead
)

// The marks of a call (RegCode.Marks), by its pc: what the call of a
// built-in at a call_method_w does around it, which changes nothing of
// the instruction's dispatch -- the site's call of a built-in reads them --
// and what lit_exec_call and lit_test_call do. Their operand, a constant
// or a pc, is the instruction's Aux, all 32 bits of it.
const (
	// RMarkDead says nothing reads the call's value but its truth: a call
	// of exec or replace is told so, as the tree's deadExec tells it, to
	// make no array or string.
	RMarkDead = 1 << iota
	// RMarkReuse marks a call of exec whose value goes to the local of
	// register B, its Aux the stack form's call (execReuseOf): the array
	// of a match may be made again in the memory of the last.
	RMarkReuse
	// RMarkLitRecv: the receiver is the RegExp lit_exec or lit_test made
	// of the literal of constant Aux, given back to the literal where the
	// call was the realm's exec or test given a primitive, which keeps
	// nothing of it.
	RMarkLitRecv
	// RMarkLitArg: the call makes its first argument, R[A+2], the literal
	// of constant Aux, itself -- nothing can see it made after the
	// arguments that follow -- and gives it back where the call was
	// String.prototype's replace, split or match searching with it
	// directly (regexpArgCall).
	RMarkLitArg
)

var rColdNames = [...]string{
	RColdNewRegExp: "new_regexp", RColdForOfStart: "for_of_start", RColdForInStart: "for_in_start",
	RColdIterClose: "iter_close", RColdIterCloseThrow: "iter_close_throw",
}

func (op RColdOp) String() string {
	if int(op) < len(rColdNames) && rColdNames[op] != "" {
		return rColdNames[op]
	}
	return fmt.Sprintf("cold(%d)", uint8(op))
}

// LitElem is an element of a literal of constants: Constants[K], or, by
// Kind, true, false, null or undefined.
type LitElem struct {
	Kind uint8
	K    uint32
}

// The kinds of a LitElem.
const (
	LitConst = iota
	LitTrue
	LitFalse
	LitNull
	LitUndefined
)

// SwitchCase is a case of an RSwitch's table: its selector, a literal, by
// Kind -- a number Num, a string Str, Constants[K], true, false, null --
// and the pc of its body.
type SwitchCase struct {
	Kind uint8
	Num  float64
	Str  string
	K    uint32
	To   uint32
}

// The kinds of a SwitchCase.
const (
	SwitchNum = iota
	SwitchStr
	SwitchTrue
	SwitchFalse
	SwitchNull
)

// RegCode is a function's code in the register form.
type RegCode struct {
	Code []RInstr
	// NumRegs is how many registers a frame of the code uses: the
	// function's locals, its temporaries, and the register form's own.
	NumRegs int
	// SrcPC is, for each instruction, the stack form's instruction it was
	// made from: what a frame's pc holds while the instruction runs, so
	// that positions, stack traces and the debugger read the stack form's
	// tables as they always did.
	SrcPC []uint32
	// SiteName is the name of each property site an instruction names,
	// indexed by its cache site.
	SiteName []uint32
	// This is the register the function's this is loaded into as it
	// starts, or -1 if the code reads no this.
	This int
	// Wide marks code of the register form's own frame layout, which the
	// register backend compiles from the AST: register 0 the function, 1
	// its this, the Params parameters from 2, and then Inits bindings, which
	// a call makes undefined, before the temporaries.
	Wide          bool
	Params, Inits int
	// Caps is, for each constant that is a nested function, the register
	// of each of its upvalues taken from this function's bindings.
	Caps [][]uint16
	// CallSites is how many call_w and call_method_w the code has, each
	// numbered by its C: its cache of the function it called last.
	CallSites int
	// Aux is, for an instruction that needs one more operand than its word
	// holds, that operand, by its pc: instanceof's cache site. Nil where no
	// instruction needs one; for add_index, the stack instruction of its
	// store, where the store's errors are placed.
	Aux []uint32
	// Marks is each call's marks (RMarkDead, ...), by its pc; nil where no
	// call has any.
	Marks []uint8
	// Switches is each RSwitch's table, in its cases' order.
	Switches [][]SwitchCase
	// Literals are the elements of the code's literals of constants
	// (RNewArrayK), each a constant or one of the values no constant is.
	Literals [][]LitElem
	// ObjKeys are the keys of the code's object literals made at once
	// (RNewObjectLit), each an index of the function's names.
	ObjKeys [][]uint16
	// LocalReg is each local's register, or -1, where the code has a
	// call_reuse, whose frame's locals the runtime reads by it; nil
	// otherwise.
	LocalReg []int32
	// UsesArguments says the code makes the arguments object, whose frame
	// keeps the arguments as passed (RArguments).
	UsesArguments bool
	// ForwardsArguments says a frame keeps its arguments as passed for
	// RApplyArgs, with no arguments object made.
	ForwardsArguments bool
	// Handlers are the code's exception handlers, innermost first.
	Handlers []RHandler
}

// RHandler is an exception handler of register code: an exception the
// instruction at a pc in one of its ranges throws goes on at To, its value
// in register Reg (docs/register-interpreter.md, "Exceptions"). A handler
// is a catch clause, or a finally clause's copy that runs for a throw and
// throws the value again at its end; the copies a normal completion, a
// break, a continue or a return run are compiled in their place, outside
// the ranges.
type RHandler struct {
	// Ranges are the [start, end) pairs of pcs the handler covers.
	Ranges []uint32
	To     uint32
	Reg    uint16
}

// HandlerAt is the handler of the instruction at pc, the innermost whose
// ranges hold it, or nil.
func (rc *RegCode) HandlerAt(pc int) *RHandler {
	for i := range rc.Handlers {
		h := &rc.Handlers[i]
		for j := 0; j+1 < len(h.Ranges); j += 2 {
			if uint32(pc) >= h.Ranges[j] && uint32(pc) < h.Ranges[j+1] {
				return h
			}
		}
	}
	return nil
}

var rOpNames = [...]string{
	RNop: "nop", RMove: "move", RLoadInt: "load_int", RLoadConst: "load_const",
	RLoadUndef: "load_undef", RLoadNull: "load_null", RLoadTrue: "load_true",
	RLoadFalse: "load_false", RLoadEmpty: "load_empty", RLoadUninit: "load_uninit",
	RLoadThis: "load_this", RCheckRef: "check_ref",
	RGetUpval: "get_upval", RSetUpval: "set_upval",
	RGetGlobal: "get_global", RSetGlobal: "set_global",
	RGetProp: "get_prop", RGetMethod: "get_method", RGetMethodW: "get_method_w", RArgsLen: "args_len", RArgsIndex: "args_index", RConcat: "concat", RStrPart: "str_part", RKeyOfBase: "key_of_base", RSetProp: "set_prop",
	RGetIndex: "get_index", RSetIndex: "set_index", RDefineField: "define_field",
	RNewObject: "new_object", RNewArray: "new_array",
	RAdd: "add", RSub: "sub", RMul: "mul", RBin: "bin",
	RInc: "inc", RDec: "dec", RUnary: "unary",
	RJmp: "jmp", RJmpF: "jmp_false", RJmpT: "jmp_true", RCallWT: "tail_call_w", RCallMWT: "tail_call_method_w", RRet: "ret", RRetUndef: "ret_undef",
	RBitAnd: "bit_and", RBitOr: "bit_or", RBitXor: "bit_xor", RShl: "shl", RShr: "shr", RUShr: "ushr",
	RAndI: "and_imm", ROrI: "or_imm", RXorI: "xor_imm", RShlI: "shl_imm", RShrI: "shr_imm", RUShrI: "ushr_imm",
	RCallW: "call_w", RAddI: "add_imm", RSubI: "sub_imm",
	RJLt: "jlt", RJNLt: "jnlt", RJLe: "jle", RJNLe: "jnle", RJGt: "jgt", RJNGt: "jngt", RJGe: "jge", RJNGe: "jnge",
	RJLtI: "jlt_imm", RJNLtI: "jnlt_imm", RJLeI: "jle_imm", RJNLeI: "jnle_imm", RJGtI: "jgt_imm", RJNGtI: "jngt_imm",
	RJGeI: "jge_imm", RJNGeI: "jnge_imm", RJSEq: "jseq", RJSNe: "jsne", RJSEqI: "jseq_imm", RJSNeI: "jsne_imm",
	RGetIndexI: "get_index_imm", RSetIndexI: "set_index_imm", RCallMW: "call_method_w", RNewW: "new_w", RClosureW: "closure_w",
	RPostInc: "post_inc", RPostDec: "post_dec", RGetLength: "get_length",
	RJEq: "jeq", RJNe: "jne", RJEqI: "jeq_imm", RJNeI: "jne_imm", RJNull: "jnull", RJNotNull: "jnotnull",
	RThrow: "throw", RInstanceOf: "instanceof", RIn: "in", RDelete: "delete", RArguments: "arguments", RNewArrayK: "new_array_k", RAddProp: "add_prop", RAddPropI: "add_prop_imm", RApplyArgs: "apply_args",
	RGetIndexPre: "get_index_pre", RGetIndexPost: "get_index_post", RNewArrayE: "new_array_empty", RAddIndex: "add_index", RAddIndexK: "add_index_k", RAddIndexI: "add_index_imm", RSwitch: "switch", RJTypeof: "jtypeof", RTypeof: "typeof", RSwitchTypeof: "switch_typeof", RNewObjectLit: "new_object_lit", RAppend: "append", RStrFreeze: "str_freeze", RCharCodeAt: "char_code_at", RIterNext: "iter_next", RLit: "regexp_lit", RCold: "cold", RJLtU: "jlt_upval", RJNLtU: "jnlt_upval", RJLeU: "jle_upval", RJNLeU: "jnle_upval",
}

// MethodCall reports whether op is call_method_w or its tail form, whose
// this is R[A+1].
func (op ROp) MethodCall() bool { return op == RCallMW || op == RCallMWT }

func (op ROp) String() string {
	if int(op) < len(rOpNames) && rOpNames[op] != "" {
		return rOpNames[op]
	}
	return fmt.Sprintf("rop(%d)", uint8(op))
}

// Format renders the instruction at pc, for tests and disassembly.
func (rc *RegCode) Format(pc int) string {
	w := rc.Code[pc]
	r := func(n uint16) string { return fmt.Sprintf("r%d", n) }
	target := func(off int32) string { return fmt.Sprintf("-> %d", pc+1+int(off)) }
	switch op := w.Op(); op {
	case RMove, RInc, RDec, RPostInc, RPostDec, RGetLength:
		return fmt.Sprintf("%s %s, %s", op, r(w.A()), r(w.B()))
	case RLoadInt:
		return fmt.Sprintf("%s %s, %d", op, r(w.A()), int32(w.BC()))
	case RLoadConst:
		return fmt.Sprintf("%s %s, k%d", op, r(w.A()), w.BC())
	case RLoadUndef, RLoadNull, RLoadTrue, RLoadFalse, RLoadEmpty, RLoadUninit, RLoadThis, RRet:
		return fmt.Sprintf("%s %s", op, r(w.A()))
	case RCheckRef:
		return fmt.Sprintf("%s %s, local %d", op, r(w.A()), w.B())
	case RGetUpval:
		return fmt.Sprintf("%s %s, u%d", op, r(w.A()), w.BC())
	case RSetUpval:
		return fmt.Sprintf("%s u%d, %s", op, w.BC(), r(w.A()))
	case RGetGlobal:
		return fmt.Sprintf("%s %s, name%d @%d", op, r(w.A()), w.B(), w.C())
	case RSetGlobal:
		return fmt.Sprintf("%s name%d @%d, %s", op, w.B(), w.C(), r(w.A()))
	case RArgsLen:
		return fmt.Sprintf("%s %s, %s", op, r(w.A()), r(w.B()))
	case RConcat:
		return fmt.Sprintf("%s %s = %s..%s", op, r(w.A()), r(w.B()), r(w.B()+w.C()-1))
	case RStrPart:
		return fmt.Sprintf("%s %s", op, r(w.A()))
	case RKeyOfBase:
		return fmt.Sprintf("%s %s = key of %s for %s", op, r(w.A()), r(w.C()), r(w.B()))
	case RArgsIndex:
		return fmt.Sprintf("%s %s, %s[%s]", op, r(w.A()), r(w.B()), r(w.C()))
	case RGetProp, RGetMethod, RGetMethodW:
		return fmt.Sprintf("%s %s, %s @%d", op, r(w.A()), r(w.B()), w.C())
	case RSetProp:
		return fmt.Sprintf("%s %s @%d, %s", op, r(w.A()), w.C(), r(w.B()))
	case RDefineField:
		return fmt.Sprintf("%s %s name%d, %s", op, r(w.A()), w.C(), r(w.B()))
	case RInstanceOf, RIn, RDelete:
		return fmt.Sprintf("%s %s, %s, %s", op, r(w.A()), r(w.B()), r(w.C()))
	case RApplyArgs:
		return fmt.Sprintf("%s %s = %s.apply(%s, %s, arguments %s)", op, r(w.A()), r(w.B()+1), r(w.B()), r(w.B()+2), r(w.C()))
	case RThrow, RArguments, RStrFreeze:
		return fmt.Sprintf("%s %s", op, r(w.A()))
	case RAppend:
		return fmt.Sprintf("%s %s, %s", op, r(w.A()), r(w.B()))
	case RIterNext:
		return fmt.Sprintf("%s %s, %s, done -> %d", op, r(w.A()), r(w.B()), pc+1+int(w.SC()))
	case RCharCodeAt:
		return fmt.Sprintf("%s %s = %s.charCodeAt(%s) @%d", op, r(w.A()), r(w.B()), r(uint16(w.X())), w.C())
	case RGetIndex, RAdd, RSub, RMul, RBitAnd, RBitOr, RBitXor, RShl, RShr, RUShr:
		return fmt.Sprintf("%s %s, %s, %s", op, r(w.A()), r(w.B()), r(w.C()))
	case RSetIndex:
		return fmt.Sprintf("%s %s[%s], %s", op, r(w.A()), r(w.B()), r(w.C()))
	case RNewObject:
		return fmt.Sprintf("%s %s, %d @%d", op, r(w.A()), w.B(), w.C())
	case RNewObjectLit:
		return fmt.Sprintf("%s %s, %s x%d @%d", op, r(w.A()), r(w.B()), w.X(), w.C())
	case RAddProp:
		return fmt.Sprintf("%s %s @%d, %s", op, r(w.A()), w.C(), r(w.B()))
	case RAddPropI:
		return fmt.Sprintf("%s %s @%d, %d", op, r(w.A()), w.C(), w.SB())
	case RNewArrayK:
		return fmt.Sprintf("%s %s, x%d @%d", op, r(w.A()), w.B(), w.C())
	case RAddIndex:
		return fmt.Sprintf("%s %s[%s], %s", op, r(w.A()), r(w.B()), r(w.C()))
	case RAddIndexK:
		return fmt.Sprintf("%s.%d %s[%d], %s", op, w.X(), r(w.A()), w.C(), r(w.B()))
	case RAddIndexI:
		return fmt.Sprintf("%s.%d %s[%s], %d", op, w.X(), r(w.A()), r(w.B()), w.SC())
	case RTypeof:
		return fmt.Sprintf("%s %s, %s", op, r(w.A()), r(w.B()))
	case RJTypeof:
		return fmt.Sprintf("%s %s, %d %v -> %+d", op, r(w.A()), w.X()&^RCmpTrue, w.X()&RCmpTrue != 0, w.SC())
	case RSwitch, RSwitchTypeof:
		return fmt.Sprintf("%s %s, table%d", op, r(w.A()), w.BC())
	case RNewArrayE:
		return fmt.Sprintf("%s %s @%d", op, r(w.A()), w.B())
	case RNewArray:
		return fmt.Sprintf("%s %s, %s x%d", op, r(w.A()), r(w.B()), w.C())
	case RBin:
		return fmt.Sprintf("%s.%s %s, %s, %s", op, Op(w.X()), r(w.A()), r(w.B()), r(w.C()))
	case RAndI, ROrI, RXorI, RShlI, RShrI, RUShrI, RAddI, RSubI:
		return fmt.Sprintf("%s %s, %s, %d", op, r(w.A()), r(w.B()), w.SC())
	case RUnary:
		return fmt.Sprintf("%s.%s %s, %s", op, Op(w.X()), r(w.A()), r(w.B()))
	case RJmp:
		return fmt.Sprintf("%s %s", op, target(int32(w.BC())))
	case RJmpF, RJmpT:
		return fmt.Sprintf("%s %s %s", op, r(w.A()), target(int32(w.BC())))
	case RJNull, RJNotNull:
		return fmt.Sprintf("%s %s %s", op, r(w.A()), target(w.SC()))
	case RJLt, RJNLt, RJLe, RJNLe, RJGt, RJNGt, RJGe, RJNGe, RJSEq, RJSNe, RJEq, RJNe:
		return fmt.Sprintf("%s %s, %s %s", op, r(w.A()), r(w.B()), target(w.SC()))
	case RJLtI, RJNLtI, RJLeI, RJNLeI, RJGtI, RJNGtI, RJGeI, RJNGeI, RJSEqI, RJSNeI, RJEqI, RJNeI:
		return fmt.Sprintf("%s %s, %d %s", op, r(w.A()), w.SB(), target(w.SC()))
	case RGetIndexI:
		return fmt.Sprintf("%s %s, %s[%d]", op, r(w.A()), r(w.B()), w.C())
	case RGetIndexPre:
		return fmt.Sprintf("%s %s, %s[%s += %d]", op, r(w.A()), r(w.B()), r(w.C()), int8(w.X()))
	case RGetIndexPost:
		return fmt.Sprintf("%s %s, %s[%s], %s += %d", op, r(w.A()), r(w.B()), r(w.C()), r(w.C()), int8(w.X()))
	case RJLtU, RJNLtU, RJLeU, RJNLeU:
		return fmt.Sprintf("%s %s, u%d %s", op, r(w.A()), w.B(), target(w.SC()))
	case RSetIndexI:
		return fmt.Sprintf("%s %s[%d], %s", op, r(w.A()), w.B(), r(w.C()))
	case RClosureW:
		return fmt.Sprintf("%s %s, k%d", op, r(w.A()), w.BC())
	case RCallW, RCallMW, RCallWT, RCallMWT, RNewW:
		return fmt.Sprintf("%s %s = %s(%d)", op, r(w.B()), r(w.A()), w.X())
	case RLit:
		m := "exec"
		if w.X()&RLitTest != 0 {
			m = "test"
		}
		if w.X()&RLitCall != 0 {
			return fmt.Sprintf("%s %s = k%d.%s(%s)", op, r(w.B()), w.C(), m, r(w.A()+2))
		}
		return fmt.Sprintf("%s %s = k%d.%s", op, r(w.A()), w.C(), m)
	case RCold:
		switch sub := RColdOp(w.X()); sub {
		case RColdNewRegExp:
			return fmt.Sprintf("%s.%s %s, k%d", op, sub, r(w.A()), w.BC())
		case RColdIterClose, RColdIterCloseThrow:
			return fmt.Sprintf("%s.%s %s", op, sub, r(w.B()))
		}
		return fmt.Sprintf("%s.%s %s, %s, %s", op, RColdOp(w.X()), r(w.A()), r(w.B()), r(w.C()))
	default:
		return op.String()
	}
}

// String renders the whole code, one instruction a line.
func (rc *RegCode) String() string {
	var sb strings.Builder
	for pc := range rc.Code {
		fmt.Fprintf(&sb, "%4d  %s\n", pc, rc.Format(pc))
	}
	for _, h := range rc.Handlers {
		fmt.Fprintf(&sb, "      handler %v -> %d, r%d\n", h.Ranges, h.To, h.Reg)
	}
	return sb.String()
}
