package vm

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	intl "github.com/go-quickjs/go-intl"
	"github.com/go-quickjs/go-intl/date"
	"github.com/go-quickjs/go-intl/temporal"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Runtime holds all state for one JavaScript world: the intern table, the
// global object, the intrinsic prototypes and the interpreter stack.
//
// A Runtime is not safe for concurrent use. Embedding hosts that need
// parallelism create one Runtime per goroutine, which is also what isolates
// untrusted scripts from each other.
type Runtime struct {
	// Realm is the realm running now: the one the function executing
	// belongs to, or the one the host is evaluating in. Its fields are
	// promoted, so r.proto and r.global are the current realm's.
	*Realm

	atoms *atomTable

	// intlService is the Intl constructor reading its options, which V8's
	// messages about an option name; see enterIntl.
	intlService string
	// temporalLoaded is Temporal's calendars and zones, read when a
	// Temporal value first needs them.
	temporalLoaded *temporal.Data
	// normalizerLoaded is Unicode normalization, read when a string is
	// first normalized or a language's case rules first need it.
	normalizerLoaded *intl.Normalizer

	// usedNewTargetProto records that the built-in constructor now running has
	// read new.target's prototype for itself. It is saved and restored around
	// each one, so a construct nested inside another answers only for itself.
	usedNewTargetProto bool

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
	// frames is the call stack, held in blocks rather than in one array: a
	// frame is large, the depth limit is high, and a runtime that never
	// recurses should not pay for the whole limit up front. The blocks are
	// allocated as the depth grows and kept for later calls, and none of them
	// ever moves -- which is what lets the interpreter hold a *frame across
	// nested calls.
	frames [][]frame
	// frameDepth is how many frames are live, which is also where the next one
	// goes: the block is the depth divided by the block size and the frame is
	// the remainder.
	frameDepth int
	// cur is the block the next frame goes in and frameBase is the depth its
	// first frame sits at, so that a push needs no division.
	cur       []frame
	frameBase int

	// limits and their accounting.
	maxFrames int
	// meter measures the heap against the memory limit, and is nil when
	// there is none.
	meter *memoryMeter
	// nesting is how deep the engine is in recursion the frames do not
	// count; see nest.
	nesting    int
	nodeQuirks bool

	// stackAccessor is the stack property every error has, whose getter and
	// setter are stackGetter and stackSetter, and stackSlot is the key an
	// object that is not an error keeps its frames under, which no script can
	// name. preparingStack is set while Error.prepareStackTrace runs.
	stackSlot      Atom
	preparingStack bool

	// ctx carries cancellation from the embedding host. The interpreter checks
	// it periodically, which is how a timeout or a cancelled request stops a
	// runaway script.
	ctx context.Context
	// abort stops whatever the runtime runs once it is closed, with or
	// without a context in force: it is how a host ends a worker, which may be
	// running a timer's callback rather than anything evaluated.
	abort <-chan struct{}
	// interruptCounter counts down to the next cancellation check, so that the
	// check costs one decrement per instruction rather than a context read.
	interruptCounter int

	// asyncModuleOrder numbers the modules that start waiting, which is the
	// order the ones waiting on them are run in when they finish.
	asyncModuleOrder int

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

	// maybeUnhandled holds the promises rejected with nothing waiting for
	// them, which the end of the turn decides about.
	maybeUnhandled []*Object
	// onUnhandledRejection is what the host wants done about one.
	onUnhandledRejection func(reason Value, promise Value)

	// microtasks is the promise job queue, drained between turns. Reactions are
	// never run synchronously: that ordering guarantee is what makes a then
	// callback observe a consistent world.
	microtasks []job
	// hostJobs is what other goroutines have finished for the runtime.
	hostJobs hostQueue

	// rng backs Math.random, created on first use.
	rng *rand.Rand

	// Host is the embedding API's handle on this runtime, which a Go function
	// called from script is given back.
	Host any

	// moduleLoader fetches a module's source; the modules it makes are each
	// realm's own.
	moduleLoader ModuleLoader
	// onImportMeta fills in a module's import.meta, which is the host's to
	// decide the contents of.
	onImportMeta func(specifier string, meta *Object)

	// evaluator compiles and runs source text for eval and the Function
	// constructor. It is nil when code generation is disabled.
	evaluator     Evaluator
	compileModule func(specifier, source string) (*Module, error)

	// pendingTail is the call a frame ended in, for run to make in its place.
	pendingTail tailCall

	// locale is the language a program means when it does not say which: the
	// one the machine is set to, unless the host chose another.
	locale string
	// clock and timeZone supply Date with the current time and the local zone.
	// They are fields rather than direct calls to the time package so that a
	// host can give a sandboxed script a fixed clock, or none at all. A nil
	// timeZone is the host's.
	clock    func() time.Time
	timeZone *intl.TimeZone
	// dates is local time as Date reckons it, go-intl's Environment, made
	// from the three above when a Date first needs it and made again when
	// one of them changes.
	dates *date.Environment
}

// Realm is a set of intrinsics and the global object that goes with them.
//
// A runtime has one it is made with, and may make more; they share
// everything else -- the stack, the job queue, the symbols. A function
// belongs to the realm it was made in, and runs in it whoever calls it.
type Realm struct {
	// agent is the runtime the realm belongs to.
	agent *Runtime
	// names records the intrinsic prototypes by name, so that one realm's
	// can be found for another's: a constructor whose new.target names no
	// prototype falls back to the one of new.target's realm.
	names []namedIntrinsic

	// modules maps a resolved specifier to its module, so that importing the
	// same module twice in a realm yields the same instance. Each realm has its
	// own: a ShadowRealm that imports a module gets an instance of its own.
	modules map[string]*Module

	// scope and sandbox are set for a context, as node:vm makes them: scope
	// is what its global code resolves names against, a proxy of the global
	// object that forwards to sandbox first. See contextify.go.
	scope   *Object
	sandbox *Object

	// global is the global object, and globalEnv is the scope that var and
	// function declarations at the top level bind into.
	global *Object
	// intlProtos holds the prototypes of the Intl constructors, which are
	// built only if something asks for Intl at all.
	intlProtos map[string]*Object
	// intlFallback is the symbol a formatter made without new is hidden under.
	intlFallback *Symbol
	// temporalDurationProto is retained because Instant difference operations
	// create Duration results after the lazy Temporal namespace has been built.
	temporalNamespace     *Object
	temporalDurationProto *Object
	// PlainDateTime conversions create PlainDate results through the retained
	// intrinsic prototype rather than an observable constructor lookup.
	temporalInstantProto        *Object
	temporalPlainDateProto      *Object
	temporalPlainDateTimeProto  *Object
	temporalPlainMonthDayProto  *Object
	temporalPlainTimeProto      *Object
	temporalPlainYearMonthProto *Object
	temporalZonedDateTimeProto  *Object
	// globalLex holds a script's top-level let, const and class bindings.
	//
	// They are not properties of the global object -- `let x = 1` does not make
	// globalThis.x -- but they outlive the script that declared them and are
	// visible to the next one and to eval, so they need somewhere of their own
	// to live. A binding still in its dead zone is stored uninitialized.
	globalLex *Object
	// intrinsics holds the prototypes and constructors that the specification
	// requires to exist before any script runs.
	proto         intrinsics
	stackAccessor Value
	stackGetter   *Object
	stackSetter   *Object
	// frameless are the built-ins a trace passes over, as V8 runs them
	// without a frame: Function.prototype.call and apply, Reflect.apply and
	// Reflect.construct.
	frameless [4]*Object
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
	// objectToStringFn is Object.prototype.toString, which tells a structured
	// clone how V8 would name an object it refuses.
	objectToStringFn *Object
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
	// funcSlab is what the built-in functions are cut from, so that a realm's
	// hundreds of them are a few dozen allocations rather than one each, and
	// building says whether it is still open. Both are done with once the
	// realm is built: a function made later lives as long as whatever holds
	// it, which is not as long as the realm.
	funcSlab []funcObject
	building bool
	// evalFn is the intrinsic eval, which a call site compares its callee
	// against: a direct eval is one that actually reaches this function, and a
	// name that resolves to anything else is an ordinary call.
	evalFn *Object
	// arrayBufferProto and typedArrayProto are held here rather than in
	// intrinsics because the typed array constructors are generated in a loop
	// and need to reach them by name.
	arrayBufferProto *Object
	// sharedArrayBufferCtor is the intrinsic SharedArrayBuffer, which its
	// slice falls back to when a species gives none.
	sharedArrayBufferCtor *Object
	// abstractModuleSource is %AbstractModuleSource%, which no global names.
	abstractModuleSource *Object
	// legacyRegExp is what RegExp.$1 and the rest describe.
	legacyRegExp legacyRegExpStatics
	// arrayBufferCtor is the intrinsic ArrayBuffer, which slice falls back to
	// when the object names no species of its own.
	arrayBufferCtor *Object
	typedArrayProto *Object
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
	// asyncIterator is %AsyncIteratorPrototype%, which every async iterator
	// inherits from and which exists to carry the one method that makes an
	// async iterator its own iterable.
	asyncIterator *Object
	arrayIter     *Object
	stringIter    *Object
	// mapIter and setIter are the prototypes a Map's and a Set's iterators
	// inherit from, which differ only in the tag they report.
	mapIter *Object
	setIter *Object
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
	// sharedArrayBuffer is SharedArrayBuffer.prototype, which a buffer over
	// memory another runtime shared is made with.
	sharedArrayBuffer *Object
	// callSite is the prototype of the objects Error.prepareStackTrace is
	// given the frames of a stack trace as.
	callSite *Object
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
	dispose            *Symbol
	asyncDispose       *Symbol
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
	// realm is the realm the closure belongs to.
	realm *Realm
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

	// evalVars holds the bindings a direct eval declared in this frame, which
	// have no slot because nothing in the source named them. It is nil for
	// almost every frame: only a sloppy function containing a direct eval gets
	// one, and one created in an enclosing function is its prototype, so a
	// single lookup finds whichever declared the name.
	evalVars *Object

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
	//
	// It is counted from the frame's base rather than from the bottom of the
	// stack, because a generator's frame is rebuilt wherever there is room for
	// it: a handler registered in one resumption is restored in another, at a
	// base that need not be the same.
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
	errSuppressed
	errorKindCount
)

var errorKindNames = [errorKindCount]string{
	"Error", "EvalError", "RangeError", "ReferenceError",
	"SyntaxError", "TypeError", "URIError", "AggregateError", "SuppressedError",
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
	// trace is an error's own stack: the frames it was made in, until its
	// stack property is first read.
	trace stackTrace
}

// StackEntry is one line of a JavaScript stack trace.
type StackEntry struct {
	Function string
	Source   string
	Line     int32
	Column   int32
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
	// An error object already carries the trace, written into its stack
	// property where it was built, so snapshotting the frames again would be
	// the same walk twice -- and most throws are caught by the script, which
	// never looks at either.
	if v.IsObject() && v.Object().class == ClassError {
		if t, ok := v.Object().data.(*Thrown); ok {
			t.Value = v
			t.Stack = t.Stack[:0]
			return t
		}
		return &Thrown{Value: v}
	}
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
	// The message and the stack, and room for a cause: an error is built all
	// at once, so the table is made with it rather than grown twice on the way.
	o := newErrorObject(r.proto.nativeErrors[kind])
	o.setOwnRaw(atomMessage, Str(NewString(msg)), propWritable|propConfigurable)
	// The frames are captured now, because they are unwound by the time
	// anything reads the stack.
	r.attachStack(o, r.captureTrace(nil, false))
	return o
}

// captureStack snapshots the current call stack.
func (r *Runtime) captureStack() []StackEntry {
	if r.frameDepth == 0 {
		return nil
	}
	out := make([]StackEntry, 0, r.frameDepth)
	r.walkStack(func(e StackEntry) { out = append(out, e) })
	return out
}

// walkStack reports each frame of the call stack, innermost first.
func (r *Runtime) walkStack(visit func(StackEntry)) {
	for i := r.frameDepth - 1; i >= 0; i-- {
		f := r.frameAt(i)
		if f.native != "" {
			visit(StackEntry{Function: f.native, Source: "native"})
			continue
		}
		if f.cl == nil {
			continue
		}
		pc := f.pc
		if pc > 0 {
			// The saved pc is past the instruction the frame is at.
			pc--
		}
		line, col := f.cl.fn.PositionAt(pc)
		visit(StackEntry{
			Function: f.cl.fn.Name,
			Source:   f.cl.fn.Source,
			Line:     line,
			Column:   col,
		})
	}
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
	if r.abort != nil {
		select {
		case <-r.abort:
			return context.Canceled
		default:
		}
	}
	if r.ctx != nil {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		default:
		}
	}
	if r.meter != nil {
		return r.checkMemory()
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
