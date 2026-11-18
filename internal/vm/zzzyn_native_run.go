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
// case their Go handlers' fast paths answer, and more: the arithmetic --
// mul_imm and the int32 forms among it -- and the bitwise operators, of
// numbers (by ToInt32 where the Go handler takes int32s alone), into any
// register, where the Go handler's stores
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
	// A step-and-branch leaves native code only for a step its fast path
	// does not make, before it commits anything: rsStep's.
	bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI, bytecode.RStepJLeU,
	bytecode.RMulI, bytecode.RAddI32, bytecode.RSubI32, bytecode.RAddII32,
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
	nativeJITCallFn = nativeJITCall
	nativeNewFn = nativeNew
	nativeSuperFn = nativeSuper
	nativeApplyFn = nativeApply
	nativeReflectFn = nativeReflect
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
// or throws (nativeDrive).
func (r *Runtime) nativeLoop(st *regState, ctx *nativeCtx, f *frame, c *regCode, start int) (Value, error) {
	ctx.depth, ctx.filled = 0, 0
	nativeSetTop(ctx, uintptr(unsafe.Pointer(st.regs)), f, c)
	ctx.ip = uintptr(c.code) + uintptr(start)*8
	ctx.top, ctx.topOK = st, 1
	ctx.cntExits = 0
	if nativeCount {
		ctx.cntExits = uintptr(unsafe.Pointer(&nativeExits[0]))
	}
	return r.nativeDrive(st, ctx, false)
}

// nativeDrive is nativeLoop's loop: the code entered -- but the first
// time where entered says the caller has entered it -- and entered again
// after each instruction it leaves to Go, until the base frame of st
// returns or throws.
func (r *Runtime) nativeDrive(st *regState, ctx *nativeCtx, entered bool) (Value, error) {
	// restarted says a tail call has given the frame to its callee in
	// place and the code goes on with the callee's (nativeRestart).
	restarted := false
	for {
		if !entered {
			ctx.budget = int64(r.backEdges)
			if nativeCount {
				atomic.AddInt64(&nativeEnters, 1)
			}
			nativeEnter(nativeBase, unsafe.Pointer(ctx))
			r.backEdges = int(ctx.budget)
		}
		entered = false
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
		if ctx.depth == ctx.cap && ctx.cap < nativeMaxDepth && (op == bytecode.RCallW || op == bytecode.RCallMW || op == bytecode.RCallWT || op == bytecode.RCallMWT || op == bytecode.RIterNext || op == bytecode.RNewW) {
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

// nativeCallbackRun runs a callback's frame f, its registers at regs, in
// its code entry kept from one call to the next (regCallback): where the
// last call left the state and the context's top frame made for the same
// frame record, only the code's first instruction and the budget are
// given before it is entered, and a return with no exit is read here.
// Anything else -- an exit, a throw, a frame given to a callee in place --
// goes on as nativeLoop does (nativeDrive), and the next call makes the
// state and the top frame again.
func (r *Runtime) nativeCallbackRun(cb *regCallback, f *frame, regs *[1 << 16]Value) (Value, error) {
	st, ctx, c := cb.st, cb.ctx, cb.c
	if cb.set != f {
		st.f, st.c, st.cl, st.err = f, c, cb.cl, nil
		st.wb = cb.base
		st.regs = regs
		ctx.depth, ctx.filled = 0, 0
		nativeSetTop(ctx, uintptr(unsafe.Pointer(regs)), f, c)
		ctx.cntExits = 0
		if nativeCount {
			ctx.cntExits = uintptr(unsafe.Pointer(&nativeExits[0]))
		}
		cb.set = f
	}
	// The base frame the top one: a call the last run made and returned
	// from natively left the deeper state's.
	if ctx.top != st {
		ctx.top = st
	}
	ctx.topOK = 1
	ctx.ip = uintptr(c.code)
	ctx.budget = int64(r.backEdges)
	if nativeCount {
		atomic.AddInt64(&nativeEnters, 1)
	}
	nativeEnter(nativeBase, unsafe.Pointer(ctx))
	r.backEdges = int(ctx.budget)
	if ctx.retDone == 1 && ctx.used == 0 {
		// The frame returned with no exit: its value in the scratch.
		ctx.retDone = 0
		v := Value{num: math.Float64frombits(ctx.scratch[sV0]), ref: *(*unsafe.Pointer)(unsafe.Pointer(&ctx.scratch[sV1]))}
		ctx.scratch[sV1] = 0
		if nativeCount {
			atomic.AddInt64(&nativeCalls, ctx.made)
			ctx.made = 0
		}
		return v, nil
	}
	cb.set = nil
	v, err := r.nativeDrive(st, ctx, true)
	nativeReset(ctx)
	return v, err
}

// nativeRestart goes on with the base frame given to a callee in place by
// a tail call (errRegRestart, regTailTo), as regRestartRun runs it again
// -- the interrupt counted, the code r.regRestart, from its first
// instruction -- but in this code entry, rather than in one of its own
// for each call. It reports false where the frame ends instead, with the
// error in st -- errRegRestart itself where the callee has native code made
// from its register code, whose caller's regRestartRun runs the frame in
// it (jitRegEnter), as regEnterW runs a call's.
func (r *Runtime) nativeRestart(st *regState, ctx *nativeCtx) bool {
	if r.nativeRestartJIT(st.f) {
		return false
	}
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

// nativeRestartJIT reports whether frame f, given to a callee in place
// (r.regRestart), runs it in the callee's native code made from its
// register code, which regRestartRun enters (jitRegEnter): a recursion
// through calls in tail position past tailCallDepth had run in this code
// entry from there on, the functions' native code never entered.
func (r *Runtime) nativeRestartJIT(f *frame) bool {
	return jitBuilt && f.cl.jitEnters && f.cl.reg == r.regRestart
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
//
// Not inlined, as it was not while it made the context itself: nativeRun
// and nativeCallback call it as before.
//
//go:noinline
func (r *Runtime) newNativeCtx(st *regState) *nativeCtx {
	ctx := new(nativeCtx)
	r.initNativeCtx(ctx, st)
	return ctx
}

// initNativeCtx makes ctx st's context, as newNativeCtx makes one.
func (r *Runtime) initNativeCtx(ctx *nativeCtx, st *regState) {
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
	ctx.jitCallFn = *(*uintptr)(unsafe.Pointer(&nativeJITCallFn))
	ctx.newFn = *(*uintptr)(unsafe.Pointer(&nativeNewFn))
	ctx.hasInst = unsafe.Offsetof(Realm{}.hasInstanceFn)
	ctx.superFn = *(*uintptr)(unsafe.Pointer(&nativeSuperFn))
	ctx.callFn = unsafe.Offsetof(Realm{}.callFn)
	ctx.applyFn = *(*uintptr)(unsafe.Pointer(&nativeApplyFn))
	ctx.reflectFn = *(*uintptr)(unsafe.Pointer(&nativeReflectFn))
	ctx.reflect = unsafe.Offsetof(Realm{}.reflectConstructFn)
	ctx.applyAt = unsafe.Offsetof(Realm{}.applyFn)
	ctx.grow()
	st.native = ctx
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
	ctx.super = regCallCache{}
	ctx.thisRef = nil
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
		// The context's slow table: the typed table's in a code entry of
		// typed code (nativeRunTyped).
		pc = (*[256]regHandler)(nativePtr(&ctx.slowTable))[w.Op()](s.regs, w, pc, s)
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
		if s.err == errRegRestart && s.f.regKind == regFrameCall && !r.nativeRestartJIT(s.f) && r.nativeRestartDeep(s, ctx) {
			// Given to a callee in place by Go (regTailTo): the callee's
			// code goes on in the frame, which the code entered itself --
			// but code with native code, which the frame's end runs it in
			// (regCallWEnd, regRerun, regRestartRun).
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
		var parent *Object
		derivedBad := false
		switch kind {
		case regFrameConstruct:
			// new_w's frame, ended as regEnterW ends one: what is not an
			// object returned gives its this, but a derived class's, whose
			// this binding is let go of, which throws once the frame is
			// gone, as constructWithTarget does.
			if nf.thisRef != nil {
				regEndThisRef(nf, rc)
				derivedBad = s.err == nil && !v.IsObject()
			} else if s.err == nil && !v.IsObject() {
				v = r.stack[nf.base+1]
			}
			kind = regFrameEnter
		case regFrameSuper:
			// super()'s call of the parent, whose return binds the
			// caller's this below.
			if nf.thisRef != nil {
				regEndThisRef(nf, rc)
			}
			parent = nf.callee
			kind = regFrameEnter
		}
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
		if derivedBad {
			caller.at(retPC)
			s.err = r.throwTypeError(errDerivedReturn)
		} else if s.err == errNoSuper {
			// A derived class's body that ended before super(): its
			// ReferenceError, once the frame is gone, as
			// constructWithTarget and callObject make it.
			caller.at(retPC)
			s.err = r.throwError(errReference, "%s", errNoSuper.Error())
		}
		bound := false
		if parent != nil && s.err == nil {
			// super(): this bound to what the parent's construction gave,
			// as superCall binds it.
			caller.at(retPC)
			if s.err = r.superBind(caller.f, parent, v); s.err == nil {
				regSuperBound(caller.f, regAt(caller.c.code, retPC), caller.regs)
				bound = true
			}
		}
		if err := s.err; bound {
			pc = retPC + 1
		} else if err == nil {
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
	f.pc = uint32(pc) + 1
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
//
// A built-in it applies without a frame -- Math's functions, push and pop,
// the native leaves whose checks pass (regNative's first cases) -- is
// applied first, with none of the state an exit makes: it throws nothing,
// runs no code and reads nothing of the frame. Anything else is
// regNative's, which asks again.
func nativeBuiltin(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if r := st.r; r.meter == nil {
		fd := st.cl.regCalls[w.C()].callee.fn()
		if fd.mathOp != 0 || fd.elemOp != 0 {
			base := st.wb + int(w.A())
			args := r.stack[base+2 : base+2+int(w.X())]
			v, ok := Undefined, false
			if fd.mathOp != 0 {
				v, ok = mathCall(fd.mathOp, args)
			} else {
				this := Undefined
				if w.Op().MethodCall() {
					this = r.stack[base+1]
				}
				if op := fd.elemOp; op < elemNativeFirst {
					v, ok = r.pureElemOp(op, this, args, true)
				} else if leafChecks[op](r, this, args) {
					var err error
					v, err = fd.native(r, this, args)
					ok = err == nil
				}
			}
			if ok {
				r.stack[st.wb+int(w.B())] = v
				return pc + 1
			}
		}
	}
	st.kept.pc = pc + 1
	st.f.pc = uint32(pc) + 1
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
// where the handler enters the callee with a frame or reads its leaf's
// answer itself.
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
		// A callee the handler enters with a frame, and a leaf whose
		// answer the handler reads itself, from the regcomp's
		// classification -- a parameter, a constant, a captured binding
		// (regLeafArg, regLeafConst, leafUp) -- which the handler then
		// answers with no exit, where Go's would have run the body. Other
		// leaves are rhCallW's miss's (pureCall, leafCall, regLeafRun),
		// and a frame that keeps its arguments is entered by regEnterW,
		// each cheaper than the exit the call would come back to. One with
		// native code the handler has Go enter (nativeJITCall), as
		// rhCallW's hit does. A runtime that runs the JIT keeps its leaves
		// on rhCallW's miss, as its callbacks keep them on callDirect: the
		// feedback dispatcher counted the call where it began, and the
		// call made again would be counted again, a second call of the
		// site to the frequencies the JIT inlines by.
		if cl, rc, kind, this, enter := r.regCallKind(fd, method, c); enter && kind != 0 && (rc.leaf == regLeafNone || (rc.leaf >= regLeafArg || rc.leafUp != 0) && !(jitBuilt && r.jitOn())) && !rc.args && !regTypedCross(rc, c) && (kind != regCallObjThis || win[1].IsObject()) {
			if !f.cl.regCallsHeld {
				r.regCallsHold(f.cl.closureCode)
			}
			regCacheCallee(e, o, cl, rc, this, kind, c)
			return pc
		}
	}
	return regTable[w.Op()](regs, w, pc, st)
}

// nativeCallMissFn is nativeCallMiss's func value, set by init.
var nativeCallMissFn func(*[1 << 16]Value, bytecode.RInstr, int, *regState) int

// nativeJITCall is call_w's and call_method_w's call of a callee its
// site's cache holds whose calls go to the JIT (closureCode's jitEnters):
// one with native code made from its register code, or whose runs have
// spent what was left of its budget (jitRegCallTier). The call handler
// calls it through callH with no exit, as it calls nativeBuiltin, its
// checks of the callee and its this made: rhCallW's entry of the callee
// -- its frame, then jitRegEnter -- without rhCallW's search of the cache
// the code has searched. A callee rhCallW leaves to regCallMiss (a leaf, a
// frame that keeps its arguments) is rhCallW's.
func nativeJITCall(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r, f, wb := st.r, st.f, st.wb
	e := &f.cl.regCalls[w.C()]
	if e.kind&(regCallOut|regCallLeaf) != 0 {
		return rhCallW(regs, w, pc, st)
	}
	st.kept.pc = pc + 1
	f.pc = uint32(pc) + 1
	base := wb + int(w.A())
	win := regWindow(r.stack, base)
	if e.kind == regCallThis {
		win[1] = e.this
	}
	o, cl, rc := e.callee, e.cl, e.rc
	argc := int(w.X())
	top := base + int(rc.numRegs)
	if top > len(r.stack) || r.frameDepth >= r.maxFrames {
		return st.done(r.throwRangeError("maximum call stack size exceeded"))
	}
	if r.interruptCounter--; r.interruptCounter <= 0 {
		if err := r.checkInterruptNow(); err != nil {
			return st.done(err)
		}
	}
	saved := r.stackTop
	if top > saved {
		r.stackTop = top
		if top > r.stackHigh {
			r.stackHigh = top
		}
	}
	if argc > int(rc.keep) {
		r.regKeepWindow(base, int(rc.keep), argc)
	}
	for i := min(argc, int(rc.params)); i < int(rc.params+rc.inits); i++ {
		win[2+i] = Undefined
	}
	off := r.frameDepth - r.frameBase
	if uint(off) >= uint(len(r.cur)) {
		r.seekFrameBlock()
		off = r.frameDepth - r.frameBase
	}
	r.frameDepth++
	if r.frameDepth > r.frameHigh {
		r.frameHigh = r.frameDepth
	}
	nf := &r.cur[off]
	nf.cl = cl
	nf.callee = o
	nf.base = base
	nf.pc = 0
	if len(nf.openUpvalues) != 0 {
		nf.openUpvalues = nf.openUpvalues[:0]
	}
	if nf.thisRef != nil || nf.withScopes != nil || nf.evalVars != nil || nf.native != "" || nf.args != nil {
		regClearFrame(nf)
	}
	if rc.newTarget {
		nf.newTarget = Undefined
	}
	nf.regKind = regFrameCall
	v, err := r.jitRegEnter(nf, cl, rc, base, 0)
	nf.regKind = 0
	if rc.ncap != 0 && len(nf.openUpvalues) != 0 {
		regCloseUpvalues(nf)
	}
	r.frameDepth--
	r.stackTop = saved
	if err != nil {
		return st.done(r.regCallWEnd(nf, regFrameCall, err, saved, wb+int(w.B())))
	}
	regWindow(r.stack, wb)[w.B()] = v
	return pc + 1
}

// nativeJITCallFn is nativeJITCall's func value, set by init.
var nativeJITCallFn func(*[1 << 16]Value, bytecode.RInstr, int, *regState) int

// nativeNew is new_w's object, made for the native handler through callH
// with no exit, as closure_w's calls nativeClosure: the construction's
// with the constructor its site's cache holds (regCallConstruct), which
// the handler has checked is R[A], made as regCtorThis makes it and put
// in R[A+1], the frame's this. It reports 1 for the handler to enter the
// constructor with it; 3 where the handler may first make the stores of
// the constructor's initializer plan, or of its forwarder's method's
// (regCode's initPlan, fwdKind), itself, without a frame -- the JIT not
// running, whose frames have feedback to give, an interrupt not due, and
// the frames the body would have had within the limit, so that its
// RangeError is the frame's -- and enter it where they do not all
// validate; 2 where the constructor is a body that only stores its
// parameters in this, which leafCall has done, R[B] the object; and 0
// where the construction is Go's (rsNewW): a prototype that is no data
// property holding an object, a root whose room is not settled, a leaf
// body leafCall does not answer. It throws nothing and runs no code.
//
//go:nocheckptr
func nativeNew(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) int {
	f := (*frame)(nativePtr(&ctx.nf))
	r := ctx.st.r
	e := &f.cl.regCalls[w.C()]
	o := e.callee
	obj := r.regCtorThis(e, o.fn())
	if obj == nil {
		return 0
	}
	a := int(w.A())
	regs[a+1] = Obj(obj)
	ctx.scratch[sThisRef] = 0
	if e.callee.fn().ctorKind == ctorDerived {
		// A derived class's frame, with its this binding, which the
		// handler gives the frame, holding the object until super()
		// binds it, as regRunFD makes one.
		ctx.thisRef = r.regThisBinding(e.rc, Obj(obj))
		ctx.scratch[sThisRef] = uint64(uintptr(unsafe.Pointer(ctx.thisRef)))
		return 1
	}
	if c := e.rc; (len(c.initPlan) > 0 && r.frameDepth < r.maxFrames || c.fwdKind != 0 && r.frameDepth+1 < r.maxFrames) &&
		r.interruptCounter > 1 && !r.jitOn() {
		if c.fwdKind != 0 {
			// The handler reads the method of obj's shape at the
			// forwarder's site, or in its table where the site has given
			// up, which leafForward would have filled (fwdCache).
			site := int(c.fwdSites & 0xffff)
			r.fwdCache(e.cl, site, obj, e.cl.names[c.siteName[site]])
		}
		return 3
	}
	if leaf := leafKind(e.cl); leaf != bytecode.LeafNone && leaf != bytecode.LeafPure && leaf != bytecode.LeafForward {
		if _, done := r.leafCall(e.cl, obj, regs[a+2:a+2+int(w.X())]); !done {
			return 0
		}
		regs[w.B()] = Obj(obj)
		return 2
	}
	return 1
}

// nativeSuper is super_call's and super_forward's entry of the parent
// constructor, made for the native handler through callH with no exit:
// in a derived class's constructor's own frame whose this super() has
// not bound yet, of a class with no fields of its own, the parent its
// constructor's prototype, a constructor the handler enters as a register
// frame (regCtorEntered), its window made at the stack's top -- the
// parent, the frame's this and the arguments, the registers' from C or
// the frame's own -- with its this binding where it is a derived class's,
// and its new.target the frame's, as superCall makes the call. It reports
// 1 for the handler to enter it, the context's super the entry, and 0
// where super() is Go's (rhSuperCall). It throws nothing and runs no
// code.
//
//go:nocheckptr
func nativeSuper(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) int {
	f := (*frame)(nativePtr(&ctx.nf))
	r := ctx.st.r
	if f.cl.fn.Kind != bytecode.KindDerivedConstructor || f.thisRef == nil || f.thisRef.init || !f.this.IsObject() ||
		f.callee == nil || !f.newTarget.IsObject() {
		return 0
	}
	fd := f.callee.fn()
	if fd == nil || fd.fieldInit() != nil {
		return 0
	}
	if c := fd.superCtor(); c != nil && c != f.callee {
		if cfd := c.fn(); cfd == nil || cfd.fieldInit() != nil {
			return 0
		}
	}
	parent := r.parentConstructorOf(f)
	if parent == nil {
		return 0
	}
	pfd := parent.fn()
	if pfd == nil {
		return 0
	}
	cl, rc, ok := r.regCtorEntered(parent, pfd)
	if !ok {
		return 0
	}
	var args []Value
	if w.Op() == bytecode.RSuperForward {
		args = f.args
	} else {
		lo := int(w.C())
		args = regs[lo : lo+int(w.B()) : lo+int(w.B())]
	}
	wb := r.stackTop
	if wb+2+len(args) > len(r.stack) {
		return 0
	}
	regNextFrameClear(r)
	win := r.stack[wb : wb+2+len(args) : wb+2+len(args)]
	win[0], win[1] = Obj(parent), f.this
	copy(win[2:], args)
	ctx.scratch[sThisRef] = 0
	if pfd.ctorKind == ctorDerived {
		ctx.thisRef = r.regThisBinding(rc, f.this)
		ctx.scratch[sThisRef] = uint64(uintptr(unsafe.Pointer(ctx.thisRef)))
	}
	ctx.super = regCallCache{callee: parent, cl: cl, rc: rc, kind: regCallEnter}
	ctx.scratch[sE] = uint64(uintptr(unsafe.Pointer(&ctx.super)))
	ctx.scratch[sWin] = uint64(uintptr(unsafe.Pointer(&win[0])))
	ctx.scratch[sArgc] = uint64(len(args))
	ctx.scratch[sNewTgt] = uint64(uintptr(unsafe.Pointer(f.newTarget.Object())))
	return 1
}

// nativeApply is apply_args's entry of its target, f.apply(this,
// arguments) by the realm's Function.prototype.apply, made for the native
// handler through callH with no exit: f a function the handler enters as
// call_w enters the callee its cache holds (regCallKind), its window made
// at the stack's top -- f, the this, as its kind takes it, and the frame's
// arguments as regApplyArgs hands them on -- without apply's frame, which
// a stack trace and a caller pass over. It reports 1 for the handler to
// enter it, the context's super the entry, and 0 where the call is Go's
// (rsApplyArgs); 3 where the handler may first make the stores of f's
// initializer plan (regCode's initPlan) itself, on the this, an object,
// without f's frame, as nativeNew's 3 says, and enter f where they do not
// all validate. It throws nothing and runs no code.
//
//go:nocheckptr
func nativeApply(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) int {
	f := (*frame)(nativePtr(&ctx.nf))
	r := ctx.st.r
	b := int(w.B())
	target, apply, this := regs[b], regs[b+1], regs[b+2]
	if !apply.IsObject() || apply.Object() != r.applyFn || !target.IsObject() {
		return 0
	}
	o := target.Object()
	fd := o.fn()
	if fd == nil || fd.closure == nil || fd.bound || r.nodeQuirks {
		// With Node's quirks, f.arguments reads the arguments as apply
		// passed them (regDirectLegacy): Go's.
		return 0
	}
	cl, rc, kind, ethis, enter := r.regCallKind(fd, true, ctx.st.c)
	switch {
	case !enter || regTypedCross(rc, ctx.st.c):
		return 0
	case kind == regCallThis:
		this = ethis
	case kind == regCallObjThis:
		if !this.IsObject() {
			return 0
		}
	case kind != regCallEnter:
		return 0
	}
	args := f.args
	if f.cl.fn.MappedArguments {
		n := min(f.cl.fn.ParamCount, len(args), len(f.locals))
		for i := 0; i < n; i++ {
			if !f.locals[i].sameBits(args[i]) {
				// A parameter has been assigned, which a mapped object
				// would show: regApplyArgs's.
				return 0
			}
		}
	}
	wb := r.stackTop
	if wb+2+len(args) > len(r.stack) {
		return 0
	}
	regNextFrameClear(r)
	win := r.stack[wb : wb+2+len(args) : wb+2+len(args)]
	win[0], win[1] = target, this
	copy(win[2:], args)
	ctx.super = regCallCache{callee: o, cl: cl, rc: rc, kind: regCallEnter}
	ctx.scratch[sE] = uint64(uintptr(unsafe.Pointer(&ctx.super)))
	ctx.scratch[sWin] = uint64(uintptr(unsafe.Pointer(&win[0])))
	ctx.scratch[sArgc] = uint64(len(args))
	if len(rc.initPlan) > 0 && rc.initNew == 0 && this.IsObject() && r.frameDepth < r.maxFrames && r.interruptCounter > 1 && !r.jitOn() {
		return 3
	}
	return 1
}

// regNextFrameClear clears what the last user of the frame record a call
// at the runtime's depth now takes left that a register frame has none of
// (regClearFrame): Go's call of the same callee, apply's or super()'s,
// leaves it the built-in's native frame, and the handler, which leaves a
// record so left to Go, would have left every call after it to Go.
func regNextFrameClear(r *Runtime) {
	if off := r.frameDepth - r.frameBase; uint(off) < uint(len(r.cur)) {
		if nf := &r.cur[off]; nf.thisRef != nil || nf.withScopes != nil || nf.evalVars != nil || nf.native != "" {
			regClearFrame(nf)
		}
	}
}

// nativeReflect is a call of the realm's Reflect.construct, its site's
// cached built-in, Reflect.construct(F, args, newTarget), made for the
// call handler through callH with no exit: a construction with F, a
// constructor new_w's handler enters (regCtorEntered), of a plain array
// of arguments with no holes, and a new.target, F where none is given, a
// function whose own prototype is a data property holding an object --
// its object made as constructWithTarget makes it, of new.target's
// prototype and of F's root, its
// window made at the stack's top, a derived class's frame given its this
// binding. It reports 1 for the handler to enter F, the context's super
// the entry, its new.target the scratch's sNewTgt; 2 where F is a body
// that only stores its parameters in this, which leafCall has done, R[B]
// the object; and 0 where the call is Go's (the built-in's). It throws
// nothing and runs no code.
//
//go:nocheckptr
func nativeReflect(regs *[1 << 16]Value, w bytecode.RInstr, pc int, ctx *nativeCtx) int {
	r := ctx.st.r
	a, argc := int(w.A()), int(w.X())
	if argc < 2 || !regs[a+2].IsObject() || !regs[a+3].IsObject() {
		return 0
	}
	o := regs[a+2].Object()
	fd := o.fn()
	if fd == nil {
		return 0
	}
	cl, rc, ok := r.regCtorEntered(o, fd)
	if !ok || r.nodeQuirks && !cl.fn.Strict && !rc.args {
		// With Node's quirks, a sloppy f.arguments reads the arguments as
		// passed (regScopedLegacy): Go's.
		return 0
	}
	nt := o
	if argc > 2 {
		if !regs[a+4].IsObject() {
			return 0
		}
		nt = regs[a+4].Object()
	}
	if nfd := nt.fn(); nfd == nil || nfd.ctorKind == ctorNone || nt.class != ClassFunction {
		return 0
	}
	arr := regs[a+3].Object()
	if arr.class != ClassArray || arr.flags&objHasSparseElements != 0 {
		return 0
	}
	args := arr.elemsOf()
	for _, v := range args {
		if isHole(v) {
			return 0
		}
	}
	p := nt.getOwnVisible(atomPrototype)
	if !p.ok() || p.flags()&propAccessor != 0 || !p.value().IsObject() {
		return 0
	}
	proto := p.value().Object()
	// Its root's room, settled or not: one construction of several
	// new.targets has a root made for each prototype in turn, which no
	// count of objects built would settle; the room is the body's count.
	root := r.shapes.ctorRoot(fd, proto)
	wb := r.stackTop
	if wb+2+len(args) > len(r.stack) {
		return 0
	}
	obj := newOrdinary[struct{}](fd, root, proto, root.roomFor(int(cl.fn.ThisProps)))
	if leaf := leafKind(cl); fd.ctorKind != ctorDerived && leaf != bytecode.LeafNone && leaf != bytecode.LeafPure && leaf != bytecode.LeafForward {
		if _, done := r.leafCall(cl, obj, args); done {
			regs[w.B()] = Obj(obj)
			return 2
		}
		return 0
	}
	regNextFrameClear(r)
	win := r.stack[wb : wb+2+len(args) : wb+2+len(args)]
	win[0], win[1] = Obj(o), Obj(obj)
	copy(win[2:], args)
	ctx.scratch[sThisRef] = 0
	if fd.ctorKind == ctorDerived {
		ctx.thisRef = r.regThisBinding(rc, Obj(obj))
		ctx.scratch[sThisRef] = uint64(uintptr(unsafe.Pointer(ctx.thisRef)))
	}
	ctx.super = regCallCache{callee: o, cl: cl, rc: rc, kind: regCallEnter}
	ctx.scratch[sE] = uint64(uintptr(unsafe.Pointer(&ctx.super)))
	ctx.scratch[sWin] = uint64(uintptr(unsafe.Pointer(&win[0])))
	ctx.scratch[sArgc] = uint64(len(args))
	ctx.scratch[sNewTgt] = uint64(uintptr(unsafe.Pointer(nt)))
	return 1
}

// nativeReflectFn is nativeReflect's func value, set by init.
var nativeReflectFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx) int

// nativeApplyFn is nativeApply's func value, set by init.
var nativeApplyFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx) int

// nativeSuperFn is nativeSuper's func value, set by init.
var nativeSuperFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx) int

// nativeNewFn is nativeNew's func value, set by init.
var nativeNewFn func(*[1 << 16]Value, bytecode.RInstr, int, *nativeCtx) int

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
		nf.pc = uint32((rec.retIP-rec.codeBase)/8) + 1
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
