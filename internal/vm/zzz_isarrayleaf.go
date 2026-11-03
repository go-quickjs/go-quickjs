package vm

import "sync"

// Array.isArray as a native leaf (leafChecks): every tier calls it without
// a frame where its argument is no proxy, as V8 reduces the call to a
// check of the receiver's map. A module keeps it in a binding of its own
// -- React's ba = Array.isArray, asked of each child it renders -- which
// native code had called with a frame (jitGoCallNested). Of a proxy,
// IsArray asks the proxy's target, and a revoked proxy throws: the
// built-in's own call.
//
// As hasOwnProperty's leaf, the functions are generic, instantiated with
// struct{} alone, and the check a method of a generic type used as a
// method value (see zzz_hasownleaf.go); objectLeaves calls isArrayLeaf.

// isArrayLeafOnce gives isArray its check.
var isArrayLeafOnce sync.Once

// isArrayLeaf gives the realm's Array.isArray its leaf's op.
func isArrayLeaf[_ any](r *Runtime) {
	isArrayLeafOnce.Do(isArrayLeafCheck[struct{}])
	if r.proto.arrayCtor == nil {
		return
	}
	m := r.proto.arrayCtor.getOwn(r.atoms.intern("isArray"))
	if !m.ok() || !m.value().IsObject() {
		return
	}
	if o := m.value().Object(); o.fn() != nil && o.fn().native != nil {
		setElemOp(r, o, elemArrIsArray)
	}
}

// isArrayLeafCheck gives the leaf its check.
func isArrayLeafCheck[_ any]() {
	leafChecks[elemArrIsArray] = isArrayCheck[struct{}]{}.check
}

// isArrayCheck is isArray's leaf's check.
type isArrayCheck[_ any] struct{}

// check reports whether Array.isArray, called with args, runs no script
// and cannot throw.
func (isArrayCheck[_]) check(_ *Runtime, _ Value, args []Value) bool {
	return len(args) == 0 || !args[0].IsObject() || proxyOf(args[0].Object()) == nil
}
