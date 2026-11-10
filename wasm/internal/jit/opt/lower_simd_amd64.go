package opt

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The SIMD instructions on amd64: a v128 in an SSE register or two slots,
// each instruction the baseline's sequence for it (wasm/internal/jit's
// simd*_amd64.go) -- the same instructions and operand orders, so the same
// bits, NaNs included -- over the registers the allocator gave: the
// result's, the operands', and the temporaries each sequence works in
// (simdTemps), which take the place of the baseline's own temporaries and
// its scratch register. relaxed_madd and relaxed_nmadd fused (FMA3 with
// AVX) are left to Go, as the baseline leaves them (simdExits).

// The scratch registers a parallel move of v128s may hold a source in:
// every one, or all but x0, which an indirect tail call's callee index
// holds.
var (
	vscratch       = []loc{{kind: lXMM, r: int(x1)}, {kind: lXMM, r: int(x0)}, {kind: lGPR, r: int(s1)}}
	vscratchNoX0   = []loc{{kind: lXMM, r: int(x1)}, {kind: lGPR, r: int(s1)}}
	simdArityThree = map[opcode.Opcode]bool{
		opcode.S128Select: true, opcode.I8x16RelaxedLaneSelect: true, opcode.I16x8RelaxedLaneSelect: true,
		opcode.I32x4RelaxedLaneSelect: true, opcode.I64x2RelaxedLaneSelect: true, opcode.F32x4Qfma: true,
		opcode.F32x4Qfms: true, opcode.F64x2Qfma: true, opcode.F64x2Qfms: true, opcode.I32x4DotI8x16I7x16AddS: true,
	}
)

// parallelKeeping is parallel where x0 holds a value meanwhile.
func (g *gen) parallelKeeping(moves []pmove) {
	if g.v128 && hasV128(moves) {
		g.pending = parallelV(moves, g.pending[:0], g.move, vscratchNoX0)
		return
	}
	g.parallel(moves)
}

// moveV copies v128 src to dst: between slots 8 bytes at a time through
// s0, the halves in the order that reads each before it is written where
// the slots meet.
func (g *gen) moveV(dst, src loc) {
	a := g.a
	switch dst.kind {
	case lXMM:
		x := amd64.XReg(dst.r)
		switch src.kind {
		case lXMM:
			a.SSEOp(amd64.MovAPD, x, amd64.XReg(src.r))
		case lSlot:
			a.LoadVec(x, rL, slotOff(src.slot))
		case lConst:
			g.loadVConst(x, src.c, src.hi)
		default:
			panic("wasm opt: a v128 from a general register")
		}
	case lSlot:
		off := slotOff(dst.slot)
		switch src.kind {
		case lXMM:
			a.StoreVec(rL, off, amd64.XReg(src.r))
		case lSlot:
			so := slotOff(src.slot)
			halves := [2]int32{0, 8}
			if dst.slot > src.slot {
				halves = [2]int32{8, 0}
			}
			for _, h := range halves {
				a.Load(s0, rL, so+h)
				a.Store(rL, off+h, s0)
			}
		case lConst:
			for i, w := range [2]uint64{src.c, src.hi} {
				if int64(w) == int64(int32(w)) {
					a.StoreImm(rL, off+int32(8*i), int32(w))
					continue
				}
				a.MovImm(s0, w)
				a.Store(rL, off+int32(8*i), s0)
			}
		default:
			panic("wasm opt: a v128 from a general register")
		}
	default:
		panic("wasm opt: a v128 into a general register")
	}
}

// loadVConst loads the v128 lo, hi into x: zeros and ones made, any other
// from the code's pool. It changes no flags.
func (g *gen) loadVConst(x amd64.XReg, lo, hi uint64) {
	switch {
	case lo == 0 && hi == 0:
		g.a.Vec(amd64.Pxor, x, x)
	case lo == ^uint64(0) && hi == ^uint64(0):
		g.a.Vec(amd64.Pcmpeqd, x, x)
	default:
		g.a.LoadVecLabel(x, g.vpool.label(lo, hi, g.a.NewLabel))
	}
}

// vopConst is x op= the constant w in both halves, loaded into t.
func (g *gen) vopConst(op amd64.VOp, x, t amd64.XReg, w uint64) {
	g.loadVConst(t, w, w)
	g.a.Vec(op, x, t)
}

// vnot complements x, with t.
func (g *gen) vnot(x, t amd64.XReg) {
	g.a.Vec(amd64.Pcmpeqd, t, t)
	g.a.Vec(amd64.Pxor, x, t)
}

// emitVConsts emits the pool of v128 constants, after the code.
func (g *gen) emitVConsts() {
	for i, k := range g.vpool.keys {
		g.a.Bind(g.vpool.labels[i])
		g.a.Quad(k[0])
		g.a.Quad(k[1])
	}
}

// hasSIMD reports whether the CPU has the SIMD the code takes: SSSE3,
// SSE4.1 and SSE4.2, as the baseline requires (its "cpu" refusal).
func hasSIMD() bool { return wjit.CPU.SIMD }

// simdExits reports whether OpSIMD v is left to Go: relaxed_madd and
// relaxed_nmadd where they fuse, which the code would need VEX encodings
// for.
func simdExits(v *Value) bool {
	switch simdOp(v) {
	case opcode.F32x4Qfma, opcode.F32x4Qfms, opcode.F64x2Qfma, opcode.F64x2Qfms:
		return wjit.CPU.RelaxedFused
	}
	return false
}

// simdTemps is the number of temporaries OpSIMD v's sequence works in. An
// instruction of three v128 operands has one first, which vprep may load
// an operand into.
func simdTemps(v *Value) int {
	op := simdOp(v)
	extra := 0
	if simdArityThree[op] {
		extra = 1
	}
	switch op {
	case opcode.S128Not, opcode.I8x16Splat,
		opcode.I8x16Ne, opcode.I16x8Ne, opcode.I32x4Ne, opcode.I64x2Ne,
		opcode.I8x16LeS, opcode.I16x8LeS, opcode.I32x4LeS, opcode.I64x2LeS,
		opcode.I8x16GeS, opcode.I16x8GeS, opcode.I32x4GeS, opcode.I64x2GeS,
		opcode.I8x16GeU, opcode.I8x16LtU, opcode.I8x16LeU, opcode.I8x16GtU,
		opcode.I16x8GeU, opcode.I16x8LtU, opcode.I16x8LeU, opcode.I16x8GtU,
		opcode.I32x4GeU, opcode.I32x4LtU, opcode.I32x4LeU, opcode.I32x4GtU,
		opcode.I64x2Abs, opcode.I8x16Neg, opcode.I16x8Neg, opcode.I32x4Neg, opcode.I64x2Neg,
		opcode.I8x16AllTrue, opcode.I16x8AllTrue, opcode.I32x4AllTrue, opcode.I64x2AllTrue,
		opcode.I16x8BitMask,
		opcode.I16x8ExtMulLowI8x16S, opcode.I16x8ExtMulHighI8x16S, opcode.I16x8ExtMulLowI8x16U, opcode.I16x8ExtMulHighI8x16U,
		opcode.I32x4ExtMulLowI16x8S, opcode.I32x4ExtMulHighI16x8S, opcode.I32x4ExtMulLowI16x8U, opcode.I32x4ExtMulHighI16x8U,
		opcode.I64x2ExtMulLowI32x4S, opcode.I64x2ExtMulHighI32x4S, opcode.I64x2ExtMulLowI32x4U, opcode.I64x2ExtMulHighI32x4U,
		opcode.I16x8ExtAddPairwiseI8x16S, opcode.I16x8ExtAddPairwiseI8x16U, opcode.I32x4ExtAddPairwiseI16x8S,
		opcode.I32x4ExtAddPairwiseI16x8U, opcode.I16x8Q15MulRSatS,
		opcode.F32x4Abs, opcode.F32x4Neg, opcode.F64x2Abs, opcode.F64x2Neg,
		opcode.F64x2ConvertLowI32x4U, opcode.I32x4TruncSatF64x2SZero:
		return 1
	case opcode.I64x2Mul, opcode.I8x16Swizzle, opcode.F32x4UConvertI32x4, opcode.I32x4SConvertF32x4,
		opcode.I32x4TruncSatF64x2UZero, opcode.I32x4RelaxedTruncF64x2UZero:
		return 2
	case opcode.I8x16Popcnt, opcode.F32x4Min, opcode.F32x4Max, opcode.F64x2Min, opcode.F64x2Max,
		opcode.I32x4UConvertF32x4, opcode.I32x4RelaxedTruncF32x4U:
		return 3
	case opcode.I8x16Shuffle:
		fromX, fromY := shuffleFrom(shuffleLanes(v))
		if fromX && fromY {
			return 2
		}
		return 1
	case opcode.I8x16Shl, opcode.I8x16ShrS, opcode.I8x16ShrU, opcode.I16x8Shl, opcode.I16x8ShrS, opcode.I16x8ShrU,
		opcode.I32x4Shl, opcode.I32x4ShrS, opcode.I32x4ShrU, opcode.I64x2Shl, opcode.I64x2ShrS, opcode.I64x2ShrU:
		_, direct := vshifts[op]
		if v.Args[1].Op == OpConst {
			if direct {
				return 0
			}
			return 1
		}
		switch {
		case direct:
			return 1
		case op == opcode.I64x2ShrS:
			return 2
		}
		return 3
	case opcode.S128Select, opcode.F32x4Qfma, opcode.F64x2Qfma:
		return extra
	case opcode.I8x16RelaxedLaneSelect, opcode.I16x8RelaxedLaneSelect, opcode.I32x4RelaxedLaneSelect,
		opcode.I64x2RelaxedLaneSelect, opcode.F32x4Qfms, opcode.F64x2Qfms:
		return extra + 1
	case opcode.I32x4DotI8x16I7x16AddS:
		if wjit.CPU.RelaxedDotSigned || wjit.CPU.RelaxedDotVNNI {
			return extra + 4
		}
		return extra + 1
	}
	return 0
}

// vprep readies OpSIMD v, whose result is a v128, for its sequence: c,
// the register the result is computed in -- its own, x0 for one spilled,
// or x1 where another operand is in its own -- holds first, and rs are
// others' registers, readable throughout: each one's own where it is not
// c, else a scratch register loaded with it. An instruction of three v128
// operands has its first temporary for that.
func (g *gen) vprep(v *Value, first *Value, others ...*Value) (c amd64.XReg, rs [2]amd64.XReg) {
	d := g.defX(v)
	c = d
	for _, o := range others {
		if o != first && g.isX(o, d) {
			c = x1
		}
	}
	var pool [3]amd64.XReg
	n := 0
	for _, s := range [2]amd64.XReg{x1, x0} {
		if s != c {
			pool[n] = s
			n++
		}
	}
	if simdArityThree[simdOp(v)] {
		pool[n] = amd64.XReg(g.temp(v, 0))
		n++
	}
	k := 0
	for i, o := range others {
		l := g.locOf(o)
		if l.kind == lXMM && amd64.XReg(l.r) != c {
			rs[i] = amd64.XReg(l.r)
			continue
		}
		s := pool[k]
		k++
		g.move(loc{kind: lXMM, r: int(s)}, l, o.Type)
		rs[i] = s
	}
	g.movX(c, first)
	return c, rs
}

// vend makes v's result, computed in c, its own.
func (g *gen) vend(v *Value, c amd64.XReg) {
	d := g.defX(v)
	if c != d {
		g.a.SSEOp(amd64.MovAPD, d, c)
	}
	g.done(v, int(d))
}

// vbin lowers a binary operation of one instruction: x op= y, of v's
// operands x and y.
func (g *gen) vbin(v *Value, op amd64.VOp, x, y *Value) {
	c, rs := g.vprep(v, x, y)
	g.a.Vec(op, c, rs[0])
	g.vend(v, c)
}

// vun lowers an operation on v's first operand in place.
func (g *gen) vun(v *Value, f func(c amd64.XReg)) {
	c, _ := g.vprep(v, v.Args[0])
	f(c)
	g.vend(v, c)
}

// vshifts are the shifts SSE has of lanes by an immediate and by a count
// in a register.
var vshifts = map[opcode.Opcode]struct {
	imm amd64.VShift
	reg amd64.VOp
}{
	opcode.I16x8Shl: {amd64.PsllwImm, amd64.Psllw}, opcode.I16x8ShrS: {amd64.PsrawImm, amd64.Psraw},
	opcode.I16x8ShrU: {amd64.PsrlwImm, amd64.Psrlw}, opcode.I32x4Shl: {amd64.PslldImm, amd64.Pslld},
	opcode.I32x4ShrS: {amd64.PsradImm, amd64.Psrad}, opcode.I32x4ShrU: {amd64.PsrldImm, amd64.Psrld},
	opcode.I64x2Shl: {amd64.PsllqImm, amd64.Psllq}, opcode.I64x2ShrU: {amd64.PsrlqImm, amd64.Psrlq},
}

// shuffleFrom reports whether a shuffle's lanes select from its first
// operand and from its second.
func shuffleFrom(lanes [16]byte) (fromX, fromY bool) {
	for _, l := range lanes {
		if l < 16 {
			fromX = true
		} else {
			fromY = true
		}
	}
	return fromX, fromY
}

// simd lowers OpSIMD v.
func (g *gen) simd(v *Value, rest []*Value) {
	if simdExits(v) {
		g.simdGo(v)
		return
	}
	a := g.a
	op := simdOp(v)
	t := func(k int) amd64.XReg { return amd64.XReg(g.temp(v, k)) }
	var x, y *Value
	x = v.Args[0]
	if len(v.Args) > 1 {
		y = v.Args[1]
	}
	switch op {
	case opcode.S128Load8x8S, opcode.S128Load8x8U, opcode.S128Load16x4S, opcode.S128Load16x4U,
		opcode.S128Load32x2S, opcode.S128Load32x2U, opcode.S128Load64Zero, opcode.S128Load32Zero:
		// The 8 bytes loaded, widened; or the 4 or 8 with zeros above.
		c := g.defX(v)
		r := g.intoG(x, s1)
		if op == opcode.S128Load32Zero {
			a.MovDToX(c, r)
		} else {
			a.MovQToX(c, r)
		}
		switch op {
		case opcode.S128Load8x8S:
			a.Vec(amd64.Pmovsxbw, c, c)
		case opcode.S128Load8x8U:
			a.Vec(amd64.Pmovzxbw, c, c)
		case opcode.S128Load16x4S:
			a.Vec(amd64.Pmovsxwd, c, c)
		case opcode.S128Load16x4U:
			a.Vec(amd64.Pmovzxwd, c, c)
		case opcode.S128Load32x2S:
			a.Vec(amd64.Pmovsxdq, c, c)
		case opcode.S128Load32x2U:
			a.Vec(amd64.Pmovzxdq, c, c)
		}
		g.done(v, int(c))

	case opcode.I8x16Splat, opcode.I16x8Splat, opcode.I32x4Splat, opcode.I64x2Splat:
		c := g.defX(v)
		r := g.intoG(x, s1)
		switch op {
		case opcode.I8x16Splat:
			a.MovDToX(c, r)
			a.Vec(amd64.Pxor, t(0), t(0))
			a.Vec(amd64.Pshufb, c, t(0))
		case opcode.I16x8Splat:
			a.MovDToX(c, r)
			a.VecImm(amd64.Pshuflw, c, c, 0)
			a.Vec(amd64.Punpcklqdq, c, c)
		case opcode.I32x4Splat:
			a.MovDToX(c, r)
			a.VecImm(amd64.Pshufd, c, c, 0)
		default:
			a.MovQToX(c, r)
			a.Vec(amd64.Punpcklqdq, c, c)
		}
		g.done(v, int(c))
	case opcode.F32x4Splat:
		g.vun(v, func(c amd64.XReg) { a.VecImm(amd64.Pshufd, c, c, 0) })
	case opcode.F64x2Splat:
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Punpcklqdq, c, c) })

	case opcode.I8x16ExtractLaneS, opcode.I8x16ExtractLaneU, opcode.I16x8ExtractLaneS, opcode.I16x8ExtractLaneU,
		opcode.I32x4ExtractLane, opcode.I64x2ExtractLane:
		xr := g.intoX(x, x1)
		r := g.defG(v)
		a.Pextr(laneBytes(op), r, xr, uint8(v.Aux2))
		switch op {
		case opcode.I8x16ExtractLaneS:
			a.Movsx8(r, r, false)
		case opcode.I16x8ExtractLaneS:
			a.Movsx16(r, r, false)
		}
		g.done(v, int(r))
	case opcode.F32x4ExtractLane, opcode.F64x2ExtractLane:
		c := g.defX(v)
		g.movX(c, x)
		if lane := uint8(v.Aux2); lane != 0 {
			if op == opcode.F32x4ExtractLane {
				a.VecImm(amd64.Pshufd, c, c, lane)
			} else {
				a.Vec(amd64.Movhlps, c, c)
			}
		}
		g.done(v, int(c))
	case opcode.I8x16ReplaceLane, opcode.I16x8ReplaceLane, opcode.I32x4ReplaceLane, opcode.I64x2ReplaceLane:
		c, _ := g.vprep(v, x)
		a.Pinsr(laneBytes(op), c, g.intoG(y, s1), uint8(v.Aux2))
		g.vend(v, c)
	case opcode.F32x4ReplaceLane:
		c, rs := g.vprep(v, x, y)
		a.VecImm(amd64.Insertps, c, rs[0], uint8(v.Aux2)<<4)
		g.vend(v, c)
	case opcode.F64x2ReplaceLane:
		c, rs := g.vprep(v, x, y)
		if v.Aux2 == 0 {
			a.Vec(amd64.Movsd, c, rs[0])
		} else {
			a.Vec(amd64.Movlhps, c, rs[0])
		}
		g.vend(v, c)

	case opcode.S128Not:
		g.vun(v, func(c amd64.XReg) { g.vnot(c, t(0)) })
	case opcode.S128And:
		g.vbin(v, amd64.Pand, x, y)
	case opcode.S128Or:
		g.vbin(v, amd64.Por, x, y)
	case opcode.S128Xor:
		g.vbin(v, amd64.Pxor, x, y)
	case opcode.S128AndNot:
		// PANDN of y and x: x &^ y.
		g.vbin(v, amd64.Pandn, y, x)
	case opcode.S128Select:
		// x&m | y&^m is ((x^y)&m)^y.
		c, rs := g.vprep(v, x, y, v.Args[2])
		a.Vec(amd64.Pxor, c, rs[0])
		a.Vec(amd64.Pand, c, rs[1])
		a.Vec(amd64.Pxor, c, rs[0])
		g.vend(v, c)
	case opcode.V128AnyTrue:
		xr := g.intoX(x, x1)
		r := g.defG(v)
		a.Op(amd64.Xor, r, r, false)
		a.Vec(amd64.Ptest, xr, xr)
		a.Setcc(amd64.CondNE, r)
		g.done(v, int(r))
	default:
		if !g.simdInt(v, op, x, y, t) && !g.simdFloat(v, op, x, y, t) && !g.simdRelaxed(v, op, x, y, t) {
			panic("wasm opt: no lowering of " + op.Name())
		}
	}
}

// simdGo runs OpSIMD v in Go (wjit.ExitSIMD): its operands staged in the
// call area two slots apart, where the interpreter's code for the
// instruction at its offset leaves its result in the first's.
func (g *gen) simdGo(v *Value) {
	g.stage(v.Args, g.argBase)
	g.exit(wjit.ExitSIMD, g.a.NewLabel(), uint64(v.At), uint64(g.argBase))
	dst := g.locOf(v)
	g.move(dst, loc{kind: lSlot, slot: int32(g.argBase)}, v.Type)
	if dst.kind != lSlot {
		g.done(v, dst.r)
	}
	g.reload(v)
}

// simdInt lowers an integer lane instruction, reporting whether op is one.
func (g *gen) simdInt(v *Value, op opcode.Opcode, x, y *Value, t func(int) amd64.XReg) bool {
	a := g.a
	switch op {
	case opcode.I8x16Add:
		g.vbin(v, amd64.Paddb, x, y)
	case opcode.I16x8Add:
		g.vbin(v, amd64.Paddw, x, y)
	case opcode.I32x4Add:
		g.vbin(v, amd64.Paddd, x, y)
	case opcode.I64x2Add:
		g.vbin(v, amd64.Paddq, x, y)
	case opcode.I8x16Sub:
		g.vbin(v, amd64.Psubb, x, y)
	case opcode.I16x8Sub:
		g.vbin(v, amd64.Psubw, x, y)
	case opcode.I32x4Sub:
		g.vbin(v, amd64.Psubd, x, y)
	case opcode.I64x2Sub:
		g.vbin(v, amd64.Psubq, x, y)
	case opcode.I8x16AddSatS:
		g.vbin(v, amd64.Paddsb, x, y)
	case opcode.I8x16AddSatU:
		g.vbin(v, amd64.Paddusb, x, y)
	case opcode.I8x16SubSatS:
		g.vbin(v, amd64.Psubsb, x, y)
	case opcode.I8x16SubSatU:
		g.vbin(v, amd64.Psubusb, x, y)
	case opcode.I16x8AddSatS:
		g.vbin(v, amd64.Paddsw, x, y)
	case opcode.I16x8AddSatU:
		g.vbin(v, amd64.Paddusw, x, y)
	case opcode.I16x8SubSatS:
		g.vbin(v, amd64.Psubsw, x, y)
	case opcode.I16x8SubSatU:
		g.vbin(v, amd64.Psubusw, x, y)
	case opcode.I16x8Mul:
		g.vbin(v, amd64.Pmullw, x, y)
	case opcode.I32x4Mul:
		g.vbin(v, amd64.Pmulld, x, y)
	case opcode.I64x2Mul:
		// lo(x)*lo(y) + (hi(x)*lo(y) + lo(x)*hi(y)) << 32, each product
		// of 32-bit halves.
		c, rs := g.vprep(v, x, y)
		yb, t1, t2 := rs[0], t(0), t(1)
		a.SSEOp(amd64.MovAPD, t1, c)
		a.VecShift(amd64.PsrlqImm, t1, 32)
		a.Vec(amd64.Pmuludq, t1, yb)
		a.SSEOp(amd64.MovAPD, t2, yb)
		a.VecShift(amd64.PsrlqImm, t2, 32)
		a.Vec(amd64.Pmuludq, t2, c)
		a.Vec(amd64.Paddq, t1, t2)
		a.VecShift(amd64.PsllqImm, t1, 32)
		a.Vec(amd64.Pmuludq, c, yb)
		a.Vec(amd64.Paddq, c, t1)
		g.vend(v, c)
	case opcode.I8x16Neg, opcode.I16x8Neg, opcode.I32x4Neg, opcode.I64x2Neg:
		sub := map[opcode.Opcode]amd64.VOp{opcode.I8x16Neg: amd64.Psubb, opcode.I16x8Neg: amd64.Psubw,
			opcode.I32x4Neg: amd64.Psubd, opcode.I64x2Neg: amd64.Psubq}[op]
		g.vun(v, func(c amd64.XReg) {
			a.Vec(amd64.Pxor, t(0), t(0))
			a.Vec(sub, t(0), c)
			a.SSEOp(amd64.MovAPD, c, t(0))
		})
	case opcode.I8x16Abs:
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Pabsb, c, c) })
	case opcode.I16x8Abs:
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Pabsw, c, c) })
	case opcode.I32x4Abs:
		g.vun(v, func(c amd64.XReg) { a.Vec(amd64.Pabsd, c, c) })
	case opcode.I64x2Abs:
		// (x ^ s) - s, s each lane's sign spread through it.
		g.vun(v, func(c amd64.XReg) {
			a.VecImm(amd64.Pshufd, t(0), c, 0xF5)
			a.VecShift(amd64.PsradImm, t(0), 31)
			a.Vec(amd64.Pxor, c, t(0))
			a.Vec(amd64.Psubq, c, t(0))
		})
	case opcode.I8x16MinS:
		g.vbin(v, amd64.Pminsb, x, y)
	case opcode.I8x16MinU:
		g.vbin(v, amd64.Pminub, x, y)
	case opcode.I8x16MaxS:
		g.vbin(v, amd64.Pmaxsb, x, y)
	case opcode.I8x16MaxU:
		g.vbin(v, amd64.Pmaxub, x, y)
	case opcode.I16x8MinS:
		g.vbin(v, amd64.Pminsw, x, y)
	case opcode.I16x8MinU:
		g.vbin(v, amd64.Pminuw, x, y)
	case opcode.I16x8MaxS:
		g.vbin(v, amd64.Pmaxsw, x, y)
	case opcode.I16x8MaxU:
		g.vbin(v, amd64.Pmaxuw, x, y)
	case opcode.I32x4MinS:
		g.vbin(v, amd64.Pminsd, x, y)
	case opcode.I32x4MinU:
		g.vbin(v, amd64.Pminud, x, y)
	case opcode.I32x4MaxS:
		g.vbin(v, amd64.Pmaxsd, x, y)
	case opcode.I32x4MaxU:
		g.vbin(v, amd64.Pmaxud, x, y)
	case opcode.I8x16RoundingAverageU:
		g.vbin(v, amd64.Pavgb, x, y)
	case opcode.I16x8RoundingAverageU:
		g.vbin(v, amd64.Pavgw, x, y)
	case opcode.I8x16Popcnt:
		// Each nibble's count from a table of sixteen.
		const table, table2 = 0x0302020102010100, 0x0403030203020201
		g.vun(v, func(c amd64.XReg) {
			lo, tt, m := t(0), t(1), t(2)
			g.loadVConst(m, splat8(0x0f), splat8(0x0f))
			a.SSEOp(amd64.MovAPD, lo, c)
			a.Vec(amd64.Pand, lo, m)
			a.VecShift(amd64.PsrlwImm, c, 4)
			a.Vec(amd64.Pand, c, m)
			g.loadVConst(tt, table, table2)
			a.Vec(amd64.Pshufb, tt, lo)
			g.loadVConst(lo, table, table2)
			a.Vec(amd64.Pshufb, lo, c)
			a.Vec(amd64.Paddb, lo, tt)
			a.SSEOp(amd64.MovAPD, c, lo)
		})
	case opcode.I8x16AllTrue, opcode.I16x8AllTrue, opcode.I32x4AllTrue, opcode.I64x2AllTrue:
		// No lane equal to 0.
		eq := map[opcode.Opcode]amd64.VOp{opcode.I8x16AllTrue: amd64.Pcmpeqb, opcode.I16x8AllTrue: amd64.Pcmpeqw,
			opcode.I32x4AllTrue: amd64.Pcmpeqd, opcode.I64x2AllTrue: amd64.Pcmpeqq}[op]
		xr := g.intoX(x, x1)
		r := g.defG(v)
		a.Op(amd64.Xor, r, r, false)
		a.Vec(amd64.Pxor, t(0), t(0))
		a.Vec(eq, t(0), xr)
		a.Vec(amd64.Ptest, t(0), t(0))
		a.Setcc(amd64.CondE, r)
		g.done(v, int(r))
	case opcode.I8x16BitMask, opcode.I16x8BitMask, opcode.I32x4BitMask, opcode.I64x2BitMask:
		xr := g.intoX(x, x1)
		r := g.defG(v)
		switch op {
		case opcode.I8x16BitMask:
			a.VecMask(amd64.Pmovmskb, r, xr)
		case opcode.I16x8BitMask:
			// The lanes narrowed to bytes keep their signs.
			a.SSEOp(amd64.MovAPD, t(0), xr)
			a.Vec(amd64.Packsswb, t(0), t(0))
			a.VecMask(amd64.Pmovmskb, r, t(0))
			a.MovZX8(r, r)
		case opcode.I32x4BitMask:
			a.VecMask(amd64.Movmskps, r, xr)
		default:
			a.VecMask(amd64.Movmskpd, r, xr)
		}
		g.done(v, int(r))
	case opcode.I8x16Shl, opcode.I8x16ShrS, opcode.I8x16ShrU, opcode.I16x8Shl, opcode.I16x8ShrS, opcode.I16x8ShrU,
		opcode.I32x4Shl, opcode.I32x4ShrS, opcode.I32x4ShrU, opcode.I64x2Shl, opcode.I64x2ShrS, opcode.I64x2ShrU:
		g.vshift(v, op, t)

	case opcode.I8x16SConvertI16x8:
		g.vbin(v, amd64.Packsswb, x, y)
	case opcode.I8x16UConvertI16x8:
		g.vbin(v, amd64.Packuswb, x, y)
	case opcode.I16x8SConvertI32x4:
		g.vbin(v, amd64.Packssdw, x, y)
	case opcode.I16x8UConvertI32x4:
		g.vbin(v, amd64.Packusdw, x, y)
	case opcode.I16x8SConvertI8x16Low, opcode.I16x8SConvertI8x16High, opcode.I16x8UConvertI8x16Low, opcode.I16x8UConvertI8x16High,
		opcode.I32x4SConvertI16x8Low, opcode.I32x4SConvertI16x8High, opcode.I32x4UConvertI16x8Low, opcode.I32x4UConvertI16x8High,
		opcode.I64x2SConvertI32x4Low, opcode.I64x2SConvertI32x4High, opcode.I64x2UConvertI32x4Low, opcode.I64x2UConvertI32x4High:
		ext, high := extendOp(op)
		g.vun(v, func(c amd64.XReg) {
			if high {
				a.VecImm(amd64.Pshufd, c, c, 0xEE)
			}
			a.Vec(ext, c, c)
		})
	case opcode.I16x8ExtMulLowI8x16S, opcode.I16x8ExtMulHighI8x16S, opcode.I16x8ExtMulLowI8x16U, opcode.I16x8ExtMulHighI8x16U:
		// Each half widened, then the 16-bit products.
		ext, high := extendOp(map[opcode.Opcode]opcode.Opcode{
			opcode.I16x8ExtMulLowI8x16S: opcode.I16x8SConvertI8x16Low, opcode.I16x8ExtMulHighI8x16S: opcode.I16x8SConvertI8x16High,
			opcode.I16x8ExtMulLowI8x16U: opcode.I16x8UConvertI8x16Low, opcode.I16x8ExtMulHighI8x16U: opcode.I16x8UConvertI8x16High}[op])
		c, rs := g.vprep(v, x, y)
		a.SSEOp(amd64.MovAPD, t(0), rs[0])
		for _, r := range []amd64.XReg{c, t(0)} {
			if high {
				a.VecImm(amd64.Pshufd, r, r, 0xEE)
			}
			a.Vec(ext, r, r)
		}
		a.Vec(amd64.Pmullw, c, t(0))
		g.vend(v, c)
	case opcode.I32x4ExtMulLowI16x8S, opcode.I32x4ExtMulHighI16x8S, opcode.I32x4ExtMulLowI16x8U, opcode.I32x4ExtMulHighI16x8U:
		// The low and high halves of the 16-bit products, interleaved.
		mulhi := amd64.Pmulhw
		if op == opcode.I32x4ExtMulLowI16x8U || op == opcode.I32x4ExtMulHighI16x8U {
			mulhi = amd64.Pmulhuw
		}
		c, rs := g.vprep(v, x, y)
		a.SSEOp(amd64.MovAPD, t(0), c)
		a.Vec(amd64.Pmullw, c, rs[0])
		a.Vec(mulhi, t(0), rs[0])
		if op == opcode.I32x4ExtMulLowI16x8S || op == opcode.I32x4ExtMulLowI16x8U {
			a.Vec(amd64.Punpcklwd, c, t(0))
		} else {
			a.Vec(amd64.Punpckhwd, c, t(0))
		}
		g.vend(v, c)
	case opcode.I64x2ExtMulLowI32x4S, opcode.I64x2ExtMulHighI32x4S, opcode.I64x2ExtMulLowI32x4U, opcode.I64x2ExtMulHighI32x4U:
		// The half's two lanes moved to the even lanes, which PMULDQ and
		// PMULUDQ multiply.
		mul, sel := amd64.Pmuldq, uint8(0x50)
		if op == opcode.I64x2ExtMulLowI32x4U || op == opcode.I64x2ExtMulHighI32x4U {
			mul = amd64.Pmuludq
		}
		if op == opcode.I64x2ExtMulHighI32x4S || op == opcode.I64x2ExtMulHighI32x4U {
			sel = 0xFA
		}
		c, rs := g.vprep(v, x, y)
		a.VecImm(amd64.Pshufd, c, c, sel)
		a.VecImm(amd64.Pshufd, t(0), rs[0], sel)
		a.Vec(mul, c, t(0))
		g.vend(v, c)
	case opcode.I16x8ExtAddPairwiseI8x16S:
		// PMADDUBSW multiplies its destination's bytes unsigned, its
		// source's signed: ones times the lanes.
		g.vun(v, func(c amd64.XReg) {
			g.loadVConst(t(0), splat8(1), splat8(1))
			a.Vec(amd64.Pmaddubsw, t(0), c)
			a.SSEOp(amd64.MovAPD, c, t(0))
		})
	case opcode.I16x8ExtAddPairwiseI8x16U:
		g.vun(v, func(c amd64.XReg) { g.vopConst(amd64.Pmaddubsw, c, t(0), splat8(1)) })
	case opcode.I32x4ExtAddPairwiseI16x8S:
		g.vun(v, func(c amd64.XReg) { g.vopConst(amd64.Pmaddwd, c, t(0), splat16(1)) })
	case opcode.I32x4ExtAddPairwiseI16x8U:
		// The lanes made signed by subtracting 0x8000, summed, and the
		// 0x10000 taken from each sum added back.
		g.vun(v, func(c amd64.XReg) {
			g.vopConst(amd64.Pxor, c, t(0), splat16(0x8000))
			g.vopConst(amd64.Pmaddwd, c, t(0), splat16(1))
			g.vopConst(amd64.Paddd, c, t(0), splat32(0x10000))
		})
	case opcode.I32x4DotI16x8S:
		g.vbin(v, amd64.Pmaddwd, x, y)
	case opcode.I16x8Q15MulRSatS:
		// PMULHRSW does not saturate: its only result past the range,
		// 0x8000 of 0x8000 * 0x8000, is complemented.
		c, rs := g.vprep(v, x, y)
		a.Vec(amd64.Pmulhrsw, c, rs[0])
		g.loadVConst(t(0), splat16(0x8000), splat16(0x8000))
		a.Vec(amd64.Pcmpeqw, t(0), c)
		a.Vec(amd64.Pxor, c, t(0))
		g.vend(v, c)

	case opcode.I8x16Eq:
		g.vbin(v, amd64.Pcmpeqb, x, y)
	case opcode.I16x8Eq:
		g.vbin(v, amd64.Pcmpeqw, x, y)
	case opcode.I32x4Eq:
		g.vbin(v, amd64.Pcmpeqd, x, y)
	case opcode.I64x2Eq:
		g.vbin(v, amd64.Pcmpeqq, x, y)
	case opcode.I8x16Ne:
		g.vbinNot(v, amd64.Pcmpeqb, x, y, t(0))
	case opcode.I16x8Ne:
		g.vbinNot(v, amd64.Pcmpeqw, x, y, t(0))
	case opcode.I32x4Ne:
		g.vbinNot(v, amd64.Pcmpeqd, x, y, t(0))
	case opcode.I64x2Ne:
		g.vbinNot(v, amd64.Pcmpeqq, x, y, t(0))
	case opcode.I8x16GtS:
		g.vbin(v, amd64.Pcmpgtb, x, y)
	case opcode.I16x8GtS:
		g.vbin(v, amd64.Pcmpgtw, x, y)
	case opcode.I32x4GtS:
		g.vbin(v, amd64.Pcmpgtd, x, y)
	case opcode.I64x2GtS:
		g.vbin(v, amd64.Pcmpgtq, x, y)
	case opcode.I8x16LtS:
		g.vbin(v, amd64.Pcmpgtb, y, x)
	case opcode.I16x8LtS:
		g.vbin(v, amd64.Pcmpgtw, y, x)
	case opcode.I32x4LtS:
		g.vbin(v, amd64.Pcmpgtd, y, x)
	case opcode.I64x2LtS:
		g.vbin(v, amd64.Pcmpgtq, y, x)
	case opcode.I8x16LeS:
		g.vbinNot(v, amd64.Pcmpgtb, x, y, t(0))
	case opcode.I16x8LeS:
		g.vbinNot(v, amd64.Pcmpgtw, x, y, t(0))
	case opcode.I32x4LeS:
		g.vbinNot(v, amd64.Pcmpgtd, x, y, t(0))
	case opcode.I64x2LeS:
		g.vbinNot(v, amd64.Pcmpgtq, x, y, t(0))
	case opcode.I8x16GeS:
		g.vbinNot(v, amd64.Pcmpgtb, y, x, t(0))
	case opcode.I16x8GeS:
		g.vbinNot(v, amd64.Pcmpgtw, y, x, t(0))
	case opcode.I32x4GeS:
		g.vbinNot(v, amd64.Pcmpgtd, y, x, t(0))
	case opcode.I64x2GeS:
		g.vbinNot(v, amd64.Pcmpgtq, y, x, t(0))
	case opcode.I8x16GeU:
		g.vcmpU(v, amd64.Pmaxub, amd64.Pcmpeqb, false, t(0))
	case opcode.I8x16LtU:
		g.vcmpU(v, amd64.Pmaxub, amd64.Pcmpeqb, true, t(0))
	case opcode.I8x16LeU:
		g.vcmpU(v, amd64.Pminub, amd64.Pcmpeqb, false, t(0))
	case opcode.I8x16GtU:
		g.vcmpU(v, amd64.Pminub, amd64.Pcmpeqb, true, t(0))
	case opcode.I16x8GeU:
		g.vcmpU(v, amd64.Pmaxuw, amd64.Pcmpeqw, false, t(0))
	case opcode.I16x8LtU:
		g.vcmpU(v, amd64.Pmaxuw, amd64.Pcmpeqw, true, t(0))
	case opcode.I16x8LeU:
		g.vcmpU(v, amd64.Pminuw, amd64.Pcmpeqw, false, t(0))
	case opcode.I16x8GtU:
		g.vcmpU(v, amd64.Pminuw, amd64.Pcmpeqw, true, t(0))
	case opcode.I32x4GeU:
		g.vcmpU(v, amd64.Pmaxud, amd64.Pcmpeqd, false, t(0))
	case opcode.I32x4LtU:
		g.vcmpU(v, amd64.Pmaxud, amd64.Pcmpeqd, true, t(0))
	case opcode.I32x4LeU:
		g.vcmpU(v, amd64.Pminud, amd64.Pcmpeqd, false, t(0))
	case opcode.I32x4GtU:
		g.vcmpU(v, amd64.Pminud, amd64.Pcmpeqd, true, t(0))

	case opcode.I8x16Swizzle:
		// An index past 15 made one with its top bit set, which PSHUFB
		// selects 0 for.
		c, rs := g.vprep(v, x, y)
		a.SSEOp(amd64.MovAPD, t(0), rs[0])
		g.vopConst(amd64.Paddusb, t(0), t(1), splat8(0x70))
		a.Vec(amd64.Pshufb, c, t(0))
		g.vend(v, c)
	case opcode.I8x16Shuffle:
		g.shuffle(v, x, y, t)
	default:
		return false
	}
	return true
}

// vbinNot is vbin with the result complemented, by t: a comparison SSE
// has only the negation of.
func (g *gen) vbinNot(v *Value, op amd64.VOp, x, y *Value, t amd64.XReg) {
	c, rs := g.vprep(v, x, y)
	g.a.Vec(op, c, rs[0])
	g.vnot(c, t)
	g.vend(v, c)
}

// vcmpU lowers an unsigned comparison: x >= y where max(x, y) == x
// (minmax the maximum), x <= y where min(x, y) == x, complemented if not,
// in t.
func (g *gen) vcmpU(v *Value, minmax, eq amd64.VOp, not bool, t amd64.XReg) {
	a := g.a
	c, rs := g.vprep(v, v.Args[0], v.Args[1])
	a.SSEOp(amd64.MovAPD, t, c)
	a.Vec(minmax, t, rs[0])
	a.Vec(eq, t, c)
	a.SSEOp(amd64.MovAPD, c, t)
	if not {
		g.vnot(c, t)
	}
	g.vend(v, c)
}

// extendOp is the PMOVSX or PMOVZX of an extension, and whether it extends
// the high half.
func extendOp(op opcode.Opcode) (amd64.VOp, bool) {
	switch op {
	case opcode.I16x8SConvertI8x16Low:
		return amd64.Pmovsxbw, false
	case opcode.I16x8SConvertI8x16High:
		return amd64.Pmovsxbw, true
	case opcode.I16x8UConvertI8x16Low:
		return amd64.Pmovzxbw, false
	case opcode.I16x8UConvertI8x16High:
		return amd64.Pmovzxbw, true
	case opcode.I32x4SConvertI16x8Low:
		return amd64.Pmovsxwd, false
	case opcode.I32x4SConvertI16x8High:
		return amd64.Pmovsxwd, true
	case opcode.I32x4UConvertI16x8Low:
		return amd64.Pmovzxwd, false
	case opcode.I32x4UConvertI16x8High:
		return amd64.Pmovzxwd, true
	case opcode.I64x2SConvertI32x4Low:
		return amd64.Pmovsxdq, false
	case opcode.I64x2SConvertI32x4High:
		return amd64.Pmovsxdq, true
	case opcode.I64x2UConvertI32x4Low:
		return amd64.Pmovzxdq, false
	}
	return amd64.Pmovzxdq, true
}

// shuffle lowers i8x16.shuffle: byte i of the result is byte lanes[i] of
// x's bytes followed by y's, each half's selected by PSHUFB with 0 where
// the other half's lane is.
func (g *gen) shuffle(v *Value, x, y *Value, t func(int) amd64.XReg) {
	a := g.a
	lanes := shuffleLanes(v)
	var m1, m2 [2]uint64
	for i, l := range lanes {
		b1, b2 := uint64(0x80), uint64(0x80)
		if l < 16 {
			b1 = uint64(l)
		} else {
			b2 = uint64(l - 16)
		}
		m1[i/8] |= b1 << (8 * (i % 8))
		m2[i/8] |= b2 << (8 * (i % 8))
	}
	fromX, fromY := shuffleFrom(lanes)
	switch {
	case !fromY:
		c, _ := g.vprep(v, x)
		g.loadVConst(t(0), m1[0], m1[1])
		a.Vec(amd64.Pshufb, c, t(0))
		g.vend(v, c)
	case !fromX:
		c, _ := g.vprep(v, y)
		g.loadVConst(t(0), m2[0], m2[1])
		a.Vec(amd64.Pshufb, c, t(0))
		g.vend(v, c)
	default:
		c, rs := g.vprep(v, x, y)
		yb := t(1)
		a.SSEOp(amd64.MovAPD, yb, rs[0])
		g.loadVConst(t(0), m1[0], m1[1])
		a.Vec(amd64.Pshufb, c, t(0))
		g.loadVConst(t(0), m2[0], m2[1])
		a.Vec(amd64.Pshufb, yb, t(0))
		a.Vec(amd64.Por, c, yb)
		g.vend(v, c)
	}
}

// vshift lowers a shift of every lane by an i32 count, taken modulo the
// lanes' width. SSE has no shifts of bytes, and no arithmetic shift right
// of 64-bit lanes.
func (g *gen) vshift(v *Value, op opcode.Opcode, t func(int) amd64.XReg) {
	a := g.a
	var bits uint64
	switch op {
	case opcode.I8x16Shl, opcode.I8x16ShrS, opcode.I8x16ShrU:
		bits = 8
	case opcode.I16x8Shl, opcode.I16x8ShrS, opcode.I16x8ShrU:
		bits = 16
	case opcode.I32x4Shl, opcode.I32x4ShrS, opcode.I32x4ShrU:
		bits = 32
	default:
		bits = 64
	}
	s := v.Args[1]
	if s.Op == OpConst {
		n := uint8(s.Aux & (bits - 1))
		g.vun(v, func(c amd64.XReg) {
			switch d, ok := vshifts[op]; {
			case n == 0:
			case ok:
				a.VecShift(d.imm, c, n)
			case op == opcode.I64x2ShrS:
				// (x >>u n ^ m) - m, m the sign bit shifted alike.
				m := uint64(1<<63) >> n
				a.VecShift(amd64.PsrlqImm, c, n)
				g.vopConst(amd64.Pxor, c, t(0), m)
				g.vopConst(amd64.Psubq, c, t(0), m)
			case op == opcode.I8x16Shl:
				a.VecShift(amd64.PsllwImm, c, n)
				g.vopConst(amd64.Pand, c, t(0), splat8(0xff<<n))
			case op == opcode.I8x16ShrU:
				a.VecShift(amd64.PsrlwImm, c, n)
				g.vopConst(amd64.Pand, c, t(0), splat8(0xff>>n))
			default:
				// I8x16ShrS: shifted unsigned, the sign extended as for
				// i64.
				m := splat8(0x80 >> n)
				a.VecShift(amd64.PsrlwImm, c, n)
				g.vopConst(amd64.Pand, c, t(0), splat8(0xff>>n))
				g.vopConst(amd64.Pxor, c, t(0), m)
				g.vopConst(amd64.Psubb, c, t(0), m)
			}
		})
		return
	}
	g.movG(s1, s)
	a.OpImm(amd64.And, s1, int32(bits-1), false)
	if op == opcode.I8x16ShrS {
		// The bytes doubled into 16-bit lanes, shifted right 8 more.
		a.OpImm(amd64.Add, s1, 8, false)
	}
	cnt := t(0)
	a.MovDToX(cnt, s1)
	g.vun(v, func(c amd64.XReg) {
		switch d, ok := vshifts[op]; {
		case ok:
			a.Vec(d.reg, c, cnt)
		case op == opcode.I64x2ShrS:
			g.loadVConst(t(1), 1<<63, 1<<63)
			a.Vec(amd64.Psrlq, t(1), cnt)
			a.Vec(amd64.Psrlq, c, cnt)
			a.Vec(amd64.Pxor, c, t(1))
			a.Vec(amd64.Psubq, c, t(1))
		default:
			// The bytes widened to 16-bit lanes, shifted, and narrowed.
			lo, xt := t(1), t(2)
			a.SSEOp(amd64.MovAPD, lo, c)
			switch op {
			case opcode.I8x16ShrS:
				a.Vec(amd64.Punpcklbw, lo, lo)
				a.Vec(amd64.Punpckhbw, c, c)
				a.Vec(amd64.Psraw, lo, cnt)
				a.Vec(amd64.Psraw, c, cnt)
				a.Vec(amd64.Packsswb, lo, c)
			default:
				a.Vec(amd64.Pxor, xt, xt)
				a.Vec(amd64.Punpcklbw, lo, xt)
				a.Vec(amd64.Punpckhbw, c, xt)
				if op == opcode.I8x16Shl {
					a.Vec(amd64.Psllw, lo, cnt)
					a.Vec(amd64.Psllw, c, cnt)
					g.vopConst(amd64.Pand, lo, xt, splat16(0xff))
					g.vopConst(amd64.Pand, c, xt, splat16(0xff))
				} else {
					a.Vec(amd64.Psrlw, lo, cnt)
					a.Vec(amd64.Psrlw, c, cnt)
				}
				a.Vec(amd64.Packuswb, lo, c)
			}
			a.SSEOp(amd64.MovAPD, c, lo)
		}
	})
}
