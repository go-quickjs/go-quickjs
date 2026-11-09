package engine

import (
	"encoding/binary"
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// bytes returns the memory's bytes at its size.
func (m *Memory) bytes() []byte {
	if m.Shared {
		return m.buf[:m.size.Load()]
	}
	return m.buf
}

// span returns the n bytes at effective address ea, trapping if any is out
// of bounds.
func (m *Memory) span(ea, n uint64) []byte {
	b := m.bytes()
	if ea > uint64(len(b)) || n > uint64(len(b))-ea {
		trap(TrapMemOutOfBounds)
	}
	return b[ea : ea+n]
}

// addr returns a reader of a memory access's effective address: the
// operand plus the offset, which for an i64 memory traps if it overflows.
func (c *compiler) addr(e entry, mem uint32, off uint64) func(*frame) uint64 {
	if c.m.Memories[mem].Limits.Is64 {
		a := c.i64(e)
		if off == 0 {
			return a
		}
		return func(f *frame) uint64 {
			v := a(f)
			ea := v + off
			if ea < v {
				trap(TrapMemOutOfBounds)
			}
			return ea
		}
	}
	if e.k == eSlot {
		s := e.slot
		return func(f *frame) uint64 { return uint64(uint32(f.l[s])) + off }
	}
	a := c.i32(e)
	return func(f *frame) uint64 { return uint64(a(f)) + off }
}

// memAt returns the reader of memory mem of the running instance.
func memAt(mem uint32) func(*frame) *Memory {
	return func(f *frame) *Memory { return f.inst.Memories[mem] }
}

// memory compiles a memory instruction, reporting whether op is one.
func (c *compiler) memory(op opcode.Opcode, at uint32) bool {
	switch op {
	case opcode.MemorySize:
		mem := c.r.u32()
		if c.m.Memories[mem].Limits.Is64 {
			c.pushTree(module.I64, ti64(func(f *frame) uint64 { return f.inst.Memories[mem].Pages() }))
		} else {
			c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(f.inst.Memories[mem].Pages()) }))
		}
		return true
	case opcode.MemoryGrow:
		mem := c.r.u32()
		if c.m.Memories[mem].Limits.Is64 {
			d := c.i64(c.pop())
			c.pushTree(module.I64, ti64(func(f *frame) uint64 { return uint64(f.inst.Memories[mem].Grow(d(f))) }))
		} else {
			d := c.i32(c.pop())
			c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(f.inst.Memories[mem].Grow(uint64(d(f)))) }))
		}
		return true
	}
	if op < opcode.I32LoadMem || op > opcode.I64StoreMem32 {
		return false
	}
	mem, off := c.r.memarg()
	if op >= opcode.I32StoreMem {
		c.storeMem(op, mem, off)
		return true
	}
	ea := c.addr(c.pop(), mem, off)
	m := memAt(mem)
	switch op {
	case opcode.I32LoadMem:
		c.pushTree(module.I32, ti32(func(f *frame) uint32 { return binary.LittleEndian.Uint32(m(f).span(ea(f), 4)) }))
	case opcode.I64LoadMem:
		c.pushTree(module.I64, ti64(func(f *frame) uint64 { return binary.LittleEndian.Uint64(m(f).span(ea(f), 8)) }))
	case opcode.F32LoadMem:
		c.pushTree(module.F32, tf32(func(f *frame) float32 {
			return math.Float32frombits(binary.LittleEndian.Uint32(m(f).span(ea(f), 4)))
		}))
	case opcode.F64LoadMem:
		c.pushTree(module.F64, tf64(func(f *frame) float64 {
			return math.Float64frombits(binary.LittleEndian.Uint64(m(f).span(ea(f), 8)))
		}))
	case opcode.I32LoadMem8S:
		c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(int32(int8(m(f).span(ea(f), 1)[0]))) }))
	case opcode.I32LoadMem8U:
		c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(m(f).span(ea(f), 1)[0]) }))
	case opcode.I32LoadMem16S:
		c.pushTree(module.I32, ti32(func(f *frame) uint32 {
			return uint32(int32(int16(binary.LittleEndian.Uint16(m(f).span(ea(f), 2)))))
		}))
	case opcode.I32LoadMem16U:
		c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(binary.LittleEndian.Uint16(m(f).span(ea(f), 2))) }))
	case opcode.I64LoadMem8S:
		c.pushTree(module.I64, ti64(func(f *frame) uint64 { return uint64(int64(int8(m(f).span(ea(f), 1)[0]))) }))
	case opcode.I64LoadMem8U:
		c.pushTree(module.I64, ti64(func(f *frame) uint64 { return uint64(m(f).span(ea(f), 1)[0]) }))
	case opcode.I64LoadMem16S:
		c.pushTree(module.I64, ti64(func(f *frame) uint64 {
			return uint64(int64(int16(binary.LittleEndian.Uint16(m(f).span(ea(f), 2)))))
		}))
	case opcode.I64LoadMem16U:
		c.pushTree(module.I64, ti64(func(f *frame) uint64 { return uint64(binary.LittleEndian.Uint16(m(f).span(ea(f), 2))) }))
	case opcode.I64LoadMem32S:
		c.pushTree(module.I64, ti64(func(f *frame) uint64 {
			return uint64(int64(int32(binary.LittleEndian.Uint32(m(f).span(ea(f), 4)))))
		}))
	case opcode.I64LoadMem32U:
		c.pushTree(module.I64, ti64(func(f *frame) uint64 { return uint64(binary.LittleEndian.Uint32(m(f).span(ea(f), 4))) }))
	}
	return true
}

// storeMem compiles a store.
func (c *compiler) storeMem(op opcode.Opcode, mem uint32, off uint64) {
	v := c.pop()
	a := c.pop()
	c.flushBelow(0)
	ea := c.addr(a, mem, off)
	m := memAt(mem)
	switch op {
	case opcode.I32StoreMem:
		x := c.i32(v)
		c.emit(func(f *frame) int32 {
			e := ea(f)
			binary.LittleEndian.PutUint32(m(f).span(e, 4), x(f))
			return 0
		})
	case opcode.I64StoreMem:
		x := c.i64(v)
		c.emit(func(f *frame) int32 {
			e := ea(f)
			binary.LittleEndian.PutUint64(m(f).span(e, 8), x(f))
			return 0
		})
	case opcode.F32StoreMem:
		x := c.f32(v)
		c.emit(func(f *frame) int32 {
			e := ea(f)
			val := x(f)
			binary.LittleEndian.PutUint32(m(f).span(e, 4), math.Float32bits(val))
			return 0
		})
	case opcode.F64StoreMem:
		x := c.f64(v)
		c.emit(func(f *frame) int32 {
			e := ea(f)
			val := x(f)
			binary.LittleEndian.PutUint64(m(f).span(e, 8), math.Float64bits(val))
			return 0
		})
	case opcode.I32StoreMem8, opcode.I64StoreMem8:
		x := c.lowBits(v)
		c.emit(func(f *frame) int32 {
			e := ea(f)
			val := x(f)
			m(f).span(e, 1)[0] = byte(val)
			return 0
		})
	case opcode.I32StoreMem16, opcode.I64StoreMem16:
		x := c.lowBits(v)
		c.emit(func(f *frame) int32 {
			e := ea(f)
			val := x(f)
			binary.LittleEndian.PutUint16(m(f).span(e, 2), uint16(val))
			return 0
		})
	case opcode.I64StoreMem32:
		x := c.i64(v)
		c.emit(func(f *frame) int32 {
			e := ea(f)
			val := x(f)
			binary.LittleEndian.PutUint32(m(f).span(e, 4), uint32(val))
			return 0
		})
	}
}

// lowBits returns an integer entry's value as a uint64.
func (c *compiler) lowBits(e entry) func(*frame) uint64 {
	if e.t.Kind == module.KindI64 {
		return c.i64(e)
	}
	v := c.i32(e)
	return func(f *frame) uint64 { return uint64(v(f)) }
}

// index returns a reader of an i32 or i64 index, as a uint64.
func (c *compiler) index(e entry, is64 bool) func(*frame) uint64 {
	if is64 {
		return c.i64(e)
	}
	v := c.i32(e)
	return func(f *frame) uint64 { return uint64(v(f)) }
}

// checkRange traps with code unless [start, start+n) lies within size.
func checkRange(start, n, size uint64, code TrapCode) {
	if start > size || n > size-start {
		trap(code)
	}
}

// prefixed compiles the instructions with a 0xfc prefix other than the
// saturating truncations, reporting whether op is one.
func (c *compiler) prefixed(op opcode.Opcode, at uint32) bool {
	switch op {
	case opcode.MemoryInit:
		seg := c.r.u32()
		mem := c.r.u32()
		n, s, d := c.i32(c.pop()), c.i32(c.pop()), c.index(c.pop(), c.m.Memories[mem].Limits.Is64)
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			dst, src, cnt := d(f), uint64(s(f)), uint64(n(f))
			data := f.inst.datas[seg]
			b := f.inst.Memories[mem].bytes()
			checkRange(src, cnt, uint64(len(data)), TrapMemOutOfBounds)
			checkRange(dst, cnt, uint64(len(b)), TrapMemOutOfBounds)
			copy(b[dst:dst+cnt], data[src:src+cnt])
			return 0
		})
	case opcode.DataDrop:
		seg := c.r.u32()
		c.flushBelow(0)
		c.emit(func(f *frame) int32 { f.inst.datas[seg] = nil; return 0 })
	case opcode.MemoryCopy:
		dm := c.r.u32()
		sm := c.r.u32()
		d64, s64 := c.m.Memories[dm].Limits.Is64, c.m.Memories[sm].Limits.Is64
		n := c.index(c.pop(), d64 && s64)
		s := c.index(c.pop(), s64)
		d := c.index(c.pop(), d64)
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			dst, src, cnt := d(f), s(f), n(f)
			db := f.inst.Memories[dm].bytes()
			sb := f.inst.Memories[sm].bytes()
			checkRange(src, cnt, uint64(len(sb)), TrapMemOutOfBounds)
			checkRange(dst, cnt, uint64(len(db)), TrapMemOutOfBounds)
			copy(db[dst:dst+cnt], sb[src:src+cnt])
			return 0
		})
	case opcode.MemoryFill:
		mem := c.r.u32()
		is64 := c.m.Memories[mem].Limits.Is64
		n := c.index(c.pop(), is64)
		v := c.i32(c.pop())
		d := c.index(c.pop(), is64)
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			dst, val, cnt := d(f), byte(v(f)), n(f)
			b := f.inst.Memories[mem].bytes()
			checkRange(dst, cnt, uint64(len(b)), TrapMemOutOfBounds)
			s := b[dst : dst+cnt]
			for i := range s {
				s[i] = val
			}
			return 0
		})
	case opcode.TableInit:
		seg := c.r.u32()
		table := c.r.u32()
		n, s, d := c.i32(c.pop()), c.i32(c.pop()), c.index(c.pop(), c.m.Tables[table].Limits.Is64)
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			dst, src, cnt := d(f), uint64(s(f)), uint64(n(f))
			elems := f.inst.elems[seg]
			t := f.inst.Tables[table]
			checkRange(src, cnt, uint64(len(elems)), TrapTableOutOfBounds)
			checkRange(dst, cnt, uint64(len(t.Elems)), TrapTableOutOfBounds)
			copy(t.Elems[dst:dst+cnt], elems[src:src+cnt])
			return 0
		})
	case opcode.ElemDrop:
		seg := c.r.u32()
		c.flushBelow(0)
		c.emit(func(f *frame) int32 { f.inst.elems[seg] = nil; return 0 })
	case opcode.TableCopy:
		dt := c.r.u32()
		st := c.r.u32()
		d64, s64 := c.m.Tables[dt].Limits.Is64, c.m.Tables[st].Limits.Is64
		n := c.index(c.pop(), d64 && s64)
		s := c.index(c.pop(), s64)
		d := c.index(c.pop(), d64)
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			dst, src, cnt := d(f), s(f), n(f)
			de := f.inst.Tables[dt].Elems
			se := f.inst.Tables[st].Elems
			checkRange(src, cnt, uint64(len(se)), TrapTableOutOfBounds)
			checkRange(dst, cnt, uint64(len(de)), TrapTableOutOfBounds)
			copy(de[dst:dst+cnt], se[src:src+cnt])
			return 0
		})
	case opcode.TableGrow:
		table := c.r.u32()
		is64 := c.m.Tables[table].Limits.Is64
		n := c.index(c.pop(), is64)
		v := c.ref(c.pop())
		if is64 {
			c.pushTree(module.I64, ti64(func(f *frame) uint64 {
				val := v(f)
				return uint64(f.inst.Tables[table].Grow(n(f), val))
			}))
		} else {
			c.pushTree(module.I32, ti32(func(f *frame) uint32 {
				val := v(f)
				return uint32(f.inst.Tables[table].Grow(n(f), val))
			}))
		}
	case opcode.TableSize:
		table := c.r.u32()
		if c.m.Tables[table].Limits.Is64 {
			c.pushTree(module.I64, ti64(func(f *frame) uint64 { return f.inst.Tables[table].Size() }))
		} else {
			c.pushTree(module.I32, ti32(func(f *frame) uint32 { return uint32(f.inst.Tables[table].Size()) }))
		}
	case opcode.TableFill:
		table := c.r.u32()
		is64 := c.m.Tables[table].Limits.Is64
		n := c.index(c.pop(), is64)
		v := c.ref(c.pop())
		d := c.index(c.pop(), is64)
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			dst, val, cnt := d(f), v(f), n(f)
			e := f.inst.Tables[table].Elems
			checkRange(dst, cnt, uint64(len(e)), TrapTableOutOfBounds)
			s := e[dst : dst+cnt]
			for i := range s {
				s[i] = val
			}
			return 0
		})
	default:
		return false
	}
	return true
}

// tableGetSet compiles table.get and table.set.
func (c *compiler) tableGetSet(op opcode.Opcode, table uint32, at uint32) {
	is64 := c.m.Tables[table].Limits.Is64
	t := c.m.Tables[table].Type
	if op == opcode.TableGet {
		i := c.index(c.pop(), is64)
		c.pushTree(t, tref(func(f *frame) Ref {
			e := f.inst.Tables[table].Elems
			x := i(f)
			if x >= uint64(len(e)) {
				trap(TrapTableOutOfBounds)
			}
			return e[x]
		}))
		return
	}
	v := c.ref(c.pop())
	i := c.index(c.pop(), is64)
	c.flushBelow(0)
	c.emit(func(f *frame) int32 {
		x := i(f)
		val := v(f)
		e := f.inst.Tables[table].Elems
		if x >= uint64(len(e)) {
			trap(TrapTableOutOfBounds)
		}
		e[x] = val
		return 0
	})
}

// endOther ends a construct of the exception handling instructions.
func (c *compiler) endOther(t *ctl, body []stmt) stmt {
	panic("engine: unsupported construct " + t.op.Name())
}
