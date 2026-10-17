package vm

// collectionOp is Map.prototype.get, has or set, or Set.prototype.has or
// add, op saying which, applied without a frame: on a Map or a Set, as each
// asks its receiver to be, a lookup or an addition calls nothing a script
// wrote and cannot throw, so no frame could show in a stack trace, as for
// Math's functions and push. It reports false for any other receiver, which
// the built-in is left to refuse, and, from a pure body that may not store,
// for an addition. It is in the package's last files, after the hot ones.
//
// Math.random (elemRandom) is applied so too, whatever its receiver: the
// next number of the runtime's cache, which a call takes as a store, only
// where its body may store -- a body that missed after it would be run
// again, and draw again.
func (r *Runtime) collectionOp(op uint8, this Value, args []Value, mayStore bool) (Value, bool) {
	if op == elemRandom {
		if !mayStore {
			r.pureRefused = true
			return Undefined, false
		}
		return Float(r.random()), true
	}
	if !this.IsObject() {
		return Undefined, false
	}
	o := this.Object()
	m, ok := o.dataOf().(*jsMap)
	if !ok {
		return Undefined, false
	}
	switch op {
	case elemMapGet, elemMapHas, elemMapSet:
		if o.class != ClassMap {
			return Undefined, false
		}
	default:
		if o.class != ClassSet {
			return Undefined, false
		}
	}
	switch op {
	case elemMapGet:
		v, _ := m.get(r, arg(args, 0))
		return v, true
	case elemMapHas, elemSetHas:
		_, ok := m.get(r, arg(args, 0))
		return Bool(ok), true
	}
	if !mayStore {
		r.pureRefused = true
		return Undefined, false
	}
	if op == elemMapSet {
		m.set(r, arg(args, 0), arg(args, 1))
	} else {
		v := canonicalKey(arg(args, 0))
		m.set(r, v, v)
	}
	return this, true
}
