package engine

import (
	"runtime"
	"sync"
	"unsafe"
)

// The pools of new GC objects native code takes from, the JIT's and the
// interpreter's native handlers' alike (jit_gc.go, native_gc.go).

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

// gcPoolCache is an instance's tables of pools no level or exec holds.
type gcPoolCache struct {
	mu   sync.Mutex
	free []*gcPools
}

// gcPoolsKept is how many tables an instance keeps for its levels and execs.
const gcPoolsKept = 4

// takePools returns a table of the instance's for a level.
func (inst *Instance) takePools() *gcPools {
	jp := &inst.pools
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
	jp := &inst.pools
	jp.mu.Lock()
	if len(jp.free) < gcPoolsKept {
		jp.free = append(jp.free, g)
	}
	jp.mu.Unlock()
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

// fill fills empty pool p of the table, charging its budget for the
// objects at once, for a call running on x, reporting whether it did: where
// try, it does nothing the budget holds only once what is reachable is
// counted again, which is left to a caller whose frames Go sees.
func (s *gcPoolSet) fill(p *gcPool, x *exec, try bool) bool {
	b := s.b
	s.held -= p.made
	p.made = 0
	n := min(p.size, max(int(gcPoolBytes/p.each), 1))
	if int64(n)*p.each > gcPoolsHeld-s.held {
		// The last fills may have been taken since: what the pools hold
		// is counted again.
		s.recount()
	}
	if room := (gcPoolsHeld - s.held) / p.each; int64(n) > room {
		n = int(max(room, 1))
	}
	epoch := b.gcEpoch()
	if try {
		if !b.tryChargeGC(int64(n) * p.each) {
			return false
		}
	} else {
		b.chargeGC(x, int64(n)*p.each)
	}
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
	s.held += p.made
	p.size = min(2*n, gcPoolCap)
	return true
}
