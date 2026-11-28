//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"
	"reflect"
	"sort"
	"sync"
	"time"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/spanclock"
)

// jitReturned is jitReturnedToHost: what the JIT does as r's turn ends
// with no frame running. The cache looks to flush (flush): an idle
// runtime, which takes no back edge's check nor compiles, had kept all its
// code and what that holds, and the process's idle workspaces
// (jit.DropIdleWorkspaces), until it ran again; the code parked a flush
// age goes (regAgeParked); and, once the program has run long enough since
// the last parking to pay for parking some of its code (parkDue), the
// cache parks that much of it, the least recently used first
// (jitRegParkSome). The looks are the flush's own and a comparison of
// times, so most of a host's short calls pay two clocks' reads.
func jitReturned(r *Runtime) {
	s := r.jit
	if s == nil {
		return
	}
	if s.unavailable {
		return
	}
	s.flush()
	s.regAgeParked(s.flushNow(), r.jitStress.park)
	if r.jitStress.park {
		// QJS_JIT_STRESS's park: all the code, at every return.
		s.jitRegPark()
	} else if n := s.parkDue(); n > 0 {
		s.jitRegParkSome(n)
	}
}

// jitParkPays is how many times what parking costs -- a parking's walk of
// the code it parks, the unparkings of that code its uses take after, and
// the compiles again of the code it lost, whose objects died while it
// was parked -- the program must have run since the last parking before
// it parks again, for each entry it parks (parkDue, parkEach). That cost
// is what parking measures; what it costs besides -- native callers
// leaving for Go to unpark a callee, the pools filled anew, the learned
// state copied -- came to some one and a half times as much again where a
// host's calls return every few milliseconds (the fresh suite's runs again
// of a kept runtime's code, 3 to 5% slower at 50 against parking once a
// flush age, 1 to 2% at 200). So some 1 to 2% of the running goes to
// parking however often a host's calls return, and the code a turn
// compiled is parked at the returns that have paid for it, the least
// recently used first, not a flush age after the last parking, which a
// program's last turn may never reach: Octane's Typescript, run after a
// parking, had kept its code and through it some 190 MB of its heap after
// the run.
const jitParkPays = 200

// parkDue is how many entries of its code the cache may park now (none,
// 0): as many as the program's running since the last parking ended
// (parkRun) pays for, jitParkPays times what each of the last parking's
// entries cost (parkEach); all it holds where no parking has measured an
// entry yet. A turn too short to measure has run nothing, and a cache
// that holds no code has nothing to park.
func (s *jitState) parkDue() int {
	n := len(s.regCache)
	if n == 0 {
		return 0
	}
	ran := s.parkClock() - s.parkRun
	if ran <= 0 {
		return 0
	}
	if each := s.parkEach(); each > 0 {
		n = int(min(ran/(jitParkPays*each), time.Duration(n)))
	}
	return n
}

// parkEach is what parking an entry costs: what the last parking that
// parked code, the unparkings since and the compiles again of the code it
// lost cost (parkCost, measured by parkSpan), for each entry it parked
// (parkedLast); before any, what the looks that parked nothing cost,
// none at the first. A parking's walk, the unparkings after and the
// compiles again are each an entry's, so that a parking is paid for by
// what it parks: paced by the last parking's cost whatever it parked, a
// parking of one function's code had made one of seventeen due after a
// fiftieth of the running that paid for it -- Octane's DeltaBlue run
// once, 8 ms, parked its code at its end, inside its time, after
// Richards had parked one function's.
func (s *jitState) parkEach() time.Duration {
	return s.parkCost / time.Duration(max(s.parkedLast, 1))
}

// parkClock is parking's clock: the program's time (running); where a test
// set the cache's clocks, the time of their ticks (jitTestConfig's parkNow,
// flushNow and clocks), and held, none goes by.
func (s *jitState) parkClock() time.Duration {
	t := &s.rt.jitTest
	if f := t.parkNow; f != nil {
		return f()
	}
	if f := t.flushNow; f != nil {
		return time.Duration(f()) * jitEpochTime
	}
	if c := t.clocks; c != nil {
		if tick, ok := c(s); ok {
			return time.Duration(tick) * jitEpochTime
		}
	}
	if jitClocksHeld.Load() {
		return 0
	}
	return s.running()
}

// parkSpan is the clock a parking and an unparking measure their cost by
// (parkCost): one that resolves their tens to hundreds of microseconds
// (internal/spanclock), which the program's clock does not on Windows, where
// Go's clock moves only at the system timer's tick. Measured by that, a
// parking cost nothing in most of its measures, and every return a tick
// after the last parking parked again: in a fixed run of Octane, each
// benchmark's code at its end, a walk of 0.15 to 0.2 ms in a run of a few
// milliseconds. Where a test set
// the cache's clocks, or holds them, it is parking's clock (parkClock).
func (s *jitState) parkSpan() time.Duration {
	t := &s.rt.jitTest
	if t.parkNow != nil || t.flushNow != nil || t.clocks != nil || jitClocksHeld.Load() {
		return s.parkClock()
	}
	return spanclock.Now()
}

// Parking. Native code reads by address what it was compiled against --
// the callees its calls compare their callee with, the prototypes and
// shapes its checks compare, the closures it inlined, the cells and pools
// it reads -- and its entry holds them (keep), and what its function
// learned holds the callees and objects its feedback met (jitLearned).
// Held strongly, code compiled while a program ran keeps that program's
// objects alive for as long as the code lives: a library function's code
// a run's closure as its call's target, and through it the run's whole
// heap; and the cache, which holds the code by its function's key, holds
// it whether or not anything else reaches the function, the function's
// own closure among what its code holds. V8 holds such objects weakly in
// its code and its feedback, and deoptimizes code whose objects die at
// the collection that finds them dead. After Octane in one runtime the
// cache held 250 MB the interpreter's heap did not -- zlib's code its 128
// MB heap, Typescript's library code the compiler's state of a run.
//
// Go gives no point to invalidate code at between an object's death and
// its memory's reuse: a cleanup (runtime.AddCleanup) or a finalizer's
// queue runs after the collector has found the object dead, on another
// goroutine, by which time a sweep may have given its memory to a new
// object, which code comparing against the old address would take for
// the object it was compiled for -- a guard passed for the wrong callee,
// a shape check for the wrong layout. So the code holds its objects
// strongly while it may run, and weakly only while it cannot: parked.
//
// As the runtime returns to its host with no frame running, once the
// program has run 200 times what parking costs since it last did
// (jitReturned, jitParkPays), the cache parks the code no run, native
// call or compile uses: it takes the entry out of the
// cache (regParked), clears its link, so that native callers call Go, and
// marks it not cached, so that its templates' calls take the path for
// code let go of; and it turns every pointer the entry and its function's
// learned state hold to a closure, an object or a shape into a weak
// pointer, the slot cleared (jitParked), its pools emptied of the objects
// made for them, and the learned state its function's other readers see
// one that holds no object (regForgotten). A collection meanwhile
// collects what only the parked code held. The first use after --
// a call Go makes of the function, a native caller's call of it, which
// leaves for Go, a loop's check, a compile asked for it -- unparks it
// (jitRegUnpark): every weak pointer's Value, which is nil from before
// its object's memory can be reused, is read; all alive, the slots are
// written back and the code goes back into the cache, its link set, with
// no compile; any dead, the code is let go of as the flush lets go of
// code, and the function warms up again. So no comparison against an
// address an object no longer has is ever made. Code parked a flush age
// and not used is let go of at the next return (regAgeParked), however
// many parkings came between. Active code pays nothing: its entries are tested as before, and a parked entry
// takes paths code let go of took already, once.

// jitParked is what a parked entry holds weakly (jitRegPark): each slot
// it cleared, with a weak pointer to what the slot held; and the learned
// state of its function, its slots among these, while the function's life
// holds a copy that holds no object (regForgotten).
type jitParked struct {
	// at is the flush's tick the entry was parked at (regAgeParked).
	at      uint64
	slots   []jitParkSlot
	learned *jitLearned
	// pools are the object pools the entry's code and state take from,
	// each parked once for all the entries that share it (jitState's
	// parkedPools): an inlined callee's literal is made from the callee's
	// pool. Whichever of them is first put back puts the pool back.
	pools []*abi.ObjectPool
	// s is the state parking the entry; seen are the structures walked,
	// so that each is walked once; weaks, the parking's, the weak pointer
	// made for each object, so that each is made once a parking however
	// many slots hold it (weak.Make finds an object's handle under a lock,
	// by a search of its span's specials).
	s     *jitState
	seen  map[unsafe.Pointer]bool
	weaks map[unsafe.Pointer]weak.Pointer[byte]
}

// jitParkSlot is a pointer slot a parked entry cleared, and a weak pointer
// to what it held.
type jitParkSlot struct {
	at *unsafe.Pointer
	w  weak.Pointer[byte]
}

// hold clears the pointer slot at, keeping a weak pointer to what it held;
// a slot holding one of the package's static objects (jitParkStatic) stays
// as it is.
func (p *jitParked) hold(at *unsafe.Pointer) {
	x := *at
	if x == nil || jitParkStatic(x) {
		return
	}
	w, ok := p.weaks[x]
	if !ok {
		w = weak.Make((*byte)(x))
		if p.weaks != nil {
			p.weaks[x] = w
		}
	}
	p.slots = append(p.slots, jitParkSlot{at: at, w: w})
	*at = nil
}

// jitParkStatic reports whether x is one of the objects of jitParkWeak's
// types that the package makes as its variables' literals: the caches'
// sentinels, and the holder of what is found nowhere. The linker puts such
// an object in the binary's data, not the heap, and a weak pointer cannot
// be made to it -- Go 1.24's weak.Make throws -- nor is one needed, since
// it never dies: a slot holding one is held strongly, as it is.
// TestJITParkStaticsListed holds the list to the package's literals.
func jitParkStatic(x unsafe.Pointer) bool {
	return x == unsafe.Pointer(noShape) || x == unsafe.Pointer(dictShape) || x == unsafe.Pointer(setterNext) ||
		x == unsafe.Pointer(absentHolder) || x == unsafe.Pointer(absentHolder.shape)
}

// The types whose pointers a parked entry holds weakly: what holds a
// program's objects, allocated in the heap but for the package's own
// statics (jitParkStatic) -- a pointer to any other type may name a
// package's variable, which a weak pointer cannot be made to.
var (
	jitParkWeak = map[reflect.Type]bool{
		reflect.TypeFor[*Object]():  true,
		reflect.TypeFor[*closure](): true,
		reflect.TypeFor[*shape]():   true,
		// A function's bytecode, a typed function's twin's among them,
		// whose VMCode is its typed code (asmTwin), which holds its heap.
		reflect.TypeFor[*bytecode.Function](): true,
	}
	// jitParkInto are the types of the structures an entry and its
	// learned state own, which no other holds, walked into: anything
	// else they point to, shared or not, is left as it is.
	jitParkInto = map[reflect.Type]bool{
		reflect.TypeFor[*jitFeedback]():     true,
		reflect.TypeFor[*jitRegFeedback]():  true,
		reflect.TypeFor[*jitForwardFrame](): true,
	}
	jitParkValue = reflect.TypeFor[Value]()
	jitParkPool  = reflect.TypeFor[*abi.ObjectPool]()
	jitParkCells = reflect.TypeFor[[]*Value]()
)

// jitParkPlan is how a parking walks a value of one type: what of the
// value's words it holds weakly (jitParkWeak), the pools it empties and
// the structures the entry owns it walks into, each at its offset in the
// value, made once from the type's fields (jitParkPlansMake), so that a
// parking reads no type's description and looks up no type as it walks.
// Each type's walk is reflect's walk of it, field by field: a pointer of
// jitParkWeak's types held, a pool emptied, a pointer to a structure the
// entry owns walked into once (jitParkInto); a Value of an object's tag
// held; a struct of this package's walked field by field, another
// package's not; a slice or an array walked element by element, and the
// keep's cells ([]*Value) cell by cell; anything else left as it is. A
// type that holds none of these has no plan, so that a slice of scalars'
// structs is not walked. TestJITParkPlanMatchesWalk holds the plans to
// reflect's walk of what the suites' code parks.
type jitParkPlan struct {
	ops []jitParkOp
}

// jitParkOp is a step of a plan, at off in the value: a pointer held, a
// pool, a Value, the cells, or a structure walked into, a slice or an
// array of elem's values of size bytes, n of the array's.
type jitParkOp struct {
	kind uint8
	off  uintptr
	elem *jitParkPlan
	size uintptr
	n    int
}

// The kinds of a plan's steps, those with an elem's plan from
// jitParkIntoOp on.
const (
	jitParkHold = iota
	jitParkPoolOp
	jitParkValueOp
	jitParkCellsOp
	jitParkIntoOp
	jitParkSliceOp
	jitParkArrayOp
)

var (
	jitParkPlansOnce sync.Once
	// jitParkPlans are the plans of the types an entry's walk begins at:
	// its keep, its calls' targets, its function's learned state, and a
	// pool's source.
	jitParkPlans struct {
		keep, targets, learned, source *jitParkPlan
	}
)

// jitParkPlansMake makes the plans parking walks by (jitParkPlans), once
// for the process, at its first parking (jitParkPlansOnce).
func jitParkPlansMake() {
	b := jitParkBuild{}
	p := &jitParkPlans
	p.keep, p.targets = b.plan(reflect.TypeFor[jitFeedback]()), b.plan(reflect.TypeFor[[]jitTarget]())
	p.learned, p.source = b.plan(reflect.TypeFor[jitLearned]()), b.plan(reflect.TypeFor[jitPoolSource]())
}

// jitParkBuild is the plans made so far, by type.
type jitParkBuild map[reflect.Type]*jitParkPlan

// plan is t's plan, nil where a value of t holds nothing a parking
// weakens. A type's plan is listed before its fields' are made, so that a
// structure holding a pointer to its own type takes the plan being made.
func (b jitParkBuild) plan(t reflect.Type) *jitParkPlan {
	if pl, ok := b[t]; ok {
		return pl
	}
	pl := &jitParkPlan{}
	b[t] = pl
	b.ops(pl, t, 0)
	if len(pl.ops) == 0 {
		b[t], pl = nil, nil
	}
	return pl
}

// ops appends to pl the steps of a value of type t at off.
func (b jitParkBuild) ops(pl *jitParkPlan, t reflect.Type, off uintptr) {
	op := jitParkOp{off: off}
	switch t.Kind() {
	case reflect.Pointer:
		switch {
		case jitParkWeak[t]:
			op.kind = jitParkHold
		case t == jitParkPool:
			op.kind = jitParkPoolOp
		case jitParkInto[t]:
			op.kind, op.elem = jitParkIntoOp, b.plan(t.Elem())
		default:
			return
		}
	case reflect.Struct:
		if t != jitParkValue {
			if t.PkgPath() == jitParkValue.PkgPath() {
				for i := range t.NumField() {
					f := t.Field(i)
					b.ops(pl, f.Type, off+f.Offset)
				}
			}
			return
		}
		op.kind = jitParkValueOp
	case reflect.Slice:
		op.kind = jitParkCellsOp
		if t != jitParkCells {
			op.kind, op.elem, op.size = jitParkSliceOp, b.plan(t.Elem()), t.Elem().Size()
		}
	case reflect.Array:
		op.kind, op.elem, op.size, op.n = jitParkArrayOp, b.plan(t.Elem()), t.Elem().Size(), t.Len()
	default:
		return
	}
	if op.kind < jitParkIntoOp || op.elem != nil {
		pl.ops = append(pl.ops, op)
	}
}

// walk holds weakly what the value at x, of pl's type, holds of the
// program (jitParkWeak), walking into the structures the entry owns.
func (p *jitParked) walk(pl *jitParkPlan, x unsafe.Pointer) {
	for i := range pl.ops {
		op := &pl.ops[i]
		at := unsafe.Add(x, op.off)
		switch op.kind {
		case jitParkHold:
			p.hold((*unsafe.Pointer)(at))
		case jitParkPoolOp:
			p.pool(*(**abi.ObjectPool)(at))
		case jitParkIntoOp:
			if y := *(*unsafe.Pointer)(at); y != nil && p.once(y) {
				p.walk(op.elem, y)
			}
		case jitParkValueOp:
			p.value((*Value)(at))
		case jitParkCellsOp:
			// The cells the code reads its constants from, the JIT's own
			// (keep's cells): what each holds.
			for _, c := range *(*[]*Value)(at) {
				if c != nil {
					p.value(c)
				}
			}
		case jitParkSliceOp:
			h := (*jitParkSliceHeader)(at)
			for j := range h.len {
				p.walk(op.elem, unsafe.Add(h.data, uintptr(j)*op.size))
			}
		case jitParkArrayOp:
			for j := range op.n {
				p.walk(op.elem, unsafe.Add(at, uintptr(j)*op.size))
			}
		}
	}
}

// jitParkSliceHeader is a slice's words.
type jitParkSliceHeader struct {
	data     unsafe.Pointer
	len, cap int
}

// value holds weakly what x holds: a value of an object's tag holds the
// object, an accessor pair or a template, each the heap's but for the
// statics hold leaves as they are.
func (p *jitParked) value(x *Value) {
	if b := math.Float64bits(x.num); b == objectBits || b == accessorBits || b == templateBits {
		p.hold(&x.ref)
	}
}

// once reports whether x is walked for the first time.
func (p *jitParked) once(x unsafe.Pointer) bool {
	if p.seen[x] {
		return false
	}
	if p.seen == nil {
		p.seen = map[unsafe.Pointer]bool{}
	}
	p.seen[x] = true
	return true
}

// pool empties pool of the objects made for it, which only it holds, and
// holds weakly what its source holds (jitPoolSource): the code has Go
// fill it again when it next runs out (ssa's GoRefill), from its source,
// as for a pool any run empties.
func (p *jitParked) pool(pool *abi.ObjectPool) {
	if pool == nil || !p.once(unsafe.Pointer(pool)) {
		return
	}
	p.pools = append(p.pools, pool)
	s := p.s
	if s.parkedPools[pool] != nil {
		return
	}
	clear(pool.Objects[:min(pool.Count, uint64(len(pool.Objects)))])
	pool.Count, pool.Proto = 0, nil
	rp := &jitParked{weaks: p.weaks}
	if src := (*jitPoolSource)(pool.Source); src != nil {
		rp.walk(jitParkPlans.source, unsafe.Pointer(src))
	}
	rp.weaks = nil
	if s.parkedPools == nil {
		s.parkedPools = map[*abi.ObjectPool]*jitParked{}
	}
	s.parkedPools[pool] = rp
}

// poolsAlive reports whether what the parked pools of p hold is alive.
func (p *jitParked) poolsAlive() bool {
	for _, pool := range p.pools {
		if rp := p.s.parkedPools[pool]; rp != nil && !rp.alive() {
			return false
		}
	}
	return true
}

// restorePools puts back the pools of p that are parked still, nil for
// what died.
func (p *jitParked) restorePools() {
	for _, pool := range p.pools {
		if rp := p.s.parkedPools[pool]; rp != nil {
			rp.restore()
			delete(p.s.parkedPools, pool)
		}
	}
}

// alive reports whether everything p holds weakly is alive still.
func (p *jitParked) alive() bool {
	for _, s := range p.slots {
		if s.w.Value() == nil {
			return false
		}
	}
	return true
}

// restore writes back each slot's pointer that is alive still, nil for one
// that is not.
func (p *jitParked) restore() {
	for _, s := range p.slots {
		*s.at = unsafe.Pointer(s.w.Value())
	}
}

// regForgotten is a copy of o that holds no object: what regForget keeps.
func regForgotten(o *jitLearned) *jitLearned {
	return &jitLearned{reg: o.reg, regLink: o.regLink,
		failed: o.failed, failedEntries: o.failedEntries, siteDeopts: o.siteDeopts, bigints: o.bigints, bigwide: o.bigwide, resumed: o.resumed,
		typedReads: o.typedReads, stringReads: o.stringReads, keyedReads: o.keyedReads, keyedWrites: o.keyedWrites,
		regHints: o.regHints}
}

// regRemembered has o, a learned state regForgotten copied, take up what
// the copy, f, learned of what holds no object while o was parked.
func regRemembered(o, f *jitLearned) {
	o.reg, o.regLink = f.reg, f.regLink
	o.failed, o.failedEntries, o.siteDeopts, o.bigints, o.bigwide, o.resumed = f.failed, f.failedEntries, f.siteDeopts, f.bigints, f.bigwide, f.resumed
	o.typedReads, o.stringReads, o.keyedReads, o.keyedWrites = f.typedReads, f.stringReads, f.keyedReads, f.keyedWrites
	o.regHints = f.regHints
}

// regAgeParked lets go of the code parked a flush age before tick, as
// the flush lets go of code unused that long, or, for QJS_JIT_STRESS's
// park (all), of all the code parked, so that its next use takes the
// path of code let go of. The callees of the code in the cache stay
// parked another flush age.
func (s *jitState) regAgeParked(tick uint64, all bool) {
	if len(s.regParked) == 0 {
		// What is left are pools whose sources' objects died, which no
		// parked code takes from any longer (regUnparkFailed).
		if len(s.parkedPools) != 0 {
			clear(s.parkedPools)
		}
		return
	}
	age := s.flushAge()
	if !all && tick < s.parkedOldest+age {
		return
	}
	// The callees of the code in the cache -- inlined there, or called
	// natively -- are used where it is, though their own uses do not count
	// those runs, as the flush counts them (regFlush): kept parked, for
	// what their state teaches the caller's next compile.
	callees := map[jitKey]bool{}
	for _, e := range s.regCache {
		for _, fb := range [2]*jitFeedback{e.keep, osrKeep(e)} {
			if fb == nil {
				continue
			}
			for _, cl := range fb.inlined {
				if cl != nil {
					callees[s.regKeyOfClosure(cl)] = true
				}
			}
			for _, l := range fb.links {
				callees[l.key] = true
			}
		}
	}
	oldest := tick
	for key, e := range s.regParked {
		p := e.parked
		switch {
		case !all && tick < p.at+age:
		case !callees[key]:
			s.regUnparkFailed(key, e)
			continue
		default:
			p.at = tick
		}
		oldest = min(oldest, p.at)
	}
	s.parkedOldest = oldest
	if len(s.regParked) == 0 {
		clear(s.parkedPools)
	}
}

// jitRegPark parks all the code no run, native call or compile uses (see
// Parking above).
func (s *jitState) jitRegPark() {
	s.jitRegParkSome(len(s.regCache))
}

// jitRegParkSome parks n of the entries whose code no run, native call or
// compile uses, the least recently used first (jitParkCands), as code
// ages, so that a parking costs what the running since the last paid for
// (parkDue) while the cache's code is parked a slice at a time, the code a
// program no longer runs first; and measures what that cost (parkEach).
func (s *jitState) jitRegParkSome(n int) {
	if s.inFlight() || len(s.regBuilds) != 0 {
		// A compile under way reads the feedback of the functions it
		// inlines and calls, which a parked entry's state is not: the
		// next return tries again.
		return
	}
	from := s.parkSpan()
	s.parkings++
	// makeRoom's list of what it may reclaim, an epoch's, is made again at
	// its next look: kept, it held the entries it listed and what they
	// hold, dropped since or not.
	s.victims, s.victimsAt, s.victimPos, s.victimLeft = nil, 0, 0, 0
	tick := s.flushNow()
	if len(s.regParked) == 0 {
		s.parkedOldest = tick
	}
	called := s.regCalledFns()
	var cands jitParkCands
	for key, e := range s.regCache {
		if e.code == nil || e.parked != nil || s.regInUseOf(e, called) || key.gone() ||
			e == s.regGoOnFrom || e == s.regGoMark.e || e.osrJob != nil {
			continue
		}
		cands = append(cands, jitParkCand{key, e, s.regUsedAt(e), s.regVictimSerial(key, uint64(e.serial))})
	}
	if len(cands) > n {
		sort.Sort(cands)
		cands = cands[:n]
	}
	// Each object's weak pointer made once, for every slot that holds it.
	weaks := map[unsafe.Pointer]weak.Pointer[byte]{}
	for _, c := range cands {
		s.regParkEntry(c.key, c.e, tick, weaks)
	}
	if len(cands) == 0 {
		// A look that parked nothing costs what it did besides what the
		// last parking cost, and the running since that parking goes on
		// paying for both.
		s.parkCost += s.parkSpan() - from
		return
	}
	s.parkCost = s.parkSpan() - from
	s.parkedLast = len(cands)
	s.parkRun = s.parkClock()
}

// jitParkCand is an entry a parking may park, with the epoch the cache
// last saw it used at (regUsedAt), and its function's state's serial
// (regVictimSerial).
type jitParkCand struct {
	key        jitKey
	e          *jitRegEntry
	at, serial uint64
}

// jitParkCands are a parking's candidates, the least recently used first
// once sorted, of those used at one epoch the state made first: ordered by
// the epoch alone, the cache's map's order, which is the process's, chose
// among them.
type jitParkCands []jitParkCand

func (c jitParkCands) Len() int { return len(c) }
func (c jitParkCands) Less(i, j int) bool {
	if c[i].at != c[j].at {
		return c[i].at < c[j].at
	}
	return c[i].serial < c[j].serial
}
func (c jitParkCands) Swap(i, j int) { c[i], c[j] = c[j], c[i] }

// regUsedAt is the epoch the cache last saw e used at (jitRegEntry's
// used, which its looks set, regAge), the latest of all where e was used
// since the last look.
func (s *jitState) regUsedAt(e *jitRegEntry) uint64 {
	if s.regUses(e) != e.seen {
		return math.MaxUint64
	}
	return e.used
}

// regParkEntry parks e, the code of the function whose key is key.
func (s *jitState) regParkEntry(key jitKey, e *jitRegEntry, tick uint64, weaks map[unsafe.Pointer]weak.Pointer[byte]) {
	if o := e.osr; o != nil {
		// Its code for on-stack replacement goes, as with its entry
		// (dropReg).
		e.osr = nil
		s.regHeld -= o.bytes
		o.dropped = true
		s.regClose(o)
	}
	jitParkPlansOnce.Do(jitParkPlansMake)
	plans := &jitParkPlans
	p := &jitParked{s: s, at: tick, weaks: weaks}
	if e.keep != nil {
		p.walk(plans.keep, unsafe.Pointer(e.keep))
	}
	p.walk(plans.targets, unsafe.Pointer(&e.targets))
	if l := e.life; l != nil && l.jitLearned != nil {
		p.learned = l.jitLearned
		l.jitLearned = regForgotten(p.learned)
		p.walk(plans.learned, unsafe.Pointer(p.learned))
	}
	p.seen, p.weaks = nil, nil
	e.parked = p
	delete(s.regCache, key)
	if s.regParked == nil {
		s.regParked = map[jitKey]*jitRegEntry{}
	}
	s.regParked[key] = e
	e.cached = false
	s.regLinkUpdate(key, nil)
	s.parkedCount++
}

// jitRegUnpark has e, parked, back in the cache, as it was, if all it held
// is alive, and reports whether it does; it lets the code go otherwise
// (see Parking above).
func (s *jitState) jitRegUnpark(e *jitRegEntry) bool {
	if e.parked == nil || s.regParked[e.key] != e {
		return false
	}
	// What it costs is parking's (parkDue).
	from := s.parkSpan()
	ok := s.regUnpark(e)
	s.parkCost += s.parkSpan() - from
	return ok
}

// regUnpark is jitRegUnpark's work, for e parked.
func (s *jitState) regUnpark(e *jitRegEntry) bool {
	p := e.parked
	if lost := !p.alive() || !p.poolsAlive(); lost || e.life != nil && s.regLearned[e.key] != e.life {
		// An object it held died, or its function's state was let go of
		// meanwhile, a realm's whose functions all went unused. Code that
		// parking lost so, which held it alive and might have run on, is
		// compiled again once its function is hot again: what that costs
		// is parking's too.
		if lost {
			s.parkCost += e.took
		}
		s.regUnparkFailed(e.key, e)
		return false
	}
	p.restore()
	p.restorePools()
	key := e.key
	e.parked = nil
	delete(s.regParked, key)
	if l := e.life; l != nil && p.learned != nil {
		regRemembered(p.learned, l.jitLearned)
		l.jitLearned = p.learned
	}
	if s.regCache == nil {
		s.regCache = map[jitKey]*jitRegEntry{}
	}
	if old := s.regCache[key]; old != nil {
		// Compiled again meanwhile: the new code stays, and this goes.
		e.dropped = true
		s.regHeld -= e.bytes
		s.regClose(e)
		return false
	}
	s.regCache[key] = e
	e.cached = true
	e.used = s.epoch()
	s.regLinkUpdate(key, e)
	s.unparked++
	return true
}

// regUnparkFailed lets go of e, parked, whose objects died or which went
// unused a flush age parked, as regLetGo lets go of code: what its slots
// hold still is written back, nil where the object died, and the code is
// closed; its function's state keeps what holds no object, and its
// function warms up anew.
func (s *jitState) regUnparkFailed(key jitKey, e *jitRegEntry) {
	p := e.parked
	e.parked = nil
	if s.regParked[key] == e {
		delete(s.regParked, key)
	}
	p.restore()
	// Its pools are put back only whole: one whose source's objects died
	// stays parked, for the other entries that share it to find dead.
	for _, pool := range p.pools {
		if rp := s.parkedPools[pool]; rp != nil && rp.alive() {
			rp.restore()
			delete(s.parkedPools, pool)
		}
	}
	s.regHeld -= e.bytes
	s.generation++
	s.floorAt = 0
	e.dropped, e.cached = true, false
	s.regClose(e)
	if s.regCache[key] == nil {
		if l := s.regLinks[key]; l != nil {
			s.regUnlink(l)
		}
	}
	if l := e.life; l != nil {
		l.invalid, l.warming, l.reopt, l.learning = false, false, false, false
		// The copy its function's state holds is what regForget keeps.
		l.regFlushed = true
		s.regAccount(l)
	}
	s.flushed++
}

// osrKeep is the keep of e's code for on-stack replacement, or nil.
func osrKeep(e *jitRegEntry) *jitFeedback {
	if e.osr == nil {
		return nil
	}
	return e.osr.keep
}
