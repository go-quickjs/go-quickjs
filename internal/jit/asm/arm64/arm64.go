// Package arm64 encodes the A64 instructions the new pipeline's arm64 code
// generator (internal/jit/mir) emits: each a 32-bit word, little-endian.
// Golden tests in internal/jit/verify check every encoding against the
// arm64asm disassembler.
package arm64

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

// Reg is a general register, X0 to X30. Register 31 is ZR in the
// instructions that take it as an operand.
type Reg uint8

// The registers the encoder names.
const (
	X0  Reg = 0
	X27 Reg = 27 // the encoder's temporary for offsets an instruction cannot hold
	ZR  Reg = 31
)

// tmp is the register the encoder builds large offsets in: Go's assembler
// temporary, which no caller keeps a value in.
const tmp = X27

// FReg is a floating-point register, D0 to D31.
type FReg uint8

// Cond is a condition code.
type Cond uint8

const (
	EQ Cond = iota
	NE
	HS // unsigned >=, carry set
	LO // unsigned <
	MI // negative; after FCMP, less than (false when unordered)
	PL
	VS // overflow; after FCMP, unordered
	VC
	HI // unsigned >
	LS // unsigned <=; after FCMP, less or equal (false when unordered)
	GE
	LT
	GT
	LE
)

// Invert is the opposite condition.
func (c Cond) Invert() Cond { return c ^ 1 }

// Label names a position in the code.
type Label int

// Asm accumulates instructions and resolves labels.
type Asm struct {
	buf    []uint32
	labels []int
	fixups []fixup
}

type fixup struct {
	at    int // instruction index
	label Label
	kind  int
}

const (
	fixB    = iota // imm26
	fixCond        // imm19 at bit 5
	fixAdr         // ADR's imm21, of bytes: immhi at bit 5, immlo (0) at 29
)

// Len is the bytes emitted so far.
func (a *Asm) Len() int { return 4 * len(a.buf) }

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

// Bind places a label at the next instruction.
func (a *Asm) Bind(l Label) {
	if a.labels[l] >= 0 {
		panic("arm64: label bound twice")
	}
	a.labels[l] = len(a.buf)
}

// Offset is where label l is bound, in bytes from the start.
func (a *Asm) Offset(l Label) int { return 4 * a.labels[l] }

// Finish resolves the labels and returns the code.
func (a *Asm) Finish() ([]byte, error) {
	for _, f := range a.fixups {
		to := a.labels[f.label]
		if to < 0 {
			return nil, fmt.Errorf("arm64: label %d not bound", f.label)
		}
		d := to - f.at
		switch f.kind {
		case fixB:
			if d < -1<<25 || d >= 1<<25 {
				return nil, fmt.Errorf("arm64: branch out of range")
			}
			a.buf[f.at] |= uint32(d) & (1<<26 - 1)
		case fixCond:
			if d < -1<<18 || d >= 1<<18 {
				return nil, fmt.Errorf("arm64: conditional branch out of range")
			}
			a.buf[f.at] |= (uint32(d) & (1<<19 - 1)) << 5
		case fixAdr:
			// d instructions are 4d bytes: immlo is 0, immhi d.
			if d < -1<<18 || d >= 1<<18 {
				return nil, fmt.Errorf("arm64: address out of range")
			}
			a.buf[f.at] |= (uint32(d) & (1<<19 - 1)) << 5
		}
	}
	out := make([]byte, 4*len(a.buf))
	for i, w := range a.buf {
		binary.LittleEndian.PutUint32(out[4*i:], w)
	}
	return out, nil
}

func (a *Asm) emit(w uint32) { a.buf = append(a.buf, w) }

func sf(wide bool) uint32 {
	if wide {
		return 1 << 31
	}
	return 0
}

// ---------------------------------------------------------------- moves

// MovRR is dst = src, 64 bits.
func (a *Asm) MovRR(dst, src Reg) { a.emit(0xAA0003E0 | uint32(src)<<16 | uint32(dst)) }

// MovRR32 is dst = src, 32 bits, clearing dst's upper half.
func (a *Asm) MovRR32(dst, src Reg) { a.emit(0x2A0003E0 | uint32(src)<<16 | uint32(dst)) }

// MovImm sets dst to a 64-bit constant: MOVZ or MOVN, then MOVK for each
// other half-word that is not already right.
func (a *Asm) MovImm(dst Reg, v uint64) {
	zeros, ones := 0, 0
	for i := 0; i < 4; i++ {
		switch uint16(v >> (16 * i)) {
		case 0:
			zeros++
		case 0xFFFF:
			ones++
		}
	}
	fill, first := uint16(0), uint32(0xD2800000) // MOVZ
	if ones > zeros {
		fill, first = 0xFFFF, 0x92800000 // MOVN
	}
	emitted := false
	for i := 0; i < 4; i++ {
		h := uint16(v >> (16 * i))
		if h == fill {
			continue
		}
		if !emitted {
			imm := uint32(h)
			if fill == 0xFFFF {
				imm = uint32(^h)
			}
			a.emit(first | uint32(i)<<21 | imm<<5 | uint32(dst))
			emitted = true
			continue
		}
		a.emit(0xF2800000 | uint32(i)<<21 | uint32(h)<<5 | uint32(dst)) // MOVK
	}
	if !emitted {
		if fill == 0 {
			a.emit(0xD2800000 | uint32(dst)) // MOVZ #0
		} else {
			a.emit(0x92800000 | uint32(dst)) // MOVN #0: all ones
		}
	}
}

// MovAddr sets dst to a 64-bit constant in four instructions, whatever its
// value: an address, so that the code's length does not depend on where
// memory is.
func (a *Asm) MovAddr(dst Reg, v uint64) {
	a.emit(0xD2800000 | uint32(uint16(v))<<5 | uint32(dst)) // MOVZ
	for i := 1; i < 4; i++ {
		a.emit(0xF2800000 | uint32(i)<<21 | uint32(uint16(v>>(16*i)))<<5 | uint32(dst)) // MOVK
	}
}

// ---------------------------------------------------------------- memory

// size is a load's or store's access width.
type size uint32

const (
	b8  size = 0
	b16 size = 1
	b32 size = 2
	b64 size = 3
)

// mem emits a load or store of r at [base + disp]: LDR/STR with a scaled
// unsigned offset when it fits, LDUR/STUR for a small unscaled one, and
// otherwise through tmp.
func (a *Asm) mem(scaled, unscaled uint32, sz size, r, base Reg, disp int32) {
	if base == ZR {
		panic("arm64: SP as a base")
	}
	scale := int32(1) << sz
	if disp >= 0 && disp%scale == 0 && disp/scale < 4096 {
		a.emit(scaled | uint32(disp/scale)<<10 | uint32(base)<<5 | uint32(r))
		return
	}
	if disp >= -256 && disp < 256 {
		a.emit(unscaled | (uint32(disp)&0x1FF)<<12 | uint32(base)<<5 | uint32(r))
		return
	}
	if base == tmp {
		panic("arm64: a large offset from the temporary")
	}
	a.MovImm(tmp, uint64(int64(disp)))
	a.emit(0x8B000000 | uint32(tmp)<<16 | uint32(base)<<5 | uint32(tmp)) // ADD tmp, base, tmp
	a.emit(scaled | uint32(tmp)<<5 | uint32(r))
}

// Load is dst = [base + disp], 64 bits.
func (a *Asm) Load(dst, base Reg, disp int32) { a.mem(0xF9400000, 0xF8400000, b64, dst, base, disp) }

// Store is [base + disp] = src, 64 bits.
func (a *Asm) Store(base Reg, disp int32, src Reg) {
	a.mem(0xF9000000, 0xF8000000, b64, src, base, disp)
}

// LoadU32 is dst = [base + disp], 32 bits, zero-extended.
func (a *Asm) LoadU32(dst, base Reg, disp int32) { a.mem(0xB9400000, 0xB8400000, b32, dst, base, disp) }

// LoadU16 is dst = [base + disp], 16 bits, zero-extended.
func (a *Asm) LoadU16(dst, base Reg, disp int32) { a.mem(0x79400000, 0x78400000, b16, dst, base, disp) }

// LoadU8 is dst = [base + disp], a byte, zero-extended.
func (a *Asm) LoadU8(dst, base Reg, disp int32) { a.mem(0x39400000, 0x38400000, b8, dst, base, disp) }

// LoadS8, LoadS16 and LoadS32 are dst = [base + disp], a byte, 16 bits or
// 32, sign-extended to 64 bits (LDRSB, LDRSH, LDRSW): a typed array's
// signed elements.
func (a *Asm) LoadS8(dst, base Reg, disp int32) { a.mem(0x39800000, 0x38800000, b8, dst, base, disp) }

func (a *Asm) LoadS16(dst, base Reg, disp int32) { a.mem(0x79800000, 0x78800000, b16, dst, base, disp) }

func (a *Asm) LoadS32(dst, base Reg, disp int32) { a.mem(0xB9800000, 0xB8800000, b32, dst, base, disp) }

// Store8, Store16 and Store32 are [base + disp] = src's low byte, 16 bits
// or 32 (STRB, STRH, STR of the W view): a typed array's elements.
func (a *Asm) Store8(base Reg, disp int32, src Reg) {
	a.mem(0x39000000, 0x38000000, b8, src, base, disp)
}

func (a *Asm) Store16(base Reg, disp int32, src Reg) {
	a.mem(0x79000000, 0x78000000, b16, src, base, disp)
}

func (a *Asm) Store32(base Reg, disp int32, src Reg) {
	a.mem(0xB9000000, 0xB8000000, b32, src, base, disp)
}

// LoadS and StoreS are dst = the float at [base + disp], and [base + disp]
// = the float in src (the S views).
func (a *Asm) LoadS(dst FReg, base Reg, disp int32) {
	a.mem(0xBD400000, 0xBC400000, b32, Reg(dst), base, disp)
}

func (a *Asm) StoreS(base Reg, disp int32, src FReg) {
	a.mem(0xBD000000, 0xBC000000, b32, Reg(src), base, disp)
}

// LoadF is dst = the double at [base + disp].
func (a *Asm) LoadF(dst FReg, base Reg, disp int32) {
	a.mem(0xFD400000, 0xFC400000, b64, Reg(dst), base, disp)
}

// StoreF is [base + disp] = src, a double.
func (a *Asm) StoreF(base Reg, disp int32, src FReg) {
	a.mem(0xFD000000, 0xFC000000, b64, Reg(src), base, disp)
}

// ---------------------------------------------------------------- integer

// ALU is a three-register integer operation.
type ALU uint32

const (
	Add  ALU = 0x0B000000
	Sub  ALU = 0x4B000000
	Adds ALU = 0x2B000000
	Subs ALU = 0x6B000000
	And  ALU = 0x0A000000
	Orr  ALU = 0x2A000000
	Eor  ALU = 0x4A000000
	Ands ALU = 0x6A000000
)

// Op is dst = n op m.
func (a *Asm) Op(op ALU, dst, n, m Reg, wide bool) {
	a.emit(uint32(op) | sf(wide) | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// Cmp sets the flags for n - m.
func (a *Asm) Cmp(n, m Reg, wide bool) { a.Op(Subs, ZR, n, m, wide) }

// Tst sets the flags for n & m.
func (a *Asm) Tst(n, m Reg, wide bool) { a.Op(Ands, ZR, n, m, wide) }

// AddImm is dst = n + imm, for any imm: an immediate form when it fits
// twelve bits, possibly shifted by twelve, and tmp otherwise. n may be ZR
// only for a sum through tmp.
func (a *Asm) AddImm(dst, n Reg, imm int64, wide bool) {
	a.addImm(0x11000000, 0x51000000, dst, n, imm, wide)
}

// CmpImm sets the flags for n - imm.
func (a *Asm) CmpImm(n Reg, imm int64, wide bool) { a.addImm(0x71000000, 0x31000000, ZR, n, imm, wide) }

// addImm emits op with imm, or its negation with -imm.
func (a *Asm) addImm(op, neg uint32, dst, n Reg, imm int64, wide bool) {
	if imm < 0 {
		op, neg, imm = neg, op, -imm
	}
	switch {
	case imm < 4096:
		a.emit(op | sf(wide) | uint32(imm)<<10 | uint32(n)<<5 | uint32(dst))
	case imm&0xFFF == 0 && imm < 1<<24:
		a.emit(op | sf(wide) | 1<<22 | uint32(imm>>12)<<10 | uint32(n)<<5 | uint32(dst))
	default:
		a.MovImm(tmp, uint64(imm))
		// The register form of the same operation (ADD/SUB/ADDS/SUBS).
		reg := uint32(Add)
		switch op {
		case 0x51000000:
			reg = uint32(Sub)
		case 0x71000000:
			reg = uint32(Subs)
		case 0x31000000:
			reg = uint32(Adds)
		}
		a.emit(reg | sf(wide) | uint32(tmp)<<16 | uint32(n)<<5 | uint32(dst))
	}
}

// Shift is a shift by a register's count, modulo the width.
type Shift uint32

const (
	Lsl Shift = 0x1AC02000
	Lsr Shift = 0x1AC02400
	Asr Shift = 0x1AC02800
)

// ShiftReg is dst = n shifted by m.
func (a *Asm) ShiftReg(op Shift, dst, n, m Reg, wide bool) {
	a.emit(uint32(op) | sf(wide) | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// ShiftImm is dst = n shifted by a constant count, below the width.
func (a *Asm) ShiftImm(op Shift, dst, n Reg, count uint8, wide bool) {
	bits := uint32(32)
	base := uint32(0x53000000) // UBFM W
	if op == Asr {
		base = 0x13000000 // SBFM W
	}
	if wide {
		bits = 64
		base |= 1<<31 | 1<<22
	}
	s := uint32(count) % bits
	var immr, imms uint32
	if op == Lsl {
		immr, imms = (bits-s)%bits, bits-1-s
	} else {
		immr, imms = s, bits-1
	}
	a.emit(base | immr<<16 | imms<<10 | uint32(n)<<5 | uint32(dst))
}

// Mvn is dst = ^n.
func (a *Asm) Mvn(dst, n Reg, wide bool) { a.emit(0x2A2003E0 | sf(wide) | uint32(n)<<16 | uint32(dst)) }

// Neg is dst = -n.
func (a *Asm) Neg(dst, n Reg, wide bool) { a.Op(Sub, dst, ZR, n, wide) }

// Cset is dst = 1 if c holds, else 0.
func (a *Asm) Cset(dst Reg, c Cond) {
	a.emit(0x1A9F07E0 | uint32(c.Invert())<<12 | uint32(dst))
}

// Sdiv is dst = n / m, signed, 64 bits: 0 when m is 0, and n when n is
// -2**63 and m is -1; it never faults.
func (a *Asm) Sdiv(dst, n, m Reg) { a.emit(0x9AC00C00 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst)) }

// Udiv is dst = n / m, unsigned, 64 bits: 0 when m is 0; it never faults.
func (a *Asm) Udiv(dst, n, m Reg) { a.emit(0x9AC00800 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst)) }

// Umulh is dst = the high 64 bits of n*m, unsigned.
func (a *Asm) Umulh(dst, n, m Reg) {
	a.emit(0x9BC07C00 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// Mul is dst = n*m, the product's low 64 bits.
func (a *Asm) Mul(dst, n, m Reg) {
	a.emit(0x9B007C00 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// Smulh is dst = the high 64 bits of n*m, signed: the product fits 64 bits
// where it is the low half's sign, repeated.
func (a *Asm) Smulh(dst, n, m Reg) {
	a.emit(0x9B407C00 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// Msub is dst = acc - n*m, 64 bits.
func (a *Asm) Msub(dst, n, m, acc Reg) {
	a.emit(0x9B008000 | uint32(m)<<16 | uint32(acc)<<10 | uint32(n)<<5 | uint32(dst))
}

// ---------------------------------------------------------------- floating point

// FMovToF is dst = src's bits.
func (a *Asm) FMovToF(dst FReg, src Reg) { a.emit(0x9E670000 | uint32(src)<<5 | uint32(dst)) }

// FMovFromF is dst = src's bits.
func (a *Asm) FMovFromF(dst Reg, src FReg) { a.emit(0x9E660000 | uint32(src)<<5 | uint32(dst)) }

// FMov is dst = src.
func (a *Asm) FMov(dst, src FReg) { a.emit(0x1E604000 | uint32(src)<<5 | uint32(dst)) }

// FloatImm is the 8-bit immediate FMovImm encodes a double with, if it has
// one: +-(16 to 31)/16 times a power of two from 2**-3 to 2**4, such as 1,
// 0.5, 2, 3 or 10. Zero has none.
func FloatImm(bits uint64) (uint8, bool) {
	if bits&(1<<48-1) != 0 {
		return 0, false
	}
	b := bits >> 54 & 1
	if repl := bits >> 54 & 0xFF; repl != 0 && repl != 0xFF || bits>>62&1 == b {
		return 0, false
	}
	return uint8(bits>>63<<7 | b<<6 | bits>>48&0x3F), true
}

// FMovImm is dst = the double imm encodes (FloatImm).
func (a *Asm) FMovImm(dst FReg, imm uint8) { a.emit(0x1E601000 | uint32(imm)<<13 | uint32(dst)) }

// FOp is a two-operand double operation.
type FOp uint32

const (
	FAdd FOp = 0x1E602800
	FSub FOp = 0x1E603800
	FMul FOp = 0x1E600800
	FDiv FOp = 0x1E601800
	// FMax and FMin are NaN if either operand is, and order -0 below +0.
	FMax FOp = 0x1E604800
	FMin FOp = 0x1E605800
)

// FArith is dst = n op m.
func (a *Asm) FArith(op FOp, dst, n, m FReg) {
	a.emit(uint32(op) | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// FCmp sets the flags for n compared with m.
func (a *Asm) FCmp(n, m FReg) { a.emit(0x1E602000 | uint32(m)<<16 | uint32(n)<<5) }

// FNeg is dst = -n, the sign flipped, NaN included.
func (a *Asm) FNeg(dst, n FReg) { a.emit(0x1E614000 | uint32(n)<<5 | uint32(dst)) }

// FSqrt is dst = the square root of n.
func (a *Asm) FSqrt(dst, n FReg) { a.emit(0x1E61C000 | uint32(n)<<5 | uint32(dst)) }

// FAbs is dst = n, its sign cleared, NaN included.
func (a *Asm) FAbs(dst, n FReg) { a.emit(0x1E60C000 | uint32(n)<<5 | uint32(dst)) }

// FRound is a rounding of a double to an integer (FRint).
type FRound uint32

const (
	FRintN FRound = 0x1E644000 // to the nearest, ties to even
	FRintM FRound = 0x1E654000 // toward -Inf
	FRintP FRound = 0x1E64C000 // toward +Inf
	FRintZ FRound = 0x1E65C000 // toward zero
)

// FRint is dst = n rounded to an integer, as op says.
func (a *Asm) FRint(op FRound, dst, n FReg) { a.emit(uint32(op) | uint32(n)<<5 | uint32(dst)) }

// FCvtToSingle is dst = n, a double, rounded to a float (dst's S view).
func (a *Asm) FCvtToSingle(dst, n FReg) { a.emit(0x1E624000 | uint32(n)<<5 | uint32(dst)) }

// FCvtFromSingle is dst = n, a float (n's S view), as a double.
func (a *Asm) FCvtFromSingle(dst, n FReg) { a.emit(0x1E22C000 | uint32(n)<<5 | uint32(dst)) }

// Fcvtzs is dst = n truncated to a signed 64-bit integer, saturating; NaN
// gives 0.
func (a *Asm) Fcvtzs(dst Reg, n FReg) { a.emit(0x9E780000 | uint32(n)<<5 | uint32(dst)) }

// Scvtf is dst = n, a signed integer, 64 bits or 32.
func (a *Asm) Scvtf(dst FReg, n Reg, wide bool) {
	a.emit(0x1E620000 | sf(wide) | uint32(n)<<5 | uint32(dst))
}

// Ucvtf is dst = n, an unsigned integer, 64 bits or 32.
func (a *Asm) Ucvtf(dst FReg, n Reg, wide bool) {
	a.emit(0x1E630000 | sf(wide) | uint32(n)<<5 | uint32(dst))
}

// ---------------------------------------------------------------- control

// B jumps to l.
func (a *Asm) B(l Label) {
	a.fixups = append(a.fixups, fixup{len(a.buf), l, fixB})
	a.emit(0x14000000)
}

// BCond jumps to l if c holds.
func (a *Asm) BCond(c Cond, l Label) {
	a.fixups = append(a.fixups, fixup{len(a.buf), l, fixCond})
	a.emit(0x54000000 | uint32(c))
}

// Cbz jumps to l if r is zero.
func (a *Asm) Cbz(r Reg, l Label, wide bool) {
	a.fixups = append(a.fixups, fixup{len(a.buf), l, fixCond})
	a.emit(0x34000000 | sf(wide) | uint32(r))
}

// Cbnz jumps to l if r is not zero.
func (a *Asm) Cbnz(r Reg, l Label, wide bool) {
	a.fixups = append(a.fixups, fixup{len(a.buf), l, fixCond})
	a.emit(0x35000000 | sf(wide) | uint32(r))
}

// Ret returns through X30.
func (a *Asm) Ret() { a.emit(0xD65F03C0) }

// Br jumps to the address in r.
func (a *Asm) Br(r Reg) { a.emit(0xD61F0000 | uint32(r)<<5) }

// Adr is dst = l's address.
func (a *Asm) Adr(dst Reg, l Label) {
	a.fixups = append(a.fixups, fixup{len(a.buf), l, fixAdr})
	a.emit(0x10000000 | uint32(dst))
}
