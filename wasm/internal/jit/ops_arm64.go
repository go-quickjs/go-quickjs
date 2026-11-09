package jit

import (
	"math"
	"math/bits"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
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
	case opcode.I32Add, opcode.I64Add:
		c.addSub(false, op == opcode.I64Add)
	case opcode.I32Sub, opcode.I64Sub:
		c.addSub(true, op == opcode.I64Sub)
	case opcode.I32And, opcode.I64And:
		c.logic(arm64.And, op == opcode.I64And)
	case opcode.I32Ior, opcode.I64Ior:
		c.logic(arm64.Orr, op == opcode.I64Ior)
	case opcode.I32Xor, opcode.I64Xor:
		c.logic(arm64.Eor, op == opcode.I64Xor)
	case opcode.I32Mul, opcode.I64Mul:
		c.mul(op == opcode.I64Mul)
	case opcode.I32DivS, opcode.I32DivU, opcode.I32RemS, opcode.I32RemU:
		c.div(op, false, at)
	case opcode.I64DivS, opcode.I64DivU, opcode.I64RemS, opcode.I64RemU:
		c.div(op, true, at)
	case opcode.I32Shl, opcode.I64Shl:
		c.shift(arm64.Lsl, false, op == opcode.I64Shl)
	case opcode.I32ShrS, opcode.I64ShrS:
		c.shift(arm64.Asr, false, op == opcode.I64ShrS)
	case opcode.I32ShrU, opcode.I64ShrU:
		c.shift(arm64.Lsr, false, op == opcode.I64ShrU)
	case opcode.I32Rol, opcode.I64Rol:
		c.shift(arm64.Ror, true, op == opcode.I64Rol)
	case opcode.I32Ror, opcode.I64Ror:
		c.shift(arm64.Ror, false, op == opcode.I64Ror)
	case opcode.I32Clz, opcode.I32Ctz, opcode.I32Popcnt:
		c.bitCount(op, false)
	case opcode.I64Clz, opcode.I64Ctz, opcode.I64Popcnt:
		c.bitCount(op, true)
	case opcode.I32Eqz:
		c.eqz(false)
	case opcode.I64Eqz:
		c.eqz(true)
	case opcode.I32Eq, opcode.I64Eq:
		c.compare(arm64.EQ, op == opcode.I64Eq)
	case opcode.I32Ne, opcode.I64Ne:
		c.compare(arm64.NE, op == opcode.I64Ne)
	case opcode.I32LtS, opcode.I64LtS:
		c.compare(arm64.LT, op == opcode.I64LtS)
	case opcode.I32LtU, opcode.I64LtU:
		c.compare(arm64.LO, op == opcode.I64LtU)
	case opcode.I32GtS, opcode.I64GtS:
		c.compare(arm64.GT, op == opcode.I64GtS)
	case opcode.I32GtU, opcode.I64GtU:
		c.compare(arm64.HI, op == opcode.I64GtU)
	case opcode.I32LeS, opcode.I64LeS:
		c.compare(arm64.LE, op == opcode.I64LeS)
	case opcode.I32LeU, opcode.I64LeU:
		c.compare(arm64.LS, op == opcode.I64LeU)
	case opcode.I32GeS, opcode.I64GeS:
		c.compare(arm64.GE, op == opcode.I64GeS)
	case opcode.I32GeU, opcode.I64GeU:
		c.compare(arm64.HS, op == opcode.I64GeU)
	case opcode.I32ConvertI64:
		c.wrap()
	case opcode.I64SConvertI32, opcode.I64SExtendI32:
		c.extend(func(r arm64.Reg) { c.a.Sxtw(r, r) }, i64)
	case opcode.I64UConvertI32:
		// An i32 is kept zero-extended, in a register and in a slot alike.
		v := c.pop()
		v.t = i64
		c.push(v)
	case opcode.I32SExtendI8:
		c.extend(func(r arm64.Reg) { c.a.Sxtb(r, r, false) }, i32)
	case opcode.I32SExtendI16:
		c.extend(func(r arm64.Reg) { c.a.Sxth(r, r, false) }, i32)
	case opcode.I64SExtendI8:
		c.extend(func(r arm64.Reg) { c.a.Sxtb(r, r, true) }, i64)
	case opcode.I64SExtendI16:
		c.extend(func(r arm64.Reg) { c.a.Sxth(r, r, true) }, i64)

	case opcode.F32Add, opcode.F64Add:
		c.floatOp(arm64.FAdd)
	case opcode.F32Sub, opcode.F64Sub:
		c.floatOp(arm64.FSub)
	case opcode.F32Mul, opcode.F64Mul:
		c.floatOp(arm64.FMul)
	case opcode.F32Div, opcode.F64Div:
		c.floatOp(arm64.FDiv)
	case opcode.F32Min, opcode.F64Min:
		c.floatOp(arm64.FMin)
	case opcode.F32Max, opcode.F64Max:
		c.floatOp(arm64.FMax)
	case opcode.F32CopySign:
		c.copySign(false)
	case opcode.F64CopySign:
		c.copySign(true)
	case opcode.F32Abs:
		c.signBit(arm64.And, false)
	case opcode.F64Abs:
		c.signBit(arm64.And, true)
	case opcode.F32Neg:
		c.signBit(arm64.Eor, false)
	case opcode.F64Neg:
		c.signBit(arm64.Eor, true)
	case opcode.F32Sqrt:
		c.floatUnary(func(x arm64.FReg) { c.a.FSqrtS(x, x) })
	case opcode.F64Sqrt:
		c.floatUnary(func(x arm64.FReg) { c.a.FSqrt(x, x) })
	case opcode.F32Ceil, opcode.F32Floor, opcode.F32Trunc, opcode.F32NearestInt:
		mode := roundMode(op)
		c.floatUnary(func(x arm64.FReg) { c.a.FRintS(mode, x, x) })
	case opcode.F64Ceil, opcode.F64Floor, opcode.F64Trunc, opcode.F64NearestInt:
		mode := roundMode(op)
		c.floatUnary(func(x arm64.FReg) { c.a.FRint(mode, x, x) })
	case opcode.F32Eq, opcode.F64Eq:
		c.floatCompare(arm64.EQ)
	case opcode.F32Ne, opcode.F64Ne:
		c.floatCompare(arm64.NE)
	case opcode.F32Lt, opcode.F64Lt:
		c.floatCompare(arm64.MI)
	case opcode.F32Gt, opcode.F64Gt:
		c.floatCompare(arm64.GT)
	case opcode.F32Le, opcode.F64Le:
		c.floatCompare(arm64.LS)
	case opcode.F32Ge, opcode.F64Ge:
		c.floatCompare(arm64.GE)

	case opcode.F32ConvertF64:
		c.floatUnary(func(x arm64.FReg) { c.a.FCvtToSingle(x, x) })
		c.stack[len(c.stack)-1].t = f32
	case opcode.F64ConvertF32:
		c.floatUnary(func(x arm64.FReg) { c.a.FCvtFromSingle(x, x) })
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

// binaryInts pops two integers, the first into a register of the
// instruction's own, which it writes its result to, and returns it with
// the second.
func (c *compiler) binaryInts(commutes bool) (arm64.Reg, value) {
	b := c.pop()
	x := c.pop()
	if commutes && x.k == kConst && b.k != kConst {
		x, b = b, x
	}
	return c.intoG(x, 0), b
}

// addSub compiles add and sub: a constant operand is an immediate.
func (c *compiler) addSub(sub, wide bool) {
	r, b := c.binaryInts(!sub)
	if b.k == kConst {
		imm := int64(b.c)
		if sub {
			imm = -imm
		}
		c.a.AddImm(r, r, imm, wide)
	} else {
		op := arm64.Add
		if sub {
			op = arm64.Sub
		}
		c.a.Op(op, r, r, c.readG(b, rTmp), wide)
		c.free(b)
	}
	c.pushReg(kindOf(wide), uint8(r))
}

// logic compiles and, or and xor: a constant operand is a logical
// immediate where it has one.
func (c *compiler) logic(op arm64.ALU, wide bool) {
	r, b := c.binaryInts(true)
	if b.k != kConst || !c.a.LogicImm(op, r, r, b.c, wide) {
		c.a.Op(op, r, r, c.readG(b, rTmp), wide)
		c.free(b)
	}
	c.pushReg(kindOf(wide), uint8(r))
}

func (c *compiler) mul(wide bool) {
	r, b := c.binaryInts(true)
	rb := c.readG(b, rTmp)
	if wide {
		c.a.Mul(r, r, rb)
	} else {
		c.a.MulW(r, r, rb)
	}
	c.free(b)
	c.pushReg(kindOf(wide), uint8(r))
}

// div compiles division and remainder. The division instructions never
// fault: a zero divisor traps first, as does the signed division of the
// least integer by -1, whose remainder, the quotient's product taken from
// the dividend, is 0 as the instruction leaves it.
func (c *compiler) div(op opcode.Opcode, wide bool, at uint32) {
	a := c.a
	b := c.pop()
	x := c.pop()
	r := c.intoG(x, 0)
	rb := c.readG(b, rTmp)
	signed := op == opcode.I32DivS || op == opcode.I32RemS || op == opcode.I64DivS || op == opcode.I64RemS
	rem := op == opcode.I32RemS || op == opcode.I32RemU || op == opcode.I64RemS || op == opcode.I64RemU
	if b.k != kConst || b.c == 0 || (!wide && uint32(b.c) == 0) {
		code := uint64(TrapDivByZero)
		if rem {
			code = TrapRemByZero
		}
		a.Cbz(rb, c.trap(code, at), wide)
	}
	minusOne := b.k != kConst || (wide && b.c == math.MaxUint64) || (!wide && uint32(b.c) == math.MaxUint32)
	if signed && !rem && minusOne {
		ok := a.NewLabel()
		a.CmpImm(rb, -1, wide)
		a.BCond(arm64.NE, ok)
		if wide {
			a.CmpImm(r, math.MinInt64, true)
		} else {
			a.CmpImm(r, math.MinInt32, false)
		}
		a.BCond(arm64.EQ, c.trap(TrapDivUnrepresentable, at))
		a.Bind(ok)
	}
	q := r
	if rem {
		q = rTmp2
	}
	switch {
	case signed && wide:
		a.Sdiv(q, r, rb)
	case signed:
		a.SdivW(q, r, rb)
	case wide:
		a.Udiv(q, r, rb)
	default:
		a.UdivW(q, r, rb)
	}
	if rem {
		if wide {
			a.Msub(r, q, rb, r)
		} else {
			a.MsubW(r, q, rb, r)
		}
	}
	c.free(b)
	c.pushReg(kindOf(wide), uint8(r))
}

// shift compiles the shifts and rotations, by a constant or by a
// register's count, which the instructions take modulo the width. A left
// rotation is a right one by the count negated.
func (c *compiler) shift(op arm64.Shift, left, wide bool) {
	a := c.a
	b := c.pop()
	x := c.pop()
	bits := uint64(31)
	if wide {
		bits = 63
	}
	r := c.intoG(x, 0)
	if b.k == kConst {
		n := b.c & bits
		if left {
			n = -n & bits
		}
		a.ShiftImm(op, r, r, uint8(n), wide)
		c.pushReg(kindOf(wide), uint8(r))
		return
	}
	rb := c.readG(b, rTmp)
	if left {
		a.Neg(rTmp2, rb, false)
		rb = rTmp2
	}
	a.ShiftReg(op, r, r, rb, wide)
	c.free(b)
	c.pushReg(kindOf(wide), uint8(r))
}

// bitCount compiles clz, ctz -- clz of the bits reversed -- and popcnt,
// the count of each byte's bits summed in a vector register.
func (c *compiler) bitCount(op opcode.Opcode, wide bool) {
	a := c.a
	r := c.intoG(c.pop(), 0)
	switch op {
	case opcode.I32Clz, opcode.I64Clz:
		a.Clz(r, r, wide)
	case opcode.I32Ctz, opcode.I64Ctz:
		a.Rbit(r, r, wide)
		a.Clz(r, r, wide)
	default:
		// An i32's upper half is clear.
		a.FMovToF(xTmp, r)
		a.Cnt8B(xTmp, xTmp)
		a.Addv8B(xTmp, xTmp)
		a.FMovFromS(r, xTmp)
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
func (c *compiler) setCond(cond arm64.Cond) {
	if c.fusable() {
		c.push(value{t: i32, k: kFlags, c: uint64(cond)})
		return
	}
	r := c.allocG(0)
	c.a.Cset(r, cond)
	c.pushReg(i32, uint8(r))
}

func (c *compiler) eqz(wide bool) {
	v := c.pop()
	c.a.CmpImm(c.readG(v, rTmp), 0, wide)
	c.free(v)
	c.setCond(arm64.EQ)
}

// mirror is the condition of a comparison with its operands swapped.
func mirror(cond arm64.Cond) arm64.Cond {
	switch cond {
	case arm64.LT:
		return arm64.GT
	case arm64.GT:
		return arm64.LT
	case arm64.LE:
		return arm64.GE
	case arm64.GE:
		return arm64.LE
	case arm64.LO:
		return arm64.HI
	case arm64.HI:
		return arm64.LO
	case arm64.LS:
		return arm64.HS
	case arm64.HS:
		return arm64.LS
	}
	return cond
}

func (c *compiler) compare(cond arm64.Cond, wide bool) {
	b := c.pop()
	x := c.pop()
	if x.k == kConst && b.k != kConst {
		x, b = b, x
		cond = mirror(cond)
	}
	rx := c.readG(x, rTmp)
	if b.k == kConst {
		imm := int64(b.c)
		if !wide {
			imm = int64(uint32(b.c))
		}
		c.a.CmpImm(rx, imm, wide)
	} else {
		c.a.Cmp(rx, c.readG(b, rTmp2), wide)
	}
	c.free(x)
	c.free(b)
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
func (c *compiler) extend(f func(arm64.Reg), t module.Kind) {
	r := c.intoG(c.pop(), 0)
	f(r)
	c.pushReg(t, uint8(r))
}

// floatOp compiles a two-operand float instruction. min and max are FMIN
// and FMAX: a NaN operand gives the NaN an addition of the operands does,
// as the interpreter's a + b, and -0 is below +0.
func (c *compiler) floatOp(op arm64.FOp) {
	b := c.pop()
	x := c.pop()
	xa := c.intoX(x)
	xb := c.readX(b)
	if x.t == f32 {
		c.a.FArithS(op, xa, xa, xb)
	} else {
		c.a.FArith(op, xa, xa, xb)
	}
	c.free(b)
	c.pushReg(x.t, uint8(xa))
}

// signBit compiles abs (op And, the sign cleared) and neg (op Eor, the
// sign flipped): bit operations in a general register, which leave a
// NaN's payload as it is.
func (c *compiler) signBit(op arm64.ALU, wide bool) {
	v := c.pop()
	x := c.intoX(v)
	sign := uint64(1) << 31
	if wide {
		sign = 1 << 63
	}
	m := sign
	if op == arm64.And {
		m = sign - 1
	}
	if wide {
		c.a.FMovFromF(rTmp, x)
		c.a.LogicImm(op, rTmp, rTmp, m, true)
		c.a.FMovToF(x, rTmp)
	} else {
		c.a.FMovFromS(rTmp, x)
		c.a.LogicImm(op, rTmp, rTmp, m, false)
		c.a.FMovToS(x, rTmp)
	}
	c.pushReg(v.t, uint8(x))
}

// copySign compiles copysign: the first operand's bits but the sign, the
// second's sign, in general registers.
func (c *compiler) copySign(wide bool) {
	a := c.a
	b := c.pop()
	v := c.pop()
	x := c.intoX(v)
	xb := c.readX(b)
	sign := uint64(1) << 31
	if wide {
		sign = 1 << 63
		a.FMovFromF(rTmp, x)
		a.FMovFromF(rTmp2, xb)
	} else {
		a.FMovFromS(rTmp, x)
		a.FMovFromS(rTmp2, xb)
	}
	a.LogicImm(arm64.And, rTmp, rTmp, sign-1, wide)
	a.LogicImm(arm64.And, rTmp2, rTmp2, sign, wide)
	a.Op(arm64.Orr, rTmp, rTmp, rTmp2, wide)
	if wide {
		a.FMovToF(x, rTmp)
	} else {
		a.FMovToS(x, rTmp)
	}
	c.free(b)
	c.pushReg(v.t, uint8(x))
}

// floatUnary applies f to the top float, in a register of its own.
func (c *compiler) floatUnary(f func(arm64.FReg)) {
	v := c.pop()
	x := c.intoX(v)
	f(x)
	c.pushReg(v.t, uint8(x))
}

// roundMode is the FRINT rounding of ceil, floor, trunc and nearest, which
// quiet a NaN as the interpreter's a + a does.
func roundMode(op opcode.Opcode) arm64.FRound {
	switch op {
	case opcode.F32Ceil, opcode.F64Ceil:
		return arm64.FRintP
	case opcode.F32Floor, opcode.F64Floor:
		return arm64.FRintM
	case opcode.F32Trunc, opcode.F64Trunc:
		return arm64.FRintZ
	}
	return arm64.FRintN
}

// floatCompare compiles a comparison of floats, whose condition is false
// for unordered operands but for ne: MI for lt and LS for le, which are
// the unsigned conditions' bits, and GT and GE, which an unordered
// result's V flag makes false.
func (c *compiler) floatCompare(cond arm64.Cond) {
	b := c.pop()
	x := c.pop()
	xa := c.readXIn(x, xTmp)
	xb := c.readXIn(b, xTmp2)
	if x.t == f32 {
		c.a.FCmpS(xa, xb)
	} else {
		c.a.FCmp(xa, xb)
	}
	c.free(x)
	c.free(b)
	c.setCond(cond)
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
			c.a.FMovToS(x, v.gpr())
		} else {
			c.a.FMovToF(x, v.gpr())
		}
		c.free(v)
		c.pushReg(t, uint8(x))
		return
	}
	r := c.allocG(0)
	if t == i32 {
		c.a.FMovFromS(r, v.xmm())
	} else {
		c.a.FMovFromF(r, v.xmm())
	}
	c.free(v)
	c.pushReg(t, uint8(r))
}

// intToFloat compiles the conversions of integers to floats: SCVTF and
// UCVTF round once, the 64-bit unsigned conversions included.
func (c *compiler) intToFloat(op opcode.Opcode) {
	v := c.pop()
	single := op == opcode.F32SConvertI32 || op == opcode.F32UConvertI32 || op == opcode.F32SConvertI64 || op == opcode.F32UConvertI64
	signed := op == opcode.F32SConvertI32 || op == opcode.F32SConvertI64 || op == opcode.F64SConvertI32 || op == opcode.F64SConvertI64
	wide := v.t == i64
	r := c.readG(v, rTmp)
	x := c.allocX(0)
	c.a.Cvtf(x, r, signed, wide, single)
	c.free(v)
	t := f64
	if single {
		t = f32
	}
	c.pushReg(t, uint8(x))
}

// truncBounds is the range a trapping truncation's operand must be in, by
// the interpreter's bounds: above lo, or at it if loIn, and below hi. Each
// is exact in the operand's type.
func truncBounds(op opcode.Opcode) (lo, hi float64, loIn bool) {
	switch op {
	case opcode.I32SConvertF32:
		return -2147483648.0, 2147483648.0, true
	case opcode.I32SConvertF64:
		return -2147483649.0, 2147483648.0, false
	case opcode.I32UConvertF32, opcode.I32UConvertF64:
		return -1, 4294967296.0, false
	case opcode.I64SConvertF32, opcode.I64SConvertF64:
		return -9223372036854775808.0, 9223372036854775808.0, true
	}
	return -1, 18446744073709551616.0, false
}

// truncate2int compiles the trapping truncations: of a NaN, or a value out
// of the result's range, by the interpreter's bounds. In range, FCVTZS and
// FCVTZU truncate exactly.
func (c *compiler) truncate2int(op opcode.Opcode, at uint32) {
	a := c.a
	v := c.pop()
	single := v.t == f32
	x := c.readX(v)
	lo, hi, loIn := truncBounds(op)
	trap := c.trap(TrapFloatUnrepresentable, at)
	cmp := func(bound float64) {
		bits := math.Float64bits(bound)
		if single {
			bits = uint64(math.Float32bits(float32(bound)))
		}
		c.loadXConst(xTmp2, bits, single)
		if single {
			a.FCmpS(x, xTmp2)
		} else {
			a.FCmp(x, xTmp2)
		}
	}
	// Below lo, or unordered: LT and LE hold for an unordered result.
	cmp(lo)
	if loIn {
		a.BCond(arm64.LT, trap)
	} else {
		a.BCond(arm64.LE, trap)
	}
	cmp(hi)
	a.BCond(arm64.GE, trap)
	signed := op == opcode.I32SConvertF32 || op == opcode.I32SConvertF64 || op == opcode.I64SConvertF32 || op == opcode.I64SConvertF64
	wide := op == opcode.I64SConvertF32 || op == opcode.I64SConvertF64 || op == opcode.I64UConvertF32 || op == opcode.I64UConvertF64
	c.free(v)
	r := c.allocG(0)
	a.Fcvtz(r, x, signed, wide, single)
	c.pushReg(kindOf(wide), uint8(r))
}

// saturate compiles the saturating truncations: FCVTZS and FCVTZU clamp to
// the result's range and give 0 for NaN, as the interpreter's do.
func (c *compiler) saturate(op opcode.Opcode) {
	v := c.pop()
	x := c.readX(v)
	signed := op == opcode.I32SConvertSatF32 || op == opcode.I32SConvertSatF64 || op == opcode.I64SConvertSatF32 || op == opcode.I64SConvertSatF64
	wide := op == opcode.I64SConvertSatF32 || op == opcode.I64SConvertSatF64 || op == opcode.I64UConvertSatF32 || op == opcode.I64UConvertSatF64
	c.free(v)
	r := c.allocG(0)
	c.a.Fcvtz(r, x, signed, wide, v.t == f32)
	c.pushReg(kindOf(wide), uint8(r))
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
	base, idx := c.address(c.pop(), mem, off, size, at, false)
	a := c.a
	if isFloat(t) {
		x := c.allocX(0)
		if t == f32 {
			a.LoadSIdx(x, base, idx)
		} else {
			a.LoadFIdx(x, base, idx)
		}
		c.freeG(idx)
		c.freeBase(base)
		c.pushReg(t, uint8(x))
		return true
	}
	r := idx
	switch op {
	case opcode.I32LoadMem, opcode.I64LoadMem32U:
		a.LoadU32Idx(r, base, idx)
	case opcode.I64LoadMem:
		a.LoadIdx(r, base, idx, false)
	case opcode.I32LoadMem8S:
		a.LoadS8Idx(r, base, idx, false)
	case opcode.I64LoadMem8S:
		a.LoadS8Idx(r, base, idx, true)
	case opcode.I32LoadMem8U, opcode.I64LoadMem8U:
		a.LoadU8Idx(r, base, idx)
	case opcode.I32LoadMem16S:
		a.LoadS16Idx(r, base, idx, false)
	case opcode.I64LoadMem16S:
		a.LoadS16Idx(r, base, idx, true)
	case opcode.I32LoadMem16U, opcode.I64LoadMem16U:
		a.LoadU16Idx(r, base, idx)
	case opcode.I64LoadMem32S:
		a.LoadS32Idx(r, base, idx)
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
	// The address first: its check takes the scratch registers, which the
	// value may then be read into.
	base, idx := c.address(addr, mem, off, size, at, false)
	if isFloat(v.t) {
		x := c.readX(v)
		if v.t == f32 {
			a.StoreSIdx(base, idx, x)
		} else {
			a.StoreFIdx(base, idx, x)
		}
	} else {
		rv := arm64.ZR
		if v.k != kConst || v.c != 0 {
			rv = c.readG(v, rTmp)
		}
		switch size {
		case 1:
			a.Store8Idx(base, idx, rv)
		case 2:
			a.Store16Idx(base, idx, rv)
		case 4:
			a.Store32Idx(base, idx, rv)
		default:
			a.StoreIdx(base, idx, rv)
		}
	}
	c.free(v)
	c.freeG(idx)
	c.freeBase(base)
}

// freeBase gives back the register address returned a memory's base in,
// unless it is memory 0's, which the code keeps.
func (c *compiler) freeBase(base arm64.Reg) {
	if base != rMem {
		c.freeG(base)
	}
}

// address checks an access of size bytes at address addr plus offset off
// against memory mem's size, trapping past it, and returns the register
// holding the memory's base -- X20 for memory 0 -- and a register of the
// instruction's own holding the effective address, to add to the base.
//
// An i32 address is zero-extended and the offset added in 64 bits, which
// nothing wraps. An i64 memory's address plus the offset may: it traps
// where it carries, as the sum plus the size does. An atomic access
// (align) traps, after that carry, if the effective address is not a
// multiple of size.
func (c *compiler) address(addr value, mem uint32, off, size uint64, at uint32, align bool) (base, idx arm64.Reg) {
	a := c.a
	trap := c.trap(TrapMemOutOfBounds, at)
	is64 := c.m.Memories[mem].Limits.Is64
	align = align && size > 1
	var r arm64.Reg
	switch {
	case addr.k == kConst:
		ea, carry := uint64(uint32(addr.c))+off, uint64(0)
		if is64 {
			ea, carry = bits.Add64(addr.c, off, 0)
		}
		_, carry2 := bits.Add64(ea, size, 0)
		switch {
		case carry != 0:
			a.B(trap)
		case align && ea&(size-1) != 0:
			a.B(c.trap(TrapUnalignedAccess, at))
		case carry2 != 0:
			a.B(trap)
		}
		// Past a branch to a trap, nothing below is reached.
		r = c.allocG(0)
		a.MovImm(r, ea)
		align = false
	case is64:
		r = c.intoG(addr, 0)
		if off != 0 {
			a.MovImm(rTmp, off)
			a.Op(arm64.Adds, r, r, rTmp, true)
			a.BCond(arm64.HS, trap)
		}
	default:
		r = c.intoG(addr, 0)
		if off != 0 {
			a.AddImm(r, r, int64(off), true)
		}
	}
	if align {
		a.LogicImm(arm64.Ands, arm64.ZR, r, size-1, true)
		a.BCond(arm64.NE, c.trap(TrapUnalignedAccess, at))
	}
	if is64 {
		a.AddsImm(rTmp, r, int64(size), true)
		a.BCond(arm64.HS, trap)
	} else {
		a.AddImm(rTmp, r, int64(size), true)
	}
	return c.boundsCheck(mem, trap, 1<<r), r
}

// boundsCheck goes to trap if the end of an access, in rTmp, is past memory
// mem's size, and returns the register holding the memory's base: X20 for
// memory 0, whose size X21 holds, else a register of the instruction's own,
// not in avoid, loaded from the memories' table. A shared memory's size is
// read where other threads store it.
func (c *compiler) boundsCheck(mem uint32, trap arm64.Label, avoid regMask) arm64.Reg {
	a := c.a
	shared := c.m.Memories[mem].Shared
	if mem == 0 {
		if shared {
			a.Load(rTmp2, rCtx, offMemSizePtr)
			a.Load(rTmp2, rTmp2, 0)
			a.Cmp(rTmp, rTmp2, true)
		} else {
			a.Cmp(rTmp, rMemSize, true)
		}
		a.BCond(arm64.HI, trap)
		return rMem
	}
	p := c.allocG(avoid)
	// The address of the memory's entry in the memories' table (Ctx.Mems),
	// whose three words are at it plus d, d+8 and d+16.
	var d int32
	if e := uint64(mem) * MemEntryWords * 8; e+16 <= math.MaxInt32 {
		a.Load(p, rCtx, offMems)
		d = int32(e)
	} else {
		a.MovImm(p, e)
		a.Load(rTmp2, rCtx, offMems)
		a.Op(arm64.Add, p, p, rTmp2, true)
	}
	if shared {
		a.Load(rTmp2, p, d+16)
		a.Load(rTmp2, rTmp2, 0)
	} else {
		a.Load(rTmp2, p, d+8)
	}
	a.Cmp(rTmp, rTmp2, true)
	a.BCond(arm64.HI, trap)
	a.Load(p, p, d)
	return p
}
