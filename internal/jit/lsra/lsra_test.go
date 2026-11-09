package lsra

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// TestAllocateRandom allocates random functions -- branches, loops, phis,
// calls and clobbers, more values live than registers -- and checks the
// result by liveness of its own: no two values live at once share a
// register or a slot, every value in a register live across a call has a
// slot and is reloaded after it, and none is live across a clobber in a
// register it clobbers.
func TestAllocateRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	cfg := &Config{Classes: [][]int{{3, 1, 0, 2}, {0, 1, 2}}, NumSlots: 1000}
	for iter := 0; iter < 10000; iter++ {
		f := randomFunc(rng)
		res, err := Allocate(f, cfg)
		if err != nil {
			t.Fatalf("iteration %d: %v", iter, err)
		}
		checkAllocation(t, iter, f, cfg, res)
	}
}

func randomFunc(rng *rand.Rand) *Func {
	nb := 1 + rng.IntN(8)
	f := &Func{Blocks: make([]Block, nb)}
	newV := func(cls int8) VReg {
		f.Class = append(f.Class, cls)
		f.FixedSlot = append(f.FixedSlot, -1)
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
	for k := 0; k < params; k++ {
		v := newV(0)
		f.FixedSlot[v] = int32(k)
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
	slotConflict := func(a, b VReg) {
		if a != b && res.Slot[a] >= 0 && res.Slot[a] == res.Slot[b] {
			t.Fatalf("iteration %d: v%d and v%d, live at once, share slot %d", iter, a, b, res.Slot[a])
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
			for v := range after {
				if slices.Contains(inst.Defs, v) {
					continue
				}
				// v lives across the instruction.
				if inst.Call && res.Reg[v] >= 0 {
					if res.Slot[v] < 0 {
						t.Fatalf("iteration %d: v%d is in a register across a call without a slot", iter, v)
					}
					if !slices.Contains(res.Across[[2]int32{int32(i), int32(j)}], v) {
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
