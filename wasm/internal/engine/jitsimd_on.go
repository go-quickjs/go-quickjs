//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// A SIMD instruction native code leaves to Go (wjit.ExitSIMD) runs the
// interpreter's own code for it: the instruction compiled as the
// interpreter compiles it, over operands read from the frame's slots, its
// result stored into the first operand's.

// simdExitFn runs one SIMD instruction on a frame whose operands are in
// its slots.
type simdExitFn func(f *frame)

// simdExit returns the code of the SIMD instruction at offset at of the
// module, its operands in the frame's slots from base, two apart, making
// it the first time.
func (cj *codeJIT) simdExit(c *Code, at uint32, base int) simdExitFn {
	if fn, ok := cj.simd.Load(at); ok {
		return fn.(simdExitFn)
	}
	fn := buildSIMDExit(c, at, base)
	if prev, ok := cj.simd.LoadOrStore(at, fn); ok {
		return prev.(simdExitFn)
	}
	// What running code needs: charged, never refused.
	cj.mu.Lock()
	cj.recordBytes.Add(simdExitBytes)
	cj.budget.charge(simdExitBytes)
	cj.mu.Unlock()
	return fn
}

func buildSIMDExit(code *Code, at uint32, base int) simdExitFn {
	m := code.Module
	c := &compiler{code: code, m: m}
	c.r = reader{b: m.Bytes, pc: int(at)}
	op := c.r.opcode()
	var params []module.ValueType
	switch {
	case op == opcode.I8x16Shuffle:
		params = []module.ValueType{module.V128, module.V128}
	case op >= opcode.I8x16ExtractLaneS && op <= opcode.F64x2ReplaceLane:
		params = []module.ValueType{module.V128}
		switch op {
		case opcode.I8x16ReplaceLane, opcode.I16x8ReplaceLane, opcode.I32x4ReplaceLane:
			params = append(params, module.I32)
		case opcode.I64x2ReplaceLane:
			params = append(params, module.I64)
		case opcode.F32x4ReplaceLane:
			params = append(params, module.F32)
		case opcode.F64x2ReplaceLane:
			params = append(params, module.F64)
		}
	default:
		params = opcode.Signature(op).Params
	}
	// Stack position h is slot base + 2h.
	c.nlocals = base
	for h, t := range params {
		c.push(c.slotEntry(t, h))
	}
	if !c.simd(op, at) || len(c.stack) != 1 {
		panic("wasm jit: no SIMD exit for " + op.Name())
	}
	st := c.store(c.pop(), base)
	return func(f *frame) { st(f) }
}
