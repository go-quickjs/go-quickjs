package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Realms.
//
// A realm is a global object and the intrinsics that go with it; a runtime
// has the one it was made with and may make more, which share its stack, its
// job queue and its symbols. A function belongs to the realm it was made in:
// a compiled one through its closure, a built-in through its funcData. Calling
// one from another realm switches to its realm for the call, so that what it
// makes -- an array, an error, an object literal -- is its realm's, and the
// global object a sloppy function falls back to is its realm's too.
//
// The runtime embeds the current realm, so r.proto and r.global are always the
// running code's. A call that stays in one realm, which is almost every call,
// pays one comparison for that.

// namedIntrinsic is an intrinsic prototype and the name it is known by in
// every realm.
type namedIntrinsic struct {
	name  string
	proto *Object
}

// NewRealm makes a realm with its own global object and intrinsics.
func (r *Runtime) NewRealm() *Realm {
	re := newRealm(r)
	prev := r.Realm
	r.Realm = re
	r.initRealm()
	// Code generation is the runtime's to allow, and a realm made after it
	// was allowed has it too.
	if r.evaluator != nil {
		r.installEval()
	}
	r.Realm = prev
	return re
}

// newRealm returns a realm with nothing in it yet.
func newRealm(r *Runtime) *Realm {
	return &Realm{
		agent:         r,
		templateCache: make(map[*bytecode.Function][]*Object),
		// Room for every intrinsic prototype a realm names, so that recording
		// them is one allocation rather than one per doubling.
		names: make([]namedIntrinsic, 0, 128),
	}
}

// initRealm builds the current realm's intrinsics and global object.
func (r *Runtime) initRealm() {
	r.building = true
	r.shapes.building = true
	r.initIntrinsics()
	r.initGlobals()
	r.registerIntrinsics()
	// What is left of the slab would be kept alive by the objects cut from it,
	// and a realm is built once.
	r.funcSlab, r.building = nil, false
	r.shapes.building = false
}

// Global returns the realm's global object.
func (re *Realm) Global() *Object { return re.global }

// RunIn runs a compiled script in a realm, as the realm's own code.
func (r *Runtime) RunIn(re *Realm, fn *bytecode.Function) (Value, error) {
	prev := r.Realm
	r.Realm = re
	defer func() { r.Realm = prev }()
	return r.Run(fn)
}

// InRealm runs fn with re as the current realm, so that what it makes -- a
// function, an error -- is re's.
func (r *Runtime) InRealm(re *Realm, fn func()) {
	prev := r.Realm
	r.Realm = re
	defer func() { r.Realm = prev }()
	fn()
}

// registerIntrinsic records an intrinsic prototype under its name.
func (r *Runtime) registerIntrinsic(name string, proto *Object) {
	if proto != nil {
		r.names = append(r.names, namedIntrinsic{name: name, proto: proto})
	}
}

// registerIntrinsics records the intrinsic prototypes that are not made by
// newCtor, which records its own.
func (r *Runtime) registerIntrinsics() {
	p := &r.proto
	for _, in := range []namedIntrinsic{
		{"Object", p.object}, {"Function", p.function}, {"Array", p.array},
		{"String", p.str}, {"Number", p.number}, {"Boolean", p.boolean},
		{"Symbol", p.symbol}, {"BigInt", p.bigint}, {"Date", p.date},
		{"RegExp", p.regexp}, {"Map", p.mapProto}, {"Set", p.setProto},
		{"Promise", p.promise}, {"%GeneratorPrototype%", p.generator},
		{"%AsyncGeneratorPrototype%", p.asyncGenerator}, {"Iterator", p.iterator},
		{"GeneratorFunction", r.genFuncProto}, {"AsyncFunction", r.asyncFuncProto},
		{"AsyncGeneratorFunction", r.asyncGenFuncProto},
		{"ArrayBuffer", r.arrayBufferProto}, {"%TypedArray%", r.typedArrayProto},
	} {
		r.registerIntrinsic(in.name, in.proto)
	}
	for k := errorKind(0); k < errorKindCount; k++ {
		r.registerIntrinsic(errorKindNames[k], p.nativeErrors[k])
	}
	for i, proto := range r.typedArrayProtos {
		if ctor := r.typedArrayCtors[i]; ctor != nil {
			if fd := ctor.fn(); fd != nil {
				r.registerIntrinsic(fd.name, proto)
			}
		}
	}
}

// intrinsicName returns the name an intrinsic prototype of the realm is known
// by, or "" for an object that is not one.
func (re *Realm) intrinsicName(proto *Object) string {
	for _, in := range re.names {
		if in.proto == proto {
			return in.name
		}
	}
	return ""
}

// counterpart returns realm to's counterpart of an intrinsic prototype of
// realm from, or the prototype itself when it has none.
func (r *Runtime) counterpart(from, to *Realm, proto *Object) *Object {
	if to == nil || to == from {
		return proto
	}
	name := from.intrinsicName(proto)
	if name == "" {
		return proto
	}
	for _, in := range to.names {
		if in.name == name {
			return in.proto
		}
	}
	// A lazily built one -- Intl's, Temporal's -- the other realm may not have
	// built yet; building it there is the other realm's business.
	if made := r.buildLazyIntrinsic(to, name); made != nil {
		return made
	}
	return proto
}

// buildLazyIntrinsic builds a lazily made intrinsic in another realm, by
// reading the global that makes it there.
func (r *Runtime) buildLazyIntrinsic(re *Realm, name string) *Object {
	prev := r.Realm
	r.Realm = re
	defer func() { r.Realm = prev }()
	for _, ns := range []string{"Intl", "Temporal"} {
		if _, err := r.getProp(r.global, r.atoms.intern(ns), r.globalThis); err != nil {
			return nil
		}
	}
	for _, in := range r.names {
		if in.name == name {
			return in.proto
		}
	}
	return nil
}

// functionRealm is GetFunctionRealm: the realm a function object belongs to,
// looking through bound functions and proxies to the function they wrap.
func (r *Runtime) functionRealm(o *Object) (*Realm, error) {
	for {
		if p := proxyOf(o); p != nil {
			if p.revoked || p.target == nil {
				return nil, r.throwTypeError("cannot perform an operation on a revoked proxy")
			}
			o = p.target
			continue
		}
		fd := o.fn()
		switch {
		case fd == nil:
			return r.Realm, nil
		case fd.boundTarget != nil:
			o = fd.boundTarget
			continue
		case fd.closure != nil:
			return fd.closure.realm, nil
		case fd.realm != nil:
			return fd.realm, nil
		}
		return r.Realm, nil
	}
}

// protoForNewTarget is the prototype an object made for a constructor whose
// new.target named none gets: the fallback's counterpart in new.target's
// realm.
func (r *Runtime) protoForNewTarget(newTarget *Object, fallback *Object) (*Object, error) {
	re, err := r.functionRealm(newTarget)
	if err != nil {
		return nil, err
	}
	return r.counterpart(r.Realm, re, fallback), nil
}
