package mir

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// BigInts of 64 bits, as amd64's (amd64_bigint.go).

// tmpBig is the encoder's temporary, X27, which a product's check holds
// a word in between instructions that make no use of it.
const tmpBig = arm64.X27

func (c *a64Compiler) bigHighBits() uint8 { return 64 - c.enc.ShortBigLowBits }

// bigSourceWord puts the pointer word of x, a tagged value, in B, read
// where it came from (keepSource), exiting where it has no source. It
// takes A and C.
func (c *a64Compiler) bigSourceWord(x *ssa.Value, guard func(arm64.Cond)) {
	c.keepSource(x)
	c.a.CmpImm(a64C, 0, true)
	guard(arm64.EQ)
	c.a.Load(a64B, a64C, c.enc.RefOffset)
}

// bigTagInto puts x's number word's tag in dst.
func (c *a64Compiler) bigTagInto(x *ssa.Value, dst arm64.Reg) {
	c.a.ShiftImm(arm64.Lsr, dst, c.gpr(x, dst), c.enc.ShortBigLowBits, true)
}

// bigJoin makes, in B, a BigInt's word of its high bits, in B, and the low
// bits of x's number word, using A.
func (c *a64Compiler) bigJoin(x *ssa.Value) {
	low := c.bigHighBits()
	c.a.ShiftImm(arm64.Lsl, a64B, a64B, c.enc.ShortBigLowBits, true)
	c.a.ShiftImm(arm64.Lsl, a64A, c.gpr(x, a64A), low, true)
	c.a.ShiftImm(arm64.Lsr, a64A, a64A, low, true)
	c.a.Op(arm64.Orr, a64B, a64B, a64A, true)
}

// bigTagIs compares A, a number word's tag, with tag's.
func (c *a64Compiler) bigTagIs(tag uint64) {
	c.movImm(a64C, tag>>c.enc.ShortBigLowBits)
	c.a.Cmp(a64A, a64C, true)
}

// bigUnboxU64 is amd64's.
func (c *a64Compiler) bigUnboxU64(v *ssa.Value, guard func(arm64.Cond)) {
	x := v.Args[0]
	wide, join := c.a.NewLabel(), c.a.NewLabel()
	c.bigSourceWord(x, guard)
	c.bigTagInto(x, a64A)
	c.bigTagIs(c.enc.ShortBigTag)
	c.a.BCond(arm64.NE, wide)
	c.movImm(a64A, c.enc.ShortBigHigh)
	c.a.Op(arm64.Sub, a64B, a64B, a64A, true)
	c.a.CmpImm(a64B, 1<<(c.bigHighBits()-1), true)
	guard(arm64.HS)
	c.a.B(join)
	c.a.Bind(wide)
	c.bigTagIs(c.enc.ShortBigWideTag)
	guard(arm64.NE)
	c.movImm(a64A, c.enc.ShortBigWideHigh)
	c.a.Op(arm64.Sub, a64B, a64B, a64A, true)
	c.a.CmpImm(a64B, 1<<c.bigHighBits(), true)
	guard(arm64.HS)
	c.a.Bind(join)
	c.bigJoin(x)
	c.setG(v, a64B)
}

// bigLow is amd64's.
func (c *a64Compiler) bigLow(v *ssa.Value, guard func(arm64.Cond)) {
	x := v.Args[0]
	wide, heap, join, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	c.bigSourceWord(x, guard)
	c.bigTagInto(x, a64A)
	c.bigTagIs(c.enc.ShortBigTag)
	c.a.BCond(arm64.NE, wide)
	c.movImm(a64A, c.enc.ShortBigHigh)
	c.a.Op(arm64.Sub, a64B, a64B, a64A, true)
	c.a.B(join)
	c.a.Bind(wide)
	c.bigTagIs(c.enc.ShortBigWideTag)
	c.a.BCond(arm64.NE, heap)
	c.movImm(a64A, c.enc.ShortBigWideHigh)
	c.a.Op(arm64.Sub, a64B, a64B, a64A, true)
	c.a.Bind(join)
	c.bigJoin(x)
	c.a.Bind(done)
	c.setG(v, a64B)
	c.cold = append(c.cold, func() {
		c.a.Bind(heap)
		c.movImm(a64C, c.enc.HeapBigTag)
		c.a.Cmp(c.gpr(x, a64A), a64C, true)
		guard(arm64.NE)
		// Every BigInt on the heap is past 64 bits: two digits at least.
		c.a.Load(a64A, a64B, 0)
		c.a.Load(a64B, a64B, c.enc.BigDigits)
		c.a.CmpImm(a64A, 0, true)
		c.a.BCond(arm64.PL, done)
		c.a.Neg(a64B, a64B, true)
		c.a.B(done)
	})
}

// bigBoxWords is amd64's: the pointer word in C and the number word in A.
func (c *a64Compiler) bigBoxWords(v *ssa.Value) {
	x := v.Args[0]
	low := c.bigHighBits()
	c.a.ShiftImm(arm64.Lsr, a64C, c.gpr(x, a64B), c.enc.ShortBigLowBits, true)
	wide, have := c.a.NewLabel(), c.a.NewLabel()
	if x.Type == ssa.Uint64 {
		c.a.CmpImm(c.gpr(x, a64B), 0, true)
		c.a.BCond(arm64.MI, wide)
	}
	c.movImm(a64A, c.enc.ShortBigHigh)
	c.a.Op(arm64.Add, a64C, a64C, a64A, true)
	c.movImm(a64B, c.enc.ShortBigTag)
	if x.Type == ssa.Uint64 {
		c.a.B(have)
		c.a.Bind(wide)
		c.movImm(a64A, c.enc.ShortBigWideHigh)
		c.a.Op(arm64.Add, a64C, a64C, a64A, true)
		c.movImm(a64B, c.enc.ShortBigWideTag)
		c.a.Bind(have)
	}
	c.a.ShiftImm(arm64.Lsl, a64A, c.gpr(x, a64A), low, true)
	c.a.ShiftImm(arm64.Lsr, a64A, a64A, low, true)
	c.a.Op(arm64.Orr, a64A, a64A, a64B, true)
}

// bigWord is amd64's.
func (c *a64Compiler) bigWord(v *ssa.Value, guard func(arm64.Cond)) bool {
	arg := func(i int) *ssa.Value { return v.Args[i] }
	switch v.Op {
	case ssa.OpBigUnboxU64:
		c.bigUnboxU64(v, guard)
	case ssa.OpBigLow:
		c.bigLow(v, guard)
	case ssa.OpBigSign:
		x := c.gpr(arg(0), a64A)
		c.a.CmpImm(x, 0, true)
		guard(arm64.MI)
		c.setG(v, x)
	case ssa.OpAddW, ssa.OpSubW, ssa.OpAndW, ssa.OpOrW, ssa.OpXorW, ssa.OpAddU64, ssa.OpSubU64:
		op := map[ssa.Op]arm64.ALU{ssa.OpAddW: arm64.Add, ssa.OpSubW: arm64.Sub, ssa.OpAndW: arm64.And, ssa.OpOrW: arm64.Orr,
			ssa.OpXorW: arm64.Eor, ssa.OpAddU64: arm64.Adds, ssa.OpSubU64: arm64.Subs}[v.Op]
		c.a.Op(op, a64A, c.gpr(arg(0), a64A), c.gpr(arg(1), a64B), true)
		switch v.Op {
		case ssa.OpAddU64:
			// A carry: past 2^64-1.
			guard(arm64.HS)
		case ssa.OpSubU64:
			// A borrow: below 0.
			guard(arm64.LO)
		}
		c.setG(v, a64A)
	case ssa.OpMulW:
		c.a.Mul(a64A, c.gpr(arg(0), a64A), c.gpr(arg(1), a64B))
		c.setG(v, a64A)
	case ssa.OpMulU64:
		x, y := c.gpr(arg(0), a64A), c.gpr(arg(1), a64B)
		c.a.Umulh(a64C, x, y)
		c.a.Mul(a64A, x, y)
		c.a.CmpImm(a64C, 0, true)
		guard(arm64.NE)
		c.setG(v, a64A)
	case ssa.OpShlW, ssa.OpShrW, ssa.OpSarW:
		x := c.gpr(arg(0), a64A)
		if n := uint8(v.Index); n != 0 {
			op := map[ssa.Op]arm64.Shift{ssa.OpShlW: arm64.Lsl, ssa.OpShrW: arm64.Lsr, ssa.OpSarW: arm64.Asr}[v.Op]
			c.a.ShiftImm(op, a64A, x, n, true)
			x = a64A
		}
		c.setG(v, x)
	case ssa.OpZextW, ssa.OpSextW:
		x := c.gpr(arg(0), a64A)
		if n := uint8(v.Index); n < 64 {
			back := arm64.Lsr
			if v.Op == ssa.OpSextW {
				back = arm64.Asr
			}
			c.a.ShiftImm(arm64.Lsl, a64A, x, 64-n, true)
			c.a.ShiftImm(back, a64A, a64A, 64-n, true)
			x = a64A
		}
		c.setG(v, x)
	case ssa.OpShlI64, ssa.OpShlU64:
		x := c.gpr(arg(0), a64C)
		if n := uint8(v.Index); n != 0 {
			back := arm64.Asr
			if v.Op == ssa.OpShlU64 {
				back = arm64.Lsr
			}
			c.a.ShiftImm(arm64.Lsl, a64A, x, n, true)
			c.a.ShiftImm(back, a64B, a64A, n, true)
			c.a.Cmp(a64B, x, true)
			guard(arm64.NE)
			x = a64A
		}
		c.setG(v, x)
	case ssa.OpDivI64, ssa.OpModI64, ssa.OpDivU64, ssa.OpModU64:
		c.bigDivide(v, guard)
	case ssa.OpPowI64:
		c.bigPower(v, guard)
	case ssa.OpBigOfF64:
		x := c.fpr(arg(0), a64F0)
		c.a.Fcvtzs(a64A, x)
		c.a.Scvtf(a64F2, a64A, true)
		c.a.FCmp(a64F2, x)
		guard(arm64.NE)
		guard(arm64.VS)
		// 2^63 and past saturate to the largest int64, whose double is
		// 2^63: no double is that int64.
		c.movImm(a64B, math.MaxInt64)
		c.a.Cmp(a64A, a64B, true)
		guard(arm64.EQ)
		c.setG(v, a64A)
	default:
		return false
	}
	return true
}

// bigDivide is amd64's. SDIV and UDIV fault at nothing: a zero divisor is
// guarded, and the most negative int64 over -1 is too, for a division.
func (c *a64Compiler) bigDivide(v *ssa.Value, guard func(arm64.Cond)) {
	signed := v.Op == ssa.OpDivI64 || v.Op == ssa.OpModI64
	mod := v.Op == ssa.OpModI64 || v.Op == ssa.OpModU64
	x, y := c.gpr(v.Args[0], a64C), c.gpr(v.Args[1], a64B)
	c.a.CmpImm(y, 0, true)
	guard(arm64.EQ)
	minus, done := c.a.NewLabel(), c.a.NewLabel()
	if signed {
		c.a.CmpImm(y, -1, true)
		c.a.BCond(arm64.EQ, minus)
		c.a.Sdiv(a64A, x, y)
	} else {
		c.a.Udiv(a64A, x, y)
	}
	if mod {
		c.a.Msub(a64A, a64A, y, x)
	}
	if signed {
		c.a.B(done)
		c.a.Bind(minus)
		if mod {
			c.movImm(a64A, 0)
		} else {
			// -x, which only the most negative int64 overflows.
			c.a.Op(arm64.Subs, a64A, arm64.ZR, x, true)
			guard(arm64.VS)
		}
		c.a.Bind(done)
	}
	c.setG(v, a64A)
}

// bigPower is amd64's, a product's range checked as OpMulI64's is: the
// power in A, the base in B, the exponent in C, D the scratch.
func (c *a64Compiler) bigPower(v *ssa.Value, guard func(arm64.Cond)) {
	if y := c.gpr(v.Args[1], a64C); y != a64C {
		c.a.MovRR(a64C, y)
	}
	c.a.CmpImm(a64C, 0, true)
	guard(arm64.LT)
	if x := c.gpr(v.Args[0], a64B); x != a64B {
		c.a.MovRR(a64B, x)
	}
	c.movImm(a64A, 1)
	loop, skip, done := c.a.NewLabel(), c.a.NewLabel(), c.a.NewLabel()
	// The product of n and m into n, guarded to be in the range.
	mul := func(n, m arm64.Reg) {
		c.a.Smulh(a64D, n, m)
		c.a.Mul(n, n, m)
		c.a.ShiftImm(arm64.Asr, tmpBig, n, 63, true)
		c.a.Cmp(a64D, tmpBig, true)
		guard(arm64.NE)
	}
	c.a.Bind(loop)
	c.movImm(a64D, 1)
	c.a.Tst(a64C, a64D, true)
	c.a.BCond(arm64.EQ, skip)
	mul(a64A, a64B)
	c.a.Bind(skip)
	c.a.ShiftImm(arm64.Lsr, a64C, a64C, 1, true)
	c.a.Cbz(a64C, done, true)
	mul(a64B, a64B)
	c.a.B(loop)
	c.a.Bind(done)
	c.setG(v, a64A)
}

// a64CmpCond is arm64's condition of an OpCmpI64 of operator op, of
// Uint64s unsigned.
func a64CmpCond(op ir.Operator, unsigned bool) arm64.Cond {
	switch op {
	case ir.Le:
		if unsigned {
			return arm64.LS
		}
		return arm64.LE
	case ir.Gt:
		if unsigned {
			return arm64.HI
		}
		return arm64.GT
	case ir.Ge:
		if unsigned {
			return arm64.HS
		}
		return arm64.GE
	}
	if unsigned {
		return arm64.LO
	}
	return arm64.LT
}

// bigRebox is amd64's: its number word in A, which it returns.
func (c *a64Compiler) bigRebox(v *ssa.Value) arm64.Reg {
	box := v.Args[1]
	at := abi.OffKeep + int32(box.Index)*int32(c.enc.ValueSize)
	slow, done := c.a.NewLabel(), c.a.NewLabel()
	c.bigBoxWords(box)
	c.a.FMovToF(a64F0, a64A)
	c.movImm(a64B, c.enc.WriteBarrier)
	c.a.LoadU8(a64B, a64B, 0)
	c.a.Cbnz(a64B, slow, false)
	c.a.Store(a64Ctx, at+c.enc.RefOffset, a64C)
	c.a.Store(a64Ctx, at+c.enc.NumOffset, a64A)
	c.a.B(done)
	c.a.Bind(slow)
	c.a.AddImm(a64A, a64Ctx, int64(at), true)
	c.valueStore()
	c.a.Load(a64A, a64Ctx, at+c.enc.NumOffset)
	c.a.Bind(done)
	return a64A
}
