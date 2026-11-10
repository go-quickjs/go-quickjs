//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"runtime"
	"sync"
	"unsafe"

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
			trapAt(TrapNullDereference, at)
		}
		return asArray(r)
	}
	ref := module.Ref(module.HeapIndex(a))
	switch op {
	case opcode.StructNew, opcode.StructNewDefault:
		rt := inst.Code.typeRTT(a)
		inst.budget.chargeGC(x, structSize(rt))
		r := newStruct(rt)
		if op == opcode.StructNew {
			for i, fl := range m.Types[a].Struct.Fields {
				valToCell(fl.Type, cellAt(r.p, rt.cells[i]), get(i, fl.Type.Unpacked()))
			}
		}
		f.put(ref, slot, Val{R: r})
	case opcode.ArrayNew, opcode.ArrayNewDefault:
		rt := inst.Code.typeRTT(a)
		et := m.Types[a].Array.Elem.Type
		if op == opcode.ArrayNewDefault {
			f.put(ref, slot, Val{R: ArrayRef(newArray(rt, u32(0), inst.budget, x, at))})
			return true
		}
		val := get(0, et.Unpacked())
		ar := newArray(rt, u32(1), inst.budget, x, at)
		for i := 0; i < ar.n; i++ {
			arraySet(ar, et, i, val)
		}
		f.put(ref, slot, Val{R: ArrayRef(ar)})
	case opcode.ArrayNewFixed:
		rt := inst.Code.typeRTT(a)
		et := m.Types[a].Array.Elem.Type
		vals := make([]Val, b)
		for i := range vals {
			vals[i] = get(i, et.Unpacked())
		}
		ar := newArray(rt, b, inst.budget, x, at)
		for i, v := range vals {
			arraySet(ar, et, i, v)
		}
		f.put(ref, slot, Val{R: ArrayRef(ar)})
	case opcode.ArrayNewData, opcode.ArrayNewElem:
		rt := inst.Code.typeRTT(a)
		src, cnt := u32(0), u32(1)
		if op == opcode.ArrayNewData {
			d := inst.datas[b]
			if uint64(src)+uint64(cnt)*uint64(rt.elemSize) > uint64(len(d)) {
				trapAt(TrapDataSegmentOutOfBounds, at)
			}
			ar := newArray(rt, cnt, inst.budget, x, at)
			copy(ar.b, d[src:])
			f.put(ref, slot, Val{R: ArrayRef(ar)})
			return true
		}
		e := inst.elems[b]
		if uint64(src)+uint64(cnt) > uint64(len(e)) {
			trapAt(TrapElementSegmentOutOfBounds, at)
		}
		ar := newArray(rt, cnt, inst.budget, x, at)
		copy(ar.r, e[src:])
		f.put(ref, slot, Val{R: ArrayRef(ar)})
	case opcode.ArrayFill:
		et := m.Types[a].Array.Elem.Type
		ar := arr(0)
		dst, val, cnt := u32(1), get(2, et.Unpacked()), u32(3)
		checkArray(ar, dst, cnt, at)
		for i := uint32(0); i < cnt; i++ {
			arraySet(ar, et, int(dst+i), val)
		}
	case opcode.ArrayCopy:
		et := m.Types[a].Array.Elem.Type
		da := arr(0)
		dst := u32(1)
		sa := arr(2)
		src, cnt := u32(3), u32(4)
		checkArray(da, dst, cnt, at)
		checkArray(sa, src, cnt, at)
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
		checkArray(ar, dst, cnt, at)
		if op == opcode.ArrayInitData {
			d := inst.datas[b]
			size := uint64(et.Size())
			if uint64(src)+uint64(cnt)*size > uint64(len(d)) {
				trapAt(TrapDataSegmentOutOfBounds, at)
			}
			copy(ar.b[uint64(dst)*size:], d[src:uint64(src)+uint64(cnt)*size])
			return true
		}
		e := inst.elems[b]
		if uint64(src)+uint64(cnt) > uint64(len(e)) {
			trapAt(TrapElementSegmentOutOfBounds, at)
		}
		copy(ar.r[dst:dst+cnt], e[src:src+cnt])
	default:
		return false
	}
	return true
}

// Pools of new structs.
//
// Native code makes a struct by taking one from a pool of its type's, made
// ahead by Go -- its header written, its fields zero -- and writing its
// fields. A level's pools are a table by the module's type index
// (Ctx.Pools), which the level's first ExitAlloc takes from the instance,
// or shares with a level below it of the same instance, and which the
// level gives back to the instance when it ends: one goroutine at a time
// takes from a table, so native code takes an object with plain loads and
// stores. Go fills an empty pool at an ExitAlloc, charging the instance's
// budget for every object it makes at once; what a table holds when it is
// collected, never reachable, is given back (Budget.giveBackGC). The
// collector sees every object: a pool's words are Go pointers in a Go
// object, and native code clears a word it takes the object of through the
// write barrier while the collector marks, as it stores the object's
// pointer into its slot.

// gcPoolCap is the most objects a pool holds, gcPoolFirst how many its first
// fill makes, each fill making twice the last's; gcPoolBytes bounds what a
// fill makes, and gcPoolsHeld what a table's pools made at their last fills
// together.
const (
	gcPoolCap   = 128
	gcPoolFirst = 8
	gcPoolBytes = 16 << 10
	gcPoolsHeld = 256 << 10
)

// gcPool is a pool of new structs of one type: the first n of objs. Native
// code reads and writes n and objs (Layout.PoolCount, Layout.PoolObjs).
type gcPool struct {
	n    uint64
	objs [gcPoolCap]unsafe.Pointer
	rt   *rtt
	// each is what a struct of the type is charged; size how many the
	// next fill makes; made what the last fill made, in bytes; epoch the
	// budget's (gcEpoch) before the last fill charged it.
	each, made int64
	size       int
	epoch      uint64
}

// gcPoolSet is a table of pools by the module's type index, charged to
// budget b.
type gcPoolSet struct {
	b   *Budget
	tab []*gcPool
	// held is what the pools' last fills made, in bytes.
	held int64
}

// gcPools is what levels and instances hold of a table: the cleanup that
// gives back what it holds runs once nothing holds it.
type gcPools struct{ set *gcPoolSet }

// giveBack gives back to the budget what the pools hold, never reachable.
func (s *gcPoolSet) giveBack() {
	for _, p := range s.tab {
		if p != nil && p.n > 0 {
			s.b.giveBackGC(p.epoch, int64(p.n)*p.each)
		}
	}
}

// jitPools is an instance's tables of pools no level holds.
type jitPools struct {
	mu   sync.Mutex
	free []*gcPools
}

// jitPoolsKept is how many tables an instance keeps for its levels.
const jitPoolsKept = 4

// takePools returns a table of the instance's for a level.
func (inst *Instance) takePools() *gcPools {
	jp := &inst.jit.pools
	jp.mu.Lock()
	if n := len(jp.free); n > 0 {
		g := jp.free[n-1]
		jp.free[n-1] = nil
		jp.free = jp.free[:n-1]
		jp.mu.Unlock()
		return g
	}
	jp.mu.Unlock()
	set := &gcPoolSet{b: inst.budget, tab: make([]*gcPool, max(len(inst.Module.Types), 1))}
	g := &gcPools{set: set}
	if set.b != nil {
		runtime.AddCleanup(g, (*gcPoolSet).giveBack, set)
	}
	return g
}

// putPools gives back table g, which a level took, to the instance.
func (inst *Instance) putPools(g *gcPools) {
	jp := &inst.jit.pools
	jp.mu.Lock()
	if len(jp.free) < jitPoolsKept {
		jp.free = append(jp.free, g)
	}
	jp.mu.Unlock()
}

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

// jitAlloc fills the pool of the structs of type ti of level lv's table,
// the innermost level's, giving it a table first if it has none, for a
// struct.new at offset at. The structs are charged to the instance's
// budget, the native frames' references counted with the rest.
func (x *exec) jitAlloc(lv *jitLevel, ti uint32) {
	if lv.pools == nil {
		x.levelPools(lv)
	}
	set := lv.pools.set
	p := set.tab[ti]
	if p == nil {
		rt := lv.inst.Code.typeRTT(ti)
		p = &gcPool{rt: rt, each: structSize(rt), size: gcPoolFirst}
		set.tab[ti] = p
	}
	if p.n > 0 {
		return
	}
	set.held -= p.made
	n := min(p.size, max(int(gcPoolBytes/p.each), 1))
	if room := (gcPoolsHeld - set.held) / p.each; int64(n) > room {
		n = int(max(room, 1))
	}
	b := lv.inst.budget
	epoch := b.gcEpoch()
	p.made = 0
	b.chargeGC(x, int64(n)*p.each)
	for i := range n {
		p.objs[i] = newStruct(p.rt).p
	}
	p.n, p.epoch, p.made = uint64(n), epoch, int64(n)*p.each
	set.held += p.made
	p.size = min(2*n, gcPoolCap)
}
