// Package bytecode defines the compiled function representation the
// compiler makes and the virtual machine runs: a Function's tables, and its
// register code (reg.go), 64-bit instructions over a frame's registers.
//
// Op is the vocabulary those instructions and the runtime share for an
// operation: an operator a register instruction carries in its X (RBin,
// RUnary), what a call of Go from native code does (the JIT's GoOp), the
// kind of a site or of a top-level declaration. It is no instruction set of
// its own.
package bytecode

import (
	"strconv"
	"strings"
)

// Op is an operation, of the vocabulary register code and the runtime share.
type Op uint8

const (
	// OpNop is no operation.
	OpNop Op = iota

	// --- Declarations -----------------------------------------------------
	// Top-level code's declarations, as the walk records them for the
	// register compiler (regcomp's TopOp). OpSetUpvalue is eval code's
	// function declaration of a name its caller binds as a var.
	OpSetUpvalue
	OpDefineGlobalVar  // var or function declaration on the global object
	OpDefineGlobalFunc // like the above but always overwrites
	// OpCheckGlobalLex is a top-level let, const or class, whose name may
	// not be a property of the global object that cannot be removed.
	OpCheckGlobalLex
	// OpCheckGlobalVar is a top-level var or function declaration, whose
	// name a lexical binding may not have.
	OpCheckGlobalVar
	// OpDeclareGlobalLex creates a script-level lexical binding, in its dead
	// zone.
	OpDeclareGlobalLex
	// OpDeclareModuleLex creates a module's top-level lexical binding in its
	// dead zone: a property of the module environment rather than a slot,
	// because what a module exports has to be something the linker can
	// forward to.
	OpDeclareModuleLex

	// --- Properties -------------------------------------------------------
	// A site's kind (regcomp's propSite) and a call of Go's operation.
	OpGetProp // obj[name]
	OpSetProp // obj[name] = value
	// OpGetPropThis reads a method, for a call with the object its receiver.
	OpGetPropThis
	OpDefineField   // define an own data property, ignoring setters
	OpDefineIndex   // as above with a computed key
	OpSetProtoOf    // set __proto__ from an object literal
	OpCopyDataProps // object spread: copy own enumerable properties

	// --- Private class members -------------------------------------------
	// A private method, getter or setter added to the list an instance of a
	// class carries (the VM's addPrivateMethod).
	OpAddPrivateMethod
	OpAddPrivateGetter
	OpAddPrivateSetter

	// OpObjectRest builds an object holding the source's own enumerable
	// properties except the A keys given, for the rest element of an object
	// pattern.
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
	OpInstanceOf
	OpNot
	OpTypeOf

	// --- Calls ------------------------------------------------------------
	OpCall       // A arguments
	OpCallMethod // A arguments, with a receiver
	OpNew        // A arguments
	// OpCallSpread and OpNewSpread take their arguments from an array, which
	// is how a call containing a spread element is made.
	OpCallSpread
	OpNewSpread
	// OpSuperCall is super(...), of A arguments.
	OpSuperCall

	// --- Construction -----------------------------------------------------
	OpArrayPush      // append to an array
	OpArraySpread    // spread an iterable into an array
	OpTemplateObject // the frozen strings array of Templates[A]

	// --- Iteration --------------------------------------------------------
	OpForInStart
	OpForOfStart
	OpIterNextOrJump // advance a for-of's iterator
	OpSpreadIter     // spread an iterable for a call
	// OpIterStep is a destructuring pattern's next value, or undefined once
	// the iterator is exhausted, and OpIterRest everything left as an array.
	OpIterStep
	OpIterRest

	// --- Miscellaneous ----------------------------------------------------
	// OpPushCallee is the function object running, which is how a named
	// function expression refers to itself.
	OpPushCallee
	OpToPropertyKey
	OpToNumber
	// OpToNumeric is OpToNumber except that a BigInt stays one, which is what
	// the update operators need: `1n++` is 2n, not an error.
	OpToNumeric

	// --- `with` -----------------------------------------------------------
	// The probes a name inside a `with` body is compiled with (regcomp's
	// WithProbe): each answers from the enclosing `with` objects, or the
	// static code runs.
	OpWithGet     // read the value
	OpWithGetThis // read the value and the object, for a call
	OpWithSet     // store a value
	OpWithDelete  // delete the binding
	OpWithTypeof  // the type of the value
	// OpWithGetUnder is the read of a reference that is read and then written
	// back -- a compound assignment, an update: the name is resolved once, by
	// the read, and the write goes to whatever the read found.
	OpWithGetUnder
	// OpWithResolve resolves a name against the `with` objects without
	// reading it, which is what a plain assignment needs.
	OpWithResolve

	// OpApplyArguments is f.apply(x, arguments) in a function whose arguments
	// object is made only on demand: f is called with the arguments
	// themselves where apply is the built-in.
	OpApplyArguments

	// opCount is the number of opcodes, used to size the name table.
	opCount
)

// The phases of an import register code's import names.
const (
	ImportEvaluate = iota // import(): the module, evaluated
	ImportDefer           // import.defer(): its deferred namespace
	ImportSource          // import.source(): its source
)

// opNames gives each opcode a readable name for disassembly and panics.
var opNames = [opCount]string{
	OpNop: "nop", OpSetUpvalue: "set_upvalue",
	OpDefineGlobalVar: "define_global_var", OpDefineGlobalFunc: "define_global_func",
	OpCheckGlobalLex: "check_global_lex", OpCheckGlobalVar: "check_global_var",
	OpDeclareGlobalLex: "declare_global_lex", OpDeclareModuleLex: "declare_module_lex",
	OpGetProp: "get_prop", OpSetProp: "set_prop", OpGetPropThis: "get_prop_this",
	OpDefineField: "define_field", OpDefineIndex: "define_index",
	OpSetProtoOf: "set_proto_of", OpCopyDataProps: "copy_data_props",
	OpAddPrivateMethod: "add_private_method", OpAddPrivateGetter: "add_private_getter",
	OpAddPrivateSetter: "add_private_setter", OpObjectRest: "object_rest",
	OpAdd: "add", OpSub: "sub", OpMul: "mul", OpDiv: "div", OpMod: "mod", OpPow: "pow",
	OpNeg: "neg", OpPos: "pos",
	OpBitAnd: "bit_and", OpBitOr: "bit_or", OpBitXor: "bit_xor", OpBitNot: "bit_not",
	OpShl: "shl", OpShr: "shr", OpUShr: "ushr",
	OpEq: "eq", OpNe: "ne", OpStrictEq: "strict_eq", OpStrictNe: "strict_ne",
	OpLt: "lt", OpLe: "le", OpGt: "gt", OpGe: "ge",
	OpInstanceOf: "instanceof", OpNot: "not", OpTypeOf: "typeof",
	OpCall: "call", OpCallMethod: "call_method", OpNew: "new",
	OpCallSpread: "call_spread", OpNewSpread: "new_spread", OpSuperCall: "super_call",
	OpArrayPush: "array_push", OpArraySpread: "array_spread", OpTemplateObject: "template_object",
	OpForInStart: "for_in_start", OpForOfStart: "for_of_start",
	OpIterNextOrJump: "iter_next_or_jump", OpSpreadIter: "spread_iter",
	OpIterStep: "iter_step", OpIterRest: "iter_rest",
	OpPushCallee: "push_callee", OpToPropertyKey: "to_property_key",
	OpToNumber: "to_number", OpToNumeric: "to_numeric",
	OpWithGet: "with_get", OpWithGetThis: "with_get_this", OpWithSet: "with_set",
	OpWithDelete: "with_delete", OpWithTypeof: "with_typeof",
	OpWithGetUnder: "with_get_under", OpWithResolve: "with_resolve",
	OpApplyArguments: "apply_arguments",
}

func (op Op) String() string {
	if int(op) < len(opNames) && opNames[op] != "" {
		return opNames[op]
	}
	return "op(" + strconv.Itoa(int(op)) + ")"
}

// WithLimitMax is the most `with` scopes a probe consults (regcomp's
// WithProbe Limit): a binding declared between two `with` statements is
// shadowed by the inner one and not by the outer.
const WithLimitMax = 1<<8 - 1

// The ways a suspended generator can be resumed. A delegating yield keeps the
// kind it was resumed with in a binding of its own, so that the code after the
// delegation can tell a return from an ordinary next -- which is the difference
// between the outer generator returning and it carrying on.
const (
	ResumeNext = iota
	ResumeThrow
	ResumeReturn
)

// The flags of a disposal (RDispose, RDisposeStep).
//
// DisposeStep, in B, makes it one step of an await using scope's
// disposal instead, the awaits between them the compiler's own: the
// capability is popped, and the disposers are run until one leaves a value
// to await, which is pushed, or none is left, when it jumps to A. With
// DisposeStart it is the first step, which reads a finally clause's
// record beneath the capability where DisposeRecord says there is one;
// with DisposeFail the await before it threw what lies beneath the
// capability, which is popped and folded into the completion; with
// DisposeFinish it is the end, which throws what the disposers threw, and
// jumps nowhere.
const (
	DisposeRecord = 1 << iota
	DisposeAsync
	DisposeStep
	DisposeStart
	DisposeFail
	DisposeFinish
)

// ModuleRequest names a module a module asks for: its specifier and, when its
// import attributes ask for one, the type of module it must be. A JSON module
// and a JavaScript one loaded from the same place are two modules, so the type
// is part of the name.
func ModuleRequest(specifier, typ string) string {
	if typ == "" {
		return specifier
	}
	return specifier + "\x00" + typ
}

// SplitModuleRequest takes a ModuleRequest apart.
func SplitModuleRequest(request string) (specifier, typ string) {
	for i := len(request) - 1; i >= 0; i-- {
		if request[i] == 0 {
			return request[:i], request[i+1:]
		}
	}
	return request, ""
}

// DeferRequest marks a module request as `import defer`'s: the module is
// loaded and linked with the rest, but not evaluated until its namespace is
// first used. It is the same module as one requested without the mark.
func DeferRequest(request string) string { return "\x01" + request }

// SplitDeferRequest takes the mark off a request, reporting whether it had it.
func SplitDeferRequest(request string) (string, bool) {
	if len(request) > 0 && request[0] == 1 {
		return request[1:], true
	}
	return request, false
}

// ReExportLocal names the binding `export {imported} from request` imports
// under: a name no identifier can spell, which says which module and which of
// its exports it stands for. The request's length goes first, because a
// specifier, like an export name, may hold any character: `export {f} from
// "demo:dep"` and `export {"dep:f"} from "demo"` are different re-exports.
func ReExportLocal(request, imported string) string {
	return "*re*" + strconv.Itoa(len(request)) + ":" + request + imported
}

// SplitReExportLocal takes a name ReExportLocal made apart, reporting whether
// it was one.
func SplitReExportLocal(local string) (request, imported string, ok bool) {
	rest, ok := strings.CutPrefix(local, "*re*")
	if !ok {
		return "", "", false
	}
	size, rest, ok := strings.Cut(rest, ":")
	if !ok {
		return "", "", false
	}
	n, err := strconv.Atoi(size)
	if err != nil || n < 0 || n > len(rest) {
		return "", "", false
	}
	return rest[:n], rest[n:], true
}

// SourceRequest marks a module request as `import source`'s, which asks for
// the module's source object rather than its instance: the module is loaded,
// but neither linked nor evaluated.
func SourceRequest(request string) string { return "\x02" + request }

// SplitSourceRequest takes the mark off a request, reporting whether it had
// it.
func SplitSourceRequest(request string) (string, bool) {
	if len(request) > 0 && request[0] == 2 {
		return request[1:], true
	}
	return request, false
}
