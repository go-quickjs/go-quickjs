package lsra

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// checkAllocation fails the test where checkResult finds res wrong.
func checkAllocation(t *testing.T, iter int, f *Func, cfg *Config, res *Result) {
	t.Helper()
	if err := checkResult(f, cfg, res); err != nil {
		t.Fatalf("iteration %d: %v\n%s", iter, err, dump(f, res))
	}
}

// checkResult checks an allocation by running the function on locations,
// as a client's code would: what each register and slot holds at each
// point -- each value written where it is defined, and into its slot too
// where it is stored there; a split value's moves; a phi's arguments and
// an edge's moves as if all at once; a call clobbering every register,
// then its reloads; an instruction's clobbers and temporaries -- met over
// the paths into each block, and each use reading a location that holds
// its value on every path. Where each value is comes from the result
// alone: its register or slot, a split one's Entry and Moves. It checks
// too that each register is of its value's class, a parameter's slot the
// one it arrives in, an instruction's temporaries apart from its operands
// and results, and the slots below Config.NumSlots. Every use must be
// dominated by its definition.
func checkResult(f *Func, cfg *Config, res *Result) error {
	n := len(f.Class)
	nb := len(f.Blocks)
	width := func(v VReg) int32 {
		if int(v) < len(f.Width) && f.Width[v] > 1 {
			return int32(f.Width[v])
		}
		return 1
	}
	for v := range n {
		if f.Class[v] == NoClass {
			continue
		}
		if r := res.Reg[v]; r >= 0 && !slices.Contains(cfg.Classes[f.Class[v]], int(r)) {
			return fmt.Errorf("v%d in register %d, not of its class", v, r)
		}
		if s := f.FixedSlot[v]; s >= 0 && res.Slot[v] >= 0 && res.Slot[v] != s {
			return fmt.Errorf("v%d arrives in slot %d but is given %d", v, s, res.Slot[v])
		}
		if s := res.Slot[v]; s >= 0 && int(s+width(VReg(v))) > cfg.NumSlots {
			return fmt.Errorf("v%d in slot %d of %d, %d wide", v, s, cfg.NumSlots, width(VReg(v)))
		}
		if (res.Split(VReg(v)) || res.Stored(VReg(v))) && (res.Reg[v] < 0 || res.Slot[v] < 0) {
			return fmt.Errorf("v%d split or stored in register %d and slot %d", v, res.Reg[v], res.Slot[v])
		}
	}
	// A location is a register, its class times 64 plus its number, or a
	// slot, from slotBase. It holds a value, or nothing of use: no value,
	// or different ones on different paths. top is no path yet.
	const slotBase = 256
	const (
		nothing int32 = -1
		top     int32 = -2
	)
	nloc := slotBase + cfg.NumSlots + 2
	in := make([][]int32, nb)
	for i := range in {
		in[i] = make([]int32, nloc)
		for k := range in[i] {
			in[i][k] = top
		}
	}
	for k := range in[0] {
		in[0][k] = nothing
	}
	for v := range n {
		if s := f.FixedSlot[v]; s >= 0 {
			for k := range width(VReg(v)) {
				in[0][slotBase+int(s+k)] = int32(v)
			}
		}
	}
	// locs are the locations of v in register r, or in its slot for -1.
	locs := func(v VReg, r int16) []int {
		if r >= 0 {
			return []int{int(f.Class[v])*64 + int(r)}
		}
		if res.Slot[v] < 0 {
			return nil
		}
		out := []int{slotBase + int(res.Slot[v])}
		if width(v) == 2 {
			out = append(out, out[0]+1)
		}
		return out
	}
	holds := func(st []int32, v VReg, r int16) bool {
		ls := locs(v, r)
		for _, l := range ls {
			if st[l] != int32(v) {
				return false
			}
		}
		return len(ls) > 0
	}
	write := func(st []int32, v VReg, r int16) {
		for _, l := range locs(v, r) {
			st[l] = int32(v)
		}
	}
	// where is each value's register at the point run is at, -1 for its
	// slot, or unknown.
	const unknown = -2
	where := make([]int16, n)
	// startWhere is where v is as block b begins.
	startWhere := func(b int, v VReg) int16 {
		if !res.Split(v) {
			return res.Reg[v]
		}
		for _, m := range res.Entry(b) {
			if m.V == v {
				return m.Reg
			}
		}
		return unknown
	}
	// copies makes the moves that copy, as if all at once: each read, then
	// each written.
	copies := func(st []int32, ms []Move, check bool) error {
		for _, m := range ms {
			if !res.Split(m.V) || (m.Reg >= 0 && m.Reg != res.Reg[m.V]) {
				return fmt.Errorf("a move of v%d into %d, split %v in %d", m.V, m.Reg, res.Split(m.V), res.Reg[m.V])
			}
			if !m.Copy {
				continue
			}
			from := res.Reg[m.V]
			if m.Reg >= 0 {
				from = -1
			}
			if check && !holds(st, m.V, from) {
				return fmt.Errorf("a move of v%d from %d, which does not hold it", m.V, from)
			}
		}
		for _, m := range ms {
			if m.Copy {
				write(st, m.V, m.Reg)
			}
		}
		return nil
	}
	meet := func(dst, src []int32) bool {
		changed := false
		for k, x := range src {
			switch y := dst[k]; {
			case x == top || y == x:
			case y == top:
				dst[k], changed = x, true
			case y != nothing:
				dst[k], changed = nothing, true
			}
		}
		return changed
	}
	// run runs block b from st, what it holds where the block begins,
	// checking each read if check, and meets what each edge leaves into
	// its successor's.
	run := func(b int, st []int32, check bool) (bool, error) {
		blk := &f.Blocks[b]
		for v := range where {
			where[v] = unknown
			if f.Class[v] != NoClass && !res.Split(VReg(v)) {
				where[v] = res.Reg[v]
			}
		}
		for _, m := range res.Entry(b) {
			if !res.Split(m.V) || m.Copy || (m.Reg >= 0 && m.Reg != res.Reg[m.V]) {
				return false, fmt.Errorf("b%d: an entry of v%d into %d", b, m.V, m.Reg)
			}
			where[m.V] = m.Reg
		}
		for _, p := range blk.Phis {
			if r := where[p.Def]; r >= 0 && res.Stored(p.Def) {
				if check && !holds(st, p.Def, r) {
					return false, fmt.Errorf("b%d: phi v%d is not in its register %d to store", b, p.Def, r)
				}
				write(st, p.Def, -1)
			}
		}
		for j := range blk.Insts {
			inst := &blk.Insts[j]
			ms := res.Moves(b, j)
			if err := copies(st, ms, check); err != nil {
				return false, fmt.Errorf("b%d.%d: %v", b, j, err)
			}
			for _, m := range ms {
				where[m.V] = m.Reg
			}
			for _, u := range inst.Uses {
				if u < 0 || f.Class[u] == NoClass {
					continue
				}
				if where[u] == unknown {
					return false, fmt.Errorf("b%d.%d: v%d used where nothing says where it is", b, j, u)
				}
				if check && !holds(st, u, where[u]) {
					return false, fmt.Errorf("b%d.%d: v%d used from %d, which does not hold it", b, j, u, where[u])
				}
			}
			for k, tv := range inst.Temps {
				r := res.Reg[tv]
				if r < 0 {
					return false, fmt.Errorf("b%d.%d: temporary v%d has no register", b, j, tv)
				}
				if c := int(f.Class[tv]); c < len(inst.Clobber) && inst.Clobber[c]&(1<<r) != 0 {
					return false, fmt.Errorf("b%d.%d: temporary v%d in register %d its instruction clobbers", b, j, tv, r)
				}
				others := append(append(slices.Clone(inst.Uses), inst.Defs...), inst.Temps[:k]...)
				for _, o := range others {
					if o >= 0 && f.Class[o] == f.Class[tv] && where[o] == r {
						return false, fmt.Errorf("b%d.%d: temporary v%d shares register %d with v%d", b, j, tv, r, o)
					}
				}
			}
			for c := range cfg.Classes {
				var mask uint64
				if inst.Call {
					mask = ^uint64(0)
				} else if c < len(inst.Clobber) {
					mask = inst.Clobber[c]
				}
				for k := 0; k < 64; k++ {
					if mask&(1<<k) != 0 {
						st[c*64+k] = nothing
					}
				}
			}
			for _, tv := range inst.Temps {
				for _, l := range locs(tv, res.Reg[tv]) {
					st[l] = nothing
				}
			}
			for _, d := range inst.Defs {
				if d < 0 || f.Class[d] == NoClass {
					continue
				}
				if where[d] == unknown {
					return false, fmt.Errorf("b%d.%d: v%d defined where nothing says where it is", b, j, d)
				}
				if s := f.FixedSlot[d]; s >= 0 {
					// It is loaded from the slot it arrives in, or is there.
					for k := range width(d) {
						if check && st[slotBase+int(s+k)] != int32(d) {
							return false, fmt.Errorf("b%d.%d: parameter v%d is not in its slot", b, j, d)
						}
					}
				}
				write(st, d, where[d])
				if where[d] >= 0 && res.Stored(d) {
					write(st, d, -1)
				}
			}
			if inst.Call {
				for _, v := range res.Across(b, j) {
					if where[v] < 0 || where[v] != res.Reg[v] {
						return false, fmt.Errorf("b%d.%d: v%d reloaded but not in its register", b, j, v)
					}
					if check && !holds(st, v, -1) {
						return false, fmt.Errorf("b%d.%d: v%d reloaded from a slot that does not hold it", b, j, v)
					}
					write(st, v, res.Reg[v])
				}
			}
		}
		changed := false
		for k, sc := range blk.Succs {
			out := slices.Clone(st)
			pi := slices.Index(f.Blocks[sc].Preds, b)
			type phiMove struct {
				arg, def VReg
				from, to int16
			}
			var pms []phiMove
			for _, p := range f.Blocks[sc].Phis {
				a := p.Args[pi]
				if a < 0 || f.Class[a] == NoClass {
					continue
				}
				if where[a] == unknown {
					return false, fmt.Errorf("b%d->b%d: argument v%d where nothing says where it is", b, sc, a)
				}
				to := startWhere(sc, p.Def)
				if to == unknown {
					return false, fmt.Errorf("b%d: phi v%d where nothing says where it is", sc, p.Def)
				}
				pms = append(pms, phiMove{a, p.Def, where[a], to})
			}
			for _, pm := range pms {
				if check && !holds(out, pm.arg, pm.from) {
					return false, fmt.Errorf("b%d->b%d: argument v%d read from %d, which does not hold it", b, sc, pm.arg, pm.from)
				}
			}
			ms := res.Edge(b, k)
			if len(ms) > 0 && len(blk.Succs) > 1 && len(f.Blocks[sc].Preds) > 1 {
				return false, fmt.Errorf("b%d->b%d: moves on a critical edge", b, sc)
			}
			if err := copies(out, ms, check); err != nil {
				return false, fmt.Errorf("b%d->b%d: %v", b, sc, err)
			}
			for _, pm := range pms {
				write(out, pm.def, pm.to)
			}
			if meet(in[sc], out) {
				changed = true
			}
		}
		return changed, nil
	}
	for changed := true; changed; {
		changed = false
		for b := range nb {
			c, err := run(b, slices.Clone(in[b]), false)
			if err != nil {
				return err
			}
			changed = changed || c
		}
	}
	for b := range nb {
		if _, err := run(b, slices.Clone(in[b]), true); err != nil {
			return err
		}
	}
	return nil
}

// dump prints f and its allocation, for a failure's message.
func dump(f *Func, res *Result) string {
	var sb strings.Builder
	loc := func(v VReg) string {
		s := fmt.Sprintf("v%d", v)
		if v < 0 || f.Class[v] == NoClass {
			return s
		}
		s += fmt.Sprintf("{c%d r%d s%d", f.Class[v], res.Reg[v], res.Slot[v])
		if res.Split(v) {
			s += " split"
		}
		if res.Stored(v) {
			s += " stored"
		}
		return s + "}"
	}
	for i := range f.Blocks {
		b := &f.Blocks[i]
		fmt.Fprintf(&sb, "b%d: preds %v succs %v depth %d entry %v\n", i, b.Preds, b.Succs, b.Depth, res.Entry(i))
		for _, p := range b.Phis {
			fmt.Fprintf(&sb, "  %s = phi", loc(p.Def))
			for _, a := range p.Args {
				fmt.Fprintf(&sb, " v%d", a)
			}
			sb.WriteByte('\n')
		}
		for j, in := range b.Insts {
			if ms := res.Moves(i, j); len(ms) > 0 {
				fmt.Fprintf(&sb, "  moves %v\n", ms)
			}
			fmt.Fprintf(&sb, "  %d:", j)
			for _, d := range in.Defs {
				sb.WriteString(" " + loc(d))
			}
			sb.WriteString(" =")
			for _, u := range in.Uses {
				fmt.Fprintf(&sb, " v%d", u)
			}
			if in.Call {
				fmt.Fprintf(&sb, " call across %v", res.Across(i, j))
			}
			if len(in.Clobber) > 0 {
				fmt.Fprintf(&sb, " clobber %v", in.Clobber)
			}
			for _, t := range in.Temps {
				sb.WriteString(" temp " + loc(t))
			}
			sb.WriteByte('\n')
		}
		for k := range b.Succs {
			if ms := res.Edge(i, k); len(ms) > 0 {
				fmt.Fprintf(&sb, "  edge %d: %v\n", k, ms)
			}
		}
	}
	return sb.String()
}
