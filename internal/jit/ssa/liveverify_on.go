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
func liveAfterRef(f *Func, points, cands []*Value, reads func(user *Value, i int) bool) [][]*Value {
	stores := points
	if len(stores) == 0 || len(cands) == 0 {
		return make([][]*Value, len(stores))
	}
	nb := 0
	for _, b := range f.Blocks {
		nb = max(nb, b.ID+1)
	}
	pos, cand := f.ints(f.nextID), f.ints(f.nextID)
	for _, b := range f.Blocks {
		for i, v := range b.Values {
			pos[v.ID] = i
		}
	}
	for k, c := range cands {
		cand[c.ID] = k + 1
	}
	// By candidate, then block ID: the last place in the block that uses
	// the candidate -- a value's index, the block's length for its end, -1
	// for its header -- or -2 for none; and whether it is live at the end.
	last, out := f.ints(len(cands)*nb), f.bools(len(cands)*nb)
	for i := range last {
		last[i] = -2
	}
	use := func(a *Value, b *Block, at int) {
		if a.ID < len(cand) && cand[a.ID] != 0 {
			k := (cand[a.ID]-1)*nb + b.ID
			last[k] = max(last[k], at)
		}
	}
	for _, b := range f.Blocks {
		if b.Header != nil {
			for _, s := range b.Header.Slots {
				if s != nil {
					use(s, b, -1)
				}
			}
		}
		for i, v := range b.Values {
			if v.Op == OpPhi {
				// A phi's argument is used at the end of its predecessor.
				for j, a := range v.Args {
					if a.ID < len(cand) && cand[a.ID] != 0 {
						out[(cand[a.ID]-1)*nb+b.Preds[j].ID] = true
					}
				}
				continue
			}
			for j, a := range v.Args {
				if reads == nil || reads(v, j) {
					use(a, b, i)
				}
			}
			if v.State != nil {
				for _, s := range v.State.Slots {
					if s != nil {
						use(s, b, i)
					}
				}
			}
		}
		if b.Control != nil {
			use(b.Control, b, len(b.Values))
		}
		if b.State != nil {
			for _, s := range b.State.Slots {
				if s != nil {
					use(s, b, len(b.Values))
				}
			}
		}
	}
	// A candidate is live at the end of a block if it is live at the start
	// of a successor: anywhere but where it is defined, if that uses it or
	// it is live at that one's end.
	var work []*Block
	for k, c := range cands {
		row, uses := out[k*nb:(k+1)*nb], last[k*nb:(k+1)*nb]
		liveIn := func(b *Block) bool { return b != c.Block && (uses[b.ID] != -2 || row[b.ID]) }
		work = work[:0]
		for _, b := range f.Blocks {
			if liveIn(b) {
				work = append(work, b)
			}
		}
		for len(work) > 0 {
			b := work[len(work)-1]
			work = work[:len(work)-1]
			for _, p := range b.Preds {
				if !row[p.ID] {
					row[p.ID] = true
					if liveIn(p) {
						work = append(work, p)
					}
				}
			}
		}
	}
	live := make([][]*Value, len(stores))
	for i, s := range stores {
		at := pos[s.ID]
		for k, c := range cands {
			if c.Block == s.Block && pos[c.ID] > at {
				continue // defined after it
			}
			if !out[k*nb+s.Block.ID] && last[k*nb+s.Block.ID] <= at {
				continue // dead after it
			}
			live[i] = append(live[i], c)
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
