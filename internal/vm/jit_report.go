//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"cmp"
	"slices"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// JITFunction is what the JIT did with one function, as JITReport lists
// it: what a run's coverage is made of. Script, Line and Column say where
// the function starts; Status is native (its code runs), slow (it has code, which
// Go stopped entering: its exits cost more than it saved), invalid (its
// code was invalidated by a deoptimization), refused (the JIT does not
// compile it, for Reason) or skipped (never tried, for Reason).
//
// Native counts the times its code ran -- Go's entries, at its start or
// into a loop, and native callers' calls -- and Declined the calls the
// interpreter made without entering it, which a slow function's are;
// Hosts, Guards and Ended count its exits: to Go for an operation, at a
// failed guard, and finished in the interpreter; GoCalls the calls of Go
// it made; Work the work done natively, in instructions, as the JIT
// estimates it; and Reopts the times it was compiled again for what it
// learned. The counts only grow while the JIT keeps what it learned of the
// function, over its compiles; one whose state it let go of counts afresh.
type JITFunction struct {
	Name, Script                                          string
	Line, Column                                          int
	Status                                                string
	Reason                                                string
	Native, Declined, Hosts, Guards, Ended, GoCalls, Work uint64
	Reopts                                                int
}

// JITCodeHook, if set, is called with each function's native code as it
// is placed: the function, the code's instructions where they run, and
// what the backend made them from (mir.Code's Marks and Listing, while
// mir.Annotate is set) -- for a profiler of native code, which maps a
// sample's address to an instruction. Set it before any runtime compiles;
// it is called on the runtime's goroutine.
var JITCodeHook func(f JITFunction, text []byte, code *mir.Code)

// JITCrossHook, if set, is called for each compile JITCompileRepeat
// repeats with what compiling it for arm64 made, on any machine: the code
// is not run, which is how a change to arm64's backend is compared where
// it cannot run. Set it before any runtime compiles.
var JITCrossHook func(f JITFunction, code *mir.Code, err error)

// JITHoldClocks stops the clocks the JIT's cache ages code by: what a run
// compiles, and with what, then depends on the run alone if the collector
// is off, which ends weak references' part in it, and two builds' code
// can be compared compile by compile. Call it before any runtime compiles,
// and restore, which it returns, after the last is done: the clocks run
// again.
func JITHoldClocks() (restore func()) {
	was := jitClocksHeld.Swap(true)
	return func() { jitClocksHeld.Store(was) }
}

// SetJITReport has the JIT note why its runs ended in the interpreter and
// what its calls of Go made (RegNativeStats). What it compiles or refuses,
// which JITReport lists, it keeps anyway.
func (r *Runtime) SetJITReport(on bool) {
	if r.jitEnabled {
		r.jitState().reporting = on
	}
}

// JITReport lists what the JIT did with each function it has met that is
// still alive, by script and line.
func (r *Runtime) JITReport() []JITFunction {
	s := r.jit
	if s == nil {
		return nil
	}
	var out []JITFunction
	for k, e := range s.regCache {
		// Register code's, its name marked so: what it compiled, what it
		// refused and why.
		fn := k.Value()
		if fn == nil {
			continue
		}
		f := jitFunctionOf(fn)
		f.Name += " [reg]"
		switch {
		case e.code != nil && e.invalid:
			f.Status = "invalid"
		case e.code != nil && e.life != nil && e.life.entrySlow:
			f.Status = "slow"
		case e.code != nil:
			f.Status = "native"
		default:
			f.Status, f.Reason = "refused", e.refusal
		}
		if l := e.life; l != nil {
			// What the function's register code did, over its compiles:
			// Go's entries at its start and into its loops, native
			// callers' calls through its link; its exits; its compiles
			// again.
			st := &l.ssaStats
			f.Native = l.starts + st.entries + l.calledIn
			if link := s.regLinks[k]; link != nil {
				f.Native += link.in
			}
			f.Hosts, f.Guards, f.Ended, f.Declined = st.hosts, st.guards, st.ended, l.slowCalls
			f.GoCalls, f.Work, f.Reopts = st.goCalls, st.work, int(l.reopts)
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b JITFunction) int {
		return cmp.Or(cmp.Compare(a.Script, b.Script), cmp.Compare(a.Line, b.Line), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// jitFunctionOf is a report's line for fn, where it is.
func jitFunctionOf(fn *bytecode.Function) JITFunction {
	f := JITFunction{Name: fn.Name}
	if f.Name == "" {
		f.Name = "(anonymous)"
	}
	if fn.TopLevel {
		f.Name = "(top level)"
	}
	if fn.Script != nil {
		f.Script = fn.Script.Name
		line, col := fn.Script.Position(fn.Start)
		f.Line, f.Column = int(line), int(col)
	}
	return f
}

// JITTierEvent is a line of the JIT's tier trace (JITTierHook), what V8's
// --trace-opt and --trace-deopt print: a function's code placed, or its
// code invalidated by a deoptimization.
type JITTierEvent struct {
	// Nothing compares events: comparable, the type had an equality
	// function the compiler lays out among package vm's code.
	_ [0]func()
	// Func is the function, as JITReport names it.
	Func JITFunction
	// Deopt marks an invalidation; otherwise the event is a compile.
	Deopt bool
	// Reason is, for a compile, what it was for: first (the function's
	// first code, for its calls and loops), callee (its first, for its
	// native callers' calls), deopt (again, after an invalidation),
	// learned (again, for what its code learned at its exits), flushed
	// (again, after the cache let its code go), again (any other), with
	// "+osr" for code compiled for a frame waiting at a loop. For a
	// deoptimization, it is what failed: a property site by its kind --
	// read, method, write, update -- and what its cache held then --
	// unfed (no shape), mono, poly or mega (past what native code is
	// compiled for) -- or the instruction's name; entry for an entry's
	// speculation, call for a call of a target the code was not compiled
	// for, inlined for a guard of an inlined callee's, loop for a loop of
	// a built-in's call.
	Reason string
	// Length is the function's register code's length, the unit of its
	// budget, which is counted in invocations of it.
	Length int
	// Invocations is how many times the function has run so far, in any
	// tier, as its tier-up counts them (jitTierInvocations): the
	// interpreter's runs its budget counted, and its code's since it was
	// last placed.
	Invocations int64
	// Age is, for a deoptimization, the work the code did from its
	// placing to its failure, in invocations' worth (jitRegSpent).
	Age int64
	// Budget is, for a deoptimization, the invocations' worth the
	// interpreter spends on the function before it is compiled again.
	Budget int64
	// Deopts counts the function's deoptimizations so far, this one
	// among them, and Compiles the compiles of it placed before this
	// event's.
	Deopts, Compiles int
	// At is, for a deoptimization, the function whose instruction failed
	// -- Func, or a callee inlined in its code -- and PC that
	// instruction's register pc, -1 where none is known: a site's
	// deoptimizations, each of which widens what it failed on, are
	// counted by them.
	At JITFunction
	PC int
	// Top marks a deoptimization at a site already built at the top of its
	// ladder, generic, which widens it no further: a loop of
	// deoptimizations there, which none should be.
	Top bool
}

// JITTierHook, if set, is called at each compile placed and each
// deoptimization that invalidates code, with what they were for: the
// tier trace, a host's debug option (v8bench's -jittier), which no hot
// path reads. Set it before any runtime compiles; it is called on the
// runtime's goroutine, for a compile in the background at its install.
var JITTierHook func(JITTierEvent)

// JITCompileClock, if set, is the clock the runtime goroutine's part of
// each compile is timed by (JITGoroutineTimes): a host's debug option, as
// the tier trace is (v8bench's -jittier); while it is nil nothing is
// timed. Set it before any runtime compiles.
var JITCompileClock func() time.Duration

// JITGoroutineTimes is the runtime goroutine's part of its compiles, as
// JITCompileClock times them: what a compile in the background leaves on
// the program's time. Spot is the time of the compiles made on the spot,
// Spots how many there were; Prepared that of the compiles handed to the
// background -- their lowering, their feedback asked ahead, their hand-over
// -- Prepares how many; Installed that of their installs, Installs how
// many; and TakenBack that of the builds of those taken back to the
// runtime's goroutine (jitRegTakeBack), TakeBacks how many. A compile made
// within another, a callee's, is the other's. The times leave out those of
// the host's hooks called meanwhile (JITTierHook, JITCodeHook), which are
// the host's.
// FinishDerives counts the compiles in the background thrown away before a
// worker derived their snapshot's answers, derived on the runtime's
// goroutine for what their asking appends to the function's learning
// (jitSnapshot's finish), counted whether JITCompileClock is set or not.
type JITGoroutineTimes struct {
	Spot, Prepared, Installed, TakenBack time.Duration
	Spots, Prepares, Installs, TakeBacks int
	FinishDerives                        int
	// EagerForwarders, EagerParked, EagerTyped and EagerSites count the
	// compiles in the background whose inline sites were asked on the
	// runtime's goroutine, their callees lowered there, for a callee taken
	// that hands its arguments on, one parked, the function's typed code,
	// or its calls asked there (no learning, a site of two calls);
	// EagerLate, the callees taken whose calls are asked there, each
	// lowered and asked there. CalleeMisses counts the compiles whose
	// worker took a callee to inline that the gathers had not, built on the
	// runtime's goroutine at their install. GatheredCallees and
	// TakenCallees count the callees the compiles' gathers took and those
	// their workers took to inline; UntakenLearned, the callees gathered and
	// not taken whose learning their seen feedback, taken at the gather,
	// changed; NodesAskedAnew, the callees gathered that the asking on the
	// runtime's goroutine asked anew where a site it asked there inlines
	// them (an accessor's), their gathers thrown away.
	EagerForwarders, EagerParked, EagerTyped, EagerSites, EagerLate int
	CalleeMisses                                                    int
	GatheredCallees, TakenCallees, UntakenLearned, NodesAskedAnew   int
	// ProgramsAgain counts the snapshots gathered of no program that
	// asked of it after all (jitRegSnapshotOf), asked again of it.
	ProgramsAgain int
}

// jitCompileClock is the runtime's JITCompileClock: the host's, or what a
// test made the runtime with.
func (r *Runtime) jitCompileClock() func() time.Duration {
	if c := r.jitTest.compileClock; c != nil {
		return c
	}
	return JITCompileClock
}

// TakeJITGoroutineTimes is the runtime goroutine's part of its compiles
// since the last call (JITGoroutineTimes), timed while JITCompileClock is
// set.
func (r *Runtime) TakeJITGoroutineTimes() JITGoroutineTimes {
	s := r.jit
	if s == nil {
		return JITGoroutineTimes{}
	}
	t := s.goroutineTimes
	s.goroutineTimes = JITGoroutineTimes{}
	return t
}

// jitTierHook is the runtime's JITTierHook: the host's, or what a test
// made the runtime with.
func (r *Runtime) jitTierHook() func(JITTierEvent) {
	h := r.jitTest.tierHook
	if h == nil {
		h = JITTierHook
	}
	if h == nil || r.jit == nil || !r.jit.timingGoroutine {
		return h
	}
	// The host's, timed apart from the compile it is called in
	// (jitHookTimed).
	s := r.jit
	return func(e JITTierEvent) {
		defer s.jitHookTimed(r.jitCompileClock())()
		h(e)
	}
}

// jitHookTimed times, by clock, a host's hook called in a compile being
// timed (JITGoroutineTimes), its time the host's, not the compile's: the
// tier trace's hook, which writes a line of the trace and flushes it, a
// system call, had been part of each compile's install on the program's
// time. It returns the function that ends the timing.
func (s *jitState) jitHookTimed(clock func() time.Duration) func() {
	from := clock()
	return func() { s.hookTime += clock() - from }
}

// The reasons a compile is made for, which the tier trace says
// (JITTierEvent's Reason): jitRegEntry's tier, jitTierOSR set beside one
// for code compiled for a frame waiting at a loop.
const (
	jitTierFirst uint8 = iota
	jitTierCallee
	jitTierDeopt
	jitTierLearned
	jitTierFlushed
	jitTierAgain
	jitTierOSR = 0x80
)

var jitTierReasons = [...]string{"first", "callee", "deopt", "learned", "flushed", "again"}

// jitTierReason is what a compile of the function whose state is life is
// for, callee for its native callers, osr for a frame waiting at a loop.
func jitTierReason(life *jitEntry, callee, osr bool) uint8 {
	var t uint8
	switch {
	case life.invalid:
		t = jitTierDeopt
	case life.reoptPending():
		t = jitTierLearned
	case life.regFlushed:
		t = jitTierFlushed
	case life.ssaSerial != 0:
		t = jitTierAgain
	case callee:
		t = jitTierCallee
	}
	if osr {
		t |= jitTierOSR
	}
	return t
}

// jitTierInvocations is how many times cl's function, life its state, has
// run so far, in any tier: in the interpreter (jitRegInvoked), Go's
// entries of its code and the runs it went on with, the calls Go made
// without entering it, and its native callers' calls since its code was
// last placed.
func (r *Runtime) jitTierInvocations(cl *closure, life *jitEntry) int64 {
	n := jitRegInvoked(cl, life) + int64(life.starts+life.goOns+life.slowCalls)
	if l := r.jit.regLinks[r.jitRegKeyOf(cl)]; l != nil {
		n += int64(l.in)
	}
	return n
}

// jitTierPlaced tells the tier trace of e, the code of cl's function c,
// placed.
func (r *Runtime) jitTierPlaced(h func(JITTierEvent), cl *closure, c *regCode, e *jitRegEntry) {
	life := e.life
	why := jitTierReasons[e.tier&^jitTierOSR]
	if e.tier&jitTierOSR != 0 {
		why += "+osr"
	}
	h(JITTierEvent{Func: jitFunctionOf(cl.fn), Reason: why, Length: len(c.rc.Code),
		Invocations: r.jitTierInvocations(cl, life), Deopts: int(life.deopts), Compiles: int(life.ssaSerial)})
}

// jitTierInvalidated tells the tier trace of e, the code of cl's function
// c, invalidated for why, at at, the interpreter to spend budget, in work,
// before it is compiled again.
func (r *Runtime) jitTierInvalidated(h func(JITTierEvent), cl *closure, c *regCode, e *jitRegEntry, why string, at jitTierSite, budget int64) {
	life := e.life
	n := int64(max(len(c.rc.Code), 1))
	fn := cl.fn
	if at.cl != nil {
		fn = at.cl.fn
	}
	h(JITTierEvent{Func: jitFunctionOf(cl.fn), Deopt: true, Reason: why, Length: len(c.rc.Code),
		Invocations: r.jitTierInvocations(cl, life), Age: r.jitRegAge(c, e) / n, Budget: budget / n,
		Deopts: int(life.deopts), Compiles: int(life.ssaSerial), At: jitFunctionOf(fn), PC: at.pc, Top: r.jit.siteTop})
	r.jit.siteTop = false
}

// jitTierSite is where a deoptimization failed, for the tier trace
// (JITTierEvent's At and PC): the register pc pc of cl's code, cl nil for
// the code's own function's, pc -1 where it is not known.
type jitTierSite struct {
	cl *closure
	pc int
}

// jitTierDeoptWhy is what a guard that failed at register pc rpc of f's
// code c, its window at wb, failed on, for the tier trace: a property
// site by its kind and what its cache held, or the instruction; entry for
// an entry's speculation (site < 0).
func (r *Runtime) jitTierDeoptWhy(f *frame, c *regCode, wb, rpc int, site int64) string {
	if site < 0 {
		return "entry"
	}
	if rpc < 0 || rpc >= len(c.rc.Code) {
		return "guard"
	}
	op, ok := r.jitRegPropOp(f, c, wb, rpc)
	if !ok {
		return c.rc.Code[rpc].Op().String()
	}
	kind := "read"
	switch {
	case op.write:
		kind = "write"
	case op.update:
		kind = "update"
	case op.method:
		kind = "method"
	}
	x := op.c
	if x == nil {
		return kind + "/unfed"
	}
	// The shapes its cache holds but the receiver's, which a miss in Go
	// may have filled before the guard's failure.
	var recv *shape
	if op.recv.IsObject() {
		recv = op.recv.Object().shape
	}
	n := 0
	other := func(c *propCache) {
		if c.shape != nil && c.shape != noShape && c.shape != recv {
			n++
		}
	}
	other(x)
	if x.poly != nil {
		for i := range x.poly.n {
			other(&x.poly.entries[i])
		}
	}
	if sn := x.seenOf(); sn != nil {
		for i := range sn.n {
			other(&sn.entries[i])
		}
	}
	switch {
	case x.fills >= maxCacheFills || n > jitPropertyCases:
		return kind + "/mega"
	case n == 0:
		return kind + "/unfed"
	case n == 1:
		return kind + "/mono"
	}
	return kind + "/poly"
}
