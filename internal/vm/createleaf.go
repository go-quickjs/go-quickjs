package vm

// Object.create as a native leaf (leafChecks): the interpreter and native
// code call it without a frame where it is given a prototype alone, an
// object or null, as V8 calls its CreateObjectWithoutProperties builtin with
// no frame of JavaScript's. Such a call makes an ordinary object of that
// prototype, as the built-in does (newCreated), and runs nothing: making an
// object reads no property of its prototype, a proxy's included. A
// properties argument is the built-in's own call: defineProperties reads
// it, which may run its getters.
//
// From native code it had been a call of Go on the register frame
// (jitRegGoCall), one at every turn of a loop making objects so, and the
// loop, judged not to pay for them, left to the interpreter.
//
// As isArray's leaf, the functions are generic, instantiated with struct{}
// alone, and the check a method of a generic type used as a method value
// (see hasownleaf.go); objectLeaves calls createLeaf.

// createLeaf gives the realm's Object.create its leaf's op.
func createLeaf[_ any](r *Runtime) {
	c := r.proto.object.getOwn(atomConstructor)
	if !c.ok() || !c.value().IsObject() {
		return
	}
	m := c.value().Object().getOwn(r.atoms.intern("create"))
	if !m.ok() || !m.value().IsObject() {
		return
	}
	if o := m.value().Object(); o.fn() != nil && o.fn().native != nil {
		setElemOp(r, o, elemObjCreate)
	}
}

// createLeafCheck gives the leaf its check.
func createLeafCheck[_ any]() {
	leafChecks[elemObjCreate] = createCheck[struct{}]{}.check
}

// createCheck is Object.create's leaf's check.
type createCheck[_ any] struct{}

// check reports whether Object.create, called with args, runs no script and
// cannot throw: its prototype an object or null, and no properties to
// define.
func (createCheck[_]) check(_ *Runtime, _ Value, args []Value) bool {
	return len(args) != 0 && (args[0].IsObject() || args[0].IsNull()) && (len(args) < 2 || args[1].IsUndefined())
}
