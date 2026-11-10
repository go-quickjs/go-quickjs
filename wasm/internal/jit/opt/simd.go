package opt

import (
	"fmt"
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The SIMD instructions: what the tier refuses of them, and how the builder
// makes them values. A computing instruction is an OpSIMD of its opcode; a
// load or a store of a whole v128 is an OpLoad or OpStore of type v128,
// and the others are a scalar load or store and an OpSIMD: the extending
// loads an i64 load widened (the load's opcode), the splatting loads a
// load splatted, load32_zero and load64_zero a load put in the low lane
// (the load's opcode), a lane's load a load replacing the lane and a lane's
// store the lane extracted and stored. The scalar accesses are then
// shared, forwarded and checked as any other.

// simdOp is OpSIMD v's opcode.
func simdOp(v *Value) opcode.Opcode { return opcode.Opcode(v.Aux & 0xffffff) }

// shuffleLanes is i8x16.shuffle v's lanes: the first eight bytes Aux2's, the
// rest five bits each from Aux's bit 24.
func shuffleLanes(v *Value) (lanes [16]byte) {
	for i := range 8 {
		lanes[i] = byte(v.Aux2 >> (8 * i))
		lanes[8+i] = byte(v.Aux>>(24+5*i)) & 31
	}
	return lanes
}

// packShuffle sets shuffle v's immediates to lanes, each below 32.
func packShuffle(v *Value, lanes []byte) {
	v.Aux, v.Aux2 = uint64(opcode.I8x16Shuffle), 0
	for i := range 8 {
		v.Aux2 |= uint64(lanes[i]&31) << (8 * i)
		v.Aux |= uint64(lanes[8+i]&31) << (24 + 5*i)
	}
}

// isSIMDMem reports whether op is a SIMD load or store.
func isSIMDMem(op opcode.Opcode) bool {
	return op >= opcode.S128LoadMem && op <= opcode.S128StoreMem || op == opcode.S128Load32Zero ||
		op == opcode.S128Load64Zero || isSIMDLaneMem(op)
}

// isSIMDLaneMem reports whether op loads or stores a lane.
func isSIMDLaneMem(op opcode.Opcode) bool {
	return op >= opcode.S128Load8Lane && op <= opcode.S128Store64Lane
}

// isLaneOp reports whether op is an extract_lane or a replace_lane.
func isLaneOp(op opcode.Opcode) bool {
	return op >= opcode.I8x16ExtractLaneS && op <= opcode.F64x2ReplaceLane
}

// isReplaceLane reports whether op is a replace_lane.
func isReplaceLane(op opcode.Opcode) bool {
	switch op {
	case opcode.I8x16ReplaceLane, opcode.I16x8ReplaceLane, opcode.I32x4ReplaceLane,
		opcode.I64x2ReplaceLane, opcode.F32x4ReplaceLane, opcode.F64x2ReplaceLane:
		return true
	}
	return false
}

// laneType is the type of the scalar an extract_lane makes or a
// replace_lane takes.
func laneType(op opcode.Opcode) Type {
	switch op {
	case opcode.I64x2ExtractLane, opcode.I64x2ReplaceLane:
		return TI64
	case opcode.F32x4ExtractLane, opcode.F32x4ReplaceLane:
		return TF32
	case opcode.F64x2ExtractLane, opcode.F64x2ReplaceLane:
		return TF64
	}
	return TI32
}

// laneBytes is the width in bytes of an extract_lane's or a replace_lane's
// lanes.
func laneBytes(op opcode.Opcode) uint8 {
	switch op {
	case opcode.I8x16ExtractLaneS, opcode.I8x16ExtractLaneU, opcode.I8x16ReplaceLane:
		return 1
	case opcode.I16x8ExtractLaneS, opcode.I16x8ExtractLaneU, opcode.I16x8ReplaceLane:
		return 2
	case opcode.I32x4ExtractLane, opcode.I32x4ReplaceLane, opcode.F32x4ExtractLane, opcode.F32x4ReplaceLane:
		return 4
	}
	return 8
}

// kindType is the SSA type of a numeric or v128 kind.
func kindType(k module.Kind) Type {
	switch k {
	case module.KindI32:
		return TI32
	case module.KindI64:
		return TI64
	case module.KindF32:
		return TF32
	case module.KindF64:
		return TF64
	}
	return TV128
}

// simdShape is the operand types and the result type of OpSIMD op, which
// is not a load or a store's (the loads' opcodes in it take their scalar).
func simdShape(op opcode.Opcode) (params []Type, result Type) {
	switch {
	case op == opcode.I8x16Shuffle:
		return []Type{TV128, TV128}, TV128
	case isLaneOp(op):
		if isReplaceLane(op) {
			return []Type{TV128, laneType(op)}, TV128
		}
		return []Type{TV128}, laneType(op)
	case op >= opcode.S128Load8x8S && op <= opcode.S128Load32x2U, op == opcode.S128Load64Zero:
		return []Type{TI64}, TV128
	case op == opcode.S128Load32Zero:
		return []Type{TI32}, TV128
	}
	sig := opcode.Signature(op)
	for _, t := range sig.Params {
		params = append(params, kindType(t.Kind))
	}
	return params, kindType(sig.Results[0].Kind)
}

// checkSIMD refuses SIMD instruction op, its immediates read from r, if
// the tier does not compile it: any on a CPU without the SIMD the code
// takes, and a half-precision one.
func checkSIMD(m *module.Module, r *reader, op opcode.Opcode) {
	switch {
	case !hasSIMD():
		refuse(RefuseSIMD, "%s without the CPU's SIMD", op.Name())
	case opcode.IsFP16SIMD(op):
		refuse(RefuseSIMD, "%s, of half precision", op.Name())
	}
	r.skipSIMD(op)
}

// simdValue makes OpSIMD op of type t on args, its lane lane, for the
// instruction at offset at.
func (bd *builder) simdValue(op opcode.Opcode, t Type, lane uint8, at uint32, args ...*Value) *Value {
	v := bd.value(OpSIMD, t, args...)
	v.Aux, v.Aux2, v.At = uint64(op), uint64(lane), at
	return v
}

// simd builds a SIMD instruction, reporting whether op is one.
func (bd *builder) simd(op opcode.Opcode, at uint32) bool {
	if op.Prefix() != opcode.PrefixSIMD {
		return false
	}
	r := &bd.r
	switch {
	case op == opcode.S128Const:
		v := bd.value(OpConst, TV128)
		v.Aux = r.fixed64()
		v.Aux2 = r.fixed64()
		bd.push(v)
	case op == opcode.I8x16Shuffle:
		lanes := r.b[r.pc : r.pc+16]
		r.pc += 16
		y := bd.pop()
		x := bd.pop()
		v := bd.value(OpSIMD, TV128, x, y)
		packShuffle(v, lanes)
		v.At = at
		bd.push(v)
	case isSIMDMem(op):
		bd.simdMem(op, at)
	case isLaneOp(op):
		lane := r.byte()
		if isReplaceLane(op) {
			s := bd.pop()
			x := bd.pop()
			bd.push(bd.simdValue(op, TV128, lane, at, x, s))
			break
		}
		bd.push(bd.simdValue(op, laneType(op), lane, at, bd.pop()))
	default:
		sig := opcode.Signature(op)
		if sig == nil || len(sig.Results) != 1 {
			panic(fmt.Sprintf("wasm opt: no signature for %s", op.Name()))
		}
		var buf [3]*Value
		args := buf[:len(sig.Params)]
		for i := len(args) - 1; i >= 0; i-- {
			args[i] = bd.pop()
		}
		bd.push(bd.simdValue(op, kindType(sig.Results[0].Kind), 0, at, args...))
	}
	return true
}

// simdMem builds a SIMD load or store.
func (bd *builder) simdMem(op opcode.Opcode, at uint32) {
	r := &bd.r
	mem, off := r.memarg()
	mx := bd.memAux(mem)
	switch op {
	case opcode.S128LoadMem:
		bd.push(bd.load(bd.pop(), mx, off, 16, false, TV128, at))
	case opcode.S128StoreMem:
		v := bd.pop()
		bd.store(bd.pop(), mx, off, 16, v, at)
	case opcode.S128Load8x8S, opcode.S128Load8x8U, opcode.S128Load16x4S, opcode.S128Load16x4U,
		opcode.S128Load32x2S, opcode.S128Load32x2U, opcode.S128Load64Zero:
		s := bd.load(bd.pop(), mx, off, 8, false, TI64, at)
		bd.push(bd.simdValue(op, TV128, 0, at, s))
	case opcode.S128Load32Zero:
		s := bd.load(bd.pop(), mx, off, 4, false, TI32, at)
		bd.push(bd.simdValue(op, TV128, 0, at, s))
	case opcode.S128Load8Splat:
		s := bd.load(bd.pop(), mx, off, 1, false, TI32, at)
		bd.push(bd.simdValue(opcode.I8x16Splat, TV128, 0, at, s))
	case opcode.S128Load16Splat:
		s := bd.load(bd.pop(), mx, off, 2, false, TI32, at)
		bd.push(bd.simdValue(opcode.I16x8Splat, TV128, 0, at, s))
	case opcode.S128Load32Splat:
		s := bd.load(bd.pop(), mx, off, 4, false, TI32, at)
		bd.push(bd.simdValue(opcode.I32x4Splat, TV128, 0, at, s))
	case opcode.S128Load64Splat:
		s := bd.load(bd.pop(), mx, off, 8, false, TI64, at)
		bd.push(bd.simdValue(opcode.I64x2Splat, TV128, 0, at, s))
	case opcode.S128Load8Lane, opcode.S128Load16Lane, opcode.S128Load32Lane, opcode.S128Load64Lane:
		lane := r.byte()
		size, t, replace := laneAccess(op)
		v := bd.pop()
		s := bd.load(bd.pop(), mx, off, size, false, t, at)
		bd.push(bd.simdValue(replace, TV128, lane, at, v, s))
	case opcode.S128Store8Lane, opcode.S128Store16Lane, opcode.S128Store32Lane, opcode.S128Store64Lane:
		lane := r.byte()
		size, t, extract := laneAccess(op)
		v := bd.pop()
		addr := bd.pop()
		bd.store(addr, mx, off, size, bd.simdValue(extract, t, lane, at, v), at)
	default:
		panic("wasm opt: no SIMD access " + op.Name())
	}
}

// laneAccess is a lane's load's or store's width, the type of the scalar
// it moves, and the instruction that puts it in the lane or takes it out.
func laneAccess(op opcode.Opcode) (size uint8, t Type, lane opcode.Opcode) {
	switch op {
	case opcode.S128Load8Lane:
		return 1, TI32, opcode.I8x16ReplaceLane
	case opcode.S128Load16Lane:
		return 2, TI32, opcode.I16x8ReplaceLane
	case opcode.S128Load32Lane:
		return 4, TI32, opcode.I32x4ReplaceLane
	case opcode.S128Load64Lane:
		return 8, TI64, opcode.I64x2ReplaceLane
	case opcode.S128Store8Lane:
		return 1, TI32, opcode.I8x16ExtractLaneU
	case opcode.S128Store16Lane:
		return 2, TI32, opcode.I16x8ExtractLaneU
	case opcode.S128Store32Lane:
		return 4, TI32, opcode.I32x4ExtractLane
	}
	return 8, TI64, opcode.I64x2ExtractLane
}

// splat8, splat16 and splat32 are a byte, 16 bits or 32 repeated through
// a word; f32s and f64s a float32's and a float64's bits in every lane.
func splat8(b uint8) uint64   { return uint64(b) * 0x0101010101010101 }
func splat16(h uint16) uint64 { return uint64(h) * 0x0001000100010001 }
func splat32(w uint32) uint64 { return uint64(w) * 0x0000000100000001 }
func f32s(f float32) uint64   { return splat32(math.Float32bits(f)) }
func f64s(f float64) uint64   { return math.Float64bits(f) }
