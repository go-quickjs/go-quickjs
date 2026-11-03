package ssa

// Native code holds a reference's number word, and reads its pointer word
// where it came from when it needs it (origin.go): a reference read from a
// cell needs the cell to keep it. A property store over that cell, while
// the reference is still used, would change it, and used to leave the
// store to Go (storeChecks): reading a field into a variable, then writing
// it -- p = this.queue; this.queue = p.link -- left native code each time.
// V8's code keeps such a value in a register, which its collector finds;
// Go's cannot. So the value is kept instead, in a cell of the context's
// that the collector scans and no store writes (abi.Context.Keep, OpKeep),
// where it is made: the value itself becomes a copy whose shadow is that
// cell (OpKept), and every use of it reads the copy, as it did the value.
// While the collector marks, native code writes the keep with Go's write
// barrier (mir's valueStore), as it makes the stores themselves.
//
// Kept where it is made, a value is kept once, wherever the stores that
// write its cell are, and its uses need not be rebuilt: the copy is the
// value, under its own ID. Kept before each store instead, as it had been,
// every use the store reached had to take the copy, through phis where
// copies met -- an SSA construction of each kept value over the whole
// function -- and a keep at a later store, of a copy an earlier one made,
// wrote the cell what it held, and was taken out again: those had been a
// tenth of a compile's time. A keep where a value is made runs on paths
// that reach no store as well, and a value made outside a loop whose
// stores keep it is kept once, not each time round.

// keepAcrossStores keeps the values read from cells that are used after a
// store that may write their cell (liveAcross, aliases), up to the build's
// limit (keepLimit) in a function, and reports whether it kept any; their
// phis' shadows are then to be made again (shadowMerges). If it kept none,
// it returns what is live across the stores, as storeChecks would find it:
// the function is as it was, but for calls it left to Go, which are stores
// no longer and change nothing live across the others.
//
// A store whose values do not all fit in the cells left is not kept for:
// a call is then made by Go, a property store checks its cell against
// theirs (storeChecks), and a BigInt's box makes the function unsafe.
func keepAcrossStores(f *Func) (bool, []*Value, [][]*Value) {
	stores, live := liveAcross(f)
	a := newAliases(f)
	// kept marks, by value ID, the values kept, which order lists in the
	// order they were found.
	var kept []bool
	var order []*Value
	var ph keepPhis
	room := f.keepLimit() - f.Keeps
	for i, s := range stores {
		key := anyKey
		if s.Op == OpPropWrite {
			key = s.Key
		}
		from := len(order)
		for _, c := range live[i] {
			if c.Op == OpKept || kept != nil && kept[c.ID] {
				continue
			}
			var may bool
			switch {
			case s.Op == OpBigBox || allocOnly(s):
				// A pool's object made, a BigInt boxed: it writes its
				// result's cell alone.
				may = a.mayOwnCell(s, c.Shadow)
			case popsElement(s):
				// An element popped: its cell, and the result's.
				may = a.mayElemCell(s, c.Shadow)
			case s.Op == OpCall:
				may = a.mayCall(s, c.Shadow)
			case s.Op == OpElemStore || s.Op == OpElemWrite:
				may = a.mayElem(c.Shadow)
			default:
				may = a.may(key, c.Shadow)
			}
			if !may {
				continue
			}
			if kept == nil {
				kept = f.bools(f.nextID)
			}
			kept[c.ID] = true
			order = append(order, c)
		}
		if len(order) == from {
			continue
		}
		order = ph.add(f, kept, order, from)
		if len(order) <= room {
			continue
		}
		// Past the cells left: none of them kept for this store.
		for _, c := range order[from:] {
			kept[c.ID] = false
		}
		order = order[:from]
		if s.Op == OpBigBox {
			// Its cell written over a value read from it before, which
			// no cell is left to keep.
			f.Unsafe = "keep cells run out at a BigInt's box"
		}
		if s.Op == OpCall && s.IC != nil {
			// Not kept, the references could be lost: Go makes the call.
			s.IC.Calls = nil
		}
	}
	if len(order) == 0 {
		return false, stores, live
	}
	keepWhereMade(f, kept, order)
	return true, nil, nil
}

// keepPhis finds the phis that must be kept with the values kept where
// they are made. A phi of a kept value -- a loop's, carrying it round --
// holds its pointer word in the value's keep cell, which the value's keep
// writes again each time the value is made: where the phi may still be
// used then, the phi is kept too, before, as a value of its own. It may be
// used there only if it dominates where the value is made: a value is used
// only where its definition dominates. A phi kept is a value of its own,
// and the phis of it hold its cell, not the value's.
type keepPhis struct {
	// users are, by value ID, the phis each is an operand of: from
	// users[at[id]] on, through next.
	at, next []int
	users    []*Value
	dom      *domTree
	seen     []int
	stamp    int
	stack    []*Value
}

// add keeps the phis order[from:] makes necessary, and those that they do,
// and returns order with them added.
func (p *keepPhis) add(f *Func, kept []bool, order []*Value, from int) []*Value {
	if p.at == nil {
		p.at, p.seen = f.ints(f.nextID), f.ints(f.nextID)
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op != OpPhi {
					break
				}
				if v.Type != Tagged {
					continue
				}
				for _, x := range v.Args {
					if x.ID < len(p.at) {
						p.users = append(p.users, v)
						p.next = append(p.next, p.at[x.ID])
						p.at[x.ID] = len(p.users)
					}
				}
			}
		}
	}
	if len(p.users) == 0 {
		return order
	}
	for j := from; j < len(order); j++ {
		c := order[j]
		p.stamp++
		p.stack = append(p.stack[:0], c)
		for len(p.stack) > 0 {
			x := p.stack[len(p.stack)-1]
			p.stack = p.stack[:len(p.stack)-1]
			for u := p.at[x.ID]; u != 0; u = p.next[u-1] {
				q := p.users[u-1]
				if kept[q.ID] || p.seen[q.ID] == p.stamp {
					continue
				}
				p.seen[q.ID] = p.stamp
				if p.dom == nil {
					p.dom = dominators(f)
				}
				if p.dom.dominates(q.Block, c.Block) {
					kept[q.ID] = true
					order = append(order, q)
					continue
				}
				p.stack = append(p.stack, q)
			}
		}
	}
	return order
}

// keepWhereMade keeps each value of order, each in a cell of its own, where
// it is made: the value made becomes a new one, of its op, and the value
// itself the copy, after its keep (OpKeepRef, OpKeep, OpKept) -- a phi's
// after its block's phis, where every phi kept there reads its pointer word
// before any is kept, since one may be read from a cell another's keep
// writes: a phi of a phi carried round a loop.
func keepWhereMade(f *Func, kept []bool, order []*Value) {
	// made is, by the kept value's ID, the value made; its block's count
	// of kept values, by block ID.
	made := f.refsOf(f.nextID)
	nb := 0
	for _, b := range f.Blocks {
		nb = max(nb, b.ID+1)
	}
	count := f.ints(nb)
	for _, c := range order {
		count[c.Block.ID]++
		m := f.alloc(*c)
		m.Uses = 3
		made[c.ID] = m
	}
	for _, c := range order {
		index := f.Keeps
		f.Keeps++
		m := made[c.ID]
		r := f.alloc(Value{Op: OpKeepRef, Type: Ptr, Args: f.refsOf(1), Block: c.Block})
		r.Args[0] = m
		k := f.alloc(Value{Op: OpKeep, Type: Source, Args: f.refsOf(2), Block: c.Block, Index: index, Uses: 1})
		k.Args[0], k.Args[1] = m, r
		*c = Value{ID: c.ID, Op: OpKept, Type: Tagged, Args: f.refsOf(2), Block: c.Block, Uses: c.Uses, Shadow: k}
		c.Args[0], c.Args[1] = k, m
	}
	for _, b := range f.Blocks {
		n := count[b.ID]
		if n == 0 {
			continue
		}
		values := f.refsOf(len(b.Values) + 3*n)[:0]
		var phis []*Value
		lead := 0
		for _, v := range b.Values {
			// A kept phi is its copy by now, its phi made anew.
			if v.Op != OpPhi && (v.ID >= len(kept) || !kept[v.ID] || made[v.ID].Op != OpPhi) {
				break
			}
			lead++
		}
		for i, v := range b.Values {
			if i == lead && len(phis) != 0 {
				values = keepGroup(values, phis)
			}
			if v.ID >= len(kept) || !kept[v.ID] {
				values = append(values, v)
				continue
			}
			m := made[v.ID]
			values = append(values, m)
			if m.Op == OpPhi {
				phis = append(phis, v)
				continue
			}
			values = keepGroup(values, []*Value{v})
		}
		if lead == len(b.Values) && len(phis) != 0 {
			values = keepGroup(values, phis)
		}
		b.Values = values
		if len(phis) != 0 && b.Header != nil && b.PC >= 0 {
			// A loop header's state is after its phis, before their keeps:
			// it names the phis, each slot replaced with another value
			// (Present).
			for _, i := range f.Present(b.Header) {
				if x := b.Header.Slots[i]; x.ID < len(kept) && kept[x.ID] && x.Block == b && made[x.ID].Op == OpPhi {
					b.Header.Slots[i] = made[x.ID]
				}
			}
		}
	}
}

// keepGroup appends to values the refs, the keeps and the copies of the
// kept values cs, made at one place.
func keepGroup(values, cs []*Value) []*Value {
	for _, c := range cs {
		values = append(values, c.Shadow.Args[1])
	}
	for _, c := range cs {
		values = append(values, c.Shadow)
	}
	return append(values, cs...)
}

// WritesKeepCell reports whether v writes the context's keep cell v.Index
// (abi.Context.Keep): a keep (OpKeep), a native call's result
// (OpCall), a BigInt's box (OpBigBox). mir gives the cells out by these
// writes (core's colorKeeps).
func (v *Value) WritesKeepCell() bool {
	return v.Op == OpCall && len(v.Calls()) != 0 || v.Op == OpKeep || v.Op == OpBigBox
}

// fuseKeepRefs has each keep read the pointer word it copies itself, where
// that reads what its OpKeepRef would have: the keep takes its value alone
// (OpKeep's one operand), and the ref goes. The keeps of a block's phis
// read every pointer word before writing any keep cell, since one phi's
// pointer word may be read from a cell another's keep writes -- a phi of
// a phi carried round a loop, whose shadow is a merge of keep cells.
// Where no value's source is a keep cell another keep of the group writes
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
