//go:build amd64 || arm64

package jit

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The threads proposal's atomic instructions. An access is to an address
// aligned to its width, which is checked after an i64 memory's address and
// offset are summed and before the bounds, as the interpreter checks them;
// loads, stores and read-modify-writes are the processor's own atomic
// instructions, sequentially consistent, at the access's width. wait and
// notify are exits to Go.

// atomicOp compiles an atomic instruction, reporting whether op is one.
func (c *compiler) atomicOp(op opcode.Opcode, at uint32) bool {
	if op.Prefix() != opcode.PrefixAtomic {
		return false
	}
	switch op {
	case opcode.AtomicFence:
		c.r.byte()
		c.fence()
		return true
	case opcode.Pause:
		return true
	case opcode.AtomicNotify, opcode.I32AtomicWait, opcode.I64AtomicWait:
		mem, off := c.r.memarg()
		c.goOp(op, at, uint64(mem), off)
		return true
	}
	width, t := atomicWidth(op)
	if width == 0 {
		return false
	}
	mem, off := c.r.memarg()
	switch kind := rmwKind(op); {
	case op >= opcode.I32AtomicLoad && op <= opcode.I64AtomicLoad32U:
		c.atomicLoad(mem, off, width, t, at)
	case op >= opcode.I32AtomicStore && op <= opcode.I64AtomicStore32U:
		c.atomicStore(mem, off, width, at)
	case kind == '?':
		c.atomicCmpxchg(mem, off, width, t, at)
	default:
		c.atomicRMW(kind, mem, off, width, t, at)
	}
	return true
}

// atomicWidth returns an atomic memory instruction's access width in bytes
// and its result's type, or 0.
func atomicWidth(op opcode.Opcode) (uint64, module.Kind) {
	switch op {
	case opcode.I32AtomicLoad, opcode.I32AtomicStore, opcode.I32AtomicAdd, opcode.I32AtomicSub,
		opcode.I32AtomicAnd, opcode.I32AtomicOr, opcode.I32AtomicXor, opcode.I32AtomicExchange,
		opcode.I32AtomicCompareExchange:
		return 4, module.KindI32
	case opcode.I64AtomicLoad, opcode.I64AtomicStore, opcode.I64AtomicAdd, opcode.I64AtomicSub,
		opcode.I64AtomicAnd, opcode.I64AtomicOr, opcode.I64AtomicXor, opcode.I64AtomicExchange,
		opcode.I64AtomicCompareExchange:
		return 8, module.KindI64
	case opcode.I32AtomicLoad8U, opcode.I32AtomicStore8U, opcode.I32AtomicAdd8U, opcode.I32AtomicSub8U,
		opcode.I32AtomicAnd8U, opcode.I32AtomicOr8U, opcode.I32AtomicXor8U, opcode.I32AtomicExchange8U,
		opcode.I32AtomicCompareExchange8U:
		return 1, module.KindI32
	case opcode.I32AtomicLoad16U, opcode.I32AtomicStore16U, opcode.I32AtomicAdd16U, opcode.I32AtomicSub16U,
		opcode.I32AtomicAnd16U, opcode.I32AtomicOr16U, opcode.I32AtomicXor16U, opcode.I32AtomicExchange16U,
		opcode.I32AtomicCompareExchange16U:
		return 2, module.KindI32
	case opcode.I64AtomicLoad8U, opcode.I64AtomicStore8U, opcode.I64AtomicAdd8U, opcode.I64AtomicSub8U,
		opcode.I64AtomicAnd8U, opcode.I64AtomicOr8U, opcode.I64AtomicXor8U, opcode.I64AtomicExchange8U,
		opcode.I64AtomicCompareExchange8U:
		return 1, module.KindI64
	case opcode.I64AtomicLoad16U, opcode.I64AtomicStore16U, opcode.I64AtomicAdd16U, opcode.I64AtomicSub16U,
		opcode.I64AtomicAnd16U, opcode.I64AtomicOr16U, opcode.I64AtomicXor16U, opcode.I64AtomicExchange16U,
		opcode.I64AtomicCompareExchange16U:
		return 2, module.KindI64
	case opcode.I64AtomicLoad32U, opcode.I64AtomicStore32U, opcode.I64AtomicAdd32U, opcode.I64AtomicSub32U,
		opcode.I64AtomicAnd32U, opcode.I64AtomicOr32U, opcode.I64AtomicXor32U, opcode.I64AtomicExchange32U,
		opcode.I64AtomicCompareExchange32U:
		return 4, module.KindI64
	}
	return 0, 0
}

// rmwKind classifies a read-modify-write instruction: '+', '-', '&', '|',
// '^', '=' (exchange) or '?' (compare and exchange).
func rmwKind(op opcode.Opcode) byte {
	switch {
	case op >= opcode.I32AtomicAdd && op <= opcode.I64AtomicAdd32U:
		return '+'
	case op >= opcode.I32AtomicSub && op <= opcode.I64AtomicSub32U:
		return '-'
	case op >= opcode.I32AtomicAnd && op <= opcode.I64AtomicAnd32U:
		return '&'
	case op >= opcode.I32AtomicOr && op <= opcode.I64AtomicOr32U:
		return '|'
	case op >= opcode.I32AtomicXor && op <= opcode.I64AtomicXor32U:
		return '^'
	case op >= opcode.I32AtomicExchange && op <= opcode.I64AtomicExchange32U:
		return '='
	case op >= opcode.I32AtomicCompareExchange && op <= opcode.I64AtomicCompareExchange32U:
		return '?'
	}
	return 0
}
