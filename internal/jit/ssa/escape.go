package ssa

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// Escape analysis, as V8's TurboFan makes it (escape-analysis.cc): an
// object the code makes that nothing but its own code sees is not made. Its
// properties are the values last stored to them, held in SSA values,
// registers and spill slots, and every read of one is that value. Where
// the code exits, the frame state names the object as V8's deoptimization
// data names a materialized object: by a word no value has, its fields at
// its home past the frame's slots (abi.VirtualBase), and Go makes the
// object of them there, one for every slot naming it (the VM's
// jitMaterialize), as V8's deoptimizer materializes it.
//
// An object is a pool's (abi.ObjectPool): an object literal's (CallSite's
// Literal), or a construction's receiver whose constructor the code
// inlines (CallSite's Receiver). It escapes -- is made as before -- where
// anything but its own code may see it and the code cannot make it there:
// a phi merging it with anything but objects not made, and every operation
// that would read it in a way not known here.
//
// Beyond TurboFan's, as Graal's partial escape analysis has it, an object
// lives across a merge and round a loop. A phi merging objects not made of
// one shape at the merge is an object not made of its own (mergeAt), if
// none of them is used past it (liveAt): its properties
// are phis of theirs, at a loop's header of what the body leaves (header,
// backEdges), so an accumulator a loop replaces each turn is its properties
// alone. And a use that needs an object made -- a call's operand, a store
// into an object that is made, a return -- has the code make it there
// (materialize, makeAt): its pool's next object, its properties defined
// along the additions that made its state; past it, it is that object. A
// path that makes it where another has not has it made where it is taken,
// as does an object the code would make on every path anyway (pruneMade),
// which would save nothing.
//
// A phi merging it with the slot an entry loads, at an entry there only for
// Go to go on at after an instruction it ran (Entry's Resume), or a loop's
// header's Go has never needed (Entry's OSR), is the object: the entry is
// dropped, Go going on in the interpreter there instead, to the next loop
// header, as after a guard's exit -- and, where a frame runs a loop long,
// on in code of its own compiled with the loop's entry (the VM's
// jitLearnLoopEntry), this code keeping the object unmade for every other
// invocation. An entry stays dropped only for objects left unmade in the
// end (syncEntries).
//
// Its stores into it may change its shape only along the shapes its sites'
// caches know, from the shape its pool's objects have, the same shape on
// every path to a merge (TurboFan merges virtual objects of one map), and
// its reads are of the shape it has there: one of its own
// properties is the value stored, one on its prototypes the property's
// cell there, the prototypes checked from its own on (OpHolderCell). What
// it does not make checks what its making did of the heap -- its
// constructor's prototype (OpProtoIs) -- and its additions what theirs did
// (OpChainCheck): every exit is the one the made object's operations had.

// VirtualFeedback is Feedback that knows how Go makes the objects of a pool
// (abi.ObjectPool) the code does not make, at an exit: their shape at the
// start, and their prototype -- an object literal's, the realm's
// Object.prototype; a construction's receiver's, the prototype the pool's
// objects were made with, which the code checks the constructor has
// (OpProtoIs) -- each held alive by the VM, and kept with the code where
// escape analysis takes them (Func's VirtualAsked); false if Go cannot make
// one so. The build asks it of every pool whose take escape analysis may
// leave unmade (poolTake), once each (askVirtualRoots), so that the
// optimization asks the VM nothing.
type VirtualFeedback interface {
	VirtualRoot(pool uintptr) (shape, proto uintptr, ok bool)
}

// virtualRoot is VirtualFeedback's answer for a pool.
type virtualRoot struct {
	shape, proto uintptr
	ok           bool
}

// askVirtualRoots asks fb, if it is VirtualFeedback, how Go makes the
// objects of every pool whose take f's escape analysis may leave unmade.
func askVirtualRoots(f *Func, fb Feedback) {
	vf, ok := fb.(VirtualFeedback)
	if !ok {
		return
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			site := poolTake(v)
			if site == nil {
				continue
			}
			if _, asked := f.virtual[site.Pool]; asked {
				continue
			}
			if f.virtual == nil {
				f.virtual = map[uintptr]virtualRoot{}
			}
			shape, proto, ok := vf.VirtualRoot(site.Pool)
			f.virtual[site.Pool] = virtualRoot{shape: shape, proto: proto, ok: ok}
		}
	}
}

// poolTake is v's call, if v takes an object from a pool that escape
// analysis may leave unmade: an object literal's, or a construction's
// receiver whose constructor the code inlines. Its operands are not looked
// at, which candidates' tests add, so that what the build asks of covers
// every take the optimization finds.
func poolTake(v *Value) *CallSite {
	if v.Op != OpCall || len(v.Calls()) != 1 {
		return nil
	}
	site := v.Calls()[0]
	if site.Pool == 0 {
		return nil
	}
	if site.Literal && site.Alloc && site.Argc == 0 || site.Receiver && !site.Derived && site.Parent == 0 && site.Via == 0 {
		return site
	}
	return nil
}

// virtual is an object the code may not make: the pool's take (OpCall), its
// keep cell and its value (OpCallCell, OpKept).
type virtual struct {
	id              int
	call, cell, obj *Value
	site            *CallSite
	shape, proto    uintptr
	escaped         bool
	// phi, for an object a phi merges of others not made (mergeAt), is
	// that phi, which is the object (obj); the others are its inputs
	// (inputsOf): each is made if it is, and it if any of them is.
	phi *Value
	// holds are the objects stored into this one's properties, which escape
	// if it does; held, whether any object holds this one (liveAt).
	holds []*virtual
	held  bool
	// entries are the entry blocks a phi merges the object with, dropped.
	entries []*Block
	// out is its state at each block's end, by block ID; nil where it is
	// not known (not made yet, or not live at a loop's header: liveIn).
	out []*vstate
	// fields is the most properties it has; home the first of its home's
	// slots (abi.VirtualFields of them, then its properties').
	fields, home int
	// words are the constants its states name it by and its shapes by
	// (ir.Word), by the word.
	words map[uint64]*Value
}

// vstate is an object's state at a point: its shape, and its properties'
// values in its table's order. A state is never changed, but replaced.
type vstate struct {
	shape  uintptr
	fields []*Value
	// keys are the properties' atoms, as the writes that added them name
	// them: what a read that searches the object's own table finds
	// (PropertySite's Scan, Megamorphic). adds are the additions that made
	// them, along the transitions from the pool's objects' shape: what the
	// code that makes the object writes (materialize).
	keys []uint32
	adds []*PropertyAdd
	// real, once the code has made the object (materialize), is it, and
	// ptr its pointer: its code from there is an object's, its uses the
	// object made.
	real, ptr *Value
}

// made is the state of an object made, real, its pointer ptr.
func made(real, ptr *Value) *vstate { return &vstate{real: real, ptr: ptr} }

// sameMade reports whether states a and b are the same object made, or
// both not.
func sameMade(a, b *vstate) bool { return a.real == b.real }

// matPoint is an object the code makes where a use needs it made -- a
// call's operand, a store into an object made, a return -- not where its
// pool's take was, as Graal's partial escape analysis materializes a
// virtual object where it escapes: its pool's next object, as an object
// literal's take (OpCall, its keep cell, OpKept), its properties defined
// along the transitions the code's writes took. obj and ptr are made once
// and kept over the rounds, which rewrite the uses after it to them.
type matPoint struct {
	o        *virtual
	st       *vstate
	pool     uintptr
	obj, ptr *Value
	at       *Value
}

// realArg is an operand a use of an object made is rewritten to.
type realArg struct {
	i int
	x *Value
}

// Roles of the values escape analysis looks at (escaper's role).
const (
	roleNone = iota
	roleCall
	roleCell
	roleObj
	rolePtr
	rolePropCell
	// roleKeep is a keep of an object (OpKeep), whose copy (OpKept) is
	// the object: what keepAcrossStores made of a phi that merges it.
	roleKeep
)

// escPlan is what a value of an object's code becomes.
type escPlan struct {
	kind escKind
	// field is a read's value (escUnbox, escField); holders and index a
	// prototype's property's (escHolder); add an addition's (escCheck).
	field   *Value
	holders HolderChain
	index   int
	add     *PropertyAdd
}

type escKind uint8

const (
	escDrop   escKind = iota + 1 // nothing
	escUnbox                     // the read's value, unboxed
	escField                     // the value
	escHolder                    // a prototype's cell (OpHolderCell)
	escCheck                     // its prototypes checked (OpChainCheck)
)

// escState is a frame state's use, by a value or a block's exit, at a point
// where it names objects not made: their states there.
type escState struct {
	v    *Value
	b    *Block
	s    *FrameState
	snap []escSnap
	// head marks a loop header's state (Block's Header), not its exit's;
	// mat the state the making of objects before v exits to (matState).
	head, mat bool
}

type escSnap struct {
	o  *virtual
	st *vstate
}

// phiKey is a phi escapeAnalysis made: of object o's property k at block
// b -- by the block, whose ID an entry's dropping changes (reorder).
type phiKey struct {
	b    *Block
	o, k int
}

// edgeCut is an edge cut from block s, the entries' dropping's: its
// index there, where it came from, whether it was a back edge, and what
// s's phis had on it. Undone, last first, the function is as it was
// (syncEntries).
type edgeCut struct {
	s, pred  *Block
	j        int
	backedge bool
	phis     []*Value
	args     []*Value
}

// escaper is escape analysis's work on a function.
type escaper struct {
	f     *Func
	objs  []*virtual
	role  []uint8
	of    []int32 // by value ID: the object's index, past one
	plans map[*Value]escPlan
	uses  []escState
	// phis are the phis merging an object's properties at a block, by
	// block, object and property.
	phis   map[phiKey]*Value
	dom    *domTree
	again  bool
	homeAt int
	// loops are the objects' states at loops' headers (header).
	loops []loopState
	// entries are the function's entries as built, unresumed its
	// Unresumed; dropped the blocks of those dropped now, and cuts what
	// dropping them cut (syncEntries).
	entries   []Entry
	unresumed []int
	dropped   []*Block
	cuts      []edgeCut
	// useBlocks and phiUses are each object's uses (indexUses).
	useBlocks [][]int32
	phiUses   [][]*Value
	// mats are the uses classify found that need an object made, by user,
	// which flow makes it at if it is not made yet (materialize); points
	// the objects made, by user and object, kept over the rounds; making
	// those flow made at each user, and the user's state there, before;
	// reals the operands rewritten where an object is made already, and
	// controls the controls.
	mats     map[*Value][]*virtual
	points   map[[2]int]*matPoint
	making   map[*Value][]*matPoint
	matState map[*Value]*FrameState
	makings  int
	reals    map[*Value][]realArg
	controls map[*Block]*Value
}

// scalarReplace is escape analysis (above): it reports whether it left any
// object unmade.
func scalarReplace(f *Func) bool {
	if f.virtual == nil {
		return false
	}
	e := &escaper{f: f}
	if !e.candidates() {
		return false
	}
	e.entries, e.unresumed = slices.Clone(f.Entries), slices.Clone(f.Unresumed)
	// Each round knows more of what the objects' code is -- a property's
	// read that is another object not made, whose uses are that one's --
	// or that more of them escape, until neither changes.
	for {
		e.again = false
		e.classify()
		if !e.live() {
			// Nothing to leave unmade: the function as it was.
			e.syncEntries()
			e.dropPhis()
			return false
		}
		if e.again {
			continue
		}
		e.syncEntries()
		e.flow()
		if e.again {
			e.propagate()
			continue
		}
		if e.keepFrameEntry() {
			e.propagate()
			continue
		}
		if !e.homes() {
			e.propagate()
			continue
		}
		break
	}
	e.transform()
	return true
}

// keepFrameEntry has the objects made for which the entry of the loop a
// frame waits at (Func's FrameAt) would be dropped, and reports whether
// there were any. Asked once the walk has settled, so that only an entry
// that would stay dropped counts: the next round puts it back.
func (e *escaper) keepFrameEntry() bool {
	f := e.f
	if f.FrameAt == 0 {
		return false
	}
	for _, b := range e.dropped {
		if int(b.Header.PC) != f.FrameAt {
			continue
		}
		kept := false
		for _, o := range e.objs {
			if !o.escaped && slices.Contains(o.entries, b) {
				e.escape(o)
				kept = true
			}
		}
		if kept {
			if !slices.Contains(f.EntryKept, f.FrameAt) {
				f.EntryKept = append(f.EntryKept, f.FrameAt)
			}
			return true
		}
	}
	return false
}

// candidates finds the pools' takes the code may not make.
func (e *escaper) candidates() bool {
	f := e.f
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			site := poolTake(v)
			if site == nil {
				continue
			}
			literal := site.Literal && site.Alloc && site.Argc == 0 && len(v.Args) == 0
			receiver := site.Receiver && !site.Derived && site.Parent == 0 && site.Via == 0 && len(v.Args) >= 1
			if !literal && !receiver {
				continue
			}
			if e.role == nil {
				e.role = make([]uint8, f.nextID)
				e.of = make([]int32, f.nextID)
			}
			o := &virtual{id: len(e.objs), call: v, site: site}
			e.objs = append(e.objs, o)
			e.set(v, roleCall, o)
		}
	}
	return len(e.objs) != 0
}

func (e *escaper) set(v *Value, role uint8, o *virtual) {
	if !e.madeHere(v) {
		e.role[v.ID], e.of[v.ID] = role, int32(o.id+1)
	}
}

// madeHere reports whether v is a value the walks made, a phi of an
// object's properties (phiOf): one has no role, however it merges objects,
// which then escape (use), a property's phi being no object.
func (e *escaper) madeHere(v *Value) bool { return v.ID >= len(e.role) }

// roleOf is v's role and its object, or roleNone.
func (e *escaper) roleOf(v *Value) (uint8, *virtual) {
	if v == nil || v.ID >= len(e.role) || e.role[v.ID] == roleNone {
		return roleNone, nil
	}
	return e.role[v.ID], e.objs[e.of[v.ID]-1]
}

// escape has o made.
func (e *escaper) escape(o *virtual) {
	if o != nil && !o.escaped {
		o.escaped = true
		e.again = true
	}
}

// entryOf reports whether b is an entry's block escape analysis may drop:
// one Go goes on at only after an instruction it ran (Entry's Resume), or a
// loop's header's, where Go has never gone on (Entry's OSR).
func (e *escaper) entryOf(b *Block) bool {
	if len(b.Preds) != 0 {
		return false
	}
	for _, en := range e.f.Entries {
		if en.Block == b {
			return en.Resume || en.OSR
		}
	}
	return false
}

// classify gives each value its role and has an object escape where a use
// of it is not one escape analysis knows. The values' roles come first, in
// the blocks' order, a value's operand being in a block that dominates its
// own; then the phis', which an order with entries inside a loop's body
// meets before their operands, until none changes; then the uses.
func (e *escaper) classify() {
	f := e.f
	e.dominators()
	e.mats = map[*Value][]*virtual{}
	for _, o := range e.objs {
		o.holds, o.held = o.holds[:0], false
	}
	for final := true; final; {
		for changed := true; changed; {
			changed = false
			for _, b := range f.Blocks {
				for _, v := range b.Values {
					if r, _ := e.roleOf(v); r != roleNone || e.madeHere(v) {
						continue
					}
					if v.Op == OpPhi && e.alias(v, false) || v.Op != OpPhi && e.assign(v) {
						changed = true
					}
				}
			}
		}
		// The phis left: one merging an object with what no entry loads,
		// or a loop's phis merging objects with each other (mergeAt); the
		// values of those that are objects then have their roles too.
		final = false
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if r, _ := e.roleOf(v); r == roleNone && v.Op == OpPhi && !e.madeHere(v) && e.alias(v, true) {
					final = true
				}
			}
		}
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			for i, a := range v.Args {
				e.use(v, i, a)
			}
			if v.Shadow != nil {
				if r, o := e.roleOf(v.Shadow); r != roleNone {
					if !(v.Op == OpLoadCell && r == rolePropCell || v.Op == OpKept && r == roleCell) {
						e.escape(o)
					}
				}
			}
			e.state(v.State, false)
		}
		if r, o := e.roleOf(b.Control); r != roleNone {
			if c := e.returnCheck(b, o); r == roleObj && c != nil {
				// Returned: made where the return's check is, whose state
				// is the return's.
				e.mats[c] = append(e.mats[c], o)
			} else {
				e.escape(o)
			}
		}
		// A yield a loop's step resumed natively writes the generator's
		// frame itself (Block's Yield), where Go makes nothing.
		e.state(b.State, b.Yield)
		// A loop's header's state names what its phis merge, which flow
		// knows; an entry's, what Go gives it.
		e.state(b.Header, !b.LoopHeader)
	}
	for _, o := range e.objs {
		if o.phi == nil && (o.cell == nil || o.obj == nil) {
			e.escape(o)
		}
		for _, h := range o.holds {
			h.held = true
		}
	}
	e.propagate()
	e.pruneMade()
}

// returnCheck is the check of the result return block b's code makes
// (the builder's OpCheckInit), if it is of object o and has a state: the
// return's, which a use there may make the object at.
func (e *escaper) returnCheck(b *Block, o *virtual) *Value {
	if b.Kind != BlockReturn {
		return nil
	}
	for i := len(b.Values) - 1; i >= 0; i-- {
		v := b.Values[i]
		if v.Op == OpCheckInit && v.State != nil {
			if r, w := e.roleOf(v.Args[0]); r == roleObj && w == o {
				return v
			}
			return nil
		}
	}
	return nil
}

// assign gives v, no phi, its role, if it has one, and reports whether it
// gave it one.
func (e *escaper) assign(v *Value) bool {
	switch v.Op {
	case OpCallCell:
		if r, o := e.roleOf(v.Args[0]); r == roleCall {
			if o.cell != nil {
				e.escape(o)
				return false
			}
			o.cell = v
			e.set(v, roleCell, o)
			return true
		}
	case OpKept:
		if r, o := e.roleOf(v.Args[0]); r == roleKeep && v.Args[1] == v.Args[0].Args[0] {
			e.set(v, roleObj, o)
			return true
		}
		if r, o := e.roleOf(v.Args[0]); r == roleCell && v.Args[1] == o.call {
			if o.obj != nil {
				e.escape(o)
				return false
			}
			o.obj = v
			e.set(v, roleObj, o)
			return true
		}
	case OpObjectOf:
		if r, o := e.roleOf(v.Args[0]); r == roleObj {
			e.set(v, rolePtr, o)
			return true
		}
	case OpKeep:
		if r, o := e.roleOf(v.Args[0]); r == roleObj {
			e.set(v, roleKeep, o)
			return true
		}
	case OpPropCell:
		if r, o := e.roleOf(v.Args[0]); r == rolePtr {
			e.set(v, rolePropCell, o)
			return true
		}
	}
	return false
}

// alias gives phi v the role of the object it merges, if it merges one
// with itself and the slots entries load (virtual's entries), and reports
// whether it did; a phi merging an object with anything else has it
// escape -- but, until final, with a phi that may yet be the object too.
func (e *escaper) alias(v *Value, final bool) bool {
	var obj *virtual
	several := false
	for _, a := range v.Args {
		r, o := e.roleOf(a)
		if r == roleObj {
			several = several || obj != nil && obj != o
			if obj == nil {
				obj = o
			}
		} else if r == roleNone && a.Op == OpPhi && a != v && !final {
			return false
		}
	}
	if several || final && obj != nil && e.pendingPhi(v) {
		return e.mergeAt(v, final)
	}
	if obj == nil || obj.escaped {
		return false
	}
	// The object, and, past the others, an entry's slot: the entry
	// dropped.
	var entries []*Block
	for j, a := range v.Args {
		if r, o := e.roleOf(a); r == roleObj && o == obj || a == v {
			continue
		}
		// The slot an entry there for Go to go on at loads, on a path from
		// it alone: the entry dropped, the path goes with it.
		p := v.Block.Preds[j]
		if !e.entryOf(a.Block) || !e.dom.dominates(a.Block, p) {
			e.escape(obj)
			return false
		}
		entries = append(entries, a.Block)
	}
	obj.entries = append(obj.entries, entries...)
	e.set(v, roleObj, obj)
	return true
}

// mergeAt has phi v, which merges different objects not made, be an
// object not made of its own, if it merges nothing else: its properties
// those of the object each edge brings, merged by phis (mergeInputs), as
// Graal's partial escape analysis merges virtual objects at a merge. Where
// it merges anything else, every object it merges is made.
//
// A phi a loop's header has may merge one that has none yet, which merges
// it in turn -- the object a loop's body made with the object before it,
// a loop's state -- and the phis left without one once the others have
// theirs (final) are taken to be objects: what they turn out to merge
// decides (flow), every object of the cycle made if one is. An entry's
// slot, as alias has it, has the entry dropped.
func (e *escaper) mergeAt(v *Value, final bool) bool {
	var site *CallSite
	var entries []*Block
	for j, a := range v.Args {
		r, o := e.roleOf(a)
		switch {
		case a == v:
		case r == roleObj && !o.escaped:
			if site == nil {
				site = o.site
			}
		case r == roleNone && a.Op == OpPhi && final:
		case r == roleNone && e.entryOf(a.Block) && e.dom.dominates(a.Block, v.Block.Preds[j]):
			entries = append(entries, a.Block)
		default:
			for _, a := range v.Args {
				if r, o := e.roleOf(a); r == roleObj {
					e.escape(o)
				}
			}
			return false
		}
	}
	m := &virtual{id: len(e.objs), obj: v, phi: v, site: site, entries: entries}
	e.objs = append(e.objs, m)
	e.set(v, roleObj, m)
	return true
}

// pendingPhi reports whether phi v merges a phi with no role yet.
func (e *escaper) pendingPhi(v *Value) bool {
	for _, a := range v.Args {
		if r, _ := e.roleOf(a); r == roleNone && a.Op == OpPhi && a != v {
			return true
		}
	}
	return false
}

// inputsOf is the objects merged object m's phi merges, but m.
func (e *escaper) inputsOf(m *virtual) []*virtual {
	var in []*virtual
	for _, a := range m.phi.Args {
		if r, o := e.roleOf(a); r == roleObj && o != m && !slices.Contains(in, o) {
			in = append(in, o)
		}
	}
	return in
}

// defBlock is the block o is made in, or merged in.
func (o *virtual) defBlock() *Block {
	if o.phi != nil {
		return o.phi.Block
	}
	return o.call.Block
}

// use is v's use of a, its operand i: one escape analysis knows, or the
// object a is of escapes.
func (e *escaper) use(v *Value, i int, a *Value) {
	r, o := e.roleOf(a)
	switch r {
	case roleNone:
		return
	case roleCall:
		if v.Op == OpCallCell && i == 0 || v.Op == OpKept && i == 1 && v.Args[0] == o.cell && o.cell != nil {
			return
		}
	case roleCell:
		if v.Op == OpKept && i == 0 && v.Args[1] == o.call {
			return
		}
	case roleObj:
		switch {
		case v.Op == OpObjectOf, v.Op == OpCheckInit, v.Op == OpKeep && i == 0:
			return
		case v.Op == OpKept && i == 1:
			if kr, ko := e.roleOf(v.Args[0]); kr == roleKeep && ko == o {
				return
			}
		case v.Op == OpPhi:
			if vr, vo := e.roleOf(v); vr == roleObj && (vo == o || vo.phi == v && slices.Contains(e.inputsOf(vo), o)) {
				return
			}
		case v.Op == OpPropWrite && i == 1:
			// Stored into an object not made: one of its properties.
			if wr, w := e.roleOf(v.Args[0]); wr == rolePtr {
				w.holds = append(w.holds, o)
				return
			}
		}
	case rolePtr:
		switch v.Op {
		case OpPropRead, OpPropCell:
			if i == 0 && !v.Upvalue && !v.Global {
				return
			}
		case OpPropWrite:
			if i == 0 && v.Type == None && !v.Upvalue && !v.Global {
				return
			}
		}
	case rolePropCell:
		if v.Op == OpLoadCell && i == 0 {
			return
		}
	case roleKeep:
		if v.Op == OpKept && i == 0 {
			return
		}
	}
	if vr, vo := e.roleOf(v); r == roleObj && vr == roleCall && i != 0 && !vo.escaped {
		// What the constructor of a take not made is given: the take goes,
		// and what its code does with it is its own use. A take found made
		// has it made in turn (flow).
		return
	}
	if r == roleObj && v.State != nil && v.Op != OpPhi && v.Shadow == nil {
		// One that needs it made: made there, unless it is already
		// (flow).
		if !slices.Contains(e.mats[v], o) {
			e.mats[v] = append(e.mats[v], o)
		}
		return
	}
	e.escape(o)
}

// state has every object a frame state names escape where Go could not
// make it from the state: a loop's header's, a parameter list's prologue's.
func (e *escaper) state(s *FrameState, header bool) {
	if s == nil {
		return
	}
	for _, i := range e.f.Present(s) {
		if r, o := e.roleOf(s.Slots[i]); r != roleNone {
			if r != roleObj || header || s.Args != 0 || s.Virtual != 0 {
				e.escape(o)
			}
		}
	}
}

// propagate has an object stored into one that escapes escape too, and
// one Go cannot make.
func (e *escaper) propagate() {
	for changed := true; changed; {
		changed = false
		for _, o := range e.objs {
			if o.phi != nil {
				// A merge's object is made with its inputs, and they with
				// it: its phi is a value's if any is; theirs merged by it, if
				// it is.
				inputs := e.inputsOf(o)
				if len(inputs) == 0 && !o.escaped {
					o.escaped, changed = true, true
				}
				for _, in := range inputs {
					if in.escaped != o.escaped {
						in.escaped, o.escaped, changed = true, true, true
					}
				}
				if !o.escaped {
					// Of one prototype, as a shape says, which is known
					// before a walk finds their shapes at the merge the
					// same, or has it made (mergeInputs) -- literals' sites
					// have roots of their own, so theirs differ. An input
					// merged in a cycle is known once one of the cycle is.
					for _, in := range inputs {
						if o.shape == 0 && in.shape != 0 {
							o.shape, o.proto, o.site, changed = in.shape, in.proto, in.site, true
						}
					}
					for _, in := range inputs {
						if in.shape != 0 && in.proto != o.proto {
							o.escaped, changed = true, true
						}
					}
				}
			} else if !o.escaped && o.shape == 0 {
				a := e.f.virtual[o.site.Pool]
				shape, proto, ok := a.shape, a.proto, a.ok
				if ok {
					// Kept alive with the code by the VM, as its answer
					// was when asked here.
					e.f.VirtualAsked = append(e.f.VirtualAsked, o.site.Pool)
				}
				if !ok || shape == 0 || proto == 0 {
					o.escaped, changed = true, true
					continue
				}
				o.shape, o.proto = shape, proto
			}
			if !o.escaped {
				continue
			}
			for _, h := range o.holds {
				if !h.escaped {
					h.escaped, changed = true, true
				}
			}
		}
		if !changed {
			// A cycle of merges no object made in the code is of.
			for _, o := range e.objs {
				if !o.escaped && o.shape == 0 {
					o.escaped, changed = true, true
				}
			}
		}
	}
}

// live reports whether any object is left unmade.
func (e *escaper) live() bool {
	for _, o := range e.objs {
		if !o.escaped {
			return true
		}
	}
	return false
}

// syncEntries has the entries dropped be those the objects left unmade
// are merged with (virtual's entries), and reports whether it changed
// them. An object made after all, its entries are put back: the cuts so
// far undone, which is what makes it simple, and those still wanted made
// again. Which is why an entry costs nothing dropped for an object that
// is made in the end -- it had cost a loop its way in. The phis that merge
// an object with an entry's slot are the object still (its role), merging
// it alone.
func (e *escaper) syncEntries() bool {
	f := e.f
	var want []*Block
	for _, en := range e.entries {
		for _, o := range e.objs {
			if !o.escaped && slices.Contains(o.entries, en.Block) {
				want = append(want, en.Block)
				break
			}
		}
	}
	if slices.Equal(want, e.dropped) {
		return false
	}
	// What the walks made has an operand for each edge as it was.
	e.dropPhis()
	for i := len(e.cuts) - 1; i >= 0; i-- {
		c := &e.cuts[i]
		c.s.Preds = slices.Insert(c.s.Preds, c.j, c.pred)
		c.s.Backedge = slices.Insert(c.s.Backedge, c.j, c.backedge)
		for n, v := range c.phis {
			v.Args = slices.Insert(v.Args, c.j, c.args[n])
		}
	}
	e.cuts = e.cuts[:0]
	f.Entries = append(f.Entries[:0], e.entries...)
	f.Unresumed = append(f.Unresumed[:0], e.unresumed...)
	f.Unentered = f.Unentered[:0]
	entries := f.Entries[:0]
	for _, en := range f.Entries {
		if !slices.Contains(want, en.Block) {
			entries = append(entries, en)
			continue
		}
		if pc := int(en.Block.Header.PC); en.OSR && !slices.Contains(f.Unentered, pc) {
			// Its loop's header, the interpreter's PC.
			f.Unentered = append(f.Unentered, pc)
		}
		for _, s := range en.Block.Succs {
			if j := slices.Index(s.Preds, en.Block); j >= 0 {
				e.cut(s, j)
			}
		}
	}
	f.Entries = entries
	e.dropped = want
	reorder(f, e.cut)
	e.dom = nil
	return true
}

// dominators is the function's dominator tree, which only dropping
// entries changes.
func (e *escaper) dominators() *domTree {
	if e.dom == nil {
		e.dom = dominators(e.f)
	}
	return e.dom
}

// cut cuts edge j of block s, for syncEntries to undo.
func (e *escaper) cut(s *Block, j int) {
	c := edgeCut{s: s, pred: s.Preds[j], j: j, backedge: s.Backedge[j]}
	for _, v := range s.Values {
		if v.Op == OpPhi {
			c.phis, c.args = append(c.phis, v), append(c.args, v.Args[j])
			v.Args = slices.Delete(slices.Clone(v.Args), j, j+1)
		}
	}
	s.Preds = slices.Delete(slices.Clone(s.Preds), j, j+1)
	s.Backedge = slices.Delete(slices.Clone(s.Backedge), j, j+1)
	e.cuts = append(e.cuts, c)
}

// dropPhis takes the phis the walks made out of their blocks: unused, as
// no transform has run, and counted in no value's uses.
func (e *escaper) dropPhis() {
	if len(e.phis) == 0 {
		return
	}
	made := make(map[*Value]bool, len(e.phis))
	blocks := map[*Block]bool{}
	for k, p := range e.phis {
		made[p], blocks[k.b] = true, true
	}
	for b := range blocks {
		b.Values = slices.DeleteFunc(b.Values, func(v *Value) bool { return made[v] })
	}
	clear(e.phis)
}

// reorder puts the blocks the entries reach in reverse post-order from
// them again, as the builder ordered them (translate), and numbers them so:
// with an entry gone, a block it had been first met from may be met later
// than a block it dominates now.
func reorder(f *Func, cut func(s *Block, j int)) {
	n := len(f.Blocks)
	// By block: one put back has the ID it had, which another may have now.
	seen := make(map[*Block]bool, n)
	order := make([]*Block, 0, n)
	var visit func(*Block)
	visit = func(b *Block) {
		if seen[b] {
			return
		}
		seen[b] = true
		for i := len(b.Succs) - 1; i >= 0; i-- {
			visit(b.Succs[i])
		}
		order = append(order, b)
	}
	for i := len(f.Entries) - 1; i >= 0; i-- {
		visit(f.Entries[i].Block)
	}
	for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
		order[i], order[j] = order[j], order[i]
	}
	for _, b := range order {
		for j := 0; j < len(b.Preds); {
			if seen[b.Preds[j]] {
				j++
			} else {
				cut(b, j)
			}
		}
	}
	f.Blocks = append(f.Blocks[:0], order...)
	for i, b := range f.Blocks {
		b.ID = i
	}
}

// flow finds each object's state at every point the code uses it -- every
// read of it, every frame state naming it -- and what each of its
// operations becomes (plans), as TurboFan's escape analysis follows a
// virtual object's fields through the graph. Each block's state is its
// forward predecessors' -- the same shape, its properties merged by phis
// where they differ, at a loop's header all of them (header). An operation
// it cannot know, a shape it cannot follow, has the object escape (again).
func (e *escaper) flow() {
	f := e.f
	e.plans = map[*Value]escPlan{}
	e.uses = e.uses[:0]
	e.loops = e.loops[:0]
	e.making, e.reals, e.controls = map[*Value][]*matPoint{}, map[*Value][]realArg{}, map[*Block]*Value{}
	e.makings = 0
	dom := e.dominators()
	e.indexUses()
	for _, o := range e.objs {
		o.out, o.fields = nil, 0
		if !o.escaped {
			o.out = make([]*vstate, len(f.Blocks))
		}
	}
	cur := make([]*vstate, len(e.objs))
	for _, b := range f.Blocks {
		for k, o := range e.objs {
			cur[k] = nil
			if o.escaped {
				continue
			}
			def := o.defBlock()
			if !e.reached(def) {
				// On a path only a dropped entry took, gone with it.
				e.escape(o)
				continue
			}
			if !dom.dominates(def, b) {
				continue
			} else if b == def {
				if o.phi != nil {
					cur[k] = e.mergeInputs(o, b)
				}
				continue
			}
			cur[k] = e.merge(o, b)
		}
		if b.LoopHeader && b.Header != nil {
			e.record(escState{b: b, s: b.Header, head: true}, cur)
		}
		for _, v := range b.Values {
			if objs := e.mats[v]; objs != nil {
				e.materialize(v, objs, cur)
			}
			if v.State != nil {
				e.record(escState{v: v, s: v.State}, cur)
			}
			r, o := e.roleOf(v)
			if r == roleCall && !o.escaped {
				cur[o.id] = &vstate{shape: o.shape}
				continue
			}
			if (r == roleNone || r == roleCall) && v.Op != OpPhi {
				// An object made already, an operand: the object made -- of
				// a take too, which, made, keeps what it is given. One a
				// take found made only since its uses were classified has
				// not been made for it.
				for i, a := range v.Args {
					ar, ao := e.roleOf(a)
					if ar != roleObj || ao.escaped {
						continue
					}
					if st := cur[ao.id]; st != nil && st.real != nil {
						e.reals[v] = append(e.reals[v], realArg{i, st.real})
					} else if r == roleCall {
						e.escape(ao)
					}
				}
			}
			if v.Op == OpLoadCell {
				if r, o := e.roleOf(v.Args[0]); r == rolePropCell && !o.escaped {
					if p, ok := e.plans[v.Args[0]]; ok && p.kind == escField {
						e.plans[v] = escPlan{kind: escField, field: p.field}
						if fr, w := e.roleOf(p.field); fr == roleObj && !w.escaped {
							// Another object not made: the read is it, and
							// its uses are that object's.
							if lr, lw := e.roleOf(v); lr != roleObj || lw != w {
								e.set(v, roleObj, w)
								e.again = true
							}
						}
					}
				}
				continue
			}
			if v.Op != OpPropRead && v.Op != OpPropCell && v.Op != OpPropWrite || len(v.Args) == 0 {
				continue
			}
			pr, o := e.roleOf(v.Args[0])
			if pr != rolePtr || o.escaped {
				continue
			}
			st := cur[o.id]
			if st == nil {
				e.escape(o)
				continue
			}
			if st.real != nil {
				// Made: the object's own code, on the object made, what it
				// stores made too.
				e.reals[v] = append(e.reals[v], realArg{0, st.ptr})
				if v.Op == OpPropWrite {
					if wr, w := e.roleOf(v.Args[1]); wr == roleObj && !w.escaped {
						e.escape(w)
					}
				}
				continue
			}
			switch v.Op {
			case OpPropRead:
				if k, ok := ownField(v, st); ok {
					x := st.fields[k]
					if r, w := e.roleOf(x); r == roleObj && !w.escaped {
						// An object read as a number, which exits: made.
						e.escape(w)
						continue
					}
					e.plans[v] = escPlan{kind: escUnbox, field: x}
					continue
				}
				e.escape(o)
			case OpPropCell:
				if v.Tries() {
					e.escape(o)
					continue
				}
				if k, ok := ownField(v, st); ok {
					e.plans[v] = escPlan{kind: escField, field: st.fields[k]}
					continue
				}
				if chain, k, ok := protoIndex(v, st.shape); ok && len(chain.List()) != 0 && chain.List()[0].Object == o.proto {
					e.plans[v] = escPlan{kind: escHolder, holders: chain, index: k}
					continue
				}
				e.escape(o)
			case OpPropWrite:
				x := v.Args[1]
				if k, ok := ownIndex(v, st.shape); ok && k < len(st.fields) {
					fields := append([]*Value(nil), st.fields...)
					fields[k] = x
					cur[o.id] = &vstate{shape: st.shape, fields: fields, keys: st.keys, adds: st.adds}
					e.plans[v] = escPlan{kind: escDrop}
					continue
				}
				add := addFrom(v, st.shape)
				if add == nil {
					e.escape(o)
					continue
				}
				p := escPlan{kind: escDrop}
				if !add.Define {
					chain := add.Chain()
					if len(chain) == 0 || chain[0].Object != o.proto {
						e.escape(o)
						continue
					}
					p = escPlan{kind: escCheck, add: add}
				}
				fields := append(append([]*Value(nil), st.fields...), x)
				keys := append(append([]uint32(nil), st.keys...), v.Key)
				adds := append(append([]*PropertyAdd(nil), st.adds...), add)
				cur[o.id] = &vstate{shape: add.Next, fields: fields, keys: keys, adds: adds}
				o.fields = max(o.fields, len(fields))
				e.plans[v] = p
			}
		}
		if b.State != nil {
			e.record(escState{b: b, s: b.State}, cur)
		}
		if r, o := e.roleOf(b.Control); r == roleObj && !o.escaped {
			if st := cur[o.id]; st != nil && st.real != nil {
				e.controls[b] = st.real
			} else {
				e.escape(o)
			}
		}
		for k, o := range e.objs {
			if !o.escaped {
				o.out[b.ID] = cur[k]
			}
		}
	}
	e.backEdges()
}

// reached reports whether b is among the function's blocks: one only a
// dropped entry reached is not (reorder), and has an ID that is another's.
func (e *escaper) reached(b *Block) bool {
	return b.ID < len(e.f.Blocks) && e.f.Blocks[b.ID] == b
}

// materialize has the objects objs that v needs made, and are not yet,
// made before it: its state then, which names them not made, is the
// making's (matState); after it, they are the objects made. An object
// holding another not made, of a state whose additions are not known, or
// past the keep cells, is made where it is taken instead.
func (e *escaper) materialize(v *Value, objs []*virtual, cur []*vstate) {
	f := e.f
	recorded := false
	for _, o := range objs {
		st := cur[o.id]
		if o.escaped || st != nil && st.real != nil {
			continue
		}
		if st == nil {
			// Not known here: made where it is taken.
			e.escape(o)
			continue
		}
		pool := e.poolOf(o, st)
		ok := pool != 0 && len(st.adds) == len(st.fields)
		for _, x := range st.fields {
			if r, w := e.roleOf(x); r == roleObj && !w.escaped {
				ok = false
			}
		}
		if !ok {
			e.escape(o)
			continue
		}
		if e.points == nil {
			e.points = map[[2]int]*matPoint{}
		}
		if f.Keeps+e.makings >= f.keepLimit() {
			// Past the keep cells: each making takes one, numbered when
			// it is made (makeAt).
			e.escape(o)
			continue
		}
		e.makings++
		key := [2]int{v.ID, o.id}
		mp := e.points[key]
		if mp == nil {
			mp = &matPoint{o: o, obj: f.slot(), ptr: f.slot()}
			e.points[key] = mp
		}
		mp.st, mp.pool, mp.at = st, pool, v
		if !recorded {
			// The state before v, v's own being rewritten past the making.
			e.record(escState{v: v, s: v.State, mat: true}, cur)
			recorded = true
		}
		e.making[v] = append(e.making[v], mp)
		cur[o.id] = made(mp.obj, mp.ptr)
	}
}

// pruneMade has an object the code would make on every path anyway, at
// the uses that need it made (mats), made where it is taken instead, as
// it was: left unmade to be made later, it saves nothing, and costs the
// compile a walk of the function, an entry dropped and its every value's
// rewriting. Kept are the objects that some path to a return takes past
// their making -- or round their loop, to be taken again -- and those a
// merge kept needs (mergeAt). It is here, before the walk, so that an
// object not worth it costs no more than one that escapes.
func (e *escaper) pruneMade() {
	if len(e.mats) == 0 {
		return
	}
	f := e.f
	made := make([][]bool, len(e.objs))
	for v, objs := range e.mats {
		for _, o := range objs {
			if made[o.id] == nil {
				made[o.id] = make([]bool, len(f.Blocks))
			}
			made[o.id][v.Block.ID] = true
		}
	}
	seen := make([]bool, len(f.Blocks))
	needed := make([]bool, len(e.objs))
	for changed := true; changed; {
		changed = false
		clear(needed)
		for _, m := range e.objs {
			if m.phi != nil && !m.escaped {
				for _, in := range e.inputsOf(m) {
					needed[in.id] = true
				}
			}
		}
		for _, o := range e.objs {
			if o.escaped || made[o.id] == nil || needed[o.id] || o.phi != nil && o.phi.Block.LoopHeader {
				// A loop's merge saves an object each turn, made once.
				continue
			}
			if !e.passesUnmade(o, made[o.id], seen) {
				e.escape(o)
				changed = true
			}
		}
		if changed {
			e.propagate()
		}
	}
}

// passesUnmade reports whether a path from o's taking reaches a return,
// or its taking again, through no block that makes it (made).
func (e *escaper) passesUnmade(o *virtual, made, seen []bool) bool {
	def := o.defBlock()
	if made[def.ID] {
		return false
	}
	clear(seen)
	work := []*Block{def}
	seen[def.ID] = true
	for len(work) != 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]
		if b.Kind == BlockReturn {
			return true
		}
		for _, s := range b.Succs {
			if s == def {
				return true
			}
			if !seen[s.ID] && !made[s.ID] {
				seen[s.ID] = true
				work = append(work, s)
			}
		}
	}
	return false
}

// poolOf is the pool an object of state st is made from: the pool of an
// object it is, or merges, whose objects have the shape its first
// addition is from; 0 if there is none. A construction's pool may be
// filled for another prototype since its take checked it, where it runs
// out (jitRefillPool): the first addition, which compares the shape, then
// exits, and an object with none could not tell, so it has no pool.
func (e *escaper) poolOf(o *virtual, st *vstate) uintptr {
	root := o.shape
	if len(st.adds) != 0 {
		root = st.adds[0].From
	}
	seen := map[*virtual]bool{}
	var find func(o *virtual) uintptr
	find = func(o *virtual) uintptr {
		if seen[o] {
			return 0
		}
		seen[o] = true
		if o.phi == nil {
			if o.shape == root && (len(st.adds) != 0 || !o.site.Receiver) {
				return o.site.Pool
			}
			return 0
		}
		for _, in := range e.inputsOf(o) {
			if p := find(in); p != 0 {
				return p
			}
		}
		return 0
	}
	return find(o)
}

// merge is o's state at b's start: its forward predecessors', which have
// the same shape, the properties that differ merged by phis; nil at a
// loop's header, or where a predecessor's is not known.
func (e *escaper) merge(o *virtual, b *Block) *vstate {
	if b.LoopHeader {
		return e.header(o, b)
	}
	var in *vstate
	same := true
	for j, p := range b.Preds {
		if b.Backedge[j] {
			return nil
		}
		ps := o.out[p.ID]
		if ps == nil {
			return nil
		}
		switch {
		case in == nil:
			in = ps
		case ps == in:
		case !sameMade(ps, in) || ps.shape != in.shape || len(ps.fields) != len(in.fields):
			// Made on one path and not on another, or two made: made
			// where it is taken, as before.
			e.escape(o)
			return nil
		case in.real != nil:
		default:
			same = false
		}
	}
	if same || in.real != nil {
		return in
	}
	fields := make([]*Value, len(in.fields))
	for k := range fields {
		x := in.fields[k]
		for _, p := range b.Preds {
			if o.out[p.ID].fields[k] != x {
				x = nil
				break
			}
		}
		if x == nil {
			x = e.phi(o, b, k)
		}
		fields[k] = x
	}
	return &vstate{shape: in.shape, fields: fields, keys: in.keys, adds: in.adds}
}

// mergeInputs is merged object m's state at its phi's block: each edge's
// object's state there, which have the same shape, their properties merged
// by phis where they differ; nil, m made, where they do not, or where an
// object merged may be used past the merge (liveAt), whose properties m's
// writes would then change.
func (e *escaper) mergeInputs(m *virtual, b *Block) *vstate {
	for _, o := range e.inputsOf(m) {
		if e.liveAt(o, m, b) {
			e.escape(m)
			return nil
		}
	}
	if b.LoopHeader {
		return e.header(m, b)
	}
	states := make([]*vstate, len(b.Preds))
	for j, p := range b.Preds {
		_, o := e.roleOf(m.phi.Args[j])
		var ps *vstate
		if !b.Backedge[j] && o != nil && !o.escaped {
			ps = o.out[p.ID]
		}
		if ps == nil || ps.real != nil || j > 0 && (ps.shape != states[0].shape || len(ps.fields) != len(states[0].fields)) {
			e.escape(m)
			return nil
		}
		states[j] = ps
	}
	in := states[0]
	fields := make([]*Value, len(in.fields))
	for k := range fields {
		x := in.fields[k]
		for _, st := range states {
			if st.fields[k] != x {
				x = nil
				break
			}
		}
		if x == nil {
			args := make([]*Value, len(states))
			for j, st := range states {
				args[j] = st.fields[k]
			}
			x = e.phiOf(m, b, k, args)
		}
		fields[k] = x
	}
	m.fields = max(m.fields, len(fields))
	return &vstate{shape: in.shape, fields: fields, keys: in.keys, adds: in.adds}
}

// liveAt reports whether object o, merged into m at b, may be used past
// the merge: whether a use of it, but m's phi, is on a path from b that
// does not make it again first -- a value's operand, a control, a frame
// state's slot (useBlocks) -- or an object holds it, whose state names it
// with no use of its own, wherever that goes.
func (e *escaper) liveAt(o, m *virtual, b *Block) bool {
	if o.held {
		return true
	}
	f := e.f
	used := make([]bool, len(f.Blocks))
	for _, id := range e.useBlocks[o.id] {
		used[id] = true
	}
	for _, u := range e.phiUses[o.id] {
		if u != m.phi {
			used[u.Block.ID] = true
		}
	}
	def := o.defBlock()
	seen := make([]bool, len(f.Blocks))
	work := []*Block{b}
	seen[b.ID] = true
	for len(work) != 0 {
		blk := work[len(work)-1]
		work = work[:len(work)-1]
		if used[blk.ID] {
			return true
		}
		for _, s := range blk.Succs {
			if !seen[s.ID] && s != def {
				seen[s.ID] = true
				work = append(work, s)
			}
		}
	}
	return false
}

// liveIn reports whether object o may be used in or past loop header b: a
// use of it in a block b dominates -- but by b's own phis, which merge it
// as it comes in (mergeAt). One that is not has no state there, and none
// is merged for it round the loop.
func (e *escaper) liveIn(o *virtual, b *Block) bool {
	for _, id := range e.useBlocks[o.id] {
		if e.dom.dominates(b, e.f.Blocks[id]) {
			return true
		}
	}
	for _, u := range e.phiUses[o.id] {
		if u.Block != b && e.dom.dominates(b, u.Block) {
			return true
		}
	}
	return false
}

// indexUses lists, for each object, the blocks its values are used in --
// as an operand but a phi's, a control, a frame state's slot -- and the
// phis that merge them (liveAt, liveIn).
func (e *escaper) indexUses() {
	f := e.f
	if cap(e.useBlocks) < len(e.objs) {
		e.useBlocks, e.phiUses = make([][]int32, len(e.objs)), make([][]*Value, len(e.objs))
	}
	e.useBlocks, e.phiUses = e.useBlocks[:len(e.objs)], e.phiUses[:len(e.objs)]
	for i := range e.useBlocks {
		e.useBlocks[i], e.phiUses[i] = e.useBlocks[i][:0], e.phiUses[i][:0]
	}
	mark := func(x *Value, b *Block) {
		if r, o := e.roleOf(x); r != roleNone && !o.escaped {
			if us := e.useBlocks[o.id]; len(us) == 0 || us[len(us)-1] != int32(b.ID) {
				e.useBlocks[o.id] = append(us, int32(b.ID))
			}
		}
	}
	state := func(s *FrameState, b *Block) {
		if s != nil {
			for _, i := range f.Present(s) {
				mark(s.Slots[i], b)
			}
		}
	}
	for _, b := range f.Blocks {
		var last *FrameState
		for _, v := range b.Values {
			if v.Op == OpPhi {
				for _, a := range v.Args {
					if r, o := e.roleOf(a); r != roleNone && !o.escaped {
						e.phiUses[o.id] = append(e.phiUses[o.id], v)
					}
				}
				continue
			}
			for _, a := range v.Args {
				mark(a, b)
			}
			if v.State != last {
				state(v.State, b)
				last = v.State
			}
		}
		mark(b.Control, b)
		state(b.State, b)
		state(b.Header, b)
	}
}

// predState is o's state at the end of b's predecessor j: for an object
// b's phi merges (mergeAt), that of the object the phi's operand j is.
func (e *escaper) predState(o *virtual, b *Block, j int) *vstate {
	if o.phi != nil && o.phi.Block == b {
		r, in := e.roleOf(o.phi.Args[j])
		if r != roleObj || in.escaped || in.out == nil {
			return nil
		}
		o = in
	}
	return o.out[b.Preds[j].ID]
}

// loopState is an object's state at a loop's header, whose phis' operands
// on its back edges flow fills in once it has walked the loop (backEdges).
type loopState struct {
	o  *virtual
	b  *Block
	st *vstate
}

// header is o's state at loop header b: its forward predecessors', which
// have the same shape, every property a phi, as TurboFan's and Graal's
// escape analyses give a loop's header a virtual object's fields as phis.
// What its back edges bring is known only after the loop (backEdges): an
// object of another shape there, or one made, has it made. Each phi's
// operands are set anew at each walk.
func (e *escaper) header(o *virtual, b *Block) *vstate {
	if !e.liveIn(o, b) {
		return nil
	}
	// Read once: an object held in a property below escapes, which may be
	// one this merges (x.next = x), whose state is then not known.
	states := make([]*vstate, len(b.Preds))
	var in *vstate
	for j := range b.Preds {
		if b.Backedge[j] {
			continue
		}
		ps := e.predState(o, b, j)
		if ps == nil || in != nil && (!sameMade(ps, in) || ps.shape != in.shape || len(ps.fields) != len(in.fields)) ||
			ps.real != nil && o.phi != nil && o.phi.Block == b {
			e.escape(o)
			return nil
		}
		states[j] = ps
		if in == nil {
			in = ps
		}
	}
	if in == nil {
		e.escape(o)
		return nil
	}
	if in.real != nil {
		// Made before the loop: made round it, if its back edges bring
		// the same (backEdges).
		e.loops = append(e.loops, loopState{o, b, in})
		return in
	}
	fields := make([]*Value, len(in.fields))
	for k := range fields {
		args := make([]*Value, len(b.Preds))
		p := e.phiOf(o, b, k, nil)
		for j := range b.Preds {
			if b.Backedge[j] {
				args[j] = p
			} else {
				args[j] = states[j].fields[k]
				if r, w := e.roleOf(args[j]); r == roleObj && !w.escaped {
					e.escape(w)
				}
			}
		}
		p.Args = args
		fields[k] = p
	}
	o.fields = max(o.fields, len(fields))
	st := &vstate{shape: in.shape, fields: fields, keys: in.keys, adds: in.adds}
	e.loops = append(e.loops, loopState{o, b, st})
	return st
}

// backEdges sets the operands on the back edges of the phis header made,
// what the loop's body leaves each object as, which must be of the shape
// it had at the header; it is made if not.
func (e *escaper) backEdges() {
	for _, l := range e.loops {
		if l.o.escaped {
			continue
		}
		for j := range l.b.Preds {
			if !l.b.Backedge[j] {
				continue
			}
			ps := e.predState(l.o, l.b, j)
			if ps == nil || !sameMade(ps, l.st) || ps.shape != l.st.shape || len(ps.fields) != len(l.st.fields) {
				e.escape(l.o)
				break
			}
			for k, p := range l.st.fields {
				x := ps.fields[k]
				if r, w := e.roleOf(x); r == roleObj && !w.escaped {
					e.escape(w)
				}
				p.Args[j] = x
			}
		}
	}
}

// phi is the phi merging o's property k at b, made the first time it is
// asked for: what o's predecessors' states have there.
func (e *escaper) phi(o *virtual, b *Block, k int) *Value {
	args := make([]*Value, len(b.Preds))
	for j, pred := range b.Preds {
		args[j] = o.out[pred.ID].fields[k]
	}
	return e.phiOf(o, b, k, args)
}

// phiOf is the phi merging o's property k at b of args, by b's
// predecessors, made the first time it is asked for.
func (e *escaper) phiOf(o *virtual, b *Block, k int, args []*Value) *Value {
	key := phiKey{b, o.id, k}
	if e.phis == nil {
		e.phis = map[phiKey]*Value{}
	}
	if p := e.phis[key]; p != nil {
		return p
	}
	f := e.f
	for j := range args {
		if r, w := e.roleOf(args[j]); r == roleObj && !w.escaped {
			// An object not made, merged with another value: made.
			e.escape(w)
		}
	}
	p := f.slot()
	p.Op, p.Type, p.Args, p.Block = OpPhi, Tagged, f.refsOf(len(args)), b
	copy(p.Args, args)
	b.Values = append([]*Value{p}, b.Values...)
	e.phis[key] = p
	return p
}

// record notes u, a frame state's use at a point, if the state names objects
// not made there: their states, and those of the objects their properties
// hold, which Go makes with them. An object whose state is not known there
// escapes.
func (e *escaper) record(u escState, cur []*vstate) {
	var snap []escSnap
	var add func(o *virtual)
	add = func(o *virtual) {
		for _, x := range snap {
			if x.o == o {
				return
			}
		}
		st := cur[o.id]
		if st == nil {
			e.escape(o)
			return
		}
		snap = append(snap, escSnap{o, st})
		if st.real != nil {
			return
		}
		for _, x := range st.fields {
			if r, w := e.roleOf(x); r == roleObj && !w.escaped {
				add(w)
			}
		}
	}
	for _, i := range e.f.Present(u.s) {
		if r, o := e.roleOf(u.s.Slots[i]); r == roleObj && !o.escaped {
			add(o)
		}
	}
	if len(snap) != 0 {
		u.snap = snap
		e.uses = append(e.uses, u)
	}
}

// ownIndex is the index of the property v, a read or a write of an object
// of shape, finds in its own table, if v's cache knows that shape so.
func ownIndex(v *Value, shape uintptr) (int, bool) {
	if v.Const.Bits == uint64(shape) && v.Holders() == nil && shape != 0 {
		return v.Index, true
	}
	for _, c := range v.Cases() {
		if c.Shape == shape && c.Holders[0].Object == 0 {
			return int(c.Index), true
		}
	}
	return 0, false
}

// ownField is the index of the property v, a read of an object of state
// st, finds in its own table: where its cache knows st's shape so, or
// where it searches the table (Scans, Megamorphic) and the object has its
// key.
func ownField(v *Value, st *vstate) (int, bool) {
	if k, ok := ownIndex(v, st.shape); ok {
		return k, k < len(st.fields)
	}
	if v.Scans() || v.Megamorphic() {
		for k, key := range st.keys {
			if key == v.Key {
				return k, true
			}
		}
	}
	return 0, false
}

// protoIndex is where v, a read of an object of shape, finds its property
// on its prototypes, if v's cache knows that shape so: the prototypes,
// from the object's own, and the index in the last one's table.
func protoIndex(v *Value, shape uintptr) (HolderChain, int, bool) {
	if v.Const.Bits == uint64(shape) && v.Holders() != nil && shape != 0 {
		return *v.Holders(), v.Index, true
	}
	for _, c := range v.Cases() {
		if c.Shape == shape && c.Holders[0].Object != 0 {
			return HolderChain{Holders: c.Holders, More: c.More}, int(c.Index), true
		}
	}
	return HolderChain{}, 0, false
}

// addFrom is the addition v, a write, makes to an object of shape, if its
// cache knows one.
func addFrom(v *Value, shape uintptr) *PropertyAdd {
	if a := v.Add(); a != nil && a.From == shape {
		return a
	}
	for _, a := range v.Adds() {
		if a.From == shape {
			return a
		}
	}
	return nil
}

// homes gives each object left unmade its home, past every slot the
// function's states have, and reports whether they fit the slots an exit
// may write (abi.MaxRecords); the object with the most properties escapes
// where they do not.
func (e *escaper) homes() bool {
	f := e.f
	at := f.slotCount
	for _, b := range f.Blocks {
		for _, s := range []*FrameState{b.State, b.Header} {
			if s != nil {
				at = max(at, s.Own())
			}
		}
		for _, v := range b.Values {
			if v.State != nil {
				at = max(at, v.State.Own())
			}
		}
	}
	at = max(at, f.Locals+f.StackSize)
	var big *virtual
	for _, o := range e.objs {
		if o.escaped {
			continue
		}
		o.home = at
		at += abi.VirtualFields + o.fields
		if big == nil || o.fields > big.fields {
			big = o
		}
	}
	if at > abi.MaxRecords {
		e.escape(big)
		return false
	}
	e.homeAt = at
	return true
}

// word is the constant word w, which o's states name, made at o's take.
func (e *escaper) word(o *virtual, w uint64) *Value {
	if c := o.words[w]; c != nil {
		return c
	}
	if o.words == nil {
		o.words = map[uint64]*Value{}
	}
	c := e.f.slot()
	c.Op, c.Type, c.Block = OpConst, Tagged, o.defBlock()
	c.Const = ir.Value{Kind: ir.Word, Bits: w}
	o.words[w] = c
	return c
}

// transform makes the code of what flow found: each object not made, its
// take the checks its making made of the heap; its operations their plans;
// and its frame states naming it by its word, its fields at its home.
func (e *escaper) transform() {
	f := e.f
	// The states, rewritten: one for each set of the objects' states a
	// state is used at.
	type rewritten struct {
		snap []escSnap
		s    *FrameState
	}
	done := map[*FrameState][]rewritten{}
	// A making exits to its user's state as it is, unless that names
	// objects not made, which the uses below rewrite.
	e.matState = make(map[*Value]*FrameState, len(e.making))
	for v := range e.making {
		e.matState[v] = v.State
	}
	for _, u := range e.uses {
		var s *FrameState
		for _, r := range done[u.s] {
			if sameSnap(r.snap, u.snap) {
				s = r.s
				break
			}
		}
		if s == nil {
			s = e.rewriteState(u.s, u.snap)
			done[u.s] = append(done[u.s], rewritten{u.snap, s})
		}
		switch {
		case u.mat:
			e.matState[u.v] = s
		case u.v != nil:
			u.v.State = s
		case u.head:
			u.b.Header = s
		default:
			u.b.State = s
		}
	}
	subst := map[*Value]*Value{}
	drop := map[*Value]bool{}
	for _, o := range e.objs {
		if !o.escaped {
			drop[o.cell], drop[o.obj] = true, true
		}
	}
	// A merge's object's words are made in its phi's block, after the
	// phis.
	words := map[*Block][]*Value{}
	for _, o := range e.objs {
		if !o.escaped && o.phi != nil {
			words[o.phi.Block] = append(words[o.phi.Block], sortedWords(o)...)
		}
	}
	for _, b := range f.Blocks {
		values := make([]*Value, 0, len(b.Values)+4)
		pending := words[b]
		for _, v := range b.Values {
			if pending != nil && v.Op != OpPhi {
				values, pending = append(values, pending...), nil
			}
			for _, mp := range e.making[v] {
				if !mp.o.escaped {
					values = append(values, e.makeAt(b, v, mp)...)
				}
			}
			for _, ra := range e.reals[v] {
				v.Args[ra.i] = ra.x
			}
			r, o := e.roleOf(v)
			if o != nil && o.escaped {
				r, o = roleNone, nil
			}
			switch {
			case drop[v]:
				continue
			case r == roleCall:
				values = append(values, e.take(o)...)
				continue
			case r == rolePtr:
				continue
			case v.Op == OpCheckInit:
				if ar, ao := e.roleOf(v.Args[0]); ar == roleObj && !ao.escaped {
					continue
				}
			}
			p, ok := e.plans[v]
			if !ok {
				if r == roleObj || r == roleKeep {
					// Another value of the object -- a phi merging it with
					// itself, a keep's copy -- whose uses are all its own
					// code's, made into what they are.
					continue
				}
				values = append(values, v)
				continue
			}
			switch p.kind {
			case escDrop:
			case escField:
				if v.Op == OpLoadCell {
					// An own property's cell's read: the value; the cell,
					// which only such reads use, goes.
					subst[v] = p.field
				}
			case escUnbox:
				u := e.guard(b, v, OpUnboxF64, Float64, p.field)
				subst[v] = u
				values = append(values, u)
			case escHolder:
				c := e.guard(b, v, OpHolderCell, Source)
				c.Key, c.Index = v.Key, p.index
				chain := p.holders
				f.cache(c).Holders = &chain
				subst[v] = c
				values = append(values, c)
			case escCheck:
				c := e.guard(b, v, OpChainCheck, None)
				f.cache(c).Add = p.add
				values = append(values, c)
			}
		}
		b.Values = append(values, pending...)
		if x := e.controls[b]; x != nil {
			b.Control = x
		}
	}
	find := func(v *Value) *Value {
		for {
			w, ok := subst[v]
			if !ok {
				return v
			}
			v = w
		}
	}
	rewrite(f, find)
	chainChecks(f)
	for _, o := range e.objs {
		if !o.escaped {
			f.Virtuals++
		}
	}
	f.HomesEnd = max(f.HomesEnd, e.homeAt)
	recount(f)
}

// makeAt is the code that makes mp's object before v: its pool's next
// object, as an object literal's take is made, and its properties defined
// on it along the additions its state was made by, each exiting, where it
// cannot, to the state before v, which names the object not made.
func (e *escaper) makeAt(b *Block, v *Value, mp *matPoint) []*Value {
	f := e.f
	s := e.matState[v]
	at := func(g *Value) *Value {
		g.Block, g.Aux, g.State = b, int(ir.HostExit), s
		s.addUse()
		return g
	}
	call := f.slot()
	call.Op, call.Type = OpCall, Tagged
	at(call)
	f.cache(call).Calls = []*CallSite{{Pool: mp.pool, Alloc: true, Literal: true, ThisSlot: -1}}
	call.Index = f.Keeps
	f.Keeps++
	cell := f.slot()
	cell.Op, cell.Type, cell.Block, cell.Args = OpCallCell, Source, b, f.refsOf(1)
	cell.Args[0] = call
	obj := mp.obj
	obj.Op, obj.Type, obj.Block, obj.Args, obj.Shadow = OpKept, Tagged, b, f.refsOf(2), cell
	obj.Args[0], obj.Args[1] = cell, call
	ptr := mp.ptr
	ptr.Op, ptr.Type, ptr.Args = OpObjectOf, Ptr, f.refsOf(1)
	ptr.Args[0] = obj
	at(ptr)
	vs := []*Value{call, cell, obj, ptr}
	for k, x := range mp.st.fields {
		add := *mp.st.adds[k]
		add.Define, add.Key = true, mp.st.keys[k]
		w := f.slot()
		w.Op, w.Type, w.Args, w.Key = OpPropWrite, None, f.refsOf(2), mp.st.keys[k]
		w.Args[0], w.Args[1] = ptr, x
		at(w)
		f.cache(w).Add = &add
		vs = append(vs, w)
	}
	return vs
}

// chainChecks drops a check of prototypes (OpChainCheck) another in its
// block made before it, nothing between them that may change a shape
// (changesShapes): a constructor's additions, each of which checks the
// same chain.
func chainChecks(f *Func) {
	for _, b := range f.Blocks {
		var known [][]Holder
		values := b.Values[:0]
		for _, v := range b.Values {
			if changesShapes(v) {
				known = known[:0]
			}
			if v.Op == OpChainCheck {
				chain := v.Add().Chain()
				if slices.ContainsFunc(known, func(k []Holder) bool { return slices.Equal(k, chain) }) {
					continue
				}
				known = append(known, chain)
			}
			values = append(values, v)
		}
		b.Values = values
	}
}

// take is what o's take becomes: room for its home in the VM's stack, and
// for a construction's receiver, its constructor's prototype checked as
// the take checked it; then the words its states name.
func (e *escaper) take(o *virtual) []*Value {
	f, call := e.f, o.call
	var vs []*Value
	room := e.guard(call.Block, call, OpFrameRoom, None)
	room.Index, room.Key = e.homeAt-f.Locals, 1
	vs = append(vs, room)
	if o.site.Receiver {
		ptr := e.guard(call.Block, call, OpObjectOf, Ptr, call.Args[0])
		// A prototype replaced since deoptimizes, as V8's code does where a
		// map it depends on changes: compiled again, the code checks the
		// new one.
		is := e.guard(call.Block, call, OpProtoIs, None, ptr)
		is.Index, is.Key, is.Const = o.site.ProtoIndex, o.site.ProtoKey, ir.Value{Bits: uint64(o.proto)}
		is.Aux = int(ir.GuardExit)
		vs = append(vs, ptr, is)
	}
	return append(vs, sortedWords(o)...)
}

// sortedWords is o's words in the order they were made: values, and so
// code, must not vary from one compile to the next.
func sortedWords(o *virtual) []*Value {
	var vs []*Value
	for _, c := range o.words {
		vs = append(vs, c)
	}
	slices.SortFunc(vs, func(x, y *Value) int { return x.ID - y.ID })
	return vs
}

// guard is a value of op at v's place, exiting where v exits.
func (e *escaper) guard(b *Block, v *Value, op Op, t Type, args ...*Value) *Value {
	f := e.f
	g := f.slot()
	g.Op, g.Type, g.Block, g.Args = op, t, b, f.refsOf(len(args))
	copy(g.Args, args)
	g.Aux, g.State = v.Aux, v.State
	if g.State != nil {
		g.State.addUse()
	}
	return g
}

// sameSnap reports whether two sets of objects' states are the same.
func sameSnap(a, b []escSnap) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rewriteState is s with the objects snap has the states of named by their
// words, their homes holding their shapes and their properties, which name
// the objects not made among them so too.
func (e *escaper) rewriteState(s *FrameState, snap []escSnap) *FrameState {
	f := e.f
	end := s.Own()
	for _, x := range snap {
		if x.st.real == nil {
			end = max(end, x.o.home+abi.VirtualFields+x.o.fields)
		}
	}
	n := f.newState(*s)
	n.Slots = f.refsOf(end)
	copy(n.Slots, s.Slots[:s.Own()])
	n.Virtual = end - s.Own()
	named := func(x *Value) *Value {
		r, o := e.roleOf(x)
		if r != roleObj || o.escaped {
			return x
		}
		for _, y := range snap {
			if y.o == o {
				if y.st.real != nil {
					return y.st.real
				}
				return e.word(o, abi.VirtualWord(o.home, len(y.st.fields)))
			}
		}
		panic("ssa: an object not made with no state")
	}
	for i := range n.Slots[:s.Own()] {
		if x := n.Slots[i]; x != nil {
			n.Slots[i] = named(x)
		}
	}
	for _, y := range snap {
		o := y.o
		if y.st.real != nil {
			continue
		}
		n.Slots[o.home] = e.word(o, uint64(y.st.shape))
		for k, x := range y.st.fields {
			n.Slots[o.home+abi.VirtualFields+k] = named(x)
		}
	}
	return n
}
