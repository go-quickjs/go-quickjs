//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"
	"slices"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

// Register code lowered ahead, on a worker, for a compile in the background
// (jitRegCompile). A lowering is a function of the register code, its
// function, its kind and its hints alone -- register code is never
// rewritten, and a function's hints only grow, each list appended to -- so
// a worker makes the program the runtime's goroutine would. A compile in
// the background whose function's program is not lowered for its hints as
// they are hands the lowering to the workers (jitAheadJob), with those of
// the callees its call sites' caches name that are not lowered for
// inlining (jitRegAheadCallees), and is asked for again at the function's
// next sample, as one the background has no room for is. Asked again,
// jitRegLower takes each program the worker made for the same register
// code and hints (regTakeAhead) in place of lowering it, and keeps and
// counts it as it would one it lowered then: the build is given the same
// programs, and the cache holds the same, counted at the same asks.
// Lowered on the goroutine, the function's program and its callees' had
// been about 50 of the 254 us a compile in the background spent there on
// Octane's Typescript.

// jitAheadKey names a lowering made ahead: the function's key and the
// kind of its program.
type jitAheadKey struct {
	key  jitKey
	kind jitLowerKind
}

// jitAheadItem is one lowering a jitAheadJob makes: of rc, fn's register
// code, as kind says, with hints h, its key's, and so the kinds that share
// its program (jitRegLowerKinds); got is what it made, failed marks one
// not made. Its key names kind as jitAheadKind does.
type jitAheadItem struct {
	at     jitAheadKey
	kind   jitLowerKind
	rc     *bytecode.RegCode
	fn     *bytecode.Function
	h      jitcompile.Hints
	got    jitLowered
	failed bool
}

// jitAheadKind is the kind a lowering ahead of kind is named by: an
// entry's for those that share their program (jitLowerShares), made at
// once.
func jitAheadKind(kind jitLowerKind) jitLowerKind {
	if jitLowerShares(kind) {
		return jitLowerSSA
	}
	return kind
}

// jitAheadJob is the lowerings ahead a compile in the background waits for
// (jitRegLowerAhead), its function's first, then its callees': made by a
// worker, before any compile queued (waitedWork), or, taken back
// (jitJobTaken), by the goroutine at each ask instead. waited is the work
// its function did waiting (jitRegWaited); ticks the epochs' ticks it has
// waited, after two of which it is let go of (regAheadTick).
type jitAheadJob struct {
	root   jitKey
	items  []jitAheadItem
	state  atomic.Int32
	done   chan struct{}
	waited atomic.Int64
	ticks  uint8
}

func (j *jitAheadJob) waitedWork() int64 { return math.MaxInt64 }

// run lowers j's items, each as jitRegLower would; a panic leaves the item
// to the goroutine's lowering.
func (j *jitAheadJob) run() {
	defer close(j.done)
	if !j.state.CompareAndSwap(jitJobQueued, jitJobWorking) {
		return
	}
	for i := range j.items {
		it := &j.items[i]
		func() {
			defer func() {
				if v := recover(); v != nil {
					it.got, it.failed = jitLowered{}, true
				}
			}()
			it.got = jitRegLowerKinds(it.rc, it.fn, it.kind, it.h)
		}()
	}
	j.state.Store(jitJobDone)
}

// jitHintsSame reports whether a and b are the same hints, list by list.
func jitHintsSame(a, b *jitcompile.Hints) bool {
	return a.KeepParams == b.KeepParams && slices.Equal(a.StringAdds, b.StringAdds) && slices.Equal(a.ElementReads, b.ElementReads) &&
		slices.Equal(a.StringReads, b.StringReads) && slices.Equal(a.WholeOps, b.WholeOps)
}

// aheadItem is the item of the lowering ahead at, of rc with hints h, and
// its job: the item nil where the job is not done, or made it of other
// code or hints, or failed; both nil for none.
func (s *jitState) aheadItem(at jitAheadKey, rc *bytecode.RegCode, h *jitcompile.Hints) (*jitAheadItem, *jitAheadJob) {
	j := s.regAhead[at]
	if j == nil {
		return nil, nil
	}
	if j.state.Load() != jitJobDone {
		return nil, j
	}
	for i := range j.items {
		if it := &j.items[i]; it.at == at {
			if it.failed || it.rc != rc || !jitHintsSame(&it.h, h) {
				return nil, j
			}
			return it, j
		}
	}
	return nil, j
}

// regTakeAhead is the lowering ahead at names, of rc with hints h, if a
// worker made it (aheadItem), taken: the lowering's caller keeps it.
func (s *jitState) regTakeAhead(at jitAheadKey, rc *bytecode.RegCode, h *jitcompile.Hints) (jitLowered, bool) {
	it, j := s.aheadItem(at, rc, h)
	if j != nil && j.state.Load() == jitJobDone {
		// Taken or not of use: what is asked for next is lowered for it.
		delete(s.regAhead, at)
	}
	if it == nil {
		return jitLowered{}, false
	}
	s.aheadTaken++
	return it.got, true
}

// jitRegLowerAhead reports whether the compile in the background of cl's
// function, its register code c, may go on: its program, of
// the kind its compile asks (jitRegCompile), lowered for its hints as they
// are, or lowered ahead (regTakeAhead), or not to be waited for. Otherwise
// it hands the lowering to the workers (jitAheadJob), with its callees'
// (jitRegAheadCallees), and the function is asked for again at its next
// sample -- at once under QJS_JIT_STRESS's background and the tests' waits,
// which wait for the lowering, so that the compile is made where it would
// have been. A lowering ahead no worker has begun once the function has
// spent jitTakeBackAsks budgets waiting is taken back, lowered as it is
// asked for, as a compile in the background is (jitRegTakeBack). It
// returns the function's lowerings ahead, if any, which its compile lets
// go of once made (regAheadEnd).
func (r *Runtime) jitRegLowerAhead(cl *closure, c *regCode, callee bool) (bool, *jitAheadJob) {
	s := r.jit
	if c.rc.Typed {
		// Typed code's, whose link asks for its code once (jitTypedLinked):
		// lowered as it is asked for.
		return true, nil
	}
	key, l, h := r.jitRegLowering(cl, c.rc)
	if l == nil {
		return true, nil
	}
	// ready is whether the program of kind is there, made or ahead, its
	// error; or the job that is making it.
	ready := func(kind jitLowerKind) (error, bool, *jitAheadJob) {
		if l.done[kind] {
			return l.errs[kind], true, nil
		}
		at := jitAheadKey{key, jitAheadKind(kind)}
		it, j := s.aheadItem(at, c.rc, &h)
		if it != nil && it.got.done[kind] {
			return it.got.errs[kind], true, nil
		}
		if j != nil && j.state.Load() == jitJobDone {
			// Made of other hints: lowered anew.
			delete(s.regAhead, at)
			j = nil
		}
		return nil, false, j
	}
	// The fallback of a program of host operations alone (jitRegCompile),
	// which shares its program (jitRegLowerKinds).
	fallback := jitLowerHot
	if callee {
		fallback = jitLowerInline
	}
	err, ok, pend := ready(jitLowerSSA)
	if ok && err != nil && jitcompile.HostOnly(err) && (callee || r.jitRegCallsNatively(cl, c)) {
		_, ok, pend = ready(fallback)
	}
	if ok {
		return true, s.regAheadRoots[key]
	}
	if pend != nil {
		// Waited for: taken back where no worker has begun it in time.
		root := s.regAheadRoots[key]
		if pend.waited.Load() >= jitTakeBackAsks*jitRegBudget(r.jitCallThreshold, c) && pend.state.CompareAndSwap(jitJobQueued, jitJobTaken) {
			return true, root
		}
		return r.jitAheadWait(pend), root
	}
	j := &jitAheadJob{root: key, done: make(chan struct{})}
	j.items = []jitAheadItem{{at: jitAheadKey{key, jitLowerSSA}, kind: jitLowerSSA, rc: c.rc, fn: cl.fn, h: h}}
	r.jitRegAheadCallees(cl, func(t *closure, rc *bytecode.RegCode, k jitKey, h jitcompile.Hints) {
		j.items = append(j.items, jitAheadItem{at: jitAheadKey{k, jitAheadKind(jitLowerInline)}, kind: jitLowerInline, rc: rc, fn: t.fn, h: h})
	})
	if s.regAhead == nil {
		s.regAhead, s.regAheadRoots = map[jitAheadKey]*jitAheadJob{}, map[jitKey]*jitAheadJob{}
	}
	for _, it := range j.items {
		s.regAhead[it.at] = j
	}
	if old := s.regAheadRoots[key]; old != nil {
		s.regAheadEnd(old)
	}
	s.regAheadRoots[key] = j
	s.aheadJobs++
	if !r.jitTest.aheadUnstarted {
		jitWorkers.submit(j)
	}
	return r.jitAheadWait(j), j
}

// jitAheadWait reports whether j is done, waiting for it under
// QJS_JIT_STRESS's background and the tests that wait for the background
// (jitTestConfig's bgDone, bgUnstarted): their compiles are made where they
// would have been, from the workers' programs.
func (r *Runtime) jitAheadWait(j *jitAheadJob) bool {
	if (r.jitStress.background || r.jitTest.bgDone || r.jitTest.bgUnstarted) && !r.jitTest.aheadUnstarted {
		<-j.done
	}
	return j.state.Load() == jitJobDone
}

// jitRegAheadCallees calls add for each callee of cl's that its call
// sites' caches, what its code learned it calls and the closures it makes
// name, and theirs in turn, that is not lowered for inlining for its hints
// as they are, nor being lowered ahead: what its compile's Inline asks to
// lower (jitRegFeedback's inline, madeInline), mostly. It reads and writes
// nothing of theirs but their keys; one it names wrongly costs a worker its
// lowering, one it misses is lowered as it is asked for.
func (r *Runtime) jitRegAheadCallees(root *closure, add func(t *closure, rc *bytecode.RegCode, key jitKey, h jitcompile.Hints)) {
	s := r.jit
	var seen [jitAheadCallees]*closure
	n := 0
	visit := func(t *closure) bool {
		if t == nil || t == root || t.fn == root.fn || n == len(seen) || slices.Contains(seen[:n], t) {
			return false
		}
		seen[n] = t
		n++
		fn := t.fn
		rc := fn.RegCompiled()
		if rc == nil || !rc.Wide || rc.Typed || fn.Async || fn.Generator || fn.TopLevel || fn.IsModule {
			return true
		}
		key := r.jitRegKeyOf(t)
		if s.regAhead[jitAheadKey{key, jitAheadKind(jitLowerInline)}] != nil {
			return true
		}
		var h jitcompile.Hints
		if e := s.regLearned[key]; e != nil {
			h = e.regHints
		}
		h.KeepParams = r.nodeQuirks && !fn.Strict
		if l := s.regLowered[key]; l != nil && l.done[jitLowerInline] {
			lh := l.hints
			lh.KeepParams = h.KeepParams
			if jitHintsSame(&lh, &h) {
				return true
			}
		}
		add(t, rc, key, h)
		return true
	}
	var walk func(cl *closure, depth int)
	walk = func(cl *closure, depth int) {
		rc := cl.reg.rc
		for _, w := range rc.Code {
			var t *closure
			switch op := w.Op(); jitRegCallOp(op) {
			case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW:
				if i := int(w.C()); i < len(cl.regCalls) {
					if e := &cl.regCalls[i]; jitRegCallKind(e) {
						t = e.cl
					}
				}
			case bytecode.RClosureW:
				if k := int(w.BC()); k < len(cl.consts) {
					t = cl.consts[k].template()
				}
			}
			if visit(t) && depth > 0 && t.reg != nil {
				walk(t, depth-1)
			}
		}
		if e := s.regLearned[r.jitRegKeyOf(cl)]; e != nil {
			for _, list := range [...][]jitInline{e.inlines, e.nativeCalls} {
				for _, x := range list {
					if visit(x.cl) && depth > 0 && x.cl.reg != nil {
						walk(x.cl, depth-1)
					}
				}
			}
		}
	}
	if root.reg != nil {
		walk(root, 1)
	}
}

// jitAheadCallees bounds the callees jitRegAheadCallees looks at.
const jitAheadCallees = 32

// jitAheadBudgetPart is the part of its budget a function spends between
// its asks while its program is lowered ahead (jitRegHot): a lowering is
// about a tenth of a compile, which takes about as long as two or three
// budgets of the interpreter's (jitTakeBackAsks).
const jitAheadBudgetPart = 4

// regAheadEnd lets go of what of j no lowering took, once the compile that
// waited for it is made, or no longer waits.
func (s *jitState) regAheadEnd(j *jitAheadJob) {
	if s.regAheadRoots[j.root] == j {
		delete(s.regAheadRoots, j.root)
	}
	for _, it := range j.items {
		if s.regAhead[it.at] == j {
			delete(s.regAhead, it.at)
		}
	}
}

// regAheadTick lets go, at an epoch's tick, of the lowerings ahead whose
// compiles have not come for them in two: functions no longer asked for.
func (s *jitState) regAheadTick() {
	for _, j := range s.regAheadRoots {
		if j.ticks++; j.ticks > 2 {
			s.regAheadEnd(j)
		}
	}
}
