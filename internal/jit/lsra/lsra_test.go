package lsra

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
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
func randomFunc(rng *rand.Rand) *Func { return randomFuncOf(rng, 8, 12) }

// randomFuncOf is randomFunc's function of up to maxBlocks blocks before
// those of its edges, each of up to maxInsts instructions.
func randomFuncOf(rng *rand.Rand, maxBlocks, maxInsts int) *Func {
	nb := 1 + rng.IntN(maxBlocks)
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
	f := &Func{Blocks: make([]Block, nb), Clobbers: randomClobbers}
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
		f.Blocks[0].Insts = append(f.Blocks[0].Insts, mk(nil, []VReg{v}, None))
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
		n := 1 + rng.IntN(maxInsts)
		if i >= nb0 {
			// A block of an edge: few instructions, or none.
			n = rng.IntN(3)
		}
		for ; n > 0; n-- {
			var uses, defs1, temps []VReg
			hint := None
			call, clob := false, int8(0)
			from := append(dominating(i), defs[i]...)
			for u := rng.IntN(3); u > 0; u-- {
				if v := pick(from, int8(rng.IntN(2))); v != None {
					uses = append(uses, v)
				}
			}
			switch rng.IntN(10) {
			case 0:
				call = true
			case 1:
				// Each of the twelve masks of randomClobbers.
				clob = int8(3*rng.IntN(4) + rng.IntN(3) + 1)
			}
			if !call && rng.IntN(4) == 0 {
				for k := 1 + rng.IntN(2); k > 0; k-- {
					temps = append(temps, newV(int8(rng.IntN(2))))
				}
			}
			for d := rng.IntN(3); d > 0; d-- {
				defs1 = append(defs1, newV(int8(rng.IntN(2))))
			}
			if len(uses) > 0 && len(defs1) > 0 {
				hint = uses[0]
			}
			defs[i] = append(defs[i], defs1...)
			in := NewInst(nil, uses, defs1, temps, hint)
			in.Call, in.Clobber = call, clob
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
			{Succs: []int{1}, Insts: []Inst{mk(nil, []VReg{0}, None), mk(nil, []VReg{3}, None)}},
			{Preds: []int{0, 2}, Succs: []int{2, 3}, Depth: 1, Phis: []Phi{{Def: 1, Args: []VReg{0, 2}}},
				Insts: []Inst{mk([]VReg{1, 3}, []VReg{2}, 1), mk([]VReg{2}, nil, None)}},
			{Preds: []int{1}, Succs: []int{1}, Depth: 1, Insts: []Inst{mk(nil, nil, None)}},
			{Preds: []int{1}, Insts: []Inst{mk([]VReg{2, 3}, nil, None)}},
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
// many against many, against the positions the lists cover; and the
// coalescing's mergeApart, and apartAt and unionFew, which test whether
// two lists meet and merge them where they do not, against the two. Half
// the second lists lie in the first's gaps, which some fill to the brim.
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
	// apart is a list of n ranges in the gaps of cover, some of them
	// whole gaps.
	apart := func(n int, cover []bool) ([]rng, []bool) {
		c := make([]bool, len(cover))
		for range n {
			p := rnd.IntN(len(cover))
			if cover[p] {
				continue
			}
			lo, hi := p, p+1
			if rnd.IntN(3) == 0 {
				for lo > 0 && !cover[lo-1] {
					lo--
				}
				for hi < len(cover) && !cover[hi] {
					hi++
				}
			} else {
				for k := rnd.IntN(6); k > 0 && hi < len(cover) && !cover[hi]; k-- {
					hi++
				}
			}
			for q := lo; q < hi; q++ {
				c[q] = true
			}
		}
		var rs []rng
		for p := 0; p < len(c); p++ {
			if !c[p] {
				continue
			}
			from := p
			for p < len(c) && c[p] {
				p++
			}
			rs = append(rs, rng{int32(from), int32(p)})
		}
		return rs, c
	}
	var ar arena[rng]
	var merged []rng
	var at []int32
	for iter := range 40000 {
		a, ca := list(1 + rnd.IntN(60))
		b, cb := list(1 + rnd.IntN(6))
		if iter%4 >= 2 {
			b, cb = apart(1+rnd.IntN(6), ca)
		}
		if len(b) == 0 {
			continue
		}
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
		var ok bool
		if merged, ok = mergeApart(merged, a, b); ok == meet || ok && !slices.Equal(merged, want) {
			t.Fatalf("mergeApart(%v, %v) = %v, %v; want %v, %v", a, b, merged, ok, want, !meet)
		}
		long, short := a, b
		if len(long) < len(short) {
			long, short = short, long
		}
		if at, ok = apartAt(at, long, short); ok == meet {
			t.Fatalf("apartAt(%v, %v) = %v, want %v", long, short, ok, !meet)
		}
		if ok {
			if u := unionFew(&ar, slices.Clone(long), short, at); !slices.Equal(u, want) {
				t.Fatalf("unionFew(%v, %v, %v) = %v, want %v", long, short, at, u, want)
			}
		}
	}
}

// TestRangesThroughBlocks has a value live through a chain of blocks,
// which has one range, from its definition to its use; and a value a
// branch's two arms are laid out after, live into the second arm and not
// the first, which has two, one ending where its block does and one
// beginning where the second arm does.
func TestRangesThroughBlocks(t *testing.T) {
	const nb = 12
	chain := &Func{Class: []int8{0, 0}, FixedSlot: []int32{-1, -1}, Blocks: make([]Block, nb)}
	for i := range chain.Blocks {
		b := &chain.Blocks[i]
		if i > 0 {
			b.Preds = []int{i - 1}
		}
		if i < nb-1 {
			b.Succs = []int{i + 1}
		}
		b.Insts = []Inst{mk(nil, nil, None)}
	}
	chain.Blocks[0].Insts = []Inst{mk(nil, []VReg{0}, None), mk(nil, []VReg{1}, None)}
	chain.Blocks[nb/2].Insts = []Inst{mk([]VReg{1}, nil, None)}
	chain.Blocks[nb-1].Insts = []Inst{mk([]VReg{0}, nil, None)}
	// v0 made, then the arms, each to the join; the second arm reads it.
	arms := &Func{Class: []int8{0}, FixedSlot: []int32{-1}, Blocks: []Block{
		{Succs: []int{1, 2}, Insts: []Inst{mk(nil, []VReg{0}, None), mk(nil, nil, None)}},
		{Preds: []int{0}, Succs: []int{3}, Insts: []Inst{mk(nil, nil, None)}},
		{Preds: []int{0}, Succs: []int{3}, Insts: []Inst{mk([]VReg{0}, nil, None)}},
		{Preds: []int{1, 2}, Insts: []Inst{mk(nil, nil, None)}},
	}}
	var al Allocator
	cfg := &Config{Classes: [][]int{{0, 1, 2}}, NumSlots: 8}
	for _, tc := range []struct {
		f    *Func
		v    VReg
		want func(starts, ends []int32) []rng
	}{
		{chain, 0, func(s, e []int32) []rng { return []rng{{s[0] + 3, s[nb-1] + 3}} }},
		{chain, 1, func(s, e []int32) []rng { return []rng{{s[0] + 5, s[nb/2] + 3}} }},
		{arms, 0, func(s, e []int32) []rng { return []rng{{s[0] + 3, e[0] + 1}, {s[2], s[2] + 3}} }},
	} {
		res, err := al.Allocate(tc.f, cfg)
		if err != nil {
			t.Fatal(err)
		}
		checkAllocation(t, 0, tc.f, cfg, res)
		if got, want := al.ranges[tc.v], tc.want(al.starts, al.ends); !slices.Equal(got, want) {
			t.Errorf("v%d's ranges %v, want %v", tc.v, got, want)
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

// TestSlotScanMethods allocates random functions, small ones and ones of
// many blocks, whose webs need many slots, and manySlotsFunc's, with the
// slots' scan finding each web's first slot by testing each in turn, by
// the slots' keys past slotTreeMin slots, and by the keys from the first,
// which must give the same allocation.
func TestSlotScanMethods(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 37))
	classes := [][][]int{{{3, 1, 0, 2}, {0, 1, 2}}, {{0, 1, 2, 3}, {2, 1, 0}}}
	var al, lin, tree Allocator
	many := 0
	defer func() {
		t.Logf("%d of 3000 large functions need %d slots or more", many, slotTreeMin)
		if many < 300 {
			t.Errorf("only %d of 3000 large functions need %d slots or more", many, slotTreeMin)
		}
	}()
	for iter := range 6000 {
		var f *Func
		if iter%2 == 0 {
			f = randomFunc(rng)
		} else {
			f = randomFuncOf(rng, 24, 40)
		}
		cfg := Config{Classes: classes[iter%len(classes)], NumSlots: 1 << 16}
		switch iter / len(classes) % 4 {
		case 1:
			cfg.Displace = true
		case 2:
			cfg.StressSplit = true
		case 3:
			cfg.NoSplit = true
		}
		res, err := al.Allocate(f, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		if iter%50 == 0 {
			checkAllocation(t, iter, f, &cfg, res)
		}
		if res.NumSlots >= slotTreeMin {
			many++
		}
		lcfg, tcfg := cfg, cfg
		lcfg.slots, tcfg.slots = slotsLinear, slotsTree
		a, err := lin.Allocate(f, &lcfg)
		if err != nil {
			t.Fatal(err)
		}
		if !sameResult(f, res, a) {
			t.Fatalf("iteration %d: the scans by the keys and of each slot in turn allocate differently", iter)
		}
		b, err := tree.Allocate(f, &tcfg)
		if err != nil {
			t.Fatal(err)
		}
		if !sameResult(f, a, b) {
			t.Fatalf("iteration %d: the scans by the keys from the first slot and of each in turn allocate differently", iter)
		}
	}
	for _, wide := range []bool{false, true} {
		f := manySlotsFunc(500, 500, wide)
		cfg := Config{Classes: [][]int{{0, 1, 2, 3}}, NumSlots: 1 << 20}
		lcfg := cfg
		lcfg.slots = slotsLinear
		a, err := al.Allocate(f, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		b, err := lin.Allocate(f, &lcfg)
		if err != nil {
			t.Fatal(err)
		}
		if !sameResult(f, a, b) {
			t.Fatalf("wide %v: the scans by the keys and of each slot in turn allocate differently", wide)
		}
	}
}

// TestOneByOneMethods allocates random functions, small ones and ones of
// many blocks, half of them with a call at a third of their instructions
// and a clobber of one of fewer kinds at a third of the rest, and
// manyCallsFunc's, with what a web lives across and its holes found from
// its uses, its ranges and the clobbers' sums, a hole cut out of its
// ranges, and by visiting each, one by one, the hole subtracted from its
// ranges anew (Config.oneByOne), which must give the same allocation.
func TestOneByOneMethods(t *testing.T) {
	rng := rand.New(rand.NewPCG(41, 43))
	classes := [][][]int{{{3, 1, 0, 2}, {0, 1, 2}}, {{0, 1}, {2, 1, 0}}}
	var al, each Allocator
	split := 0
	for iter := range 20000 {
		var f *Func
		if iter%2 == 0 {
			f = randomFunc(rng)
		} else {
			f = randomFuncOf(rng, 24, 40)
		}
		if iter/2%2 == 0 {
			nk := 1 + rng.IntN(len(randomClobbers))
			f.Clobbers = randomClobbers[:nk]
			for i := range f.Blocks {
				for j := range f.Blocks[i].Insts {
					in := &f.Blocks[i].Insts[j]
					switch {
					case in.Call:
					case len(in.Temps()) == 0 && rng.IntN(3) == 0:
						in.Call, in.Clobber = true, 0
					case in.Clobber != 0 || rng.IntN(3) == 0:
						in.Clobber = int8(1 + rng.IntN(nk))
					}
				}
			}
		}
		cfg := Config{Classes: classes[iter%len(classes)], NumSlots: 1 << 16}
		switch iter / 4 % 3 {
		case 1:
			cfg.Displace = true
		case 2:
			cfg.StressSplit = true
		}
		res, err := al.Allocate(f, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		if iter%100 == 0 {
			checkAllocation(t, iter, f, &cfg, res)
		}
		if res.Splits() {
			split++
		}
		ecfg := cfg
		ecfg.oneByOne = true
		b, err := each.Allocate(f, &ecfg)
		if err != nil {
			t.Fatal(err)
		}
		if !sameResult(f, res, b) {
			t.Fatalf("iteration %d: the methods by the uses and one by one allocate differently\n%s\n%s", iter, dump(f, res), dump(f, b))
		}
	}
	t.Logf("%d of 20000 functions split", split)
	for _, c := range []struct {
		long, calls, every int
		blocks             bool
	}{{300, 300, 0, false}, {300, 300, 0, true}, {6, 600, 2, false}, {6, 600, 3, true}} {
		f := manyCallsFunc(c.long, c.calls, c.every, c.blocks)
		cfg := Config{Classes: [][]int{{0, 1, 2, 3}}, NumSlots: 1 << 20}
		ecfg := cfg
		ecfg.oneByOne = true
		a, err := al.Allocate(f, &cfg)
		if err != nil {
			t.Fatal(err)
		}
		b, err := each.Allocate(f, &ecfg)
		if err != nil {
			t.Fatal(err)
		}
		if !sameResult(f, a, b) {
			t.Fatalf("%+v: the methods by the uses and one by one allocate differently", c)
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
			{Succs: []int{1}, Insts: []Inst{mk(nil, []VReg{0}, None), mk(nil, nil, None)}},
			{Preds: []int{0, 2}, Succs: []int{2, 3}, Depth: 1, Phis: []Phi{{Def: 1, Args: []VReg{0, 2}}},
				Insts: []Inst{mk([]VReg{1}, []VReg{2}, 1), mk(nil, []VReg{3}, None),
					{Call: true, Hint: None}, mk([]VReg{2}, nil, None)}},
			{Preds: []int{1}, Succs: []int{1}, Depth: 1, Insts: []Inst{mk(nil, nil, None)}},
			{Preds: []int{1}, Insts: []Inst{mk([]VReg{3, 2}, nil, None)}},
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

// TestCallHoles has values that live across calls give their register up
// over the calls, where holes there cost less than reloading them after
// each call: x is read, then three calls, then read twice -- reloaded after
// each call its reloads would weigh as much as its reads, and it lived in
// its slot -- and y, made before a loop that calls, is read twice after
// it, where a reload after the call weighs eight times a read. Each is in
// its register where it is read, loaded once from its slot after the
// calls, and no call reloads it.
func TestCallHoles(t *testing.T) {
	call := Inst{Call: true, Hint: None}
	f := &Func{
		Class:     []int8{0, 0},
		FixedSlot: []int32{-1, -1},
		Blocks: []Block{
			{Succs: []int{1}, Insts: []Inst{mk(nil, []VReg{0}, None), mk([]VReg{0}, nil, None), call, call, call,
				mk([]VReg{0}, nil, None), mk([]VReg{0}, nil, None), mk(nil, []VReg{1}, None)}},
			{Preds: []int{0, 2}, Succs: []int{2, 3}, Depth: 1, Insts: []Inst{call, mk(nil, nil, None)}},
			{Preds: []int{1}, Succs: []int{1}, Depth: 1, Insts: []Inst{mk(nil, nil, None)}},
			{Preds: []int{1}, Insts: []Inst{mk([]VReg{1}, nil, None), mk([]VReg{1}, nil, None)}},
		},
	}
	for _, cfg := range []*Config{
		{Classes: [][]int{{0, 1}}, NumSlots: 8},
		{Classes: [][]int{{0, 1}}, NumSlots: 8, Displace: true},
	} {
		res, err := Allocate(f, cfg)
		if err != nil {
			t.Fatal(err)
		}
		checkAllocation(t, 0, f, cfg, res)
		for _, v := range []VReg{0, 1} {
			if res.Reg[v] < 0 || !res.Split(v) {
				t.Errorf("%+v: v%d in register %d, split %v\n%s", *cfg, v, res.Reg[v], res.Split(v), dump(f, res))
			}
		}
		for _, c := range [][2]int{{0, 2}, {0, 3}, {0, 4}, {1, 0}} {
			if a := res.Across(c[0], c[1]); len(a) > 0 {
				t.Errorf("%+v: the call at b%d.%d reloads %v\n%s", *cfg, c[0], c[1], a, dump(f, res))
			}
		}
	}
	// Without splitting, both live in their slots, as before holes.
	cfg := &Config{Classes: [][]int{{0, 1}}, NumSlots: 8, NoSplit: true}
	res, err := Allocate(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	checkAllocation(t, 0, f, cfg, res)
	if res.Reg[0] >= 0 || res.Reg[1] >= 0 {
		t.Errorf("without splitting: v0 in %d, v1 in %d, the loop's call reloads %v\n%s", res.Reg[0], res.Reg[1], res.Across(1, 0), dump(f, res))
	}
}

// mk is an instruction of uses and defs with hint.
func mk(uses, defs []VReg, hint VReg) Inst { return NewInst(nil, uses, defs, nil, hint) }

// randomClobbers are the masks randomFunc's instructions clobber: of the
// first class one of its first four registers, of the second one of its
// first three.
var randomClobbers = func() (m [][]uint64) {
	for a := range 4 {
		for b := range 3 {
			m = append(m, []uint64{1 << uint(a), 1 << uint(b)})
		}
	}
	return m
}()

// manySlotsFunc is a function whose webs need thousands of slots: long
// values, made first and all read at the end, and short ones, made eight
// at a time and read, one group after another. With four registers, the
// long ones, of two uses over the whole function, live in slots, each its
// own, and so do half of each group, in the first slots past the long
// ones'. A wide one, of two slots, is every fourth where wide is set.
func manySlotsFunc(long, short int, wide bool) *Func {
	f := &Func{}
	newV := func() VReg {
		f.Class = append(f.Class, 0)
		f.FixedSlot = append(f.FixedSlot, -1)
		w := int8(1)
		if wide && len(f.Class)%4 == 0 {
			w = 2
		}
		f.Width = append(f.Width, w)
		return VReg(len(f.Class) - 1)
	}
	var insts []Inst
	var longs []VReg
	for range long {
		v := newV()
		longs = append(longs, v)
		insts = append(insts, mk(nil, []VReg{v}, None))
	}
	for range short / 8 {
		var group [8]VReg
		for k := range group {
			group[k] = newV()
			insts = append(insts, mk(nil, []VReg{group[k]}, None))
		}
		for k := len(group) - 1; k >= 0; k-- {
			insts = append(insts, mk([]VReg{group[k]}, nil, None))
		}
	}
	for i := len(longs) - 1; i >= 0; i-- {
		insts = append(insts, mk([]VReg{longs[i]}, nil, None))
	}
	f.Blocks = []Block{{Insts: insts}}
	return f
}

// BenchmarkManySlots times the allocation of manySlotsFunc's functions,
// and its slots' phase alone (slots-ns/op), which gives each web needing
// a slot the first one free.
func BenchmarkManySlots(b *testing.B) {
	for _, bc := range []struct {
		name        string
		long, short int
		wide        bool
	}{
		{"1000+1000", 1000, 1000, false},
		{"2000+2000", 2000, 2000, false},
		{"4000+4000", 4000, 4000, false},
		{"4000+4000wide", 4000, 4000, true},
	} {
		b.Run(bc.name, func(b *testing.B) {
			f := manySlotsFunc(bc.long, bc.short, bc.wide)
			var al Allocator
			var mark time.Time
			var slots time.Duration
			cfg := &Config{Classes: [][]int{{0, 1, 2, 3}}, NumSlots: 1 << 20, Lap: func(p Phase) {
				now := time.Now()
				if p == PhaseSlots {
					slots += now.Sub(mark)
				}
				mark = now
			}}
			for b.Loop() {
				mark = time.Now()
				if _, err := al.Allocate(f, cfg); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(slots.Nanoseconds())/float64(b.N), "slots-ns/op")
		})
	}
}

// manyCallsFunc is a function of long values live across many calls: the
// values made first, then the calls, then each value read twice, the
// last made first. Each lives across every call, whose reloads weigh more
// than its reads: it gives its register up over the calls, in a hole from
// its definition to its first read. Where every is not 0, each value is
// read after every that many calls too, which weigh more than the load
// after them: a hole over each run of them. Where blocks is set, each
// call is a block of its own, one after another.
func manyCallsFunc(long, calls, every int, blocks bool) *Func {
	f := &Func{}
	var longs []VReg
	var insts []Inst
	for range long {
		f.Class = append(f.Class, 0)
		f.FixedSlot = append(f.FixedSlot, -1)
		longs = append(longs, VReg(len(f.Class)-1))
		insts = append(insts, mk(nil, longs[len(longs)-1:], None))
	}
	call := func() Inst { return Inst{Call: true, Hint: None} }
	if blocks {
		f.Blocks = append(f.Blocks, Block{Insts: insts})
		insts = nil
	}
	for k := range calls {
		insts = append(insts, call())
		if every > 0 && (k+1)%every == 0 {
			for i := range longs {
				insts = append(insts, mk(longs[i:i+1], nil, None))
			}
		}
		if blocks {
			f.Blocks = append(f.Blocks, Block{Insts: insts})
			insts = nil
		}
	}
	for i := len(longs) - 1; i >= 0; i-- {
		insts = append(insts, mk(longs[i:i+1], nil, None), mk(longs[i:i+1], nil, None))
	}
	f.Blocks = append(f.Blocks, Block{Insts: insts})
	for i := range f.Blocks {
		if i > 0 {
			f.Blocks[i].Preds = []int{i - 1}
		}
		if i+1 < len(f.Blocks) {
			f.Blocks[i].Succs = []int{i + 1}
		}
	}
	return f
}

// BenchmarkManyCalls times the allocation of manyCallsFunc's functions,
// values by calls, and the scan's phase (scan-ns/op), which punches each
// value's holes over the calls, and the slots' (slots-ns/op), which finds
// each call's reloads.
func BenchmarkManyCalls(b *testing.B) {
	for _, bc := range []struct {
		name               string
		long, calls, every int
		blocks             bool
	}{
		{"1000x1000", 1000, 1000, 0, false},
		{"2000x2000", 2000, 2000, 0, false},
		{"4000x4000", 4000, 4000, 0, false},
		{"1000x4000", 1000, 4000, 0, false},
		{"4000x1000", 4000, 1000, 0, false},
		{"1000x1000blocks", 1000, 1000, 0, true},
		{"2000x2000blocks", 2000, 2000, 0, true},
		{"4000x4000blocks", 4000, 4000, 0, true},
		{"4x4000every2", 4, 4000, 2, false},
		{"4x8000every2", 4, 8000, 2, false},
		{"4x16000every2", 4, 16000, 2, false},
	} {
		b.Run(bc.name, func(b *testing.B) {
			f := manyCallsFunc(bc.long, bc.calls, bc.every, bc.blocks)
			var al Allocator
			var mark time.Time
			var phases [NumPhases]time.Duration
			cfg := &Config{Classes: [][]int{{0, 1, 2, 3}}, NumSlots: 1 << 20, Lap: func(p Phase) {
				now := time.Now()
				phases[p] += now.Sub(mark)
				mark = now
			}}
			for b.Loop() {
				mark = time.Now()
				if _, err := al.Allocate(f, cfg); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(phases[PhaseScan].Nanoseconds())/float64(b.N), "scan-ns/op")
			b.ReportMetric(float64(phases[PhaseSlots].Nanoseconds())/float64(b.N), "slots-ns/op")
		})
	}
}

// TestManyCalls allocates manyCallsFunc's functions and checks them: no
// value is reloaded after a call.
func TestManyCalls(t *testing.T) {
	for _, c := range []struct {
		long, calls, every int
		blocks             bool
	}{{200, 200, 0, false}, {200, 200, 0, true}, {4, 400, 2, false}, {4, 400, 2, true}} {
		f := manyCallsFunc(c.long, c.calls, c.every, c.blocks)
		cfg := &Config{Classes: [][]int{{0, 1, 2, 3}}, NumSlots: 1 << 20}
		res, err := Allocate(f, cfg)
		if err != nil {
			t.Fatal(err)
		}
		checkAllocation(t, 0, f, cfg, res)
		if len(res.acrossV) != 0 {
			t.Errorf("%+v: %d reloads after calls", c, len(res.acrossV))
		}
	}
}

// manyPredsFunc is a function one of whose blocks n others branch to. Of
// kind predsSwitch, the entry branches to n blocks, each defining and
// reading a value and going to a block that reads one the entry defines (a
// switch's arms meeting again); of predsPhi, each arm's value is read by a
// phi of the block they meet in, instead; of predsFail, a chain of n blocks
// each branches to the next and to one that fails, which reads the
// entry's value.
func manyPredsFunc(n, kind int) *Func {
	f := &Func{}
	reg := func() VReg {
		f.Class = append(f.Class, 0)
		f.FixedSlot = append(f.FixedSlot, -1)
		return VReg(len(f.Class) - 1)
	}
	x := reg()
	f.Blocks = append(f.Blocks, Block{Insts: []Inst{mk(nil, []VReg{x}, None)}})
	last := n + 1
	if kind == predsFail {
		f.Blocks[0].Succs = []int{1}
		var preds []int
		for k := 1; k <= n; k++ {
			v := reg()
			next := k + 1
			if k == n {
				next = last + 1
			}
			preds = append(preds, k)
			f.Blocks = append(f.Blocks, Block{Preds: []int{k - 1}, Succs: []int{next, last}, Insts: []Inst{mk([]VReg{x}, []VReg{v}, None), mk([]VReg{v}, nil, None)}})
		}
		f.Blocks = append(f.Blocks, Block{Preds: preds, Insts: []Inst{mk([]VReg{x}, nil, None)}})
		f.Blocks = append(f.Blocks, Block{Preds: []int{n}, Insts: []Inst{mk([]VReg{x}, nil, None)}})
		return f
	}
	var args []VReg
	var preds []int
	for k := 1; k <= n; k++ {
		v := reg()
		args = append(args, v)
		preds = append(preds, k)
		f.Blocks[0].Succs = append(f.Blocks[0].Succs, k)
		insts := []Inst{mk([]VReg{x}, []VReg{v}, None)}
		if kind == predsSwitch {
			insts = append(insts, mk([]VReg{v}, nil, None))
		}
		f.Blocks = append(f.Blocks, Block{Preds: []int{0}, Succs: []int{last}, Insts: insts})
	}
	join := Block{Preds: preds, Insts: []Inst{mk([]VReg{x}, nil, None)}}
	if kind == predsPhi {
		p := reg()
		join.Phis = []Phi{{Def: p, Args: args}}
		join.Insts = []Inst{mk([]VReg{p, x}, nil, None)}
	}
	f.Blocks = append(f.Blocks, join)
	return f
}

// manyPredsFunc's kinds.
const (
	predsSwitch = iota
	predsPhi
	predsFail
)

// BenchmarkManyPreds times the allocation of manyPredsFunc's functions of
// kinds predsSwitch and predsFail, a block of n predecessors, and the
// liveness phase (liveness-ns/op), which finds each edge's index among its
// successor's predecessors. (Of predsPhi the liveness sets, n values live
// across the n arms' ends, grow with n squared whatever else does.)
func BenchmarkManyPreds(b *testing.B) {
	for _, kind := range []int{predsSwitch, predsFail} {
		for _, n := range []int{1000, 2000, 4000, 8000, 16000} {
			name := fmt.Sprintf("switch%d", n)
			if kind == predsFail {
				name = fmt.Sprintf("fail%d", n)
			}
			b.Run(name, func(b *testing.B) {
				f := manyPredsFunc(n, kind)
				var al Allocator
				var mark time.Time
				var phases [NumPhases]time.Duration
				cfg := &Config{Classes: [][]int{{0, 1, 2, 3}}, NumSlots: 1 << 20, Lap: func(p Phase) {
					now := time.Now()
					phases[p] += now.Sub(mark)
					mark = now
				}}
				for b.Loop() {
					mark = time.Now()
					if _, err := al.Allocate(f, cfg); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(phases[PhaseLiveness].Nanoseconds())/float64(b.N), "liveness-ns/op")
			})
		}
	}
}

// TestManyPreds allocates manyPredsFunc's functions and checks them.
func TestManyPreds(t *testing.T) {
	for kind := range 3 {
		for _, n := range []int{1, 2, 40, 300} {
			f := manyPredsFunc(n, kind)
			cfg := &Config{Classes: [][]int{{0, 1, 2, 3}}, NumSlots: 1 << 20}
			res, err := Allocate(f, cfg)
			if err != nil {
				t.Fatal(err)
			}
			checkAllocation(t, 0, f, cfg, res)
		}
	}
}

// TestManySlots allocates manySlotsFunc's functions and checks them: each
// long value in a slot of its own, the short ones sharing.
func TestManySlots(t *testing.T) {
	for _, wide := range []bool{false, true} {
		f := manySlotsFunc(300, 300, wide)
		cfg := &Config{Classes: [][]int{{0, 1, 2, 3}}, NumSlots: 1 << 20}
		res, err := Allocate(f, cfg)
		if err != nil {
			t.Fatal(err)
		}
		checkAllocation(t, 0, f, cfg, res)
		if res.NumSlots < 300 || res.NumSlots > 300*2+16 {
			t.Errorf("wide %v: %d slots", wide, res.NumSlots)
		}
	}
}

// TestWalkMethods allocates random functions, small ones and ones of many
// blocks, half with calls and clobbers as TestOneByOneMethods makes them,
// a fifth with their blocks laid out in another order and a fifth
// defining a register twice, a loop whose register is used before its
// definition, and manySlotsFunc's and manyCallsFunc's, by the walks that
// do more at once -- liveness's one walk that defines, marks and lists
// the uses its paths start from and the instructions with hints, the
// coalescing's merge that tests as it goes and its searches that put the
// few ranges into the many where they found them, the walk of the blocks
// that weighs the phis and gives the hints, the scan's entries that say
// where each web on its lists may change, a hole's cost from the blocks
// where its web lives, a web's events merged from its members' runs with
// those in order one after another joined first -- and with each as it
// was (Config.markPass, twoPass, hintPass, plainLists, blockByBlock,
// mergeAll), which must give the same allocation.
func TestWalkMethods(t *testing.T) {
	rng := rand.New(rand.NewPCG(47, 53))
	// More registers than values live at once, too: the hints choose.
	classes := [][][]int{{{3, 1, 0, 2}, {0, 1, 2}}, {{0, 1}, {2, 1, 0}}, {{7, 6, 5, 4, 3, 2, 1, 0}, {5, 4, 3, 2, 1, 0}}}
	olds := []struct {
		name string
		set  func(*Config)
	}{
		{"markPass", func(c *Config) { c.markPass = true }},
		{"twoPass", func(c *Config) { c.twoPass = true }},
		{"plainLists", func(c *Config) { c.plainLists = true }},
		{"hintPass", func(c *Config) { c.hintPass = true }},
		{"blockByBlock", func(c *Config) { c.blockByBlock = true }},
		{"mergeAll", func(c *Config) { c.mergeAll = true }},
		{"regByReg", func(c *Config) { c.regByReg = true }},
		{"phiWalks", func(c *Config) { c.phiWalks = true }},
		{"searchPreds", func(c *Config) { c.searchPreds = true }},
	}
	var al, old Allocator
	twice := 0
	// The webs' hints as the webs' phase leaves them, by each web's first
	// member: their order chooses among the registers free.
	hints := func(al *Allocator, out *[]VReg) func(Phase) {
		return func(p Phase) {
			if p != PhaseWebs {
				return
			}
			*out = (*out)[:0]
			for _, w := range al.webs {
				if w == nil {
					continue
				}
				*out = append(*out, w.members[0], VReg(len(w.hints)))
				for _, h := range w.hints {
					*out = append(*out, h.members[0])
				}
			}
		}
	}
	var hintsA, hintsB []VReg
	check := func(name string, f *Func, cfg Config) {
		t.Helper()
		cfg.Lap = hints(&al, &hintsA)
		a, errA := al.Allocate(f, &cfg)
		for _, o := range olds {
			ocfg := cfg
			o.set(&ocfg)
			ocfg.Lap = hints(&old, &hintsB)
			b, errB := old.Allocate(f, &ocfg)
			if (errA == nil) != (errB == nil) {
				t.Fatalf("%s: %v by the walks, %v with %s", name, errA, errB, o.name)
			}
			if errA == nil && !sameResult(f, a, b) {
				t.Fatalf("%s: the walks and %s allocate differently\n%s\n%s", name, o.name, dump(f, a), dump(f, b))
			}
			if !slices.Equal(hintsA, hintsB) {
				t.Fatalf("%s: the walks and %s give the webs different hints", name, o.name)
			}
		}
	}
	for iter := range 8000 {
		var f *Func
		if iter%2 == 0 {
			f = randomFunc(rng)
		} else {
			f = randomFuncOf(rng, 24, 40)
		}
		if iter/2%2 == 0 {
			nk := 1 + rng.IntN(len(randomClobbers))
			f.Clobbers = randomClobbers[:nk]
			for i := range f.Blocks {
				for j := range f.Blocks[i].Insts {
					in := &f.Blocks[i].Insts[j]
					switch {
					case in.Call:
					case len(in.Temps()) == 0 && rng.IntN(3) == 0:
						in.Call, in.Clobber = true, 0
					case in.Clobber != 0 || rng.IntN(3) == 0:
						in.Clobber = int8(1 + rng.IntN(nk))
					}
				}
			}
		}
		switch iter % 5 {
		case 4:
			// A register defined twice: an instruction's definition made
			// another of its class's, of another block.
			if redefine(rng, f) {
				twice++
			}
		case 3:
			// The blocks but the entry laid out in another order: uses
			// before their definitions.
			shuffleBlocks(rng, f)
		}
		cfg := Config{Classes: classes[iter%len(classes)], NumSlots: 1 << 16}
		switch iter / 4 % 4 {
		case 1:
			cfg.Displace = true
		case 2:
			cfg.StressSplit = true
		case 3:
			cfg.NoSplit = true
		}
		check("random", f, cfg)
	}
	if twice < 500 {
		t.Fatalf("only %d functions define a register twice", twice)
	}
	// v1 used at a loop's top and defined after, once, with no phi: live
	// into the loop from the entry and around it.
	loop := &Func{
		Class:     []int8{0, 0, 0},
		FixedSlot: []int32{-1, -1, -1},
		Blocks: []Block{
			{Succs: []int{1}, Insts: []Inst{mk(nil, []VReg{0}, None)}},
			{Preds: []int{0, 1}, Succs: []int{1, 2}, Insts: []Inst{mk([]VReg{1, 0}, []VReg{2}, None), mk([]VReg{2}, []VReg{1}, None), mk([]VReg{1}, nil, None)}},
			{Preds: []int{1}, Insts: []Inst{mk([]VReg{1}, nil, None)}},
		},
	}
	for _, c := range classes {
		check("loop", loop, Config{Classes: c, NumSlots: 16})
	}
	big := Config{Classes: [][]int{{0, 1, 2, 3}}, NumSlots: 1 << 20}
	for _, wide := range []bool{false, true} {
		check("manySlots", manySlotsFunc(300, 300, wide), big)
	}
	for _, c := range []struct {
		long, calls, every int
		blocks             bool
	}{{200, 200, 0, false}, {200, 200, 0, true}, {6, 400, 2, false}, {6, 400, 3, true}} {
		check("manyCalls", manyCallsFunc(c.long, c.calls, c.every, c.blocks), big)
	}
	for kind := range 3 {
		for _, n := range []int{manyPreds, manyPreds + 1, 3 * manyPreds} {
			check("manyPreds", manyPredsFunc(n, kind), big)
		}
	}
}

// TestPredIndex finds each block's index among the predecessors of blocks
// of many, some listed more than once, by predIndex and by a search, and
// those of blocks that are not among them.
func TestPredIndex(t *testing.T) {
	rng := rand.New(rand.NewPCG(67, 71))
	var al Allocator
	for range 300 {
		nb := 2 + rng.IntN(200)
		f := &Func{Blocks: make([]Block, nb)}
		for b := range f.Blocks {
			np := rng.IntN(3 * manyPreds)
			for range np {
				f.Blocks[b].Preds = append(f.Blocks[b].Preds, rng.IntN(nb))
			}
		}
		al.f, al.predsSorted = f, false
		for s := range f.Blocks {
			ps := f.Blocks[s].Preds
			if len(ps) <= manyPreds {
				continue
			}
			for range 2 * len(ps) {
				p := rng.IntN(nb)
				if got, want := al.predIndex(s, p), slices.Index(ps, p); got != want {
					t.Fatalf("block %d among %v: %d, not %d", p, ps, got, want)
				}
			}
		}
	}
}

// redefine makes a definition of an instruction of f that of another
// register of its class and width, defined in another block, and reports
// whether it found one.
func redefine(rng *rand.Rand, f *Func) bool {
	type at struct{ b, j, k int }
	var defs []at
	for i := range f.Blocks {
		for j := range f.Blocks[i].Insts {
			in := &f.Blocks[i].Insts[j]
			for k := range in.Defs() {
				defs = append(defs, at{i, j, k})
			}
		}
	}
	for range 20 {
		if len(defs) < 2 {
			return false
		}
		x, y := defs[rng.IntN(len(defs))], defs[rng.IntN(len(defs))]
		if x.b == y.b {
			continue
		}
		dx := &f.Blocks[x.b].Insts[x.j].Regs[int(f.Blocks[x.b].Insts[x.j].NUses)+x.k]
		dy := f.Blocks[y.b].Insts[y.j].Defs()[y.k]
		if *dx == dy || f.Class[*dx] != f.Class[dy] || widthOf(f, *dx) != widthOf(f, dy) || f.FixedSlot[*dx] >= 0 || f.FixedSlot[dy] >= 0 {
			continue
		}
		*dx = dy
		return true
	}
	return false
}

// shuffleBlocks lays f's blocks but the entry out in a random order.
func shuffleBlocks(rng *rand.Rand, f *Func) {
	nb := len(f.Blocks)
	perm := make([]int, nb) // perm[old] is the block's new index
	for i := range perm {
		perm[i] = i
	}
	rng.Shuffle(nb-1, func(i, j int) { perm[i+1], perm[j+1] = perm[j+1], perm[i+1] })
	blocks := make([]Block, nb)
	for i, b := range f.Blocks {
		for k := range b.Preds {
			b.Preds[k] = perm[b.Preds[k]]
		}
		for k := range b.Succs {
			b.Succs[k] = perm[b.Succs[k]]
		}
		blocks[perm[i]] = b
	}
	f.Blocks = blocks
}

// TestJoinRuns sorts random runs, each in order, some empty, some lying
// one after another, by joinRuns and mergeRuns, which must give what a
// sort does.
func TestJoinRuns(t *testing.T) {
	rnd := rand.New(rand.NewPCG(59, 61))
	var al Allocator
	for range 20000 {
		var ev, runs []int32
		at := int32(rnd.IntN(10))
		for range 1 + rnd.IntN(12) {
			if rnd.IntN(3) != 0 {
				// The next run from where this one ends, or anywhere.
				at = int32(rnd.IntN(60))
			}
			for k := rnd.IntN(5); k > 0; k-- {
				at += int32(rnd.IntN(3))
				ev = append(ev, at)
			}
			runs = append(runs, int32(len(ev)))
		}
		want := slices.Sorted(slices.Values(ev))
		joined := joinRuns(ev, slices.Clone(runs))
		lo := int32(0)
		for _, hi := range joined {
			if !slices.IsSorted(ev[lo:hi]) {
				t.Fatalf("joinRuns(%v, %v) = %v: run %v not in order", ev, runs, joined, ev[lo:hi])
			}
			lo = hi
		}
		if got := al.mergeRuns(slices.Clone(ev), joined); !slices.Equal(got, want) {
			t.Fatalf("mergeRuns(%v, joinRuns %v) = %v, want %v", ev, joined, got, want)
		}
	}
}
