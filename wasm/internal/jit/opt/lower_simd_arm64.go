package opt

import (
	"encoding/binary"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The SIMD instructions on arm64: a v128 in a V register or two slots,
// each instruction the baseline's ASIMD sequence for it (wasm/internal/
// jit's simd*_arm64.go) -- the same instructions and operand orders, so
// the same bits, NaNs included -- over the registers the allocator gave:
// the result's, the operands', and the temporaries a sequence works in
// (simdTemps), which take the place of the baseline's own temporaries and
// its scratch registers. Most are one instruction from the operands'
// registers to the result's; a sequence that writes its result before it
// reads an operand begins with a copy of one operand (vprep) or computes
// in a temporary (vapart).

// The scratch registers a parallel move of v128s may hold a source in:
// every one, or all but x0, which an indirect tail call's callee index
// holds.
var (
	vscratch     = []loc{{kind: lXMM, r: int(x1)}, {kind: lXMM, r: int(x0)}, {kind: lGPR, r: int(s1)}}
	vscratchNoX0 = []loc{{kind: lXMM, r: int(x1)}, {kind: lGPR, r: int(s1)}}
	// simdArityThree are the instructions of three v128 operands, whose
	// first temporary vprep and vapart may load an operand into.
	simdArityThree = map[opcode.Opcode]bool{
		opcode.S128Select: true, opcode.I8x16RelaxedLaneSelect: true, opcode.I16x8RelaxedLaneSelect: true,
		opcode.I32x4RelaxedLaneSelect: true, opcode.I64x2RelaxedLaneSelect: true, opcode.F32x4Qfma: true,
		opcode.F32x4Qfms: true, opcode.F64x2Qfma: true, opcode.F64x2Qfms: true, opcode.I32x4DotI8x16I7x16AddS: true,
	}
)

// The float registers' save area is at a multiple of 16 bytes, so that each
// register's store is one instruction.
var _ = [1]struct{}{}[unsafe.Offsetof(wjit.Ctx{}.SaveV)%16]

// parallelKeeping is parallel where x0 holds a value meanwhile.
func (g *gen) parallelKeeping(moves []pmove) {
	if g.v128 && hasV128(moves) {
		g.pending = parallelV(moves, g.pending[:0], g.move, vscratchNoX0)
		return
	}
	g.parallel(moves)
}

// isX reports whether v is in float register x.
func (g *gen) isX(v *Value, x arm64.FReg) bool {
	l := g.locOf(v)
	return l.kind == lXMM && arm64.FReg(l.r) == x
}

// movX sets float register x to v.
func (g *gen) movX(x arm64.FReg, v *Value) { g.move(loc{kind: lXMM, r: int(x)}, g.locOf(v), v.Type) }

// moveV copies v128 src to dst: between slots 8 bytes at a time through
// s0, the halves in the order that reads each before it is written where
// the slots meet.
func (g *gen) moveV(dst, src loc) {
	a := g.a
	switch dst.kind {
	case lXMM:
		x := arm64.FReg(dst.r)
		switch src.kind {
		case lXMM:
			a.VMov(x, arm64.FReg(src.r))
		case lSlot:
			a.LoadQ(x, rL, slotOff(src.slot))
		case lConst:
			g.loadVConst(x, src.c, src.hi)
		default:
			panic("wasm opt: a v128 from a general register")
		}
	case lSlot:
		off := slotOff(dst.slot)
		switch src.kind {
		case lXMM:
			a.StoreQ(rL, off, arm64.FReg(src.r))
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
				r := arm64.ZR
				if w != 0 {
					a.MovImm(s0, w)
					r = s0
				}
				a.Store(rL, off+int32(8*i), r)
			}
		default:
			panic("wasm opt: a v128 from a general register")
		}
	default:
		panic("wasm opt: a v128 into a general register")
	}
}

// loadVConst loads the v128 lo, hi into x: zeros made, any other from the
// code's pool. It changes no flags.
func (g *gen) loadVConst(x arm64.FReg, lo, hi uint64) {
	if lo == 0 && hi == 0 {
		g.a.V3(arm64.VEor, arm64.V8, x, x, x)
		return
	}
	g.a.LoadQLabel(x, g.vpool.label(lo, hi, g.a.NewLabel))
}

// emitVConsts emits the pool of v128 constants, after the code.
func (g *gen) emitVConsts() {
	var b [16]byte
	for i, k := range g.vpool.keys {
		g.a.Bind(g.vpool.labels[i])
		binary.LittleEndian.PutUint64(b[:], k[0])
		binary.LittleEndian.PutUint64(b[8:], k[1])
		g.a.Data(b[:])
	}
}

// hasSIMD reports whether the CPU has the SIMD the code takes: ASIMD,
// every A64 processor's.
func hasSIMD() bool { return true }

// simdExits reports whether OpSIMD v is left to Go: none is on arm64.
func simdExits(v *Value) bool { return false }

// simdTemps is the number of temporaries OpSIMD v's sequence works in. An
// instruction of three v128 operands has one first, which vprep and vapart
// may load an operand into.
func simdTemps(v *Value) int {
	switch simdOp(v) {
	case opcode.F32x4Abs, opcode.F64x2Abs, opcode.F32x4Neg, opcode.F64x2Neg, opcode.V128AnyTrue,
		opcode.I8x16AllTrue, opcode.I16x8AllTrue, opcode.I32x4AllTrue, opcode.I64x2AllTrue, opcode.I8x16Shuffle,
		opcode.F32x4Pmin, opcode.F32x4Pmax, opcode.F64x2Pmin, opcode.F64x2Pmax,
		opcode.S128Select, opcode.I8x16RelaxedLaneSelect, opcode.I16x8RelaxedLaneSelect,
		opcode.I32x4RelaxedLaneSelect, opcode.I64x2RelaxedLaneSelect, opcode.F32x4Qfma, opcode.F64x2Qfma:
		return 1
	case opcode.I8x16BitMask, opcode.I16x8BitMask, opcode.I32x4BitMask, opcode.I64x2BitMask,
		opcode.I32x4DotI16x8S, opcode.I16x8DotI8x16I7x16S, opcode.F32x4Qfms, opcode.F64x2Qfms:
		return 2
	case opcode.I32x4DotI8x16I7x16AddS:
		return 3
	case opcode.I8x16Shl, opcode.I8x16ShrS, opcode.I8x16ShrU, opcode.I16x8Shl, opcode.I16x8ShrS, opcode.I16x8ShrU,
		opcode.I32x4Shl, opcode.I32x4ShrS, opcode.I32x4ShrU, opcode.I64x2Shl, opcode.I64x2ShrS, opcode.I64x2ShrU:
		if v.Args[1].Op != OpConst {
			return 1
		}
	}
	return 0
}

// vpool3 is the scratch registers vprep and vapart load operands into: x1
// and x0, but c, and an instruction of three v128 operands' first
// temporary.
func (g *gen) vpool3(v *Value, c arm64.FReg) (pool [3]arm64.FReg, n int) {
	for _, s := range [2]arm64.FReg{x1, x0} {
		if s != c {
			pool[n] = s
			n++
		}
	}
	if simdArityThree[simdOp(v)] {
		pool[n] = arm64.FReg(g.temp(v, 0))
		n++
	}
	return pool, n
}

// vprep readies OpSIMD v, whose result is a v128 a sequence computes in
// place from a copy of first: c, the register it is computed in -- its
// own, x0 for one spilled, or x1 where another operand is in its own --
// holds first, and rs are others' registers, readable throughout: each
// one's own where it is not c, else a scratch register loaded with it.
func (g *gen) vprep(v *Value, first *Value, others ...*Value) (c arm64.FReg, rs [3]arm64.FReg) {
	d := g.defX(v)
	c = d
	for _, o := range others {
		if o != first && g.isX(o, d) {
			c = x1
		}
	}
	pool, _ := g.vpool3(v, c)
	k := 0
	for i, o := range others {
		l := g.locOf(o)
		if l.kind == lXMM && arm64.FReg(l.r) != c {
			rs[i] = arm64.FReg(l.r)
			continue
		}
		g.move(loc{kind: lXMM, r: int(pool[k])}, l, o.Type)
		rs[i] = pool[k]
		k++
	}
	g.movX(c, first)
	return c, rs
}

// vread returns the registers of ops, each its own or a scratch register
// loaded: for a sequence that reads them all before it writes its result,
// or computes it in a temporary of its own.
func (g *gen) vread(v *Value, ops ...*Value) (rs [3]arm64.FReg) {
	pool, _ := g.vpool3(v, 255)
	k := 0
	for i, o := range ops {
		l := g.locOf(o)
		if l.kind == lXMM {
			rs[i] = arm64.FReg(l.r)
			continue
		}
		g.move(loc{kind: lXMM, r: int(pool[k])}, l, o.Type)
		rs[i] = pool[k]
		k++
	}
	return rs
}

// vend makes v's result, computed in c, its own.
func (g *gen) vend(v *Value, c arm64.FReg) {
	d := g.defX(v)
	if c != d {
		g.a.VMov(d, c)
	}
	g.done(v, int(d))
}

// v3 lowers an operation of one instruction on lanes of size s: x op y,
// into the result's register.
func (g *gen) v3(v *Value, op arm64.VOp, s arm64.VSize, x, y *Value) {
	rs := g.vread(v, x, y)
	c := g.defX(v)
	g.a.V3(op, s, c, rs[0], rs[1])
	g.done(v, int(c))
}

// v3Not is v3 with the result complemented.
func (g *gen) v3Not(v *Value, op arm64.VOp, s arm64.VSize, x, y *Value) {
	rs := g.vread(v, x, y)
	c := g.defX(v)
	g.a.V3(op, s, c, rs[0], rs[1])
	g.a.V2(arm64.VNot, arm64.V8, c, c)
	g.done(v, int(c))
}

// v2 lowers f of the result's register and v's first operand's.
func (g *gen) v2(v *Value, f func(c, x arm64.FReg)) {
	rs := g.vread(v, v.Args[0])
	c := g.defX(v)
	f(c, rs[0])
	g.done(v, int(c))
}

// v2op lowers a one-instruction operation on lanes of size s.
func (g *gen) v2op(v *Value, op arm64.VOp, s arm64.VSize) {
	g.v2(v, func(c, x arm64.FReg) { g.a.V2(op, s, c, x) })
}

// laneSize is the VSize of lanes of n bytes.
func laneSize(n uint8) arm64.VSize {
	switch n {
	case 1:
		return arm64.V8
	case 2:
		return arm64.V16
	case 4:
		return arm64.V32
	}
	return arm64.V64
}

// lanesOf is the lane size of an instruction, by its shape.
func lanesOf(op opcode.Opcode) arm64.VSize {
	switch name := op.Name(); {
	case name[:5] == "i8x16":
		return arm64.V8
	case name[:5] == "i16x8":
		return arm64.V16
	case name[:5] == "i32x4", name[:5] == "f32x4":
		return arm64.V32
	}
	return arm64.V64
}

// splatSize is the lane size of a splat.
func splatSize(op opcode.Opcode) arm64.VSize {
	switch op {
	case opcode.I8x16Splat:
		return arm64.V8
	case opcode.I16x8Splat:
		return arm64.V16
	case opcode.I32x4Splat:
		return arm64.V32
	}
	return arm64.V64
}

// simd lowers OpSIMD v.
func (g *gen) simd(v *Value) {
	a := g.a
	op := simdOp(v)
	t := func(k int) arm64.FReg { return arm64.FReg(g.temp(v, k)) }
	x := v.Args[0]
	var y *Value
	if len(v.Args) > 1 {
		y = v.Args[1]
	}
	s := lanesOf(op)
	switch op {
	case opcode.S128Load8x8S, opcode.S128Load8x8U, opcode.S128Load16x4S, opcode.S128Load16x4U,
		opcode.S128Load32x2S, opcode.S128Load32x2U, opcode.S128Load64Zero, opcode.S128Load32Zero:
		// The 8 bytes loaded, widened; or the 4 or 8 with zeros above,
		// as a write of the S or D view leaves them.
		c := g.defX(v)
		r := g.intoG(x, s1)
		if op == opcode.S128Load32Zero {
			a.FMovToS(c, r)
		} else {
			a.FMovToF(c, r)
		}
		switch op {
		case opcode.S128Load8x8S:
			a.VShiftImm(arm64.VSshll, arm64.V8, c, c, 0)
		case opcode.S128Load8x8U:
			a.VShiftImm(arm64.VUshll, arm64.V8, c, c, 0)
		case opcode.S128Load16x4S:
			a.VShiftImm(arm64.VSshll, arm64.V16, c, c, 0)
		case opcode.S128Load16x4U:
			a.VShiftImm(arm64.VUshll, arm64.V16, c, c, 0)
		case opcode.S128Load32x2S:
			a.VShiftImm(arm64.VSshll, arm64.V32, c, c, 0)
		case opcode.S128Load32x2U:
			a.VShiftImm(arm64.VUshll, arm64.V32, c, c, 0)
		}
		g.done(v, int(c))

	case opcode.I8x16Splat, opcode.I16x8Splat, opcode.I32x4Splat, opcode.I64x2Splat:
		c := g.defX(v)
		a.Dup(splatSize(op), c, g.readG(x, s1))
		g.done(v, int(c))
	case opcode.F32x4Splat:
		g.v2(v, func(c, x arm64.FReg) { a.DupElem(arm64.V32, c, x, 0) })
	case opcode.F64x2Splat:
		g.v2(v, func(c, x arm64.FReg) { a.DupElem(arm64.V64, c, x, 0) })

	case opcode.I8x16ExtractLaneS, opcode.I8x16ExtractLaneU, opcode.I16x8ExtractLaneS, opcode.I16x8ExtractLaneU,
		opcode.I32x4ExtractLane, opcode.I64x2ExtractLane:
		xr := g.intoX(x, x1)
		r := g.defG(v)
		ls := laneSize(laneBytes(op))
		switch op {
		case opcode.I8x16ExtractLaneS, opcode.I16x8ExtractLaneS:
			a.Smov(ls, r, xr, uint8(v.Aux2))
		default:
			a.Umov(ls, r, xr, uint8(v.Aux2))
		}
		g.done(v, int(r))
	case opcode.F32x4ExtractLane, opcode.F64x2ExtractLane:
		// The float alone in the register, the rest of it clear.
		ls := laneSize(laneBytes(op))
		g.v2(v, func(c, x arm64.FReg) { a.DupScalar(ls, c, x, uint8(v.Aux2)) })
	case opcode.I8x16ReplaceLane, opcode.I16x8ReplaceLane, opcode.I32x4ReplaceLane, opcode.I64x2ReplaceLane:
		c, _ := g.vprep(v, x)
		a.InsG(laneSize(laneBytes(op)), c, uint8(v.Aux2), g.readG(y, s1))
		g.vend(v, c)
	case opcode.F32x4ReplaceLane, opcode.F64x2ReplaceLane:
		c, rs := g.vprep(v, x, y)
		a.InsElem(laneSize(laneBytes(op)), c, uint8(v.Aux2), rs[0], 0)
		g.vend(v, c)

	case opcode.S128Not:
		g.v2op(v, arm64.VNot, arm64.V8)
	case opcode.S128And:
		g.v3(v, arm64.VAnd, arm64.V8, x, y)
	case opcode.S128Or:
		g.v3(v, arm64.VOrr, arm64.V8, x, y)
	case opcode.S128Xor:
		g.v3(v, arm64.VEor, arm64.V8, x, y)
	case opcode.S128AndNot:
		g.v3(v, arm64.VBic, arm64.V8, x, y)
	case opcode.S128Select, opcode.I8x16RelaxedLaneSelect, opcode.I16x8RelaxedLaneSelect, opcode.I32x4RelaxedLaneSelect,
		opcode.I64x2RelaxedLaneSelect:
		// BSL keeps x's bits where the mask's are set, y's elsewhere: every
		// bit of the mask selects, as bitselect.
		c, rs := g.vprep(v, v.Args[2], x, y)
		a.V3(arm64.VBsl, arm64.V8, c, rs[0], rs[1])
		g.vend(v, c)
	case opcode.V128AnyTrue:
		xr := g.intoX(x, x1)
		r := g.defG(v)
		a.V2(arm64.VUmaxv, arm64.V8, t(0), xr)
		a.Umov(arm64.V8, r, t(0), 0)
		a.CmpImm(r, 0, false)
		a.Cset(r, arm64.NE)
		g.done(v, int(r))
	default:
		if !g.simdInt(v, op, s, x, y, t) && !g.simdFloat(v, op, x, y, t) && !g.simdRelaxed(v, op, x, y, t) {
			panic("wasm opt: no lowering of " + op.Name())
		}
	}
}

// simdInt lowers an integer lane instruction on lanes of size s, reporting
// whether op is one.
func (g *gen) simdInt(v *Value, op opcode.Opcode, s arm64.VSize, x, y *Value, t func(int) arm64.FReg) bool {
	a := g.a
	switch op {
	case opcode.I8x16Add, opcode.I16x8Add, opcode.I32x4Add, opcode.I64x2Add:
		g.v3(v, arm64.VAdd, s, x, y)
	case opcode.I8x16Sub, opcode.I16x8Sub, opcode.I32x4Sub, opcode.I64x2Sub:
		g.v3(v, arm64.VSub, s, x, y)
	case opcode.I8x16AddSatS, opcode.I16x8AddSatS:
		g.v3(v, arm64.VSqadd, s, x, y)
	case opcode.I8x16AddSatU, opcode.I16x8AddSatU:
		g.v3(v, arm64.VUqadd, s, x, y)
	case opcode.I8x16SubSatS, opcode.I16x8SubSatS:
		g.v3(v, arm64.VSqsub, s, x, y)
	case opcode.I8x16SubSatU, opcode.I16x8SubSatU:
		g.v3(v, arm64.VUqsub, s, x, y)
	case opcode.I16x8Mul, opcode.I32x4Mul:
		g.v3(v, arm64.VMul, s, x, y)
	case opcode.I64x2Mul:
		// ASIMD has no product of 64-bit lanes: each lane's in general
		// registers.
		c, rs := g.vprep(v, x, y)
		for lane := uint8(0); lane < 2; lane++ {
			a.Umov(arm64.V64, s1, c, lane)
			a.Umov(arm64.V64, s0, rs[0], lane)
			a.Mul(s1, s1, s0)
			a.InsG(arm64.V64, c, lane, s1)
		}
		g.vend(v, c)
	case opcode.I8x16Neg, opcode.I16x8Neg, opcode.I32x4Neg, opcode.I64x2Neg:
		g.v2op(v, arm64.VNeg, s)
	case opcode.I8x16Abs, opcode.I16x8Abs, opcode.I32x4Abs, opcode.I64x2Abs:
		g.v2op(v, arm64.VAbs, s)
	case opcode.I8x16MinS, opcode.I16x8MinS, opcode.I32x4MinS:
		g.v3(v, arm64.VSmin, s, x, y)
	case opcode.I8x16MinU, opcode.I16x8MinU, opcode.I32x4MinU:
		g.v3(v, arm64.VUmin, s, x, y)
	case opcode.I8x16MaxS, opcode.I16x8MaxS, opcode.I32x4MaxS:
		g.v3(v, arm64.VSmax, s, x, y)
	case opcode.I8x16MaxU, opcode.I16x8MaxU, opcode.I32x4MaxU:
		g.v3(v, arm64.VUmax, s, x, y)
	case opcode.I8x16RoundingAverageU, opcode.I16x8RoundingAverageU:
		g.v3(v, arm64.VUrhadd, s, x, y)
	case opcode.I8x16Popcnt:
		g.v2op(v, arm64.VCnt, arm64.V8)
	case opcode.I8x16AllTrue, opcode.I16x8AllTrue, opcode.I32x4AllTrue, opcode.I64x2AllTrue:
		// No lane equal to 0: no byte of the lanes' comparisons with 0 set.
		xr := g.intoX(x, x1)
		r := g.defG(v)
		a.V2(arm64.VCmeq0, s, t(0), xr)
		a.V2(arm64.VUmaxv, arm64.V8, t(0), t(0))
		a.Umov(arm64.V8, r, t(0), 0)
		a.CmpImm(r, 0, false)
		a.Cset(r, arm64.EQ)
		g.done(v, int(r))
	case opcode.I8x16BitMask, opcode.I16x8BitMask, opcode.I32x4BitMask, opcode.I64x2BitMask:
		// Each negative lane made its bit, and the bits summed.
		xr := g.intoX(x, x1)
		r := g.defG(v)
		m, k := t(0), t(1)
		a.V2(arm64.VCmlt0, s, m, xr)
		switch op {
		case opcode.I8x16BitMask:
			g.loadVConst(k, 0x8040201008040201, 0x8040201008040201)
			a.V3(arm64.VAnd, arm64.V8, m, m, k)
			// The high half's bytes interleaved with the low half's: each
			// 16-bit lane low byte low half, high byte high half.
			a.Ext(k, m, m, 8)
			a.V3(arm64.VZip1, arm64.V8, m, m, k)
			a.V2(arm64.VAddv, arm64.V16, m, m)
			a.Umov(arm64.V16, r, m, 0)
		case opcode.I16x8BitMask:
			g.loadVConst(k, 0x0008000400020001, 0x0080004000200010)
			a.V3(arm64.VAnd, arm64.V8, m, m, k)
			a.V2(arm64.VAddv, arm64.V16, m, m)
			a.Umov(arm64.V16, r, m, 0)
		case opcode.I32x4BitMask:
			g.loadVConst(k, 0x0000000200000001, 0x0000000800000004)
			a.V3(arm64.VAnd, arm64.V8, m, m, k)
			a.V2(arm64.VAddv, arm64.V32, m, m)
			a.Umov(arm64.V32, r, m, 0)
		default:
			g.loadVConst(k, 1, 2)
			a.V3(arm64.VAnd, arm64.V8, m, m, k)
			a.V3(arm64.VAddp, arm64.V64, m, m, m)
			a.Umov(arm64.V64, r, m, 0)
		}
		g.done(v, int(r))
	case opcode.I8x16Shl, opcode.I8x16ShrS, opcode.I8x16ShrU, opcode.I16x8Shl, opcode.I16x8ShrS, opcode.I16x8ShrU,
		opcode.I32x4Shl, opcode.I32x4ShrS, opcode.I32x4ShrU, opcode.I64x2Shl, opcode.I64x2ShrS, opcode.I64x2ShrU:
		g.vshift(v, op, s, t)

	case opcode.I8x16SConvertI16x8, opcode.I8x16UConvertI16x8, opcode.I16x8SConvertI32x4, opcode.I16x8UConvertI32x4:
		// x's lanes narrowed into the lower half, y's into the upper.
		n := arm64.VSqxtn
		if op == opcode.I8x16UConvertI16x8 || op == opcode.I16x8UConvertI32x4 {
			n = arm64.VSqxtun
		}
		c, rs := g.vprep(v, x, y)
		a.V2(n, s, c, c)
		a.V2(n|arm64.VQ, s, c, rs[0])
		g.vend(v, c)
	case opcode.I16x8SConvertI8x16Low, opcode.I32x4SConvertI16x8Low, opcode.I64x2SConvertI32x4Low:
		g.v2(v, func(c, x arm64.FReg) { a.VShiftImm(arm64.VSshll, s-1, c, x, 0) })
	case opcode.I16x8SConvertI8x16High, opcode.I32x4SConvertI16x8High, opcode.I64x2SConvertI32x4High:
		g.v2(v, func(c, x arm64.FReg) { a.VShiftImm(arm64.VSshll|arm64.VQ, s-1, c, x, 0) })
	case opcode.I16x8UConvertI8x16Low, opcode.I32x4UConvertI16x8Low, opcode.I64x2UConvertI32x4Low:
		g.v2(v, func(c, x arm64.FReg) { a.VShiftImm(arm64.VUshll, s-1, c, x, 0) })
	case opcode.I16x8UConvertI8x16High, opcode.I32x4UConvertI16x8High, opcode.I64x2UConvertI32x4High:
		g.v2(v, func(c, x arm64.FReg) { a.VShiftImm(arm64.VUshll|arm64.VQ, s-1, c, x, 0) })
	case opcode.I16x8ExtMulLowI8x16S, opcode.I32x4ExtMulLowI16x8S, opcode.I64x2ExtMulLowI32x4S:
		g.v3(v, arm64.VSmull, s-1, x, y)
	case opcode.I16x8ExtMulHighI8x16S, opcode.I32x4ExtMulHighI16x8S, opcode.I64x2ExtMulHighI32x4S:
		g.v3(v, arm64.VSmull|arm64.VQ, s-1, x, y)
	case opcode.I16x8ExtMulLowI8x16U, opcode.I32x4ExtMulLowI16x8U, opcode.I64x2ExtMulLowI32x4U:
		g.v3(v, arm64.VUmull, s-1, x, y)
	case opcode.I16x8ExtMulHighI8x16U, opcode.I32x4ExtMulHighI16x8U, opcode.I64x2ExtMulHighI32x4U:
		g.v3(v, arm64.VUmull|arm64.VQ, s-1, x, y)
	case opcode.I16x8ExtAddPairwiseI8x16S, opcode.I32x4ExtAddPairwiseI16x8S:
		g.v2op(v, arm64.VSaddlp, s-1)
	case opcode.I16x8ExtAddPairwiseI8x16U, opcode.I32x4ExtAddPairwiseI16x8U:
		g.v2op(v, arm64.VUaddlp, s-1)
	case opcode.I32x4DotI16x8S:
		// The products of each half, then the sums of their pairs.
		g.dot(v, arm64.V32, t(0), t(1))
		g.vend(v, t(0))
	case opcode.I16x8Q15MulRSatS:
		g.v3(v, arm64.VSqrdmulh, arm64.V16, x, y)

	case opcode.I8x16Eq, opcode.I16x8Eq, opcode.I32x4Eq, opcode.I64x2Eq:
		g.v3(v, arm64.VCmeq, s, x, y)
	case opcode.I8x16Ne, opcode.I16x8Ne, opcode.I32x4Ne, opcode.I64x2Ne:
		g.v3Not(v, arm64.VCmeq, s, x, y)
	case opcode.I8x16GtS, opcode.I16x8GtS, opcode.I32x4GtS, opcode.I64x2GtS:
		g.v3(v, arm64.VCmgt, s, x, y)
	case opcode.I8x16LtS, opcode.I16x8LtS, opcode.I32x4LtS, opcode.I64x2LtS:
		g.v3(v, arm64.VCmgt, s, y, x)
	case opcode.I8x16GeS, opcode.I16x8GeS, opcode.I32x4GeS, opcode.I64x2GeS:
		g.v3(v, arm64.VCmge, s, x, y)
	case opcode.I8x16LeS, opcode.I16x8LeS, opcode.I32x4LeS, opcode.I64x2LeS:
		g.v3(v, arm64.VCmge, s, y, x)
	case opcode.I8x16GtU, opcode.I16x8GtU, opcode.I32x4GtU:
		g.v3(v, arm64.VCmhi, s, x, y)
	case opcode.I8x16LtU, opcode.I16x8LtU, opcode.I32x4LtU:
		g.v3(v, arm64.VCmhi, s, y, x)
	case opcode.I8x16GeU, opcode.I16x8GeU, opcode.I32x4GeU:
		g.v3(v, arm64.VCmhs, s, x, y)
	case opcode.I8x16LeU, opcode.I16x8LeU, opcode.I32x4LeU:
		g.v3(v, arm64.VCmhs, s, y, x)

	case opcode.I8x16Swizzle, opcode.I8x16RelaxedSwizzle:
		// TBL selects 0 for an index past the table's 16 bytes.
		rs := g.vread(v, x, y)
		c := g.defX(v)
		a.Tbl(c, rs[0], 1, rs[1])
		g.done(v, int(c))
	case opcode.I8x16Shuffle:
		// TBL of the table of x and y, copied into V30 and V31, which are
		// consecutive.
		var idx [2]uint64
		for i, l := range shuffleLanes(v) {
			idx[i/8] |= uint64(l&31) << (8 * (i % 8))
		}
		g.movX(x1, x)
		g.movX(x0, y)
		g.loadVConst(t(0), idx[0], idx[1])
		c := g.defX(v)
		a.Tbl(c, x1, 2, t(0))
		g.done(v, int(c))
	default:
		return false
	}
	return true
}

// dot computes in c the sums of the pairs of products of v's first two
// operands' lanes of size s-1, each product of size s, as SMULL, SMULL2
// and ADDP make them; t is its temporary.
func (g *gen) dot(v *Value, s arm64.VSize, c, t arm64.FReg) {
	a := g.a
	rs := g.vread(v, v.Args[0], v.Args[1])
	a.V3(arm64.VSmull, s-1, c, rs[0], rs[1])
	a.V3(arm64.VSmull|arm64.VQ, s-1, t, rs[0], rs[1])
	a.V3(arm64.VAddp, s, c, c, t)
}

// vshift lowers a shift of every lane of size s by an i32 count, taken
// modulo the lanes' width: by an immediate where the count is a constant,
// else by SSHL or USHL of the count, negated to shift right.
func (g *gen) vshift(v *Value, op opcode.Opcode, s arm64.VSize, t func(int) arm64.FReg) {
	a := g.a
	bits := uint64(8) << s
	left := op == opcode.I8x16Shl || op == opcode.I16x8Shl || op == opcode.I32x4Shl || op == opcode.I64x2Shl
	signed := op == opcode.I8x16ShrS || op == opcode.I16x8ShrS || op == opcode.I32x4ShrS || op == opcode.I64x2ShrS
	n := v.Args[1]
	if n.Op == OpConst {
		k := uint8(n.Aux & (bits - 1))
		g.v2(v, func(c, x arm64.FReg) {
			switch {
			case k == 0:
				if c != x {
					a.VMov(c, x)
				}
			case left:
				a.VShiftImm(arm64.VShl, s, c, x, k)
			case signed:
				a.VShiftImm(arm64.VSshr, s, c, x, k)
			default:
				a.VShiftImm(arm64.VUshr, s, c, x, k)
			}
		})
		return
	}
	r := g.readG(n, s0)
	a.LogicImm(arm64.And, s0, r, bits-1, false)
	if !left {
		a.Neg(s0, s0, false)
	}
	a.Dup(s, t(0), s0)
	op3 := arm64.VUshl
	if signed {
		op3 = arm64.VSshl
	}
	g.v2(v, func(c, x arm64.FReg) { a.V3(op3, s, c, x, t(0)) })
}

// simdFloat lowers a float lane instruction or a conversion, reporting
// whether op is one. The vector instructions process NaNs and order zeros
// lane by lane as the scalar ones the interpreter's operators compile to,
// with the FPCR Go leaves.
func (g *gen) simdFloat(v *Value, op opcode.Opcode, x, y *Value, t func(int) arm64.FReg) bool {
	a := g.a
	s := arm64.V32
	if op.Name()[:5] == "f64x2" {
		s = arm64.V64
	}
	switch op {
	case opcode.F32x4Add, opcode.F64x2Add:
		g.v3(v, arm64.VFadd, s, x, y)
	case opcode.F32x4Sub, opcode.F64x2Sub:
		g.v3(v, arm64.VFsub, s, x, y)
	case opcode.F32x4Mul, opcode.F64x2Mul:
		g.v3(v, arm64.VFmul, s, x, y)
	case opcode.F32x4Div, opcode.F64x2Div:
		g.v3(v, arm64.VFdiv, s, x, y)
	case opcode.F32x4Min, opcode.F64x2Min:
		g.v3(v, arm64.VFmin, s, x, y)
	case opcode.F32x4Max, opcode.F64x2Max:
		g.v3(v, arm64.VFmax, s, x, y)
	case opcode.F32x4Pmin, opcode.F64x2Pmin, opcode.F32x4Pmax, opcode.F64x2Pmax:
		// pmin is y < x ? y : x and pmax x < y ? y : x: the comparison's
		// mask selects y's lanes.
		rs := g.vread(v, x, y)
		c := t(0)
		if op == opcode.F32x4Pmax || op == opcode.F64x2Pmax {
			a.V3(arm64.VFcmgt, s, c, rs[1], rs[0])
		} else {
			a.V3(arm64.VFcmgt, s, c, rs[0], rs[1])
		}
		a.V3(arm64.VBsl, arm64.V8, c, rs[1], rs[0])
		g.vend(v, c)
	case opcode.F32x4Sqrt, opcode.F64x2Sqrt:
		g.v2op(v, arm64.VFsqrt, s)
	case opcode.F32x4Abs, opcode.F64x2Abs, opcode.F32x4Neg, opcode.F64x2Neg:
		// A mask of the sign bits, which changes no NaN's payload.
		m, mop := uint64(1<<63), arm64.VBic
		if s == arm64.V32 {
			m = splat32(0x80000000)
		}
		if op == opcode.F32x4Neg || op == opcode.F64x2Neg {
			mop = arm64.VEor
		}
		g.loadVConst(t(0), m, m)
		g.v2(v, func(c, x arm64.FReg) { a.V3(mop, arm64.V8, c, x, t(0)) })
	case opcode.F32x4Ceil, opcode.F64x2Ceil:
		g.v2op(v, arm64.VFrintp, s)
	case opcode.F32x4Floor, opcode.F64x2Floor:
		g.v2op(v, arm64.VFrintm, s)
	case opcode.F32x4Trunc, opcode.F64x2Trunc:
		g.v2op(v, arm64.VFrintz, s)
	case opcode.F32x4NearestInt, opcode.F64x2NearestInt:
		g.v2op(v, arm64.VFrintn, s)
	case opcode.F32x4Eq, opcode.F64x2Eq:
		g.v3(v, arm64.VFcmeq, s, x, y)
	case opcode.F32x4Ne, opcode.F64x2Ne:
		g.v3Not(v, arm64.VFcmeq, s, x, y)
	case opcode.F32x4Gt, opcode.F64x2Gt:
		g.v3(v, arm64.VFcmgt, s, x, y)
	case opcode.F32x4Ge, opcode.F64x2Ge:
		g.v3(v, arm64.VFcmge, s, x, y)
	case opcode.F32x4Lt, opcode.F64x2Lt:
		g.v3(v, arm64.VFcmgt, s, y, x)
	case opcode.F32x4Le, opcode.F64x2Le:
		g.v3(v, arm64.VFcmge, s, y, x)

	case opcode.F32x4DemoteF64x2Zero:
		g.v2op(v, arm64.VFcvtn, arm64.V64)
	case opcode.F64x2PromoteLowF32x4:
		g.v2op(v, arm64.VFcvtl, arm64.V64)
	case opcode.F32x4SConvertI32x4:
		g.v2op(v, arm64.VScvtf, arm64.V32)
	case opcode.F32x4UConvertI32x4:
		g.v2op(v, arm64.VUcvtf, arm64.V32)
	case opcode.F64x2ConvertLowI32x4S:
		g.v2(v, func(c, x arm64.FReg) {
			a.VShiftImm(arm64.VSshll, arm64.V32, c, x, 0)
			a.V2(arm64.VScvtf, arm64.V64, c, c)
		})
	case opcode.F64x2ConvertLowI32x4U:
		g.v2(v, func(c, x arm64.FReg) {
			a.VShiftImm(arm64.VUshll, arm64.V32, c, x, 0)
			a.V2(arm64.VUcvtf, arm64.V64, c, c)
		})
	case opcode.I32x4SConvertF32x4, opcode.I32x4RelaxedTruncF32x4S:
		// FCVTZS and FCVTZU saturate, and give 0 for NaN.
		g.v2op(v, arm64.VFcvtzs, arm64.V32)
	case opcode.I32x4UConvertF32x4, opcode.I32x4RelaxedTruncF32x4U:
		g.v2op(v, arm64.VFcvtzu, arm64.V32)
	case opcode.I32x4TruncSatF64x2SZero, opcode.I32x4RelaxedTruncF64x2SZero:
		g.v2(v, func(c, x arm64.FReg) {
			a.V2(arm64.VFcvtzs, arm64.V64, c, x)
			a.V2(arm64.VSqxtn, arm64.V32, c, c)
		})
	case opcode.I32x4TruncSatF64x2UZero, opcode.I32x4RelaxedTruncF64x2UZero:
		g.v2(v, func(c, x arm64.FReg) {
			a.V2(arm64.VFcvtzu, arm64.V64, c, x)
			a.V2(arm64.VUqxtn, arm64.V32, c, c)
		})
	default:
		return false
	}
	return true
}

// simdRelaxed lowers a relaxed SIMD instruction with the interpreter's
// results here (engine/relaxed_other.go), reporting whether op is one: the
// deterministic instruction's but relaxed_madd and relaxed_nmadd, which
// fuse.
func (g *gen) simdRelaxed(v *Value, op opcode.Opcode, x, y *Value, t func(int) arm64.FReg) bool {
	a := g.a
	switch op {
	case opcode.F32x4Qfma, opcode.F32x4Qfms, opcode.F64x2Qfma, opcode.F64x2Qfms:
		// FMLA into the addend, the factors in their order: one
		// rounding, and the NaN of the interpreter's fmaPick32 and
		// fmaPick64. nmadd's first factor is negated first, a NaN's sign
		// flipped as FNEG flips it.
		s := arm64.V64
		if op == opcode.F32x4Qfma || op == opcode.F32x4Qfms {
			s = arm64.V32
		}
		c, rs := g.vprep(v, v.Args[2], x, y)
		xa := rs[0]
		if op == opcode.F32x4Qfms || op == opcode.F64x2Qfms {
			a.V2(arm64.VFneg, s, t(1), xa)
			xa = t(1)
		}
		a.V3(arm64.VFmla, s, c, xa, rs[1])
		g.vend(v, c)
	case opcode.F32x4RelaxedMin:
		g.v3(v, arm64.VFmin, arm64.V32, x, y)
	case opcode.F32x4RelaxedMax:
		g.v3(v, arm64.VFmax, arm64.V32, x, y)
	case opcode.F64x2RelaxedMin:
		g.v3(v, arm64.VFmin, arm64.V64, x, y)
	case opcode.F64x2RelaxedMax:
		g.v3(v, arm64.VFmax, arm64.V64, x, y)
	case opcode.I16x8RelaxedQ15MulRS:
		g.v3(v, arm64.VSqrdmulh, arm64.V16, x, y)
	case opcode.I16x8DotI8x16I7x16S:
		// The products of signed bytes, each pair's sum wrapped to 16
		// bits.
		g.dot(v, arm64.V16, t(0), t(1))
		g.vend(v, t(0))
	case opcode.I32x4DotI8x16I7x16AddS:
		// Those summed in pairs to 32 bits, plus the addend.
		c := t(1)
		rs := g.vread(v, x, y, v.Args[2])
		a.V3(arm64.VSmull, arm64.V8, c, rs[0], rs[1])
		a.V3(arm64.VSmull|arm64.VQ, arm64.V8, t(2), rs[0], rs[1])
		a.V3(arm64.VAddp, arm64.V16, c, c, t(2))
		a.V2(arm64.VSaddlp, arm64.V16, c, c)
		a.V3(arm64.VAdd, arm64.V32, c, c, rs[2])
		g.vend(v, c)
	default:
		return false
	}
	return true
}
