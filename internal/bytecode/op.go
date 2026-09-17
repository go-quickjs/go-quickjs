// Package bytecode defines the instruction set and the compiled function
// representation that the compiler emits and the virtual machine executes.
//
// Instructions are fixed-width structs rather than a packed byte stream. A byte
// stream is more compact and is what the C implementation uses, but decoding
// variable-length operands costs more in Go than the extra memory does: there
// is no computed goto, so every instruction already pays for a bounds-checked
// switch, and shrinking the operand fetch to a struct field access is the
// larger win.
package bytecode

// Op is an instruction opcode.
type Op uint8

const (
	// --- Constants and simple pushes -------------------------------------
	OpNop       Op = iota
	OpPushConst    // push Constants[A]
	OpPushUndef    // push undefined
	OpPushNull     // push null
	OpPushTrue     // push true
	OpPushFalse    // push false
	OpPushThis     // push the current this binding
	OpPushInt      // push the int32 in A, sign-extended
	OpPushEmptyString
	// OpPushUninitialized stores the temporal-dead-zone marker, which a let or
	// const binding holds until its declaration runs.
	OpPushUninitialized

	// --- Stack shuffling --------------------------------------------------
	OpDup  // duplicate the top
	OpDup2 // duplicate the top two, preserving order
	OpDrop // discard the top
	OpSwap // exchange the top two
	OpRot3 // move the third element to the top
	OpRot4 // move the fourth element to the top
	// OpInsert2 and OpInsert3 push a copy of the top down past 2 or 3 slots.
	// Property assignment needs them to keep the assigned value available as
	// the expression's result while the receiver and key are consumed.
	OpInsert2
	OpInsert3
	OpNipUnder    // remove A values from beneath the top of the stack
	OpAssignConst // throw: the name in Names[A] is a const binding
	OpInsert4

	// --- Local variables --------------------------------------------------
	OpGetLocal // push Locals[A]
	OpSetLocal // pop into Locals[A]
	OpPutLocal // store the top into Locals[A] without popping
	// OpGetLocalCheck reports a ReferenceError if the local is still in its
	// temporal dead zone, which let and const bindings require.
	OpGetLocalCheck
	OpSetLocalCheck // assignment to a const or a TDZ binding
	OpInitLocal     // first store to a let/const, clearing the dead zone

	// --- Closure variables ------------------------------------------------
	OpGetUpvalue
	OpSetUpvalue
	OpGetUpvalueCheck
	OpSetUpvalueCheck
	OpInitUpvalue
	// OpCloseUpvalues converts every open upvalue at or above local A into a
	// closed one, which happens when a block that captured bindings exits.
	OpCloseUpvalues

	// --- Global variables -------------------------------------------------
	OpGetGlobal    // push global Names[A]; ReferenceError if absent
	OpGetGlobalOpt // push global Names[A] or undefined; used by typeof
	OpSetGlobal
	OpDefineGlobalVar  // var/function declaration on the global object
	OpDefineGlobalFunc // like the above but always overwrites
	// OpCheckGlobalLex reports a top-level let, const or class whose name is
	// already a property of the global object that cannot be removed.
	OpCheckGlobalLex
	// OpCheckGlobalVar reports a top-level var or function declaration whose
	// name a lexical binding already has. B is 1 for a function, which has the
	// extra requirement that the property it replaces be one it could create.
	OpCheckGlobalVar
	// OpDeclareGlobalLex creates a script-level lexical binding, in its dead
	// zone. A is the name; B is 1 for a let or class, 0 for a const.
	OpDeclareGlobalLex
	// OpInitGlobalLex gives one its first value, which is what takes it out of
	// the dead zone.
	OpInitGlobalLex
	// OpDeclareModuleLex creates a module's top-level lexical binding in its
	// dead zone. A is the name; B is 1 for a let or class, 0 for a const.
	//
	// It is a property of the module environment rather than a slot, because
	// what a module exports has to be something the linker can forward to.
	OpDeclareModuleLex
	// OpInitModuleLex pops a value and gives a binding its first one, taking
	// it out of the dead zone.
	OpInitModuleLex

	// --- Properties -------------------------------------------------------
	OpGetProp    // obj -> obj[Names[A]]
	OpSetProp    // obj value -> ; assigns obj[Names[A]]
	OpGetIndex   // obj key -> obj[key]
	OpSetIndex   // obj key value ->
	OpDeleteProp // obj key -> bool
	// OpDeleteVar deletes a binding named by Names[A], which succeeds only for
	// a configurable property of the global object.
	OpDeleteVar
	// OpGetPropThis and OpGetIndexThis leave the receiver beneath the fetched
	// value, so that a method call can pass it as `this` without re-evaluating
	// the object expression.
	OpGetPropThis
	OpGetIndexThis
	OpDefineField  // define an own data property, ignoring setters
	OpDefineIndex  // as above with a computed key
	OpDefineGetter // define an accessor's getter half
	OpDefineSetter // define an accessor's setter half
	// The computed-key forms of the accessor definitions, which take the key
	// from the stack beneath the function.
	OpDefineGetterIndex
	OpDefineSetterIndex
	// OpSetFuncName names the function on top of the stack after the property
	// key beneath it, which is how a method with a computed key gets a name.
	// A is 0 for a plain method, 1 for a getter and 2 for a setter.
	OpSetFuncName
	OpGetLength     // a fast path for the very common `.length`
	OpSetProtoOf    // set __proto__ from an object literal
	OpCopyDataProps // object spread: copy own enumerable properties

	// --- Private class members -------------------------------------------
	// A private name's key is minted when its class is evaluated, so every
	// instruction below takes it from a binding rather than from the name
	// table. A is the name, which only an error message uses; B says where the
	// key is: a slot of this function when its low bit is clear, one of its
	// upvalues when it is set, and the index in the rest.
	OpGetPrivate
	OpSetPrivate
	OpDefinePrivate
	OpDefinePrivateGetter
	OpDefinePrivateSetter
	OpPrivateIn // `#x in obj`
	OpGetPrivateMethod
	// OpPrivateName mints the key for one private name of one class evaluation,
	// which is what keeps two evaluations of the same class apart. A is the
	// name, for the error messages an access through the key produces.
	OpPrivateName
	// OpNewPrivateMethods pushes the list of private methods and accessors an
	// instance of a class carries, which OpAddPrivateMethod fills in as the
	// class body is evaluated and OpInstallPrivateMethods copies onto each
	// instance as it is made.
	//
	// They belong to the instance rather than to the prototype: an object that
	// merely inherits from the prototype is not an instance, and asking it for
	// a private member has to fail.
	OpNewPrivateMethods
	OpAddPrivateMethod
	OpAddPrivateGetter
	OpAddPrivateSetter
	OpInstallPrivateMethods

	// --- Parameters and arguments -----------------------------------------
	// OpRestParam gathers the arguments from index A onwards into an array,
	// which is what a rest parameter binds.
	OpRestParam
	// OpGetArguments materializes the arguments object.
	OpGetArguments
	// OpArrayRest pushes the elements of the array on the stack from index A
	// onwards, for the rest element of an array pattern.
	OpArrayRest
	// OpObjectRest builds an object holding the source's own enumerable
	// properties except the A keys sitting above it on the stack, for the rest
	// element of an object pattern.
	OpObjectRest

	// --- Arithmetic -------------------------------------------------------
	OpAdd
	OpSub
	OpMul
	OpDiv
	OpMod
	OpPow
	OpNeg
	OpPos
	OpInc
	OpDec

	// --- Bitwise ----------------------------------------------------------
	OpBitAnd
	OpBitOr
	OpBitXor
	OpBitNot
	OpShl
	OpShr  // signed right shift
	OpUShr // unsigned right shift

	// --- Comparison and logic --------------------------------------------
	OpEq
	OpNe
	OpStrictEq
	OpStrictNe
	OpLt
	OpLe
	OpGt
	OpGe
	OpIn
	OpInstanceOf
	OpNot
	OpTypeOf
	// OpIsNullish tests for null or undefined without popping, which ?. and ??
	// both need.
	OpIsNullish

	// --- Control flow -----------------------------------------------------
	OpJump            // unconditional, to A
	OpJumpIfFalse     // pops
	OpJumpIfTrue      // pops
	OpJumpIfFalseKeep // peeks; used by && and ||
	OpJumpIfTrueKeep
	OpJumpIfNullish // peeks; used by ?? and ?.
	OpJumpIfNotNullish

	// --- Calls ------------------------------------------------------------
	OpCall // A = argument count; stack: callee args...
	// OpDirectEval is a call whose callee may turn out to be the intrinsic
	// eval, in which case the evaluated code shares the caller's scope. A is
	// the index into EvalScopes and B the argument count.
	OpDirectEval
	OpCallMethod // A = argument count; stack: this callee args...
	OpNew        // A = argument count
	// OpCallSpread and OpNewSpread take their arguments from an array on the
	// stack rather than from individual slots, which is how a call containing
	// a spread element is compiled.
	OpCallSpread
	OpNewSpread
	OpSuperCall
	OpReturn
	OpReturnUndef

	// --- Function and object construction ---------------------------------
	OpClosure // build a closure from function template Constants[A]
	OpNewObject
	OpNewArray     // A = element count, taken from the stack
	OpNewArrayFrom // build from an iterator result already on the stack
	OpArrayPush    // append to the array beneath the top
	OpArraySpread  // spread an iterable into the array beneath
	OpDefineMethod // attach a method to an object or class prototype
	OpNewClass
	OpNewRegExp
	OpConcat         // string concatenation for templates, A = part count
	OpTemplateObject // push the frozen strings array for Templates[A]

	// --- Iteration --------------------------------------------------------
	OpForInStart
	OpForOfStart
	OpForAwaitOfStart
	OpIterNext // pushes value and a done flag
	OpIterClose
	OpIterNextOrJump // advances, or jumps to A when exhausted
	// OpAsyncIterNext calls the async iterator's next method and pushes the
	// promise it returns, which OpAwait then settles.
	OpAsyncIterNext
	// OpIterResultOrJump unpacks an iterator result object, pushing its value
	// or jumping to A when it reports done.
	OpIterResultOrJump
	OpSpreadIter // spread an iterable onto the stack for a call
	// OpIterResume drives a `yield*`: the cursor is beneath a value and the
	// kind of resumption that produced it, and the kind decides which of the
	// delegate's three methods is called. A is 1 for an async delegation,
	// whose result is a promise to await.
	//
	// A delegate with no throw is closed and the delegation fails; one with no
	// return simply ends, which is what lets `yield*` over a plain iterator
	// work at all.
	OpIterResume
	// OpIterUnpackDelegate reads the result of one `yield*` step. A is where to
	// jump when the delegation is over; B packs the local holding the kind of
	// resumption that produced the result with a flag saying whether the
	// result object is yielded as it is.
	//
	// A result that says done ends the delegation -- as a return of the outer
	// generator when that is what was forwarded, and as the value of the
	// yield* otherwise.
	OpIterUnpackDelegate
	// OpIterSend calls the cursor's next method with the value on top of the
	// stack, which is how `yield*` forwards what its caller sent in. The cursor
	// stays beneath, and the raw iterator result replaces the sent value.
	OpIterSend
	// OpIterSendAsync is OpIterSend for an async iterator, pushing the promise
	// that OpAwait then settles.
	OpIterSendAsync
	// OpIterUnpack replaces an iterator result with its value, jumping to A
	// when the result reports done. The value is pushed either way, since
	// `yield*` evaluates to whatever the delegate returned.
	OpIterUnpack
	// OpEndParams marks the end of a generator's parameter prologue. It is a
	// no-op except when the interpreter is running that prologue on its own,
	// which is how a generator binds its parameters at call time rather than on
	// its first resumption.
	OpEndParams
	// The three instructions a destructuring pattern drives its iterator with.
	// The cursor stays on the operand stack for the whole pattern, so that an
	// abrupt exit closes it the way it closes a for-of's; A is how far below
	// the top it sits, since a target's reference may have been pushed above
	// it.
	//
	// OpIterStep pushes the next value, or undefined once the iterator is
	// exhausted -- a pattern with more names than the source has values leaves
	// the rest undefined. OpIterRest pushes everything left as an array.
	// OpIterCloseNormal pops the cursor and tells an iterator the pattern
	// stopped short of, which unlike a close during an abrupt completion has
	// nothing else in flight and so reports what went wrong.
	OpIterStep
	OpIterRest
	OpIterCloseNormal
	// OpGetPropUnder and OpGetIndexUnder read a property of an object that is
	// not on top of the stack, which an object pattern needs: the target of
	// each property is evaluated before the property is read, so by then the
	// source sits underneath whatever the target's reference left behind.
	//
	// For OpGetPropUnder, A is the name and B how far down the source is. For
	// OpGetIndexUnder, A is how far down the key is, with the source just
	// below it. Neither consumes anything.
	OpGetPropUnder
	OpGetIndexUnder
	// OpIterToArray replaces an array-destructuring source with a dense array of
	// the values its iterator produces. A is how many to pull, or IterAll when
	// the pattern has a rest element and needs every one.
	OpIterToArray

	// --- Exceptions -------------------------------------------------------
	OpThrow
	// OpPushCatch registers a handler at A; OpPopCatch unregisters the
	// innermost one. Finally blocks are compiled as a handler plus an explicit
	// re-throw, so the VM needs no separate notion of them.
	OpPushCatch
	OpPopCatch
	OpPushFinally
	OpRethrow
	OpThrowTypeError // used for TDZ and const-assignment failures

	// --- Generators and async ---------------------------------------------
	OpYield
	// OpYieldStar suspends inside a `yield*`, where the three ways a generator
	// can be resumed all have to be forwarded to the inner iterator rather
	// than acted on here. It pushes the value that came back and which of the
	// three it was: 0 for next, 1 for throw, 2 for return.
	OpYieldStar
	OpAwait
	OpInitialYield // suspends a generator before its first statement
	OpAsyncReturn

	// --- Miscellaneous ----------------------------------------------------
	OpGetSuperProp
	OpGetSuperIndex
	OpSetSuperProp
	OpSetSuperIndex
	// OpSuperBase pushes the object a super reference reads from, which is
	// settled before the key is computed: changing the home object's prototype
	// while the key runs does not move the reference.
	OpSuperBase
	OpNewTarget
	// OpImportMeta pushes the running module's import.meta object, creating it
	// on first use.
	OpImportMeta
	// OpPushCallee pushes the function object currently executing, which is
	// how a named function expression refers to itself.
	OpPushCallee
	OpToObject
	OpCheckCoercible // throw if the value on top is null or undefined
	OpToPropertyKey
	// OpToPropertyKeyOfBase is OpToPropertyKey where the object the key will be
	// used on is beneath it, and is required to be coercible first.
	OpToPropertyKeyOfBase
	OpToNumber
	// OpToNumeric is OpToNumber except that a BigInt stays one, which is what
	// the update operators need: `1n++` is 2n, not an error.
	OpToNumeric
	OpToString
	// --- `with` -----------------------------------------------------------
	// Inside a `with` body every name compiles to one of the probes below
	// followed by the instruction that would have been emitted anyway. A probe
	// answers from the enclosing `with` objects and jumps to B, or falls
	// through to the static instruction. A is the name.
	OpWithPush // push the object on top onto the frame's `with` chain
	OpWithPop
	OpWithGet     // push the value and jump
	OpWithGetThis // push the value and the object, for a call
	OpWithSet     // store the value on top, leave it, and jump
	OpWithDelete  // push whether the delete succeeded and jump
	OpWithTypeof  // push the type of the value and jump
	// OpWithGetUnder and OpWithPutUnder are the two halves of a reference that
	// is read and then written back: a compound assignment, or an update. The
	// name is resolved once, by the read, and the write goes to whatever the
	// read found -- which a second probe would not necessarily find again,
	// since a getter may have deleted the property in between.
	OpWithGetUnder // replace the placeholder base with the object, push the value, jump
	OpWithPutUnder // store into the base beneath the value, drop it, and jump
	// OpWithResolve resolves a name against the `with` objects without reading
	// it, which is what a plain assignment needs: the reference is settled
	// before the value is evaluated, but nothing is read from it.
	OpWithResolve
	OpSetName // give an anonymous function the name in Names[A]
	OpSetHomeObject
	OpCheckThisInit // a derived constructor must call super() before `this`
	OpInitThis
	// OpThrowDeleteSuper reports `delete super.x`, which parses and then fails:
	// a super reference names a property of the home object's prototype, and
	// there is no object it could be removed from.
	OpThrowDeleteSuper

	// --- Lexical parameters ------------------------------------------------
	// A parameter list with a default, a pattern or a rest element binds its
	// names one at a time, so one the prologue has not reached yet may not be
	// read: `function f(a = b, b) {}` is a reference error, and so is
	// `function f(a = a) {}`.
	//
	// OpParamsToDeadZone puts the first A parameter slots into the dead zone,
	// which is where such a list starts. OpInitParam takes slot A back out by
	// filling it from argument A, and OpParamNeedsDefault pushes whether
	// argument A was undefined or absent, which is when its default runs.
	OpParamsToDeadZone
	OpInitParam
	OpParamNeedsDefault

	// opCount is the number of opcodes, used to size the name table.
	opCount
)

// opNames gives each opcode a readable name for disassembly and panics.
var opNames = [opCount]string{
	OpNop: "nop", OpPushConst: "push_const", OpPushUndef: "push_undef",
	OpPushNull: "push_null", OpPushTrue: "push_true", OpPushFalse: "push_false",
	OpPushThis: "push_this", OpPushInt: "push_int",
	OpPushEmptyString:   "push_empty_string",
	OpPushUninitialized: "push_uninitialized", OpInitParam: "init_param",
	OpParamNeedsDefault: "param_needs_default",
	OpParamsToDeadZone:  "params_to_dead_zone",

	OpDup: "dup", OpDup2: "dup2", OpDrop: "drop", OpSwap: "swap",
	OpRot3: "rot3", OpRot4: "rot4",
	OpInsert2: "insert2", OpInsert3: "insert3", OpInsert4: "insert4",

	OpGetLocal: "get_local", OpSetLocal: "set_local", OpPutLocal: "put_local",
	OpGetLocalCheck: "get_local_check", OpSetLocalCheck: "set_local_check",
	OpInitLocal: "init_local",

	OpGetUpvalue: "get_upvalue", OpSetUpvalue: "set_upvalue",
	OpGetUpvalueCheck: "get_upvalue_check", OpSetUpvalueCheck: "set_upvalue_check",
	OpInitUpvalue: "init_upvalue", OpCloseUpvalues: "close_upvalues",

	OpGetGlobal: "get_global", OpGetGlobalOpt: "get_global_opt",
	OpSetGlobal: "set_global", OpDefineGlobalVar: "define_global_var",
	OpDefineGlobalFunc: "define_global_func",
	OpCheckGlobalLex:   "check_global_lex",
	OpCheckGlobalVar:   "check_global_var",
	OpDeclareGlobalLex: "declare_global_lex",
	OpInitGlobalLex:    "init_global_lex",
	OpDeclareModuleLex: "declare_module_lex",
	OpInitModuleLex:    "init_module_lex",

	OpGetProp: "get_prop", OpSetProp: "set_prop", OpGetIndex: "get_index",
	OpSetIndex: "set_index", OpDeleteProp: "delete_prop",
	OpDeleteVar:   "delete_var",
	OpGetPropThis: "get_prop_this", OpGetIndexThis: "get_index_this",
	OpDefineField: "define_field", OpDefineIndex: "define_index",
	OpDefineGetter: "define_getter", OpDefineSetter: "define_setter",
	OpDefineGetterIndex: "define_getter_index",
	OpDefineSetterIndex: "define_setter_index",
	OpSetFuncName:       "set_func_name",
	OpGetLength:         "get_length", OpSetProtoOf: "set_proto_of",
	OpCopyDataProps: "copy_data_props",

	OpGetPrivate: "get_private", OpSetPrivate: "set_private",
	OpDefinePrivate: "define_private", OpPrivateIn: "private_in",
	OpDefinePrivateGetter:   "define_private_getter",
	OpDefinePrivateSetter:   "define_private_setter",
	OpGetPrivateMethod:      "get_private_method",
	OpPrivateName:           "private_name",
	OpNewPrivateMethods:     "new_private_methods",
	OpAddPrivateMethod:      "add_private_method",
	OpAddPrivateGetter:      "add_private_getter",
	OpAddPrivateSetter:      "add_private_setter",
	OpInstallPrivateMethods: "install_private_methods",
	OpRestParam:             "rest_param",
	OpGetArguments:          "get_arguments",
	OpArrayRest:             "array_rest",
	OpObjectRest:            "object_rest",

	OpAdd: "add", OpSub: "sub", OpMul: "mul", OpDiv: "div", OpMod: "mod",
	OpPow: "pow", OpNeg: "neg", OpPos: "pos", OpInc: "inc", OpDec: "dec",

	OpBitAnd: "bit_and", OpBitOr: "bit_or", OpBitXor: "bit_xor",
	OpBitNot: "bit_not", OpShl: "shl", OpShr: "shr", OpUShr: "ushr",

	OpEq: "eq", OpNe: "ne", OpStrictEq: "strict_eq", OpStrictNe: "strict_ne",
	OpLt: "lt", OpLe: "le", OpGt: "gt", OpGe: "ge", OpIn: "in",
	OpInstanceOf: "instanceof", OpNot: "not", OpTypeOf: "typeof",
	OpIsNullish: "is_nullish",

	OpJump: "jump", OpJumpIfFalse: "jump_if_false", OpJumpIfTrue: "jump_if_true",
	OpJumpIfFalseKeep: "jump_if_false_keep", OpJumpIfTrueKeep: "jump_if_true_keep",
	OpJumpIfNullish: "jump_if_nullish", OpJumpIfNotNullish: "jump_if_not_nullish",

	OpCall: "call", OpDirectEval: "direct_eval", OpCallMethod: "call_method", OpNew: "new",
	OpCallSpread: "call_spread", OpNewSpread: "new_spread",
	OpSuperCall: "super_call", OpReturn: "return", OpReturnUndef: "return_undef",

	OpClosure: "closure", OpNewObject: "new_object", OpNewArray: "new_array",
	OpNewArrayFrom: "new_array_from", OpArrayPush: "array_push",
	OpArraySpread: "array_spread", OpDefineMethod: "define_method",
	OpNewClass: "new_class", OpNewRegExp: "new_regexp", OpConcat: "concat",
	OpTemplateObject: "template_object",

	OpForInStart: "for_in_start", OpForOfStart: "for_of_start",
	OpForAwaitOfStart: "for_await_of_start", OpIterNext: "iter_next",
	OpIterClose: "iter_close", OpIterNextOrJump: "iter_next_or_jump",
	OpAsyncIterNext:      "async_iter_next",
	OpIterResultOrJump:   "iter_result_or_jump",
	OpSpreadIter:         "spread_iter",
	OpIterSend:           "iter_send",
	OpIterSendAsync:      "iter_send_async",
	OpIterUnpack:         "iter_unpack",
	OpEndParams:          "end_params",
	OpIterResume:         "iter_resume",
	OpIterUnpackDelegate: "iter_unpack_delegate",
	OpIterToArray:        "iter_to_array",

	OpThrow: "throw", OpPushCatch: "push_catch", OpPopCatch: "pop_catch",
	OpPushFinally: "push_finally", OpRethrow: "rethrow",
	OpThrowTypeError: "throw_type_error",

	OpYield: "yield", OpYieldStar: "yield_star", OpAwait: "await",
	OpInitialYield: "initial_yield", OpAsyncReturn: "async_return",

	OpGetSuperProp: "get_super_prop", OpGetSuperIndex: "get_super_index",
	OpSetSuperProp: "set_super_prop", OpSetSuperIndex: "set_super_index",
	OpSuperBase: "super_base",
	OpNewTarget: "new_target", OpImportMeta: "import_meta",
	OpPushCallee:          "push_callee",
	OpToObject:            "to_object",
	OpCheckCoercible:      "check_coercible",
	OpToPropertyKey:       "to_property_key",
	OpToPropertyKeyOfBase: "to_property_key_of_base", OpToNumber: "to_number",
	OpToNumeric: "to_numeric",
	OpToString:  "to_string", OpWithPush: "with_push", OpWithPop: "with_pop",
	OpWithGet: "with_get", OpWithGetThis: "with_get_this", OpWithSet: "with_set",
	OpWithDelete: "with_delete", OpWithTypeof: "with_typeof",
	OpWithGetUnder: "with_get_under", OpWithPutUnder: "with_put_under",
	OpWithResolve: "with_resolve",
	OpSetName:     "set_name", OpSetHomeObject: "set_home_object",
	OpCheckThisInit:    "check_this_init",
	OpInitThis:         "init_this",
	OpThrowDeleteSuper: "throw_delete_super",
}

func (op Op) String() string {
	if int(op) < len(opNames) && opNames[op] != "" {
		return opNames[op]
	}
	return "op(" + itoa(int(op)) + ")"
}

// itoa avoids importing strconv into this leaf package for the one place a
// number must be formatted.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// IterAll is OpIterToArray's operand when the whole iterator must be drained,
// which a pattern with a rest element requires.
const IterAll = ^uint32(0)

// A `with` probe packs two things into its A operand: the name, in the low
// bits, and how many of the innermost `with` objects to consult, in the high
// ones. The count is needed because a binding declared between two `with`
// statements is shadowed by the inner one and not by the outer.
const (
	WithLimitShift = 24
	WithNameMask   = 1<<WithLimitShift - 1
	WithLimitMax   = 1<<(32-WithLimitShift) - 1
)
