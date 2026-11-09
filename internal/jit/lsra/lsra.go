// Package lsra is a linear-scan register allocator over an abstract
// machine function: blocks of instructions that use and define virtual
// registers of classes, with fixed clobbers, calls and phis. It knows
// nothing of the source language or the target's instructions: a client
// describes its function, and lowers it with the locations the allocator
// gives.
//
// Each virtual register has one location for its whole life -- a
// register, or a spill slot -- as Poletto and Sarkar's linear scan gives
// it: its live interval is the span from its first definition to its
// last use, holes included, and when a class's registers run out the
// interval of the lowest spill weight is spilled whole. A value in a
// register that is live across a call (Inst.Call) is kept in a slot too,
// stored where it is defined and loaded again after each call it crosses
// (Result.Across); a call clobbers every register. An instruction's fixed
// clobbers (Inst.Clobber) keep the registers they name from the
// intervals live across it.
package lsra

import (
	"errors"
	"math/bits"
	"slices"
)

// VReg is a virtual register: an index into Func's per-register slices.
type VReg int32

// None is no virtual register.
const None VReg = -1

// NoClass is the class of a virtual register that is not allocated: its
// client makes its value where it is used (a constant).
const NoClass int8 = -1

// Func is a function to allocate.
type Func struct {
	// Class is each virtual register's class, an index into
	// Config.Classes, or NoClass.
	Class []int8
	// FixedSlot is the slot a virtual register arrives in -- a parameter,
	// in its frame slot -- which it keeps if it needs one, or -1. The slot
	// holds nothing else until the register is defined.
	FixedSlot []int32
	// Blocks are the function's blocks in layout order, the entry first.
	Blocks []Block
}

// Block is a basic block.
type Block struct {
	// Preds and Succs are block indices; a block with phis has an edge
	// to it from each predecessor, in order, that no other block shares
	// (no critical edges).
	Preds, Succs []int
	// Depth is the block's loop depth, which weighs its uses.
	Depth int
	// Phis are defined at the block's start, each from Args[i] on the
	// edge from Preds[i].
	Phis []Phi
	// Insts are the block's instructions: the last uses what the block's
	// branch or return reads.
	Insts []Inst
}

// Phi is a phi: its definition and an argument for each predecessor
// (None for one that is not allocated).
type Phi struct {
	Def  VReg
	Args []VReg
}

// Inst is an instruction: it reads Uses, then writes Defs.
type Inst struct {
	Uses, Defs []VReg
	// Clobber is, for each class, the registers it writes besides its
	// Defs, as bits by register number: no interval live across it may
	// be in them.
	Clobber []uint64
	// Call says it clobbers every register: the intervals live across it
	// in registers are stored at their definitions and loaded after it.
	Call bool
	// Hint is a use whose register Defs[0] would like, the two operands
	// of a two-address instruction: None for none.
	Hint VReg
}

// Config is the target's registers and slots.
type Config struct {
	// Classes are the register classes: each its allocatable registers,
	// by register number below 64, in the order they are preferred.
	Classes [][]int
	// NumSlots is the number of spill slots there are.
	NumSlots int
}

// Result is where each virtual register lives.
type Result struct {
	// Reg is each virtual register's register, or -1 for one spilled or
	// not allocated.
	Reg []int16
	// Slot is each virtual register's slot: a spilled one's home, or one
	// in a register live across a call, its copy there; else -1.
	Slot []int32
	// Across is, for each call by block and instruction index, the
	// virtual registers in registers live across it, which it must load
	// again from their slots.
	Across map[[2]int32][]VReg
	// NumSlots is the number of slots used: those below it.
	NumSlots int
}

// ErrSlots is the error of a function whose spills need more slots than
// Config.NumSlots.
var ErrSlots = errors.New("lsra: not enough spill slots")

type interval struct {
	v        VReg
	from, to int32
	weight   float64
	// avoid is the registers of its class that an instruction it lives
	// across clobbers.
	avoid uint64
	reg   int
	// calls is whether it lives across a call, callWeight the weight of
	// the reloads after the calls it lives across.
	calls      bool
	callWeight float64
}

// Allocate allocates f's virtual registers.
func Allocate(f *Func, cfg *Config) (*Result, error) {
	n := len(f.Class)
	words := (n + 63) / 64
	nb := len(f.Blocks)
	// Liveness: the registers live into and out of each block, over the
	// blocks backwards until nothing changes.
	liveIn := make([][]uint64, nb)
	liveOut := make([][]uint64, nb)
	gen := make([][]uint64, nb)
	kill := make([][]uint64, nb)
	alloc := func(v VReg) bool { return v >= 0 && f.Class[v] != NoClass }
	for i := range f.Blocks {
		b := &f.Blocks[i]
		liveIn[i] = make([]uint64, words)
		liveOut[i] = make([]uint64, words)
		g := make([]uint64, words)
		k := make([]uint64, words)
		for j := len(b.Insts) - 1; j >= 0; j-- {
			in := &b.Insts[j]
			for _, d := range in.Defs {
				if alloc(d) {
					k[d/64] |= 1 << (d % 64)
					g[d/64] &^= 1 << (d % 64)
				}
			}
			for _, u := range in.Uses {
				if alloc(u) {
					g[u/64] |= 1 << (u % 64)
				}
			}
		}
		for _, p := range b.Phis {
			if alloc(p.Def) {
				k[p.Def/64] |= 1 << (p.Def % 64)
				g[p.Def/64] &^= 1 << (p.Def % 64)
			}
		}
		gen[i], kill[i] = g, k
	}
	for changed := true; changed; {
		changed = false
		for i := nb - 1; i >= 0; i-- {
			b := &f.Blocks[i]
			out := liveOut[i]
			for _, s := range b.Succs {
				sb := &f.Blocks[s]
				for w := range out {
					nw := out[w] | liveIn[s][w]
					if nw != out[w] {
						out[w] = nw
						changed = true
					}
				}
				// A successor's phis read their arguments on the edge.
				pi := slices.Index(sb.Preds, i)
				for _, p := range sb.Phis {
					if a := p.Args[pi]; alloc(a) && out[a/64]&(1<<(a%64)) == 0 {
						out[a/64] |= 1 << (a % 64)
						changed = true
					}
				}
			}
			in := liveIn[i]
			for w := range in {
				nw := gen[i][w] | out[w]&^kill[i][w]
				if nw != in[w] {
					in[w] = nw
					changed = true
				}
			}
		}
	}

	// The intervals, over positions: a block's phis are defined at its
	// start; instruction j reads at start+2+2j and writes one after; the
	// block ends at start+2+2*len(Insts).
	ivs := make([]*interval, n)
	extend := func(v VReg, p int32, depth int) {
		if !alloc(v) {
			return
		}
		iv := ivs[v]
		if iv == nil {
			iv = &interval{v: v, from: p, to: p, reg: -1}
			ivs[v] = iv
		}
		iv.from = min(iv.from, p)
		iv.to = max(iv.to, p)
		iv.weight += depthWeight(depth)
	}
	span := func(v VReg, p int32) {
		if iv := ivs[v]; iv != nil {
			iv.from = min(iv.from, p)
			iv.to = max(iv.to, p)
		} else if alloc(v) {
			ivs[v] = &interval{v: v, from: p, to: p, reg: -1}
		}
	}
	type clobber struct {
		at     int32
		masks  []uint64
		call   bool
		key    [2]int32
		weight float64
	}
	var clobbers []clobber
	pos := int32(0)
	starts := make([]int32, nb)
	ends := make([]int32, nb)
	for i := range f.Blocks {
		b := &f.Blocks[i]
		start := pos
		end := start + 2 + 2*int32(len(b.Insts))
		starts[i], ends[i] = start, end
		for _, p := range b.Phis {
			extend(p.Def, start, b.Depth)
		}
		for j := range b.Insts {
			in := &b.Insts[j]
			u := start + 2 + 2*int32(j)
			for _, v := range in.Uses {
				extend(v, u, b.Depth)
			}
			for _, v := range in.Defs {
				extend(v, u+1, b.Depth)
			}
			if in.Call || len(in.Clobber) > 0 {
				clobbers = append(clobbers, clobber{at: u, masks: in.Clobber, call: in.Call, key: [2]int32{int32(i), int32(j)}, weight: depthWeight(b.Depth)})
			}
		}
		pos = end + 2
	}
	for i := range f.Blocks {
		for w, word := range liveIn[i] {
			for word != 0 {
				v := VReg(w*64 + bits.TrailingZeros64(word))
				word &= word - 1
				span(v, starts[i])
			}
		}
		for w, word := range liveOut[i] {
			for word != 0 {
				v := VReg(w*64 + bits.TrailingZeros64(word))
				word &= word - 1
				span(v, ends[i])
			}
		}
	}
	// What each interval lives across: the clobbers, in order of their
	// positions, from the first past its start.
	crossed := func(iv *interval, visit func(c *clobber)) {
		k, _ := slices.BinarySearchFunc(clobbers, iv.from+1, func(c clobber, p int32) int { return int(c.at - p) })
		for ; k < len(clobbers) && clobbers[k].at < iv.to; k++ {
			visit(&clobbers[k])
		}
	}
	var list []*interval
	for _, iv := range ivs {
		if iv == nil {
			continue
		}
		list = append(list, iv)
		cls := f.Class[iv.v]
		crossed(iv, func(c *clobber) {
			if c.call {
				iv.calls = true
				iv.callWeight += c.weight
			}
			if int(cls) < len(c.masks) {
				iv.avoid |= c.masks[cls]
			}
		})
	}
	slices.SortFunc(list, func(a, b *interval) int {
		if a.from != b.from {
			return int(a.from - b.from)
		}
		return int(a.v - b.v)
	})

	// Hints: a two-address definition wants its operand's register, a
	// phi its arguments', an argument its phi's.
	hints := make([][]VReg, n)
	for i := range f.Blocks {
		b := &f.Blocks[i]
		for _, p := range b.Phis {
			if !alloc(p.Def) {
				continue
			}
			for _, a := range p.Args {
				if alloc(a) && f.Class[a] == f.Class[p.Def] {
					hints[p.Def] = append(hints[p.Def], a)
					hints[a] = append(hints[a], p.Def)
				}
			}
		}
		for j := range b.Insts {
			in := &b.Insts[j]
			if in.Hint != None && len(in.Defs) > 0 && alloc(in.Defs[0]) && alloc(in.Hint) && f.Class[in.Hint] == f.Class[in.Defs[0]] {
				hints[in.Defs[0]] = append(hints[in.Defs[0]], in.Hint)
			}
		}
	}

	res := &Result{Reg: make([]int16, n), Slot: make([]int32, n), Across: map[[2]int32][]VReg{}}
	for i := range res.Reg {
		res.Reg[i] = -1
		res.Slot[i] = -1
	}
	spilled := make([]bool, n)
	for cls, regs := range cfg.Classes {
		var active []*interval
		for _, cur := range list {
			if int(f.Class[cur.v]) != cls {
				continue
			}
			if cur.calls && cur.callWeight >= cur.weight {
				// Its reloads after the calls it lives across would cost
				// more than its uses gain: it lives in its slot.
				spilled[cur.v] = true
				continue
			}
			// Expire the intervals that end before cur begins.
			keep := active[:0]
			for _, a := range active {
				if a.to >= cur.from {
					keep = append(keep, a)
				}
			}
			active = keep
			var used uint64
			for _, a := range active {
				used |= 1 << a.reg
			}
			free := func(r int) bool { return used&(1<<r) == 0 && cur.avoid&(1<<r) == 0 }
			reg := -1
			for _, h := range hints[cur.v] {
				if r := res.Reg[h]; r >= 0 && free(int(r)) {
					reg = int(r)
					break
				}
			}
			if reg < 0 {
				for _, r := range regs {
					if free(r) {
						reg = r
						break
					}
				}
			}
			if reg >= 0 {
				cur.reg = reg
				res.Reg[cur.v] = int16(reg)
				active = append(active, cur)
				continue
			}
			// No register: spill the interval of the lowest weight among
			// cur and those whose register cur may take.
			victim := cur
			for _, a := range active {
				if cur.avoid&(1<<a.reg) != 0 {
					continue
				}
				if a.weight < victim.weight || (a.weight == victim.weight && a.to > victim.to) {
					victim = a
				}
			}
			spilled[victim.v] = true
			if victim == cur {
				continue
			}
			cur.reg = victim.reg
			res.Reg[cur.v] = int16(victim.reg)
			res.Reg[victim.v] = -1
			victim.reg = -1
			active = slices.DeleteFunc(active, func(a *interval) bool { return a == victim })
			active = append(active, cur)
		}
	}

	// Slots: for the spilled intervals, and those in registers that live
	// across a call; a fixed slot is held from the function's start.
	var slotted []*interval
	for _, iv := range list {
		if spilled[iv.v] || iv.calls {
			slotted = append(slotted, iv)
		}
	}
	type held struct {
		slot int32
		to   int32
	}
	var busy []held
	maxSlot := int32(-1)
	for v, s := range f.FixedSlot {
		if s < 0 {
			continue
		}
		to := int32(0)
		if iv := ivs[v]; iv != nil {
			to = iv.from
			if spilled[v] || iv.calls {
				to = iv.to
				res.Slot[v] = s
			}
		}
		busy = append(busy, held{s, to})
		maxSlot = max(maxSlot, s)
	}
	for _, iv := range slotted {
		if res.Slot[iv.v] >= 0 {
			continue
		}
		// The slots held past iv's start are taken; those held no longer
		// are never again, the intervals coming in order of their starts.
		var taken []bool
		keep := busy[:0]
		for _, h := range busy {
			if h.to >= iv.from {
				keep = append(keep, h)
				for int(h.slot) >= len(taken) {
					taken = append(taken, false)
				}
				taken[h.slot] = true
			}
		}
		busy = keep
		s := int32(0)
		for int(s) < len(taken) && taken[s] {
			s++
		}
		if int(s) >= cfg.NumSlots {
			return nil, ErrSlots
		}
		res.Slot[iv.v] = s
		busy = append(busy, held{s, iv.to})
		maxSlot = max(maxSlot, s)
	}
	res.NumSlots = int(maxSlot + 1)

	// The calls each interval in a register lives across.
	for _, iv := range list {
		if !iv.calls || res.Reg[iv.v] < 0 {
			continue
		}
		crossed(iv, func(c *clobber) {
			if c.call {
				res.Across[c.key] = append(res.Across[c.key], iv.v)
			}
		})
	}
	return res, nil
}

// depthWeight is the weight of a use at loop depth d.
func depthWeight(d int) float64 {
	w := 1.0
	for ; d > 0 && w < 1e6; d-- {
		w *= 8
	}
	return w
}
