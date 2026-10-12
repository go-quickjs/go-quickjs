// Package amd64 encodes the x86-64 instructions the JIT emits, one function
// per instruction form. It knows nothing of JavaScript or of Go's register
// conventions; callers keep to those (internal/jit/mir). Every form is tested
// against an independent disassembler in internal/jit/verify.
package amd64

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

// Reg is a general-purpose register, in the hardware's numbering.
type Reg uint8

const (
	RAX Reg = iota
	RCX
	RDX
	RBX
	RSP
	RBP
	RSI
	RDI
	R8
	R9
	R10
	R11
	R12
	R13
	R14
	R15
)

// XReg is an SSE register, X0 to X15.
type XReg uint8

// Cond is a condition code, in the hardware's numbering.
type Cond uint8

const (
	CondO  Cond = 0x0
	CondNO Cond = 0x1
	CondB  Cond = 0x2 // below, unsigned <; also carry
	CondAE Cond = 0x3
	CondE  Cond = 0x4
	CondNE Cond = 0x5
	CondBE Cond = 0x6
	CondA  Cond = 0x7
	CondS  Cond = 0x8
	CondNS Cond = 0x9
	CondP  Cond = 0xA // parity: an unordered (NaN) comparison
	CondNP Cond = 0xB
	CondL  Cond = 0xC
	CondGE Cond = 0xD
	CondLE Cond = 0xE
	CondG  Cond = 0xF
)

// Invert returns the opposite condition.
func (c Cond) Invert() Cond { return c ^ 1 }

// Label is a position in the code, bound once.
type Label int

// Asm accumulates code. Branches to labels are patched by Finish.
type Asm struct {
	buf    []byte
	labels []int // offset, or -1 while unbound
	fixups []fixup
}

type fixup struct {
	at    int // where the rel32 is
	label Label
}

// Len is the number of bytes emitted so far.
func (a *Asm) Len() int { return len(a.buf) }

// Reset empties the assembler for the next function, keeping its memory.
func (a *Asm) Reset() {
	a.buf, a.labels, a.fixups = a.buf[:0], a.labels[:0], a.fixups[:0]
}

// Bytes is the memory the assembler keeps for the next function.
func (a *Asm) Bytes() int {
	return cap(a.buf)*int(unsafe.Sizeof(a.buf[:1][0])) + cap(a.labels)*int(unsafe.Sizeof(0)) + cap(a.fixups)*int(unsafe.Sizeof(fixup{}))
}

// NewLabel makes an unbound label.
func (a *Asm) NewLabel() Label {
	a.labels = append(a.labels, -1)
	return Label(len(a.labels) - 1)
}

// Bind places l at the current position.
func (a *Asm) Bind(l Label) {
	if a.labels[l] >= 0 {
		panic(fmt.Sprintf("amd64: label %d bound twice", l))
	}
	a.labels[l] = len(a.buf)
}

// Offset is a bound label's position.
func (a *Asm) Offset(l Label) int { return a.labels[l] }

// Finish patches every branch and returns the code.
func (a *Asm) Finish() ([]byte, error) {
	for _, f := range a.fixups {
		to := a.labels[f.label]
		if to < 0 {
			return nil, fmt.Errorf("amd64: label %d never bound", f.label)
		}
		binary.LittleEndian.PutUint32(a.buf[f.at:], uint32(int32(to-(f.at+4))))
	}
	return a.buf, nil
}

// emit, emit2 and emit3 append one, two and three bytes: appends of a
// fixed number, which the compiler makes a capacity check and stores, not
// a variadic append's copy of a slice made for it.
func (a *Asm) emit(b byte) { a.buf = append(a.buf, b) }

func (a *Asm) emit2(b0, b1 byte) { a.buf = append(a.buf, b0, b1) }

func (a *Asm) emit3(b0, b1, b2 byte) { a.buf = append(a.buf, b0, b1, b2) }

func (a *Asm) imm32(v int32) { a.buf = binary.LittleEndian.AppendUint32(a.buf, uint32(v)) }

// rex emits a REX prefix when one is needed: w for 64-bit operands, r for
// the ModRM reg field, b for the r/m (or base) field. force emits one even
// without bits, so that SPL, BPL, SIL and DIL are byte registers rather
// than AH, CH, DH and BH.
func (a *Asm) rex(w bool, r, b byte, force bool) {
	v := byte(0x40)
	if w {
		v |= 8
	}
	if r&8 != 0 {
		v |= 4
	}
	if b&8 != 0 {
		v |= 1
	}
	if v != 0x40 || force {
		a.emit(v)
	}
}

// modrmRR is ModRM for two registers.
func (a *Asm) modrmRR(reg, rm byte) { a.emit(0xC0 | (reg&7)<<3 | rm&7) }

// modrmMem is ModRM (and SIB) for [base + disp]: a byte of displacement
// where it fits, as most of a frame's slots' do, else four. RSP and R12 as
// a base need a SIB byte; RBP and R13 are fine with a displacement, which
// is always given.
func (a *Asm) modrmMem(reg byte, base Reg, disp int32) {
	short := disp == int32(int8(disp))
	mod := byte(0x80)
	if short {
		mod = 0x40
	}
	a.emit(mod | (reg&7)<<3 | byte(base)&7)
	if base&7 == 4 {
		a.emit(0x24)
	}
	if short {
		a.emit(byte(disp))
	} else {
		a.imm32(disp)
	}
}

// Ret returns.
func (a *Asm) Ret() { a.emit(0xC3) }

// Jmp jumps to l.
func (a *Asm) Jmp(l Label) {
	a.emit(0xE9)
	a.fixups = append(a.fixups, fixup{len(a.buf), l})
	a.imm32(0)
}

// JmpReg jumps to the address in r.
func (a *Asm) JmpReg(r Reg) {
	if r >= 8 {
		a.emit(0x41)
	}
	a.emit2(0xFF, 0xE0|byte(r&7))
}

// LeaLabel is dst = the address l is bound at, RIP-relative.
func (a *Asm) LeaLabel(dst Reg, l Label) {
	rex := byte(0x48)
	if dst >= 8 {
		rex |= 0x04
	}
	a.emit3(rex, 0x8D, byte(dst&7)<<3|5)
	a.fixups = append(a.fixups, fixup{len(a.buf), l})
	a.imm32(0)
}

// Jcc jumps to l if c holds.
func (a *Asm) Jcc(c Cond, l Label) {
	a.emit2(0x0F, 0x80|byte(c))
	a.fixups = append(a.fixups, fixup{len(a.buf), l})
	a.imm32(0)
}

// MovRR is dst = src, 64 bits.
func (a *Asm) MovRR(dst, src Reg) {
	a.rex(true, byte(src), byte(dst), false)
	a.emit(0x89)
	a.modrmRR(byte(src), byte(dst))
}

// MovRR32 is dst = src, 32 bits, clearing dst's upper half.
func (a *Asm) MovRR32(dst, src Reg) {
	a.rex(false, byte(src), byte(dst), false)
	a.emit(0x89)
	a.modrmRR(byte(src), byte(dst))
}

// MovImm sets dst to a 64-bit constant, in the shortest form: a 32-bit move
// for values that zero-extend, a sign-extended 32-bit move, or movabs.
func (a *Asm) MovImm(dst Reg, v uint64) {
	switch {
	case v>>32 == 0:
		a.rex(false, 0, byte(dst), false)
		a.emit(0xB8 + byte(dst)&7)
		a.imm32(int32(uint32(v)))
	case int64(v) == int64(int32(v)):
		a.rex(true, 0, byte(dst), false)
		a.emit(0xC7)
		a.modrmRR(0, byte(dst))
		a.imm32(int32(v))
	default:
		a.rex(true, 0, byte(dst), false)
		a.emit(0xB8 + byte(dst)&7)
		a.buf = binary.LittleEndian.AppendUint64(a.buf, v)
	}
}

// MovAddr sets dst to a 64-bit constant in ten bytes, whatever its value:
// an address, so that the code's length does not depend on where memory
// is.
func (a *Asm) MovAddr(dst Reg, v uint64) {
	a.rex(true, 0, byte(dst), false)
	a.emit(0xB8 + byte(dst)&7)
	a.buf = binary.LittleEndian.AppendUint64(a.buf, v)
}

// Load is dst = [base + disp], 64 bits.
func (a *Asm) Load(dst, base Reg, disp int32) {
	a.rex(true, byte(dst), byte(base), false)
	a.emit(0x8B)
	a.modrmMem(byte(dst), base, disp)
}

// Lea is dst = base + disp.
func (a *Asm) Lea(dst, base Reg, disp int32) {
	a.rex(true, byte(dst), byte(base), false)
	a.emit(0x8D)
	a.modrmMem(byte(dst), base, disp)
}

// LoadU32 is dst = [base + disp], 32 bits, zero-extended.
func (a *Asm) LoadU32(dst, base Reg, disp int32) {
	a.rex(false, byte(dst), byte(base), false)
	a.emit(0x8B)
	a.modrmMem(byte(dst), base, disp)
}

// Cqo sign-extends RAX into RDX.
func (a *Asm) Cqo() { a.emit2(0x48, 0x99) }

// Idiv divides RDX:RAX by src, signed, 64 bits: RAX is the quotient and RDX
// the remainder. It faults on a zero divisor, or a quotient past 64 bits.
func (a *Asm) Idiv(src Reg) {
	a.rex(true, 0, byte(src), false)
	a.emit(0xF7)
	a.modrmRR(7, byte(src))
}

// LoadU16 is dst = [base + disp], 16 bits, zero-extended.
func (a *Asm) LoadU16(dst, base Reg, disp int32) {
	a.rex(false, byte(dst), byte(base), false)
	a.emit2(0x0F, 0xB7)
	a.modrmMem(byte(dst), base, disp)
}

// LoadU8 is dst = [base + disp], a byte, zero-extended.
func (a *Asm) LoadU8(dst, base Reg, disp int32) {
	a.rex(false, byte(dst), byte(base), false)
	a.emit2(0x0F, 0xB6)
	a.modrmMem(byte(dst), base, disp)
}

// Store is [base + disp] = src, 64 bits.
func (a *Asm) Store(base Reg, disp int32, src Reg) {
	a.rex(true, byte(src), byte(base), false)
	a.emit(0x89)
	a.modrmMem(byte(src), base, disp)
}

// DecMem decrements the 64-bit integer at [base + disp], setting the flags.
func (a *Asm) DecMem(base Reg, disp int32) {
	a.rex(true, 0, byte(base), false)
	a.emit(0xFF)
	a.modrmMem(1, base, disp)
}

// ALU is a two-register integer operation.
type ALU uint8

const (
	Add ALU = 0x01
	Or  ALU = 0x09
	And ALU = 0x21
	Sub ALU = 0x29
	Xor ALU = 0x31
	Cmp ALU = 0x39
	// Test sets the flags for dst & src.
	Test ALU = 0x85
)

// subcode is an ALU's ModRM extension in its immediate form.
func (op ALU) subcode() byte {
	switch op {
	case Add:
		return 0
	case Or:
		return 1
	case And:
		return 4
	case Sub:
		return 5
	case Xor:
		return 6
	case Cmp:
		return 7
	}
	panic("amd64: no immediate form")
}

// Op is dst op= src, 64 bits if wide, else 32 (clearing dst's upper half,
// except for Cmp and Test, which only set flags).
func (a *Asm) Op(op ALU, dst, src Reg, wide bool) {
	a.rex(wide, byte(src), byte(dst), false)
	a.emit(byte(op))
	a.modrmRR(byte(src), byte(dst))
}

// OpImm is dst op= imm, sign-extended to 64 bits if wide.
func (a *Asm) OpImm(op ALU, dst Reg, imm int32, wide bool) {
	a.rex(wide, 0, byte(dst), false)
	a.emit(0x81)
	a.modrmRR(op.subcode(), byte(dst))
	a.imm32(imm)
}

// Shift is a shift's ModRM extension.
type Shift uint8

const (
	Shl Shift = 4
	Shr Shift = 5 // logical
	Sar Shift = 7 // arithmetic
)

// ShiftCL shifts dst by CL (masked by the hardware to five bits for 32-bit
// operands, six for 64), 64 bits if wide.
func (a *Asm) ShiftCL(op Shift, dst Reg, wide bool) {
	a.rex(wide, 0, byte(dst), false)
	a.emit(0xD3)
	a.modrmRR(byte(op), byte(dst))
}

// ShiftImm shifts dst by a constant, 64 bits if wide.
func (a *Asm) ShiftImm(op Shift, dst Reg, n uint8, wide bool) {
	a.rex(wide, 0, byte(dst), false)
	a.emit(0xC1)
	a.modrmRR(byte(op), byte(dst))
	a.emit(n)
}

// Not32 is dst = ^dst, 32 bits.
func (a *Asm) Not32(dst Reg) {
	a.rex(false, 0, byte(dst), false)
	a.emit(0xF7)
	a.modrmRR(2, byte(dst))
}

// Setcc sets dst's low byte to 1 if c holds, else 0; MovZX8 widens it.
func (a *Asm) Setcc(c Cond, dst Reg) {
	a.rex(false, 0, byte(dst), dst >= 4)
	a.emit2(0x0F, 0x90|byte(c))
	a.modrmRR(0, byte(dst))
}

// MovZX8 is dst = src's low byte, zero-extended.
func (a *Asm) MovZX8(dst, src Reg) {
	a.rex(false, byte(dst), byte(src), src >= 4)
	a.emit2(0x0F, 0xB6)
	a.modrmRR(byte(dst), byte(src))
}

// MovQToX moves 64 bits from a general register to an SSE register.
func (a *Asm) MovQToX(dst XReg, src Reg) {
	a.emit(0x66)
	a.rex(true, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x6E)
	a.modrmRR(byte(dst), byte(src))
}

// MovQFromX moves 64 bits from an SSE register to a general register.
func (a *Asm) MovQFromX(dst Reg, src XReg) {
	a.emit(0x66)
	a.rex(true, byte(src), byte(dst), false)
	a.emit2(0x0F, 0x7E)
	a.modrmRR(byte(src), byte(dst))
}

// LoadSD is dst = the double at [base + disp].
func (a *Asm) LoadSD(dst XReg, base Reg, disp int32) {
	a.emit(0xF2)
	a.rex(false, byte(dst), byte(base), false)
	a.emit2(0x0F, 0x10)
	a.modrmMem(byte(dst), base, disp)
}

// StoreSD is [base + disp] = the double in src.
func (a *Asm) StoreSD(base Reg, disp int32, src XReg) {
	a.emit(0xF2)
	a.rex(false, byte(src), byte(base), false)
	a.emit2(0x0F, 0x11)
	a.modrmMem(byte(src), base, disp)
}

// SSE is a two-register double operation.
type SSE uint8

const (
	AddSD   SSE = 0x58
	MulSD   SSE = 0x59
	SubSD   SSE = 0x5C
	DivSD   SSE = 0x5E
	UcomiSD SSE = 0x2E // sets ZF, PF and CF; PF on an unordered (NaN) pair
	XorPD   SSE = 0x57
	MovAPD  SSE = 0x28 // a register copy
	SqrtSD  SSE = 0x51
	AndPD   SSE = 0x54
	OrPD    SSE = 0x56
)

// SSEOp is dst op= src (for UcomiSD, compares dst with src).
func (a *Asm) SSEOp(op SSE, dst, src XReg) {
	switch op {
	case AddSD, MulSD, SubSD, DivSD, SqrtSD:
		a.emit(0xF2)
	default:
		a.emit(0x66)
	}
	a.rex(false, byte(dst), byte(src), false)
	a.emit2(0x0F, byte(op))
	a.modrmRR(byte(dst), byte(src))
}

// Rounding modes of RoundSD.
const (
	RoundFloor = 1
	RoundCeil  = 2
	RoundTrunc = 3
)

// RoundSD rounds src to an integer in dst, as mode says (RoundFloor...),
// an inexact result unreported: SSE4.1's ROUNDSD.
func (a *Asm) RoundSD(dst, src XReg, mode uint8) {
	a.emit(0x66)
	a.rex(false, byte(dst), byte(src), false)
	a.emit3(0x0F, 0x3A, 0x0B)
	a.modrmRR(byte(dst), byte(src))
	a.emit(mode | 8)
}

// Cvtsd2ss converts a double to the nearest float, in dst's low 32 bits.
func (a *Asm) Cvtsd2ss(dst, src XReg) {
	a.emit(0xF2)
	a.rex(false, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x5A)
	a.modrmRR(byte(dst), byte(src))
}

// Cvtss2sd converts a float, src's low 32 bits, to a double.
func (a *Asm) Cvtss2sd(dst, src XReg) {
	a.emit(0xF3)
	a.rex(false, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x5A)
	a.modrmRR(byte(dst), byte(src))
}

// Cvttsd2si truncates a double to a signed 64-bit integer. Out of range,
// NaN and infinities give 0x8000000000000000.
func (a *Asm) Cvttsd2si(dst Reg, src XReg) {
	a.emit(0xF2)
	a.rex(true, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x2C)
	a.modrmRR(byte(dst), byte(src))
}

// Cvtsi2sd converts a signed integer, 64 bits if wide, else 32, to a double.
func (a *Asm) Cvtsi2sd(dst XReg, src Reg, wide bool) {
	a.emit(0xF2)
	a.rex(wide, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x2A)
	a.modrmRR(byte(dst), byte(src))
}
