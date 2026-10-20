package ssa

import (
	"fmt"
)

// Native code holds a reference's number word, and reads its pointer word
// where it came from when it needs it (origin.go): a reference read from a
// cell needs the cell to keep it. A property store over that cell, while
// the reference is still used, would change it, and used to leave the
// store to Go (storeChecks): reading a field into a variable, then writing
// it -- p = this.queue; this.queue = p.link -- left native code each time.
// V8's code keeps such a value in a register, which its collector finds;
// Go's cannot. So the value is kept instead, before the store, in a cell of
// the context's that the collector scans and no store writes
// (abi.Context.Keep, OpKeep), and the code reads it as a copy whose shadow
// is that cell (OpKept): every use the store reaches, rebuilt as SSA. While
// the collector marks, native code writes the keep with Go's write barrier
// (mir's valueStore), as it makes the stores themselves.

// keepAcrossStores keeps, before each property store, the values read from
// cells that are used after it (liveAcross), up to the build's limit
// (keepLimit) in a function, and reports whether it kept any; their phis' shadows are then
// to be made again (shadowMerges). If it kept none, it returns what is live
// across the stores, as storeChecks would find it: the function is as it
// was, but for calls it left to Go, which are stores no longer and change
// nothing live across the others.
func keepAcrossStores(f *Func) (bool, []*Value, [][]*Value) {
	stores, live := liveAcross(f)
	a := newAliases(f)
	// kept are, for each value kept (order, at, by value ID, 1 plus its
	// index there), its copies.
	n := f.nextID
	at := f.ints(n)
	var order []*Value
	var kept [][]*Value
	// cell is, by value ID, 1 plus the keep cell each value kept has, one
	// for all the stores that keep it: its pointer word, like its number
	// word, is the same wherever it is read -- a value carried round a loop
	// is a phi, a value of its own -- so a store keeping it again writes
	// what the cell holds. added marks, by value ID, the values a store
	// keeps, as 1 plus its index.
	cell, added := f.ints(n), f.ints(n)
	for i, s := range stores {
		// A store that may write a cell a value used after it was read from
		// (aliases) keeps that value, and every other used after it that has
		// a shadow, and the value it stores, which it reads after the keeps:
		// every pointer word is read before any keep cell is written, since
		// one may be read from a cell another writes -- a value kept at this
		// store the last time round a loop, which this pass, making the
		// keeps, cannot yet tell. So a store keeps all of them or none.
		// A call is a store of any key: what its callee may write. It reads
		// its operands' pointer words before the callee runs, as a store
		// reads the value it stores.
		key, read := anyKey, s.Args
		switch s.Op {
		case OpPropWrite:
			key, read = s.Key, s.Args[1:2]
		case OpElemStore:
			read = s.Args[2:3]
		case OpElemWrite, OpBigBox:
			// A number's, a BigInt's: no pointer word to read.
			read = nil
		}
		alias := false
		for _, c := range live[i] {
			if s.Op == OpBigBox || allocOnly(s) {
				// A pool's object made, a BigInt boxed: it writes its result's
				// cell alone.
				alias = alias || c.Op != OpKept && a.mayOwnCell(s, c.Shadow)
				continue
			}
			if popsElement(s) {
				// An element popped: its cell, and the result's.
				alias = alias || c.Op != OpKept && a.mayElemCell(s, c.Shadow)
				continue
			}
			if s.Op == OpCall {
				alias = alias || c.Op != OpKept && a.mayCall(s, c.Shadow)
				continue
			}
			if s.Op == OpElemStore || s.Op == OpElemWrite {
				alias = alias || c.Op != OpKept && a.mayElem(c.Shadow)
				continue
			}
			alias = alias || c.Op != OpKept && a.may(key, c.Shadow)
		}
		if !alias {
			continue
		}
		cands := f.scr().vals[:0]
		for _, list := range [2][]*Value{live[i], read} {
			for _, c := range list {
				if c.Type == Tagged && c.Shadow != nil && c.Op != OpKept && added[c.ID] != i+1 {
					added[c.ID] = i + 1
					cands = append(cands, c)
				}
			}
		}
		f.scr().vals = cands[:0]
		fresh := 0
		for _, c := range cands {
			if cell[c.ID] == 0 {
				fresh++
			}
		}
		if f.Keeps+fresh > f.keepLimit() {
			if s.Op == OpBigBox {
				// Its cell written over a value read from it before, which
				// no cell is left to keep.
				f.Unsafe = "keep cells run out at a BigInt's box"
			}
			if s.Op == OpCall {
				// Not kept, the references could be lost: Go makes the call.
				if s.IC != nil {
					s.IC.Calls = nil
				}
			}
			continue
		}
		if len(cands) == 0 {
			continue
		}
		// The refs, then the keeps, then the copies, before the store.
		b := s.Block
		where := indexOf(b.Values, s)
		m := len(cands)
		values := f.refsOf(len(b.Values) + 3*m)
		copy(values, b.Values[:where])
		copy(values[where+3*m:], b.Values[where:])
		ins := values[where : where+3*m]
		for j, c := range cands {
			r := f.alloc(Value{Op: OpKeepRef, Type: Ptr, Args: f.refsOf(1), Block: b})
			r.Args[0] = c
			index := cell[c.ID] - 1
			if index < 0 {
				index = f.Keeps
				cell[c.ID] = index + 1
				f.Keeps++
			}
			k := f.alloc(Value{Op: OpKeep, Type: Source, Args: f.refsOf(2), Block: b, Index: index})
			k.Args[0], k.Args[1] = c, r
			v := f.alloc(Value{Op: OpKept, Type: Tagged, Args: f.refsOf(2), Block: b})
			v.Args[0], v.Args[1], v.Shadow = k, c, k
			ins[j], ins[m+j], ins[2*m+j] = r, k, v
			if at[c.ID] == 0 {
				order = append(order, c)
				kept = append(kept, nil)
				at[c.ID] = len(order)
			}
			kept[at[c.ID]-1] = f.appendValue(kept[at[c.ID]-1], v)
		}
		b.Values = values
	}
	if len(order) == 0 {
		return false, stores, live
	}
	reconstruct(f, order, kept)
	return true, nil, nil
}

func indexOf(vs []*Value, v *Value) int {
	for i, w := range vs {
		if w == v {
			return i
		}
	}
	panic("ssa: a value is not in its block")
}

// reconstruct makes each use of each kept value c take the definition that
// reaches it, c's own or one of its copies (kept[c], each defined where it
// is), with phis where they meet, as an SSA builder reads a variable
// (Braun et al.): a use takes the last definition before it in its block,
// or else what reaches the block's start -- from its one predecessor's
// end, or from a phi of every predecessor's, dropped when its arguments
// are one value. One walk of the function serves every value: a compile
// at run time pays for each.
func reconstruct(f *Func, order []*Value, kept [][]*Value) {
	nb := len(f.Blocks)
	// pos is each value's index in its block, phis made here coming before
	// every one; cand, by value ID, is 1 plus the kept value's index.
	pos, cand := f.ints(f.nextID), f.ints(f.nextID)
	for _, b := range f.Blocks {
		for i, v := range b.Values {
			pos[v.ID] = i
		}
	}
	type def struct {
		block, at int
		v         *Value
	}
	defs := make([][]def, len(order))
	for k, c := range order {
		cand[c.ID] = k + 1
		defs[k] = make([]def, 0, 1+len(kept[k]))
		defs[k] = append(defs[k], def{c.Block.ID, pos[c.ID], c})
		for _, d := range kept[k] {
			defs[k] = append(defs[k], def{d.Block.ID, pos[d.ID], d})
		}
	}
	// candOf is 1 plus v's index among the kept values, or 0; phis made here
	// are past the table.
	candOf := func(v *Value) int {
		if v == nil || v.ID >= len(cand) {
			return 0
		}
		return cand[v.ID]
	}
	// The last definition of the k'th value in a block before index at.
	before := func(k int, b *Block, at int) *Value {
		var last *Value
		best := -2
		for _, d := range defs[k] {
			if d.block == b.ID && d.at < at && d.at > best {
				last, best = d.v, d.at
			}
		}
		return last
	}
	// entry is the definition of the k'th value that reaches block b's
	// start, by k*nb+b, for the blocks a use has walked back to: a table
	// of every value by every block had been most of what a compile of a
	// large function allocated, a few hundred kept values by a few
	// thousand blocks, almost none of whose entries were ever met.
	sc := f.scr()
	if sc.reaching == nil {
		sc.reaching = map[int]*Value{}
	}
	entry := sc.reaching
	defer clear(entry)
	// alias is the value each phi made here and found to merge one is, by
	// its ID past the function's (base): nil until one is.
	base := f.nextID
	var alias []*Value
	resolve := func(v *Value) *Value {
		for v.ID >= base && v.ID-base < len(alias) && alias[v.ID-base] != nil {
			v = alias[v.ID-base]
		}
		return v
	}
	var phis [][]*Value
	var atStart func(k int, b *Block) *Value
	atEnd := func(k int, b *Block) *Value {
		if d := before(k, b, len(b.Values)); d != nil {
			return d
		}
		return atStart(k, b)
	}
	atStart = func(k int, b *Block) *Value {
		if v, ok := entry[k*nb+b.ID]; ok {
			return resolve(v)
		}
		switch len(b.Preds) {
		case 0:
			panic(fmt.Sprintf("ssa: no definition of %v reaches b%d", order[k], b.ID))
		case 1:
			v := atEnd(k, b.Preds[0])
			entry[k*nb+b.ID] = v
			return v
		}
		phi := f.alloc(Value{Op: OpPhi, Type: Tagged, Args: f.refsOf(len(b.Preds)), Block: b})
		entry[k*nb+b.ID] = phi
		var same *Value
		trivial := true
		for i, p := range b.Preds {
			a := resolve(atEnd(k, p))
			phi.Args[i] = a
			if a == phi {
				continue
			}
			if same != nil && same != a {
				trivial = false
			}
			same = a
		}
		if trivial && same != nil {
			for len(alias) <= phi.ID-base {
				alias = append(alias, nil)
			}
			alias[phi.ID-base] = same
			entry[k*nb+b.ID] = same
			return same
		}
		if phis == nil {
			phis = make([][]*Value, nb)
		}
		phis[b.ID] = append(phis[b.ID], phi)
		defs[k] = append(defs[k], def{b.ID, -1, phi})
		return phi
	}
	reach := func(k int, b *Block, at int) *Value {
		if d := before(k, b, at); d != nil {
			return d
		}
		return atStart(k, b)
	}
	// A frame state its users share -- the guards of one instruction do,
	// some before a keep and some after -- is copied before one's slot
	// changes.
	// users counts, by state ID, the values and blocks that use each.
	users := f.ints(f.nextState)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.State != nil {
				users[v.State.id]++
			}
		}
		if b.State != nil {
			users[b.State.id]++
		}
	}
	state := func(s *FrameState, b *Block, at int) *FrameState {
		if s == nil {
			return nil
		}
		out := s
		copied := false
		for i, x := range s.Slots {
			if candOf(x) == 0 {
				continue
			}
			r := reach(candOf(x)-1, b, at)
			if r == x {
				continue
			}
			if !copied && s.id < len(users) && users[s.id] > 1 {
				users[s.id]--
				out = f.newState(*s)
				out.Slots = f.refsOf(len(s.Slots))
				copy(out.Slots, s.Slots)
			}
			copied = true
			out.Slots[i] = r
		}
		return out
	}
	for _, b := range f.Blocks {
		lead := 0
		for _, v := range b.Values {
			if v.Op != OpPhi {
				break
			}
			lead++
		}
		for _, v := range b.Values {
			at := pos[v.ID]
			if v.Op == OpPhi {
				for i, a := range v.Args {
					if k := candOf(a); k != 0 {
						v.Args[i] = atEnd(k-1, b.Preds[i])
					}
				}
				continue
			}
			for i, a := range v.Args {
				if k := candOf(a); k != 0 {
					v.Args[i] = reach(k-1, b, at)
				}
			}
			v.State = state(v.State, b, at)
		}
		if k := candOf(b.Control); k != 0 {
			b.Control = reach(k-1, b, len(b.Values))
		}
		b.State = state(b.State, b, len(b.Values))
		if b.Header != nil && b.PC >= 0 {
			// A loop header's state is after its phis.
			for i, x := range b.Header.Slots {
				if k := candOf(x); k != 0 {
					b.Header.Slots[i] = reach(k-1, b, lead)
				}
			}
		}
	}
	for id, ps := range phis {
		if len(ps) != 0 {
			b := f.Blocks[id]
			values := f.refsOf(len(ps) + len(b.Values))
			copy(values, ps)
			copy(values[len(ps):], b.Values)
			b.Values = values
		}
	}
	if len(alias) == 0 {
		return
	}
	// Phis made, then found to merge one value, are that value.
	slots := func(s *FrameState) {
		if s != nil {
			for i, x := range s.Slots {
				if x != nil {
					s.Slots[i] = resolve(x)
				}
			}
		}
	}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			for i, a := range v.Args {
				v.Args[i] = resolve(a)
			}
			slots(v.State)
		}
		if b.Control != nil {
			b.Control = resolve(b.Control)
		}
		slots(b.State)
		slots(b.Header)
	}
}
