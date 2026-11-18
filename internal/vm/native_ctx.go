package vm

import "unsafe"

// The native handlers' context and frame records, the bases of the
// fields they read relative to it, the call handler's scratch words and
// a dispatcher's type: the layout the handlers' generator reads
// (internal/vm/internal/nativegen, NativeLayout), in every build, so that
// its helper runs on any host. The handlers themselves are amd64's and
// arm64's (zzzyn_native_run.go); every other architecture keeps a regState's
// context nil.

// nativeCtx is what the code reads and writes: ip in, where to start, and
// out, the instruction it left to Go; the top frame's registers, caches,
// constants and code; and the native frame stack, frames, one record for
// each call the code made itself, depth of them. A frame's regState keeps
// one, made at its first native run and kept with the state.
type nativeCtx struct {
	ip uintptr
	// pc and w are, at an exit, the instruction's index in the top frame's
	// code and the instruction itself, which the exit stub writes beside
	// ip so that Go goes on from them at once.
	pc      int64
	w       uint64
	regs    uintptr
	table   uintptr
	ic      uintptr
	budget  int64
	objBits uint64
	tagBase uint64
	one     float64
	nan     uint64
	hole    uint64
	undef   uint64
	trueB   uint64
	falseB  uint64
	// wb is the runtime's write-barrier flag's address, storeRef the
	// routine that stores a pointer with the barrier (nativeStoreRef).
	wb       uintptr
	storeRef uintptr
	// epoch is the address of the runtime's global epoch.
	epoch uintptr
	// codeBase is the top frame's code's first instruction, rc its
	// regCode and nf its frame record, as addresses: what they point to
	// is kept alive elsewhere (the closure, the runtime's frame blocks).
	codeBase uintptr
	rc       uintptr
	nf       uintptr
	depth    int64
	// cc is the top frame's closure's code (closureCode): its constants,
	// global sites' epochs and call sites, read through it, always as Go
	// last made them.
	cc uintptr
	// rt is the address of the runtime's first field the code reads,
	// and ccBase how far a closureCode's is past its start (rtBase,
	// ccBase).
	rt     uintptr
	ccBase uintptr
	// made counts the calls the code made itself (a test's count).
	made int64
	// filled is how many of the frame stack's callers have their pc
	// written (nativeFillPCs); a return below it lowers it.
	filled int64
	// topOK says states[depth-1] is the top frame's, made by nativeTop
	// since the last call or return.
	topOK int64
	// retDone says the base frame has returned (1) or yielded (2), its
	// value in the scratch's sV0 and sV1.
	retDone int64
	// strBits is a string's tag word; typeofs the runtime's strings typeof
	// answers (typeofStrs), nil where it has not made one yet; funcType,
	// proxyType and typedType the type words of a function's, a proxy's
	// and a typed array's internal slots (*funcData, *proxyData,
	// *typedArrayData); typedEpoch the address of the runtime's
	// typedEpoch, which a typed array's view is good for.
	strBits    uint64
	typeofs    uintptr
	funcType   uintptr
	proxyType  uintptr
	typedType  uintptr
	typedEpoch uintptr
	// has is where a Realm has its in cache (Realm.has): an offset from
	// the realm running now, as primProps is, never a realm's address,
	// which a context made in one realm and kept for the next would read
	// once the first was collected.
	has uintptr
	// bigHigh is shortBigHigh's address, which a short BigInt's ref
	// points into.
	bigHigh  uintptr
	bigHighX uintptr
	// null and uninit are null's and the uninitialized binding's words,
	// and emptyStr the empty string's.
	null     uint64
	uninit   uint64
	emptyStr uintptr
	// setterNext is the transition a site's entry has for a setter's
	// write, which is no addition.
	setterNext uintptr
	// polyEntries is where a site's propPoly has its entries, which the
	// JIT's tag moves.
	polyEntries uintptr
	// frames is the native frame stack's first record (recs), and cap
	// how many it has room for: frames[d-1] is the caller's state that
	// frame d returns to. A call past cap exits; Go grows the stack
	// (nativeGrow) up to nativeMaxDepth, and past it makes the call
	// itself, in a code entry of its own.
	frames uintptr
	cap    int64
	// scratch is what a handler keeps across a store by storeRef, which
	// keeps no register but DI.
	scratch [16]uint64
	// The exit's call of Go (nativeCallH): the Go table's and the slow
	// table's addresses, the trampoline's, where the code goes on after
	// the call, the exits' counters' address where a test counts, and
	// ended, set where a handler ended the top frame, which the code
	// leaves to nativeRun.
	goTable   uintptr
	slowTable uintptr
	callH     uintptr
	after     uintptr
	cntExits  uintptr
	ended     int64
	// protoStr, protoNum and primProps are where a Realm has its string
	// and number prototypes and its cache of their properties (primProps),
	// which the JIT's tag moves: offsets from the realm running now.
	protoStr  uintptr
	protoNum  uintptr
	primProps uintptr
	// closureFn is the func value of nativeClosure, which closure_w's
	// handler calls through callH with no exit.
	closureFn uintptr
	// litFn is the func value of nativeLit, which new_object_lit's and
	// new_array_e's handler calls through callH with no exit.
	litFn uintptr
	// builtinFn is the func value of nativeBuiltin, which the call
	// handler calls through callH for a built-in its site cached.
	builtinFn uintptr
	// iterResFn is the func value of nativeIterRes, which a generator's
	// step that next() entered calls for its result object.
	iterResFn uintptr
	// callMissFn is the func value of nativeCallMiss, which the call
	// handler calls for a callee other than its site cached.
	callMissFn uintptr
	// jitCallFn is the func value of nativeJITCall, which the call handler
	// calls through callH with no exit for a callee whose calls go to the
	// JIT (closureCode's jitEnters).
	jitCallFn uintptr
	// miss asks the exit stub for nativeCallMiss rather than the
	// instruction's Go handler: the call handler's callee other than its
	// site cached.
	miss uintptr
	// fv is the top frame's feedback vector, a slot a site into which a
	// handler ORs the kinds of value it met (the JIT's next step, as
	// Ignition's): none yet, its room kept here and in the frame record,
	// which a call switches and a return gives back, and R14, which no
	// handler uses, kept for it to be pinned in, so that a handler's note
	// is one OR to memory and spills nothing.
	fv uintptr

	// What only Go reads: the frame's state, the records frames points
	// into, and the states of the frames above the base frame, made when
	// an exit leaves one of them to Go (nativeTop).
	st     *regState
	recs   []nativeRec
	states []*regState
	// top is the top frame's state while topOK says so (nativeTop).
	top *regState
	// disp is the dispatcher whose table is table (nativeDispatch).
	disp *nativeTable
	// used is how many of states the run has given a frame, the deepest
	// depth nativeTop or the exit stub (makeTop) has made a state for:
	// what nativeLeave clears, so
	// that a context whose stack a deep recursion grew costs a callback's
	// entry no more than one never grown.
	used int64

	// newFn is the func value of nativeNew, which new_w's handler calls
	// through callH with no exit for the object it constructs: past the
	// words the code read before it, which it moves none of.
	newFn uintptr
	// hasInst is where a Realm has its Function.prototype[Symbol.hasInstance]
	// (hasInstanceFn): an offset from the realm running now, as has is.
	hasInst uintptr
	// superFn is the func value of nativeSuper, which super_call's and
	// super_forward's handler calls through callH with no exit, and super
	// the call cache entry it makes the parent constructor's, which the
	// handler enters as call_w enters its site's (nativeReset clears it).
	superFn uintptr
	super   regCallCache
	// thisRef is the this binding nativeNew or nativeSuper made last, which
	// the handler gives the frame it enters by the scratch's word: held
	// here too, so that it is made on the heap, as only a pointer the
	// collector sees can make it, and lives until the frame has it.
	thisRef *thisBinding
	// callFn is where a Realm has its Function.prototype.call (callFn): an
	// offset from the realm running now, as has is.
	callFn uintptr
	// applyFn is the func value of nativeApply, which apply_args's
	// handler calls through callH with no exit.
	applyFn uintptr
	// reflectFn is the func value of nativeReflect, which the call
	// handler calls through callH with no exit for a call of the site's
	// built-in Reflect.construct, and reflect where a Realm has it
	// (reflectConstructFn), an offset from the realm running now.
	reflectFn uintptr
	reflect   uintptr
	// applyAt is where a Realm has its Function.prototype.apply
	// (applyFn), an offset from the realm running now, as callFn is.
	applyAt uintptr
	// fwdArgs is where a construction through this.k.call(this, ...)
	// lays the arguments it hands on for the stores its method makes
	// (the native initializer): words the collector does not read, for
	// values the forwarder's window or code holds while they are here.
	fwdArgs [2 * 8]uint64
}

// nativeTypedCtx is a typed code entry's context (zzzyn_native_typed.go):
// a nativeCtx, and past it the entry's heap -- its base and its length in
// bytes, which the typed handlers read, as of the runtime's typedEpoch,
// and the heap they are of, the entry's code's, nil where it has none --
// so that every other context stays the size it was.
type nativeTypedCtx struct {
	nativeCtx
	heapBase uintptr
	heapLen  uintptr
	heap     *asmHeap
}

// typedCtx is ctx's nativeTypedCtx, ctx a typed entry's.
func (ctx *nativeCtx) typedCtx() *nativeTypedCtx { return (*nativeTypedCtx)(unsafe.Pointer(ctx)) }

// nativeMaxDepth is how deep the native frame stack goes.
const nativeMaxDepth = 128

// nativeRec is a native frame's record of its caller: the call's
// instruction, the caller's top-frame state, the stack top to give back
// and where the value goes (B times 16, or a for-of's step's A); and for a
// generator's step the code entered (regFrameGenStep), the generator,
// which the caller's cursor keeps alive.
type nativeRec struct {
	retIP    uintptr
	regs     uintptr
	ic       uintptr
	codeBase uintptr
	rc       uintptr
	nf       uintptr
	cc       uintptr
	saved    int64
	bOff     int64
	fv       uintptr
	gen      uintptr
}

// The Runtime's and a closureCode's fields the code reads are at offsets
// that the JIT's tag changes: the context's rt and cc point at the first
// of them (rtBase, ccBase past the start), and the code reads them
// relative to it, which no tag changes, so that one block serves every
// build (TestRegNativeGenerated, under each tag, checks it is the same).
var (
	rtBase = unsafe.Offsetof(Runtime{}.stack)
	ccBase = min(unsafe.Offsetof(closureCode{}.ic), unsafe.Offsetof(closureCode{}.consts),
		unsafe.Offsetof(closureCode{}.regEpochs), unsafe.Offsetof(closureCode{}.regCalls))
)

// The call handler's scratch words.
const (
	sE = iota
	sWin
	sKind
	sRC
	sArgc
	sBase
	sTop
	sNF
	sSaved
	sI
	sEnd
	sV0
	sV1
	// sEnt says the callee is entered as regEnterW enters it, keeping its
	// arguments, which start at sKa on the argument stack.
	sEnt
	sKa
)

// A construction's and a super()'s entry (nativeNew, nativeSuper) are
// given to the call handler's entry in two of its words it does not use:
// the this binding of a derived class's frame, or 0, and super()'s
// new.target, an object.
const (
	sThisRef = sV0
	sNewTgt  = sV1
)

// nativeTable is a native dispatcher (internal/vm/internal/nativegen,
// "Variants"): its table's address, which a context gives the code
// (ctx.table), and which operations have a handler in it, every other's
// entry the exit stub's.
type nativeTable struct {
	addr uintptr
	has  [256]bool
}
