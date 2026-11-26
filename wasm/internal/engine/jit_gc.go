//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"encoding/binary"
	"unsafe"

	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// jitGCOp runs GC instruction op for native code, as the interpreter's
// ops_gc.go does, with immediates a and b, its operands in the stack
// positions of the exited frame from the one whose numeric slot is slot,
// its result left in the first. What it allocates is charged to the
// instance's budget, the native frames' references counted with the rest.
// It reports whether op is one. at is the instruction's offset in the
// module, for its traps.
func (x *exec) jitGCOp(inst *Instance, op opcode.Opcode, a, b uint32, f *exitFrame, slot int, at uint32) bool {
	m := inst.Module
	get := func(i int, t module.ValueType) Val { return f.get(t, slot+2*i) }
	u32 := func(i int) uint32 { return uint32(f.l[slot+2*i]) }
	arr := func(i int) *Array {
		r := f.r[f.rslot(slot+2*i)]
		if r.IsNull() {
			trapAt(TrapNullDereference, uint64(at))
		}
		return asArray(r)
	}
	if wjit.StubOp(op) {
		x.jitNew(inst, op, a, b, f, slot, at, false)
		return true
	}
	switch op {
	case opcode.ArrayFill:
		et := m.Types[a].Array.Elem.Type
		ar := arr(0)
		dst, val, cnt := u32(1), get(2, et.Unpacked()), u32(3)
		checkArray(ar, dst, cnt, uint64(at))
		for i := uint32(0); i < cnt; i++ {
			arraySet(ar, et, int(dst+i), val)
		}
	case opcode.ArrayCopy:
		et := m.Types[a].Array.Elem.Type
		da := arr(0)
		dst := u32(1)
		sa := arr(2)
		src, cnt := u32(3), u32(4)
		checkArray(da, dst, cnt, uint64(at))
		checkArray(sa, src, cnt, uint64(at))
		if et.IsRef() {
			copy(da.refs()[dst:dst+cnt], sa.refs()[src:src+cnt])
		} else {
			size := uint32(et.Size())
			copy(da.bytes()[dst*size:(dst+cnt)*size], sa.bytes()[src*size:(src+cnt)*size])
		}
	case opcode.ArrayInitData, opcode.ArrayInitElem:
		et := m.Types[a].Array.Elem.Type
		ar := arr(0)
		dst, src, cnt := u32(1), u32(2), u32(3)
		checkArray(ar, dst, cnt, uint64(at))
		if op == opcode.ArrayInitData {
			d := inst.datas[b]
			size := uint64(et.Size())
			if uint64(src)+uint64(cnt)*size > uint64(len(d)) {
				trapAt(TrapDataSegmentOutOfBounds, uint64(at))
			}
			copy(ar.bytes()[uint64(dst)*size:], d[src:uint64(src)+uint64(cnt)*size])
			return true
		}
		e := inst.elems[b]
		if uint64(src)+uint64(cnt) > uint64(len(e)) {
			trapAt(TrapElementSegmentOutOfBounds, uint64(at))
		}
		copy(ar.refs()[dst:dst+cnt], e[src:src+cnt])
	default:
		return false
	}
	return true
}

// jitNew runs instruction op, one that makes an object (wjit.StubOp), for
// native code, as jitGCOp does, with immediates a and b, its operands in
// the stack positions of the exited frame from the one whose numeric slot
// is slot, the object left in the first. Where try -- for the allocation
// stub (jit_alloc.go) -- it does nothing where the instruction would trap
// or the budget holds the charge only once what is reachable is counted
// again, which the exit does, and reports whether it made the object.
func (x *exec) jitNew(inst *Instance, op opcode.Opcode, a, b uint32, f *exitFrame, slot int, at uint32, try bool) bool {
	m := inst.Module
	get := func(i int, t module.ValueType) Val { return f.get(t, slot+2*i) }
	u32 := func(i int) uint32 { return uint32(f.l[slot+2*i]) }
	rt := inst.Code.typeRTT(a)
	// charge charges n bytes, or, where try, reports whether the budget
	// holds them as they are.
	charge := func(n int64) bool {
		if try {
			return inst.budget.tryChargeGC(n)
		}
		inst.budget.chargeGC(x, n)
		return true
	}
	// array makes an array of n elements, nil where try declines it.
	array := func(n uint32) *Array {
		if !try {
			return newArray(rt, n, inst.budget, x, uint64(at))
		}
		size := rt.elemSize
		if rt.refElems {
			size = 8
		}
		if uint64(n) > uint64(maxArrayLength(size)) || !inst.budget.tryChargeGC(arraySize(rt, n)) {
			return nil
		}
		return makeArray(rt, int(n))
	}
	ref := module.Ref(module.HeapIndex(a))
	switch op {
	case opcode.StructNew, opcode.StructNewDefault:
		if !charge(structSize(rt)) {
			return false
		}
		r := newStruct(rt)
		if op == opcode.StructNew {
			for i, fl := range m.Types[a].Struct.Fields {
				valToCell(fl.Type, cellAt(r.p, rt.cells[i]), get(i, fl.Type.Unpacked()))
			}
		}
		f.put(ref, slot, Val{R: r})
	case opcode.ArrayNew, opcode.ArrayNewDefault:
		n := u32(0)
		if op == opcode.ArrayNew {
			n = u32(1)
		}
		ar := array(n)
		if ar == nil {
			return false
		}
		if op == opcode.ArrayNew {
			et := m.Types[a].Array.Elem.Type
			val := get(0, et.Unpacked())
			for i := 0; i < ar.n; i++ {
				arraySet(ar, et, i, val)
			}
		}
		f.put(ref, slot, Val{R: ArrayRef(ar)})
	case opcode.ArrayNewFixed:
		// The operands stay in their slots until the result is put.
		ar := array(b)
		if ar == nil {
			return false
		}
		f.fixedElems(ar, m.Types[a].Array.Elem.Type.Kind, slot)
		f.put(ref, slot, Val{R: ArrayRef(ar)})
	case opcode.ArrayNewData:
		src, cnt := u32(0), u32(1)
		d := inst.datas[b]
		if uint64(src)+uint64(cnt)*uint64(rt.elemSize) > uint64(len(d)) {
			if try {
				return false
			}
			trapAt(TrapDataSegmentOutOfBounds, uint64(at))
		}
		ar := array(cnt)
		if ar == nil {
			return false
		}
		copy(ar.bytes(), d[src:])
		f.put(ref, slot, Val{R: ArrayRef(ar)})
	case opcode.ArrayNewElem:
		src, cnt := u32(0), u32(1)
		e := inst.elems[b]
		if uint64(src)+uint64(cnt) > uint64(len(e)) {
			if try {
				return false
			}
			trapAt(TrapElementSegmentOutOfBounds, uint64(at))
		}
		ar := array(cnt)
		if ar == nil {
			return false
		}
		copy(ar.refs(), e[src:])
		f.put(ref, slot, Val{R: ArrayRef(ar)})
	}
	return true
}

// fixedElems stores array.new_fixed's operands, of storage type t, from
// the stack positions of f from the one whose numeric slot is slot, into
// the elements of ar, as many: each straight from its slot, as arraySet
// stores it.
func (f *exitFrame) fixedElems(ar *Array, t module.Kind, slot int) {
	if ar.n == 0 {
		return
	}
	switch t {
	case module.KindRef:
		e := unsafe.Slice((*Ref)(ar.e), ar.n)
		for i := range e {
			e[i] = f.r[f.rslot(slot+2*i)]
		}
	case module.KindI8:
		e := unsafe.Slice((*byte)(ar.e), ar.n)
		for i := range e {
			e[i] = uint8(f.l[slot+2*i])
		}
	case module.KindI16:
		e := unsafe.Slice((*byte)(ar.e), 2*ar.n)
		for i := range ar.n {
			binary.LittleEndian.PutUint16(e[2*i:], uint16(f.l[slot+2*i]))
		}
	case module.KindI32, module.KindF32:
		e := unsafe.Slice((*byte)(ar.e), 4*ar.n)
		for i := range ar.n {
			binary.LittleEndian.PutUint32(e[4*i:], uint32(f.l[slot+2*i]))
		}
	case module.KindV128:
		e := unsafe.Slice((*byte)(ar.e), 16*ar.n)
		for i := range ar.n {
			binary.LittleEndian.PutUint64(e[16*i:], f.l[slot+2*i])
			binary.LittleEndian.PutUint64(e[16*i+8:], f.l[slot+2*i+1])
		}
	default:
		e := unsafe.Slice((*byte)(ar.e), 8*ar.n)
		for i := range ar.n {
			binary.LittleEndian.PutUint64(e[8*i:], f.l[slot+2*i])
		}
	}
}

// Pools of new objects.
//
// Native code makes a struct or a short array by taking one from a pool,
// made ahead by Go -- its header written, its fields or elements zero --
// and writing its fields, or its length and its elements. A level's pools
// are a table by the module's type index (Ctx.Pools): a struct type's
// entry is its pool, an array type's a table of pools by the class of the
// arrays' blocks (arrayPools), whose arrays an array of any length its
// class has room for is taken from. The level's first ExitAlloc takes a
// table from the instance, or shares the table of a level below it of the
// same instance, and the level gives it back to the instance when it
// ends: one goroutine at a time
// takes from a table, so native code takes an object with plain loads and
// stores. Go fills an empty pool at an ExitAlloc, charging the instance's
// budget for every object it makes at once; what a table holds when it is
// collected, never reachable, is given back (Budget.giveBackGC). The
// collector sees every object: a pool's words are Go pointers in a Go
// object, and native code clears a word it takes the object of through the
// write barrier while the collector marks, as it stores the object's
// pointer into its slot.

// levelPools gives level lv, the innermost, a table of pools: a level's
// below of the same instance, or one of the instance's.
func (x *exec) levelPools(lv *jitLevel) {
	for i := x.jit.n - 2; i >= 0; i-- {
		if o := x.jit.levels[i]; o.inst == lv.inst && o.pools != nil {
			lv.pools, lv.ownPools = o.pools, false
			lv.ctx.Pools = o.ctx.Pools
			return
		}
	}
	lv.pools, lv.ownPools = lv.inst.takePools(), true
	lv.ctx.Pools = uintptr(unsafe.Pointer(&lv.pools.set.tab[0]))
}

// dropPools gives back the table of level lv, which ends.
func (lv *jitLevel) dropPools() {
	if lv.ownPools {
		lv.inst.putPools(lv.pools)
	}
	lv.pools, lv.ownPools = nil, false
}

// jitAlloc fills a pool of level lv's table, the innermost level's, giving
// it a table first if it has none, for an ExitAlloc of frame f whose
// Arg0 is what and whose Arg2 is length, at offset at: the type index in
// what's low word, and in its high word 0 for a struct type, the class of
// an array's block plus 1, AllocLength for the class of an array of
// length elements, or AllocMake and the reference slot an array of length
// elements is made into here. The objects are charged to the instance's
// budget, the native frames' references counted with the rest.
func (x *exec) jitAlloc(lv *jitLevel, what, length uint64, f *exitFrame, at uint32) {
	x.allocFor(lv, what, length, f, at, false)
}

// allocFor is jitAlloc's work, which the allocation stub's Go does too
// (jitAllocStub), reporting whether it was done: where try, it does nothing
// that would trap or that the budget holds only once what is reachable is
// counted again, which the exit, its frames as Go sees them, does.
func (x *exec) allocFor(lv *jitLevel, what, length uint64, f *exitFrame, at uint32, try bool) bool {
	ti, hi := uint32(what), uint32(what>>32)
	class := int(hi) - 1
	b := lv.inst.budget
	switch {
	case hi&wjit.AllocMake != 0:
		t := lv.inst.Code.typeRTT(ti)
		var a *Array
		if try {
			size := t.elemSize
			if t.refElems {
				size = 8
			}
			if length > uint64(maxArrayLength(size)) || !b.tryChargeGC(arraySize(t, uint32(length))) {
				return false
			}
			a = makeArray(t, int(length))
		} else {
			a = newArray(t, uint32(length), b, x, uint64(at))
		}
		f.r[hi&^wjit.AllocMake] = ArrayRef(a)
		return true
	case hi == wjit.AllocLength:
		class = arrayClass(int(length) * lv.inst.Code.typeRTT(ti).elemBytes())
	}
	if lv.pools == nil {
		x.levelPools(lv)
	}
	set := lv.pools.set
	p := set.pool(lv.inst.Code, ti, class)
	if p.n > 0 {
		return true
	}
	return set.fill(p, x, try)
}
