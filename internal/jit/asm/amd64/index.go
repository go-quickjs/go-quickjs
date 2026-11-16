package amd64

// Indexed addressing, [base + index*scale + disp], which the register
// interpreter's native handlers use (internal/vm/zzzyn_native_amd64.go):
// a register's Value at the window and a byte offset, and the dispatch's
// jump through its table by the opcode.

// rexX is rex with the SIB index's bit.
func (a *Asm) rexX(w bool, r, index, b byte) {
	v := byte(0x40)
	if w {
		v |= 8
	}
	if r&8 != 0 {
		v |= 4
	}
	if index&8 != 0 {
		v |= 2
	}
	if b&8 != 0 {
		v |= 1
	}
	if v != 0x40 {
		a.emit(v)
	}
}

// modrmIdx is ModRM and SIB for [base + index*scale + disp], scale 1, 2,
// 4 or 8; index is not RSP.
func (a *Asm) modrmIdx(reg byte, base, index Reg, scale uint8, disp int32) {
	var ss byte
	switch scale {
	case 2:
		ss = 1
	case 4:
		ss = 2
	case 8:
		ss = 3
	}
	mod := byte(0x00)
	switch {
	case disp == 0 && base&7 != 5:
	case disp == int32(int8(disp)):
		mod = 0x40
	default:
		mod = 0x80
	}
	a.emit(mod | (reg&7)<<3 | 4)
	a.emit(ss<<6 | byte(index&7)<<3 | byte(base&7))
	switch mod {
	case 0x40:
		a.emit(byte(disp))
	case 0x80:
		a.imm32(disp)
	}
}

// LoadIdx is dst = [base + index*scale + disp], 64 bits.
func (a *Asm) LoadIdx(dst, base, index Reg, scale uint8, disp int32) {
	a.rexX(true, byte(dst), byte(index), byte(base))
	a.emit(0x8B)
	a.modrmIdx(byte(dst), base, index, scale, disp)
}

// StoreIdx is [base + index*scale + disp] = src, 64 bits.
func (a *Asm) StoreIdx(base, index Reg, scale uint8, disp int32, src Reg) {
	a.rexX(true, byte(src), byte(index), byte(base))
	a.emit(0x89)
	a.modrmIdx(byte(src), base, index, scale, disp)
}

// StoreSDIdx is [base + index*scale + disp] = the double in src.
func (a *Asm) StoreSDIdx(base, index Reg, scale uint8, disp int32, src XReg) {
	a.emit(0xF2)
	a.rexX(false, byte(src), byte(index), byte(base))
	a.emit2(0x0F, 0x11)
	a.modrmIdx(byte(src), base, index, scale, disp)
}

// CmpIdx compares r with [base + index*scale + disp], 64 bits.
func (a *Asm) CmpIdx(r, base, index Reg, scale uint8, disp int32) {
	a.rexX(true, byte(r), byte(index), byte(base))
	a.emit(0x3B)
	a.modrmIdx(byte(r), base, index, scale, disp)
}

// CmpIdxImm8 compares the 64 bits at [base + index*scale + disp] with
// imm, sign-extended.
func (a *Asm) CmpIdxImm8(base, index Reg, scale uint8, disp int32, imm int8) {
	a.rexX(true, 0, byte(index), byte(base))
	a.emit(0x83)
	a.modrmIdx(7, base, index, scale, disp)
	a.emit(byte(imm))
}

// LeaIdx is dst = base + index*scale + disp.
func (a *Asm) LeaIdx(dst, base, index Reg, scale uint8, disp int32) {
	a.rexX(true, byte(dst), byte(index), byte(base))
	a.emit(0x8D)
	a.modrmIdx(byte(dst), base, index, scale, disp)
}

// JmpIdx jumps to the address at [base + index*scale + disp].
func (a *Asm) JmpIdx(base, index Reg, scale uint8, disp int32) {
	a.rexX(false, 0, byte(index), byte(base))
	a.emit(0xFF)
	a.modrmIdx(4, base, index, scale, disp)
}

// CmpMemImm8 compares the 64 bits at [base + disp] with imm,
// sign-extended.
func (a *Asm) CmpMemImm8(base Reg, disp int32, imm int8) {
	a.rex(true, 0, byte(base), false)
	a.emit(0x83)
	a.modrmMem(7, base, disp)
	a.emit(byte(imm))
}
