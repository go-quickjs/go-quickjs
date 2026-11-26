//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"cmp"
	"encoding/binary"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

// jitCacheBytes is the JIT's budget (jitBudget) in a runtime without a
// memory limit, and maxJITBudget the most a JIT memory limit gives it,
// which no sum of what it holds overflows. 64 MiB holds every program
// measured -- Octane's Typescript peaks at 21 MiB, Octane run whole in one
// runtime at 44 -- with room for larger ones, and is the WebAssembly
// engine's default too (wasm.DefaultJITMemoryLimit). It is a ceiling, not
// a reservation: a runtime holds what its program's code takes, and the
// flush gives back what has gone unused, as the runtime runs and as it
// returns to its host (jitReturned).
const (
	jitCacheBytes = 64 << 20
	maxJITBudget  = 1 << 50
)

// The tiering budgets are V8's (src/flags/flag-definitions.h), counted
// as its TieringManager counts them: invocations of a function, each its
// bytecode's length, the interpreter's back edges spending it too, each
// its loop's length (jitRegEdges). Our
// one optimizing tier is the closest to Maglev, V8's first: TurboFan's
// invocation_count_for_turbofan (3000) has no counterpart here.
const (
	// jitInvocationsForFeedback is invocation_count_for_feedback_allocation:
	// a function is given its entry, which holds its budget and its
	// caches' mark as V8's feedback vector holds its budget, after so many
	// invocations; one called fewer times costs no native state.
	jitInvocationsForFeedback = 8
	// jitInvocationsForMaglev is invocation_count_for_maglev: a function is
	// compiled once the interpreter has spent so many invocations' worth
	// of its budget, and compiled again so after an eager deoptimization
	// invalidated its code (jitRegInvalidate) or after its code learned at
	// its exits (jitRegLearnedDue).
	jitInvocationsForMaglev = 400
	// jitInvocationsAfterICUpdate is minimum_invocations_after_ic_update:
	// a change of the function's caches' feedback raises what is left of
	// its budget to so many invocations' worth, never lowering it, as
	// TieringManager::NotifyICChanged does.
	jitInvocationsAfterICUpdate = 500
)

// jitBuilt guards the JIT's hooks in the interpreter; see jit_disabled.go.
const jitBuilt = true

type jitFields struct {
	jitEnabled bool
	// jitBackground is SetJITBackground's and SetJITBackgroundAuto's:
	// whether compiles are made on the process's workers (jitRegSubmit),
	// as under QJS_JIT_STRESS's background (jitInBackground) --
	// jitOnSpot, jitInWorkers or jitInWorkersAuto.
	jitBackground uint8
	// jitCallThreshold is how many invocations' worth of a function's
	// budget the interpreter spends before it is compiled, or compiled
	// again (jitInvocationsForMaglev), and jitICThreshold what a change of
	// its caches' feedback raises the rest to (jitInvocationsAfterICUpdate):
	// 1 each under QJS_JIT_STRESS's threshold and in the tests that compile
	// at once.
	jitCallThreshold uint16
	jitICThreshold   uint16
	jit              *jitState
	// jitFrameFn and jitFramePC are the function and the loop's header a
	// back edge's check is under way at, whose frame waits there for what
	// the check compiles (jitRegLoop, jitRegFrameAt).
	jitFrameFn *bytecode.Function
	jitFramePC int
	// jitMemoryLimit is Config's JITMemoryLimit: the JIT's budget, if more
	// than 0 (jitBudget).
	jitMemoryLimit int64
	// jitStress is QJS_JIT_STRESS's budget and deoptimization period, or
	// zero; see jitStressConfig.
	jitStress jitStressConfig
	// jitTest is what a test made the runtime's JIT with (testConfig's
	// jit), which the test may change while the runtime lives: zero but in
	// a test's runtime.
	jitTest jitTestConfig
	// jitLazyTop is the depth below which the native levels whose frames
	// calls of Go put off are (jitState's lazy), 0 for none: the
	// Runtime's, not the JIT state's, so that frameAt's test of it is one
	// load and frameAt stays small enough to inline.
	jitLazyTop int
}

// jitTestConfig is what a test makes a runtime's JIT with, and may change
// on the runtime while it lives, in place of what the process's compiles
// are made with (jitEncoding, mir.Settings) and its tools' hooks
// (JITCompileRepeat, JITCodeHook, JITCrossHook). The encoding's and the
// options' are read when the runtime's JIT state is made, and changed on
// it after by jitSetEncoding; the rest where they are used.
type jitTestConfig struct {
	// marking, if set, is the flag the runtime's compiles read for the
	// collector's (abi.Encoding's WriteBarrier), and storeRef and callGo
	// the routines its code stores pointers and calls Go through
	// (StoreRef, CallGo).
	marking          *uint8
	storeRef, callGo uint64
	// round, where set, is whether the compiles round in one instruction
	// (Round).
	round testSwitch
	// annotate, checkAllocations and tableExits, where set, are the
	// compiles' mir.Options.
	annotate, checkAllocations, tableExits testSwitch
	// goCallWork, if set, is called at each of native code's calls of Go
	// but a leaf's, which it sends through jitHostCall's dispatch.
	goCallWork func()
	// ssaOptimized, if set, is given each compile's SSA once optimized,
	// and has the runtime compile on the spot.
	ssaOptimized func(fn *bytecode.Function, f *ssa.Func)
	// noInline, if set, says which functions the compiles inline nowhere.
	noInline func(fn *bytecode.Function) bool
	// keepLimit, if more than 0, is how many keep cells a compile's calls
	// may hold (jitFeedback's keepLimit).
	keepLimit int
	// compileNote, if set, is called at each compile the runtime asks.
	compileNote func(r *Runtime, cl *closure)
	// typedViewMade, if set, is called at each view's copy jitTypedView
	// makes.
	typedViewMade func()
	// pollMade, if set, is called at each of loops' polls Go gives a
	// new budget and native code goes on after, once Go has brought the
	// frame up to date there (jitRegRehome), with whether it had a
	// description to do so by; noRehome has it bring none up to date.
	pollMade func(described bool)
	noRehome bool
	// rehomeEvery, if not 0, is the runtime's jitRehomeEvery.
	rehomeEvery int
	// epochNow and flushNow, if set, stand in for the cache's clocks
	// (epoch, flushNow); clocks, if set, for both where it says so --
	// every test runtime's held but those a test lets run
	// (jit_clocks_test.go).
	epochNow, flushNow func() uint64
	clocks             func(s *jitState) (uint64, bool)
	// parkNow, if set, stands in for parking's clock (parkClock).
	parkNow func() time.Duration
	// flushAge, if not 0, is the runtime's jitFlushAge.
	flushAge uint64
	// compileRepeat, codeHook and crossHook, where set, are the runtime's
	// JITCompileRepeat, JITCodeHook and JITCrossHook.
	compileRepeat int
	codeHook      func(f JITFunction, text []byte, code *mir.Code)
	crossHook     func(f JITFunction, code *mir.Code, err error)
	// tierHook, where set, is the runtime's JITTierHook, and compileClock
	// its JITCompileClock.
	tierHook     func(JITTierEvent)
	compileClock func() time.Duration
	// bgDone has each compile handed to the workers waited for as it is
	// handed over (jitRegSubmit): done at once, it is installed at the
	// checks after, as any is, however long the workers would have taken.
	bgDone bool
	// bgUnstarted keeps each compile handed to the background from the
	// workers (jitRegSubmit): queued, no worker begins it, as where every
	// worker is busy.
	bgUnstarted bool
	// aheadUnstarted keeps each lowering ahead from the workers
	// (jitRegLowerAhead): queued, no worker begins it, nor is it waited for.
	aheadUnstarted bool
	// workspaceBack, where set, is called with each compile in the
	// background as its install hands its workspace back (letGoApart);
	// placeAtInstall has each placed by its install, not its worker.
	workspaceBack  func(*jitRegJob)
	placeAtInstall bool
	// bgReplayed, where set, is called by the worker with each compile in
	// the background once it has put back what its snapshot's questions
	// took back (workRecovering's replay).
	bgReplayed func(*jitRegJob)
	// snapAsked, where set, is called with each question a snapshot asks
	// ahead (jitRegSnapshotOf), its results as the feedback gave them, and
	// whether they answer something.
	snapAsked func(s *jitSnapshot, m jitSnapMethod, arg int64, answered bool, results []any)
	// snapKeepsFeedback has a snapshot keep the register feedback it was
	// asked of (jitSnapshot's rf) past its asking, for a test to ask it
	// again.
	snapKeepsFeedback bool
	// procs, if not 0, stands in for GOMAXPROCS where the runtime's mode
	// asks it (jitBackgroundNow).
	procs int
}

// jitSetEncoding sets s's encoding and options as its runtime's test
// config says: the process's (jitEncoding, mir.Settings), but what the
// test set. Compiles from then on are made so.
func (s *jitState) jitSetEncoding() {
	t := &s.rt.jitTest
	s.enc = jitEncoding
	if t.marking != nil {
		s.enc.WriteBarrier = uint64(uintptr(unsafe.Pointer(t.marking)))
	}
	if t.storeRef != 0 {
		s.enc.StoreRef = t.storeRef
	}
	if t.callGo != 0 {
		s.enc.CallGo = t.callGo
	}
	s.enc.Round = t.round.or(s.enc.Round)
	s.opts = mir.Settings()
	s.opts.Annotate = t.annotate.or(s.opts.Annotate)
	s.opts.CheckAllocations = t.checkAllocations.or(s.opts.CheckAllocations)
	s.opts.TableExits = t.tableExits.or(s.opts.TableExits)
}

// jitNoInlined reports whether the runtime's compiles inline fn nowhere
// (jitTestConfig's noInline).
func (r *Runtime) jitNoInlined(fn *bytecode.Function) bool {
	n := r.jitTest.noInline
	return n != nil && n(fn)
}

// jitCompileRepeat, jitCodeHook and jitCrossHook are the runtime's
// JITCompileRepeat, JITCodeHook and JITCrossHook: the host's, or what a
// test made the runtime with.
func (r *Runtime) jitCompileRepeat() int {
	if n := r.jitTest.compileRepeat; n != 0 {
		return n
	}
	return JITCompileRepeat
}

func (r *Runtime) jitCodeHook() func(f JITFunction, text []byte, code *mir.Code) {
	h := r.jitTest.codeHook
	if h == nil {
		h = JITCodeHook
	}
	if h == nil || r.jit == nil || !r.jit.timingGoroutine {
		return h
	}
	// The host's, timed apart from the compile it is called in
	// (jitHookTimed).
	s := r.jit
	return func(f JITFunction, text []byte, code *mir.Code) {
		defer s.jitHookTimed(r.jitCompileClock())()
		h(f, text, code)
	}
}

func (r *Runtime) jitCrossHook() func(f JITFunction, code *mir.Code, err error) {
	if h := r.jitTest.crossHook; h != nil {
		return h
	}
	return JITCrossHook
}

// jitStressConfig drives the JIT harder than ordinary use, so that tests and
// test262 reach the paths ordinary thresholds rarely do. QJS_JIT_STRESS holds
// a comma-separated list:
//
//	threshold  compile on a function's first framed call
//	budget=N   give loops a budget of N back edges, at the end of which
//	           native code leaves for Go -- not calling Go for another, as it
//	           otherwise does (ssa.GoPoll) -- so that every exit, record and
//	           re-entry is taken
//	deopt=N    finish the invocation in the interpreter at every Nth such
//	           exit, from wherever native code had got to
//	check      have every compile check its register allocation, and panic
//	           where it is wrong (mir.CheckAllocations)
//	background=K
//	           compile in the background (jitRegSubmit), WithJITBackground or
//	           not, and install each compile exactly K checks after it was
//	           handed over (jitDrain), waiting for it if need be: 0 at once
//	miss       with background, have every register compile's worker leave
//	           its build, as one that asked what its snapshot was not asked
//	           (jitSnapMiss), for the runtime's goroutine to build at its
//	           install, so that the path a miss takes is taken by every one
//	takeback   with background, give no compile to a worker, in place of
//	           its schedule: each is taken back at its function's second
//	           ask and made on the spot (jitRegTakeBack), or dropped once
//	           its function goes unused (jitRegDropStale), so that the path
//	           a compile takes where every worker is busy is taken by every
//	           one
//	park       park the code as the runtime returns to its host every time,
//	           not once its running has paid for it (jitReturned), and let
//	           go of the code still parked then, so that code is unparked,
//	           or let go of for an object that died or for going unused,
//	           at the first use after every host's call
//
// It is read once, as QJS_NONATIVE is, and each runtime takes a copy.
type jitStressConfig struct {
	threshold  bool
	background bool
	budget     uint16
	deopt      uint16
	// checks is background's K.
	checks uint16
	miss   bool
	// primLeave, for tests, has a write of a primitive's property leave
	// native code at a host exit, its site never learning the primitive
	// (jitPolySite's prim): the tests of what an exit with no entry after
	// it does -- on-stack replacement, objects carried unmade -- make one
	// so.
	primLeave bool
	// park is QJS_JIT_STRESS's park.
	park bool
	// takeBack is QJS_JIT_STRESS's takeback.
	takeBack bool
}

var jitStressDefault = parseJITStress(os.Getenv("QJS_JIT_STRESS"))

func parseJITStress(spec string) jitStressConfig {
	var c jitStressConfig
	for _, item := range strings.Split(spec, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(item), "=")
		n, _ := strconv.ParseUint(value, 10, 16)
		switch name {
		case "threshold", "1":
			c.threshold = true
		case "budget":
			c.budget = uint16(n)
		case "deopt":
			c.deopt = uint16(n)
		case "background":
			c.background, c.checks = true, uint16(n)
		case "check":
			mir.CheckAllocations = true
		case "miss":
			c.miss = true
		case "park":
			c.park = true
		case "takeback":
			c.takeBack = true
		}
	}
	return c
}

// JITStats reports what the JIT has done in this runtime so far.
func (r *Runtime) JITStats() JITStats {
	s := r.jit
	if s == nil {
		if r.jitEnabled {
			return JITStats{Budget: int64(r.jitBudget()), Pooled: jit.PooledWorkspaceBytes()}
		}
		return JITStats{}
	}
	return JITStats{Compiled: s.compiled, Entries: s.entries, Guards: s.guards,
		Hosts: s.hosts, Budgets: s.budgets, Interpreted: s.interpreted, SSAEntries: s.ssaEntries, SSARecords: s.ssaRecords,
		Held: int64(s.held()), Budget: int64(r.jitBudget()), Evicted: s.evicted, Refused: s.refusedRoom,
		Reoptimized: s.reoptimized, Inlined: s.inlined, InlinedCode: s.inlinedCode,
		Pooled: jit.PooledWorkspaceBytes(), Contexts: len(s.ssaCtxs), ContextsMost: s.ctxMost, ContextsShrunk: s.ctxShrunk,
		Parkings: s.parkings, Parked: s.parkedCount, Unparked: s.unparked}
}

// The runtime's modes of compiling (jitBackground): on the spot, the
// runtime's goroutine waiting for each compile; on the process's workers;
// and on the workers where the process has more than one P, on the spot
// where it has one (jitBackgroundNow).
const (
	jitOnSpot uint8 = iota
	jitInWorkers
	jitInWorkersAuto
)

// SetJITBackground has the JIT compile on the process's workers, the
// runtime's goroutine running on meanwhile, and install the code once it is
// done, whatever GOMAXPROCS is; or, off, the runtime wait for each compile,
// which is the default of a Runtime New makes. It is set before the
// runtime runs anything. It is not a field of Config: a field more there
// changes New's code, and package vm's every function after it moves.
func (r *Runtime) SetJITBackground(on bool) {
	r.jitBackground = jitOnSpot
	if on {
		r.jitBackground = jitInWorkers
	}
}

// SetJITBackgroundAuto has the JIT compile on the process's workers where
// GOMAXPROCS is more than 1, and on the spot where it is 1, as decided at
// each compile (jitBackgroundNow).
func (r *Runtime) SetJITBackgroundAuto() { r.jitBackground = jitInWorkersAuto }

// jitBackgroundNow reports whether the runtime's mode has its compiles made
// on the workers now. With one P, a worker runs only when the runtime's
// goroutine is preempted, which native code never is, so that a compile
// handed over waits for a time slice, the function running in the
// interpreter meanwhile: at GOMAXPROCS 1 the V8 suite's first runs took 1.5
// to 4.8 times as long as on the spot, Octane's Typescript 27% longer. The
// WebAssembly engine's optimizing tier decides so too.
func (r *Runtime) jitBackgroundNow() bool {
	switch r.jitBackground {
	case jitInWorkers:
		return true
	case jitInWorkersAuto:
		if n := r.jitTest.procs; n != 0 {
			return n > 1
		}
		return runtime.GOMAXPROCS(0) > 1
	}
	return false
}

// JITBackground reports whether this runtime runs the JIT and compiles in
// the background now (SetJITBackground, SetJITBackgroundAuto,
// QJS_JIT_STRESS's background).
func (r *Runtime) JITBackground() bool {
	return r.jitEnabled && (r.jitBackgroundNow() || r.jitStress.background)
}

// jitScheduled reports whether QJS_JIT_STRESS's background schedules the
// installs, each at its Kth check: not with takeback, whose compiles no
// worker begins.
func (r *Runtime) jitScheduled() bool {
	return r.jitStress.background && !r.jitStress.takeBack
}

// JITCacheCounts reports what the JIT's cache dropped in this runtime: the
// code it flushed for going unused (flush).
func (r *Runtime) JITCacheCounts() (flushed uint64) {
	if s := r.jit; s != nil {
		return s.flushed
	}
	return 0
}

// jitStressDeopt reports whether this budget exit should finish the
// invocation in the interpreter (jitStressConfig.deopt).
func (r *Runtime) jitStressDeopt(s *jitState) bool {
	if r.jitStress.deopt == 0 {
		return false
	}
	s.stressExits++
	return s.stressExits%uint64(r.jitStress.deopt) == 0
}

type jitRealmFields struct {
	// jitSelf is the realm's half of its code's keys in the cache (jitKey),
	// made once: weak.Make searches its span's specials at each call.
	jitSelf                        weak.Pointer[Realm]
	jitCharCodeAt, jitFromCharCode Value
	// jitSubstring, jitSlice, jitSubstr and jitCharAt are
	// String.prototype's substring, slice, substr and charAt, whose
	// one-character results native code makes itself
	// (ssa.OpSubstringChar).
	jitSubstring, jitSlice, jitSubstr, jitCharAt Value
	// jitLoops are Array.prototype's methods native code makes loops of
	// (ssa.ForEachSite), by their kind (abi.LoopForEach and those after
	// it); nil for none. jitTypedLoops are %TypedArray%.prototype's of the
	// same names, whose loops native code makes of a typed array's calls
	// (abi.LoopTyped): map's and filter's results typed arrays Go makes
	// where the species is the type's own (jitNewTypedResult).
	jitLoops, jitTypedLoops [abi.LoopKinds]*Object
	// jitTypedSpecies is %TypedArray%[Symbol.species]'s getter as the
	// realm made it (jitPlainTypedSpecies).
	jitTypedSpecies *Object
	// jitParseInt is the global parseInt, Number.parseInt too, whose call
	// of a number native code makes itself (ssa.ParseIntFeedback).
	jitParseInt *Object
	// jitBigToString is BigInt.prototype.toString, whose call on a BigInt
	// native code makes by a call of Go of its own (ssa.GoBigString).
	jitBigToString *Object
	// jitSearches are String.prototype's and Array.prototype's indexOf,
	// lastIndexOf and includes, by their kind (jitSearchNames), whose calls
	// on strings and arrays native code makes by a call of Go of their own
	// (ssa.GoIndexOf).
	jitSearches [jitSearchKinds]*Object
	// jitPlainSet is a filter of the addresses of the leaves whose calls
	// jitGoCallPlain makes without a frame (jitLeafOp, of the runtime's
	// leafFns), which a call of Go is asked to be of before anything else
	// of it is looked at: a bit for each address's hash (jitPlainSlot),
	// set for every leaf's -- where Date.now's calls had spent 7% of their
	// time on a walk of eight -- and of a few others', which jitPlainCall
	// then refuses. A table of the addresses, of 64, took 32 of them: the
	// String methods' made 34, and two, by the order a map gave them, were
	// called with frames.
	jitPlainSet [4]uint64
	// jitMapNexts are %MapIteratorPrototype%.next and
	// %SetIteratorPrototype%.next, whose iterators a for...of's step
	// native code makes walks itself (ssa.MapStepFeedback).
	jitMapNexts [2]*Object
}

// jitPlainSlot is an address's bit in jitPlainSet.
func jitPlainSlot(p uintptr) uint64 { return (uint64(p) >> 4) * 0x9e3779b97f4a7c15 >> 56 }

// The kinds of jitSearches: String.prototype's first, then Array's.
const (
	jitSearchStrings = 0
	jitSearchArrays  = 3
	jitSearchKinds   = 6
)

// jitSearchNames are jitSearches' names, by kind, within each prototype.
var jitSearchNames = [3]string{"indexOf", "lastIndexOf", "includes"}

// jitLoopNames are the methods of jitLoops, by kind.
var jitLoopNames = [abi.LoopKinds]string{
	abi.LoopForEach: "forEach", abi.LoopReduce: "reduce", abi.LoopMap: "map", abi.LoopFilter: "filter",
	abi.LoopSome: "some", abi.LoopEvery: "every", abi.LoopFind: "find", abi.LoopFindIndex: "findIndex",
	abi.LoopSort: "sort",
}

// recordJITStringIntrinsic notes the realm's String.prototype.charCodeAt and
// String.fromCharCode, of String, ctor, which native code makes itself.
func (r *Runtime) recordJITStringIntrinsic(ctor *Object) {
	r.jitCharCodeAt = r.proto.str.getOwn(r.atoms.intern("charCodeAt")).value()
	r.jitFromCharCode = ctor.getOwn(r.atoms.intern("fromCharCode")).value()
}

// recordJITSubstrings notes the realm's String.prototype substring, slice,
// substr and charAt, once the realm has them all, before any script runs,
// whose one-character results native code makes itself,
// Array.prototype's forEach, reduce and the others whose calls it makes
// loops of (jitLoops), and the leaves it calls without a frame
// (jitPlainSet): once every built-in is made (initGlobals' last step).
func (r *Runtime) recordJITSubstrings() {
	own := func(name string) Value {
		if m := r.proto.str.getOwn(r.atoms.intern(name)); m.ok() {
			return m.value()
		}
		return Undefined
	}
	r.jitSubstring, r.jitSlice, r.jitSubstr, r.jitCharAt = own("substring"), own("slice"), own("substr"), own("charAt")
	for k, name := range jitSearchNames {
		r.jitSearches[jitSearchStrings+k], r.jitSearches[jitSearchArrays+k] = nil, nil
		if m := own(name); m.IsObject() {
			r.jitSearches[jitSearchStrings+k] = m.Object()
		}
		if m := r.proto.array.getOwn(r.atoms.intern(name)); m.ok() && m.value().IsObject() {
			r.jitSearches[jitSearchArrays+k] = m.value().Object()
		}
	}
	r.jitBigToString = nil
	if m := r.proto.bigint.getOwn(r.atoms.intern("toString")); m.ok() && m.value().IsObject() {
		r.jitBigToString = m.value().Object()
	}
	r.jitParseInt = nil
	if m := r.global.getOwn(r.atoms.intern("parseInt")); m.ok() && m.value().IsObject() {
		r.jitParseInt = m.value().Object()
	}
	r.jitPlainSet = [4]uint64{}
	for _, o := range r.leafFns {
		if fd := o.fn(); fd != nil && (jitLeafOp(fd.elemOp) || fd.elemOp == elemPush) {
			s := jitPlainSlot(uintptr(unsafe.Pointer(o)))
			r.jitPlainSet[s>>6] |= 1 << (s & 63)
		}
	}
	if r.callFn != nil {
		// G.call(recv, ...) of a leaf G (jitPlainCall).
		s := jitPlainSlot(uintptr(unsafe.Pointer(r.callFn)))
		r.jitPlainSet[s>>6] |= 1 << (s & 63)
	}
	for k, p := range [2]*Object{r.proto.mapIter, r.proto.setIter} {
		r.jitMapNexts[k] = nil
		if p == nil {
			continue
		}
		if m := p.getOwn(atomNext); m.ok() && m.value().IsObject() {
			if fd := m.value().Object().fn(); fd != nil && fd.iterNext == iterNextMap {
				r.jitMapNexts[k] = m.value().Object()
			}
		}
	}
	// %TypedArray%[Symbol.species]'s getter, as the realm made it, which a
	// typed array's map and filter ask their species is
	// (jitPlainTypedSpecies).
	r.jitTypedSpecies = nil
	if r.typedArrayCtor != nil {
		if sp := r.typedArrayCtor.getOwn(r.atoms.internSymbol(r.wellKnown.species)); sp.ok() && sp.flags()&propAccessor != 0 {
			if a := sp.value().accessorPair(); a != nil {
				r.jitTypedSpecies = a.getter
			}
		}
	}
	for kind, name := range jitLoopNames {
		r.jitLoops[kind], r.jitTypedLoops[kind] = nil, nil
		if m := r.proto.array.getOwn(r.atoms.intern(name)); name != "" && m.ok() && m.value().IsObject() {
			r.jitLoops[kind] = m.value().Object()
		}
		if r.typedArrayProto == nil {
			continue
		}
		if m := r.typedArrayProto.getOwn(r.atoms.intern(name)); name != "" && m.ok() && m.value().IsObject() {
			r.jitTypedLoops[kind] = m.value().Object()
		}
	}
}

// jitLoopKind is the kind of loop native code makes of a call of o
// (abi.LoopForEach and those after it): the realm's Array.prototype.forEach,
// or another of jitLoops; abi.LoopTyped with it for one of jitTypedLoops;
// or 0.
func (r *Runtime) jitLoopKind(o *Object) int {
	if o != nil {
		for kind, fn := range r.jitLoops {
			if fn == o {
				return kind
			}
		}
		for kind, fn := range r.jitTypedLoops {
			if fn == o {
				return kind | abi.LoopTyped
			}
		}
	}
	return 0
}

// jitLoopFn is the built-in whose call a loop of kind is (jitLoopKind), or
// nil.
func (r *Runtime) jitLoopFn(kind int) *Object {
	base := kind & abi.LoopKindMask
	if base <= 0 || base >= abi.LoopKinds {
		return nil
	}
	if kind&abi.LoopTyped != 0 {
		return r.jitTypedLoops[base]
	}
	return r.jitLoops[base]
}

// jitLoopTyped is the typed array a loop of a %TypedArray%.prototype
// method's is made over, as Go reads it (jitTypedLoops): a view of a
// number type or a BigInt type (jit_bigtyped.go) and a fixed length, its prototype its type's, the realm's --
// an instance of a subclass, a view that tracks a resizable buffer's
// length, are Go's -- its storage looked at, which native code reads its
// length and elements by (fixedStorage); or nil.
func (r *Runtime) jitLoopTyped(v Value) *typedArrayData {
	if !v.IsObject() || v.Object().class != ClassTypedArray {
		return nil
	}
	t, ok := v.Object().dataOf().(*typedArrayData)
	if !ok || t.tracking || int(t.kind) >= len(r.typedArrayProtos) || v.Object().proto() != r.typedArrayProtos[t.kind] {
		return nil
	}
	if t.st == nil && t.fixedStorage() == nil {
		return nil
	}
	return t
}

// jitIsSubstring reports whether o is the realm's String.prototype
// substring, slice, substr or charAt, called with argc arguments: one,
// charAt's, two, the others'.
func (r *Runtime) jitIsSubstring(o *Object, argc int) bool {
	kind, ok := r.jitSubstringOf(o)
	return ok && (kind == ssa.SubstrAt) == (argc == 1) && argc >= 1 && argc <= 2
}

// jitSubstringOf is which of the realm's String.prototype substring,
// slice, substr and charAt o is (jitRealmFields), as OpSubstringChar's Aux
// says, if it is one.
func (r *Runtime) jitSubstringOf(o *Object) (kind int, ok bool) {
	for _, m := range []struct {
		v    Value
		kind int
	}{{r.jitSubstring, 0}, {r.jitSlice, 0}, {r.jitSubstr, ssa.SubstrLength}, {r.jitCharAt, ssa.SubstrAt}} {
		if m.v.IsObject() && m.v.Object() == o {
			return m.kind, true
		}
	}
	return 0, false
}

// jitCodeFields is the JIT's in a template's closureCode, which every
// closure made from the template shares, as V8's closures share their
// function's feedback and code through its SharedFunctionInfo: how hot the
// function is, its permanent refusal, and the hint of its code, a slot in
// the runtime's hint table (jitState.hints), not a pointer -- none of which
// registers a weak key on cold or refused calls. A closure made anew, as
// each call of a function making an arrow makes one, is as warm as its
// function, and knows its code from its first call: each closure's own,
// the hint of every new one had been 0 until Go made a call of it, which
// ran it in the interpreter, and a generic call of one natively
// (ssa.CallSite's Generic), which finds its code by its hint, had been
// Go's; and a function whose closures are each called a few times, AWFY
// DeltaBlue's forEach callbacks, was never compiled, its calls each warm-up
// of a closure of its own. jitResumeSkip is how many resumptions of a
// generator's or an async function's code judged slow Go makes in the
// interpreter without looking at the code again (jitRegEnter).
type jitCodeFields struct {
	jitResumeSkip uint8
	jitHint       [4]byte
	// jitRegRefused says the function is not compiled from its register
	// code; jitReg is its code, nil until compiled (jit_tier.go).
	jitRegRefused bool
	// jitRegHostOnly says the cache held the function's code refused for
	// host operations alone at the last resumption Go made of it, and
	// jitRegColdWork is the work its resumptions have done since, which
	// asks again at jitHotWork (jitRegResumed), kept by the closure's
	// template: no lookup of the cache at each resumption.
	jitRegHostOnly bool
	jitRegColdWork uint16
	jitReg         *jitRegEntry
	// jitRegKey is the template's function's key in the realm its closures
	// run in (jitRegKeyOf), made at its first use.
	jitRegKey jitKey
	// jitRegCounts are what the feedback dispatcher counted of the
	// function's register code (jit_freq.go): its runs, then the
	// calls made at each of its call sites; nil until its call sites'
	// caches are made, and in a runtime that runs no JIT.
	jitRegCounts []uint64
	// jitRegOps is the function's register code's operand lattice
	// (jit_lattice.go): a byte for each instruction, of the kinds of
	// value the operands of an arithmetic, comparison or bitwise one met
	// (abi.Operands), which the feedback dispatcher ORs in, and two past
	// them, whether it has run to a return and whether a compile took it
	// for nothing (jitLatticeRan); nil in a runtime that runs no feedback
	// dispatcher.
	jitRegOps []uint8
	// jitRegTick counts down the runs of the function's register code
	// left of its budget, from jitRegTickSet, which the feedback
	// dispatcher's returns count (jitRegArm): at 0 they mark its calls
	// for Go (jitEnters), which spends their runs (jitRegCallTier).
	jitRegTick, jitRegTickSet int64
	// jitRegEdges counts down the work of the function's budget left for
	// its frames' back edges in the interpreter, from jitRegEdgesSet: each
	// backward jump its distance, which the feedback dispatcher's jumps
	// (nativegen's feedback.go) and the Go table's (regEdgeTick) subtract,
	// as V8's JumpLoop spends its function's budget. At 0 or less they end
	// the budget of back edges, so that the jump's check (regBackEdge)
	// spends the work counted (jitRegLoop) and counts again
	// (jitRegArmEdges). 0, the template's at first, has its first back
	// edge's check begin it.
	jitRegEdges, jitRegEdgesSet int64
}

// hint is the hint of the code of cl's function, its template's closures'
// (jitCodeFields), 0 for a closure with nothing of a template's.
func (cl *closure) hint() uint32 {
	if cl.closureCode == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(cl.closureCode.jitHint[:])
}

func (cl *closure) setHint(h uint32) {
	if cl.closureCode != nil {
		binary.LittleEndian.PutUint32(cl.closureCode.jitHint[:], h)
	}
}

// jitHintSlot is a slot of the hint table (jitState.hints): its function's
// address, which is not a reference -- the cache's keys are weak -- but
// what a closure's function must be for the closure's hint to name the
// slot's link (regHint).
type jitHintSlot struct {
	_  uintptr
	fn uintptr
	// link is the function's link, which a generic call reads (abi's
	// HintTable); the slot is padded to a power of two, which native code
	// indexes it by.
	link *jitLink
	_    uintptr
}

// jitKey is what the cache keeps code by: a function and the realm the code
// is for, as V8 keeps optimized code by native context. Code is compiled for
// its realm -- its literals' pools, the prototypes and the functions it
// checks against, what it calls natively -- and a realm that runs the same
// bytecode, a compiled program run in two, warms up and compiles its own.
// A function's lowering is the realm's no more than its bytecode is, and
// is shared (jitState.lowered). Both halves are weak: a realm a host has
// let go, as a function collected, takes its entries with it (age).
type jitKey struct {
	weak.Pointer[bytecode.Function]
	realm weak.Pointer[Realm]
}

// gone reports whether k's function or its realm has been collected.
func (k jitKey) gone() bool { return k.Value() == nil || k.realm.Value() == nil }

// jitRealmKey is the current realm's half of a jitKey. Native code runs in
// the realm it was compiled for, and a call of a function of another
// switches r.Realm before it gets here, so that is the running function's.
func (r *Runtime) jitRealmKey() weak.Pointer[Realm] {
	re := r.Realm
	if re.jitSelf == (weak.Pointer[Realm]{}) {
		re.jitSelf = weak.Make(re)
	}
	return re.jitSelf
}

// jitKeyOf is the key of fn's code for the current realm.
func (r *Runtime) jitKeyOf(fn *bytecode.Function) jitKey {
	return jitKey{weak.Make(fn), r.jitRealmKey()}
}

// jitRegKeyOf is jitKeyOf(cl.fn), which register code's every call of Go,
// exit and level of a native call asks, kept by cl's template
// (jitCodeFields' jitRegKey), not found by the function's address:
// weak.Make finds or makes the runtime's handle of fn at each call -- a
// search of its span's specials -- a sixth of AWFY's CD under the register
// JIT. A closure of another realm than the running one's has its key made.
func (r *Runtime) jitRegKeyOf(cl *closure) jitKey {
	cc := cl.closureCode
	if cc == nil || cl.realm != r.Realm {
		return r.jitKeyOf(cl.fn)
	}
	if cc.jitRegKey.Pointer == (weak.Pointer[bytecode.Function]{}) {
		cc.jitRegKey = r.jitKeyOf(cl.fn)
	}
	return cc.jitRegKey
}

type jitEntry struct {
	// ssaSerial counts the code's compiles: which code a site's
	// deoptimization was in (jitPolySite's missed).
	ssaSerial uint32
	// ssaStats is what the code did: how often it was entered, left for
	// Go, failed a guard (by bytecode PC) and polled.
	ssaStats jitSSAStats
	reopt    bool
	reopts   uint8
	// calledIn counts native callers' calls of the code that its links
	// counted (jitLink's in) before they counted afresh or were let go
	// of: the calls its report counts (JITReport's Native) went down at
	// each, -jitreport's figures for the function negative.
	calledIn uint64
	// reoptAsks counts the times the code was marked to be compiled again
	// for what it learned (jitRegReopt): a compile in the background
	// prepared before the last of them (jitRegJob's asks) was built from
	// feedback asked ahead of it, and its install leaves the mark set.
	reoptAsks uint32
	// ssaHostsLeft are the PCs of the host operations of its own the code
	// leaves to Go (ssa.Func's HostsLeft): a call there its code has never
	// been seen to make is one seldom made (jitRegFeedback's InlineExit).
	// Each install makes it anew, never writing the list it replaces, so
	// that a copy of its header is the list as it was when copied.
	ssaHostsLeft []int32
	// regCallsMade marks, of register code's learning, the register pcs of
	// the calls Go made for its code or at its exits (jitRegCallSeen), a
	// bit each, of register code regCallsMadeOf instructions long: a call
	// its code leaves to Go and has never made is seldom made
	// (jitRegFeedback's InlineExit). A pc's first call makes the set anew,
	// a copy with its bit, never writing the set it replaces, so that a
	// copy of its header is the set as it was when copied. A count, made
	// in place, had been read only for whether it was 0.
	regCallsMade   []uint64
	regCallsMadeOf int
	// osrFrames counts the calls that went on in the code kept for a loop,
	// up to jitFramesLeaving, and osrCalls is the function's calls when the
	// count began (jitRegFramesLeave).
	osrFrames uint32
	osrCalls  uint64
	// keptAt is starts when the code was compiled with entries kept for
	// the frame its compile was asked for at (ssa.Func's EntryKept): called
	// again jitKeptStarts times, or linked by a native caller, the function
	// is compiled again without them, as keptGone says it is being.
	keptAt   uint64
	keptGone bool
	// keptLeft counts, of the calls since the code was compiled or keptAt,
	// those that left it (jitRegGoOn), and keptCall is the call last counted,
	// by the function's calls, Go's and native callers'.
	keptLeft, keptCall uint64
	// invalid marks code an eager deoptimization invalidated: Go enters it no
	// more, nor do native callers call it (its link), and the interpreter
	// runs the function until it has spent a fresh budget, tierBudget,
	// when it is compiled again (jitRegInvalidate, jitRegHot) -- as V8's
	// deoptimizer invalidates a function's current code and resets its
	// interrupt budget, and the interpreter's spending it tiers the
	// function up again. tierMark is the function's caches' feedback
	// (jitFeedbackMark) when the budget began: a change since delays the
	// compile by another budget, as V8's NotifyICChanged does. deopts
	// counts the code's eager deoptimizations, compiles apart, with no
	// limit, as V8 sets none: each widens the site it failed at
	// (jitRegSiteDeopted), so that they are bounded by the function's
	// sites.
	invalid    bool
	tierBudget int64
	tierMark   uint64
	deopts     uint16
	// warming marks an entry made before its function is compiled
	// (jitRegHot), which holds its budget, tierBudget, and its caches'
	// mark, tierMark, while the interpreter spends it (jitRegWarmUp).
	warming bool
	// regFed says the back edges of a register function's frames have
	// spent the work its caches are given before its budget begins
	// (jitRegLoop).
	regFed bool
	// learning marks valid code that learned at its exits what to be
	// compiled again for, which it is once it has spent tierBudget past
	// tierBase, with tierMark unchanged (jitRegLearnedDue).
	learning bool
	tierBase int64
	// tierLearned is what a register function's code had learned
	// (jitLearnedCount) when its budget for it last began, which a call
	// of Go its native run makes compares (jitRegLearnedDueWith).
	tierLearned int
	// starts counts Go's entries at a function's start, not into a loop;
	// goOns the runs of it the interpreter went on with where its code
	// left them (jitRegGoOn).
	starts, goOns uint64
	// osrWork is the work the interpreter did in the function's loops
	// before Go entered its code at one of them (jitRegLoop), which its
	// budget for what it learned counts as an invocation's (jitRegSpent).
	osrWork int64
	// jitLearned is what the function's code has learned, which outlives
	// its compiles, and which its code for on-stack replacement shares.
	*jitLearned
	// genLearned marks a generator's step learned since the code was
	// compiled (jitRegGenStepSeen).
	genLearned bool
	// polyReopt marks code to be compiled again for its property sites
	// (jitLearned's poly).
	polyReopt bool
	// nativeOut counts the native callers' calls that left native code
	// from inside it (jitRegUnwound); notNative marks code native callers
	// no longer call, for leaving on too many (jitUnwindShare), until Go
	// has made nativeRetry calls to it, more for each time it was found to
	// (nativeBackoff, jitNativeRetry).
	nativeOut     uint64
	notNative     bool
	nativeRetry   uint32
	nativeBackoff uint8
	// regCallers are, for a function's register code (jitState's
	// regLearned), the keys of the functions whose register code calls it
	// natively, through its link, up to jitNativeCallers: compiled again
	// to inline it once its calls are decided (jitRegCallersReopt). Keys,
	// which hold no function alive: what a caller learned outlives its
	// code.
	regCallers []jitKey
	// regCounted marks, for a function's register code, the state the
	// JIT keeps for it (jitState's regLearned), whose bytes (bytes) count
	// in what the JIT holds (regHeld, regAccount); regFlushed marks one
	// whose code the cache let go of (regLetGo), whose next compile is one
	// again of code dropped (jitState's recompiled).
	regCounted, regFlushed bool
	// inlineReopt marks code to be compiled again for the calls found to
	// inline. upgradeReopts counts the times it was to inline a call it
	// makes natively, its callee now inlining its own calls
	// (jitRegCallersReopt), and unwindReopts the times it was for leaving
	// native code on too many of its native callers' calls (jitRegUnwound).
	inlineReopt   bool
	upgradeReopts uint8
	unwindReopts  uint8
	// entrySlow marks code Go no longer enters (jitRegStretchWork); loopSlow
	// code whose native stretches within an invocation do not pay -- its
	// exits (jitRegStretchWork), its calls of Go (jitRegRunGoProfit) --
	// which Go does not enter by on-stack replacement either.
	entrySlow bool
	loopSlow  bool
	// typed marks typed code's (asmTwin), compiled at its module's link
	// with no speculation: no stretch, call of Go or exit judges it, as
	// none of them is a speculation's cost (jitRegStretchWork).
	typed bool
	// deferred marks a refusal for want of room in the JIT's budget, made at
	// the cache generation in generation: it is retried once that has moved
	// on.
	deferred   bool
	generation uint64
	// reoptRoom is how many bytes more than it holds the entry's last
	// compile needed and was refused, for want of room in the budget: the
	// room a compile again asks for before it compiles (jitRegReoptimize).
	reoptRoom int
	// bytes is what a function's register state holds, as last measured
	// (regAccount), which the cache counts against the JIT's budget while
	// it holds the state; busy marks a state whose function is being
	// compiled, which the cache does not let go of meanwhile to make room.
	bytes int
	busy  uint8
	// used is the cache's epoch (jitState.epoch) at which the state was
	// last found used -- its uses other than at the look before, seen --
	// or made; serial orders states by when they were made. The cache lets
	// the least recently used go to make room (makeRoom). flushSeen is its
	// uses at the cache's last look for code to flush (regFlush).
	used, seen, serial uint64
	flushSeen          uint64
	// slowCalls counts the calls the interpreter made, that did not enter
	// its code (entrySlow).
	slowCalls uint64
	// invoked is how many times the interpreter invoked the function
	// before it had code (jitRegInvoked, jitRegArm) -- invocations, not the
	// work its loops' back edges spent of its budget, which a call's
	// frequency is not made of (jitRegCalledOften). Its code keeps it
	// (jitRegCompileFor). See invocations.
	invoked int64
	// slowSince is slowCalls when Go last stopped entering the code, and
	// slowBackoff how many times it has: Go enters it again after
	// jitNativeRetry<<slowBackoff calls, judging it afresh (jitRegRetrySlow).
	slowSince   uint64
	slowBackoff uint8
}

// jitLink is where native callers call a function's code, as V8's code
// calls a function through the code it holds, whatever code that is now:
// the address of the code's entry at its start, which they jump to (mir's
// native calls, ssa.CallSite's Entry), or 0 while there is none they may
// -- no code, code that leaves too often (notNative), code the cache
// dropped -- when Go runs the function (abi.ExitEnter); and the calls they
// made to it since its code was last compiled, which they count
// (CallSite's Count). It outlives the function's entries: the cache may
// drop an entry, its code with it, and compile the function again, and
// the callers' code, which holds the link's address, calls the new code
// through it, never code that is gone, nor leaves for Go at every call
// for want of a target compiled since. Callers' code is compiled to call
// any code of the function: what it writes for the callee -- its
// arguments, its receiver if its body reads one (UsesThis), the receiver
// coerced -- is the function's, not one compile's.
//
// The function's link is found by its key (jitState.regLinks).
type jitLink struct {
	entry uintptr
	// body is the code's entry at its start, past its call entry, where a
	// call in tail position goes on in the frame it gives up (mir's
	// tailJump): 0 whenever entry is.
	body uintptr
	in   uint64
	// refs counts the code that calls through the link, which keeps it
	// while any does (regPruneLinks).
	refs int
	key  jitKey
	// The function's frame and flags (abi.LinkCoerce...), for a generic
	// call (abi.Encoding's Link fields).
	params, locals, frame, flags uint64
	// hint is, for a register link (regLinkOf), its slot of the hint
	// table (regHint), which generic calls find it by; 0 for none.
	hint uint32
	// gen is, for a generator's function, its code's generator entry,
	// where a loop's step that resumes one natively jumps (ssa.CallSite's
	// Gens), or 0 while there is none it may: 0 whenever entry is, for
	// the same reasons, and for a generator's code entry always is.
	gen uintptr
}

// setCode has native callers call code through l: its call entry, which
// makes the callee's frame (abi.Context's ReturnTo), and its start, where
// a call in tail position goes on; and a generator's steps its generator
// entry. Code with no call entry has Go run the function at each native
// call (abi.ExitEnter).
func (l *jitLink) setCode(code *jit.SSACode) {
	l.entry, l.body, l.gen = code.CallEntry(), code.EntryAddress(0), code.GenEntry()
	if l.entry == 0 {
		l.body = 0
	}
}

// clearCode has native callers call l's function's code no more: they
// leave its calls, and its generators' steps, to Go.
func (l *jitLink) clearCode() {
	l.entry, l.body, l.gen = 0, 0, 0
}

// Weak keys prevent a refusal or cached program from retaining a source graph.
// Native entry cannot call Go, so buffers are shared only until a guard exit.
type jitState struct {
	// manyRealms says a look of the cache's has found code kept for
	// another realm than the current one's (jitFlushRealms).
	manyRealms bool
	// recompiled counts the compiles of functions whose code the cache
	// had let go of (regLetGo).
	recompiled uint64
	// unavailable marks a runtime the OS will not give executable memory.
	unavailable bool
	// reporting marks a runtime whose JIT notes why its runs ended in the
	// interpreter and what its calls of Go made, for RegNativeStats
	// (SetJITReport).
	reporting bool
	// rt is the runtime s is the state of, whose current realm is the
	// realm of the entries it asks for (jitKey).
	rt *Runtime
	// regCache is the code compiled from functions' register code, by the
	// same keys (jit_native.go): the cache's other form.
	regCache map[jitKey]*jitRegEntry
	// regLinks are the links native callers call functions' register code
	// through, by the same keys (jit_call.go).
	regLinks map[jitKey]*jitLink
	// regGoOn is the frame the interpreter runs where register code left
	// it with no entry, and regForced marks the interpreter's check made
	// due at once at its next back edge, where Go goes on in its code
	// (jitRegGoOnAt, jitRegLoop); regGoOnFrom is the code whose run left
	// it, which a frame that goes on in it again went on in already
	// (jitRegLoopCode). regGoOnOSR says whether that run was
	// begun at a loop's header, by on-stack replacement (jitRegStretch's
	// osr), and regGoOnRun hands it to the run that goes on with the
	// frame (jitRegGoOnAt): 1 for a run begun at the function's start, 2
	// for one begun at a loop, 0 for a run of its own.
	regGoOn     *frame
	regGoOnFrom *jitRegEntry
	regForced   bool
	regGoOnOSR  bool
	regGoOnRun  uint8
	// regPaused is the frame of a native call a poll unwound, which Go goes
	// on with in its code (jitRegFinishLevel): a poll is a pause, and the
	// call leaves native code, as its native callers count it
	// (jitRegUnwound), at the first exit after it that is no poll nor its
	// return.
	regPaused *frame
	// regKeptSeeding marks the compiles of register callees that give up
	// the entries kept for a frame (jitRegCalleesKeptNoEntries), which ask
	// for no more of them.
	regKeptSeeding bool
	// regHeld is what the register code in regCache takes (held), and
	// what the JIT keeps for each function besides, its learned state in
	// regLearned (regAccount).
	regHeld int
	// regLearned is what the code made from a function's register code
	// learned at its exits, by the function's key, kept across its
	// compiles: an entry holding only what it learned (jitLearned), by
	// register pc -- the pcs whose guards failed, which its next compiles
	// build generic (jitRegFeedback's Generic), and the property sites'
	// cases, primitives and megamorphic marks (jitRegPropertySeen).
	// regSerial numbers the
	// register code's compiles (jitRegEntry's serial).
	regLearned map[jitKey]*jitEntry
	regSerial  uint32
	// regLowered are the programs of functions' register code, by the
	// function's key (jitRegLower).
	regLowered map[jitKey]*jitLowering
	// regAhead are the lowerings ahead under way or done, not taken yet,
	// by what they lower, regAheadRoots by the function whose compile
	// waits for them (jit_lowerahead.go).
	regAhead      map[jitAheadKey]*jitAheadJob
	regAheadRoots map[jitKey]*jitAheadJob
	// hintsTaken are the closures whose functions' hints the compiles under
	// way, hintsDepth of them, have taken (jitHintsOnce).
	hintsTaken []*closure
	hintsDepth int
	// aheadJobs counts the lowerings ahead handed to the workers,
	// aheadTaken the programs taken of them.
	aheadJobs, aheadTaken uint64
	// regLowerLook is the cache's age (agedAt) at the flush's last look at
	// regLowered, before which a program not asked for since is let go of
	// (regFlush).
	regLowerLook uint64
	// regBuilds are the feedback of the register compiles under way on
	// the runtime's goroutine, built or installed (jitRegCompile,
	// jitRegInstall): the links their code is to call through, which no
	// code holds yet (jitLink's refs), are let go of by none meanwhile
	// (regUnlink).
	regBuilds []**jitRegFeedback
	// regHostOps counts register code's host exits by the register
	// instruction Go ran whole, by its operation, for tests
	// (RegNativeStats' HostOps): an array, not a map of the operation's
	// name, whose hashing and string had cost each exit more than the
	// rest of counting it.
	regHostOps [bytecode.ROpCount]uint64
	// regEnded counts the runs of register code that ended in the
	// interpreter (interpreted) by why they did, the instruction they
	// left at and its function, while the JIT reports (RegNativeStats'
	// Ended); regEndWhy is why jitRegLeft last left a frame to it.
	regEnded  map[string]uint64
	regEndWhy string
	// wholeLeft counts the instructions Go ran whole for native code that
	// did not go on natively (jitRegWhole), for tests (RegNativeStats').
	wholeLeft uint64
	// regGoOps counts register code's calls of Go that may run script by
	// what they made (noteRegGoCall), while the JIT reports
	// (RegNativeStats' GoCallOps).
	regGoOps map[jitRegGoKey]uint64
	// callRefs bounds the pointer words any code's native calls write
	// themselves to their contexts' RecordRef (jit.SSACode's CallRefs),
	// which a run's end clears (jitRegStretch).
	callRefs uint64
	// megaGC is the collector's cycle (jitGCEpoch) the megamorphic cache
	// was last emptied at (flush).
	megaGC uint64
	// resumed counts native calls whose callee Go finished, after which
	// their callers' code went on natively (jitRegUnwind); resumedLevels
	// those whose caller was itself a level Go was finishing.
	resumed, resumedLevels uint64
	// loopsFinished counts the loops of built-ins' calls Go went on with
	// at an exit (jitRegLoopFinish).
	loopsFinished uint64
	// sortsRestarted counts the sorts native code made (ssa's sortCall)
	// that left it, which Go then sorted again from the first.
	sortsRestarted uint64
	// materialized counts the closures Go made at exits that native code
	// had not made (jitRegMaterialize).
	materialized uint64
	// searchArgs are a search's arguments (jitSearch), given to its Go
	// function, which keeps nothing of them, and cleared after.
	searchArgs [2]Value
	// ssaShared is what the contexts from ssaSharedFrom on hold of what
	// they share (jitShareContexts); ssaSharedGen counts the times either,
	// or bounds, changed, which a call of Go compares, not the struct.
	ssaShared     jitShared
	ssaSharedFrom int
	ssaSharedGen  uint64
	// bounds are how deep the contexts' native calls may go, which they
	// all point to (abi.Bounds), and jitShareContexts writes; levelBound
	// the LevelLimit the VM's frames alone would give, which the contexts
	// grow to (jitGrowContexts).
	bounds     abi.Bounds
	levelBound uint64
	// ctxShrunk counts the times contexts were given back
	// (jitShrinkContexts), and ctxMost is the most there have been.
	ctxShrunk uint64
	ctxMost   int
	// hintTable is hints for native code (abi.HintTable), which every
	// context points to, as they do to mega (abi.MegaCache).
	hintTable abi.HintTable
	mega      *abi.MegaCache
	// charStrings are the runtime's strings of one character below 256, by
	// code unit, which native code makes a string's elements and
	// String.fromCharCode's of (abi.Context.CharStrings): the strings
	// unitString hands out.
	charStrings *[256]Value
	// typeofCells are the runtime's strings typeof answers, in
	// typeofNames' order, which native code loads typeof's answer from
	// (ssa.OpTypeofCell): typeofString's strings.
	typeofCells *[8]Value
	// builders are the strings loops' appends build (jitAppend), until
	// native code leaves for Go (jitFreezeBuilders); appended counts the
	// appends made in place.
	builders []jitBuilder
	appended uint64
	// arena holds the code of every entry: a few mappings, however many
	// functions the runtime compiles.
	arena *jit.Arena
	// regexpHost places the runtime's patterns' native code in the arena,
	// within the budget (jitRegExpNative); nil until the first pattern.
	regexpHost *regexp.NativeHost
	// generation advances whenever the cache releases code, which is when a
	// program refused for want of budget may fit.
	generation uint64
	// hints holds each register link a closure remembers in a slot, from 1
	// (jitCodeFields, hint; regHint); hintFree are the slots free.
	hints    []jitHintSlot
	hintFree []uint32
	// budget is what the JIT may hold (jitBudget), as of the last compile;
	// lowerBytes what its lowerings hold (jitLowering.bytes), which, with
	// its register code (regHeld) and what the runtime's JIT holds besides
	// (sharedBytes), it holds (held).
	budget     int
	lowerBytes int
	// perInstr is about how many bytes of the budget an entry holds for
	// each instruction of its function's bytecode, from the last compiles:
	// how much room a compile is likely to need (makeRoom).
	perInstr int
	// victims is makeRoom's list of what it may reclaim, in the order it
	// does, made at victimsAt (1 plus the epoch) and taken from at
	// victimPos by the looks after it in the epoch; victimLeft is what the
	// rest held when listed.
	victims     []jitVictim
	victimsAt   uint64
	victimPos   int
	victimLeft  int
	hosts       uint64
	entries     uint64
	guards      uint64
	budgets     uint64
	osrs        uint64
	compiled    uint64
	reoptimized uint64
	// unwound counts the native calls Go finished (jitRegUnwind) after their
	// callees left native code or paused at a poll, and callsLeft those
	// whose callees left it: at an exit, not a poll, or after a poll paused
	// them (jitRegUnwound).
	unwound, callsLeft uint64
	// invalidated counts the codes invalidated (jitRegInvalidate), and
	// topDeopts those invalidated at a site already built at the top of its
	// ladder (jitRegSiteDeopted), which widens it no further: none should
	// be, and the tests that run code deoptimizing over and over check it.
	// siteTop says the deoptimization counted last was such a one, for the
	// tier trace's (JITTierEvent's Top).
	invalidated, topDeopts uint64
	siteTop                bool
	// shapeDeopts counts the deoptimizations at property sites of code Go
	// entered, for a shape the code was not compiled for or a site that
	// knew none (jitRegDeoptSeen), and shapeHosts such exits inside
	// inlined callees, which leave the operation to Go (jitRegInlineLeft):
	// among guards and hosts, the exits that feed the sites' caches.
	shapeDeopts, shapeHosts uint64
	// budgetSeed varies the back-edge budget (backEdgeBudget), which is
	// stressBudget under QJS_JIT_STRESS's budget.
	budgetSeed   uint32
	stressBudget int
	// charCodeAtKey is the atom jitStringMethod looks charCodeAt up by,
	// once charCodeAtKnown.
	charCodeAtKey   Atom
	charCodeAtKnown bool
	// pollCredit is how much the budgets calls of Go gave loops (ssa.GoPoll)
	// added to the back-edge counter: a native stretch's work, estimated
	// from how far the counter fell (jitRegStretchWork), adds it back.
	pollCredit int
	// lowered are the functions' programs (jitRegLower), and whether each may
	// be inlined (its jitLowerInline lowering's error).
	lowered     map[weak.Pointer[bytecode.Function]]*jitLowering
	interpreted uint64
	// slowResumes counts the looks at code judged slow that resumptions
	// made, each for the next jitSSAProbe too (jitRegEnter).
	slowResumes uint64
	// osrLearned counts the loops' entries compiled apart from their
	// functions' code (jitRegLoopCode).
	osrLearned  uint64
	wentOn      uint64
	stressExits uint64
	// ssaCtxs are the contexts native code runs in, one for each run of
	// native code (jitRegStretch) and each native call (abi.Context's
	// Level), ctxTop the
	// first free; ssaCtx is the one Go entered last, whose exit it reads.
	// They are made jitContexts at a time, linked (abi.Context.Next), and
	// never move: more are added while native code runs in them
	// (jitGrowContexts).
	ssaCtxs []*abi.Context
	ctxTop  int
	ssaCtx  *abi.Context
	// goRuns are, by context, what a run of native code began there has
	// pinned (jitRegPinLevel); goPinned the code calls of Go are suspended
	// in, innermost last, and retired the code compiled again or dropped
	// meanwhile, closed once none is (closeSSA). goCalls counts the calls
	// of Go native code made.
	goCalls uint64
	// regGoMark is goMark for register code: the last call of Go a native
	// callee's code made (jitRegCalleeProfit).
	regGoMark jitRegGoMark
	// lazy are the native levels whose frames calls of Go put off, the
	// innermost call's last; the Runtime's jitLazyTop is the depth below
	// which they are (jitLazyLevels).
	lazy []jitLazyLevels
	// lazyCalls counts the calls that put frames off, lazyMade the times
	// frames put off were made, and lazyHeld the codes not closed for them.
	lazyCalls, lazyMade, lazyHeld uint64
	goRuns                        []jitGoRun
	// calleeOpen is, by context, what closures a native callee made
	// captured of its locals, not closed yet (abi.Context's Open), and
	// calleeFrame the frame a callee's context stands for while Go makes
	// one or closes them (jitCalleeFrame).
	calleeOpen  [][]*upvalue
	calleeFrame frame
	goPinned    []*jit.SSACode
	retired     []jitRetired
	// regRuns are the register frames native code made from register
	// code runs on, by context (jitRegStretch), which its calls of Go
	// make theirs on (jitRegGoCall); regPending is a call of Go one made
	// that its exit finishes.
	regRuns    []jitRegRun
	regPending jitGoPending
	// regNestLevels and regNestFrames hold, as a stack, the levels a call
	// of Go from a native callee's code under way runs past and the frames
	// Go made of them (jitRegGoCallNested): such calls nest, script they
	// run making native calls that call Go, each taking its stretch past
	// the one it runs in.
	regNestLevels []jitRegLevel
	regNestFrames []*frame
	// regRunAt is one past the context of the run of register code whose
	// native code runs now (jitRegStretch), 0 where none does or a stack
	// form's run is the innermost: the run a call of Go from a native
	// callee's code is of (jitRegRootOf), known without a walk down its
	// levels.
	regRunAt int
	// regLazyLevels and regLazyFrames hold, as a stack, the levels of
	// calls of Go from native callees' code that put them off
	// (jitLazyLevels' reg) and the frames Go made of them once one was
	// read (jitRegMakeLazy).
	regLazyLevels []jitRegLevel
	regLazyFrames []*frame
	// regGens says a register function's link has had a generator entry
	// (jitLink's gen): only then may a native callee's level be a
	// generator's resumption, which a call of Go makes the frames of at
	// once (jitRegGensIn).
	regGens bool
	// regRoom is the most slots any register code compiled takes past its
	// window (jitRegEntry's scratch and frame), which the contexts' StackEnd
	// leaves below the VM's stack's end (jitStackEnd): a native call of
	// register code, or a call in tail position, checks the room for the
	// callee's window alone, a constant, and its code's frame fits past it.
	regRoom uint64
	// regLevelsFound counts the levels of native calls Go found the code
	// of and pinned (jitRegLevelAt): the work a call of Go from a native
	// callee's code does for the levels below it.
	regLevelsFound uint64
	// regNestedCalls counts the calls of Go from native callees' code
	// (jitRegGoCallNested), and regLazyMade the times the levels such a
	// call put off were made (jitRegMakeLazy), and regLazyLevelsPut the
	// levels such calls put off.
	regNestedCalls, regLazyMade, regLazyLevelsPut uint64
	// regRetired are register entries the cache let go of while their
	// code was in use -- runs in it, native calls of it Go has the levels
	// of, or pinned (goPinned): a native call of it under way, which a
	// call of Go from its level returned to -- where a level running it
	// finds it (jitRegLevelAt), closed once none is (regClose,
	// jitRegCloseRetired).
	regRetired []*jitRegEntry
	// regJobs are the register compiles in the background, in the order
	// they were handed over, and regPendingJobs each by its function's
	// key (jit_bg.go).
	regJobs        []*jitRegJob
	regPendingJobs map[jitKey]*jitRegJob
	// regSnapMisses counts the register compiles in the background whose
	// build asked what their snapshot was not asked, built at the install.
	regSnapMisses uint64
	// regResumes counts the suspended frames of register code entered in
	// their native code where they resumed (jitRegEnter from regGenRun).
	regResumes uint64
	ssaEntries uint64
	ssaRecords uint64
	// rehomed counts the registers Go brought up to date at loops' polls
	// (jitRegRehome), and rehomeDue the polls that may to pass over before
	// the next that does.
	rehomed   uint64
	rehomeDue int
	// backendPanics counts the compiles a backend refused for panicking.
	backendPanics uint64
	// inlined counts the callees the code placed inlines, and inlinedCode
	// their instructions (ssa.Func's Inlined, InlinedCode).
	inlined, inlinedCode uint64
	// bigLeafs counts the BigInts' operators native code had made
	// without a call of Go (jitBigLeaf), bigLeafNew the new blocks it
	// made them in, none having been given back.
	bigLeafs, bigLeafNew uint64
	// leafFast counts the leaves' calls native code made through the VM's
	// routine for them (jitLeafGo), leafRefused those it left to an exit.
	leafFast, leafRefused uint64
	// goFast counts the other calls of Go native code made through the
	// VM's routine for them (jitGoDirect), goStub those made through a
	// routine of their own (abi.Encoding's GoStubs, jit_gostub.go).
	goFast, goStub uint64
	// ssaKeeps is the most keep cells (abi.Context.Keep) any code compiled
	// uses: what Go clears in a native callee's context.
	ssaKeeps int
	// ssaSpills is the most spill slots any code placed uses (mir's
	// Code.Spills): every context has room for them (abi.Context's
	// SpillMore), so that the code may run at any level, entered by Go or
	// by a native caller (spillsFor).
	ssaSpills int
	// exitScratch is what exits that leave their frames to Go are read
	// into (jit.ExitScratch).
	exitScratch jit.ExitScratch
	// scans counts the cache's looks for what to reclaim (makeRoom),
	// evicted the entries dropped to make room, flushed those
	// dropped for going unused (flush), and refusedRoom the times there was
	// none to be made. floor is what the
	// JIT held at the last look that it could not reclaim -- young code,
	// code that runs, what the runtime's JIT needs -- and floorAt 1 plus the
	// epoch of that look, or 0 once something it held has gone since: until
	// then, the floor only rises, and a need past the budget above it is
	// refused without another look. agedAt is 1 plus the epoch the cache
	// last found its entries' uses in (age), and flushedAt the flush's tick
	// at its last look for code to flush (flush).
	scans, serial, evicted, flushed, refusedRoom uint64
	floor                                        int
	floorAt, agedAt, flushedAt                   uint64
	// settling marks settle's look past the budget, which takes young
	// code too (regVictims, regReclaim); inSettle, settle itself, whose
	// looks settle nothing again (keepWithin); and settleAfter counts the
	// compiles and installs under way, each of which ends with a settle.
	settling, inSettle bool
	settleAfter        uint16

	// What follows is last so that it moves no field before it, which
	// the interpreter's and the native code's calls of Go read.
	//
	// copies are the object spreads Go learned to make by their layouts
	// (jitCopyFast), made the first time one is learned.
	copies *[jitCopyEntries]jitCopyEntry
	// arrayIter is where arrays find their iteration's methods, to spread
	// them by their elements (jitArrayIterates).
	arrayIter jitArrayIter
	// The cache's clocks' time (running): from clockFrom, less
	// compileTime, what the compiles took, and, compiling, the compile
	// begun at compileFrom; compiling counts the compiles begun within
	// it.
	clockFrom, compileFrom time.Time
	compileTime            time.Duration
	compiling              int
	draining               bool
	// onSpot, while set, has a runtime that compiles in the background
	// compile on the spot (jitInBackground): a typed link's compile the
	// background has no room for (jitTypedLinked).
	onSpot                   bool
	bgSubmitted, bgDiscarded uint64
	// bgTakenBack counts the compiles in the background taken back to be
	// made on the spot, no worker having begun them (jitRegTakeBack).
	bgTakenBack uint64
	// bgDropped counts the compiles in the background dropped unbegun,
	// their functions gone unused (jitRegDropStale), and jobsTick is the
	// flush's tick that last looked for them.
	bgDropped, jobsTick uint64
	// goroutineTimes is the runtime goroutine's part of its compiles,
	// timed while JITCompileClock is set, timingGoroutine while one is
	// being timed (JITGoroutineTimes).
	goroutineTimes  JITGoroutineTimes
	timingGoroutine bool
	// hookTime is the time the host's hooks took while a compile was
	// timed (jitHookTimed), which its time leaves out.
	hookTime time.Duration
	// arrayRooms are pools with large inherited hints. The weak list
	// releases completed samples without keeping discarded code's pools.
	arrayRooms []weak.Pointer[abi.ObjectPool]
	// plainWrites counts the property writes Go made without a frame for
	// native code (jitGoCallPlain's, RegNativeStats' PlainWrites).
	plainWrites uint64
	// enc and opts are how the runtime's compiles are made: the process's
	// (jitEncoding, mir.Settings), but where a test made the runtime
	// otherwise (jitSetEncoding).
	enc  abi.Encoding
	opts mir.Options
	// regParked is the code parked as the runtime returned to its host,
	// out of regCache, by its function's key (jitRegPark), and parkedOldest
	// no later than the flush's tick the oldest of it was parked at
	// (regAgeParked).
	regParked    map[jitKey]*jitRegEntry
	parkedOldest uint64
	// parkRun is parking's clock (parkClock) as the last parking ended,
	// and parkCost what it and the unparkings since cost by it (parkDue).
	parkRun, parkCost time.Duration
	// parkedPools are the object pools of parked code, each with what its
	// source holds weakly (jitParked's pools).
	parkedPools map[*abi.ObjectPool]*jitParked
	// parkings counts the parkings, parkedCount the code parked, and
	// unparked the code put back in the cache as it was (jitRegUnpark),
	// for tests and the report (JITStats).
	parkings, parkedCount, unparked uint64
}

func (r *Runtime) initJIT(enabled bool, memoryLimit int64) {
	r.jitMemoryLimit = memoryLimit
	// Debugger interrupt callbacks may evaluate code and mutate frames or arrays.
	// Native budget exits retain borrowed views, so debugger runtimes stay in Go.
	r.jitEnabled = enabled && r.debug == nil
	r.jitCallThreshold, r.jitICThreshold = jitInvocationsForMaglev, jitInvocationsAfterICUpdate
	r.jitStress = jitStressDefault
	if t := r.test; t != nil {
		r.jitTest = t.jit
	}
	if r.jitStress.threshold {
		r.jitCallThreshold, r.jitICThreshold = 1, 1
	}
}

// jitHeldBytes is what the JIT holds for the runtime (jitState.held).
func (r *Runtime) jitHeldBytes() int64 {
	if r.jit == nil {
		return 0
	}
	return int64(r.jit.held())
}

// jitMapSlot is what the JIT counts for an entry of a map it keeps by a
// function's weak key, besides the entry's value: the key, the value's
// word, a share of the map's table, and the weak pointer's record on the
// function.
const jitMapSlot = 64

// What the JIT counts for each thing it keeps, apart from the slices it
// holds: a link, a lowering, an object pool, a context.
var (
	jitEntryFixed    = int(unsafe.Sizeof(jitEntry{})) + int(unsafe.Sizeof(jitLearned{})) + int(unsafe.Sizeof(jitHintSlot{})) + 2*jitMapSlot
	jitLinkFixed     = int(unsafe.Sizeof(jitLink{})) + jitMapSlot
	jitLoweringFixed = int(unsafe.Sizeof(jitLowering{})) + jitMapSlot
	jitPoolFixed     = int(unsafe.Sizeof(abi.ObjectPool{})) + int(unsafe.Sizeof(jitPoolSource{}))
	jitContextBytes  = int(unsafe.Sizeof(abi.Context{}))
	// jitContextShare is what a context holds, with its places in what is
	// kept by context (sharedBytes).
	jitContextShare = jitContextBytes + int(unsafe.Sizeof((*abi.Context)(nil))) + int(unsafe.Sizeof(jitGoRun{})) + int(unsafe.Sizeof([]*upvalue(nil)))
)

// jitCapBytes is the memory s's backing array holds.
func jitCapBytes[T any](s []T) int {
	var t T
	return cap(s) * int(unsafe.Sizeof(t))
}

// learnedBytes is what e holds of what it has learned, which outlives its
// compiles: the sites that failed, its calls, literals and reads met with
// more shapes, and the object pools of its literals and constructions,
// with the objects made for them.
func (e *jitEntry) learnedBytes() int {
	n := jitCapBytes(e.failed) + jitCapBytes(e.failedEntries) + jitCapBytes(e.siteDeopts) + jitCapBytes(e.resumed) + jitCapBytes(e.bigints) + jitCapBytes(e.bigwide) + jitCapBytes(e.literals) + jitCapBytes(e.poly) + jitCapBytes(e.typedReads) +
		jitCapBytes(e.stringReads) + jitCapBytes(e.keyedReads) + jitCapBytes(e.keyedWrites) + jitCapBytes(e.strSites) + jitCapBytes(e.bigSites) + jitCapBytes(e.numSites) + jitCapBytes(e.boolSites) + jitCapBytes(e.nativeCalls) + jitCapBytes(e.genericCalls) + jitCapBytes(e.viaLeaves) + jitCapBytes(e.inlines) + jitCapBytes(e.notInline) +
		jitCapBytes(e.regCallers) + jitCapBytes(e.callSites) + jitCapBytes(e.settling)
	for i := range e.poly {
		p := &e.poly[i]
		n += jitCapBytes(p.cases) + jitCapBytes(p.reads) + jitCapBytes(p.adds) + jitCapBytes(p.missed)
	}
	for i := range e.literals {
		n += jitPoolBytes(e.literals[i].pool)
	}
	n += jitPoolBytes(e.cellsPool)
	for i := range e.creates {
		n += jitPoolBytes(e.creates[i].pool)
	}
	for _, calls := range [2][]jitInline{e.inlines, e.nativeCalls} {
		for i := range calls {
			n += jitPoolBytes(calls[i].pool)
		}
	}
	return n
}

// jitPoolBytes is what pool holds: itself, and as many objects as it is
// filled with (Size), each as big as the last it holds.
func jitPoolBytes(pool *abi.ObjectPool) int {
	if pool == nil {
		return 0
	}
	n := jitPoolFixed
	if pool.Cells != 0 {
		// A function's cells (jitFillCells): the pointers, then the cells.
		return n + int(pool.Size)*int(pool.Cells)*(8+int(unsafe.Sizeof(upvalue{})))
	}
	if src := (*jitPoolSource)(pool.Source); src != nil && src.tmpl.template() != nil {
		// A closure's functions (jitFillClosures): an arrow's object, its
		// extra beside it, the most a function's is, and its cells.
		return n + int(pool.Size)*(int(unsafe.Sizeof(arrowFuncObject{}))+8*len(src.tmpl.template().fn.Upvalues))
	}
	if c := pool.Count; c > 0 && c <= abi.PoolCapacity {
		o := (*Object)(pool.Objects[c-1])
		n += int(pool.Size) * (int(objectSize) + jitCapBytes(o.vals()) + jitCapBytes(o.elemsOf()))
	}
	return n
}

// held is what the JIT holds for the runtime: its register code and its
// functions' state (regHeld), the programs it has lowered (jitRegLower),
// and what it holds besides (sharedBytes).
func (s *jitState) held() int {
	return s.lowerBytes + s.sharedBytes() + s.regHeld
}

// spillShare is what each context holds of spill slots past its own
// (abi.Context's SpillMore).
func (s *jitState) spillShare() int { return 8 * max(s.ssaSpills-abi.SpillSlots, 0) }

// spillRoom is what giving every context room for the spill slots of mc
// adds to what the JIT holds: nothing for code of no more than the most
// placed so far.
func (s *jitState) spillRoom(mc *mir.Code) int {
	return s.ctxCounted() * 8 * max(mc.Spills-max(s.ssaSpills, abi.SpillSlots), 0)
}

// ctxCounted is how many of the contexts the budget counts: the first
// chunk, which a runtime running native code holds whatever its depth --
// the rest are the native levels' stack, bounded by the VM's depth limit
// and given back once unused (jitContexts, jitShrinkContexts).
func (s *jitState) ctxCounted() int { return min(len(s.ssaCtxs), jitContexts) }

// spillsFor gives every context room for the spill slots of mc, placed:
// a level reached later is given it as it is made (jitGrowContexts). No
// code's entry or call checks the room, which nothing takes back while
// the runtime lives.
func (s *jitState) spillsFor(mc *mir.Code) {
	if mc.Spills <= s.ssaSpills {
		return
	}
	s.ssaSpills = mc.Spills
	for _, c := range s.ssaCtxs {
		c.GrowSpill(mc.Spills)
	}
}

// sharedBytes is what the JIT holds for the runtime apart from its entries
// and lowerings: its state; the first chunk of the contexts native code
// runs in, with their places in what is kept by context (ctxCounted) --
// not the native levels' stack past it, which the VM's depth limit
// bounds, as V8's stack is not of its code space; its hint table and
// links; and code compiled again or dropped that a call of Go is
// still suspended in (retired). Not the workspace a compile is made in,
// which is the process's, the runtime's only while it compiles
// (jit.Workspace), a compile's working memory as V8's compile zones are
// not its code space: they hold what the recent compiles needed
// (ssa.Workspace's Rewind), and counted against the budget they had code
// dropped for them and were let go themselves to make room, for the next
// compile to make again -- Octane's Typescript, its median compile a
// megabyte and its biggest forty, allocated twice as much as with them
// kept, Box2D three times, and dropped dozens of functions' code.
func (s *jitState) sharedBytes() int {
	n := int(unsafe.Sizeof(*s)) + s.ctxCounted()*(jitContextShare+s.spillShare()) + jitCapBytes(s.hints) +
		jitCapBytes(s.hintFree) + jitCapBytes(s.victims) +
		jitCapBytes(s.goPinned) + jitCapBytes(s.retired) + jitCapBytes(s.arrayRooms)
	for _, x := range s.retired {
		n += x.code.Footprint()
	}
	if s.regexpHost != nil {
		n += s.regexpHost.Held()
	}
	return n
}

// jitVictim is what the cache may reclaim to make room (makeRoom): a
// lowering or an entry, by its class.
type jitVictim struct {
	key jitKey
	// e is the register state of a state's victim (regReclaim).
	e            *jitEntry
	re           *jitRegEntry
	l            *jitLowering
	reg          bool
	class        uint8
	used, serial uint64
	bytes        int
}

// The classes of what the cache reclaims, in the order it does: a
// function's programs, lowered again at a compile's cost of a few percent;
// a refusal remembered, which spares a compile only; then code, which
// costs a whole compile to make again.
const (
	jitVictimLowering = iota
	jitVictimRefusal
	jitVictimCode
)

// makeRoom reclaims what the JIT holds, until it holds need bytes less
// than its budget, and reports whether it does: as V8 bounds its code, and
// all that describes it, by memory, reclaiming code by its age, not by how
// many functions have it. It reclaims, in their order (jitVictim's
// classes), the least recently used first: programs lowered for functions
// the cache has no entry for; refusals remembered; and code not used in this
// epoch or the one before (jitCodeAge), as V8 flushes only code old
// enough. A function's code is never dropped for another's while it is
// young: a program with more hot functions than the budget holds had
// compiled each at every pass, each dropping the next one run. Nor an
// entry whose code
// runs (inUse), or that is being compiled (busy). If even all it may
// reclaim would not make the room, it reclaims nothing, and the newcomer
// runs in the interpreter until some code is old.
func (s *jitState) makeRoom(need int) bool {
	held := s.held()
	if held+need <= s.budget {
		return true
	}
	now := s.epoch()
	if s.floorAt == now+1 && s.floor+need > s.budget {
		// No more could be reclaimed at a look in this epoch, and nothing
		// held then has gone since.
		s.refusedRoom++
		return false
	}
	s.age(now)
	if held = s.held(); held+need <= s.budget {
		return true
	}
	if len(s.regParked) != 0 {
		// Code parked as the runtime returned to its host, which nothing
		// has used since, goes first (jitRegPark).
		for key, e := range s.regParked {
			s.regUnparkFailed(key, e)
		}
		if held = s.held(); held+need <= s.budget {
			return true
		}
	}
	// The order listed at this epoch's first look, taken from where the
	// last look left it: listing and sorting all the cache holds at each
	// look made a run of compiles that each found the budget full
	// quadratic -- a loop evaluating sixty thousand scripts, each a
	// refusal the cache remembers, took minutes. What was listed but has
	// gone, or is no longer reclaimable, is passed over; listed anew once
	// the rest, as listed, could not make the room.
	if s.victimsAt != now+1 || held-s.victimLeft+need > s.budget {
		if !s.listVictims(now, need) {
			s.refusedRoom++
			return false
		}
	}
	for s.victimPos < len(s.victims) && s.held()+need > s.budget {
		x := &s.victims[s.victimPos]
		s.victimPos++
		s.victimLeft -= x.bytes
		s.reclaim(x, now)
	}
	if s.victimPos == len(s.victims) {
		// Let go, not kept empty: it is listed anew at the next look, and
		// what it held counted against the budget meanwhile.
		s.victims, s.victimsAt, s.victimLeft = nil, 0, 0
	}
	return s.held()+need <= s.budget
}

// listVictims lists, in makeRoom's order, what the cache may reclaim at
// epoch now, for makeRoom's looks in the epoch to take from, and reports
// whether reclaiming it all would make room for need; if not, it lists
// nothing, and the floor is noted.
func (s *jitState) listVictims(now uint64, need int) bool {
	s.scans++
	clear(s.victims)
	v, reclaimable := s.victims[:0], 0
	for key, l := range s.lowered {
		v = append(v, jitVictim{key: jitKey{Pointer: key}, l: l, class: jitVictimLowering, used: l.used, bytes: l.bytes})
	}
	v = s.regVictims(v, now)
	for i := range v {
		reclaimable += v[i].bytes
	}
	// The floor is what would stay held were it all reclaimed: not the
	// list kept from the epoch's last look either, which goes, as this one
	// would once its last is taken (makeRoom). Counted, it made the floor
	// its size too high: a compile at a budget within that of the room was
	// refused with room for it.
	if floor := s.held() - jitCapBytes(s.victims) - reclaimable; floor+need > s.budget {
		s.floor, s.floorAt = floor, now+1
		s.victims, s.victimsAt, s.victimPos, s.victimLeft = nil, 0, 0, 0
		return false
	}
	slices.SortFunc(v, func(a, b jitVictim) int {
		if a.class != b.class {
			return int(a.class) - int(b.class)
		}
		if a.used != b.used {
			return cmp.Compare(a.used, b.used)
		}
		return cmp.Compare(a.serial, b.serial)
	})
	s.victims, s.victimsAt, s.victimPos, s.victimLeft = v, now+1, 0, reclaimable
	return true
}

// reclaim drops x, a victim makeRoom listed, if it is still one at epoch
// now -- still held, not running, not being compiled, and code still old.
func (s *jitState) reclaim(x *jitVictim, now uint64) {
	switch x.class {
	case jitVictimLowering:
		if x.reg {
			s.regTrimLowering(x.key, x.l)
			return
		}
		if l := s.lowered[x.key.Pointer]; l == x.l {
			s.dropLowering(x.key.Pointer, x.l)
		}
	default:
		s.regReclaim(x, now)
	}
}

// jitFlushAge is how many of the flush's ticks (flushNow) apart the
// cache looks for code to flush (flush): code unused between two looks
// is, a second of the program's apart. V8's code ages so before it is
// flushed. A test of the cache's room sets its runtime's past any tick it
// reaches (jitTestConfig's flushAge).
const jitFlushAge = 20

// flushAge is the runtime's jitFlushAge.
func (s *jitState) flushAge() uint64 {
	if a := s.rt.jitTest.flushAge; a != 0 {
		return a
	}
	return jitFlushAge
}

// flushNow is the flush's clock: jitEpochTime's ticks of the program's
// time (running) alone. The cache's epochs count the collector's cycles
// too, which a program that collects all the while -- a test's collector,
// run in a loop -- has go by within a call: code a function runs once a
// call was flushed within it.
func (s *jitState) flushNow() uint64 {
	t := &s.rt.jitTest
	if f := t.flushNow; f != nil {
		return f()
	}
	if c := t.clocks; c != nil {
		if tick, ok := c(s); ok {
			return tick
		}
	}
	if jitClocksHeld.Load() {
		return 0
	}
	return uint64(s.running() / jitEpochTime)
}

// flush drops, once every jitFlushAge ticks, the entries unused since its
// last look, whatever room the budget has, as V8 flushes code that has
// gone unused for long: what an entry learned and compiled holds the
// objects it was compiled for -- the callees its calls were seen to call,
// the closures it inlined, the holders its checks compare -- and its
// function's bytecode, which keys it, outlives them all, kept by the
// function that made the closures. Octane's zlib, once run, had kept its
// 128 MB heap so, through the closures its own code had called. An
// entry whose code runs, or young (jitCodeAge), stays (regFlush). What
// its code learned of its sites is kept (regLearned), and the function is
// compiled again once it is hot again. It looks at a back-edge check
// (backEdgeBudget), before a compile (age) and as the runtime returns to
// its host (jitReturned).
func (s *jitState) flush() {
	if g := jitGCEpoch.Load(); g != s.megaGC {
		// A collection ended since the last look: the megamorphic cache
		// (abi.MegaCache) is emptied, as V8 clears its stub caches after
		// every full collection (heap.cc's ClearStubCaches). An entry
		// holds its shape, which holds its prototype, and so the
		// prototype's methods and every binding they capture: kept until
		// another shape took its place, a finished program's shapes had
		// kept its whole heap alive -- AWFY DeltaBlue's, 17 MB, through
		// Havlak, every collection of which marked it again.
		s.megaGC = g
		if s.mega != nil {
			clear(s.mega[:])
		}
	}
	tick := s.flushNow()
	if tick != s.jobsTick {
		// An epoch's tick: compiles in the background whose functions went
		// unused since the last dropped, unbegun.
		s.jobsTick = tick
		if s.inFlight() {
			s.rt.jitRegDropStale()
		}
		if len(s.regAheadRoots) != 0 {
			s.regAheadTick()
		}
	}
	if tick < s.flushedAt+s.flushAge() {
		return
	}
	s.flushedAt = tick
	s.jitShrinkContexts()
	s.regFlush(tick, s.epoch())
	jitFlushRealms[struct{}](s)
	s.keepWithin()
}

// settle has the JIT hold no more than its budget once a compile has
// ended. Room for a compile is never made of young code (jitCodeAge), so
// that two functions do not take each other's room in turn; but what grew
// since the room was made -- a function's state measured again as its code
// is let go of (regLetGo), the programs a compile lowered for its callees
// -- had the JIT hold past its budget with young code alone left to
// reclaim, and nothing that would take it: 479,888 bytes of a 458,752-byte
// budget, its floor noted above the budget. Past it, settle's look takes
// young code too, the least recently used first, and none that runs.
func (s *jitState) settle() {
	s.inSettle = true
	defer func() { s.inSettle = false }()
	if s.held() <= s.budget || s.makeRoom(0) {
		return
	}
	s.settling = true
	s.floorAt, s.victims, s.victimsAt, s.victimPos, s.victimLeft = 0, nil, 0, 0, 0
	s.makeRoom(0)
	// Its list and its floor, which counted young code as reclaimable,
	// are not the next look's.
	s.settling = false
	s.floorAt, s.victims, s.victimsAt, s.victimPos, s.victimLeft = 0, nil, 0, 0, 0
}

// keepWithin has the JIT hold no more than its budget once a look of the
// cache's (age, flush) has counted what it holds anew, where no compile's
// end settles it after: every function's state measured again (regAge),
// grown by what its code learned at its exits since, and the states kept
// with no room for them (regLearnedOf), counted from the look on. The
// look's own makeRoom takes what is old; past it, young code goes too
// (settle), as at a compile's end. Looked at outside a compile -- at a
// function's first sample, a link's hint, the contexts' growth, a regular
// expression's compile, a back edge's flush -- the JIT had held past its
// budget until the next compile ended: with all it held young, the look
// found that reclaiming all it might would not make the room, so reclaimed
// none of it -- not even the states it had just counted, which it might
// have -- and noted its floor, and the compiles after it were deferred,
// none to end with a settle. A state grown a few kilobytes past all the
// look could take besides code so left the JIT 125 KB past a budget of
// 257 KB (TestJITHeldWithinBudgetAfterALook), and a program of 300 hot
// functions at a budget of 1 MB ended holding 1,233,688 bytes on arm64
// under load (TestJITMemoryLimitOption). Within a compile or a background
// compile's install (settleAfter), the end settles, as before; within
// settle, it is settling.
func (s *jitState) keepWithin() {
	if s.settleAfter != 0 || s.inSettle {
		return
	}
	if s.budget = s.rt.jitBudget(); s.held() > s.budget {
		s.settle()
	}
}

// settleKeeping is settle, keeping keep, register code a compile just made,
// which its caller is about to enter, and the code it goes with (regRunning's
// busy).
func (s *jitState) settleKeeping(keep ...*jitRegEntry) {
	for _, e := range keep {
		if e != nil {
			e.busy++
		}
	}
	s.settle()
	for _, e := range keep {
		if e != nil {
			e.busy--
		}
	}
}

// learn has perInstr follow what an entry of a function of instructions
// instructions came to hold, bytes: rounded up, so that a compile asks for
// no less room than an entry like it took -- an eighth of the way to it, at
// least a byte where it is more: truncated, a step of less than one had
// left perInstr short of what entries took, by up to 7 bytes an instruction,
// and a budget filled by such compiles took the last one and threw it away.
func (s *jitState) learn(instructions, bytes int) {
	n := max(instructions, 1)
	x := (bytes + n - 1) / n
	if s.perInstr == 0 {
		s.perInstr = x
		return
	}
	d := x - s.perInstr
	if d > 0 {
		d = (d + 7) / 8
	} else {
		d /= 8
	}
	s.perInstr += d
}

// age has the cache find, once an epoch, which of its entries were used
// since it last looked, by their uses, and measure each again, as what an
// entry learns grows its slices and pools; and drop the entries, lowerings
// and links of the functions collected since, whose weak keys are empty,
// and the links neither an entry nor code has (regAge); and flush code
// gone unused (flush).
func (s *jitState) age(now uint64) {
	if s.agedAt == now+1 {
		return
	}
	s.agedAt = now + 1
	s.flush()
	for key, l := range s.lowered {
		if key.Value() == nil {
			s.dropLowering(key, l)
		}
	}
	s.regAge(now)
	s.keepWithin()
}

// dropLowering forgets fn's lowering, l.
func (s *jitState) dropLowering(key weak.Pointer[bytecode.Function], l *jitLowering) {
	delete(s.lowered, key)
	s.lowerBytes -= l.bytes
	s.floorAt = 0
}

// jitCodeAge is how many epochs code goes unused before the cache may
// drop it for another function's (makeRoom), and jitEpochTime how long an
// epoch lasts at most.
const (
	jitCodeAge   = 2
	jitEpochTime = 50 * time.Millisecond
)

// epoch is the cache's clock: the collector's cycles (jitGCEpoch), as V8
// ages code by them, and, for a program that allocates little -- a
// numeric kernel, one phase of a program's after another -- each
// jitEpochTime of the program's time (running) besides.
func (s *jitState) epoch() uint64 {
	t := &s.rt.jitTest
	if f := t.epochNow; f != nil {
		return f()
	}
	if c := t.clocks; c != nil {
		if tick, ok := c(s); ok {
			return tick
		}
	}
	if jitClocksHeld.Load() {
		return 0
	}
	g, k := jitGCEpoch.Load(), jitRepeatCycles.Load()
	return g - min(k, g) + uint64(s.running()/jitEpochTime)
}

// jitClocksHeld, which a host sets for its runs (JITHoldClocks), stops
// every runtime's cache's clocks (epoch, flushNow); a test sets its own
// runtime's (jitTestConfig's epochNow, flushNow and clocks).
var jitClocksHeld atomic.Bool

// running is the program's time, which the cache's clocks run on: the
// time since the runtime's JIT state was made, less what its compiles
// took (compileBegin), which is the JIT's, not the program's. Code aged
// by the compiles' time had the more of it dropped, and compiled again,
// the more the JIT compiled: Octane's Typescript, its compiles a fifth of
// its time, looked for code to flush a time more each run of three, and
// compiled 70 to 190 functions again; a compile made longer, by a tenth,
// compiled more again. V8 ages code by the collector's cycles and by what
// the program runs, not by its compiler's work.
func (s *jitState) running() time.Duration {
	now := time.Now()
	if s.clockFrom.IsZero() {
		s.clockFrom = now
	}
	d := now.Sub(s.clockFrom) - s.compileTime
	if s.compiling != 0 {
		d -= now.Sub(s.compileFrom)
	}
	return d
}

// compileBegin begins a compile, whose time the cache's clocks leave out
// (running) until compileEnd ends it; a compile made within one -- a
// callee's -- is part of it.
func (s *jitState) compileBegin() {
	if s.compiling == 0 {
		s.compileFrom = time.Now()
	}
	s.compiling++
}

// compileEnd ends the compile compileBegin began.
func (s *jitState) compileEnd() {
	if s.compiling--; s.compiling == 0 {
		s.compileTime += time.Since(s.compileFrom)
	}
}

// jitGCEpoch counts the collector's cycles, process-wide: a finalizer the
// collector runs once a cycle counts it and sets itself again
// (jitArmEpoch), from the first runtime that runs the JIT on.
var (
	jitGCEpoch   atomic.Uint64
	jitEpochOnce sync.Once
)

// jitRepeatCycles are the collector's cycles JITCompileRepeat's compiles
// ran, which the cache's clock leaves out, as it leaves out their time,
// part of their compile's (running): the program's code ages, and is
// flushed, as it would have without them.
var jitRepeatCycles atomic.Uint64

// jitEpochMark is what the epoch's finalizer is set on: an object with a
// pointer, so that it is not a tiny allocation, whose finalizer could wait
// for the objects sharing its block.
type jitEpochMark struct{ _ *byte }

func jitArmEpoch() {
	runtime.SetFinalizer(&jitEpochMark{}, func(*jitEpochMark) {
		jitGCEpoch.Add(1)
		jitArmEpoch()
	})
}

func (r *Runtime) releaseJIT() {
	if r.jit == nil {
		return
	}
	r.jit.abandonJobs()
	r.jit.dropRegCode()
	// Not while a native run is under way, suspended in a call of Go that
	// released it: it goes on with the state its contexts and pins are in.
	if len(r.jit.regCache) == 0 && r.jit.ctxTop == 0 {
		r.jit = nil
	}
}

// jitBudget bounds what the JIT holds (jitState.held): its code and all
// that describes it, what it has learned, the programs it lowered, the
// first contexts its code runs in -- not its compiles' working memory, which
// what the recent compiles needed bounds, nor the contexts a deeper
// recursion of native calls takes, which the VM's depth limit bounds
// (sharedBytes). The memory meter
// never counts it: a script's memory, and so whether it fails with
// ErrMemoryLimit, is the same with the JIT on and off. It is the runtime's
// JIT memory limit (Config.JITMemoryLimit) if it has one; else a runtime
// with a memory limit gives the JIT at most an eighth of it, which keeps
// the process bounded.
func (r *Runtime) jitBudget() int {
	if r.jitMemoryLimit > 0 {
		return int(min(r.jitMemoryLimit, int64(maxJITBudget)))
	}
	if r.meter != nil {
		return int(min(int64(jitCacheBytes), r.meter.limit/8))
	}
	return jitCacheBytes
}

// jitCompiles reports whether a compile of fn could fit the budget at all:
// whether what the compile's entry and its lowering will hold -- the code,
// its entries' and exits' descriptions, the program lowered -- fits it
// (jitCompileBytes), as the budget bounds what the JIT holds, not a
// compile's working memory (jitBudget), which is linear in the function's
// size as what it holds is. It costs a few comparisons and never measures
// the heap, so a refusal can be cheaply repeated; the compile's code is
// measured as it is placed (jitRegPlace, makeRoom). It is what bounds how
// large a function is compiled: some 16,000 instructions at the default
// budget, 65,000 at 32 MiB, whatever its slots.
func jitCompiles(fn *bytecode.Function, budget int) bool {
	return jitCompileBytes(fn) <= int64(budget)
}

// What a compile's entry and lowering hold, about, for each instruction of
// the function and each of its slots (jitCompileBytes): of the functions
// of the V8 suite, Octane and AWFY, compiled from their register code, an
// instruction's share of the code, the descriptions and the program is 260
// bytes at the median, 421 at the 99th percentile and 638 at most, of a
// function of fifty; pdfjs's wrapper of 107,000 instructions and 17,897
// slots, 538.
const (
	jitCompileInstrBytes = 512
	jitCompileSlotBytes  = 64
)

// jitCompileBytes is what a compile of fn will hold, about: its
// instructions' and slots' shares (jitCompileInstrBytes,
// jitCompileSlotBytes) and 32 KiB. It had been a word for each slot at
// each instruction, of no fewer than 256 slots -- the dense tables of
// slots by instructions that the frame states, the liveness and the
// builder's definitions had been, linear now -- which refused a function
// of 4,080 instructions at the default budget, and of 1,000 slots one of
// 1,000. The instructions are counted as inlining counts them
// (jitRegInlineSize), in the unit the shares were measured in.
func jitCompileBytes(fn *bytecode.Function) int64 {
	rc := fn.RegCode()
	if rc == nil {
		return 32 << 10
	}
	n := (len(rc.Code)*20 + 8) / 17
	return int64(n)*jitCompileInstrBytes + int64(rc.NumRegs+len(fn.Upvalues))*jitCompileSlotBytes + 32<<10
}

// jitState is the runtime's JIT state, made at its first use.
func (r *Runtime) jitState() *jitState {
	if r.jit == nil {
		r.jit = &jitState{rt: r, arena: jit.NewArena(), stressBudget: int(r.jitStress.budget)}
		r.jit.jitSetEncoding()
		// Parking's period begins with the state (parkDue).
		r.jit.parkRun = r.jit.parkClock()
		jitEpochOnce.Do(jitArmEpoch)
	}
	return r.jit
}

// jitOn reports whether this runtime runs the JIT. The call path tests it
// before each hook, so with the JIT off a call pays this flag test and
// nothing else; without the JIT built in it is the constant false.
func (r *Runtime) jitOn() bool { return r.jitEnabled }

// backEdgeBudget is the back-edge budget after a check: about
// backEdgeCheckInterval, varied from one check to the next. The budget is
// the interpreter's and native code's alike, and runs out
// in whatever loop runs then. Were it the same every time, a loop whose
// iteration takes, with its callees' loops, a number of back edges that
// divides it would run it out at the same place in every period -- in a
// callee, say, never at the loop's own back edge, which is then never
// promoted: a fixed sampling period aliasing with a periodic program.
// Varied, each back edge is as likely as any other to be the one. Before
// the JIT has any state, nothing native runs, and it is the interval.
func (s *jitState) backEdgeBudget() int {
	if s == nil {
		return backEdgeCheckInterval
	}
	s.flush()
	if s.stressBudget != 0 {
		return s.stressBudget
	}
	// xorshift32, never 0.
	x := s.budgetSeed
	if x == 0 {
		x = 0x9e3779b9
	}
	x ^= x << 13
	x ^= x >> 17
	x ^= x << 5
	s.budgetSeed = x
	return backEdgeCheckInterval/2 + int(x%backEdgeCheckInterval)
}

// SetJITWarmUp has r compile a function once calls invocations' worth of
// its budget are spent, and wait afterICUpdate invocations' worth after a
// change of its caches' feedback, in place of V8's counts
// (jitInvocationsForMaglev, jitInvocationsAfterICUpdate): for tests of
// what native code does once compiled, which run a function a few times.
// QJS_JIT_STRESS's threshold, compiling at once, stays.
func (r *Runtime) SetJITWarmUp(calls, afterICUpdate int) {
	if r.jitStress.threshold {
		return
	}
	r.jitCallThreshold = uint16(min(max(calls, 1), 0xffff))
	r.jitICThreshold = uint16(min(max(afterICUpdate, 1), 0xffff))
}

// jitHotWork is how much work Go's calls of a function with no loop and no
// array work do -- its instructions, at each call -- before it is compiled
// (LowerRegHot): a hundredth of what V8 counts before it optimizes.
// Richards' setup methods, called twice in each of its runs, are not;
// Towers' recursion, eight thousand calls in each, is in its first.
const jitHotWork = 16 << 10
