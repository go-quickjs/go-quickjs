package mir

import (
	"fmt"
	"slices"
	"sort"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// CheckAllocations, if set, has every compile check its register
// allocation (checkAllocation) and panic with what it found wrong: for
// tests, and for runs that are to find a miscompile where it is made
// (QJS_JIT_STRESS's check). Set it before any compile.
var CheckAllocations bool

// AllocationError is what a failed check of an allocation panics with:
// compiles do not take it for a function they refuse (compileAMD64's
// recover passes it on).
type AllocationError struct{ Msg string }

func (e *AllocationError) Error() string { return "mir: allocation check: " + e.Msg }

// checkAllocation checks allocate's work against what is plainly right,
// found by other means: the intervals against liveness computed by block
// sets over every value, iterated until none grows, the uses listed
// (refIntervals); no two values whose intervals meet given one register of
// a class, or one spill slot; and the slots a native call saves its
// registers in its own.
func (c *core) checkAllocation(iv []interval, spillCount int) {
	fail := func(format string, args ...any) {
		panic(&AllocationError{fmt.Sprintf(format, args...)})
	}
	ref := c.refIntervals()
	nf := c.f.NumValues()
	for id := range nf {
		a, b := iv[id], ref[id]
		if (a.v == nil) != (b.v == nil) || a.v != nil && (a.start != b.start || a.end != b.end) {
			fail("%v: interval %d..%d (made %t), want %d..%d (made %t)", c.valueOf(iv, ref, id), a.start, a.end, a.v != nil, b.start, b.end, b.v != nil)
		}
	}
	// Each register, by class, and each spill slot: its values' intervals
	// in order of start, each ending before the next begins.
	type span struct{ start, end, id int }
	byLoc := map[[2]int][]span{}
	for id := range iv {
		it := &iv[id]
		if it.v == nil {
			continue
		}
		l, ok := c.loc(it.v)
		if !ok {
			fail("%v has an interval and no location", it.v)
		}
		key := [2]int{0, l.reg}
		switch {
		case l.reg >= 0 && isFloat(it.v):
			key[0] = 1
		case l.reg < 0 && id >= nf:
			continue // a budget loop's register given up: counted in memory
		case l.reg < 0:
			if l.spill < 0 || l.spill >= spillCount {
				fail("%v: spill slot %d of %d", it.v, l.spill, spillCount)
			}
			key = [2]int{2, l.spill}
		}
		byLoc[key] = append(byLoc[key], span{it.start, it.end, id})
	}
	for key, spans := range byLoc {
		sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
		for i := 1; i < len(spans); i++ {
			if a, b := spans[i-1], spans[i]; b.start <= a.end {
				fail("v%d (%d..%d) and v%d (%d..%d) share location %v", a.id, a.start, a.end, b.id, b.start, b.end, key)
			}
		}
	}
	// A call's saves: each register once, each in a slot of its own past
	// the spill slots, or read back from the frame.
	for _, b := range c.order {
		for _, v := range b.Values {
			saves := c.savesOf(v)
			for i, s := range saves {
				if s.frame == 0 && s.slot < spillCount {
					fail("%v saves r%d in spill slot %d", v, s.reg, s.slot)
				}
				for _, t := range saves[:i] {
					if t.reg == s.reg && t.float == s.float || s.frame == 0 && t.frame == 0 && t.slot == s.slot {
						fail("%v saves r%d (slot %d) and r%d (slot %d)", v, t.reg, t.slot, s.reg, s.slot)
					}
				}
			}
		}
	}
}

// valueOf names value id for a failed check.
func (c *core) valueOf(iv, ref []interval, id int) string {
	if iv[id].v != nil {
		return iv[id].v.String()
	}
	if ref[id].v != nil {
		return ref[id].v.String()
	}
	return fmt.Sprintf("v%d", id)
}

// refIntervals are the intervals, by value ID, as allocate made them
// before its uses were met once and its liveness found value by value:
// every use listed, block-level sets of the values used outside the block
// defining them, iterated backwards over the blocks until none grows, and
// each value's interval stretched over each block it is live in or out of.
func (c *core) refIntervals() []interval {
	nv, nb := c.f.NumValues(), numBlocks(c.f)
	pos := make([]int, nv)
	for i := range pos {
		pos[i] = -1
	}
	start, end := make([]int, nb), make([]int, nb)
	var at []*ssa.Block
	for _, b := range c.order {
		start[b.ID] = len(at)
		at = append(at, b)
		for _, v := range b.Values {
			pos[v.ID] = len(at)
			at = append(at, b)
		}
		end[b.ID] = len(at)
		at = append(at, b)
	}
	type use struct {
		v   *ssa.Value
		pos int
	}
	var uses []use
	need := func(v *ssa.Value) bool { return hasResult(v) && !c.isLazy(v) }
	useState := func(s *ssa.FrameState, at int, moved *ssa.Block) {
		if s == nil {
			return
		}
		add := func(x *ssa.Value) {
			if moved == nil || x.Op != ssa.OpPhi || x.Block != moved {
				uses = append(uses, use{x, at})
			}
		}
		for _, v := range s.Slots {
			if v == nil {
				continue
			}
			if x := bigKept(v); x != nil {
				add(x)
				if c.isLazy(v) {
					continue
				}
			}
			if c.isLazy(v) || remat(v) {
				for _, a := range v.Args {
					add(a)
				}
				continue
			}
			add(v)
			if v.Shadow != nil {
				add(v.Shadow)
			}
		}
	}
	for _, b := range c.order {
		for i, v := range b.Values {
			if v.Op == ssa.OpPhi {
				continue
			}
			p, fused := pos[v.ID], c.isFused(v)
			if fused {
				p = end[b.ID]
			}
			for _, a := range v.Args {
				uses = append(uses, use{a, p})
				if a.Shadow != nil {
					uses = append(uses, use{a.Shadow, p})
				}
			}
			at := p
			if v.Op == ssa.OpCall && len(v.Calls()) != 0 && (v.Calls()[0].Go != 0 || ssa.RefillsPools(v)) {
				at++
			}
			if !fused && i+1 < len(b.Values) && b.Values[i+1].State == v.State {
				continue
			}
			useState(v.State, at, nil)
		}
		if b.Control != nil {
			if c.isLazy(b.Control) {
				for _, a := range b.Control.Args {
					uses = append(uses, use{a, end[b.ID]})
				}
			} else {
				uses = append(uses, use{b.Control, end[b.ID]})
				if b.Control.Shadow != nil {
					uses = append(uses, use{b.Control.Shadow, end[b.ID]})
				}
			}
		}
		useState(b.State, end[b.ID], nil)
		if b.LoopHeader {
			useState(b.Header, start[b.ID], nil)
		}
		for _, s := range b.Succs {
			for i, p := range s.Preds {
				if p != b {
					continue
				}
				for _, phi := range s.Values {
					if phi.Op != ssa.OpPhi {
						break
					}
					uses = append(uses, use{phi.Args[i], end[b.ID]})
				}
				if s.LoopHeader && s.Backedge[i] {
					useState(s.Header, end[b.ID], s)
				}
			}
		}
	}
	defBlock := make([]*ssa.Block, nv)
	for _, b := range c.order {
		for _, v := range b.Values {
			defBlock[v.ID] = b
		}
	}
	gid := make([]int, nv)
	var byID []*ssa.Value
	for _, u := range uses {
		if need(u.v) && gid[u.v.ID] == 0 && defBlock[u.v.ID] != at[u.pos] {
			byID = append(byID, u.v)
			gid[u.v.ID] = len(byID)
		}
	}
	words := (len(byID) + 63) / 64
	defsIn, usesIn, liveIn := make([]valueSet, nb), make([]valueSet, nb), make([]valueSet, nb)
	for _, b := range c.order {
		defsIn[b.ID], usesIn[b.ID], liveIn[b.ID] = make(valueSet, words), make(valueSet, words), make(valueSet, words)
		for _, v := range b.Values {
			if g := gid[v.ID]; g != 0 {
				defsIn[b.ID].add(g - 1)
			}
		}
	}
	for _, u := range uses {
		if g := gid[u.v.ID]; g != 0 && defBlock[u.v.ID] != at[u.pos] {
			usesIn[at[u.pos].ID].add(g - 1)
		}
	}
	in := make(valueSet, words)
	for changed := true; changed; {
		changed = false
		for i := len(c.order) - 1; i >= 0; i-- {
			b := c.order[i]
			copy(in, usesIn[b.ID])
			defs := defsIn[b.ID]
			for _, s := range b.Succs {
				if live := liveIn[s.ID]; live != nil {
					for w := range in {
						in[w] |= live[w] &^ defs[w]
					}
				}
			}
			if !slices.Equal(in, liveIn[b.ID]) {
				copy(liveIn[b.ID], in)
				changed = true
			}
		}
	}
	iv := make([]interval, nv)
	get := func(v *ssa.Value) *interval {
		it := &iv[v.ID]
		if it.v == nil {
			p := pos[v.ID]
			if v.Op == ssa.OpPhi {
				p = start[v.Block.ID]
			}
			*it = interval{v: v, start: p, end: p}
		}
		return it
	}
	for _, b := range c.order {
		for _, v := range b.Values {
			if need(v) {
				get(v)
			}
		}
	}
	for _, u := range uses {
		if need(u.v) {
			if it := get(u.v); u.pos > it.end {
				it.end = u.pos
			}
		}
	}
	for _, b := range c.order {
		bs := start[b.ID]
		liveIn[b.ID].each(func(id int) {
			it := get(byID[id])
			it.start, it.end = min(it.start, bs), max(it.end, bs)
		})
		for _, s := range b.Succs {
			if liveIn[s.ID] == nil {
				continue
			}
			liveIn[s.ID].each(func(id int) {
				v := byID[id]
				if v.Block == s && v.Op == ssa.OpPhi {
					return
				}
				it := get(v)
				it.end = max(it.end, end[b.ID])
				if bs < it.start && !defsIn[b.ID].has(id) {
					it.start = bs
				}
			})
		}
	}
	return iv
}
