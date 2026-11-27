//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
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
	// eagerWhy is why a snapshot is eager: its code (typed, no closure,
	// nothing learned) or a site of two calls (jitEagerSites).
	eagerWhy uint8
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
	// What the gather made, by register pc, that an answer finds made --
	// a RegExp literal's pool (lits), a step's generators and its result
	// objects' pool (gens), a generator function's call's (genMakes) --
	// and the constructions whose pool it learned (hidden), and what it
	// read of the heap for them: super()'s targets (supers), the array
	// prototypes (protos, where protosTaken), the cache of copied data
	// properties' layouts (copies).
	lits        []jitTargetLit
	gens        []jitTargetGen
	genMakes    []jitTargetGenMake
	hidden      []jitTargetHidden
	supers      []jitTargetSuper
	protos      jitCallProtos
	protosTaken bool
	copies      uintptr
	// facts, regs and sites -- the closure's copy's (jitCallSiteFacts)
	// -- are what the derivation's source answers (jitCallCopy), as the
	// gather found them; realm the realm's built-ins.
	regs  []jitTargetReg
	sites []jitCallSiteFact
	rpcs  []int32
	realm jitCallRealm
	// constructs and rooms are the constructions' facts, by function
	// object and argument count, and their pools' rooms, of the learned
	// constructions the derivation reads (jitCallSrc's construct,
	// poolRoom).
	constructs []jitTargetConstruct
	rooms      []jitTargetRoom
	// byReg indexes regs, made on the worker as it first derives.
	byReg map[*closure]jitTargetReg
	// regAt indexes regs on the gather's side, made once they are many.
	regAt map[*closure]int
}

// jitTargetLit is the pool of the RegExp literal the call at register pc
// rpc gives its RegExp back to (jitCallSrc's literal); bound marks one a
// PC of the program asks NativeCalls at (jitTargetsBind).
type jitTargetLit struct {
	rpc   int32
	bound bool
	pool  *abi.ObjectPool
}

// jitTargetGen is genNext's answer at register pc rpc.
type jitTargetGen struct {
	rpc   int32
	bound bool
	cls   []*closure
	pool  *abi.ObjectPool
}

// jitTargetGenMake is genMake's answer at register pc rpc for o.
type jitTargetGenMake struct {
	rpc   int32
	bound bool
	o     *Object
	m     jitGenMakeFact
}

// jitTargetHidden marks the constructions at register pc rpc the gather
// learned, the learning's native calls from from on, which the asking's
// NativeCalls had not met before its own question learned them
// (allocHidden).
type jitTargetHidden struct {
	rpc, from int32
	bound     bool
}

// jitTargetSuper is superTarget's answer for active's super() of argc
// arguments.
type jitTargetSuper struct {
	active, p, skip *Object
	argc            int
	cl              *closure
}

// jitTargetKey is a target by its function object, closure and register
// code.
type jitTargetKey struct {
	o  *Object
	cl *closure
	rc *regCode
}

// jitTargetReg is a closure's register code as the gather found it, and
// the link native callers call it through, nil where the gather made
// none.
type jitTargetReg struct {
	cl *closure
	rc *regCode
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
	jitSnapBinders = append(jitSnapBinders, jitTargetsBind)
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
// calls' (jitCallsGather), whose copy of the call caches it reads, of the
// register code alone, never the program (jitSnapBinders): what an answer
// must find made, made by register pc where the asking would make it --
// a construction's pool learned (jitRegSeedConstruct), a RegExp
// literal's, a step's result objects', a generator function's call's --
// and the facts of every target the copies name, their links, and what
// the hard calls read of the heap; the learning's headers are taken once
// the PCs are asked (late). The worker derives every answer from them.
func jitTargetsGather(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.calls
	t := &d.tg
	*t = jitTargetsData{}
	if d.cl == nil || fb.rc.Typed || fb.fb.e == nil || fb.fb.e.jitLearned == nil {
		// Asked at every PC, where the asking asks them (jitTargetsAskedAt),
		// Resumed and EntryGeneric as before.
		t.eager, t.eagerWhy = true, jitEagerCode
		return
	}
	r := fb.fb.r
	rc := fb.rc
	cc := s.root.d.closureCopy(d.cl)
	t.sites, t.rpcs = cc.sites, cc.rpcs
	t.gathered, t.live, t.realm = true, fb.fb.e, r.jitCallRealmNow()
	t.regs = make([]jitTargetReg, 0, min(len(t.sites), 8))
	l := t.live
	live := jitCallLive{fb}
	pushes := false
	for _, at := range t.rpcs {
		rpc := int(at)
		w := rc.Code[rpc]
		op := w.Op()
		argc := int(w.X())
		switch op {
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT:
			if !t.siteKnownAt(rpc, w, d.copied, l) {
				// A site of two calls, which the facts do not hold: asked
				// at every PC (jitTargetsAskedAt), as the asking asks it,
				// what this gather made found made.
				*t = jitTargetsData{eager: true, eagerWhy: jitEagerSites}
				return
			}
			method := op.MethodCall()
			if site := int(w.C()); site < len(d.copied) {
				if e := &d.copied[site]; e.callee != nil && e.cl != nil {
					t.target(s, r, e.callee, e.cl, e.rc, argc)
				} else if method && argc >= 1 && e.callee != nil && e.kind&^regCallMarks == regCallNative {
					if fd := e.callee.fn(); fd != nil && fd.elemOp == elemPush {
						pushes = true
					}
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
			if op == bytecode.RCallMW && rpc < len(rc.Marks) && rc.Marks[rpc]&(bytecode.RMarkLitArg|bytecode.RMarkLitRecv) != 0 {
				t.lits = append(t.lits, jitTargetLit{rpc: int32(rpc), pool: live.literal(rpc, w)})
			}
			if method && argc <= 1 && slices.ContainsFunc(l.genSteps, func(x jitGenStep) bool { return int(x.pc) == rpc }) {
				cls, pool := live.genNext(rpc)
				t.gens = append(t.gens, jitTargetGen{rpc: int32(rpc), cls: cls, pool: pool})
				for _, cl := range cls {
					t.regOf(s, r, cl)
					t.linkOf(s, r, cl, cl.reg)
				}
			}
			for i := range l.genMakes {
				if x := &l.genMakes[i]; int(x.pc) == rpc {
					t.genMakes = append(t.genMakes, jitTargetGenMake{rpc: int32(rpc), o: x.obj, m: live.genMake(rpc, x.obj)})
				}
			}
		case bytecode.RNewW:
			if !jitLearnedAt(l.nativeCalls, rpc, true) {
				t.seed(fb, rpc, w)
			}
		case bytecode.RLit:
			if w.X()&bytecode.RLitCall != 0 && !slices.Contains(l.failed, int32(rpc)) {
				t.lits = append(t.lits, jitTargetLit{rpc: int32(rpc), pool: live.literal(rpc, w)})
			}
		case bytecode.RCopyDataProps:
			if t.copies == 0 {
				t.copies = live.copies()
			}
		case bytecode.RSuperCall:
			if fb.fb.fn.Kind != bytecode.KindDerivedConstructor || jitRegArrowBeforeSuper(rc, fb.fb.fn) {
				continue
			}
			for i := range l.nativeCalls {
				if x := &l.nativeCalls[i]; int(x.pc) == rpc && x.active != nil {
					argc := int(w.B())
					p, skip, cl := live.superTarget(x.active, argc)
					t.supers = append(t.supers, jitTargetSuper{active: x.active, p: p, skip: skip, argc: argc, cl: cl})
					if cl != nil && cl.reg != nil {
						t.regOf(s, r, cl)
						t.linkOf(s, r, cl, cl.reg)
					}
					break
				}
			}
		}
	}
	if pushes {
		t.protos, t.protosTaken = live.arrayProtos(), true
	}
	for i := range l.nativeCalls {
		x := &l.nativeCalls[i]
		if x.obj == nil || x.forEach || x.active != nil || int(x.pc) >= len(rc.Code) {
			continue
		}
		w := rc.Code[x.pc]
		argc := int(w.X())
		switch w.Op() {
		case bytecode.RIterNext, bytecode.RApplyArgs:
			// A step's next, called of no argument (stepSites); a
			// forwarding call's function, its arguments the frame's.
			argc = 0
		}
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
			t.factsOf(s, r, x.obj, x.cl, reg.rc)
			if c.ok && c.cl == x.cl && c.parent == x.parent && reg.rc != nil {
				t.linkOf(s, r, x.cl, reg.rc)
			}
			continue
		}
		if x.cl == nil {
			continue
		}
		t.target(s, r, x.obj, x.cl, x.cl.reg, argc)
	}
	for i := range l.viaLeaves {
		// A leaf called through call, whose kind the snapshot asks ahead
		// (jitRegSnapshotOf's LeafKind) of what a ViaLeaf may answer.
		if x := &l.viaLeaves[i]; jitViaLeafCallable(x.obj, t.realm.realm) {
			s.root.d.leaves = append(s.root.d.leaves, uintptr(unsafe.Pointer(x.obj)))
		}
	}
}

// Why a snapshot's NativeCalls are asked at its PCs (jitTargetsData's
// eagerWhy).
const (
	jitEagerCode = 1 + iota
	jitEagerSites
)

// jitLearnedAt reports whether list has an entry of the call at register
// pc rpc, of a pool where pool says.
func jitLearnedAt(list []jitInline, rpc int, pool bool) bool {
	for i := range list {
		if int(list[i].pc) == rpc && (!pool || list[i].pool != nil) {
			return true
		}
	}
	return false
}

// seed learns, as the asking's question of the construction w at register
// pc rpc learns it, the function the binding read before it holds now,
// its objects from a pool (jitRegSeedConstruct), where nothing is learned
// there yet: as Inline's question there would, where it asks
// regCallTargets, or else NativeCalls', which reads the learning for an
// array's pool before it (regAllocSite) -- what it learns is hidden from
// that read (hidden), as it was from the asking's.
func (t *jitTargetsData) seed(fb *jitRegFeedback, rpc int, w bytecode.RInstr) {
	l, rc := t.live, fb.rc
	argc := int(w.X())
	generic := slices.Contains(l.genericCalls, int32(rpc))
	tail := rpc < len(rc.Marks) && rc.Marks[rpc]&bytecode.RMarkTail != 0
	inline := !tail && !slices.Contains(l.notInline, int32(rpc)) && !generic
	if !inline && (tail || generic && argc+1 <= abi.MaxGoArgs) {
		// Neither question learns it: a tail call's is no construction's,
		// and a generic one is Go's before NativeCalls' regCallTargets.
		return
	}
	n := len(l.nativeCalls)
	if o := fb.regConstructorNow(rpc, int(w.A())); o != nil {
		fb.fb.r.jitRegSeedConstruct(l, int32(rpc), o, argc)
	}
	if !inline && len(l.nativeCalls) > n {
		t.hidden = append(t.hidden, jitTargetHidden{rpc: int32(rpc), from: int32(n)})
	}
}

// siteKnownAt reports whether the facts of the call w at register pc rpc
// hold what its derivation may ask (jitCallSrc's now, seenCallees): its
// site's, of its register pc alone, a method call's seen callees, and its
// binding read where it is a call the interpreter has not filled (calls)
// and the learning l knows nothing at the call -- the derivation's
// learning has no fewer entries there.
func (t *jitTargetsData) siteKnownAt(rpc int, w bytecode.RInstr, calls []regCallCache, l *jitEntry) bool {
	x := t.site(rpc, w)
	if x == nil {
		return false
	}
	method, argc := w.Op().MethodCall(), int(w.X())
	if method && !x.seenAsked {
		return false
	}
	if l == nil || !jitCallUnfilled(calls, w, method, argc, true) {
		return true
	}
	wants := method && argc >= 1 && !jitLearnedAt(l.nativeCalls, rpc, false) ||
		!method && !jitLearnedAt(l.nativeCalls, rpc, false) && !jitLearnedAt(l.inlines, rpc, false)
	return !wants || x.asked
}

// jitTargetsAskedAt is which of NativeCalls and ViaLeaf the asking asks at
// the program's instruction pc of s on the runtime's goroutine, in its
// order among the methods there (askReg, jitSnapAskedAt): both, at every
// PC of a snapshot whose answers the worker does not derive -- typed
// code's, which reads its function tables -- after Inline's question of
// the PC, as the feedback was asked.
func jitTargetsAskedAt(s *jitSnapshot, pc int) uint64 {
	if s.d.calls.tg.eager {
		return 1<<snapNativeCalls | 1<<snapViaLeaf
	}
	return 0
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
	f := t.factsOf(s, r, o, cl, rc)
	if rc != reg.rc {
		f = t.factsOf(s, r, o, cl, reg.rc)
	}
	if reg.rc == nil {
		return
	}
	if f.callable(cl, reg.rc, argc, t.realm.quirks) || argc >= 1 && f.callable(cl, reg.rc, argc-1, t.realm.quirks) {
		t.linkOf(s, r, cl, reg.rc)
	}
}

// factsOf is the facts of o, cl's, of register code rc (jitRegFactsOf),
// taken once a compile, by its root's gathers (jitTargetsShared): what of
// a target the heap holds does not change while the compile is prepared,
// nothing of the script's running meanwhile, and a closure's register
// code made meanwhile is another rc. None of no register code: no call
// of it is native (jitRegTargetFree).
func (t *jitTargetsData) factsOf(s *jitSnapshot, r *Runtime, o *Object, cl *closure, rc *regCode) jitCallFacts {
	if rc == nil {
		return jitCallFacts{}
	}
	sh := &s.root.d.targets
	k := jitTargetKey{o, cl, rc}
	if f, ok := sh.facts[k]; ok {
		return f
	}
	if sh.facts == nil {
		sh.facts = make(map[jitTargetKey]jitCallFacts, 64)
	}
	f := r.jitRegFactsOf(o, cl, rc)
	sh.facts[k] = f
	return f
}

// linkOf makes, once a compile, the link native callers call cl's code rc
// through (regLinkOf), kept in the snapshot's gained links (gatherLink).
func (t *jitTargetsData) linkOf(s *jitSnapshot, r *Runtime, cl *closure, rc *regCode) {
	sh := &s.root.d.targets
	k := jitLinkKey{cl, rc}
	if _, ok := sh.links[k]; ok {
		return
	}
	if sh.links == nil {
		sh.links = make(map[jitLinkKey]*jitLink, 16)
	}
	sh.links[k] = s.gatherLink(r.jitState().regLinkOf(cl, rc))
}

// jitTargetsShared is what a compile's snapshots' targets' gathers take
// once for all of them (jitSnapData's targets): the facts of each target
// by function object, closure and register code, and the links by
// closure and register code.
type jitTargetsShared struct {
	facts map[jitTargetKey]jitCallFacts
	links map[jitLinkKey]*jitLink
}

// jitLinkKey is a link's closure and register code.
type jitLinkKey struct {
	cl *closure
	rc *regCode
}

// regAtOf is the index in t's regs of cl's register code as the gather
// found it, taken once.
func (t *jitTargetsData) regAtOf(s *jitSnapshot, r *Runtime, cl *closure) int {
	if t.regAt != nil {
		if i, ok := t.regAt[cl]; ok {
			return i
		}
	} else {
		for i := range t.regs {
			if t.regs[i].cl == cl {
				return i
			}
		}
	}
	t.regs = append(t.regs, jitTargetReg{cl: cl, rc: cl.reg})
	i := len(t.regs) - 1
	if t.regAt == nil && len(t.regs) > 8 {
		// Many: found by a map from here, the gather's own.
		t.regAt = make(map[*closure]int, 2*len(t.regs))
		for k := range t.regs {
			t.regAt[t.regs[k].cl] = k
		}
	} else if t.regAt != nil {
		t.regAt[cl] = i
	}
	return i
}

// regOf is cl's register code as the gather found it, taken once.
func (t *jitTargetsData) regOf(s *jitSnapshot, r *Runtime, cl *closure) *jitTargetReg {
	return &t.regs[t.regAtOf(s, r, cl)]
}

// index makes t's indexes, on the worker.
func (t *jitTargetsData) index() {
	if t.byReg != nil {
		return
	}
	t.byReg = make(map[*closure]jitTargetReg, len(t.regs))
	for _, x := range t.regs {
		t.byReg[x.cl] = x
	}
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
	if rc == nil {
		return jitCallFacts{}
	}
	f, ok := c.s.root.d.targets.facts[jitTargetKey{o, cl, rc}]
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
	if l := c.s.root.d.targets.links[jitLinkKey{cl, rc}]; l != nil {
		return l
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

func (c jitCallCopy) allocHidden(i int, x *jitInline) bool {
	for _, h := range c.data().hidden {
		if h.rpc == x.pc && i >= int(h.from) {
			return true
		}
	}
	return false
}

func (c jitCallCopy) seed(int, bytecode.RInstr, int) {}

func (c jitCallCopy) literal(rpc int, w bytecode.RInstr) *abi.ObjectPool {
	for _, x := range c.data().lits {
		if int(x.rpc) == rpc {
			return x.pool
		}
	}
	panic("jit: a snapshot's derivation met a RegExp literal its gather did not make")
}

func (c jitCallCopy) genNext(rpc int) ([]*closure, *abi.ObjectPool) {
	for _, x := range c.data().gens {
		if int(x.rpc) == rpc {
			return x.cls, x.pool
		}
	}
	panic("jit: a snapshot's derivation met a step its gather did not take")
}

func (c jitCallCopy) genMake(rpc int, o *Object) jitGenMakeFact {
	for _, x := range c.data().genMakes {
		if int(x.rpc) == rpc && x.o == o {
			return x.m
		}
	}
	panic("jit: a snapshot's derivation met a generator's call its gather did not take")
}

func (c jitCallCopy) superTarget(active *Object, argc int) (*Object, *Object, *closure) {
	for _, x := range c.data().supers {
		if x.active == active && x.argc == argc {
			return x.p, x.skip, x.cl
		}
	}
	panic("jit: a snapshot's derivation met a super() its gather did not take")
}

func (c jitCallCopy) arrayProtos() jitCallProtos {
	t := c.data()
	if !t.protosTaken {
		panic("jit: a snapshot's derivation met a push its gather did not take")
	}
	return t.protos
}

func (c jitCallCopy) copies() uintptr {
	t := c.data()
	if t.copies == 0 {
		panic("jit: a snapshot's derivation met a copy of data properties its gather did not take")
	}
	return t.copies
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

// jitTargetsBind marks, once s's program is bound (bind), what its gather
// made at register pcs that a PC of the program asks NativeCalls at: the
// rest, made where the asking would not have, are counted
// (TestJITSnapshotAnswersAsAsked).
func jitTargetsBind(s *jitSnapshot) {
	t := &s.d.calls.tg
	s.d.calls.p, s.d.native.p = s.p, s.p
	if !t.gathered || len(t.lits)+len(t.gens)+len(t.genMakes)+len(t.hidden) == 0 {
		return
	}
	asked := make(map[int32]bool)
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapNativeCalls) != 0 {
			asked[int32(s.p.Origin[pc])] = true
		}
	}
	for i := range t.lits {
		t.lits[i].bound = asked[t.lits[i].rpc]
	}
	for i := range t.gens {
		t.gens[i].bound = asked[t.gens[i].rpc]
	}
	for i := range t.genMakes {
		t.genMakes[i].bound = asked[t.genMakes[i].rpc]
	}
	for i := range t.hidden {
		t.hidden[i].bound = asked[t.hidden[i].rpc]
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
		asks := jitSnapAsks[op]
		at := int64(pc)
		if asks&(1<<snapNativeCalls) != 0 {
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
		if asks&(1<<snapViaLeaf) != 0 {
			// After NativeCalls at the PC, whose learning it reads.
			before := s.begin(sink)
			r0, r1, r2 := q.viaLeaf(pc)
			eff := s.endDerived(sink, before)
			s.heard(snapViaLeaf, at, r2, r0, r1, r2)
			if r2 || eff != 0 {
				a.ViaLeaf.reserve(int(s.room[snapViaLeaf]))
				a.ViaLeaf.put(at, eff, r2, jitSnapAnsViaLeaf{r0, r1, r2})
			}
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
func jitCallSiteFacts(r *Runtime, cl *closure, calls []regCallCache) (out []jitCallSiteFact, rpcs []int32) {
	rc := cl.fn.RegCode()
	if rc == nil || rc.Typed {
		return nil, nil
	}
	// Read, not made (regLearnedOf would make it): a closure the walk
	// copies that no feedback has asked of has learned nothing.
	l := r.jitState().regLearned[r.jitRegKeyOf(cl)]
	fb := &jitRegFeedback{fb: &jitFeedback{r: r, fn: cl.fn, cl: cl, e: l}, rc: rc}
	// By register, past 1: the last instruction whose A it is, and the
	// last get_method_w that wrote it.
	regs := make([]int32, 2*(rc.NumRegs+1))
	last, method := regs[:rc.NumRegs+1:rc.NumRegs+1], regs[rc.NumRegs+1:]
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
		case bytecode.RLit, bytecode.RCopyDataProps, bytecode.RSuperCall:
			rpcs = append(rpcs, int32(rpc))
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT, bytecode.RNewW:
			rpcs = append(rpcs, int32(rpc))
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
			switch {
			case op == bytecode.RNewW:
			case isMethod && int(w.X()) >= 1 && !jitLearnedAt(l.nativeCalls, rpc, false):
				x.asked = true
				if q := at(&last, int(w.A())); q >= 0 {
					x.now, x.cl, x.rc = fb.regNowTargetAt(q, w, true)
				}
			case !isMethod && !jitLearnedAt(l.nativeCalls, rpc, false) && !jitLearnedAt(l.inlines, rpc, false):
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
	return out, rpcs
}
