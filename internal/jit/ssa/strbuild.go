package ssa

// A loop's string builder.
//
// A loop that builds a string, s += x at each iteration, makes a string at
// each: the last iteration's is dead once the next has its own, and for
// PDF.js's fonts, built a character at a time, those were most of what
// the collector saw. Where the string is used in the loop only by its own
// additions, nothing but those additions sees any string but the last, and
// each may grow the last in place: the addition of Go it is (GoAdd) becomes
// an append (GoAppend), which makes the string once per entry into the loop
// and appends to it after, its tail in a buffer of its own. A string so
// built is mutable only while nothing else can see it, and is frozen
// (OpFreeze) wherever the loop's code uses it outside the loop, and by Go
// wherever native code leaves for Go: after that, an append makes another,
// of the frozen one and the rest.
//
// The values the loop builds the string in are a web: the phi at the
// loop's header, the additions' results, and the phis of the loop that
// merge them, as an addition made on one branch makes. In the loop each is
// used only by another of them, as the left operand of an addition, or as
// an operand of a phi of the loop; frame states may name any of them,
// where an exit leaves to Go, which freezes the string. Outside the loop,
// each use is preceded by a freeze of what it uses, a phi's at the end of
// the edge's block or at the start of its own. An addition whose right
// operand is in the web too, s += s, is no append.

// stringBuilders finds the webs of f's loops' string builders, makes their
// additions appends, and freezes them where they leave their loop.
func stringBuilders(f *Func) {
	// A web's first append is of a phi: the header's, or one merging it.
	adds := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if isGoAdd(v) && v.Args[0].Op == OpPhi {
				adds++
			}
		}
	}
	if adds == 0 {
		return
	}
	// Each value's uses, by values, with the operand's index, and by
	// blocks' controls: uses[first[id]:first[id+1]].
	type use struct {
		v     *Value
		block *Block
		arg   int
	}
	first := make([]int32, f.nextID+1)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			for _, a := range v.Args {
				first[a.ID+1]++
			}
		}
		if b.Control != nil {
			first[b.Control.ID+1]++
		}
	}
	for i := 1; i < len(first); i++ {
		first[i] += first[i-1]
	}
	uses := make([]use, first[f.nextID])
	next := make([]int32, f.nextID)
	copy(next, first)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			for i, a := range v.Args {
				uses[next[a.ID]] = use{v: v, arg: i}
				next[a.ID]++
			}
		}
		if b.Control != nil {
			uses[next[b.Control.ID]] = use{block: b}
			next[b.Control.ID]++
		}
	}
	usesOf := func(v *Value) []use { return uses[first[v.ID]:first[v.ID+1]] }
	// The result of an addition: the kept value of its keep cell.
	result := func(call *Value) *Value {
		for _, u := range usesOf(call) {
			if u.v != nil && u.v.Op == OpKept && u.v.Args[1] == call {
				return u.v
			}
		}
		return nil
	}
	maxBlock := 0
	for _, b := range f.Blocks {
		maxBlock = max(maxBlock, b.ID)
	}
	index := make([]int, maxBlock+1)
	for i, b := range f.Blocks {
		index[b.ID] = i
	}
	// covered marks the webs found, which an inner loop's are part of;
	// inWeb the web being grown, members its values.
	covered := make([]bool, f.nextID)
	inWeb := make([]bool, f.nextID)
	inLoop := make([]bool, len(f.Blocks))
	var members, work, appends []*Value
	type outside struct {
		v    *Value
		user use
	}
	var leaves []outside
	var ctx builderCheck
	for _, h := range f.Blocks {
		if !h.LoopHeader {
			continue
		}
		loopDone := false
		for _, phi := range h.Values {
			if phi.Op != OpPhi || phi.Type != Tagged || covered[phi.ID] {
				continue
			}
			if !loopDone {
				clear(inLoop)
				loopBlocks(h, inLoop, index)
				loopDone = true
			}
			for _, m := range members {
				inWeb[m.ID] = false
			}
			members, work, appends, leaves = append(members[:0], phi), append(work[:0], phi), appends[:0], leaves[:0]
			inWeb[phi.ID] = true
			join := func(v *Value) {
				if !inWeb[v.ID] {
					inWeb[v.ID] = true
					members = append(members, v)
					work = append(work, v)
				}
			}
			ok := true
			for len(work) > 0 && ok {
				m := work[len(work)-1]
				work = work[:len(work)-1]
				for _, u := range usesOf(m) {
					block := u.block
					if u.v != nil {
						block = u.v.Block
					}
					if !inLoop[index[block.ID]] {
						leaves = append(leaves, outside{m, u})
						continue
					}
					switch {
					case u.v == nil:
						// A branch on it in the loop.
						ok = false
					case u.v.Op == OpPhi:
						join(u.v)
					case isGoAdd(u.v) && u.arg == 0:
						r := result(u.v)
						if r == nil {
							ok = false
							break
						}
						appends = append(appends, u.v)
						join(r)
					case u.v.Op == OpCallCell && isGoAdd(u.v.Args[0]):
						// The keep cell of an addition of it, which its result
						// is kept in.
					default:
						ok = false
					}
				}
			}
			if !ok || len(appends) == 0 {
				continue
			}
			for _, a := range appends {
				if inWeb[a.Args[1].ID] {
					ok = false
				}
			}
			if !ok || !ctx.appendsLast(f, inWeb, appends, index) {
				continue
			}
			for _, v := range members {
				covered[v.ID] = true
			}
			for _, a := range appends {
				// The call's cache may be another value's too.
				site := *a.Calls()[0]
				site.Go = GoAppend
				ic := *a.IC
				ic.Calls = []*CallSite{&site}
				a.IC = &ic
			}
			var frozen [][2]int
			for _, l := range leaves {
				switch {
				case l.user.v != nil && l.user.v.Op == OpPhi:
					// A phi outside the loop merging it: it frozen in the
					// edge's block, where that is outside the loop too -- the
					// preheader of a loop after it, which may build on it --
					// or else the phi's result, past the phis of its block.
					if p := l.user.v.Block.Preds[l.user.arg]; !inLoop[index[p.ID]] {
						frozen = freezeIn(f, p, l.v, frozen)
					} else {
						frozen = freezeAfterPhis(f, l.user.v.Block, l.user.v, frozen)
					}
				case l.user.v != nil:
					frozen = freezeIn(f, l.user.v.Block, l.v, frozen)
				default:
					frozen = freezeIn(f, l.user.block, l.v, frozen)
				}
			}
		}
	}
}

// builderCheck is appendsLast's, its slices kept from one web to the next.
type builderCheck struct {
	places []place
	seen   []bool
	work   []*Block
}

// place is where a value of a web is used: in block, at a value's index
// there, -1 at its start, or len(Values) at its end -- its control's, and a
// phi's of a successor on the edge from it.
type place struct {
	v     *Value
	block *Block
	i     int
}

// appendsLast reports whether each append is its left operand's last use:
// an append grows the string in place, the same one its result is, so no
// use of the operand may follow it -- by a value, a control, a phi on an
// edge after it, a frame state an exit makes the frame of, a shadow -- but
// past the operand's definition, which makes another. The phis of the web
// are its only values a use may follow on another path, and the right
// operands are in no web, so an append in place is the same as a new
// string where these are true: t = s + x; if (c) break; s = t, whose break
// uses the s before t, makes no append.
func (c *builderCheck) appendsLast(f *Func, inWeb []bool, appends []*Value, index []int) bool {
	c.places = c.places[:0]
	add := func(b *Block, v *Value, i int) {
		if inWeb[v.ID] {
			c.places = append(c.places, place{v, b, i})
		}
	}
	state := func(b *Block, s *FrameState, i int) {
		if s != nil {
			for _, x := range s.Slots {
				if x != nil {
					add(b, x, i)
				}
			}
		}
	}
	for _, b := range f.Blocks {
		state(b, b.State, -1)
		state(b, b.Header, -1)
		for i, v := range b.Values {
			if v.Op == OpPhi {
				for k, x := range v.Args {
					add(b.Preds[k], x, len(b.Preds[k].Values))
				}
			} else {
				for _, x := range v.Args {
					add(b, x, i)
				}
			}
			if v.Shadow != nil {
				add(b, v.Shadow, i)
			}
			state(b, v.State, i)
		}
		if b.Control != nil {
			add(b, b.Control, len(b.Values))
		}
	}
	if len(c.seen) < len(f.Blocks) {
		c.seen = make([]bool, len(f.Blocks))
	}
	for _, a := range appends {
		m, b := a.Args[0], a.Block
		i := 0
		for b.Values[i] != a {
			i++
		}
		// The blocks a reaches before m's definition: a use in one follows
		// a, and one in a's block does if past a or if a reaches its block
		// again.
		clear(c.seen)
		c.work = append(c.work[:0], b.Succs...)
		for len(c.work) > 0 {
			s := c.work[len(c.work)-1]
			c.work = c.work[:len(c.work)-1]
			if s == m.Block || c.seen[index[s.ID]] {
				continue
			}
			c.seen[index[s.ID]] = true
			c.work = append(c.work, s.Succs...)
		}
		for _, p := range c.places {
			if p.v == m && (c.seen[index[p.block.ID]] || p.block == b && p.i > i) {
				return false
			}
		}
	}
	return true
}

// isGoAdd reports whether v is an addition Go makes (GoAdd), of two
// operands.
func isGoAdd(v *Value) bool {
	if v.Op != OpCall || len(v.Args) != 2 {
		return false
	}
	calls := v.Calls()
	return len(calls) == 1 && calls[0].Go == GoAdd
}

// loopBlocks marks in inLoop, by the blocks' places in f.Blocks (index, by
// ID), the blocks of h's loop: h, and every block that reaches one of its
// back edges without passing through h.
func loopBlocks(h *Block, inLoop []bool, index []int) {
	inLoop[index[h.ID]] = true
	var work []*Block
	for i, p := range h.Preds {
		if i < len(h.Backedge) && h.Backedge[i] && !inLoop[index[p.ID]] {
			inLoop[index[p.ID]] = true
			work = append(work, p)
		}
	}
	for len(work) > 0 {
		b := work[len(work)-1]
		work = work[:len(work)-1]
		for _, p := range b.Preds {
			if !inLoop[index[p.ID]] {
				inLoop[index[p.ID]] = true
				work = append(work, p)
			}
		}
	}
}

// freezeIn freezes v in b, once, before the first of b's values that uses
// it -- a phi's use is its predecessor's -- or at its end, before its
// control is used; frozen lists the blocks and values frozen so far.
func freezeIn(f *Func, b *Block, v *Value, frozen [][2]int) [][2]int {
	key := [2]int{b.ID, v.ID}
	for _, k := range frozen {
		if k == key {
			return frozen
		}
	}
	fz := f.newValue(b, OpFreeze, None, v)
	vals := b.Values[:len(b.Values)-1]
	for i, x := range vals {
		if x.Op == OpPhi {
			continue
		}
		for _, a := range x.Args {
			if a == v {
				copy(b.Values[i+1:], vals[i:])
				b.Values[i] = fz
				return append(frozen, key)
			}
		}
	}
	return append(frozen, key)
}

// freezeAfterPhis freezes v, a phi of b, past b's phis.
func freezeAfterPhis(f *Func, b *Block, v *Value, frozen [][2]int) [][2]int {
	key := [2]int{b.ID, v.ID}
	for _, k := range frozen {
		if k == key {
			return frozen
		}
	}
	fz := f.newValue(b, OpFreeze, None, v)
	vals := b.Values[:len(b.Values)-1]
	i := 0
	for i < len(vals) && vals[i].Op == OpPhi {
		i++
	}
	copy(b.Values[i+1:], vals[i:])
	b.Values[i] = fz
	return append(frozen, key)
}
