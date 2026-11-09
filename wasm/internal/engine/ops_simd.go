package engine

import (
	"encoding/binary"
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// simd compiles a SIMD instruction, reporting whether op is one.
func (c *compiler) simd(op opcode.Opcode, at uint32) bool {
	if op.Prefix() != opcode.PrefixSIMD {
		return false
	}
	switch {
	case op == opcode.S128Const:
		lo := c.r.fixed64()
		hi := c.r.fixed64()
		c.push(entry{t: module.V128, k: eConst, c: lo, hi: hi, local: -1})
	case op == opcode.I8x16Shuffle:
		var lanes [16]uint8
		copy(lanes[:], c.r.b[c.r.pc:c.r.pc+16])
		c.r.pc += 16
		c.shuffle(lanes)
	case op >= opcode.I8x16ExtractLaneS && op <= opcode.F64x2ReplaceLane:
		c.lane(op, int(c.r.byte()))
	case op >= opcode.I8x16Splat && op <= opcode.F64x2Splat:
		c.splat(op)
	case op >= opcode.S128LoadMem && op <= opcode.S128Load64Splat,
		op == opcode.S128Load32Zero, op == opcode.S128Load64Zero:
		mem, off := c.r.memarg()
		c.loadVec(op, mem, off)
	case op == opcode.S128StoreMem:
		mem, off := c.r.memarg()
		v := c.v128(c.pop())
		a := c.pop()
		c.flushBelow(0)
		ea := c.addr(a, mem, off)
		m := memAt(mem)
		c.emit(func(f *frame) int32 {
			e := ea(f)
			x := v(f)
			b := m(f).span(e, 16)
			binary.LittleEndian.PutUint64(b, x.Lo)
			binary.LittleEndian.PutUint64(b[8:], x.Hi)
			return 0
		})
	case op >= opcode.S128Load8Lane && op <= opcode.S128Store64Lane:
		mem, off := c.r.memarg()
		c.memLane(op, mem, off, int(c.r.byte()))
	default:
		return c.simdOp(op)
	}
	return true
}

// shuffle compiles i8x16.shuffle: lane i of the result is byte lanes[i] of
// the first operand's bytes followed by the second's.
func (c *compiler) shuffle(lanes [16]uint8) {
	y := c.v128(c.pop())
	x := c.v128(c.pop())
	c.pushTree(module.V128, tv128(func(f *frame) V128 {
		a, b := x(f), y(f)
		var ab [32]uint8
		binary.LittleEndian.PutUint64(ab[0:], a.Lo)
		binary.LittleEndian.PutUint64(ab[8:], a.Hi)
		binary.LittleEndian.PutUint64(ab[16:], b.Lo)
		binary.LittleEndian.PutUint64(ab[24:], b.Hi)
		var r [16]uint8
		for i, k := range lanes {
			r[i] = ab[k&31]
		}
		return fromU8(r)
	}))
}

// laneShape returns the width of the lanes an extract_lane or replace_lane
// opcode names.
func laneShape(op opcode.Opcode) int {
	switch op {
	case opcode.I8x16ExtractLaneS, opcode.I8x16ExtractLaneU, opcode.I8x16ReplaceLane:
		return 8
	case opcode.I16x8ExtractLaneS, opcode.I16x8ExtractLaneU, opcode.I16x8ReplaceLane:
		return 16
	case opcode.I32x4ExtractLane, opcode.F32x4ExtractLane, opcode.I32x4ReplaceLane, opcode.F32x4ReplaceLane:
		return 32
	}
	return 64
}

// lane compiles extract_lane and replace_lane of lane l.
func (c *compiler) lane(op opcode.Opcode, l int) {
	w := laneShape(op)
	hi, sh := laneAt(l, w)
	mask := uint64(math.MaxUint64)
	if w < 64 {
		mask = 1<<w - 1
	}
	switch op {
	case opcode.I8x16ReplaceLane, opcode.I16x8ReplaceLane, opcode.I32x4ReplaceLane:
		s := c.i32(c.pop())
		v := c.v128(c.pop())
		c.pushTree(module.V128, tv128(func(f *frame) V128 { x := v(f); return setLane(x, hi, sh, mask, uint64(s(f))) }))
		return
	case opcode.I64x2ReplaceLane:
		s := c.i64(c.pop())
		v := c.v128(c.pop())
		c.pushTree(module.V128, tv128(func(f *frame) V128 { x := v(f); return setLane(x, hi, sh, mask, s(f)) }))
		return
	case opcode.F32x4ReplaceLane:
		s := c.f32(c.pop())
		v := c.v128(c.pop())
		c.pushTree(module.V128, tv128(func(f *frame) V128 {
			x := v(f)
			return setLane(x, hi, sh, mask, uint64(math.Float32bits(s(f))))
		}))
		return
	case opcode.F64x2ReplaceLane:
		s := c.f64(c.pop())
		v := c.v128(c.pop())
		c.pushTree(module.V128, tv128(func(f *frame) V128 { x := v(f); return setLane(x, hi, sh, mask, math.Float64bits(s(f))) }))
		return
	}
	v := c.v128(c.pop())
	switch op {
	case opcode.I8x16ExtractLaneS:
		c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(int32(int8(getLane(v(f), hi, sh, mask)))) }))
	case opcode.I16x8ExtractLaneS:
		c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(int32(int16(getLane(v(f), hi, sh, mask)))) }))
	case opcode.I8x16ExtractLaneU, opcode.I16x8ExtractLaneU, opcode.I32x4ExtractLane:
		c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(getLane(v(f), hi, sh, mask)) }))
	case opcode.I64x2ExtractLane:
		c.pushTree(module.I64, ti64(func(f *frame) uint64 { return getLane(v(f), hi, sh, mask) }))
	case opcode.F32x4ExtractLane:
		c.pushTree(module.F32, tf32(func(f *frame) float32 {
			return math.Float32frombits(uint32(getLane(v(f), hi, sh, mask)))
		}))
	case opcode.F64x2ExtractLane:
		c.pushTree(module.F64, tf64(func(f *frame) float64 { return math.Float64frombits(getLane(v(f), hi, sh, mask)) }))
	}
}

// splat compiles the splats of a scalar into every lane.
func (c *compiler) splat(op opcode.Opcode) {
	var t tv128
	switch op {
	case opcode.I8x16Splat:
		s := c.i32(c.pop())
		t = func(f *frame) V128 { x := uint64(uint8(s(f))) * 0x0101010101010101; return V128{x, x} }
	case opcode.I16x8Splat:
		s := c.i32(c.pop())
		t = func(f *frame) V128 { x := uint64(uint16(s(f))) * 0x0001000100010001; return V128{x, x} }
	case opcode.I32x4Splat:
		s := c.i32(c.pop())
		t = func(f *frame) V128 { x := uint64(s(f)) * 0x0000000100000001; return V128{x, x} }
	case opcode.I64x2Splat:
		s := c.i64(c.pop())
		t = func(f *frame) V128 { x := s(f); return V128{x, x} }
	case opcode.F32x4Splat:
		s := c.f32(c.pop())
		t = func(f *frame) V128 { x := uint64(math.Float32bits(s(f))) * 0x0000000100000001; return V128{x, x} }
	case opcode.F64x2Splat:
		s := c.f64(c.pop())
		t = func(f *frame) V128 { x := math.Float64bits(s(f)); return V128{x, x} }
	}
	c.pushTree(module.V128, t)
}

// loadVec compiles the loads of a whole vector, of its lanes widened, of a
// splatted lane, and of a lane with zeros above it.
func (c *compiler) loadVec(op opcode.Opcode, mem uint32, off uint64) {
	ea := c.addr(c.pop(), mem, off)
	m := memAt(mem)
	var t tv128
	switch op {
	case opcode.S128LoadMem:
		t = func(f *frame) V128 {
			b := m(f).span(ea(f), 16)
			return V128{binary.LittleEndian.Uint64(b), binary.LittleEndian.Uint64(b[8:])}
		}
	case opcode.S128Load8x8S:
		t = func(f *frame) V128 {
			b := m(f).span(ea(f), 8)
			var r [8]uint16
			for i := range r {
				r[i] = uint16(int8(b[i]))
			}
			return fromU16(r)
		}
	case opcode.S128Load8x8U:
		t = func(f *frame) V128 {
			b := m(f).span(ea(f), 8)
			var r [8]uint16
			for i := range r {
				r[i] = uint16(b[i])
			}
			return fromU16(r)
		}
	case opcode.S128Load16x4S:
		t = func(f *frame) V128 {
			b := m(f).span(ea(f), 8)
			var r [4]uint32
			for i := range r {
				r[i] = uint32(int16(binary.LittleEndian.Uint16(b[2*i:])))
			}
			return fromU32(r)
		}
	case opcode.S128Load16x4U:
		t = func(f *frame) V128 {
			b := m(f).span(ea(f), 8)
			var r [4]uint32
			for i := range r {
				r[i] = uint32(binary.LittleEndian.Uint16(b[2*i:]))
			}
			return fromU32(r)
		}
	case opcode.S128Load32x2S:
		t = func(f *frame) V128 {
			b := m(f).span(ea(f), 8)
			return V128{
				uint64(int32(binary.LittleEndian.Uint32(b))),
				uint64(int32(binary.LittleEndian.Uint32(b[4:]))),
			}
		}
	case opcode.S128Load32x2U:
		t = func(f *frame) V128 {
			b := m(f).span(ea(f), 8)
			return V128{uint64(binary.LittleEndian.Uint32(b)), uint64(binary.LittleEndian.Uint32(b[4:]))}
		}
	case opcode.S128Load8Splat:
		t = func(f *frame) V128 {
			x := uint64(m(f).span(ea(f), 1)[0]) * 0x0101010101010101
			return V128{x, x}
		}
	case opcode.S128Load16Splat:
		t = func(f *frame) V128 {
			x := uint64(binary.LittleEndian.Uint16(m(f).span(ea(f), 2))) * 0x0001000100010001
			return V128{x, x}
		}
	case opcode.S128Load32Splat:
		t = func(f *frame) V128 {
			x := uint64(binary.LittleEndian.Uint32(m(f).span(ea(f), 4))) * 0x0000000100000001
			return V128{x, x}
		}
	case opcode.S128Load64Splat:
		t = func(f *frame) V128 {
			x := binary.LittleEndian.Uint64(m(f).span(ea(f), 8))
			return V128{x, x}
		}
	case opcode.S128Load32Zero:
		t = func(f *frame) V128 { return V128{uint64(binary.LittleEndian.Uint32(m(f).span(ea(f), 4))), 0} }
	case opcode.S128Load64Zero:
		t = func(f *frame) V128 { return V128{binary.LittleEndian.Uint64(m(f).span(ea(f), 8)), 0} }
	}
	c.pushTree(module.V128, t)
}

// memLane compiles the loads into a lane and the stores of a lane.
func (c *compiler) memLane(op opcode.Opcode, mem uint32, off uint64, l int) {
	var n int
	switch op {
	case opcode.S128Load8Lane, opcode.S128Store8Lane:
		n = 1
	case opcode.S128Load16Lane, opcode.S128Store16Lane:
		n = 2
	case opcode.S128Load32Lane, opcode.S128Store32Lane:
		n = 4
	default:
		n = 8
	}
	hi, sh := laneAt(l, 8*n)
	mask := uint64(math.MaxUint64)
	if n < 8 {
		mask = 1<<(8*n) - 1
	}
	v := c.v128(c.pop())
	a := c.pop()
	m := memAt(mem)
	if op <= opcode.S128Load64Lane {
		ea := c.addr(a, mem, off)
		c.pushTree(module.V128, tv128(func(f *frame) V128 {
			e := ea(f)
			x := v(f)
			return setLane(x, hi, sh, mask, readLE(m(f).span(e, uint64(n))))
		}))
		return
	}
	c.flushBelow(0)
	ea := c.addr(a, mem, off)
	c.emit(func(f *frame) int32 {
		e := ea(f)
		x := v(f)
		writeLE(m(f).span(e, uint64(n)), getLane(x, hi, sh, mask))
		return 0
	})
}

// readLE and writeLE read and write a little-endian integer of len(b)
// bytes.
func readLE(b []byte) uint64 {
	var x uint64
	for i := len(b) - 1; i >= 0; i-- {
		x = x<<8 | uint64(b[i])
	}
	return x
}

func writeLE(b []byte, x uint64) {
	for i := range b {
		b[i] = byte(x)
		x >>= 8
	}
}
