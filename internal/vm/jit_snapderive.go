//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

// A snapshot's answers derived on the worker (jit_snapshot.go): the
// runtime's goroutine gathers, for each snapshot -- the function's and each
// callee's -- a copy of what a method's answers are made of that the
// interpreter or the goroutine may change meanwhile (jitSnapData), as
// V8's WarpOracle copies an inline cache's stubs, and the worker, before
// it builds, derives the method's answers from that copy into the
// snapshot's tables (jitSnapAnswers) as the goroutine's asking had put
// them: the build asks them as before, the generated methods unchanged.
//
// A method derived so (jitSnapDerived) is not asked at the snapshot's PCs
// (askReg). Its gather (jitSnapFuncGatherers) runs once a snapshot, on the
// goroutine; its derivation (jitSnapDerivers), on the worker, or on the
// goroutine where it takes the compile back (jitRegTakeBack), reads only
// the copy and what does not change once made -- the register code, the
// closure's names, the fields of a shape a cache remembered, or of a
// shared one -- and writes nothing of the runtime's. Its answers' effects,
// what the feedback's asking appended to its lists, it appends to the
// snapshot's sink (sink), a feedback of no runtime's, between begin and
// end as a question asked of the feedback does: they are effects as the
// asking's are, put back at the install as often as the build asks them
// (replay). The answers' and effects' are the method's own, the
// derivation one implementation with the feedback's -- the feedback's,
// on the spot, derives from the live caches as the worker does from their
// copy.
//
// A derivation may append no link to its effects: what the snapshot took
// back of links the runtime's goroutine reads while the job is pending
// (holdsLink), and a link is the runtime's to make. Nor may an answer of
// it name a pool or a leaf (jitSnapPools, Leaf's), which the snapshot
// asks of at the goroutine once the asking is done.

// jitSnapData is what a snapshot's gathers copied, by method family, for
// its derivations.
type jitSnapData struct {
	// gathered marks a snapshot whose gathers have run, which its
	// derivations derive from; derived, the root's, a compile whose
	// derivations have run; finished, the root's, one whose finishes have
	// (finish).
	gathered, derived, finished bool
	// nodes, the root's, are the snapshots whose gathers ran, the root's
	// first, in the order they ran: what finish goes through, in an order
	// of the program's, not a map's.
	nodes []*jitSnapshot
	// asked is the runtime's test hook of the questions a snapshot asks
	// (jitTestConfig's snapAsked), which a derivation calls as an asker
	// does (heard): the gather's copy of it, of a runtime's fixed
	// configuration.
	asked func(s *jitSnapshot, m jitSnapMethod, arg int64, answered bool, results []any)
	// prop is the property sites' (jit_snapprop.go).
	prop jitPropData
	// native is the learning's methods' (jit_snapnative.go), calls the
	// calls' (jit_snapcalls.go).
	native jitNativeData
	calls  jitCallsData
	// rt is the runtime the gathers ran on, the root's: the closures'
	// copies' (closureCopy).
	rt *Runtime
	// copies, the root's, are the closures' copies its compile's gathers
	// made (closureCopy).
	copies map[*closure]*jitClosureCopy
}

// jitClosureCopy is what of a closure's state a compile's gathers copy, on
// the runtime's goroutine, for the worker to derive answers from: its
// property sites' caches (ic), each one's earlier entries a copy of its
// own (jitCopiedPoly), and its calls' caches (calls, the calls family's).
// The interpreter writes them in place as it runs; a copy is made once a
// compile, of the function's closure and of each callee's, whichever
// snapshot asks it first, and read by every snapshot of the compile that
// names the closure.
type jitClosureCopy struct {
	ic    []propCache
	polys []jitCopiedPoly
	calls []regCallCache
}

// jitCopiedPoly is a cache's earlier entries copied (propPoly), with
// theirs (propSeen): copied together, each closure's are one allocation.
type jitCopiedPoly struct {
	p propPoly
	s propSeen
}

// closureCopy is cl's copy (jitClosureCopy), of d, the compile's root's
// data: made at its first ask, its caches copied whole.
func (d *jitSnapData) closureCopy(cl *closure) *jitClosureCopy {
	if c := d.copies[cl]; c != nil {
		return c
	}
	c := &jitClosureCopy{}
	c.ic, c.polys = jitCopyCaches(cl.ic)
	c.calls = jitCallsCopy(d.rt, cl)
	if d.copies == nil {
		d.copies = map[*closure]*jitClosureCopy{}
	}
	d.copies[cl] = c
	return c
}

// jitCopyCaches is a copy of ic, each entry's earlier entries (propPoly) a
// copy of its own, their earlier entries (propSeen) with them, of what an
// answer or a callee's seen feedback (jitSeenFeedbackInto) reads of them:
// the interpreter changes them in place. The copy is
// ic's bulk; the earlier entries, which only polymorphic sites have, one
// allocation more.
func jitCopyCaches(ic []propCache) (out []propCache, polys []jitCopiedPoly) {
	out = append([]propCache(nil), ic...)
	np := 0
	for i := range out {
		if out[i].poly != nil {
			np++
		}
	}
	if np == 0 {
		return out, nil
	}
	polys = make([]jitCopiedPoly, np)
	k := 0
	for i := range out {
		p := out[i].poly
		if p == nil {
			continue
		}
		q := &polys[k]
		k++
		q.p = *p
		q.p.mega = nil
		if p.seen != nil {
			// The interpreter appends to reads and deep, never writing
			// what they hold: the copy shares what they held.
			q.s = *p.seen
			q.p.seen = &q.s
		}
		out[i].poly = &q.p
	}
	return out, polys
}

// jitSnapDerived are the methods whose answers a snapshot derives on the
// worker (a bit by jitSnapMethod), which askReg does not ask; of them,
// jitSnapAskedAtGather are those the family's gather asks itself, on the
// goroutine, at the PCs the copy says may answer -- the rest answer
// nothing, as the feedback would -- where the answer reads the heap, the
// worker deriving none of them.
var jitSnapDerived, jitSnapAskedAtGather uint64

// jitSnapFuncGatherers are the gathers of the derived methods' families, each
// run once a snapshot, on the runtime's goroutine, before its PCs are
// asked; jitSnapDerivers their derivations, run once a snapshot, on the
// worker. Each family appends its own in an init of its file.
var (
	jitSnapFuncGatherers []func(s *jitSnapshot, fb *jitRegFeedback)
	jitSnapDerivers      []func(s *jitSnapshot)
)

// jitSnapFinishers are the families' finishes of a snapshot whose
// derivations ran, each run once a snapshot, on the runtime's goroutine,
// when its compile is done with: installed, thrown away, or built again
// from the feedback asked anew (jitRegInstall). What a derivation could not
// write of the runtime's -- what the asking appends to the lists of what a
// function's code learned (jitLearned's nativeCalls, inlines) -- it keeps
// in its snapshot, and its family's finish writes it, as a concurrent
// compile writes no feedback until it is done: the asking had written it
// as the compile was prepared, which the finish puts in the place it would
// have had.
var jitSnapFinishers []func(s *jitSnapshot)

// gather runs the families' gathers of s, fb's snapshot.
func (s *jitSnapshot) gather(fb *jitRegFeedback) {
	s.d.gathered, s.d.asked, s.d.rt = true, fb.fb.r.jitTest.snapAsked, fb.fb.r
	s.root.d.nodes = append(s.root.d.nodes, s)
	for _, g := range jitSnapFuncGatherers {
		g(s, fb)
	}
}

// derive runs the families' derivations of s, the snapshot of a
// compile's own function, and of each callee's it asked, once: the
// answers put into their tables, sealed again. The worker runs it before
// it builds; a snapshot's first question runs it too (ready), as a test's
// build of a snapshot asks it.
func (s *jitSnapshot) derive() {
	if s.d.derived {
		return
	}
	s.d.derived = true
	if len(jitSnapDerivers) == 0 {
		return
	}
	all := append([]*jitSnapshot{s}, mapValues(s.callees)...)
	for _, c := range all {
		if !c.d.gathered {
			// A callee asked nothing (nestReg).
			continue
		}
		for _, d := range jitSnapDerivers {
			d(c)
		}
		c.a.seal()
	}
}

// finish runs the families' finishes (jitSnapFinishers) of s, a
// compile's root, and of each snapshot of it whose gathers ran, in the
// order they ran, once, on the runtime's goroutine. A compile whose
// derivations have not run -- one thrown away before a worker came to it --
// derives here first (JITGoroutineTimes' FinishDerives counts them): what
// the asking appended as the compile was prepared is the function's
// whatever came of the compile, and the compiles after it are made from
// it.
func (s *jitSnapshot) finish() {
	if s.d.finished || !s.d.gathered {
		return
	}
	s.d.finished = true
	if len(jitSnapFinishers) == 0 {
		return
	}
	if !s.d.derived {
		s.derive()
		if r := s.d.rt; r != nil && r.jit != nil {
			r.jit.goroutineTimes.FinishDerives++
		}
	}
	for _, c := range s.d.nodes {
		for _, f := range jitSnapFinishers {
			f(c)
		}
	}
}

// ready derives the answers of s's compile, if they are not yet: what a
// question asks first.
func (s *jitSnapshot) ready() {
	if !s.root.d.derived {
		s.root.derive()
	}
}

// heard calls the runtime's test hook of the questions asked, where set,
// with a derived answer, as the asker calls it with the feedback's.
func (s *jitSnapshot) heard(m jitSnapMethod, arg int64, answered bool, results ...any) {
	if h := s.d.asked; h != nil {
		h(s, m, arg, answered, results)
	}
}

// sinkOf is the feedback s's derivations append their answers' effects
// to: of no runtime, the root's its own keep, a callee's keeping in the
// root's, as the asked feedback's do.
func (s *jitSnapshot) sinkOf() *jitFeedback {
	if s.sink != nil {
		return s.sink
	}
	if s.root == s {
		s.sink = &jitFeedback{}
	} else {
		s.sink = &jitFeedback{root: s.root.sinkOf()}
	}
	return s.sink
}

// endDerived is end for a derivation's question, its effects appended to
// sink since before: no link among them (jitSnapshot's derivations).
func (s *jitSnapshot) endDerived(sink *jitFeedback, before [2]jitSnapLens) int32 {
	if len(sink.links) != before[0][3] || len(sink.keep().links) != before[1][3] {
		panic("jit: a snapshot's derivation appended a link")
	}
	return s.end(sink, before)
}
