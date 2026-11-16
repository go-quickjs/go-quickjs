package vm

import (
	"math"
	"os"
	"sync/atomic"
	"unsafe"
	_ "unsafe" // linkname

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register interpreter's native threaded handlers on amd64
// (docs/register-interpreter.md, "Native handlers"): machine code for the
// hot operations, each handler ending in its own indirect jump through
// the native table, as V8's Ignition's do. What a handler's fast path
// does not answer -- an operation with no native handler, a value of
// another kind, a cache that missed, a call Go makes -- goes to the
// shared exit stub, which returns here: nativeRun runs the instruction by
// its Go handler from regTable (and its slow function), as runReg would,
// and enters the code again after it.
//
// The code is made offline by nativeEmit (zzzyn_native_gen_amd64_test.go)
// and assembled into the binary (zzzyn_native_handlers_amd64.s, go
// generate): one block for every runtime and every build tag, never
// patched, every address it needs given by the context in DI.
//
// The machine registers, kept across the dispatch:
//
//	RBX  the instruction's address        R12  the frame's registers
//	R13  the native table                 R15  the closure's caches (ic)
//	R11  the back-edge budget             R10  tagBase, the tags' least
//	RDI  the context (nativeCtx)
//
// and, in a handler, RAX the instruction word, RCX, RDX, RSI, R8, R9,
// X0, X1 and X3 its scratch. A pointer word is stored plainly while the
// collector does not mark and by the barrier's routine (nativeStoreRef)
// while it does: the same work marking or not but the barrier's, and no
// call of Go for the marking's sake (the JIT's marking rule).

// regNativeOn runs register frames by the native handlers. QJS_NONATIVE
// set in the environment, read once, leaves them to the Go table's loop,
// to compare the two and as an escape hatch. The quickjs_verify tag's
// shape checks are the Go handlers' alone.
var regNativeOn = os.Getenv("QJS_NONATIVE") == "" && !verifyShapes

// SetRegNative runs register frames by the native handlers or not, and
// reports whether they were: a test's switch, as QJS_NONATIVE is the
// environment's. The frames a runtime runs meanwhile run as they began.
func SetRegNative(on bool) bool {
	was := regNativeOn
	regNativeOn = on && !verifyShapes
	return was
}

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
	// retDone says the base frame has returned, its value in the
	// scratch's sV0 and sV1.
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
	// has is the address of the runtime's in cache (Runtime.has).
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
}

// nativeMaxDepth is how deep the native frame stack goes.
const nativeMaxDepth = 128

// nativeRec is a native frame's record of its caller: the call's
// instruction, the caller's top-frame state, the stack top to give back
// and where the value goes (B times 16).
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

// The counters and switches of the tests: process-wide, off but in a
// test (nativeCounting, nativeMarking).
var (
	// nativeCount turns the counters on.
	nativeCount bool
	// nativeEnters counts the code's entries: one, and one after each
	// exit to Go.
	nativeEnters int64
	// nativeExits counts the exits to Go by operation.
	nativeExits [256]int64
	// nativeCalls counts the calls the code made itself.
	nativeCalls int64
	// nativeMarkFlag, set by a test, is the flag the contexts made after
	// it read for the collector's, so that the code stores as it does
	// while the collector marks.
	nativeMarkFlag atomic.Pointer[uint8]
)

// nativeRun runs frame f as register code c, from instruction start, by
// the native handlers: runReg's work, the instructions the code leaves run
// by their Go handlers.
func (r *Runtime) nativeRun(f *frame, c *regCode, start int) (Value, error) {
	st := r.regEnter(f, c)
	ctx := st.native
	if ctx == nil {
		ctx = r.newNativeCtx(st)
	}
	ctx.epoch = uintptr(unsafe.Pointer(&r.shapes.globalEpoch))
	ctx.depth, ctx.filled = 0, 0
	nativeSetTop(ctx, uintptr(unsafe.Pointer(st.regs)), f, c)
	ctx.ip = uintptr(c.code) + uintptr(start)*8
	ctx.top, ctx.topOK = st, 1
	ctx.cntExits = 0
	if nativeCount {
		ctx.cntExits = uintptr(unsafe.Pointer(&nativeExits[0]))
	}
	// restarted says a tail call has given the frame to its callee in
	// place and the code goes on with the callee's (nativeRestart).
	restarted := false
	for {
		ctx.budget = int64(r.backEdges)
		if nativeCount {
			atomic.AddInt64(&nativeEnters, 1)
		}
		nativeEnter(nativeBase, unsafe.Pointer(ctx))
		r.backEdges = int(ctx.budget)
		if ctx.retDone != 0 {
			// The base frame returned: its value in the scratch.
			ctx.retDone = 0
			v := Value{num: math.Float64frombits(ctx.scratch[sV0]), ref: *(*unsafe.Pointer)(unsafe.Pointer(&ctx.scratch[sV1]))}
			ctx.scratch[sV1] = 0
			return r.nativeFinish(st, ctx, v, restarted)
		}
		s := ctx.top
		if ctx.topOK == 0 {
			s = r.nativeTop(st, ctx)
		}
		if ctx.ended != 0 {
			// A Go handler the code called ended the top frame (regDone).
			ctx.ended = 0
			if v, done := r.nativeEnd(s, ctx, regDone, regAt(s.c.code, int(ctx.pc))); done {
				if st.err == errRegRestart && r.nativeRestart(st, ctx) {
					restarted = true
					continue
				}
				return r.nativeFinish(st, ctx, v, restarted)
			}
			continue
		}
		// The instruction the code left, run by its Go handler in the top
		// frame: only a frame's end goes further.
		pc, w := int(ctx.pc), bytecode.RInstr(ctx.w)
		op := w.Op()
		if nativeCount {
			atomic.AddInt64(&nativeExits[op], 1)
		}
		if ctx.depth == ctx.cap && ctx.cap < nativeMaxDepth && (op == bytecode.RCallW || op == bytecode.RCallMW || op == bytecode.RCallWT || op == bytecode.RCallMWT) {
			// A call the frame stack has no room for: the stack grown,
			// the call tried again, nothing of it done.
			ctx.grow()
			continue
		}
		pc = regTable[op](s.regs, w, pc, s)
		// The instructions after it that have no native handler are run
		// here too, rather than each entering the code to exit at once.
		code := s.c.code
		for pc >= 0 {
			w = regAt(code, pc)
			op = w.Op()
			if nativeHas[op] {
				break
			}
			if nativeCount {
				atomic.AddInt64(&nativeExits[op], 1)
			}
			pc = regTable[op](s.regs, w, pc, s)
		}
		if pc >= 0 {
			ctx.ip = uintptr(code) + uintptr(pc)*8
			continue
		}
		if v, done := r.nativeEnd(s, ctx, pc, w); done {
			if st.err == errRegRestart && r.nativeRestart(st, ctx) {
				restarted = true
				continue
			}
			return r.nativeFinish(st, ctx, v, restarted)
		}
	}
}

// nativeRestart goes on with the base frame given to a callee in place by
// a tail call (errRegRestart, regTailTo), as regRestartRun runs it again
// -- the interrupt counted, the code r.regRestart, from its first
// instruction -- but in this code entry, rather than in one of its own
// for each call. It reports false where the frame ends instead, with the
// error in st.
func (r *Runtime) nativeRestart(st *regState, ctx *nativeCtx) bool {
	rc := r.regRestart
	r.regRestart = nil
	if r.interruptCounter--; r.interruptCounter <= 0 {
		if err := r.checkInterruptNow(); err != nil {
			st.err = err
			return false
		}
	}
	if r.stopped != nil {
		st.err = r.stopped
		return false
	}
	f := st.f
	st.err = nil
	st.c, st.cl = rc, f.cl
	st.wb = f.base - int(rc.locals)
	st.regs = regWindow(r.stack, st.wb)
	nativeSetTop(ctx, uintptr(unsafe.Pointer(st.regs)), f, rc)
	ctx.ip = uintptr(rc.code)
	ctx.top, ctx.topOK = st, 1
	return true
}

// nativeFinish ends nativeRun with v or st's error. A frame given to
// callees in place closes the captures of the last one's code, as
// regRestartRun closes them: its caller closes only the first's.
func (r *Runtime) nativeFinish(st *regState, ctx *nativeCtx, v Value, restarted bool) (Value, error) {
	if restarted && len(st.f.openUpvalues) != 0 {
		regCloseUpvalues(st.f)
	}
	err := st.err
	r.nativeLeave(st, ctx)
	if err != nil {
		return Undefined, err
	}
	return v, nil
}

// newNativeCtx makes st's context, its words that are the runtime's and
// the build's set once.
func (r *Runtime) newNativeCtx(st *regState) *nativeCtx {
	ctx := new(nativeCtx)
	ctx.st = st
	ctx.table = nativeTableAddr()
	ctx.objBits = objectBits
	ctx.tagBase = tagBase
	ctx.one = 1
	ctx.nan = canonicalNaN
	ctx.hole = holeBits
	ctx.undef = math.Float64bits(Undefined.num)
	ctx.trueB, ctx.falseB = trueBits, falseBits
	ctx.storeRef = nativeStoreRefAddr()
	ctx.ccBase = ccBase
	ctx.strBits = math.Float64bits(mkTag(KindString, 0))
	ctx.funcType = nativeTypeWord((*funcData)(nil))
	ctx.proxyType = nativeTypeWord((*proxyData)(nil))
	ctx.typedType = nativeTypeWord((*typedArrayData)(nil))
	ctx.bigHigh = uintptr(unsafe.Pointer(&shortBigHigh[0]))
	ctx.bigHighX = uintptr(unsafe.Pointer(&shortBigHighX[0]))
	ctx.null = math.Float64bits(Null.num)
	ctx.uninit = math.Float64bits(uninitialized.num)
	ctx.emptyStr = uintptr(unsafe.Pointer(emptyString))
	ctx.setterNext = uintptr(unsafe.Pointer(setterNext))
	ctx.polyEntries = unsafe.Offsetof(propPoly{}.entries)
	// The runtime's: a regState is of one runtime's list.
	ctx.rt = uintptr(unsafe.Pointer(r)) + rtBase
	ctx.wb = uintptr(unsafe.Pointer(&nativeWB.enabled))
	if m := nativeMarkFlag.Load(); m != nil {
		ctx.wb = uintptr(unsafe.Pointer(m))
	}
	ctx.typeofs = uintptr(unsafe.Pointer(&r.typeofStrs[0]))
	ctx.typedEpoch = uintptr(unsafe.Pointer(&r.typedEpoch))
	ctx.has = uintptr(unsafe.Pointer(&r.has))
	ctx.goTable = uintptr(unsafe.Pointer(&regTable[0]))
	ctx.slowTable = uintptr(unsafe.Pointer(&regSlowTable[0]))
	ctx.callH = nativeCallHAddr()
	ctx.protoStr = unsafe.Offsetof(Realm{}.proto) + unsafe.Offsetof(intrinsics{}.str)
	ctx.protoNum = unsafe.Offsetof(Realm{}.proto) + unsafe.Offsetof(intrinsics{}.number)
	ctx.primProps = unsafe.Offsetof(Realm{}.primProps)
	ctx.grow()
	st.native = ctx
	return ctx
}

// grow gives the native frame stack twice the room, 8 records at first,
// up to nativeMaxDepth.
func (ctx *nativeCtx) grow() {
	n := min(max(2*len(ctx.recs), 8), nativeMaxDepth)
	recs := make([]nativeRec, n)
	copy(recs, ctx.recs)
	states := make([]*regState, n)
	copy(states, ctx.states)
	ctx.recs, ctx.states = recs, states
	ctx.frames = uintptr(unsafe.Pointer(&recs[0]))
	ctx.cap = int64(n)
}

// nativeLeave ends nativeRun once the base frame has returned or thrown:
// the states made for the frames above it hold nothing of theirs.
func (r *Runtime) nativeLeave(st *regState, ctx *nativeCtx) {
	if nativeCount {
		atomic.AddInt64(&nativeCalls, ctx.made)
	}
	ctx.made = 0
	ctx.top = nil
	for _, s := range ctx.states {
		if s == nil {
			break
		}
		s.f, s.c, s.cl, s.err = nil, nil, nil, nil
	}
	r.regLeave(st)
}

// nativeTypeWord is the type word of an interface holding v's dynamic
// type, which native code compares an internal slots' with.
func nativeTypeWord(v any) uintptr { return (*[2]uintptr)(unsafe.Pointer(&v))[0] }

// nativeSetTop makes frame f, running c with its registers at regs, the
// context's top frame.
func nativeSetTop(ctx *nativeCtx, regs uintptr, f *frame, c *regCode) {
	cl := f.cl
	ctx.regs = regs
	ctx.ic = 0
	if len(cl.ic) > 0 {
		ctx.ic = uintptr(unsafe.Pointer(&cl.ic[0]))
	}
	ctx.codeBase = uintptr(c.code)
	ctx.rc = uintptr(unsafe.Pointer(c))
	ctx.nf = uintptr(unsafe.Pointer(f))
	ctx.cc = uintptr(unsafe.Pointer(cl.closureCode)) + ccBase
	ctx.fv = 0
}

// nativeTop is the top frame's state, depth above the base frame: one
// made from the native frame stack's top, the frame record, its register
// code and its window, for Go to run an instruction of it as runReg would.
// The one made is kept while the top frame is the same (topOK, which a
// call and a return of the code's and Go's clear), and the callers' pcs
// are written first (nativeFillPCs).
//
//go:nocheckptr
func (r *Runtime) nativeTop(st0 *regState, ctx *nativeCtx) *regState {
	d := ctx.depth
	if d == 0 {
		ctx.top, ctx.topOK = st0, 1
		return st0
	}
	if ctx.filled < d {
		nativeFillPCs(ctx)
	}
	s := ctx.states[d-1]
	if ctx.topOK != 0 {
		ctx.top = s
		return s
	}
	if s == nil {
		s = &regState{r: r}
		ctx.states[d-1] = s
	}
	nf := (*frame)(nativePtr(&ctx.nf))
	rc := (*regCode)(nativePtr(&ctx.rc))
	s.f, s.c, s.cl = nf, rc, nf.cl
	s.wb = nf.base - int(rc.locals)
	s.regs = regWindow(r.stack, s.wb)
	s.err = nil
	ctx.top = s
	ctx.topOK = 1
	return s
}

// nativeEnd goes on where an exit's handler left the top frame s, pc
// negative: the operation's slow function run, and the frame's end, a
// native frame's return or throw its caller's, as rhCallW's would be
// after runReg. It reports whether the base frame has returned or
// thrown, with what it returns (its error in its state).
func (r *Runtime) nativeEnd(s *regState, ctx *nativeCtx, pc int, w bytecode.RInstr) (Value, bool) {
	if pc != regDone {
		pc = ^pc
		w = regAt(s.c.code, pc)
		pc = regSlowTable[w.Op()](s.regs, w, pc, s)
	}
	for pc < 0 {
		// The frame is done: what ret returns the instruction it ended at
		// says, as runReg reads it.
		v := Undefined
		if s.err == nil && w.Op() == bytecode.RRet {
			v = s.regs[w.A()]
		}
		d := ctx.depth
		if d == 0 {
			return v, true
		}
		// Frame d is done: its caller goes on, as rhCallW does after
		// runReg, or meets the throw.
		rec := ctx.recs[d-1]
		nf, rc := s.f, s.c
		kind := nf.regKind
		nf.regKind = 0
		if (rc.ncap != 0 || kind == regFrameEnter) && len(nf.openUpvalues) != 0 {
			regCloseUpvalues(nf)
		}
		r.frameDepth--
		r.stackTop = int(rec.saved)
		if a := nf.args; kind == regFrameEnter && a != nil {
			// The arguments kept for it, the last on the argument stack,
			// as regEnterW gives them back.
			r.argStack = r.argStack[:len(r.argStack)-len(a)]
		}
		nativePop(ctx, &rec)
		caller := r.nativeTop(ctx.st, ctx)
		retPC := int((rec.retIP - rec.codeBase) / 8)
		dst := caller.wb + int(rec.bOff/16)
		if err := s.err; err == nil {
			r.stack[dst] = v
			pc = retPC + 1
		} else if kind == regFrameEnter {
			// regCallMiss's end of regEnterW's frame.
			if err = r.regMissEnd(err, dst); err == nil {
				pc = retPC + 1
			} else {
				caller.kept.pc = retPC + 1
				pc = caller.fail(err)
			}
		} else if err = r.regCallWEnd(nf, regFrameCall, err, int(rec.saved), dst); err == nil {
			pc = retPC + 1
		} else {
			caller.kept.pc = retPC + 1
			pc = caller.fail(err)
		}
		s.err = nil
		s = caller
		w = regAt(s.c.code, retPC)
	}
	ctx.ip = uintptr(s.c.code) + uintptr(pc)*8
	return Undefined, false
}

// nativeFillPCs writes the pc of each caller on the native frame stack
// that has none written yet: its source position past its call, as
// rhCallW writes it before runReg, so that what Go runs -- a stack trace,
// the debugger -- reads the frames runReg would have made.
//
//go:nocheckptr
func nativeFillPCs(ctx *nativeCtx) {
	for d := ctx.filled; d < ctx.depth; d++ {
		rec := &ctx.recs[d]
		nf := (*frame)(nativePtr(&rec.nf))
		rc := (*regCode)(nativePtr(&rec.rc))
		nf.pc = regSrc(rc, int((rec.retIP-rec.codeBase)/8)) + 1
	}
	ctx.filled = ctx.depth
}

// nativePtr is the pointer the address at p holds: an object that is
// kept alive elsewhere, which the context and the frame stack hold as
// addresses.
func nativePtr(p *uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(p)) }

// nativePop makes the caller rec records the top frame again.
func nativePop(ctx *nativeCtx, rec *nativeRec) {
	ctx.regs, ctx.ic, ctx.codeBase = rec.regs, rec.ic, rec.codeBase
	ctx.rc, ctx.nf, ctx.cc, ctx.fv = rec.rc, rec.nf, rec.cc, rec.fv
	ctx.depth--
	ctx.filled = min(ctx.filled, ctx.depth)
	ctx.topOK = 0
}

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

// nativeWB is the runtime's write-barrier flag, as the JIT reads it
// (internal/jit/barrier.go).
//
//go:linkname nativeWB runtime.writeBarrier
var nativeWB struct {
	enabled bool
	pad     [3]byte
	alignme uint64
}

// nativeBase is the handlers' block's address, where the code is entered.
var nativeBase = nativeHandlersAddr()

// nativeHas says which operations have a handler of their own in the
// native table, every other's the exit stub's, which the last's is.
var nativeHas = func() (has [256]bool) {
	at := nativeTableAddr()
	t := (*[256]uintptr)(nativePtr(&at))
	for op := range t {
		has[op] = t[op] != t[len(t)-1]
	}
	return has
}()

// nativeStoreRef is jumped to by native code to store a pointer while the
// collector marks: the slot in AX, the pointer in CX, where to go on in
// R11, the context in DI, which alone it keeps.
func nativeStoreRef()

func nativeStoreRefAddr() uintptr

// nativeStoreRefGo is the store, with the write barrier.
//
//go:nosplit
//go:norace
//go:nocheckptr
func nativeStoreRefGo(slot *unsafe.Pointer, p unsafe.Pointer) { *slot = p }

// nativeCallH is where an exit calls the Go handler of its instruction:
// its func value in DX, its arguments in AX, BX, CX and SI (the state),
// the context in DI. It is a function the Go function that entered the
// code called, as the JIT's callGo is, and goes on at the context's after
// with the handler's result in AX and the context in DI.
func nativeCallH()

func nativeCallHAddr() uintptr

// nativeEnter jumps to code with the context in DI; the code returns to
// nativeEnter's caller.
//
//go:noescape
func nativeEnter(code uintptr, ctx unsafe.Pointer)

// regNativeHandlers is the assembled block of the handlers
// (zzzyn_native_handlers_amd64.s); nativeHandlersAddr is its address and
// nativeTableAddr its table's, each operation's handler's address, the
// exit stub's where it has none.
func regNativeHandlers()

func nativeHandlersAddr() uintptr

func nativeTableAddr() uintptr
