//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"runtime"
	"sync"
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
		// The operands stay in their slots until the result is put.
		ar := newArray(rt, b, inst.budget, x, at)
		for i := range int(b) {
			arraySet(ar, et, i, get(i, et.Unpacked()))
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
			copy(ar.bytes(), d[src:])
			f.put(ref, slot, Val{R: ArrayRef(ar)})
			return true
		}
		e := inst.elems[b]
		if uint64(src)+uint64(cnt) > uint64(len(e)) {
			trapAt(TrapElementSegmentOutOfBounds, at)
		}
		ar := newArray(rt, cnt, inst.budget, x, at)
		copy(ar.refs(), e[src:])
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
			copy(da.refs()[dst:dst+cnt], sa.refs()[src:src+cnt])
		} else {
			size := uint32(et.Size())
			copy(da.bytes()[dst*size:(dst+cnt)*size], sa.bytes()[src*size:(src+cnt)*size])
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
			copy(ar.bytes()[uint64(dst)*size:], d[src:uint64(src)+uint64(cnt)*size])
			return true
		}
		e := inst.elems[b]
		if uint64(src)+uint64(cnt) > uint64(len(e)) {
			trapAt(TrapElementSegmentOutOfBounds, at)
		}
		copy(ar.refs()[dst:dst+cnt], e[src:src+cnt])
	default:
		return false
	}
	return true
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

// gcPoolCap is the most objects a pool holds, gcPoolFirst how many its first
// fill makes, each fill making twice the last's; gcPoolBytes bounds what a
// fill makes, and gcPoolsHeld what a table's pools hold together: what
// their last fills made, counted again from what they hold where that
// leaves a fill short (gcPoolSet.recount).
const (
	gcPoolCap   = 128
	gcPoolFirst = 8
	gcPoolBytes = 16 << 10
	gcPoolsHeld = 256 << 10
)

// gcPool is a pool of new objects of one type, and for an array type of
// one class: the first n of objs. Native code reads and writes n and objs
// (Layout.PoolCount, Layout.PoolObjs).
type gcPool struct {
	n    uint64
	objs [gcPoolCap]unsafe.Pointer
	rt   *rtt
	// class is an array pool's class, -1 for a struct pool.
	class int
	// each is what an object of the pool is charged; size how many the
	// next fill makes; made what the last fill made, in bytes, or what the
	// pool held at the table's last recount; epoch the budget's (gcEpoch)
	// before the last fill charged it.
	each, made int64
	size       int
	epoch      uint64
}

// arrayPools is an array type's pools by class: native code reads a
// class's pool at the class's word.
type arrayPools [len(arrayRooms)]*gcPool

// gcPoolSet is a table of pools by the module's type index, charged to
// budget b: an entry is a struct type's *gcPool or an array type's
// *arrayPools, nil until its first fill.
type gcPoolSet struct {
	b   *Budget
	tab []unsafe.Pointer
	// all are the table's pools; held is what their last fills made, in
	// bytes (gcPool.made).
	all  []*gcPool
	held int64
}

// gcPools is what levels and instances hold of a table: the cleanup that
// gives back what it holds runs once nothing holds it.
type gcPools struct{ set *gcPoolSet }

// giveBack gives back to the budget what the pools hold, never reachable.
func (s *gcPoolSet) giveBack() {
	for _, p := range s.all {
		if p.n > 0 {
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
	set := &gcPoolSet{b: inst.budget, tab: make([]unsafe.Pointer, max(len(inst.Module.Types), 1))}
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

// jitAlloc fills a pool of level lv's table, the innermost level's, giving
// it a table first if it has none, for an ExitAlloc of frame f whose
// Arg0 is what and whose Arg2 is length, at offset at: the type index in
// what's low word, and in its high word 0 for a struct type, the class of
// an array's block plus 1, AllocLength for the class of an array of
// length elements, or AllocMake and the reference slot an array of length
// elements is made into here. The objects are charged to the instance's
// budget, the native frames' references counted with the rest.
func (x *exec) jitAlloc(lv *jitLevel, what, length uint64, f *exitFrame, at uint32) {
	ti, hi := uint32(what), uint32(what>>32)
	class := int(hi) - 1
	switch {
	case hi&wjit.AllocMake != 0:
		a := newArray(lv.inst.Code.typeRTT(ti), uint32(length), lv.inst.budget, x, at)
		f.r[hi&^wjit.AllocMake] = ArrayRef(a)
		return
	case hi == wjit.AllocLength:
		class = arrayClass(int(length) * lv.inst.Code.typeRTT(ti).elemBytes())
	}
	if lv.pools == nil {
		x.levelPools(lv)
	}
	set := lv.pools.set
	p := set.pool(lv.inst.Code, ti, class)
	if p.n > 0 {
		return
	}
	set.held -= p.made
	n := min(p.size, max(int(gcPoolBytes/p.each), 1))
	if int64(n)*p.each > gcPoolsHeld-set.held {
		// The last fills may have been taken since: what the pools hold
		// is counted again.
		set.recount()
	}
	if room := (gcPoolsHeld - set.held) / p.each; int64(n) > room {
		n = int(max(room, 1))
	}
	b := lv.inst.budget
	epoch := b.gcEpoch()
	p.made = 0
	b.chargeGC(x, int64(n)*p.each)
	if p.class < 0 {
		for i := range n {
			p.objs[i] = newStruct(p.rt).p
		}
	} else {
		for i := range n {
			p.objs[i] = unsafe.Pointer(arrayBlock(p.rt, p.class))
		}
	}
	p.n, p.epoch, p.made = uint64(n), epoch, int64(n)*p.each
	set.held += p.made
	p.size = min(2*n, gcPoolCap)
}

// recount makes what the table's pools are taken to hold what they hold:
// each pool's made its objects', which native code has taken the rest of.
func (s *gcPoolSet) recount() {
	s.held = 0
	for _, p := range s.all {
		p.made = int64(p.n) * p.each
		s.held += p.made
	}
}

// pool returns the table's pool of type ti of code c, of the arrays of
// class for an array type and -1 for a struct type, made first if there
// is none.
func (s *gcPoolSet) pool(c *Code, ti uint32, class int) *gcPool {
	e := &s.tab[ti]
	if class < 0 {
		if *e == nil {
			rt := c.typeRTT(ti)
			p := &gcPool{rt: rt, class: -1, each: structSize(rt), size: gcPoolFirst}
			s.all = append(s.all, p)
			*e = unsafe.Pointer(p)
		}
		return (*gcPool)(*e)
	}
	if *e == nil {
		*e = unsafe.Pointer(new(arrayPools))
	}
	ap := (*arrayPools)(*e)
	if ap[class] == nil {
		rt := c.typeRTT(ti)
		ap[class] = &gcPool{rt: rt, class: class, each: int64(arrayHeader) + int64(arrayRooms[class]), size: gcPoolFirst}
		s.all = append(s.all, ap[class])
	}
	return ap[class]
}
