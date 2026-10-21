package vm

import (
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"
	"weak"
)

// WeakMap.
//
// An entry of a WeakMap is an ephemeron: its value is alive while both the map
// and the key are, and the entry alone keeps neither alive. In particular a
// value that refers to its own key, directly or through anything else, must
// not keep that key alive -- the common case of a WeakMap holding data about
// objects, some of which point back at the object they describe.
//
// Go's collector has no ephemerons. A map that held its keys weakly and its
// values strongly, as this one once did, kept such a key alive through its
// own value for as long as the map lived. So the value is held by the key
// instead: every object or symbol that is a key of some WeakMap carries a
// weakMapRefs, its values in each map it is a key of, under the map's
// weakMapState. A value is then reachable through its key, which is all the
// collector has to see: once nothing else reaches the key, the key, its
// weakMapRefs and the values in it go together, whatever the values refer to.
//
// That leaves the map's own death. Its values are on keys that may well live
// on, and they must not outlive the map. So the map keeps a weak reference to
// each of its keys, and runtime.AddCleanup tells the runtime when the map is
// gone; the runtime then takes the map's value off every key that is still
// there. The cleanup runs on a goroutine of its own while the runtime may be
// running a script, so it only queues the map (weakMapQueue), and the
// runtime releases what is queued on its own goroutine, at the checks a
// running script makes for interrupts (sweepStaleSlots) and at the end of a
// turn. Nothing a script's WeakMap operations touch is shared with another
// goroutine, so they take no lock.
//
// A closed runtime has no turns, so nothing would release the maps that die
// after it closed, and a host that kept one of their keys would keep their
// values too. So closing it releases every map at once (ReleaseClosed), once
// no script is running: one that closed its own runtime runs on to its next
// interrupt check, and must find its WeakMaps as they were until then.
//
// A key's death needs nothing from anyone: its weakMapRefs goes with it, and
// the map's weak reference to it names nothing from then on, which the map
// drops the next time it prunes its keys.
//
// The weak references are to the keys themselves, not to their weakMapRefs:
// the collector keeps a record on each object a weak pointer is made to, in a
// list per span of memory that it searches whenever it adds one, and a span
// of objects, most of which are not keys, has a shorter list than one of
// weakMapRefs, every one of which would be. A key that a WeakRef or a WeakSet
// refers to as well shares the one record.
//
// What remains of the gap is the other direction: a value that refers to its
// map keeps the map alive for as long as the key lives, where an ephemeron
// would let the map, and so the value, go once nothing but the value reached
// it. That needs the collector's help.

// weakMap is a WeakMap's internal slot. It has a pointer of its own, so it is
// never made by the tiny allocator, whose blocks a cleanup cannot be attached
// to.
type weakMap struct {
	// state is nil until the first set: a map that never had an entry has
	// nothing to release, and is not given a cleanup.
	state *weakMapState
}

func newWeakMap() *weakMap { return &weakMap{} }

// weakMapState is what releasing a map needs once the map has gone. It is the
// cleanup's argument, so it must not reach the map, and it does not: nothing
// in it refers to the map at all.
type weakMapState struct {
	// id names the map's entries on its keys. The maps' ids are given out in
	// the order the maps are given their first entries, and are never given
	// out again, so a key keeps its entries in the order of their maps' ids,
	// and finds a map's by a search rather than a Go map of its own -- one
	// that every map not yet released, a gone map's included, made larger:
	// a script that makes a WeakMap after another over the same keys left
	// each key a Go map of a hundred and eighty entries, and a set of a new
	// key three times as long as QuickJS's.
	id uint64
	// keys holds a weak reference to every object that is, or has been, a
	// key of the map, and syms to every symbol: those whose values the
	// map's death has to release. A key is added when it is first given an
	// entry, and a deleted entry stays on its key, marked (weakMapDeleted),
	// so a key given one again is not added twice; so each is a list, not a
	// set to look each key up in. They are apart so that each reference is
	// a word, the list saying what it refers to: a weakTarget, which says
	// so itself, is three, and a map's list of a thousand keys had grown
	// to 24 KB, made again at each doubling.
	keys, syms []weakKey
	// pruneAt is the length of the lists together at which they are next
	// pruned of the keys that have been collected, twice what was left the
	// last time, so that pruning costs a constant per key added.
	pruneAt int
	// pruned is the collector's cycle (gcEpoch) the keys were last pruned
	// in, or the map made in. Only a collection takes keys, so keys are not
	// pruned again until one has run: a map given a thousand keys between
	// two collections had looked at each of them twice, to find none gone.
	pruned uint32
}

// weakKey is a weak reference to an object or a symbol, as the address
// of the allocation, which whoever keeps it knows the kind of. It names
// nothing once the target has been collected, and nothing that is ever
// another's: the collector keeps one record for each object a weak
// reference is made to, which goes with the object.
type weakKey = weak.Pointer[byte]

// makeWeakKey is a weak reference to the object or symbol whose
// allocation p is.
func makeWeakKey(p unsafe.Pointer) weakKey { return weak.Make((*byte)(p)) }

// weakMapRefs is a key's values, one for each WeakMap it is a key of. It is
// made the first time the object or symbol becomes a key, and lives as long
// as the key does.
type weakMapRefs struct {
	// first is the entry of the first map, which for most keys is the only
	// one, without more. Its m is nil while it is empty.
	first weakMapFirst
	// more holds the entries of other maps, nil while there are none. It is
	// a pointer, so that a key of one map is given 40 bytes rather than 64.
	more *weakMapMore
	// key is the weak reference to the key, made once, which the maps of its
	// entries keep.
	key weakKey
}

// weakMapFirst is a key's value in its first map, the map named by its
// state, which the entry holds.
type weakMapFirst struct {
	m     *weakMapState
	value Value
}

// weakMapMore is a key's values in the maps other than its first.
type weakMapMore struct {
	// entries are in the order of their maps' ids, so that the entry of the
	// newest map, the one most often given new keys, is added at the end.
	// The entry of a map that has gone stays, its value weakMapGone, until
	// gone says it is as many as the rest: then the entries are compacted,
	// which costs a constant for each that has gone.
	entries []weakMapEntry
	// gone counts the entries of the maps that have gone.
	gone int
}

// weakMapDeleted is the value of a deleted entry. It is no value a script can
// hold, so get never answers with it.
var weakMapDeleted = Value{num: mkTag(KindUninitialized, 2)}

// weakMapGone is the value of an entry in more of a map that has gone.
var weakMapGone = Value{num: mkTag(KindUninitialized, 3)}

// weakMapIDs gives the maps their ids. It is the process's rather than a
// runtime's, so that no two maps' entries on a key share an id, whatever
// runtime the key came from.
var weakMapIDs atomic.Uint64

// weakMapEntry is a key's value in one map, the map named by its id.
type weakMapEntry struct {
	id    uint64
	value Value
}

// find is the index in more's entries of the entry of the map with the id,
// or where it would go, and whether it is there. more may be nil.
func (more *weakMapMore) find(id uint64) (int, bool) {
	if more == nil {
		return 0, false
	}
	e := more.entries
	n := len(e)
	if n == 0 || e[n-1].id < id {
		return n, false
	}
	lo, hi := 0, n-1
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if e[m].id < id {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo, e[lo].id == id
}

// get is the key's value in the map p.
// The map's get asks first whether the entry is the key's first, and
// calls this only when it is not, or is deleted.
func (refs *weakMapRefs) get(p *weakMapState) (Value, bool) {
	if refs.first.m == p {
		if v := refs.first.value; !v.sameBits(weakMapDeleted) {
			return v, true
		}
		return Undefined, false
	}
	i, ok := refs.more.find(p.id)
	if !ok {
		return Undefined, false
	}
	v := refs.more.entries[i].value
	if v.sameBits(weakMapDeleted) || v.sameBits(weakMapGone) {
		return Undefined, false
	}
	return v, true
}

// set sets the key's value in the map p, and reports whether the entry is
// new: not there before, not even deleted. q is the registry whose spare
// entries (weakMapRegistry.spare) a key that needs room for more takes, or
// nil.
func (refs *weakMapRefs) set(p *weakMapState, v Value, q *weakMapRegistry) bool {
	if refs.first.m == p {
		refs.first.value = v
		return false
	}
	return refs.setMore(p, v, q)
}

// setMore is set for a key whose entry in p, if any, is not its first.
func (refs *weakMapRefs) setMore(p *weakMapState, v Value, q *weakMapRegistry) bool {
	more := refs.more
	if refs.first.m == nil && more == nil {
		refs.first = weakMapFirst{p, v}
		return true
	}
	i, ok := more.find(p.id)
	if ok {
		e := &more.entries[i]
		gone := e.value.sameBits(weakMapGone)
		if gone {
			more.gone--
		}
		e.value = v
		return gone
	}
	if refs.first.m == nil {
		refs.first = weakMapFirst{p, v}
		return true
	}
	if more == nil {
		more = q.takeSpare()
		refs.more = more
	}
	if more.gone > 0 && 2*more.gone >= len(more.entries) {
		more.compact()
		i, _ = more.find(p.id)
	}
	more.entries = slices.Insert(more.entries, i, weakMapEntry{p.id, v})
	return true
}

// compact takes the entries of the maps that have gone out of more. It
// keeps their room unless they used little of it: the maps a key is given
// entries in between two collections are gone together, and the next
// ones' entries need the room again -- given back each time, a key of a
// WeakMap after another had grown its entries from nothing again after
// every collection. Their room is given back once a collection's entries
// were fewer than a quarter of it.
func (more *weakMapMore) compact() {
	n := len(more.entries)
	e := slices.DeleteFunc(more.entries, func(e weakMapEntry) bool { return e.value.sameBits(weakMapGone) })
	if cap(e) > 4*n {
		e = slices.Clone(e)
	}
	more.entries, more.gone = e, 0
}

// delete marks the key's entry in the map p deleted, and reports whether
// there was one. The entry stays, so that the map need not look the key up
// in its list to forget it, nor add it again if it is set again; it costs
// the key a value's room until the key or the map has gone.
func (refs *weakMapRefs) delete(p *weakMapState) bool {
	v := &refs.first.value
	if refs.first.m != p {
		i, ok := refs.more.find(p.id)
		if !ok {
			return false
		}
		v = &refs.more.entries[i].value
	}
	if v.sameBits(weakMapDeleted) || v.sameBits(weakMapGone) {
		return false
	}
	*v = weakMapDeleted
	return true
}

// remove removes the key's entry in the map p, deleted or not, once the map
// has gone, and reports whether there was one. An entry in more is left
// marked, as gone counts it, and more is dropped once every entry in it
// has gone, to be q's spare if q is not nil.
func (refs *weakMapRefs) remove(p *weakMapState, q *weakMapRegistry) bool {
	if refs.first.m == p {
		refs.first = weakMapFirst{}
		return true
	}
	more := refs.more
	i, ok := more.find(p.id)
	if !ok || more.entries[i].value.sameBits(weakMapGone) {
		return false
	}
	more.entries[i].value = weakMapGone
	if more.gone++; more.gone == len(more.entries) {
		refs.more = nil
		q.keepSpare(more)
	}
	return true
}

// weakRefsOf is the weakMapRefs of a key, made if create is set and there is
// none yet, or nil: for a value that is not an object or a symbol, and for
// one that has never been a key, when create is not set.
func weakRefsOf(k Value, create bool) *weakMapRefs {
	switch {
	case k.IsObject():
		o := k.object()
		refs := o.weakMapRefsOf()
		if refs == nil && create {
			refs = o.newWeakMapRefs()
			refs.key = makeWeakKey(k.ref)
		}
		return refs
	case k.IsSymbol():
		s := k.Symbol()
		if s.weakMapRefs == nil && create {
			s.weakMapRefs = &weakMapRefs{key: makeWeakKey(k.ref)}
		}
		return s.weakMapRefs
	}
	return nil
}

// get is the value of the key k, which may be anything. It takes the runtime
// for getOrInsertComputed's sake, as a Map's get does, and does not use it.
func (m *weakMap) get(_ *Runtime, k Value) (Value, bool) {
	if m.state == nil {
		return Undefined, false
	}
	refs := weakRefsOf(k, false)
	if refs == nil {
		return Undefined, false
	}
	if v := refs.first.value; refs.first.m == m.state && !v.sameBits(weakMapDeleted) {
		return v, true
	}
	return refs.get(m.state)
}

// set sets the value of the key k, which the caller has checked can be held
// weakly.
func (m *weakMap) set(r *Runtime, k, v Value) {
	s := m.state
	if s == nil {
		s = &weakMapState{id: weakMapIDs.Add(1), pruned: gcEpoch.Load()}
		m.state = s
		reg := r.weakMapRegistry()
		reg.live[s] = struct{}{}
		runtime.AddCleanup(m, reportWeakMapDeath, weakMapDeath{reg, s})
	}
	refs := weakRefsOf(k, true)
	if refs.set(s, v, r.weakMaps) {
		s.track(refs.key, k.IsSymbol())
	}
}

// delete removes the key k's entry, and reports whether there was one.
func (m *weakMap) delete(k Value) bool {
	s := m.state
	if s == nil {
		return false
	}
	refs := weakRefsOf(k, false)
	return refs != nil && refs.delete(s)
}

// track records that the key k refers to, a symbol if sym is set and an
// object if not, has been given its first entry.
func (s *weakMapState) track(k weakKey, sym bool) {
	if len(s.keys)+len(s.syms) >= s.pruneAt && gcEpoch.Load() != s.pruned {
		s.pruned = gcEpoch.Load()
		s.keys, s.syms = pruneWeakKeys(s.keys), pruneWeakKeys(s.syms)
		s.pruneAt = max(2*(len(s.keys)+len(s.syms)), 8)
	}
	if sym {
		s.syms = append(s.syms, k)
	} else {
		s.keys = append(s.keys, k)
	}
}

// pruneWeakKeys is l without the keys that have been collected, in its
// array.
func pruneWeakKeys(l []weakKey) []weakKey {
	kept := l[:0]
	for _, q := range l {
		if q.Value() != nil {
			kept = append(kept, q)
		}
	}
	clear(l[len(kept):])
	return kept
}

// release takes the map's entries off the keys that are left, once the map
// has gone, keeping in q, if it is not nil, what a key's other maps'
// entries were in for the next maps to take.
func (s *weakMapState) release(q *weakMapRegistry) {
	for _, k := range s.keys {
		s.releaseKey(k, false, q)
	}
	for _, k := range s.syms {
		s.releaseKey(k, true, q)
	}
	s.keys, s.syms = nil, nil
}

// releaseKey takes the map's entry off the key k refers to, a symbol if sym
// is set and an object if not, if it is still there.
func (s *weakMapState) releaseKey(k weakKey, sym bool, q *weakMapRegistry) {
	p := k.Value()
	if p == nil {
		return
	}
	var refs *weakMapRefs
	if sym {
		refs = (*Symbol)(unsafe.Pointer(p)).weakMapRefs
	} else {
		refs = (*Object)(unsafe.Pointer(p)).weakMapRefsOf()
	}
	if refs != nil {
		refs.remove(s, q)
	}
}

// weakMapRegistry is a runtime's WeakMaps that have entries: those not yet
// released, and those that have gone, from the cleanups that report them,
// until the runtime releases them.
type weakMapRegistry struct {
	// live holds every map's state until the map is released, for a closed
	// runtime to release them all. Only the runtime's goroutine touches it.
	live map[*weakMapState]struct{}

	// pending says that dead has maps in it, for the runtime to ask without
	// taking the lock at every interrupt check.
	pending atomic.Bool
	// mu guards dead, which the cleanup goroutine appends to.
	mu   sync.Mutex
	dead []*weakMapState

	// spare holds the weakMapMore of the keys whose entries in maps other
	// than their first have all been released, emptied, with their room,
	// for a key that needs one to take (setMore) -- until a collection
	// has run since they were kept (spareEpoch), when no key has needed
	// them for a cycle and they go. A script that makes a WeakMap after
	// another over the same keys empties a key's entries at each
	// collection and fills them again before the next: made anew each
	// time, they had been more than half of what the script allocated.
	spare      []*weakMapMore
	spareEpoch uint32
}

// keepSpare keeps more, emptied, as a spare, if q is not nil.
func (q *weakMapRegistry) keepSpare(more *weakMapMore) {
	if q == nil {
		return
	}
	q.dropSpares()
	more.entries, more.gone = more.entries[:0], 0
	q.spare = append(q.spare, more)
}

// takeSpare is a spare weakMapMore, or a new one.
func (q *weakMapRegistry) takeSpare() *weakMapMore {
	if q != nil {
		if n := len(q.spare); n > 0 {
			more := q.spare[n-1]
			q.spare[n-1] = nil
			q.spare = q.spare[:n-1]
			return more
		}
	}
	return &weakMapMore{}
}

// dropSpares lets the spares go if a collection has run since they were
// kept.
func (q *weakMapRegistry) dropSpares() {
	if e := gcEpoch.Load(); e != q.spareEpoch {
		clear(q.spare)
		q.spare = q.spare[:0]
		q.spareEpoch = e
	}
}

// weakMapDeath is a map's cleanup argument: its state, and the registry to
// report it to.
type weakMapDeath struct {
	q *weakMapRegistry
	s *weakMapState
}

// reportWeakMapDeath queues a map that has gone. It runs on the cleanup
// goroutine.
func reportWeakMapDeath(d weakMapDeath) {
	d.q.mu.Lock()
	d.q.dead = append(d.q.dead, d.s)
	d.q.pending.Store(true)
	d.q.mu.Unlock()
}

// weakMapRegistry is the runtime's registry of WeakMaps, made when the first
// map is given an entry.
func (r *Runtime) weakMapRegistry() *weakMapRegistry {
	if r.weakMaps == nil {
		r.weakMaps = &weakMapRegistry{live: map[*weakMapState]struct{}{}}
	}
	return r.weakMaps
}

// releaseDeadWeakMaps takes the values of the maps that have gone off their
// keys. It runs on the runtime's goroutine, where nothing else is touching
// those keys.
//
// It is asked at every interrupt check rather than once a collection: a map
// is reported a little after the collection that found it gone, and the
// sooner its entries leave its keys, the less the keys' other entries are
// slowed by them -- a script that makes a WeakMap after another over the
// same keys leaves each key an entry for every map not yet released.
func (r *Runtime) releaseDeadWeakMaps() {
	q := r.weakMaps
	if q == nil || !q.pending.Load() {
		return
	}
	q.mu.Lock()
	dead := q.dead
	q.dead = nil
	q.pending.Store(false)
	q.mu.Unlock()
	q.dropSpares()
	for _, s := range dead {
		s.release(q)
		delete(q.live, s)
	}
}

// releaseAllWeakMaps releases every map, gone or not, for a closed runtime.
// A map released while it lives has no entries left: the runtime runs no
// more scripts to miss them.
func (r *Runtime) releaseAllWeakMaps() {
	q := r.weakMaps
	if q == nil {
		return
	}
	r.releaseDeadWeakMaps()
	for s := range q.live {
		s.release(nil)
	}
	clear(q.live)
	clear(q.spare)
	q.spare = nil
}
