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
	// derivations have run.
	gathered, derived bool
	// asked is the runtime's test hook of the questions a snapshot asks
	// (jitTestConfig's snapAsked), which a derivation calls as an asker
	// does (heard): the gather's copy of it, of a runtime's fixed
	// configuration.
	asked func(s *jitSnapshot, m jitSnapMethod, arg int64, answered bool, results []any)
	// prop is the property sites' (jit_snapprop.go).
	prop jitPropData
}

// jitSnapDerived are the methods whose answers a snapshot derives on the
// worker (a bit by jitSnapMethod), which askReg does not ask.
var jitSnapDerived uint64

// jitSnapFuncGatherers are the gathers of the derived methods' families, each
// run once a snapshot, on the runtime's goroutine, before its PCs are
// asked; jitSnapDerivers their derivations, run once a snapshot, on the
// worker. Each family appends its own in an init of its file.
var (
	jitSnapFuncGatherers []func(s *jitSnapshot, fb *jitRegFeedback)
	jitSnapDerivers      []func(s *jitSnapshot)
)

// gather runs the families' gathers of s, fb's snapshot.
func (s *jitSnapshot) gather(fb *jitRegFeedback) {
	s.d.gathered, s.d.asked = true, fb.fb.r.jitTest.snapAsked
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
