package engine

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
	"weak"
)

// Memory limits.
//
// A Budget bounds the memory a group of instances holds: their linear
// memories and tables, and the GC objects they can reach. Memories and
// tables only grow, and are charged exactly, as they are made and grown; a
// grow past the budget fails as a grow that cannot be had does, returning
// -1. GC objects are charged as they are allocated, and when what has been
// charged would pass the limit, the objects still reachable from the
// budget's roots are counted again, since the collector may have freed the
// rest: if they and what is wanted are over the limit, execution stops
// with ErrMemoryLimit, which no wasm handler catches.
//
// A budget may instead leave the counting to an outer meter -- a
// JavaScript runtime's, which measures its whole heap, wasm's included --
// and only charge it. The meter's walk asks the wasm objects it reaches what
// they hold (MemoryFootprint), and the budget itself for what the frames of
// the calls running hold.

// ErrMemoryLimit stops execution that needs more memory than its budget
// allows.
var ErrMemoryLimit = errors.New("wasm: memory limit exceeded")

// Outer is a meter a budget charges in place of counting itself.
type Outer interface {
	// TryReserve reports whether n more bytes of memories or tables fit,
	// and if so counts them.
	TryReserve(n int64) bool
	// Charge counts n bytes of GC objects, returning the error that stops
	// execution if they do not fit.
	Charge(n int64) error
	// Exceeded returns the error that stops execution when what was asked
	// for does not fit.
	Exceeded() error
}

// outerChunk is how much a budget charging an outer meter lets GC objects
// take before it charges them.
const outerChunk = 64 << 10

// Budget bounds the memory a group of instances may hold.
type Budget struct {
	limit int64
	outer Outer

	// fixed is what the memories and tables hold; gcLive what reachable GC
	// objects held at the last count, and gcNew what has been allocated
	// since.
	fixed, gcLive, gcNew atomic.Int64
	// counts is how many counts of the reachable GC objects have been
	// made: a charge made since the last is still in gcNew.
	counts atomic.Uint64

	mu sync.Mutex
	// insts are the instances charged to a budget that counts itself, whose
	// globals, tables and segments are the roots of its GC objects. They are
	// held weakly: an instance no longer used is no root.
	insts []weak.Pointer[Instance]
	// active are the execs running calls of the budget's instances, for a
	// budget charging an outer meter: the roots of what their frames hold.
	active []*exec
}

// NewBudget returns a budget of limit bytes that counts itself.
func NewBudget(limit int64) *Budget { return &Budget{limit: limit} }

// track makes inst's references roots of the budget's GC objects.
func (b *Budget) track(inst *Instance) {
	if b == nil || b.outer != nil {
		return
	}
	b.mu.Lock()
	b.insts = append(b.insts, weak.Make(inst))
	b.mu.Unlock()
}

// NewOuterBudget returns a budget that charges outer.
func NewOuterBudget(outer Outer) *Budget { return &Budget{outer: outer} }

// Used returns what the budget counts as held: its memories and tables, and
// its GC objects as of the last count plus what has been allocated since.
func (b *Budget) Used() int64 { return b.fixed.Load() + b.gcLive.Load() + b.gcNew.Load() }

// reserve takes n bytes for a memory or a table, reporting whether they fit.
func (b *Budget) reserve(n int64) bool {
	if b == nil || n <= 0 {
		return true
	}
	if b.outer != nil {
		return b.outer.TryReserve(n)
	}
	for {
		used := b.fixed.Load()
		if used+n+b.gcLive.Load()+b.gcNew.Load() > b.limit {
			// GC objects since freed may make room.
			if b.gcNew.Load() == 0 || !b.recount(nil, n) {
				return false
			}
			continue
		}
		if b.fixed.CompareAndSwap(used, used+n) {
			return true
		}
	}
}

// exceeded returns the error that stops execution when what was asked for
// does not fit.
func (b *Budget) exceeded() error {
	if b != nil && b.outer != nil {
		return b.outer.Exceeded()
	}
	return ErrMemoryLimit
}

// release gives back n bytes of a memory or a table no longer held.
func (b *Budget) release(n int64) {
	if b != nil && b.outer == nil && n > 0 {
		b.fixed.Add(-n)
	}
}

// chargeGC counts a GC object of n bytes that a call running on x makes,
// stopping execution if it does not fit even once what is reachable has
// been counted again.
func (b *Budget) chargeGC(x *exec, n int64) {
	if b == nil {
		return
	}
	if b.outer != nil {
		if pending := b.gcNew.Add(n); pending >= outerChunk {
			b.gcNew.Add(-pending)
			if err := b.outer.Charge(pending); err != nil {
				panic(hostError{err})
			}
		}
		return
	}
	// A count walks everything reachable, so one is made only once a
	// sixteenth of the limit has been allocated since the last: what is held
	// may pass the limit by that much before it is found.
	newBytes := b.gcNew.Add(n)
	if newBytes+b.gcLive.Load()+b.fixed.Load() <= b.limit || newBytes < b.limit/16 {
		return
	}
	if !b.recount(x, 0) {
		panic(hostError{ErrMemoryLimit})
	}
}

// gcEpoch returns what giveBackGC is given of a charge made after it.
func (b *Budget) gcEpoch() uint64 {
	if b == nil {
		return 0
	}
	return b.counts.Load()
}

// giveBackGC gives back n bytes of GC objects charged, and never made
// reachable, after epoch (gcEpoch): what a count since has not counted.
// A budget charging an outer meter takes them off what it has yet to
// charge.
func (b *Budget) giveBackGC(epoch uint64, n int64) {
	if b == nil || n <= 0 {
		return
	}
	if b.outer != nil {
		b.gcNew.Add(-n)
		return
	}
	b.mu.Lock()
	if b.counts.Load() == epoch {
		b.gcNew.Add(-n)
	}
	b.mu.Unlock()
}

// recount counts the GC objects reachable from the budget's roots and from
// the frames of the call running on x, if any, reporting whether they, the
// memories and tables, and need more bytes fit. Calls of the budget's
// instances running on other goroutines are not seen: what only their
// frames hold is counted at their own next count, so the total is an
// estimate, low by that.
func (b *Budget) recount(x *exec, need int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[unsafe.Pointer]struct{}{}
	var live int64
	var work []Ref
	visit := func(r Ref) {
		if r.p == nil || r.IsI31() {
			return
		}
		switch r.Kind() {
		case ObjStruct, ObjArray, ObjExn:
		default:
			return
		}
		if _, ok := seen[r.p]; ok {
			return
		}
		seen[r.p] = struct{}{}
		work = append(work, r)
	}
	kept := b.insts[:0]
	for _, w := range b.insts {
		if inst := w.Value(); inst != nil {
			inst.visitRoots(visit)
			kept = append(kept, w)
		}
	}
	clear(b.insts[len(kept):])
	b.insts = kept
	if x != nil {
		x.visitFrames(visit)
	}
	for len(work) > 0 {
		r := work[len(work)-1]
		work = work[:len(work)-1]
		live += objectSize(r, visit)
	}
	b.gcLive.Store(live)
	b.gcNew.Store(0)
	b.counts.Add(1)
	return b.fixed.Load()+live+need <= b.limit
}

// enter and leave bracket a call running on x, for a budget charging an
// outer meter, so that what its frames hold is counted.
func (b *Budget) enter(x *exec) {
	b.mu.Lock()
	b.active = append(b.active, x)
	b.mu.Unlock()
}

func (b *Budget) leave(x *exec) {
	b.mu.Lock()
	for i := len(b.active) - 1; i >= 0; i-- {
		if b.active[i] == x {
			last := len(b.active) - 1
			copy(b.active[i:], b.active[i+1:])
			b.active[last] = nil
			b.active = b.active[:last]
			break
		}
	}
	b.mu.Unlock()
}

// objectSize returns the bytes GC object r takes, visiting the references
// it holds.
func objectSize(r Ref, visit func(Ref)) int64 {
	switch r.Kind() {
	case ObjStruct:
		cells := structCells(r.p)
		for _, c := range cells[1:] {
			visit(c)
		}
		return int64(len(cells)) * int64(unsafe.Sizeof(Ref{}))
	case ObjArray:
		a := asArray(r)
		if a.hdr.rtt.refElems {
			for _, e := range a.refs() {
				visit(e)
			}
		}
		return arraySize(a.hdr.rtt, uint32(a.n))
	case ObjExn:
		e := r.Exn()
		for _, v := range e.Vals {
			visit(v.R)
		}
		return int64(unsafe.Sizeof(Exception{})) + int64(len(e.Vals))*int64(unsafe.Sizeof(Val{}))
	}
	return 0
}

// structSize and arraySize are what a new struct or array is charged.
func structSize(t *rtt) int64 { return int64(t.ncells) * int64(unsafe.Sizeof(Ref{})) }

func arraySize(t *rtt, n uint32) int64 {
	return int64(arrayHeader) + int64(n)*int64(t.elemBytes())
}

// visitFrames visits the references in the frames of the calls running on
// x: the reference slots of its stacks up to where it has taken them.
func (x *exec) visitFrames(visit func(Ref)) {
	for i, c := range x.rchunks {
		end := len(c)
		if i == x.rc {
			end = x.rsp
		} else if i > x.rc {
			break
		}
		for _, r := range c[:end] {
			visit(r)
		}
	}
	for _, v := range x.vals {
		visit(v.R)
	}
	for _, e := range x.caught {
		visit(ExnRef(e))
	}
}

// visitRoots visits the references an instance holds outside GC objects:
// its globals, its tables and its element segments.
func (inst *Instance) visitRoots(visit func(Ref)) {
	for _, g := range inst.Globals {
		visit(g.Ref)
	}
	for _, t := range inst.Tables {
		for _, r := range t.Elems {
			visit(r)
		}
	}
	for _, seg := range inst.elems {
		for _, r := range seg {
			visit(r)
		}
	}
}

// charge is what a memory or a table holds of a budget, given back when it
// is collected. It is an object of its own so that the cleanup that gives
// it back does not keep the memory or the table alive.
type charge struct {
	b *Budget
	n atomic.Int64
}

// newCharge charges owner's n bytes to b, returning the charge it is to keep,
// nil for no budget, and whether they fit.
func newCharge[T any](owner *T, b *Budget, n int64) (*charge, bool) {
	if b == nil {
		return nil, true
	}
	if !b.reserve(n) {
		return nil, false
	}
	c := &charge{b: b}
	c.n.Store(n)
	runtime.AddCleanup(owner, func(c *charge) { c.b.release(c.n.Swap(0)) }, c)
	return c, true
}

// take charges n more bytes, reporting whether they fit; a nil charge takes
// anything.
func (c *charge) take(n int64) bool {
	if c == nil {
		return true
	}
	if !c.b.reserve(n) {
		return false
	}
	c.n.Add(n)
	return true
}

// giveBack returns n bytes taken that were not used after all.
func (c *charge) giveBack(n int64) {
	if c != nil {
		c.n.Add(-n)
		c.b.release(n)
	}
}
