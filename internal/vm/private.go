package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Private class members.
//
// A private name is not a string. Two classes may spell one the same way
// without sharing it, and neither may two evaluations of the same class:
//
//	function make() { return class { #x = 1; static read(o) { return o.#x } } }
//	const A = make(), B = make();
//	A.read(new B())                  // TypeError, not 1
//
// So the key is minted when the class is evaluated and kept in a hidden binding
// the class body closes over, which the compiler arranges. What is stored here
// is only what a key is and how a member reached through one behaves.
//
// The member itself is an ordinary property under that key, marked private so
// that no reflective operation reports it. A private method is stored on each
// instance rather than on the prototype: an object that merely inherits from
// the prototype is not an instance, and asking it for a private member has to
// fail.

// newPrivateName mints the key for one private name of one class evaluation.
//
// It is a symbol, so that it can be a property key without colliding with
// anything: no two calls return the same one, and nothing a script can write
// produces one at all.
func (r *Runtime) newPrivateName(name Atom) Value {
	return Sym(NewSymbol(r.atoms.name(name), true))
}

// privateKey reads the key a private instruction's operand points at.
//
// The binding holding it is an ordinary one, reached the way any other name is:
// from a slot of this function when the low bit is clear, from one of its
// upvalues when it is set.
func privateKey(f *frame, cl *closure, ref uint32) Atom {
	var v Value
	if ref&1 == 0 {
		v = f.locals[ref>>1]
	} else {
		v = cl.upvalues[ref>>1].get()
	}
	if !v.IsSymbol() {
		// The compiler guarantees the binding holds a key; a zero atom is the
		// empty name, which no object has, so a bug reports a miss rather than
		// reaching for the wrong member.
		return atomEmpty
	}
	return cl.realm.atoms.internSymbol(v.Symbol())
}

// definePrivateAccessor adds a private getter or setter, joining it to the
// other half if that is already there.
func (r *Runtime) definePrivateAccessor(target *Object, key Atom, fn *Object, getter bool) {
	var get, set *Object
	if p := target.getOwn(key); p != nil && p.isAccessor() {
		if a := p.getterSetter(); a != nil {
			get, set = a.getter, a.setter
		}
	}
	if getter {
		get = fn
	} else {
		set = fn
	}
	a := &accessor{getter: get, setter: set}
	target.setOwnRaw(key, Value{num: mkTag(KindObject, 0), ref: a},
		propAccessor|propPrivate)
}

// privateMethods is the list of private methods and accessors an instance of a
// class carries.
//
// The functions are made once, when the class is evaluated, and shared by every
// instance -- two instances of a class have the same private method, not two
// equal ones -- so the list is built as the class body runs and copied onto
// each object as it is constructed.
type privateMethods struct {
	entries []privateMethod
}

type privateMethod struct {
	key Atom
	fn  *Object
	op  bytecode.Op
}

// newPrivateMethods returns an empty list, wrapped so that it can live in a
// binding like any other value. Nothing a script can write reaches it.
func (r *Runtime) newPrivateMethods() Value {
	o := newObject(nil, ClassObject)
	o.data = &privateMethods{}
	return Obj(o)
}

func privateMethodsOf(v Value) *privateMethods {
	if !v.IsObject() {
		return nil
	}
	m, _ := v.Object().data.(*privateMethods)
	return m
}

// addPrivateMethod records one member of a class as the class body evaluates it.
func (r *Runtime) addPrivateMethod(list Value, key Atom, fn Value, op bytecode.Op) {
	m := privateMethodsOf(list)
	if m == nil || !fn.IsObject() {
		return
	}
	m.entries = append(m.entries, privateMethod{key: key, fn: fn.Object(), op: op})
}

// installPrivateMethods gives one instance the private members of its class.
//
// It runs where the specification installs them: once `this` exists, which in a
// derived class is after super() has returned, and before any field
// initializer. A method is not writable, so assigning to one fails the way
// assigning to any method of a frozen shape would.
func (r *Runtime) installPrivateMethods(this Value, list Value) error {
	m := privateMethodsOf(list)
	if m == nil || !this.IsObject() {
		return nil
	}
	o := this.Object()
	for i, e := range m.entries {
		// An object cannot be given the same private member twice, which a
		// constructor that returns an object it has already built would
		// otherwise do. The two halves of an accessor are one member, so only
		// a key this installation has not already reached counts.
		if o.getOwn(e.key) != nil && !hasPrivateKey(m.entries[:i], e.key) {
			return r.throwTypeError("%s is already present on this object",
				r.atoms.name(e.key))
		}
		if !o.IsExtensible() {
			// A private member is a member like any other: an object closed to
			// new ones takes none, which a base constructor that sealed `this`
			// is what usually causes.
			return r.throwTypeError("cannot add %s to a non-extensible object",
				r.atoms.name(e.key))
		}
		switch e.op {
		case bytecode.OpAddPrivateGetter:
			r.definePrivateAccessor(o, e.key, e.fn, true)
		case bytecode.OpAddPrivateSetter:
			r.definePrivateAccessor(o, e.key, e.fn, false)
		default:
			o.setOwnRaw(e.key, Obj(e.fn), propPrivate)
		}
	}
	return nil
}

// hasPrivateKey reports whether a key appears among the entries already
// installed, which is what tells the second half of an accessor apart from a
// member the object already had.
func hasPrivateKey(entries []privateMethod, key Atom) bool {
	for _, e := range entries {
		if e.key == key {
			return true
		}
	}
	return false
}
