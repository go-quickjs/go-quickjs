package engine

import (
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Register code's memory instructions. A load or a store of an i32 memory 0
// at an offset below 2^16 is an operation of the first table, the memory
// read from the instance's field; any other is rExt's generic access, the
// memory and the opcode in the word after the instruction and the offset in
// the next (regEA).

// The memory operations of the second table.
const (
	xLoadAny       regOp = 64 + iota // A = load of memory at address B
	xStoreAny                        // store of B into memory at address A
	xMemorySizeAny                   // A = memory BC's size in pages
	xMemoryGrowAny                   // A = memory grown by B pages, the memory in the word after
	xMemoryInit                      // memory.init: destination A, source B, count C
	xMemoryCopy                      // memory.copy: destination A, source B, count C
	xMemoryFill                      // memory.fill: destination A, value B, count C
	xDataDrop                        // data.drop of segment BC
)

// memoryOp compiles a memory instruction, reporting whether op is one.
func (c *regCompiler) memoryOp(op opcode.Opcode) bool {
	switch op {
	case opcode.MemorySize:
		mem := c.r.u32()
		h := len(c.stack)
		if c.fastMemory(mem) {
			c.def(module.I32, h, c.emit(rExt, xMemorySize, ns(c.numSlot(h)), 0, 0))
			return true
		}
		c.def(c.memIndex(mem), h, c.emitBC(rExt, xMemorySizeAny, ns(c.numSlot(h)), mem))
		return true
	case opcode.MemoryGrow:
		mem := c.r.u32()
		d := c.pop()
		h := len(c.stack)
		ds := c.read(d, h)
		if c.fastMemory(mem) {
			c.def(module.I32, h, c.emit(rExt, xMemoryGrow, ns(c.numSlot(h)), ds, 0))
			return true
		}
		c.def(c.memIndex(mem), h, c.emit(rExt, xMemoryGrowAny, ns(c.numSlot(h)), ds, 0, uint64(mem)))
		return true
	case opcode.MemoryInit:
		seg := c.r.u32()
		mem := c.r.u32()
		c.bulk(xMemoryInit, uint64(seg)|uint64(mem)<<32)
		return true
	case opcode.DataDrop:
		c.emitBC(rExt, xDataDrop, 0, c.r.u32())
		return true
	case opcode.MemoryCopy:
		dm := c.r.u32()
		sm := c.r.u32()
		c.bulk(xMemoryCopy, uint64(dm)|uint64(sm)<<32)
		return true
	case opcode.MemoryFill:
		c.bulk(xMemoryFill, uint64(c.r.u32()))
		return true
	}
	rop, store, ok := regMemOp(op)
	if !ok {
		return false
	}
	mem, off := c.r.memarg()
	if store {
		v := c.pop()
		a := c.pop()
		h := len(c.stack)
		as := c.read(a, h)
		vs := c.read(v, h+1)
		if c.fastMemory(mem) && off <= math.MaxUint16 {
			c.emit(rop, 0, as, vs, off)
		} else {
			c.emit(rExt, xStoreAny, as, vs, 0, uint64(mem)|uint64(op)<<32, off)
		}
		return true
	}
	a := c.pop()
	h := len(c.stack)
	as := c.read(a, h)
	t := loadType(op)
	if c.fastMemory(mem) && off <= math.MaxUint16 {
		c.def(t, h, c.emit(rop, 0, ns(c.numSlot(h)), as, off))
	} else {
		c.def(t, h, c.emit(rExt, xLoadAny, ns(c.numSlot(h)), as, 0, uint64(mem)|uint64(op)<<32, off))
	}
	return true
}

// fastMemory reports whether memory mem is memory 0 of 32-bit addresses,
// which the first table's accesses address.
func (c *regCompiler) fastMemory(mem uint32) bool {
	return mem == 0 && !c.m.Memories[0].Limits.Is64
}

// memIndex returns the type of memory mem's addresses and sizes.
func (c *regCompiler) memIndex(mem uint32) module.ValueType {
	if c.m.Memories[mem].Limits.Is64 {
		return module.I64
	}
	return module.I32
}

// bulk compiles an instruction of three operands, A, B and C, and a word of
// immediates after it: a bulk operation of memories, tables or arrays.
func (c *regCompiler) bulk(x uint8, imm uint64) {
	n := c.pop()
	s := c.pop()
	d := c.pop()
	h := len(c.stack)
	ds := c.read(d, h)
	ss := c.read(s, h+1)
	nsl := c.read(n, h+2)
	c.emit(rExt, x, ds, ss, nsl, imm)
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

// regEA returns the memory and the effective address of the generic access
// at pc, whose address is in field e: the memory is the low half of the
// word after the instruction, the offset the next word. An i64 memory's
// address that overflows traps.
func regEA(l unsafe.Pointer, e uint64, pc int32, f *frame) (*Memory, uint64) {
	m := f.inst.Memories[uint32(f.rc.code[pc+1])]
	a, off := *rs(l, e), f.rc.code[pc+2]
	if m.Is64 {
		ea := a + off
		if ea < a {
			trapAt(TrapMemOutOfBounds, regAt(pc))
		}
		return m, ea
	}
	return m, uint64(uint32(a)) + off
}

func rhLoadAny(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	m, ea := regEA(l, w>>32, pc, f)
	*rs(l, w>>16) = regLoadAny(opcode.Opcode(f.rc.code[pc+1]>>32), m, ea, regAt(pc))
	return pc + 3
}

func rhStoreAny(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	m, ea := regEA(l, w>>16, pc, f)
	regStoreAny(opcode.Opcode(f.rc.code[pc+1]>>32), m, ea, regAt(pc), *rs(l, w>>32))
	return pc + 3
}

func rhMemorySizeAny(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = f.inst.Memories[uint32(w>>32)].Pages()
	return pc + 1
}

func rhMemoryGrowAny(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	m := f.inst.Memories[uint32(f.rc.code[pc+1])]
	d := *rs(l, w>>32)
	if !m.Is64 {
		d = uint64(uint32(d))
	}
	r := uint64(m.Grow(d))
	if !m.Is64 {
		r = uint64(uint32(r))
	}
	*rs(l, w>>16) = r
	return pc + 2
}

func rhMemoryInit(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	imm := f.rc.code[pc+1]
	dst, src, cnt := *rs(l, w>>16), uint64(uint32(*rs(l, w>>32))), uint64(uint32(*rs(l, w>>48)))
	data := f.inst.datas[uint32(imm)]
	b := f.inst.Memories[uint32(imm>>32)].bytes()
	checkRange(src, cnt, uint64(len(data)), TrapMemOutOfBounds, regAt(pc))
	checkRange(dst, cnt, uint64(len(b)), TrapMemOutOfBounds, regAt(pc))
	copy(b[dst:dst+cnt], data[src:src+cnt])
	return pc + 2
}

func rhMemoryCopy(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	imm := f.rc.code[pc+1]
	dst, src, cnt := *rs(l, w>>16), *rs(l, w>>32), *rs(l, w>>48)
	db := f.inst.Memories[uint32(imm)].bytes()
	sb := f.inst.Memories[uint32(imm>>32)].bytes()
	checkRange(src, cnt, uint64(len(sb)), TrapMemOutOfBounds, regAt(pc))
	checkRange(dst, cnt, uint64(len(db)), TrapMemOutOfBounds, regAt(pc))
	copy(db[dst:dst+cnt], sb[src:src+cnt])
	return pc + 2
}

func rhMemoryFill(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	dst, val, cnt := *rs(l, w>>16), byte(*rs(l, w>>32)), *rs(l, w>>48)
	b := f.inst.Memories[uint32(f.rc.code[pc+1])].bytes()
	checkRange(dst, cnt, uint64(len(b)), TrapMemOutOfBounds, regAt(pc))
	s := b[dst : dst+cnt]
	for i := range s {
		s[i] = val
	}
	return pc + 2
}

func rhDataDrop(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	f.inst.datas[uint32(w>>32)] = nil
	return pc + 1
}

func init() {
	for x, h := range map[regOp]regHandler{
		xLoadAny: rhLoadAny, xStoreAny: rhStoreAny, xMemorySizeAny: rhMemorySizeAny, xMemoryGrowAny: rhMemoryGrowAny,
		xMemoryInit: rhMemoryInit, xMemoryCopy: rhMemoryCopy, xMemoryFill: rhMemoryFill, xDataDrop: rhDataDrop,
	} {
		regExtTable[x] = h
	}
}

// The families still compiled by the closure tree.

func (c *regCompiler) atomicOp(op opcode.Opcode) bool { return false }
func (c *regCompiler) exnOp(op opcode.Opcode) bool    { return false }
func (c *regCompiler) endTry(t *rctl)                 { panic("wasm: a try in register code") }
