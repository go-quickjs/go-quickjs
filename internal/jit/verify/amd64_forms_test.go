//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"testing"

	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// The forms WebAssembly's baseline code adds (asm/amd64/forms.go), each
// with every register it takes.

// memx is an expected memory operand with an index.
type memx struct {
	base, index x86asm.Reg
	scale       uint8
	disp        int64
}

// expectX is expect, with memx operands as well.
func expectX(t *testing.T, name string, code []byte, op x86asm.Op, width int, args ...any) {
	t.Helper()
	inst := decodeOne(t, name, code)
	if inst.Op != op {
		t.Fatalf("%s: % x is %v, want %v", name, code, inst, op)
	}
	if width != 0 && inst.MemBytes != width {
		t.Fatalf("%s: % x is %v, a %d-byte access, want %d", name, code, inst, inst.MemBytes, width)
	}
	for i, want := range args {
		got := inst.Args[i]
		ok := false
		switch w := want.(type) {
		case x86asm.Reg:
			ok = got == w
		case memx:
			m, isMem := got.(x86asm.Mem)
			ok = isMem && m.Base == w.base && m.Index == w.index && m.Scale == 1<<w.scale && int64(int32(m.Disp)) == w.disp
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

func r16(r amd64.Reg) x86asm.Reg { return x86asm.AX + x86asm.Reg(r) }

// indexes are the registers an index may be: all but RSP.
var indexes = func() (rs []amd64.Reg) {
	for _, r := range regs {
		if r != amd64.RSP {
			rs = append(rs, r)
		}
	}
	return
}()

var cmovs = []x86asm.Op{x86asm.CMOVO, x86asm.CMOVNO, x86asm.CMOVB, x86asm.CMOVAE, x86asm.CMOVE, x86asm.CMOVNE, x86asm.CMOVBE, x86asm.CMOVA,
	x86asm.CMOVS, x86asm.CMOVNS, x86asm.CMOVP, x86asm.CMOVNP, x86asm.CMOVL, x86asm.CMOVGE, x86asm.CMOVLE, x86asm.CMOVG}

func TestAMD64WasmInteger(t *testing.T) {
	expect(t, "Cdq", encode(func(a *amd64.Asm) { a.Cdq() }), x86asm.CDQ)
	for _, r := range regs {
		expect(t, "Idiv32", encode(func(a *amd64.Asm) { a.Idiv32(r) }), x86asm.IDIV, r32(r))
		expect(t, "Div32", encode(func(a *amd64.Asm) { a.Div32(r) }), x86asm.DIV, r32(r))
		for _, n := range []uint8{1, 31, 63} {
			expect(t, "Rol imm", encode(func(a *amd64.Asm) { a.ShiftImm(amd64.Rol, r, n, true) }), x86asm.ROL, r64(r), int64(n))
			expect(t, "Ror imm", encode(func(a *amd64.Asm) { a.ShiftImm(amd64.Ror, r, n, false) }), x86asm.ROR, r32(r), int64(n))
		}
		expect(t, "Rol cl", encode(func(a *amd64.Asm) { a.ShiftCL(amd64.Rol, r, false) }), x86asm.ROL, r32(r), x86asm.CL)
		expect(t, "Ror cl", encode(func(a *amd64.Asm) { a.ShiftCL(amd64.Ror, r, true) }), x86asm.ROR, r64(r), x86asm.CL)
		for _, disp := range disps {
			for _, imm := range []int32{0, 1, -1, 0x7fffffff, -0x80000000} {
				expectX(t, "StoreImm32", encode(func(a *amd64.Asm) { a.StoreImm32(r, disp, imm) }), x86asm.MOV, 4, mem{r64(r), int64(disp)}, int64(imm))
			}
		}
	}
	for _, d := range regs {
		for _, s := range regs {
			name := fmt.Sprintf("r%d,r%d", d, s)
			expect(t, "Bsf "+name, encode(func(a *amd64.Asm) { a.Bsf(d, s, true) }), x86asm.BSF, r64(d), r64(s))
			expect(t, "Bsf32 "+name, encode(func(a *amd64.Asm) { a.Bsf(d, s, false) }), x86asm.BSF, r32(d), r32(s))
			expect(t, "Bsr "+name, encode(func(a *amd64.Asm) { a.Bsr(d, s, true) }), x86asm.BSR, r64(d), r64(s))
			expect(t, "Popcnt "+name, encode(func(a *amd64.Asm) { a.Popcnt(d, s, true) }), x86asm.POPCNT, r64(d), r64(s))
			expect(t, "Popcnt32 "+name, encode(func(a *amd64.Asm) { a.Popcnt(d, s, false) }), x86asm.POPCNT, r32(d), r32(s))
			expect(t, "Movsx8 "+name, encode(func(a *amd64.Asm) { a.Movsx8(d, s, true) }), x86asm.MOVSX, r64(d), r8(s))
			expect(t, "Movsx8_32 "+name, encode(func(a *amd64.Asm) { a.Movsx8(d, s, false) }), x86asm.MOVSX, r32(d), r8(s))
			expect(t, "Movsx16 "+name, encode(func(a *amd64.Asm) { a.Movsx16(d, s, true) }), x86asm.MOVSX, r64(d), r16(s))
			expect(t, "Movsx16_32 "+name, encode(func(a *amd64.Asm) { a.Movsx16(d, s, false) }), x86asm.MOVSX, r32(d), r16(s))
			expect(t, "Imul32 "+name, encode(func(a *amd64.Asm) { a.Imul32(d, s) }), x86asm.IMUL, r32(d), r32(s))
			for c := amd64.Cond(0); c < 16; c++ {
				expect(t, "Cmov "+name, encode(func(a *amd64.Asm) { a.Cmov(c, d, s, true) }), cmovs[c], r64(d), r64(s))
				expect(t, "Cmov32 "+name, encode(func(a *amd64.Asm) { a.Cmov(c, d, s, false) }), cmovs[c], r32(d), r32(s))
			}
		}
	}
}

func TestAMD64WasmIndexed(t *testing.T) {
	for _, d := range regs {
		for _, b := range regs {
			for _, x := range indexes {
				for _, disp := range []int32{0, 8, -8, 127, 128, 0x7fffffff, -0x80000000} {
					m := memx{r64(b), r64(x), 0, int64(disp)}
					name := fmt.Sprintf("r%d,[r%d+r%d+%d]", d, b, x, disp)
					expectX(t, "LoadIdx "+name, encode(func(a *amd64.Asm) { a.LoadIdx(d, b, x, 1, disp) }), x86asm.MOV, 8, r64(d), m)
					expectX(t, "LoadU32Idx "+name, encode(func(a *amd64.Asm) { a.LoadU32Idx(d, b, x, disp) }), x86asm.MOV, 4, r32(d), m)
					expectX(t, "LoadU16Idx "+name, encode(func(a *amd64.Asm) { a.LoadU16Idx(d, b, x, disp) }), x86asm.MOVZX, 2, r32(d), m)
					expectX(t, "LoadU8Idx "+name, encode(func(a *amd64.Asm) { a.LoadU8Idx(d, b, x, disp) }), x86asm.MOVZX, 1, r32(d), m)
					expectX(t, "LoadS8Idx "+name, encode(func(a *amd64.Asm) { a.LoadS8Idx(d, b, x, disp, true) }), x86asm.MOVSX, 1, r64(d), m)
					expectX(t, "LoadS8Idx32 "+name, encode(func(a *amd64.Asm) { a.LoadS8Idx(d, b, x, disp, false) }), x86asm.MOVSX, 1, r32(d), m)
					expectX(t, "LoadS16Idx "+name, encode(func(a *amd64.Asm) { a.LoadS16Idx(d, b, x, disp, true) }), x86asm.MOVSX, 2, r64(d), m)
					expectX(t, "LoadS16Idx32 "+name, encode(func(a *amd64.Asm) { a.LoadS16Idx(d, b, x, disp, false) }), x86asm.MOVSX, 2, r32(d), m)
					expectX(t, "LoadS32Idx "+name, encode(func(a *amd64.Asm) { a.LoadS32Idx(d, b, x, disp) }), x86asm.MOVSXD, 4, r64(d), m)
					expectX(t, "StoreIdx "+name, encode(func(a *amd64.Asm) { a.StoreIdx(b, x, 1, disp, d) }), x86asm.MOV, 8, m, r64(d))
					expectX(t, "Store32Idx "+name, encode(func(a *amd64.Asm) { a.Store32Idx(b, x, disp, d) }), x86asm.MOV, 4, m, r32(d))
					expectX(t, "Store16Idx "+name, encode(func(a *amd64.Asm) { a.Store16Idx(b, x, disp, d) }), x86asm.MOV, 2, m, r16(d))
					expectX(t, "Store8Idx "+name, encode(func(a *amd64.Asm) { a.Store8Idx(b, x, disp, d) }), x86asm.MOV, 1, m, r8(d))
					for scale := uint8(0); scale < 4; scale++ {
						ms := memx{r64(b), r64(x), scale, int64(disp)}
						expectX(t, "LeaIdx "+name, encode(func(a *amd64.Asm) { a.LeaIdx(d, b, x, 1<<scale, disp) }), x86asm.LEA, 0, r64(d), ms)
						expectX(t, "LoadIdxScaled "+name, encode(func(a *amd64.Asm) { a.LoadIdxScaled(d, b, x, scale, disp) }), x86asm.MOV, 8, r64(d), ms)
					}
				}
			}
		}
	}
	for _, f := range xregs {
		for _, b := range regs {
			for _, x := range indexes {
				for _, disp := range []int32{0, -8, 128, 0x7fffffff} {
					m := memx{r64(b), r64(x), 0, int64(disp)}
					expectX(t, "LoadSSIdx", encode(func(a *amd64.Asm) { a.LoadSSIdx(f, b, x, disp) }), x86asm.MOVSS, 4, xr(f), m)
					expectX(t, "LoadSDIdx", encode(func(a *amd64.Asm) { a.LoadSDIdx(f, b, x, disp) }), x86asm.MOVSD_XMM, 8, xr(f), m)
					expectX(t, "StoreSSIdx", encode(func(a *amd64.Asm) { a.StoreSSIdx(b, x, disp, f) }), x86asm.MOVSS, 4, m, xr(f))
					expectX(t, "StoreSDIdx", encode(func(a *amd64.Asm) { a.StoreSDIdx(b, x, 1, disp, f) }), x86asm.MOVSD_XMM, 8, m, xr(f))
				}
			}
			for _, disp := range disps {
				expectX(t, "LoadSS", encode(func(a *amd64.Asm) { a.LoadSS(f, b, disp) }), x86asm.MOVSS, 4, xr(f), mem{r64(b), int64(disp)})
			}
		}
	}
}

func TestAMD64WasmSSE(t *testing.T) {
	ops := map[amd64.SSE]x86asm.Op{amd64.AddSD: x86asm.ADDSS, amd64.MulSD: x86asm.MULSS, amd64.SubSD: x86asm.SUBSS,
		amd64.DivSD: x86asm.DIVSS, amd64.UcomiSD: x86asm.UCOMISS, amd64.XorPD: x86asm.XORPS, amd64.MovAPD: x86asm.MOVAPS,
		amd64.SqrtSD: x86asm.SQRTSS, amd64.AndPD: x86asm.ANDPS, amd64.OrPD: x86asm.ORPS}
	for op, xop := range ops {
		for _, d := range xregs {
			for _, s := range xregs {
				expect(t, fmt.Sprintf("%v x%d,x%d", xop, d, s), encode(func(a *amd64.Asm) { a.SSEOpS(op, d, s) }), xop, xr(d), xr(s))
			}
		}
	}
	for _, d := range xregs {
		for _, s := range xregs {
			for mode := uint8(0); mode <= 3; mode++ {
				expect(t, "RoundSS", encode(func(a *amd64.Asm) { a.RoundSS(d, s, mode) }), x86asm.ROUNDSS, xr(d), xr(s), int64(mode|8))
			}
		}
	}
	for _, x := range xregs {
		for _, r := range regs {
			expect(t, "Cvttss2si", encode(func(a *amd64.Asm) { a.Cvttss2si(r, x, true) }), x86asm.CVTTSS2SI, r64(r), xr(x))
			expect(t, "Cvttss2si32", encode(func(a *amd64.Asm) { a.Cvttss2si(r, x, false) }), x86asm.CVTTSS2SI, r32(r), xr(x))
			expect(t, "Cvttsd2si32", encode(func(a *amd64.Asm) { a.Cvttsd2si32(r, x) }), x86asm.CVTTSD2SI, r32(r), xr(x))
			expect(t, "Cvtsi2ss", encode(func(a *amd64.Asm) { a.Cvtsi2ss(x, r, false) }), x86asm.CVTSI2SS, xr(x), r32(r))
			expect(t, "Cvtsi2ss wide", encode(func(a *amd64.Asm) { a.Cvtsi2ss(x, r, true) }), x86asm.CVTSI2SS, xr(x), r64(r))
			expect(t, "MovDToX", encode(func(a *amd64.Asm) { a.MovDToX(x, r) }), x86asm.MOVD, xr(x), r32(r))
			expect(t, "MovDFromX", encode(func(a *amd64.Asm) { a.MovDFromX(r, x) }), x86asm.MOVD, r32(r), xr(x))
		}
	}
}

// JmpLong keeps its four bytes of displacement however near its label is,
// so that a table of them has entries of one length.
func TestAMD64JmpLong(t *testing.T) {
	code := encode(func(a *amd64.Asm) {
		l := a.NewLabel()
		a.JmpLong(l)
		a.JmpLong(l)
		a.Bind(l)
		a.Ret()
	})
	if len(code) != 11 {
		t.Fatalf("JmpLong: % x, want two five-byte jumps and a ret", code)
	}
	for n := 1; n <= 9; n++ {
		expect(t, fmt.Sprintf("Nop %d", n), encode(func(a *amd64.Asm) { a.Nop(n) }), x86asm.NOP)
	}
	for i, rel := range []int{5, 0} {
		inst := decodeOne(t, "JmpLong", code[5*i:5*i+5])
		if inst.Op != x86asm.JMP || inst.Args[0] != x86asm.Rel(rel) {
			t.Fatalf("JmpLong %d: %v, want jmp .%+d", i, inst, rel)
		}
	}
}

// The atomic read-modify-writes at a base plus an index, at each width, with
// every register, and the fence.
func TestAMD64WasmAtomics(t *testing.T) {
	sized := func(r amd64.Reg, size uint8) x86asm.Reg {
		switch size {
		case 1:
			return r8(r)
		case 2:
			return r16(r)
		case 4:
			return r32(r)
		}
		return r64(r)
	}
	type form struct {
		name string
		op   x86asm.Op
		lock bool
		emit func(a *amd64.Asm, b, x amd64.Reg, disp int32, r amd64.Reg, size uint8)
	}
	forms := []form{
		{"XchgIdx", x86asm.XCHG, false, (*amd64.Asm).XchgIdx},
		{"LockXaddIdx", x86asm.XADD, true, (*amd64.Asm).LockXaddIdx},
		{"LockCmpxchgIdx", x86asm.CMPXCHG, true, (*amd64.Asm).LockCmpxchgIdx},
	}
	for _, f := range forms {
		for _, size := range []uint8{1, 2, 4, 8} {
			for _, r := range regs {
				for _, b := range regs {
					for _, x := range indexes {
						for _, disp := range []int32{0, -8, 0x7fffffff} {
							name := fmt.Sprintf("%s/%d r%d,[r%d+r%d+%d]", f.name, size, r, b, x, disp)
							code := encode(func(a *amd64.Asm) { f.emit(a, b, x, disp, r, size) })
							expectX(t, name, code, f.op, int(size), memx{r64(b), r64(x), 0, int64(disp)}, sized(r, size))
							inst := decodeOne(t, name, code)
							locked := false
							for _, p := range inst.Prefix {
								if p&0xFF == x86asm.PrefixLOCK&0xFF {
									locked = true
								}
							}
							if locked != f.lock {
								t.Fatalf("%s: % x is %v, LOCK %v, want %v", name, code, inst, locked, f.lock)
							}
						}
					}
				}
			}
		}
	}
	expect(t, "Mfence", encode(func(a *amd64.Asm) { a.Mfence() }), x86asm.MFENCE)
}
