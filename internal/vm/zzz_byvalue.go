package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// A closure may take by value what its maker never assigns once the
// closure is made (bytecode.UpvalueDesc's ByValue): a const initialized
// by then, a strict function's parameter nothing stores to. It copies the
// value into a cell of its own, closed from the start, which no frame
// keeps open and closes as it returns: Havlak's and DeltaBlue's callbacks,
// made at every call of their maker to read its parameters and consts,
// had each been a cell besides, opened on the frame and closed with it.
// It is in the package's last files, out of the way of the closures made
// otherwise (newClosureIn), which are made as they were.

// The variants whose captures are all taken by value carry their cells in
// the same allocation too.
type scriptFuncObject1v struct {
	scriptFuncObject
	captured [1]*upvalue
	own      [1]upvalue
}

type scriptFuncObject2v struct {
	scriptFuncObject
	captured [2]*upvalue
	own      [2]upvalue
}

type arrowFuncObject1v struct {
	arrowFuncObject
	captured [1]*upvalue
	own      [1]upvalue
}

type arrowFuncObject2v struct {
	arrowFuncObject
	captured [2]*upvalue
	own      [2]upvalue
}

type arrowFuncObject3v struct {
	arrowFuncObject
	captured [3]*upvalue
	own      [3]upvalue
}

// newClosureByValue is newClosureIn for the template tmpl, some of whose
// captures are by value, made in f, whose captured locals are on its
// slots: the closure copies those into cells of its own, in the object's
// allocation where they are all by value and few, else in the room the
// bindings it is the first to share are given, with them. What follows
// is newClosureIn's, step for step: that function is left as it was, so
// that a closure that takes nothing by value is made by the same code.
func newClosureByValue[_ any](r *Runtime, f *frame, tmpl *closure) *Object {
	descs := tmpl.fn.Upvalues
	arrow := tmpl.fn.Kind == bytecode.KindArrow
	proto := r.funcProtoFor(tmpl.fn)
	var o *Object
	var fd *funcData
	var child *closure
	var upvalues []*upvalue
	var extra *funcExtra
	var cells []upvalue
	own := byValueCount[struct{}](descs)
	// Where they are all by value and few, the variant that carries their
	// cells in the object's allocation.
	var fo *scriptFuncObject
	switch {
	case own != len(descs):
	case arrow && own == 1:
		x := &arrowFuncObject1v{}
		fo, extra, upvalues, cells = &x.scriptFuncObject, &x.extra, x.captured[:], x.own[:]
	case arrow && own == 2:
		x := &arrowFuncObject2v{}
		fo, extra, upvalues, cells = &x.scriptFuncObject, &x.extra, x.captured[:], x.own[:]
	case arrow && own == 3:
		x := &arrowFuncObject3v{}
		fo, extra, upvalues, cells = &x.scriptFuncObject, &x.extra, x.captured[:], x.own[:]
	case !arrow && own == 1:
		x := &scriptFuncObject1v{}
		fo, upvalues, cells = &x.scriptFuncObject, x.captured[:], x.own[:]
	case !arrow && own == 2:
		x := &scriptFuncObject2v{}
		fo, upvalues, cells = &x.scriptFuncObject, x.captured[:], x.own[:]
	}
	if fo != nil {
		fo.Object = Object{shape: rootFor(ClassFunction, proto), class: ClassFunction, flags: objExtensible}
		fo.embedExt(&fo.side)
		fo.side.data = &fo.fn
		o, fd, child = &fo.Object, &fo.fn, &fo.cl
	} else {
		o, fd, child, upvalues, extra = newScriptFuncObject(proto, ClassFunction, len(descs), arrow)
	}
	*child = closure{
		fn: tmpl.fn, closureCode: tmpl.closureCode,
		realm: r.Realm, env: f.cl.env, upvalues: upvalues,
	}
	var room []upvalue
	for i, desc := range descs {
		if !desc.FromParent {
			upvalues[i] = f.cl.upvalues[desc.Index]
			continue
		}
		var u *upvalue
		switch {
		case desc.ByValue && len(cells) != 0:
			u, cells = &cells[0], cells[1:]
		case desc.ByValue:
			if len(room) == 0 {
				room = make([]upvalue, cellsWanted[struct{}](f, descs[i:]))
			}
			u, room = &room[0], room[1:]
		default:
			if u = openUpvalue(f, int(desc.Index)); u == nil {
				if len(room) == 0 {
					room = make([]upvalue, cellsWanted[struct{}](f, descs[i:]))
				}
				u, room = &room[0], room[1:]
				u.slot = &f.locals[desc.Index]
				f.openUpvalues = append(f.openUpvalues, u)
			}
			upvalues[i] = u
			continue
		}
		u.closed = f.locals[desc.Index]
		u.slot = &u.closed
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

// byValueCount is how many of descs are taken by value. Not inlined: so
// the generics this file adds come to a multiple of 64 bytes, and the
// generic functions laid out after them keep their alignment within their
// cache lines (an odd multiple of 32 had moved every one of them by half a
// line).
//
//go:noinline
func byValueCount[_ any](descs []bytecode.UpvalueDesc) int {
	n := 0
	for _, desc := range descs {
		if desc.ByValue {
			n++
		}
	}
	return n
}

// cellsWanted is uncaptured for a closure that takes some of its captures
// by value: the bindings no upvalue is open on yet, and those it copies,
// each of which has a cell of its own.
func cellsWanted[_ any](f *frame, descs []bytecode.UpvalueDesc) int {
	n := 0
	for _, desc := range descs {
		if desc.FromParent && (desc.ByValue || openUpvalue(f, int(desc.Index)) == nil) {
			n++
		}
	}
	return n
}
