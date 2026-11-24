package lsra

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// TestAllocateRandom allocates random functions -- branches, loops, phis,
// calls and clobbers, more values live than registers -- and checks each
// result by running the function on its locations (checkResult), splits
// and all; an Allocator kept from one function to the next allocates as a
// new one does.
func TestAllocateRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	plain := &Config{Classes: [][]int{{3, 1, 0, 2}, {0, 1, 2}}, NumSlots: 1000}
	displace := &Config{Classes: plain.Classes, NumSlots: plain.NumSlots, Displace: true}
	stress := &Config{Classes: plain.Classes, NumSlots: plain.NumSlots, StressSplit: true}
	var al Allocator
	split := 0
	for iter := 0; iter < 10000; iter++ {
		f := randomFunc(rng)
		// A third of the functions are allocated with Displace, a third
		// split wherever the allocator can.
		cfg := [...]*Config{plain, displace, stress}[iter%3]
		res, err := Allocate(f, cfg)
		if err != nil {
			t.Fatalf("iteration %d: %v", iter, err)
		}
		checkAllocation(t, iter, f, cfg, res)
		if res.Splits() {
			split++
		}
		again, err := al.Allocate(f, cfg)
		if err != nil {
			t.Fatalf("iteration %d: %v from a kept allocator", iter, err)
		}
		if !sameResult(f, res, again) {
			t.Fatalf("iteration %d: a kept allocator's result differs", iter)
		}
	}
	t.Logf("%d of 10000 functions split", split)
	if split < 500 {
		t.Fatalf("only %d of 10000 functions split", split)
	}
}

// randomFunc makes a random function in SSA form, each use dominated by
// its definition: blocks each to the next, some forward or back as well,
// and in most functions a block on each critical edge, where its moves
// go.
func randomFunc(rng *rand.Rand) *Func {
	nb := 1 + rng.IntN(8)
	succs := make([][]int, nb)
	for i := 0; i < nb-1; i++ {
		succs[i] = append(succs[i], i+1)
		// None back to the entry, which defines the parameters.
		if rng.IntN(3) == 0 {
			if j := 1 + rng.IntN(nb-1); j != i+1 {
				succs[i] = append(succs[i], j)
			}
		}
	}
	if rng.IntN(5) != 0 {
		npreds := make([]int, nb)
		for i := range succs {
			for _, s := range succs[i] {
				npreds[s]++
			}
		}
		for i := 0; i < nb; i++ {
			if len(succs[i]) < 2 {
				continue
			}
			for k, s := range succs[i] {
				if npreds[s] > 1 {
					succs = append(succs, []int{s})
					succs[i][k] = len(succs) - 1
				}
			}
		}
	}
	nb0 := nb
	nb = len(succs)
	f := &Func{Blocks: make([]Block, nb)}
	for i := range f.Blocks {
		f.Blocks[i].Succs = succs[i]
		for _, s := range succs[i] {
			f.Blocks[s].Preds = append(f.Blocks[s].Preds, i)
		}
	}
	// The dominators: each block's comes before it, the blocks of edges,
	// last, dominating only themselves.
	idom := make([]int, nb)
	for i := range idom {
		idom[i] = -1
	}
	idom[0] = 0
	chain := func(b int) []int {
		var out []int
		for ; b != 0; b = idom[b] {
			out = append(out, b)
		}
		return append(out, 0)
	}
	for changed := true; changed; {
		changed = false
		for b := 1; b < nb; b++ {
			d := -1
			for _, p := range f.Blocks[b].Preds {
				switch {
				case idom[p] < 0:
				case d < 0:
					d = p
				default:
					on := map[int]bool{}
					for _, x := range chain(p) {
						on[x] = true
					}
					for _, x := range chain(d) {
						if on[x] {
							d = x
							break
						}
					}
				}
			}
			if d >= 0 && d != idom[b] {
				idom[b], changed = d, true
			}
		}
	}
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
	// defs are what each block defines.
	defs := make([][]VReg, nb)
	params := 1 + rng.IntN(3)
	slot := int32(0)
	for k := 0; k < params; k++ {
		v := newV(int8(rng.IntN(2)))
		f.FixedSlot[v] = slot
		slot += int32(f.Width[v])
		f.Blocks[0].Insts = append(f.Blocks[0].Insts, Inst{Defs: []VReg{v}, Hint: None})
		defs[0] = append(defs[0], v)
	}
	// dominating is what the blocks that strictly dominate b define.
	dominating := func(b int) []VReg {
		var out []VReg
		if b == 0 {
			return out
		}
		for _, x := range chain(idom[b]) {
			out = append(out, defs[x]...)
		}
		return out
	}
	pick := func(from []VReg, cls int8) VReg {
		var c []VReg
		for _, v := range from {
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
		// Phis only where every predecessor has one successor, each
		// argument what its predecessor has where it ends: one made
		// already, or one that dominates this block.
		phis := len(b.Preds) > 1
		for _, p := range b.Preds {
			if len(f.Blocks[p].Succs) != 1 {
				phis = false
			}
		}
		if phis {
			for k := rng.IntN(3); k > 0; k-- {
				cls := int8(rng.IntN(2))
				var args []VReg
				for _, p := range b.Preds {
					from := dominating(i)
					if p < i {
						from = append(dominating(p), defs[p]...)
					}
					args = append(args, pick(from, cls))
				}
				if slices.Contains(args, None) {
					continue
				}
				d := newV(cls)
				b.Phis = append(b.Phis, Phi{Def: d, Args: args})
				defs[i] = append(defs[i], d)
			}
		}
		n := 1 + rng.IntN(12)
		if i >= nb0 {
			// A block of an edge: few instructions, or none.
			n = rng.IntN(3)
		}
		for ; n > 0; n-- {
			var in Inst
			in.Hint = None
			from := append(dominating(i), defs[i]...)
			for u := rng.IntN(3); u > 0; u-- {
				if v := pick(from, int8(rng.IntN(2))); v != None {
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
			defs[i] = append(defs[i], in.Defs...)
			b.Insts = append(b.Insts, in)
		}
	}
	return f
}

// sameResult reports whether two results are the same: locations,
// reloads and moves.
func sameResult(f *Func, a, b *Result) bool {
	if !slices.Equal(a.Reg, b.Reg) || !slices.Equal(a.Slot, b.Slot) || a.NumSlots != b.NumSlots || !slices.Equal(a.flags, b.flags) {
		return false
	}
	for i := range f.Blocks {
		if !slices.Equal(a.Entry(i), b.Entry(i)) {
			return false
		}
		for j := range f.Blocks[i].Insts {
			if !slices.Equal(a.Across(i, j), b.Across(i, j)) || !slices.Equal(a.Moves(i, j), b.Moves(i, j)) {
				return false
			}
		}
		for k := range f.Blocks[i].Succs {
			if !slices.Equal(a.Edge(i, k), b.Edge(i, k)) {
				return false
			}
		}
	}
	return true
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
	dataflow := *cfg
	dataflow.dataflow = true
	for iter := 0; iter < 5000; iter++ {
		f := randomFunc(rng)
		a, err := Allocate(f, cfg)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Allocate(f, &dataflow)
		if err != nil {
			t.Fatal(err)
		}
		if !sameResult(f, a, b) {
			t.Fatalf("iteration %d: the two liveness methods allocate differently", iter)
		}
	}
}

// TestCheckCatchesMutations checks that checkResult finds allocations
// made wrong on purpose: holes that leave out a position where their web
// meets the one they are punched for, a split value's load into its
// register dropped, where a block begins said wrongly, and a reload after
// a call dropped.
func TestCheckCatchesMutations(t *testing.T) {
	cfg := &Config{Classes: [][]int{{3, 1, 0, 2}, {0, 1, 2}}, NumSlots: 1000}
	mutations := []struct {
		name string
		// mutate changes res, and reports whether it found what to
		// change.
		mutate func(f *Func, res *Result) bool
	}{
		{"holes too small", func(f *Func, res *Result) bool { return res.Splits() }},
		{"a load dropped", func(f *Func, res *Result) bool {
			for i := range res.gap.mv {
				if m := &res.gap.mv[i]; m.Copy && m.Reg >= 0 {
					m.Copy = false
					return true
				}
			}
			return false
		}},
		{"an entry wrong", func(f *Func, res *Result) bool {
			for i := range res.entryMv {
				if m := &res.entryMv[i]; m.Reg < 0 {
					m.Reg = res.Reg[m.V]
				} else {
					m.Reg = -1
				}
				return true
			}
			return false
		}},
		{"a reload dropped", func(f *Func, res *Result) bool {
			if len(res.acrossV) == 0 {
				return false
			}
			res.acrossV = res.acrossV[1:]
			for k := range res.acrossOff {
				res.acrossOff[k] = max(res.acrossOff[k]-1, 0)
			}
			return true
		}},
	}
	for _, mu := range mutations {
		rng := rand.New(rand.NewPCG(23, 29))
		cfg.mutateHoles = mu.name == "holes too small"
		found, tried := 0, 0
		for iter := 0; iter < 3000 && found < 20; iter++ {
			f := randomFunc(rng)
			res, err := Allocate(f, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !mu.mutate(f, res) {
				continue
			}
			tried++
			if checkResult(f, cfg, res) != nil {
				found++
			}
		}
		t.Logf("%s: found in %d of %d functions", mu.name, found, tried)
		if found == 0 {
			t.Errorf("%s: never found, in %d functions", mu.name, tried)
		}
	}
}

// TestWeightOfStoredValues has a loop make two values before a call, one
// register between them: x, which the loop carries and reads, and y, which
// it makes anew each turn and only its exit reads. A value that lives
// across a call is stored where it is made, in a register or not, so y's
// register would save it its one read after the loop, less than its reload
// after the call costs: it lives in its slot, and x keeps the register.
func TestWeightOfStoredValues(t *testing.T) {
	// v0: x's first value; v1 = phi(v0, v2); v2 = v1 + 1; v3 made anew;
	// a call; the loop's test reads v2; its exit v3 and v2.
	f := &Func{
		Class:     []int8{0, 0, 0, 0},
		FixedSlot: []int32{-1, -1, -1, -1},
		Blocks: []Block{
			{Succs: []int{1}, Insts: []Inst{{Defs: []VReg{0}, Hint: None}, {Hint: None}}},
			{Preds: []int{0, 2}, Succs: []int{2, 3}, Depth: 1, Phis: []Phi{{Def: 1, Args: []VReg{0, 2}}},
				Insts: []Inst{{Uses: []VReg{1}, Defs: []VReg{2}, Hint: 1}, {Defs: []VReg{3}, Hint: None},
					{Call: true, Hint: None}, {Uses: []VReg{2}, Hint: None}}},
			{Preds: []int{1}, Succs: []int{1}, Depth: 1, Insts: []Inst{{Hint: None}}},
			{Preds: []int{1}, Insts: []Inst{{Uses: []VReg{3, 2}, Hint: None}}},
		},
	}
	for _, cfg := range []*Config{
		{Classes: [][]int{{0}}, NumSlots: 8},
		{Classes: [][]int{{0}}, NumSlots: 8, Displace: true},
		{Classes: [][]int{{0}}, NumSlots: 8, NoSplit: true},
	} {
		res, err := Allocate(f, cfg)
		if err != nil {
			t.Fatal(err)
		}
		checkAllocation(t, 0, f, cfg, res)
		if res.Reg[3] >= 0 {
			t.Errorf("%+v: y in register %d\n%s", *cfg, res.Reg[3], dump(f, res))
		}
		for _, v := range []VReg{0, 1, 2} {
			if res.Reg[v] < 0 || res.Split(v) {
				t.Errorf("%+v: x's v%d in register %d, split %v\n%s", *cfg, v, res.Reg[v], res.Split(v), dump(f, res))
			}
		}
	}
}
