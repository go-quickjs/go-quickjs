package vm

import (
	"errors"
	"math"
	"strconv"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/fdlibm"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// The interpreter.
//
// One contiguous slice serves as both the local-variable storage and the
// operand stack. A frame occupies a window of it laid out as locals followed by
// operands, so entering a call costs a bounds check and a slice reslice rather
// than an allocation.
//
// The slice is allocated once at its full size and never grown. That is what
// makes it safe for an upvalue to hold a *Value pointing into a live frame: the
// backing array never moves. It also gives the sandbox its stack limit for
// free, since exhausting the slice is exactly "maximum call stack size
// exceeded".

// defaultStackSize is the number of value slots available to all frames
// together. At 24 bytes per slot this is about 6 MB, which allows a recursion
// depth in the tens of thousands for typical functions.
const defaultStackSize = 256 * 1024

// callDepthLimit bounds recursion independently of the slot count, so that a
// function with very few locals still cannot recurse without bound.
const defaultCallDepthLimit = 8192

// maxGoRecursion bounds how deep the engine recurses in Go, whatever a host
// asks for: a call takes a few kilobytes of the goroutine's stack, and one
// that grows past Go's limit -- a gigabyte on a 64-bit platform and a quarter
// of one on a 32-bit one -- ends the process, which nothing can recover
// from. It bounds the depth of calls, and separately the recursion no frame
// counts: a proxy forwarding to its target, JSON's nesting, flat's.
// It is 50,000 on a 64-bit platform and 25,000 on a 32-bit one.
const maxGoRecursion = 12500 << (unsafe.Sizeof(uintptr(0)) / 4)

// frameBlockSize is how many frames are allocated at a time. A frame is a
// couple of hundred bytes and the depth limit is thousands, so allocating the
// limit up front would cost every runtime megabytes it will almost certainly
// never use; a block is small enough to be cheap and large enough that the
// allocation is rare.
const frameBlockSize = 64

// functionNameFromKey derives a method's name from its property key.
//
// A symbol-keyed method is named after the symbol's description in brackets,
// and one whose symbol has no description is anonymous, which is the only way
// to name a function that cannot be spelled as an identifier.
func functionNameFromKey(key Value, kind uint32) string {
	name := ""
	switch {
	case key.IsSymbol():
		sym := key.Symbol()
		if sym.HasDescription {
			name = "[" + sym.Description + "]"
		}
	case key.IsString():
		name = key.String().Go()
	}
	switch kind {
	case 1:
		return "get " + name
	case 2:
		return "set " + name
	}
	return name
}

// defineHalfAccessor installs one half of an accessor, leaving the other half
// as whatever a previous definition put there.
//
// A get/set pair written separately reaches this twice, and the second must not
// discard the first.
func (r *Runtime) defineHalfAccessor(o *Object, key Atom, fn *Object, isGetter bool,
	flags propFlags) {
	var getter, setter *Object
	if isGetter {
		getter = fn
	} else {
		setter = fn
	}
	r.defineAccessor(o, key, getter, setter, flags)
}

// memberFlags turns a define instruction's class-member operand into the
// attributes the member gets. A class's members are not enumerable, so that
// Object.keys of an instance lists its fields and not the methods it inherits.
func memberFlags(isClassMember uint32) propFlags {
	if isClassMember != 0 {
		return propWritable | propConfigurable
	}
	return propDefault
}

// moduleLexProp finds the binding a name has in module code, which is either
// one of the module's own or a script-level lexical binding: the module
// environment inherits from the global object, and those sit in front of it.
func (r *Runtime) moduleLexProp(env *Object, name Atom) ownProp {
	if p := env.getOwn(name); p.ok() {
		return p
	}
	if !r.lexShadows(name) {
		return ownProp{i: -1}
	}
	return r.globalLex.getOwn(name)
}

// deleteEvalVar removes a binding a direct eval declared, reporting whether
// there was one.
func deleteEvalVar(o *Object, name Atom) bool {
	for ; o != nil; o = o.proto() {
		if o.getOwn(name).ok() {
			o.deleteOwn(name)
			return true
		}
	}
	return false
}

// evalVarProp finds a binding a direct eval declared in an enclosing function.
//
// The objects are chained by prototype, innermost first, so one lookup covers
// every function between here and the one that declared the name.
func evalVarProp(o *Object, name Atom) ownProp {
	for ; o != nil; o = o.proto() {
		if p := o.getOwn(name); p.ok() {
			return p
		}
	}
	return ownProp{i: -1}
}

// declareGlobalLex adds a script-level lexical binding of name, in its dead
// zone, and records that globalLex has one of the name.
func (r *Runtime) declareGlobalLex(name Atom, flags propFlags) {
	r.globalLex.setOwnRaw(name, uninitialized, flags)
	// It may shadow a global native code reads by its position.
	r.shapes.globalEpoch++
	i := int(uint32(name) >> 6)
	for len(r.lexNames) <= i {
		r.lexNames = append(r.lexNames, 0)
	}
	r.lexNames[i] |= 1 << (uint32(name) & 63)
}

// lexShadows reports whether globalLex has a binding of name, which a
// global of the name is then behind.
func (r *Runtime) lexShadows(name Atom) bool {
	i := uint(uint32(name) >> 6)
	return i < uint(len(r.lexNames)) && r.lexNames[i]&(1<<(uint32(name)&63)) != 0
}

// globalLexProp finds a script-level lexical binding, which sits in front of
// the global object: `let x = 1` at a script's top level is reached by name but
// is not a property of globalThis.
func (r *Runtime) globalLexProp(env *Object, name Atom) ownProp {
	if !r.isGlobalScope(env) || !r.lexShadows(name) {
		return ownProp{i: -1}
	}
	return r.globalLex.getOwn(name)
}

// checkGlobalVarName rejects a script-level var or function declaration whose
// name a lexical binding already has.
func (r *Runtime) checkGlobalVarName(name Atom) error {
	if r.globalLex.getOwn(name).ok() {
		return r.throwError(errSyntax, "%q has already been declared", r.atoms.name(name))
	}
	return nil
}

// canDeclareGlobalFunc reports whether a top-level function declaration may
// take a name the global object already has.
//
// A property that can be deleted may always be replaced. One that cannot must
// already look like what the declaration would create -- a writable, enumerable
// data property -- or the declaration cannot honour it.
func (r *Runtime) canDeclareGlobalFunc(name Atom) error {
	p := r.global.getOwn(name)
	if !p.ok() {
		if !r.global.IsExtensible() {
			return r.throwTypeError("cannot declare %q on a non-extensible global",
				r.atoms.name(name))
		}
		return nil
	}
	if p.flags()&propConfigurable != 0 {
		return nil
	}
	if p.flags()&propAccessor != 0 ||
		p.flags()&propWritable == 0 || p.flags()&propEnumerable == 0 {
		return r.throwTypeError("cannot redeclare %q as a function", r.atoms.name(name))
	}
	return nil
}

// tick advances the interrupt counter from outside the interpreter loop.
//
// A built-in that walks a long sequence without running any bytecode -- an
// Array method over an array-like with an enormous length, say -- would
// otherwise be unstoppable, which is exactly the hang the context bound exists
// to prevent.
// nest enters one level of the recursion the call stack's frames do not
// count, refusing one past maxGoRecursion as a call past the depth limit is
// refused. Each nest is matched by an unnest.
func (r *Runtime) nest() error {
	if r.nesting >= maxGoRecursion {
		return r.throwRangeError("maximum call stack size exceeded")
	}
	r.nesting++
	return nil
}

// unnest leaves a level nest entered.
func (r *Runtime) unnest() { r.nesting-- }

func (r *Runtime) tick() error {
	r.interruptCounter--
	if r.interruptCounter <= 0 {
		return r.checkInterruptNow()
	}
	return nil
}

// closeUpvaluesFrom closes every upvalue pointing at or above a local slot,
// which happens when a block that captured bindings exits.
func (r *Runtime) closeUpvaluesFrom(f *frame, slot int) {
	kept := f.openUpvalues[:0]
	for _, u := range f.openUpvalues {
		// Compare addresses to decide whether the upvalue points into the
		// region being released.
		if pointsAtOrAbove(u.slot, f.locals, slot) {
			u.close()
			continue
		}
		kept = append(kept, u)
	}
	f.openUpvalues = kept
}

// pointsAtOrAbove reports whether p refers to locals[slot] or a later slot.
func pointsAtOrAbove(p *Value, locals []Value, slot int) bool {
	for i := slot; i < len(locals); i++ {
		if p == &locals[i] {
			return true
		}
	}
	return false
}

// newClosure builds a closure from a nested function template, capturing the
// upvalues its descriptor names (makeClosure).
func (r *Runtime) newClosure(f *frame, c Value) *Object { return r.newClosureIn(f, c, nil) }

// newClosureIn is newClosure capturing f's captured locals from cells,
// where it is not nil: those of native code whose captured locals are in
// cells of their own, in their locals' order (jitcompile's ir.Program
// Cells, the JIT's GoCells), not open on f's slots.
func (r *Runtime) newClosureIn(f *frame, c Value, cells []*upvalue) *Object {
	tmpl := c.template()
	if tmpl.fn.CapturesByValue && cells == nil {
		// What it takes by value it copies, into cells of its own, which no
		// frame keeps open (newClosureByValue). Native code's locals in cells
		// of their own are shared all the same: what never changes is the
		// same either way.
		return newClosureByValue[struct{}](r, f, tmpl, nil, 0)
	}

	// The object, its function data, its closure and the bindings that closure
	// captures are one allocation: a closure made in a loop is as common as
	// any object literal, and each of the four was a separate one.
	arrow := tmpl.fn.Kind == bytecode.KindArrow
	o, fd, child, upvalues, extra := newScriptFuncObject(
		r.funcProtoFor(tmpl.fn), ClassFunction, len(tmpl.fn.Upvalues), arrow)

	// The environment is inherited, so a function declared in a module sees
	// the module's bindings rather than only the globals.
	*child = closure{
		fn: tmpl.fn, closureCode: tmpl.closureCode,
		realm: r.Realm, env: f.cl.env, upvalues: upvalues,
	}
	// The bindings it is the first to capture share one allocation: a
	// closure made in a loop often captures several of its maker's, each of
	// which was one. Like a scope's context in other engines, the room lives
	// as long as any binding in it does.
	var room []upvalue
	for i, desc := range tmpl.fn.Upvalues {
		if !desc.FromParent {
			upvalues[i] = f.cl.upvalues[desc.Index]
			continue
		}
		if cells != nil {
			upvalues[i] = cells[cellOfLocal(f.cl.fn, int(desc.Index))]
			continue
		}
		u := openUpvalue(f, int(desc.Index))
		if u == nil {
			if len(room) == 0 {
				room = make([]upvalue, uncaptured(f, tmpl.fn.Upvalues[i:]))
			}
			u, room = &room[0], room[1:]
			u.slot = &f.locals[desc.Index]
			f.openUpvalues = append(f.openUpvalues, u)
		}
		upvalues[i] = u
	}

	kind := ctorKindOf(tmpl.fn)
	*fd = funcData{
		closure:  child,
		name:     tmpl.fn.Name,
		length:   int32(tmpl.fn.Length),
		ctorKind: kind,
	}
	// A function created inside a `with` body keeps the objects: the names
	// in its own body resolve against them too, and the frame that pushed
	// them is gone by the time it runs. What a direct eval declared in the
	// enclosing function travels the same way.
	if f.withScopes != nil || f.evalVars != nil {
		if extra == nil {
			extra = &funcExtra{}
		}
		extra.rare = scopesOf(f.withScopes, f.evalVars)
	}
	fd.extra = extra
	if arrow {
		// An arrow captures its surroundings rather than receiving them from
		// the call. Nesting works because an arrow created inside another has
		// already inherited them, so reading the creating frame is enough.
		fd.arrow = true
		fd.extra.lexThis = f.this
		fd.extra.lexThisRef = f.thisRef
		if fd.closure != nil && fd.closure.fn.UsesNewTarget {
			fd.extra.setNewTarget(f.newTarget)
		}
		if f.callee != nil {
			if outer := f.callee.fn(); outer != nil {
				// super resolves against the enclosing method's home object,
				// and super() calls its constructor's parent: the arrow shares
				// the part that says them (funcData's classPart).
				fd.cls = outer.cls
			}
		}
	}

	// A constructible function carries a fresh .prototype object, which is what
	// `new` gives the instance and where a class hangs its methods. An arrow or
	// a method is not constructible and does not get one.
	if instance := r.instanceProtoFor(tmpl.fn); instance != nil {
		// A generator function is not constructible, but it still has a
		// .prototype: it is what the generator objects it produces inherit
		// from. It carries no constructor back-reference, since nothing
		// constructs it.
		proto := newObject(instance, ClassObject)
		o.setOwnRaw(atomPrototype, Obj(proto), propWritable)
		return o
	}
	if kind != ctorNone {
		if tmpl.fn.Kind == bytecode.KindNormal {
			// An ordinary function's prototype is built when something first
			// asks for it. Most functions are called rather than constructed
			// from, and the object and its back-reference are two allocations
			// per closure that nothing ever looks at.
			fd.protoPending = true
			return o
		}
		proto := newObject(r.proto.object, ClassObject)
		proto.setOwnRaw(atomConstructor, Obj(o), propWritable|propConfigurable)
		flags := propWritable
		switch tmpl.fn.Kind {
		case bytecode.KindConstructor, bytecode.KindDerivedConstructor:
			// A class's prototype cannot be replaced, which is what makes a
			// static member named "prototype" an error.
			flags = 0
		}
		o.setOwnRaw(atomPrototype, Obj(proto), flags)
		// A constructor's home object is its own prototype, which is what
		// makes `super.m()` inside a constructor find the parent's method.
		// Nothing outside a class can name super, so setting it on every
		// constructible function is harmless.
		fd.setHomeObject(proto)
	}
	return o
}

// materializeFunctionProto creates the .prototype an ordinary function was
// promised, as a real own property that anything reading the property table
// finds.
//
// It is deferred rather than skipped: the object's identity has to be stable,
// so the first thing to ask for it is what decides what it is, and everything
// afterwards sees the same one.
func (r *Runtime) materializeFunctionProto(o *Object) {
	fd := o.fn()
	if fd == nil || !fd.protoPending {
		return
	}
	fd.protoPending = false
	// It is the function's realm's object, whichever realm reads it first.
	objProto := r.proto.object
	if fd.closure != nil && fd.closure.realm != nil {
		objProto = fd.closure.realm.proto.object
	}
	proto := newObject(objProto, ClassObject)
	proto.setOwnRaw(atomConstructor, Obj(o), propWritable|propConfigurable)
	// It goes where it would have been had the function been built with it:
	// after length and name, which come first whenever they are created, and
	// before anything a script has added since. An ownKeys walk reports the
	// table's order, so building it late may not reorder it.
	at := 0
	for at < int(o.valn) &&
		(o.shape.keys[at].key == atomLength || o.shape.keys[at].key == atomName) {
		at++
	}
	o.insertProp(at, Property{
		key: atomPrototype, value: Obj(proto), flags: propWritable,
	})
	// A constructible function's home object is its own prototype. Nothing
	// outside a class can name super, so an ordinary function's is never read,
	// but it costs nothing to keep it the same as an eagerly built one's.
	fd.setHomeObject(proto)
}

func ctorKindOf(fn *bytecode.Function) ctorKind {
	// A generator or async function has a prototype property but is not a
	// constructor: there is no object for `new` to build, since calling one
	// produces an iterator or a promise.
	if fn.Generator || fn.Async {
		return ctorNone
	}
	switch fn.Kind {
	case bytecode.KindNormal:
		return ctorBase
	case bytecode.KindConstructor:
		return ctorBase
	case bytecode.KindDerivedConstructor:
		return ctorDerived
	}
	return ctorNone
}

// derivedResult settles what a derived constructor hands back: an object it
// returned itself, or the object super() built.
//
// The object has to exist by the time the constructor returns, whatever route
// the return took -- which is why this is at the return rather than at the
// return statement, where a finally clause may still call super() afterwards.
// An arrow written inside one shares the binding but is not the constructor,
// so its kind is what tells them apart.
func (r *Runtime) derivedResult(f *frame, cl *closure, v Value) (Value, error) {
	if v.IsObject() || cl.fn.Kind != bytecode.KindDerivedConstructor {
		return v, nil
	}
	if !v.IsUndefined() {
		// Returning anything else is a TypeError, but not one the constructor
		// can catch: it is raised by the construction, after the body has
		// finished. The value is handed out as it is so that the caller can
		// report it from there.
		return v, nil
	}
	if !f.thisRef.init {
		return Undefined, errNoSuper
	}
	return f.thisRef.value, nil
}

// declareOnScope declares a global var or function in a context, as node's
// contexts do. Both are the realm's global object's, as any script's are; a
// function is the sandbox's too, where the context's code reads it from and a
// deletion can take it away, while a var reaches the sandbox only when it is
// assigned. A var the context already has is left as it is; a function
// replaces what is there, and is assigned where it cannot be redefined.
func (r *Runtime) declareOnScope(env *Object, name Atom, v Value, flags propFlags, replace bool) error {
	p := proxyOf(env)
	if !replace {
		d, err := r.ownDescriptorOf(env, name)
		if err != nil || !d.IsUndefined() {
			return err
		}
	}
	if own := p.target.getOwn(name); !own.ok() || own.flags()&propConfigurable != 0 {
		p.target.setOwnRaw(name, v, flags|propEnumerable)
	}
	if !replace {
		return nil
	}
	ok, err := r.defineProperty(p.sandbox, name, &propDesc{
		value: v, hasValue: true,
		writable: true, hasWritable: true,
		enumerable: true, hasEnumerable: true,
		configurable: true, hasConfigurable: true,
	})
	if err != nil || ok {
		return err
	}
	_, err = r.setProp(p.sandbox, name, v, Obj(p.sandbox), false)
	return err
}

// errNoSuper is a derived constructor finishing without having called super().
// It is not an exception the body can catch: the construction fails once the
// body is done, and the ReferenceError it becomes is the caller's realm's.
var errNoSuper = errors.New("a derived constructor must call super() before returning")

// captureLocal returns the upvalue for a local slot, reusing an existing one so
// that two closures over the same variable share it.
func (r *Runtime) captureLocal(f *frame, slot int) *upvalue {
	for _, u := range f.openUpvalues {
		if u.slot == &f.locals[slot] {
			return u
		}
	}
	u := &upvalue{slot: &f.locals[slot]}
	f.openUpvalues = append(f.openUpvalues, u)
	return u
}

// openUpvalue is the upvalue already open on a local slot, or nil.
func openUpvalue(f *frame, slot int) *upvalue {
	for _, u := range f.openUpvalues {
		if u.slot == &f.locals[slot] {
			return u
		}
	}
	return nil
}

// cellOfLocal is captured local slot's index among fn's captured locals,
// in their order: its cell's, where they are in cells of their own
// (newClosureIn).
func cellOfLocal(fn *bytecode.Function, slot int) int {
	n := 0
	for _, l := range fn.Locals[:slot] {
		if l.Captured {
			n++
		}
	}
	return n
}

// uncaptured counts the bindings of a frame's that the upvalues descs name
// and no upvalue is open on yet.
func uncaptured(f *frame, descs []bytecode.UpvalueDesc) int {
	n := 0
	for _, desc := range descs {
		if desc.FromParent && openUpvalue(f, int(desc.Index)) == nil {
			n++
		}
	}
	return n
}

// newArrayFrom builds an array holding a copy of the given values.
func (r *Runtime) newArrayFrom(vals []Value) *Object {
	o := newArrayObject(r.proto.array, len(vals))
	copy(o.elemsOf(), vals)
	return o
}

// stringKeyRead is o[s] for a string s the runtime has handed out as a key
// before (keyAtoms), where o and each of its prototypes up to the one
// holding the key are ordinary objects holding it, if at all, as a plain
// data property: the walk getProp makes, without getIndexed's conversion
// of the key and its questions of each object's kind. A symbol table kept
// in an object -- Typescript's StringHashTable, this.table[key] -- is read
// so, most often for a key it does not have, which every prototype is
// asked for. A key whose atom is an array index is an element of o's dense
// storage, read as getIndexed reads one by its number, which names the
// same property: a[k] in for (k in a), whose keys are an array's indices.
// It reports false for anything else.
func (r *Runtime) stringKeyRead(o *Object, s *String) (Value, bool) {
	e := &r.keyAtoms[keySlot(s)]
	if e.s != s || e.none {
		return Undefined, false
	}
	key := e.atom
	if key.IsIndex() {
		if i := key.Index(); uint(i) < uint(len(o.elemsOf())) && o.flags&objMappedArguments == 0 {
			if v := o.elemsOf()[i]; !isHole(v) {
				return v, true
			}
		}
		return Undefined, false
	}
	for p := o; p != nil; p = p.proto() {
		if p.class != ClassObject {
			return Undefined, false
		}
		if i := p.findOwn(key); i >= 0 {
			if p.shape.keys[i].flags&(propAccessor|propPrivate|propDeleted|propUninit) != 0 {
				return Undefined, false
			}
			return p.vals()[i], true
		}
	}
	return Undefined, true
}

// getIndexed reads a computed property, taking a fast path for an array index
// on a dense array.
func (r *Runtime) getIndexed(obj, key Value) (Value, error) {
	if obj.IsNullish() {
		// The base is checked before the key is converted: a key whose
		// toString throws must not run at all when there is nothing to read
		// from.
		return Undefined, r.throwTypeError("cannot read property of %s", r.describe(obj))
	}
	if obj.IsObject() && key.IsNumber() {
		o := obj.Object()
		// A mapped arguments object's indices are not what its dense storage
		// says, so it has no fast path.
		if i := uint32(key.Number()); float64(i) == key.Number() &&
			uint(i) < uint(len(o.elemsOf())) && o.flags&objMappedArguments == 0 {
			if v := o.elemsOf()[i]; !isHole(v) {
				return v, nil
			}
		}
		if v, ok := typedRead(obj, key.num); ok {
			return v, nil
		}
		if t, i, ok := typedElemIndex(o, key.num); ok {
			return t.getElem(i), nil
		}
	}
	n, err := r.toPropertyName(key)
	if err != nil {
		return Undefined, err
	}
	o := r.protoOfPrimitive(obj)
	if obj.IsObject() {
		o = obj.Object()
	}
	k, ok := r.keyFor(o, n)
	if !ok {
		return Undefined, nil
	}
	return r.getValueProp(obj, k)
}

// setIndexed assigns to a computed property, where the dense storage of an
// array has not taken the store.
func (r *Runtime) setIndexed(obj, key, val Value, strict bool) error {
	if obj.IsNullish() {
		// The base is checked before the key is converted, as for a read.
		return r.throwTypeError("cannot set property of %s", r.describe(obj))
	}
	if obj.IsObject() && key.IsNumber() {
		// A typed array is its own receiver here, so an integer index is a
		// write to its buffer -- unless the buffer is immutable, whose refusal
		// the long way words.
		if t, i, ok := typedElemIndex(obj.Object(), key.num); ok {
			if st, _ := t.buffer.dataOf().(*arrayBufferData); st != nil && !st.immutable {
				return r.setElem(t, i, val)
			}
		}
		if appendElem(obj.Object(), key.num, val) || fillHole(obj.Object(), key.num, val) ||
			setOrdinaryIndex(obj.Object(), key.num, val) {
			return nil
		}
	}
	k, err := r.toPropertyKey(key)
	if err != nil {
		return err
	}
	if t := r.shapes; t != nil && obj.IsObject() && !k.IsIndex() {
		// A keyed store: what it adds to the object past the soft limit
		// makes it a dictionary (shapeAdded).
		was := t.keyed
		t.keyed = obj.Object()
		err := r.setValueProp(obj, k, val, strict)
		t.keyed = was
		return err
	}
	return r.setValueProp(obj, k, val, strict)
}

// appendElem stores v at index n of an array whose length is n, as an
// assignment would, and reports whether it did. That is the assignment when
// the array may grow -- extensible, its length writable, dense, no index in
// its own table to be asked first -- and nothing up the chain has an element
// or an index in its table, to hold a setter or a read-only one.
func appendElem(o *Object, n float64, v Value) bool {
	const want = objExtensible | objArrayLengthWritable
	i := uint32(n)
	if o.class != ClassArray || float64(i) != n || uint(i) != uint(len(o.elemsOf())) ||
		o.flags&(want|objHasSparseElements|objMappedArguments|objSharedElems) != want || !o.noIndexKeys() {
		return false
	}
	for p := o.proto(); p != nil; p = p.proto() {
		if p.class != ClassObject && p.class != ClassArray || len(p.elemsOf()) != 0 || !p.noIndexKeys() {
			return false
		}
	}
	// What setElem would do for an index at the length of an array that
	// is not an arguments object.
	els := o.elemsOf()
	end := len(els)
	if end == cap(els) {
		appendGrown[struct{}](o, v)
		return true
	}
	els = els[:end+1]
	els[end] = v
	o.extOf().elems = els
	return true
}

// fillHole stores v at index n of an array whose element n is a hole, as an
// assignment would, and reports whether it did: what appendElem asks of
// the array but a writable length, and of its chain, makes the assignment
// a new element's -- new Array(n) leaves n holes to be filled.
func fillHole(o *Object, n float64, v Value) bool {
	// A length that is not writable does not stop it: the length stays.
	const want = objExtensible
	i := uint32(n)
	if o.class != ClassArray || float64(i) != n || uint(i) >= uint(len(o.elemsOf())) || !isHole(o.elemsOf()[i]) ||
		o.flags&(want|objHasSparseElements|objMappedArguments) != want || !o.noIndexKeys() {
		return false
	}
	for p := o.proto(); p != nil; p = p.proto() {
		if p.class != ClassObject && p.class != ClassArray || len(p.elemsOf()) != 0 || !p.noIndexKeys() {
			return false
		}
	}
	o.elemsOf()[i] = v
	return true
}

// setOrdinaryIndex stores v at index n of o, an ordinary object, as an
// assignment would, where that runs nothing and throws nothing, and
// reports whether it did: o's own writable data property is written, or a
// new one added to o -- extensible, with no element, nothing up its chain
// having an element or an index (noneIndexed) -- along its layout's
// transition, as setProp's walk would have ended by adding it. An object
// filled in as a table of indices had each of its stores walk there,
// asking its table twice whether it had the key.
func setOrdinaryIndex(o *Object, n float64, v Value) bool {
	i := uint32(n)
	if o.class != ClassObject || float64(i) != n || i >= atomIndexTag || len(o.elemsOf()) != 0 {
		return false
	}
	// The atom of an index short enough for one to carry, as
	// numberToAtom makes it; -0 is 0's.
	key := Atom(atomIndexTag | i)
	if j := o.findOwn(key); j >= 0 {
		if o.shape.keys[j].flags&(propPrivate|propAccessor|propUninit|propWritable) != propWritable {
			return false
		}
		o.vals()[j] = v
		return true
	}
	if o.flags&objExtensible == 0 || !noneIndexed(o.proto()) {
		return false
	}
	o.appendProp(Property{key: key, flags: propDefault, value: v})
	return true
}

// construct implements the `new` operator.
func (r *Runtime) construct(callee Value, args []Value) (Value, error) {
	return r.constructWithTarget(callee, args, callee)
}

// isConstructor reports whether a value responds to new.
func isConstructor(v Value) bool {
	if !v.IsObject() || v.Object() == nil {
		return false
	}
	o := v.Object()
	if p := proxyOf(o); p != nil {
		return p.constructor
	}
	fd := o.fn()
	return fd != nil && fd.ctorKind != ctorNone
}

// constructWithTarget builds an object with one constructor while another
// decides what it is.
//
// The two differ whenever a subclass is involved: `new D()` on a class derived
// from B eventually runs B's constructor, but the object is a D, so the
// prototype comes from D. Reflect.construct exposes the same split directly.
func (r *Runtime) constructWithTarget(callee Value, args []Value, newTarget Value) (Value, error) {
	if !callee.IsObject() || callee.Object() == nil {
		return Undefined, r.throwTypeError("%s is not a constructor", r.describe(callee))
	}
	o := callee.Object()
	if p := proxyOf(o); p != nil {
		return r.proxyConstruct(p, args, newTarget)
	}
	fd := o.fn()
	if fd == nil || fd.ctorKind == ctorNone {
		return Undefined, r.throwTypeError("%s is not a constructor", fd.nameOr("value"))
	}

	// A bound function that is its own new.target hands the target on, one
	// link at a time -- which is what decides both the prototype of the object
	// being built and the new.target the innermost function sees.
	for at := o; ; {
		fd := at.fn()
		if fd == nil || !fd.bound {
			break
		}
		if newTarget.IsObject() && newTarget.Object() == at {
			newTarget = Obj(fd.extra.rare.boundTarget)
		}
		at = fd.extra.rare.boundTarget
	}

	// The new object's prototype comes from new.target's .prototype property,
	// falling back to Object.prototype when that is not an object.
	target := o
	if newTarget.IsObject() {
		target = newTarget.Object()
	}

	// A native constructor builds its own object, so there is nothing to make
	// here -- and reading new.target's prototype now would be out of order: a
	// built-in checks its arguments first, and the read is observable.
	if fd.native != nil {
		return constructNativeAt(r, o, args, newTarget, target != o)
	}

	// A function's own data property is what the read would find, and is
	// read from its table. A proxy, an accessor, or a prototype still to be
	// made is read the general way.
	var protoVal Value
	var err error
	if p := target.getOwnVisible(atomPrototype); p.ok() && target.class == ClassFunction && p.flags()&propAccessor == 0 {
		protoVal = p.value()
	} else if protoVal, err = r.getProp(target, atomPrototype, Obj(target)); err != nil {
		return Undefined, err
	}
	var proto *Object
	if protoVal.IsObject() {
		proto = protoVal.Object()
	} else {
		// A constructor that named no prototype falls back to the one of the
		// realm it came from -- and a revoked proxy no longer has a realm to
		// be asked about.
		if proto, err = r.protoForNewTarget(target, r.proto.object); err != nil {
			return Undefined, err
		}
	}
	// The object is made with room for what the constructor is about to put in
	// it, which the compiler counted. A bound function has no body of its own
	// to have counted, so its object starts with none.
	// The constructor's objects start from a shape of its own, which
	// remembers the room they came to need, where its body did not say.
	props := 0
	var root *shape
	if fd.closure != nil {
		root = r.shapes.ctorRoot(fd, proto)
		props = root.roomFor(int(fd.closure.fn.ThisProps))
	}
	obj := newOrdinary[struct{}](fd, root, proto, props)
	this := Obj(obj)

	// A base class gives the instance its private methods and fields before the
	// constructor body runs, so the body finds them already there. A derived one
	// has no instance yet: its super() does this once the parent hands one back.
	if fd.ctorKind != ctorDerived {
		if err := r.initInstanceElements(o, this); err != nil {
			return Undefined, err
		}
	}
	var res Value
	if !fd.bound && fd.closure != nil && fd.closure.realm == r.Realm {
		// A compiled constructor of this realm is run directly: nothing
		// callObject would ask of it -- a proxy, a bound target, a generator,
		// a realm to switch to -- applies.
		if err = r.tick(); err == nil {
			// So is a constructor that only stores its parameters in this,
			// and one that only hands them on to a method of it.
			done := false
			switch leafKind(fd.closure) {
			case bytecode.LeafNone, bytecode.LeafPure:
			case bytecode.LeafForward:
				done, err = r.leafForward(fd.closure, o, obj, args, newTarget)
			default:
				res, done = r.leafCall(fd.closure, obj, args)
			}
			if !done {
				res, err = constructRun(r, this, args, newTarget, o, fd)
				if err == errNoSuper {
					err = r.throwError(errReference, "%s", errNoSuper.Error())
				}
				if !fd.planned {
					planCall(fd)
				}
			}
		}
	} else {
		res, err = r.callObject(o, this, args, newTarget)
	}
	if err != nil {
		return Undefined, err
	}
	if fd.ctorKind != ctorDerived {
		// What the object came to have, for the room the constructor's
		// next ones are made with.
		root.noteBuilt(int(obj.valn))
	}
	// A constructor that returns an object overrides the newly created one;
	// any other return value is ignored -- except in a derived constructor,
	// where returning something that is neither an object nor undefined is a
	// TypeError. It is raised here rather than at the return so that a try
	// inside the constructor cannot catch it: the body has already finished.
	if res.IsObject() {
		return res, nil
	}
	if fd.ctorKind == ctorDerived {
		return Undefined, r.throwTypeError(errDerivedReturn)
	}
	return this, nil
}

// errDerivedReturn is the TypeError a derived constructor's construction
// throws for a result neither an object nor undefined.
const errDerivedReturn = "a derived constructor may return only an object or undefined"

// constructNative runs a built-in constructor, which makes its own object
// because an Error needs a stack and an Array needs array storage.
//
// Most of them know nothing of new.target, so the prototype it chose is put
// back afterwards -- which is what makes Reflect.construct(Array, [], F)
// produce an F. The ones that do know ask through protoFromNewTarget, and the
// read that answers them is the only one: doing it again here would run a
// prototype getter twice.
func (r *Runtime) constructNative(o *Object, args []Value, newTarget Value, derived bool) (Value, error) {
	saved := r.usedNewTargetProto
	r.usedNewTargetProto = false
	res, err := r.callObject(o, Undefined, args, newTarget)
	askedItself := r.usedNewTargetProto
	r.usedNewTargetProto = saved
	if err != nil {
		return Undefined, err
	}
	if !res.IsObject() {
		// Nothing was built, which only a constructor that refuses to be one
		// reaches. An ordinary object stands in for the `this` a scripted
		// constructor would have had.
		return Obj(newObject(r.proto.object, ClassObject)), nil
	}
	if !derived || askedItself {
		return res, nil
	}
	protoVal, err := r.getProp(newTarget.Object(), atomPrototype, newTarget)
	if err != nil {
		return Undefined, err
	}
	if protoVal.IsObject() {
		res.Object().setProto(protoVal.Object())
		return res, nil
	}
	// The constructor chose its own realm's prototype; new.target's realm
	// may have another.
	from, err := r.functionRealm(o)
	if err != nil {
		return Undefined, err
	}
	to, err := r.functionRealm(newTarget.Object())
	if err != nil {
		return Undefined, err
	}
	res.Object().setProto(r.counterpart(from, to, res.Object().proto()))
	return res, nil
}

func (fd *funcData) nameOr(fallback string) string {
	if fd == nil || fd.name == "" {
		return fallback
	}
	return fd.name
}

// call2 calls fn with two arguments, which are passed on the runtime's
// argument stack rather than in a slice made for the call. No callee keeps
// the slice it is given -- the interpreter's own calls give it a piece of
// their stack, and a generator copies its arguments -- so any fn will do.
func (r *Runtime) call2(fn, this, a, b Value) (Value, error) {
	i := len(r.argStack)
	r.argStack = append(r.argStack, a, b)
	res, err := r.call(fn, this, r.argStack[i:i+2:i+2])
	r.argStack[i], r.argStack[i+1] = Undefined, Undefined
	r.argStack = r.argStack[:i]
	return res, err
}

// callIntrinsic1 calls a function with one argument, as call does, but for
// one of the realm's own built-ins that reads its argument before anything
// else can run and keeps nothing of it, when the argument list comes from a
// stack the runtime keeps rather than being allocated for the call. A call
// made while this one runs stacks its own above it; one that grows the stack
// leaves this call's list where it was, unchanged.
func (r *Runtime) callIntrinsic1(fn *Object, this, a Value) (Value, error) {
	i := len(r.argStack)
	r.argStack = append(r.argStack, a)
	res, err := r.call(Obj(fn), this, r.argStack[i:i+1:i+1])
	r.argStack[i] = Undefined
	r.argStack = r.argStack[:i]
	return res, err
}

// ordinaryHasInstance answers what Function.prototype[Symbol.hasInstance]
// would, where it can be answered without running anything a script could
// see: c an ordinary function, and v a primitive -- which no function has
// as an instance, and which OrdinaryHasInstance answers before it reads the
// prototype -- or an object whose chain is free of proxies, whose traps are
// code, with c's own prototype a data property holding an object. It
// reports false in ok for anything else, which has the method called.
func ordinaryHasInstance(c *Object, v Value) (yes, ok bool) {
	fd := c.fn()
	if fd == nil || fd.bound || proxyOf(c) != nil {
		return false, false
	}
	if !v.IsObject() {
		return false, true
	}
	p := c.getOwnVisible(atomPrototype)
	if !p.ok() || p.flags()&propAccessor != 0 || !p.value().IsObject() {
		return false, false
	}
	target := p.value().Object()
	for o := v.Object(); ; {
		if proxyOf(o) != nil {
			return false, false
		}
		if o = o.proto(); o == nil {
			return false, true
		}
		if o == target {
			return true, true
		}
	}
}

// instanceOf implements the instanceof operator.
func (r *Runtime) instanceOf(obj, ctor Value) (bool, error) {
	return r.instanceOfAt(nil, obj, ctor)
}

// instanceOfAt is instanceOf with the cache of the instanceof operator's
// read of Symbol.hasInstance, or nil.
func (r *Runtime) instanceOfAt(ic *propCache, obj, ctor Value) (bool, error) {
	if !ctor.IsObject() {
		return false, r.throwTypeError("the right operand of \"instanceof\" must be an object")
	}
	c := ctor.Object()
	if yes, ok := r.instanceOfKnown(c, obj); ok {
		if ic != nil {
			// The site's cache is kept as the lookup below keeps it: the
			// register interpreter's native handler answers from it alone,
			// and comes here each time it misses.
			r.cachedProp(ic, c, r.hasInstanceAtom)
		}
		return yes, nil
	}

	// A Symbol.hasInstance method overrides the default behaviour. Where the
	// constructor is one the operator has met, the cache says where it is.
	hasInstance, found := Undefined, false
	if ic != nil {
		hasInstance, found = r.cachedProp(ic, c, r.hasInstanceAtom)
	}
	if !found {
		var err error
		if hasInstance, err = r.getProp(c, r.hasInstanceAtom, ctor); err != nil {
			return false, err
		}
	}
	var err error
	if !hasInstance.IsNullish() {
		var res Value
		if hasInstance.IsObject() && hasInstance.Object() == r.hasInstanceFn {
			if yes, ok := ordinaryHasInstance(c, obj); ok {
				if obj.IsObject() {
					r.noteInstanceOf(c)
				}
				return yes, nil
			}
			res, err = r.callIntrinsic1(r.hasInstanceFn, ctor, obj)
		} else {
			res, err = r.call(hasInstance, ctor, []Value{obj})
		}
		if err != nil {
			return false, err
		}
		return res.Truthy(), nil
	}

	if !c.IsCallable() {
		return false, r.throwTypeError("the right operand of \"instanceof\" is not callable")
	}
	if !obj.IsObject() {
		return false, nil
	}
	protoVal, err := r.getProp(c, atomPrototype, ctor)
	if err != nil {
		return false, err
	}
	if !protoVal.IsObject() {
		return false, r.throwTypeError("the prototype of the right operand is not an object")
	}
	target := protoVal.Object()
	// The chain is walked by asking each object for its prototype, so a proxy
	// in it runs its trap rather than being read around.
	for o := obj.Object(); ; {
		next, err := r.protoOf(o)
		if err != nil {
			return false, err
		}
		if !next.IsObject() {
			return false, nil
		}
		o = next.Object()
		if o == target {
			return true, nil
		}
	}
}

// ---------------------------------------------------------------------------
// Operator helpers
// ---------------------------------------------------------------------------

// add implements the + operator, which is the only arithmetic operator that
// also concatenates.
func (r *Runtime) add(a, b Value) (Value, error) {
	if a.isShortBig() && b.isShortBig() {
		if v, ok := shortArith(bytecode.OpAdd, a, b); ok {
			return v, nil
		}
	}
	if a.IsBigInt() && b.IsBigInt() {
		// Two BigInts, which convert to themselves.
		return r.bigOp(bytecode.OpAdd, a, b)
	}
	if a.IsString() && b.IsString() {
		// Two strings, as most string building is: what toPrimitive and
		// toString would hand back as they are.
		s, err := r.concat(a.String(), b.String())
		if err != nil {
			return Undefined, err
		}
		return Str(s), nil
	}
	// What converts an object b may append to the binding a was read from.
	if b.IsObject() {
		strFreeze[struct{}](a)
	}
	pa, err := r.toPrimitive(a, hintDefault)
	if err != nil {
		return Undefined, err
	}
	pb, err := r.toPrimitive(b, hintDefault)
	if err != nil {
		return Undefined, err
	}
	// If either side is a string after coercion, the result is a string.
	if pa.IsString() || pb.IsString() {
		if pa.IsString() && pb.IsNumber() {
			if s, ok := r.concatInt(pa.String(), pb.num, false); ok {
				return Str(s), nil
			}
		} else if pb.IsString() && pa.IsNumber() {
			if s, ok := r.concatInt(pb.String(), pa.num, true); ok {
				return Str(s), nil
			}
		}
		sa, err := r.toString(pa)
		if err != nil {
			return Undefined, err
		}
		sb, err := r.toString(pb)
		if err != nil {
			return Undefined, err
		}
		s, err := r.concat(sa, sb)
		if err != nil {
			return Undefined, err
		}
		return Str(s), nil
	}
	if pa.IsBigInt() && pb.IsBigInt() {
		// What toNumeric would leave as they are.
		return r.bigOp(bytecode.OpAdd, pa, pb)
	}
	na, err := r.toNumeric(pa)
	if err != nil {
		return Undefined, err
	}
	nb, err := r.toNumeric(pb)
	if err != nil {
		return Undefined, err
	}
	if na.IsBigInt() != nb.IsBigInt() {
		return Undefined, r.throwTypeError("cannot mix BigInt and other types")
	}
	if na.IsBigInt() {
		return r.bigOp(bytecode.OpAdd, na, nb)
	}
	return Float(na.Number() + nb.Number()), nil
}

// concatInt is s + String(f), or with numFirst String(f) + s, for a safe
// integer f, made in one allocation where the result is short enough for
// Concat to have copied it: the number's own string is never made. It reports
// false for anything else -- a long result, a rope, a runtime with a memory
// limit -- which is left to toString and concat.
//
// The digits are ASCII, so no surrogate can pair across the join, and only
// the string's own end carries an unpaired one to the result's.
func (r *Runtime) concatInt(s *String, f float64, numFirst bool) (*String, bool) {
	i := int64(f)
	if float64(i) != f || f <= -1<<53 || f >= 1<<53 || s.left != nil || r.meter != nil {
		return nil, false
	}
	if s.length == 0 {
		// n + "" is the number's string, which below 1024 the runtime
		// keeps.
		return r.numberString(f), true
	}
	var buf [24]byte
	d := strconv.AppendInt(buf[:0], i, 10)
	n := len(s.s) + len(d)
	if n >= ropeThreshold {
		return nil, false
	}
	out, b := newStringBytes(n)
	if numFirst {
		copy(b[copy(b, d):], s.s)
		out.endsHigh = s.endsHigh
	} else {
		copy(b[copy(b, s.s):], d)
		out.startsLow = s.startsLow
	}
	out.s = unsafe.String(unsafe.SliceData(b), n)
	out.length, out.ascii = s.length+len(d), s.ascii
	return out, true
}

// arith implements the arithmetic operators other than +.
func (r *Runtime) arith(op bytecode.Op, a, b Value) (Value, error) {
	if a.isShortBig() && b.isShortBig() {
		if v, ok := shortArith(op, a, b); ok {
			return v, nil
		}
	}
	if a.IsBigInt() && b.IsBigInt() {
		// Two BigInts, which convert to themselves.
		return r.bigOp(op, a, b)
	}
	na, err := r.toNumeric(a)
	if err != nil {
		return Undefined, err
	}
	nb, err := r.toNumeric(b)
	if err != nil {
		return Undefined, err
	}
	if na.IsBigInt() != nb.IsBigInt() {
		return Undefined, r.throwTypeError("cannot mix BigInt and other types")
	}
	if na.IsBigInt() {
		return r.bigOp(op, na, nb)
	}
	return Float(numericOp(op, na.Number(), nb.Number())), nil
}

// int32Op applies a bitwise or shift operator to two numbers.
func int32Op(op bytecode.Op, x, y float64) Value {
	switch op {
	case bytecode.OpBitAnd:
		return Int32(jsnum.ToInt32(x) & jsnum.ToInt32(y))
	case bytecode.OpBitOr:
		return Int32(jsnum.ToInt32(x) | jsnum.ToInt32(y))
	case bytecode.OpBitXor:
		return Int32(jsnum.ToInt32(x) ^ jsnum.ToInt32(y))
	case bytecode.OpShl:
		// Only the low five bits of the shift count are used.
		return Int32(jsnum.ToInt32(x) << (jsnum.ToUint32(y) & 31))
	case bytecode.OpShr:
		return Int32(jsnum.ToInt32(x) >> (jsnum.ToUint32(y) & 31))
	default:
		return Uint32(jsnum.ToUint32(x) >> (jsnum.ToUint32(y) & 31))
	}
}

// bitwise applies a bitwise or shift operator to operands that are not both
// numbers. Each is coerced once -- a valueOf runs once, as it does in every
// engine -- and a pair of BigInts has the BigInt operator.
func (r *Runtime) bitwise(op bytecode.Op, a, b Value) (Value, error) {
	if a.isShortBig() && b.isShortBig() && op != bytecode.OpUShr {
		if v, ok := shortBitwise(op, a, b); ok {
			return v, nil
		}
	}
	if a.IsBigInt() && b.IsBigInt() && op != bytecode.OpUShr {
		// Two BigInts, which convert to themselves.
		return r.bigBits(op, a, b)
	}
	na, err := r.toNumeric(a)
	if err != nil {
		return Undefined, err
	}
	nb, err := r.toNumeric(b)
	if err != nil {
		return Undefined, err
	}
	if na.IsBigInt() || nb.IsBigInt() {
		if op == bytecode.OpUShr {
			// Unsigned shift has no BigInt form: a BigInt has no width for
			// the sign bit to be shifted out of.
			return Undefined, r.throwTypeError("BigInt has no unsigned right shift")
		}
		v, _, err := r.bigBitwise(op, na, nb)
		return v, err
	}
	return int32Op(op, na.Number(), nb.Number()), nil
}

// binImm is OpBinImm for a left operand that is not a number: what the
// operator's own instruction does then.
func (r *Runtime) binImm(op bytecode.Op, a, b Value) (Value, error) {
	if a.isShortBig() && b.isShortBig() {
		if v, ok := shortBinImm(op, a, b); ok {
			return v, nil
		}
	}
	switch op {
	case bytecode.OpAdd:
		return r.add(a, b)
	case bytecode.OpSub, bytecode.OpMul:
		return r.arith(op, a, b)
	}
	return r.bitwise(op, a, b)
}

// bigBitwise applies a bitwise or shift operator when either side may be a
// BigInt, reporting whether it handled the operation.
//
// It reports false when both sides turn out to be numbers, leaving the caller
// to take its own faster path.
func (r *Runtime) bigBitwise(op bytecode.Op, a, b Value) (Value, bool, error) {
	na, err := r.toNumeric(a)
	if err != nil {
		return Undefined, false, err
	}
	nb, err := r.toNumeric(b)
	if err != nil {
		return Undefined, false, err
	}
	if !na.IsBigInt() && !nb.IsBigInt() {
		return Undefined, false, nil
	}
	if na.IsBigInt() != nb.IsBigInt() {
		return Undefined, false, r.throwTypeError("cannot mix BigInt and other types")
	}
	if na.isShortBig() && nb.isShortBig() {
		if v, ok := shortBitwise(op, na, nb); ok {
			return v, true, nil
		}
	}

	v, err := r.bigBits(op, na, nb)
	return v, true, err
}

// negate implements unary minus.
func (r *Runtime) negate(a Value) (Value, error) {
	n, err := r.toNumeric(a)
	if err != nil {
		return Undefined, err
	}
	if n.IsBigInt() {
		if n.isInt64Big() && n.int64Big() != math.MinInt64 {
			return shortBig(-n.int64Big()), nil
		}
		// The most negative int64's, or one on the heap.
		return bigNeg(n), nil
	}
	return Float(-n.Number()), nil
}

// numericOp applies a binary arithmetic operator to two float64 values.
func numericOp(op bytecode.Op, a, b float64) float64 {
	switch op {
	case bytecode.OpAdd:
		return a + b
	case bytecode.OpSub:
		return a - b
	case bytecode.OpMul:
		return a * b
	case bytecode.OpDiv:
		return a / b
	case bytecode.OpMod:
		return jsMod(a, b)
	case bytecode.OpPow:
		return jsPow(a, b)
	}
	return math.NaN()
}

// jsMod is the % operator on numbers, which truncates and keeps the sign of the
// dividend, as math.Mod does. So does Go's % on integers, without math.Mod's
// work over the exponents, for the positive int32 dividend and int32 divisor
// most code has; a dividend of zero or less is left to math.Mod, which gives
// -0 for one that is negative and divides exactly.
func jsMod(a, b float64) float64 {
	if ai, bi := int32(a), int32(b); float64(ai) == a && float64(bi) == b && ai > 0 && bi != 0 {
		return float64(ai % bi)
	}
	return math.Mod(a, b)
}

// jsPow implements the ** operator and Math.pow as V8 does: the C library's
// pow -- here Arm's, from its Optimized Routines, which glibc ships and so
// Node on Linux uses -- after the cases where JavaScript's differs from C's,
// and the two V8 answers without calling it.
func jsPow(a, b float64) float64 {
	switch {
	case math.IsNaN(b):
		// 1 ** NaN is 1 in C.
		return math.NaN()
	case (a == 1 || a == -1) && math.IsInf(b, 0):
		return math.NaN()
	case b == 2:
		return a * a
	case b == 0.5:
		if math.IsInf(a, 0) {
			return math.Inf(1)
		}
		// +0 for -0, where sqrt keeps the sign.
		return math.Sqrt(a + 0)
	}
	return fdlibm.PowC(a, b)
}

// compareFloats evaluates a relational operator on two numbers.
func compareFloats(op bytecode.Op, a, b float64) bool {
	switch op {
	case bytecode.OpLt:
		return a < b
	case bytecode.OpLe:
		return a <= b
	case bytecode.OpGt:
		return a > b
	default:
		return a >= b
	}
}

// relationalResult maps a three-way comparison to the boolean a relational
// operator produces. An undefined comparison, which arises from NaN, makes
// every operator false.
func relationalResult(op bytecode.Op, c cmpResult) bool {
	if c == cmpUndefined {
		return false
	}
	switch op {
	case bytecode.OpLt:
		return c == cmpLess
	case bytecode.OpLe:
		return c == cmpLess || c == cmpEqual
	case bytecode.OpGt:
		return c == cmpGreater
	default:
		return c == cmpGreater || c == cmpEqual
	}
}

// linkClass wires a derived class to its parent.
//
// Two chains are established. The prototypes are linked so that an instance
// inherits the parent's methods, and the constructors are linked so that a
// static method is visible on the subclass -- which is the part that surprises
// people, since it has no analogue in most class systems.
func (r *Runtime) linkClass(ctorVal, parent Value) error {
	if !ctorVal.IsObject() {
		return r.throwTypeError("a class constructor must be a function")
	}
	ctor := ctorVal.Object()
	fd := ctor.fn()
	if fd == nil {
		return r.throwTypeError("a class constructor must be a function")
	}

	// `class X extends null` produces a class whose instances have no
	// prototype, which is legal.
	if parent.IsNull() {
		if p, err := r.getProp(ctor, atomPrototype, ctorVal); err == nil && p.IsObject() {
			p.Object().setProto(nil)
		}
		fd.ctorKind = ctorDerived
		return nil
	}
	if !isConstructor(parent) {
		// An arrow, a method and a generator are callable but not
		// constructable, and a class has to call what it extends.
		return r.throwTypeError("a class may only extend a constructor or null")
	}
	parentObj := parent.Object()

	protoVal, err := r.getProp(ctor, atomPrototype, ctorVal)
	if err != nil {
		return err
	}
	parentProtoVal, err := r.getProp(parentObj, atomPrototype, parent)
	if err != nil {
		return err
	}
	// What the parent calls its prototype is what the instances inherit from,
	// and it has to be something they can: an object, or nothing at all.
	if !parentProtoVal.IsObject() && !parentProtoVal.IsNull() {
		return r.throwTypeError("the superclass prototype is neither an object nor null")
	}
	if protoVal.IsObject() {
		if parentProtoVal.IsObject() {
			// The prototype, as the constructor below, takes the layout
			// the others made so share, rather than one of its own.
			classProto[struct{}](protoVal.Object(), parentProtoVal.Object())
		} else {
			protoVal.Object().setProto(nil)
		}
	}
	// Static inheritance.
	classProto[struct{}](ctor, parentObj)

	fd.ctorKind = ctorDerived
	fd.setSuperCtor(ctor)
	return nil
}

// superCall invokes the parent constructor on the current instance.
//
// The specification has a derived constructor receive `this` from its parent
// rather than creating it, with the binding uninitialized until super()
// returns. Here the instance is created up front and the parent runs against
// it, which gives the same result for every hierarchy that does not observe
// the difference through new.target or a base constructor that returns an
// object of its own.
func (r *Runtime) superCall(f *frame, args []Value) error {
	parent := r.parentConstructorOf(f)
	if parent == nil {
		return r.throwTypeError("\"super\" is only valid in a derived constructor")
	}
	if !isConstructor(Obj(parent)) {
		// The class's prototype may have been changed to something that cannot
		// be constructed, which is only visible now.
		return r.throwTypeError("the superclass is not a constructor")
	}
	if f.thisRef == nil {
		return r.throwError(errReference, "super() has already been called")
	}
	// new.target is forwarded rather than replaced: inside a base constructor
	// reached through super(), new.target is the derived class the caller
	// actually wrote `new` against. An abstract base distinguishes the two --
	// `new Iterator()` is an error while `new C()` for `class C extends
	// Iterator` is not -- so the difference is observable.
	newTarget := f.newTarget
	if newTarget.IsUndefined() {
		newTarget = Obj(parent)
	}
	// A base parent is entered directly rather than through construct, which
	// would build a second object for the one already made here, so its own
	// instance initializer is run on the way in. A derived parent runs its own
	// in its own super().
	if pfd := parent.fn(); pfd != nil && pfd.ctorKind != ctorDerived {
		if err := superInit(r, parent, f); err != nil {
			return err
		}
	}
	res, err := superConstruct(r, parent, f, args, newTarget)
	if err != nil {
		return err
	}
	return r.superBind(f, parent, res)
}

// superBind is superCall's after the parent's construction gave res: this
// bound to it, and the fields of the class whose constructor is running
// put on the instance.
func (r *Runtime) superBind(f *frame, parent *Object, res Value) error {
	// `this` is bound once, and the second super() finds it already bound. The
	// parent still ran: the check comes after the construction, not before it,
	// so a second call has the parent's side effects and none of its own.
	if f.thisRef.init {
		return r.throwError(errReference, "super() has already been called")
	}
	// A base constructor that builds its own object -- every native one does,
	// because an Error needs a stack and an Array needs array storage -- hands
	// it back instead of writing into the `this` passed in. The derived
	// constructor adopts it, keeping the prototype that newTarget chose so
	// that the result is still an instance of the derived class. An object a
	// script's constructor returns is its own, with the prototype it has, as
	// super() binds this to whatever the parent's construction gave: class
	// R extends P, P's constructor returning {}, makes no R.
	if res.IsObject() && (!f.this.IsObject() || res.Object() != f.this.Object()) {
		if pfd := parent.fn(); f.this.IsObject() && (pfd == nil || pfd.closure == nil || pfd.bound) {
			res.Object().setProto(f.this.Object().proto())
			// Fields the derived constructor set before super() would be lost,
			// but writing to `this` before super() is already an error, so
			// there is nothing to carry over.
		}
		f.this = res
	}
	f.thisRef.value = f.this
	f.thisRef.init = true
	// The fields belong to the class whose constructor is running, which is not
	// necessarily what is running: super() may be written inside an arrow, and
	// the arrow inherits the link to the class rather than being it. They are
	// put on the instance now: after the parent has finished with it, before
	// this constructor's body sees it.
	ctor := f.callee
	if fd := f.callee.fn(); fd != nil && fd.superCtor() != nil {
		ctor = fd.superCtor()
	}
	return r.initInstanceElements(ctor, f.this)
}

// initInstanceElements gives a new instance the private methods and fields of
// the class that is constructing it.
//
// A class without either has no initializer, which is the common case and costs
// nothing.
func (r *Runtime) initInstanceElements(ctor *Object, this Value) error {
	if ctor == nil {
		return nil
	}
	fd := ctor.fn()
	if fd == nil || fd.fieldInit() == nil {
		return nil
	}
	_, err := r.callObject(fd.fieldInit(), this, nil, Undefined)
	return err
}

// parentConstructorOf finds the constructor a frame's super() refers to.
func (r *Runtime) parentConstructorOf(f *frame) *Object {
	if f.callee == nil {
		return nil
	}
	fd := f.callee.fn()
	if fd == nil {
		return nil
	}
	if fd.superCtor() != nil {
		// GetSuperConstructor reads the running class's prototype now rather
		// than remembering what it extended.
		return fd.superCtor().proto()
	}
	// A method reaches the parent through its home object rather than through
	// a stored link.
	if fd.homeObject() != nil && fd.homeObject().proto() != nil {
		if ctor := fd.homeObject().proto().getOwn(atomConstructor); ctor.ok() &&
			ctor.value().IsObject() {
			return ctor.value().Object()
		}
	}
	return nil
}

// superGet reads a property through super, resolving it on the home object's
// prototype while leaving `this` as the current receiver.
func (r *Runtime) superGet(f *frame, key Atom) (Value, error) {
	start, this, err := r.superBase(f)
	if err != nil {
		return Undefined, err
	}
	// The receiver stays the instance, so an inherited getter sees the right
	// object.
	return r.getProp(start, key, this)
}

// superBase resolves what a super reference reads from, and the receiver it
// reads with.
//
// It is settled before the key is computed, so that changing the home object's
// prototype while the key runs does not move the reference.
func (r *Runtime) superBase(f *frame) (*Object, Value, error) {
	start, this, err := r.superRef(f)
	if err != nil {
		return nil, Undefined, err
	}
	if start == nil {
		// `class C extends null` leaves the home object with no prototype, so
		// there is nothing to read from -- which the specification reports
		// rather than answering undefined.
		return nil, Undefined, r.throwTypeError("\"super\" has no prototype to read from")
	}
	return start, this, nil
}

// superRef is superBase without the requirement that there be a prototype to
// reach.
//
// A reference with no base is only an error once something is done with it, and
// that happens after the rest of the expression has run: `super[k()] = v()`
// calls both before it complains.
func (r *Runtime) superRef(f *frame) (*Object, Value, error) {
	if f.callee == nil {
		return nil, Undefined, r.throwTypeError("\"super\" is only valid inside a method")
	}
	fd := f.callee.fn()
	if fd == nil || fd.homeObject() == nil {
		return nil, Undefined, r.throwTypeError("\"super\" is only valid inside a method")
	}
	this, bound := f.thisValue()
	if !bound {
		return nil, Undefined, r.throwError(errReference,
			"\"this\" is not bound until super() has been called")
	}
	return fd.homeObject().proto(), this, nil
}

// superSet writes through a super reference.
//
// The lookup starts at the home object's prototype, but the receiver is the
// instance -- so an inherited setter runs with the right `this`, and an
// assignment that finds no setter lands on the instance rather than on the
// prototype.
func (r *Runtime) superSet(f *frame, key Atom, val Value, strict bool) error {
	start, this, err := r.superRef(f)
	if err != nil {
		return err
	}
	if start == nil {
		// `class C extends null`, or a home object whose prototype was taken
		// away: the reference has no base, and assigning through one is a
		// TypeError rather than a write to the instance.
		return r.throwTypeError("\"super\" has no prototype to assign to")
	}
	_, err = r.setProp(start, key, val, this, strict)
	return err
}

// superSetFrom writes through a super reference whose base was already
// resolved, which is what a computed key needs: the base is settled before the
// key runs.
func (r *Runtime) superSetFrom(f *frame, base Value, key Atom, val Value, strict bool) error {
	this, bound := f.thisValue()
	if !bound {
		return r.throwError(errReference,
			"\"this\" is not bound until super() has been called")
	}
	if !base.IsObject() {
		if !this.IsObject() {
			return r.throwTypeError("cannot assign to a super property of %s", r.describe(this))
		}
		_, err := r.setOnReceiver(this.Object(), key, val, strict)
		return err
	}
	_, err := r.setProp(base.Object(), key, val, this, strict)
	return err
}

// getPrivate reads a private class member.
//
// Unlike an ordinary property, a missing private member is an error rather than
// undefined: the field is part of the class's shape, so reaching for one on an
// object that does not have it is a bug the language reports rather than
// papering over.
// setPrivate writes a private member, running a private setter if the class
// declared one.
//
// A private accessor is stored as an ordinary accessor property carrying the
// private flag, so the write has to look for one rather than always installing
// a data property -- otherwise `set #m(v)` would be shadowed the first time
// anything assigned to #m.
func (r *Runtime) setPrivate(o *Object, key, name Atom, val Value) error {
	// An own property and nothing else: a private member belongs to the object
	// that has it, so an object that merely inherits from an instance's
	// prototype is not one and cannot be written through.
	p := o.getOwn(key)
	if !p.ok() {
		// A private field is added when the object is constructed and never
		// afterwards, so writing one to an object that does not have it is a
		// mistake rather than a way to add it.
		return r.throwTypeError("private member %s is not present on this object",
			r.atoms.name(name))
	}
	if p.isAccessor() {
		a := p.getterSetter()
		if a == nil || a.setter == nil {
			return r.throwTypeError("private member %s has no setter", r.atoms.name(name))
		}
		_, err := r.call(Obj(a.setter), Obj(o), []Value{val})
		return err
	}
	if p.flags()&propWritable == 0 {
		// A method is installed read-only, so this is one.
		return r.throwTypeError("private method %s is read-only", r.atoms.name(name))
	}
	p.setValue(val)
	return nil
}

func (r *Runtime) getPrivate(o *Object, key, name Atom) (Value, error) {
	p := o.getOwn(key)
	if !p.ok() {
		return Undefined, r.throwTypeError(
			"private member %s is not present on this object", r.atoms.name(name))
	}
	if p.isAccessor() {
		a := p.getterSetter()
		if a == nil || a.getter == nil {
			return Undefined, r.throwTypeError(
				"private member %s has no getter", r.atoms.name(name))
		}
		return r.call(Obj(a.getter), Obj(o), nil)
	}
	return p.value(), nil
}

// templateObject returns the strings a tagged template site hands its tag.
//
// It is built once per site and reused, because a tag is entitled to hang state
// off the object and to compare it against one from a later call -- which is
// the usual way a tag caches whatever it derived from the text.
func (r *Runtime) templateObject(fn *bytecode.Function, idx int) *Object {
	table := r.templateCache
	sites := table[uintptr(unsafe.Pointer(fn))]
	if sites == nil || sites.fn.Value() != fn {
		sites = newTemplateSites[struct{}](r.Realm, table, fn, make([]*Object, len(fn.Templates)))
	}
	cache := sites.objects
	if o := cache[idx]; o != nil {
		return o
	}

	t := fn.Templates[idx]
	cooked := make([]Value, len(t.Cooked))
	raw := make([]Value, len(t.Raw))
	for i := range t.Cooked {
		// A malformed escape has no cooked meaning. That is an error in an
		// ordinary template and merely undefined here, so that a tag with its
		// own escape conventions can read the raw text instead.
		cooked[i] = Undefined
		if t.CookedValid[i] {
			cooked[i] = Str(NewString(t.Cooked[i]))
		}
		raw[i] = Str(NewString(t.Raw[i]))
	}

	o := r.newArrayFrom(cooked)
	o.setOwnRaw(atomRaw, Obj(r.freezeArray(r.newArrayFrom(raw))), 0)
	r.freezeArray(o)
	cache[idx] = o
	return o
}

// freezeArray makes an array's elements read-only, which the strings a tag
// receives have to be: the same object is handed out again on the next call, so
// a tag that wrote to it would be writing to every later call's argument.
func (r *Runtime) freezeArray(o *Object) *Object {
	for i, el := range o.elemsOf() {
		if !isHole(el) {
			o.setOwnRaw(r.atoms.indexAtom(uint32(i)), el, propEnumerable)
		}
	}
	o.markSparse()
	o.extOf().elems = nil
	// The length is synthesized rather than stored, so its writability is a
	// flag rather than a property attribute.
	o.flags &^= objExtensible | objArrayLengthWritable
	o.layoutChanged()
	for i := range o.vals() {
		o.shape.keys[i].flags &^= propWritable | propConfigurable
	}
	return o
}

// endTurn releases what the turn left behind.
//
// Two things outlive a call for reasons that stop applying once nothing is
// running: the operand stack above the live frames, which popFrame leaves dirty
// because clearing it per call was the interpreter's single largest cost, and
// the values a WeakRef handed out, which have to survive the turn they were
// read in and no longer. Doing both here costs one pass over the part of the
// stack that was actually used, once per turn.
func (r *Runtime) endTurn() {
	if r.stackHigh > r.stackTop {
		clear(r.stack[r.stackTop:r.stackHigh])
		r.stackHigh = r.stackTop
		if jitStackHighLowered != nil {
			jitStackHighLowered(r)
		}
	}
	r.clearReturnedFrames()
	r.releaseKeptValues()
	r.releaseDeadWeakMaps()
	if r.frameDepth == 0 {
		// Nothing runs that read a call cache's entry: the caches let go
		// of the functions their sites called last (releaseRegCalls).
		r.releaseRegCalls()
	}
}
