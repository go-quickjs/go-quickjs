//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"slices"
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
		`function f(a,b,c,d,n){let s0=0,s1=0,s2=0,s3=0;for(let i=0;i<n;i++){s0=(s0+i)|0;s1=(s1+s0)|0;s2=(s2^s1)|0;s3=(s3+s2)|0}const t=(a|0)+(b|0)+(c|0)+(d|0);return(t+s0+s1+s2+s3)|0}`,
		`function f(a,b,c,d,n){let s0=0,s1=0,s2=0,s3=0;for(let i=0;i<n;i++){s0=(s0+i)|0;s1=(s1+s0)|0;s2=(s2^s1)|0;s3=(s3+s2)|0}return(a+b+c+d+s0+s1+s2+s3)|0}`,
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

// How many times the code reads or writes a spill slot, in all and in the
// edges' moves, over slotCorpus as each lowering makes it: what the spill
// choice (mir's allocate) decides. Spilling whichever interval ended last,
// at 04862771: 20533 and 9456. By the cost of each in a slot, for what it
// frees: 15697 and 7268. Counting a phi's move as none where the phi or its
// argument is in a slot the other may share (allocate's inSlot): 15159 and
// 6724.
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
			for k, m := range mc.Marks {
				for _, in := range stretch(mc, k) {
					if spillAccess(in) || floatSlotAccess(in) {
						all++
						if strings.HasPrefix(m.Tag, "edge ") {
							edges++
						}
					}
				}
			}
		}
	}
	t.Logf("%d slot accesses, %d of them in edges", all, edges)
	if all > 15159 || edges > 6724 {
		t.Errorf("%d slot accesses, %d of them in edges: want at most 15159 and 6724", all, edges)
	}
}
