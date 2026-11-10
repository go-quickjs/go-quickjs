package vm

import "unsafe"

// instanceOfEntry is a constructor instanceof has answered for by
// OrdinaryHasInstance, with what proves it still would be: its layout as it
// was then, and where its prototype property is in it.
//
// The layout proves it all. The constructor is an ordinary function, not
// bound and no proxy, which an object never stops being. Its layout names
// its prototype, Function.prototype, whose Symbol.hasInstance is the
// realm's and can never be another, being neither writable nor
// configurable; and its own keys, none of them Symbol.hasInstance, and its
// prototype property a data property. Any change to those -- a
// Symbol.hasInstance of its own, another prototype, an accessor or a
// deletion -- gives it another layout, since the entry remembers this one
// (remember). The prototype property's value is read anew each time, so a
// script assigning it is seen, and the instance's chain is walked as it is.
//
// V8 answers the same with a map check; the entries are the runtime's, by
// the constructor, rather than a site's: a program tests the same few
// constructors at many sites (a Lisp's pairs, at 48). They are forgotten
// once each time the collector has run (sweepStaleSlots), so that a realm
// whose constructor one was can go.
type instanceOfEntry struct {
	ctor  *Object
	shape *shape
	idx   int32
}

// instanceOfEntries is how many constructors the runtime remembers.
const instanceOfEntries = 8

// instanceOfSlot is the entry c would be remembered in.
func instanceOfSlot(c *Object) uintptr {
	return uintptr(unsafe.Pointer(c)) >> 5 & (instanceOfEntries - 1)
}

// instanceOfKnown answers v instanceof c from the entry remembered for c, in
// ok, or reports false there for instanceOfAt to answer.
func (r *Runtime) instanceOfKnown(c *Object, v Value) (yes, ok bool) {
	e := &r.ctorsKnown[instanceOfSlot(c)]
	if e.ctor != c || c.shape != e.shape {
		return false, false
	}
	if !v.IsObject() {
		return false, true
	}
	p := c.vals()[e.idx]
	if !p.IsObject() {
		// A TypeError, which instanceOfAt throws.
		return false, false
	}
	target := p.Object()
	// A proxy in the chain runs its getPrototypeOf trap, which instanceOfAt
	// calls.
	for o := v.Object(); o.class != ClassProxy; {
		if o = o.proto(); o == nil {
			return false, true
		}
		if o == target {
			return true, true
		}
	}
	return false, false
}

// noteInstanceOf remembers c, whose instanceof has just been answered by the
// realm's Symbol.hasInstance found on its prototype, if it is an ordinary
// function and its layout is one that proves it (instanceOfEntry).
func (r *Runtime) noteInstanceOf(c *Object) {
	s := c.shape
	if s == nil || s == dictShape || s.loose || c.flags&objPolyProto != 0 {
		return
	}
	if fd := c.fn(); fd == nil || fd.bound {
		return
	}
	if c.findOwn(r.hasInstanceAtom) >= 0 {
		return
	}
	// Its prototype's Symbol.hasInstance is the realm's, for good.
	h := s.proto
	if h == nil || h.class == ClassProxy {
		return
	}
	hi := h.findOwn(r.hasInstanceAtom)
	if hi < 0 || h.shape.keys[hi].flags&(propAccessor|propWritable|propConfigurable|propDeleted|propUninit) != 0 {
		return
	}
	if hv := h.vals()[hi]; !hv.IsObject() || hv.Object() != r.hasInstanceFn {
		return
	}
	i := c.findOwn(atomPrototype)
	if i < 0 || s.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
		return
	}
	r.ctorsKnown[instanceOfSlot(c)] = instanceOfEntry{ctor: c, shape: remember(s), idx: i}
}
