//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"math/bits"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// loopBlocks is the blocks of f's loops: each header's, and those that
// reach one of its back edges without passing it.
func loopBlocks(f *ssa.Func) map[*ssa.Block]bool {
	in := map[*ssa.Block]bool{}
	for _, h := range f.Blocks {
		if !h.LoopHeader {
			continue
		}
		in[h] = true
		var stack []*ssa.Block
		for i, p := range h.Preds {
			if h.Backedge[i] && !in[p] {
				in[p] = true
				stack = append(stack, p)
			}
		}
		for len(stack) > 0 {
			b := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, p := range b.Preds {
				if !in[p] {
					in[p] = true
					stack = append(stack, p)
				}
			}
		}
	}
	return in
}

// floatSlotAccess reports an instruction that reads or writes a spill slot
// from or to a floating-point register (spillAccess's general ones).
func floatSlotAccess(in x86asm.Inst) bool {
	if in.Op != x86asm.MOVSD_XMM {
		return false
	}
	for _, a := range in.Args[:2] {
		if m, ok := a.(x86asm.Mem); ok && m.Base == x86asm.RDI && m.Disp >= int64(abi.OffSpill) && m.Disp < int64(abi.OffSpill)+8*abi.SpillSlots {
			return true
		}
	}
	return false
}

// The allocation spills, of the intervals that vie for a register, the one
// whose slot costs least for what it gives up (mir's allocate): here the
// parameters, read once after the loop, not the loop's sums and counter,
// read and written every turn. Compiled with no feedback, as each lowering
// makes it, the function's numbers are floats, and the parameters go round
// the loop as phis of their own, beside the sums, in fourteen registers.
// Spilling whichever ended last, the allocation had put three of the sums
// in slots, stored and loaded back each turn, as they live to the return,
// past the parameters' phis.
func TestMirSpillsWhatTheLoopDoesNotRead(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000}
	defer func() { mir.Annotate = false }()
	mir.Annotate = true
	for _, src := range []string{
		`function f(a,b,c,d,e,g,h,k,n){let s0=0,s1=0,s2=0,s3=0;for(let i=0;i<n;i++){s0=(s0+i)|0;s1=(s1+s0)|0;s2=(s2^s1)|0;s3=(s3+s2)|0}const t=(a|0)+(b|0)+(c|0)+(d|0)+(e|0)+(g|0)+(h|0)+(k|0);return(t+s0+s1+s2+s3)|0}`,
		`function f(a,b,c,d,e,g,h,k,n){let s0=0,s1=0,s2=0,s3=0;for(let i=0;i<n;i++){s0=(s0+i)|0;s1=(s1+s0)|0;s2=(s2^s1)|0;s3=(s3+s2)|0}return(a+b+c+d+e+g+h+k+s0+s1+s2+s3)|0}`,
	} {
		ps := lowered(t, src)
		if len(ps) == 0 {
			t.Fatalf("%s: not lowered", src)
		}
		for i, p := range ps {
			f, err := ssa.BuildWith(p, globals{})
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			ssa.Optimize(f)
			mc, err := mir.CompileAMD64(f, enc)
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			loop := loopBlocks(f)
			if len(loop) == 0 {
				t.Fatalf("%s (%d): no loop:\n%s", src, i, f)
			}
			locs := " " + mc.Locations()
			spilled := strings.Count(locs, "spill")
			if spilled == 0 {
				t.Fatalf("%s (%d): nothing spilled, the test asks for too few registers: %s", src, i, locs)
			}
			// No value of the loop's in a slot but a phi the loop does not
			// change, its own argument at the back edge, which the loop's
			// code neither reads nor writes.
			for b := range loop {
				for _, v := range b.Values {
					if v.Op == ssa.OpPhi && slices.Contains(v.Args, v) {
						continue
					}
					if strings.Contains(locs, fmt.Sprintf(" %v:spill", v)) {
						t.Errorf("%s (%d): %v of the loop's b%d spilled:\n%s\n%s", src, i, v, b.ID, f, locs)
					}
				}
			}
			// Nor any slot read or written in the loop's code: its values'
			// stretches and its edges'.
			for k, m := range mc.Marks {
				var id int
				in := false
				if _, err := fmt.Sscanf(m.Tag, "v%d ", &id); err == nil {
					for b := range loop {
						for _, v := range b.Values {
							in = in || v.ID == id
						}
					}
				} else if strings.HasPrefix(m.Tag, "edge ") {
					var from, to int
					if _, err := fmt.Sscanf(m.Tag, "edge b%d->b%d", &from, &to); err == nil {
						for b := range loop {
							in = in || b.ID == to && b.LoopHeader && func() bool {
								for _, p := range b.Preds {
									if p.ID == from && loop[p] {
										return true
									}
								}
								return false
							}()
						}
					}
				}
				if !in {
					continue
				}
				for _, ins := range stretch(mc, k) {
					if spillAccess(ins) || floatSlotAccess(ins) {
						t.Errorf("%s (%d): %s reads or writes a spill slot: %v", src, i, m.Tag, ins)
					}
				}
			}
		}
	}
}

// slotCorpus is loops of k values each turn changes, k from 6 to 20, each
// alone and inside another loop: more values than registers, so that
// which are spilled decides how many slots the code reads and writes.
func slotCorpus() []string {
	var out []string
	for k := 6; k <= 20; k += 2 {
		for form := range 4 {
			var vs, us, rs []string
			for j := range k {
				vs = append(vs, fmt.Sprintf("a%d=o.p%d", j, j%4))
				us = append(us, []string{
					fmt.Sprintf("a%d=(a%d+a%d)|0", j, j, (j+1)%k),
					fmt.Sprintf("a%d=a%d*0.5+a%d", j, j, (j+1)%k),
					fmt.Sprintf("if(a%d>i)a%d=(a%d-i)|0", j, j, j),
					fmt.Sprintf("a%d=o.p%d+a%d", j, j%4, (j+3)%k),
				}[form])
				rs = append(rs, fmt.Sprintf("a%d", j))
			}
			let, body, ret := "let "+strings.Join(vs, ",")+";", strings.Join(us, ";"), "return "+strings.Join(rs, "+")
			out = append(out,
				"function f(o,n){"+let+"for(let i=0;i<n;i++){"+body+"}"+ret+"}",
				"function f(o,n){"+let+"for(let j=0;j<n;j++){for(let i=0;i<n;i++){"+body+"}o.q=a1}"+ret+"}")
		}
	}
	return out
}

// slotTraffic is how often code mc of f reads or writes a spill slot as it
// runs, in all and in its edges' moves: each access weighed as the
// allocator weighs its block (mir's edgeWeights), 16 to the power of the
// loops holding it, at most three -- an edge's, the loops holding both its
// ends -- and an exit's or a cold path's as the function's entry.
func slotTraffic(f *ssa.Func, mc *mir.Code) (all, edges int) {
	// Each loop, a bit: its header and the blocks that reach one of its
	// back edges without passing it.
	in := map[int]uint64{}
	byID := map[int]*ssa.Block{}
	for _, b := range f.Blocks {
		byID[b.ID] = b
	}
	bit := 0
	for _, h := range f.Blocks {
		if !h.LoopHeader || bit == 64 {
			continue
		}
		m := uint64(1) << bit
		bit++
		in[h.ID] |= m
		for i, p := range h.Preds {
			if !h.Backedge[i] {
				continue
			}
			stack := []*ssa.Block{p}
			for len(stack) > 0 {
				b := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if in[b.ID]&m != 0 {
					continue
				}
				in[b.ID] |= m
				stack = append(stack, b.Preds...)
			}
		}
	}
	block := map[string]int{}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			block[fmt.Sprintf("v%d", v.ID)] = b.ID
		}
	}
	weight := func(mask uint64) int { return []int{1, 16, 256, 4096}[min(bits.OnesCount64(mask), 3)] }
	id := func(s string) int { n, _ := strconv.Atoi(strings.TrimPrefix(s, "b")); return n }
	for k, m := range mc.Marks {
		w, edge := 1, false
		switch fs := strings.Fields(m.Tag); {
		case len(fs) == 0:
		case fs[0] == "edge":
			ends := strings.Split(fs[1], "->")
			w, edge = weight(in[id(ends[0])]&in[id(ends[1])]), true
		case strings.HasPrefix(fs[0], "v"):
			if b, ok := block[fs[0]]; ok {
				w = weight(in[b])
			}
		case strings.HasPrefix(fs[0], "b"):
			w = weight(in[id(fs[0])])
		}
		for _, in := range stretch(mc, k) {
			if spillAccess(in) || floatSlotAccess(in) {
				all += w
				if edge {
					edges += w
				}
			}
		}
	}
	return all, edges
}

// How often the code reads or writes a spill slot as it runs (slotTraffic),
// in all and in the edges' moves, over slotCorpus as each lowering makes
// it: what the spill choice (mir's allocate) decides. Spilling whichever
// interval ended last, at 04862771: 1159078, 518316 of it in edges. By the
// cost of each in a slot, for what it frees: 917902 and 553508. Counting a
// phi's move as none where the phi or its argument is in a slot the other
// may share (allocate's inSlot): 906324 and 542374. Splitting rather than
// spilling a value whose uses but frame states' are behind: 906328 and
// 542378, a store more at four functions' entries.
func TestMirSlotsOfLoops(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000}
	defer func() { mir.Annotate = false }()
	mir.Annotate = true
	edges, all := 0, 0
	for _, src := range slotCorpus() {
		for i, p := range lowered(t, src) {
			f, err := ssa.BuildWith(p, stepping{props{p: p}})
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			ssa.Optimize(f)
			mc, err := mir.CompileAMD64(f, enc)
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			a, e := slotTraffic(f, mc)
			all, edges = all+a, edges+e
		}
	}
	t.Logf("slot traffic %d, %d of it in edges", all, edges)
	if all > 906328 || edges > 542378 {
		t.Errorf("slot traffic %d, %d of it in edges: want at most 906328 and 542378", all, edges)
	}
}

// reloads lists where code mc of f loads a spill slot that its block
// wrote before it, in its own code: a value stored as it is made and
// loaded back for a use further on, in one block's turn.
func reloads(f *ssa.Func, mc *mir.Code, blocks map[*ssa.Block]bool) []string {
	block := map[int]*ssa.Block{}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			block[v.ID] = b
		}
	}
	var out []string
	var cur *ssa.Block
	stored := map[int64]string{}
	for k, m := range mc.Marks {
		var id int
		if _, err := fmt.Sscanf(m.Tag, "v%d ", &id); err != nil || block[id] == nil || !blocks[block[id]] {
			// Not a value's of the blocks: an edge, a cold path, an exit.
			cur = nil
			continue
		}
		if b := block[id]; b != cur {
			cur = b
			clear(stored)
		}
		for _, in := range stretch(mc, k) {
			if !spillAccess(in) && !floatSlotAccess(in) {
				continue
			}
			if mem, ok := in.Args[0].(x86asm.Mem); ok {
				stored[mem.Disp] = m.Tag
			} else if mem, ok := in.Args[1].(x86asm.Mem); ok {
				if at, ok := stored[mem.Disp]; ok {
					out = append(out, fmt.Sprintf("b%d: slot %#x stored at %s, loaded at %s", cur.ID, mem.Disp, at, m.Tag))
				}
			}
		}
	}
	return out
}

// A value whose uses but frame states' are all behind, when a register is
// wanted, is split rather than spilled (mir's allocate): it keeps its
// register up to its last such use, and its slot, written as it is made,
// is what the frame states after read. Here a loop of BigInt.asUintN's
// and asIntN's, as microbench's bigint64_fnv1a has them, every register
// taken: each reads the global BigInt -- its cell, then the function --
// whose next instruction reads asUintN off it, and which only the frame
// states read after, to the call, which reads no receiver. Spilling
// whole, the allocation kept those in their registers and spilled a value
// the turn read again (a shift's result, read by the zero-extension three
// instructions on): stored and loaded back each turn. Split, the cells
// and the function are stored as they are made and nothing the turn
// stores is loaded back in it.
func TestMirSplitsWhatOnlyStatesRead(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000}
	defer func() { mir.Annotate = false }()
	mir.Annotate = true
	src := bigCorpus[0]
	for i, p := range lowered(t, src) {
		f, err := ssa.BuildWith(p, bigs{p: p})
		if err != nil {
			t.Fatalf("%s (%d): %v", src, i, err)
		}
		ssa.Optimize(f)
		mc, err := mir.CompileAMD64(f, enc)
		if err != nil {
			t.Fatalf("%s (%d): %v", src, i, err)
		}
		locs := mc.Locations()
		if !strings.Contains(locs, "+spill") {
			t.Errorf("%s (%d): nothing split: %s", src, i, locs)
		}
		for _, r := range reloads(f, mc, loopBlocks(f)) {
			t.Errorf("%s (%d): %s\n%s\n%s", src, i, r, f, locs)
		}
	}
}

// An entry's load of a slot that only its unboxing there reads, and frame
// states, holds no register (mir's findLazy): the unboxing reads the slot
// itself. An entry of a loop of many locals had held each loaded number
// in a register to its last guard, as many as it loads -- the four sums'
// loop, entered at its header, eleven, which spilled four.
func TestMirEntryLoadsUnboxed(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000}
	src := `function f(a,b,c,d,n){let s0=0,s1=0,s2=0,s3=0;for(let i=0;i<n;i++){s0=(s0+i)|0;s1=(s1+s0)|0;s2=(s2^s1)|0;s3=(s3+s2)|0}return(a+b+c+d+s0+s1+s2+s3)|0}`
	checked := 0
	for i, p := range lowered(t, src) {
		f, err := ssa.BuildWith(p, globals{})
		if err != nil {
			t.Fatalf("%s (%d): %v", src, i, err)
		}
		ssa.Optimize(f)
		mc, err := mir.CompileAMD64(f, enc)
		if err != nil {
			t.Fatalf("%s (%d): %v", src, i, err)
		}
		locs := " " + mc.Locations()
		for _, b := range f.Blocks {
			if b.PC >= 0 {
				continue
			}
			for _, v := range b.Values {
				if v.Op != ssa.OpLoadSlot || !onlyUnboxedIn(f, v, b) {
					continue
				}
				checked++
				if !strings.Contains(locs, fmt.Sprintf(" %v:lazy", v)) {
					t.Errorf("%s (%d): %v, which only its unboxing reads, has a location:\n%s\n%s", src, i, v, f, locs)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatalf("%s: no entry's load only its unboxing reads", src)
	}
}

// onlyUnboxedIn reports whether v's only operation, of every block's, is
// an unboxing in b, and some is.
func onlyUnboxedIn(f *ssa.Func, v *ssa.Value, b *ssa.Block) bool {
	n := 0
	for _, c := range f.Blocks {
		for _, u := range c.Values {
			for _, a := range u.Args {
				if a != v {
					continue
				}
				if u.Op != ssa.OpUnboxF64 || c != b {
					return false
				}
				n++
			}
		}
		if c.Control == v {
			return false
		}
	}
	return n != 0
}
