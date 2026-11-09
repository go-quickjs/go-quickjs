package amd64

// The vector forms WebAssembly's SIMD instructions need: SSE2's, SSSE3's,
// SSE4.1's and SSE4.2's operations on two XMM registers, the ones taking an
// immediate as well, the shifts by an immediate, the moves of a lane to and
// from a general register, and the unaligned loads and stores of 16 bytes.
// None is VEX-encoded: a CPU with SSE4.2 runs them all.

// VOp is an SSE instruction on two XMM registers, dst op= src: its
// mandatory prefix (bits 16-23, 0 for none), its opcode map (bits 8-15: 1
// for 0F, 2 for 0F 38, 3 for 0F 3A) and its opcode (bits 0-7).
type VOp uint32

const (
	m0F   = 1 << 8
	m0F38 = 2 << 8
	m0F3A = 3 << 8
	p66   = 0x66 << 16
	pF2   = 0xF2 << 16
	pF3   = 0xF3 << 16
)

// The operations on two registers.
const (
	// Integer lanes, SSE2.
	Paddb      VOp = p66 | m0F | 0xFC
	Paddw      VOp = p66 | m0F | 0xFD
	Paddd      VOp = p66 | m0F | 0xFE
	Paddq      VOp = p66 | m0F | 0xD4
	Psubb      VOp = p66 | m0F | 0xF8
	Psubw      VOp = p66 | m0F | 0xF9
	Psubd      VOp = p66 | m0F | 0xFA
	Psubq      VOp = p66 | m0F | 0xFB
	Paddsb     VOp = p66 | m0F | 0xEC
	Paddsw     VOp = p66 | m0F | 0xED
	Paddusb    VOp = p66 | m0F | 0xDC
	Paddusw    VOp = p66 | m0F | 0xDD
	Psubsb     VOp = p66 | m0F | 0xE8
	Psubsw     VOp = p66 | m0F | 0xE9
	Psubusb    VOp = p66 | m0F | 0xD8
	Psubusw    VOp = p66 | m0F | 0xD9
	Pmullw     VOp = p66 | m0F | 0xD5
	Pmulhw     VOp = p66 | m0F | 0xE5
	Pmulhuw    VOp = p66 | m0F | 0xE4
	Pmuludq    VOp = p66 | m0F | 0xF4
	Pmaddwd    VOp = p66 | m0F | 0xF5
	Pand       VOp = p66 | m0F | 0xDB
	Pandn      VOp = p66 | m0F | 0xDF // dst = ^dst & src
	Por        VOp = p66 | m0F | 0xEB
	Pxor       VOp = p66 | m0F | 0xEF
	Pcmpeqb    VOp = p66 | m0F | 0x74
	Pcmpeqw    VOp = p66 | m0F | 0x75
	Pcmpeqd    VOp = p66 | m0F | 0x76
	Pcmpgtb    VOp = p66 | m0F | 0x64
	Pcmpgtw    VOp = p66 | m0F | 0x65
	Pcmpgtd    VOp = p66 | m0F | 0x66
	Pminub     VOp = p66 | m0F | 0xDA
	Pmaxub     VOp = p66 | m0F | 0xDE
	Pminsw     VOp = p66 | m0F | 0xEA
	Pmaxsw     VOp = p66 | m0F | 0xEE
	Pavgb      VOp = p66 | m0F | 0xE0
	Pavgw      VOp = p66 | m0F | 0xE3
	Packsswb   VOp = p66 | m0F | 0x63
	Packuswb   VOp = p66 | m0F | 0x67
	Packssdw   VOp = p66 | m0F | 0x6B
	Punpcklbw  VOp = p66 | m0F | 0x60
	Punpcklwd  VOp = p66 | m0F | 0x61
	Punpckldq  VOp = p66 | m0F | 0x62
	Punpcklqdq VOp = p66 | m0F | 0x6C
	Punpckhbw  VOp = p66 | m0F | 0x68
	Punpckhwd  VOp = p66 | m0F | 0x69
	Punpckhdq  VOp = p66 | m0F | 0x6A
	Punpckhqdq VOp = p66 | m0F | 0x6D
	// The shifts of every lane by the count in src's low 64 bits.
	Psllw VOp = p66 | m0F | 0xF1
	Pslld VOp = p66 | m0F | 0xF2
	Psllq VOp = p66 | m0F | 0xF3
	Psrlw VOp = p66 | m0F | 0xD1
	Psrld VOp = p66 | m0F | 0xD2
	Psrlq VOp = p66 | m0F | 0xD3
	Psraw VOp = p66 | m0F | 0xE1
	Psrad VOp = p66 | m0F | 0xE2

	// Float lanes, SSE and SSE2.
	Addps     VOp = m0F | 0x58
	Subps     VOp = m0F | 0x5C
	Mulps     VOp = m0F | 0x59
	Divps     VOp = m0F | 0x5E
	Sqrtps    VOp = m0F | 0x51
	Minps     VOp = m0F | 0x5D // dst < src ? dst : src
	Maxps     VOp = m0F | 0x5F // dst > src ? dst : src
	Andps     VOp = m0F | 0x54
	Andnps    VOp = m0F | 0x55 // dst = ^dst & src
	Orps      VOp = m0F | 0x56
	Xorps     VOp = m0F | 0x57
	Movhlps   VOp = m0F | 0x12 // dst's low 64 bits = src's high 64
	Movlhps   VOp = m0F | 0x16 // dst's high 64 bits = src's low 64
	Addpd     VOp = p66 | m0F | 0x58
	Subpd     VOp = p66 | m0F | 0x5C
	Mulpd     VOp = p66 | m0F | 0x59
	Divpd     VOp = p66 | m0F | 0x5E
	Sqrtpd    VOp = p66 | m0F | 0x51
	Minpd     VOp = p66 | m0F | 0x5D
	Maxpd     VOp = p66 | m0F | 0x5F
	Movsd     VOp = pF2 | m0F | 0x10 // dst's low 64 bits = src's
	Cvtdq2ps  VOp = m0F | 0x5B
	Cvtps2pd  VOp = m0F | 0x5A
	Cvtpd2ps  VOp = p66 | m0F | 0x5A
	Cvttps2dq VOp = pF3 | m0F | 0x5B
	Cvttpd2dq VOp = p66 | m0F | 0xE6
	Cvtdq2pd  VOp = pF3 | m0F | 0xE6

	// SSSE3.
	Pshufb    VOp = p66 | m0F38 | 0x00
	Pmaddubsw VOp = p66 | m0F38 | 0x04 // dst's bytes unsigned, src's signed
	Pmulhrsw  VOp = p66 | m0F38 | 0x0B
	Pabsb     VOp = p66 | m0F38 | 0x1C
	Pabsw     VOp = p66 | m0F38 | 0x1D
	Pabsd     VOp = p66 | m0F38 | 0x1E

	// SSE4.1 and SSE4.2.
	Ptest    VOp = p66 | m0F38 | 0x17
	Pmovsxbw VOp = p66 | m0F38 | 0x20
	Pmovsxwd VOp = p66 | m0F38 | 0x23
	Pmovsxdq VOp = p66 | m0F38 | 0x25
	Pmovzxbw VOp = p66 | m0F38 | 0x30
	Pmovzxwd VOp = p66 | m0F38 | 0x33
	Pmovzxdq VOp = p66 | m0F38 | 0x35
	Pmuldq   VOp = p66 | m0F38 | 0x28
	Pcmpeqq  VOp = p66 | m0F38 | 0x29
	Packusdw VOp = p66 | m0F38 | 0x2B
	Pcmpgtq  VOp = p66 | m0F38 | 0x37
	Pminsb   VOp = p66 | m0F38 | 0x38
	Pminsd   VOp = p66 | m0F38 | 0x39
	Pminuw   VOp = p66 | m0F38 | 0x3A
	Pminud   VOp = p66 | m0F38 | 0x3B
	Pmaxsb   VOp = p66 | m0F38 | 0x3C
	Pmaxsd   VOp = p66 | m0F38 | 0x3D
	Pmaxuw   VOp = p66 | m0F38 | 0x3E
	Pmaxud   VOp = p66 | m0F38 | 0x3F
	Pmulld   VOp = p66 | m0F38 | 0x40
)

// The operations on two registers and an immediate (VecImm).
const (
	Pshufd   VOp = p66 | m0F | 0x70 // dst's lane i = src's lane imm>>(2i)&3, 32 bits
	Pshuflw  VOp = pF2 | m0F | 0x70 // the same of the low four 16-bit lanes
	Pshufhw  VOp = pF3 | m0F | 0x70 // of the high four
	Shufps   VOp = m0F | 0xC6       // dst's lanes 0, 1 from dst, 2, 3 from src
	Cmpps    VOp = m0F | 0xC2
	Cmppd    VOp = p66 | m0F | 0xC2
	Roundps  VOp = p66 | m0F3A | 0x08
	Roundpd  VOp = p66 | m0F3A | 0x09
	Pblendw  VOp = p66 | m0F3A | 0x0E
	Palignr  VOp = p66 | m0F3A | 0x0F
	Insertps VOp = p66 | m0F3A | 0x21 // src's lane imm>>6 into dst's lane imm>>4&3, lanes imm&15 zeroed
)

// The predicates of Cmpps and Cmppd: each lane all ones where it holds.
const (
	CmpEQ    = 0
	CmpLT    = 1
	CmpLE    = 2
	CmpUnord = 3
	CmpNEQ   = 4 // unordered too
	CmpNLT   = 5
	CmpNLE   = 6
	CmpOrd   = 7
)

// vop emits op's prefix, a REX prefix where one is needed (with w for a
// 64-bit general register) and its escape and opcode bytes, for a ModRM
// whose reg field is r and whose r/m (or base) field is b, and x for its
// SIB index field.
func (a *Asm) vop(op VOp, w bool, r, x, b byte) {
	if p := byte(op >> 16); p != 0 {
		a.emit(p)
	}
	a.rexSIB(w, r, x, b, false)
	switch op >> 8 & 0xFF {
	case 1:
		a.emit2(0x0F, byte(op))
	case 2:
		a.emit3(0x0F, 0x38, byte(op))
	case 3:
		a.emit3(0x0F, 0x3A, byte(op))
	}
}

// Vec is dst = dst op src (for Ptest, sets ZF if dst & src is 0, and CF if
// ^dst & src is).
func (a *Asm) Vec(op VOp, dst, src XReg) {
	a.vop(op, false, byte(dst), 0, byte(src))
	a.modrmRR(byte(dst), byte(src))
}

// VecImm is op with an immediate: dst = op(dst, src, imm).
func (a *Asm) VecImm(op VOp, dst, src XReg, imm uint8) {
	a.vop(op, false, byte(dst), 0, byte(src))
	a.modrmRR(byte(dst), byte(src))
	a.emit(imm)
}

// VShift is a shift of every lane of a register by an immediate: its
// opcode (bits 0-7, after 66 0F) and its ModRM reg field (bits 8-15).
type VShift uint16

const (
	PsllwImm  VShift = 6<<8 | 0x71
	PsrlwImm  VShift = 2<<8 | 0x71
	PsrawImm  VShift = 4<<8 | 0x71
	PslldImm  VShift = 6<<8 | 0x72
	PsrldImm  VShift = 2<<8 | 0x72
	PsradImm  VShift = 4<<8 | 0x72
	PsllqImm  VShift = 6<<8 | 0x73
	PsrlqImm  VShift = 2<<8 | 0x73
	PsrldqImm VShift = 3<<8 | 0x73 // by bytes, the whole register
	PslldqImm VShift = 7<<8 | 0x73
)

// VecShift shifts every lane of dst by n bits (bytes for the whole-register
// shifts); a count past the lane's width gives 0, or for the arithmetic
// shifts the sign.
func (a *Asm) VecShift(op VShift, dst XReg, n uint8) {
	a.emit(0x66)
	a.rex(false, 0, byte(dst), false)
	a.emit2(0x0F, byte(op))
	a.modrmRR(byte(op>>8), byte(dst))
	a.emit(n)
}

// LoadVec is dst = the 16 bytes at [base + disp], unaligned (MOVDQU).
func (a *Asm) LoadVec(dst XReg, base Reg, disp int32) {
	a.emit(0xF3)
	a.rex(false, byte(dst), byte(base), false)
	a.emit2(0x0F, 0x6F)
	a.modrmMem(byte(dst), base, disp)
}

// StoreVec is [base + disp] = the 16 bytes of src, unaligned.
func (a *Asm) StoreVec(base Reg, disp int32, src XReg) {
	a.emit(0xF3)
	a.rex(false, byte(src), byte(base), false)
	a.emit2(0x0F, 0x7F)
	a.modrmMem(byte(src), base, disp)
}

// LoadVecIdx is dst = the 16 bytes at [base + index + disp], unaligned.
func (a *Asm) LoadVecIdx(dst XReg, base, index Reg, disp int32) {
	a.emit(0xF3)
	a.rexSIB(false, byte(dst), byte(index), byte(base), false)
	a.emit2(0x0F, 0x6F)
	a.modrmSIB(byte(dst), base, index, 0, disp)
}

// StoreVecIdx is [base + index + disp] = the 16 bytes of src, unaligned.
func (a *Asm) StoreVecIdx(base, index Reg, disp int32, src XReg) {
	a.emit(0xF3)
	a.rexSIB(false, byte(src), byte(index), byte(base), false)
	a.emit2(0x0F, 0x7F)
	a.modrmSIB(byte(src), base, index, 0, disp)
}

// LoadVecLabel is dst = the 16 bytes at the address l is bound at,
// RIP-relative and unaligned: a constant of the code's own.
func (a *Asm) LoadVecLabel(dst XReg, l Label) {
	a.emit(0xF3)
	if dst >= 8 {
		a.emit(0x44)
	}
	a.emit3(0x0F, 0x6F, byte(dst&7)<<3|5)
	a.fix(l, fixLea, 0)
	a.imm32(0)
}

// Pinsr is dst's lane lane of size bytes (1, 2, 4 or 8) = src's low bytes
// (PINSRB, PINSRW, PINSRD, PINSRQ).
func (a *Asm) Pinsr(size uint8, dst XReg, src Reg, lane uint8) {
	switch size {
	case 2:
		a.vop(p66|m0F|0xC4, false, byte(dst), 0, byte(src))
	case 1:
		a.vop(p66|m0F3A|0x20, false, byte(dst), 0, byte(src))
	default:
		a.vop(p66|m0F3A|0x22, size == 8, byte(dst), 0, byte(src))
	}
	a.modrmRR(byte(dst), byte(src))
	a.emit(lane)
}

// Pextr is dst = src's lane lane of size bytes (1, 2, 4 or 8),
// zero-extended (PEXTRB, PEXTRW, PEXTRD, PEXTRQ).
func (a *Asm) Pextr(size uint8, dst Reg, src XReg, lane uint8) {
	if size == 2 {
		a.vop(p66|m0F|0xC5, false, byte(dst), 0, byte(src))
		a.modrmRR(byte(dst), byte(src))
		a.emit(lane)
		return
	}
	op := VOp(p66 | m0F3A | 0x16)
	if size == 1 {
		op = p66 | m0F3A | 0x14
	}
	a.vop(op, size == 8, byte(src), 0, byte(dst))
	a.modrmRR(byte(src), byte(dst))
	a.emit(lane)
}

// The moves of each lane's top bit into a general register's low bits
// (VecMask).
const (
	Pmovmskb VOp = p66 | m0F | 0xD7
	Movmskps VOp = m0F | 0x50
	Movmskpd VOp = p66 | m0F | 0x50
)

// VecMask is dst = op of src: the top bit of each of src's lanes, lane i's
// in bit i, the rest of dst clear.
func (a *Asm) VecMask(op VOp, dst Reg, src XReg) {
	a.vop(op, false, byte(dst), 0, byte(src))
	a.modrmRR(byte(dst), byte(src))
}
