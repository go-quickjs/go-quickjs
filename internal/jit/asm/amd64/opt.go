package amd64

// The forms WebAssembly's optimizing tier adds: operations with an operand
// in memory -- a spill slot -- and a multiply by an immediate.

// OpMem is dst op= [base + disp], 64 bits if wide, else 32 (clearing dst's
// upper half, except for Cmp, which only sets flags): Add, Or, And, Sub,
// Xor or Cmp.
func (a *Asm) OpMem(op ALU, dst, base Reg, disp int32, wide bool) {
	if op == Test {
		panic("amd64: no TEST of a register by memory")
	}
	a.rex(wide, byte(dst), byte(base), false)
	a.emit(byte(op) + 2)
	a.modrmMem(byte(dst), base, disp)
}

// ImulMem is dst *= [base + disp], the low half of the product, 64 bits if
// wide, else 32, clearing dst's upper half.
func (a *Asm) ImulMem(dst, base Reg, disp int32, wide bool) {
	a.rex(wide, byte(dst), byte(base), false)
	a.emit2(0x0F, 0xAF)
	a.modrmMem(byte(dst), base, disp)
}

// ImulImm is dst = src * imm, imm sign-extended, the low half of the
// product, 64 bits if wide, else 32, clearing dst's upper half.
func (a *Asm) ImulImm(dst, src Reg, imm int32, wide bool) {
	a.rex(wide, byte(dst), byte(src), false)
	if imm >= -128 && imm <= 127 {
		a.emit(0x6B)
		a.modrmRR(byte(dst), byte(src))
		a.emit(byte(int8(imm)))
		return
	}
	a.emit(0x69)
	a.modrmRR(byte(dst), byte(src))
	a.imm32(imm)
}

// StoreSS is [base + disp] = the float in src's low 32 bits.
func (a *Asm) StoreSS(base Reg, disp int32, src XReg) {
	a.emit(0xF3)
	a.rex(false, byte(src), byte(base), false)
	a.emit2(0x0F, 0x11)
	a.modrmMem(byte(src), base, disp)
}
