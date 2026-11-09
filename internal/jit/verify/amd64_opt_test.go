//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"testing"

	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// The forms WebAssembly's optimizing tier adds (asm/amd64/opt.go), each
// with every register it takes.

func TestAMD64WasmOpt(t *testing.T) {
	ops := []struct {
		op  amd64.ALU
		ins x86asm.Op
	}{{amd64.Add, x86asm.ADD}, {amd64.Or, x86asm.OR}, {amd64.And, x86asm.AND}, {amd64.Sub, x86asm.SUB}, {amd64.Xor, x86asm.XOR}, {amd64.Cmp, x86asm.CMP}}
	for _, r := range regs {
		for _, base := range regs {
			for _, disp := range []int32{0, 8, -16, 1 << 20} {
				for _, o := range ops {
					expect(t, "OpMem", encode(func(a *amd64.Asm) { a.OpMem(o.op, r, base, disp, true) }), o.ins, r64(r), mem{r64(base), int64(disp)})
					expect(t, "OpMem32", encode(func(a *amd64.Asm) { a.OpMem(o.op, r, base, disp, false) }), o.ins, r32(r), mem{r64(base), int64(disp)})
				}
				expect(t, "ImulMem", encode(func(a *amd64.Asm) { a.ImulMem(r, base, disp, true) }), x86asm.IMUL, r64(r), mem{r64(base), int64(disp)})
				expect(t, "ImulMem32", encode(func(a *amd64.Asm) { a.ImulMem(r, base, disp, false) }), x86asm.IMUL, r32(r), mem{r64(base), int64(disp)})
			}
		}
		for _, src := range regs {
			for _, imm := range []int32{3, -1, 127, -128, 1000, -70000, 1 << 30} {
				expect(t, "ImulImm", encode(func(a *amd64.Asm) { a.ImulImm(r, src, imm, true) }), x86asm.IMUL, r64(r), r64(src), int64(imm))
				expect(t, "ImulImm32", encode(func(a *amd64.Asm) { a.ImulImm(r, src, imm, false) }), x86asm.IMUL, r32(r), r32(src), int64(imm))
			}
		}
	}
	for _, x := range xregs {
		for _, base := range regs {
			for _, disp := range []int32{0, 8, -16, 1 << 20} {
				expect(t, "StoreSS", encode(func(a *amd64.Asm) { a.StoreSS(base, disp, x) }), x86asm.MOVSS, mem{r64(base), int64(disp)}, xr(x))
			}
		}
	}
}

// TestAMD64WasmOptStrings checks the string instructions the optimizing
// tier's memory.copy and memory.fill run: REP MOVSB and REP STOSB, bytes
// upward from RSI to RDI, RCX of them.
func TestAMD64WasmOptStrings(t *testing.T) {
	for _, c := range []struct {
		name string
		code []byte
		op   x86asm.Op
	}{
		{"RepMovsb", encode(func(a *amd64.Asm) { a.RepMovsb() }), x86asm.MOVSB},
		{"RepStosb", encode(func(a *amd64.Asm) { a.RepStosb() }), x86asm.STOSB},
	} {
		inst := decodeOne(t, c.name, c.code)
		rep := false
		for _, p := range inst.Prefix {
			if p&0xFF == x86asm.PrefixREP {
				rep = true
			}
		}
		dst, _ := inst.Args[0].(x86asm.Mem)
		if inst.Op != c.op || !rep || dst.Base != x86asm.RDI {
			t.Fatalf("%s: % x is %v, want REP %v to [RDI]", c.name, c.code, inst, c.op)
		}
	}
}
