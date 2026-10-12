//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"testing"

	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// Golden tests for internal/jit/asm/amd64: every form, with every register
// it takes and a range of displacements and immediates, must decode as one
// instruction of the intended operation and operands, using every byte.

func r64(r amd64.Reg) x86asm.Reg { return x86asm.RAX + x86asm.Reg(r) }
func r32(r amd64.Reg) x86asm.Reg { return x86asm.EAX + x86asm.Reg(r) }
func xr(r amd64.XReg) x86asm.Reg { return x86asm.X0 + x86asm.Reg(r) }
func r8(r amd64.Reg) x86asm.Reg {
	if r < 4 {
		return x86asm.AL + x86asm.Reg(r)
	}
	return x86asm.SPB + x86asm.Reg(r-4)
}

var (
	regs = func() (rs []amd64.Reg) {
		for r := amd64.RAX; r <= amd64.R15; r++ {
			rs = append(rs, r)
		}
		return
	}()
	xregs = func() (rs []amd64.XReg) {
		for r := amd64.XReg(0); r < 16; r++ {
			rs = append(rs, r)
		}
		return
	}()
	disps = []int32{0, 8, -8, 0x7fffffff, -0x80000000, 4096}
)

// setcc and jcc are each condition's operation, in the hardware's order.
var (
	setcc = []x86asm.Op{x86asm.SETO, x86asm.SETNO, x86asm.SETB, x86asm.SETAE, x86asm.SETE, x86asm.SETNE, x86asm.SETBE, x86asm.SETA,
		x86asm.SETS, x86asm.SETNS, x86asm.SETP, x86asm.SETNP, x86asm.SETL, x86asm.SETGE, x86asm.SETLE, x86asm.SETG}
	jcc = []x86asm.Op{x86asm.JO, x86asm.JNO, x86asm.JB, x86asm.JAE, x86asm.JE, x86asm.JNE, x86asm.JBE, x86asm.JA,
		x86asm.JS, x86asm.JNS, x86asm.JP, x86asm.JNP, x86asm.JL, x86asm.JGE, x86asm.JLE, x86asm.JG}
)

// mem is an expected memory operand: only its base and displacement.
type mem struct {
	base x86asm.Reg
	disp int64
}

// decodeOne decodes code as exactly one instruction.
func decodeOne(t *testing.T, name string, code []byte) x86asm.Inst {
	t.Helper()
	inst, err := x86asm.Decode(code, 64)
	if err != nil {
		t.Fatalf("%s: % x does not decode: %v", name, code, err)
	}
	if inst.Len != len(code) {
		t.Fatalf("%s: % x decodes as %v, %d of %d bytes", name, code, inst, inst.Len, len(code))
	}
	return inst
}

// expect checks an instruction's operation and operands. An operand is an
// x86asm.Reg, a mem, or an int64 immediate.
// expectBytes is expect for an instruction that reads or writes memory,
// checking the access's width as well.
func expectBytes(t *testing.T, name string, code []byte, op x86asm.Op, width int, args ...any) {
	t.Helper()
	expect(t, name, code, op, args...)
	if inst := decodeOne(t, name, code); inst.MemBytes != width {
		t.Fatalf("%s: % x is %v, a %d-byte access, want %d", name, code, inst, inst.MemBytes, width)
	}
}

func expect(t *testing.T, name string, code []byte, op x86asm.Op, args ...any) {
	t.Helper()
	inst := decodeOne(t, name, code)
	if inst.Op != op {
		t.Fatalf("%s: % x is %v, want %v", name, code, inst, op)
	}
	for i, want := range args {
		got := inst.Args[i]
		ok := false
		switch w := want.(type) {
		case x86asm.Reg:
			ok = got == w
		case mem:
			m, isMem := got.(x86asm.Mem)
			ok = isMem && m.Base == w.base && m.Index == 0 && int64(int32(m.Disp)) == w.disp
		case int64:
			im, isImm := got.(x86asm.Imm)
			ok = isImm && int64(im) == w
		}
		if !ok {
			t.Fatalf("%s: % x is %v; operand %d is %v, want %v", name, code, inst, i, got, want)
		}
	}
}

func encode(f func(a *amd64.Asm)) []byte {
	var a amd64.Asm
	f(&a)
	code, err := a.Finish()
	if err != nil {
		panic(err)
	}
	return code
}

func TestAMD64Divide(t *testing.T) {
	expect(t, "Cqo", encode(func(a *amd64.Asm) { a.Cqo() }), x86asm.CQO)
	for _, r := range regs {
		expect(t, fmt.Sprintf("Idiv r%d", r), encode(func(a *amd64.Asm) { a.Idiv(r) }), x86asm.IDIV, r64(r))
	}
}

func TestAMD64Moves(t *testing.T) {
	for _, d := range regs {
		for _, s := range regs {
			name := fmt.Sprintf("r%d,r%d", d, s)
			expect(t, "MovRR "+name, encode(func(a *amd64.Asm) { a.MovRR(d, s) }), x86asm.MOV, r64(d), r64(s))
			expect(t, "MovRR32 "+name, encode(func(a *amd64.Asm) { a.MovRR32(d, s) }), x86asm.MOV, r32(d), r32(s))
			for _, disp := range disps {
				expect(t, "Load "+name, encode(func(a *amd64.Asm) { a.Load(d, s, disp) }), x86asm.MOV, r64(d), mem{r64(s), int64(disp)})
				expect(t, "Lea "+name, encode(func(a *amd64.Asm) { a.Lea(d, s, disp) }), x86asm.LEA, r64(d), mem{r64(s), int64(disp)})
				expect(t, "Store "+name, encode(func(a *amd64.Asm) { a.Store(s, disp, d) }), x86asm.MOV, mem{r64(s), int64(disp)}, r64(d))
				expectBytes(t, "LoadU32 "+name, encode(func(a *amd64.Asm) { a.LoadU32(d, s, disp) }), x86asm.MOV, 4, r32(d), mem{r64(s), int64(disp)})
				expectBytes(t, "LoadU16 "+name, encode(func(a *amd64.Asm) { a.LoadU16(d, s, disp) }), x86asm.MOVZX, 2, r32(d), mem{r64(s), int64(disp)})
				expectBytes(t, "LoadU8 "+name, encode(func(a *amd64.Asm) { a.LoadU8(d, s, disp) }), x86asm.MOVZX, 1, r32(d), mem{r64(s), int64(disp)})
			}
			expect(t, "MovZX8 "+name, encode(func(a *amd64.Asm) { a.MovZX8(d, s) }), x86asm.MOVZX, r32(d), r8(s))
		}
		for _, v := range []uint64{0, 1, 0x7fffffff, 0xffffffff, ^uint64(0), 1 << 63, 0x7ff8000000000000, 0xfff8000000000008} {
			code := encode(func(a *amd64.Asm) { a.MovImm(d, v) })
			inst := decodeOne(t, "MovImm", code)
			im, ok := inst.Args[1].(x86asm.Imm)
			var got uint64
			switch inst.Args[0] {
			case r32(d):
				got = uint64(uint32(im))
			case r64(d):
				got = uint64(im)
			default:
				t.Fatalf("MovImm r%d, %#x: %v", d, v, inst)
			}
			if inst.Op != x86asm.MOV || !ok || got != v {
				t.Fatalf("MovImm r%d, %#x: %v", d, v, inst)
			}
		}
		for _, disp := range disps {
			expect(t, "DecMem", encode(func(a *amd64.Asm) { a.DecMem(d, disp) }), x86asm.DEC, mem{r64(d), int64(disp)})
		}
	}
}

func TestAMD64Integer(t *testing.T) {
	ops := map[amd64.ALU]x86asm.Op{amd64.Add: x86asm.ADD, amd64.Or: x86asm.OR, amd64.And: x86asm.AND,
		amd64.Sub: x86asm.SUB, amd64.Xor: x86asm.XOR, amd64.Cmp: x86asm.CMP, amd64.Test: x86asm.TEST}
	for op, xop := range ops {
		for _, d := range regs {
			for _, s := range regs {
				expect(t, fmt.Sprintf("%v r%d,r%d", xop, d, s), encode(func(a *amd64.Asm) { a.Op(op, d, s, true) }), xop, r64(d), r64(s))
				expect(t, fmt.Sprintf("%v32 r%d,r%d", xop, d, s), encode(func(a *amd64.Asm) { a.Op(op, d, s, false) }), xop, r32(d), r32(s))
			}
			if op == amd64.Test {
				continue
			}
			for _, imm := range []int32{0, 1, -1, 0x7fffffff, -0x80000000} {
				expect(t, fmt.Sprintf("%v r%d,%d", xop, d, imm), encode(func(a *amd64.Asm) { a.OpImm(op, d, imm, true) }), xop, r64(d), int64(imm))
			}
		}
	}
	shifts := map[amd64.Shift]x86asm.Op{amd64.Shl: x86asm.SHL, amd64.Shr: x86asm.SHR, amd64.Sar: x86asm.SAR}
	for op, xop := range shifts {
		for _, d := range regs {
			expect(t, "shift cl", encode(func(a *amd64.Asm) { a.ShiftCL(op, d, false) }), xop, r32(d), x86asm.CL)
			expect(t, "shift cl wide", encode(func(a *amd64.Asm) { a.ShiftCL(op, d, true) }), xop, r64(d), x86asm.CL)
			for _, n := range []uint8{1, 31, 51, 63} {
				expect(t, "shift imm", encode(func(a *amd64.Asm) { a.ShiftImm(op, d, n, true) }), xop, r64(d), int64(n))
			}
		}
	}
	for _, d := range regs {
		expect(t, "Not32", encode(func(a *amd64.Asm) { a.Not32(d) }), x86asm.NOT, r32(d))
		for c := amd64.Cond(0); c < 16; c++ {
			expect(t, "Setcc", encode(func(a *amd64.Asm) { a.Setcc(c, d) }), setcc[c], r8(d))
		}
	}
}

func TestAMD64SSE(t *testing.T) {
	ops := map[amd64.SSE]x86asm.Op{amd64.AddSD: x86asm.ADDSD, amd64.MulSD: x86asm.MULSD, amd64.SubSD: x86asm.SUBSD,
		amd64.DivSD: x86asm.DIVSD, amd64.UcomiSD: x86asm.UCOMISD, amd64.XorPD: x86asm.XORPD, amd64.MovAPD: x86asm.MOVAPD,
		amd64.SqrtSD: x86asm.SQRTSD, amd64.AndPD: x86asm.ANDPD, amd64.OrPD: x86asm.ORPD}
	for op, xop := range ops {
		for _, d := range xregs {
			for _, s := range xregs {
				expect(t, fmt.Sprintf("%v x%d,x%d", xop, d, s), encode(func(a *amd64.Asm) { a.SSEOp(op, d, s) }), xop, xr(d), xr(s))
			}
		}
	}
	for _, d := range xregs {
		for _, s := range xregs {
			for mode := uint8(1); mode <= 3; mode++ {
				expect(t, "RoundSD", encode(func(a *amd64.Asm) { a.RoundSD(d, s, mode) }), x86asm.ROUNDSD, xr(d), xr(s), int64(mode|8))
			}
			expect(t, "Cvtsd2ss", encode(func(a *amd64.Asm) { a.Cvtsd2ss(d, s) }), x86asm.CVTSD2SS, xr(d), xr(s))
			expect(t, "Cvtss2sd", encode(func(a *amd64.Asm) { a.Cvtss2sd(d, s) }), x86asm.CVTSS2SD, xr(d), xr(s))
		}
	}
	for _, x := range xregs {
		for _, r := range regs {
			expect(t, "MovQToX", encode(func(a *amd64.Asm) { a.MovQToX(x, r) }), x86asm.MOVQ, xr(x), r64(r))
			expect(t, "MovQFromX", encode(func(a *amd64.Asm) { a.MovQFromX(r, x) }), x86asm.MOVQ, r64(r), xr(x))
			expect(t, "Cvttsd2si", encode(func(a *amd64.Asm) { a.Cvttsd2si(r, x) }), x86asm.CVTTSD2SI, r64(r), xr(x))
			expect(t, "Cvtsi2sd", encode(func(a *amd64.Asm) { a.Cvtsi2sd(x, r, false) }), x86asm.CVTSI2SD, xr(x), r32(r))
			expect(t, "Cvtsi2sd wide", encode(func(a *amd64.Asm) { a.Cvtsi2sd(x, r, true) }), x86asm.CVTSI2SD, xr(x), r64(r))
			for _, disp := range disps {
				expect(t, "LoadSD", encode(func(a *amd64.Asm) { a.LoadSD(x, r, disp) }), x86asm.MOVSD_XMM, xr(x), mem{r64(r), int64(disp)})
				expect(t, "StoreSD", encode(func(a *amd64.Asm) { a.StoreSD(r, disp, x) }), x86asm.MOVSD_XMM, mem{r64(r), int64(disp)}, xr(x))
			}
		}
	}
}

func TestAMD64Branches(t *testing.T) {
	// A forward branch over n bytes, and a backward one to the start.
	for c := amd64.Cond(0); c < 16; c++ {
		code := encode(func(a *amd64.Asm) {
			l := a.NewLabel()
			a.Jcc(c, l)
			a.MovImm(amd64.RAX, 1)
			a.Bind(l)
			a.Ret()
		})
		inst := decodeOne(t, "Jcc", code[:6])
		if inst.Op != jcc[c] || inst.Args[0] != x86asm.Rel(5) {
			t.Fatalf("Jcc %d: %v", c, inst)
		}
	}
	code := encode(func(a *amd64.Asm) {
		l := a.NewLabel()
		a.Bind(l)
		a.Ret()
		a.Jmp(l)
	})
	if inst := decodeOne(t, "Jmp", code[1:]); inst.Op != x86asm.JMP || inst.Args[0] != x86asm.Rel(-6) {
		t.Fatalf("Jmp back: %v", inst)
	}
}

// JmpReg jumps through every register, and LeaLabel takes a label's
// address, RIP-relative, into every register.
func TestAMD64IndirectJumps(t *testing.T) {
	for _, r := range regs {
		expect(t, "JmpReg", encode(func(a *amd64.Asm) { a.JmpReg(r) }), x86asm.JMP, r64(r))
		code := encode(func(a *amd64.Asm) {
			l := a.NewLabel()
			a.LeaLabel(r, l)
			a.Ret()
			a.Bind(l)
		})
		inst := decodeOne(t, "LeaLabel", code[:7])
		m, ok := inst.Args[1].(x86asm.Mem)
		if inst.Op != x86asm.LEA || inst.Args[0] != r64(r) || !ok || m.Base != x86asm.RIP || m.Disp != 1 {
			t.Fatalf("LeaLabel %v: % x is %v", r, code, inst)
		}
	}
}
