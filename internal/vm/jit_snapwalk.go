//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math/bits"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The inline sites' family of a snapshot's derived methods
// (jit_snapderive.go): Inline, ForEach and CalledClosure, derived on the
// worker (jitInlineOf, jitForEachOf, jitCalledClosureOf) from what the
// snapshot's gathers copied, as V8's Warp decides its inlining from the
// feedback its serializer copied. The callees an answer may take are found
// on the runtime's goroutine by Inline's own decision before it lowers
// (jitInlinePrefix), asked of the gathers' copies at each call by its
// register pc once a snapshot's gathers have run (discover) -- its calls'
// targets kept for the derivation, which asks no more of them -- with the
// loops' callbacks and the templates of the closures the function makes.
// Each is a node: what an answer reads of it taken there, what its caches
// have seen taken into its learning, as an answer inlining it takes it
// (jitRegSeenInlined), the hints its program is lowered of copied, and a
// snapshot of its own gathered there of its register code alone, nothing
// of it lowered -- its program made on the worker of those hints, or the
// runtime's where it has one lowered of them -- its answers derived on the
// worker where a derivation first takes it to inline it (calleeOf), as the
// asking had asked a callee where an answer named it (nestReg); its own
// calls asked so in turn, as deep as the builder inlines. What a
// derivation would write of the runtime's is kept for the finish
// (jitFinishEntries): the entry of a callee that had learned nothing, made
// where a derivation took it, and the programs the worker lowered.
//
// A compile whose asking meets what a worker cannot decide -- a forwarder
// inlined with its call's arguments, which reads its frame's receiver of
// the heap; a callee parked as the runtime returned to its host, which the
// asking unparks; the function's typed code, or a call site whose binding
// the facts do not hold, whose calls the asking asks on the runtime's
// goroutine -- asks its inline sites there, as before, its callees lowered
// there (JITGoroutineTimes' Eager counts, by kind). So does a compile while
// a method the build asks at a PC is asked neither by the worker nor by a
// gather (jitSnapNodesBlocked), which a node, its program not lowered on
// the goroutine, could not answer. A callee a derivation takes that the
// gather did not is a miss (jitSnapMiss), the compile built on the
// runtime's goroutine at its install (CalleeMisses).

// jitInlineMethods are the inline sites' methods.
const jitInlineMethods uint64 = 1<<snapInline | 1<<snapForEach | 1<<snapCalledClosure

// jitSnapInlineDepth is how deep in callees a snapshot's inline sites are
// asked: as deep as the builder inlines (ssa.MaxInlineDepth), whose most
// deeply inlined callees it asks no inline site's question
// (jitInlineData's deep).
const jitSnapInlineDepth = ssa.MaxInlineDepth

func init() {
	jitSnapDerived |= jitInlineMethods
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherInline)
	jitSnapDerivers[jitDeriveInline] = append(jitSnapDerivers[jitDeriveInline], deriveInline)
	jitSnapFinishers[jitFinishEntries] = append(jitSnapFinishers[jitFinishEntries], finishInline)
	jitSnapAskedAt = append(jitSnapAskedAt, inlineAskedAt)
}

// jitSnapNodesBlocked are the methods the build asks at a PC that are
// derived on no worker and asked by no gather: the asking asks them at the
// program's PCs, which a node has none of on the runtime's goroutine. While
// any is, a compile's inline sites are asked there (jitWalkBlocked).
func jitSnapNodesBlocked() uint64 {
	var atPC uint64
	for m, at := range jitSnapAtPC {
		if at {
			atPC |= 1 << m
		}
	}
	return jitRegSnapMethods &^ jitRegSnapStatePC &^ jitSnapDerived &^ jitSnapAskedAtGather & atPC
}

// jitWalkEager is why a compile's inline sites are asked on the runtime's
// goroutine, its callees lowered there, 0 for none.
type jitWalkEager uint8

const (
	jitWalkDerived jitWalkEager = iota
	// jitWalkBlocked: a method the build asks at a PC is neither derived
	// nor gathered (jitSnapNodesBlocked).
	jitWalkBlocked
	// jitWalkForwarder: a callee taken hands its arguments on, inlined
	// with them (jitRegForwardsInline).
	jitWalkForwarder
	// jitWalkParked: a callee taken was parked (jitRegUnpark).
	jitWalkParked
	// jitWalkTyped: the function is typed code.
	jitWalkTyped
	// jitWalkSites: the function's calls are asked on the runtime's
	// goroutine (jitTargetsData's eager): no learning, or a site whose
	// facts do not hold what a call's derivation reads.
	jitWalkSites
)

// jitInlineData is a snapshot's own of the inline sites' family: whether
// its answers are derived (derives) or, as deep as the builder inlines,
// none asked (deep); what of its learning ForEach reads that exits write
// in place or that reads the heap, as the gather found it; and its calls'
// targets, found by register pc (kept).
type jitInlineData struct {
	derives, deep, discovered bool
	// atTop are the register pcs of the loops' calls whose site is at the
	// top of its ladder (jitRegSiteAtTop).
	atTop []int32
	// loops are the learning's loops' facts, by their index among its
	// native calls.
	loops []jitLoopFact
	// kept are its calls' targets as Inline's gather found them, by
	// register pc, in order; keptLists what finding them appended to the
	// keep, runs of which each kept's is.
	kept      []jitKeptTargets
	keptLists jitSnapLists
}

// jitKeptTargets are the native targets of the call at register pc rpc,
// its window at window (ssa.CallSite's Window), as a gather found them
// (callTargets) -- how many, and the one where there is one, all Inline
// reads of them -- and what finding them appended to the keep (lists):
// what the derivation of Inline's answer there takes, finding none again.
type jitKeptTargets struct {
	rpc    int32
	window int32
	n      int
	site   ssa.CallSite
	o      *Object
	cl     *closure
	// from and to are the runs of its snapshot's keptLists, by list in
	// jitSnapLens' order, of what finding them appended.
	from, to [8]int32
}

// jitLoopFact is what ForEach reads of the i-th of the learning's native
// calls, a loop's: its built-in's kind (jitLoopKind), the index of its
// typed arrays' prototype among the realm's, and its cbLeft.
type jitLoopFact struct {
	i           int
	kind, typed int
	obj, proto  *Object
	cbLeft      bool
}

// jitWalkData is the root's of the inline sites' family: why its compile
// asks them on the goroutine (eager), or the closures its gathers' asking
// looked at (nodes, by closure) -- the callees it took among them, in the
// order it took them (order) -- and what of the heap and the runtime's an
// answer reads, as the gather found it.
type jitWalkData struct {
	eager jitWalkEager
	nodes map[*closure]*jitWalkNode
	order []*jitWalkNode
	// counts are jitCountedCallee's answers of the closures the gather's
	// asking asked them of (jitRegCallColdOf).
	counts map[*closure]bool
	// noInline, global and rootFn are the test's switch of what is never
	// inlined, the realm's global object and the compile's own function.
	noInline func(*bytecode.Function) bool
	global   Value
	rootFn   *bytecode.Function
	// missed marks a compile whose derivation took a callee its gathers
	// did not (calleeOf).
	missed bool
}

// jitWalkNode is a closure the asking looked at: its key, its register
// code as the gather found it (reg, nil for none yet, and rc, its
// function's), what an answer reads of it, and its learning (e, scratch
// where the runtime has none, made for the gather); and, of one taken
// (taken), its snapshot (s).
type jitWalkNode struct {
	cl      *closure
	key     jitKey
	reg     *regCode
	rc      *bytecode.RegCode
	e       *jitEntry
	scratch bool
	// level is how many callees deep its snapshot is, 0 for one too long
	// to inline, which no snapshot of its asks anything of.
	level int
	s     *jitSnapshot
	// readsArgs and forwards are jitRegReadsArguments' and jitRegForwards'
	// answers; mega and seenMega
	// jitMegaGetter's before and after its seen feedback was taken
	// (jitRegSeenInlined, megaOf), seen once an answer inlining it took it.
	readsArgs, forwards bool
	// notForwarded are the questions of its inlining with a call's
	// arguments that the gather answered no to (forwardsInline).
	notForwarded             []jitForwardAsk
	megaDone, mega, seenMega bool
	seen                     bool
	// merged marks one whose learning its seen feedback changed
	// (JITGoroutineTimes' UntakenLearned, where no derivation takes it).
	merged bool
	// h and cached are the hints its program is lowered with and the
	// runtime's programs of them (jitRegHintsCopy); got what the worker
	// lowered; prog the program the asking on the runtime's goroutine
	// lowered it to, where it asked a site that inlines it (askedAnew).
	h      jitcompile.Hints
	cached *jitLowered
	got    jitLowered
	prog   *ir.Program
	// taken marks one the gather took, its snapshot made; decided one a
	// derivation has taken (calleeOf), whose snapshot it asked or left
	// unasked, as nestReg decides it; touched one a derivation read
	// anything of, whose entry the finish makes.
	taken, decided, touched bool
}

// node is cl's node, or a miss.
func (w *jitWalkData) node(cl *closure, depth int) *jitWalkNode {
	if n := w.nodes[cl]; n != nil {
		n.touched = true
		return n
	}
	w.missed = true
	panic(jitSnapMiss{snapInline, 0, depth})
}

// asked reports whether c, a node's snapshot, was taken by a derivation:
// its questions asked, or left unasked, once.
func (w *jitWalkData) asked(c *jitSnapshot) bool {
	if w == nil {
		return true
	}
	for _, n := range w.order {
		if n.s == c {
			return n.decided
		}
	}
	return true
}

// askedAnew makes c, a snapshot the asking on the runtime's goroutine asked
// of a feedback of cl's (nestReg), cl's node's, its program p, lowered for
// kind; nothing where cl is none's.
func (w *jitWalkData) askedAnew(cl *closure, c *jitSnapshot, p *ir.Program, kind jitLowerKind) {
	if w == nil {
		return
	}
	n := w.nodes[cl]
	if n == nil {
		return
	}
	if !n.taken {
		n.taken = true
		w.order = append(w.order, n)
	}
	n.s, n.decided, n.e, n.scratch = c, true, c.fb.e, false
	if kind == jitLowerInline {
		n.prog = p
	}
}

// fact is cl's node, made at its first ask, of what an answer reads of it
// besides its megamorphic getters (megaOf), of r, the compile's s's.
func (w *jitWalkData) fact(r *Runtime, s *jitSnapshot, cl *closure) *jitWalkNode {
	if n := w.nodes[cl]; n != nil {
		return n
	}
	js := r.jitState()
	n := &jitWalkNode{cl: cl, key: r.jitRegKeyOf(cl), reg: cl.reg, rc: cl.fn.RegCompiled()}
	n.e = js.regLearned[n.key]
	if n.e == nil {
		n.e, n.scratch = js.jitLearnedScratch(n.key), true
	}
	if cl.closureCode != nil {
		n.readsArgs, n.forwards = jitRegReadsArguments(cl), jitRegForwards(cl)
	}
	if w.nodes == nil {
		w.nodes = map[*closure]*jitWalkNode{}
	}
	w.nodes[cl] = n
	return n
}

// megaOf takes, once, n's megamorphic getters' answer (jitMegaGetter)
// before and after what its caches have seen is taken into its learning,
// as an answer inlining it takes it (jitRegSeenInlined), which it takes
// here, k the compile's root feedback: into a runtime's entry, the
// compile's asking takes it no more; into a scratch one, the asking of a
// site that inlines it on the runtime's goroutine takes it into the entry
// it makes (askedAnew).
func (n *jitWalkNode) megaOf(r *Runtime, k *jitFeedback) {
	if n.megaDone {
		return
	}
	n.megaDone = true
	if n.reg == nil {
		return
	}
	c := n.cl
	n.mega = r.jitMegaGetterIn(c, n.reg, n.e)
	n.seenMega = n.mega
	if n.seen = slices.Contains(k.regSeen, c); !n.seen {
		if !n.scratch {
			k.regSeen = append(k.regSeen, c)
		}
		was := jitPolyWeight(n.e)
		jitSeenFeedbackInto(n.e, n.reg.rc, c.ic, c.names, r.jitDeepAddFrom)
		if n.merged = jitPolyWeight(n.e) != was; n.merged {
			// What it learned changed: its answer after, of that.
			n.seenMega = r.jitMegaGetterIn(c, n.reg, n.e)
		}
	}
}

// gatherInline is the family's gather of s, fb's snapshot: what ForEach
// reads of fb's learning, the targets' facts of its loops' callbacks.
func gatherInline(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.inline
	*d = jitInlineData{}
	root := s.root
	r := fb.fb.r
	t := &s.d.calls.tg
	if root.d.walk == nil {
		// Made at the compile's first gather, the root's.
		w := &jitWalkData{counts: map[*closure]bool{}, noInline: r.jitTest.noInline, global: r.globalThis, rootFn: fb.fb.keep().fn}
		root.d.walk = w
		if jitSnapNodesBlocked() != 0 {
			root.walkEager(jitWalkBlocked)
		}
	}
	w := root.d.walk
	if !t.gathered {
		// Asked at the PCs, on the runtime's goroutine (inlineAskedAt):
		// the function's own, its compile's.
		if s == root && w.eager == jitWalkDerived {
			why := jitWalkSites
			if fb.rc.Typed {
				why = jitWalkTyped
			}
			s.walkEager(why)
		}
		return
	}
	if w.eager != jitWalkDerived {
		return
	}
	if s.depth >= jitSnapInlineDepth {
		// As deep as the builder inlines: none of its inline sites asked.
		d.deep = true
		return
	}
	d.derives = true
	l := fb.fb.e
	for i := range l.nativeCalls {
		x := &l.nativeCalls[i]
		if !x.forEach || x.cb == nil {
			continue
		}
		f := jitLoopFact{i: i, kind: r.jitLoopKind(x.obj), typed: slices.Index(r.typedArrayProtos[:], x.proto), obj: x.obj, proto: x.proto, cbLeft: x.cbLeft}
		d.loops = append(d.loops, f)
		if jitRegSiteAtTop(l, int(x.pc)) && !slices.Contains(d.atTop, x.pc) {
			d.atTop = append(d.atTop, x.pc)
		}
		if x.cb.reg != nil && f.kind != 0 {
			// The callback's facts and link, which its call site reads
			// (jitCallCtx's callSite), of its argument count.
			argc := 3
			switch f.kind & abi.LoopKindMask {
			case abi.LoopReduce:
				argc = 4
			case abi.LoopSort:
				argc = 2
			}
			t.target(s, r, x.cbObj, x.cb, x.cb.reg, argc)
		}
	}
}

// gatherNodes takes, for s, the root of fb's compile, once its asking is
// done (jitRegSnapshotOf), the callees its snapshots' inline sites may take
// (discover): each snapshot taken, its gathers run, asked so in turn.
func (s *jitSnapshot) gatherNodes(fb *jitRegFeedback) {
	w := s.d.walk
	if w == nil || w.eager != jitWalkDerived {
		return
	}
	for i := 0; i < len(s.d.nodes) && w.eager == jitWalkDerived; i++ {
		c := s.d.nodes[i]
		if d := &c.d.inline; d.derives && !d.discovered && !c.d.dropped {
			d.discovered = true
			c.discover(fb.fb.r)
		}
	}
}

// discover asks s's inline sites, by register pc, as far as Inline decides
// before it lowers (jitInlinePrefix), of the gathers' copies, on the
// runtime's goroutine, and takes each callee an answer may inline
// (takeNode): Inline's at each call, its targets kept for the derivation
// (jitKeptTargets); each loop's callback (ForEach); and, of the compile's
// own function, each template of a closure it makes (CalledClosure), its
// register code made as that question makes it.
func (s *jitSnapshot) discover(r *Runtime) {
	q := *s.callCtx()
	q.p = nil
	src := jitInlineSnap{s: s, gather: true}
	w := s.root.d.walk
	rc := s.rc
	calls := 0
	for _, wi := range rc.Code {
		switch wi.Op() {
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW, bytecode.RCallWT, bytecode.RCallMWT:
			calls++
		}
	}
	s.d.inline.kept = make([]jitKeptTargets, 0, calls)
	for rpc, wi := range rc.Code {
		switch wi.Op() {
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW, bytecode.RCallWT, bytecode.RCallMWT:
		default:
			continue
		}
		pre, ok := jitInlinePrefix(&q, rpc, src)
		if w.eager != jitWalkDerived {
			return
		}
		if ok {
			s.takeNode(r, pre.cl)
		}
	}
	for _, f := range s.d.inline.loops {
		l := s.d.calls.tg.learned
		if f.i < len(l.nativeCalls) {
			if x := &l.nativeCalls[f.i]; x.cb != nil && x.cb.reg != nil {
				s.takeNode(r, x.cb)
			}
		}
	}
	if s == s.root {
		for _, wi := range rc.Code {
			if t, _ := jitCalledTemplate(rc, s.d.calls.cl, wi); t != nil {
				t.fn.RegCode()
				s.takeNode(r, t)
			}
		}
	}
}

// takeNode makes cl a node of s's compile, a callee of s's an answer may
// inline: its seen feedback taken (megaOf), the hints its program is
// lowered of copied; and, where it may be asked -- not too long to inline
// -- its snapshot gathered, of its register code alone. A callee
// forwarding its arguments or parked has the compile's inline sites asked
// on the runtime's goroutine (walkEager).
func (s *jitSnapshot) takeNode(r *Runtime, cl *closure) {
	root := s.root
	w := root.d.walk
	n := w.fact(r, root, cl)
	if n.taken || n.rc == nil || cl.fn == w.rootFn {
		return
	}
	k := root.fb.keep()
	if r.jitState().regParked[n.key] != nil {
		root.walkEager(jitWalkParked)
		return
	}
	n.taken = true
	w.order = append(w.order, n)
	n.megaOf(r, k)
	_, n.h, n.cached = r.jitRegHintsCopy(cl, n.rc)
	if jitSnapMayInline(cl) {
		n.level = s.depth + 1
	}
	key := jitSnapCallee{cl: cl, fn: cl.fn, root: k}
	if old := root.callees[key]; old != nil {
		// Asked already, of a site the asking asked on the runtime's
		// goroutine (an accessor's, nestReg).
		n.s, n.decided, n.e, n.scratch = old, true, old.fb.e, false
		if old.p != nil {
			n.prog = old.p
		}
		return
	}
	f := &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl, e: n.e, root: k}, rc: n.rc}
	cs := &jitSnapshot{root: root, fb: f.fb, depth: n.level}
	cs.d.node = true
	cs.reg = &jitRegSnapshot{s: cs, regs: len(n.rc.Code)}
	n.s = cs
	root.callees[key] = cs
	if n.level == 0 {
		return
	}
	cs.rf, cs.rc = f, n.rc
	cs.askOnceReg(f)
	cs.gather(f)
	tg := &cs.d.calls.tg
	if tg.gathered {
		tg.late(f)
		return
	}
	// Its calls' targets asked at its PCs (jitTargetsData's eager): lowered
	// here, and asked at them what the worker does not derive.
	r.jit.goroutineTimes.EagerLate++
	p, err := r.jitRegLower(cl, n.rc, jitLowerInline)
	if err != nil || ssa.NeverInlined(p) {
		return
	}
	n.prog = p
	f.p = p
	cs.bindProgram(p)
	cs.bind()
	cs.askPCs(f, 0)
	f.snapshotStatePCs(cs)
}

// walkEager marks s's compile's inline sites asked on the runtime's
// goroutine, for why, and asks them at the PCs of the snapshots gathered
// before, whose derivations would have answered them: the function's,
// and the callees' the asking made (an accessor's, nestReg); a node's,
// its program not lowered, is dropped, asked anew where a site inlines it.
func (s *jitSnapshot) walkEager(why jitWalkEager) {
	w := s.d.walk
	if w.eager != jitWalkDerived {
		return
	}
	w.eager = why
	if r := s.d.rt; r != nil && r.jit != nil {
		e := &r.jit.goroutineTimes
		switch why {
		case jitWalkForwarder:
			e.EagerForwarders++
		case jitWalkParked:
			e.EagerParked++
		case jitWalkTyped:
			e.EagerTyped++
		case jitWalkSites:
			e.EagerSites++
		}
	}
	for _, c := range s.d.nodes {
		d := &c.d.inline
		if !d.derives {
			continue
		}
		d.derives, d.kept = false, nil
		if c.p == nil || c.rf == nil {
			c.d.dropped = true
			continue
		}
		c.askPCs(c.rf, jitInlineMethods)
		// The learning as those questions left it (jitTargetsData's late).
		c.rf.snapshotStatePCs(c)
	}
}

// inlineAskedAt is which of the inline sites' methods the asking asks at
// the program's instruction pc of s, on the runtime's goroutine: all of
// them where its answers are not derived (jitInlineData's derives), but
// for a snapshot as deep as the builder inlines.
func inlineAskedAt(s *jitSnapshot, pc int) uint64 {
	if s.d.inline.derives || s.d.inline.deep {
		return 0
	}
	return jitInlineMethods
}

// deriveInline puts s's answers of the inline sites' methods, at every PC
// whose op asks each, in the asking's order of the methods there, from
// what the gathers took; their effects appended to the snapshot's sink.
func deriveInline(s *jitSnapshot) {
	if !s.d.inline.derives || s.p == nil {
		return
	}
	q := s.callCtx()
	src := jitInlineSnap{s: s}
	sink := s.sinkOf()
	a := &s.a
	for pc, op := range s.ops {
		asks := jitSnapAsks[op] & jitInlineMethods
		for asks != 0 {
			m := jitSnapMethod(bits.TrailingZeros64(asks))
			asks &= asks - 1
			at := int64(pc)
			before := s.begin(sink)
			switch m {
			case snapCalledClosure:
				r0, r1, r2 := jitCalledClosureOf(q, pc, src)
				eff := s.endDerived(sink, before)
				s.heard(m, at, r2, r0, r1, r2)
				if r2 || eff != 0 {
					a.CalledClosure.reserve(int(s.room[m]))
					a.CalledClosure.put(at, eff, r2, jitSnapAnsCalledClosure{r0, r1, r2})
				}
			case snapInline:
				r0, r1 := jitInlineOf(q, pc, src)
				eff := s.endDerived(sink, before)
				s.heard(m, at, r1, r0, r1)
				if r1 || eff != 0 {
					a.Inline.reserve(int(s.room[m]))
					a.Inline.put(at, eff, r1, jitSnapAnsInline{r0, r1})
				}
			case snapForEach:
				r0, r1 := jitForEachOf(q, pc, src)
				eff := s.endDerived(sink, before)
				s.heard(m, at, r1, r0, r1)
				if r1 || eff != 0 {
					a.ForEach.reserve(int(s.room[m]))
					a.ForEach.put(at, eff, r1, jitSnapAnsForEach{r0, r1})
				}
			}
		}
	}
}

// finishInline is the family's finish of s, the compile's root, for its
// nodes, before what the later finishes write into what they learned
// (jitFinishEntries): the entry of each callee a derivation read that had
// learned nothing, made of what its gather took into its scratch one, and
// the programs the worker lowered, kept as the runtime's where they are of
// its hints still (regKeepLoweredAt).
func finishInline(s *jitSnapshot) {
	w := s.d.walk
	if s != s.root || w == nil {
		return
	}
	r := s.d.rt
	if r.jit == nil {
		return
	}
	js := r.jit
	t := &js.goroutineTimes
	if w.missed {
		t.CalleeMisses++
	}
	for _, n := range w.order {
		t.GatheredCallees++
		switch {
		case n.decided:
			t.TakenCallees++
		case n.merged && !n.scratch:
			t.UntakenLearned++
		}
		if n.scratch && n.touched {
			if l := js.regLearned[n.key]; l == nil {
				js.regLearnedAdopt(n.key, n.e)
			} else {
				// Made meanwhile: the callee's targets' appends written into
				// it, its seen feedback taken anew.
				if n.s != nil {
					if tg := &n.s.d.calls.tg; tg.live == n.e {
						tg.live = l
					}
				}
				if n.reg != nil {
					r.jitRegSeenFeedback(n.cl, n.reg)
				}
			}
		}
		if slices.Contains(n.got.done[:], true) {
			r.regKeepLoweredAt(n.cl, n.rc, &n.h, &n.got)
		}
	}
}

// askPCs asks fb, at each of s's program's PCs, the methods the register
// feedback has of its op that the worker does not derive, those the
// families ask there all the same (jitSnapAskedAt), and those of also.
func (s *jitSnapshot) askPCs(fb *jitRegFeedback, also uint64) {
	for pc, op := range s.ops {
		asks := jitSnapAsks[op] & (jitRegSnapMethods&^jitRegSnapStatePC&^jitSnapDerived | also)
		for _, at := range jitSnapAskedAt {
			asks |= at(s, pc) & jitSnapAsks[op]
		}
		for asks != 0 {
			m := jitSnapMethod(bits.TrailingZeros64(asks))
			asks &= asks - 1
			if ask := jitRegSnapAskers[m]; ask != nil {
				ask(s, fb, pc)
			}
		}
	}
}

// jitPolyWeight is how much e's learning of its property sites' shapes
// holds (jitLearned's poly), which taking what a callee's caches have seen
// grows (jitSeenFeedbackInto) where it learns anything.
func jitPolyWeight(e *jitEntry) int {
	n := len(e.poly)
	for i := range e.poly {
		n += len(e.poly[i].cases) + len(e.poly[i].adds)
	}
	return n
}

// jitInlineSnap is the snapshot s's source of the inline sites' answers
// (jitInlineSrc): what its gathers and its root's took; of its gather's
// asking by register pc (discover) where gather says, which reads the
// closures it looks at on the runtime's goroutine.
type jitInlineSnap struct {
	s      *jitSnapshot
	gather bool
}

func (q jitInlineSnap) w() *jitWalkData { return q.s.root.d.walk }

// fact is cl's node: made at the gather, looked up at the derivation.
func (q jitInlineSnap) fact(cl *closure) *jitWalkNode {
	if q.gather {
		return q.w().fact(q.s.d.rt, q.s.root, cl)
	}
	return q.w().node(cl, q.s.depth)
}

func (q jitInlineSnap) forward(int) (ssa.InlineSite, bool, bool) {
	// No forwarded callee's snapshot is derived (jitWalkForwarder).
	return ssa.InlineSite{}, false, false
}

func (q jitInlineSnap) callee() bool               { return q.s != q.s.root }
func (q jitInlineSnap) rootFn() *bytecode.Function { return q.w().rootFn }

func (q jitInlineSnap) frequency(pc int) (float64, bool) {
	if q.gather {
		return q.s.d.native.frequencyAt(pc)
	}
	return q.s.d.native.frequency(pc)
}

func (q jitInlineSnap) noInlined(fn *bytecode.Function) bool {
	n := q.w().noInline
	return n != nil && n(fn)
}

func (q jitInlineSnap) megaGetter(cl *closure) bool {
	n := q.fact(cl)
	if q.gather {
		// Its answer before its seen feedback or after, as a derivation
		// takes it first: a callee either may be taken.
		n.megaOf(q.s.d.rt, q.s.root.fb.keep())
		return n.mega && n.seenMega
	}
	if !n.megaDone {
		panic(jitSnapMiss{snapInline, 0, q.s.depth})
	}
	if n.seen {
		return n.seenMega
	}
	return n.mega
}

func (q jitInlineSnap) forwardsInline(cl *closure, ctor *Object, argc int) bool {
	n := q.fact(cl)
	if q.gather {
		if q.s.d.rt.jitRegForwardsInline(cl, ctor, argc, jitInlineDepth) {
			// A forwarder inlined with its call's arguments: asked on the
			// goroutine.
			q.s.root.walkEager(jitWalkForwarder)
			return true
		}
		n.notForwarded = append(n.notForwarded, jitForwardAsk{ctor, argc})
		return false
	}
	if slices.Contains(n.notForwarded, jitForwardAsk{ctor, argc}) {
		return false
	}
	panic(jitSnapMiss{snapInline, 0, q.s.depth})
}

// jitForwardAsk is a question of a forwarder's inlining with its call's
// arguments (jitRegForwardsInline): constructed with ctor, or called, of
// argc arguments.
type jitForwardAsk struct {
	ctor *Object
	argc int
}

func (q jitInlineSnap) reg(cl *closure) *regCode { return q.fact(cl).reg }

func (q jitInlineSnap) code(fn *bytecode.Function) *bytecode.RegCode {
	// Made by the gather where an answer makes it (discover).
	return fn.RegCompiled()
}

func (q jitInlineSnap) readsArgs(cl *closure) bool { return q.fact(cl).readsArgs }
func (q jitInlineSnap) forwards(cl *closure) bool  { return q.fact(cl).forwards }

func (q jitInlineSnap) counted(cl *closure) bool {
	w := q.w()
	c, ok := w.counts[cl]
	if !ok {
		if !q.gather {
			panic(jitSnapMiss{snapInline, 0, q.s.depth})
		}
		// Of no code (closureCode) only where a test made it.
		c = cl.closureCode != nil && jitCountedCallee(cl)
		w.counts[cl] = c
	}
	return c
}

func (q jitInlineSnap) lower(cl *closure, rc *bytecode.RegCode, kind jitLowerKind) (*ir.Program, error) {
	n := q.w().node(cl, q.s.depth)
	if rc != n.rc || !n.taken {
		panic(jitSnapMiss{snapInline, 0, q.s.depth})
	}
	if kind == jitLowerInline && n.prog != nil {
		// Lowered on the runtime's goroutine, which a node's snapshot is
		// asked of (askedAnew).
		return n.prog, nil
	}
	if c := n.cached; c != nil && c.done[kind] {
		return c.progs[kind], c.errs[kind]
	}
	if !n.got.done[kind] {
		got := jitRegLowerKinds(rc, cl.fn, kind, n.h)
		for i := range got.done {
			if got.done[i] && !n.got.done[i] {
				n.got.done[i], n.got.progs[i], n.got.errs[i] = true, got.progs[i], got.errs[i]
			}
		}
	}
	return n.got.progs[kind], n.got.errs[kind]
}

func (q jitInlineSnap) seenInlined(cl *closure) {
	// Taken at the gather (megaOf); its megamorphic getters' answer is the
	// one after it from here.
	q.w().node(cl, q.s.depth).seen = true
}

func (q jitInlineSnap) calleeOf(cl *closure, p *ir.Program, fwd *jitForwardFrame) ssa.Feedback {
	if fwd != nil {
		panic(jitSnapMiss{snapInline, 0, q.s.depth})
	}
	return q.take(cl, p)
}

func (q jitInlineSnap) madeOf(cl *closure, rc *bytecode.RegCode, p *ir.Program) ssa.Feedback {
	return q.take(cl, p)
}

// take is the feedback of cl's node, its snapshot, taken to inline as p:
// at its first take, its program bound and its answers derived, where a
// callee's are asked (nestReg) -- not for a program no site inlines
// (ssa.NeverInlined) -- and no more after. Its depth is its gather's, no
// more than any depth it is taken at.
func (q jitInlineSnap) take(cl *closure, p *ir.Program) ssa.Feedback {
	n := q.w().node(cl, q.s.depth)
	c := n.s
	if c == nil {
		// Looked at, not taken: the gather's decision was otherwise.
		q.w().missed = true
		panic(jitSnapMiss{snapInline, 0, q.s.depth})
	}
	if n.decided {
		return c.reg
	}
	n.decided = true
	if c.d.gathered && c.depth < jitSnapDepth && !ssa.NeverInlined(p) {
		c.d.node = false
		if c.p == nil {
			c.bindProgram(p)
		}
		c.deriveOne()
	}
	return c.reg
}

func (q jitInlineSnap) siteAtTop(rpc int) bool {
	return slices.Contains(q.s.d.inline.atTop, int32(rpc))
}

func (q jitInlineSnap) loopKind(o *Object) int {
	for _, f := range q.s.d.inline.loops {
		if f.obj == o {
			return f.kind
		}
	}
	panic(jitSnapMiss{snapForEach, 0, q.s.depth})
}

func (q jitInlineSnap) typedProto(proto *Object) int {
	for _, f := range q.s.d.inline.loops {
		if f.proto == proto {
			return f.typed
		}
	}
	panic(jitSnapMiss{snapForEach, 0, q.s.depth})
}

func (q jitInlineSnap) cbLeft(i int, _ *jitInline) bool {
	for _, f := range q.s.d.inline.loops {
		if f.i == i {
			return f.cbLeft
		}
	}
	panic(jitSnapMiss{snapForEach, 0, q.s.depth})
}

func (q jitInlineSnap) global() Value { return q.w().global }

// kept is the call's targets at c's program's instruction pc kept by the
// gather (jitKeptTargets), what finding them appended put back in c's
// source's keep.
func (q jitInlineSnap) kept(c *jitCallCtx, pc int) (int, ssa.CallSite, *Object, *closure, bool) {
	if q.gather {
		return 0, ssa.CallSite{}, nil, nil, false
	}
	d := &q.s.d.inline
	rpc := int32(c.rpcOf(pc))
	i, ok := slices.BinarySearchFunc(d.kept, rpc, func(x jitKeptTargets, rpc int32) int { return int(x.rpc - rpc) })
	if !ok || int(d.kept[i].window) != c.window(pc) {
		// A call given a literal's copy (RMarkLitArg), whose window is its
		// own: found again.
		return 0, ssa.CallSite{}, nil, nil, false
	}
	x := &d.kept[i]
	d.keptLists.appendRuns(c.src.keep(), &x.from, &x.to)
	return x.n, x.site, x.o, x.cl, true
}

// found keeps, at the gather, the targets found of the call at register
// pc pc, what finding them appended to c's source's keep past before taken
// back.
func (q jitInlineSnap) found(c *jitCallCtx, pc int, before jitSnapLens, n int, site ssa.CallSite, o *Object, cl *closure) {
	if !q.gather {
		return
	}
	d := &q.s.d.inline
	d.kept = append(d.kept, jitKeptTargets{rpc: int32(pc), window: int32(c.window(pc)), n: n})
	x := &d.kept[len(d.kept)-1]
	if n == 1 {
		x.site, x.o, x.cl = site, o, cl
	}
	x.from, x.to = d.keptLists.cut(c.src.keep(), before)
}

// cut moves what f's lists gained past the lengths n to the ends of l's,
// f's cut back to them: the runs of l's, by list in jitSnapLens' order,
// from and to, that it moved.
func (l *jitSnapLists) cut(f *jitFeedback, n jitSnapLens) (from, to [8]int32) {
	if now := jitSnapLensOf(f); jitSnapSame(&now, &n) {
		return from, to
	}
	from = [8]int32{int32(len(l.shapes)), int32(len(l.holders)), int32(len(l.cells)), int32(len(l.links)),
		int32(len(l.inlined)), int32(len(l.made)), int32(len(l.linked)), int32(len(l.pools))}
	l.shapes, f.shapes = append(l.shapes, f.shapes[n[0]:]...), f.shapes[:n[0]]
	l.holders, f.holders = append(l.holders, f.holders[n[1]:]...), f.holders[:n[1]]
	l.cells, f.cells = append(l.cells, f.cells[n[2]:]...), f.cells[:n[2]]
	l.links, f.links = append(l.links, f.links[n[3]:]...), f.links[:n[3]]
	l.inlined, f.inlined = append(l.inlined, f.inlined[n[4]:]...), f.inlined[:n[4]]
	l.made, f.made = append(l.made, f.made[n[5]:]...), f.made[:n[5]]
	l.linked, f.linked = append(l.linked, f.linked[n[6]:]...), f.linked[:n[6]]
	l.pools, f.pools = append(l.pools, f.pools[n[7]:]...), f.pools[:n[7]]
	to = [8]int32{int32(len(l.shapes)), int32(len(l.holders)), int32(len(l.cells)), int32(len(l.links)),
		int32(len(l.inlined)), int32(len(l.made)), int32(len(l.linked)), int32(len(l.pools))}
	return from, to
}

// appendRuns appends to f's lists the runs of l's from and to (cut), each
// as it was appended.
func (l *jitSnapLists) appendRuns(f *jitFeedback, from, to *[8]int32) {
	if *from == *to {
		return
	}
	f.shapes = append(f.shapes, l.shapes[from[0]:to[0]]...)
	f.holders = append(f.holders, l.holders[from[1]:to[1]]...)
	f.cells = append(f.cells, l.cells[from[2]:to[2]]...)
	f.links = append(f.links, l.links[from[3]:to[3]]...)
	f.inlined = append(f.inlined, l.inlined[from[4]:to[4]]...)
	f.made = append(f.made, l.made[from[5]:to[5]]...)
	f.linked = append(f.linked, l.linked[from[6]:to[6]]...)
	f.pools = append(f.pools, l.pools[from[7]:to[7]]...)
}
