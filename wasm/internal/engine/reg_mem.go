package engine

import (
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Register code's memory instructions.

// memoryOp compiles a memory instruction, reporting whether op is one.
func (c *regCompiler) memoryOp(op opcode.Opcode) bool {
	switch op {
	case opcode.MemorySize:
		c.memory0(c.r.u32())
		h := len(c.stack)
		c.def(module.I32, h, c.emit(rExt, xMemorySize, ns(c.numSlot(h)), 0, 0))
		return true
	case opcode.MemoryGrow:
		c.memory0(c.r.u32())
		d := c.pop()
		h := len(c.stack)
		ds := c.read(d, h)
		c.def(module.I32, h, c.emit(rExt, xMemoryGrow, ns(c.numSlot(h)), ds, 0))
		return true
	}
	rop, store, ok := regMemOp(op)
	if !ok {
		return false
	}
	c.access(op, rop, store)
	return true
}

// memory0 bails out of an instruction of a memory other than memory 0, or of
// a 64-bit memory.
func (c *regCompiler) memory0(mem uint32) {
	if mem != 0 || c.m.Memories[0].Limits.Is64 {
		panic(regBail("a memory other than an i32 memory 0"))
	}
}

// access compiles a load or a store of memory 0.
func (c *regCompiler) access(op opcode.Opcode, rop regOp, store bool) {
	mem, off := c.r.memarg()
	c.memory0(mem)
	if off > math.MaxUint16 {
		panic(regBail("a memory offset past 65,535"))
	}
	if store {
		v := c.pop()
		a := c.pop()
		h := len(c.stack)
		as := c.read(a, h)
		vs := c.read(v, h+1)
		c.emit(rop, 0, as, vs, off)
		return
	}
	a := c.pop()
	h := len(c.stack)
	as := c.read(a, h)
	t := loadType(op)
	c.def(t, h, c.emit(rop, 0, ns(c.numSlot(h)), as, off))
}

// loadType returns the type a load of op produces.
func loadType(op opcode.Opcode) module.ValueType {
	switch op {
	case opcode.I64LoadMem, opcode.I64LoadMem8S, opcode.I64LoadMem8U, opcode.I64LoadMem16S,
		opcode.I64LoadMem16U, opcode.I64LoadMem32S, opcode.I64LoadMem32U:
		return module.I64
	case opcode.F32LoadMem:
		return module.F32
	case opcode.F64LoadMem:
		return module.F64
	}
	return module.I32
}

// The families still compiled by the closure tree.

func (c *regCompiler) tableOp(op opcode.Opcode) bool  { return false }
func (c *regCompiler) simdOp(op opcode.Opcode) bool   { return false }
func (c *regCompiler) gcOp(op opcode.Opcode) bool     { return false }
func (c *regCompiler) atomicOp(op opcode.Opcode) bool { return false }
func (c *regCompiler) exnOp(op opcode.Opcode) bool    { return false }
func (c *regCompiler) endTry(t *rctl)                 { panic("wasm: a try in register code") }
