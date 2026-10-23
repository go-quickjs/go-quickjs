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
// anything but its own code may see it: a store of it into an object that
// is made, a call's operand, a return, a phi merging it with anything but
// itself, a loop's header's state, and every operation that would read it
// in a way not known here. A phi merging it with the slot an entry loads,
// at an entry there only for Go to go on at after an instruction it ran
// (Entry's Resume), is the object: the entry is dropped, Go going on in the
// interpreter there instead, to the next loop header, as after a guard's
// exit. Its stores into it may change its shape only along the shapes its
// sites' caches know, from the shape its pool's objects have, the same
// shape on every path to a merge (TurboFan merges virtual objects of one
// map), and its reads are of the shape it has there: one of its own
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
// (OpProtoIs) -- each kept alive by the VM; false if Go cannot make one so.
type VirtualFeedback interface {
	VirtualRoot(pool uintptr) (shape, proto uintptr, ok bool)
}

// virtual is an object the code may not make: the pool's take (OpCall), its
// keep cell and its value (OpCallCell, OpKept).
type virtual struct {
	id              int
	call, cell, obj *Value
	site            *CallSite
	shape, proto    uintptr
	escaped         bool
	// holds are the objects stored into this one's properties, which escape
	// if it does.
	holds []*virtual
	// entries are the entry blocks a phi merges the object with, dropped.
	entries []*Block
	// out is its state at each block's end, by block ID; nil where it is
	// not known (not made yet, or past a loop's header).
	out []*vstate
	// fields is the most properties it has; home the first of its home's
	// slots (abi.VirtualFields of them, then its properties').
	fields, home int
	// words are the constants its states name it by, its shapes and its
	// prototype by (ir.Word), by the word.
	words map[uint64]*Value
}

// vstate is an object's state at a point: its shape, and its properties'
// values in its table's order. A state is never changed, but replaced.
type vstate struct {
	shape  uintptr
	fields []*Value
	// keys are the properties' atoms, as the writes that added them name
	// them: what a read that searches the object's own table finds
	// (PropertySite's Scan, Megamorphic).
	keys []uint32
}

// Roles of the values escape analysis looks at (escaper's role).
const (
	roleNone = iota
	roleCall
	roleCell
	roleObj
	rolePtr
	rolePropCell
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
}

type escSnap struct {
	o  *virtual
	st *vstate
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
	phis   map[[3]int]*Value
	dom    *domTree
	again  bool
	homeAt int
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
	// Each round knows more of what the objects' code is -- a property's
	// read that is another object not made, whose uses are that one's --
	// or that more of them escape, until neither changes.
	dropped := false
	for {
		e.again = false
		e.classify()
		if !e.live() {
			return dropped
		}
		dropped = e.dropEntries() || dropped
		if e.again {
			continue
		}
		e.flow()
		if e.again {
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

// candidates finds the pools' takes the code may not make.
func (e *escaper) candidates() bool {
	f := e.f
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpCall || len(v.Calls()) != 1 {
				continue
			}
			site := v.Calls()[0]
			literal := site.Literal && site.Alloc && site.Argc == 0 && site.Pool != 0 && len(v.Args) == 0
			receiver := site.Receiver && site.Pool != 0 && !site.Derived && site.Parent == 0 && site.Via == 0 && len(v.Args) >= 1
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
	if v.ID < len(e.role) {
		e.role[v.ID], e.of[v.ID] = role, int32(o.id+1)
	}
}

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

// entryOf is the entry whose block b is, if Go goes on there only after an
// instruction it ran (Entry's Resume).
func (e *escaper) entryOf(b *Block) bool {
	if len(b.Preds) != 0 {
		return false
	}
	for _, en := range e.f.Entries {
		if en.Block == b {
			return en.Resume
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
	e.dom = dominators(f)
	for _, o := range e.objs {
		o.holds = o.holds[:0]
	}
	for changed := true; changed; {
		changed = false
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if r, _ := e.roleOf(v); r != roleNone {
					continue
				}
				if v.Op == OpPhi && e.alias(v, false) || v.Op != OpPhi && e.assign(v) {
					changed = true
				}
			}
		}
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if r, _ := e.roleOf(v); r == roleNone && v.Op == OpPhi {
				// A phi merging an object with what no entry loads.
				e.alias(v, true)
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
			e.escape(o)
		}
		// A yield a loop's step resumed natively writes the generator's
		// frame itself (Block's Yield), where Go makes nothing.
		e.state(b.State, b.Yield)
		e.state(b.Header, true)
	}
	for _, o := range e.objs {
		if o.cell == nil || o.obj == nil {
			e.escape(o)
		}
	}
	e.propagate()
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
	for _, a := range v.Args {
		r, o := e.roleOf(a)
		if r == roleObj {
			if obj != nil && obj != o {
				e.escape(obj)
				e.escape(o)
				return false
			}
			obj = o
		} else if r == roleNone && a.Op == OpPhi && a != v && !final {
			return false
		}
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
		if v.Block.LoopHeader || !e.entryOf(a.Block) || !e.dom.dominates(a.Block, p) {
			e.escape(obj)
			return false
		}
		entries = append(entries, a.Block)
	}
	obj.entries = append(obj.entries, entries...)
	e.set(v, roleObj, obj)
	return true
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
		case v.Op == OpObjectOf, v.Op == OpCheckInit:
			return
		case v.Op == OpPhi:
			if vr, vo := e.roleOf(v); vr == roleObj && vo == o {
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
	}
	e.escape(o)
}

// state has every object a frame state names escape where Go could not
// make it from the state: a loop's header's, a parameter list's prologue's.
func (e *escaper) state(s *FrameState, header bool) {
	if s == nil {
		return
	}
	for _, x := range s.Slots {
		if r, o := e.roleOf(x); r != roleNone {
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
			if !o.escaped && o.shape == 0 {
				shape, proto, ok := e.f.virtual.VirtualRoot(o.site.Pool)
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

// dropEntries drops the entries the objects left unmade are merged with
// (virtual's entries), and the phis that merged them, which are the
// objects now.
func (e *escaper) dropEntries() bool {
	f := e.f
	drop := map[*Block]bool{}
	for _, o := range e.objs {
		if !o.escaped {
			for _, b := range o.entries {
				drop[b] = true
			}
		}
		o.entries = o.entries[:0]
	}
	if len(drop) == 0 {
		return false
	}
	entries := f.Entries[:0]
	for _, en := range f.Entries {
		if !drop[en.Block] {
			entries = append(entries, en)
		}
	}
	f.Entries = entries
	for b := range drop {
		for _, s := range b.Succs {
			j := -1
			for k, p := range s.Preds {
				if p == b {
					j = k
				}
			}
			if j < 0 {
				continue
			}
			s.Preds = append(s.Preds[:j:j], s.Preds[j+1:]...)
			s.Backedge = append(s.Backedge[:j:j], s.Backedge[j+1:]...)
			for _, v := range s.Values {
				if v.Op == OpPhi {
					v.Args = append(v.Args[:j:j], v.Args[j+1:]...)
				}
			}
		}
	}
	reorder(f)
	// The phis merging an object with the entries' slots are the object.
	subst := map[*Value]*Value{}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpPhi {
				continue
			}
			if r, o := e.roleOf(v); r == roleObj && !o.escaped {
				subst[v] = o.obj
			}
		}
	}
	if len(subst) != 0 {
		rewrite(f, func(v *Value) *Value {
			if w, ok := subst[v]; ok {
				return w
			}
			return v
		})
	}
	return true
}

// reorder puts the blocks the entries reach in reverse post-order from
// them again, as the builder ordered them (translate), and numbers them so:
// with an entry gone, a block it had been first met from may be met later
// than a block it dominates now.
func reorder(f *Func) {
	n := len(f.Blocks)
	seen := make([]bool, n)
	order := make([]*Block, 0, n)
	var visit func(*Block)
	visit = func(b *Block) {
		if seen[b.ID] {
			return
		}
		seen[b.ID] = true
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
		preds, backedge := b.Preds[:0], b.Backedge[:0]
		for i, p := range b.Preds {
			if seen[p.ID] {
				preds = append(preds, p)
				backedge = append(backedge, b.Backedge[i])
			} else {
				for _, v := range b.Values {
					if v.Op == OpPhi {
						v.Args = append(v.Args[:len(preds):len(preds)], v.Args[len(preds)+1:]...)
					}
				}
			}
		}
		b.Preds, b.Backedge = preds, backedge
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
// where they differ -- and none at a loop's header: an object is not left
// unmade across a loop. An operation it cannot know, a shape it cannot
// follow, has the object escape (again).
func (e *escaper) flow() {
	f := e.f
	e.plans = map[*Value]escPlan{}
	e.uses = e.uses[:0]
	dom := dominators(f)
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
			if o.escaped || b == o.call.Block || !dom.dominates(o.call.Block, b) {
				continue
			}
			cur[k] = e.merge(o, b)
		}
		for _, v := range b.Values {
			if v.State != nil {
				e.record(v, nil, v.State, cur)
			}
			r, o := e.roleOf(v)
			if r == roleCall && !o.escaped {
				cur[o.id] = &vstate{shape: o.shape}
				continue
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
					cur[o.id] = &vstate{shape: st.shape, fields: fields, keys: st.keys}
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
				cur[o.id] = &vstate{shape: add.Next, fields: fields, keys: keys}
				o.fields = max(o.fields, len(fields))
				e.plans[v] = p
			}
		}
		if b.State != nil {
			e.record(nil, b, b.State, cur)
		}
		for k, o := range e.objs {
			if !o.escaped {
				o.out[b.ID] = cur[k]
			}
		}
	}
}

// merge is o's state at b's start: its forward predecessors', which have
// the same shape, the properties that differ merged by phis; nil at a
// loop's header, or where a predecessor's is not known.
func (e *escaper) merge(o *virtual, b *Block) *vstate {
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
		case ps.shape != in.shape || len(ps.fields) != len(in.fields):
			e.escape(o)
			return nil
		default:
			same = false
		}
	}
	if same {
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
	return &vstate{shape: in.shape, fields: fields, keys: in.keys}
}

// phi is the phi merging o's property k at b, made the first time it is
// asked for: what o's predecessors' states have there.
func (e *escaper) phi(o *virtual, b *Block, k int) *Value {
	key := [3]int{b.ID, o.id, k}
	if e.phis == nil {
		e.phis = map[[3]int]*Value{}
	}
	if p := e.phis[key]; p != nil {
		return p
	}
	f := e.f
	args := make([]*Value, len(b.Preds))
	for j, pred := range b.Preds {
		args[j] = o.out[pred.ID].fields[k]
		if r, w := e.roleOf(args[j]); r == roleObj && !w.escaped {
			// An object not made, merged with another value: made.
			e.escape(w)
		}
	}
	p := f.slot()
	p.Op, p.Type, p.Args, p.Block = OpPhi, Tagged, f.refsOf(len(args)), b
	copy(p.Args, args)
	for _, a := range args {
		a.Uses++
	}
	b.Values = append([]*Value{p}, b.Values...)
	e.phis[key] = p
	return p
}

// record notes state s's use at a point -- value v's, or block b's exit --
// where it names objects not made: their states there, and those of the
// objects their properties hold, which Go makes with them. An object whose
// state is not known there escapes.
func (e *escaper) record(v *Value, b *Block, s *FrameState, cur []*vstate) {
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
		for _, x := range st.fields {
			if r, w := e.roleOf(x); r == roleObj && !w.escaped {
				add(w)
			}
		}
	}
	for _, x := range s.Slots {
		if r, o := e.roleOf(x); r == roleObj && !o.escaped {
			add(o)
		}
	}
	if len(snap) != 0 {
		e.uses = append(e.uses, escState{v: v, b: b, s: s, snap: snap})
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
	c.Op, c.Type, c.Block = OpConst, Tagged, o.call.Block
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
		if u.v != nil {
			u.v.State = s
		} else {
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
	for _, b := range f.Blocks {
		values := make([]*Value, 0, len(b.Values)+4)
		for _, v := range b.Values {
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
		b.Values = values
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
	// In the order they were made: values, and so code, must not vary
	// from one compile to the next.
	at := len(vs)
	for _, c := range o.words {
		vs = append(vs, c)
	}
	slices.SortFunc(vs[at:], func(x, y *Value) int { return x.ID - y.ID })
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
// words, their homes holding their shapes, their prototypes and their
// properties, which name the objects not made among them so too.
func (e *escaper) rewriteState(s *FrameState, snap []escSnap) *FrameState {
	f := e.f
	end := s.Own()
	for _, x := range snap {
		end = max(end, x.o.home+abi.VirtualFields+x.o.fields)
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
		n.Slots[o.home] = e.word(o, uint64(y.st.shape))
		n.Slots[o.home+1] = e.word(o, uint64(o.proto))
		for k, x := range y.st.fields {
			n.Slots[o.home+abi.VirtualFields+k] = named(x)
		}
	}
	return n
}
