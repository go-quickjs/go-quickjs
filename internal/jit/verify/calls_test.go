//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// calls is props whose plain calls have each called one function natively,
// of two parameters, which reads no receiver.
type calls struct{ props }

func (f calls) NativeCalls(pc int) []ssa.CallSite {
	if pc+1 >= len(f.p.Maps) || f.p.Code[pc].Op != ir.Call && f.p.Code[pc].Op != ir.Host {
		return nil
	}
	operands := int(f.p.Maps[pc].Depth) - int(f.p.Maps[pc+1].Depth) + 1
	if operands < 1 {
		return nil
	}
	return []ssa.CallSite{{Callee: 0x50000, Closure: 0x51000, Entry: 0x52000, Count: 0x53000, Argc: operands - 1,
		Params: 2, LocalCount: 3, MaxStack: 4, ThisSlot: -1}}
}

// callCorpus calls a function passed in natively: its result dropped and
// used, numbers boxed as its arguments, values live across it.
var callCorpus = []string{
	`function f(g,n){let s=0;for(let i=0;i<n;i++){g(i*0.5,i);s+=g(s,i+1)}return s}`,
	`function f(g,o,n){let s=0;for(let i=0;i<n;i++){const h=i*0.25;g(h,o);s+=h}return s}`,
	`function f(g,a,b){if(a===1){g(a,b);return b}const c=(3-a)-b;g(a-1,c);g(a,b);g(a-1,c);return c}`,
}

// insts decodes code from off to end.
func insts(code []byte, off, end int) []x86asm.Inst {
	var out []x86asm.Inst
	for off < end {
		in, err := x86asm.Decode(code[off:end], 64)
		if err != nil {
			return out
		}
		out = append(out, in)
		off += in.Len
	}
	return out
}

// ctxMem reports an instruction's operand i that is the context's word at
// disp.
func ctxMem(in x86asm.Inst, i int, disp int32) bool {
	m, ok := in.Args[i].(x86asm.Mem)
	return ok && m.Base == x86asm.RDI && m.Disp == int64(disp)
}

// A native call is made in fewer instructions (mir's call): a result
// nothing uses is stored nowhere and tests no write-barrier flag; a number
// boxed before the call is read where it is, not boxed again; the stack's
// room is compared where its end is; the receiver and a local loaded at
// entry are read back from the frame, not saved. The callee's code makes
// its frame's registers at its call entry, which its start does not read
// again, and returns to a native caller from where its result is, writing
// the context's Ret only for Go.
func TestMirCallsLean(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000,
		LinkEntry: 8, LinkIn: 16, ClosureUpvalues: 24}
	defer func() { mir.Annotate = false }()
	mir.Annotate = true
	dropped, used, boxes, entries, returns, frames := 0, 0, 0, 0, 0, 0
	for _, src := range callCorpus {
		for i, p := range lowered(t, src) {
			f, err := ssa.BuildWith(p, calls{props{p: p}})
			if err != nil {
				continue
			}
			f.CallEntry = true
			ssa.Optimize(f)
			mc, err := mir.CompileAMD64(f, enc)
			if err != nil {
				t.Fatalf("%s (%d): %v", src, i, err)
			}
			byID := map[string]*ssa.Value{}
			for _, b := range f.Blocks {
				for _, v := range b.Values {
					byID[fmt.Sprintf("v%d", v.ID)] = v
				}
			}
			name := fmt.Sprintf("%s (%d)", src, i)
			for k, m := range mc.Marks {
				end := mc.Text
				if k+1 < len(mc.Marks) {
					end = min(end, mc.Marks[k+1].Off)
				}
				ins := insts(mc.Bytes, m.Off, end)
				tag := strings.Fields(m.Tag)
				switch {
				case m.Tag == "call entry":
					entries++
					// What follows it is the start's code, not a read of
					// the frame's registers.
					if next := insts(mc.Bytes, end, mc.Text); len(next) != 0 && (ctxMem(next[0], 1, abi.OffLocals) || ctxMem(next[0], 1, abi.OffStack)) {
						t.Errorf("%s: the start reads the frame's registers again after the call entry: %v", name, next[0])
					}
				case len(tag) == 3 && tag[1] == "end" && tag[2] == fmt.Sprint(ssa.BlockReturn):
					returns++
					// The return to a native caller first: ReturnTo read
					// before Ret is written.
					to, ret := -1, -1
					for j, in := range ins {
						if to < 0 && in.Op == x86asm.MOV && ctxMem(in, 1, abi.OffReturnTo) {
							to = j
						}
						if ret < 0 && in.Op == x86asm.MOV && ctxMem(in, 0, abi.OffRet) {
							ret = j
						}
					}
					if to < 0 || ret >= 0 && ret < to {
						t.Errorf("%s: %s writes Ret (%d) before it reads ReturnTo (%d): %v", name, m.Tag, ret, to, ins)
					}
				case len(tag) == 2 && tag[1] == "call":
					v := byID[tag[0]]
					if v == nil || len(v.Calls()) == 0 || v.Calls()[0].Go != 0 {
						continue
					}
					flags, keeps, ucomis, rooms := 0, 0, 0, 0
					for _, in := range ins {
						switch {
						case in.Op == x86asm.MOVZX:
							flags++
						case in.Op == x86asm.UCOMISD:
							ucomis++
						case in.Op == x86asm.CMP && ctxMem(in, 1, abi.OffStackEnd):
							rooms++
						}
						for _, a := range in.Args[:1] {
							if m, ok := a.(x86asm.Mem); ok && m.Base == x86asm.RDI && m.Disp >= int64(abi.OffKeep) && m.Disp < int64(abi.OffKeep)+16*abi.MaxKeeps {
								keeps++
							}
						}
					}
					// What the callee returns to reads back from the frame
					// is not saved before the jump to it.
					jump := -1
					for j, in := range ins {
						if _, ok := in.Args[0].(x86asm.Mem); ok && in.Op == x86asm.JMP {
							jump = j
						}
					}
					if jump >= 0 {
						reread := map[x86asm.Reg]bool{}
						for _, in := range ins[jump+1:] {
							if m, ok := in.Args[1].(x86asm.Mem); ok && (in.Op == x86asm.MOV || in.Op == x86asm.MOVSD_XMM) &&
								(m.Base == x86asm.RSI || m.Base == x86asm.RDI && m.Disp == int64(abi.OffThis)) {
								if r, ok := in.Args[0].(x86asm.Reg); ok {
									reread[r] = true
								}
							}
						}
						frames += len(reread)
						for _, in := range ins[:jump] {
							if a, ok := in.Args[0].(x86asm.Mem); ok && a.Base == x86asm.RDI && a.Disp >= int64(abi.OffSpill) && a.Disp < int64(abi.OffSpill)+8*abi.SpillSlots {
								if r, ok := in.Args[1].(x86asm.Reg); ok && reread[r] {
									t.Errorf("%s: %s saves %v, which it reads back from the frame: %v", name, m.Tag, r, ins)
								}
							}
						}
					}
					if rooms != 1 {
						t.Errorf("%s: %s compares the stack's room with its end in memory %d times: %v", name, m.Tag, rooms, ins)
					}
					if dead := v.Uses == 0; dead {
						dropped++
						if flags != 0 || keeps != 0 {
							t.Errorf("%s: %s, whose result nothing uses, tests the barrier flag %d times and stores %d words to keep cells: %v", name, m.Tag, flags, keeps, ins)
						}
					} else {
						used++
					}
					// A box with a location of its own, an operand: read
					// where it is.
					held := 0
					for _, a := range v.Args {
						if a.Op == ssa.OpBoxF64 {
							held++
						}
					}
					if held > 0 {
						boxes++
						if ucomis != 0 {
							t.Errorf("%s: %s boxes again %d of its operands that are in their places: %v", name, m.Tag, ucomis, ins)
						}
					}
				}
			}
		}
	}
	if dropped == 0 || used == 0 || boxes == 0 || entries == 0 || returns == 0 || frames == 0 {
		t.Fatalf("calls %d dropped, %d used, %d with boxes held, %d registers read back from frames; %d call entries, %d returns compiled",
			dropped, used, boxes, frames, entries, returns)
	}
	t.Logf("calls %d dropped, %d used, %d with boxes held, %d registers read back from frames; %d call entries, %d returns",
		dropped, used, boxes, frames, entries, returns)
}

// A native call that does not make its call natively -- its callee past
// the deepest context, or with no code -- has Go make it from native code,
// out of line: one such call of Go a call, whichever way leads there, on
// both targets (mir's call). Each way had had its own copy, the same
// instructions and a description of their own each time: a twentieth of
// Typescript's code.
func TestMirCallsOneGoFallback(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		Object: 0xFFF8000000000007, Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000, WriteBarrier: 0x71000,
		LinkEntry: 8, LinkIn: 16, ClosureUpvalues: 24, CallGo: 0x72000}
	for _, target := range []struct {
		name    string
		compile func(*ssa.Func, abi.Encoding) (*mir.Code, error)
	}{{"amd64", mir.CompileAMD64}, {"arm64", mir.CompileARM64}} {
		made, fallbacks := 0, 0
		for _, src := range callCorpus {
			for i, p := range lowered(t, src) {
				f, err := ssa.BuildWith(p, calls{props{p: p}})
				if err != nil {
					continue
				}
				f.CallEntry = true
				ssa.Optimize(f)
				mc, err := target.compile(f, enc)
				if err != nil {
					t.Fatalf("%s: %s (%d): %v", target.name, src, i, err)
				}
				keeps := map[int]bool{}
				for _, b := range f.Blocks {
					for _, v := range b.Values {
						if v.Op == ssa.OpCall && len(v.Calls()) != 0 && v.Calls()[0].Go == 0 {
							made++
							keeps[v.Index] = true
						}
					}
				}
				// By the call's PC, which each call of Go in its place says.
				per := map[uint32]int{}
				for _, st := range mc.GoCalls {
					if ssa.GoOp(st.Op&0xff) == ssa.GoCall && keeps[int(st.Keep)] {
						per[st.PC]++
						fallbacks++
					}
				}
				for pc, n := range per {
					if n > 1 {
						t.Errorf("%s: %s (%d): the call at %d has %d calls of Go in its place", target.name, src, i, pc, n)
					}
				}
			}
		}
		if made == 0 || fallbacks == 0 {
			t.Fatalf("%s: %d calls, %d calls of Go in their place: the corpus makes none", target.name, made, fallbacks)
		}
	}
}
