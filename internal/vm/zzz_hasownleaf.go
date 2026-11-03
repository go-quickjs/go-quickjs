package vm

// Object.prototype.hasOwnProperty as a native leaf (leafChecks): every tier
// calls it without a frame where nothing it does can run the script's code
// or throw, as V8 calls its builtin with no frame of JavaScript's. React
// asks it of each property an element is given, through a binding of its
// own -- for (k in props) hasOwnProperty.call(props, k) -- and of its
// reserved names, RESERVED.hasOwnProperty(k): ordinary calls of a
// built-in, each a frame and a tick in the tree tier, and from native code
// a call of Go with the frames of the levels it is called from
// (jitGoCallNested), some 17,000 of them in each render of the SSR
// benchmark's page.
//
// The check passes where the receiver is an object, neither a proxy, whose
// getOwnPropertyDescriptor trap would run, nor a module namespace, whose
// bindings may be uninitialized, which throws; and the key is no object,
// which ToPropertyKey converts by calling its toString or valueOf. A
// primitive receiver is the built-in's own call: its ToObject makes a
// wrapper.
//
// As regexpLeaves' are, the functions are generic, instantiated with
// struct{} alone, and the check a method of a generic type used as a
// method value: laid out after every function and closure of the package,
// they move none of them (see zzz_regexpleaf.go). regexpLeaves, the
// realm's last setup of its leaves, calls objectLeaves.

// objectLeaves gives the realm's Object.prototype.hasOwnProperty its
// leaf's op, and Array.isArray its own (isArrayLeaf).
func objectLeaves[_ any](r *Runtime) {
	isArrayLeaf[struct{}](r)
	m := r.proto.object.getOwn(r.atoms.intern("hasOwnProperty"))
	if !m.ok() || !m.value().IsObject() {
		return
	}
	if o := m.value().Object(); o.fn() != nil && o.fn().native != nil {
		setElemOp(r, o, elemObjHasOwn)
	}
}

// hasOwnLeafCheck gives the leaf its check.
func hasOwnLeafCheck[_ any]() {
	leafChecks[elemObjHasOwn] = hasOwnLeaf[struct{}]{}.check
}

// hasOwnLeaf is hasOwnProperty's leaf.
type hasOwnLeaf[_ any] struct{}

// check reports whether hasOwnProperty, called on this with args, runs no
// script and cannot throw.
func (hasOwnLeaf[_]) check(_ *Runtime, this Value, args []Value) bool {
	if !this.IsObject() || len(args) != 0 && args[0].IsObject() {
		return false
	}
	o := this.Object()
	return proxyOf(o) == nil && o.class != ClassModuleNamespace
}
