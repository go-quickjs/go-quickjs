package amd64

// The forms below are those WebAssembly's baseline code needs beyond the
// JavaScript JIT's: 32-bit division, bit counts, conditional moves, sign
// extensions of registers, rotations, single-precision SSE, loads and
// stores at a base plus a scaled index plus a displacement, and the atomic
// read-modify-writes and the fence of the threads proposal.

// Rotations, in the shift forms' ModRM extension (ShiftCL, ShiftImm).
const (
	Rol Shift = 0
	Ror Shift = 1
)

// rexX emits a REX prefix when one is needed, as rex does, with x for the
// SIB index field.
func (a *Asm) rexSIB(w bool, r, x, b byte, force bool) {
	v := byte(0x40)
	if w {
		v |= 8
	}
	if r&8 != 0 {
		v |= 4
	}
	if x&8 != 0 {
		v |= 2
	}
	if b&8 != 0 {
		v |= 1
	}
	if v != 0x40 || force {
		a.emit(v)
	}
}

// modrmSIB is ModRM and SIB for [base + index<<scale + disp], scale 0 to 3:
// a byte of displacement where it fits, else four, always given, so that
// RBP and R13 serve as bases. index must not be RSP, which the SIB byte
// takes as no index.
func (a *Asm) modrmSIB(reg byte, base, index Reg, scale uint8, disp int32) {
	if index == RSP {
		panic("amd64: RSP cannot be an index")
	}
	short := disp == int32(int8(disp))
	mod := byte(0x80)
	if short {
		mod = 0x40
	}
	a.emit2(mod|(reg&7)<<3|4, scale<<6|byte(index&7)<<3|byte(base&7))
	if short {
		a.emit(byte(disp))
	} else {
		a.imm32(disp)
	}
}

// Cdq sign-extends EAX into EDX.
func (a *Asm) Cdq() { a.emit(0x99) }

// Idiv32 divides EDX:EAX by src, signed, 32 bits: EAX is the quotient and
// EDX the remainder, each zero-extended to 64 bits. It faults on a zero
// divisor, or a quotient past 32 bits.
func (a *Asm) Idiv32(src Reg) {
	a.rex(false, 0, byte(src), false)
	a.emit(0xF7)
	a.modrmRR(7, byte(src))
}

// Div32 divides EDX:EAX by src, unsigned, 32 bits, as Idiv32 does.
func (a *Asm) Div32(src Reg) {
	a.rex(false, 0, byte(src), false)
	a.emit(0xF7)
	a.modrmRR(6, byte(src))
}

// Bsf is dst = the index of src's lowest set bit, 64 bits if wide, else 32.
// Of a src of 0 it sets ZF and leaves dst undefined.
func (a *Asm) Bsf(dst, src Reg, wide bool) {
	a.rex(wide, byte(dst), byte(src), false)
	a.emit2(0x0F, 0xBC)
	a.modrmRR(byte(dst), byte(src))
}

// Popcnt is dst = the number of src's set bits, 64 bits if wide, else 32.
// The caller checks that the CPU has it.
func (a *Asm) Popcnt(dst, src Reg, wide bool) {
	a.emit(0xF3)
	a.rex(wide, byte(dst), byte(src), false)
	a.emit2(0x0F, 0xB8)
	a.modrmRR(byte(dst), byte(src))
}

// Cmov is dst = src if c holds, 64 bits if wide, else 32: a 32-bit move
// clears dst's upper half whether or not c holds.
func (a *Asm) Cmov(c Cond, dst, src Reg, wide bool) {
	a.rex(wide, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x40|byte(c))
	a.modrmRR(byte(dst), byte(src))
}

// Movsx8 is dst = src's low byte, sign-extended to 64 bits if wide, else to
// 32 and the upper half cleared.
func (a *Asm) Movsx8(dst, src Reg, wide bool) {
	a.rex(wide, byte(dst), byte(src), src >= 4)
	a.emit2(0x0F, 0xBE)
	a.modrmRR(byte(dst), byte(src))
}

// Movsx16 is dst = src's low 16 bits, sign-extended as Movsx8 extends.
func (a *Asm) Movsx16(dst, src Reg, wide bool) {
	a.rex(wide, byte(dst), byte(src), false)
	a.emit2(0x0F, 0xBF)
	a.modrmRR(byte(dst), byte(src))
}

// JmpLong jumps to l with a 32-bit displacement that Finish never shortens:
// an entry of a table of jumps, each the same length.
func (a *Asm) JmpLong(l Label) {
	a.emit(0xE9)
	// A displacement Finish keeps at 32 bits and patches to its label, as a
	// lea's: relative to the end of the four bytes, the instruction's end.
	a.fix(l, fixLea, 0)
	a.imm32(0)
}

// JmpNear jumps to l with a byte's displacement, which Finish patches and
// never lengthens: l is within a byte's displacement of the jump's end as
// the code is emitted, and Finish only shortens the code between, else
// Finish fails. The jump is no candidate for Finish's rounds, which a jump
// of 32 bits that would be shortened is.
func (a *Asm) JmpNear(l Label) {
	a.emit(0xEB)
	a.fix(l, fixJmp8, 0)
	a.emit(0)
}

// Nop emits one NOP of n bytes, n at most 9: padding that keeps the
// entries of a table of jumps the same length.
func (a *Asm) Nop(n int) { a.buf = append(a.buf, nops[n]...) }

// StoreImm32 stores imm at [base + disp], 32 bits.
func (a *Asm) StoreImm32(base Reg, disp int32, imm int32) {
	a.rex(false, 0, byte(base), false)
	a.emit(0xC7)
	a.modrmMem(0, base, disp)
	a.imm32(imm)
}

// LoadIdxScaled is dst = [base + index<<scale + disp], 64 bits: an entry of
// a table of words.
func (a *Asm) LoadIdxScaled(dst, base, index Reg, scale uint8, disp int32) {
	a.rexSIB(true, byte(dst), byte(index), byte(base), false)
	a.emit(0x8B)
	a.modrmSIB(byte(dst), base, index, scale, disp)
}

// LoadU32Idx is dst = [base + index + disp], 32 bits, zero-extended.
func (a *Asm) LoadU32Idx(dst, base, index Reg, disp int32) {
	a.rexSIB(false, byte(dst), byte(index), byte(base), false)
	a.emit(0x8B)
	a.modrmSIB(byte(dst), base, index, 0, disp)
}

// LoadU16Idx is dst = [base + index + disp], 16 bits, zero-extended.
func (a *Asm) LoadU16Idx(dst, base, index Reg, disp int32) {
	a.rexSIB(false, byte(dst), byte(index), byte(base), false)
	a.emit2(0x0F, 0xB7)
	a.modrmSIB(byte(dst), base, index, 0, disp)
}

// LoadU8Idx is dst = [base + index + disp], a byte, zero-extended.
func (a *Asm) LoadU8Idx(dst, base, index Reg, disp int32) {
	a.rexSIB(false, byte(dst), byte(index), byte(base), false)
	a.emit2(0x0F, 0xB6)
	a.modrmSIB(byte(dst), base, index, 0, disp)
}

// LoadS8Idx is dst = [base + index + disp], a byte, sign-extended to 64
// bits if wide, else to 32, the upper half cleared.
func (a *Asm) LoadS8Idx(dst, base, index Reg, disp int32, wide bool) {
	a.rexSIB(wide, byte(dst), byte(index), byte(base), false)
	a.emit2(0x0F, 0xBE)
	a.modrmSIB(byte(dst), base, index, 0, disp)
}

// LoadS16Idx is dst = [base + index + disp], 16 bits, sign-extended as
// LoadS8Idx extends.
func (a *Asm) LoadS16Idx(dst, base, index Reg, disp int32, wide bool) {
	a.rexSIB(wide, byte(dst), byte(index), byte(base), false)
	a.emit2(0x0F, 0xBF)
	a.modrmSIB(byte(dst), base, index, 0, disp)
}

// LoadS32Idx is dst = [base + index + disp], 32 bits, sign-extended to 64.
func (a *Asm) LoadS32Idx(dst, base, index Reg, disp int32) {
	a.rexSIB(true, byte(dst), byte(index), byte(base), false)
	a.emit(0x63)
	a.modrmSIB(byte(dst), base, index, 0, disp)
}

// Store32Idx is [base + index + disp] = src's low 32 bits.
func (a *Asm) Store32Idx(base, index Reg, disp int32, src Reg) {
	a.rexSIB(false, byte(src), byte(index), byte(base), false)
	a.emit(0x89)
	a.modrmSIB(byte(src), base, index, 0, disp)
}

// Store16Idx is [base + index + disp] = src's low 16 bits.
func (a *Asm) Store16Idx(base, index Reg, disp int32, src Reg) {
	a.emit(0x66)
	a.rexSIB(false, byte(src), byte(index), byte(base), false)
	a.emit(0x89)
	a.modrmSIB(byte(src), base, index, 0, disp)
}

// Store8Idx is [base + index + disp] = src's low byte.
func (a *Asm) Store8Idx(base, index Reg, disp int32, src Reg) {
	a.rexSIB(false, byte(src), byte(index), byte(base), src >= 4)
	a.emit(0x88)
	a.modrmSIB(byte(src), base, index, 0, disp)
}

// SSEOpS is the single-precision form of op, dst op= src: ADDSS for AddSD,
// and so on; UCOMISS for UcomiSD; ANDPS, ORPS, XORPS and MOVAPS for the
// bitwise operations and the copy, which act on the whole register.
func (a *Asm) SSEOpS(op SSE, dst, src XReg) {
	switch op {
	case AddSD, MulSD, SubSD, DivSD, SqrtSD:
		a.emit(0xF3)
	}
	a.rex(false, byte(dst), byte(src), false)
	a.emit2(0x0F, byte(op))
	a.modrmRR(byte(dst), byte(src))
}

// RoundSS rounds the float in src to an integer in dst, as RoundSD rounds a
// double: SSE4.1's ROUNDSS.
func (a *Asm) RoundSS(dst, src XReg, mode uint8) {
	a.emit(0x66)
	a.rex(false, byte(dst), byte(src), false)
	a.emit3(0x0F, 0x3A, 0x0A)
	a.modrmRR(byte(dst), byte(src))
	a.emit(mode | 8)
}

// Cvttss2si truncates a float to a signed integer, 64 bits if wide, else 32.
// Out of range, NaN and infinities give the integer's lowest value.
func (a *Asm) Cvttss2si(dst Reg, src XReg, wide bool) {
	a.emit(0xF3)
	a.rex(wide, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x2C)
	a.modrmRR(byte(dst), byte(src))
}

// Cvttsd2si32 truncates a double to a signed 32-bit integer, dst's upper half
// cleared. Out of range, NaN and infinities give 0x80000000.
func (a *Asm) Cvttsd2si32(dst Reg, src XReg) {
	a.emit(0xF2)
	a.rex(false, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x2C)
	a.modrmRR(byte(dst), byte(src))
}

// Cvtsi2ss converts a signed integer, 64 bits if wide, else 32, to the
// nearest float.
func (a *Asm) Cvtsi2ss(dst XReg, src Reg, wide bool) {
	a.emit(0xF3)
	a.rex(wide, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x2A)
	a.modrmRR(byte(dst), byte(src))
}

// MovDToX moves 32 bits from a general register to an SSE register, zeroing
// the rest of it.
func (a *Asm) MovDToX(dst XReg, src Reg) {
	a.emit(0x66)
	a.rex(false, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x6E)
	a.modrmRR(byte(dst), byte(src))
}

// MovDFromX moves an SSE register's low 32 bits to a general register,
// zero-extended.
func (a *Asm) MovDFromX(dst Reg, src XReg) {
	a.emit(0x66)
	a.rex(false, byte(src), byte(dst), false)
	a.emit2(0x0F, 0x7E)
	a.modrmRR(byte(src), byte(dst))
}

// LoadSS is dst = the float at [base + disp], the rest of dst zeroed.
func (a *Asm) LoadSS(dst XReg, base Reg, disp int32) {
	a.emit(0xF3)
	a.rex(false, byte(dst), byte(base), false)
	a.emit2(0x0F, 0x10)
	a.modrmMem(byte(dst), base, disp)
}

// LoadSSIdx is dst = the float at [base + index + disp].
func (a *Asm) LoadSSIdx(dst XReg, base, index Reg, disp int32) {
	a.emit(0xF3)
	a.rexSIB(false, byte(dst), byte(index), byte(base), false)
	a.emit2(0x0F, 0x10)
	a.modrmSIB(byte(dst), base, index, 0, disp)
}

// LoadSDIdx is dst = the double at [base + index + disp].
func (a *Asm) LoadSDIdx(dst XReg, base, index Reg, disp int32) {
	a.emit(0xF2)
	a.rexSIB(false, byte(dst), byte(index), byte(base), false)
	a.emit2(0x0F, 0x10)
	a.modrmSIB(byte(dst), base, index, 0, disp)
}

// StoreSSIdx is [base + index + disp] = the float in src.
func (a *Asm) StoreSSIdx(base, index Reg, disp int32, src XReg) {
	a.emit(0xF3)
	a.rexSIB(false, byte(src), byte(index), byte(base), false)
	a.emit2(0x0F, 0x11)
	a.modrmSIB(byte(src), base, index, 0, disp)
}

// ---------------------------------------------------------------- atomics

// atomicIdx emits a read-modify-write of the size bytes (1, 2, 4 or 8) at
// [base + index + disp] with register r: op8 is the byte form's opcode, op
// the others', after 0F if twoByte, and the LOCK prefix first if lock.
func (a *Asm) atomicIdx(lock, twoByte bool, op8, op byte, size uint8, base, index Reg, disp int32, r Reg) {
	if lock {
		a.emit(0xF0)
	}
	if size == 2 {
		a.emit(0x66)
	}
	a.rexSIB(size == 8, byte(r), byte(index), byte(base), size == 1 && r >= 4)
	if twoByte {
		a.emit(0x0F)
	}
	if size == 1 {
		a.emit(op8)
	} else {
		a.emit(op)
	}
	a.modrmSIB(byte(r), base, index, 0, disp)
}

// XchgIdx exchanges r's low size bytes (1, 2, 4 or 8) with those at
// [base + index + disp], atomically (XCHG, which locks without a prefix):
// a sequentially consistent store, and the load of the old value. A 4-byte
// exchange clears r's upper half; a narrower one leaves r's other bits.
func (a *Asm) XchgIdx(base, index Reg, disp int32, r Reg, size uint8) {
	a.atomicIdx(false, false, 0x86, 0x87, size, base, index, disp, r)
}

// LockXaddIdx adds r's low size bytes to those at [base + index + disp]
// atomically (LOCK XADD), leaving the old value in them, as XchgIdx does.
func (a *Asm) LockXaddIdx(base, index Reg, disp int32, r Reg, size uint8) {
	a.atomicIdx(true, true, 0xC0, 0xC1, size, base, index, disp, r)
}

// LockCmpxchgIdx compares the size bytes at [base + index + disp] with
// AX's and, if they are equal, stores r's there and sets ZF, else loads
// them into AX's and clears ZF, atomically (LOCK CMPXCHG). A 4-byte load
// clears AX's upper half; a narrower one leaves AX's other bits.
func (a *Asm) LockCmpxchgIdx(base, index Reg, disp int32, r Reg, size uint8) {
	a.atomicIdx(true, true, 0xB0, 0xB1, size, base, index, disp, r)
}

// Mfence orders every load and store before it before every one after it
// (MFENCE).
func (a *Asm) Mfence() { a.emit3(0x0F, 0xAE, 0xF0) }
