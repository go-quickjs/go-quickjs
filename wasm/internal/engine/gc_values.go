package engine

import (
	"encoding/binary"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// fieldGet returns a reader of a field of storage type t from a cell, as the
// value type it is read as: packed fields extended as signed asks.
func cellToVal(t module.ValueType, signed bool) func(c *Ref) Val {
	switch t.Kind {
	case module.KindI8:
		if signed {
			return func(c *Ref) Val { return Val{N: uint64(uint32(int32(int8(c.i))))} }
		}
		return func(c *Ref) Val { return Val{N: uint64(uint8(c.i))} }
	case module.KindI16:
		if signed {
			return func(c *Ref) Val { return Val{N: uint64(uint32(int32(int16(c.i))))} }
		}
		return func(c *Ref) Val { return Val{N: uint64(uint16(c.i))} }
	case module.KindV128:
		return func(c *Ref) Val {
			next := (*Ref)(unsafe.Add(unsafe.Pointer(c), unsafe.Sizeof(Ref{})))
			return Val{N: c.i, Hi: next.i}
		}
	case module.KindRef:
		return func(c *Ref) Val { return Val{R: *c} }
	}
	return func(c *Ref) Val { return Val{N: c.i} }
}

// valToCell stores v, of storage type t, into a cell.
func valToCell(t module.ValueType, c *Ref, v Val) {
	switch t.Kind {
	case module.KindI8:
		c.i = uint64(uint8(v.N))
	case module.KindI16:
		c.i = uint64(uint16(v.N))
	case module.KindV128:
		c.i = v.N
		(*Ref)(unsafe.Add(unsafe.Pointer(c), unsafe.Sizeof(Ref{}))).i = v.Hi
	case module.KindRef:
		*c = v.R
	case module.KindI32, module.KindF32:
		c.i = uint64(uint32(v.N))
	default:
		c.i = v.N
	}
}

// arrayGet reads element i, within its length, of array a of storage type
// t.
func arrayGet(a *Array, t module.ValueType, i int, signed bool) Val {
	switch t.Kind {
	case module.KindRef:
		return Val{R: *(*Ref)(unsafe.Add(a.e, uintptr(i)*unsafe.Sizeof(Ref{})))}
	case module.KindI8:
		v := *(*uint8)(unsafe.Add(a.e, i))
		if signed {
			return Val{N: uint64(uint32(int32(int8(v))))}
		}
		return Val{N: uint64(v)}
	case module.KindI16:
		v := binary.LittleEndian.Uint16(elemBytes(a, 2*i, 2))
		if signed {
			return Val{N: uint64(uint32(int32(int16(v))))}
		}
		return Val{N: uint64(v)}
	case module.KindI32, module.KindF32:
		return Val{N: uint64(binary.LittleEndian.Uint32(elemBytes(a, 4*i, 4)))}
	case module.KindV128:
		e := elemBytes(a, 16*i, 16)
		return Val{N: binary.LittleEndian.Uint64(e), Hi: binary.LittleEndian.Uint64(e[8:])}
	}
	return Val{N: binary.LittleEndian.Uint64(elemBytes(a, 8*i, 8))}
}

// arraySet writes element i, within its length, of array a of storage type
// t.
func arraySet(a *Array, t module.ValueType, i int, v Val) {
	switch t.Kind {
	case module.KindRef:
		*(*Ref)(unsafe.Add(a.e, uintptr(i)*unsafe.Sizeof(Ref{}))) = v.R
	case module.KindI8:
		*(*uint8)(unsafe.Add(a.e, i)) = byte(v.N)
	case module.KindI16:
		binary.LittleEndian.PutUint16(elemBytes(a, 2*i, 2), uint16(v.N))
	case module.KindI32, module.KindF32:
		binary.LittleEndian.PutUint32(elemBytes(a, 4*i, 4), uint32(v.N))
	case module.KindV128:
		e := elemBytes(a, 16*i, 16)
		binary.LittleEndian.PutUint64(e, v.N)
		binary.LittleEndian.PutUint64(e[8:], v.Hi)
	default:
		binary.LittleEndian.PutUint64(elemBytes(a, 8*i, 8), v.N)
	}
}

// elemBytes returns the size bytes at off among the elements of number
// array a.
func elemBytes(a *Array, off, size int) []byte {
	return unsafe.Slice((*byte)(unsafe.Add(a.e, off)), size)
}

// checkArray traps, as the instruction at offset at, unless [i, i+n) lies
// within array a.
func checkArray(a *Array, i, n uint32, at uint64) {
	if uint64(i)+uint64(n) > uint64(a.n) {
		trapAt(TrapArrayOutOfBounds, at)
	}
}

// evalConstGC evaluates a GC instruction of a constant expression.
func evalConstGC(inst *Instance, op opcode.Opcode, r *reader, stack *[]Val) (Val, bool) {
	m := inst.Module
	pop := func() Val {
		s := *stack
		v := s[len(s)-1]
		*stack = s[:len(s)-1]
		return v
	}
	push := func(v Val) { *stack = append(*stack, v) }
	switch op {
	case opcode.StructNew, opcode.StructNewDefault:
		ti := r.u32()
		rt := rttOf(m.Canonical[ti])
		inst.budget.chargeGC(nil, structSize(rt))
		s := newStruct(rt)
		if op == opcode.StructNew {
			fields := m.Types[ti].Struct.Fields
			vals := make([]Val, len(fields))
			for i := len(fields) - 1; i >= 0; i-- {
				vals[i] = pop()
			}
			for i, fl := range fields {
				valToCell(fl.Type, cellAt(s.p, rt.cells[i]), vals[i])
			}
		}
		push(Val{R: s})
	case opcode.ArrayNew, opcode.ArrayNewDefault:
		ti := r.u32()
		rt := rttOf(m.Canonical[ti])
		et := m.Types[ti].Array.Elem.Type
		n := uint32(pop().N)
		var v Val
		if op == opcode.ArrayNew {
			v = pop()
		}
		a := newArray(rt, n, inst.budget, nil, 0)
		if op == opcode.ArrayNew {
			for i := 0; i < a.n; i++ {
				arraySet(a, et, i, v)
			}
		}
		push(Val{R: ArrayRef(a)})
	case opcode.ArrayNewFixed:
		ti := r.u32()
		n := r.u32()
		rt := rttOf(m.Canonical[ti])
		et := m.Types[ti].Array.Elem.Type
		vals := make([]Val, n)
		for i := int(n) - 1; i >= 0; i-- {
			vals[i] = pop()
		}
		a := newArray(rt, n, inst.budget, nil, 0)
		for i, v := range vals {
			arraySet(a, et, i, v)
		}
		push(Val{R: ArrayRef(a)})
	case opcode.RefI31:
		push(Val{R: I31(uint32(pop().N))})
	case opcode.AnyConvertExtern, opcode.ExternConvertAny:
	default:
		return Val{}, false
	}
	return Val{}, true
}
