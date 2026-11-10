package arm64

// The loads and stores that move their base register, which the
// WebAssembly optimizing tier's memory.copy and memory.fill
// (wasm/internal/jit/opt) loop with: a load or store of a byte or a word
// and a pair of 128-bit registers, at the base and then advancing it past
// what was accessed (post-index), or first moving it back over what is
// accessed (pre-index, an offset below 0). The base must not be the
// register loaded.

// advanceSize is the size field, bits 31 and 30, of a load or store of a
// byte (1) or a word (8) whose base advances.
func advanceSize(size uint8) uint32 {
	switch size {
	case 1:
		return 0
	case 8:
		return 3 << 30
	}
	panic("arm64: an advancing access of a byte or a word only")
}

// imm9 is a pre- or post-index offset's field, bits 20 to 12.
func imm9(imm int32) uint32 {
	if imm < -256 || imm > 255 {
		panic("arm64: an index offset past 9 bits")
	}
	return (uint32(imm) & 0x1FF) << 12
}

// imm7q is a pair of 128-bit registers' pre- or post-index offset's field,
// bits 21 to 15, in units of 16 bytes.
func imm7q(imm int32) uint32 {
	if imm%16 != 0 || imm < -1024 || imm > 1008 {
		panic("arm64: a pair's index offset not a multiple of 16 in 7 bits")
	}
	return (uint32(imm/16) & 0x7F) << 15
}

func checkBase(base Reg) {
	if base == ZR {
		panic("arm64: SP as a base")
	}
}

// LoadPost is dst = the size bytes (1 or 8) at [base], zero-extended, then
// base += imm (LDRB, LDR post-index).
func (a *Asm) LoadPost(dst, base Reg, imm int32, size uint8) {
	checkBase(base)
	a.emit(0x38400400 | advanceSize(size) | imm9(imm) | uint32(base)<<5 | uint32(dst))
}

// StorePost is [base] = src's low size bytes (1 or 8), then base += imm
// (STRB, STR post-index).
func (a *Asm) StorePost(base Reg, imm int32, src Reg, size uint8) {
	checkBase(base)
	a.emit(0x38000400 | advanceSize(size) | imm9(imm) | uint32(base)<<5 | uint32(src))
}

// LoadPre is base += imm, then dst = the size bytes (1 or 8) at [base],
// zero-extended (LDRB, LDR pre-index).
func (a *Asm) LoadPre(dst, base Reg, imm int32, size uint8) {
	checkBase(base)
	a.emit(0x38400C00 | advanceSize(size) | imm9(imm) | uint32(base)<<5 | uint32(dst))
}

// StorePre is base += imm, then [base] = src's low size bytes (1 or 8)
// (STRB, STR pre-index).
func (a *Asm) StorePre(base Reg, imm int32, src Reg, size uint8) {
	checkBase(base)
	a.emit(0x38000C00 | advanceSize(size) | imm9(imm) | uint32(base)<<5 | uint32(src))
}

// LoadPairQPost is Vt1 = the 16 bytes at [base] and Vt2 the 16 after
// them, then base += imm, a multiple of 16 (LDP Q post-index). t1 and t2
// differ.
func (a *Asm) LoadPairQPost(t1, t2 FReg, base Reg, imm int32) {
	checkBase(base)
	a.emit(0xACC00000 | imm7q(imm) | uint32(t2)<<10 | uint32(base)<<5 | uint32(t1))
}

// StorePairQPost is [base] = the 16 bytes of Vt1 and the 16 after them =
// Vt2's, then base += imm, a multiple of 16 (STP Q post-index).
func (a *Asm) StorePairQPost(base Reg, imm int32, t1, t2 FReg) {
	checkBase(base)
	a.emit(0xAC800000 | imm7q(imm) | uint32(t2)<<10 | uint32(base)<<5 | uint32(t1))
}

// LoadPairQPre is base += imm, a multiple of 16, then Vt1 = the 16 bytes
// at [base] and Vt2 the 16 after them (LDP Q pre-index). t1 and t2 differ.
func (a *Asm) LoadPairQPre(t1, t2 FReg, base Reg, imm int32) {
	checkBase(base)
	a.emit(0xADC00000 | imm7q(imm) | uint32(t2)<<10 | uint32(base)<<5 | uint32(t1))
}

// StorePairQPre is base += imm, a multiple of 16, then [base] = the 16
// bytes of Vt1 and the 16 after them = Vt2's (STP Q pre-index).
func (a *Asm) StorePairQPre(base Reg, imm int32, t1, t2 FReg) {
	checkBase(base)
	a.emit(0xAD800000 | imm7q(imm) | uint32(t2)<<10 | uint32(base)<<5 | uint32(t1))
}
