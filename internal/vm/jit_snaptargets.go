//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
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
	// eager one whose NativeCalls and ViaLeaf are asked at every PC
	// (jitTargetsAskedAt). Without gathered Resumed and EntryGeneric are
	// asked as before (snapshotStatePCs).
	gathered, eager bool
	// live is the function's learning, learned the copy of its lists'
	// headers the derivation reads and appends to, past nNative and
	// nInline of nativeCalls and inlines: the appends the finish writes.
	live, learned    *jitEntry
	nNative, nInline int
	// late are live's nativeCalls and inlines once the snapshot's PCs
	// were asked, which the questions asked there appended to
	// (snapshotStatePCs): Resumed's.
	lateNative, lateInline []jitInline
	// natives and inlines are the appends to the learned lists, made
	// ready for the finish on the worker (prepare), where prepared.
	prepared         bool
	natives, inlines jitLearnedPrepared
	// selfKnown marks a feedback that had its function's calls of itself
	// already (jitFeedback's selfCallPCs), selfCalls them.
	selfKnown bool
	selfCalls []int32
	// resumed and failed are Resumed's and EntryGeneric's lists.
	resumed, failed []uint32
	// asked marks the PCs whose NativeCalls the gather asked, a bit by
	// the program's instruction.
	asked []uint64
	// facts, regs and sites -- the closure's copy's (jitCallSiteFacts)
	// -- are what the derivation's source answers (jitCallCopy), as the
	// gather found them; realm the realm's built-ins.
	facts []jitTargetFacts
	regs  []jitTargetReg
	sites []jitCallSiteFact
	realm jitCallRealm
	// constructs and rooms are the constructions' facts, by function
	// object and argument count, and their pools' rooms, of the learned
	// constructions the derivation reads (jitCallSrc's construct,
	// poolRoom).
	constructs []jitTargetConstruct
	rooms      []jitTargetRoom
	// byFacts and byReg index them, made on the worker as it first
	// derives.
	byFacts map[jitTargetKey]jitCallFacts
	byReg   map[*closure]jitTargetReg
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

// jitTargetConstruct is a construction's facts with o of argc arguments.
type jitTargetConstruct struct {
	o    *Object
	argc int
	c    jitCallConstruct
}

// jitTargetRoom is a pool's room as the gather found it.
type jitTargetRoom struct {
	pool *abi.ObjectPool
	room uint64
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
	jitSnapAskedAt = append(jitSnapAskedAt, jitTargetsAskedAt)
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
		// Asked at every PC, where the asking asks them (jitTargetsAskedAt),
		// Resumed and EntryGeneric as before.
		t.eager = true
		return
	}
	r := fb.fb.r
	t.gathered, t.live, t.realm = true, fb.fb.e, r.jitCallRealmNow()
	t.learned = jitLearnedCopy(t.live)
	q := jitCallCtx{p: d.p, rc: d.rc, fn: fb.fb.fn, cl: d.cl, src: jitCallCopy{s}}
	t.asked = make([]uint64, (len(s.ops)+63)/64)
	t.sites = s.root.d.closureCopy(d.cl).sites
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapNativeCalls) == 0 {
			continue
		}
		if q.callsLive(pc) || !t.siteKnown(&q, pc) || jitSnapAsks[op]&(1<<snapViaLeaf) != 0 && t.viaLeafAt(int32(d.p.Origin[pc])) {
			t.asked[pc>>6] |= 1 << (pc & 63)
		}
	}
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
		if site := int(w.C()); site < len(d.copied) && w.Op() != bytecode.RNewW && w.Op() != bytecode.RApplyArgs {
			if e := &d.copied[site]; e.callee != nil && e.cl != nil {
				t.target(s, r, e.callee, e.cl, e.rc, argc)
			}
		}
		if x := t.site(rpc, w); x != nil {
			for _, c := range x.seen {
				t.target(s, r, c.o, c.cl, c.cl.reg, argc)
			}
			if x.cl != nil && x.rc != nil {
				t.target(s, r, x.now, x.cl, x.rc, argc)
			}
		}
	}
	for _, x := range t.learned.nativeCalls {
		if x.obj == nil || x.forEach || x.active != nil || int(x.pc) >= len(rc.Code) {
			continue
		}
		argc := int(rc.Code[x.pc].X())
		if x.pool != nil {
			// A construction's, from its pool: what its site reads
			// (jitCallCtx's callSite, allocSite), and its pool's root
			// (jitSnapshot's pools).
			t.rooms = append(t.rooms, jitTargetRoom{x.pool, x.pool.Room})
			s.root.d.pools = append(s.root.d.pools, x.pool)
			if x.cl == nil {
				continue
			}
			c := r.jitRegConstructOf(x.obj, argc)
			t.constructs = append(t.constructs, jitTargetConstruct{x.obj, argc, c})
			reg := t.regOf(s, r, x.cl)
			t.facts = append(t.facts, jitTargetFacts{jitTargetKey{x.obj, x.cl, reg.rc}, r.jitRegFactsOf(x.obj, x.cl, reg.rc)})
			if c.ok && c.cl == x.cl && c.parent == x.parent && reg.rc != nil && reg.link == nil {
				t.setLink(x.cl, s.gatherLink(r.jitState().regLinkOf(x.cl, reg.rc)))
			}
			continue
		}
		if x.cl == nil {
			continue
		}
		t.target(s, r, x.obj, x.cl, x.cl.reg, argc)
	}
}

// jitTargetsAskedAt is which of NativeCalls and ViaLeaf the asking asks at
// the program's instruction pc of s on the runtime's goroutine, in its
// order among the methods there (askReg, jitSnapAskedAt): where the
// gather found the answer reads the heap, or at every PC of a snapshot it
// does not derive. Asked there, after Inline's question of the PC, as the
// feedback was asked: a construction's NativeCalls finds the pool
// Inline's question learned (regAllocSite), and asked before it, a
// snapshot's construction was Go's where the next one's was made from
// the pool.
func jitTargetsAskedAt(s *jitSnapshot, pc int) uint64 {
	t := &s.d.calls.tg
	switch {
	case t.eager:
		return 1<<snapNativeCalls | 1<<snapViaLeaf
	case !t.gathered || t.asked[pc>>6]&(1<<(pc&63)) == 0:
		return 0
	case t.viaLeafAt(int32(s.d.calls.p.Origin[pc])):
		return 1<<snapNativeCalls | 1<<snapViaLeaf
	}
	return 1 << snapNativeCalls
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
}

// siteKnown reports whether the facts of the call at the program's
// instruction pc, if it is one, hold what its derivation may ask
// (jitCallSrc's now, seenCallees): its site's, of its register pc alone,
// its binding read where the call is one the interpreter has not filled
// and the learning, as the gather copied it, knows nothing at the call.
// The derivation's learning has no fewer entries at the call: none, an
// append there by what it derived before it.
func (t *jitTargetsData) siteKnown(q *jitCallCtx, pc int) bool {
	w, op, ok := jitRegAt(q.p, q.rc, pc)
	if !ok || op != ir.Host || !q.p.Code[pc].Window {
		return true
	}
	switch w.Op() {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT:
	default:
		return true
	}
	rpc := int(q.p.Origin[pc])
	x := t.site(rpc, w)
	if x == nil {
		return false
	}
	method, argc := w.Op().MethodCall(), int(w.X())
	l := t.learned
	if method && !x.seenAsked {
		return false
	}
	if l == nil || !jitCallUnfilled(q.src.calls(int(w.C())), w, method, argc, true) {
		return true
	}
	at := func(list []jitInline) bool {
		return slices.ContainsFunc(list, func(y jitInline) bool { return int(y.pc) == rpc })
	}
	wants := method && argc >= 1 && !at(l.nativeCalls) || !method && !at(l.nativeCalls) && !at(l.inlines)
	return !wants || x.asked
}

// site is the facts of the call w at register pc rpc, nil for none.
func (t *jitTargetsData) site(rpc int, w bytecode.RInstr) *jitCallSiteFact {
	if i := int(w.C()); i < len(t.sites) && int(t.sites[i].rpc) == rpc {
		return &t.sites[i]
	}
	return nil
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
	x := c.data().site(rpc, w)
	if x == nil || !x.seenAsked {
		panic("jit: a snapshot's derivation of a call met a method call its gather did not see")
	}
	return x.seen
}

func (c jitCallCopy) now(rpc int, w bytecode.RInstr, method bool) (*Object, *closure, *regCode) {
	x := c.data().site(rpc, w)
	if x == nil || !x.asked {
		panic("jit: a snapshot's derivation of a call met a binding its gather did not read")
	}
	return x.now, x.cl, x.rc
}

func (c jitCallCopy) construct(o *Object, argc int) jitCallConstruct {
	for _, x := range c.data().constructs {
		if x.o == o && x.argc == argc {
			return x.c
		}
	}
	panic("jit: a snapshot's derivation of a construction met a function its gather did not take")
}

func (c jitCallCopy) poolRoom(pool *abi.ObjectPool) uint64 {
	for _, x := range c.data().rooms {
		if x.pool == pool {
			return x.room
		}
	}
	panic("jit: a snapshot's derivation of a construction met a pool its gather did not take")
}

func (c jitCallCopy) realm() jitCallRealm { return c.data().realm }

func (c jitCallCopy) keep() *jitFeedback { return c.s.sinkOf().keep() }

func (c jitCallCopy) live() *jitRegFeedback { return nil }

// late takes, for s, the snapshot of fb, what fb's learning has once its
// PCs were asked (jitTargetsData's late): what Resumed is derived from,
// with what the derivations append. Asked there before, as the gather
// is, Resumed and EntryGeneric are not where the worker derives them.
func (t *jitTargetsData) late(fb *jitRegFeedback) {
	// The learning the derivation reads and appends to, copied here, once
	// the PCs' questions on the runtime's goroutine have appended to it:
	// Inline's at a PC, which the asking had asked before NativeCalls
	// there, among them. Copied at the gather, a derivation had not known
	// the callee an Inline inlined at the PC (its inlines), and learned
	// the binding's function there too, unlike the asking.
	l := t.live
	t.lateNative, t.lateInline = l.nativeCalls, l.inlines
	t.learned = jitLearnedCopy(l)
	t.nNative, t.nInline = len(t.learned.nativeCalls), len(t.learned.inlines)
	t.resumed, t.failed = l.resumed, l.failedEntries
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
		for _, list := range [...][]jitInline{t.learned.nativeCalls, t.learned.inlines} {
			for i := range list {
				x := &list[i]
				if x.cl != nil && x.cl.fn == fn {
					put(snapResumed, int64(x.pc)+1)
				}
			}
		}
	}
	for _, pc := range t.failed {
		put(snapEntryGeneric, int64(pc))
	}
	// The appends made ready for the finish, here, on the worker: the
	// targets' stage is the last that appends.
	t.prepare()
}

// jitTargetsFinish writes into s's function's learning what its
// derivations appended to the copy (jitSnapFinishers), on the runtime's
// goroutine, once the compile is done with, as the worker prepared it
// (jitLearnedPrepared): nothing where the asking on the goroutine had
// appended it all, the prepared list where the list is as the PCs' asking
// left it, and only where an exit learned meanwhile the appends put in
// among what it has (jitLearnedMerge).
func jitTargetsFinish(s *jitSnapshot) {
	t := &s.d.calls.tg
	if !t.gathered || t.learned == nil {
		return
	}
	if !t.prepared {
		t.prepare()
	}
	l := t.live
	l.nativeCalls = t.natives.finish(l.nativeCalls, t.nNative)
	l.inlines = t.inlines.finish(l.inlines, t.nInline)
}

// jitLearnedPrepared is what a compile's derivations appended to a learned
// list of calls, made ready on the worker for the finish to write: add,
// the appends the list had not when its PCs' asking was done (late), and
// merged, late with them after it, or nil where there are none.
type jitLearnedPrepared struct {
	late, add, merged []jitInline
}

// prepare makes t's appends ready for its finish (jitLearnedPrepared), on
// the worker, once every derivation has appended: what each list's
// derivations appended past what it had when copied, less what the PCs'
// asking on the runtime's goroutine appended to the live list (late), and
// the list that is late with them after. The appends and late's are of
// different calls -- a call's asking on the goroutine appended what its
// derivation does -- so that each call's entries are in the asking's order.
func (t *jitTargetsData) prepare() {
	t.prepared = true
	t.natives = jitLearnedPrepare(t.lateNative, t.nNative, t.learned.nativeCalls[t.nNative:])
	t.inlines = jitLearnedPrepare(t.lateInline, t.nInline, t.learned.inlines[t.nInline:])
}

// jitLearnedPrepare is the jitLearnedPrepared of add, the appends to a list
// that had n entries when copied and late once its PCs were asked.
func jitLearnedPrepare(late []jitInline, n int, add []jitInline) jitLearnedPrepared {
	p := jitLearnedPrepared{late: late}
	if len(add) == 0 {
		return p
	}
	since := late[min(n, len(late)):]
	for i := range add {
		if !jitLearnedHas(since, &add[i]) {
			p.add = append(p.add, add[i])
		}
	}
	if len(p.add) != 0 {
		p.merged = make([]jitInline, 0, len(late)+len(p.add))
		p.merged = append(append(p.merged, late...), p.add...)
	}
	return p
}

// finish is live, a learned list that had n entries when copied, with p's
// appends: live as it is where there are none; p's merged where live is
// as the PCs' asking left it; else merged in (jitLearnedMerge).
func (p *jitLearnedPrepared) finish(live []jitInline, n int) []jitInline {
	switch {
	case len(p.add) == 0:
		return live
	case len(live) == len(p.late) && unsafe.SliceData(live) == unsafe.SliceData(p.late):
		return p.merged
	}
	return jitLearnedMerge(live, n, p.add)
}

// jitLearnedMerge is live, a learned list of calls that had n entries when
// a compile's snapshot copied it, with add, what the compile's
// derivations appended to the copy, put where the asking had put them:
// after those n, before what was appended since -- what exits learned
// meanwhile, and what the questions asked on the runtime's goroutine
// appended (jitTargetsData's late). Of each target one entry is kept
// (jitLearnedSame): add's own are none of the n, which the derivations
// knew, so only those since are looked at. Readers find a call's targets
// in the order of its entries (regCallTargets' last loop, jitCallTargets'
// cap), so the order of a call's entries is the asking's; entries of
// different calls are read apart.
func jitLearnedMerge(live []jitInline, n int, add []jitInline) []jitInline {
	n = min(n, len(live))
	since := live[n:]
	var fresh []jitInline
	for i := range add {
		if !jitLearnedHas(since, &add[i]) {
			fresh = append(fresh, add[i])
		}
	}
	if len(fresh) == 0 {
		return live
	}
	out := make([]jitInline, 0, len(live)+len(fresh))
	out = append(out, live[:n]...)
	out = append(out, fresh...)
	return append(out, since...)
}

// jitLearnedHas reports whether list has an entry of x's target
// (jitLearnedSame).
func jitLearnedHas(list []jitInline, x *jitInline) bool {
	for i := range list {
		if jitLearnedSame(x, &list[i]) {
			return true
		}
	}
	return false
}

// jitLearnedSame reports whether a and b, entries of a learned list of
// calls, are of the same target: the same call, object, way of calling --
// through Function.prototype.call or not, from a pool or not -- and loop.
func jitLearnedSame(a, b *jitInline) bool {
	return a.pc == b.pc && a.obj == b.obj && a.via == b.via && (a.pool != nil) == (b.pool != nil) && a.forEach == b.forEach
}

// jitCallSiteFact is what a call site's native targets read of the heap
// (jitCallSrc's now and seenCallees), as a compile's gather found it, once
// for each closure (jitClosureCopy's sites, by the call's site, its C):
// the function the binding read before the call holds now -- a method
// call's G of G.call(...) (regViaNow), a plain call's or a construction's
// function (regConstructorNow) -- with its closure and register code, made
// here where the asking would make it (regOf); and the functions a method
// call's read has met (jitRegSeenCallees).
type jitCallSiteFact struct {
	// rpc is the call's register pc, -1 for a site of no call.
	rpc int32
	// asked marks a site whose binding was read (now).
	asked bool
	now   *Object
	cl    *closure
	rc    *regCode
	// seen are the method call's seen callees, where seenAsked.
	seenAsked bool
	seen      []jitCallee
}

// jitCallSiteFacts are the facts of cl's call sites (jitCallSiteFact), by
// site, of calls its call caches, calls, have not filled, as the asking
// reads them -- the binding's value at the asking's conditions, the
// learning l at none of the call's register pc -- and of every method
// call's seen callees: one pass forward over the register code, the
// register each instruction last wrote (its A) and its last get_method_w
// kept by register, where the asking scans back from each call.
func jitCallSiteFacts(r *Runtime, cl *closure, calls []regCallCache) []jitCallSiteFact {
	rc := cl.fn.RegCode()
	if rc == nil || rc.Typed {
		return nil
	}
	l := r.jitState().regLearnedOf(r.jitRegKeyOf(cl))
	fb := &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl, e: l}, rc: rc}
	var out []jitCallSiteFact
	// By register, past 1: the last instruction whose A it is, and the
	// last get_method_w that wrote it.
	last := make([]int32, 0, 64)
	method := make([]int32, 0, 64)
	at := func(list *[]int32, reg int) int {
		if reg < len(*list) {
			return int((*list)[reg]) - 1
		}
		return -1
	}
	set := func(list *[]int32, reg, q int) {
		for len(*list) <= reg {
			*list = append(*list, 0)
		}
		(*list)[reg] = int32(q + 1)
	}
	for rpc, w := range rc.Code {
		switch op := w.Op(); op {
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT, bytecode.RNewW:
			site := int(w.C())
			for len(out) <= site {
				out = append(out, jitCallSiteFact{rpc: -1})
			}
			x := &out[site]
			if x.rpc >= 0 {
				// Two calls of one site: neither has facts.
				x.rpc = -2
				break
			}
			x.rpc = int32(rpc)
			isMethod := op.MethodCall()
			if isMethod {
				x.seenAsked = true
				if q := at(&method, int(w.A())); q >= 0 && rpc-1-q <= jitRegSeenReach && (op == bytecode.RCallMW || op == bytecode.RCallMWT) {
					x.seen = r.jitRegSeenCalleesAt(cl, rc, q)
				}
			}
			if !jitCallUnfilled(calls, w, isMethod, int(w.X()), l != nil) || l == nil {
				break
			}
			has := func(list []jitInline) bool {
				return slices.ContainsFunc(list, func(y jitInline) bool { return int(y.pc) == rpc })
			}
			switch {
			case op == bytecode.RNewW:
			case isMethod && int(w.X()) >= 1 && !has(l.nativeCalls):
				x.asked = true
				if q := at(&last, int(w.A())); q >= 0 {
					x.now, x.cl, x.rc = fb.regNowTargetAt(q, w, true)
				}
			case !isMethod && !has(l.nativeCalls) && !has(l.inlines):
				x.asked = true
				if q := at(&last, int(w.A())); q >= 0 {
					x.now, x.cl, x.rc = fb.regNowTargetAt(q, w, false)
				}
			}
		}
		set(&last, int(w.A()), rpc)
		if w.Op() == bytecode.RGetMethodW {
			set(&method, int(w.A()), rpc)
		}
	}
	return out
}
