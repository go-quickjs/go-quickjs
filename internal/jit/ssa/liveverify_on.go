//go:build quickjs_verify

package ssa

import "fmt"

// verifyLiveness is set by the quickjs_verify build tag: each liveAcross
// is checked against liveAcrossRef, the per-value liveness it replaced.
const verifyLiveness = true

// liveAcrossRef is liveAcross as it was first written, per candidate:
// the reference the bitset liveness is checked against. It is every property store, and for each the values with a
// shadow -- read from cells, and phis that may hold such -- used after it.
func liveAcrossRef(f *Func) ([]*Value, [][]*Value) {
	var stores, cands []*Value
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
	return stores, liveAfterRef(f, stores, cands, nil)
}

// liveAfterRef is, for each of points, the candidates live after it: used
// after it, where reads, if not nil, says an operand's use reads its value
// (an operand of user, its i'th).
//
// It takes the candidates one at a time, each from its uses back to where
// it is defined, with marks by block stamped with the candidate's number
// rather than a table of every candidate by every block: TestJITLongCode's
// longest function has 45,880 candidates and 46,000 blocks, whose tables
// had asked for 17 GB, where the function itself takes some hundreds of
// megabytes. Its memory is the blocks', the values' and the points', and
// what it returns; its time is each candidate's uses, the blocks it is
// live through and the points in them.
func liveAfterRef(f *Func, points, cands []*Value, reads func(user *Value, i int) bool) [][]*Value {
	stores := points
	live := make([][]*Value, len(stores))
	if len(stores) == 0 || len(cands) == 0 {
		return live
	}
	nb := 0
	for _, b := range f.Blocks {
		nb = max(nb, b.ID+1)
	}
	pos, cand := make([]int, f.nextID), make([]int, f.nextID)
	for _, b := range f.Blocks {
		for i, v := range b.Values {
			pos[v.ID] = i
		}
	}
	for k, c := range cands {
		cand[c.ID] = k + 1
	}
	// The points in each block, in their order there: a list by block ID
	// (first), each point's next in its block (next), plus one, or 0.
	first, next := make([]int, nb), make([]int, len(stores))
	for i := len(stores) - 1; i >= 0; i-- {
		b := stores[i].Block.ID
		next[i], first[b] = first[b], i+1
	}
	// Each candidate's uses: the block, and the place in it -- a value's
	// index, the block's length for its end, -1 for its header -- or, for a
	// phi's argument, the predecessor it is used at the end of (end).
	type use struct {
		b        *Block
		at, next int
		end      bool
	}
	uses := []use{{}}
	head := make([]int, len(cands))
	add := func(a *Value, b *Block, at int, end bool) {
		if a != nil && a.ID < len(cand) && cand[a.ID] != 0 {
			k := cand[a.ID] - 1
			uses = append(uses, use{b: b, at: at, end: end, next: head[k]})
			head[k] = len(uses) - 1
		}
	}
	for _, b := range f.Blocks {
		if b.Header != nil {
			for _, s := range b.Header.All() {
				add(s, b, -1, false)
			}
		}
		for i, v := range b.Values {
			if v.Op == OpPhi {
				// A phi's argument is used at the end of its predecessor.
				for j, a := range v.Args {
					add(a, b.Preds[j], 0, true)
				}
				continue
			}
			for j, a := range v.Args {
				if reads == nil || reads(v, j) {
					add(a, b, i, false)
				}
			}
			if v.State != nil {
				for _, s := range v.State.All() {
					add(s, b, i, false)
				}
			}
		}
		if b.Control != nil {
			add(b.Control, b, len(b.Values), false)
		}
		if b.State != nil {
			for _, s := range b.State.All() {
				add(s, b, len(b.Values), false)
			}
		}
	}
	// By block ID, stamped with the candidate's number plus one: the last
	// place in the block that uses it (last, at lastAt), whether it is live
	// at the block's end (out), and whether the block is listed (listed).
	last, lastAt, out, listed := make([]int, nb), make([]int, nb), make([]int, nb), make([]int, nb)
	var touched, work []*Block
	for k, c := range cands {
		e := k + 1
		touched = touched[:0]
		list := func(b *Block) {
			if listed[b.ID] != e {
				listed[b.ID] = e
				touched = append(touched, b)
			}
		}
		for u := head[k]; u != 0; u = uses[u].next {
			x := &uses[u]
			if x.end {
				out[x.b.ID] = e
			} else if last[x.b.ID] != e {
				last[x.b.ID], lastAt[x.b.ID] = e, x.at
			} else {
				lastAt[x.b.ID] = max(lastAt[x.b.ID], x.at)
			}
			list(x.b)
		}
		// A candidate is live at the end of a block if it is live at the
		// start of a successor: anywhere but where it is defined, if that
		// uses it or it is live at that one's end.
		liveIn := func(b *Block) bool { return b != c.Block && (last[b.ID] == e || out[b.ID] == e) }
		work = work[:0]
		for _, b := range touched {
			if liveIn(b) {
				work = append(work, b)
			}
		}
		for len(work) > 0 {
			b := work[len(work)-1]
			work = work[:len(work)-1]
			for _, p := range b.Preds {
				if out[p.ID] != e {
					out[p.ID] = e
					list(p)
					if liveIn(p) {
						work = append(work, p)
					}
				}
			}
		}
		for _, b := range touched {
			for i := first[b.ID]; i != 0; i = next[i-1] {
				s := stores[i-1]
				at := pos[s.ID]
				if c.Block == b && pos[c.ID] > at {
					continue // defined after it
				}
				if out[b.ID] != e && (last[b.ID] != e || lastAt[b.ID] <= at) {
					continue // dead after it
				}
				live[i-1] = append(live[i-1], c)
			}
		}
	}
	return live
}

// checkKeeps panics where a keep writes a keep cell that a value used
// after it may hold its pointer word in, through its shadow: a phi of a
// kept value, carried round a loop to the value's keep, not kept itself
// (keepPhis). A keep's own value, read by its ref before, and a copy's,
// whose number word alone it takes, are no reads of the pointer word.
func checkKeeps(f *Func) {
	var keeps, cands []*Value
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			switch {
			case v.Op == OpKeep:
				keeps = append(keeps, v)
			case v.Type == Tagged && v.Shadow != nil:
				cands = append(cands, v)
			}
		}
	}
	reads := func(user *Value, i int) bool {
		return !(user.Op == OpKept && i == 1 || user.Op == OpKeep && i == 0 && len(user.Args) == 2)
	}
	live := liveAfterRef(f, keeps, cands, reads)
	for i, k := range keeps {
		for _, c := range live[i] {
			seen := map[*Value]bool{}
			var holds func(s *Value) bool
			holds = func(s *Value) bool {
				if s == nil || seen[s] {
					return false
				}
				seen[s] = true
				switch s.Op {
				case OpKeep:
					return s.Index == k.Index
				case OpPhi:
					for _, a := range s.Args {
						if holds(a) {
							return true
						}
					}
				}
				return false
			}
			if holds(c.Shadow) {
				panic(fmt.Sprintf("ssa: %v keeps into cell %d, where %v, used after it, may hold its pointer word", k, k.Index, c))
			}
		}
	}
}

// checkShadows panics unless every phi that needs a shadow has the one
// shadowMerges would make it: of its operands' shadows, or of their slots
// as constants in the predecessors they come from (mendShadows).
func checkShadows(f *Func) {
	origin := origins(f)
	need := map[*Value]bool{}
	var walk func(v *Value)
	walk = func(v *Value) {
		if v.Op != OpPhi || need[v] || origin.At(v) == OriginScalar {
			return
		}
		need[v] = true
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
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if !need[v] {
				continue
			}
			if v.Shadow == nil || v.Shadow.Op != OpPhi || len(v.Shadow.Args) != len(v.Args) {
				panic(fmt.Sprintf("ssa: %v needs a shadow, has %v", v, v.Shadow))
			}
			for i, a := range v.Args {
				s := v.Shadow.Args[i]
				k := -1
				if o := origin.At(a); o >= 0 {
					k = o
				}
				ok := a.Shadow != nil && s == a.Shadow || a.Shadow == nil && s.Op == OpConstSource && s.Aux == k && s.Block == b.Preds[i]
				if !ok {
					panic(fmt.Sprintf("ssa: %v's shadow takes %v for %v, whose shadow is %v", v, s, a, a.Shadow))
				}
			}
		}
	}
}
