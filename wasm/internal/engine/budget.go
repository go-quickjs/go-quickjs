package engine

import (
	"errors"
	"runtime"
	"slices"
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
// A budget that counts itself is shared by instances that run on several
// goroutines at once, and a count must not read the globals and tables of
// an instance another goroutine is changing. So the goroutines using its
// instances -- a call, an instantiation, a host changing a global or a
// table -- each take a turn (Enter), and a count walks every instance only
// while no other goroutine holds one; else it walks the instances of the
// calls running on its own goroutine, and what only the others reach is
// counted at a later count, low by that as a count is already for what
// the frames of other goroutines hold.
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
	// busy is how many goroutines hold a turn (Enter); walking is set,
	// under mu, while a count walks every instance, which a turn begun
	// meanwhile waits out.
	busy    atomic.Int32
	walking atomic.Bool

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

// Enter takes a turn using the budget's instances -- running their code,
// instantiating one, or changing a global or a table of theirs from the
// host -- for a budget that counts itself, waiting out a count walking
// every instance; Exit gives it back. A call back into wasm that a host
// function makes with the context it was given is part of the call's turn
// and takes none of its own: a host that hands that context to another
// goroutine, which calls into wasm while the call runs, runs it outside
// any turn.
func (b *Budget) Enter() {
	if b == nil || b.outer != nil {
		return
	}
	b.busy.Add(1)
	for b.walking.Load() {
		b.mu.Lock()
		b.mu.Unlock()
	}
}

// Exit gives back a turn Enter took.
func (b *Budget) Exit() {
	if b != nil && b.outer == nil {
		b.busy.Add(-1)
	}
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
	// The object being made is not reachable yet, so the count does not
	// see it: it must fit beside what is, and it is charged again after.
	if !b.recount(x, n) {
		panic(hostError{ErrMemoryLimit})
	}
	b.gcNew.Add(n)
}

// tryChargeGC counts a GC object of n bytes as chargeGC does where that
// needs no count of what is reachable and no charge of the outer meter,
// reporting whether it counted it: else nothing is counted.
func (b *Budget) tryChargeGC(n int64) bool {
	if b == nil {
		return true
	}
	if b.outer != nil {
		for {
			pending := b.gcNew.Load()
			if pending+n >= outerChunk {
				return false
			}
			if b.gcNew.CompareAndSwap(pending, pending+n) {
				return true
			}
		}
	}
	newBytes := b.gcNew.Add(n)
	if newBytes+b.gcLive.Load()+b.fixed.Load() <= b.limit || newBytes < b.limit/16 {
		return true
	}
	b.gcNew.Add(-n)
	return false
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
//
// While another goroutine holds a turn (Enter) no instance it may be
// changing is read: the count walks the instances of the calls running on
// x alone, and each other instance counts what it held at the last count
// that walked it (Instance.budgetLive), an estimate of what it holds now.
func (b *Budget) recount(x *exec, need int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	// The count's own goroutine holds one turn. walking is set before busy
	// is read, and a turn sets busy before it reads walking: a turn begun
	// as the count decides either is seen by it or waits for it.
	b.walking.Store(true)
	all := b.busy.Load() <= 1
	if all {
		defer b.walking.Store(false)
	} else {
		b.walking.Store(false)
	}
	seen := map[unsafe.Pointer]struct{}{}
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
	// drain counts what the references visited so far reach.
	drain := func() int64 {
		var n int64
		for len(work) > 0 {
			r := work[len(work)-1]
			work = work[:len(work)-1]
			n += objectSize(r, visit)
		}
		return n
	}
	// mine are the instances of the calls running on x, which a count
	// walks whoever else holds a turn.
	var mine []*Instance
	if x != nil && !all {
		for _, c := range x.calls {
			if c.fn != nil && c.fn.Inst != nil && c.fn.Inst.budget == b && !slices.Contains(mine, c.fn.Inst) {
				mine = append(mine, c.fn.Inst)
			}
		}
	}
	var live int64
	kept := b.insts[:0]
	for _, w := range b.insts {
		inst := w.Value()
		if inst == nil {
			continue
		}
		kept = append(kept, w)
		if all || slices.Contains(mine, inst) {
			// Each object is the first walked instance's to reach it.
			inst.visitRoots(visit)
			inst.budgetLive = drain()
		}
		live += inst.budgetLive
	}
	clear(b.insts[len(kept):])
	b.insts = kept
	if x != nil {
		x.visitFrames(visit)
		live += drain()
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
// its globals, its tables and its element segments. An instance being
// instantiated has those made so far: a global's or a table's initializer
// that allocates may count what is reachable before the rest exist.
func (inst *Instance) visitRoots(visit func(Ref)) {
	for _, g := range inst.Globals {
		if g != nil {
			visit(g.Ref)
		}
	}
	for _, t := range inst.Tables {
		if t == nil {
			continue
		}
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

// reserveCharge charges b n bytes for an object yet to be made, so that
// one that does not fit is refused before it is allocated, returning the
// charge, nil for no budget, and whether they fit. The object made keeps
// the charge (keepCharge); one that cannot be made gives it back (drop).
func reserveCharge(b *Budget, n int64) (*charge, bool) {
	if b == nil {
		return nil, true
	}
	if !b.reserve(n) {
		return nil, false
	}
	c := &charge{b: b}
	c.n.Store(n)
	return c, true
}

// keepCharge gives c back when owner is collected.
func keepCharge[T any](c *charge, owner *T) {
	if c != nil {
		runtime.AddCleanup(owner, func(c *charge) { c.b.release(c.n.Swap(0)) }, c)
	}
}

// drop gives back the charge of an object that was not made.
func (c *charge) drop() {
	if c != nil {
		c.b.release(c.n.Swap(0))
	}
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
