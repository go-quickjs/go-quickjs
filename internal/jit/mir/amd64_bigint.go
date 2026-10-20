package mir

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// BigInts of 64 bits (ssa's bigint.go): Uint64s from 0 to 2^64-1, held in
// their values in the int64 range's encoding below 2^63 and its wide one
// past it (abi.Encoding's ShortBigWideTag), and the words of BigInts' low
// 64 bits.

// bigHighBits is how many high bits of a BigInt held in its value its
// pointer word holds, as its distance from ShortBigHigh: the int64's 15.
func (c *compiler) bigHighBits() uint8 { return 64 - c.enc.ShortBigLowBits }

// bigSourceWord puts the pointer word of x, a tagged value, in scratchB,
// read where it came from (keepSource), exiting where it has no source.
// It takes scratchA and scratchC.
func (c *compiler) bigSourceWord(x *ssa.Value, guard func(amd64.Cond)) {
	c.keepSource(x)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	guard(amd64.CondE)
	c.a.Load(scratchB, scratchC, c.enc.RefOffset)
}

// bigTagInto puts x's number word's tag, its bits past the low ones a
// BigInt held in its value keeps there, in dst.
func (c *compiler) bigTagInto(x *ssa.Value, dst amd64.Reg) {
	if r := c.gpr(x, dst); r != dst {
		c.a.MovRR(dst, r)
	}
	c.a.ShiftImm(amd64.Shr, dst, c.enc.ShortBigLowBits, true)
}

// bigJoin makes, in scratchB, a BigInt's word of its high bits, in
// scratchB, and the low bits of x's number word, using scratchA.
func (c *compiler) bigJoin(x *ssa.Value) {
	low := c.bigHighBits()
	c.a.ShiftImm(amd64.Shl, scratchB, c.enc.ShortBigLowBits, true)
	if r := c.gpr(x, scratchA); r != scratchA {
		c.a.MovRR(scratchA, r)
	}
	c.a.ShiftImm(amd64.Shl, scratchA, low, true)
	c.a.ShiftImm(amd64.Shr, scratchA, low, true)
	c.a.Op(amd64.Or, scratchB, scratchA, true)
}

// bigUnboxU64 is the BigInt a tagged value is, from 0 to 2^64-1 (ssa's
// OpBigUnboxU64), which it guards: held in the value in the int64 range,
// its high bits below 2^14, its sign clear; or past it, its 16 high bits
// below 2^15, the 65th, the sign's, clear.
func (c *compiler) bigUnboxU64(v *ssa.Value, guard func(amd64.Cond)) {
	x := v.Args[0]
	wide, join := c.a.NewLabel(), c.a.NewLabel()
	c.bigSourceWord(x, guard)
	c.bigTagInto(x, scratchA)
	c.a.OpImm(amd64.Cmp, scratchA, int32(c.enc.ShortBigTag>>c.enc.ShortBigLowBits), true)
	c.a.Jcc(amd64.CondNE, wide)
	c.movImm(scratchA, c.enc.ShortBigHigh)
	c.a.Op(amd64.Sub, scratchB, scratchA, true)
	c.a.OpImm(amd64.Cmp, scratchB, 1<<(c.bigHighBits()-1), true)
	guard(amd64.CondAE)
	c.a.Jmp(join)
	c.a.Bind(wide)
	c.a.OpImm(amd64.Cmp, scratchA, int32(c.enc.ShortBigWideTag>>c.enc.ShortBigLowBits), true)
	guard(amd64.CondNE)
	c.movImm(scratchA, c.enc.ShortBigWideHigh)
	c.a.Op(amd64.Sub, scratchB, scratchA, true)
	c.a.OpImm(amd64.Cmp, scratchB, 1<<c.bigHighBits(), true)
	guard(amd64.CondAE)
	c.a.Bind(join)
	c.bigJoin(x)
	c.setG(v, scratchB)
}

// bigLow is the low 64 bits of a BigInt's two's complement (ssa's
// OpBigLow), which it guards to be one: held in the value, either way --
// its 64 or 65 bits' low 64 -- or, out of line, on the heap, its first
// digit's word, negated if it is below zero.
func (c *compiler) bigLow(v *ssa.Value, guard func(amd64.Cond)) {
	x := v.Args[0]
	wide, heap, join, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.bigSourceWord(x, guard)
	c.bigTagInto(x, scratchA)
	c.a.OpImm(amd64.Cmp, scratchA, int32(c.enc.ShortBigTag>>c.enc.ShortBigLowBits), true)
	c.a.Jcc(amd64.CondNE, wide)
	c.movImm(scratchA, c.enc.ShortBigHigh)
	c.a.Op(amd64.Sub, scratchB, scratchA, true)
	c.a.Jmp(join)
	c.a.Bind(wide)
	c.a.OpImm(amd64.Cmp, scratchA, int32(c.enc.ShortBigWideTag>>c.enc.ShortBigLowBits), true)
	c.a.Jcc(amd64.CondNE, heap)
	c.movImm(scratchA, c.enc.ShortBigWideHigh)
	c.a.Op(amd64.Sub, scratchB, scratchA, true)
	c.a.Bind(join)
	c.bigJoin(x)
	c.a.Bind(done)
	c.setG(v, scratchB)
	c.cold = append(c.cold, func() {
		c.a.Bind(heap)
		c.cmpWord(c.gpr(x, scratchA), c.enc.HeapBigTag)
		guard(amd64.CondNE)
		// Every BigInt on the heap is past 64 bits: two digits at least.
		c.a.Load(scratchA, scratchB, 0)
		c.a.Load(scratchB, scratchB, c.enc.BigDigits)
		c.a.Op(amd64.Test, scratchA, scratchA, true)
		c.a.Jcc(amd64.CondNS, done)
		c.a.Op(amd64.Xor, scratchA, scratchA, false)
		c.a.Op(amd64.Sub, scratchA, scratchB, true)
		c.a.MovRR(scratchB, scratchA)
		c.a.Jmp(done)
	})
}

// bigBoxWords makes, of v's operand, an unboxed BigInt, its value's words:
// its pointer word in scratchC and its number word in scratchA -- an
// Int64's, or a Uint64's below 2^63, in the int64 range's encoding, a
// Uint64's past it in the wide one. It uses scratchB.
func (c *compiler) bigBoxWords(v *ssa.Value) {
	x := v.Args[0]
	low := c.bigHighBits()
	if r := c.gpr(x, scratchB); r != scratchC {
		c.a.MovRR(scratchC, r)
	}
	c.a.ShiftImm(amd64.Shr, scratchC, c.enc.ShortBigLowBits, true)
	wide, have := c.a.NewLabel(), c.a.NewLabel()
	if x.Type == ssa.Uint64 {
		c.a.Op(amd64.Test, c.gpr(x, scratchB), c.gpr(x, scratchB), true)
		c.a.Jcc(amd64.CondS, wide)
	}
	c.movImm(scratchA, c.enc.ShortBigHigh)
	c.a.Op(amd64.Add, scratchC, scratchA, true)
	c.movImm(scratchB, c.enc.ShortBigTag)
	if x.Type == ssa.Uint64 {
		c.a.Jmp(have)
		c.a.Bind(wide)
		c.movImm(scratchA, c.enc.ShortBigWideHigh)
		c.a.Op(amd64.Add, scratchC, scratchA, true)
		c.movImm(scratchB, c.enc.ShortBigWideTag)
		c.a.Bind(have)
	}
	if r := c.gpr(x, scratchA); r != scratchA {
		c.a.MovRR(scratchA, r)
	}
	c.a.ShiftImm(amd64.Shl, scratchA, low, true)
	c.a.ShiftImm(amd64.Shr, scratchA, low, true)
	c.a.Op(amd64.Or, scratchA, scratchB, true)
}

// bigWord makes the words' operators of ssa's bigint.go, which exit
// nowhere, and the checked ones, which do; and reports whether v is one.
func (c *compiler) bigWord(v *ssa.Value, guard func(amd64.Cond)) bool {
	arg := func(i int) *ssa.Value { return v.Args[i] }
	into := func(x *ssa.Value) {
		if r := c.gpr(x, scratchA); r != scratchA {
			c.a.MovRR(scratchA, r)
		}
	}
	switch v.Op {
	case ssa.OpBigUnboxU64:
		c.bigUnboxU64(v, guard)
	case ssa.OpBigLow:
		c.bigLow(v, guard)
	case ssa.OpBigSign:
		x := c.gpr(arg(0), scratchA)
		c.a.Op(amd64.Test, x, x, true)
		guard(amd64.CondS)
		c.setG(v, x)
	case ssa.OpAddW, ssa.OpSubW, ssa.OpAndW, ssa.OpOrW, ssa.OpXorW, ssa.OpAddU64, ssa.OpSubU64:
		into(arg(0))
		y := c.gpr(arg(1), scratchB)
		op := map[ssa.Op]amd64.ALU{ssa.OpAddW: amd64.Add, ssa.OpSubW: amd64.Sub, ssa.OpAndW: amd64.And, ssa.OpOrW: amd64.Or,
			ssa.OpXorW: amd64.Xor, ssa.OpAddU64: amd64.Add, ssa.OpSubU64: amd64.Sub}[v.Op]
		c.a.Op(op, scratchA, y, true)
		if v.Op == ssa.OpAddU64 || v.Op == ssa.OpSubU64 {
			// A carry or a borrow: past 0 to 2^64-1.
			guard(amd64.CondB)
		}
		c.setG(v, scratchA)
	case ssa.OpMulW:
		into(arg(0))
		c.a.Imul(scratchA, c.gpr(arg(1), scratchB))
		c.setG(v, scratchA)
	case ssa.OpMulU64:
		// MUL writes RDX, the operands' base, which is read again after it.
		into(arg(0))
		c.a.Mul(c.gpr(arg(1), scratchB))
		c.a.MovRR(scratchC, regStack)
		c.a.Load(regStack, regCtx, abi.OffStack)
		c.a.Op(amd64.Test, scratchC, scratchC, true)
		guard(amd64.CondNE)
		c.setG(v, scratchA)
	case ssa.OpShlW, ssa.OpShrW, ssa.OpSarW:
		into(arg(0))
		if n := uint8(v.Index); n != 0 {
			op := map[ssa.Op]amd64.Shift{ssa.OpShlW: amd64.Shl, ssa.OpShrW: amd64.Shr, ssa.OpSarW: amd64.Sar}[v.Op]
			c.a.ShiftImm(op, scratchA, n, true)
		}
		c.setG(v, scratchA)
	case ssa.OpZextW, ssa.OpSextW:
		into(arg(0))
		if n := uint8(v.Index); n < 64 {
			back := amd64.Shr
			if v.Op == ssa.OpSextW {
				back = amd64.Sar
			}
			c.a.ShiftImm(amd64.Shl, scratchA, 64-n, true)
			c.a.ShiftImm(back, scratchA, 64-n, true)
		}
		c.setG(v, scratchA)
	case ssa.OpShlI64, ssa.OpShlU64:
		// Shifted back, it is what it was where no bit was lost.
		into(arg(0))
		if n := uint8(v.Index); n != 0 {
			back := amd64.Sar
			if v.Op == ssa.OpShlU64 {
				back = amd64.Shr
			}
			c.a.ShiftImm(amd64.Shl, scratchA, n, true)
			c.a.MovRR(scratchB, scratchA)
			c.a.ShiftImm(back, scratchB, n, true)
			c.a.Op(amd64.Cmp, scratchB, c.gpr(arg(0), scratchC), true)
			guard(amd64.CondNE)
		}
		c.setG(v, scratchA)
	case ssa.OpDivI64, ssa.OpModI64, ssa.OpDivU64, ssa.OpModU64:
		c.bigDivide(v, guard)
	case ssa.OpPowI64:
		c.bigPower(v, guard)
	case ssa.OpBigOfF64:
		x := c.xmm(arg(0), xScratch0)
		c.a.Cvttsd2si(scratchA, x)
		c.a.Cvtsi2sd(xScratch2, scratchA, true)
		c.a.SSEOp(amd64.UcomiSD, xScratch2, x)
		guard(amd64.CondNE)
		guard(amd64.CondP)
		c.setG(v, scratchA)
	default:
		return false
	}
	return true
}

// bigDivide is a BigInt division or remainder of two Int64s or two
// Uint64s, which exits for a zero divisor, and a signed division for the
// most negative int64 over -1: the remainder of anything over -1 is 0,
// which IDIV would fault at. IDIV and DIV take RDX, the operands' base,
// which is read again after them.
func (c *compiler) bigDivide(v *ssa.Value, guard func(amd64.Cond)) {
	signed := v.Op == ssa.OpDivI64 || v.Op == ssa.OpModI64
	mod := v.Op == ssa.OpModI64 || v.Op == ssa.OpModU64
	if y := c.gpr(v.Args[1], scratchC); y != scratchC {
		c.a.MovRR(scratchC, y)
	}
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	guard(amd64.CondE)
	if r := c.gpr(v.Args[0], scratchA); r != scratchA {
		c.a.MovRR(scratchA, r)
	}
	minus, done := c.a.NewLabel(), c.a.NewLabel()
	if signed {
		c.a.OpImm(amd64.Cmp, scratchC, -1, true)
		c.a.Jcc(amd64.CondE, minus)
		c.a.Cqo()
		c.a.Idiv(scratchC)
	} else {
		c.a.Op(amd64.Xor, regStack, regStack, false)
		c.a.Div(scratchC)
	}
	if mod {
		c.a.MovRR(scratchA, regStack)
	}
	c.a.Load(regStack, regCtx, abi.OffStack)
	if signed {
		c.a.Jmp(done)
		c.a.Bind(minus)
		if mod {
			c.a.Op(amd64.Xor, scratchA, scratchA, false)
		} else {
			// -x, which only the most negative int64 overflows.
			c.a.Op(amd64.Xor, scratchB, scratchB, false)
			c.a.Op(amd64.Sub, scratchB, scratchA, true)
			guard(amd64.CondO)
			c.a.MovRR(scratchA, scratchB)
		}
		c.a.Bind(done)
	}
	c.setG(v, scratchA)
}

// bigPower is x ** y of two Int64s (ssa's OpPowI64), by squaring and
// multiplying, which exits for a negative exponent and for any product
// past the int64 range: a square past it is one only an exponent with a
// bit above makes, whose power is past it too, but for a base of -1, 0 or
// 1, whose squares are 1 and 0. The power in scratchA, the base in
// scratchB, the exponent in scratchC.
func (c *compiler) bigPower(v *ssa.Value, guard func(amd64.Cond)) {
	if y := c.gpr(v.Args[1], scratchC); y != scratchC {
		c.a.MovRR(scratchC, y)
	}
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	guard(amd64.CondS)
	if x := c.gpr(v.Args[0], scratchB); x != scratchB {
		c.a.MovRR(scratchB, x)
	}
	c.movImm(scratchA, 1)
	loop, skip, square, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	// The exponent's low bit shifted out into the carry, the zero flag
	// whether bits are left.
	c.a.Bind(loop)
	c.a.ShiftImm(amd64.Shr, scratchC, 1, true)
	c.a.Jcc(amd64.CondAE, skip)
	c.a.Imul(scratchA, scratchB)
	guard(amd64.CondO)
	c.a.Op(amd64.Test, scratchC, scratchC, true)
	c.a.Jcc(amd64.CondE, done)
	c.a.Jmp(square)
	c.a.Bind(skip)
	c.a.Jcc(amd64.CondE, done)
	c.a.Bind(square)
	c.a.Imul(scratchB, scratchB)
	guard(amd64.CondO)
	c.a.Jmp(loop)
	c.a.Bind(done)
	c.setG(v, scratchA)
}

// bigCmpCond is the condition an OpCmpI64 of operator op holds at, of
// Uint64s unsigned.
func bigCmpCond(op ir.Operator, unsigned bool) amd64.Cond {
	switch op {
	case ir.Le:
		if unsigned {
			return amd64.CondBE
		}
		return amd64.CondLE
	case ir.Gt:
		if unsigned {
			return amd64.CondA
		}
		return amd64.CondG
	case ir.Ge:
		if unsigned {
			return amd64.CondAE
		}
		return amd64.CondGE
	}
	if unsigned {
		return amd64.CondB
	}
	return amd64.CondL
}

// bigRebox makes v, the value a BigInt's box keeps in its cell (ssa's
// OpBigBox), again for an exit: its words made of the unboxed BigInt and
// written to the cell -- with Go's write barrier while the collector
// marks -- its number word in scratchA, which it returns. It appends no
// cold code: an exit's is laid out after the cold code.
func (c *compiler) bigRebox(v *ssa.Value) amd64.Reg {
	box := v.Args[1]
	at := abi.OffKeep + int32(box.Index)*int32(c.enc.ValueSize)
	slow, done := c.a.NewLabel(), c.a.NewLabel()
	c.bigBoxWords(box)
	c.a.MovQToX(xScratch0, scratchA)
	c.movImm(scratchB, c.enc.WriteBarrier)
	c.a.LoadU8(scratchB, scratchB, 0)
	c.a.Op(amd64.Test, scratchB, scratchB, false)
	c.a.Jcc(amd64.CondNE, slow)
	c.a.Store(regCtx, at+c.enc.RefOffset, scratchC)
	c.a.Store(regCtx, at+c.enc.NumOffset, scratchA)
	c.a.Jmp(done)
	c.a.Bind(slow)
	c.a.MovRR(scratchA, regCtx)
	c.a.OpImm(amd64.Add, scratchA, at, true)
	c.valueStore()
	c.a.Load(scratchA, regCtx, at+c.enc.NumOffset)
	c.a.Bind(done)
	return scratchA
}
