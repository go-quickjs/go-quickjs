package jit

import (
	"math"
	"math/bits"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

const (
	i32 = module.KindI32
	i64 = module.KindI64
	f32 = module.KindF32
	f64 = module.KindF64
)

// numeric compiles a numeric instruction, reporting whether op is one.
func (c *compiler) numeric(op opcode.Opcode, at uint32) bool {
	switch op {
	case opcode.I32Add:
		c.intOp(amd64.Add, false, true)
	case opcode.I32Sub:
		c.intOp(amd64.Sub, false, false)
	case opcode.I32And:
		c.intOp(amd64.And, false, true)
	case opcode.I32Ior:
		c.intOp(amd64.Or, false, true)
	case opcode.I32Xor:
		c.intOp(amd64.Xor, false, true)
	case opcode.I64Add:
		c.intOp(amd64.Add, true, true)
	case opcode.I64Sub:
		c.intOp(amd64.Sub, true, false)
	case opcode.I64And:
		c.intOp(amd64.And, true, true)
	case opcode.I64Ior:
		c.intOp(amd64.Or, true, true)
	case opcode.I64Xor:
		c.intOp(amd64.Xor, true, true)
	case opcode.I32Mul:
		c.mul(false)
	case opcode.I64Mul:
		c.mul(true)
	case opcode.I32DivS, opcode.I32DivU, opcode.I32RemS, opcode.I32RemU:
		c.div(op, false, at)
	case opcode.I64DivS, opcode.I64DivU, opcode.I64RemS, opcode.I64RemU:
		c.div(op, true, at)
	case opcode.I32Shl:
		c.shift(amd64.Shl, false)
	case opcode.I32ShrS:
		c.shift(amd64.Sar, false)
	case opcode.I32ShrU:
		c.shift(amd64.Shr, false)
	case opcode.I32Rol:
		c.shift(amd64.Rol, false)
	case opcode.I32Ror:
		c.shift(amd64.Ror, false)
	case opcode.I64Shl:
		c.shift(amd64.Shl, true)
	case opcode.I64ShrS:
		c.shift(amd64.Sar, true)
	case opcode.I64ShrU:
		c.shift(amd64.Shr, true)
	case opcode.I64Rol:
		c.shift(amd64.Rol, true)
	case opcode.I64Ror:
		c.shift(amd64.Ror, true)
	case opcode.I32Clz, opcode.I32Ctz, opcode.I32Popcnt:
		c.bitCount(op, false)
	case opcode.I64Clz, opcode.I64Ctz, opcode.I64Popcnt:
		c.bitCount(op, true)
	case opcode.I32Eqz:
		c.eqz(false)
	case opcode.I64Eqz:
		c.eqz(true)
	case opcode.I32Eq, opcode.I64Eq:
		c.compare(amd64.CondE, op == opcode.I64Eq)
	case opcode.I32Ne, opcode.I64Ne:
		c.compare(amd64.CondNE, op == opcode.I64Ne)
	case opcode.I32LtS, opcode.I64LtS:
		c.compare(amd64.CondL, op == opcode.I64LtS)
	case opcode.I32LtU, opcode.I64LtU:
		c.compare(amd64.CondB, op == opcode.I64LtU)
	case opcode.I32GtS, opcode.I64GtS:
		c.compare(amd64.CondG, op == opcode.I64GtS)
	case opcode.I32GtU, opcode.I64GtU:
		c.compare(amd64.CondA, op == opcode.I64GtU)
	case opcode.I32LeS, opcode.I64LeS:
		c.compare(amd64.CondLE, op == opcode.I64LeS)
	case opcode.I32LeU, opcode.I64LeU:
		c.compare(amd64.CondBE, op == opcode.I64LeU)
	case opcode.I32GeS, opcode.I64GeS:
		c.compare(amd64.CondGE, op == opcode.I64GeS)
	case opcode.I32GeU, opcode.I64GeU:
		c.compare(amd64.CondAE, op == opcode.I64GeU)
	case opcode.I32ConvertI64:
		c.wrap()
	case opcode.I64SConvertI32:
		c.extend(func(r amd64.Reg) { c.a.Movsxd(r, r) }, i64)
	case opcode.I64UConvertI32:
		// An i32 is kept zero-extended, in a register and in a slot alike.
		v := c.pop()
		v.t = i64
		c.push(v)
	case opcode.I32SExtendI8:
		c.extend(func(r amd64.Reg) { c.a.Movsx8(r, r, false) }, i32)
	case opcode.I32SExtendI16:
		c.extend(func(r amd64.Reg) { c.a.Movsx16(r, r, false) }, i32)
	case opcode.I64SExtendI8:
		c.extend(func(r amd64.Reg) { c.a.Movsx8(r, r, true) }, i64)
	case opcode.I64SExtendI16:
		c.extend(func(r amd64.Reg) { c.a.Movsx16(r, r, true) }, i64)
	case opcode.I64SExtendI32:
		c.extend(func(r amd64.Reg) { c.a.Movsxd(r, r) }, i64)

	case opcode.F32Add, opcode.F32Sub, opcode.F32Mul, opcode.F32Div,
		opcode.F64Add, opcode.F64Sub, opcode.F64Mul, opcode.F64Div:
		c.floatOp(op)
	case opcode.F32Min, opcode.F32Max, opcode.F64Min, opcode.F64Max:
		c.minMax(op == opcode.F32Max || op == opcode.F64Max, op == opcode.F32Min || op == opcode.F32Max)
	case opcode.F32CopySign, opcode.F64CopySign:
		c.copySign(op == opcode.F32CopySign)
	case opcode.F32Abs:
		c.mask(amd64.AndPD, 0x7fffffff)
	case opcode.F64Abs:
		c.mask(amd64.AndPD, 0x7fffffffffffffff)
	case opcode.F32Neg:
		c.mask(amd64.XorPD, 0x80000000)
	case opcode.F64Neg:
		c.mask(amd64.XorPD, 1<<63)
	case opcode.F32Sqrt:
		c.floatUnary(func(x amd64.XReg) { c.a.SSEOpS(amd64.SqrtSD, x, x) })
	case opcode.F64Sqrt:
		c.floatUnary(func(x amd64.XReg) { c.a.SSEOp(amd64.SqrtSD, x, x) })
	case opcode.F32Ceil, opcode.F32Floor, opcode.F32Trunc, opcode.F32NearestInt:
		mode := roundMode(op)
		c.floatUnary(func(x amd64.XReg) { c.a.RoundSS(x, x, mode) })
	case opcode.F64Ceil, opcode.F64Floor, opcode.F64Trunc, opcode.F64NearestInt:
		mode := roundMode(op)
		c.floatUnary(func(x amd64.XReg) { c.a.RoundSD(x, x, mode) })
	case opcode.F32Eq, opcode.F32Ne, opcode.F32Lt, opcode.F32Gt, opcode.F32Le, opcode.F32Ge,
		opcode.F64Eq, opcode.F64Ne, opcode.F64Lt, opcode.F64Gt, opcode.F64Le, opcode.F64Ge:
		c.floatCompare(op)

	case opcode.F32ConvertF64:
		c.floatUnary(func(x amd64.XReg) { c.a.Cvtsd2ss(x, x) })
		c.stack[len(c.stack)-1].t = f32
	case opcode.F64ConvertF32:
		c.floatUnary(func(x amd64.XReg) { c.a.Cvtss2sd(x, x) })
		c.stack[len(c.stack)-1].t = f64
	case opcode.I32ReinterpretF32, opcode.I64ReinterpretF64, opcode.F32ReinterpretI32, opcode.F64ReinterpretI64:
		c.reinterpret(op)
	case opcode.F32SConvertI32, opcode.F32UConvertI32, opcode.F32SConvertI64, opcode.F32UConvertI64,
		opcode.F64SConvertI32, opcode.F64UConvertI32, opcode.F64SConvertI64, opcode.F64UConvertI64:
		c.intToFloat(op)
	case opcode.I32SConvertF32, opcode.I32UConvertF32, opcode.I32SConvertF64, opcode.I32UConvertF64,
		opcode.I64SConvertF32, opcode.I64UConvertF32, opcode.I64SConvertF64, opcode.I64UConvertF64:
		c.truncate2int(op, at)
	case opcode.I32SConvertSatF32, opcode.I32UConvertSatF32, opcode.I32SConvertSatF64, opcode.I32UConvertSatF64,
		opcode.I64SConvertSatF32, opcode.I64UConvertSatF32, opcode.I64SConvertSatF64, opcode.I64UConvertSatF64:
		c.saturate(op)
	default:
		return false
	}
	return true
}

func kindOf(wide bool) module.Kind {
	if wide {
		return i64
	}
	return i32
}

// intOp compiles a two-operand integer operation of the ALU.
func (c *compiler) intOp(op amd64.ALU, wide, commutes bool) {
	b := c.pop()
	x := c.pop()
	if commutes && x.k == kConst && b.k != kConst {
		x, b = b, x
	}
	r := c.intoG(x, 0)
	if imm, ok := imm32(b, wide); ok {
		c.a.OpImm(op, r, imm, wide)
	} else {
		rb := c.intoG(b, 1<<r)
		c.a.Op(op, r, rb, wide)
		c.freeG(rb)
	}
	c.pushReg(kindOf(wide), uint8(r))
}

func (c *compiler) mul(wide bool) {
	b := c.pop()
	x := c.pop()
	if x.k == kConst && b.k != kConst {
		x, b = b, x
	}
	r := c.intoG(x, 0)
	rb := c.intoG(b, 1<<r)
	if wide {
		c.a.Imul(r, rb)
	} else {
		c.a.Imul32(r, rb)
	}
	c.freeG(rb)
	c.pushReg(kindOf(wide), uint8(r))
}

// div compiles division and remainder, which take the dividend in AX and
// leave the quotient there and the remainder in DX.
func (c *compiler) div(op opcode.Opcode, wide bool, at uint32) {
	a := c.a
	b := c.pop()
	x := c.pop()
	const ax, dx = amd64.RAX, amd64.RDX
	axdx := uint16(1<<ax | 1<<dx)
	c.evictG(ax, axdx)
	c.evictG(dx, axdx)
	// The divisor, in neither.
	rb := c.intoG(b, axdx)
	// The dividend, in AX.
	if !(x.k == kReg && x.gpr() == ax) {
		c.gused |= 1 << ax
		c.loadG(ax, x)
		if x.k == kReg {
			c.freeG(x.gpr())
		}
	}
	c.gused |= 1 << dx
	signed := op == opcode.I32DivS || op == opcode.I32RemS || op == opcode.I64DivS || op == opcode.I64RemS
	rem := op == opcode.I32RemS || op == opcode.I32RemU || op == opcode.I64RemS || op == opcode.I64RemU
	if b.k != kConst || b.c == 0 || (!wide && uint32(b.c) == 0) {
		a.Op(amd64.Test, rb, rb, wide)
		code := uint64(TrapDivByZero)
		if rem {
			code = TrapRemByZero
		}
		a.Jcc(amd64.CondE, c.trap(code, at))
	}
	done := a.NewLabel()
	minusOne := b.k != kConst || (wide && b.c == math.MaxUint64) || (!wide && uint32(b.c) == math.MaxUint32)
	if signed && minusOne {
		ok := a.NewLabel()
		a.OpImm(amd64.Cmp, rb, -1, wide)
		a.Jcc(amd64.CondNE, ok)
		if rem {
			// x rem -1 is 0, where the division faults for the lowest x.
			a.Op(amd64.Xor, dx, dx, false)
			a.Jmp(done)
		} else {
			if wide {
				a.MovImm(rTmp, 1<<63)
				a.Op(amd64.Cmp, ax, rTmp, true)
			} else {
				a.OpImm(amd64.Cmp, ax, math.MinInt32, false)
			}
			a.Jcc(amd64.CondE, c.trap(TrapDivUnrepresentable, at))
		}
		a.Bind(ok)
	}
	switch {
	case signed && wide:
		a.Cqo()
		a.Idiv(rb)
	case signed:
		a.Cdq()
		a.Idiv32(rb)
	case wide:
		a.Op(amd64.Xor, dx, dx, false)
		a.Div(rb)
	default:
		a.Op(amd64.Xor, dx, dx, false)
		a.Div32(rb)
	}
	a.Bind(done)
	c.freeG(rb)
	if rem {
		c.freeG(ax)
		c.pushReg(kindOf(wide), uint8(dx))
	} else {
		c.freeG(dx)
		c.pushReg(kindOf(wide), uint8(ax))
	}
}

// shift compiles the shifts and rotations, whose count is a constant or CL.
func (c *compiler) shift(op amd64.Shift, wide bool) {
	a := c.a
	b := c.pop()
	x := c.pop()
	bits := uint64(31)
	if wide {
		bits = 63
	}
	if b.k == kConst {
		r := c.intoG(x, 0)
		a.ShiftImm(op, r, uint8(b.c&bits), wide)
		c.pushReg(kindOf(wide), uint8(r))
		return
	}
	const cx = amd64.RCX
	c.evictG(cx, 1<<cx)
	r := c.intoG(x, 1<<cx)
	if !(b.k == kReg && b.gpr() == cx) {
		c.gused |= 1 << cx
		c.loadG(cx, b)
		c.free(b)
	}
	a.ShiftCL(op, r, wide)
	c.freeG(cx)
	c.pushReg(kindOf(wide), uint8(r))
}

// bitCount compiles clz, ctz and popcnt.
func (c *compiler) bitCount(op opcode.Opcode, wide bool) {
	a := c.a
	r := c.intoG(c.pop(), 0)
	width := uint64(32)
	if wide {
		width = 64
	}
	switch op {
	case opcode.I32Clz, opcode.I64Clz:
		// BSR gives the highest set bit's index, and leaves a zero's
		// alone: 2*width-1 there, and the index flipped, is the count.
		a.MovImm(rTmp, 2*width-1)
		a.Bsr(r, r, wide)
		a.Cmov(amd64.CondE, r, rTmp, wide)
		a.OpImm(amd64.Xor, r, int32(width-1), wide)
	case opcode.I32Ctz, opcode.I64Ctz:
		a.MovImm(rTmp, width)
		a.Bsf(r, r, wide)
		a.Cmov(amd64.CondE, r, rTmp, wide)
	default:
		a.Popcnt(r, r, wide)
	}
	c.pushReg(kindOf(wide), uint8(r))
}

// fusable reports whether the next instruction takes a condition from the
// flags: a br_if or an if.
func (c *compiler) fusable() bool {
	if c.r.pc >= len(c.r.b) {
		return false
	}
	op := opcode.Opcode(c.r.b[c.r.pc])
	return op == opcode.BrIf || op == opcode.If
}

// setCond leaves condition cond, just computed, as an i32: in the flags for
// the br_if or if that follows, else in a register.
func (c *compiler) setCond(cond amd64.Cond) {
	if c.fusable() {
		c.push(value{t: i32, k: kFlags, c: uint64(cond)})
		return
	}
	r := c.allocG(0)
	c.a.Setcc(cond, r)
	c.a.MovZX8(r, r)
	c.pushReg(i32, uint8(r))
}

func (c *compiler) eqz(wide bool) {
	v := c.pop()
	r := c.intoG(v, 0)
	c.a.Op(amd64.Test, r, r, wide)
	c.freeG(r)
	c.setCond(amd64.CondE)
}

// mirror is the condition of a comparison with its operands swapped.
func mirror(cond amd64.Cond) amd64.Cond {
	switch cond {
	case amd64.CondL:
		return amd64.CondG
	case amd64.CondG:
		return amd64.CondL
	case amd64.CondLE:
		return amd64.CondGE
	case amd64.CondGE:
		return amd64.CondLE
	case amd64.CondB:
		return amd64.CondA
	case amd64.CondA:
		return amd64.CondB
	case amd64.CondBE:
		return amd64.CondAE
	case amd64.CondAE:
		return amd64.CondBE
	}
	return cond
}

func (c *compiler) compare(cond amd64.Cond, wide bool) {
	b := c.pop()
	x := c.pop()
	if x.k == kConst && b.k != kConst {
		x, b = b, x
		cond = mirror(cond)
	}
	r := c.intoG(x, 0)
	if imm, ok := imm32(b, wide); ok {
		c.a.OpImm(amd64.Cmp, r, imm, wide)
	} else {
		rb := c.intoG(b, 1<<r)
		c.a.Op(amd64.Cmp, r, rb, wide)
		c.freeG(rb)
	}
	c.freeG(r)
	c.setCond(cond)
}

func (c *compiler) wrap() {
	v := c.pop()
	switch v.k {
	case kConst:
		c.pushConst(i32, uint64(uint32(v.c)))
	case kSlot:
		// A slot's word is read 32 bits wide.
		v.t = i32
		r := c.intoG(v, 0)
		c.pushReg(i32, uint8(r))
	default:
		c.a.MovRR32(v.gpr(), v.gpr())
		c.pushReg(i32, v.r)
	}
}

// extend applies a sign extension to the top integer, retyping it t.
func (c *compiler) extend(f func(amd64.Reg), t module.Kind) {
	r := c.intoG(c.pop(), 0)
	f(r)
	c.pushReg(t, uint8(r))
}

func (c *compiler) floatOp(op opcode.Opcode) {
	b := c.pop()
	x := c.pop()
	xa := c.intoX(x)
	xb := c.readX(b)
	var sse amd64.SSE
	switch op {
	case opcode.F32Add, opcode.F64Add:
		sse = amd64.AddSD
	case opcode.F32Sub, opcode.F64Sub:
		sse = amd64.SubSD
	case opcode.F32Mul, opcode.F64Mul:
		sse = amd64.MulSD
	default:
		sse = amd64.DivSD
	}
	if x.t == f32 {
		c.a.SSEOpS(sse, xa, xb)
	} else {
		c.a.SSEOp(sse, xa, xb)
	}
	c.free(b)
	c.pushReg(x.t, uint8(xa))
}

// sse emits op on floats of type single or double.
func (c *compiler) sse(single bool, op amd64.SSE, dst, src amd64.XReg) {
	if single {
		c.a.SSEOpS(op, dst, src)
	} else {
		c.a.SSEOp(op, dst, src)
	}
}

// minMax compiles min and max: NaN if either operand is, -0 below +0.
func (c *compiler) minMax(isMax, single bool) {
	a := c.a
	b := c.pop()
	x := c.pop()
	xa := c.intoX(x)
	xb := c.readX(b)
	nan, ne, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	c.sse(single, amd64.UcomiSD, xa, xb)
	a.Jcc(amd64.CondP, nan)
	a.Jcc(amd64.CondNE, ne)
	// Equal: zeros of either sign are told apart by their sign bits.
	if isMax {
		a.SSEOp(amd64.AndPD, xa, xb)
	} else {
		a.SSEOp(amd64.OrPD, xa, xb)
	}
	a.Jmp(done)
	a.Bind(ne)
	if isMax {
		a.Jcc(amd64.CondA, done)
	} else {
		a.Jcc(amd64.CondB, done)
	}
	a.SSEOp(amd64.MovAPD, xa, xb)
	a.Jmp(done)
	a.Bind(nan)
	c.sse(single, amd64.AddSD, xa, xb)
	a.Bind(done)
	c.free(b)
	c.pushReg(x.t, uint8(xa))
}

// loadMask loads the bits m into the scratch SSE register.
func (c *compiler) loadMask(m uint64) {
	c.a.MovImm(rTmp, m)
	c.a.MovQToX(xTmp, rTmp)
}

// mask applies a bitwise operation with m to the top float: abs and neg.
func (c *compiler) mask(op amd64.SSE, m uint64) {
	v := c.pop()
	x := c.intoX(v)
	c.loadMask(m)
	c.a.SSEOp(op, x, xTmp)
	c.pushReg(v.t, uint8(x))
}

func (c *compiler) copySign(single bool) {
	b := c.pop()
	v := c.pop()
	xa := c.intoX(v)
	xb := c.intoX(b)
	sign := uint64(1 << 63)
	if single {
		sign = 1 << 31
	}
	c.loadMask(sign - 1)
	c.a.SSEOp(amd64.AndPD, xa, xTmp)
	c.loadMask(sign)
	c.a.SSEOp(amd64.AndPD, xb, xTmp)
	c.a.SSEOp(amd64.OrPD, xa, xb)
	c.freeX(xb)
	c.pushReg(v.t, uint8(xa))
}

// floatUnary applies f to the top float, in a register of its own.
func (c *compiler) floatUnary(f func(amd64.XReg)) {
	v := c.pop()
	x := c.intoX(v)
	f(x)
	c.pushReg(v.t, uint8(x))
}

func roundMode(op opcode.Opcode) uint8 {
	switch op {
	case opcode.F32Ceil, opcode.F64Ceil:
		return amd64.RoundCeil
	case opcode.F32Floor, opcode.F64Floor:
		return amd64.RoundFloor
	case opcode.F32Trunc, opcode.F64Trunc:
		return amd64.RoundTrunc
	}
	return amd64.RoundNearest
}

func (c *compiler) floatCompare(op opcode.Opcode) {
	a := c.a
	b := c.pop()
	x := c.pop()
	single := x.t == f32
	xa := c.intoX(x)
	xb := c.readX(b)
	r := c.allocG(0)
	a.Op(amd64.Xor, r, r, false)
	switch op {
	case opcode.F32Eq, opcode.F64Eq:
		a.Op(amd64.Xor, rTmp, rTmp, false)
		c.sse(single, amd64.UcomiSD, xa, xb)
		a.Setcc(amd64.CondE, r)
		a.Setcc(amd64.CondNP, rTmp)
		a.Op(amd64.And, r, rTmp, false)
	case opcode.F32Ne, opcode.F64Ne:
		a.Op(amd64.Xor, rTmp, rTmp, false)
		c.sse(single, amd64.UcomiSD, xa, xb)
		a.Setcc(amd64.CondNE, r)
		a.Setcc(amd64.CondP, rTmp)
		a.Op(amd64.Or, r, rTmp, false)
	case opcode.F32Gt, opcode.F64Gt:
		c.sse(single, amd64.UcomiSD, xa, xb)
		a.Setcc(amd64.CondA, r)
	case opcode.F32Ge, opcode.F64Ge:
		c.sse(single, amd64.UcomiSD, xa, xb)
		a.Setcc(amd64.CondAE, r)
	case opcode.F32Lt, opcode.F64Lt:
		c.sse(single, amd64.UcomiSD, xb, xa)
		a.Setcc(amd64.CondA, r)
	default:
		c.sse(single, amd64.UcomiSD, xb, xa)
		a.Setcc(amd64.CondAE, r)
	}
	c.freeX(xa)
	c.free(b)
	c.pushReg(i32, uint8(r))
}

func (c *compiler) reinterpret(op opcode.Opcode) {
	v := c.pop()
	var t module.Kind
	switch op {
	case opcode.I32ReinterpretF32:
		t = i32
	case opcode.I64ReinterpretF64:
		t = i64
	case opcode.F32ReinterpretI32:
		t = f32
	default:
		t = f64
	}
	if v.k != kReg {
		// A constant's and a slot's bits are the same either way.
		v.t = t
		c.push(v)
		return
	}
	if isFloat(t) {
		x := c.allocX(0)
		if t == f32 {
			c.a.MovDToX(x, v.gpr())
		} else {
			c.a.MovQToX(x, v.gpr())
		}
		c.free(v)
		c.pushReg(t, uint8(x))
		return
	}
	r := c.allocG(0)
	if t == i32 {
		c.a.MovDFromX(r, v.xmm())
	} else {
		c.a.MovQFromX(r, v.xmm())
	}
	c.free(v)
	c.pushReg(t, uint8(r))
}

func (c *compiler) intToFloat(op opcode.Opcode) {
	a := c.a
	v := c.pop()
	single := op == opcode.F32SConvertI32 || op == opcode.F32UConvertI32 || op == opcode.F32SConvertI64 || op == opcode.F32UConvertI64
	r := c.intoG(v, 0)
	x := c.allocX(0)
	cvt := func(wide bool) {
		if single {
			a.Cvtsi2ss(x, r, wide)
		} else {
			a.Cvtsi2sd(x, r, wide)
		}
	}
	switch op {
	case opcode.F32SConvertI32, opcode.F64SConvertI32:
		cvt(false)
	case opcode.F32UConvertI32, opcode.F64UConvertI32, opcode.F32SConvertI64, opcode.F64SConvertI64:
		// An unsigned i32 is zero-extended: as an i64 it converts exactly
		// as itself.
		cvt(true)
	default:
		// At 2^63 and above, halved, its lowest bit kept as a sticky bit,
		// converted and doubled: one rounding.
		big, done := a.NewLabel(), a.NewLabel()
		a.Op(amd64.Test, r, r, true)
		a.Jcc(amd64.CondS, big)
		cvt(true)
		a.Jmp(done)
		a.Bind(big)
		a.MovRR(rTmp, r)
		a.OpImm(amd64.And, rTmp, 1, false)
		a.ShiftImm(amd64.Shr, r, 1, true)
		a.Op(amd64.Or, r, rTmp, true)
		cvt(true)
		c.sse(single, amd64.AddSD, x, x)
		a.Bind(done)
	}
	c.freeG(r)
	t := f64
	if single {
		t = f32
	}
	c.pushReg(t, uint8(x))
}

// toDouble returns the top float in a register of the instruction's own, a
// double.
func (c *compiler) toDouble() amd64.XReg {
	v := c.pop()
	x := c.intoX(v)
	if v.t == f32 {
		c.a.Cvtss2sd(x, x)
	}
	return x
}

// truncate2int compiles the trapping truncations: of a NaN, or a value out
// of the result's range, by the interpreter's bounds.
func (c *compiler) truncate2int(op opcode.Opcode, at uint32) {
	a := c.a
	x := c.toDouble()
	var lo, hi float64
	loIn := false
	var t module.Kind
	unsigned := false
	switch op {
	case opcode.I32SConvertF32:
		lo, hi, loIn, t = -2147483648.0, 2147483648.0, true, i32
	case opcode.I32SConvertF64:
		lo, hi, t = -2147483649.0, 2147483648.0, i32
	case opcode.I32UConvertF32, opcode.I32UConvertF64:
		lo, hi, t, unsigned = -1, 4294967296.0, i32, true
	case opcode.I64SConvertF32, opcode.I64SConvertF64:
		lo, hi, loIn, t = -9223372036854775808.0, 9223372036854775808.0, true, i64
	default:
		lo, hi, t, unsigned = -1, 18446744073709551616.0, i64, true
	}
	trap := c.trap(TrapFloatUnrepresentable, at)
	c.loadXConst(xTmp, lo)
	a.SSEOp(amd64.UcomiSD, x, xTmp)
	if loIn {
		a.Jcc(amd64.CondB, trap)
	} else {
		a.Jcc(amd64.CondBE, trap)
	}
	c.loadXConst(xTmp, hi)
	a.SSEOp(amd64.UcomiSD, x, xTmp)
	a.Jcc(amd64.CondAE, trap)
	r := c.allocG(0)
	switch {
	case t == i32 && !unsigned:
		a.Cvttsd2si32(r, x)
	case t == i32:
		a.Cvttsd2si(r, x)
		a.MovRR32(r, r)
	case !unsigned:
		a.Cvttsd2si(r, x)
	default:
		c.f64ToU64(r, x)
	}
	c.freeX(x)
	c.pushReg(t, uint8(r))
}

// f64ToU64 converts the double in x, in [0, 2^64), to an integer in r,
// truncating; x is the caller's to lose.
func (c *compiler) f64ToU64(r amd64.Reg, x amd64.XReg) {
	a := c.a
	big, done := a.NewLabel(), a.NewLabel()
	c.loadXConst(xTmp, 9223372036854775808.0)
	a.SSEOp(amd64.UcomiSD, x, xTmp)
	a.Jcc(amd64.CondAE, big)
	a.Cvttsd2si(r, x)
	a.Jmp(done)
	a.Bind(big)
	a.SSEOp(amd64.SubSD, x, xTmp)
	a.Cvttsd2si(r, x)
	a.MovImm(rTmp, 1<<63)
	a.Op(amd64.Or, r, rTmp, true)
	a.Bind(done)
}

// saturate compiles the saturating truncations: NaN to 0, a value past the
// result's range to its bound.
func (c *compiler) saturate(op opcode.Opcode) {
	a := c.a
	x := c.toDouble()
	r := c.allocG(0)
	var lo, hi float64
	var loVal, hiVal uint64
	var t module.Kind
	signed := false
	switch op {
	case opcode.I32SConvertSatF32, opcode.I32SConvertSatF64:
		lo, hi, loVal, hiVal, t, signed = -2147483648.0, 2147483647.0, 0x80000000, 0x7fffffff, i32, true
	case opcode.I32UConvertSatF32, opcode.I32UConvertSatF64:
		lo, hi, loVal, hiVal, t = -1, 4294967295.0, 0, 0xffffffff, i32
	case opcode.I64SConvertSatF32, opcode.I64SConvertSatF64:
		lo, hi, loVal, hiVal, t, signed = -9223372036854775808.0, 9223372036854775808.0, 1<<63, 1<<63-1, i64, true
	default:
		lo, hi, loVal, hiVal, t = -1, 18446744073709551616.0, 0, math.MaxUint64, i64
	}
	low, high, done := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.MovImm(r, 0)
	a.SSEOp(amd64.UcomiSD, x, x)
	a.Jcc(amd64.CondP, done)
	c.loadXConst(xTmp, lo)
	a.SSEOp(amd64.UcomiSD, x, xTmp)
	a.Jcc(amd64.CondBE, low)
	c.loadXConst(xTmp, hi)
	a.SSEOp(amd64.UcomiSD, x, xTmp)
	a.Jcc(amd64.CondAE, high)
	switch {
	case t == i32 && signed:
		a.Cvttsd2si32(r, x)
	case t == i32:
		a.Cvttsd2si(r, x)
		a.MovRR32(r, r)
	case signed:
		a.Cvttsd2si(r, x)
	default:
		c.f64ToU64(r, x)
	}
	a.Jmp(done)
	a.Bind(low)
	a.MovImm(r, loVal)
	a.Jmp(done)
	a.Bind(high)
	a.MovImm(r, hiVal)
	a.Bind(done)
	c.freeX(x)
	c.pushReg(t, uint8(r))
}

// memoryOp compiles a load or a store, reporting whether op is one.
func (c *compiler) memoryOp(op opcode.Opcode, at uint32) bool {
	if op < opcode.I32LoadMem || op > opcode.I64StoreMem32 {
		return false
	}
	mem, off := c.r.memarg()
	if op >= opcode.I32StoreMem {
		c.storeOp(op, mem, off, at)
		return true
	}
	var size uint64
	var t module.Kind
	switch op {
	case opcode.I32LoadMem:
		size, t = 4, i32
	case opcode.I64LoadMem:
		size, t = 8, i64
	case opcode.F32LoadMem:
		size, t = 4, f32
	case opcode.F64LoadMem:
		size, t = 8, f64
	case opcode.I32LoadMem8S, opcode.I32LoadMem8U:
		size, t = 1, i32
	case opcode.I32LoadMem16S, opcode.I32LoadMem16U:
		size, t = 2, i32
	case opcode.I64LoadMem8S, opcode.I64LoadMem8U:
		size, t = 1, i64
	case opcode.I64LoadMem16S, opcode.I64LoadMem16U:
		size, t = 2, i64
	default:
		size, t = 4, i64
	}
	base, idx, disp := c.address(c.pop(), mem, off, size, at, 0)
	a := c.a
	if isFloat(t) {
		x := c.allocX(0)
		if t == f32 {
			a.LoadSSIdx(x, base, idx, disp)
		} else {
			a.LoadSDIdx(x, base, idx, disp)
		}
		c.freeG(idx)
		c.freeBase(base)
		c.pushReg(t, uint8(x))
		return true
	}
	r := idx
	switch op {
	case opcode.I32LoadMem, opcode.I64LoadMem32U:
		a.LoadU32Idx(r, base, idx, disp)
	case opcode.I64LoadMem:
		a.LoadIdx(r, base, idx, 1, disp)
	case opcode.I32LoadMem8S:
		a.LoadS8Idx(r, base, idx, disp, false)
	case opcode.I64LoadMem8S:
		a.LoadS8Idx(r, base, idx, disp, true)
	case opcode.I32LoadMem8U, opcode.I64LoadMem8U:
		a.LoadU8Idx(r, base, idx, disp)
	case opcode.I32LoadMem16S:
		a.LoadS16Idx(r, base, idx, disp, false)
	case opcode.I64LoadMem16S:
		a.LoadS16Idx(r, base, idx, disp, true)
	case opcode.I32LoadMem16U, opcode.I64LoadMem16U:
		a.LoadU16Idx(r, base, idx, disp)
	case opcode.I64LoadMem32S:
		a.LoadS32Idx(r, base, idx, disp)
	}
	c.freeBase(base)
	c.pushReg(t, uint8(r))
	return true
}

func (c *compiler) storeOp(op opcode.Opcode, mem uint32, off uint64, at uint32) {
	a := c.a
	v := c.pop()
	addr := c.pop()
	var size uint64
	switch op {
	case opcode.I32StoreMem8, opcode.I64StoreMem8:
		size = 1
	case opcode.I32StoreMem16, opcode.I64StoreMem16:
		size = 2
	case opcode.I32StoreMem, opcode.F32StoreMem, opcode.I64StoreMem32:
		size = 4
	default:
		size = 8
	}
	if isFloat(v.t) {
		x := c.intoX(v)
		base, idx, disp := c.address(addr, mem, off, size, at, 0)
		if v.t == f32 {
			a.StoreSSIdx(base, idx, disp, x)
		} else {
			a.StoreSDIdx(base, idx, 1, disp, x)
		}
		c.freeX(x)
		c.freeG(idx)
		c.freeBase(base)
		return
	}
	rv := c.intoG(v, 0)
	base, idx, disp := c.address(addr, mem, off, size, at, 1<<rv)
	switch size {
	case 1:
		a.Store8Idx(base, idx, disp, rv)
	case 2:
		a.Store16Idx(base, idx, disp, rv)
	case 4:
		a.Store32Idx(base, idx, disp, rv)
	default:
		a.StoreIdx(base, idx, 1, disp, rv)
	}
	c.freeG(rv)
	c.freeG(idx)
	c.freeBase(base)
}

// freeBase gives back the register address returned a memory's base in,
// unless it is memory 0's, which the code keeps.
func (c *compiler) freeBase(base amd64.Reg) {
	if base != rMem {
		c.freeG(base)
	}
}

// address checks an access of size bytes at address addr plus offset off
// against memory mem's size, trapping past it, and returns the register
// holding the memory's base, and a register of the instruction's own (not
// in avoid) and a displacement whose sum is the effective address, to add
// to the base. The base is R15 for memory 0.
//
// An i32 address is zero-extended, and the sum, computed in 64 bits, does
// not wrap. An i64 memory's address plus the offset may: it traps where it
// carries, as the sum plus the size does.
func (c *compiler) address(addr value, mem uint32, off, size uint64, at uint32, avoid uint16) (base, idx amd64.Reg, disp int32) {
	a := c.a
	trap := c.trap(TrapMemOutOfBounds, at)
	if c.m.Memories[mem].Limits.Is64 {
		return c.address64(addr, mem, off, size, trap, avoid)
	}
	if addr.k == kConst {
		ea := uint64(uint32(addr.c)) + off
		a.MovImm(rTmp, ea+size)
		base = c.boundsCheck(mem, trap, avoid)
		r := c.allocG(avoid | 1<<base)
		a.MovImm(r, ea)
		return base, r, 0
	}
	r := c.intoG(addr, avoid)
	if end := off + size; end <= math.MaxInt32 {
		a.Lea(rTmp, r, int32(end))
	} else {
		a.MovImm(rTmp, end)
		a.Op(amd64.Add, rTmp, r, true)
	}
	base = c.boundsCheck(mem, trap, avoid|1<<r)
	if off <= math.MaxInt32 {
		return base, r, int32(off)
	}
	a.MovImm(rTmp, off)
	a.Op(amd64.Add, r, rTmp, true)
	return base, r, 0
}

// address64 is address for an i64 memory: the address plus the offset is
// the effective address, and it and its sum with the size trap where they
// carry out of 64 bits.
func (c *compiler) address64(addr value, mem uint32, off, size uint64, trap amd64.Label, avoid uint16) (base, idx amd64.Reg, disp int32) {
	a := c.a
	if addr.k == kConst {
		ea, carry := bits.Add64(addr.c, off, 0)
		end, carry2 := bits.Add64(ea, size, 0)
		if carry|carry2 != 0 {
			// Never in bounds: the bounds check below is never reached.
			a.Jmp(trap)
		}
		a.MovImm(rTmp, end)
		base = c.boundsCheck(mem, trap, avoid)
		r := c.allocG(avoid | 1<<base)
		a.MovImm(r, ea)
		return base, r, 0
	}
	r := c.intoG(addr, avoid)
	if off != 0 {
		if off <= math.MaxInt32 {
			a.OpImm(amd64.Add, r, int32(off), true)
		} else {
			a.MovImm(rTmp, off)
			a.Op(amd64.Add, r, rTmp, true)
		}
		a.Jcc(amd64.CondB, trap)
	}
	a.MovRR(rTmp, r)
	a.OpImm(amd64.Add, rTmp, int32(size), true)
	a.Jcc(amd64.CondB, trap)
	base = c.boundsCheck(mem, trap, avoid|1<<r)
	return base, r, 0
}

// boundsCheck goes to trap if the end of an access, in rTmp, is past memory
// mem's size, and returns the register holding the memory's base: R15 for
// memory 0. A shared memory's size is read where other threads store it.
func (c *compiler) boundsCheck(mem uint32, trap amd64.Label, avoid uint16) amd64.Reg {
	a := c.a
	shared := c.m.Memories[mem].Shared
	if shared {
		p := c.allocG(avoid)
		a.Load(p, rCtx, offMemSizePtr)
		a.CmpMem(rTmp, p, 0, true)
		c.freeG(p)
	} else {
		a.CmpMem(rTmp, rCtx, offMemSize, true)
	}
	a.Jcc(amd64.CondA, trap)
	return rMem
}
