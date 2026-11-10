package lsra

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// TestAllocateRandom allocates random functions -- branches, loops, phis,
// calls and clobbers, more values live than registers -- and checks the
// result by liveness of its own: no two values live at once share a
// register or a slot -- a value two slots wide taking both -- every value
// in a register live across a call has a slot and is reloaded after it,
// none is live across a clobber in a register it clobbers, and an
// instruction's temporaries have registers apart from its operands', its
// results' and those of the values live across it.
func TestAllocateRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	cfg := &Config{Classes: [][]int{{3, 1, 0, 2}, {0, 1, 2}}, NumSlots: 1000}
	// An allocator kept from one function to the next allocates as a new
	// one does.
	var al Allocator
	for iter := 0; iter < 10000; iter++ {
		f := randomFunc(rng)
		res, err := Allocate(f, cfg)
		if err != nil {
			t.Fatalf("iteration %d: %v", iter, err)
		}
		checkAllocation(t, iter, f, cfg, res)
		again, err := al.Allocate(f, cfg)
		if err != nil {
			t.Fatalf("iteration %d: %v from a kept allocator", iter, err)
		}
		if !slices.Equal(res.Reg, again.Reg) || !slices.Equal(res.Slot, again.Slot) || res.NumSlots != again.NumSlots {
			t.Fatalf("iteration %d: a kept allocator's result differs", iter)
		}
		for i := range f.Blocks {
			for j := range f.Blocks[i].Insts {
				if !slices.Equal(res.Across(i, j), again.Across(i, j)) {
					t.Fatalf("iteration %d: a kept allocator's reloads at b%d.%d differ", iter, i, j)
				}
			}
		}
	}
}

func randomFunc(rng *rand.Rand) *Func {
	nb := 1 + rng.IntN(8)
	f := &Func{Blocks: make([]Block, nb)}
	// In some functions the second class's registers are two slots wide.
	wide := rng.IntN(3) == 0
	newV := func(cls int8) VReg {
		f.Class = append(f.Class, cls)
		f.FixedSlot = append(f.FixedSlot, -1)
		w := int8(1)
		if wide && cls == 1 {
			w = 2
		}
		f.Width = append(f.Width, w)
		return VReg(len(f.Class) - 1)
	}
	// Edges: each block to the next, some forward or back as well.
	for i := 0; i < nb-1; i++ {
		f.Blocks[i].Succs = append(f.Blocks[i].Succs, i+1)
		if rng.IntN(3) == 0 {
			j := rng.IntN(nb)
			if j != i+1 {
				f.Blocks[i].Succs = append(f.Blocks[i].Succs, j)
			}
		}
	}
	for i := range f.Blocks {
		for _, s := range f.Blocks[i].Succs {
			f.Blocks[s].Preds = append(f.Blocks[s].Preds, i)
		}
	}
	var defined []VReg
	params := 1 + rng.IntN(3)
	slot := int32(0)
	for k := 0; k < params; k++ {
		v := newV(int8(rng.IntN(2)))
		f.FixedSlot[v] = slot
		slot += int32(f.Width[v])
		f.Blocks[0].Insts = append(f.Blocks[0].Insts, Inst{Defs: []VReg{v}, Hint: None})
		defined = append(defined, v)
	}
	pick := func(cls int8) VReg {
		var c []VReg
		for _, v := range defined {
			if f.Class[v] == cls {
				c = append(c, v)
			}
		}
		if len(c) == 0 {
			return None
		}
		return c[rng.IntN(len(c))]
	}
	for i := range f.Blocks {
		b := &f.Blocks[i]
		b.Depth = rng.IntN(3)
		// Phis only where every predecessor has one successor.
		phis := len(b.Preds) > 1
		for _, p := range b.Preds {
			if len(f.Blocks[p].Succs) != 1 {
				phis = false
			}
		}
		if phis {
			for k := rng.IntN(3); k > 0; k-- {
				cls := int8(rng.IntN(2))
				d := newV(cls)
				var args []VReg
				for range b.Preds {
					args = append(args, pick(cls))
				}
				if slices.Contains(args, None) {
					continue
				}
				b.Phis = append(b.Phis, Phi{Def: d, Args: args})
				defined = append(defined, d)
			}
		}
		for k := 1 + rng.IntN(12); k > 0; k-- {
			var in Inst
			in.Hint = None
			for u := rng.IntN(3); u > 0; u-- {
				if v := pick(int8(rng.IntN(2))); v != None {
					in.Uses = append(in.Uses, v)
				}
			}
			switch rng.IntN(10) {
			case 0:
				in.Call = true
			case 1:
				in.Clobber = []uint64{1 << uint(rng.IntN(4)), 1 << uint(rng.IntN(3))}
			}
			if !in.Call && rng.IntN(4) == 0 {
				for k := 1 + rng.IntN(2); k > 0; k-- {
					in.Temps = append(in.Temps, newV(int8(rng.IntN(2))))
				}
			}
			for d := rng.IntN(3); d > 0; d-- {
				in.Defs = append(in.Defs, newV(int8(rng.IntN(2))))
			}
			if len(in.Uses) > 0 && len(in.Defs) > 0 {
				in.Hint = in.Uses[0]
			}
			defined = append(defined, in.Defs...)
			b.Insts = append(b.Insts, in)
		}
	}
	return f
}

func checkAllocation(t *testing.T, iter int, f *Func, cfg *Config, res *Result) {
	t.Helper()
	n := len(f.Class)
	type set map[VReg]bool
	liveIn := make([]set, len(f.Blocks))
	liveOut := make([]set, len(f.Blocks))
	for i := range f.Blocks {
		liveIn[i], liveOut[i] = set{}, set{}
	}
	transfer := func(i int, out set, visit func(j int, after set)) set {
		b := &f.Blocks[i]
		live := set{}
		for v := range out {
			live[v] = true
		}
		for j := len(b.Insts) - 1; j >= 0; j-- {
			if visit != nil {
				visit(j, live)
			}
			for _, d := range b.Insts[j].Defs {
				delete(live, d)
			}
			for _, u := range b.Insts[j].Uses {
				live[u] = true
			}
		}
		for _, p := range b.Phis {
			delete(live, p.Def)
		}
		return live
	}
	for changed := true; changed; {
		changed = false
		for i := len(f.Blocks) - 1; i >= 0; i-- {
			out := set{}
			for _, s := range f.Blocks[i].Succs {
				for v := range liveIn[s] {
					out[v] = true
				}
				pi := slices.Index(f.Blocks[s].Preds, i)
				for _, p := range f.Blocks[s].Phis {
					out[p.Args[pi]] = true
				}
			}
			in := transfer(i, out, nil)
			if len(in) != len(liveIn[i]) || len(out) != len(liveOut[i]) {
				changed = true
			}
			liveIn[i], liveOut[i] = in, out
		}
	}
	conflict := func(a, b VReg, where string) {
		if a == b || f.Class[a] != f.Class[b] {
			return
		}
		if ra, rb := res.Reg[a], res.Reg[b]; ra >= 0 && ra == rb {
			t.Fatalf("iteration %d: v%d and v%d, live at once %s, share register %d", iter, a, b, where, ra)
		}
	}
	width := func(v VReg) int32 { return max(int32(f.Width[v]), 1) }
	slotConflict := func(a, b VReg) {
		sa, sb := res.Slot[a], res.Slot[b]
		if a != b && sa >= 0 && sb >= 0 && sa < sb+width(b) && sb < sa+width(a) {
			t.Fatalf("iteration %d: v%d and v%d, live at once, share slots %d and %d", iter, a, b, sa, sb)
		}
	}
	for v := 0; v < n; v++ {
		if res.Reg[v] < 0 && res.Slot[v] < 0 {
			// Unused values need no location.
			used := false
			for i := range f.Blocks {
				for _, in := range f.Blocks[i].Insts {
					used = used || slices.Contains(in.Uses, VReg(v))
				}
				for _, p := range f.Blocks[i].Phis {
					used = used || slices.Contains(p.Args, VReg(v))
				}
			}
			if used {
				t.Fatalf("iteration %d: v%d is used but has no location", iter, v)
			}
		}
	}
	for i := range f.Blocks {
		b := &f.Blocks[i]
		in := liveIn[i]
		var defs []VReg
		for _, p := range b.Phis {
			defs = append(defs, p.Def)
		}
		for v := range in {
			for w := range in {
				conflict(v, w, "into a block")
				slotConflict(v, w)
			}
			for _, d := range defs {
				conflict(v, d, "at a block's phis")
				slotConflict(v, d)
			}
		}
		for _, d := range defs {
			for _, e := range defs {
				conflict(d, e, "among phis")
			}
		}
		transfer(i, liveOut[i], func(j int, after set) {
			inst := &b.Insts[j]
			for _, d := range inst.Defs {
				for v := range after {
					conflict(d, v, "after a definition")
					slotConflict(d, v)
				}
				for _, e := range inst.Defs {
					conflict(d, e, "among definitions")
				}
			}
			for k, tv := range inst.Temps {
				r := res.Reg[tv]
				if r < 0 {
					t.Fatalf("iteration %d: temporary v%d has no register", iter, tv)
				}
				if int(f.Class[tv]) < len(inst.Clobber) && inst.Clobber[f.Class[tv]]&(1<<r) != 0 {
					t.Fatalf("iteration %d: temporary v%d in register %d its instruction clobbers", iter, tv, r)
				}
				others := append(append(slices.Clone(inst.Uses), inst.Defs...), inst.Temps[:k]...)
				for v := range after {
					others = append(others, v)
				}
				for _, v := range others {
					conflict(tv, v, "at its instruction")
				}
			}
			for v := range after {
				if slices.Contains(inst.Defs, v) {
					continue
				}
				// v lives across the instruction.
				if inst.Call && res.Reg[v] >= 0 {
					if res.Slot[v] < 0 {
						t.Fatalf("iteration %d: v%d is in a register across a call without a slot", iter, v)
					}
					if !slices.Contains(res.Across(i, j), v) {
						t.Fatalf("iteration %d: v%d is not reloaded after a call it lives across", iter, v)
					}
				}
				if r := res.Reg[v]; r >= 0 && int(f.Class[v]) < len(inst.Clobber) && inst.Clobber[f.Class[v]]&(1<<r) != 0 {
					t.Fatalf("iteration %d: v%d lives in register %d across its clobber", iter, v, r)
				}
			}
		})
	}
	for v := 0; v < n; v++ {
		if r := res.Reg[v]; r >= 0 && !slices.Contains(cfg.Classes[f.Class[v]], int(r)) {
			t.Fatalf("iteration %d: v%d in register %d, not of its class", iter, v, r)
		}
		if s := f.FixedSlot[v]; s >= 0 && res.Slot[v] >= 0 && res.Slot[v] != s {
			t.Fatalf("iteration %d: v%d arrives in slot %d but is given %d", iter, v, s, res.Slot[v])
		}
		if s := res.Slot[v]; s >= 0 && int(s+width(VReg(v))) > cfg.NumSlots {
			t.Fatalf("iteration %d: v%d in slot %d of %d, %d wide", iter, v, s, cfg.NumSlots, width(VReg(v)))
		}
	}
}

// TestCoalesce gives a loop's phi, its argument from the back edge and its
// argument from the entry one register, so that neither edge moves, and a
// value live across the loop, in its holes too, another.
func TestCoalesce(t *testing.T) {
	// v0: a parameter; v1 = phi(v0, v2); v2 = v1 op v3; v3 live throughout.
	f := &Func{
		Class:     []int8{0, 0, 0, 0},
		FixedSlot: []int32{0, -1, -1, 1},
		Blocks: []Block{
			{Succs: []int{1}, Insts: []Inst{{Defs: []VReg{0}, Hint: None}, {Defs: []VReg{3}, Hint: None}}},
			{Preds: []int{0, 2}, Succs: []int{2, 3}, Depth: 1, Phis: []Phi{{Def: 1, Args: []VReg{0, 2}}},
				Insts: []Inst{{Uses: []VReg{1, 3}, Defs: []VReg{2}, Hint: 1}, {Uses: []VReg{2}, Hint: None}}},
			{Preds: []int{1}, Succs: []int{1}, Depth: 1, Insts: []Inst{{Hint: None}}},
			{Preds: []int{1}, Insts: []Inst{{Uses: []VReg{2, 3}, Hint: None}}},
		},
	}
	cfg := &Config{Classes: [][]int{{0, 1}}, NumSlots: 8}
	res, err := Allocate(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkAllocation(t, 0, f, cfg, res)
	if res.Reg[0] < 0 || res.Reg[0] != res.Reg[1] || res.Reg[1] != res.Reg[2] {
		t.Fatalf("the phi and its arguments in %v, %v, %v", res.Reg[0], res.Reg[1], res.Reg[2])
	}
	if res.Reg[3] < 0 || res.Reg[3] == res.Reg[1] {
		t.Fatalf("v3 in %v beside %v", res.Reg[3], res.Reg[1])
	}
}

// TestRanges checks overlap and unionInto, few ranges against many and
// many against many, against the positions the lists cover.
func TestRanges(t *testing.T) {
	rnd := rand.New(rand.NewPCG(3, 5))
	list := func(n int) ([]rng, []bool) {
		cover := make([]bool, 400)
		for range n {
			from := rnd.IntN(390)
			for p := from; p < from+1+rnd.IntN(8); p++ {
				cover[p] = true
			}
		}
		var rs []rng
		for p := 0; p < len(cover); p++ {
			if !cover[p] {
				continue
			}
			from := p
			for p < len(cover) && cover[p] {
				p++
			}
			rs = append(rs, rng{int32(from), int32(p)})
		}
		return rs, cover
	}
	var ar arena[rng]
	for iter := range 20000 {
		a, ca := list(1 + rnd.IntN(60))
		b, cb := list(1 + rnd.IntN(6))
		if iter%2 == 1 {
			a, b, ca, cb = b, a, cb, ca
		}
		meet := false
		for p := range ca {
			meet = meet || ca[p] && cb[p]
		}
		if got := overlap(a, b); got != meet {
			t.Fatalf("overlap(%v, %v) = %v", a, b, got)
		}
		u := unionInto(&ar, slices.Clone(a), b)
		var want []rng
		for p := 0; p < len(ca); p++ {
			if !ca[p] && !cb[p] {
				continue
			}
			from := p
			for p < len(ca) && (ca[p] || cb[p]) {
				p++
			}
			want = append(want, rng{int32(from), int32(p)})
		}
		if !slices.Equal(u, want) {
			t.Fatalf("unionInto(%v, %v) = %v, want %v", a, b, u, want)
		}
	}
}

// TestLivenessMethods allocates random functions with liveness found by
// the paths from the uses (explore) and by the dataflow, which must give
// the same allocation.
func TestLivenessMethods(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 17))
	cfg := &Config{Classes: [][]int{{3, 1, 0, 2}, {0, 1, 2}}, NumSlots: 1000}
	defer func() { forceDataflow = false }()
	for iter := 0; iter < 5000; iter++ {
		f := randomFunc(rng)
		forceDataflow = false
		a, err := Allocate(f, cfg)
		if err != nil {
			t.Fatal(err)
		}
		forceDataflow = true
		b, err := Allocate(f, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(a.Reg, b.Reg) || !slices.Equal(a.Slot, b.Slot) || a.NumSlots != b.NumSlots {
			t.Fatalf("iteration %d: the two liveness methods allocate differently", iter)
		}
	}
}
