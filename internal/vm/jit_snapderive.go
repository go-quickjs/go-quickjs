//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "github.com/go-quickjs/go-quickjs/internal/jit/abi"

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
// A derivation may append to its effects no link but one its compile's
// gathers made (gatherLink): a link is the runtime's to make, and the
// runtime's goroutine holds those the job may call through while it is
// pending (holdsLink). Nor may an answer of it name a pool or a leaf
// (jitSnapPools, Leaf's), which the snapshot asks of at the goroutine once
// the asking is done. What it would write of the runtime's -- the
// learning's appends -- it keeps, for its family's finish to write once
// the compile is done with (jitSnapFinishers).

// jitSnapData is what a snapshot's gathers copied, by method family, for
// its derivations.
type jitSnapData struct {
	// gathered marks a snapshot whose gathers have run, which its
	// derivations derive from; bound, one whose gathers' answers are put at
	// its program's PCs (bind); derived, the root's, a compile whose
	// derivations have run; finished, the root's, one whose finishes have
	// (finish).
	gathered, bound, derived, finished bool
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
	// consts is the constants' and captured bindings' (jit_snapconst.go).
	consts jitConstData
	// native is the learning's methods' (jit_snapnative.go), calls the
	// calls' (jit_snapcalls.go).
	native jitNativeData
	calls  jitCallsData
	// rt is the runtime the gathers ran on, the root's: the closures'
	// copies' (closureCopy).
	rt *Runtime
	// copies, the root's, are the closures' copies its compile's gathers
	// made (closureCopy), found by a search of copied while they are few,
	// by copies once they are many.
	copied []jitCopied
	copies map[*closure]*jitClosureCopy
	// links, the root's, are the links its compile's gathers made
	// (gatherLink): the only ones a derivation's effects may name.
	links map[*jitLink]struct{}
	// deriving, the root's, marks a compile whose derivations have begun,
	// and derivedLinks, the root's, what their effects took back of links
	// (take): written on the worker, apart from gained's links, which the
	// runtime's goroutine reads while the job is pending (holdsLink).
	deriving     bool
	derivedLinks []*jitLink
	// pools, the root's, are the pools its gathers found that a
	// derivation's answers may name, whose roots the snapshot asks ahead
	// (jitRegSnapshotOf's VirtualRoot) with the answers'.
	pools []*abi.ObjectPool
	// leaves, the root's, are the leaves its gathers found a derivation's
	// answers may name, whose kinds the snapshot asks ahead
	// (jitRegSnapshotOf's LeafKind) with the answers'.
	leaves []uintptr
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
	// sites are its call sites' facts, by site (jitCallSiteFacts): what
	// of the heap a call's native targets read.
	sites []jitCallSiteFact
}

// jitCopiedPoly is a cache's earlier entries copied (propPoly), with
// theirs (propSeen): copied together, each closure's are one allocation.
type jitCopiedPoly struct {
	p propPoly
	s propSeen
}

// jitCopied is a closure's copy, by its closure (jitSnapData's copied).
type jitCopied struct {
	cl *closure
	c  *jitClosureCopy
}

// closureCopy is cl's copy (jitClosureCopy), of d, the compile's root's
// data: made at its first ask, its caches copied whole.
func (d *jitSnapData) closureCopy(cl *closure) *jitClosureCopy {
	if d.copies != nil {
		if c := d.copies[cl]; c != nil {
			return c
		}
	} else {
		for _, x := range d.copied {
			if x.cl == cl {
				return x.c
			}
		}
	}
	c := &jitClosureCopy{}
	c.ic, c.polys = jitCopyCaches(cl.ic)
	c.calls = jitCallsCopy(d.rt, cl)
	c.sites = jitCallSiteFacts(d.rt, cl, c.calls)
	switch {
	case d.copies != nil:
		d.copies[cl] = c
	case len(d.copied) < 8:
		d.copied = append(d.copied, jitCopied{cl, c})
	default:
		// Many: found by a map from here.
		d.copies = make(map[*closure]*jitClosureCopy, 2*len(d.copied)+1)
		for _, x := range d.copied {
			d.copies[x.cl] = x.c
		}
		d.copies[cl] = c
	}
	return c
}

// jitCopyCaches is a copy of ic, each entry's earlier entries
// (propPoly) a copy of its own, their earlier entries (propSeen) with
// them, of what an answer or a callee's seen feedback (jitSeenFeedbackInto)
// reads of them: the interpreter changes them in place.
func jitCopyCaches(ic []propCache) (out []propCache, polys []jitCopiedPoly) {
	if len(ic) == 0 {
		return nil, nil
	}
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
// worker, by stage (jitSnapDeriveStage). Each family appends its own in an
// init of its file.
var (
	jitSnapFuncGatherers []func(s *jitSnapshot, fb *jitRegFeedback)
	jitSnapDerivers      [jitDeriveStages][]func(s *jitSnapshot)
)

// jitSnapDeriveStage is when a family's derivation runs among the others'
// of a snapshot, a stage's after every earlier stage's, whatever the
// files they are in: what a derivation appends to the copy of the
// learning (jitCallSrc's learned), a later stage's reads.
type jitSnapDeriveStage uint8

const (
	// jitDeriveCopies are the derivations that read their copies alone
	// and append to no learned list: the property sites', the learning's
	// methods', the calls' built-ins'.
	jitDeriveCopies jitSnapDeriveStage = iota
	// jitDeriveInline is Inline's, which appends to the learned calls.
	jitDeriveInline
	// jitDeriveTargets is NativeCalls', which appends to them too, and
	// the methods that read what both appended (Resumed).
	jitDeriveTargets
	jitDeriveStages
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

// jitSnapAskedAt are the families' choices of the derived methods asked
// of the feedback at a PC all the same, on the runtime's goroutine, in the
// asking's order among the methods there (askReg): a bit by method, where
// the family's gather found the answer reads the heap or makes what the
// runtime keeps, and the method's question at the PC may depend on what
// an earlier method's there appended.
var jitSnapAskedAt []func(s *jitSnapshot, pc int) uint64

// jitSnapBinders are the families' bindings of what their gathers took,
// keyed by register pc, to the PCs of the program the function is lowered
// to (s's p): the answers a gather asked of the heap at a register pc,
// put into their tables at each PC lowered of it whose op asks them. Each
// runs once a snapshot, once both are done -- on the goroutine where the
// program is lowered there, on the worker otherwise -- before the
// derivations (bind). A family appends its own in an init of its file.
//
// A gather reads the register code (s.rc), never the program (s.p, s.ops):
// a callee's snapshot may be gathered before anything lowers it, its
// lowering made on the worker.
var jitSnapBinders []func(s *jitSnapshot)

// bind runs the families' bindings of s, gathered, to its program, once.
func (s *jitSnapshot) bind() {
	if s.d.bound || !s.d.gathered || s.p == nil {
		return
	}
	s.d.bound = true
	for _, b := range jitSnapBinders {
		b(s)
	}
}

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
	s.d.derived, s.d.deriving = true, true
	// The snapshots whose gathers ran, in the order they ran: a callee
	// that asked nothing (nestReg) has none.
	for _, c := range s.d.nodes {
		c.bind()
		c.runDerivers(&jitSnapDerivers)
		c.a.seal()
	}
}

// runDerivers runs derivers' derivations of s, stage by stage.
func (s *jitSnapshot) runDerivers(derivers *[jitDeriveStages][]func(s *jitSnapshot)) {
	for _, stage := range derivers {
		for _, d := range stage {
			d(s)
		}
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

// gatherLink keeps l, a link a gather of s made or found on the runtime's
// goroutine (regLinkOf), in the root's gained links, which the runtime
// lets go of none of while the compile is pending (holdsLink), and among
// those a derivation's effects may name (endDerived).
func (s *jitSnapshot) gatherLink(l *jitLink) *jitLink {
	root := s.root
	if _, ok := root.d.links[l]; ok {
		return l
	}
	if root.d.links == nil {
		root.d.links = map[*jitLink]struct{}{}
	}
	root.d.links[l] = struct{}{}
	root.gained.links = append(root.gained.links, l)
	return l
}

// endDerived is end for a derivation's question, its effects appended to
// sink since before: no link among them but those its compile's gathers
// made (gatherLink), which the runtime's goroutine holds while the job is
// pending; a link is the runtime's to make.
func (s *jitSnapshot) endDerived(sink *jitFeedback, before [2]jitSnapLens) int32 {
	s.derivedLinks(sink.links, before[0][3])
	if k := sink.keep(); k != sink {
		s.derivedLinks(k.links, before[1][3])
	}
	return s.end(sink, before)
}

// derivedLinks panics where links past n, a derivation's, has one its
// compile's gathers did not make.
func (s *jitSnapshot) derivedLinks(links []*jitLink, n int) {
	if len(links) <= n {
		return
	}
	for _, l := range links[n:] {
		if _, ok := s.root.d.links[l]; !ok {
			panic("jit: a snapshot's derivation appended a link its gather did not make")
		}
	}
}
