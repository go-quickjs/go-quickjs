package vm

import (
	"math"
	"sync"
	"unsafe"
)

// DataView.prototype's getters and setters as native leaves (leafChecks):
// a read or a write of an index in the view, its buffer there, is called
// without a frame by every tier, as String's methods are, and native
// code's call of one is a leaf's (ssa.GoLeaf), as V8's code reduces a
// DataView's accessors rather than calling them. Each had been an
// ordinary built-in's call: in the tree tier a frame and a tick, in native
// code a call of Go (jitGoCall) -- its frame, its operands copied and
// cleared, its code pinned -- for an access of a few bytes, which made a
// loop of them twice as slow with the JIT as without.
//
// A check passes only where the accessor runs nothing of the script's,
// throws nothing and keeps nothing (leafChecks): the receiver a DataView,
// the index undefined or a number whose integer, ToIndex's, is in the
// view with the bytes the accessor moves, the buffer neither detached, nor
// shrunk under the view, nor shared memory, which another agent may grow;
// and, for a setter, the buffer not immutable and the value a number --
// a BigInt for a BigInt's setter -- which its conversion takes as it is.
// The endianness flag is converted by ToBoolean, which calls nothing.
// Anything else is the built-in's, called the ordinary way.
//
// The accessor itself is given a form that finds the bytes as the check
// does, once (dataViewBytes), where the built-in's own asked for its
// buffer four times over, and does as the built-in does for anything
// else: a leaf's check and its call had otherwise cost the tree tier more
// than the frame they spare.
//
// The functions are generic, each instantiated with struct{} alone: Go
// lays a generic function's instantiations out after every function and
// closure of the package (see zzz_direct.go). initDataViewBuiltins
// reaches them through dataViewLeaves, called where its last call,
// defToStringTag's, was: its code grows within the padding that rounds it
// to 32 bytes, and every function and closure of the package stays at its
// address (go tool nm). The checks are given their places in leafChecks
// once, when the first realm makes its DataView.prototype.

// dataViewLeafOnce gives the DataView's leaves their checks.
var dataViewLeafOnce sync.Once

// dataViewLeaves is initDataViewBuiltins' last call: proto's toStringTag,
// as it was, and then each accessor of proto, DataView.prototype as the
// realm made it, declared the native leaf of its width (elemViewGet1...)
// and given its one-pass form.
func dataViewLeaves[_ any](r *Runtime, proto *Object) {
	r.defToStringTag(proto, "DataView")
	dataViewLeafOnce.Do(dataViewLeafChecks[struct{}])
	for _, spec := range dataViewTypes {
		get, set := dataViewLeafOps[struct{}](spec)
		if p := proto.getOwn(r.atoms.intern("get" + spec.name)); p.ok() && p.value().IsObject() {
			if fd := p.value().Object().fn(); fd != nil && fd.native != nil {
				l := &dataViewLeaf[struct{}]{spec: spec, slow: fd.native, size: spec.size}
				fd.native = l.get
				setElemOp(r, p.value().Object(), get)
			}
		}
		if p := proto.getOwn(r.atoms.intern("set" + spec.name)); p.ok() && p.value().IsObject() {
			if fd := p.value().Object().fn(); fd != nil && fd.native != nil {
				l := &dataViewLeaf[struct{}]{spec: spec, slow: fd.native, size: spec.size, set: true, big: dataViewBig[struct{}](spec)}
				fd.native = l.put
				setElemOp(r, p.value().Object(), set)
			}
		}
	}
}

// dataViewLeafOps are the leaves of spec's getter and setter: by the bytes
// they move, a setter of a BigInt's type apart, whose value is a BigInt.
func dataViewLeafOps[_ any](spec dataViewType) (get, set uint8) {
	switch spec.size {
	case 1:
		get, set = elemViewGet1, elemViewSet1
	case 2:
		get, set = elemViewGet2, elemViewSet2
	case 4:
		get, set = elemViewGet4, elemViewSet4
	default:
		get, set = elemViewGet8, elemViewSet8
	}
	if dataViewBig[struct{}](spec) {
		set = elemViewSetBig
	}
	return get, set
}

// dataViewBig reports whether spec's values are BigInts.
func dataViewBig[_ any](spec dataViewType) bool {
	return spec.kind == elemBigInt64 || spec.kind == elemBigUint64
}

// dataViewLeafChecks gives the leaves their checks.
func dataViewLeafChecks[_ any]() {
	for op, size := range map[uint8]int{elemViewGet1: 1, elemViewGet2: 2, elemViewGet4: 4, elemViewGet8: 8,
		elemViewSet1: 1, elemViewSet2: 2, elemViewSet4: 4, elemViewSet8: 8, elemViewSetBig: 8} {
		l := &dataViewLeaf[struct{}]{size: size, set: op >= elemViewSet1, big: op == elemViewSetBig}
		leafChecks[op] = l.check
	}
}

// dataViewLeaf is an accessor's leaf: the bytes it moves, whether it sets
// them, and whether from a BigInt; and, for the accessor's own one-pass
// form, its type and the built-in's function, for anything that form does
// not make. Its methods are what would be closures: a generic function's
// closures are laid out ahead of every generic instantiation there was,
// and moved them all (directRest, which runs every callback a built-in
// calls, among them).
type dataViewLeaf[_ any] struct {
	spec     dataViewType
	slow     NativeFunc
	size     int
	set, big bool
}

// check is the leaf's check (leafChecks): the bytes in the view, and, for
// a setter, the buffer writable and the value a number, or a BigInt.
func (l *dataViewLeaf[_]) check(_ *Runtime, this Value, args []Value) bool {
	if l.set && (len(args) < 2 || !l.big && !args[1].IsNumber() || l.big && *(*uint64)(unsafe.Pointer(&args[1].num)) < heapBigBits) {
		return false
	}
	b, _, ok := dataViewBytes[struct{}](this, args, l.size)
	return ok && (!l.set || !b.immutable)
}

// dataViewBytes is the buffer of this's view and the offset in it of the
// size bytes at args' index, where this is a DataView whose buffer, not
// shared memory, is there under it, and the index, undefined or a number,
// is one of the view's with size bytes from it in the view, as the
// accessor's ToIndex and bounds checks would find it -- false for
// anything else.
func dataViewBytes[_ any](this Value, args []Value, size int) (*arrayBufferData, int, bool) {
	if !this.IsObject() {
		return nil, 0, false
	}
	o := this.Object()
	if o.class != ClassDataView {
		return nil, 0, false
	}
	d, ok := o.extAt().data.(*dataViewData)
	if !ok || d.buffer == nil {
		return nil, 0, false
	}
	b, ok := d.buffer.dataOf().(*arrayBufferData)
	if !ok || b.detached || b.block != nil {
		return nil, 0, false
	}
	i := 0.0
	if len(args) > 0 {
		switch a := args[0]; {
		case a.IsNumber():
			// ToIntegerOrInfinity: NaN is 0, and -0 and what truncates to
			// it are 0 too, which the comparisons below take as 0.
			if i = math.Trunc(a.num); i != i {
				i = 0
			}
		case !a.IsUndefined():
			return nil, 0, false
		}
	}
	n := len(b.bytes)
	length := d.byteLength
	if d.tracking {
		length = n - d.byteOffset
	}
	if d.byteOffset > n || length < 0 || d.byteOffset+length > n || !(i >= 0) || i+float64(size) > float64(length) {
		return nil, 0, false
	}
	return b, d.byteOffset + int(i), true
}

// get is the getter's one-pass form: the bytes dataViewBytes finds, and
// slow for anything else.
func (l *dataViewLeaf[_]) get(rt *Runtime, this Value, args []Value) (Value, error) {
	b, off, ok := dataViewBytes[struct{}](this, args, l.size)
	if !ok {
		return l.slow(rt, this, args)
	}
	little := l.spec.argc == 2 && len(args) > 1 && args[1].Truthy()
	return decodeView(b.bytes[off:off+l.size], l.spec, little), nil
}

// put is the setter's one-pass form: where the check would pass, the
// value converted and stored, and slow for anything else.
func (l *dataViewLeaf[_]) put(rt *Runtime, this Value, args []Value) (Value, error) {
	if len(args) < 2 || !l.big && !args[1].IsNumber() || l.big && *(*uint64)(unsafe.Pointer(&args[1].num)) < heapBigBits {
		return l.slow(rt, this, args)
	}
	b, off, ok := dataViewBytes[struct{}](this, args, l.size)
	if !ok || b.immutable {
		return l.slow(rt, this, args)
	}
	little := l.spec.argc == 2 && len(args) > 2 && args[2].Truthy()
	var num float64
	var bv *BigInt
	if l.big {
		var err error
		if bv, err = rt.toBigIntOperand(args[1]); err != nil {
			return l.slow(rt, this, args)
		}
	} else {
		num = args[1].num
	}
	encodeView(b.bytes[off:off+l.size], l.spec, little, num, bv)
	return Undefined, nil
}
