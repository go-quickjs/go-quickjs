package arm64

// The forms below are those a baseline WebAssembly compiler
// (wasm/internal/jit) needs beyond the JavaScript JIT's: logical
// immediates, rotations, bit counts, conditional selects, 32-bit division,
// sign extensions of bytes and half-words, loads and stores at a base plus
// an index register, and single-precision arithmetic, comparisons,
// roundings and conversions.

// ---------------------------------------------------------------- logical immediates

// LogicImm is dst = n op imm for op And, Orr, Eor or Ands, as one
// instruction with a logical immediate, reporting whether imm has one
// (LogicalImm); if not, it emits nothing. A 32-bit operation's imm is its
// low 32 bits.
func (a *Asm) LogicImm(op ALU, dst, n Reg, imm uint64, wide bool) bool {
	if !wide {
		imm = uint64(uint32(imm))
	}
	f, ok := LogicalImm(imm, wide)
	if !ok {
		return false
	}
	var base uint32
	switch op {
	case And:
		base = 0x12000000
	case Orr:
		base = 0x32000000
	case Eor:
		base = 0x52000000
	case Ands:
		base = 0x72000000
	default:
		panic("arm64: no logical immediate form")
	}
	a.emit(base | sf(wide) | f<<10 | uint32(n)<<5 | uint32(dst))
	return true
}

// ---------------------------------------------------------------- integer

// Ror is a rotation right by a register's count, modulo the width
// (RORV), for ShiftReg and ShiftImm.
const Ror Shift = 0x1AC02C00

// rorImm is dst = n rotated right by count (EXTR n, n).
func (a *Asm) rorImm(dst, n Reg, count uint8, wide bool) {
	if wide {
		a.emit(0x93C00000 | uint32(n)<<16 | uint32(count&63)<<10 | uint32(n)<<5 | uint32(dst))
		return
	}
	a.emit(0x13800000 | uint32(n)<<16 | uint32(count&31)<<10 | uint32(n)<<5 | uint32(dst))
}

// Clz is dst = the count of n's leading zero bits, 64 bits or 32.
func (a *Asm) Clz(dst, n Reg, wide bool) {
	a.emit(0x5AC01000 | sf(wide) | uint32(n)<<5 | uint32(dst))
}

// Rbit is dst = n's bits reversed, 64 bits or 32.
func (a *Asm) Rbit(dst, n Reg, wide bool) {
	a.emit(0x5AC00000 | sf(wide) | uint32(n)<<5 | uint32(dst))
}

// Csel is dst = n if c holds, else m.
func (a *Asm) Csel(dst, n, m Reg, c Cond, wide bool) {
	a.emit(0x1A800000 | sf(wide) | uint32(m)<<16 | uint32(c)<<12 | uint32(n)<<5 | uint32(dst))
}

// Sxtb and Sxth are dst = n's low byte or half-word, sign-extended to 64
// bits or to 32 (the upper half then cleared).
func (a *Asm) Sxtb(dst, n Reg, wide bool) {
	if wide {
		a.emit(0x93401C00 | uint32(n)<<5 | uint32(dst))
		return
	}
	a.emit(0x13001C00 | uint32(n)<<5 | uint32(dst))
}

func (a *Asm) Sxth(dst, n Reg, wide bool) {
	if wide {
		a.emit(0x93403C00 | uint32(n)<<5 | uint32(dst))
		return
	}
	a.emit(0x13003C00 | uint32(n)<<5 | uint32(dst))
}

// SdivW and UdivW are dst = n / m, 32 bits, signed and unsigned: 0 when m
// is 0, and n when n is -2**31 and m is -1; they never fault.
func (a *Asm) SdivW(dst, n, m Reg) { a.emit(0x1AC00C00 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst)) }

func (a *Asm) UdivW(dst, n, m Reg) { a.emit(0x1AC00800 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst)) }

// MsubW is dst = acc - n*m, 32 bits.
func (a *Asm) MsubW(dst, n, m, acc Reg) {
	a.emit(0x1B008000 | uint32(m)<<16 | uint32(acc)<<10 | uint32(n)<<5 | uint32(dst))
}

// ---------------------------------------------------------------- indexed memory

// memIdx emits the register-offset form of a load or store whose
// unsigned-offset form is scaled: the address is base + idx, idx taken
// whole (LSL #0).
func (a *Asm) memIdx(scaled uint32, r, base, idx Reg) {
	if base == ZR || idx == ZR {
		panic("arm64: SP or ZR as an address register")
	}
	a.emit(scaled&^(1<<24) | 1<<21 | uint32(idx)<<16 | 3<<13 | 2<<10 | uint32(base)<<5 | uint32(r))
}

// LoadU32Idx, LoadU16Idx and LoadU8Idx are dst = [base + idx], 32 bits,
// 16 or a byte, zero-extended.
func (a *Asm) LoadU32Idx(dst, base, idx Reg) { a.memIdx(0xB9400000, dst, base, idx) }

func (a *Asm) LoadU16Idx(dst, base, idx Reg) { a.memIdx(0x79400000, dst, base, idx) }

func (a *Asm) LoadU8Idx(dst, base, idx Reg) { a.memIdx(0x39400000, dst, base, idx) }

// LoadS8Idx and LoadS16Idx are dst = [base + idx], a byte or 16 bits,
// sign-extended to 64 bits, or to 32 with the upper half cleared;
// LoadS32Idx sign-extends 32 bits to 64.
func (a *Asm) LoadS8Idx(dst, base, idx Reg, wide bool) {
	if wide {
		a.memIdx(0x39800000, dst, base, idx)
		return
	}
	a.memIdx(0x39C00000, dst, base, idx)
}

func (a *Asm) LoadS16Idx(dst, base, idx Reg, wide bool) {
	if wide {
		a.memIdx(0x79800000, dst, base, idx)
		return
	}
	a.memIdx(0x79C00000, dst, base, idx)
}

func (a *Asm) LoadS32Idx(dst, base, idx Reg) { a.memIdx(0xB9800000, dst, base, idx) }

// StoreIdx, Store32Idx, Store16Idx and Store8Idx are [base + idx] = src,
// 64 bits, or its low 32, 16 or 8.
func (a *Asm) StoreIdx(base, idx, src Reg) { a.memIdx(0xF9000000, src, base, idx) }

func (a *Asm) Store32Idx(base, idx, src Reg) { a.memIdx(0xB9000000, src, base, idx) }

func (a *Asm) Store16Idx(base, idx, src Reg) { a.memIdx(0x79000000, src, base, idx) }

func (a *Asm) Store8Idx(base, idx, src Reg) { a.memIdx(0x39000000, src, base, idx) }

// LoadSIdx and LoadFIdx are dst = the float or the double at [base + idx];
// StoreSIdx and StoreFIdx store them.
func (a *Asm) LoadSIdx(dst FReg, base, idx Reg) { a.memIdx(0xBD400000, Reg(dst), base, idx) }

func (a *Asm) LoadFIdx(dst FReg, base, idx Reg) { a.memIdx(0xFD400000, Reg(dst), base, idx) }

func (a *Asm) StoreSIdx(base, idx Reg, src FReg) { a.memIdx(0xBD000000, Reg(src), base, idx) }

func (a *Asm) StoreFIdx(base, idx Reg, src FReg) { a.memIdx(0xFD000000, Reg(src), base, idx) }

// ---------------------------------------------------------------- single precision

// single is a double operation's encoding made its single-precision one:
// the type field, bits 23 and 22, 00 for single where 01 is double.
func single(op uint32) uint32 { return op &^ (1 << 22) }

// FArithS is dst = n op m, floats.
func (a *Asm) FArithS(op FOp, dst, n, m FReg) {
	a.emit(single(uint32(op)) | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// FCmpS sets the flags for n compared with m, floats.
func (a *Asm) FCmpS(n, m FReg) { a.emit(single(0x1E602000) | uint32(m)<<16 | uint32(n)<<5) }

// FSqrtS is dst = the square root of n, a float.
func (a *Asm) FSqrtS(dst, n FReg) { a.emit(single(0x1E61C000) | uint32(n)<<5 | uint32(dst)) }

// FRintS is dst = n rounded to an integer, as op says, a float.
func (a *Asm) FRintS(op FRound, dst, n FReg) { a.emit(single(uint32(op)) | uint32(n)<<5 | uint32(dst)) }

// FCsel is dst = n if c holds, else m: doubles, or floats when single.
func (a *Asm) FCsel(dst, n, m FReg, c Cond, isSingle bool) {
	op := uint32(0x1E600C00)
	if isSingle {
		op = single(op)
	}
	a.emit(op | uint32(m)<<16 | uint32(c)<<12 | uint32(n)<<5 | uint32(dst))
}

// FMovToS is dst = src's low 32 bits, as a float (FMOV S, W): the rest of
// dst cleared.
func (a *Asm) FMovToS(dst FReg, src Reg) { a.emit(0x1E270000 | uint32(src)<<5 | uint32(dst)) }

// FMovFromS is dst = the float src's bits, the upper half cleared (FMOV W,
// S).
func (a *Asm) FMovFromS(dst Reg, src FReg) { a.emit(0x1E260000 | uint32(src)<<5 | uint32(dst)) }

// Fcvtz is dst = n truncated toward zero to an integer -- signed or
// unsigned, 64 bits or 32 -- from a double, or a float when isSingle:
// saturating at the integer's bounds, NaN giving 0.
func (a *Asm) Fcvtz(dst Reg, n FReg, signed, wide, isSingle bool) {
	op := uint32(0x1E780000) // FCVTZS, double
	if !signed {
		op |= 1 << 16 // FCVTZU
	}
	if isSingle {
		op = single(op)
	}
	a.emit(op | sf(wide) | uint32(n)<<5 | uint32(dst))
}

// Cvtf is dst = n, a signed or unsigned integer of 64 bits or 32, rounded
// to a double, or a float when isSingle (SCVTF, UCVTF): one rounding.
func (a *Asm) Cvtf(dst FReg, n Reg, signed, wide, isSingle bool) {
	op := uint32(0x1E620000) // SCVTF, double
	if !signed {
		op |= 1 << 16 // UCVTF
	}
	if isSingle {
		op = single(op)
	}
	a.emit(op | sf(wide) | uint32(n)<<5 | uint32(dst))
}

// Cnt8B is dst = the count of set bits of each of n's low eight bytes
// (CNT Vd.8B, Vn.8B), the rest of dst cleared.
func (a *Asm) Cnt8B(dst, n FReg) { a.emit(0x0E205800 | uint32(n)<<5 | uint32(dst)) }

// Addv8B is dst's low byte = the sum of n's low eight bytes (ADDV Bd,
// Vn.8B), the rest of dst cleared.
func (a *Asm) Addv8B(dst, n FReg) { a.emit(0x0E31B800 | uint32(n)<<5 | uint32(dst)) }
