//go:build amd64 || arm64

package jit

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The SIMD instructions' front end: v128.const, the instructions' operand
// and result types, and the exit through the interpreter for what a
// backend leaves to it (simdNative, in simd_GOARCH.go; goInstr).

const v128 = module.KindV128

// simd compiles a SIMD instruction, reporting whether op is one: natively
// where the backend has it, else as an exit that runs the interpreter's
// code for it.
func (c *compiler) simd(op opcode.Opcode, at uint32) bool {
	if op.Prefix() != opcode.PrefixSIMD {
		return false
	}
	if op == opcode.S128Const {
		lo := c.r.fixed64()
		hi := c.r.fixed64()
		c.push(value{t: v128, k: kConst, c: lo, hi: hi})
		return true
	}
	if c.simdNative(op, at) {
		return true
	}
	if !c.goInstr(at) {
		panic("wasm jit: no exit for " + op.Name())
	}
	return true
}

// laneBytes is the width in bytes of the lanes an extract_lane or
// replace_lane names.
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

// memLaneBytes is the width of the lane a load_lane or store_lane reads or
// writes.
func memLaneBytes(op opcode.Opcode) uint8 {
	switch op {
	case opcode.S128Load8Lane, opcode.S128Store8Lane:
		return 1
	case opcode.S128Load16Lane, opcode.S128Store16Lane:
		return 2
	case opcode.S128Load32Lane, opcode.S128Store32Lane:
		return 4
	}
	return 8
}

// hasV128 reports whether any of ts is a v128, which takes two slots: a
// call's values are then copied by their layout.
func hasV128(ts []module.ValueType) bool {
	for _, t := range ts {
		if t.Kind == v128 {
			return true
		}
	}
	return false
}

// vconst returns the label of the constant v128 lo, hi in the code's pool
// of them, which the backend places (newVConst, emitVConsts).
func (c *compiler) vconst(lo, hi uint64) asmLabel {
	k := [2]uint64{lo, hi}
	if l, ok := c.vconsts[k]; ok {
		return l
	}
	l := c.newVConst(lo, hi)
	if c.vconsts == nil {
		c.vconsts = map[[2]uint64]asmLabel{}
	}
	c.vconsts[k] = l
	c.vconstOrder = append(c.vconstOrder, k)
	return l
}

// splat8, splat16 and splat32 are a byte, 16 bits or 32 repeated through
// a word.
func splat8(b uint8) uint64   { return uint64(b) * 0x0101010101010101 }
func splat16(h uint16) uint64 { return uint64(h) * 0x0001000100010001 }
func splat32(w uint32) uint64 { return uint64(w) * 0x0000000100000001 }
