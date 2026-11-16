//go:build amd64 || arm64

package vm

import (
	"math"
	"os"
	"sync/atomic"
	"unsafe"
	_ "unsafe" // linkname

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The handlers' files, after the other generators', before layoutpad's
// (zzzz_layoutpad.go), which measures what they make.
//
//go:generate go run ./internal/nativegen/cmd

// The register interpreter's native threaded handlers on amd64 and arm64
// (docs/register-interpreter.md, "Native handlers"): machine code for the
// hot operations, each handler ending in its own indirect jump through
// the native table, as V8's Ignition's do. What a handler's fast path
// does not answer -- an operation with no native handler, a value of
// another kind, a cache that missed, a call Go makes -- goes to the
// shared exit stub, which returns here: nativeRun runs the instruction by
// its Go handler from regTable (and its slow function), as runReg would,
// and enters the code again after it.
//
// The code is made offline by internal/vm/internal/nativegen, from the
// package's layout (zzzyn_native_layout.go), and assembled into the
// binary (zzzyn_native_handlers_<arch>.s, go generate): one block for
// every runtime and every build tag of an architecture, never patched,
// every address it needs given by the context. The trampolines that enter
// it and that it calls Go by are each architecture's own
// (zzzyn_native_<arch>.s).
//
// The machine registers on amd64, kept across the dispatch:
//
//	RBX  the instruction's address        R12  the frame's registers
//	R13  the native table                 R15  the closure's caches (ic)
//	R11  the back-edge budget             R10  tagBase, the tags' least
//	RDI  the context (nativeCtx)
//
// and, in a handler, RAX the instruction word, RCX, RDX, RSI, R8, R9,
// X0, X1 and X3 its scratch. On arm64 (nativegen's arm64.go):
//
//	R19  the instruction's address        R20  the frame's registers
//	R21  the native table                 R22  the closure's caches (ic)
//	R23  the back-edge budget             R24  tagBase, the tags' least
//	R25  the context (nativeCtx)
//
// and R0 the instruction word, R1 to R7, R16, R17 and D0 to D3 a
// handler's scratch; R30 holds the return to nativeRun throughout. A
// pointer word is stored plainly while the
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

// nativeExitTable is the Go functions the exit stub calls for an
// instruction the native code leaves (the context's goTable): regTable's
// handler, but for the operations whose native handler's fast path does
// all the Go handler's does (nativeSlowOnMiss), whose slow function it
// is. Their handler would only find again what the native code found
// and give back ^pc, a Go call more for each miss -- an addition of a
// string, a bitwise operator of one -- before the slow function's.
var nativeExitTable [256]regHandler

// nativeSlowOnMiss are the operations whose native handlers answer every
// case their Go handlers' fast paths answer, and more: the arithmetic and
// the bitwise operators, of numbers (by ToInt32 where the Go handler
// takes int32s alone), into any register, where the Go handler's stores
// only into one that holds no pointer; and the property and global reads
// and writes by their sites, whose own entries, holders and, for a read,
// earlier shapes (propPoly) the native code reads as rhGetProp,
// rhSetProp, rhGetGlobal and rhSetGlobal do, and more; bin's division
// and int32 remainder (rhBin); the private members by their sites' own
// entries (rhGetPrivateSite, rhSetPrivateSite); and in by the realm's in
// cache, whose index keys' answers rsIn finds as rhIn does. A
// compare-and-branch is not among them: its Go handler counts a back
// edge's budget as the slow function does not.
var nativeSlowOnMiss = [...]bytecode.ROp{
	bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RAddI, bytecode.RSubI,
	bytecode.RInc, bytecode.RDec, bytecode.RPostInc, bytecode.RPostDec,
	bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI,
	bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr,
	bytecode.RGetProp, bytecode.RGetMethod, bytecode.RSetProp, bytecode.RGetGlobal, bytecode.RSetGlobal,
	bytecode.RBin, bytecode.RGetPrivate, bytecode.RSetPrivate, bytecode.RIn,
}

// The exit table, made after regTable (zzreg_table.go's and
// zzreg_zztable.go's inits, which sort first).
func init() {
	nativeExitTable = regTable
	nativeClosureFn = nativeClosure
	nativeLitFn = nativeLit
	nativeBuiltinFn = nativeBuiltin
	nativeIterResFn = nativeIterRes
	nativeCallMissFn = nativeCallMiss
	for _, op := range nativeSlowOnMiss {
		nativeExitTable[op] = regSlowTable[op]
	}
}

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
	v, err := r.nativeLoop(st, ctx, f, c, start)
	r.nativeLeave(st, ctx)
	return v, err
}

// nativeCallback is a callback's code entry's state and context, which it
// keeps from one call to the next (regCallback): a state off the
// runtime's list, as regEnter takes one, given back by the callback's end.
func (r *Runtime) nativeCallback(c *regCode) (*regState, *nativeCtx) {
	st := r.regFree
	if st == nil {
		st = &regState{r: r}
	} else {
		r.regFree = st.next
	}
	ctx := st.native
	if ctx == nil {
		ctx = r.newNativeCtx(st)
	}
	ctx.epoch = uintptr(unsafe.Pointer(&r.shapes.globalEpoch))
	return st, ctx
}

// nativeLoop is nativeRun's run of frame f, the base frame of st, whose
// context ctx is, from instruction start: the code entered, and entered
// again after each instruction it leaves to Go, until the frame returns
// or throws. A callback's frame kept across calls (regCallback) is run by
// it each call.
func (r *Runtime) nativeLoop(st *regState, ctx *nativeCtx, f *frame, c *regCode, start int) (Value, error) {
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
		if d := ctx.retDone; d != 0 {
			// The base frame returned, or yielded (2): its value in the
			// scratch.
			ctx.retDone = 0
			v := Value{num: math.Float64frombits(ctx.scratch[sV0]), ref: *(*unsafe.Pointer)(unsafe.Pointer(&ctx.scratch[sV1]))}
			ctx.scratch[sV1] = 0
			if d == 2 {
				// rhYield's suspension, which no handler takes.
				pc := int(ctx.pc)
				st.kept.pc = pc + 1
				st.err = r.regYield(st.f, v, pc+1)
				v = Undefined
			}
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
		miss := ctx.miss != 0
		ctx.miss = 0
		if nativeCount {
			atomic.AddInt64(&nativeExits[op], 1)
		}
		if ctx.depth == ctx.cap && ctx.cap < nativeMaxDepth && (op == bytecode.RCallW || op == bytecode.RCallMW || op == bytecode.RCallWT || op == bytecode.RCallMWT || op == bytecode.RIterNext) {
			// A call, or a generator's step, the frame stack has no room
			// for: the stack grown, the call tried again, nothing of it
			// done.
			ctx.grow()
			continue
		}
		if miss {
			pc = nativeCallMiss(s.regs, w, pc, s)
		} else {
			pc = regTable[op](s.regs, w, pc, s)
		}
		// The instructions after it that have no native handler are run
		// here too, rather than each entering the code to exit at once.
		code := s.c.code
		for pc >= 0 {
			w = regAt(code, pc)
			op = w.Op()
			if ctx.disp.has[op] {
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

// nativeRestartDeep is nativeRestart's of a frame the code entered itself,
// s the top frame's state, given to a callee in place by Go (regTailTo):
// where the native tail call left it to Go -- the interrupt due, say --
// the frame goes on with the callee's code in this code entry, the frame
// the code's still, as regRestartRun runs it again, rather than ended for
// regRerun to run it, and every later call of its chain, in a code entry
// of its own. It reports false where the frame ends instead, with the
// error in s.
func (r *Runtime) nativeRestartDeep(s *regState, ctx *nativeCtx) bool {
	rc := r.regRestart
	r.regRestart = nil
	if r.interruptCounter--; r.interruptCounter <= 0 {
		if err := r.checkInterruptNow(); err != nil {
			s.err = err
			return false
		}
	}
	if r.stopped != nil {
		s.err = r.stopped
		return false
	}
	f := s.f
	s.err = nil
	s.c, s.cl = rc, f.cl
	s.wb = f.base - int(rc.locals)
	s.regs = regWindow(r.stack, s.wb)
	nativeSetTop(ctx, uintptr(unsafe.Pointer(s.regs)), f, rc)
	ctx.ip = uintptr(rc.code)
	ctx.top, ctx.topOK = s, 1
	return true
}

// nativeFinish ends nativeLoop with v or st's error. A frame given to
// callees in place closes the captures of the last one's code, as
// regRestartRun closes them: its caller closes only the first's.
func (r *Runtime) nativeFinish(st *regState, ctx *nativeCtx, v Value, restarted bool) (Value, error) {
	if restarted && len(st.f.openUpvalues) != 0 {
		regCloseUpvalues(st.f)
	}
	err := st.err
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
	ctx.disp = r.nativeDispatch()
	ctx.table = ctx.disp.addr
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
	ctx.goTable = uintptr(unsafe.Pointer(&nativeExitTable[0]))
	ctx.slowTable = uintptr(unsafe.Pointer(&regSlowTable[0]))
	ctx.callH = nativeCallHAddr()
	ctx.protoStr = unsafe.Offsetof(Realm{}.proto) + unsafe.Offsetof(intrinsics{}.str)
	ctx.protoNum = unsafe.Offsetof(Realm{}.proto) + unsafe.Offsetof(intrinsics{}.number)
	ctx.primProps = unsafe.Offsetof(Realm{}.primProps)
	ctx.has = unsafe.Offsetof(Realm{}.has)
	ctx.closureFn = *(*uintptr)(unsafe.Pointer(&nativeClosureFn))
	ctx.litFn = *(*uintptr)(unsafe.Pointer(&nativeLitFn))
	ctx.builtinFn = *(*uintptr)(unsafe.Pointer(&nativeBuiltinFn))
	ctx.iterResFn = *(*uintptr)(unsafe.Pointer(&nativeIterResFn))
	ctx.callMissFn = *(*uintptr)(unsafe.Pointer(&nativeCallMissFn))
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
// the states made for the frames above it hold nothing of theirs. They
// are made only at the depths an exit left to Go (nativeTop), so that one
// below may be none: each the run gave a frame (ctx.used) is cleared, or a
// deep one would keep its frame's closure, and so its realm, for as long
// as the base frame's state. Those past it were cleared by the run that
// used them: a callback's entry clears what it used, not the 128 states a
// recursion once grew the stack to.
func (r *Runtime) nativeLeave(st *regState, ctx *nativeCtx) {
	nativeReset(ctx)
	r.regLeave(st)
}

// nativeReset lets go of what a run of ctx's code left: the states of the
// frames it entered, its top's, its count of calls.
func nativeReset(ctx *nativeCtx) {
	if nativeCount {
		atomic.AddInt64(&nativeCalls, ctx.made)
	}
	ctx.made = 0
	ctx.top = nil
	for _, s := range ctx.states[:ctx.used] {
		if s != nil {
			s.f, s.c, s.cl, s.err = nil, nil, nil, nil
		}
	}
	ctx.used = 0
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
	if d > ctx.used {
		ctx.used = d
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
		if s.err == errRegRestart && s.f.regKind == regFrameCall && r.nativeRestartDeep(s, ctx) {
			// Given to a callee in place by Go (regTailTo): the callee's
			// code goes on in the frame, which the code entered itself.
			return Undefined, false
		}
		// Frame d is done: its caller goes on, as rhCallW does after
		// runReg, or meets the throw.
		rec := ctx.recs[d-1]
		if s.f.regKind == regFrameGenStep {
			s, pc, w = r.nativeGenEnd(s, ctx, &rec, v)
			continue
		}
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

// nativeGenEnd ends s, the frame of a generator the iter_next handler
// stepped (regFrameGenStep), where Go has ended it -- a suspension it
// made (a yield with captures open, a yield*), a return with v, a throw
// -- as regGenRun, regGenNext and regGenStep end a step, and goes on in
// the caller as regIterNext does with what the step gave: its caller's
// state, the pc it goes on at, and its iter_next.
//
//go:nocheckptr
func (r *Runtime) nativeGenEnd(s *regState, ctx *nativeCtx, rec *nativeRec, v Value) (*regState, int, bytecode.RInstr) {
	f, c := s.f, s.c
	f.regKind = 0
	err := s.err
	s.err = nil
	g := (*generator)(nativePtr(&rec.gen))
	raw, done := false, false
	if sig, ok := err.(*suspendSignal); ok {
		err = nil
		n, from := int(c.numRegs), int(c.genFrom)
		g.pc = uint32(f.savedSP)
		g.saveWith(f.withScopes)
		saved := g.vals[: n-from : n-from]
		kept := r.stack[f.base+from : f.base+n]
		copy(saved, kept)
		if ups := f.openUpvalues; len(ups) != 0 || g.saved != nil {
			regMoveUps(ups, kept, saved)
			g.keep(nil, nil, ups)
		}
		g.delegating = sig.delegate
		g.state = genSuspendedYield
		v, raw = sig.value, sig.raw
		sig.value = Undefined
	} else {
		for _, u := range f.openUpvalues {
			u.close()
		}
		g.saved = nil
		g.state = genCompleted
		done = true
	}
	f.savedSP = 0
	f.args = nil
	f.detachSuspended()
	// The generator's frame and next's.
	r.frameDepth -= 2
	r.stackTop = int(rec.saved)
	nativePop(ctx, rec)
	caller := r.nativeTop(ctx.st, ctx)
	retPC := int((rec.retIP - rec.codeBase) / 8)
	w := regAt(caller.c.code, retPC)
	caller.kept.pc = retPC + 1
	if w.Op() == bytecode.RCallMW {
		// A step next() made (genNext): its result, as next makes it, or
		// its throw at the call.
		if err != nil {
			return caller, caller.fail(err), w
		}
		if raw && v.IsObject() {
			caller.regs[w.B()] = v
		} else {
			caller.regs[w.B()] = Obj(r.iterResult(v, done))
		}
		return caller, retPC + 1, w
	}
	st := cursorOf(caller.regs[w.B()])
	if err == nil && raw && v.IsObject() {
		// A delegate's own result, which a yield* hands out as it is.
		res := v
		var d Value
		if d, err = r.getValueProp(res, atomDone); err == nil {
			if done = d.Truthy(); !done {
				v, err = r.getValueProp(res, atomValue)
			}
		}
	}
	switch {
	case err != nil:
		st.done = true
		return caller, caller.fail(err), w
	case done:
		st.done = true
		return caller, retPC + 1 + int(w.SC()), w
	}
	caller.regs[w.A()] = v
	return caller, retPC + 1, w
}

// nativeClosure is closure_w as its native handler calls it, through
// callH with no exit: the top frame's closure made as rsClosureW makes
// it (regClosure), with no state of the frame's, which nothing it does
// reads. The handler stores it in R[A].
//
//go:nocheckptr
func nativeClosure(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) *Object {
	f := (*frame)(nativePtr(&ctx.nf))
	c := (*regCode)(nativePtr(&ctx.rc))
	f.pc = c.src[pc] + 1
	return ctx.st.r.regClosure(f, c, w, f.base-int(c.locals))
}

// nativeClosureFn is nativeClosure's func value, whose address a
// context gives the code (closureFn), set by init: nativeClosure reaches
// newNativeCtx, which reads it.
var nativeClosureFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx) *Object

// nativeLit is new_object_lit and new_array_e as their native handler
// calls them, through callH with no exit, as closure_w's calls
// nativeClosure: the top frame's literal made into R[A] as
// rhNewObjectLit and rhNewArrayE make it, which throws nothing, runs no
// code and reads no state of the frame's but its frame, code and
// registers.
//
//go:nocheckptr
func nativeLit(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) {
	f := (*frame)(nativePtr(&ctx.nf))
	c := (*regCode)(nativePtr(&ctx.rc))
	k := regKept{pc: pc + 1}
	if w.Op() == bytecode.RNewObjectLit {
		ctx.st.r.regNewObjectLit(f, c, &k, regs)
		return
	}
	ctx.st.r.regNewArrayE(f, c, &k, regs)
}

// nativeLitFn is nativeLit's func value, set by init, as
// nativeClosureFn is.
var nativeLitFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx)

// nativeBuiltin is call_w's and call_method_w's call of the built-in its
// site cached (regCallNative) as the call handler makes it, a handler of
// the exit's protocol called with no exit (nativegen's light): rhCallW's
// call of it, regNative, in the top frame's state, which the code has
// made, without rhCallW's search of the cache the code has searched.
func nativeBuiltin(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.kept.pc = pc + 1
	st.f.pc = regSrc(st.c, pc) + 1
	return st.done(st.r.regNative(st.cl.regCalls[w.C()].callee, w, st.wb))
}

// nativeBuiltinFn is nativeBuiltin's func value, set by init.
var nativeBuiltinFn func(*[1 << 16]Value, bytecode.RInstr, int, *regState) int

// nativeIterRes is the result of a generator's next(), called with no
// exit as nativeLit is, where the call handler stepped the generator
// itself (nativegen's genNext) and its yield or return gave the value to
// the call's register B: the value made the { value, done } object next
// returns (iterResult), done where pc is not 0. It throws nothing and runs
// no code.
func nativeIterRes(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) {
	regs[w.B()] = Obj(ctx.st.r.iterResult(regs[w.B()], pc != 0))
}

// nativeCallMiss is call_w's and call_method_w's exit where the handler
// meets a callee other than its site's cache holds and of another
// template (ctx.miss): where it is a closure the handler
// enters (regCallKind), the cache is made the new callee's and the call
// made again, which the code then enters itself, rather than Go entering
// it in a code entry of its own (regCallMiss) -- a site that calls several
// functions in turn, an array of them or a method of several layouts --
// where the handler enters the callee with a frame.
// The cache holds the callee the second time, so that a call the handler
// leaves to Go anyway is its Go handler's then (rhCallW, rhCallWT for the
// calls in tail position whose handlers are call_w's). Anything else is
// the Go handler's.
func nativeCallMiss(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r, f, c := st.r, st.f, st.c
	win := regWindow(r.stack, st.wb+int(w.A()))
	calls := f.cl.regCalls
	if uint(w.C()) >= uint(len(calls)) || !regIsObject(&win[0]) {
		return regTable[w.Op()](regs, w, pc, st)
	}
	e, o := &calls[w.C()], (*Object)(win[0].ref)
	method := w.Op().MethodCall()
	if e.callee == o || e.kind == regCallGen || e.kind == regCallMarked || method && pc < len(c.rc.Marks) && c.rc.Marks[pc] != 0 {
		return regTable[w.Op()](regs, w, pc, st)
	}
	if fd := o.fn(); fd != nil && fd.closure != nil && !fd.bound {
		// Only a callee the handler enters with a frame: a leaf rhCallW's
		// miss answers without one (pureCall, leafCall), and a frame that
		// keeps its arguments is entered by regEnterW, each cheaper than
		// the exit the call would come back to.
		if cl, rc, kind, this, enter := r.regCallKind(fd, method); enter && kind != 0 && rc.leaf == regLeafNone && !rc.args && (kind != regCallObjThis || win[1].IsObject()) {
			regCacheCallee(e, o, cl, rc, this, kind)
			return pc
		}
	}
	return regTable[w.Op()](regs, w, pc, st)
}

// nativeCallMissFn is nativeCallMiss's func value, set by init.
var nativeCallMissFn func(*[1 << 16]Value, bytecode.RInstr, int, *regState) int

// nativeIterResFn is nativeIterRes's func value, set by init.
var nativeIterResFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx)

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

// newNativeTable is the dispatcher whose table is at addr.
func newNativeTable(addr uintptr) *nativeTable {
	d := &nativeTable{addr: addr}
	t := (*[256]uintptr)(nativePtr(&addr))
	for op := range t {
		d.has[op] = t[op] != nativeExitAddr
	}
	return d
}

// nativeExitAddr is the exit stub's address: plain's table's entry of the
// last operation, which has no handler.
var nativeExitAddr = func() uintptr {
	at := nativeTableAddr()
	return (*[256]uintptr)(nativePtr(&at))[255]
}()

// nativePlain is the plain dispatcher, the only one in builds without
// the JIT; nativeFeedback, in builds with it, a runtime's that runs the
// JIT, whose handlers record the JIT's feedback
// (zzzyn_native_feedback.go), and plain in builds without.
var nativePlain = newNativeTable(nativeTableAddr())

// nativeDispatch is the dispatcher r's contexts give the code.
func (r *Runtime) nativeDispatch() *nativeTable {
	if r.jitOn() {
		return nativeFeedback
	}
	return nativePlain
}

// nativeStoreRef is jumped to by native code to store a pointer while the
// collector marks: on amd64 the slot in AX, the pointer in CX, where to go
// on in R11, the context in DI, which alone it keeps; on arm64 the slot in
// R0, the pointer in R1, where to go on in R16, the context in R25, which
// it keeps with the link register.
func nativeStoreRef()

func nativeStoreRefAddr() uintptr

// nativeStoreRefGo is the store, with the write barrier.
//
//go:nosplit
//go:norace
//go:nocheckptr
func nativeStoreRefGo(slot *unsafe.Pointer, p unsafe.Pointer) { *slot = p }

// nativeCallH is where an exit calls the Go handler of its instruction:
// on amd64 its func value in DX, its arguments in AX, BX, CX and SI (the
// state), the context in DI; on arm64 its func value in R26, its arguments
// in R0 to R3, the context in R25. It is a function the Go function that
// entered the code called, as the JIT's callGo is, and goes on at the
// context's after with the handler's result in AX (R0) and the context
// in DI (R25).
func nativeCallH()

func nativeCallHAddr() uintptr

// nativeEnter jumps to code with the context in DI (R25 on arm64); the
// code returns to nativeEnter's caller.
//
//go:noescape
func nativeEnter(code uintptr, ctx unsafe.Pointer)

// regNativeHandlers is the assembled block of the handlers
// (zzzyn_native_handlers_<arch>.s); nativeHandlersAddr is its address and
// nativeTableAddr its table's, each operation's handler's address, the
// exit stub's where it has none.
func regNativeHandlers()

func nativeHandlersAddr() uintptr

func nativeTableAddr() uintptr
