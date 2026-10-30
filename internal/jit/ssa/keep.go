package ssa

import (
	"fmt"
	"slices"
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
	dropRekeeps(f)
	return true, nil, nil
}

// WritesKeepCell reports whether v writes the context's keep cell v.Index
// (abi.Context.Keep): a keep (OpKeep), a native call's result
// (OpCall), a BigInt's box (OpBigBox). mir gives the cells out by these
// writes (core's colorKeeps), and dropRekeeps drops a keep only where they
// show its cell holds what it would write.
func (v *Value) WritesKeepCell() bool {
	return v.Op == OpCall && len(v.Calls()) != 0 || v.Op == OpKeep || v.Op == OpBigBox
}

// dropRekeeps takes out the keeps that copy into a cell the word it holds
// already. keepAcrossStores gives a value one cell for every store that
// keeps it, and reconstruct then has a later store's keep of it take the
// copy an earlier store's keep made, which reads its pointer word from that
// same cell: the later keep writes the cell the word it holds. Such a keep
// is dropped, and its copy is the earlier one -- whose cell the collector
// scans with the context's, so nothing it must see changes, whether or not
// it marks: Go's write barrier is for a pointer slot that changes, and this
// one does not. They had been a fifth to two fifths of the keeps a compile
// made (Box2D 19%, a React renderer 28%, Typescript 35%, PdfJS 41%), each
// some hundred bytes of code before a store. A keep is dropped only if the
// copy it takes is an earlier keep's of its cell, that keep dominates it,
// and every write of the cell is a keep of one value, its copies followed
// back to it (keptRoot): otherwise what the cell holds when the keep runs
// is not known to be what it writes. The quickjs_verify build panics where
// a keep of a cell's own copy fails the last two.
func dropRekeeps(f *Func) {
	n := f.Keeps
	if n == 0 {
		return
	}
	// owner is, by cell, the value every keep of the cell keeps; mixed
	// marks a cell another kind of write writes, or keeps of two values.
	owner, mixed := f.refsOf(n), f.bools(n)
	roots := f.refsOf(f.nextID)
	found := false
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if !v.WritesKeepCell() || v.Index < 0 || v.Index >= n {
				continue
			}
			if v.Op != OpKeep {
				mixed[v.Index] = true
				continue
			}
			switch r := keptRoot(v.Args[0], roots); {
			case owner[v.Index] == nil:
				owner[v.Index] = r
			case owner[v.Index] != r:
				mixed[v.Index] = true
			}
			found = found || isRekeep(v)
		}
	}
	if !found {
		return
	}
	dom := dominators(f)
	subst := f.refsOf(f.nextID)
	dropped := false
	for _, b := range f.Blocks {
		for at, k := range b.Values {
			if k.Op != OpKeep || !isRekeep(k) {
				continue
			}
			a := k.Args[0]
			first := a.Args[0]
			ok := !mixed[k.Index] && dom.dominates(a.Block, b) && (a.Block != b || slices.Contains(b.Values[:at], a))
			if !ok {
				if verifyLiveness {
					panic(fmt.Sprintf("ssa: %v keeps %v into cell %d, which %v may not hold (mixed %v)", k, a, k.Index, first, mixed[k.Index]))
				}
				continue
			}
			subst[k.ID] = first
			dropped = true
		}
	}
	if !dropped {
		return
	}
	// Each dropped keep's copy is the copy it took.
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op == OpKept && subst[v.Args[0].ID] != nil {
				subst[v.ID] = v.Args[0].Args[0]
			}
		}
	}
	rewrite(f, func(v *Value) *Value {
		for v.ID < len(subst) && subst[v.ID] != nil {
			v = subst[v.ID]
		}
		return v
	})
}

// isRekeep reports whether k, a keep, takes a copy an earlier keep of its
// own cell made (OpKept of an OpKeep with k's Index).
func isRekeep(k *Value) bool {
	a := k.Args[0]
	return a.Op == OpKept && a.Args[0].Op == OpKeep && a.Args[0].Index == k.Index
}

// keptRoot is the value x is a copy of, through keeps' copies (OpKept) and
// the phis reconstruct made of one value's copies, or x: roots holds, by
// value ID, each phi's once found, or the phi itself while it is looked at
// -- a phi of a loop's copies reaches itself.
func keptRoot(x *Value, roots []*Value) *Value {
	for x.Op == OpKept {
		x = x.Args[1]
	}
	if x.Op != OpPhi || x.ID >= len(roots) {
		return x
	}
	if r := roots[x.ID]; r != nil {
		return r
	}
	roots[x.ID] = x
	var root *Value
	for _, a := range x.Args {
		r := keptRoot(a, roots)
		if r == x {
			continue
		}
		if root != nil && r != root {
			return x
		}
		root = r
	}
	if root == nil {
		root = x
	}
	roots[x.ID] = root
	return root
}

// fuseKeepRefs has each keep read the pointer word it copies itself, where
// that reads what its OpKeepRef would have: the keep takes its value alone
// (OpKeep's one operand), and the ref goes. A store's keeps read every
// pointer word before writing any keep cell, since one value's pointer
// word may be read from a cell another's keep writes -- a phi of copies
// kept the last time round a loop, whose shadow is a merge of keep cells.
// Where no value's source is a keep cell another keep of the store writes
// before its own, reading it at its own keep reads the same word: so it
// is for nearly every keep, and the refs had been a tenth of a compile's
// values, each a register live across the keeps before its own.
func fuseKeepRefs(f *Func) {
	if f.Keeps == 0 {
		return
	}
	// writes is, by keep cell, the stamp of the keep whose ref-to-keep
	// stretch writes it, and seen, by value ID, that of the walk that met
	// a phi: stamp numbers the keeps looked at. dropped marks the refs
	// fused, by value ID.
	var writes, seen []int
	var dropped []bool
	var stack []*Value
	stamp := 0
	for _, b := range f.Blocks {
		fused := false
		for at, k := range b.Values {
			if k.Op != OpKeep || len(k.Args) != 2 || k.Args[1].Op != OpKeepRef || k.Args[1].Block != b {
				continue
			}
			r := k.Args[1]
			if writes == nil {
				writes, seen, dropped = f.ints(f.Keeps), f.ints(f.nextID), f.bools(f.nextID)
			}
			// Where r is: the keeps after it and before k write their cells
			// in between.
			from := -1
			for i := at - 1; i >= 0; i-- {
				if b.Values[i] == r {
					from = i
					break
				}
			}
			if from < 0 || r.ID >= len(dropped) {
				continue
			}
			stamp++
			between := false
			for _, w := range b.Values[from+1 : at] {
				if w.Op == OpKeep && w.Index >= 0 && w.Index < len(writes) {
					writes[w.Index], between = stamp, true
				}
			}
			ok := true
			if between {
				// The keep cells k's value's source may be: its shadow, and
				// through phis, theirs.
				stack = append(stack[:0], k.Args[0].Shadow)
				for len(stack) > 0 && ok {
					s := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					switch {
					case s == nil:
					case s.Op == OpKeep:
						ok = s.Index < 0 || s.Index >= len(writes) || writes[s.Index] != stamp
					case s.Op == OpPhi:
						if s.ID < len(seen) {
							if seen[s.ID] == stamp {
								continue
							}
							seen[s.ID] = stamp
						}
						stack = append(stack, s.Args...)
					}
				}
			}
			if ok {
				// The ref is k's alone (keepAcrossStores makes one for each).
				k.Args = k.Args[:1]
				dropped[r.ID], fused = true, true
			}
		}
		if fused {
			// The refs no keep reads any more go.
			kept := b.Values[:0]
			for _, v := range b.Values {
				if v.Op == OpKeepRef && v.ID < len(dropped) && dropped[v.ID] {
					continue
				}
				kept = append(kept, v)
			}
			b.Values = kept
		}
	}
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
	// The definitions of the kept values, c's own and its copies', by
	// block: start[b.ID] to start[b.ID+1] in defs, each with its value's
	// index among the kept and its place in the block. A use asks only its
	// own block's, which hold a few, where a list by value had been walked
	// whole at every use -- every frame state's slot among them.
	type def struct {
		k, at int
		v     *Value
	}
	start := f.ints(nb + 1)
	n := 0
	for k, c := range order {
		cand[c.ID] = k + 1
		start[c.Block.ID+1]++
		for _, d := range kept[k] {
			start[d.Block.ID+1]++
		}
		n += 1 + len(kept[k])
	}
	for b := 0; b < nb; b++ {
		start[b+1] += start[b]
	}
	defs, fill := make([]def, n), f.ints(nb)
	add := func(k int, d *Value) {
		b := d.Block.ID
		defs[start[b]+fill[b]] = def{k, pos[d.ID], d}
		fill[b]++
	}
	for k, c := range order {
		add(k, c)
		for _, d := range kept[k] {
			add(k, d)
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
	// The last definition of the k'th value in a block before index at: a
	// phi made here at its start is atStart's.
	before := func(k int, b *Block, at int) *Value {
		var last *Value
		best := -1
		for _, d := range defs[start[b.ID]:start[b.ID+1]] {
			if d.k == k && d.at < at && d.at > best {
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
		return phi
	}
	// reach is the definition of the k'th value that reaches index at of
	// block b: the walk below asks it of one block at a time, which keeps
	// what reaches the block's start for each value it has asked of
	// (startOf, by 1 plus the block's ID in startIn).
	startIn, startOf := f.ints(len(order)), f.refsOf(len(order))
	reach := func(k int, b *Block, at int) *Value {
		if d := before(k, b, at); d != nil {
			return d
		}
		if startIn[k] != b.ID+1 {
			startIn[k], startOf[k] = b.ID+1, atStart(k, b)
		}
		return resolve(startOf[k])
	}
	// A frame state its users share -- the guards of one instruction do,
	// some before a keep and some after -- is copied before one's slot
	// changes.
	// users counts, by state ID, the values and blocks that use each, and
	// kin is, by state ID, 1 if one of its slots holds a kept value, 2 if
	// none does, 0 until it is looked at: a state its users share is read
	// for that once, not by each of them.
	users, kin := f.ints(f.nextState), f.bools(2*f.nextState)
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
	holdsKept := func(s *FrameState) bool {
		if s.id >= len(users) {
			return true // a copy made here
		}
		if !kin[2*s.id] {
			kin[2*s.id] = true
			for _, x := range s.Slots {
				if candOf(x) != 0 {
					kin[2*s.id+1] = true
					break
				}
			}
		}
		return kin[2*s.id+1]
	}
	state := func(s *FrameState, b *Block, at int) *FrameState {
		if s == nil || !holdsKept(s) {
			return s
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
	// Phis made, then found to merge one value, are that value: each
	// state's slots resolved once, however many use it (done, by state ID
	// for the states there were, or past them).
	done := f.bools(f.nextState)
	slots := func(s *FrameState) {
		if s != nil {
			if s.id < len(done) {
				if done[s.id] {
					return
				}
				done[s.id] = true
			}
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
