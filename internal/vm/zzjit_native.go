//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"cmp"
	"encoding/binary"
	"errors"
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
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitCacheBytes is the JIT's budget (jitBudget) in a runtime without a
// memory limit, and maxJITBudget the most a JIT memory limit gives it,
// which no sum of what it holds overflows.
const (
	jitCacheBytes = 8 << 20
	maxJITBudget  = 1 << 50
)
const jitHotCalls = 8

// jitBuilt guards the JIT's hooks in the interpreter; see zzjit_disabled.go.
const jitBuilt = true

type jitFields struct {
	jitEnabled       bool
	jitCallThreshold uint8
	jitDeoptDepth    uint32
	jit              *jitState
	// jitMemoryLimit is Config's JITMemoryLimit: the JIT's budget, if more
	// than 0 (jitBudget).
	jitMemoryLimit int64
	// jitStress is QJS_JIT_STRESS's budget and deoptimization period, or
	// zero; see jitStressConfig.
	jitStress jitStressConfig
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
//
// It is read once, as QJS_NOTREE is, and each runtime takes a copy.
type jitStressConfig struct {
	threshold bool
	budget    uint16
	deopt     uint16
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
		}
	}
	return c
}

// JITStats reports what the JIT has done in this runtime so far.
func (r *Runtime) JITStats() JITStats {
	s := r.jit
	if s == nil {
		if r.jitEnabled {
			return JITStats{Budget: int64(r.jitBudget())}
		}
		return JITStats{}
	}
	return JITStats{Compiled: s.compiled, Entries: s.entries, Guards: s.guards,
		Hosts: s.hosts, Budgets: s.budgets, Interpreted: s.interpreted, SSAEntries: s.ssaEntries, SSARecords: s.ssaRecords,
		Held: int64(s.held()), Budget: int64(r.jitBudget()), Evicted: s.evicted, Refused: s.refusedRoom}
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
	jitCharCodeAt Value
}

func (r *Runtime) recordJITStringIntrinsic() {
	r.jitCharCodeAt = r.proto.str.getOwn(r.atoms.intern("charCodeAt")).value
}

// A closure belongs to one runtime. Hotness and permanent selection refusals
// live here without registering a weak key on cold or refused calls. The
// fields fit the closure's padding beside pureMiss, so it keeps its 128-byte
// size class: jitHint is not a pointer but a slot in the runtime's hint
// table (jitState.hint), and bytes, so that nothing is aligned past it.
type jitClosureFields struct {
	jitRefused   bool
	jitCalls     uint8
	jitLoopDelay uint8
	jitHint      [4]byte
}

func (c *jitClosureFields) hint() uint32 { return binary.LittleEndian.Uint32(c.jitHint[:]) }

func (c *jitClosureFields) setHint(h uint32) { binary.LittleEndian.PutUint32(c.jitHint[:], h) }

// jitHintSlot is a slot of the hint table (jitState.hints): the entry it
// holds, and its function's address, which is not a reference -- the
// cache's keys are weak -- but what a closure's function must be for the
// closure's hint to name the entry (hint).
type jitHintSlot struct {
	e  *jitEntry
	fn uintptr
}

// hint returns the entry cl's hint names, or nil if the hint is empty or
// stale: its slot freed, or holding another function's entry since. A slot
// that holds an entry of cl's function holds the one the cache has for it,
// whichever entry the hint was given for. An address names one function
// while cl lives, which keeps cl's function: a function collected since
// had another address while it lived, and none is made at cl's function's
// until cl is collected. So a slot reused, however often, never hands cl
// another function's code, where a tag had wrapped. The state is discarded
// only when a closed runtime releases it, so a hint never outlives the
// table it indexes.
func (s *jitState) hint(cl *closure) *jitEntry {
	h := cl.hint()
	if h == 0 || s == nil || int(h) >= len(s.hints) {
		return nil
	}
	slot := &s.hints[h]
	if slot.fn != uintptr(unsafe.Pointer(cl.fn)) {
		return nil
	}
	return slot.e
}

// entryOf is the program cached for cl's function, as cl's hint names it.
func (r *Runtime) entryOf(cl *closure) *jitEntry { return r.jit.hint(cl) }

// hintFor is the hint naming e, or 0 for none.
func hintFor(e *jitEntry) uint32 {
	if e == nil {
		return 0
	}
	return e.hint
}

type jitEntry struct {
	// hint is the slot a closure remembers this entry by (jitState.hints),
	// which only an entry the cache holds has.
	hint uint32
	// bytes is what the entry holds, as last measured (measure), which the
	// cache counts against the JIT's budget while it holds the entry; busy
	// marks an entry being compiled, whose code the cache does not drop
	// meanwhile to make room (makeRoom).
	bytes int
	busy  uint8
	// ssa is the new pipeline's code, when it compiled the function, and
	// ssaShapes the shapes its guards compare objects with, which it holds
	// by address and this keeps alive.
	ssa       *jit.SSACode
	ssaShapes []*shape
	// ssaProg is the program the code was compiled from: the function's
	// own lowering, or, for a callee with no loop of its own, its lowering
	// for inlining (jitLower), which the runtime keeps anyway.
	ssaProg *ir.Program
	// ssaHolders are the prototypes it compares receivers' with
	// (ssa.Holder), held by address, which this keeps alive.
	ssaHolders []*Object
	// ssaStrings marks code that calls charCodeAt, for which Go looks up the
	// intrinsic before every entry.
	ssaStrings bool
	// ssaStats is what the code did: how often it was entered, left for
	// Go, failed a guard (by bytecode PC) and polled. ssaLoop is the
	// function's loops' mean length, in instructions, which the work a
	// native stretch did is estimated with (jitSSAProfit).
	ssaStats jitSSAStats
	ssaLoop  uint32
	// failed and failedEntries are the sites, by slot IR PC, and the
	// entries, by PC, whose speculation failed in the code; reopt marks
	// code to be compiled again, with them generic, at its next entry, and
	// reopts counts the times it was (jitDeoptimized).
	failed        []int32
	failedEntries []uint32
	reopt         bool
	reopts        uint8
	// fed are the property sites, each with the shape the code was
	// compiled for there, or none (jitFed); fedInlined those of the
	// callees it inlines whose caches knew nothing (jitInlineFed).
	fed        []jitFedSite
	fedInlined []jitFedInlined
	// ssaInlinedAt are the calls the code inlines, by pc: a call decided
	// for inlining since leaves from code compiled before, which is not
	// the callee's leaving (jitCallSeen).
	ssaInlinedAt []int32
	// reoptBudget is how many native stretches the code runs before it is
	// compiled again for what it has learned (reoptDue).
	reoptBudget uint32
	// feedReopt marks code compiled before a site it has run since had any
	// feedback: compiled again at its next invocation (calls, by Go or
	// native callers), or once the invocation it ran in has done
	// feedWork's worth of work more (reoptDue). starts counts Go's entries
	// at a function's start, not into a loop.
	feedReopt            bool
	starts               uint64
	feedStarts, feedWork uint64
	// literals are the object literals the code makes natively, each with
	// its pool, kept across compiles (jitFeedback.Literal); definesUnfed
	// the literals' fields compiled before their transitions were made,
	// which the code leaves at (jitDefineSeen).
	literals     []jitLiteral
	definesUnfed []int32
	// ssaCells are the cells the code reads string constants from
	// (jitFeedback.StringCell).
	ssaCells []*Value
	// poly are the reads that met objects of shapes the code was not
	// compiled for, each with up to jitPropertyCases of them, which it is
	// compiled again for (jitPolySeen); polyReopt marks that it is to be,
	// polyPending that it is to be once polySettled exits in a row have
	// found no other (jitPolySettle).
	poly        []jitPolySite
	polyReopt   bool
	polyPending bool
	polySettled uint8
	// strSites are the method reads that met a string receiver, which the
	// code reads off String.prototype once compiled again
	// (jitFeedback.StringProperty).
	strSites []uint32
	// entryMisses counts the times Go went on in the interpreter, having
	// found no entry, since the code was compiled; allEntries marks code
	// to be compiled with every one (jitGoOn).
	entryMisses uint8
	allEntries  bool
	// link is where native callers call the function's code (jitLink):
	// its entry, and the count of their calls since the code was last
	// compiled (in). nativeOut counts those that left native code
	// from inside it (jitUnwindNative); notNative marks code native
	// callers no longer call, for leaving on too many (jitUnwindShare),
	// until Go has made nativeRetry calls to it, more for each time it was
	// found to (nativeBackoff, jitNativeRetry).
	link          *jitLink
	nativeOut     uint64
	notNative     bool
	nativeRetry   uint32
	nativeBackoff uint8
	// ssaLinks are the links of the functions the code calls natively,
	// whose addresses it holds, and ssaInlined the closures of those it
	// inlines, whose frames Go makes at an exit inside them
	// (jitUnwindNative).
	ssaLinks   []*jitLink
	ssaInlined []*closure
	// ssaPools are the object pools the code's constructions take from,
	// whose addresses it holds.
	ssaPools []*abi.ObjectPool
	// ssaKeeps is how many keep cells (abi.Context.Keep) its code has used,
	// the most of any it had: what Go clears when the code returns.
	ssaKeeps int
	// ssaRuns is how many runs of its code Go is inside (runSSAIn): one may
	// go on in it after a native call, so the cache keeps it (inUse).
	ssaRuns int32
	// pinCode is the code a call of Go from a native callee last pinned for
	// the run whose pins pinSerial names (jitGoRun's serial): a later call
	// from that callee in that run finds it pinned without a search.
	pinCode   *jit.SSACode
	pinSerial uint64
	// nativeCalls are the calls the code makes natively, or will when
	// compiled again, each with the function it was seen to call.
	nativeCalls []jitInline
	// inlines are the calls the code inlines, or will when compiled again,
	// each with the function it was seen to call (jitCallSeen); notInline
	// are those that stopped being: what the callee does kept leaving
	// native code.
	inlines   []jitInline
	notInline []int32
	// nativeCallers are the entries whose code calls this one's natively,
	// up to jitNativeCallers: when this code is compiled again to inline
	// more, they are too, to inline it with its calls (jitInlineNativeCalls).
	nativeCallers []*jitEntry
	// callSites is what jitCallSeen knows of each call, by PC: how often it has
	// left native code, up to jitCallsToInline, then jitCallDone or
	// jitCallInlined; nil until a call does. inlinePending marks calls
	// found to inline since the code was compiled, which it is compiled
	// again for once the calls around them have been seen.
	callSites     []uint8
	inlinePending bool
	// inlineReopt marks code to be compiled again for the calls found to
	// inline, and inlineReopts counts the times it was, apart from reopts.
	inlineReopt  bool
	inlineReopts uint8
	// upgradeReopt marks code to be compiled again to inline a call it
	// makes natively, its callee now inlining its own calls
	// (jitCallersReopt), and upgradeReopts counts the times it was, apart
	// from the others, which its own calls need.
	upgradeReopt  bool
	upgradeReopts uint8
	// unwindReopt marks code to be compiled again for leaving native code
	// on too many of its native callers' calls (jitUnwound), and
	// unwindReopts counts the times it was.
	unwindReopt  bool
	unwindReopts uint8
	// ssaCallee marks code compiled for native callers alone, from
	// LowerSSAInline (jitNativeCallee).
	ssaCallee bool
	// slowTries counts the existing tiers' tries to enter code Go no
	// longer enters (entrySlow) that has learned what to be compiled again
	// for (reoptPending): its budget (reoptDue), which nothing native
	// spends any more, as V8's interpreter spends a function's.
	slowTries uint32
	// entrySlow marks code Go no longer enters (jitSSAProfit), and this
	// code that reads its receiver.
	entrySlow bool
	this      bool
	// deferred marks a refusal for want of room in the JIT's budget, made at
	// the cache generation in generation: it is retried once that has moved
	// on.
	// calleeRefused marks a function that does not compile for native
	// callers alone (jitNativeCallee).
	deferred      bool
	generation    uint64
	calleeRefused bool
	// used is the cache's epoch (jitState.epoch) at which the entry was
	// last found used -- its uses (uses) other than at the look before,
	// seen -- or made; serial orders entries by when they were made. The
	// cache drops the least recently used to make room (makeRoom).
	// slowCalls counts the calls the existing tiers made, that did not
	// enter its code (entrySlow): it is used all the same.
	used, seen, serial uint64
	slowCalls          uint64
}

// uses is how often e's code, or what e knows, was used: Go's entries,
// into its start or its loops, native callers' calls, the existing
// tiers' calls that do not enter it. Compiling it again counts some of it
// afresh: it changes all the same.
func (e *jitEntry) uses() uint64 {
	return e.starts + e.ssaStats.entries + e.link.in + e.slowCalls
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
// The function's link is found by its key (jitState.links) while an entry
// the cache holds owns it (owner) or callers' code calls through it
// (refs); then it is dropped.
type jitLink struct {
	entry uintptr
	in    uint64
	owner *jitEntry
	refs  int
	key   weak.Pointer[bytecode.Function]
}

// Weak keys prevent a refusal or cached program from retaining a source graph.
// Native entry cannot call Go, so buffers are shared only until a guard exit.
type jitState struct {
	// unavailable marks a runtime the OS will not give executable memory.
	unavailable bool
	cache       map[weak.Pointer[bytecode.Function]]*jitEntry
	// links are the functions' links (jitLink), by the same keys.
	links map[weak.Pointer[bytecode.Function]]*jitLink
	// resumed counts native calls whose callee Go finished, after which
	// their callers' code went on natively (runSSA); resumedLevels those
	// whose caller was itself a level Go was finishing (jitUnwindNative).
	resumed, resumedLevels uint64
	// ssaShared is what the contexts from ssaSharedFrom on hold of what
	// they share (jitShareContexts); ssaSharedGen counts the times either
	// changed, which a call of Go compares, not the struct.
	ssaShared     jitShared
	ssaSharedFrom int
	ssaSharedGen  uint64
	// arena holds the code of every entry: a few mappings, however many
	// functions the runtime compiles.
	arena *jit.Arena
	// ssaWork and mirWork are the memory the new pipeline compiles in,
	// rewound after each compile (ssa.Workspace, mir.Workspace).
	ssaWork ssa.Workspace
	mirWork mir.Workspace
	// generation advances whenever the cache releases code, which is when a
	// program refused for want of budget may fit.
	generation uint64
	// hints holds each cached entry in a slot, from 1, that a closure
	// remembers (jitClosureFields, hint); hintFree are the slots free. The
	// table grows to as many entries as the cache has held at once, which
	// the budget bounds: each entry holds at least jitEntryFixed bytes of it.
	hints    []jitHintSlot
	hintFree []uint32
	// budget is what the JIT may hold (jitBudget), as of the last compile;
	// entryBytes and lowerBytes what its entries and its lowerings hold
	// (jitEntry.measure, jitLowering.bytes), which, with what the runtime's
	// JIT holds besides (sharedBytes), it holds (held).
	budget                 int
	entryBytes, lowerBytes int
	// perInstr is about how many bytes of the budget an entry holds for
	// each instruction of its function's bytecode, from the last compiles:
	// how much room a compile is likely to need (makeRoom).
	perInstr int
	// compiling marks a compile under way, in the workspaces, which the
	// cache does not release meanwhile.
	compiling bool
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
	// seeding is how many compiles deep jitSeedCalls has started compiles
	// (jitSeedCallee); jitWantedCallees' and jitForwardTarget's are past
	// jitSeedDepth, starting none. seedingFns are the functions whose calls
	// are being decided, from the outermost.
	seeding    int
	seedingFns []*bytecode.Function
	// unwound counts the native calls whose callees left native code, which
	// Go finished (jitUnwindNative).
	unwound uint64
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
	// from how far the counter fell (jitSSAProfit), adds it back.
	pollCredit int
	// unwinding holds the levels and frames of the native calls Go is
	// finishing (jitUnwindNative), each unwind's past those of the ones it
	// runs inside: reused, not allocated each time.
	unwinding       []jitNativeLevel
	unwindingFrames []*frame
	// lowered are the functions' programs (jitLower), and whether each may
	// be inlined (jitInlinable).
	lowered     map[weak.Pointer[bytecode.Function]]*jitLowering
	interpreted uint64
	// goOn is the frame the interpreter runs for jitGoOn, until a loop's
	// header Go goes on natively at, goOnPC with goOnDepth operands;
	// wentOn counts the times it did.
	goOn              *frame
	goOnPC, goOnDepth int
	wentOn            uint64
	stressExits       uint64
	// ssaCtxs are the contexts native code runs in, one for each runSSA
	// running and each native call (abi.Context's Level), ctxTop the
	// first free; ssaCtx is the one Go entered last, whose exit it reads.
	// They are made jitContexts at a time, linked (abi.Context.Next), and
	// never move: more are added while native code runs in them
	// (jitGrowContexts).
	ssaCtxs []*abi.Context
	ctxTop  int
	ssaCtx  *abi.Context
	// goRuns are, by context, the frame, entry and code a run of native
	// code began there runs (runSSALoop), which a call of Go it makes is
	// made for (jitGoCall); goPinned the code calls of Go are suspended
	// in, innermost last, and retired the code compiled again or dropped
	// meanwhile, closed once none is (closeSSA). goPending is a call of
	// Go that made the call and left the rest to Go, at its exit; goCalls
	// counts the calls Go made so.
	goCalls    uint64
	pinSerial  uint64
	goMark     jitGoMark
	levels     jitLevels
	goRuns     []jitGoRun
	goPinned   []*jit.SSACode
	retired    []jitRetired
	goPending  jitGoPending
	ssaEntries uint64
	ssaRecords uint64
	// backendPanics counts the compiles a backend refused for panicking.
	backendPanics uint64
	// entered counts the native calls Go ran the callee of from its
	// start, the callee having no native code (abi.ExitEnter).
	entered uint64
	// ssaKeeps is the most keep cells (abi.Context.Keep) any code compiled
	// uses: what Go clears in a native callee's context.
	ssaKeeps int
	// exitScratch is what exits that leave their frames to Go are read
	// into (jit.ExitScratch).
	exitScratch jit.ExitScratch
	// markingExits counts the exits the collector's marking made
	// (abi.Context.ExitMarking), a native callee's included.
	markingExits uint64
	// scans counts the cache's looks for what to reclaim (makeRoom), serial
	// the entries made, evicted those dropped to make room, and
	// refusedRoom the times there was none to be made. floor is what the
	// JIT held at the last look that it could not reclaim -- young code,
	// code that runs, what the runtime's JIT needs -- and floorAt 1 plus the
	// epoch of that look, or 0 once something it held has gone since: until
	// then, the floor only rises, and a need past the budget above it is
	// refused without another look. agedAt is 1 plus the epoch the cache
	// last found its entries' uses in (age).
	scans, serial, evicted, refusedRoom uint64
	floor                               int
	floorAt, agedAt                     uint64
}

// Only selected own reference fields are prepared. The receiver bound keeps
// cycles and unusually large object graphs on the ordinary host path.
const jitReferenceReceivers = 32

type jitReferences struct {
	cells [jitReferenceReceivers][ir.MaxProperties]ir.ReferenceCell
}

func (r *Runtime) initJIT(enabled bool, memoryLimit int64) {
	r.jitMemoryLimit = memoryLimit
	// Debugger interrupt callbacks may evaluate code and mutate frames or arrays.
	// Native budget exits retain borrowed views, so debugger runtimes stay in Go.
	r.jitEnabled = enabled && r.debug == nil
	r.jitCallThreshold = jitHotCalls
	r.jitStress = jitStressDefault
	if r.jitStress.threshold {
		r.jitCallThreshold = 1
	}
}

// Loop promotion returns through a panic caught by the callee's runTree.
// A nested tree without its own recovery would return from its caller
// instead, so a nested call keeps runTree's recover while its loop may still
// promote, and only then: once the JIT has refused the function, keeps it for
// callers, or has given up on its guards, it costs nothing to trees.
// callTree asks only when jitOn.
func (r *Runtime) jitTreeRecovery(f *frame) bool {
	if f.cl.jitRefused {
		return false
	}
	e := r.jit.hint(f.cl)
	return e == nil || e.deferred || e.ssa != nil
}

// jitCodeBytes is the machine code the JIT's entries hold. What the JIT
// holds in all, which its budget bounds, is jitHeldBytes. None of it is the
// script's memory (see jitBudget).
func (r *Runtime) jitCodeBytes() int64 {
	var n int64
	if r.jit != nil {
		for _, e := range r.jit.cache {
			n += int64(e.ssa.Size())
		}
	}
	return n
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
// holds: an entry with its hint slot (jitEntryFixed), a link, a lowering,
// an object pool, a context.
var (
	jitEntryFixed    = int(unsafe.Sizeof(jitEntry{})) + int(unsafe.Sizeof(jitHintSlot{})) + jitMapSlot
	jitLinkFixed     = int(unsafe.Sizeof(jitLink{})) + jitMapSlot
	jitLoweringFixed = int(unsafe.Sizeof(jitLowering{})) + jitMapSlot
	jitPoolFixed     = int(unsafe.Sizeof(abi.ObjectPool{})) + int(unsafe.Sizeof(jitPoolSource{}))
	jitContextBytes  = int(unsafe.Sizeof(abi.Context{}))
)

// jitCapBytes is the memory s's backing array holds.
func jitCapBytes[T any](s []T) int {
	var t T
	return cap(s) * int(unsafe.Sizeof(t))
}

// measure is what e holds now: itself and its hint slot; its code, with
// what describes the code's entries and exits (jit.SSACode's Footprint);
// what the code was compiled with (compiledBytes); and what the entry has
// learned since, its object pools' objects among it (learnedBytes). Not
// its function's lowering, which the runtime's lowerings hold (jitLower),
// nor its link (sharedBytes).
func (e *jitEntry) measure() int {
	return jitEntryFixed + e.ssa.Footprint() + e.compiledBytes() + e.learnedBytes()
}

// compiledBytes is what e holds that its code was compiled with: the
// slices a compile gives it (setSSA), which jitCompiledBytes measures.
func (e *jitEntry) compiledBytes() int {
	return jitCompiledBytes(e.ssaShapes, e.ssaHolders, e.fed, e.fedInlined, e.ssaInlinedAt, e.definesUnfed, e.ssaCells, e.ssaInlined, e.ssaPools, e.ssaLinks)
}

// compiledBytes is what fb's code would have its entry hold, in place of
// what the entry's code was compiled with (jitEntry.compiledBytes).
func (fb *jitFeedback) compiledBytes() int {
	return jitCompiledBytes(fb.shapes, fb.holders, fb.fed, fb.fedInlined, fb.inlinedAt, fb.definesUnfed, fb.cells, fb.inlined, fb.pools, fb.links)
}

func jitCompiledBytes(shapes []*shape, holders []*Object, fed []jitFedSite, fedInlined []jitFedInlined, inlinedAt, definesUnfed []int32,
	cells []*Value, inlined []*closure, pools []*abi.ObjectPool, links []*jitLink) int {
	return jitCapBytes(shapes) + jitCapBytes(holders) + jitCapBytes(fed) + jitCapBytes(fedInlined) + jitCapBytes(inlinedAt) +
		jitCapBytes(definesUnfed) + jitCapBytes(cells) + len(cells)*int(valueSize) + jitCapBytes(inlined) + jitCapBytes(pools) + jitCapBytes(links)
}

// learnedBytes is what e holds of what it has learned, which outlives its
// compiles: the sites that failed, its calls, literals and reads met with
// more shapes, and the object pools of its literals and constructions,
// with the objects made for them.
func (e *jitEntry) learnedBytes() int {
	n := jitCapBytes(e.failed) + jitCapBytes(e.failedEntries) + jitCapBytes(e.literals) + jitCapBytes(e.poly) +
		jitCapBytes(e.strSites) + jitCapBytes(e.nativeCalls) + jitCapBytes(e.inlines) + jitCapBytes(e.notInline) +
		jitCapBytes(e.nativeCallers) + jitCapBytes(e.callSites)
	for i := range e.poly {
		p := &e.poly[i]
		n += jitCapBytes(p.cases) + jitCapBytes(p.reads) + jitCapBytes(p.adds)
	}
	for i := range e.literals {
		n += jitPoolBytes(e.literals[i].pool)
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
	if c := pool.Count; c > 0 && c <= abi.PoolCapacity {
		o := (*Object)(pool.Objects[c-1])
		n += int(pool.Size) * (int(objectSize) + jitCapBytes(o.props) + jitCapBytes(o.elems))
	}
	return n
}

// held is what the JIT holds for the runtime: its entries (measure), the
// programs it has lowered (jitLower), and what it holds besides
// (sharedBytes).
func (s *jitState) held() int {
	return s.entryBytes + s.lowerBytes + s.sharedBytes()
}

// sharedBytes is what the JIT holds for the runtime apart from its entries
// and lowerings: its state; the contexts native code runs in; its hint
// table and links; the workspaces its compiles keep (workBytes); and code
// compiled again or dropped that a call of Go is still suspended in
// (retired).
func (s *jitState) sharedBytes() int {
	n := int(unsafe.Sizeof(*s)) + len(s.ssaCtxs)*jitContextBytes + jitCapBytes(s.ssaCtxs) + jitCapBytes(s.hints) +
		jitCapBytes(s.hintFree) + len(s.links)*jitLinkFixed + jitCapBytes(s.victims) + jitCapBytes(s.goRuns) +
		jitCapBytes(s.goPinned) + jitCapBytes(s.retired) + jitCapBytes(s.unwinding) + jitCapBytes(s.unwindingFrames) +
		jitCapBytes(s.seedingFns) + s.workBytes()
	for _, x := range s.retired {
		n += x.code.Footprint()
	}
	return n
}

// workBytes is what the compiles' workspaces keep between compiles.
func (s *jitState) workBytes() int { return s.ssaWork.Bytes() + s.mirWork.Bytes() }

// account counts what e holds now, if the cache holds it.
func (s *jitState) account(e *jitEntry) {
	if e.hint == 0 {
		return
	}
	n := e.measure()
	if n < e.bytes {
		s.floorAt = 0
	}
	s.entryBytes += n - e.bytes
	e.bytes = n
}

// dropEntry closes e's code and removes it from the cache, reporting false if
// the OS would not release the code, which leaves e owned for a retry.
func (s *jitState) dropEntry(key weak.Pointer[bytecode.Function], e *jitEntry) bool {
	held := e.ssa.Size() != 0
	e.link.entry = 0
	if s.closeSSA(e.ssa, e) != nil {
		return false
	}
	s.setLinks(e, nil)
	s.forget(key, e)
	if held {
		s.generation++
	}
	return true
}

// inUse reports whether e's code runs, or may go on, below Go now: a run
// of it, which goes on in its code after a native call whose callee Go
// finishes. The cache does not drop it to make room then, as V8 keeps code
// a frame is in: the run would find its code gone.
func (s *jitState) inUse(e *jitEntry) bool {
	return e.ssaRuns != 0
}

// jitVictim is what the cache may reclaim to make room (makeRoom): a
// lowering, an entry, or the compiles' workspaces, by its class.
type jitVictim struct {
	key          weak.Pointer[bytecode.Function]
	e            *jitEntry
	l            *jitLowering
	class        uint8
	used, serial uint64
	bytes        int
}

// The classes of what the cache reclaims, in the order it does: a
// function's programs, lowered again at a compile's cost of a few percent;
// a refusal remembered, which spares a compile only; code, which costs a
// compile to make again; then the workspaces, which every compile uses.
const (
	jitVictimLowering = iota
	jitVictimRefusal
	jitVictimCode
	jitVictimWork
)

// makeRoom reclaims what the JIT holds, until it holds need bytes less
// than its budget, and reports whether it does: as V8 bounds its code, and
// all that describes it, by memory, reclaiming code by its age, not by how
// many functions have it. It reclaims, in their order (jitVictim's
// classes), the least recently used first: programs lowered for functions
// the cache has no entry for; refusals remembered; code not used in this
// epoch or the one before (jitCodeAge), as V8 flushes only code old
// enough; and the compiles' workspaces, unless a compile is under way. A
// function's code is never dropped for another's while it is young: a
// program with more hot functions than the budget holds had compiled each
// at every pass, each dropping the next one run. Nor an entry whose code
// runs (inUse), or that is being compiled (busy). If even all it may
// reclaim would not make the room, it reclaims nothing, and the newcomer
// runs in the existing tiers until some code is old.
func (s *jitState) makeRoom(need int) bool {
	held := s.roomHeld()
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
	if held = s.roomHeld(); held+need <= s.budget {
		return true
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
	for s.victimPos < len(s.victims) && s.roomHeld()+need > s.budget {
		x := &s.victims[s.victimPos]
		s.victimPos++
		s.victimLeft -= x.bytes
		if !s.reclaim(x, now) {
			return false
		}
	}
	if s.victimPos == len(s.victims) {
		// Let go, not kept empty: it is listed anew at the next look, and
		// what it held counted against the budget meanwhile.
		s.victims, s.victimsAt, s.victimLeft = nil, 0, 0
	}
	return s.roomHeld()+need <= s.budget
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
		if k := s.links[key]; k == nil || k.owner == nil {
			v = append(v, jitVictim{key: key, l: l, class: jitVictimLowering, used: l.used, bytes: l.bytes})
		}
	}
	for key, e := range s.cache {
		if s.inUse(e) || e.busy != 0 {
			continue
		}
		class := uint8(jitVictimRefusal)
		if e.holdsCode() {
			if e.used+jitCodeAge > now {
				continue
			}
			class = jitVictimCode
		}
		v = append(v, jitVictim{key: key, e: e, class: class, used: e.used, serial: e.serial, bytes: e.bytes})
	}
	if !s.compiling {
		v = append(v, jitVictim{class: jitVictimWork, bytes: s.workBytes()})
	}
	for i := range v {
		reclaimable += v[i].bytes
	}
	if floor := s.roomHeld() - reclaimable; floor+need > s.budget {
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
// now -- still held, not running, not being compiled, and code still old
// -- and reports false only if dropping an entry failed.
func (s *jitState) reclaim(x *jitVictim, now uint64) bool {
	switch x.class {
	case jitVictimLowering:
		if l := s.lowered[x.key]; l == x.l {
			if k := s.links[x.key]; k == nil || k.owner == nil {
				s.dropLowering(x.key, x.l)
			}
		}
	case jitVictimWork:
		if !s.compiling {
			s.ssaWork, s.mirWork = ssa.Workspace{}, mir.Workspace{}
		}
	default:
		e := s.cache[x.key]
		if e != x.e || s.inUse(e) || e.busy != 0 || e.holdsCode() != (x.class == jitVictimCode) ||
			e.holdsCode() && e.used+jitCodeAge > now {
			return true
		}
		if !s.dropEntry(x.key, e) {
			return false
		}
		s.evicted++
	}
	return true
}

// roomHeld is what the JIT holds, as makeRoom counts it: the workspaces of
// a compile under way are its own until it ends, when the JIT has the
// room for them it needs (settle).
func (s *jitState) roomHeld() int {
	n := s.held()
	if s.compiling {
		n -= s.workBytes()
	}
	return n
}

// settle has the JIT hold no more than its budget once a compile has
// ended, its workspaces kept for the next, if there is the room.
func (s *jitState) settle() {
	if s.held() > s.budget {
		s.makeRoom(0)
	}
}

// roomFor makes room for e's code, of which e is to hold need bytes in all
// in place of what it holds now (jitEntry.bytes), and reports whether there
// is the room. The code is compiled: what it calls through -- links, which
// go with the last code calling through them and the entry that owns them
// -- and e itself are kept meanwhile.
func (s *jitState) roomFor(e *jitEntry, links []*jitLink, need int) bool {
	for _, l := range links {
		l.refs++
	}
	if e != nil {
		need -= e.bytes
		e.busy++
	}
	ok := s.makeRoom(need)
	if e != nil {
		e.busy--
	}
	for _, l := range links {
		// A link whose owner went keeps its key for the compile's code,
		// which calls through it from now on (setLinks), or until the
		// cache finds it with neither (age).
		l.refs--
	}
	return ok
}

// learn has perInstr follow what an entry of a function of instructions
// instructions came to hold, bytes.
func (s *jitState) learn(instructions, bytes int) {
	x := bytes / max(instructions, 1)
	if s.perInstr == 0 {
		s.perInstr = x
	} else {
		s.perInstr += (x - s.perInstr) / 8
	}
}

// age has the cache find, once an epoch, which of its entries were used
// since it last looked, by their uses, and measure each again, as what an
// entry learns grows its slices and pools; and drop the entries, lowerings
// and links of the functions collected since, whose weak keys are empty,
// and the links neither an entry nor code has (unlink).
func (s *jitState) age(now uint64) {
	if s.agedAt == now+1 {
		return
	}
	s.agedAt = now + 1
	for key, e := range s.cache {
		if key.Value() == nil {
			s.dropEntry(key, e)
			continue
		}
		if u := e.uses(); u != e.seen {
			e.seen, e.used = u, now
		}
		s.account(e)
	}
	for key, l := range s.lowered {
		if key.Value() == nil {
			s.dropLowering(key, l)
		}
	}
	for key, l := range s.links {
		if l.owner == nil && (l.refs == 0 || key.Value() == nil) {
			delete(s.links, key)
		}
	}
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

// epoch is the cache's clock (jitEpochNow).
func (s *jitState) epoch() uint64 { return jitEpochNow() }

// jitGCEpoch counts the collector's cycles, process-wide: a finalizer the
// collector runs once a cycle counts it and sets itself again
// (jitArmEpoch), from the first runtime that runs the JIT on.
var (
	jitGCEpoch   atomic.Uint64
	jitEpochOnce sync.Once
	jitEpochBase = time.Now()
)

// jitEpochNow is the cache's clock: the collector's cycles (jitGCEpoch), as
// V8 ages code by them, and, for a program that allocates little -- a
// numeric kernel, one phase of a program's after another -- each
// jitEpochTime besides. A variable, which the tests set.
var jitEpochNow = func() uint64 {
	return jitGCEpoch.Load() + uint64(time.Since(jitEpochBase)/jitEpochTime)
}

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

// holdsCode reports whether e has native code, which the cache keeps
// while it is young.
func (e *jitEntry) holdsCode() bool {
	return e.ssa.Size() != 0
}

// forget removes e from the cache and frees its hint slot; its link is
// no longer its.
func (s *jitState) forget(key weak.Pointer[bytecode.Function], e *jitEntry) {
	delete(s.cache, key)
	if e.hint != 0 {
		s.hints[e.hint] = jitHintSlot{}
		s.hintFree = append(s.hintFree, e.hint)
		e.hint = 0
	}
	s.entryBytes -= e.bytes
	e.bytes = 0
	s.floorAt = 0
	s.disown(e)
}

// disown has e's link no longer e's: an entry the cache does not hold.
func (s *jitState) disown(e *jitEntry) {
	if l := e.link; l != nil && l.owner == e {
		l.owner, l.entry = nil, 0
		s.unlink(l)
		// Its function's lowering is now one makeRoom may reclaim: the
		// floor noted before counted it as held for good.
		s.floorAt = 0
	}
}

// linkFor gives e, the entry made for fn, fn's link, the one its callers'
// code calls through if any does: e's code, once compiled, is what they
// call (setSSA).
func (s *jitState) linkFor(fn *bytecode.Function, e *jitEntry) {
	key := weak.Make(fn)
	l := s.links[key]
	if l == nil {
		if s.links == nil {
			s.links = map[weak.Pointer[bytecode.Function]]*jitLink{}
		}
		l = &jitLink{key: key}
		s.links[key] = l
	}
	l.owner, l.entry, l.in = e, 0, 0
	e.link = l
}

// setLinks has e's code call through links, those of the functions it
// calls natively, in place of what it called through before.
func (s *jitState) setLinks(e *jitEntry, links []*jitLink) {
	for _, l := range links {
		l.refs++
	}
	old := e.ssaLinks
	e.ssaLinks = links
	for _, l := range old {
		l.refs--
		s.unlink(l)
	}
}

// unlink forgets l once no entry owns it and no code calls through it.
func (s *jitState) unlink(l *jitLink) {
	if l.owner == nil && l.refs == 0 && s.links[l.key] == l {
		delete(s.links, l.key)
	}
}

// remember caches e for key, in a free hint slot, or a new one, and counts
// what it holds.
func (s *jitState) remember(key weak.Pointer[bytecode.Function], e *jitEntry) {
	s.cache[key] = e
	var i uint32
	if n := len(s.hintFree); n != 0 {
		i, s.hintFree = s.hintFree[n-1], s.hintFree[:n-1]
	} else {
		if n := s.hintsCap(); n != cap(s.hints) {
			// Grown as hintGrowth said, for the room made for it.
			s.hints = append(make([]jitHintSlot, 0, n), s.hints...)
		}
		if len(s.hints) == 0 {
			// Slot 0 is no hint.
			s.hints = append(s.hints, jitHintSlot{})
		}
		i = uint32(len(s.hints))
		s.hints = append(s.hints, jitHintSlot{})
	}
	s.hints[i] = jitHintSlot{e: e, fn: uintptr(unsafe.Pointer(key.Value()))}
	e.hint = i
	s.account(e)
}

// hintsCap is the hint table's capacity once remember has taken a slot of
// it, and hintGrowth the bytes that adds to what the JIT holds: none while
// a slot is free, and the table's growth when it is full. A compile makes
// room for it with its code's, before remember: the table grew past the
// budget the room had been made in, a full one by half again.
func (s *jitState) hintsCap() int {
	n := len(s.hints) + 1
	if len(s.hints) == 0 {
		n++
	}
	if len(s.hintFree) != 0 || n <= cap(s.hints) {
		return cap(s.hints)
	}
	return max(n, 2*cap(s.hints), 16)
}

func (s *jitState) hintGrowth() int {
	return (s.hintsCap() - cap(s.hints)) * int(unsafe.Sizeof(jitHintSlot{}))
}

func (r *Runtime) releaseJIT() {
	if r.jit == nil {
		return
	}
	for key, e := range r.jit.cache {
		r.jit.dropEntry(key, e)
	}
	// Not while a native run is under way, suspended in a call of Go that
	// released it: it goes on with the state its contexts and pins are in.
	if len(r.jit.cache) == 0 && r.jit.ctxTop == 0 {
		r.jit = nil
	}
}

// jitBudget bounds what the JIT holds (jitState.held): its code and all
// that describes it, what it has learned, the programs it lowered, the
// contexts its code runs in, its compiles' workspaces. The memory meter
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
// the compiler's transient work -- analysis tables over every slot for each
// instruction, and the code it emits -- must. It costs a few comparisons
// and never measures the heap, so a refusal can be cheaply repeated.
func jitCompiles(fn *bytecode.Function, budget int) bool {
	// Top-level and module code is never compiled (lowering refuses it):
	// no entry remembers it, which each script a loop evaluates would
	// otherwise be.
	if fn.TopLevel || fn.IsModule {
		return false
	}
	return len(fn.Code)*ir.MaxSlots*8+32<<10 <= budget
}

// jitState is the runtime's JIT state, made at its first use.
func (r *Runtime) jitState() *jitState {
	if r.jit == nil {
		r.jit = &jitState{cache: make(map[weak.Pointer[bytecode.Function]]*jitEntry), arena: jit.NewArena(), stressBudget: int(r.jitStress.budget)}
		jitEpochOnce.Do(jitArmEpoch)
	}
	return r.jit
}

// jitFor is the entry for a closure's function, compiling it if need be with
// what the closure's caches know of its sites; nil when resources or the
// OS refuse one for now, which may change.
func (r *Runtime) jitFor(cl *closure) *jitEntry {
	return r.jitForFunction(cl.fn, cl)
}

// jitForFunction is jitFor for fn, with cl's caches, if cl is not nil. A
// program the new pipeline does not compile is a refusal the cache keeps,
// for good; one past the budget, until the cache releases code.
func (r *Runtime) jitForFunction(fn *bytecode.Function, cl *closure) *jitEntry {
	if r.jit != nil && r.jit.unavailable {
		return nil
	}
	if len(fn.Code) > jitcompile.MaxInstructions {
		return nil
	}
	key := weak.Make(fn)
	if r.jit != nil {
		if e := r.jit.cache[key]; e != nil {
			if !e.deferred || e.generation == r.jit.generation {
				return e
			}
			r.jit.forget(key, e)
		}
	}
	budget := r.jitBudget()
	if !jitCompiles(fn, budget) {
		if fn.TopLevel || fn.IsModule {
			// Refused for good, as before any entry: the runtime has run
			// the JIT, as a refusal the cache remembered had made it.
			r.jitState()
		}
		return nil
	}
	r.jitState()
	s := r.jit
	s.budget = budget
	// Room for as much as its entry is likely to hold (perInstr), made from
	// what is old -- or none, and it runs in the existing tiers for now.
	s.age(s.epoch())
	if !s.makeRoom(len(fn.Code) * s.perInstr) {
		return nil
	}
	s.serial++
	e := &jitEntry{used: s.epoch(), serial: s.serial}
	s.linkFor(fn, e)
	if p, err := r.jitLower(fn, jitLowerSSA); err == nil {
		if cl != nil {
			// The calls its caches have met.
			r.jitSeedCalls(cl, e, p)
		}
		code, fb, err := r.compileSSA(fn, cl, p, e)
		switch {
		case code != nil:
			e.setSSA(s, fn, p, code, fb)
			s.compiled++
			s.remember(key, e)
			s.settle()
			r.jitWantedCallees(e, fb)
			return e
		case errors.Is(err, jit.ErrUnavailable):
			s.unavailable = true
			s.disown(e)
			return nil
		case errors.Is(err, jit.ErrCodeBudget):
			// It may fit once the cache has released code. Until then the
			// cached refusal spares a compile at every warm-up, and holds
			// no more than itself: the program lowered for it, which the
			// retry lowers again, would take the room it is remembered in
			// -- with the young code holding the rest, no refusal was
			// remembered, and each warm-up compiled again.
			e.deferred, e.generation = true, s.generation
			if l := s.lowered[key]; l != nil {
				s.dropLowering(key, l)
			}
		}
	}
	if !s.makeRoom(jitEntryFixed + s.hintGrowth()) {
		// No room to remember the refusal in, for now.
		s.disown(e)
		s.settle()
		return nil
	}
	s.remember(key, e)
	// The compile's workspaces, kept only if there is the room, as after
	// a compile that places its code.
	s.settle()
	return e
}

// jitOn reports whether this runtime runs the JIT. The call path tests it
// before each hook, so with the JIT off a call pays this flag test and
// nothing else; without the JIT built in it is the constant false.
func (r *Runtime) jitOn() bool { return r.jitEnabled }

// tryJITFrame is the call path's hook, for a runtime that runs the JIT.
func (r *Runtime) tryJITFrame(f *frame) (Value, error, bool) {
	if f.cl.jitRefused {
		return Undefined, nil, false
	}
	if e := r.jit.hint(f.cl); e != nil && e.entrySlow && !e.reoptPending() {
		e.slowCalls++
		return Undefined, nil, false
	}
	// Cold calls stay in the existing tiers without allocating native state.
	// Saturating the counter keeps hot selection independent of call overflow.
	if f.cl.jitCalls < r.jitCallThreshold {
		f.cl.jitCalls++
		if f.cl.jitCalls < r.jitCallThreshold {
			return Undefined, nil, false
		}
	}
	return r.tryJITAt(f, 0, 0, false)
}

// jitBackEdge is executeAt's interrupted block in a build with the JIT: the
// same check, after which a loop that ran a full budget may promote.
func (r *Runtime) jitBackEdge(f *frame, pc uint32, sp int) (Value, error, bool) {
	fullBudget := r.backEdges == 0
	r.backEdges = r.jit.backEdgeBudget()
	if err := r.checkInterruptNow(); err != nil {
		// An interrupt is the host stopping the script rather than a
		// JavaScript exception, so it is not catchable.
		return Undefined, err, true
	}
	r.sweepStaleSlots(sp)
	return r.tryJITLoop(f, pc, sp, fullBudget)
}

// backEdgeBudget is the back-edge budget after a check: about
// backEdgeCheckInterval, varied from one check to the next. The budget is
// the interpreter's, the tree tier's and native code's alike, and runs out
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

// The existing back-edge interrupt budget supplies coarse work feedback.
// Promotion starts at the completed branch's target, never at function entry.
func (r *Runtime) tryJITLoop(f *frame, pc uint32, sp int, fullBudget bool) (Value, error, bool) {
	if !r.jitEnabled || !fullBudget || f.cl.jitRefused || int(r.jitDeoptDepth) == r.frameDepth || f.cl.fn.TopLevel || f.cl.fn.IsModule {
		return Undefined, nil, false
	}
	if f.cl.jitLoopDelay != 0 {
		f.cl.jitLoopDelay--
		return Undefined, nil, false
	}
	f.cl.jitCalls = r.jitCallThreshold
	if s := r.jit; s != nil && s.goOn == f {
		// The frame whose native code Go goes on in, at an entry it has:
		// in that run, not a run of its own (jitGoOn).
		if e := s.hint(f.cl); e != nil && e.ssa != nil && !e.entrySlow && e.ssa.HasEntry(int(pc)) {
			s.goOnPC, s.goOnDepth = int(pc), sp-f.base
			return Undefined, errGoOnNatively, true
		}
	}
	return r.tryJITAt(f, int(pc), sp-f.base, true)
}

func (r *Runtime) tryJITAt(f *frame, pc, depth int, osr bool) (Value, error, bool) {
	e := r.jit.hint(f.cl)
	if e == nil || e.ssa != nil && e.ssa.Size() == 0 {
		e = r.jitFor(f.cl)
		f.cl.setHint(hintFor(e))
	}
	if e != nil && e.ssa != nil {
		if e.notNative {
			jitRetryNative(e)
		}
		if e.inlinePending && e.inlineReopts < jitInlineReoptimizations {
			e.inlinePending, e.inlineReopt = false, true
		}
		if e.entrySlow && e.reoptPending() {
			e.slowTries++
		}
		if !osr {
			e.starts++
		} else {
			r.jit.osrs++
		}
		if e.reoptDue() {
			r.jitReoptimize(f.cl, e)
		}
		if e.entrySlow {
			// Its native stretches cost more at their exits than they save
			// (jitSSAProfit): the existing tiers run it -- until it is
			// compiled again for what it learned, its budget spent by their
			// tries to enter it (slowTries).
			return Undefined, nil, false
		}
		return r.runSSA(f, e, pc, depth)
	}
	if e == nil {
		if f.cl.fn.TopLevel || f.cl.fn.IsModule {
			// Never compiled (jitCompiles): refused for good, as a refusal
			// the cache remembered was, without an entry.
			f.cl.jitRefused = true
			return Undefined, nil, false
		}
		// Resource and OS refusals may change. Retry after another warmup,
		// rather than charging every call for an unsuccessful compilation.
		f.cl.jitCalls = 0
		f.cl.jitLoopDelay = jitHotCalls
		return Undefined, nil, false
	}
	if e.deferred {
		// Out of code budget: warm up again and look again then, which
		// costs a cache lookup until the cache releases code.
		f.cl.setHint(0)
		f.cl.jitCalls = 0
		f.cl.jitLoopDelay = jitHotCalls
		return Undefined, nil, false
	}
	f.cl.jitRefused = true
	return Undefined, nil, false
}

type jitBinary struct {
	left, right, dest, delta int
	literal                  Value
	strict, negate, branch   bool
	op                       bytecode.Op
}

func jitBinaryBoundary(in bytecode.Instr) bool {
	op := in.Op
	switch in.Op {
	case bytecode.OpBinLocal, bytecode.OpBinImm, bytecode.OpJumpIfCmpFalse:
		op = bytecode.Op(in.B)
	case bytecode.OpLocalBinImm:
		op = bytecode.Op(in.A >> 24)
	}
	return op == bytecode.OpMod || op >= bytecode.OpEq && op <= bytecode.OpStrictNe
}

// jitBinaryGeneric reports an instruction applying a binary arithmetic,
// bitwise or comparison operator, plain or fused with a local, an
// immediate or a branch, which jitBinaryAt describes.
func jitBinaryGeneric(in bytecode.Instr) bool {
	op := in.Op
	switch in.Op {
	case bytecode.OpBinLocal, bytecode.OpBinImm, bytecode.OpJumpIfCmpFalse:
		op = bytecode.Op(in.B)
	case bytecode.OpLocalBinImm:
		op = bytecode.Op(in.A >> 24)
	}
	return jitBinaryOperator(op)
}

// jitBinaryOperator reports a binary arithmetic, bitwise or comparison
// operator.
func jitBinaryOperator(op bytecode.Op) bool {
	return op >= bytecode.OpAdd && op <= bytecode.OpPow ||
		op >= bytecode.OpBitAnd && op <= bytecode.OpUShr && op != bytecode.OpBitNot ||
		op >= bytecode.OpEq && op <= bytecode.OpGe
}

// jitArith is an arithmetic or bitwise operator's value, as the
// interpreter computes it.
func (r *Runtime) jitArith(op bytecode.Op, a, b Value) (Value, error) {
	switch op {
	case bytecode.OpAdd:
		if a.IsNumber() && b.IsNumber() {
			return Float(a.Number() + b.Number()), nil
		}
		return r.add(a, b)
	case bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod, bytecode.OpPow:
		if a.IsNumber() && b.IsNumber() {
			return Float(numericOp(op, a.Number(), b.Number())), nil
		}
		return r.arith(op, a, b)
	}
	return r.bitwise(op, a, b)
}

// Operands use the native layout: locals, upvalues, then the live stack.
// The original bytecode retains strict/loose comparison semantics. Remainder
// also uses this layout, including fused local and immediate operands.
func jitBinaryAt(in bytecode.Instr, sp int) (jitBinary, bool) {
	c := jitBinary{left: sp - 2, right: sp - 1, dest: sp - 2, delta: -1}
	op := in.Op
	switch in.Op {
	case bytecode.OpJumpIfCmpFalse:
		op = bytecode.Op(in.B)
		c.branch, c.delta = true, -2
	case bytecode.OpBinLocal:
		op = bytecode.Op(in.B)
		c.left, c.right, c.dest, c.delta = sp-1, int(in.A), sp-1, 0
	case bytecode.OpBinImm:
		op = bytecode.Op(in.B)
		c.left, c.right, c.dest, c.delta = sp-1, -1, sp-1, 0
		c.literal = Float(float64(int32(in.A)))
	case bytecode.OpLocalBinImm:
		op = bytecode.Op(in.A >> 24)
		c.left, c.right, c.dest, c.delta = int(in.A&0xffffff), -1, sp, 1
		c.literal = Float(float64(int32(in.B)))
	default:
		if !jitBinaryOperator(op) {
			return c, false
		}
	}
	c.op = op
	c.strict = op == bytecode.OpStrictEq || op == bytecode.OpStrictNe
	c.negate = op == bytecode.OpNe || op == bytecode.OpStrictNe
	return c, c.strict || op == bytecode.OpEq || op == bytecode.OpNe || op == bytecode.OpMod
}

type jitTreeExit struct {
	value Value
	err   error
}

// tryJITTreeLoop is the tree's back-edge hook. Its gate inlines into
// backEdgeCheck, so with the JIT off a check pays a flag test.
func (r *Runtime) tryJITTreeLoop(c *tctx, pc, depth int, fullBudget bool) {
	if r.jitEnabled {
		r.tryJITTreeLoopEnabled(c, pc, depth, fullBudget)
	}
}

func (r *Runtime) tryJITTreeLoopEnabled(c *tctx, pc, depth int, fullBudget bool) {
	// The tree's check set the interval: the budget varies about it.
	r.backEdges = r.jit.backEdgeBudget()
	if v, err, done := r.tryJITLoop(c.f, c.cl.fn.Code[pc].A, c.f.base+depth, fullBudget); done {
		if isGeneratorTemplate(c.cl.fn) {
			// A generator's tree runs under runGeneratorTree's recover,
			// not runTree's: the run's end is told it as the tree tells
			// it -- a suspension in treeSuspend, a return in ret, an
			// exception as a throw, whose loops' iterators the run closed
			// already (jitInterpret), closing them again doing nothing.
			switch s, ok := err.(*suspendSignal); {
			case ok:
				if s != &r.treeSig {
					r.treeSig = *s
				}
				r.treeSuspend = &r.treeSig
				panic(treeThrow{})
			case err == nil:
				c.ret = v
				panic(treeThrow{})
			}
			panic(treeThrow{err: err})
		}
		// Tree branch nodes return block indices. Unwind to runTree only
		// after native execution or its interpreter fallback has completed.
		panic(jitTreeExit{v, err})
	}
}

func jitTreeResult(p any) (Value, error, bool) {
	if exit, ok := p.(jitTreeExit); ok {
		return exit.value, exit.err, true
	}
	return Undefined, nil, false
}

// jitCallAt makes the call, method call or construction in, its operands
// on the VM's stack up to sp, as the interpreter makes it: its result,
// and where on the stack the result goes.
func (r *Runtime) jitCallAt(in bytecode.Instr, sp int) (Value, int, error) {
	stack := r.stack
	argc := int(in.A)
	args := stack[sp-argc : sp]
	callee := stack[sp-argc-1]
	sp -= argc + 1
	if in.Op == bytecode.OpNew {
		// A construction native code did not make, as the interpreter
		// makes it.
		v, err := r.construct(callee, args)
		if err == nil && v.IsObject() && callee.IsObject() && callee.Object() == r.proto.arrayCtor {
			// With the layout the arrays native code makes have
			// (jitFillPool): one made at an exit for its pool is used
			// as they are.
			r.ensureShape(v.Object())
		}
		return v, sp, err
	}
	this := Undefined
	if in.Op == bytecode.OpCallMethod {
		sp--
		this = stack[sp]
	}
	v, err := r.callDirect(callee, this, args)
	return v, sp, err
}

// jitHost runs an explicitly lowered host operation with normal VM ordering.
// No borrowed view or native scalar root survives a callback. Consecutive host
// operations and intervening fused local copies share one frame publication;
// sixteen instructions bound the Go batch before native selection resumes.
func (r *Runtime) jitHost(f *frame, sp, limit int) (int, int, error) {
	steps := 0
	for range limit {
		pc := int(f.pc)
		in := f.cl.fn.Code[pc]
		n := len(f.locals) + len(f.cl.upvalues)
		if jitBinaryBoundary(in) || jitBinaryGeneric(in) {
			// The new pipeline's code also leaves an operation whose
			// speculation failed before to Go (ssa.Feedback's Generic).
			cmp, _ := jitBinaryAt(in, n+sp-f.base)
			left := r.jitFrameValue(f, cmp.left)
			right := cmp.literal
			if cmp.right >= 0 {
				right = r.jitFrameValue(f, cmp.right)
			}
			f.pc++
			sp += cmp.delta
			steps++
			if cmp.op < bytecode.OpEq || cmp.op > bytecode.OpGe {
				value, err := r.jitArith(cmp.op, left, right)
				if err != nil {
					return sp, steps, err
				}
				r.stack[f.base+cmp.dest-n] = value
				continue
			}
			var result bool
			if cmp.op >= bytecode.OpLt {
				if left.IsNumber() && right.IsNumber() {
					result = compareFloats(cmp.op, left.Number(), right.Number())
				} else {
					c, err := r.compare(left, right)
					if err != nil {
						return sp, steps, err
					}
					result = relationalResult(cmp.op, c)
				}
			} else if result = left.StrictEquals(right); !cmp.strict {
				var err error
				result, err = r.looseEquals(left, right)
				if err != nil {
					return sp, steps, err
				}
			}
			result = result != cmp.negate
			if cmp.branch {
				if !result {
					f.pc = in.A
				}
			} else {
				r.stack[f.base+cmp.dest-n] = Bool(result)
			}
			continue
		}
		if in.Op == bytecode.OpThrow {
			// Made here, so that a handler of the function's that catches
			// it goes on natively (jitCatch), as the tree tier's throw
			// goes on to its handler (throwToHandler).
			f.pc++
			steps++
			return sp - 1, steps, r.throw(r.stack[sp-1])
		}
		if in.Op == bytecode.OpRethrow {
			// A finally clause's normal completion: its record dropped. Any
			// other the interpreter reproduces.
			if k := r.stack[sp-1]; !k.IsNumber() || completionKind(k.Number()) != completionNormal {
				return sp, steps, nil
			}
			sp -= 2
			f.pc++
			steps++
			continue
		}
		if in.Op == bytecode.OpSetLocalGet {
			f.locals[in.A] = r.stack[sp-1]
			r.stack[sp-1] = f.locals[in.B]
			f.pc++
			steps++
			continue
		}
		switch in.Op {
		case bytecode.OpPushThis, bytecode.OpGetProp, bytecode.OpSetProp, bytecode.OpCall,
			bytecode.OpCallMethod, bytecode.OpGetGlobal, bytecode.OpSetGlobal, bytecode.OpSetGlobalStrict,
			bytecode.OpGetPropThis, bytecode.OpSetIndex, bytecode.OpNewArray,
			bytecode.OpGetIndex, bytecode.OpNew, bytecode.OpPushConst, bytecode.OpInstanceOf, bytecode.OpTypeOf,
			bytecode.OpCheckGlobalRef, bytecode.OpAssertResolved, bytecode.OpNewObject, bytecode.OpNewRegExp, bytecode.OpDefineField,
			bytecode.OpPushEmptyString, bytecode.OpGetUpvalue, bytecode.OpGetUpvalueCheck, bytecode.OpSetUpvalue,
			bytecode.OpInitUpvalue, bytecode.OpSetUpvalueCheck, bytecode.OpConcat, bytecode.OpNewDisposeCapability, bytecode.OpAddDisposable,
			bytecode.OpForOfStart, bytecode.OpForInStart, bytecode.OpIterNextOrJump, bytecode.OpIterClose, bytecode.OpIterCloseNormal:
		case bytecode.OpDispose:
			if in.B != 0 {
				// A step of an await using scope's disposal.
				return sp, steps, nil
			}
		default:
			return sp, steps, nil
		}
		f.pc++
		steps++
		stack := r.stack
		var v Value
		var err error
		switch in.Op {
		case bytecode.OpNewArray:
			n := int(in.A)
			var o *Object
			o = r.newArrayFrom(stack[sp-n : sp])
			v = Obj(o)
			sp -= n
		case bytecode.OpConcat:
			n := int(in.A)
			var s *String
			if s, err = r.jitConcat(stack[sp-n : sp]); err != nil {
				return sp, steps, err
			}
			v = Str(s)
			sp -= n
		case bytecode.OpPushThis:
			var bound bool
			v, bound = f.thisValue()
			if !bound {
				err = r.throwError(errReference, "\"this\" is not bound until super() has been called")
			}
		case bytecode.OpGetProp:
			// Through the site's cache first, as the interpreter reads it.
			sp--
			v, err = r.jitGetProp(f.cl, in, stack[sp])
		case bytecode.OpSetProp:
			sp -= 2
			obj, val := stack[sp], stack[sp+1]
			if obj.IsObject() {
				err = r.setPropCached(&f.cl.ic[in.B], obj.Object(), f.cl.names[in.A], val, f.cl.fn.Strict)
			} else {
				err = r.setValueProp(obj, f.cl.names[in.A], val, f.cl.fn.Strict)
			}
			if err != nil {
				return sp, steps, err
			}
			continue
		case bytecode.OpCheckGlobalRef:
			var found bool
			found, err = r.jitGlobalRef(f, in)
			v = Bool(found)
		case bytecode.OpAssertResolved:
			if !stack[sp-2].Truthy() {
				return sp, steps, r.throwReferenceError("%s is not defined", r.atoms.name(f.cl.names[in.A]))
			}
			stack[sp-2] = stack[sp-1]
			sp--
			continue
		case bytecode.OpPushEmptyString:
			v = Str(emptyString)
		case bytecode.OpGetUpvalue, bytecode.OpGetUpvalueCheck:
			// As the interpreter reads a captured binding.
			v = f.cl.upvalues[in.A].get()
			if in.Op == bytecode.OpGetUpvalueCheck && v.IsUninitialized() {
				return sp, steps, r.throwReferenceError("cannot access %q before initialization", f.cl.fn.Upvalues[in.A].Name)
			}
		case bytecode.OpSetUpvalue, bytecode.OpInitUpvalue, bytecode.OpSetUpvalueCheck:
			// As the interpreter assigns one.
			if in.Op == bytecode.OpSetUpvalueCheck {
				if f.cl.upvalues[in.A].get().IsUninitialized() {
					return sp, steps, r.throwReferenceError("cannot access %q before initialization", f.cl.fn.Upvalues[in.A].Name)
				}
				if !f.cl.fn.Upvalues[in.A].Mutable {
					return sp, steps, r.throwTypeError("assignment to constant variable %q", f.cl.fn.Upvalues[in.A].Name)
				}
			}
			sp--
			f.cl.upvalues[in.A].set(stack[sp])
			continue
		case bytecode.OpNewRegExp:
			v, err = r.newRegExpLiteral(&f.cl.fn.Constants[in.A])
		case bytecode.OpNewObject:
			// As the interpreter makes a literal's object.
			o := newLiteralObject(r.proto.object, ClassObject, int(in.A))
			o.shape = r.shapes.siteRoot(&f.cl.ic[in.B])
			v = Obj(o)
		case bytecode.OpDefineField:
			sp--
			val, obj := stack[sp], stack[sp-1]
			if obj.IsObject() {
				if o := obj.Object(); o.class == ClassObject && o.flags&objExtensible != 0 {
					if next := o.transition(f.cl.names[in.A], propDefault); next != nil {
						o.appendTransition(Property{key: f.cl.names[in.A], flags: propDefault, value: val}, next)
						continue
					}
				}
				if err := r.defineOwnProp(obj.Object(), f.cl.names[in.A], val, propDefault); err != nil {
					return sp, steps, err
				}
			}
			continue
		case bytecode.OpSetGlobal, bytecode.OpSetGlobalStrict:
			// As the interpreter assigns: a direct eval's variable, a
			// script's lexical binding, or the global object's property.
			sp--
			if in.Op == bytecode.OpSetGlobal {
				err = r.setGlobalIn(f, f.cl, in, stack[sp])
			} else {
				err = r.setGlobalStrict(f, f.cl, in, stack[sp])
			}
			if err != nil {
				return sp, steps, err
			}
			continue
		case bytecode.OpSetIndex:
			sp -= 3
			obj, key, value := stack[sp], stack[sp+1], stack[sp+2]
			if !(key.IsNumber() && setElem(obj, key, value)) {
				if err := r.setIndexed(obj, key, value, f.cl.fn.Strict); err != nil {
					return sp, steps, err
				}
			}
			continue
		case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
			v, sp, err = r.jitCallAt(in, sp)
		case bytecode.OpGetGlobal:
			c := tctx{r: r, f: f, cl: f.cl, locals: f.locals}
			v, err = r.getGlobalAt(&c, in, pc)
		case bytecode.OpGetPropThis:
			recv := stack[sp-1]
			if recv.IsObject() {
				var ok bool
				if v, ok = r.cachedProp(&f.cl.ic[in.B], recv.Object(), f.cl.names[in.A]); ok {
					break
				}
			}
			v, err = r.getValueProp(recv, f.cl.names[in.A])
		case bytecode.OpGetIndex:
			sp -= 2
			v, err = r.getIndexed(stack[sp], stack[sp+1])
		case bytecode.OpPushConst:
			// A string the new pipeline's code has Go push.
			v = f.cl.consts[in.A]
		case bytecode.OpTypeOf:
			sp--
			v = Str(r.typeofString(stack[sp]))
		case bytecode.OpInstanceOf:
			// One the new pipeline's code did not answer itself.
			sp -= 2
			var yes bool
			yes, err = r.instanceOfAt(&f.cl.ic[in.B], stack[sp], stack[sp+1])
			v = Bool(yes)
		case bytecode.OpForOfStart, bytecode.OpForInStart:
			// A loop's cursor, as executeAt's cases make it.
			sp--
			if in.Op == bytecode.OpForOfStart {
				v, err = r.startForOf(stack[sp])
			} else {
				v, err = r.startForIn(stack[sp])
			}
		case bytecode.OpIterNextOrJump:
			// The cursor stays: its next value, or, done, the loop's end,
			// which native code goes on at (an entry, ir.Program's
			// Entries). Native code polls at the loop's back edge.
			var ok bool
			if v, ok, err = r.iterNext(stack[sp-1]); err == nil && !ok {
				f.pc = in.A
				continue
			}
		case bytecode.OpIterClose:
			if err := r.iterCloseNormal(stack[sp-1]); err != nil {
				return sp, steps, err
			}
			continue
		case bytecode.OpIterCloseNormal:
			sp--
			if err := r.iterCloseNormal(stack[sp]); err != nil {
				return sp, steps, err
			}
			continue
		case bytecode.OpNewDisposeCapability, bytecode.OpAddDisposable, bytecode.OpDispose:
			var push bool
			if sp, v, push, err = r.jitUsing(in, sp); err != nil {
				return sp, steps, err
			}
			if !push {
				continue
			}
		default:
			panic("invalid JIT host operation")
		}
		if err != nil {
			return sp, steps, err
		}
		r.stack[sp] = v
		sp++
	}
	return sp, steps, nil
}

// jitConcat is concat as the interpreter does it: every part converted
// before any is joined, a number left as it is, and the length checked
// before and after the join.
func (r *Runtime) jitConcat(parts []Value) (*String, error) {
	for i := range parts {
		if parts[i].IsString() || parts[i].IsNumber() {
			continue
		}
		s, err := r.toString(parts[i])
		if err != nil {
			return nil, err
		}
		parts[i] = Str(s)
	}
	total := 0
	for _, p := range parts {
		if p.IsString() && total <= maxStringLength {
			total += p.String().length
		}
	}
	if total > maxStringLength {
		return nil, r.throwStringLength()
	}
	out, err := r.joinParts(parts)
	if err != nil {
		return nil, err
	}
	if out.length > maxStringLength {
		return nil, r.throwStringLength()
	}
	return out, nil
}

// jitUsing runs in, new_dispose_capability, add_disposable or dispose (but
// a step of an await using scope's disposal), with the stack's top at sp,
// as executeAt's cases do: the top after it, and the value it pushes, if
// push.
func (r *Runtime) jitUsing(in bytecode.Instr, sp int) (int, Value, bool, error) {
	stack := r.stack
	switch in.Op {
	case bytecode.OpNewDisposeCapability:
		o := newObject(nil, ClassObject)
		o.data = &disposeCapability{}
		return sp, Obj(o), true, nil
	case bytecode.OpAddDisposable:
		sp--
		c := stack[sp].Object().data.(*disposeCapability)
		return sp, Undefined, false, r.addDisposableResource(c, stack[sp-1], in.A == 1)
	}
	sp--
	c := stack[sp].Object().data.(*disposeCapability)
	// A finally clause knows from its record whether an exception is what
	// is leaving the scope, which the disposal folds into what it throws.
	var completion error
	if in.A&bytecode.DisposeRecord != 0 && completionKind(stack[sp-1].Number()) == completionThrow {
		completion = r.throw(stack[sp-2])
	}
	if in.A&bytecode.DisposeAsync != 0 && c.hasAsync() {
		p := r.newPromise()
		r.disposeResourcesAsync(c, completion, func(err error) {
			if err == nil || err == completion {
				r.resolvePromise(p, Undefined)
				return
			}
			r.rejectPromise(p, thrownValue(err))
		})
		return sp, Obj(p), true, nil
	}
	if err := r.disposeResources(c, completion); err != nil && err != completion {
		return sp, Undefined, false, err
	}
	return sp, Undefined, in.A&bytecode.DisposeAsync != 0, nil
}

// jitGetProp is get_prop as the interpreter does it for an object: its own
// small table, then the site's cache, then the generic read.
func (r *Runtime) jitGetProp(cl *closure, in bytecode.Instr, obj Value) (Value, error) {
	if obj.IsObject() {
		o := obj.Object()
		if v, ok := plainOwn(o, cl.names[in.A]); ok {
			return v, nil
		}
		if v, ok, err := r.cachedGet(&cl.ic[in.B], o, cl.names[in.A]); ok {
			return v, err
		}
	}
	return r.getValueProp(obj, cl.names[in.A])
}

func (r *Runtime) jitFrameValue(f *frame, index int) Value {
	if index < len(f.locals) {
		return f.locals[index]
	}
	index -= len(f.locals)
	if index < len(f.cl.upvalues) {
		return f.cl.upvalues[index].get()
	}
	return r.stack[f.base+index-len(f.cl.upvalues)]
}

func (r *Runtime) jitInterpret(f *frame, sp int, pending error) (Value, error, bool) {
	if _, ok := pending.(*Thrown); ok {
		// Unwound here, as executeAt's onError unwinds what it throws: one
		// nothing in the frame catches leaves it, every loop it was in
		// abandoned, its iterators closed -- which executeAt does not for
		// an exception it is given.
		var caught bool
		if sp, caught = r.unwindToHandler(f, sp, pending); !caught {
			r.closeIteratorsIn(f.base, sp)
			return Undefined, pending, true
		}
		pending = nil
	}
	if r.jit != nil {
		r.jit.interpreted++
	}
	previous := r.jitDeoptDepth
	r.jitDeoptDepth = uint32(r.frameDepth)
	defer func() { r.jitDeoptDepth = previous }()
	v, err := r.executeAt(f, sp, pending)
	return v, err, true
}

// jitGlobalRef is strict mode's check_global_ref, as the interpreter makes
// it: whether the name resolves, to a global variable, a direct eval's
// variable, a script's or a module's lexical binding, or a property the
// global object has or inherits.
func (r *Runtime) jitGlobalRef(f *frame, in bytecode.Instr) (bool, error) {
	cl := f.cl
	name, env := cl.names[in.A], cl.scope()
	switch {
	case hasOwnGlobal(env, &cl.ic[in.B], name):
	case f.evalVars != nil && evalVarProp(f.evalVars, name) != nil:
	case r.globalLexProp(env, name) != nil:
	case !r.isGlobalScope(env) && r.moduleLexProp(env, name) != nil:
	case r.nodeQuirks && chainHasProxy(env):
	default:
		return r.hasPropErr(env, name)
	}
	return true, nil
}
