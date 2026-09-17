package vm

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Runtime holds all state for one JavaScript world: the intern table, the
// global object, the intrinsic prototypes and the interpreter stack.
//
// A Runtime is not safe for concurrent use. Embedding hosts that need
// parallelism create one Runtime per goroutine, which is also what isolates
// untrusted scripts from each other.
type Runtime struct {
	atoms *atomTable

	// global is the global object, and globalEnv is the scope that var and
	// function declarations at the top level bind into.
	global *Object

	// intrinsics holds the prototypes and constructors that the specification
	// requires to exist before any script runs.
	proto intrinsics

	// stack backs every frame's locals and operands. It is allocated once at
	// its full size and never grown, which is what lets an upvalue safely hold
	// a pointer into a live frame's locals: the backing array never moves.
	// Running out of it is exactly "maximum call stack size exceeded".
	stack []Value
	// stackTop is the first unused slot of stack.
	stackTop int
	// stackHigh is the deepest the stack has been used since the last turn
	// ended, which is how much of it endTurn has to clear.
	stackHigh int
	// frames is the call stack. Like the operand stack it is reused across
	// calls, and its length bounds recursion depth.
	frames []frame

	// limits and their accounting.
	maxFrames   int
	memoryLimit int64
	memoryUsed  int64

	// ctx carries cancellation from the embedding host. The interpreter checks
	// it periodically, which is how a timeout or a cancelled request stops a
	// runaway script.
	ctx context.Context
	// interruptCounter counts down to the next cancellation check, so that the
	// check costs one decrement per instruction rather than a context read.
	interruptCounter int

	// globalThis is the global object as a Value, held once because a sloppy
	// call with no receiver substitutes it on every call.
	globalThis Value

	// genFuncProto, asyncFuncProto and asyncGenFuncProto are the intrinsic
	// prototypes of the three kinds of function that are not ordinary.
	genFuncProto      *Object
	asyncFuncProto    *Object
	asyncGenFuncProto *Object

	// typedArrayCtor is %TypedArray%, and typedArrayProtos and typedArrayCtors
	// hold one prototype and one constructor per element type.
	//
	// The constructors are recorded rather than read back from each prototype's
	// constructor property, which a script may redefine: the species protocol
	// falls back to the intrinsic, and an intrinsic that a script can replace
	// is not one.
	typedArrayCtor   *Object
	typedArrayProtos [12]*Object
	typedArrayCtors  [12]*Object

	// promiseCtor is the intrinsic Promise, which the capability machinery
	// compares against to take its fast path.
	promiseCtor *Object

	// throwTypeErrorFn is %ThrowTypeError%, the one function that both reading
	// and writing a restricted property calls. There is exactly one of it per
	// realm, which a script can observe.
	throwTypeErrorFn *Object

	// arrayValuesFn is Array.prototype.values, which the iteration fast paths
	// compare against: an array iterates the way they assume only if this is
	// still what its Symbol.iterator resolves to.
	arrayValuesFn *Object

	// uint8Proto is Uint8Array.prototype, which the base64 conversions need in
	// order to build their results.
	uint8Proto *Object

	// iteratorCtor, helperProto and wrapProto back the iterator helpers.
	iteratorCtor *Object
	helperProto  *Object
	wrapProto    *Object

	// symbolRegistry backs Symbol.for and Symbol.keyFor.
	symbolRegistry map[string]*Symbol

	// wellKnown holds the well-known symbols, which the interpreter consults
	// for iteration, coercion and instanceof.
	wellKnown wellKnownSymbols

	// keptAlive holds the values a WeakRef has handed out during the current
	// job. Two calls to deref in one turn have to answer the same way, so the
	// target cannot be collected between them; the list is released when the
	// job queue drains.
	keptAlive []Value
	// cleanups carries the finalization callbacks the collector has released.
	// It is the boundary between the collector's goroutine and this one.
	cleanups *cleanupQueue
	// registries are the FinalizationRegistry objects, so that retired
	// registrations can be pruned. Held weakly: a registry with live
	// registrations is kept alive by the cleanups it armed, and one with none
	// has nothing left to prune.
	registries []weakTarget
	// onCleanupError reports a finalization callback's failure to the host,
	// since a callback belongs to no script and there is nothing to throw at.
	onCleanupError func(error)

	// microtasks is the promise job queue, drained between turns. Reactions are
	// never run synchronously: that ordering guarantee is what makes a then
	// callback observe a consistent world.
	microtasks []func()

	// rng backs Math.random, created on first use.
	rng *rand.Rand

	// modules maps a resolved specifier to its module, so that importing the
	// same module twice yields the same instance.
	modules      map[string]*Module
	moduleLoader ModuleLoader
	// evalFn is the intrinsic eval, which a call site compares its callee
	// against: a direct eval is one that actually reaches this function, and a
	// name that resolves to anything else is an ordinary call.
	evalFn *Object
	// evaluator compiles and runs source text for eval and the Function
	// constructor. It is nil when code generation is disabled.
	evaluator     Evaluator
	compileModule func(specifier, source string) (*Module, error)

	// arrayBufferProto and typedArrayProto are held here rather than in
	// intrinsics because the typed array constructors are generated in a loop
	// and need to reach them by name.
	arrayBufferProto *Object
	typedArrayProto  *Object

	// clock and timeZone supply Date with the current time and the local zone.
	// They are fields rather than direct calls to the time package so that a
	// host can give a sandboxed script a fixed clock, or none at all.
	clock    func() time.Time
	timeZone *time.Location

	// templateCache keeps the object identity that tagged templates require:
	// the same template site must hand the same strings array to its tag on
	// every evaluation.
	templateCache map[*bytecode.Function][]*Object
}

// intrinsics holds the built-in prototypes and constructors.
type intrinsics struct {
	object         *Object
	function       *Object
	array          *Object
	str            *Object
	number         *Object
	boolean        *Object
	symbol         *Object
	bigint         *Object
	err            *Object
	date           *Object
	regexp         *Object
	mapProto       *Object
	setProto       *Object
	promise        *Object
	generator      *Object
	asyncGenerator *Object
	iterator       *Object
	arrayIter      *Object
	stringIter     *Object
	// regexpStringIter is the prototype matchAll's iterator inherits from.
	regexpStringIter *Object
	// regexpCtor is %RegExp%, the fallback when a species lookup finds none.
	regexpCtor *Object
	// arrayCtor is %Array%, which a species lookup compares against to decide
	// whether a plain array will do.
	arrayCtor *Object

	// nativeErrors are the prototypes of TypeError, RangeError and friends,
	// indexed by errorKind.
	nativeErrors [errorKindCount]*Object
	// errorCtors are the corresponding constructors.
	errorCtors [errorKindCount]*Object
}

// wellKnownSymbols are the symbols the language itself uses.
type wellKnownSymbols struct {
	iterator           *Symbol
	asyncIterator      *Symbol
	hasInstance        *Symbol
	toPrimitive        *Symbol
	toStringTag        *Symbol
	species            *Symbol
	isConcatSpreadable *Symbol
	unscopables        *Symbol
	match              *Symbol
	matchAll           *Symbol
	replace            *Symbol
	search             *Symbol
	split              *Symbol
}

// closure is a function template paired with the upvalues it captured.
type closure struct {
	fn *bytecode.Function
	// upvalues are the captured bindings, shared with the frames that own them
	// until those frames return.
	upvalues []*upvalue
	// names maps the template's name table to atoms, resolved once when the
	// template is first used so that property access needs no interning.
	names []Atom
	// consts caches the materialized constant pool for the same reason.
	consts []Value
	// realm is the runtime the closure belongs to.
	realm *Runtime
	// env is the environment an unqualified name resolves against. It is nil
	// for a script, whose names resolve on the global object, and the module
	// environment for module code -- which inherits from the global object, so
	// the prototype chain performs the scope lookup.
	env *Object
}

// upvalue is a captured variable.
//
// While the owning frame is live the upvalue points into that frame's local
// slice, so reads and writes see the same storage as the owner. When the frame
// returns, the value is copied into the box and the pointer is redirected at
// it, which is what keeps a closure working after its enclosing call has
// finished.
type upvalue struct {
	// slot points at the live location, either into a frame's locals or at
	// closed below.
	slot *Value
	// closed holds the value once the owning frame has returned.
	closed Value
}

func (u *upvalue) get() Value  { return *u.slot }
func (u *upvalue) set(v Value) { *u.slot = v }

// close detaches the upvalue from a frame that is about to return.
func (u *upvalue) close() {
	u.closed = *u.slot
	u.slot = &u.closed
}

// frame is one activation record.
type frame struct {
	cl *closure
	// locals is a window into the runtime's shared local storage.
	locals []Value
	// base is the operand stack offset at which this frame's stack begins.
	base int
	pc   uint32

	this Value
	// thisRef is a derived constructor's `this`, which is a binding rather than
	// a value: it is unbound until super() runs, and reading it before then is
	// a ReferenceError -- which is what stops a subclass from touching an
	// object the base class has not finished building. It is shared with every
	// arrow created inside the constructor, so that binding it is visible
	// through them too. Nil for everything else, which is the common case.
	thisRef   *thisBinding
	newTarget Value
	// callee is the function object being executed, which a named function
	// expression refers to by its own name.
	callee *Object
	// args is the argument list as passed, which `arguments` and the rest
	// parameter both read.
	args []Value

	// paramsOnly runs the frame's parameter prologue and stops there, which is
	// how a generator binds its parameters when it is called rather than on
	// its first resumption.
	paramsOnly bool

	// openUpvalues lists the upvalues that point into this frame's locals and
	// must be closed when it returns.
	openUpvalues []*upvalue

	// withScopes are the objects of the `with` statements this frame is inside,
	// outermost first. Nil for almost every frame: `with` is forbidden in
	// strict mode, so nothing modern has one.
	withScopes []*Object

	// handlers is the exception handler stack for this frame.
	handlers []handler

	// savedSP records the operand stack depth at a suspension, so that a
	// generator saves exactly the live portion of its stack.
	savedSP int

	// native names the Go function for a frame that is executing native code,
	// so that stack traces can show it.
	native string
}

// handler is a registered catch or finally target.
type handler struct {
	pc uint32
	// stackDepth is the operand stack depth to restore before jumping, since an
	// exception can be thrown with a partly-built expression on the stack.
	stackDepth int
	// isFinally marks a handler that must re-throw after running.
	isFinally bool
}

// errorKind enumerates the standard error constructors.
type errorKind uint8

const (
	errError errorKind = iota
	errEval
	errRange
	errReference
	errSyntax
	errType
	errURI
	errAggregate
	errorKindCount
)

var errorKindNames = [errorKindCount]string{
	"Error", "EvalError", "RangeError", "ReferenceError",
	"SyntaxError", "TypeError", "URIError", "AggregateError",
}

// Thrown carries a JavaScript exception through Go's error mechanism.
//
// Native code signals a throw by returning an error. Wrapping the thrown value
// rather than formatting it keeps the original object identity, so that a
// script can catch and inspect exactly what it threw even when the throw passed
// through a Go implementation of a built-in.
type Thrown struct {
	Value Value
	// stack is captured at throw time, because the frames are gone by the time
	// a handler runs.
	Stack []StackEntry
}

// StackEntry is one line of a JavaScript stack trace.
type StackEntry struct {
	Function string
	Source   string
	Line     int32
}

func (t *Thrown) Error() string {
	// Formatting must not call back into the interpreter, because an error may
	// be reported while the runtime is in an inconsistent state. Only the
	// already-materialized message is used.
	if t.Value.IsObject() {
		o := t.Value.Object()
		if p := o.getOwn(atomMessage); p != nil && !p.isAccessor() && p.value.IsString() {
			name := "Error"
			if np := o.getOwn(atomName); np != nil && !np.isAccessor() && np.value.IsString() {
				name = np.value.String().Go()
			} else if o.proto != nil {
				if np := o.proto.getOwn(atomName); np != nil && !np.isAccessor() && np.value.IsString() {
					name = np.value.String().Go()
				}
			}
			return name + ": " + p.value.String().Go()
		}
	}
	if t.Value.IsString() {
		return "Uncaught " + t.Value.String().Go()
	}
	return "Uncaught " + t.Value.Kind().String()
}

// ---------------------------------------------------------------------------
// Throwing
// ---------------------------------------------------------------------------

// throw builds a Thrown for an already-constructed value.
func (r *Runtime) throw(v Value) error {
	return &Thrown{Value: v, Stack: r.captureStack()}
}

// throwError constructs and throws one of the standard error types.
func (r *Runtime) throwError(kind errorKind, format string, args ...any) error {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return r.throw(Obj(r.newError(kind, msg)))
}

func (r *Runtime) throwTypeError(format string, args ...any) error {
	return r.throwError(errType, format, args...)
}

func (r *Runtime) throwRangeError(format string, args ...any) error {
	return r.throwError(errRange, format, args...)
}

func (r *Runtime) throwReferenceError(format string, args ...any) error {
	return r.throwError(errReference, format, args...)
}

func (r *Runtime) throwSyntaxError(format string, args ...any) error {
	return r.throwError(errSyntax, format, args...)
}

// newError builds an error object of the given kind.
func (r *Runtime) newError(kind errorKind, msg string) *Object {
	o := newObject(r.proto.nativeErrors[kind], ClassError)
	o.setOwnRaw(atomMessage, Str(NewString(msg)), propWritable|propConfigurable)
	// The stack is materialized eagerly, because the frames are unwound by the
	// time anything reads it.
	o.setOwnRaw(atomStack, Str(NewString(r.formatStack(msg, kind))), propWritable|propConfigurable)
	return o
}

// captureStack snapshots the current call stack.
func (r *Runtime) captureStack() []StackEntry {
	if len(r.frames) == 0 {
		return nil
	}
	out := make([]StackEntry, 0, len(r.frames))
	for i := len(r.frames) - 1; i >= 0; i-- {
		f := &r.frames[i]
		if f.native != "" {
			out = append(out, StackEntry{Function: f.native, Source: "native"})
			continue
		}
		if f.cl == nil {
			continue
		}
		out = append(out, StackEntry{
			Function: f.cl.fn.Name,
			Source:   f.cl.fn.Source,
			Line:     f.cl.fn.LineAt(f.pc),
		})
	}
	return out
}

// formatStack renders a stack trace in the conventional form.
func (r *Runtime) formatStack(msg string, kind errorKind) string {
	s := errorKindNames[kind]
	if msg != "" {
		s += ": " + msg
	}
	for _, e := range r.captureStack() {
		name := e.Function
		if name == "" {
			name = "<anonymous>"
		}
		s += "\n    at " + name
		if e.Source != "" {
			s += " (" + e.Source
			if e.Line > 0 {
				s += ":" + itoa32(e.Line)
			}
			s += ")"
		}
	}
	return s
}

func itoa32(v int32) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// Resource limits
// ---------------------------------------------------------------------------

// checkInterrupt reports whether the host has cancelled execution.
//
// The context is consulted only every interruptCheckInterval instructions,
// because reading a context's Done channel on every instruction would dominate
// the interpreter loop.
const interruptCheckInterval = 4096

func (r *Runtime) checkInterrupt() error {
	r.interruptCounter--
	if r.interruptCounter > 0 {
		return nil
	}
	return r.checkInterruptNow()
}

// checkInterruptNow performs the actual check and rearms the counter.
//
// It is separate from the decrement so that the interpreter can inline the
// counter into its loop and call this only when it expires. The select is what
// makes it unsuitable for inlining.
func (r *Runtime) checkInterruptNow() error {
	r.interruptCounter = interruptCheckInterval
	if r.ctx == nil {
		return nil
	}
	select {
	case <-r.ctx.Done():
		return r.ctx.Err()
	default:
		return nil
	}
}

// accountMemory charges n bytes against the runtime's budget.
func (r *Runtime) accountMemory(n int64) error {
	if r.memoryLimit <= 0 {
		return nil
	}
	r.memoryUsed += n
	if r.memoryUsed > r.memoryLimit {
		return r.throwError(errRange, "out of memory")
	}
	return nil
}

// thisBinding is a derived constructor's `this`.
type thisBinding struct {
	value Value
	init  bool
}

// thisValue returns the frame's `this`, or reports that it is not yet bound.
func (f *frame) thisValue() (Value, bool) {
	if f.thisRef != nil {
		return f.thisRef.value, f.thisRef.init
	}
	return f.this, true
}
