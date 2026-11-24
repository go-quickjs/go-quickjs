package engine

import (
	"unsafe"
)

// The native handlers' allocations. struct.new, struct.new_default,
// array.new, array.new_default and array.new_fixed of the types they take
// (regGC.natTi) take the object from a pool of the exec's table for the
// instance (execPools), as the JIT's code takes from its level's -- the
// same tables (gcpool.go) -- and store its values by the record's natVals.
// An empty pool, a frame without the table, and an array past the pools go
// to the allocation stub, wasmNativeAlloc, which calls nativeAllocGo from a
// frame of its own, as wasmNativeStoreRef calls its store: it fills the
// pool, or makes the array, filled, into its slot, without leaving native
// code. What it cannot do so -- a trap, a charge the budget holds only once
// what is reachable is counted again -- it leaves to the instruction's Go
// handler, the exit, which allocates as the Go table does.

// The allocation stub's requests (nativeAllocGo's req, the record's index
// above): a struct's pool filled, an array's of the class of its length,
// and an array past the pools made into the record's slot and filled.
const (
	allocStruct = 1 + iota
	allocArray
	allocMake
)

// What nativeAllocGo answers: the code takes the exit, takes from the
// pool again, or goes on past the instruction.
const (
	allocExit = iota
	allocRetry
	allocDone
)

// poolsAddr returns the address of the exec's table of pools for inst, or
// 0 where it has none yet.
func (x *exec) poolsAddr(inst *Instance) uintptr {
	for _, p := range x.pools {
		if p.inst == inst {
			return uintptr(unsafe.Pointer(&p.p.set.tab[0]))
		}
	}
	return 0
}

// poolsFor returns the exec's table of pools for inst, taking one of the
// instance's first if it has none.
func (x *exec) poolsFor(inst *Instance) *gcPoolSet {
	for _, p := range x.pools {
		if p.inst == inst {
			return p.p.set
		}
	}
	p := inst.takePools()
	x.pools = append(x.pools, execPools{inst, p})
	return p.set
}

// dropPools gives back the exec's tables of pools to their instances.
func (x *exec) dropPools() {
	for i, p := range x.pools {
		p.inst.putPools(p.p)
		x.pools[i] = execPools{}
	}
	x.pools = x.pools[:0]
	x.natPools = 0
}

// nativeAllocGo does what the allocation stub asks for the instruction of
// record req>>8 of the frame of context c, answering what the code does
// next (allocExit ...).
func nativeAllocGo(c *nativeCtx, req uintptr) uintptr {
	// The context is the frame's own.
	f := (*frame)(unsafe.Add(unsafe.Pointer(c), -int(unsafe.Offsetof(frame{}.nctx))))
	g := &f.rc.gc[req>>8]
	x, inst := f.x, f.inst
	if t := inst.test; t != nil && t.allocHook != nil {
		t.allocHook()
	}
	set := x.poolsFor(inst)
	x.natPools = uintptr(unsafe.Pointer(&set.tab[0]))
	ti := g.natTi - 1
	l := unsafe.Pointer(&f.l[0])
	var n uint32
	if req&0xff != allocStruct {
		n = uint32(len(g.natVals))
		if len(g.ops) > 0 {
			n = uint32(num(l, g.ops[len(g.ops)-1]))
		}
	}
	switch req & 0xff {
	case allocStruct:
		if p := set.pool(inst.Code, ti, -1); p.n > 0 || set.fill(p, x, true) {
			return allocRetry
		}
	case allocArray:
		bytes := int(n) << g.natShift
		if bytes > arrayBlockBytes {
			return allocExit
		}
		if p := set.pool(inst.Code, ti, arrayClass(bytes)); p.n > 0 || set.fill(p, x, true) {
			return allocRetry
		}
	case allocMake:
		size := g.rt.elemSize
		if g.rt.refElems {
			size = 8
		}
		if n > uint32(maxArrayLength(size)) || !inst.budget.tryChargeGC(arraySize(g.rt, n)) {
			return allocExit
		}
		a := makeArray(g.rt, int(n))
		if len(g.natVals) > 0 {
			v := getVal(l, f, g.k, g.ops[0])
			for i := 0; i < a.n; i++ {
				arraySet(a, g.et, i, v)
			}
		}
		f.r[g.dst] = ArrayRef(a)
		return allocDone
	}
	return allocExit
}
