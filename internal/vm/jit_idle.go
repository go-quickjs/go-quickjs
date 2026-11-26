//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"
	"reflect"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// jitReturned is jitReturnedToHost: what the JIT does as r's turn ends
// with no frame running. The process's idle workspaces go
// (jit.DropIdleWorkspaces); the cache looks to flush (flush): an idle
// runtime, which takes no back edge's check nor compiles, had kept all
// its code and what that holds until it ran again; and, once a flush age
// of the program's running since it last did, it parks its code
// (jitRegPark). The look is the flush's own, which drops only what has
// gone unused for the flush's age, so most of a host's short calls pay a
// clock's read.
func jitReturned(r *Runtime) {
	s := r.jit
	if s == nil {
		return
	}
	jit.DropIdleWorkspaces()
	if s.unavailable {
		return
	}
	s.flush()
	switch tick := s.flushNow(); {
	case r.jitStress.park:
		// QJS_JIT_STRESS's park: at every return.
		s.jitRegPark()
	case s.parkedAt == 0:
		// The first return: the period begins.
		s.parkedAt = max(tick, 1)
	case tick >= s.parkedAt+s.flushAge():
		s.parkedAt = tick
		s.jitRegPark()
	}
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
// As the runtime returns to its host with no frame running, at most once
// a flush age of the program's running (jitReturned), the cache parks the
// code no run, native call or compile uses: it takes the entry out of the
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
// address an object no longer has is ever made. Code parked and not used
// by the next parking, a flush age later, is let go of then. Active code
// pays nothing: its entries are tested as before, and a parked entry
// takes paths code let go of took already, once.

// jitParked is what a parked entry holds weakly (jitRegPark): each slot
// it cleared, with a weak pointer to what the slot held; and the learned
// state of its function, its slots among these, while the function's life
// holds a copy that holds no object (regForgotten).
type jitParked struct {
	slots   []jitParkSlot
	learned *jitLearned
	// pools are the object pools the entry's code and state take from,
	// each parked once for all the entries that share it (jitState's
	// parkedPools): an inlined callee's literal is made from the callee's
	// pool. Whichever of them is first put back puts the pool back.
	pools []*abi.ObjectPool
	// s is the state parking the entry; seen are the structures walked,
	// so that each is walked once.
	s    *jitState
	seen map[unsafe.Pointer]bool
}

// jitParkSlot is a pointer slot a parked entry cleared, and a weak pointer
// to what it held.
type jitParkSlot struct {
	at *unsafe.Pointer
	w  weak.Pointer[byte]
}

// hold clears the pointer slot at, keeping a weak pointer to what it held.
func (p *jitParked) hold(at *unsafe.Pointer) {
	if *at == nil {
		return
	}
	p.slots = append(p.slots, jitParkSlot{at: at, w: weak.Make((*byte)(*at))})
	*at = nil
}

// The types whose pointers a parked entry holds weakly: what holds a
// program's objects, all of them allocated in the heap -- a pointer
// to any other may name a package's variable, which a weak pointer
// cannot be made to.
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

// walk holds weakly what v, addressable, holds of the program (jitParkWeak),
// walking into the structures the entry owns.
func (p *jitParked) walk(v reflect.Value) {
	t := v.Type()
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		at := (*unsafe.Pointer)(unsafe.Pointer(v.UnsafeAddr()))
		switch {
		case jitParkWeak[t]:
			p.hold(at)
		case t == jitParkPool:
			p.pool((*abi.ObjectPool)(*at))
		case jitParkInto[t]:
			if p.once(*at) {
				p.walk(v.Elem())
			}
		}
	case reflect.Struct:
		if t == jitParkValue {
			// A value of an object's tag holds the object, an accessor
			// pair or a template, each the heap's.
			x := (*Value)(unsafe.Pointer(v.UnsafeAddr()))
			if b := math.Float64bits(x.num); b == objectBits || b == accessorBits || b == templateBits {
				p.hold(&x.ref)
			}
			return
		}
		if t.PkgPath() != jitParkValue.PkgPath() {
			return
		}
		for i := range v.NumField() {
			p.walk(v.Field(i))
		}
	case reflect.Slice:
		if t == jitParkCells {
			// The cells the code reads its constants from, the JIT's own
			// (keep's cells): what each holds.
			for i := range v.Len() {
				if c := v.Index(i); !c.IsNil() {
					p.walk(c.Elem())
				}
			}
			return
		}
		if !jitParkElem(t.Elem()) {
			return
		}
		for i := range v.Len() {
			p.walk(v.Index(i))
		}
	case reflect.Array:
		if !jitParkElem(t.Elem()) {
			return
		}
		for i := range v.Len() {
			p.walk(v.Index(i))
		}
	}
}

// jitParkElem reports whether elements of type t may hold what walk
// weakens: a pointer of those types, a pool, a structure an entry owns,
// or a struct or array of this package's.
func jitParkElem(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer:
		return jitParkWeak[t] || jitParkInto[t] || t == jitParkPool
	case reflect.Struct:
		return t.PkgPath() == jitParkValue.PkgPath()
	case reflect.Array, reflect.Slice:
		return jitParkElem(t.Elem())
	}
	return false
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
	rp := &jitParked{}
	if src := (*jitPoolSource)(pool.Source); src != nil {
		rp.walk(reflect.ValueOf(src).Elem())
	}
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
		failed: o.failed, failedEntries: o.failedEntries, bigints: o.bigints, bigwide: o.bigwide, resumed: o.resumed,
		typedReads: o.typedReads, stringReads: o.stringReads, keyedReads: o.keyedReads, keyedWrites: o.keyedWrites,
		regHints: o.regHints}
}

// regRemembered has o, a learned state regForgotten copied, take up what
// the copy, f, learned of what holds no object while o was parked.
func regRemembered(o, f *jitLearned) {
	o.reg, o.regLink = f.reg, f.regLink
	o.failed, o.failedEntries, o.bigints, o.bigwide, o.resumed = f.failed, f.failedEntries, f.bigints, f.bigwide, f.resumed
	o.typedReads, o.stringReads, o.keyedReads, o.keyedWrites = f.typedReads, f.stringReads, f.keyedReads, f.keyedWrites
	o.regHints = f.regHints
}

// jitRegPark lets go of the code still parked since the last parking,
// unused a flush age, and parks the code no run, native call or compile
// uses (see Parking above).
func (s *jitState) jitRegPark() {
	s.parkings++
	// makeRoom's list of what it may reclaim, an epoch's, is made again at
	// its next look: kept, it held the entries it listed and what they
	// hold, dropped since or not.
	s.victims, s.victimsAt, s.victimPos, s.victimLeft = nil, 0, 0, 0
	if len(s.regParked) != 0 {
		// The callees of the code in the cache -- inlined there, or called
		// natively -- are used where it is, though their own uses do not
		// count those runs, as the flush counts them (regFlush): kept
		// parked, for what their state teaches the caller's next compile.
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
		for key, e := range s.regParked {
			if !callees[key] {
				s.regUnparkFailed(key, e)
			}
		}
	}
	if len(s.regParked) == 0 {
		// What is left are pools whose sources' objects died, which no
		// parked code takes from any longer (regUnparkFailed).
		clear(s.parkedPools)
	}
	if s.inFlight() || len(s.regBuilds) != 0 {
		// A compile under way reads the feedback of the functions it
		// inlines and calls, which a parked entry's state is not.
		return
	}
	called := s.regCalledFns()
	for key, e := range s.regCache {
		if e.code == nil || e.parked != nil || s.regInUseOf(e, called) || key.gone() ||
			e == s.regGoOnFrom || e == s.regGoMark.e || e.osrJob != nil {
			continue
		}
		s.regParkEntry(key, e)
	}
}

// regParkEntry parks e, the code of the function whose key is key.
func (s *jitState) regParkEntry(key jitKey, e *jitRegEntry) {
	if o := e.osr; o != nil {
		// Its code for on-stack replacement goes, as with its entry
		// (dropReg).
		e.osr = nil
		s.regHeld -= o.bytes
		o.dropped = true
		s.regClose(o)
	}
	p := &jitParked{s: s}
	if e.keep != nil {
		p.walk(reflect.ValueOf(e.keep).Elem())
	}
	p.walk(reflect.ValueOf(&e.targets).Elem())
	if l := e.life; l != nil && l.jitLearned != nil {
		p.learned = l.jitLearned
		l.jitLearned = regForgotten(p.learned)
		p.walk(reflect.ValueOf(p.learned).Elem())
	}
	p.seen = nil
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
	p := e.parked
	if p == nil || s.regParked[e.key] != e {
		return false
	}
	if !p.alive() || !p.poolsAlive() || e.life != nil && s.regLearned[e.key] != e.life {
		// An object it held died, or its function's state was let go of
		// meanwhile, a realm's whose functions all went unused.
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
