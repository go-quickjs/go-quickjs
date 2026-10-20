package ssa

import (
	"fmt"
	"math"
	"math/bits"
	"slices"
)

// Native code holds a slot's number word and never its pointer word, so a
// tagged value it moves to another slot, or returns, is either a primitive
// it can write itself or the reference some slot held when native code was
// entered, which only Go can copy. Native code writes the frame only when
// it exits, so at every exit each slot still holds what it held at entry:
// an exit that finds the reference there has Go copy it. A value's origin
// says which slot to look in.
//
// Most values have one origin a compiler can name. A phi can merge two
// slots' values -- a loop that does x=o, entered at its header with x
// already holding a reference -- and two references can share a number
// word, as two objects do, so no test of the word tells them apart. Such a
// phi has a shadow: a Source phi holding, at run time, where its value came
// from -- the address of the slot's value, which is in the frame, in the
// context for the receiver, or in a captured binding's cell -- or 0 for a
// primitive. An exit passes the shadow to Go, and native code reads the
// pointer word there, the same way wherever it is.
//
// A reference loaded from an object (OpLoadCell) came from a heap cell, not
// a slot: its shadow is the cell's address. The cell holds the reference
// until native code exits, when Go copies it from there: the one store
// native code makes to a heap cell, a property's (OpPropWrite), first
// compares the cell with those of the references live after it
// (storeChecks), and leaves the write to Go on a match. A phi merging such a value has a shadow too, whose
// argument for it is the cell. This holds only while Go's heap does not
// move and nothing changes the object graph while native code runs, which
// is outside what unsafe.Pointer's rules promise: a deliberate choice
// (internal/jit/README.md, "Reading the heap in place"), checked with each
// new Go.

// Origins of a tagged value, besides a slot's index.
const (
	// OriginScalar: a primitive made natively.
	OriginScalar = -1
	// OriginAmbiguous: a merge of two slots' values, which its shadow tells
	// apart at run time.
	OriginAmbiguous = -2
	originNone      = -3 // a phi not yet reached
	// OriginHeap: loaded from a heap cell, which its shadow is.
	OriginHeap = -4
)

// OriginMap is each tagged value's origin (Origins), by value ID.
type OriginMap struct{ of []int }

// originAbsent marks a value with no origin: one not tagged, or not in the
// function when its origins were found.
const originAbsent = math.MinInt

// Of is v's origin, and whether it has one.
func (m OriginMap) Of(v *Value) (int, bool) {
	if v.ID >= len(m.of) || m.of[v.ID] == originAbsent {
		return 0, false
	}
	return m.of[v.ID], true
}

// At is v's origin, or 0 if it has none.
func (m OriginMap) At(v *Value) int {
	o, _ := m.Of(v)
	return o
}

// Origins returns each tagged value's origin: OriginScalar, or the slot
// whose value at entry it may be -- when it is not a primitive -- or
// OriginAmbiguous.
func Origins(f *Func) OriginMap {
	if f.originsKept {
		if verifyLiveness {
			// The shadows made since are values with no origin.
			fresh, kept := origins(f).of, f.origins.of
			if len(fresh) < len(kept) || !slices.Equal(fresh[:len(kept)], kept) ||
				slices.ContainsFunc(fresh[len(kept):], func(o int) bool { return o != originAbsent }) {
				panic("ssa: the origins kept are not the function's")
			}
		}
		return f.origins
	}
	return origins(f)
}

// origins is Origins, computed.
func origins(f *Func) OriginMap {
	origin := f.ints(f.nextID)
	for i := range origin {
		origin[i] = originAbsent
	}
	at := func(v *Value) int {
		if v.ID >= len(origin) || origin[v.ID] == originAbsent {
			return 0
		}
		return origin[v.ID]
	}
	phis := f.scr().originPhis[:0]
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Type != Tagged {
				continue
			}
			switch v.Op {
			case OpPhi:
				origin[v.ID] = originNone
				phis = append(phis, v)
			case OpLoadSlot:
				origin[v.ID] = v.Aux
			case OpLoadCell, OpKept:
				origin[v.ID] = OriginHeap
			default:
				origin[v.ID] = OriginScalar
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, p := range phis {
			o := originNone
			for _, a := range p.Args {
				o = joinOrigin(o, at(a))
			}
			if o != origin[p.ID] {
				origin[p.ID] = o
				changed = true
			}
		}
	}
	for _, p := range phis {
		if origin[p.ID] == originNone {
			origin[p.ID] = OriginScalar
		}
	}
	f.scr().originPhis = phis[:0]
	return OriginMap{origin}
}

func joinOrigin(a, b int) int {
	switch {
	case a == OriginHeap && b != originNone || b == OriginHeap && a != originNone:
		// Two cells, or a cell and anything else: known only at run time.
		return OriginAmbiguous
	case a == originNone || a == b:
		return b
	case b == originNone:
		return a
	case a == OriginScalar:
		return b
	case b == OriginScalar:
		return a
	}
	return OriginAmbiguous
}

// Finish gives the phis their shadows and the stores their checks
// (shadowMerges, storeChecks), which running or compiling f needs, unless
// Optimize has: Build leaves them to Optimize, which would make them again
// for the phis and stores it keeps.
func (f *Func) Finish() {
	if !f.shadowed {
		shadowMerges(f)
		storeChecks(f)
		f.shadowed, f.originsKept = true, true
	}
}

// shadowMerges gives every ambiguous phi a shadow, and every phi that
// flows into one and may hold a reference, so that each shadow's arguments
// are shadows or constants: a load's slot, or -1 for a primitive, which
// native code makes the slot's address, or 0 (OpConstSource). The
// constants are made in the predecessor the argument comes from.
func shadowMerges(f *Func) {
	origin := origins(f)
	f.origins = origin
	need := f.bools(f.nextID)
	any := false
	var walk func(v *Value)
	walk = func(v *Value) {
		if v.Op != OpPhi || need[v.ID] || origin.At(v) == OriginScalar {
			return
		}
		need[v.ID], any = true, true
		for _, a := range v.Args {
			walk(a)
		}
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpPhi && origin.At(v) == OriginAmbiguous {
				walk(v)
			}
		}
	}
	if !any {
		return
	}
	// In block order, so that values are numbered the same every time.
	var shadows []*Value
	fresh := f.bools(f.nextID)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.ID < len(need) && need[v.ID] && v.Shadow == nil {
				s := f.alloc(Value{Op: OpPhi, Type: Source, Args: f.refsOf(len(v.Args)), Block: b})
				v.Shadow, fresh[v.ID] = s, true
				shadows = append(shadows, v)
			}
		}
	}
	for _, v := range shadows {
		for i, a := range v.Args {
			var s *Value
			switch {
			case a.Shadow != nil:
				s = a.Shadow
			case origin.At(a) >= 0:
				s = f.constSource(v.Block.Preds[i], origin.At(a))
			default:
				s = f.constSource(v.Block.Preds[i], -1)
			}
			v.Shadow.Args[i] = s
			s.Uses++
		}
	}
	// Each shadow right after its phi: a block's, all at once.
	for i := 0; i < len(shadows); {
		b, j := shadows[i].Block, i
		for j < len(shadows) && shadows[j].Block == b {
			j++
		}
		vs := f.refsOf(len(b.Values) + j - i)[:0]
		for _, v := range b.Values {
			vs = append(vs, v)
			if v.ID < len(fresh) && fresh[v.ID] {
				vs = append(vs, v.Shadow)
			}
		}
		b.Values = vs
		i = j
	}
}

// constSource makes a source constant at the end of a block.
func (f *Func) constSource(b *Block, k int) *Value {
	v := f.alloc(Value{Op: OpConstSource, Type: Source, Aux: k, Block: b})
	b.Values = f.appendValue(b.Values, v)
	return v
}

// clearShadows drops every phi's shadow, for passes to remake them after
// values have been replaced; the shadows themselves are then dead. A loaded
// value keeps its cell, which is its argument as well.
func clearShadows(f *Func) {
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpPhi {
				v.Shadow = nil
			}
		}
	}
}

// storeChecks gives every property store the cells it must not write: the
// shadows of the tagged values live after it that have one -- references
// loaded from cells, and phis that may hold one. Native code holds such a
// value's number word and reads its pointer word where its shadow says
// when Go needs it; a store to that cell would change the pointer word
// under it. The store compares each with the property's address, and
// leaves the write to Go on a match while the cell holds a pointer word:
// one it does not hold, a primitive's, is not lost -- and a cell a live
// reference was read from cannot have changed since, every store to it
// having left. They are its operands after the object and the value.
func storeChecks(f *Func) {
	stores, live := liveAcross(f)
	storeChecksAt(f, stores, live)
}

// checkStoreChecks panics unless the stores' checks are those storeChecks
// makes afresh (quickjs_verify): Optimize makes them from what
// keepAcrossStores found live, when it kept nothing.
func checkStoreChecks(f *Func) {
	var made [][]*Value
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if storeChecked(v) != 0 {
				made = append(made, slices.Clone(v.Args))
			}
		}
	}
	clearStoreChecks(f)
	storeChecks(f)
	i := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if n := storeChecked(v); n != 0 {
				if !slices.Equal(v.Args, made[i]) {
					panic(fmt.Sprintf("ssa: %v's checks %v, made afresh %v", v, made[i][n:], v.Args[n:]))
				}
				i++
			}
		}
	}
}

// storeChecked is how many operands precede a store's checks: a store to
// a heap cell (OpPropWrite, OpElemStore, OpElemWrite); 0 for any other
// value. An element write of a number (OpElemWrite) natively writes only
// over a number, but what it leaves to Go (GoElemWrite) writes over any
// element and grows the array, moving its elements: with checks, it
// exits instead.
func storeChecked(v *Value) int {
	switch v.Op {
	case OpPropWrite:
		return 2
	case OpElemStore, OpElemWrite:
		return 3
	}
	return 0
}

// storeChecksAt is storeChecks with what is live across the stores.
func storeChecksAt(f *Func, stores []*Value, live [][]*Value) {
	a := newAliases(f)
	// added marks, by value ID, the checks the store being made has, as 1
	// plus its index: each is made once.
	var added []int
	for i, s := range stores {
		n := storeChecked(s)
		if n == 0 || len(live[i]) == 0 {
			continue
		}
		if added == nil {
			added = f.ints(f.nextID)
		}
		for _, c := range s.Args[n:] {
			if c.ID < len(added) {
				added[c.ID] = i + 1
			}
		}
		for _, c := range live[i] {
			sh := c.Shadow
			may := (s.Op == OpElemStore || s.Op == OpElemWrite) && a.mayElem(sh) || s.Op == OpPropWrite && a.may(s.Key, sh)
			if !may || sh.ID < len(added) && added[sh.ID] == i+1 || sh.ID >= len(added) && slices.Contains(s.Args[n:], sh) {
				continue
			}
			s.Args = f.appendValue(s.Args, sh)
			sh.Uses++
			if sh.ID < len(added) {
				added[sh.ID] = i + 1
			}
		}
	}
}

// aliases says which sources a property store may write: a property's cell
// read through the store's key, a global binding's of that name; through a
// phi, any of its arguments' -- and a keep's, while the collector marks,
// its value's own (OpKeep). Elements, frame slots and the context's cells
// are no property's, and a cell read through another key is another
// property's.
//
// What a phi or a keep may be is the cells it reaches through phis and
// keeps, which every query asks of the same way: so each has a summary of
// them (aliasSum), made once, and a query of it is a look at that, not a
// search. The summaries are found as Tarjan's algorithm finds a graph's
// strongly connected components: the phis of a loop reach each other, and
// have one summary, of every cell any of them reaches. A search for each
// query, its answers kept by phi and key, had been most of a compile of a
// function of many stores and loops: Typescript's compiler's took a quarter
// of its time with the JIT, most of it in the maps the answers were kept
// in.
type aliases struct {
	// assigned is the function's (Func's assigned).
	assigned []bool
	// sums are the summaries; of, by value ID, 1 plus the index of a phi's
	// or a keep's, once found, or 0.
	sums []aliasSum
	of   []int32
	// The search's state, by value ID: num is 1 plus the order a value was
	// met in, while it is searched; low the least its search reached; and
	// stack the values met whose components are not yet found.
	num, low []int32
	stack    []*Value
	next     int32
}

// aliasSum is what the cells a phi or a keep reaches through phis and keeps
// are: all, a cell any store may write; any, one a store of anyKey -- a
// call's -- may write, other than a call's result cell; anyCall, a call's
// result cell; elem, an element's cell; keys, the keys of the cells only
// stores of those keys write, sorted; calls, the IDs of the calls whose
// result cells they are, sorted, or every call's if allCalls.
type aliasSum struct {
	all, any, anyCall, elem, allCalls bool
	keys                              []uint32
	calls                             []int32
}

// aliasSetLimit bounds a summary's sets: past it, a summary says any key,
// or any call, which only makes more checks and keeps.
const aliasSetLimit = 64

func newAliases(f *Func) *aliases {
	a := &f.scr().aliases
	for i := range a.sums {
		a.sums[i].keys, a.sums[i].calls = a.sums[i].keys[:0], a.sums[i].calls[:0]
	}
	a.sums = a.sums[:0]
	a.of, a.num, a.low = a.grow(a.of, f.nextID), a.grow(a.num, f.nextID), a.grow(a.low, f.nextID)
	clear(a.stack)
	a.stack, a.next = a.stack[:0], 0
	a.assigned = f.assigned
	return a
}

// grow is s cleared, at least n long.
func (a *aliases) grow(s []int32, n int) []int32 {
	if cap(s) < n {
		return make([]int32, n, n+n/2)
	}
	s = s[:n]
	clear(s)
	return s
}

// at makes the tables long enough for value ID id: values made since the
// summaries began, keeps among them.
func (a *aliases) at(id int) {
	if id < len(a.of) {
		return
	}
	n := max(id+1, 2*len(a.of))
	for _, t := range []*[]int32{&a.of, &a.num, &a.low} {
		*t = append(*t, make([]int32, n-len(*t))...)
	}
}

// sum is the summary of a phi or a keep, s.
func (a *aliases) sum(s *Value) *aliasSum {
	a.at(s.ID)
	if a.of[s.ID] == 0 {
		a.search(s)
	}
	return &a.sums[a.of[s.ID]-1]
}

// edges are the phis and keeps s reaches in one step, and its leaves: a
// phi's arguments, a keep's value's shadow.
func aliasEdges(s *Value) []*Value {
	if s.Op == OpKeep {
		if x := s.Args[0]; x.Shadow != nil {
			return []*Value{x.Shadow}
		}
		return nil
	}
	return s.Args
}

// search finds the summary of s's component, and those of the components
// it reaches (Tarjan).
func (a *aliases) search(s *Value) {
	a.next++
	a.num[s.ID], a.low[s.ID] = a.next, a.next
	a.stack = append(a.stack, s)
	for _, x := range aliasEdges(s) {
		if x.Op != OpPhi && x.Op != OpKeep {
			continue
		}
		a.at(x.ID)
		switch {
		case a.of[x.ID] != 0:
		case a.num[x.ID] == 0:
			a.search(x)
			a.low[s.ID] = min(a.low[s.ID], a.low[x.ID])
		default:
			// On the stack: in s's component.
			a.low[s.ID] = min(a.low[s.ID], a.num[x.ID])
		}
	}
	if a.low[s.ID] != a.num[s.ID] {
		return
	}
	// s is its component's first: the component is s and what is above it
	// on the stack, and its summary is all their leaves' and the summaries
	// of the components they reach.
	at := len(a.stack) - 1
	for a.stack[at] != s {
		at--
	}
	if len(a.sums) < cap(a.sums) {
		// The sets an earlier compile's summary here held, emptied.
		a.sums = a.sums[:len(a.sums)+1]
		old := &a.sums[len(a.sums)-1]
		*old = aliasSum{keys: old.keys[:0], calls: old.calls[:0]}
	} else {
		a.sums = append(a.sums, aliasSum{})
	}
	n := int32(len(a.sums))
	sum := &a.sums[n-1]
	for _, v := range a.stack[at:] {
		a.of[v.ID] = n
	}
	for _, v := range a.stack[at:] {
		for _, x := range aliasEdges(v) {
			if x.Op == OpPhi || x.Op == OpKeep {
				if m := a.of[x.ID]; m != n {
					a.merge(sum, &a.sums[m-1])
				}
				continue
			}
			a.leaf(sum, x)
		}
	}
	slices.Sort(sum.keys)
	sum.keys = slices.Compact(sum.keys)
	slices.Sort(sum.calls)
	sum.calls = slices.Compact(sum.calls)
	a.limit(sum)
	clear(a.stack[at:])
	a.stack = a.stack[:at]
}

// leaf adds to sum a cell, x, that is neither a phi nor a keep: as may,
// mayCall, mayOwnCell and mayElemCell answer of it.
func (a *aliases) leaf(sum *aliasSum, x *Value) {
	switch x.Op {
	case OpPropCell, OpGlobalCell, OpLexCell, OpBindingCell, OpStringProp:
		sum.any = true
		sum.keys = append(sum.keys, x.Key)
	case OpElemCell:
		sum.any, sum.elem = true, true
	case OpStringMethod, OpStringChar, OpCodeChar, OpSubstringChar, OpConstSource, OpConstCell:
	case OpCallCell, OpBigCell:
		// A call's result's cell, or a BigInt's box's: what writes it.
		sum.anyCall = true
		sum.calls = append(sum.calls, int32(x.Args[0].ID))
	case OpUpvalueCell:
		sum.any = true
		sum.keys = append(sum.keys, upvalueKey)
	case OpLocalCell:
		sum.any = sum.any || x.Index < len(a.assigned) && a.assigned[x.Index]
		sum.keys = append(sum.keys, localKey(x.Index))
	default:
		sum.all = true
	}
	if len(sum.keys) > 2*aliasSetLimit || len(sum.calls) > 2*aliasSetLimit {
		a.limit(sum)
	}
}

// merge adds the summary from to sum.
func (a *aliases) merge(sum, from *aliasSum) {
	sum.all, sum.any, sum.anyCall = sum.all || from.all, sum.any || from.any, sum.anyCall || from.anyCall
	sum.elem, sum.allCalls = sum.elem || from.elem, sum.allCalls || from.allCalls
	sum.keys = append(sum.keys, from.keys...)
	sum.calls = append(sum.calls, from.calls...)
	if len(sum.keys) > 2*aliasSetLimit || len(sum.calls) > 2*aliasSetLimit {
		a.limit(sum)
	}
}

// limit keeps sum's sets within aliasSetLimit, or says any key or call.
func (a *aliases) limit(sum *aliasSum) {
	if len(sum.keys) > aliasSetLimit {
		slices.Sort(sum.keys)
		if sum.keys = slices.Compact(sum.keys); len(sum.keys) > aliasSetLimit {
			sum.all, sum.keys = true, sum.keys[:0]
		}
	}
	if len(sum.calls) > aliasSetLimit {
		slices.Sort(sum.calls)
		if sum.calls = slices.Compact(sum.calls); len(sum.calls) > aliasSetLimit {
			sum.allCalls, sum.calls = true, sum.calls[:0]
		}
	}
}

// hasCall reports whether sum's cells may be call's result cell.
func (sum *aliasSum) hasCall(call *Value) bool {
	if sum.allCalls {
		return sum.anyCall
	}
	_, ok := slices.BinarySearch(sum.calls, int32(call.ID))
	return ok
}

// anyKey is a call's key: a callee may write any property.
const anyKey = ^uint32(0)

// upvalueKey is the key of a store to a captured binding (OpPropWrite's
// Upvalue).
const upvalueKey = ^uint32(1)

// localKey is the key of a store to captured local slot, in its cell
// (OpLocalCell): its own, as no other store writes the frame's slot -- not
// a captured binding's, which is another frame's, or a box -- and a call
// may. One key for every local made each store to one check every value
// loaded from any of them: a function of sixty captured locals compiled to
// a hundred and forty kilobytes.
func localKey(slot int) uint32 { return ^uint32(2) - uint32(slot) }

// may reports whether a store through key may write the cell at s.
func (a *aliases) may(key uint32, s *Value) bool {
	switch s.Op {
	case OpPropCell, OpGlobalCell, OpLexCell, OpBindingCell, OpStringProp:
		return key == anyKey || s.Key == key
	case OpElemCell:
		// No property store writes an element; a call's callee may, or
		// pop it.
		return key == anyKey
	case OpStringMethod, OpStringChar, OpCodeChar, OpSubstringChar, OpConstSource, OpConstCell:
		return false
	case OpCallCell:
		// A call's result's keep cell, which no store writes: a call may
		// (mayCall).
		return key == anyKey
	case OpBigCell:
		// A BigInt's box's keep cell, which only it writes (mayOwnCell).
		return false
	case OpUpvalueCell:
		// A call may assign it, or an assignment to a captured binding,
		// any: two closures may capture one binding.
		return key == anyKey || key == upvalueKey
	case OpLocalCell:
		// A call writes one a closure assigns.
		return key == anyKey && s.Index < len(a.assigned) && a.assigned[s.Index] || key == localKey(s.Index)
	case OpKeep, OpPhi:
		sum := a.sum(s)
		if sum.all {
			return true
		}
		if key == anyKey {
			return sum.any || sum.anyCall
		}
		_, ok := slices.BinarySearch(sum.keys, key)
		return ok
	}
	return true
}

// mayCall reports whether call, a call of any key, may write the cell at s:
// as may, but a call's result's keep cell is written by that call alone,
// none other -- each call has its own -- so that a value another call
// returned is kept across a call no more than one a property gave.
func (a *aliases) mayCall(call, s *Value) bool {
	switch s.Op {
	case OpCallCell:
		return s.Args[0] == call
	case OpKeep, OpPhi:
		sum := a.sum(s)
		return sum.all || sum.any || sum.hasCall(call)
	}
	return a.may(anyKey, s)
}

// mayOwnCell reports whether the cell at s may be the result cell of call,
// one that makes nothing but a pool's object (allocOnly): the one cell such
// a call writes, each time it is made, over the last result.
func (a *aliases) mayOwnCell(call, s *Value) bool {
	switch s.Op {
	case OpCallCell, OpBigCell:
		return s.Args[0] == call
	case OpKeep, OpPhi:
		return a.sum(s).hasCall(call)
	}
	return false
}

// mayElem reports whether a store to an element (OpElemStore) may write
// the cell at s: an element's, which any element store may be, whatever
// its array -- none but an element's, as a property store writes none.
func (a *aliases) mayElem(s *Value) bool {
	switch s.Op {
	case OpElemCell:
		return true
	case OpPropCell, OpGlobalCell, OpLexCell, OpBindingCell, OpStringProp, OpStringMethod, OpStringChar, OpCodeChar, OpSubstringChar, OpConstSource, OpConstCell,
		OpCallCell, OpBigCell, OpUpvalueCell, OpLocalCell:
		return false
	case OpKeep, OpPhi:
		sum := a.sum(s)
		return sum.all || sum.elem
	}
	return true
}

// mayElemCell reports whether the cell at s may be an element's, or the
// result cell of call, which pops an element (popsElement).
func (a *aliases) mayElemCell(call, s *Value) bool {
	switch s.Op {
	case OpElemCell:
		return true
	case OpCallCell:
		return s.Args[0] == call
	case OpKeep, OpPhi:
		sum := a.sum(s)
		return sum.elem || sum.hasCall(call)
	}
	return false
}

// liveAcross is every property store, and for each the values with a
// shadow -- read from cells, and phis that may hold such -- used after it.
// Liveness is by bitsets over those values: each block's, live at its
// start and at its end, to a fixed point, then a walk back through each
// block with a store, from its end, past each value: its own value dies,
// what it uses lives.
func liveAcross(f *Func) ([]*Value, [][]*Value) {
	stores, live := liveAcrossSets(f)
	if verifyLiveness {
		refStores, refLive := liveAcrossRef(f)
		if !slices.Equal(stores, refStores) {
			panic("ssa: liveAcross's stores differ from the reference's")
		}
		for i := range live {
			if !slices.Equal(live[i], refLive[i]) {
				panic(fmt.Sprintf("ssa: live across %v: %v, the reference %v", stores[i], live[i], refLive[i]))
			}
		}
	}
	return stores, live
}

// liveAcrossSets is liveAcross. What it returns is the Func's scratch's,
// until it runs again.
func liveAcrossSets(f *Func) ([]*Value, [][]*Value) {
	sc := f.scr()
	stores, cands := sc.liveStores[:0], sc.liveCands[:0]
	defer func() { sc.liveStores, sc.liveCands = stores[:0], cands[:0] }()
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			switch {
			case v.Op == OpPropWrite, v.Op == OpElemStore, v.Op == OpElemWrite, v.Op == OpCall && v.Calls() != nil, v.Op == OpBigBox:
				stores = append(stores, v)
			case v.Type == Tagged && v.Shadow != nil:
				// A kept value's too: while the collector marks, it is still
				// read from its cell (OpKeep).
				cands = append(cands, v)
			}
		}
	}
	live := sc.live[:0]
	for range stores {
		live = append(live, nil)
	}
	sc.live = live
	if len(stores) == 0 || len(cands) == 0 {
		return stores, live
	}
	nb := 0
	for _, b := range f.Blocks {
		nb = max(nb, b.ID+1)
	}
	cand := f.ints(f.nextID)
	for k, c := range cands {
		cand[c.ID] = k + 1
	}
	w := (len(cands) + 63) / 64
	// By block ID: what it uses (before defining it, as SSA has it), what
	// it defines, what its successors' phis take from it, and what is live
	// at its start and at its end.
	sets := idTable(sc.liveWords, 5*nb*w+w)
	sc.liveWords = sets
	sets, cur := sets[:5*nb*w], sets[5*nb*w:]
	use, def, phiOut, in, out := sets[:nb*w], sets[nb*w:2*nb*w], sets[2*nb*w:3*nb*w], sets[3*nb*w:4*nb*w], sets[4*nb*w:]
	row := func(s []uint64, b *Block) []uint64 { return s[b.ID*w : (b.ID+1)*w] }
	add := func(s []uint64, v *Value) {
		if v != nil && v.ID < len(cand) && cand[v.ID] != 0 {
			k := cand[v.ID] - 1
			s[k/64] |= 1 << (k % 64)
		}
	}
	states := func(s []uint64, st *FrameState) {
		if st != nil {
			for _, x := range st.Slots {
				add(s, x)
			}
		}
	}
	// uses adds what v uses, a phi's arguments aside.
	uses := func(s []uint64, v *Value) {
		for _, a := range v.Args {
			add(s, a)
		}
		states(s, v.State)
	}
	for _, b := range f.Blocks {
		u, d := row(use, b), row(def, b)
		if b.Header != nil {
			states(u, b.Header)
		}
		var last *FrameState
		for _, v := range b.Values {
			add(d, v)
			if v.Op == OpPhi {
				// A phi's argument is used at the end of its predecessor.
				for j, a := range v.Args {
					add(row(phiOut, b.Preds[j]), a)
				}
				continue
			}
			for _, a := range v.Args {
				add(u, a)
			}
			// The guards of an instruction share its state, added once.
			if v.State != last {
				states(u, v.State)
				last = v.State
			}
		}
		add(u, b.Control)
		states(u, b.State)
	}
	// The blocks are in reverse post-order: backward, a loop takes a
	// second round or so.
	for changed := true; changed; {
		changed = false
		for i := len(f.Blocks) - 1; i >= 0; i-- {
			b := f.Blocks[i]
			o, n, u, d, p := row(out, b), row(in, b), row(use, b), row(def, b), row(phiOut, b)
			for j := range o {
				x := p[j]
				for _, s := range b.Succs {
					x |= in[s.ID*w+j]
				}
				o[j] = x
				if y := (u[j] | x) &^ d[j]; y != n[j] {
					n[j], changed = y, true
				}
			}
		}
	}
	// Back through each block with a store: what is live after each value,
	// each store's in one list, from liveFrom[i] on.
	flat, from := sc.liveFlat[:0], sc.liveFrom[:0]
	for range stores {
		from = append(from, 0)
	}
	k := 0
	for _, b := range f.Blocks {
		first := k
		for k < len(stores) && stores[k].Block == b {
			k++
		}
		if first == k {
			continue
		}
		copy(cur, row(out, b))
		add(cur, b.Control)
		states(cur, b.State)
		next := k - 1
		for i := len(b.Values) - 1; i >= 0 && next >= first; i-- {
			v := b.Values[i]
			if v == stores[next] {
				from[next] = len(flat)
				for j, x := range cur {
					for x != 0 {
						bit := bits.TrailingZeros64(x)
						x &= x - 1
						flat = append(flat, cands[j*64+bit])
					}
				}
				live[next] = flat[from[next]:len(flat):len(flat)]
				next--
			}
			if v.ID < len(cand) && cand[v.ID] != 0 {
				c := cand[v.ID] - 1
				cur[c/64] &^= 1 << (c % 64)
			}
			if v.Op != OpPhi {
				uses(cur, v)
			}
		}
	}
	sc.liveFlat, sc.liveFrom = flat[:0], from[:0]
	return stores, live
}

// clearStoreChecks drops the stores' checks, for storeChecks to remake.
func clearStoreChecks(f *Func) {
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if n := storeChecked(v); n != 0 {
				for _, a := range v.Args[n:] {
					a.Uses--
				}
				v.Args = v.Args[:n]
			}
		}
	}
}
