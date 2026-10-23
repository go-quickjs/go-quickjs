//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// budgetCorpus is loops that call nothing, Go's calls from their
// operations aside (a remainder's, an element's store past its array's
// length), and loops that call.
var budgetCorpus = []string{
	`function f(n){var j,s=0;for(j=0;j<n;j++)s+=j;return s}`,
	`function f(a,n){let s=0;for(let i=0;i<n;i++)s+=a[i]%3.5;return s}`,
	`function f(a,n){for(let i=0;i<n;i++)a[i]=i;return a}`,
	`function f(g,n){let s=0;for(let i=0;i<n;i++)s+=g(i);return s}`,
}

// A loop that calls nothing counts its back-edge budget in a register
// (mir's budget): its back edges decrement no memory -- no store and load
// on each turn's critical path -- and every call of Go in it stores the
// budget back before it and loads it after (Go may read and change it).
// A loop that calls counts in memory, as its callees' loops do.
func TestMirBudgetInRegister(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000,
		CallGo: 0x72000}
	defer func() { mir.Annotate = false }()
	mir.Annotate = true
	inRegister, inMemory, goCalls, entries := 0, 0, 0, 0
	for _, src := range append(append([]string(nil), budgetCorpus...), jsCorpus...) {
		for i, p := range lowered(t, src) {
			f, err := ssa.BuildWith(p, calls{props{p: p}})
			if err != nil {
				continue
			}
			ssa.Optimize(f)
			mc, err := mir.CompileAMD64(f, enc)
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			name := fmt.Sprintf("%s (%d)", src, i)
			calling := false
			for _, b := range f.Blocks {
				for _, v := range b.Values {
					if v.Op == ssa.OpCall {
						calling = true
					}
				}
			}
			for k, m := range mc.Marks {
				end := mc.Text
				if k+1 < len(mc.Marks) {
					end = min(end, mc.Marks[k+1].Off)
				}
				ins := insts(mc.Bytes, m.Off, end)
				if strings.HasPrefix(m.Tag, "edge ") && !strings.HasPrefix(mc.Marks[max(k-1, 0)].Tag, "cold") {
					// The way into a loop counting in a register reads
					// no budget (mir's budgetStart): a loop entered for
					// no turn touches none.
					back, empties, reads := false, false, false
					for _, in := range ins {
						if r, ok := in.Args[0].(x86asm.Reg); ok && (in.Op == x86asm.SUB && in.Args[1] == x86asm.Imm(1) || in.Op == x86asm.XOR && in.Args[1] == r) {
							back = back || in.Op == x86asm.SUB
							empties = empties || in.Op == x86asm.XOR
						}
						if _, mem := in.Args[0].(x86asm.Mem); mem && in.Op == x86asm.DEC {
							back = true
						}
						if a, ok := in.Args[1].(x86asm.Mem); ok && in.Op == x86asm.MOV && a.Base == x86asm.RDI && a.Disp == int64(abi.OffBackEdges) {
							reads = true
						}
					}
					if empties {
						entries++
					}
					if empties && !back && reads {
						t.Errorf("%s: %s, into a loop counting in a register, reads the budget: %v", name, m.Tag, ins)
					}
				}
				if strings.HasPrefix(m.Tag, "edge ") {
					for _, in := range ins {
						if _, mem := in.Args[0].(x86asm.Mem); mem && in.Op == x86asm.DEC {
							// A loop that calls nothing may count in memory
							// too where it needs every register (the budget
							// the allocator's first spill): not budgetCorpus's.
							if !calling && slices.Contains(budgetCorpus[:3], src) {
								t.Errorf("%s: %s decrements the budget in memory: %v", name, m.Tag, ins)
							}
							inMemory++
						}
						if r, ok := in.Args[0].(x86asm.Reg); ok && in.Op == x86asm.SUB && in.Args[1] == x86asm.Imm(1) && r != x86asm.RSP {
							inRegister++
						}
					}
				}
				// A call of Go in a loop counting in a register: the
				// budget stored before the jump to the routine where the
				// register holds it, and the register emptied after it,
				// to be read again at the next back edge (mir's
				// budgetStart).
				for j, in := range ins {
					if in.Op != x86asm.JMP || j < 3 || j+2 >= len(ins) {
						continue
					}
					if _, ok := in.Args[0].(x86asm.Rel); !ok || !isLea(ins[j-1]) {
						continue
					}
					var stored x86asm.Reg
					for i, b := range ins[max(0, j-6):j] {
						if mem, ok := b.Args[0].(x86asm.Mem); ok && b.Op == x86asm.MOV && mem.Disp == 0 && isBackEdges(ins[max(0, j-7):j]) {
							stored, _ = b.Args[1].(x86asm.Reg)
						}
						// The flush: stored, out of line, where it holds
						// the budget (mir's budgetFlush).
						if r := flushed(ins[max(0, j-6)+i:]); r != 0 {
							stored = r
						}
					}
					if stored != 0 {
						goCalls++
						// The poll's call of Go, which gives a new budget,
						// reads it again at once.
						r, ok := ins[j+1].Args[0].(x86asm.Reg)
						emptied := ok && ins[j+1].Op == x86asm.XOR && ins[j+1].Args[1] == r && r-x86asm.R8L == stored-x86asm.R8
						a, isMem := ins[j+1].Args[1].(x86asm.Mem)
						loaded := isMem && ins[j+1].Op == x86asm.MOV && a.Base == x86asm.RDI && a.Disp == int64(abi.OffBackEdges)
						if !emptied && !loaded {
							t.Errorf("%s: %s: a call of Go stores the budget back, but neither empties nor reads its register after: %v", name, m.Tag, ins[j:])
						}
					}
				}
			}
		}
	}
	// A walk counts in memory (mir's walks): its turns wait on its loads,
	// and it is entered often for few of them.
	for i, p := range lowered(t, `function f(x,y){while(y!==null){if(x===null)return true;x=x.next;y=y.next}return false}`) {
		f, err := ssa.BuildWith(p, calls{props{p: p}})
		if err != nil {
			continue
		}
		ssa.Optimize(f)
		mc, err := mir.CompileAMD64(f, enc)
		if err != nil {
			t.Fatalf("walk (%d): %v", i, err)
		}
		dec := false
		for k, m := range mc.Marks {
			end := mc.Text
			if k+1 < len(mc.Marks) {
				end = min(end, mc.Marks[k+1].Off)
			}
			for _, in := range insts(mc.Bytes, m.Off, end) {
				if r, ok := in.Args[0].(x86asm.Reg); ok && in.Op == x86asm.SUB && in.Args[1] == x86asm.Imm(1) && r != x86asm.RSP && strings.HasPrefix(m.Tag, "edge ") {
					t.Errorf("walk (%d): %s counts its budget in a register", i, m.Tag)
				}
				if _, mem := in.Args[0].(x86asm.Mem); mem && in.Op == x86asm.DEC && strings.HasPrefix(m.Tag, "edge ") {
					dec = true
				}
			}
		}
		if !dec {
			t.Errorf("walk (%d): no back edge decrements the budget in memory", i)
		}
	}
	if entries == 0 {
		t.Error("no way into a loop counting in a register")
	}
	if inRegister == 0 || inMemory == 0 || goCalls == 0 {
		t.Fatalf("back edges counting in registers %d, in memory %d; calls of Go storing a budget %d", inRegister, inMemory, goCalls)
	}
	t.Logf("back edges counting in registers %d, in memory %d; calls of Go storing a budget %d", inRegister, inMemory, goCalls)
}

func isLea(in x86asm.Inst) bool { return in.Op == x86asm.LEA }

// flushed is the register ins begin by testing for 0 and jumping if not:
// mir's budgetFlush.
func flushed(ins []x86asm.Inst) x86asm.Reg {
	if len(ins) < 2 || ins[0].Op != x86asm.TEST || ins[1].Op != x86asm.JNE {
		return 0
	}
	if r, ok := ins[0].Args[0].(x86asm.Reg); ok && ins[0].Args[1] == r {
		return r
	}
	return 0
}

// isBackEdges reports whether ins load the context's BackEdges.
func isBackEdges(ins []x86asm.Inst) bool {
	for _, in := range ins {
		if m, ok := in.Args[1].(x86asm.Mem); ok && in.Op == x86asm.MOV && m.Base == x86asm.RDI && m.Disp == int64(abi.OffBackEdges) {
			return true
		}
	}
	return false
}

// An If's way out of a loop the other way stays in is laid out with the
// cold code (mir's leaves): the loop's way falls through from the test.
// The edge out of a loop counting its budget in a register stores the
// register back, and laid out after the test, it had the test jump past
// it on every turn: microbench's prop_read, prop_write and
// array_length_read a few percent slower.
func TestMirLoopExitsOutOfLine(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000,
		CallGo: 0x72000}
	defer func() { mir.Annotate = false }()
	mir.Annotate = true
	exits := 0
	for _, src := range budgetCorpus[:3] {
		for i, p := range lowered(t, src) {
			f, err := ssa.BuildWith(p, calls{props{p: p}})
			if err != nil {
				continue
			}
			ssa.Optimize(f)
			mc, err := mir.CompileAMD64(f, enc)
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			for k, m := range mc.Marks {
				end := mc.Text
				if k+1 < len(mc.Marks) {
					end = min(end, mc.Marks[k+1].Off)
				}
				stores := false
				code := insts(mc.Bytes, m.Off, end)
				for i, in := range code {
					if mem, ok := in.Args[0].(x86asm.Mem); ok && in.Op == x86asm.MOV && mem.Base == x86asm.R11 && mem.Disp == 0 {
						stores = true
					}
					if flushed(code[i:]) != 0 {
						stores = true
					}
				}
				switch {
				case !stores:
				case regexp.MustCompile(`^b[0-9]+$`).MatchString(m.Tag):
					// A loop's way out to a block of its own: the
					// budget stored back at the block's start
					// (mir's flushIn), the edge moving nothing.
					exits++
				case strings.HasPrefix(m.Tag, "edge ") && (k == 0 || mc.Marks[k-1].Off != m.Off || !strings.HasPrefix(mc.Marks[k-1].Tag, "cold ")):
					t.Errorf("%s (%d): %s, storing the budget back, is laid out in the loop's code", src, i, m.Tag)
				}
			}
		}
	}
	if exits == 0 {
		t.Fatal("no loop's way out stored its budget back at its block")
	}
}
