//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
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

// Native code made from register code (docs/register-interpreter.md,
// "Tiering to the JIT"): a function's register code lowered
// (compile.LowerReg), built with its feedback (jitRegFeedback), optimized
// and compiled by the JIT's backend (jitRegFor), and run on the register
// frame's window in place (jitRegRun), its exits going on in the register
// interpreter's frame: a host exit runs the register instruction whole by
// the interpreter's slow paths (regWhole) and native code goes on at the
// next entry; a deoptimization, and an exit after which the code has no
// entry, have runReg run the rest of the invocation.

// jitRegEntry is a function's code compiled from its register code, in
// the JIT's cache (jitState's regCache), or the reason none is.
type jitRegEntry struct {
	p    *ir.Program
	code *jit.SSACode
	// kinds are, for typed code, the kinds of its code's slots (ssa.Func's
	// Kinds): its program's, and its inlined typed callees' past them,
	// which its native calls' records write raw (jitRegApplyRecords).
	kinds []uint8
	// keep is what the code reads by address -- the shapes, holders and
	// constants' cells its feedback answered with -- kept alive with it.
	keep *jitFeedback
	// keeps is how many keep cells the code uses; scratch how many scratch
	// slots its program has (ssa.Func's Scratch), past the window in
	// memory; and frame how many slots past those its runs take: the homes
	// of the objects it does not make and its loops' slots (ssa.Func's
	// HomesEnd, LoopsEnd).
	keeps, scratch, frame int
	// loops are its loops' headers that it has entries at, by register
	// pc, sorted: where a frame in the register interpreter goes on in it
	// by on-stack replacement (loopEntry).
	loops []uint32
	// seen, used, flushSeen and flushAt are what the cache ages it by
	// (regAge, regFlush).
	seen, used, flushSeen, flushAt uint64
	// invalid marks code a guard of which failed (jitRegInvalidate): its
	// runs finish in the interpreter, and the function is compiled again
	// once the interpreter has spent a fresh budget (jitRegWarmUp). runs
	// counts the runs under way, which keep it from being closed; dropped
	// marks code the cache let go of while one was, closed once the last is
	// done (jitRegStretch).
	invalid bool
	runs    int
	dropped bool
	// busy marks code a compile of its function replaces while the compile
	// makes room for its own (jitRegPlace), which counts this code's room
	// as given back: the cache does not let go of it meanwhile.
	busy int
	// key is its function's key in the cache, and life the function's
	// state that outlives its compiles (jitRegLifeOf): what its code
	// learned, its budget, its deoptimizations, what its runs did.
	key  jitKey
	life *jitEntry
	// hostOnly marks a refusal of host operations alone (jitRegCompile),
	// which a later ask lowers again.
	hostOnly bool
	// callee marks code of host operations alone compiled for native
	// callers (LowerRegInline), which is compiled again so
	// (jitRegReoptimize): compiled for Go to enter, it was refused at every
	// entry, never replaced.
	callee bool
	// levels counts the native calls under way of the code that Go runs
	// beside (jitRegLevelAt): the cache neither drops it nor compiles it
	// again while any is.
	levels int
	bytes  int
	// cached says the cache holds e for its function (jitRegCached),
	// which a level finds through its closure's template (jitRegLevelAt).
	cached  bool
	refusal string
	// retPC and retUndef are where the interpreter ends a frame the code
	// returned from by on-stack replacement, retReg the register retPC's
	// ret returns, and goesOn whether it can (findReturns).
	retPC, retReg, retUndef int
	goesOn                  bool
	// serial numbers the compile (jitState's regSerial): a property site
	// that deoptimizes for a shape again in code compiled since it first
	// did is megamorphic (jitDeoptSeenOp).
	serial uint32
	// rc is the register code it was compiled from.
	rc *bytecode.RegCode
	// loopMean is its loops' mean length in the program's instructions,
	// at least one, which a stretch's back edges are counted by
	// (jitRegStretchWork, irLoopMean); callsNative says the code makes
	// native calls (jitRegCodeCallsNatively).
	loopMean    int
	callsNative bool
	// unentered are the register pcs of the loops' headers whose entries
	// escape analysis dropped (ssa.Func's Unentered); osr is the code
	// compiled for a frame waiting at one (jitRegLoopCode), kept apart from
	// this, osrOf in it the code it was compiled beside; osrMiss and
	// osrMissPC the frame, by its address, and the header a loop's check
	// last found no entry at (jitRegLoopCode); osrJob that code's compile
	// in the background, which the frames waiting at the loop wait out in
	// the interpreter until it is installed (jitRegInstall).
	unentered  []int
	osr, osrOf *jitRegEntry
	osrJob     *jitRegJob
	// kept are the register pcs of the loops' headers it has entries at
	// only for the frame its compile was asked for at (ssa.Func's
	// EntryKept), which a function called again gives up (jitRegKeptGone).
	kept      []int
	osrMiss   uintptr
	osrMissPC int
	// at is, for each register pc, the program's first instruction of its
	// lowering, -1 for none (ipc).
	at []int32
	// unresumed are the register pcs after calls the code has no entry
	// after, Go not having gone on there (ssa.Func's Unresumed): where Go
	// goes on at one, the code is compiled again with the entry
	// (jitRegResumeSeen).
	unresumed []int
	// targets are the calls' targets its function had learned when the code
	// was placed (jitTargetsOf), which it is compiled for
	// (jitRegCallRepeats).
	targets []jitTarget
}

// loopEntry is the entry, an instruction of the program, of the loop
// whose header is register pc rpc, if the code has one there: the OSR
// entry map, each loop header's register pc to the build's entry at the
// first instruction of its lowering, its live registers loaded from the
// window (ir.Program's Live).
func (e *jitRegEntry) loopEntry(rpc int) (int, bool) {
	if _, ok := slices.BinarySearch(e.loops, uint32(rpc)); !ok {
		return 0, false
	}
	return e.ipc(rpc), true
}

// ipc is the first instruction of the program's lowering of register pc
// rpc (ir.Program's At), -1 where there is none, by a table made as the
// code is placed (at): every entry, at a call, a loop or after an exit,
// asks.
func (e *jitRegEntry) ipc(rpc int) int {
	if uint(rpc) < uint(len(e.at)) {
		return int(e.at[rpc])
	}
	return e.p.At(uint32(rpc))
}

// jitRegFor is cl's register code c compiled, from the cache or anew; nil
// where the JIT has no memory for it now. An entry with no code holds the
// refusal.
func (r *Runtime) jitRegFor(cl *closure, c *regCode) *jitRegEntry {
	return r.jitRegForAs(cl, c, false)
}

// jitRegForAs is jitRegFor, for native callers where callee says
// (jitRegNativeCallee): a function of host operations alone, which a
// compile for Go to enter refuses but where its calls are native ones
// (jitRegCompile), is compiled for them (LowerRegInline). A refusal for
// that is not kept: asked again, the function is lowered again.
func (r *Runtime) jitRegForAs(cl *closure, c *regCode, callee bool) *jitRegEntry {
	s := r.jitState()
	if s.unavailable {
		return nil
	}
	key := r.jitRegKeyOf(cl)
	if j := s.regPendingJobs[key]; j != nil {
		// Being compiled in the background (jitRegSubmit): installed at
		// a later check -- this ask one where the compile is done, as a
		// call that enters the function is, outside QJS_JIT_STRESS's
		// schedule of checks. Native callers' calls of a callee Go makes
		// ask here and enter no code of the callee's: Are We Fast Yet's
		// DeltaBlue called markUnsatisfied through Go 11,000 times with
		// its compile done, between the checks that installed it.
		if j.state.Load() != jitJobDone || r.jitStress.background {
			return nil
		}
		r.jitDrain()
		if e := s.regCache[key]; e != nil && e.code != nil && !e.invalid && s.regPendingJobs[key] == nil {
			return e
		}
		return nil
	}
	if e := s.regCache[key]; e != nil {
		if !e.invalid && !e.hostOnly {
			return e
		}
		if s.regCalled(e) {
			// Run by native calls under way, which find their code in the
			// cache (jitRegLevelAt): compiled again once they are done.
			return nil
		}
	}
	if l := s.regLearned[key]; l != nil && l.deferred && l.generation == s.generation {
		// Deferred for want of room, and the cache has let no code go since
		// (jitRegPlace): a lookup, not a compile's preparation and the
		// settling after it.
		s.refusedRoom++
		return nil
	}
	return r.jitRegCompileFor(cl, c, key, callee)
}

// jitRegCompileFor compiles cl's function's register code c anew, the
// function's key key, and replaces what the cache holds for it with the
// result: runs of the code it replaces go on in it until they next leave it
// (jitRegGoOnAt), and it is closed once the last is done. nil where there
// is no room, or the compile went to the background. A refusal replaces no
// valid code: the function keeps it. Whatever came of it, the JIT holds no
// more than its budget after (settle): the programs of the callees it
// inlined, lowered as it built, count past the room it asked for, and a
// compile refused or deferred had left them over the budget.
func (r *Runtime) jitRegCompileFor(cl *closure, c *regCode, key jitKey, callee bool) (e *jitRegEntry) {
	// The code it returns, which its caller enters, is kept (settleKeeping).
	s := r.jit
	s.settleAfter++
	defer func() {
		s.settleAfter--
		s.settleKeeping(e)
	}()
	e = r.jitRegCompile(cl, c, key, callee, 0)
	if e == nil {
		return nil
	}
	if old := r.jit.regCache[key]; e.code == nil && old != nil && old.code != nil && !old.invalid {
		return e
	}
	r.jitRegCached(key, e)
	return e
}

// jitRegCached keeps e, the code of the function whose key is key, in the
// cache, its link the code's, in place of what the cache held for it,
// which is let go of; and counts the compile, the function's first or
// again (JITStats' Compiled and Reoptimized, JITReport's Reopts).
func (r *Runtime) jitRegCached(key jitKey, e *jitRegEntry) {
	s := r.jit
	if s.regCache == nil {
		s.regCache = map[jitKey]*jitRegEntry{}
	}
	if old := s.regCache[key]; old != nil && old != e {
		s.dropReg(key, old)
	}
	s.regCache[key] = e
	e.cached = true
	s.regHeld += e.bytes
	if life := e.life; life != nil && e.code != nil && life.ssaSerial != 0 {
		// Compiled again: what it leaves on is counted afresh, and it is
		// judged afresh -- it may have left on what it is compiled for now
		// -- so Go enters it and native callers call it until the new code
		// is found not to pay in turn; its backoffs stay.
		if l := s.regLinks[key]; l != nil {
			l.in = 0
		}
		life.nativeOut, life.notNative, life.nativeRetry, life.entrySlow = 0, false, 0, false
		if !life.keptGone {
			// Judged by what it does from now on (its ssaStats begun
			// afresh); what it did stays counted for its report. But code
			// compiled again only to give up the entries kept for a frame
			// (jitRegKeptGone) does as it did, and is judged by all of it.
			st := &life.ssaStats
			st.judged = st.entries + st.goCalls
			st.workBase, st.stretchBase = st.work, st.judged
			st.runGoCalls, st.runWork, st.osrStretches, st.osrWork = 0, 0, 0, 0
		}
		// Its calls of Go as a native callee too, against its native
		// callers' calls counted afresh (its link's in, above): counted
		// over its compiles while those were not, code compiled again was
		// judged at once by calls its native callers' were never counted
		// against -- CD's putIntoMap, AWFY's Json's callbacks, judged not
		// to pay on their first look. Reset with the link's in, which is
		// reset whether or not the code was compiled again only to give up
		// its kept entries.
		st := &life.ssaStats
		st.calleeGoCalls, st.calleeWork = 0, 0
	}
	if life := e.life; life != nil && e.code != nil {
		// Its calls since, which give up the entries kept for a frame
		// (jitRegEnter).
		life.keptGone, life.keptAt, life.keptLeft = false, life.starts, 0
	}
	s.regLinkUpdate(key, e)
	if e.code != nil && e.keep != nil {
		s.regJoinCallers(key, e.keep.links)
	}
	if e.code != nil {
		defer r.jitRegCalleesKeptNoEntries(e)
	}
	if life := e.life; life != nil && e.code != nil {
		if life.regFlushed {
			// Compiled again once hot again, its code let go of before
			// (regLetGo).
			life.regFlushed = false
			s.recompiled++
		}
		if life.ssaSerial++; life.ssaSerial == 1 {
			s.compiled++
		} else {
			s.reoptimized++
			if life.reopts < ^uint8(0) {
				life.reopts++
			}
		}
		// Valid code: no budget to spend before the next compile, nothing
		// learned pending.
		life.invalid, life.warming, life.reopt, life.learning = false, false, false, false
		if life.genLearned {
			// Its loops' steps over generators resume them natively now,
			// which each had been a call of Go: the loops it was judged
			// slow for are judged afresh.
			life.genLearned, life.loopSlow = false, false
		}
	}
	// What the function learned for this compile, counted.
	s.regAccount(e.life)
}

// jitCompileNote, where a test sets it (recordJITCompilesForTest), is
// told of each closure whose register code r compiles.
var jitCompileNote func(r *Runtime, cl *closure)

// jitRegCompile is jitRegFor's compile: LowerReg, the SSA build with
// jitRegFeedback, Optimize, the backend, the code placed in the JIT's
// arena. A refusal is an entry with no code; nil is no room, or a compile
// handed to the background (jitRegSubmit), installed at a later check
// (jitRegInstall).
//
// frameAt, where not 0, is the register pc of a loop's header whose entry
// a frame waits at (ssa.Func's FrameAt, which escape analysis keeps): code
// for on-stack replacement there (jitRegLoopCode), compiled in the
// background where the function's code is, as V8's OSR compiles are
// concurrent: the frame goes on in the interpreter until it is installed
// (jitRegInstall). Where it is 0, it is the loop a frame of the function
// waits at for this compile, if any (jitRegFrameAt).
func (r *Runtime) jitRegCompile(cl *closure, c *regCode, key jitKey, callee bool, frameAt int) *jitRegEntry {
	s := r.jit
	osrAt := frameAt
	if frameAt == 0 {
		frameAt = r.jitRegFrameAt(cl)
	}
	if !jitCompiles(cl.fn, r.jitBudget()) {
		// Too large for the JIT's memory (jitCompiles): no room, at every
		// ask.
		return nil
	}
	background := r.jitInBackground(cl.fn)
	if background && (s.jobsFull() || osrAt == 0 && s.regPendingJobs[key] != nil) {
		// As many compiles in the background as there may be, or one of
		// the function's under way or waiting to be installed
		// (jitRegInstallWaits): asked again at a later sample.
		return nil
	}
	// Young from its compile on (jitCodeAge): made unused since epoch 0,
	// code compiled when the budget was full was the first that the next
	// compile's room let go of, and the compiles that filled it dropped
	// each other's code.
	life := s.regLearnedOf(key)
	if life.deferred && life.generation == s.generation {
		// No room for it at its last compile, and the cache has let no
		// code go since (jitRegPlace).
		s.refusedRoom++
		return nil
	}
	e := &jitRegEntry{key: key, life: life, used: s.epoch()}
	// Its state kept while the compile fills and counts it: the room made
	// for the code would otherwise let go of the state of the very function
	// it is made for (regLifeSpare), whose build then filled a state the
	// cache no longer counted -- top-level code's 206 KB of closure pools
	// on arm64, counted only later, past the budget -- and a function short
	// of room let go of its state at every attempt and was never compiled.
	life.busy++
	defer func() { life.busy-- }()
	s.budget = r.jitBudget()
	if !s.jitRegRoomAhead(cl, e.life, s.jitRegReplaced(key, osrAt)) {
		// None, all the cache holds young or in use, and it is not
		// compiled -- deferred as one refused room for its code
		// (jitRegPlace) -- rather than compiled and thrown away.
		e.life.deferred, e.life.generation = true, s.generation
		return nil
	}
	p, err := r.jitRegLower(cl, c.rc, jitLowerSSA)
	if l := s.regLowered[key]; l != nil {
		// Its programs kept while the compile uses them: the room made for
		// the code, or for a callee's compiled meanwhile, would let go of
		// the program the code is compiled from, which the code then held
		// uncounted (regLoweringSpare).
		l.busy++
		defer func() { l.busy-- }()
	}
	if err != nil && jitcompile.HostOnly(err) {
		// Host operations alone, no loop, no element's work: compiled for
		// its native callers (LowerRegInline), or, hot, where a call it
		// makes is native (LowerRegHot); refused for now otherwise.
		switch {
		case callee:
			p, err = r.jitRegLower(cl, c.rc, jitLowerInline)
			e.callee = true
		case r.jitRegCallsNatively(cl, c):
			p, err = r.jitRegLower(cl, c.rc, jitLowerHot)
		default:
			e.hostOnly = true
		}
	}
	if err != nil {
		e.refusal = err.Error()
		return e
	}
	e.p = p
	s.regSerial++
	e.serial = s.regSerial
	if jitCompileNote != nil {
		jitCompileNote(r, cl)
	}
	r.jitRegSeenFeedback(cl, c)
	rf := r.jitRegFeedbackFor(cl, p)
	// The links its feedback asks for, which no code holds until it is
	// placed, are let go of by none meanwhile (regUnlink).
	defer s.regBuilding(&rf)()
	if background {
		// Asked ahead here, built on a worker (jit_bg.go).
		snap := jitRegSnapshotOf(rf, p)
		if !snap.s.bad {
			j := &jitRegJob{cl: cl, c: c, key: key, realm: r.Realm, callee: callee, e: e, p: p, rf: rf, snap: snap, deopts: e.life.deopts, frameAt: frameAt,
				w: jit.GetWorkspace(), enc: jitEncoding, opts: mir.Settings(), miss: r.jitStress.miss}
			if osrAt != 0 {
				// The code kept beside the cache's for a loop
				// (jitRegLoopCode), installed beside that code if it is
				// still the function's then.
				j.osrAt, j.osrBeside = osrAt, s.regCache[key]
			}
			r.jitRegSubmit(j)
			return nil
		}
		// What the questions took back cannot be put back: built here,
		// from the feedback as it is now, its lists as the asking left
		// them.
	}
	w := jit.GetWorkspace()
	defer func() {
		if w != nil {
			jit.PutWorkspace(w)
		}
	}()
	s.compileBegin()
	defer s.compileEnd()
	if t := mir.Timing; t != nil {
		t.Start()
	}
	f, err := ssa.BuildIn(&w.SSA, p, rf)
	if err != nil {
		e.refusal = fmt.Sprintf("%v: %v", jitRefused, err)
		return e
	}
	f.FrameAt = frameAt
	looped := jitRegShape(f, p, cl, c)
	jitRegLap(mir.PhaseBuild)
	ssa.Optimize(f)
	jitRegLap(mir.PhaseOptimize)
	if jitSSAOptimized != nil {
		jitSSAOptimized(cl.fn, f)
	}
	mc, err := mir.CompileWith(&w.MIR, f, jitEncoding, mir.Settings())
	keepLimit := rf.fb.keepLimit
	if errors.Is(err, mir.ErrKeeps) {
		// More keep cells live at once than the context has, which the
		// build could not know: built again with no more than that, the
		// calls past them left to Go.
		keepLimit = abi.MaxKeeps
		rf, f, looped, mc, err = r.jitRegBuild(w, cl, c, p, keepLimit, frameAt)
	}
	if err != nil {
		e.refusal = fmt.Sprintf("%v: %v", jitRefused, err)
		return e
	}
	if !r.jitRegPlace(e, c, rf, f, mc, looped, s.jitRegReplaced(key, osrAt)) {
		return nil
	}
	if JITCompileRepeat > 0 {
		// In the workspace the compile used, given back: in another, of its
		// own, they had measured a workspace whose recent compiles were the
		// repeats alone.
		jit.PutWorkspace(w)
		w = nil
		r.jitRegCompileRepeat(cl, c, p, keepLimit, frameAt)
	}
	return e
}

// jitRegLap ends phase ph of a register compile the host times
// (mir.Timing).
func jitRegLap(ph mir.Phase) {
	if t := mir.Timing; t != nil {
		t.Lap(ph)
	}
}

// jitRegCompileRepeat is JITCompileRepeat's compiles of c's register code
// lowered as p: p built again that many times with fresh feedback, as
// jitRegBuild builds it with keepLimit, optimized and compiled -- for arm64
// where JITCrossHook is set -- all each made thrown away, and what they
// allocated counted in mir.Timing.
func (r *Runtime) jitRegCompileRepeat(cl *closure, c *regCode, p *ir.Program, keepLimit, frameAt int) {
	c0 := jitGCCycles()
	bytes0, objects0 := jitAllocated()
	for range JITCompileRepeat {
		w := jit.GetWorkspace()
		if t := mir.Timing; t != nil {
			t.Start()
		}
		rf := r.jitRegFeedbackFor(cl, p)
		rf.fb.keepLimit = keepLimit
		if f, err := ssa.BuildIn(&w.SSA, p, rf); err == nil {
			f.FrameAt = frameAt
			jitRegShape(f, p, cl, c)
			jitRegLap(mir.PhaseBuild)
			ssa.Optimize(f)
			jitRegLap(mir.PhaseOptimize)
			if JITCrossHook != nil {
				code, err := mir.CompileARM64(f, jitEncoding)
				JITCrossHook(jitFunctionOf(c.fn), code, err)
			} else {
				mir.CompileWith(&w.MIR, f, jitEncoding, mir.Settings())
			}
		}
		jit.PutWorkspace(w)
	}
	jitRepeatCycles.Add(jitGCCycles() - c0)
	if t := mir.Timing; t != nil {
		bytes1, objects1 := jitAllocated()
		t.Bytes += int64(bytes1 - bytes0)
		t.Objects += int64(objects1 - objects0)
	}
}

// jitRegFrameAt is the register pc of the loop's header a frame of cl's
// function waits at for this compile (ssa.Func's FrameAt), or 0: there is
// none, or the compile is of another function.
func (r *Runtime) jitRegFrameAt(cl *closure) int {
	if r.jitFrameFn != cl.fn {
		return 0
	}
	return r.jitFramePC
}

// regLoopAround is the register pc of the header of the innermost loop of
// c's around register pc rpc, 0 for none: the target of the first backward
// jump from rpc on that goes to rpc or before it.
func regLoopAround(c *regCode, rpc int) int {
	bl := c.rc.BackLive
	for i := 0; i+1 < len(bl); i += 2 {
		q := int(bl[i])
		if q < rpc {
			continue
		}
		if t := q + 1 + int(regJumpOff(regAt(c.code, q))); t <= rpc {
			return t
		}
	}
	return 0
}

// jitRegRoomAhead makes the room a compile of cl's function, life its
// state, asks for before it lowers anything, and reports whether there is
// that room: a first compile, room for as much as its code is likely to
// hold (perInstr); a compile again in place of old, its function's code,
// only the room its last compile was short of (reoptRoom), old's own
// counted as given back. Asked of the cache's mean, the room asked for code
// far larger than the mean's -- 1,250 bytes an instruction against 500 --
// was short of what it then took.
func (s *jitState) jitRegRoomAhead(cl *closure, life *jitEntry, old *jitRegEntry) bool {
	if !life.regCounted {
		// Its state, kept with no room for it (regLearnedOf), counted from
		// now on, in the room made for it first.
		if !s.makeRoom(jitEntryFixed + life.learnedBytes()) {
			return false
		}
		life.regCounted = true
		s.regAccount(life)
	}
	if old == nil {
		n := 0
		if c := cl.reg; c != nil {
			n = len(c.rc.Code)
		}
		return s.makeRoom(n * s.perInstr)
	}
	if life.reoptRoom == 0 {
		return true
	}
	return s.regRoomFor(old, s.regReplaced(old)+life.reoptRoom)
}

// jitRegReplaced is the code a compile of the function whose key is key
// replaces once placed: the cache's code for it, valid or not, which goes
// with the code kept beside it for a loop (dropReg); or, for a compile of
// code for a loop, frameAt not 0, the code kept beside the cache's
// (jitRegLoopCode). nil where there is none.
func (s *jitState) jitRegReplaced(key jitKey, frameAt int) *jitRegEntry {
	old := s.regCache[key]
	if old == nil || frameAt != 0 && old.osr == nil {
		return nil
	}
	if frameAt != 0 {
		old = old.osr
	}
	if old.code == nil {
		return nil
	}
	return old
}

// regReplaced is what the JIT holds for old, code a compile replaces: its
// bytes and those of the code kept beside it, which go with it (dropReg).
func (s *jitState) regReplaced(old *jitRegEntry) int {
	n := old.bytes
	if o := old.osr; o != nil {
		n += o.bytes
	}
	return n
}

// regRoomFor makes room for a compile's code, need bytes, in place of old,
// the code it replaces once placed, nil for none, and reports whether there
// is the room: what old holds counts as given back, and old is not let go
// of meanwhile (busy). Asked of the budget with old's room counted twice, a
// function compiled again with code as large as half the budget -- an
// asm.js program's loop of ten thousand instructions -- had its finished
// compile thrown away.
func (s *jitState) regRoomFor(old *jitRegEntry, need int) bool {
	if old == nil {
		return s.makeRoom(need)
	}
	old.busy++
	ok := s.makeRoom(need - s.regReplaced(old))
	old.busy--
	return ok
}

// jitRegPlace places mc, e's code, the backend's of f, built from c's
// register code lowered with its feedback rf, in the JIT's arena, and
// fills e from what the build found; it reports false where the JIT has
// no room for it now, or the host places no code. old, where not nil, is
// the code e replaces once placed (jitRegReplaced).
func (r *Runtime) jitRegPlace(e *jitRegEntry, c *regCode, rf *jitRegFeedback, f *ssa.Func, mc *mir.Code, looped map[uint32]bool, old *jitRegEntry) bool {
	s := r.jit
	p := e.p
	// The contexts' room for its spill slots, which they keep (spillsFor).
	need, spill := jit.CodeFootprint(mc), s.spillRoom(mc)
	// And what the compile had its function's state hold besides -- the
	// pools of the literals, closures and constructions it makes, filled
	// as it was built -- which regAccount counts once it is placed. Asked
	// for the code alone, top-level code declaring forty functions, its
	// closures' pools 206 KB, was placed with 106 KB of code at a budget of
	// 256 KB, which the two then held past while it ran, neither one the
	// cache's to reclaim.
	grown := 0
	if l := e.life; l != nil && l.regCounted {
		grown = max(jitEntryFixed+l.learnedBytes()-l.bytes, 0)
	}
	s.budget = r.jitBudget()
	if !s.regRoomFor(old, need+spill+grown) {
		// No room, all the cache holds young or in use: not compiled again
		// for want of it until the cache has let some code go (generation);
		// and, the function's code compiled again, not until there is the
		// room it was short of (reoptRoom).
		e.life.deferred, e.life.generation = true, s.generation
		if old != nil {
			e.life.reoptRoom = max(need+spill+grown-s.regReplaced(old), 1)
		}
		// Nor its programs, which the compile asks for again: they would
		// take the room it is remembered in.
		if l := s.regLowered[e.key]; l != nil {
			s.dropRegLowering(e.key, l)
		}
		return false
	}
	e.life.deferred, e.life.reoptRoom = false, 0
	code, err := jit.NewSSACode(s.arena, mc)
	if err != nil {
		// The host places no code (jit.ErrUnavailable), which is the
		// only failure: code of any length is placed.
		s.unavailable = true
		return false
	}
	s.spillsFor(mc)
	s.inlined += uint64(len(f.Inlined))
	s.inlinedCode += uint64(f.InlinedCode)
	e.code, e.bytes = code, need
	// The pointer words its native calls write to their contexts' records,
	// which a run's end clears (jitRegStretch): uncounted, a run left them,
	// and whatever the callees had had in their registers stayed reachable.
	s.callRefs = max(s.callRefs, uint64(code.CallRefs()))
	// What the entry and its program hold, by the function's instructions:
	// the room the next compile asks for first.
	s.learn(len(c.rc.Code), need+p.Footprint())
	e.targets = jitTargetsOf(e.life)
	e.scratch = f.Scratch
	e.keeps = f.Keeps
	s.ssaKeeps = max(s.ssaKeeps, f.Keeps)
	if f.HomesEnd != 0 {
		e.frame = f.HomesEnd - p.Locals
	}
	if f.Loops {
		e.frame = max(e.frame, f.LoopsEnd-p.Locals)
	}
	for rpc := range looped {
		if code.HasEntry(p.At(rpc)) {
			e.loops = append(e.loops, rpc)
		}
	}
	slices.Sort(e.loops)
	e.findReturns(c.rc)
	e.rc = c.rc
	e.at = make([]int32, len(c.rc.Code))
	for rpc := range e.at {
		e.at[rpc] = int32(p.At(uint32(rpc)))
	}
	e.loopMean = e.irLoopMean(c)
	e.callsNative = len(mc.Calls) != 0
	// Register pcs both: a header's PC is its register pc (ssa.Header's PC,
	// which LowerReg's maps give).
	e.unresumed = slices.Clone(f.Unresumed)
	if life := e.life; life != nil {
		// The register pcs of the calls it leaves to Go (ssa.Func's
		// HostsLeft), one of which its code has never made is seldom made
		// (InlineExit).
		life.ssaHostsLeft = life.ssaHostsLeft[:0]
		for _, pc := range f.HostsLeft {
			if pc >= 0 && pc < len(p.Origin) {
				life.ssaHostsLeft = append(life.ssaHostsLeft, int32(p.Origin[pc]))
			}
		}
	}
	e.unentered = slices.Clone(f.Unentered)
	e.kept = slices.Clone(f.EntryKept)
	e.kinds = f.Kinds
	// The pools' roots and prototypes escape analysis took, which the
	// objects it does not make are made with at an exit.
	rf.fb.keepVirtuals(f.VirtualAsked)
	// The closures it makes that it only calls (ssa.Func's Called), and its
	// loops' callbacks, those it does not make among them.
	rf.fb.called = f.Called
	rf.fb.callbacks, rf.fb.unmade = f.Callbacks, f.Unmade
	// What the code reads by address, its feedback's keep; the closure
	// and the function the feedback asked are not.
	e.keep = rf.fb.keep()
	e.keep.cl, e.keep.fn, e.keep.prog, e.keep.regSeen = nil, nil, nil, nil
	// Its code calls through its links from now on, until it is closed
	// (regClose).
	s.regHoldLinks(e.keep, 1)
	if JITCodeHook != nil {
		// -jitcode's record of the code.
		JITCodeHook(jitFunctionOf(c.fn), code.Text(), mc)
	}
	return true
}

// Register code in the JIT's cache is held, aged, reclaimed and flushed by
// the cache's looks (makeRoom, flush): its bytes count in what the JIT
// holds (held, regHeld), against the one budget; makeRoom reclaims refusals
// first, then code unused this epoch and the one before, the least recently
// used first (regVictims); and flush drops code unused between two of its
// looks (regFlush). What the code learned is the function's (regLearned),
// and outlives it: compiled again once hot again, it is compiled with all
// of it. Before, register code had half the budget, counted by a scan of
// the cache at every compile, and was never let go of but with its
// function.

// regUses is how often e's code was used: Go's entries at its function's
// start and into its loops, the runs Go went on in it after it left, and
// native callers' calls through its link (jitEntry's uses).
func (s *jitState) regUses(e *jitRegEntry) uint64 {
	l := e.life
	if l == nil {
		return 0
	}
	n := l.starts + l.ssaStats.entries + l.goOns
	if k := s.regLinkFor(e); k != nil {
		n += k.in
	}
	return n
}

// regInUse reports whether e's code runs, or may go on, below Go now: a run
// of it, a native call of it, or a compile of its function in the
// background, which replaces it.
//
// A native call of it under way is one Go has the level of (regLevelsOf),
// or one whose context says it runs e's function (regCalled), as the stack
// form's inUse finds its entries' (called): code a native callee's level
// runs stays while the call does, Go having found its level or not -- the
// flush, at a back-edge budget Go gave a native callee's loop, had let
// such code go and closed it under the call, whose level Go then found no
// code for (jitRegLevelAt). Before, every function native callers call
// counted as in use while any native code ran, and the flush, which runs
// under native code, and the cache's room, which compiles asked for from
// calls of Go made, let none of them go: Octane's Mandreel kept its
// functions' code, and the budget, full for the benchmarks after it,
// refused zlib's and Typescript's compiles millions of times.
func (s *jitState) regInUse(e *jitRegEntry) bool {
	return s.regRunning(e) || s.regCalled(e)
}

// regRunning reports whether e's code runs below Go now as Go knows it: a
// run of it, a level of a native call of it Go found (regLevelsOf), or a
// compile of its function in the background -- regInUse but for the
// native calls only their contexts say are under way (regCalled).
func (s *jitState) regRunning(e *jitRegEntry) bool {
	return e.runs != 0 || e.busy != 0 || s.regPendingJobs[e.key] != nil || s.regLevelsOf(e) != 0
}

// regCalledFns is the functions native calls under way run (regCalled),
// found once for a look at all the cache holds; nil while no run's native
// code is.
func (s *jitState) regCalledFns() map[*bytecode.Function]bool {
	if s.ctxTop == 0 {
		return nil
	}
	var fns map[*bytecode.Function]bool
	for _, c := range s.ssaCtxs {
		if c.Live == abi.LiveCall && c.Closure != nil {
			if fns == nil {
				fns = map[*bytecode.Function]bool{}
			}
			fns[(*closure)(c.Closure).fn] = true
		}
	}
	return fns
}

// regInUseOf is regInUse for a look at all the cache holds, the functions
// native calls under way run found once (regCalledFns).
func (s *jitState) regInUseOf(e *jitRegEntry, called map[*bytecode.Function]bool) bool {
	return s.regRunning(e) || called != nil && called[e.key.Value()]
}

// regAge notes, at epoch now, which register code was used since the last
// look (jitRegEntry's used), and which functions' kept state
// (regLearned) was, and counts again what each function's state holds
// (regAccount); and lets go of what belonged to functions that are gone,
// and of the links no code calls through of functions the cache holds
// nothing for (regPruneLinks).
func (s *jitState) regAge(now uint64) {
	for key, e := range s.regCache {
		if key.gone() {
			if !s.regInUse(e) {
				s.dropReg(key, e)
			}
			continue
		}
		if u := s.regUses(e); u != e.seen {
			e.seen, e.used = u, now
		}
	}
	for key, l := range s.regLearned {
		if key.gone() {
			s.regDropLife(key, l)
			continue
		}
		if u := regLifeUses(l); u != l.seen {
			l.seen, l.used = u, now
		}
		if !l.regCounted {
			// One kept with no room for it (regLearnedOf) counted from now
			// on, the room made among it (makeRoom): what may be reclaimed
			// has grown, past the floor noted before.
			l.regCounted = true
			s.floorAt = 0
		}
		s.regAccount(l)
	}
	for key, l := range s.regLowered {
		if key.gone() {
			s.dropRegLowering(key, l)
		}
	}
	s.regPruneLinks()
}

// regAccount counts what l, a function's state (regLearned), holds now
// in what the JIT holds (regHeld), if the JIT keeps it (regCounted): its
// entry and what it learned, the object pools' objects among it
// (learnedBytes). Not its code, which its function's entry in regCache
// counts.
func (s *jitState) regAccount(l *jitEntry) {
	if l == nil || !l.regCounted {
		return
	}
	n := jitEntryFixed + l.learnedBytes()
	if n < l.bytes {
		s.floorAt = 0
	}
	s.regHeld += n - l.bytes
	l.bytes = n
}

// regDropLife lets go of l, the state the JIT kept for the function whose
// key is key (regLearned): what it learned, the objects its pools and its
// calls' targets hold among it. Learning again, the function makes a new
// one.
func (s *jitState) regDropLife(key jitKey, l *jitEntry) {
	if s.regLearned[key] == l {
		delete(s.regLearned, key)
	}
	if l.regCounted {
		s.regHeld -= l.bytes
		l.bytes, l.regCounted = 0, false
		s.floorAt = 0
	}
}

// regLifeUses is how often the function whose state l is ran: its runs
// while it had no code (invoked, which its samples bring up to date,
// jitRegArm), and its code's uses (regUses).
func regLifeUses(l *jitEntry) uint64 {
	n := uint64(l.invoked) + l.starts + l.ssaStats.entries + l.goOns + l.slowCalls
	if k := l.regLink; k != nil {
		n += k.in
	}
	return n
}

// regLifeSpare reports whether l, the state of the function whose key is
// key, is one the cache may let go of whole (regVictims, regFlush): its
// function has no code in the cache -- code's state goes with it
// (regLetGo) -- none is being compiled for it, and no code let go of
// still runs it (regRetired).
func (s *jitState) regLifeSpare(key jitKey, l *jitEntry, called map[*bytecode.Function]bool) bool {
	if e := s.regCache[key]; e != nil && (e.code != nil || s.regInUseOf(e, called)) {
		return false
	}
	if l.busy != 0 || s.regPendingJobs[key] != nil {
		return false
	}
	for _, e := range s.regRetired {
		if e.key == key || e.life == l {
			return false
		}
	}
	return true
}

// regLetGoLife lets go of the state of the function whose key is key,
// l, with what the cache holds for it -- an entry with no code, a
// refusal -- and its programs (regLifeSpare).
func (s *jitState) regLetGoLife(key jitKey, l *jitEntry) {
	if e := s.regCache[key]; e != nil {
		s.dropReg(key, e)
	}
	s.regDropLife(key, l)
	if lw := s.regLowered[key]; lw != nil {
		s.regTrimLowering(key, lw)
	}
}

// regPruneLinks lets go of the links (regLinks) of functions gone, and of
// those no code calls through of functions the cache holds nothing for
// (regUnlink).
func (s *jitState) regPruneLinks() {
	for key, l := range s.regLinks {
		if key.gone() {
			s.regDropLink(l)
			continue
		}
		s.regUnlink(l)
	}
}

// regUnlink lets go of l, a register link, once nothing calls through it:
// its function has nothing in the cache (regCache), no code holds it
// (refs, regHoldLinks), and no compile under way may -- one on the
// runtime's goroutine (regBuilds), or in the background, whose
// feedback has asked for it. A link is made at a call Go
// makes for a native caller, for a generic call to find the function by
// (regHint), and by every compile that calls a function natively: kept
// for good, a program streaming functions through the cache kept one for
// each it had ever called.
func (s *jitState) regUnlink(l *jitLink) {
	if l.refs != 0 || s.regLinks[l.key] != l || s.regCache[l.key] != nil {
		return
	}
	for _, rf := range s.regBuilds {
		if *rf != nil && slices.Contains((*rf).fb.keep().links, l) {
			return
		}
	}
	for _, j := range s.regPendingJobs {
		if j.holdsLink(l) {
			return
		}
	}
	s.regDropLink(l)
}

// holdsLink reports whether j, a compile in the background, may call
// through l once installed: its feedback asked for l, which its keep holds
// or its snapshot's questions took back, to be put back at the install
// (jitSnapshot's effects).
func (j *jitRegJob) holdsLink(l *jitLink) bool {
	if j.rf != nil && slices.Contains(j.rf.fb.keep().links, l) {
		return true
	}
	if j.snap == nil {
		return false
	}
	root := j.snap.s
	for _, s := range append([]*jitSnapshot{root}, mapValues(root.callees)...) {
		for _, x := range s.effects {
			if slices.Contains(x.own.links, l) || slices.Contains(x.kept.links, l) {
				return true
			}
		}
	}
	return false
}

// regBuilding notes *rf, the feedback of a register compile under way,
// whose links are let go of by none (regUnlink) until the function it
// returns is called, as the compile ends.
func (s *jitState) regBuilding(rf **jitRegFeedback) func() {
	s.regBuilds = append(s.regBuilds, rf)
	return func() {
		if i := slices.Index(s.regBuilds, rf); i >= 0 {
			s.regBuilds = slices.Delete(s.regBuilds, i, i+1)
		}
	}
}

// regDropLink takes l out of regLinks: native callers no longer call
// code through it, and its slot of the hint table is freed (regUnhint),
// where a call finds no link, and Go makes it.
func (s *jitState) regDropLink(l *jitLink) {
	l.clearCode()
	s.regUnhint(l)
	if s.regLinks[l.key] == l {
		delete(s.regLinks, l.key)
	}
	if life := s.regLearned[l.key]; life != nil && life.regLink == l {
		life.regLink = nil
	}
}

// regHoldLinks counts code whose keep is fb calling through each of its
// links once (jitLink's refs), by d: 1 as the code is placed, -1 as it is
// closed, when a link no code calls through any longer is let go of
// (regUnlink).
func (s *jitState) regHoldLinks(fb *jitFeedback, d int) {
	if fb == nil {
		return
	}
	links := fb.links
	for i, l := range links {
		if slices.Contains(links[:i], l) {
			continue
		}
		l.refs += d
		if d < 0 && l.refs <= 0 {
			l.refs = 0
			s.regUnlink(l)
		}
	}
}

// regVictims appends to v what of the register code makeRoom may reclaim at
// epoch now (listVictims): the programs lowered for functions
// (regLoweringSpare), refusals, and code not used this epoch or the one
// before (jitCodeAge), none in use, each with the program its code was
// compiled from, which goes with it (regReclaim).
//
// A program kept is the cheapest to reclaim, whatever its function's
// entry: the next compile that asks for it lowers it again, at a few
// percent of a compile's cost. Before, a function's programs went only
// with its entry, and the inlining kind -- a callee's, which its callers'
// compiles asked for -- stayed with a callee whose code was young or in
// use: of the 38.5 MB Octane's programs held at its end, 71% were callees'
// kept for inlining, and the budget, full, refused compiles for them.
func (s *jitState) regVictims(v []jitVictim, now uint64) []jitVictim {
	for key, l := range s.regLowered {
		if n := s.regLoweringSpare(key, l); n > 0 {
			v = append(v, jitVictim{key: key, l: l, reg: true, class: jitVictimLowering, used: l.used, serial: s.regVictimSerial(key, 0), bytes: n})
		}
	}
	called := s.regCalledFns()
	for key, e := range s.regCache {
		if s.regInUseOf(e, called) {
			continue
		}
		class := uint8(jitVictimRefusal)
		if e.code != nil {
			if e.used+jitCodeAge > now && !s.settling {
				continue
			}
			class = jitVictimCode
		}
		n := e.bytes
		if l := s.regLowered[key]; l != nil {
			n += l.bytes - s.regLoweringSpare(key, l)
		}
		if l := e.life; l != nil && l.regCounted {
			// What it learned that holds objects goes with it (regForget),
			// and a refusal's state with it whole (regLetGoLife).
			if e.code == nil {
				n += l.bytes
			} else {
				n += max(l.bytes-jitEntryFixed, 0) / 2
			}
		}
		serial := uint64(e.serial)
		if class == jitVictimRefusal {
			serial = s.regVictimSerial(key, serial)
		}
		v = append(v, jitVictim{key: key, re: e, reg: true, class: class, used: e.used, serial: serial, bytes: n})
	}
	// The state kept of functions the cache holds nothing for -- warming
	// up, or their code let go of -- reclaimed with the refusals.
	for key, l := range s.regLearned {
		if s.regCache[key] != nil || !l.regCounted || !s.regLifeSpare(key, l, called) {
			continue
		}
		v = append(v, jitVictim{key: key, e: l, reg: true, class: jitVictimRefusal, used: l.used, serial: l.serial, bytes: l.bytes})
	}
	return v
}

// regVictimSerial is the serial makeRoom orders a victim of the function
// whose key is key by, among those of its class and epoch: its state's
// (regLearned), numbered as the states are made, with refusals and
// lowerings, which share their classes with the states and with each
// other; or, with none, or, the cache's own, its own. Ties had been broken
// by the order the cache's maps gave them, which is the process's, and
// which of two programs or two warming states went first had a full
// budget's compiles differ from one run to the next.
func (s *jitState) regVictimSerial(key jitKey, own uint64) uint64 {
	if l := s.regLearned[key]; l != nil && l.serial != 0 {
		return l.serial
	}
	return own
}

// regLoweringHeld is the kind of l's program that the code the cache holds
// for its function, whose key is key, was compiled from (jitRegEntry's p),
// or -1 for none: that program goes on being held, by the code, whatever
// l does, and goes with it (regReclaim).
func (s *jitState) regLoweringHeld(key jitKey, l *jitLowering) int {
	if e := s.regCache[key]; e != nil && e.code != nil && e.p != nil {
		for k, p := range l.progs {
			if p == e.p {
				return k
			}
		}
	}
	return -1
}

// regLoweringSpare is what of l, the register lowering of the function
// whose key is key, makeRoom and the flush may reclaim (regTrimLowering):
// its programs but the one its function's code holds (regLoweringHeld) --
// all of it, with no such program -- or nothing while a compile of the
// function uses them (jitLowering's busy, a compile in the background).
func (s *jitState) regLoweringSpare(key jitKey, l *jitLowering) int {
	if l.busy != 0 || s.regPendingJobs[key] != nil {
		return 0
	}
	held := s.regLoweringHeld(key, l)
	if held < 0 {
		return l.bytes
	}
	return l.bytes - jitLoweringFixed - l.progs[held].Footprint()
}

// regTrimLowering lets go of what of l, the register lowering of the
// function whose key is key, is spare (regLoweringSpare): its programs but
// the one its function's code holds, or, with none, l itself. A compile
// that asks for one lowers it again (jitRegLower); what failed to lower
// stays known, holding nothing.
func (s *jitState) regTrimLowering(key jitKey, l *jitLowering) {
	if l.busy != 0 || s.regPendingJobs[key] != nil || s.regLowered[key] != l {
		return
	}
	held := s.regLoweringHeld(key, l)
	if held < 0 {
		s.dropRegLowering(key, l)
		return
	}
	for k, p := range l.progs {
		if k == held || p == nil {
			continue
		}
		n := p.Footprint()
		l.bytes -= n
		s.lowerBytes -= n
		l.done[k], l.progs[k], l.errs[k] = false, nil, nil
	}
	s.floorAt = 0
}

// regReclaim drops x, register code makeRoom listed, if it still is a
// victim at epoch now (reclaim).
func (s *jitState) regReclaim(x *jitVictim, now uint64) {
	e := x.re
	if e == nil {
		// A function's state alone (regVictims).
		if l := x.e; s.regLearned[x.key] == l && s.regCache[x.key] == nil && s.regLifeSpare(x.key, l, s.regCalledFns()) {
			// Reclaimable when listed, it was not of the floor (listVictims),
			// which stays: reset, a stream of functions each taking the room
			// of another's state had the next compile refused room list all
			// the cache holds again, at every function.
			floorAt := s.floorAt
			s.regLetGoLife(x.key, l)
			s.floorAt = floorAt
			s.evicted++
		}
		return
	}
	if s.regCache[x.key] != e || s.regInUse(e) || (e.code != nil) != (x.class == jitVictimCode) || e.code != nil && e.used+jitCodeAge > now && !s.settling {
		return
	}
	if e.code == nil && e.life != nil && s.regLearned[x.key] == e.life && s.regLifeSpare(x.key, e.life, s.regCalledFns()) {
		// A refusal, with what its function learned (regVictims).
		s.regLetGoLife(x.key, e.life)
	} else {
		s.regLetGo(x.key, e)
	}
	if l := s.regLowered[x.key]; l != nil {
		s.dropRegLowering(x.key, l)
	}
	s.evicted++
}

// regFlush drops, at the flush's tick, the register code unused since the
// flush's last look, not young, not in use (flush): used are the code that
// ran, by its uses, and the callees of code used -- inlined there, or
// called natively -- whose own uses do not count what runs of them there,
// and whose state what the caller compiles again learns from. It lets go of
// the programs no compile asked for since its last look (regTrimLowering),
// as the flush ages code: a callee's program for inlining, which no caller
// is compiled with any longer, had been kept for as long as the callee's
// code, the callee warm or not. And it lets go of all the JIT keeps for the
// functions of a realm none of whose code was used and none of whose
// functions ran since its last look, but their code, which goes as any code
// does: what they learned, their entries with no code, their programs. A
// realm a host dropped is reachable from them, what its functions learned
// holding its objects, and only so let go of.
func (s *jitState) regFlush(tick, now uint64) {
	called := s.regCalledFns()
	cur := s.rt.jitRealmKey()
	live := map[weak.Pointer[Realm]]bool{cur: true}
	used := map[jitKey]bool{}
	var work []jitKey
	for key, e := range s.regCache {
		if key.realm != cur {
			s.manyRealms = true
		}
		if e.code == nil {
			continue
		}
		if u := s.regUses(e); u != e.flushSeen || e.flushAt == 0 {
			e.flushSeen, e.flushAt = u, tick+1
			used[key] = true
			work = append(work, key)
		}
	}
	for key, l := range s.regLearned {
		if key.realm != cur {
			s.manyRealms = true
		}
		if u := regLifeUses(l); u != l.flushSeen {
			l.flushSeen = u
			live[key.realm] = true
		}
	}
	mark := func(key jitKey) {
		if !used[key] {
			used[key] = true
			work = append(work, key)
		}
	}
	markClosure := func(cl *closure) {
		if cl != nil {
			mark(s.regKeyOfClosure(cl))
		}
	}
	markFeedback := func(fb *jitFeedback) {
		if fb == nil {
			return
		}
		for _, cl := range fb.inlined {
			markClosure(cl)
		}
		for _, l := range fb.links {
			mark(l.key)
		}
	}
	for len(work) != 0 {
		key := work[len(work)-1]
		work = work[:len(work)-1]
		live[key.realm] = true
		if e := s.regCache[key]; e != nil {
			markFeedback(e.keep)
			if e.osr != nil {
				markFeedback(e.osr.keep)
			}
		}
		if l := s.regLearned[key]; l != nil {
			for _, calls := range [2][]jitInline{l.inlines, l.nativeCalls} {
				for _, x := range calls {
					markClosure(x.cl)
				}
			}
		}
	}
	for key, e := range s.regCache {
		if e.code == nil || used[key] {
			continue
		}
		if e.used+jitCodeAge > now || s.regInUseOf(e, called) {
			continue
		}
		s.regLetGo(key, e)
		if l := s.regLowered[key]; l != nil {
			s.dropRegLowering(key, l)
		}
		s.flushed++
	}
	// The realms none of whose functions ran: what the JIT keeps of their
	// functions with no code.
	for key, l := range s.regLearned {
		if live[key.realm] || l.used+jitCodeAge > now || !s.regLifeSpare(key, l, called) {
			continue
		}
		s.regLetGoLife(key, l)
	}
	for key, e := range s.regCache {
		if live[key.realm] || e.code != nil || e.life != nil || s.regInUseOf(e, called) {
			continue
		}
		s.dropReg(key, e)
	}
	// Programs asked for since the last look were found used then
	// (jitLowering's used, the cache's age at the ask, regLowering).
	last := s.regLowerLook
	s.regLowerLook = s.agedAt
	if last == 0 {
		return
	}
	for key, l := range s.regLowered {
		if l.used < last {
			s.regTrimLowering(key, l)
		}
	}
}

// regKeyOfClosure is the key of cl's function's code in cl's realm, the
// current one's where it has none: the key its code is cached by.
func (s *jitState) regKeyOfClosure(cl *closure) jitKey {
	if cc := cl.closureCode; cc != nil && cc.jitRegKey.Pointer != (weak.Pointer[bytecode.Function]{}) {
		return cc.jitRegKey
	}
	re := cl.realm
	if re == nil {
		return s.rt.jitKeyOf(cl.fn)
	}
	if re.jitSelf == (weak.Pointer[Realm]{}) {
		re.jitSelf = weak.Make(re)
	}
	return jitKey{weak.Make(cl.fn), re.jitSelf}
}

// regLetGo drops e, the code of the function whose key is key, from the
// cache for room or for going unused: the function warms up anew, and
// compiled again once it is hot is compiled with what it had learned of its
// sites -- what holds no object (regForget).
func (s *jitState) regLetGo(key jitKey, e *jitRegEntry) {
	had := e.code != nil
	s.dropReg(key, e)
	if l := e.life; l != nil {
		l.invalid, l.warming, l.reopt, l.learning = false, false, false, false
		regForget(l)
		l.regFlushed = l.regFlushed || had
		s.regAccount(l)
	}
}

// regForget has l, what a function's register code learned (regLearned),
// keep only what holds no object, once the cache let its code go: the sites
// whose speculation failed, the reads and the arithmetic it met others at,
// where Go went on, and its lowerings' hints. The rest -- the callees its
// calls met, the closures it inlined, the pools its constructions and
// literals took their objects from, the generators its loops met -- holds
// the objects its code was compiled for, and the pools every object they
// handed out, the program's: kept with the function, whose bytecode
// outlives them, they had kept a benchmark's whole heap alive after it
// ended, which every later collection marked again -- AWFY DeltaBlue's 35
// MB through Havlak, whose collections took half as long again. The
// function relearns them once it runs again. Its link stays (regLink):
// native callers call the function through it whatever code it has,
// regLinks keeps it by the function's key, and it holds no object of the
// program.
func regForget(l *jitEntry) {
	o := l.jitLearned
	if o == nil {
		return
	}
	l.jitLearned = &jitLearned{reg: o.reg, regLink: o.regLink,
		failed: o.failed, failedEntries: o.failedEntries, bigints: o.bigints, bigwide: o.bigwide, resumed: o.resumed,
		typedReads: o.typedReads, stringReads: o.stringReads, keyedReads: o.keyedReads, keyedWrites: o.keyedWrites,
		regHints: o.regHints}
}

// dropReg closes e's code, or, while a run or a native call is in it, has
// the last of them close it (dropped), and takes it out of the cache.
func (s *jitState) dropReg(key jitKey, e *jitRegEntry) {
	if s.regCache[key] == e {
		delete(s.regCache, key)
		s.regLinkUpdate(key, nil)
		if l := s.regLinks[key]; l != nil {
			// Its function's link, if no code calls through it.
			defer s.regUnlink(l)
		}
		s.regHeld -= e.bytes
		if o := e.osr; o != nil {
			// Its code for on-stack replacement goes with it.
			e.osr = nil
			s.regHeld -= o.bytes
			o.dropped = true
			s.regClose(o)
		}
		if e.code != nil {
			// Room made: compiles deferred for want of it are asked again
			// (jitRegPlace).
			s.generation++
		}
		// What the floor noted as held for good has gone, and its
		// function's state, which its code had kept, is the cache's to
		// reclaim now (regLifeSpare): looked for again. Kept, the floor had
		// a full budget refuse room with a hundred kilobytes of such states
		// reclaimable, the JIT holding past its budget until the epoch's
		// end.
		s.floorAt = 0
	}
	e.dropped, e.cached = true, false
	s.regClose(e)
}

// regClose closes the code of e, which the cache let go of (dropped), once
// no run and no native call is in it.
func (s *jitState) regClose(e *jitRegEntry) {
	if !e.dropped || e.code == nil {
		return
	}
	if s.regInUse(e) || len(s.goPinned) != 0 && slices.Contains(s.goPinned, e.code) {
		// In use: kept where a level running it finds it (jitRegLevelAt)
		// -- a native call of it under way, which a call of Go from its
		// level returned to (jitRegPinLevel), say -- and closed once the
		// last use is done (jitRegCloseRetired).
		if !slices.Contains(s.regRetired, e) {
			s.regRetired = append(s.regRetired, e)
		}
		return
	}
	_ = s.closeSSA(e.code, e.keep)
	s.regHoldLinks(e.keep, -1)
	// What the code read by address, and the calls' targets it was
	// compiled for, go with it: the entry stays where its function's
	// template names it until a call finds it let go of (jitRegLetGoOf),
	// and a function not called again had kept them -- holders, pools,
	// closures -- for good.
	e.code, e.keep, e.targets = nil, nil, nil
	if len(s.regRetired) != 0 {
		if i := slices.Index(s.regRetired, e); i >= 0 {
			s.regRetired = slices.Delete(s.regRetired, i, i+1)
		}
	}
}

// jitRegPinLevel pins e's code, a native callee's that a call of Go from
// its level is made in, to the run in context root (goPinned), until the
// run's native code leaves (jitGoUnpin): once the call is done, its level
// goes on in that code, natively, and a drop of its entry meanwhile -- the
// cache's, for room, or for code compiled again -- had closed the code
// there (regClose) and left the level with no code to be unwound by.
func (r *Runtime) jitRegPinLevel(root int, e *jitRegEntry) {
	s := r.jit
	if root >= len(s.goRuns) || e.code == nil {
		return
	}
	run := &s.goRuns[root]
	if !run.pinned {
		run.pinned, run.pinnedAt = true, len(s.goPinned)
	}
	if !slices.Contains(s.goPinned[run.pinnedAt:], e.code) {
		s.goPinned = append(s.goPinned, e.code)
	}
}

// jitRegCloseRetired closes the entries dropped while their code was
// pinned (regRetired) that no run pins any longer.
func (s *jitState) jitRegCloseRetired() {
	for _, e := range slices.Clone(s.regRetired) {
		s.regClose(e)
	}
}

// dropRegCode drops every entry of the register code's cache, as the
// runtime releases its JIT.
func (s *jitState) dropRegCode() {
	for key, e := range s.regCache {
		s.dropReg(key, e)
	}
}

// jitRegSlot is the value slot i of p names in f's register frame, its
// window at wb: a register, a captured binding in its cell, or a slot past
// the window, where mir puts them -- a scratch slot first, then what is
// past the program's Locals, an object's home, a loop's slot.
func (r *Runtime) jitRegSlot(f *frame, p *ir.Program, wb, i int) *Value {
	scratch := p.Locals - jitRegScratchBase(p)
	switch {
	case i < p.Regs:
		return &r.stack[wb+i]
	case i >= p.UpvalueBase && i < p.UpvalueBase+p.Upvalues:
		return f.cl.upvalues[i-p.UpvalueBase].slot
	case i >= p.Locals:
		return &r.stack[wb+p.Regs+scratch+i-p.Locals]
	}
	return &r.stack[wb+p.Regs+i-(p.Locals-scratch)]
}

// jitRegApplyRecords writes the slots ctx's records name into f's register
// frame, every value read before any is written: an exit's own records
// native code applies itself as it leaves, so these are a native call's
// caller's state. kinds are the code's slots' kinds (jitRegEntry's): a
// typed slot's word is written raw, the inlined typed callees' past the
// program's slots among them.
func (r *Runtime) jitRegApplyRecords(f *frame, p *ir.Program, kinds []uint8, wb int, ctx *abi.Context) {
	n := int(ctx.Records)
	if n == 0 {
		return
	}
	r.jit.ssaRecords += uint64(n)
	const flags = abi.RecordScalar | abi.RecordMaybe | abi.RecordDirect
	var buf [8]Value
	src := buf[:0]
	if n > len(buf) {
		src = make([]Value, 0, n)
	}
	src = src[:n]
	for i := range n {
		switch rec := &ctx.Record[i]; {
		case rec.Slot&abi.RecordDirect != 0:
			slot := abi.Slot{Num: rec.Word, Ref: ctx.RecordRef[i]}
			src[i] = *(*Value)(unsafe.Pointer(&slot))
		case rec.Slot&abi.RecordScalar != 0:
			src[i] = Value{num: math.Float64frombits(rec.Word)}
		case rec.Slot&abi.RecordMaybe != 0:
			src[i] = Value{num: math.Float64frombits(rec.Word)}
			if s := jitSource(&rec.Arg); s != nil && s.ref != nil {
				src[i] = *s
			}
		case p.This && int(rec.Arg) == p.UpvalueBase+p.Upvalues:
			// A derived constructor's this binding, the context's This,
			// which a record reads only.
			src[i] = *(*Value)(unsafe.Pointer(&ctx.This))
		default:
			src[i] = *r.jitRegSlot(f, p, wb, int(rec.Arg))
		}
	}
	for i, rec := range ctx.Record[:n] {
		slot := int(rec.Slot &^ flags)
		if slot < len(kinds) && kinds[slot] != abi.SlotValue {
			// Typed code's raw slot, written raw (abi.RawWord).
			src[i] = Value{num: math.Float64frombits(abi.RawWord(kinds[slot], math.Float64bits(src[i].num)))}
		}
		*r.jitRegSlot(f, p, wb, slot) = src[i]
	}
	ctx.Records = 0
}

// jitRegMaterialize makes, in f's register frame, the objects e's code did
// not make (ssa's escape.go): a register that holds the word of one gets
// the object, made of its home past the window, one for each home, the
// homes cleared after.
func (r *Runtime) jitRegMaterialize(f *frame, e *jitRegEntry, wb int) {
	r.jitRegMaterializeSpans(f, e, wb, nil)
}

// jitRegMaterializeSpans is jitRegMaterialize, the windows of the callees
// e's code inlined that an exit inside them wrote too -- spans, each its
// first slot and its end in the VM's stack -- whose registers name the
// same objects as the frame's: one made for each home, which every slot
// naming it, the frame's and the callees', is given.
func (r *Runtime) jitRegMaterializeSpans(f *frame, e *jitRegEntry, wb int, spans [][2]int) {
	if k := e.keep; k != nil && len(k.made) != 0 && f != nil {
		r.jitRegMadeIn(f, e, wb, spans)
	}
	if e.frame == 0 {
		return
	}
	p := e.p
	end := min(len(r.stack), wb+p.Regs+e.scratch+e.frame)
	named := namesVirtual(r.stack[wb:end])
	for _, sp := range spans {
		named = named || sp[0] < sp[1] && sp[1] <= len(r.stack) && namesVirtual(r.stack[sp[0]:sp[1]])
	}
	if !named {
		return
	}
	type made struct {
		home, fields int
		o            *Object
	}
	var objs []made
	at := func(slot int) *Value { return r.jitRegSlot(f, p, wb, slot) }
	var get func(w uint64) *Object
	fix := func(v *Value) {
		if isVirtualWord(v) {
			*v = Obj(get(math.Float64bits(v.num)))
		}
	}
	get = func(w uint64) *Object {
		home, fields, _ := abi.IsVirtualWord(w)
		for _, m := range objs {
			if m.home == home {
				return m.o
			}
		}
		sw := math.Float64bits(at(home).num)
		o := newShapedLiteral(*(**shape)(unsafe.Pointer(&sw)), ClassObject, fields)
		objs = append(objs, made{home, fields, o})
		for k := range fields {
			v := *at(home + abi.VirtualFields + k)
			fix(&v)
			o.appendVal(v)
		}
		return o
	}
	for i := wb; i < end; i++ {
		fix(&r.stack[i])
	}
	for _, sp := range spans {
		for i := sp[0]; i < min(sp[1], len(r.stack)); i++ {
			fix(&r.stack[i])
		}
	}
	for _, m := range objs {
		for k := range abi.VirtualFields + m.fields {
			*at(m.home + k) = Undefined
		}
	}
	r.jit.materialized += uint64(len(objs))
}

// jitRegMadeIn makes, in the registers of f's window at wb, its scratch
// slots and frame, and the windows of the callees e's code inlined that an
// exit inside them wrote (spans), the closures e's code did not make (ssa's
// calledClosures): one for each template, on f, as closure_w makes it
// (regClosure) -- its captured registers' cells open on the frame by now
// (jitRegReopenCells) -- and every slot holding the template given it.
func (r *Runtime) jitRegMadeIn(f *frame, e *jitRegEntry, wb int, spans [][2]int) {
	made := e.keep.made
	c := f.cl.reg
	objs := make([]*Object, len(made))
	fix := func(v *Value) {
		t := v.template()
		if t == nil {
			return
		}
		i := slices.Index(made, t)
		if i < 0 {
			return
		}
		if objs[i] == nil {
			// The frame's own template of the function: code of a function
			// is the code of each of its closures, and e's names the
			// templates of the closure it was compiled for, each closure's
			// its own (regExtend).
			k := slices.IndexFunc(f.cl.consts, func(x Value) bool { u := x.template(); return u != nil && u.fn == t.fn })
			if c == nil || k < 0 || k >= len(c.rc.Caps) {
				panic("vm: an unmade closure's template is not its maker's")
			}
			objs[i] = r.regClosure(f, c, bytecode.MakeRBC(bytecode.RClosureW, 0, 0, uint32(k)), wb)
			r.jit.materialized++
		}
		*v = Obj(objs[i])
	}
	end := min(len(r.stack), wb+e.p.Regs+e.scratch+e.frame)
	for i := wb; i < end; i++ {
		fix(&r.stack[i])
	}
	for _, sp := range spans {
		for i := sp[0]; i < min(sp[1], len(r.stack)); i++ {
			fix(&r.stack[i])
		}
	}
}

// jitRegRun runs f, running c with its window at wb, in e's code from
// register pc rpc, an entry's, until it returns, and goes on in the
// register interpreter where the code leaves for good.
func (r *Runtime) jitRegRun(f *frame, c *regCode, e *jitRegEntry, wb, rpc int) (Value, error) {
	pc, v, done, err := r.jitRegGoOn(f, c, e, wb, rpc)
	switch {
	case err != nil:
		return Undefined, err
	case done:
		return v, nil
	}
	return r.runRegFor(f, c, pc)
}

// jitRegGoOn runs f, running c with its window at wb, in e's code from
// register pc rpc, an entry's, until it returns -- done, with what it
// returns -- or throws what no handler of the frame's takes, err, or
// leaves the frame to the register interpreter, which goes on with it at
// register pc pc: after a deoptimization, at a register pc the code has no
// entry at, or where the code is not to run. A frame the interpreter runs
// goes on so in its own loop (jitRegLoop); a call's, in runReg.
func (r *Runtime) jitRegGoOn(f *frame, c *regCode, e *jitRegEntry, wb, rpc int) (pc int, v Value, done bool, err error) {
	return r.jitRegGoOnAt(f, c, e, wb, rpc, -1, 0)
}

// jitRegGoOnAt is jitRegGoOn, but where resume is not 0 begun by going on
// in e's code where a native call it made returns to, in context at, the
// frame's level of a native call Go finished the callee of (jitRegUnwind),
// the call's result in the next context's RetValue.
func (r *Runtime) jitRegGoOnAt(f *frame, c *regCode, e *jitRegEntry, wb, rpc, at int, resume uintptr) (pc int, v Value, done bool, err error) {
	run := r.jit.regGoOnRun
	r.jit.regGoOnRun = 0
	if resume == 0 && !r.jitRegThisBound(f, wb) {
		return rpc, Undefined, false, nil
	}
	// Whether the run began at a loop's header: its stretches after host
	// exits are judged as on-stack replacement's (jitRegStretchWork). A run
	// going on with a frame another run of the code left (jitRegLoop's
	// regGoOnRun) is judged as that run, whose on-stack replacement or not
	// it was begun by. Judged as begun at the loop, a function whose calls
	// each leave at a turn of their loop, gone on with at its header for
	// the turns after, had its code found slow for the loop (loopSlow) at
	// once, the rest of each call the interpreter's.
	loop, fixed := run == 2, run != 0
	if j := r.jit.regPendingJobs[e.key]; resume == 0 && j != nil && j.waiting {
		// A compile of this function waiting for the run that left
		// (jitRegCompileDueNow): installed now, a check, and gone on in
		// below.
		r.jitDrain()
	}
	for {
		ipc := 0
		if resume == 0 && f.cl.reg == c {
			if ne := f.cl.closureCode.jitReg; ne != nil && ne != e && ne != e.osrOf && !ne.invalid && ne.code != nil {
				// Replaced meanwhile (jitRegCached): on in the function's
				// code now.
				e = ne
			} else if e.life != nil && e.life.reopt && r.jitRegLearnedDue(f.cl, e) {
				// Due to be compiled again for what it learned: compiled
				// now, at the exit, and gone on in that code -- keeping the
				// entry of the loop around where the frame goes on, for a
				// function hardly called yet: one called for a long loop,
				// which this frame is in (jitRegFrameAt).
				if e.life.starts < jitKeptStarts {
					r.jitFrameFn, r.jitFramePC = f.cl.fn, regLoopAround(c, rpc)
				}
				ne := r.jitRegReoptimize(f.cl, c, e, rpc)
				r.jitFrameFn = nil
				if ne != nil {
					e = ne
				}
			}
		}
		if resume == 0 {
			ipc = e.ipc(rpc)
			if e.invalid || e.code == nil || ipc < 0 || !e.code.HasEntry(ipc) || wb+e.p.Regs+e.scratch+e.frame > len(r.stack) {
				if ne := r.jitRegResumeSeen(f, c, e, rpc); ne != nil {
					// Compiled again with the entry, gone on in from here.
					e = ne
					continue
				}
				if r.jit.reporting {
					r.jit.regEndWhy = jitRegNoEntryWhy(f, c, e, wb, ipc)
				}
				return r.jitRegLeaveTo(f, c, e, rpc, loop), Undefined, false, nil
			}
		}
		if !fixed && resume == 0 && ipc < len(e.p.Origin) && e.isLoop(int(e.p.Origin[ipc])) {
			loop = true
		}
		ctx, ret, ok := r.jitRegStretch(f, c, e, wb, ipc, at, resume, loop)
		at, resume = -1, 0
		if !ok {
			return r.jitRegLeaveTo(f, c, e, rpc, loop), Undefined, false, nil
		}
		next, pc, v, done, err, native := r.jitRegLeft(f, c, e, wb, ctx, ret)
		// The run's hold on e let go of (jitRegStretch): code the cache let
		// go of while it was in it, closed now if no other run is; what
		// goes on goes on in its replacement.
		e.runs--
		r.jit.regClose(e)
		if !native {
			if !done && err == nil {
				pc = r.jitRegLeaveTo(f, c, e, pc, loop)
			}
			return pc, v, done, err
		}
		if ctx.ExitKind == abi.ExitHost && len(e.kept) != 0 && e.life != nil {
			r.jitRegKeptLeft(e)
		}
		if loop && ctx.ExitKind == abi.ExitHost && e.life != nil && e.life.loopSlow {
			// A loop that leaves at its turns, judged not to pay for its
			// exits (loopSlow): the interpreter goes on with it after the
			// instruction Go ran -- array_fill_down's stores to a sparse
			// array had left and gone on natively at each turn.
			if r.jit.reporting {
				r.jit.regEndWhy = "slow loop"
			}
			return r.jitRegLeaveTo(f, c, e, next, loop), Undefined, false, nil
		}
		rpc = next
	}
}

// jitRegThisBound reports whether native code may run f, its window at wb:
// not an arrow's frame that shares a derived constructor's this binding
// (funcExtra's lexThisRef) not bound yet, which the interpreter runs. One
// bound has its register 1 made the value bound, which its maker's lexThis,
// taken before super() bound it, may not be: the arrows its code makes from
// their pools capture this from there (ssa.ClosureSite's LexThisReg), and
// the binding, once bound, is fixed.
func (r *Runtime) jitRegThisBound(f *frame, wb int) bool {
	if f.thisRef == nil || f.cl.fn.Kind != bytecode.KindArrow {
		return true
	}
	if !f.thisRef.init {
		return false
	}
	r.stack[wb+1] = f.thisRef.value
	return true
}

// jitRegLeaveTo leaves f, whose run of e's code left it, to the interpreter
// at register pc rpc, and returns rpc. Valid code the frame goes on in at
// the next back edge of the interpreter's, whose check is made due at once
// (jitRegLoop): an iteration that left, as V8's code deoptimizes, leaves
// the rest of that iteration to the interpreter, not the rest of the
// invocation. Before, the interpreter ran it all, native code entered again
// only at a sample a full back-edge budget later. Code invalidated, a guard
// of which failed, leaves the interpreter the rest of the invocation, as a
// deoptimized invocation is run. osr says whether the run was begun at a
// loop's header: the frame goes on in a run judged as it was (regGoOnOSR),
// so that one begun at the function's start is no loop's that its code is
// found slow for (loopSlow), but slow only where its entries do not pay
// (entrySlow).
func (r *Runtime) jitRegLeaveTo(f *frame, c *regCode, e *jitRegEntry, rpc int, osr bool) int {
	s := r.jit
	s.interpreted++
	if s.reporting {
		s.noteRegEnded(c, rpc)
	}
	if life := e.life; life != nil {
		life.goOns++
		life.ssaStats.ended++
		r.jitRegKeptLeft(e)
	}
	if cc := f.cl.closureCode; !e.invalid && cc != nil && cc.jitReg != nil && cc.jitReg.code != nil && !cc.jitReg.invalid &&
		(len(cc.jitReg.loops) != 0 || len(cc.jitReg.unentered) != 0) && !jitRunSlow(cc.jitReg.life, osr) && !regEndsFrame(f.cl.reg, rpc) {
		// Code with a loop to go on at -- an entry it has, or one escape
		// analysis dropped (unentered), whose frames go on in the code kept
		// for them (jitRegLoopCode). One with no loop, the frame's
		// invocation ends in the interpreter, and the check made due at
		// once would be its caller's loop's, for nothing -- each of AWFY
		// Richards' runTask's 13 million exits had paid one. Asked of loops
		// alone, a loop whose object stayed unmade had every call that left
		// it finish in the interpreter: carried_exit_500 ran at the
		// interpreter's speed.
		s.regGoOn, s.regGoOnFrom, s.regForced, s.regGoOnOSR = f, e, true, osr
		r.backEdges = 1
	}
	return rpc
}

// jitRegKeptLeft counts a call of e's function that left its code, once
// however often: code kept for a frame goes by it (jitRegKeptCallsLeave). A
// host exit counts in code with entries kept for a frame (jitRegEntry's
// kept): its objects made, the code has an entry after the exit, where code
// leaving them unmade has none and leaves -- as the call would in the code
// that gives the entries up.
func (r *Runtime) jitRegKeptLeft(e *jitRegEntry) {
	life := e.life
	calls := life.starts
	if l := r.jit.regLinkFor(e); l != nil {
		calls += l.in
	}
	if life.keptCall != calls {
		life.keptCall = calls
		life.keptLeft++
	}
}

// regEndsFrame reports whether register pc rpc of c is an instruction at
// which its frame's run in the interpreter ends or suspends -- a return, a
// yield, an await -- so that no back edge of the frame's comes after it in
// that run: a check made due at once for the frame (jitRegLeaveTo) would be
// the next frame's, its caller's or the generator's consumer's, whose
// budget it spends for no sample (jitRegLoop). A generator's code leaving
// at each yield had made every back edge of its for...of loop's such a
// check, the loop's function never sampled, never compiled.
func regEndsFrame(c *regCode, rpc int) bool {
	if c == nil || rpc < 0 || rpc >= len(c.rc.Code) {
		return false
	}
	switch regAt(c.code, rpc).Op() {
	case bytecode.RRet, bytecode.RRetUndef, bytecode.RYield, bytecode.RYieldStar, bytecode.RAwait:
		return true
	}
	return false
}

// jitRegLeft is what Go makes of the way e's code, run on f's window at wb,
// left in ctx, ret what a return returned: native code goes on at register
// pc next, native; or the frame is done, with v; or it threw err, which no
// handler of the frame's takes; or the interpreter goes on with it at
// register pc pc.
func (r *Runtime) jitRegLeft(f *frame, c *regCode, e *jitRegEntry, wb int, ctx *abi.Context, ret Value) (next, pc int, v Value, done bool, err error, native bool) {
	s := r.jit
	exitPC := int(ctx.ExitPC)
	if ctx.Loop != 0 && ctx.ExitKind != abi.ExitReturn && exitPC < len(c.rc.Code) && regAt(c.code, exitPC).Op() == bytecode.RCallMW {
		// Inside a loop of a built-in's call: Go goes on with it.
		s.regEndWhy = "built-in's loop left"
		return r.jitRegLoopLeft(f, c, e, wb, ctx)
	}
	switch ctx.ExitKind {
	case abi.ExitReturn:
		return 0, 0, ret, true, nil, false
	case abi.ExitDeopt:
		// A guard failed before its instruction committed anything: the
		// frame holds the state at its start, which the interpreter runs
		// the rest of the invocation from, and the code is invalidated,
		// compiled again from what the interpreter learns once it has
		// spent a fresh budget (jitRegInvalidate).
		s.guards++
		e.life.ssaStats.guards++
		r.jitRegMaterialize(f, e, wb)
		if int64(ctx.ExitSite) < 0 {
			// An entry's speculation, at a loop's header or the start: the
			// entry's (EntryGeneric).
			r.jitRegEntrySeen(f, exitPC)
		} else if !r.jitRegBigIntSeen(f, e, wb, int(int64(ctx.ExitSite))) {
			// An operator of BigInts it learned is not generic: the code
			// compiled again makes it of them (jitBigIntNote).
			r.jitRegDeoptSeen(f, c, e, wb, exitPC)
		}
		r.jitRegInvalidate(f.cl, e)
		s.regEndWhy = "guard failed"
		return 0, exitPC, Undefined, false, nil, false
	case abi.ExitHost:
		// Go runs the register instruction whole, then native code goes
		// on at the next entry.
		s.hosts++
		e.life.ssaStats.hosts++
		r.jitRegMaterialize(f, e, wb)
		f.pc = uint32(exitPC) + 1
		var ran bool
		if p := s.regPending; p.set && p.pc == exitPC && p.ctx == int(ctx.Level) {
			// A call Go made from native code, which left after it
			// (jitRegGoCall), or a native call whose callee Go finished
			// (jitRegUnwind): what it answered goes to the frame, as the
			// call's register instruction would have it.
			s.regPending = jitGoPending{}
			if p.step {
				p.v, p.err = r.jitRegStepCalled(f, c, wb, exitPC, p.v, p.err)
			}
			if p.whole {
				next, ran, err = r.regFinishWhole(wb, p)
			} else {
				next, ran, err = r.regFinishCall(f, c, wb, exitPC, p.v, p.err)
			}
		} else {
			s.noteRegHost(c, exitPC)
			if r.jitRegStringAddSeen(r.jitRegLearnedFor(f, e), c, wb, exitPC) {
				// An addition that met a string, Go's from the next
				// lowering on.
				r.jitRegReopt(f.cl, e)
			}
			seen := r.jitRegHostSeen(f, c, wb, exitPC)
			r.jitRegPrivateSeen(f, c, e, wb, exitPC)
			r.jitRegCallMissed(f, c, e, wb, exitPC)
			if r.jitRegBigIntExit(f, e, wb, int(int64(ctx.ExitSite))) {
				// A generic operator that met BigInts, which the code
				// compiled again calls Go for (ssa.GoArith).
				r.jitRegReopt(f.cl, e)
			}
			var callee Value
			w := regAt(c.code, exitPC)
			if w.Op() == bytecode.RNewW && wb+int(w.A()) < len(r.stack) {
				callee = r.stack[wb+int(w.A())]
			} else if w.Op() == bytecode.RApplyArgs && wb+int(w.B())+2 < len(r.stack) {
				// The function it forwards to, before its register is
				// written.
				callee = r.stack[wb+int(w.B())]
			}
			// A method call's operands, which may be a loop's built-in's
			// (jitRegForEachSeen), read before Go runs it.
			var mcallee, mrecv, mcb Value
			if w.Op() == bytecode.RCallMW && w.X() >= 1 && wb+int(w.A())+2 < len(r.stack) {
				mcallee, mrecv, mcb = r.stack[wb+int(w.A())], r.stack[wb+int(w.A())+1], r.stack[wb+int(w.A())+2]
			}
			next, ran, err = r.regWhole(f, c, wb, exitPC)
			if err != nil && mcallee.IsObject() && !e.invalid {
				// A loop's built-in's call whose callback threw: learned
				// all the same -- a loop whose callback throws at every
				// call was never made.
				r.jitRegForEachSeen(f, e, exitPC, mcallee, mrecv, mcb, int(w.X()))
			}
			if seen.write && err == nil && ran {
				r.jitRegWriteSeen(f, c, e, exitPC, seen)
			}
			if seen.elem {
				// An element's kind learned: compiled again for it, no
				// deoptimization.
				r.jitRegReopt(f.cl, e)
			}
			if err == nil && ran {
				// A call native code did not make, which it may once
				// compiled again -- a loop's built-in's too, whose checks
				// failed: a callback made anew at each call of its maker,
				// called by its code from here, as at a call of Go; and a
				// super()'s parent.
				repeats := r.jitRegCallRepeats(f, c, e, exitPC, callee)
				r.jitRegCallSeen(f, c, e, exitPC, callee)
				if repeats {
					// What it learned there, it learned at an earlier exit:
					// invalidated, as V8's code deoptimizes at a call of a
					// target it was not compiled for, the interpreter
					// running the function until it is compiled again with
					// it, rather than leaving at each such call -- a call
					// of eight functions, four learned one at a time, had
					// left at every other turn until its loop was judged
					// slow for it.
					r.jitRegInvalidate(f.cl, e)
				}
				r.jitRegSuperSeen(f, c, e, exitPC)
				if mcallee.IsObject() {
					r.jitRegViaSeen(f, e, exitPC, mcallee, mrecv, int(w.X()))
					r.jitRegForEachSeen(f, e, exitPC, mcallee, mrecv, mcb, int(w.X()))
				}
			}
			if l := e.life; l.polyReopt {
				// The site met what the code was not compiled for and its
				// cache cannot hold: compiled again for it once that is
				// due, its code valid meanwhile.
				l.polyReopt = false
				r.jitRegReopt(f.cl, e)
			}
		}
		if !ran {
			if sig := r.jitRegSuspend(f, c, wb, exitPC); sig != nil {
				// A yield, a yield* or an await: the frame suspended here.
				return 0, 0, Undefined, false, sig, false
			}
			s.regEndWhy = "host exit Go does not run whole"
			return 0, exitPC, Undefined, false, nil, false
		}
		if err != nil {
			return 0, 0, Undefined, false, err, false
		}
		if r.stopped != nil {
			return 0, 0, Undefined, false, r.stopped, false
		}
		return next, 0, Undefined, false, nil, true
	case abi.ExitPoll:
		// The interpreter's back-edge check, at a loop's header, the
		// stretch's work counted (jitRegStretchWork). Code due to be
		// compiled again for what it learned is, and the loop goes on in
		// that code (jitRegGoOnAt); before, it went on in the interpreter
		// until a sample a budget later.
		s.budgets++
		e.life.ssaStats.polls++
		r.backEdges = s.backEdgeBudget()
		if exitPC == 0 {
			// The poll at the code's start (ssa.OpPoll), which the run
			// going on there passes again: counted once. Under a stress
			// budget of one back edge, a function entered at its start
			// had polled and left there for ever.
			r.backEdges++
		}
		if err := r.checkInterruptPoll(); err != nil {
			return 0, 0, Undefined, false, err, false
		}
		// The objects and the closures the code did not make, which the
		// frame names by their homes and templates: made before anything
		// else reads the frame -- the interpreter, where the code has no
		// entry at the header, escape analysis having dropped it for them
		// (ssa.Func's Unentered), or where a stress deoptimization
		// finishes the invocation there; or code compiled for a frame
		// waiting there -- as at any other exit. Left unmade, the
		// interpreter read a home's word as the object, a null, and a
		// closure only called inlined was its template, "not a function".
		r.jitRegMaterialize(f, e, wb)
		if r.jitStressDeopt(s) {
			s.regEndWhy = "stress"
			return 0, exitPC, Undefined, false, nil, false
		}
		return exitPC, 0, Undefined, false, nil, true
	}
	// No other way out of register code's native code yet: what it left,
	// the interpreter goes on with from its start.
	r.jitRegMaterialize(f, e, wb)
	s.regEndWhy = fmt.Sprintf("exit kind %d", ctx.ExitKind)
	return 0, exitPC, Undefined, false, nil, false
}

// jitRegNoEntryWhy is why a run of e's code, which f running c with its
// window at wb was to go on in at the program's instruction ipc, did not:
// the reason the -jitreport counts runs ended in the interpreter by
// (noteRegEnded).
func jitRegNoEntryWhy(f *frame, c *regCode, e *jitRegEntry, wb, ipc int) string {
	switch {
	case e.invalid:
		return "invalid code"
	case e.code == nil:
		return "code closed"
	case ipc < 0:
		return "no lowering"
	case !e.code.HasEntry(ipc):
		return "no entry"
	}
	return "no stack"
}

// jitRegStretch runs e's code once, from its entry at instruction ipc, on
// f's window at wb, in the next context -- or, resume not 0, going on where
// a native call it made returns to (jitRegGoOnAt), in context at -- and
// returns that context with the exit's kind, register pc and the frame
// written -- an exit that left its frame to Go (abi.ExitTable) applied here
// -- and, for a return, the value returned, read before what the run kept
// is let go. A native call whose callee left native code Go finishes
// (jitRegUnwind), and the code goes on after it where it can, as V8's lazy
// deoptimization leaves the caller's frame alone; where it cannot -- code
// a guard of which failed meanwhile, a throw -- the call's state is
// written to the frame, its exit a host exit at the call, which Go
// finishes with the callee's answer (regPending). ok is false where it did
// not run: no context, or the code closed. Where ok, the run still holds e
// (its runs), which its caller lets go of once it has made of the exit what
// it needs (jitRegGoOnAt): an exit's frame names what e's code did not make
// -- the closures of its feedback's made, the objects whose homes its frame
// slots are -- which jitRegLeft makes from e. Let go of here, code the
// cache had dropped during the run was closed before it, its feedback gone,
// and a callback it had not made was handed to the call Go ran as its
// template: TypedArray.prototype.map threw that it was no function.
func (r *Runtime) jitRegStretch(f *frame, c *regCode, e *jitRegEntry, wb, ipc, at int, resume uintptr, loop bool) (ctx *abi.Context, ret Value, ok bool) {
	s := r.jit
	idx := s.ctxTop
	if at >= 0 {
		idx = at
	}
	if idx >= len(s.ssaCtxs)-1 && !r.jitGrowContexts() {
		s.regEndWhy = "no context"
		return nil, Undefined, false
	}
	top := s.ctxTop
	s.ctxTop = idx + 1
	e.runs++
	ctx = s.ssaCtxs[idx]
	s.ssaCtx = ctx
	ctx.ReturnTo, ctx.Live, ctx.TailReturn = 0, 0, 0
	r.jitShareContexts(idx, f)
	p := e.p
	ctx.Locals = unsafe.Pointer(&r.stack[wb])
	ctx.Stack = unsafe.Add(ctx.Locals, (p.Regs+e.scratch)*int(jitEncoding.ValueSize))
	ctx.Upvalues = unsafe.Pointer(unsafe.SliceData(f.cl.upvalues))
	if resume == 0 {
		ctx.Cells, ctx.Loop = nil, 0
	}
	ctx.Open = uint64(len(f.openUpvalues))
	if f.cl.fn.HasTailCall {
		// Its calls of itself in tail position, deep in the stack, give
		// the frame to the call in place natively where the interpreter
		// would (ssa.OpTailSelf, regTailTo): its closure, and whether it
		// would -- a call's frame, or regEnterW's of no construction, that
		// shares no derived constructor's this and keeps no arguments --
		// and its receiver's home, which a call with another rewrites.
		ctx.Closure, ctx.TailFrame, ctx.FrameThis = unsafe.Pointer(f.cl), 0, unsafe.Pointer(&f.this)
		if (f.regKind == regFrameCall || f.regKind == regFrameEnter && f.newTarget.IsUndefined()) && f.thisRef == nil &&
			f.args == nil && f.withScopes == nil && f.evalVars == nil && !c.suspends {
			ctx.TailFrame = 1
		}
	}
	if p.This {
		// A derived constructor's: its this binding -- its this slot --
		// and what its super() natively needs, as a native construction's
		// context has them (ssa.CallSite's Super). Code going on where a
		// native call returns to finds the binding a super() it made
		// natively bound in the context, which the frame is given first.
		if resume != 0 {
			jitBindThis(f, ctx)
		}
		jitRegDerivedContext(ctx, f)
	}
	if p.Forwards {
		// Its f.apply(x, arguments) hands the frame's arguments on natively
		// (ssa.CallSite's Forward), from its context (jitForwardEntry).
		jitForwardEntry(ctx, f.args)
	}
	// No frame in goRuns (jitGoRun) for Go's calls from the code to read:
	// they leave what needs one to an exit.
	if idx >= len(s.goRuns) {
		s.goRuns = append(s.goRuns, make([]jitGoRun, idx+1-len(s.goRuns))...)
	}
	s.goRuns[idx] = jitGoRun{}
	// The register frame its calls of Go make theirs on (jitRegGoCall).
	if idx >= len(s.regRuns) {
		s.regRuns = append(s.regRuns, make([]jitRegRun, idx+1-len(s.regRuns))...)
	}
	// A run begun at a loop's header, by on-stack replacement, whose calls
	// of Go are judged as they come (jitRegRunGoProfit) -- or going on
	// after a host exit of one: loop, as jitRegGoOnAt judges it.
	osr := loop
	s.regRuns[idx] = jitRegRun{f: f, c: c, e: e, wb: wb, osr: osr, edges: r.backEdges - s.pollCredit, rpc: -1, stretch: r.backEdges - s.pollCredit}
	s.regPending = jitGoPending{}
	s.entries++
	s.ssaEntries++
	// Each stretch, an entry or one after an exit or a poll
	// (jitRegStretchWork).
	e.life.ssaStats.entries++
	saved, raised := r.stackTop, -1
	if need := wb + p.Regs + e.scratch + e.frame; need > saved && need <= len(r.stack) {
		// The scratch slots, homes and loops' slots past the window,
		// which the stack's top covers while the code runs.
		r.stackTop, r.stackHigh, raised = need, max(r.stackHigh, need), need
	}
	var err error
	edges := r.backEdges - s.pollCredit
	// The run calls of Go from its native callees' code are of
	// (jitRegRootOf), until it leaves.
	runAt := s.regRunAt
	s.regRunAt = idx + 1
	if resume != 0 {
		err = e.code.Resume(resume, s.ssaCtxs[idx+1])
	} else {
		err = e.code.Run(ipc, ctx)
	}
	if r.gcYield.on && r.gcMarking() {
		r.gcYieldNow()
	}
	for err == nil && s.ssaCtxs[idx+1].Live == abi.LiveCall {
		// A native call's callee left native code: Go finishes it, and
		// every native call between, then the code goes on where the
		// call returns to, its state as it left it, the call's result in
		// the callee's context (RetValue), written with Go's barrier -- the
		// one pointer native code stores before it tests the collector's
		// flag again, into a keep cell nothing native reads the old value
		// of (mir's resumeCall) -- the collector marking or not. Not into
		// code that is invalid now, or closed: what Go ran may have run
		// the function again and found its code wanting.
		callee := s.ssaCtxs[idx+1]
		back, base := callee.ReturnTo, callee.Base
		rpc, _ := jit.CallPC(ctx)
		f.pc = uint32(rpc) + 1
		v, uerr, gen := r.jitRegUnwind(idx)
		if uerr == nil && r.stopped == nil && back != 0 && !e.invalid && e.code != nil && e.code.Contains(back) {
			callee.Base, callee.Live, callee.ReturnTo = base, 0, 0
			*(*Value)(unsafe.Pointer(&callee.RetValue)) = v
			s.resumed++
			s.regPending = jitGoPending{}
			err = e.code.Resume(back, callee)
			if r.gcYield.on && r.gcMarking() {
				r.gcYieldNow()
			}
			continue
		}
		// The call's state written to the frame below, the call's exit a
		// host exit at it, which Go finishes with what the callee answered
		// (jitRegLeft).
		jit.ExpandCall(ctx)
		s.regPending = jitGoPending{set: true, pc: int(rpc), ctx: idx, v: v, err: uerr, step: !gen}
		break
	}
	s.regRunAt = runAt
	if r.stackTop == raised {
		r.stackTop = saved
	}
	if p.This {
		// A super() native code made bound the context's This, which no
		// exit writes: the frame's binding is it.
		jitBindThis(f, ctx)
	}
	if err == nil && ctx.Cells != nil {
		// Its cells, as the frame has them, for whatever runs it now.
		r.jitRegReopenCells(f, e, wb, ctx, true)
	}
	if len(s.builders) != 0 {
		r.jitFreezeBuilders()
	}
	if s.goRuns[idx].pinned {
		r.jitGoUnpin(idx)
		if len(s.regRetired) != 0 {
			s.jitRegCloseRetired()
		}
	}
	if err == nil && ctx.ExitKind == abi.ExitTable {
		s.ssaRecords += uint64(s.exitScratch.Apply(ctx))
	}
	if err == nil {
		r.jitRegStretchWork(e, ctx, ipc, edges, osr)
		r.jitRegApplyRecords(f, p, e.kinds, wb, ctx)
		if ctx.ExitKind == abi.ExitReturn {
			ret = Value{num: math.Float64frombits(ctx.Ret)}
			if src := jitSource(&ctx.RetFrom); src != nil && src.ref != nil {
				ret = *src
			}
		}
	}
	if err == nil && ctx.ExitKind == abi.ExitHost {
		// An exit inside a callee the code inlined at the call it left at:
		// Go finishes the callee, and the call goes on with what it
		// answered, as a native callee's (regPending).
		rpc := int(ctx.ExitPC)
		f.pc = uint32(rpc) + 1
		if v, uerr, ok := r.jitRegInlined(idx, f, e, rpc, wb); ok {
			s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: v, err: uerr}
		}
	}
	// What the run kept, and what its native calls' callees ran with in the
	// contexts past it -- closure, keeps, records, result -- each a frame
	// gone once it returned, up to the first context no call ran in.
	clear(ctx.Keep[:e.keeps])
	ctx.This, ctx.Upvalues, ctx.Instance = abi.Slot{}, nil, abi.Slot{}
	ctx.Closure, ctx.FrameThis = nil, nil
	if n := max(ctx.RecordHigh, s.callRefs); n != 0 {
		clear(ctx.RecordRef[:n])
		ctx.RecordHigh = 0
	}
	for i := idx + 1; i < len(s.ssaCtxs); i++ {
		c := s.ssaCtxs[i]
		if c.Closure == nil {
			break
		}
		if n := max(c.RecordHigh, s.callRefs); n != 0 {
			clear(c.RecordRef[:n])
			c.RecordHigh = 0
		}
		clear(c.Keep[:s.ssaKeeps])
		c.Closure, c.Upvalues, c.Cells = nil, nil, nil
		c.This, c.RetValue, c.Instance = abi.Slot{}, abi.Slot{}, abi.Slot{}
	}
	s.goRuns[idx] = jitGoRun{}
	s.regRuns[idx] = jitRegRun{}
	s.ctxTop = top
	if top == 0 {
		// No native code runs now, nor a call of Go it made: the stand-in
		// frame such a call of a callee's was made on (jitRegFrameRun),
		// its closure and receiver a frame gone too.
		s.calleeFrame = frame{}
	}
	if top == 0 && len(s.regRetired) != 0 {
		// No native code runs now: what the cache let go of while it did
		// is closed, if nothing else uses it (regClose).
		s.jitRegCloseRetired()
	}
	if closed := e.code == nil; closed || err != nil {
		// Code the cache let go of while this run was in it, closed now if
		// no other is.
		e.runs--
		s.regClose(e)
		if s.reporting {
			s.regEndWhy = "code closed"
			if err != nil {
				s.regEndWhy = err.Error()
			}
		}
		return nil, Undefined, false
	}
	return ctx, ret, true
}

// irLoopMean is the mean length of the loops of c's register code, in the
// program's instructions -- from each backward jump's target to it, as
// lowered -- at least one: every loop's, in the units jitSSAMinWork counts
// work in, which a stretch's back edges are counted by (jitRegStretchWork,
// jitRegCalleeProfit). Before, it was the mean of the loops the code has
// entries at alone, in register instructions.
func (e *jitRegEntry) irLoopMean(c *regCode) int {
	bl := c.rc.BackLive
	total, loops := 0, 0
	for i := 0; i+1 < len(bl); i += 2 {
		q := int(bl[i])
		t := q + 1 + int(regJumpOff(regAt(c.code, q)))
		if t < 0 || t > q || q >= len(e.at) {
			continue
		}
		end := len(e.p.Code)
		if q+1 < len(e.at) && e.at[q+1] >= 0 {
			end = int(e.at[q+1])
		}
		if from := int(e.at[t]); from >= 0 && end > from {
			total += end - from
			loops++
		}
	}
	if loops == 0 {
		return 1
	}
	return max(total/loops, 1)
}

// isLoop reports whether register pc rpc is the header of a loop e's code
// has an entry at.
func (e *jitRegEntry) isLoop(rpc int) bool {
	_, ok := slices.BinarySearch(e.loops, uint32(rpc))
	return ok
}

// jitRegStretchWork counts the work a stretch of e's code did natively,
// begun at instruction ipc with the back-edge counter, less the polls'
// credit, at edges, and left as ctx says: the back edges it took, each its
// loops' mean length, and the instructions from its start to where it left.
// The code spends it toward its next compile (jitRegSpent): a loop that
// leaves at the same exit each turn, for what it learned there, had spent
// nothing between its function's calls, and was compiled again only at the
// next one.
//
// The stretch judges the code: each stretch -- an entry, a call of Go --
// costs what an exit and an entry would (jitSSAMinWork); every jitSSAProbe
// of them, code whose work does not make up for them Go no longer enters
// (entrySlow), at its start nor at its loops' headers, its native callers
// calling it still, until it is retried (jitRegRetrySlow); and stretches
// begun by on-stack replacement are judged apart (loopSlow), Go no longer
// entering the code at its loops.
func (r *Runtime) jitRegStretchWork(e *jitRegEntry, ctx *abi.Context, ipc, edges int, osr bool) {
	life := e.life
	if life.typed {
		return
	}
	st := &life.ssaStats
	work := uint64(0)
	if back := edges - (r.backEdges - r.jit.pollCredit); back > 0 {
		work = uint64(back) * uint64(e.loopMean)
	}
	if ctx.ExitKind == abi.ExitHost || ctx.ExitKind == abi.ExitDeopt || ctx.ExitKind == abi.ExitReturn {
		if x := e.ipc(int(ctx.ExitPC)); x > ipc {
			work += uint64(x - ipc)
		}
	}
	st.work += work
	if osr {
		st.osrStretches++
		st.osrWork += work
		if st.osrStretches%jitSSAProbe == 0 && st.osrWork < st.osrStretches*jitSSAMinWork {
			life.entrySlow, life.loopSlow = true, true
		}
	}
	stretches := st.entries + st.goCalls
	if stretches-st.judged < jitSSAProbe {
		return
	}
	st.judged = stretches
	if st.work-st.workBase < (stretches-st.stretchBase)*jitSSAMinWork {
		life.entrySlow = true
	}
}

// jitRegRunWork is the work run's stretch has done natively so far, as
// its end will count it (jitRegStretchWork): the back edges it has taken,
// each its code's loops' mean length.
func (r *Runtime) jitRegRunWork(run *jitRegRun) int64 {
	if back := run.stretch - (r.backEdges - r.jit.pollCredit); back > 0 && run.e != nil {
		return int64(back) * int64(run.e.loopMean)
	}
	return 0
}

// jitRegRunGoProfit judges a call of Go e's code makes at register pc rpc
// in run, a run begun at a loop's header: each costs native code what an
// exit and an entry would (jitSSAMinWork), against the work done natively
// since the call before -- the back edges taken, at the loops' mean length,
// or the distance from it -- and every jitSSAProbe such calls, code whose
// calls do not pay Go no longer enters (entrySlow, loopSlow), unless it
// calls functions natively (jitRegCodeCallsNatively), whose work the back
// edges count only in part. It reports whether the code is judged so, for
// the run to leave once the call is made (jitRegRun's leave).
func (r *Runtime) jitRegRunGoProfit(e *jitRegEntry, run *jitRegRun, rpc int) bool {
	life := e.life
	if life.typed {
		return false
	}
	st := &life.ssaStats
	edges := r.backEdges - r.jit.pollCredit
	if back := run.edges - edges; back > 0 {
		st.runWork += uint64(back) * uint64(e.loopMean)
	} else if run.rpc >= 0 && rpc > run.rpc {
		st.runWork += uint64(e.ipc(rpc) - e.ipc(run.rpc))
	}
	run.edges, run.rpc = edges, rpc
	st.runGoCalls++
	if st.runGoCalls%jitSSAProbe == 0 && st.runWork < st.runGoCalls*jitSSAMinWork && !e.jitRegCodeCallsNatively() {
		life.entrySlow, life.loopSlow = true, true
	}
	return life.loopSlow
}

// jitRegCodeCallsNatively reports whether e's code calls functions
// natively: callees it learned it calls (nativeCalls), or native calls it
// was compiled with (callsNative).
func (e *jitRegEntry) jitRegCodeCallsNatively() bool {
	return e.callsNative || len(e.life.nativeCalls) != 0
}

// jitRegGoMark is the last call of Go a native callee's register code made
// (jitState's regGoMark): its code, the stretch it was made in (jitState's
// entries then), the back-edge counter after it, less the polls' credit,
// and its register pc -- what the next one's work is counted from.
type jitRegGoMark struct {
	e       *jitRegEntry
	stretch uint64
	edges   int
	rpc     int
}

// jitRegCalleeProfit accounts for a call of Go e's code makes at register
// pc rpc as a native callee, and every jitSSAProbe such calls decides
// whether its native code pays: each costs what an exit and an entry would
// (jitSSAMinWork), against the work done natively since the last -- the
// back edges since, at its loops' mean length, and the distance from the
// last call, at a later pc in the same stretch -- beyond its native
// callers' calls (its link's in), which pay no entry. Code that does not
// pay Go no longer enters and native callers no longer call (notNative)
// until retried (jitRegRetryNative).
func (r *Runtime) jitRegCalleeProfit(e *jitRegEntry, rpc int) {
	s := r.jit
	life := e.life
	if life == nil || e.invalid || e.code == nil || life.typed {
		return
	}
	st := &life.ssaStats
	m := &s.regGoMark
	if back := m.edges - (r.backEdges - s.pollCredit); m.e != nil && back > 0 {
		st.calleeWork += uint64(back) * uint64(e.loopMean)
	}
	if m.e == e && m.stretch == s.entries && rpc > m.rpc {
		st.calleeWork += uint64(max(e.ipc(rpc)-e.ipc(m.rpc), 0))
	}
	st.calleeGoCalls++
	l := s.regLinkFor(e)
	in := uint64(0)
	if l != nil {
		in = l.in
	}
	if st.calleeGoCalls%jitSSAProbe != 0 || st.calleeGoCalls <= in {
		return
	}
	if st.calleeWork < (st.calleeGoCalls-in)*jitSSAMinWork {
		life.entrySlow = true
		life.notNative, life.nativeRetry = true, 0
		if l != nil {
			l.clearCode()
		}
		life.nativeBackoff = min(life.nativeBackoff+1, jitNativeBackoffs)
	}
}

// jitRegRetrySlow counts a call Go made of e's code without entering it
// (entrySlow), and has Go enter it again after jitNativeRetry such calls,
// doubled each time it was found not to pay, judging it afresh: code judged
// on its first stretches, its calls not yet decided, may pay once it has
// learned.
func (r *Runtime) jitRegRetrySlow(e *jitRegEntry) {
	life := e.life
	if e.callee || e.code == nil {
		// Code for native callers alone, which Go does not enter.
		return
	}
	if life.slowCalls++; life.slowCalls-life.slowSince < jitNativeRetry<<life.slowBackoff {
		return
	}
	st := &life.ssaStats
	life.entrySlow, life.loopSlow = false, false
	life.slowSince, life.slowBackoff = life.slowCalls, min(life.slowBackoff+1, jitNativeBackoffs)
	st.judged = st.entries + st.goCalls
	st.workBase, st.stretchBase = st.work, st.judged
	st.runGoCalls, st.runWork, st.osrStretches, st.osrWork = 0, 0, 0, 0
}

// jitRegDerivedContext gives ctx, the context native code runs f in, a
// derived constructor's frame, what a native construction's has
// (ssa.CallSite's Derived): its this binding, the uninitialized value until
// its super() binds it (abi.Context's This, the code's this slot), its
// instance, new.target and function, which its super() reads.
func jitRegDerivedContext(ctx *abi.Context, f *frame) {
	if f.thisRef == nil || f.callee == nil || !f.newTarget.IsObject() || f.cl.fn.Kind != bytecode.KindDerivedConstructor {
		return
	}
	this := uninitialized
	if f.thisRef.init {
		this = f.thisRef.value
	}
	*(*Value)(unsafe.Pointer(&ctx.This)) = this
	*(*Value)(unsafe.Pointer(&ctx.Instance)) = f.thisRef.value
	ctx.NewTarget, ctx.Function = uint64(uintptr(unsafe.Pointer(f.newTarget.Object()))), uint64(uintptr(unsafe.Pointer(f.callee)))
}

// jitRegRun is a register frame native code runs on, in a context
// (jitState's regRuns): f, running c, its window at wb, in e's code.
type jitRegRun struct {
	f  *frame
	c  *regCode
	e  *jitRegEntry
	wb int
	// osr marks a run begun at a loop's header; edges is the back-edge
	// counter, less the polls' credit, when it began or last called Go,
	// and rpc the register pc of that call, -1 for none
	// (jitRegRunGoProfit); leave says the call of Go being made has the
	// code judged slow, which the run leaves once it is made.
	osr   bool
	edges int
	rpc   int
	// stretch is the back-edge counter, less the polls' credit, when the
	// run's stretch began, which its end counts the work of
	// (jitRegStretchWork), and a call of Go during it the work so far
	// (jitRegRunWork).
	stretch int
	// own counts the levels of the call's own that run e's code, which
	// leaving unwinds (jitRegCompileDueNow): none for the run's own, and,
	// -1, counted when asked for a call of Go from a native callee's code
	// (jitRegGoCallNested), whose run is in context root.
	leave bool
	own   int
	root  int
}

// jitRegGoCall is a call of Go (ssa.GoCall) native code made from register
// code makes at a typeof, which runs nothing, an instanceof, or a call,
// call_w's, call_method_w's or new_w's, on the register frame of the run in
// context idx, as the interpreter's slow path makes it (regCallSlow,
// regConstruct), not leaving native code: the function, the receiver and
// the arguments the code passed (abi.Context's GoArgs) put in a window of
// their own past the VM's stack's top -- not in the frame's, whose
// registers the code keeps values in as it found them -- the frame's pc
// past the call meanwhile, as rhCallW sets it, which a stack trace shows.
// Its result goes to the call's keep cell. A call that threw, or that the
// runtime stopped in, or after which the code was found invalid, Go
// finishes at the exit native code then takes (regPending, regFinishCall),
// the call not made again. A call in tail position, which the interpreter
// makes in constant space, is no call of Go (jitRegFeedback's NativeCalls).
func (r *Runtime) jitRegGoCall(ctx *abi.Context, idx int) bool {
	run := &r.jit.regRuns[idx]
	// A derived constructor's frame's binding, which a super() native
	// code made bound in the context alone (jitRegDerivedContext): bound
	// in the frame too before Go reads it, as for a call of Go on a level
	// (jitRegPushLevel) and an operation on the frame (jitRegFrameRun).
	// A super.m read Go made after a native super() had found it unbound.
	jitBindThis(run.f, ctx)
	if r.jitRegPlainWhole(ctx, run.f.cl, run.c) {
		return true
	}
	rpc := int(ctx.ExitPC)
	judged := run.osr && run.e != nil && run.e.life != nil && r.jitRegGoSettled(ctx, run.c, rpc)
	run.leave = judged && r.jitRegRunGoProfit(run.e, run, rpc)
	ok := r.jitRegGoCallOn(ctx, idx, *run)
	if s := r.jit; judged && idx < len(s.regRuns) {
		// The work the next call is judged against starts here: what this
		// one ran, which the interpreter would run alike, is none of it.
		s.regRuns[idx].edges, s.regRuns[idx].rpc = r.backEdges-s.pollCredit, rpc
	}
	return ok
}

// jitRegPlainWhole makes the call of Go ctx makes of register code c's
// instruction run whole (abi.GoCallWhole) without a frame where that runs
// nothing and throws nothing (jitRegFrameless), as jitGoCallPlain makes the
// operations it takes: ahead of the judgment of a run's calls of Go and
// their count, which such a call, a native operation's cost, is not.
func (r *Runtime) jitRegPlainWhole(ctx *abi.Context, cl *closure, c *regCode) bool {
	rpc := int(ctx.ExitPC)
	return ctx.GoOp>>abi.GoOpKindShift&0xff == abi.GoCallWhole && c != nil && rpc < len(c.rc.Code) &&
		r.jitRegFrameless(ctx, cl, c, rpc, regAt(c.code, rpc).Op())
}

// jitRegGoSettled reports whether the call of Go ctx makes at register pc
// rpc of c is one Go makes for good, which jitRegRunGoProfit judges: not
// a call of a function, which the code may make natively once it has
// learned the call (jitRegCallSeen).
func (r *Runtime) jitRegGoSettled(ctx *abi.Context, c *regCode, rpc int) bool {
	if rpc >= len(c.rc.Code) {
		return false
	}
	w := regAt(c.code, rpc)
	switch op := w.Op(); op {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW, bytecode.RCallWT, bytecode.RCallMWT:
		if ctx.GoOp>>abi.GoOpKindShift&0xff == abi.GoCallWhole {
			return true
		}
		at := 0
		if op.MethodCall() {
			at = 1
		}
		var callee Value
		if jitRegWideCall(w) {
			// In the callee's window (jitRegGoCallOn).
			callee = r.stack[r.stackTop]
		} else {
			callee = jitGoArg(ctx, at)
		}
		if !callee.IsObject() {
			return true
		}
		fd := callee.Object().fn()
		return fd == nil || fd.native != nil
	}
	return true
}

// jitRegWideCall reports whether the call of Go native code makes of
// register call w has its operands in the callee's window past the VM's
// stack's top (mir's windowGoCall), not in the context's: more than it
// holds (abi.MaxGoArgs).
func jitRegWideCall(w bytecode.RInstr) bool {
	switch w.Op() {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW, bytecode.RCallWT, bytecode.RCallMWT:
	default:
		return false
	}
	n := int(w.X()) + 1
	if w.Op().MethodCall() {
		n++
	}
	return n > abi.MaxGoArgs
}

// jitRegGoCallOn is jitRegGoCall on run's frame, the run's own or a level's
// of its native calls (jitRegGoCallNested).
func (r *Runtime) jitRegGoCallOn(ctx *abi.Context, idx int, run jitRegRun) bool {
	s := r.jit
	f, c := run.f, run.c
	rpc := int(ctx.ExitPC)
	if rpc >= len(c.rc.Code) {
		return false
	}
	if run.e != nil && run.e.life != nil {
		run.e.life.ssaStats.goCalls++
	}
	w := regAt(c.code, rpc)
	op := w.Op()
	argc := int(w.X())
	kind := ctx.GoOp >> abi.GoOpKindShift & 0xff
	if op == bytecode.RIterNext && (kind == abi.GoCallStepResult || kind == abi.GoCallMethod) {
		// A loop's step that calls its iterator's next natively
		// (regStepSites): the call of Go of its answer, or of the next.
		return r.jitRegStepCallMade(ctx, idx, run, rpc, kind)
	}
	if kind == abi.GoCallStep && op == bytecode.RIterNext {
		// A loop's step over a generator native code may resume once
		// compiled again (jitRegGenStepSeen), its cursor the step's operand.
		r.jitRegGenStepSeen(f, run.e, rpc, jitGoArg(ctx, 0))
		if st := cursorOf(jitGoArg(ctx, 0)); st != nil && !st.done && st.arr == nil && !st.forIn &&
			(r.genNextFn == nil || st.next.ref != unsafe.Pointer(r.genNextFn)) {
			return r.jitRegStepMade(ctx, idx, run, rpc, st)
		}
	}
	if op == bytecode.RIterStep && kind == abi.GoCallFill || op == bytecode.RIterRest && kind == abi.GoCallRest {
		// A destructuring's step that native code had none of its own for,
		// or its rest element: made here.
		return r.jitRegIterMade(ctx, idx, run, rpc, w)
	}
	if kind == abi.GoCallWhole && (op == bytecode.RNewClass || op == bytecode.RCopyDataProps) {
		// A class linked to its parent, an object spread that runs a
		// getter: made on the operands, not run whole.
		return r.jitRegOpMade(ctx, idx, run, rpc, w)
	}
	if kind == abi.GoCallWhole || kind == abi.GoCallStep && op == bytecode.RIterNext || kind == abi.GoCallForOf && op == bytecode.RForOfStart ||
		kind == abi.GoCallForIn && op == bytecode.RForInStart {
		// A for...of's cursor native code did not take from a generator's
		// room (regWholeSite's ForOf) too.
		return r.jitRegWhole(ctx, idx, run, rpc)
	}
	if op == bytecode.RSuperCall && kind == abi.GoCallSuper {
		return r.jitRegSuperMade(ctx, idx, run, rpc, w)
	}
	if op == bytecode.RApplyArgs && kind == abi.GoCallForward {
		return r.jitRegForwardMade(ctx, idx, run, rpc)
	}
	if op == bytecode.RTypeof && kind == abi.GoCallTypeOf {
		// typeof's name, as rhTypeof makes it, which runs nothing.
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Str(r.typeofString(jitGoArg(ctx, 0)))
		return true
	}
	switch {
	case op == bytecode.RCallW && kind == 0, op == bytecode.RCallMW && kind == abi.GoCallMethod, op == bytecode.RNewW && kind == abi.GoCallNew:
	case op == bytecode.RCallWT && kind == 0, op == bytecode.RCallMWT && kind == abi.GoCallMethod:
		// A call in tail position native code makes while the stack is
		// shallow, as rhCallWT makes the ordinary call there.
		if r.frameDepth >= tailCallDepth {
			return false
		}
	case op == bytecode.RLit && kind == abi.GoCallMethod && w.X()&bytecode.RLitCall != 0:
		// regexp_lit's call, of the method its expansion read, on its
		// literal's RegExp, of its one argument (compile's regexpLit).
		argc = 1
	case op == bytecode.RGetProp && kind == abi.GoCallGet, op == bytecode.RSetProp && kind == abi.GoCallSet,
		(op == bytecode.RGetMethod || op == bytecode.RGetMethodW) && kind == abi.GoCallGet:
		// get_method's and get_method_w's too, a megamorphic site's miss
		// (ssa's megamorphic read): its value the expansion's, which puts it
		// and the receiver in their registers after the call.
		// A property's read or write whose site found an accessor the code
		// does not inline nor call natively (ssa.AccessorSite's call of
		// Go): made as the interpreter makes it, through the site's cache,
		// the getter or the setter called on the frame.
		f.pc = uint32(rpc) + 1
		last, shared := s.ssaCtx, s.ssaSharedGen
		s.goCalls++
		r.noteRegGoCall(w, false, Value{})
		recv := jitGoArg(ctx, 0)
		v, err := r.jitRegPropertyMade(f, w, Atom(ctx.GoOp>>abi.GoOpIndexShift), recv, jitGoArg(ctx, 1))
		s.ssaCtx = last
		if err == nil && op != bytecode.RSetProp {
			// A read native code did not find, learned as at an exit
			// (jitRegReadSeen).
			r.jitRegReadSeen(f, c, run, rpc, recv)
		}
		if s.ssaSharedGen != shared {
			r.jitShareContexts(idx, f)
		}
		if err != nil || r.stopped != nil || run.e.invalid || run.leave {
			s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: v, err: err}
			return false
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
		return true
	case op == bytecode.RInstanceOf && kind == abi.GoCallInstanceOf:
		// As rsInstanceOf makes it, by the site's cache: it may run code.
		aux := int(c.rc.Aux[rpc])
		if aux >= len(f.cl.ic) {
			return false
		}
		f.pc = uint32(rpc) + 1
		last, shared := s.ssaCtx, s.ssaSharedGen
		s.goCalls++
		yes, err := r.instanceOfAt(&f.cl.ic[aux], jitGoArg(ctx, 0), jitGoArg(ctx, 1))
		s.ssaCtx = last
		if s.ssaSharedGen != shared {
			r.jitShareContexts(idx, f)
		}
		if err != nil || r.stopped != nil || run.e.invalid || run.leave {
			s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: Bool(yes), err: err}
			return false
		}
		*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Bool(yes)
		return true
	default:
		return false
	}
	if int(ctx.GoOp>>abi.GoOpArgcShift&0xff) != argc {
		return false
	}
	if idx == len(s.ssaCtxs)-1 && s.bounds.LevelLimit == uint64(len(s.ssaCtxs)) {
		// A call native code made by Go for want of a context past this one
		// (mir's native calls): more of them, for the calls after it to be
		// native -- a recursion of native calls had gone through Go every
		// fourth level.
		r.jitGrowContexts()
	}
	top := r.stackTop
	end := top + 2 + argc
	if end > len(r.stack) {
		return false
	}
	at := 0
	this := Undefined
	var callee, first Value
	if jitRegWideCall(w) {
		// More operands than the context holds: native code wrote them to
		// the callee's window past the stack's top (mir's windowGoCall),
		// the frame the call is made of.
		if op.MethodCall() {
			this = r.stack[top+1]
		}
		callee, first = r.stack[top], r.stack[top+2]
		r.stack[top+1] = this
	} else {
		if op.MethodCall() {
			this, at = jitGoArg(ctx, 0), 1
		}
		callee = jitGoArg(ctx, at)
		if argc != 0 {
			first = jitGoArg(ctx, at+1)
		}
		r.stack[top] = callee
		r.stack[top+1] = this
		for i := range argc {
			r.stack[top+2+i] = jitGoArg(ctx, at+1+i)
		}
	}
	r.noteRegGoCall(w, false, callee)
	r.stackTop, r.stackHigh = end, max(r.stackHigh, end)
	s.goCalls++
	f.pc = uint32(rpc) + 1
	last, shared := s.ssaCtx, s.ssaSharedGen
	var v Value
	var err error
	if op == bytecode.RNewW {
		v, err = r.regConstruct(nil, 0, r.stack[top], top, argc)
	} else if op == bytecode.RLit {
		// As regLit calls it: the RegExp given back where the built-in
		// called keeps nothing of it (RMarkLitRecv).
		m := uint8(bytecode.RMarkLitRecv)
		if w.X()&bytecode.RLitDead != 0 {
			m |= bytecode.RMarkDead
		}
		v, err = r.regMarkedCall(f, c, top, 1, rpc, m, uint32(w.C()))
	} else if op == bytecode.RCallMW && rpc < len(c.rc.Marks) && c.rc.Marks[rpc]&bytecode.RMarkLitArg != 0 {
		// A call that makes its first argument a RegExp literal's
		// (RMarkLitArg), which native code made from the literal's pool:
		// called with it, and the RegExp given back to the pool where the
		// built-in called keeps nothing of it (jitRegExpLitDone) -- not the
		// literal made again in Go (regMarkedCall).
		v, err = r.jitRegLitArgMade(f, run.e, rpc, c.rc.Marks[rpc], callee, this, r.stack[top+2:end])
	} else if o, fd := r.jitRegBuiltin(f, c, w, rpc, callee); fd != nil {
		// A built-in of this realm, called directly, as rhCallW's site
		// calls its own (regNative): regCallSlow's search of the site's
		// cache, its marks, the closures' and the generic call's, had been
		// most of what such a call cost -- JSON.parse's, Object.create's.
		v, err = r.jitRegNativeMade(o, fd, this, r.stack[top+2:end])
	} else {
		v, err = r.regCallSlow(f, c, w, top, rpc)
		if err == errRegRestart {
			// The callee's frame, entered by regEnterW, given to a callee
			// in place by a call in tail position (regTailTo): run again,
			// the frame past the top as regEnterW left it, as regMissEnd
			// runs it. Left as an error, it unwound every frame to the
			// first that runs one again, which ran the callee's code on
			// its own registers.
			u := uint(r.frameDepth)
			v, err = r.regRerun(&r.frames[u/frameBlockSize][u%frameBlockSize], regFrameEnter, r.stackTop)
		}
		if err == errTailCall {
			// The callee's frame ended in a tail call.
			v, err = r.regTailRest()
		}
	}
	jitClearArgs(r.stack, top, end)
	r.stackTop = top
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		r.jitShareContexts(idx, f)
	}
	if err == nil && r.stopped == nil && jitRegCallUnsettled(run.e, rpc, op) {
		// What the call called, which the code may call natively once
		// compiled again (jitRegCallSeen), or make a loop of its own
		// (jitRegForEachSeen): asked while the call is unsettled
		// (jitRegCallSettle).
		if !r.jitRegViaCall(op, callee, this) {
			// G.call(x, ...) of a script function G keeps the call
			// unsettled, as a call of a function with code does: its G
			// called natively once compiled (jitRegViaSeen), which a call
			// settled as the built-in's never learned -- a parent whose
			// compile was put off, called by Go at every construction.
			jitRegCallSettle(run.e, c, rpc, callee)
		}
		if op == bytecode.RNewW && callee.IsObject() && callee.Object() == r.proto.arrayCtor {
			// new Array(n), its length the call's operand (jitRegAllocLength).
			n := Undefined
			if argc == 1 {
				n = first
			}
			r.jitRegAllocLength(f, run.e, rpc, w, callee.Object(), n)
		} else {
			r.jitRegCallSeen(f, c, run.e, rpc, callee)
			if op == bytecode.RCallW || op == bytecode.RCallMW {
				// A generator function's, whose generators the code may make
				// (jitRegGenMakeSeen).
				r.jitRegGenMakeSeen(f, run.e, rpc, callee)
			}
		}
		if op == bytecode.RCallMW {
			// G.call(x, ...), G native callers may call (jitRegViaSeen).
			r.jitRegViaSeen(f, run.e, rpc, callee, this, argc)
			r.jitRegForEachSeen(f, run.e, rpc, callee, this, first, argc)
			// A generator's next, which the code may call natively
			// (jitRegGenCallSeen).
			r.jitRegGenCallSeen(f, run.e, rpc, this, callee, argc)
		}
	}
	if err != nil && r.stopped == nil && op == bytecode.RCallMW && !run.e.invalid && jitRegCallUnsettled(run.e, rpc, op) {
		// A loop's built-in's call whose callback threw, learned all the
		// same.
		r.jitRegForEachSeen(f, run.e, rpc, callee, this, first, argc)
	}
	if err != nil || r.stopped != nil || run.e.invalid || run.leave || r.jitRegCompileDueNow(&run, idx) {
		s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: v, err: err}
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitRegLitArgMade makes, for native code of e, the call at register pc
// rpc of f's register code, marked m, of callee on this, which makes its
// first argument, args[0], the RegExp of its literal (RMarkLitArg) --
// native code took it from the literal's pool (lowerreg's literal before
// the call) -- as regMarkedCall makes it once it has the literal: a dead
// replace's told so (regDeadCall); the RegExp given back to the pool where
// the call was the realm's replace, split or match searching with it
// directly (regexpArgCall), which keeps nothing of it.
func (r *Runtime) jitRegLitArgMade(f *frame, e *jitRegEntry, rpc int, m uint8, callee, this Value, args []Value) (Value, error) {
	if !callee.IsObject() {
		return r.regCall(callee, this, args)
	}
	o := callee.Object()
	var lit *jitLiteral
	if len(args) != 0 && e != nil && e.life != nil && r.regexpArgCall(o, this, args) {
		for i := range e.life.literals {
			if l := &e.life.literals[i]; int(l.pc) == rpc && l.fn == f.cl.fn && l.lit != nil && l.pool != nil {
				lit = l
				break
			}
		}
	}
	var rx Value
	if lit != nil {
		rx = args[0]
	}
	var v Value
	var err error
	if m&bytecode.RMarkDead != 0 && o == r.strReplaceFn {
		v, err = r.regDeadCall(o, this, args)
	} else {
		v, err = r.regMarkedRest(o, this, args)
	}
	if err == nil && lit != nil {
		r.jitRegExpLitDone(lit, rx)
	}
	return v, err
}

// jitRegBuiltin is the built-in a call of Go of register instruction w at
// register pc rpc of c calls, callee, which jitRegNativeMade calls
// directly: one of the runtime's realm with a native body, neither bound,
// nor Function.prototype.call, nor one of Math's, which mathCall answers,
// nor one the call's marks have regMarkedCall make (regMarked). The site's
// cache learns it as regCallSlow has it learn one (regCallNative), for the
// interpreter's calls. Otherwise nil.
func (r *Runtime) jitRegBuiltin(f *frame, c *regCode, w bytecode.RInstr, rpc int, callee Value) (*Object, *funcData) {
	switch w.Op() {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT:
	default:
		return nil, nil
	}
	if !callee.IsObject() {
		return nil, nil
	}
	o := callee.Object()
	fd := o.fn()
	if fd == nil || fd.native == nil || fd.mathOp != 0 || fd.bound || o == r.callFn || fd.realm != nil && fd.realm != r.Realm {
		return nil, nil
	}
	if w.Op().MethodCall() && rpc < len(c.rc.Marks) {
		if m := c.rc.Marks[rpc]; m != 0 && r.regMarked(c, o, rpc, m) {
			return nil, nil
		}
	}
	if calls := f.cl.regCalls; uint(w.C()) < uint(len(calls)) {
		if e := &calls[w.C()]; e.callee != o || e.kind != regCallNative {
			if !f.cl.regCallsHeld {
				r.regCallsHold(f.cl.closureCode)
			}
			*e = regCallCache{callee: o, kind: regCallNative}
		}
	}
	return o, fd
}

// jitRegNativeMade calls fd, o's native body, on this with args: in a frame
// of its own (nativeFrame), the interrupt checked, its result charged to
// the meter.
func (r *Runtime) jitRegNativeMade(o *Object, fd *funcData, this Value, args []Value) (Value, error) {
	if err := r.tick(); err != nil {
		return Undefined, err
	}
	if err := r.nativeFrame(o, fd, this, args, Undefined); err != nil {
		return Undefined, err
	}
	v, err := fd.native(r, this, args)
	r.frameDepth--
	if r.meter != nil && err == nil {
		err = r.chargeResult(v)
	}
	return v, err
}

// jitRegStepMade makes, for native code, the for...of loop's step at
// register pc rpc of run's code -- its cursor st, of an iterator whose next
// is no generator's, which the step calls: the step itself (regStepNext, as
// iter_next steps such a cursor), the next called as register code calls
// it, and its value, or, the cursor done, the uninitialized value, to the
// call's keep cell, which the code takes for the loop's end. Run whole
// (jitRegWhole) instead, as the interpreter's instruction on the frame, its
// registers saved and put back around it, each step had cost a third more
// than the call of next it made: a for...of over an iterator written in
// script, microbench_extra's iterator_for_of.
func (r *Runtime) jitRegStepMade(ctx *abi.Context, idx int, run jitRegRun, rpc int, st *iterState) bool {
	s := r.jit
	f, c := run.f, run.c
	w := regAt(c.code, rpc)
	r.noteRegGoCall(w, false, Value{})
	s.goCalls++
	f.pc = uint32(rpc) + 1
	last, shared := s.ssaCtx, s.ssaSharedGen
	var v Value
	var more bool
	var err error
	if jitScriptNext(st) {
		// A next written in script, which the code calls natively once
		// compiled again (jitRegStepNextSeen), and its result's layout.
		res, cerr := r.regCall(st.next, st.iter, nil)
		if cerr == nil && r.stopped == nil {
			r.jitRegStepNextSeen(f, run.e, rpc, st, res)
		}
		v, more, err = r.regStepOf(st, res, cerr)
	} else {
		v, more, err = r.regStepNext(st)
	}
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		r.jitShareContexts(idx, f)
	}
	if err == nil && !more {
		// Done: the loop's end (regFinishCall's).
		v = uninitialized
	}
	if err != nil || r.stopped != nil || run.e.invalid || run.leave || r.jitRegCompileDueNow(&run, idx) {
		s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: v, err: err}
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitScriptNext reports whether st's iterator's next, which a loop's step
// calls (regStepNext), is written in script.
func jitScriptNext(st *iterState) bool {
	if !st.next.IsObject() {
		return false
	}
	fd := st.next.Object().fn()
	return fd != nil && fd.closure != nil && fd.native == nil
}

// jitStepResult is the layout of the results of the iterator's next of a
// loop's step at register pc pc (jitLearned's stepResults): its shape, and
// where its own data properties done and value are in it.
type jitStepResult struct {
	pc          int32
	shape       *shape
	done, value int32
}

// jitRegStepNextSeen notes the step at register pc rpc of f's code e,
// which Go made of st, whose iterator's next is written in script and
// returned res: the next, which the code calls natively once compiled
// again (regStepSites), as a call learns its callee (jitRegCallSeen), and
// the layout of res where its done and value are its own data properties,
// which the code reads natively (ssa.CallSite's StepResult).
func (r *Runtime) jitRegStepNextSeen(f *frame, e *jitRegEntry, rpc int, st *iterState, res Value) {
	if e == nil || e.invalid {
		return
	}
	o := st.next.Object()
	cl := o.fn().closure
	if cl.reg == nil || !r.jitRegCallable(o, cl, cl.reg, 0) {
		return
	}
	l := r.jitRegLearnedFor(f, e)
	if l == nil {
		return
	}
	learned := false
	if !slices.ContainsFunc(l.nativeCalls, func(x jitInline) bool { return int(x.pc) == rpc }) {
		l.nativeCalls = append(l.nativeCalls, jitInline{pc: int32(rpc), cl: cl, obj: o})
		r.jitRegKnownCallee(cl, f.cl)
		r.jitState().regLinkIfCached(cl)
		learned = true
	}
	if res.IsObject() && !slices.ContainsFunc(l.stepResults, func(x jitStepResult) bool { return int(x.pc) == rpc }) {
		if p := res.Object(); p.shape != nil && p.class == ClassObject {
			done, value := p.findOwn(atomDone), p.findOwn(atomValue)
			if keys := p.keys(); done >= 0 && value >= 0 && keys[done].flags&propAccessor == 0 && keys[value].flags&propAccessor == 0 {
				l.stepResults = append(l.stepResults, jitStepResult{pc: int32(rpc), shape: remember(p.shape), done: done, value: value})
				learned = true
			}
		}
	}
	if learned {
		r.jitRegReopt(f.cl, e)
	}
}

// regStepSites are what NativeCalls answers for a loop's step at the
// program's instruction pc, iter_next's, that has met an iterator written
// in script (jitRegStepNextSeen): a native call of its next, of the
// iterator and no argument, as a method call's is made (regCallSite), and
// the call of Go of the step's answer (ssa.CallSite's StepResult), which
// native code makes itself of a result of the layout the step learned --
// or none, where the step has met none, or the next is no longer one
// native callers call.
func (f *jitRegFeedback) regStepSites(pc int) []ssa.CallSite {
	l := f.fb.e
	if l == nil {
		return nil
	}
	rpc := int32(f.p.Origin[pc])
	var sites []ssa.CallSite
	for _, x := range l.nativeCalls {
		if x.pc != rpc || x.pool != nil || x.cl == nil || x.cl.reg == nil {
			continue
		}
		if site, ok := f.regCallSite(pc, x.obj, x.cl, x.cl.reg, 0, true, nil, nil, false, false); ok {
			sites = append(sites, site)
		}
		break
	}
	if len(sites) == 0 {
		return nil
	}
	res := ssa.CallSite{Go: ssa.GoCall, StepResult: true, ThisSlot: -1}
	for _, x := range l.stepResults {
		if x.pc == rpc {
			res.ResultShape, res.DoneAt, res.ValueAt = uintptr(unsafe.Pointer(x.shape)), int(x.done), int(x.value)
			k := f.fb.keep()
			k.shapes = append(k.shapes, x.shape)
			break
		}
	}
	return append(sites, res)
}

// jitRegStepCallMade makes, for native code, what a loop's step at
// register pc rpc of run's code, iter_next's, that calls its iterator's
// next natively (regStepSites) leaves to Go: the call of the next, of the
// operands' receiver and function, where native code did not make it (a
// context past the deepest, a receiver to coerce), its result the call's
// keep cell's (kind abi.GoCallMethod); or the iterator protocol on the
// result it made, of the operands' cursor and result (abi.GoCallStepResult,
// regStepOf), the step's value the keep cell's. Either goes on in native
// code; what leaves -- a throw, the runtime stopped, the code found
// invalid -- leaves with the step's answer pending (regPending, a next's
// result made the answer once the frame is written, jitRegStepCalled), for
// regFinishCall to finish the step with, the next never called again.
func (r *Runtime) jitRegStepCallMade(ctx *abi.Context, idx int, run jitRegRun, rpc int, kind uint64) bool {
	s := r.jit
	f, c := run.f, run.c
	w := regAt(c.code, rpc)
	var st *iterState
	if kind == abi.GoCallStepResult {
		if st = cursorOf(jitGoArg(ctx, 0)); st == nil {
			return false
		}
	}
	r.noteRegGoCall(w, false, Value{})
	s.goCalls++
	f.pc = uint32(rpc) + 1
	last, shared := s.ssaCtx, s.ssaSharedGen
	var v Value
	var err error
	if st == nil {
		callee := jitGoArg(ctx, 1)
		v, err = r.regCall(callee, jitGoArg(ctx, 0), nil)
		if fd := jitFnOf(callee); err == nil && fd != nil && fd.closure != nil && fd.closure.reg != nil {
			// A next with no code native callers call: compiled for them
			// (jitRegKnownCallee), as a call's known callee is.
			r.jitRegKnownCallee(fd.closure, f.cl)
		}
	} else {
		v, err = r.jitRegStepAnswer(st, jitGoArg(ctx, 1), nil)
	}
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		r.jitShareContexts(idx, f)
	}
	if err != nil || r.stopped != nil || run.e.invalid || run.leave || r.jitRegCompileDueNow(&run, idx) {
		s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: v, err: err, step: st == nil}
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitFnOf is the function data of v, a function object, or nil.
func jitFnOf(v Value) *funcData {
	if !v.IsObject() {
		return nil
	}
	return v.Object().fn()
}

// jitRegStepAnswer is the value a loop's step of st answers, or what it
// throws, of res, what its iterator's next native code called returned,
// or err, what it threw (regStepOf): the uninitialized value once done,
// which regFinishCall takes for the loop's end.
func (r *Runtime) jitRegStepAnswer(st *iterState, res Value, err error) (Value, error) {
	v, more, err := r.regStepOf(st, res, err)
	if err == nil && !more {
		v = uninitialized
	}
	return v, err
}

// jitRegStepCalled is v and err, what a native call made at register pc
// rpc of c, on the frame whose window is at wb, written, returned or
// threw, as the call's instruction takes it (jitGoPending's step): for a
// loop's step whose next native code called (regStepSites), the step's
// answer (jitRegStepAnswer) -- Go finished the next, which left native
// code, or made it, and finishes the step from it, the iterator's next
// called no more.
func (r *Runtime) jitRegStepCalled(f *frame, c *regCode, wb, rpc int, v Value, err error) (Value, error) {
	if rpc >= len(c.rc.Code) {
		return v, err
	}
	w := regAt(c.code, rpc)
	if w.Op() != bytecode.RIterNext {
		return v, err
	}
	st := cursorOf(r.stack[wb+int(w.B())])
	if st == nil {
		return v, err
	}
	return r.jitRegStepAnswer(st, v, err)
}

// jitRegViaCall reports whether a call op of callee on this is G.call(x,
// ...) through the realm's Function.prototype.call of G, this, a function
// written in script.
func (r *Runtime) jitRegViaCall(op bytecode.ROp, callee, this Value) bool {
	if op != bytecode.RCallMW || r.callFn == nil || !callee.IsObject() || callee.Object() != r.callFn || !this.IsObject() {
		return false
	}
	fd := this.Object().fn()
	return fd != nil && fd.closure != nil && fd.native == nil
}

// jitRegCallUnsettled reports whether the learning of a call of Go
// (jitRegCallSeen and the rest) has anything to note of the call op at
// register pc rpc of e's code: not once the call is settled as one Go makes
// (jitCallDone, jitRegCallSettle), unless it constructs -- a pool to fill.
// Most calls of Go are such, a built-in's: asked here, they cost no
// learning.
func jitRegCallUnsettled(e *jitRegEntry, rpc int, op bytecode.ROp) bool {
	if op == bytecode.RNewW || e == nil || e.life == nil {
		return true
	}
	sites := e.life.callSites
	return uint(rpc) >= uint(len(sites)) || sites[rpc] != jitCallDone
}

// jitRegCallSettle counts a call of Go of callee at register pc rpc of c,
// e's code, toward its settling as one Go makes (jitCallDone): after
// jitCallsToInline calls of built-ins, which the learning has seen each of,
// it is settled. A call of a function with code keeps it unsettled: the
// code calls it natively once it has learned it (jitRegCallSeen), and
// refills its generators' pool there (jitRegGenMakeSeen).
func jitRegCallSettle(e *jitRegEntry, c *regCode, rpc int, callee Value) {
	if e == nil || e.life == nil || rpc >= len(c.rc.Code) || !callee.IsObject() {
		return
	}
	if fd := callee.Object().fn(); fd != nil && fd.native == nil {
		return
	}
	l := e.life
	if len(l.callSites) != len(c.rc.Code) {
		l.callSites = make([]uint8, len(c.rc.Code))
	}
	if l.callSites[rpc] == jitCallNative {
		// Settled on the functions it calls natively (jitRegCallSettling),
		// whose learning goes on.
		return
	}
	if l.callSites[rpc]++; l.callSites[rpc] >= jitCallsToInline {
		l.callSites[rpc] = jitCallDone
	}
}

// jitRegWhole runs, for native code, register code's instruction at
// register pc rpc whole on run's frame (abi.GoCallWhole, ir's Whole), as
// an exit there would have Go run it (regWhole), not leaving native code:
// the registers it reads given the values the code passed (abi.Context's
// GoArgs), its result, the register it writes, to the call's keep cell.
// The code keeps values in the frame's registers as it found them, so
// what the call wrote there is put back -- but a captured register of the
// run's own frame, which is where its cell holds it. What does not go on
// at the next instruction -- a throw, a handler that took one, the
// runtime stopped, the code found invalid -- Go finishes at the exit the
// code then takes (regPending, regFinishWhole): the register the
// instruction wrote, or the handler's, written over the exit's records,
// the instruction not run again. On a native callee's frame (a level),
// what reads the frame's this is left to the exit.
func (r *Runtime) jitRegWhole(ctx *abi.Context, idx int, run jitRegRun, rpc int) bool {
	s := r.jit
	f, c, e, wb := run.f, run.c, run.e, run.wb
	if e == nil || e.p == nil {
		return false
	}
	ipc := e.ipc(rpc)
	if ipc < 0 || ipc+1 >= len(e.p.Code) {
		return false
	}
	if e.p.Origin[ipc+1] == e.p.Origin[ipc] || ipc > 0 && e.p.Origin[ipc-1] == e.p.Origin[ipc] {
		return false
	}
	ops, argc, dest, ok := e.p.Code[ipc].WholeOperands(e.p.Regs)
	whole := ctx.GoOp>>abi.GoOpKindShift&0xff == abi.GoCallWhole
	// A for...of's or a for...in's step (iter_next, ir's IterStep): its
	// cursor's register read, its value's written, as the interpreter's
	// step makes it -- the uninitialized value once done, where it jumps to
	// the loop's end. Left to an exit, Bounce's for...of left native code
	// at its steps.
	step := false
	if in := &e.p.Code[ipc]; !ok && !whole && in.Op == ir.IterStep && !in.When && !in.Rest &&
		in.Left.Slot >= 0 && in.Left.Slot < e.p.Regs && in.Dest >= 0 && in.Dest < e.p.Regs {
		ops[0], argc, dest, ok, step = in.Left.Slot, 1, in.Dest, true, true
	}
	if !ok || whole && int(ctx.GoOp>>abi.GoOpArgcShift&0xff) != argc || wb+e.p.Regs > len(r.stack) {
		return false
	}
	own := ctx.Live == 0
	switch op := regAt(c.code, rpc).Op(); {
	case !own && (op == bytecode.RLoadThis || op == bytecode.RThis || op == bytecode.RDerivedRet):
		// What reads a this binding, which a native callee's stand-in
		// frame has none of.
		return false
	}
	after := int(e.p.Origin[ipc+1])
	h := c.rc.HandlerAt(rpc)
	if h != nil && int(h.To) == after {
		// A handler at the next instruction, where what threw and what did
		// not would go on alike.
		return false
	}
	if own && ctx.Cells != nil {
		// Code whose captured registers are in cells of their own: a cell
		// native code gave a register at a scope's end (mir's cellsClose)
		// is open on no register, which holds what the register held at
		// the scope's start, not what the cell holds now. The instruction
		// reads and writes the frame's registers, as the interpreter's
		// does: the cells open on them again first, holding what they
		// hold, as Go has them before it runs the frame at an exit
		// (jitRegReopenCells) -- read from the register, a let of a loop's
		// turn had been the one the frame held when native code entered.
		r.jitRegReopenCells(f, e, wb, ctx, true)
	}
	var regs [4]int
	var saved [4]Value
	n := 0
	for i := 0; i <= argc; i++ {
		reg := dest
		if i < argc {
			reg = ops[i]
		}
		if reg < 0 || own && e.p.IsCaptured(reg) || slices.Contains(regs[:n], reg) {
			continue
		}
		regs[n], saved[n] = reg, r.stack[wb+reg]
		n++
	}
	for i := range argc {
		if reg := ops[i]; !own || !e.p.IsCaptured(reg) {
			r.stack[wb+reg] = jitGoArg(ctx, i)
		}
	}
	s.goCalls++
	r.noteRegGoCall(regAt(c.code, rpc), true, Value{})
	last, shared := s.ssaCtx, s.ssaSharedGen
	next, _, err := r.regWhole(f, c, wb, rpc)
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		r.jitShareContexts(idx, f)
	}
	var v Value
	if dest >= 0 {
		v = r.stack[wb+dest]
	}
	done := false
	if w := regAt(c.code, rpc); step && err == nil && next != after && next == rpc+1+int(w.SC()) {
		// Done: the loop's end is next, its register as it was.
		v, done = uninitialized, true
	}
	if err != nil || next != after && !done || r.stopped != nil || e.invalid || run.leave {
		// Left at the call's exit, whose records write the frame as it was
		// before the instruction: then the register it wrote, or the
		// handler's that took what it threw.
		p := jitGoPending{set: true, pc: rpc, ctx: idx, whole: true, next: next, err: err, reg: -1}
		switch {
		case err != nil:
		case h != nil && next == int(h.To):
			p.reg, p.v = int(h.Reg), r.stack[wb+int(h.Reg)]
		case done:
			// The loop's end, nothing written.
		default:
			p.reg, p.v = dest, v
		}
		s.regPending = p
		s.wholeLeft++
		return false
	}
	for i := range n {
		r.stack[wb+regs[i]] = saved[i]
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// jitRegOpMade makes, for native code, register code's instruction w at
// register pc rpc on its operands, the values the code passed
// (abi.Context's GoArgs), with run's frame: new_class's link of a class to
// its parent, which throws for one that is not a constructor, and
// copy_data_props's spread that jitRegFrameless did not make, which may run
// a getter. Neither writes a register, so nothing in the frame's registers
// is saved and put back, as jitRegWhole does. What does not go on at the
// next instruction Go finishes at the exit native code then takes
// (regPending): a throw as a call's is (regFinishCall), to the frame's
// handler; past the instruction where the runtime stopped or the code was
// found invalid.
func (r *Runtime) jitRegOpMade(ctx *abi.Context, idx int, run jitRegRun, rpc int, w bytecode.RInstr) bool {
	s := r.jit
	f := run.f
	if run.e == nil || int(ctx.GoOp>>abi.GoOpArgcShift&0xff) != 2 {
		return false
	}
	a, b := jitGoArg(ctx, 0), jitGoArg(ctx, 1)
	f.pc = uint32(rpc) + 1
	s.goCalls++
	r.noteRegGoCall(w, true, Value{})
	last, shared := s.ssaCtx, s.ssaSharedGen
	var err error
	if w.Op() == bytecode.RNewClass {
		err = r.linkClass(a, b)
	} else if a.IsObject() && !b.IsNullish() {
		err = r.copyDataProps(a.Object(), b)
	}
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		r.jitShareContexts(idx, f)
	}
	if err != nil {
		// Thrown: to the frame's handler, as a call's throw goes
		// (regFinishCall's regCatch).
		s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, err: err}
		return false
	}
	if r.stopped != nil || run.e.invalid || run.leave {
		s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, whole: true, next: rpc + 1, reg: -1}
		s.wholeLeft++
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = Undefined
	return true
}

// jitRegSuperMade is the call of Go native code made from a derived
// constructor's register code makes at its super_call, w, at register pc
// rpc (ssa.CallSite's Super, GoCall), on run's frame -- the run's own, or a
// level's Go made (jitRegPushLevel): its operands the this slot, the
// binding, and the arguments; the parent constructed by superCall, which
// binds the frame's this, the this it bound the result, and the context's
// This, which a native super() binds. The parent it constructed with is
// learned (jitRegSuperSeen). What threw, or after which the code is
// invalid, Go finishes at the exit native code then takes (regPending).
func (r *Runtime) jitRegSuperMade(ctx *abi.Context, idx int, run jitRegRun, rpc int, w bytecode.RInstr) bool {
	s := r.jit
	f, c := run.f, run.c
	argc := int(w.B())
	if int(ctx.GoOp>>abi.GoOpArgcShift&0xff) != argc || f.thisRef == nil || f.callee == nil || f.cl.fn.Kind != bytecode.KindDerivedConstructor {
		return false
	}
	jitBindThis(f, ctx)
	if this := jitGoArg(ctx, 0); !this.IsUninitialized() && !f.thisRef.init {
		// Bound by a super() native code made, which this one finds bound.
		f.this, f.thisRef.value, f.thisRef.init = this, this, true
	}
	r.jitRegSuperSeen(f, c, run.e, rpc)
	args := make([]Value, argc)
	for i := range args {
		args[i] = jitGoArg(ctx, 1+i)
	}
	f.pc = uint32(rpc) + 1
	last, shared := s.ssaCtx, s.ssaSharedGen
	s.goCalls++
	r.noteRegGoCall(w, false, Value{})
	err := r.jitSuperCall(f, args)
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		r.jitShareContexts(idx, f)
	}
	v := f.this
	if err != nil || r.stopped != nil || run.e.invalid || run.leave || r.jitRegCompileDueNow(&run, idx) {
		s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: v, err: err}
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.This)) = v
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// regFinishWhole finishes, on the registers of a window at wb, an
// instruction Go ran whole for native code (jitRegWhole) that did not go
// on natively, as p says: the register it wrote written over what the
// exit's records wrote, or its exception, which no handler of the frame's
// took, thrown. It reports as regWhole does.
func (r *Runtime) regFinishWhole(wb int, p jitGoPending) (next int, ok bool, _ error) {
	if p.err != nil {
		return 0, true, p.err
	}
	if p.reg >= 0 {
		r.stack[wb+p.reg] = p.v
	}
	return p.next, true, nil
}

// jitRegPropertyMade makes, for native code, the property read or write w
// of f's register code, of key, on obj -- of val, a write -- as the
// interpreter makes it, through the site's cache: a getter or a setter is
// called, with obj as its this. A write's result is undefined.
func (r *Runtime) jitRegPropertyMade(f *frame, w bytecode.RInstr, key Atom, obj, val Value) (Value, error) {
	var c *propCache
	if site := int(w.C()); site < len(f.cl.ic) {
		c = &f.cl.ic[site]
	}
	if w.Op() == bytecode.RSetProp {
		if obj.IsObject() && c != nil {
			return Undefined, r.setPropCached(c, obj.Object(), key, val, f.cl.fn.Strict)
		}
		return Undefined, r.setValueProp(obj, key, val, f.cl.fn.Strict)
	}
	if obj.IsObject() && c != nil {
		o := obj.Object()
		if v, ok := plainOwn(o, key); ok {
			return v, nil
		}
		if v, ok, err := r.cachedGet(c, o, key); ok {
			return v, err
		}
	}
	return r.getValueProp(obj, key)
}

// jitRegIterMade makes a destructuring's step or rest element, at register
// pc rpc of run's frame, of the cursor native code passed, as the
// interpreter's iter_step and iter_rest do (regIterStep, regIterRest): the
// cursor's next value, undefined once it is done, or the array of what it
// has left, to the call's keep cell. It had run the instruction whole
// (jitRegWhole), its registers saved and put back, and a step of a cursor
// native code makes none of (abi.GoCallFill) had left native code. What the
// iterator's next throws, and what else does not go on after the call, Go
// finishes at the call's exit (regPending, regFinishCall), as a getter's.
func (r *Runtime) jitRegIterMade(ctx *abi.Context, idx int, run jitRegRun, rpc int, w bytecode.RInstr) bool {
	s := r.jit
	f := run.f
	f.pc = uint32(rpc) + 1
	last, shared := s.ssaCtx, s.ssaSharedGen
	s.goCalls++
	r.noteRegGoCall(w, false, Value{})
	var v Value
	var err error
	if w.Op() == bytecode.RIterRest {
		v, err = r.iterRest(jitGoArg(ctx, 0))
	} else {
		v, err = r.iterStep(jitGoArg(ctx, 0))
	}
	s.ssaCtx = last
	if s.ssaSharedGen != shared {
		r.jitShareContexts(idx, f)
	}
	if err != nil || r.stopped != nil || run.e.invalid || run.leave || r.jitRegCompileDueNow(&run, idx) {
		s.regPending = jitGoPending{set: true, pc: rpc, ctx: idx, v: v, err: err}
		return false
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[ctx.GoKeep])) = v
	return true
}

// regFinishCall finishes, on f's registers, a window at wb, the call at
// register pc rpc of c that Go made for native code (jitRegGoCall) with v
// or err as its answer, as rhCallW finishes one: its value to its register,
// its exception to the frame's handlers (regCatch). It reports as regWhole
// does.
func (r *Runtime) regFinishCall(f *frame, c *regCode, wb, rpc int, v Value, err error) (next int, ok bool, _ error) {
	w := regAt(c.code, rpc)
	k := regKept{pc: rpc + 1}
	if err == nil {
		dest := int(w.B())
		switch w.Op() {
		case bytecode.RInstanceOf, bytecode.RTypeof, bytecode.RGetProp, bytecode.RGetMethod, bytecode.RApplyArgs,
			bytecode.RIterStep, bytecode.RIterRest:
			// R[A]'s: a getter's, called for get_prop (an accessor
			// inlined, jitRegInlined), as typeof's and instanceof's, and a
			// destructuring's step's and rest's (jitRegIterMade).
			dest = int(w.A())
		case bytecode.RGetMethodW:
			// R[A]'s, and the receiver to R[A+1], as get_method_w puts
			// them.
			dest = int(w.A())
			r.stack[wb+dest+1] = r.stack[wb+int(w.B())]
		case bytecode.RSetProp:
			// A setter's result, dropped.
			return k.pc, true, nil
		case bytecode.RIterNext:
			// A loop's step whose generator Go finished (jitRegGenDone):
			// done, the uninitialized value, to the loop's end, as
			// regIterNext goes there; its value to R[A] otherwise.
			if v.IsUninitialized() {
				return rpc + 1 + int(w.SC()), true, nil
			}
			dest = int(w.A())
		case bytecode.RSuperCall:
			// The parent a super() native code made returned what Go
			// finished it with (jitRegUnwind): the frame's this bound to
			// it if an object, else to the instance (jitSuperBound) --
			// bound already where Go made the super() -- R[A]'s, and a
			// derived constructor's register 1's, as super_call writes
			// them.
			v = jitSuperBound(f, v)
			dest = int(w.A())
			if f.cl.fn.Kind == bytecode.KindDerivedConstructor {
				r.stack[wb+1] = v
			}
		}
		r.stack[wb+dest] = v
		return k.pc, true, nil
	}
	h := r.regCatch(f, c, &k, regWindow(r.stack, wb), err)
	if h < 0 {
		return 0, true, err
	}
	return h, true, nil
}

// RegNativeResult is how a call's native run went (RegNativeCall).
type RegNativeResult struct {
	// Value and Err are what the call answered.
	Value Value
	Err   error
	// Skipped says why the call was made as an ordinary call: no register
	// frame, a refusal, no memory for the code.
	Skipped string
	// Compiled says the code was compiled for this call; Entered, at
	// which register pc native code was first entered, -1 for none;
	// OSR that it was a loop's header the evaluator had reached.
	Compiled bool
	Entered  int
	OSR      bool
}

// RegNativeStats are the runtime's counts of what native code made from
// register code did, for tests: its exits by kind, the invocations the
// interpreter finished, the records Go applied, the objects it made.
type RegNativeStats struct {
	Entries, Hosts, Guards, Polls, Interpreted, Records, Materialized uint64
	// GoCalls counts the calls of Go native code made that ran script, a
	// call's on the register frame among them (jitRegGoCall).
	GoCalls uint64
	// Unwound counts the native calls' callees Go finished after one of
	// them left native code (jitRegUnwind), and Resumed the native callers
	// that went on natively after such a callee.
	Unwound, Resumed uint64
	// HostOps is Hosts by the register instruction Go ran whole.
	HostOps map[string]uint64
	// GoCallOps is the calls of Go native code made that may run script,
	// by the register instruction and what it called -- a built-in by its
	// name, a function as "function" or, where it had code native callers
	// may call, "function with code" -- counted while the runtime reports
	// (SetJITReport).
	GoCallOps map[string]uint64
	// WholeLeft counts the instructions Go ran whole for native code,
	// called from it, that it finished at an exit (jitRegWhole).
	WholeLeft uint64
	// NestedGoCalls counts the calls of Go from native callees' code
	// (jitRegGoCallNested); Levels the levels of native calls whose code
	// Go found and pinned (jitRegLevelAt) -- a call's calling level, and
	// every level it unwinds -- and LevelsMade the times Go made the
	// frames of levels a call of Go had put off, one of them read
	// (jitRegMakeLazy), of LevelsPutOff the calls put off in all.
	NestedGoCalls, Levels, LevelsMade, LevelsPutOff uint64
	// Ended is Interpreted by why the run ended in the interpreter, the
	// register instruction it left at and its function, counted while
	// the runtime reports (SetJITReport).
	Ended map[string]uint64
}

// RegNativeStats reports r's counts (RegNativeStats); zero before any.
func (r *Runtime) RegNativeStats() RegNativeStats {
	s := r.jit
	if s == nil {
		return RegNativeStats{}
	}
	return RegNativeStats{Entries: s.ssaEntries, Hosts: s.hosts, Guards: s.guards, Polls: s.budgets,
		Interpreted: s.interpreted, Records: s.ssaRecords, Materialized: s.materialized, GoCalls: s.goCalls,
		Unwound: s.unwound, Resumed: s.resumed, HostOps: s.regHostCounts(), WholeLeft: s.wholeLeft,
		GoCallOps: s.regGoOpsByName(), NestedGoCalls: s.regNestedCalls, Levels: s.regLevelsFound, LevelsMade: s.regLazyMade, LevelsPutOff: s.regLazyLevelsPut,
		Ended: maps.Clone(s.regEnded)}
}

// jitRegGoKey is what a call of Go register code made made
// (RegNativeStats' GoCallOps): the register instruction and, for a call,
// what it called.
type jitRegGoKey struct {
	op     bytecode.ROp
	whole  bool
	callee string
}

// noteRegGoCall counts, while s reports, a call of Go of register
// instruction w that made what k says, callee the function a call called.
func (r *Runtime) noteRegGoCall(w bytecode.RInstr, whole bool, callee Value) {
	s := r.jit
	if !s.reporting {
		return
	}
	k := jitRegGoKey{op: w.Op(), whole: whole}
	if callee.IsObject() {
		switch fd := callee.Object().fn(); {
		case fd == nil:
			k.callee = "object"
		case fd.native != nil:
			k.callee = fd.name
			if k.callee == "" {
				k.callee = "built-in"
			}
		case fd.closure != nil && !fd.bound && r.jitRegHas(fd.closure):
			k.callee = "function with code"
		case fd.closure != nil && !fd.bound:
			k.callee = "function"
		default:
			k.callee = "bound"
		}
	}
	if s.regGoOps == nil {
		s.regGoOps = map[jitRegGoKey]uint64{}
	}
	s.regGoOps[k]++
}

// regGoOpsByName is regGoOps by a name of each key.
func (s *jitState) regGoOpsByName() map[string]uint64 {
	if len(s.regGoOps) == 0 {
		return nil
	}
	m := make(map[string]uint64, len(s.regGoOps))
	for k, n := range s.regGoOps {
		name := k.op.String()
		if k.whole {
			name = "whole " + name
		}
		if k.callee != "" {
			name += "->" + k.callee
		}
		m[name] += n
	}
	return m
}

// noteRegHost counts a host exit at register pc rpc of c by its register
// instruction (RegNativeStats' HostOps).
func (s *jitState) noteRegHost(c *regCode, rpc int) {
	if rpc < 0 || rpc >= len(c.rc.Code) {
		return
	}
	if op := regAt(c.code, rpc).Op(); op < bytecode.ROpCount {
		s.regHostOps[op]++
	}
}

// regHostCounts are the host exits noteRegHost counted, by the register
// instruction's name.
func (s *jitState) regHostCounts() map[string]uint64 {
	m := map[string]uint64{}
	for op, n := range s.regHostOps {
		if n != 0 {
			m[bytecode.ROp(op).String()] += n
		}
	}
	return m
}

// noteRegEnded counts, while s reports, a run of register code c that
// ended in the interpreter at register pc rpc, for why regEndWhy says.
func (s *jitState) noteRegEnded(c *regCode, rpc int) {
	why := s.regEndWhy
	s.regEndWhy = ""
	if why == "" {
		why = "left"
	}
	op := "?"
	if rpc >= 0 && rpc < len(c.rc.Code) {
		op = regAt(c.code, rpc).Op().String()
	}
	k := why + " at " + op + " in " + jitFunctionOf(c.fn).Name
	if s.regEnded == nil {
		s.regEnded = map[string]uint64{}
	}
	s.regEnded[k]++
}

// RegNativeCall calls fn, a function of r's, with args, as a call from Go
// does, its frame run by native code compiled from its register code at
// its first call (jitRegFor) -- from its start, or, if osr, by on-stack
// replacement at the first loop header the evaluator reaches a second
// time, the frame run by the evaluator with the VM until then, as
// RegLowerCall runs it -- its exits going on in the register frame
// (jitRegRun). For tests, which compare it with an ordinary call.
func (r *Runtime) RegNativeCall(fnv Value, args []Value, osr bool) RegNativeResult {
	var o *Object
	var fd *funcData
	if fnv.IsObject() {
		o = fnv.Object()
		fd = o.fn()
	}
	var c *regCode
	if fd != nil && fd.closure != nil && !fd.bound && fd.extra == nil && fd.closure.realm == r.Realm {
		if c = fd.closure.reg; c == nil {
			c = r.regOf(fd.closure)
		}
	}
	plain := func(why string) RegNativeResult {
		v, err := r.Call(fnv, Undefined, args)
		return RegNativeResult{Value: v, Err: err, Skipped: why, Entered: -1}
	}
	if !r.jitEnabled {
		return plain("no JIT")
	}
	if c == nil || !c.wide || c.suspends || fd.arrow || c.leaf != 0 {
		return plain("no register frame")
	}
	cl := fd.closure
	var compiled bool
	if r.jit == nil || r.jit.regCache[r.jitRegKeyOf(cl)] == nil || r.jit.regCache[r.jitRegKeyOf(cl)].invalid {
		compiled = true
	}
	// Compiled here, on the spot: the call runs the code it asks for,
	// whatever the runtime's compiles in the background.
	bg, stressBg := r.jitBackground, r.jitStress.background
	r.jitBackground, r.jitStress.background = false, false
	e := r.jitRegFor(cl, c)
	r.jitBackground, r.jitStress.background = bg, stressBg
	switch {
	case e == nil:
		return plain("no room")
	case e.code == nil:
		return plain("refused: " + e.refusal)
	}
	this := Undefined
	if c.coerceThis {
		this = r.globalThis
	}
	base := r.stackTop
	if base+int(c.numRegs)+e.scratch+e.frame > len(r.stack) {
		return plain("no room")
	}
	m := min(int(c.params), len(args))
	window := r.stack[base : base+2+m : base+2+m]
	window[0] = Obj(o)
	window[1] = this
	copy(window[2:], args[:m])
	res := RegNativeResult{Compiled: compiled, Entered: -1}
	res.Value, res.Err = r.regLowerEnter(o, cl, c, base, m, args, func(f *frame) (Value, error) {
		if !osr || len(e.loops) == 0 {
			res.Entered = 0
			return r.jitRegRun(f, c, e, base, 0)
		}
		// The evaluator, the VM making what it does not, up to a loop
		// header the frame reaches a second time.
		p := e.p
		x := &regRunner{r: r, f: f, c: c, p: p, wb: base}
		slots := make([]ir.Value, p.Locals)
		x.load(slots)
		seen := map[uint32]bool{}
		pc := 0
		for {
			exit, err := p.EvaluateHost(slots, nil, x, pc, 1)
			switch {
			case err == errRegThrown:
				return Undefined, x.thrown
			case err != nil:
				return Undefined, err
			case exit.Kind == ir.Returned:
				return x.fromIR(exit.Value), nil
			}
			at := exit.State.PC
			x.store(slots)
			if exit.Kind != ir.BudgetExit {
				// Left for good: runReg from its pc.
				return r.runRegFor(f, c, int(at))
			}
			if _, ok := e.loopEntry(int(at)); ok {
				if seen[at] {
					res.Entered, res.OSR = int(at), true
					return r.jitRegRun(f, c, e, base, int(at))
				}
				seen[at] = true
			}
			pc = p.At(at)
			x.handles = x.handles[:0]
			x.load(slots)
		}
	})
	return res
}
