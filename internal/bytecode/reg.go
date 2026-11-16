package bytecode

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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
	// each turn.
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
	// loops that match: R[A+1] = the literal of Constants[C], made or
	// given back, and R[A] its method, exec or test (RLitTest), not read
	// where it is the realm's built-in,
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
	// stack form makes tail_call: once the stack is deep, the frame is
	// given to the callee in place or given up. Ops of their own, so that the ordinary call checks nothing
	// of it.
	RCallWT
	RCallMWT
	// RRet returns R[A]; RRetUndef returns undefined.
	RRet
	RRetUndef
	// RGetPrivate: R[A] = R[B].#R[C], a read of a private member, a
	// per-turn step of a class's methods:
	// Aux is its cache site in the high 16 bits, 0 for none, and its name,
	// with its #, for the errors in the low.
	RGetPrivate
	// RSetPrivate: R[A].#R[C] = R[B], a write of a private member, its
	// Aux as RGetPrivate's.
	RSetPrivate
	// RYield suspends a generator's frame with R[B], and on its
	// resumption R[A] = the value sent in, the next instruction a jump past
	// the return path a return resumes at: the step of a loop over a
	// generator, which a resumption by next goes on past (zzreg_zgen.go).
	RYield

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
	// The operations that run per turn of no hot loop the suites have:
	// declarations, classes, destructuring, generators, with, eval. Each is
	// an operation of its own, as every other is, its handler reached in
	// one dispatch (docs/register-interpreter.md, "The loop").
	//
	// RNewRegExp: R[A] = the RegExp literal of Constants[BC].
	RNewRegExp
	// RForOfStart and RForInStart: R[A] = a cursor over R[B], a
	// for-of loop's or a for-in loop's (startForOf, startForIn).
	RForOfStart
	RForInStart
	// RIterClose closes cursor R[B] as a break, a continue or a
	// return that leaves its loop does: an error of its return method's is
	// thrown. RIterCloseThrow closes it as a throw that leaves the loop
	// does: an error of its return method's is dropped.
	RIterClose
	RIterCloseThrow
	// Top-level code's declarations, each what the stack form's instruction
	// of the same name does, of the name Names[B] and the flag C:
	// check_global_lex, check_global_var, define_global_var,
	// declare_global_lex, and define_global_func of the function R[A].
	// RInitGlobalLex initializes the script's lexical binding Names[B]
	// to R[A] (init_global_lex).
	RCheckGlobalLex
	RCheckGlobalVar
	RDefineGlobalVar
	RDeclareGlobalLex
	RDefineGlobalFunc
	RInitGlobalLex
	// RGetGlobalOpt: R[A] = the global Names[B], or undefined where
	// there is none (get_global_opt, typeof's operand).
	RGetGlobalOpt
	// RDeclareModuleLex declares the module's lexical binding Names[B],
	// mutable where C is not 0, in its dead zone (declare_module_lex), and
	// RInitModuleLex initializes it to R[A] (init_module_lex).
	RDeclareModuleLex
	RInitModuleLex
	// Destructuring's: RIterStep, R[A] = the next value of cursor R[B],
	// undefined past its end (iter_step); RIterRest, R[A] = an array of
	// the values cursor R[B] has left (iter_rest); RCheckCoercible
	// throws where R[A] is undefined or null (check_coercible);
	// RToPropertyKey, R[A] = R[A] as a property key (to_property_key);
	// RObjectRest, R[A] = an object of R[B]'s own enumerable properties
	// but the Aux keys in the registers from C (object_rest).
	RIterStep
	RIterRest
	RCheckCoercible
	RToPropertyKey
	RObjectRest
	// RRestParam: R[A] = an array of the arguments from the B-th
	// (rest_param).
	RRestParam
	// RToString: R[A] = ToString(R[A]), a template's substitution
	// converted before what follows it runs (to_string).
	RToString
	// RTemplateObject: R[A] = the template object of the function's
	// Templates[BC], one for each site (template_object).
	RTemplateObject
	// An object literal's: RCopyDataProps copies R[B]'s own enumerable
	// properties to R[A] (copy_data_props, a spread); RDefineGetter and
	// RDefineSetter define accessor R[B] of R[A] named Names[C];
	// RDefineGetterIndex and RDefineSetterIndex define accessor
	// R[C] of R[A] keyed R[B]; RDefineIndex defines R[A][R[B]] = R[C]
	// (define_index); RSetFuncName names function R[A] after key R[B],
	// with "get " or "set " for C 1 or 2 (set_func_name);
	// RSetHomeObject makes R[B] function R[A]'s home object;
	// RSetProtoOf sets R[A]'s prototype to R[B] where it is an object
	// or null (__proto__: v).
	RCopyDataProps
	RDefineGetter
	RDefineSetter
	RDefineGetterIndex
	RDefineSetterIndex
	RDefineIndex
	RSetFuncName
	RSetHomeObject
	RSetProtoOf
	// Spreads: RArrayPush appends R[B] to array R[A], a hole where it
	// is the uninitialized marker (array_push); RArraySpread appends
	// what iterating R[B] gives (array_spread); RCallSpread calls R[A]
	// with this R[A+1] and the elements of array R[A+2] as its arguments,
	// or constructs it where C is 1, into R[B] (call_spread, new_spread).
	RArrayPush
	RArraySpread
	RCallSpread
	// RAssignConst throws the TypeError of an assignment to the
	// constant Names[B] (assign_const).
	RAssignConst
	// A strict assignment to a global whose value runs code: R[A] = whether
	// the global Names[B] resolves, asked before the value is made, through
	// cache site C (check_global_ref); RAssertResolved throws the
	// ReferenceError of Names[B] where R[A] says it did not (assert_resolved).
	RCheckGlobalRef
	RAssertResolved
	// Classes' (each what the stack form's instruction of the same name
	// does): RNewClass makes R[A] the constructor of a class extending
	// R[B] (new_class); RSetFieldInit gives constructor R[A] the
	// instances' field initializer R[B] (set_field_init); RDefineMethod
	// defines R[A]'s method Names[C] = R[B], not enumerable (define_method);
	// RClassGetter and RClassSetter define accessor R[B] of R[A]
	// named Names[C], and RClassGetterIndex, RClassSetterIndex and
	// RClassIndex accessor or method R[C] of R[A] keyed R[B], each not
	// enumerable, as a class's members are.
	RNewClass
	RSetFieldInit
	RDefineMethod
	RClassGetter
	RClassSetter
	RClassGetterIndex
	RClassSetterIndex
	RClassIndex
	// Private names': RPrivateName, R[A] = a new key of the private
	// name Names[B] (private_name); RNewPrivateMethods, R[A] = a new
	// list of private methods (new_private_methods); RAddPrivateMethod,
	// RAddPrivateGetter and RAddPrivateSetter add function R[B] to
	// list R[A] under key R[C]; RInstallPrivateMethods gives this the
	// methods of list R[A]; RDefinePrivate, RDefinePrivateMethod,
	// RDefinePrivateGetter and RDefinePrivateSetter add field,
	// method or accessor R[B] to R[A] under key R[C]; RPrivateIn, R[A] = whether R[B] has the private member keyed
	// R[C] (#x in o).
	RPrivateName
	RNewPrivateMethods
	RAddPrivateMethod
	RAddPrivateGetter
	RAddPrivateSetter
	RInstallPrivateMethods
	RDefinePrivate
	RDefinePrivateMethod
	RDefinePrivateGetter
	RDefinePrivateSetter
	RPrivateIn
	// super's and this's: RSuperCall, R[A] = this, bound by a call of
	// the parent constructor with the B arguments in the registers from C
	// (super_call); RSuperForward with the frame's arguments, as a
	// derived class's constructor it was not given makes it;
	// RSuperSpread with the elements of array R[B]; RSuperGet, R[A]
	// = super[Names[B]]; RSuperSet, super[Names[B]] = R[A];
	// RSuperBase, R[A] = what super reads from, or null
	// (super_base); RSuperGetIndex, R[A] = R[B][R[C]] read with this as
	// the receiver; RSuperSetIndex, R[A][R[B]] = R[C] written with this
	// as the receiver; RThis, R[A] = this, which a derived
	// constructor's super() binds (this_bound); RCheckThis throws
	// where it is not bound yet (check_this_init); RNewTarget, R[A] =
	// new.target; RDerivedRet, R[A] = what a derived constructor
	// returning R[A] hands back (derived_ret); RDeleteSuper throws the
	// ReferenceError of a super reference deleted.
	RSuperCall
	RSuperForward
	RSuperSpread
	RSuperGet
	RSuperSet
	RSuperBase
	RSuperGetIndex
	RSuperSetIndex
	RThis
	RCheckThis
	RNewTarget
	RDerivedRet
	RDeleteSuper
	// RCloseRegs closes the upvalues closures keep open on the B
	// registers from R[A], whose bindings a scope's end leaves: the next
	// entry of the scope makes new ones (close_regs).
	RCloseRegs
	// RWrap is R[A] = R[B] op R[C], op the operator Aux names -- add,
	// sub, mul or a bitwise one but >>> -- whose value is the second
	// argument of the two of the call of a method named asUintN or asIntN
	// right after it, X.asUintN(w, x op y): where the callee is
	// BigInt.asUintN or asIntN and x and y BigInts held in their Values,
	// it makes the call's value, x op y of width w, at once (wrapShort)
	// and passes the call over -- never a BigInt past 64 bits, which the
	// call would cut down again -- as the tree's wrapCall does (wrap).
	RWrap
	// A generator's and an async function's (zzreg_zgen.go): RGenEntry,
	// the code's first instruction, run by its first run and by a
	// resumption that throws, which it throws at the suspension; a
	// resumption that goes on starts at its pc. REndParams suspends a
	// generator its creation ran once its parameters are bound (RYield is
	// a yield's); RAwait suspends it to
	// await R[B], and on its resumption R[A] = what that settled to;
	// RYieldStar suspends a yield* with R[B], and on any resumption R[A]
	// = the value sent in and R[A+1] how, a ResumeNext, ResumeThrow or
	// ResumeReturn. RIterResume, R[A] = what cursor R[B]'s next, throw
	// or return, by the kind in R[C+1], gives for R[C] -- the result, or, a
	// return or a throw it has no method for ending the delegation, the
	// value to end it with, the next instruction a jump taken then;
	// RIterUnpackDelegate, R[A] = result R[B] where it is not done -- its
	// value in an async generator -- and its value where it is, cursor R[C]
	// marked done and the next instruction, a jump, taken.
	// RForAwaitOfStart, R[A] = a cursor over R[B] for a for await or an
	// async generator's yield* (startForAwaitOf). A for await's:
	// RAsyncIterNext, R[A] = what cursor R[B]'s next returns;
	// RIterResult, R[A] = result R[B]'s value, or where it is done, the
	// next instruction, a jump, taken; RAsyncIterClose, R[A] = what
	// cursor R[B]'s return method returns, the cursor done, or where it has
	// none, the next instruction, a jump, taken; RAsyncIterCheck throws
	// where R[A], what a return method's result settled to, is not an
	// object.
	RGenEntry
	REndParams
	RAwait
	RYieldStar
	RIterResume
	RIterUnpackDelegate
	RForAwaitOfStart
	RAsyncIterNext
	RIterResult
	RAsyncIterClose
	RAsyncIterCheck
	// RGetUpvalCheck, R[A] = upvalue BC, and RSetUpvalCheck,
	// upvalue BC = R[A], throw where the binding is in its dead zone, and
	// the store where it is a constant (zzreg_znames.go).
	RGetUpvalCheck
	RSetUpvalCheck
	// RDeleteVar: R[A] = delete of the global Names[B] (delete_var).
	RDeleteVar
	// RImport: R[A] = import(R[B]), import.defer or import.source by
	// the phase C>>2 (an Import constant), with the options R[B+1] where C&3
	// is 2 (the stack form's import). RImportMeta: R[A] = import.meta.
	RImport
	RImportMeta
	// A scope with using declarations (the stack form's
	// new_dispose_capability, add_disposable and dispose):
	// RNewDispose: R[A] = a new stack of resources; RAddDisposable
	// adds R[B] to R[A]'s, an await using's where C is 1;
	// RDispose disposes of R[A]'s, a throw of R[B] leaving the scope
	// where C is 1; RDisposeStep is a step of an await using scope's
	// disposal, of R[B]'s resources, the mode in C (bytecode.DisposeStart,
	// DisposeRecord, DisposeFail, DisposeFinish): R[A] is what a throw
	// leaving or an await failing threw, and is then what to await, and
	// R[A+1] whether there is one.
	RNewDispose
	RAddDisposable
	RDispose
	RDisposeStep
	// RDirectEval is a call written eval(...) (the stack form's
	// direct_eval): the callee R[A], its receiver R[A+1] and C arguments
	// from R[A+2], or, where Aux has RDirectEvalSpread, an array of them
	// in R[A+2]; R[B] = what it gives. Where the callee is the realm's
	// eval and the first argument a string, the code runs in the
	// caller's scope, the function's EvalScopes[Aux&^RDirectEvalSpread];
	// otherwise it is an ordinary call, a tail call where marked
	// RMarkTail.
	RDirectEval
	// The probes of a name a with object or an eval's variable may
	// answer (regcomp's with.go), each of the name Names[B] in the
	// innermost C of the frame's with objects and eval's variables, R[Aux]
	// = whether one has it: RWithGet, R[A] = its value there;
	// RWithGetThis, R[A] = its value and R[A+1] the object, a callee
	// and its receiver; RWithDelete, R[A] = delete of it there;
	// RWithSet stores R[A] there; RWithResolve, R[A] = the object,
	// or undefined. RWithRead, R[A] = Names[C] of the object R[B], and
	// RWithWrite stores R[A] there: a reference's read and store past
	// its with_resolve. RWithPush puts the object of R[A] on the
	// frame's with objects (ToObject), and RWithPop takes it off.
	RWithGet
	RWithGetThis
	RWithDelete
	RWithSet
	RWithResolve
	RWithRead
	RWithWrite
	RWithPush
	RWithPop
	// RThrowRef throws a ReferenceError of message Names[B]
	// (throw_reference_error): sloppy code's assignment to a call, made
	// once the call has been.
	RThrowRef
	// RDebug is a place code compiled for a debugger can stop (the
	// stack form's debug_stmt): its index in the function's
	// Debug.Statements in Aux, and B 1 for a debugger statement. It does
	// nothing unless a debugger has asked to stop somewhere, or B is 1
	// and one is listening.
	RDebug
	// RMovFar copies far register BC to register A (mov_far): a
	// binding past the 65,536 registers a 16-bit operand reaches, which
	// a function whose bindings do not fit them has (far registers, past
	// NumRegs' near ones). RMovToFar copies register A to far register
	// BC (mov_to_far), and RFarInit makes the Aux far registers from
	// BC undefined (far_init), as the function starts.
	RMovFar
	RMovToFar
	RFarInit

	// ROpCount is how many operations there are, each below it: the
	// interpreter's table has an entry for every value an ROp can hold,
	// those from ROpCount on a handler that panics.
	ROpCount
)

// RCmpTrue is the bit of a compare-and-branch's X (jtypeof's, a fused
// set_upval's) that says it jumps when the comparison holds, rather than
// when it does not.
const RCmpTrue = 0x80

// RDirectEvalSpread is the bit of direct_eval's Aux that says its
// arguments are an array.
const RDirectEvalSpread = 1 << 31

// RNameFar is a name operand that says the name's index in Names is past
// 16 bits: FarNames, by the instruction's pc, has it. Only the cold
// operations and the miss paths that read a name read it.
const RNameFar = 0xFFFF

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
	// RMarkTail marks a direct eval in tail position, as the stack form
	// makes it (tail_call): where it is an ordinary call and the stack is
	// deep, its frame is given up. The calls of the loop have ops of their
	// own for it (RCallWT, RCallMWT).
	RMarkTail
)

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
	// function's locals, its temporaries, and the register form's own;
	// and, where its bindings do not fit 16 bits, its far registers after
	// them all, which only RMovFar, RMovToFar and RFarInit
	// reach.
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
	ObjKeys [][]uint32
	// FarNames is, for an instruction whose name operand is RNameFar,
	// the name's index in Names, by its pc -- for check_ref, whose B is
	// a slot, the slot's index in Locals; nil where none is.
	FarNames []uint32
	// LocalReg is each local's register, or -1, where the code has a
	// call_reuse, whose frame's locals the runtime reads by it, a
	// direct_eval, whose code it links to them, or is compiled for a
	// debugger, which shows and sets them; nil otherwise.
	LocalReg []int32
	// UsesArguments says the code makes the arguments object, whose frame
	// keeps the arguments as passed (RArguments).
	UsesArguments bool
	// ForwardsArguments says a frame keeps its arguments as passed for
	// RApplyArgs, with no arguments object made.
	ForwardsArguments bool
	// Handlers are the code's exception handlers, innermost first.
	Handlers []RHandler
	// BackLive is, for each backward jump, by pc in order, its pc and the
	// first register past those live at the loop's head: the registers from
	// there are temporaries no turn reads before it writes them, which the
	// sweep of stale slots clears (sweepStaleSlots).
	BackLive []uint32
	// KeepsArgs says a frame keeps its arguments as passed, for a rest
	// parameter (rest_param) or a super call that hands them on
	// (super_forward).
	KeepsArgs bool
	// ThisRef says the code reads this from the binding a derived
	// constructor's super() binds (this_bound), not from register 1: a
	// derived constructor's, or an arrow's written inside one.
	ThisRef bool
	// Names, Constants and PropSites are the function's names, constants
	// and property sites with those the register code adds, where it was
	// compiled at the function's first call (RegLater): the Function's own
	// are shared by then and never change, and a runtime adds what is past
	// them to a closure's tables as it first runs the code (regExtend).
	// Nil and 0 for code compiled with its function, whose additions are
	// the Function's.
	Names     []string
	Constants []Constant
	PropSites uint32
}

// RegLater is a function's register code, compiled when it is first asked
// for: functions never called are never compiled, as V8 compiles lazily.
// Top-level code's is compiled with the code, and given as one already
// done (RegReady). It is safe for concurrent use: a Function is shared by the runtimes that
// run its program.
type RegLater struct {
	mu   sync.Mutex
	done atomic.Bool
	rc   *RegCode
	// make compiles the code, holding what the compiler recorded until
	// then; it is dropped once it has run.
	make func() *RegCode
	// old says the function runs on the stack interpreter and the tree
	// tier wherever it runs (Function.KeepOldTiers).
	old atomic.Bool
}

// RegNone is a RegLater with no code: top-level code's that the register
// backend refused, which a runtime running register code counts as a
// fallback, where a function with no RegLater at all was compiled with no
// register form.
func RegNone() *RegLater {
	l := &RegLater{}
	l.done.Store(true)
	return l
}

// KeepOldTiers has f run on the stack interpreter and the tree tier, its
// register form unused, in every runtime: a runtime that runs no register
// code -- one with the JIT -- marks the code it loads so, where it was
// compiled for one that does, since the tree a function is run as is the
// same in every runtime. A host compiles a runtime's code for it, with no
// register form for such a runtime, so this is for code handed to both.
func (f *Function) KeepOldTiers() {
	if l := f.Reg; l != nil && !l.old.Load() {
		l.old.Store(true)
	}
}

// OldTiers reports whether KeepOldTiers marked f.
func (f *Function) OldTiers() bool {
	l := f.Reg
	return l != nil && l.old.Load()
}

// NewRegLater returns the register code that make compiles when first
// asked for.
func NewRegLater(make func() *RegCode) *RegLater { return &RegLater{make: make} }

// RegReady returns rc, register code compiled already, as a RegLater, or
// nil where rc is nil.
func RegReady(rc *RegCode) *RegLater {
	if rc == nil {
		return nil
	}
	l := &RegLater{rc: rc}
	l.done.Store(true)
	return l
}

// RegCode is f's register code, compiled now if it is compiled lazily and
// has not been, or nil where it has none.
func (f *Function) RegCode() *RegCode {
	l := f.Reg
	if l == nil {
		return nil
	}
	if l.done.Load() {
		return l.rc
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.done.Load() {
		l.rc, l.make = l.make(), nil
		l.done.Store(true)
	}
	return l.rc
}

// RegCompiled is f's register code where it has been compiled, without
// compiling it, or nil.
func (f *Function) RegCompiled() *RegCode {
	if l := f.Reg; l != nil && l.done.Load() {
		return l.rc
	}
	return nil
}

// LiveAt is how many of the frame's registers are live at the head of the
// loop the backward jump at pc goes to, or NumRegs where pc is none.
func (rc *RegCode) LiveAt(pc int) int {
	lo, hi := 0, len(rc.BackLive)/2
	for lo < hi {
		m := (lo + hi) / 2
		switch p := int(rc.BackLive[2*m]); {
		case p == pc:
			return int(rc.BackLive[2*m+1])
		case p < pc:
			lo = m + 1
		default:
			hi = m
		}
	}
	return rc.NumRegs
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
	// Catches says the handler is a catch clause's, which keeps what it
	// catches; any other throws it again once it has run (a finally
	// clause, an iterator's close), which a debugger asks to know.
	Catches bool
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
	RGetIndexPre: "get_index_pre", RGetIndexPost: "get_index_post", RNewArrayE: "new_array_empty", RAddIndex: "add_index", RAddIndexK: "add_index_k", RAddIndexI: "add_index_imm", RSwitch: "switch", RJTypeof: "jtypeof", RTypeof: "typeof", RSwitchTypeof: "switch_typeof", RNewObjectLit: "new_object_lit", RAppend: "append", RStrFreeze: "str_freeze", RCharCodeAt: "char_code_at", RIterNext: "iter_next", RYield: "yield", RLit: "regexp_lit", RGetPrivate: "get_private", RSetPrivate: "set_private", RJLtU: "jlt_upval", RJNLtU: "jnlt_upval", RJLeU: "jle_upval", RJNLeU: "jnle_upval",
	RNewRegExp: "new_regexp", RForOfStart: "for_of_start", RForInStart: "for_in_start",
	RIterClose: "iter_close", RIterCloseThrow: "iter_close_throw",
	RCheckGlobalLex: "check_global_lex", RCheckGlobalVar: "check_global_var",
	RDefineGlobalVar: "define_global_var", RDeclareGlobalLex: "declare_global_lex",
	RDefineGlobalFunc: "define_global_func", RInitGlobalLex: "init_global_lex",
	RGetGlobalOpt: "get_global_opt", RDeclareModuleLex: "declare_module_lex",
	RInitModuleLex: "init_module_lex", RIterStep: "iter_step", RIterRest: "iter_rest",
	RCheckCoercible: "check_coercible", RToPropertyKey: "to_property_key",
	RObjectRest: "object_rest", RRestParam: "rest_param", RToString: "to_string",
	RTemplateObject: "template_object", RCopyDataProps: "copy_data_props",
	RDefineGetter: "define_getter", RDefineSetter: "define_setter",
	RDefineGetterIndex: "define_getter_index", RDefineSetterIndex: "define_setter_index",
	RDefineIndex: "define_index", RSetFuncName: "set_func_name",
	RSetHomeObject: "set_home_object", RSetProtoOf: "set_proto_of",
	RArrayPush: "array_push", RArraySpread: "array_spread", RCallSpread: "call_spread",
	RAssignConst: "assign_const", RCheckGlobalRef: "check_global_ref",
	RAssertResolved: "assert_resolved", RWrap: "wrap",
	RNewClass: "new_class", RSetFieldInit: "set_field_init", RDefineMethod: "define_method",
	RClassGetter: "class_getter", RClassSetter: "class_setter",
	RClassGetterIndex: "class_getter_index", RClassSetterIndex: "class_setter_index",
	RClassIndex: "class_index", RPrivateName: "private_name",
	RNewPrivateMethods: "new_private_methods", RAddPrivateMethod: "add_private_method",
	RAddPrivateGetter: "add_private_getter", RAddPrivateSetter: "add_private_setter",
	RInstallPrivateMethods: "install_private_methods", RDefinePrivate: "define_private",
	RDefinePrivateMethod: "define_private_method", RDefinePrivateGetter: "define_private_getter",
	RDefinePrivateSetter: "define_private_setter", RPrivateIn: "private_in", RSuperCall: "super_call",
	RSuperForward: "super_forward", RSuperSpread: "super_spread", RSuperGet: "super_get",
	RSuperSet: "super_set", RSuperBase: "super_base", RSuperGetIndex: "super_get_index",
	RSuperSetIndex: "super_set_index", RThis: "this_bound", RCheckThis: "check_this_init",
	RNewTarget: "new_target", RDerivedRet: "derived_ret", RDeleteSuper: "delete_super",
	RCloseRegs: "close_regs", RGenEntry: "gen_entry", REndParams: "end_params",
	RAwait: "await", RYieldStar: "yield_star", RIterResume: "iter_resume",
	RIterUnpackDelegate: "iter_unpack_delegate", RForAwaitOfStart: "for_await_of_start",
	RAsyncIterNext: "async_iter_next", RIterResult: "iter_result",
	RAsyncIterClose: "async_iter_close", RAsyncIterCheck: "async_iter_check",
	RGetUpvalCheck: "get_upval_check", RSetUpvalCheck: "set_upval_check",
	RDeleteVar: "delete_var", RImport: "import", RImportMeta: "import_meta",
	RNewDispose: "new_dispose_capability", RAddDisposable: "add_disposable", RDispose: "dispose",
	RDisposeStep: "dispose_step", RDirectEval: "direct_eval",
	RWithGet: "with_get", RWithGetThis: "with_get_this", RWithDelete: "with_delete",
	RWithSet: "with_set", RWithResolve: "with_resolve", RWithRead: "with_read",
	RWithWrite: "with_write", RWithPush: "with_push", RWithPop: "with_pop",
	RThrowRef: "throw_reference_error", RDebug: "debug",
	RMovFar: "mov_far", RMovToFar: "mov_to_far", RFarInit: "far_init",
}

// Cold reports whether op is one of the operations of no hot loop,
// RNewRegExp to RFarInit, which native code leaves to Go but for a few.
func (op ROp) Cold() bool { return op >= RNewRegExp && op < ROpCount }

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
	case RMove, RInc, RDec, RPostInc, RPostDec, RGetLength, RYield:
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
	case RGetPrivate:
		return fmt.Sprintf("%s %s, %s.#%s", op, r(w.A()), r(w.B()), r(w.C()))
	case RSetPrivate:
		return fmt.Sprintf("%s %s.#%s, %s", op, r(w.A()), r(w.C()), r(w.B()))
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
	case RNewRegExp:
		return fmt.Sprintf("%s %s, k%d", op, r(w.A()), w.BC())
	case RIterClose, RIterCloseThrow:
		return fmt.Sprintf("%s %s", op, r(w.B()))
	}
	if op := w.Op(); op >= RNewRegExp && op < ROpCount {
		// The operations of no hot loop: their operands as they lie.
		return fmt.Sprintf("%s %s, %s, %s", op, r(w.A()), r(w.B()), r(w.C()))
	}
	return w.Op().String()
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
