//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// jitGCOp runs GC instruction op for native code, as the interpreter's
// ops_gc.go does, with immediates a and b, its operands in the stack
// positions of the exited frame from the one whose numeric slot is slot,
// its result left in the first. What it allocates is charged to the
// instance's budget, the native frames' references counted with the rest.
// It reports whether op is one.
func (x *exec) jitGCOp(inst *Instance, op opcode.Opcode, a, b uint32, f *exitFrame, slot int) bool {
	m := inst.Module
	get := func(i int, t module.ValueType) Val { return f.get(t, slot+2*i) }
	u32 := func(i int) uint32 { return uint32(f.l[slot+2*i]) }
	arr := func(i int) *Array {
		r := f.r[f.rslot(slot+2*i)]
		if r.IsNull() {
			trap(TrapNullDereference)
		}
		return asArray(r)
	}
	ref := module.Ref(module.HeapIndex(a))
	switch op {
	case opcode.StructNew, opcode.StructNewDefault:
		rt := rttOf(m.Canonical[a])
		inst.budget.chargeGC(x, structSize(rt))
		r := newStruct(rt)
		if op == opcode.StructNew {
			for i, fl := range m.Types[a].Struct.Fields {
				valToCell(fl.Type, cellAt(r.p, rt.cells[i]), get(i, fl.Type.Unpacked()))
			}
		}
		f.put(ref, slot, Val{R: r})
	case opcode.ArrayNew, opcode.ArrayNewDefault:
		rt := rttOf(m.Canonical[a])
		et := m.Types[a].Array.Elem.Type
		if op == opcode.ArrayNewDefault {
			f.put(ref, slot, Val{R: ArrayRef(newArray(rt, u32(0), inst.budget, x))})
			return true
		}
		val := get(0, et.Unpacked())
		ar := newArray(rt, u32(1), inst.budget, x)
		for i := 0; i < ar.n; i++ {
			arraySet(ar, et, i, val)
		}
		f.put(ref, slot, Val{R: ArrayRef(ar)})
	case opcode.ArrayNewFixed:
		rt := rttOf(m.Canonical[a])
		et := m.Types[a].Array.Elem.Type
		vals := make([]Val, b)
		for i := range vals {
			vals[i] = get(i, et.Unpacked())
		}
		ar := newArray(rt, b, inst.budget, x)
		for i, v := range vals {
			arraySet(ar, et, i, v)
		}
		f.put(ref, slot, Val{R: ArrayRef(ar)})
	case opcode.ArrayNewData, opcode.ArrayNewElem:
		rt := rttOf(m.Canonical[a])
		src, cnt := u32(0), u32(1)
		if op == opcode.ArrayNewData {
			d := inst.datas[b]
			if uint64(src)+uint64(cnt)*uint64(rt.elemSize) > uint64(len(d)) {
				trap(TrapDataSegmentOutOfBounds)
			}
			ar := newArray(rt, cnt, inst.budget, x)
			copy(ar.b, d[src:])
			f.put(ref, slot, Val{R: ArrayRef(ar)})
			return true
		}
		e := inst.elems[b]
		if uint64(src)+uint64(cnt) > uint64(len(e)) {
			trap(TrapElementSegmentOutOfBounds)
		}
		ar := newArray(rt, cnt, inst.budget, x)
		copy(ar.r, e[src:])
		f.put(ref, slot, Val{R: ArrayRef(ar)})
	case opcode.ArrayFill:
		et := m.Types[a].Array.Elem.Type
		ar := arr(0)
		dst, val, cnt := u32(1), get(2, et.Unpacked()), u32(3)
		checkArray(ar, dst, cnt)
		for i := uint32(0); i < cnt; i++ {
			arraySet(ar, et, int(dst+i), val)
		}
	case opcode.ArrayCopy:
		et := m.Types[a].Array.Elem.Type
		da := arr(0)
		dst := u32(1)
		sa := arr(2)
		src, cnt := u32(3), u32(4)
		checkArray(da, dst, cnt)
		checkArray(sa, src, cnt)
		if et.IsRef() {
			copy(da.r[dst:dst+cnt], sa.r[src:src+cnt])
		} else {
			size := uint32(et.Size())
			copy(da.b[dst*size:(dst+cnt)*size], sa.b[src*size:(src+cnt)*size])
		}
	case opcode.ArrayInitData, opcode.ArrayInitElem:
		et := m.Types[a].Array.Elem.Type
		ar := arr(0)
		dst, src, cnt := u32(1), u32(2), u32(3)
		checkArray(ar, dst, cnt)
		if op == opcode.ArrayInitData {
			d := inst.datas[b]
			size := uint64(et.Size())
			if uint64(src)+uint64(cnt)*size > uint64(len(d)) {
				trap(TrapDataSegmentOutOfBounds)
			}
			copy(ar.b[uint64(dst)*size:], d[src:uint64(src)+uint64(cnt)*size])
			return true
		}
		e := inst.elems[b]
		if uint64(src)+uint64(cnt) > uint64(len(e)) {
			trap(TrapElementSegmentOutOfBounds)
		}
		copy(ar.r[dst:dst+cnt], e[src:src+cnt])
	default:
		return false
	}
	return true
}
