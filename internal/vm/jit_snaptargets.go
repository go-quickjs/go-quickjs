//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// The calls' targets of a snapshot's derived methods (jit_snapderive.go):
// NativeCalls, the native calls a call makes (jitCallCtx's nativeCalls),
// derived on the worker from a copy of what they are made of -- the
// closure's call caches (jitClosureCopy's calls), the headers of what the
// code learned of its calls (jitLearned's lists, which only grow or are
// made anew), what of each target the heap holds (jitCallFacts), the links
// and the realm's built-ins -- through jitCallSrc, the feedback's answer on
// the spot the same code over the live state; and with it the methods that
// read what NativeCalls appends to the learning: ViaLeaf at its PC, Resumed
// once the PCs are asked (selfCallPCs), and EntryGeneric beside Resumed.
//
// The answer appends to the learning (regCallTargets), which a concurrent
// compile does not write, as V8's does not write feedback: the derivation
// appends to its copy, and the family's finish (jitSnapFinishers) writes
// the appends into the function's lists once the compile is done with,
// after what the lists had when they were copied and before what exits
// learned meanwhile, as the asking had placed them. Where the answer reads
// the heap or makes what the runtime keeps -- a binding's value
// (regViaNow, regConstructorNow), a pool, a literal, a built-in's checks,
// register code (regOf) -- the gather asks it on the runtime's goroutine
// (callsLive) before it copies the learning, so that the copy holds what
// that asking appended; it asks ViaLeaf there too, where the code learned
// one at the PC, and answers it nowhere else.

// jitTargetsData is what a snapshot's gather took for NativeCalls'
// derivation (jitCallsData's tg).
type jitTargetsData struct {
	// gathered marks a snapshot whose NativeCalls the worker derives;
	// without it the gather asked them all, and Resumed and EntryGeneric
	// are asked as before (snapshotStatePCs).
	gathered bool
	// live is the function's learning, learned the copy of its lists'
	// headers the derivation reads and appends to, past nNative and
	// nInline of nativeCalls and inlines: the appends the finish writes.
	live, learned    *jitEntry
	nNative, nInline int
	// late are live's nativeCalls and inlines once the snapshot's PCs
	// were asked, which the questions asked there appended to
	// (snapshotStatePCs): Resumed's.
	lateNative, lateInline []jitInline
	// selfKnown marks a feedback that had its function's calls of itself
	// already (jitFeedback's selfCallPCs), selfCalls them.
	selfKnown bool
	selfCalls []int32
	// resumed and failed are Resumed's and EntryGeneric's lists.
	resumed, failed []uint32
	// asked marks the PCs whose NativeCalls the gather asked, a bit by
	// the program's instruction.
	asked []uint64
	// facts, regs, links and seen are what the derivation's source
	// answers (jitCallCopy), as the gather found them; realm the realm's
	// built-ins.
	facts []jitTargetFacts
	regs  []jitTargetReg
	seen  []jitTargetSeen
	realm jitCallRealm
	// byFacts, byReg and bySeen index them, made on the worker as it
	// first derives.
	byFacts map[jitTargetKey]jitCallFacts
	byReg   map[*closure]jitTargetReg
	bySeen  map[int32][]jitCallee
	// regAt indexes regs on the gather's side, made once they are many.
	regAt map[*closure]int
}

// jitTargetKey is a target by its function object, closure and register
// code.
type jitTargetKey struct {
	o  *Object
	cl *closure
	rc *regCode
}

// jitTargetFacts is a target's facts (jitCallFacts).
type jitTargetFacts struct {
	k jitTargetKey
	f jitCallFacts
}

// jitTargetReg is a closure's register code as the gather found it, and
// the link native callers call it through, nil where the gather made
// none.
type jitTargetReg struct {
	cl   *closure
	rc   *regCode
	link *jitLink
}

// jitTargetSeen is jitRegSeenCallees' answer at register pc rpc.
type jitTargetSeen struct {
	rpc     int32
	callees []jitCallee
}

// jitTargetsMethods are the methods NativeCalls' family derives, and of
// them those its gather asks where they may answer (ViaLeaf).
const jitTargetsMethods uint64 = 1<<snapNativeCalls | 1<<snapViaLeaf | 1<<snapResumed | 1<<snapEntryGeneric

func init() {
	jitSnapDerived |= jitTargetsMethods
	jitSnapAskedAtGather |= 1 << snapViaLeaf
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, jitTargetsGather)
	jitSnapDerivers[jitDeriveTargets] = append(jitSnapDerivers[jitDeriveTargets], jitTargetsDerive)
	jitSnapFinishers = append(jitSnapFinishers, jitTargetsFinish)
}

// jitLearnedCopy is a copy of l's lists' headers, nativeCalls and inlines
// cut to their lengths, so that an append to them makes a list of their
// own: a derivation's, which leaves the backing arrays l and its exits
// append to as they are.
func jitLearnedCopy(l *jitEntry) *jitEntry {
	if l == nil || l.jitLearned == nil {
		return nil
	}
	c := *l.jitLearned
	c.nativeCalls = slices.Clip(c.nativeCalls)
	c.inlines = slices.Clip(c.inlines)
	return &jitEntry{jitLearned: &c}
}

// callCtx is the question of s's calls' targets of its gather's copy, nil
// where its NativeCalls were asked at the gather (jitTargetsData's
// gathered).
func (s *jitSnapshot) callCtx() *jitCallCtx {
	d := &s.d.calls
	if !d.tg.gathered {
		return nil
	}
	return &jitCallCtx{p: d.p, rc: d.rc, fn: s.fb.fn, cl: d.cl, src: jitCallCopy{s}}
}

// jitTargetsGather is the family's gather of s, fb's snapshot, after the
// calls' (jitCallsGather), whose copy of the call caches it reads: the
// NativeCalls and ViaLeaf asked where they read the heap; then the
// learning's headers, the facts of every target the copies name, their
// links, the method calls' seen callees and the realm's built-ins.
func jitTargetsGather(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.calls
	t := &d.tg
	*t = jitTargetsData{}
	if d.cl == nil || fb.rc.Typed || fb.fb.e == nil || fb.fb.e.jitLearned == nil {
		// Asked here, at every PC, Resumed and EntryGeneric as before.
		for pc, op := range s.ops {
			if jitSnapAsks[op]&(1<<snapNativeCalls) != 0 {
				jitRegSnapAskNativeCalls(s, fb, pc)
			}
			if jitSnapAsks[op]&(1<<snapViaLeaf) != 0 {
				jitRegSnapAskViaLeaf(s, fb, pc)
			}
		}
		return
	}
	r := fb.fb.r
	t.gathered, t.live, t.realm = true, fb.fb.e, r.jitCallRealmNow()
	t.learned = jitLearnedCopy(t.live)
	q := jitCallCtx{p: d.p, rc: d.rc, fn: fb.fb.fn, cl: d.cl, src: jitCallCopy{s}}
	t.asked = make([]uint64, (len(s.ops)+63)/64)
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapNativeCalls) == 0 {
			continue
		}
		if q.callsLive(pc) || jitSnapAsks[op]&(1<<snapViaLeaf) != 0 && t.viaLeafAt(int32(d.p.Origin[pc])) {
			t.asked[pc>>6] |= 1 << (pc & 63)
		}
	}
	for pc, op := range s.ops {
		if t.asked[pc>>6]&(1<<(pc&63)) == 0 {
			continue
		}
		jitRegSnapAskNativeCalls(s, fb, pc)
		if jitSnapAsks[op]&(1<<snapViaLeaf) != 0 && t.viaLeafAt(int32(d.p.Origin[pc])) {
			jitRegSnapAskViaLeaf(s, fb, pc)
		}
	}
	// The headers once that asking appended.
	t.learned = jitLearnedCopy(t.live)
	t.nNative, t.nInline = len(t.learned.nativeCalls), len(t.learned.inlines)
	t.resumed, t.failed = t.live.resumed, t.live.failedEntries
	rc := fb.rc
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapNativeCalls) == 0 || t.asked[pc>>6]&(1<<(pc&63)) != 0 || op != ir.Host || !d.p.Code[pc].Window {
			// Not a call's: no target.
			continue
		}
		rpc := int(d.p.Origin[pc])
		if rpc >= len(rc.Code) {
			continue
		}
		w := rc.Code[rpc]
		argc := int(w.X())
		if site := int(w.C()); site < len(d.copied) {
			if e := &d.copied[site]; e.callee != nil && e.cl != nil {
				t.target(s, r, e.callee, e.cl, e.rc, argc)
			}
		}
		if w.Op().MethodCall() {
			seen := r.jitRegSeenCallees(d.cl, rc, rpc, w)
			t.seen = append(t.seen, jitTargetSeen{int32(rpc), seen})
			for _, c := range seen {
				t.target(s, r, c.o, c.cl, c.cl.reg, argc)
			}
		}
	}
	for _, x := range t.learned.nativeCalls {
		if x.cl == nil || x.obj == nil || x.forEach || x.active != nil || x.pool != nil || int(x.pc) >= len(rc.Code) {
			continue
		}
		t.target(s, r, x.obj, x.cl, x.cl.reg, int(rc.Code[x.pc].X()))
	}
}

// viaLeafAt reports whether the code learned a leaf called through
// Function.prototype.call at register pc rpc (jitViaLeafSeen), where
// ViaLeaf may answer.
func (t *jitTargetsData) viaLeafAt(rpc int32) bool {
	return slices.ContainsFunc(t.live.viaLeaves, func(x jitViaLeaf) bool { return x.pc == rpc })
}

// target takes the facts of o, cl's, running rc, which a call of argc
// arguments may call natively, and of cl's register code as it is now,
// which a target the derivation learns is called with (regCallTargets'
// last loop); and the link of cl where native callers may call it so,
// through Function.prototype.call too.
func (t *jitTargetsData) target(s *jitSnapshot, r *Runtime, o *Object, cl *closure, rc *regCode, argc int) {
	reg := t.regOf(s, r, cl)
	t.facts = append(t.facts, jitTargetFacts{jitTargetKey{o, cl, rc}, r.jitRegFactsOf(o, cl, rc)})
	if rc != reg.rc {
		t.facts = append(t.facts, jitTargetFacts{jitTargetKey{o, cl, reg.rc}, r.jitRegFactsOf(o, cl, reg.rc)})
	}
	if reg.link != nil || reg.rc == nil {
		return
	}
	f := r.jitRegFactsOf(o, cl, reg.rc)
	if f.callable(cl, reg.rc, argc, t.realm.quirks) || argc >= 1 && f.callable(cl, reg.rc, argc-1, t.realm.quirks) {
		reg.link = s.gatherLink(r.jitState().regLinkOf(cl, reg.rc))
		t.setLink(cl, reg.link)
	}
}

// regOf is cl's register code as the gather found it, taken once.
func (t *jitTargetsData) regOf(s *jitSnapshot, r *Runtime, cl *closure) jitTargetReg {
	if t.regAt != nil {
		if i, ok := t.regAt[cl]; ok {
			return t.regs[i]
		}
	} else {
		for _, x := range t.regs {
			if x.cl == cl {
				return x
			}
		}
	}
	x := jitTargetReg{cl: cl, rc: cl.reg}
	t.regs = append(t.regs, x)
	if t.regAt == nil && len(t.regs) > 8 {
		// Many: found by a map from here, the gather's own.
		t.regAt = make(map[*closure]int, 2*len(t.regs))
		for i, y := range t.regs {
			t.regAt[y.cl] = i
		}
	} else if t.regAt != nil {
		t.regAt[cl] = len(t.regs) - 1
	}
	return x
}

// setLink keeps l as cl's link.
func (t *jitTargetsData) setLink(cl *closure, l *jitLink) {
	if t.regAt != nil {
		t.regs[t.regAt[cl]].link = l
		return
	}
	for i := range t.regs {
		if t.regs[i].cl == cl {
			t.regs[i].link = l
		}
	}
}

// index makes t's indexes, on the worker.
func (t *jitTargetsData) index() {
	if t.byFacts != nil {
		return
	}
	t.byFacts = make(map[jitTargetKey]jitCallFacts, len(t.facts))
	for _, f := range t.facts {
		t.byFacts[f.k] = f.f
	}
	t.byReg = make(map[*closure]jitTargetReg, len(t.regs))
	for _, x := range t.regs {
		t.byReg[x.cl] = x
	}
	t.bySeen = make(map[int32][]jitCallee, len(t.seen))
	for _, x := range t.seen {
		t.bySeen[x.rpc] = x.callees
	}
}

// jitCallCopy is s's gather's copy of what its calls' targets are derived
// from (jitCallSrc), which the worker reads: a question of anything the
// gather did not take is its mistake, and panics.
type jitCallCopy struct{ s *jitSnapshot }

func (c jitCallCopy) data() *jitTargetsData { return &c.s.d.calls.tg }

func (c jitCallCopy) learned() *jitEntry { return c.data().learned }

func (c jitCallCopy) calls(site int) []regCallCache { return c.s.d.calls.copied }

func (c jitCallCopy) target(o *Object, cl *closure, rc *regCode) jitCallFacts {
	t := c.data()
	t.index()
	f, ok := t.byFacts[jitTargetKey{o, cl, rc}]
	if !ok {
		panic("jit: a snapshot's derivation of a call met a target its gather did not take")
	}
	return f
}

func (c jitCallCopy) reg(cl *closure) *regCode {
	t := c.data()
	t.index()
	x, ok := t.byReg[cl]
	if !ok {
		panic("jit: a snapshot's derivation of a call met a closure its gather did not take")
	}
	return x.rc
}

func (c jitCallCopy) link(cl *closure, rc *regCode) *jitLink {
	t := c.data()
	t.index()
	if x := t.byReg[cl]; x.link != nil && x.rc == rc {
		return x.link
	}
	panic("jit: a snapshot's derivation of a call met a link its gather did not make")
}

func (c jitCallCopy) seenCallees(rpc int, w bytecode.RInstr) []jitCallee {
	t := c.data()
	t.index()
	seen, ok := t.bySeen[int32(rpc)]
	if !ok {
		panic("jit: a snapshot's derivation of a call met a method call its gather did not see")
	}
	return seen
}

func (c jitCallCopy) realm() jitCallRealm { return c.data().realm }

func (c jitCallCopy) keep() *jitFeedback { return c.s.sinkOf().keep() }

func (c jitCallCopy) live() *jitRegFeedback { return nil }

// late takes, for s, the snapshot of fb, what fb's learning has once its
// PCs were asked (jitTargetsData's late): what Resumed is derived from,
// with what the derivations append. Asked there before, as the gather
// is, Resumed and EntryGeneric are not where the worker derives them.
func (t *jitTargetsData) late(fb *jitRegFeedback) {
	if e := fb.fb.e; e != nil {
		t.lateNative, t.lateInline = e.nativeCalls, e.inlines
	}
	if fb.fb.selfKnown {
		// Known already (selfCallPCs), which the feedback answers from:
		// as it is.
		t.selfKnown, t.selfCalls = true, slices.Clone(fb.fb.selfCalls)
	}
}

// jitTargetsDerive puts s's answers of NativeCalls, at every PC whose op
// asks it that the gather did not ask, from what it took, their effects
// appended to the snapshot's sink; then Resumed's and EntryGeneric's,
// at the PCs where they answer.
func jitTargetsDerive(s *jitSnapshot) {
	d := &s.d.calls
	t := &d.tg
	if !t.gathered {
		return
	}
	t.index()
	q := s.callCtx()
	sink := s.sinkOf()
	a := &s.a
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapNativeCalls) == 0 || t.asked[pc>>6]&(1<<(pc&63)) != 0 {
			continue
		}
		at := int64(pc)
		before := s.begin(sink)
		r0 := q.nativeCalls(pc)
		eff := s.endDerived(sink, before)
		ok := len(r0) != 0
		s.heard(snapNativeCalls, at, ok, r0)
		if ok || eff != 0 {
			a.NativeCalls.reserve(int(s.room[snapNativeCalls]))
			a.NativeCalls.put(at, eff, ok, jitSnapAnsNativeCalls{r0})
		}
	}
	// Resumed after a call of the function itself, the learning's native
	// and inlined calls (selfCallPCs): those the PCs' questions appended
	// on the runtime's goroutine (late), and the derivations' -- all of
	// them, NativeCalls' and any other family's, derived before.
	n := int64(len(d.rc.Code))
	put := func(m jitSnapMethod, pc int64) {
		if pc < 0 || pc >= n {
			return
		}
		s.heard(m, pc, true, true)
		if m == snapResumed {
			a.Resumed.put(pc, 0, true, jitSnapAnsResumed{true})
		} else {
			a.EntryGeneric.put(pc, 0, true, jitSnapAnsEntryGeneric{true})
		}
	}
	for _, pc := range t.resumed {
		put(snapResumed, int64(pc))
	}
	if t.selfKnown {
		for _, pc := range t.selfCalls {
			put(snapResumed, int64(pc)+1)
		}
	} else {
		fn := s.fb.fn
		for _, list := range [...][]jitInline{t.lateNative, t.learned.nativeCalls[t.nNative:], t.lateInline, t.learned.inlines[t.nInline:]} {
			for _, x := range list {
				if x.cl != nil && x.cl.fn == fn {
					put(snapResumed, int64(x.pc)+1)
				}
			}
		}
	}
	for _, pc := range t.failed {
		put(snapEntryGeneric, int64(pc))
	}
}

// jitTargetsFinish writes into s's function's learning what its
// derivations appended to the copy (jitSnapFinishers), on the runtime's
// goroutine, once the compile is done with: after what the lists had when
// they were copied, before what exits appended meanwhile (jitLearnedMerge).
func jitTargetsFinish(s *jitSnapshot) {
	t := &s.d.calls.tg
	if !t.gathered || t.learned == nil {
		return
	}
	l := t.live
	l.nativeCalls = jitLearnedMerge(l.nativeCalls, t.nNative, t.learned.nativeCalls[t.nNative:])
	l.inlines = jitLearnedMerge(l.inlines, t.nInline, t.learned.inlines[t.nInline:])
}

// jitLearnedMerge is live, a learned list of calls that had n entries when
// a compile's snapshot copied it, with add, what the compile's
// derivations appended to the copy, put where the asking had put them:
// after those n, before what was appended since -- what exits learned
// meanwhile, and what the questions asked on the runtime's goroutine
// appended (jitTargetsData's late), which add has again. Of each target
// one entry is kept (jitLearnedSame), the first. A list that has every one
// of add already is live as it is: the questions on the goroutine had
// appended them, as they were asked. Readers find a call's targets in the
// order of its entries (regCallTargets' last loop, jitCallTargets' cap),
// so the order of a call's entries is the asking's; entries of different
// calls are read apart.
func jitLearnedMerge(live []jitInline, n int, add []jitInline) []jitInline {
	if len(add) == 0 {
		return live
	}
	missing := false
	for _, x := range add {
		if !slices.ContainsFunc(live, func(y jitInline) bool { return jitLearnedSame(x, y) }) {
			missing = true
			break
		}
	}
	if !missing {
		return live
	}
	n = min(n, len(live))
	out := make([]jitInline, 0, len(live)+len(add))
	out = append(out, live[:n]...)
	for _, x := range add {
		if !slices.ContainsFunc(out, func(y jitInline) bool { return jitLearnedSame(x, y) }) {
			out = append(out, x)
		}
	}
	for _, x := range live[n:] {
		if !slices.ContainsFunc(out[n:], func(y jitInline) bool { return jitLearnedSame(x, y) }) {
			out = append(out, x)
		}
	}
	return out
}

// jitLearnedSame reports whether a and b, entries of a learned list of
// calls, are of the same target: the same call, object, way of calling --
// through Function.prototype.call or not, from a pool or not -- and loop.
func jitLearnedSame(a, b jitInline) bool {
	return a.pc == b.pc && a.obj == b.obj && a.via == b.via && (a.pool != nil) == (b.pool != nil) && a.forEach == b.forEach
}
