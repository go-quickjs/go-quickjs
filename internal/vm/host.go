package vm

import "sort"

// The extension points a host builds on: modules whose exports come from Go,
// objects and promises made from Go, and the queue that carries the work
// between them.
//
// Everything here is deliberately small. What a host actually wants -- a
// console, a clock, a filesystem, an HTTP client -- is not the engine's
// business to define, and none of it is present unless the host puts it there.

// NativeExport is one export of a module the host supplies.
type NativeExport struct {
	Name  string
	Value Value
}

// DefineNativeModule registers a module whose exports come from the host rather
// than from source.
//
// The module is already evaluated: it has no body to run, so importing it can
// neither fail nor observe an order. It answers to the specifier it was
// registered under whatever a module loader would make of that name, which is
// what lets `import {readFile} from "fs"` reach the host's fs rather than a
// file called fs.
//
// Registering a name twice replaces what it exports, so that a host can install
// its modules in whatever order suits it.
func (r *Runtime) DefineNativeModule(specifier string, exports []NativeExport) *Module {
	m := r.newModule(specifier, nil)
	m.native = true
	m.state = ModuleEvaluated
	// The namespace lists its exports in sorted order, so the bindings are made
	// in that order too: what a host passes in has no order of its own worth
	// preserving, and this way two runtimes agree.
	sorted := make([]NativeExport, len(exports))
	copy(sorted, exports)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, e := range sorted {
		m.env.setOwnRaw(r.atoms.intern(e.Name), e.Value, propEnumerable)
		m.exports[e.Name] = e.Name
	}
	if r.modules == nil {
		r.modules = make(map[string]*Module)
	}
	r.modules[specifier] = m
	return m
}

// nativeModule returns a module the host registered under this exact name.
func (r *Runtime) nativeModule(specifier string) *Module {
	if m, ok := r.modules[specifier]; ok && m.native {
		return m
	}
	return nil
}

// NewPlainObject returns an ordinary empty object, for a host assembling a
// value to hand to script.
func (r *Runtime) NewPlainObject() *Object { return newObject(r.proto.object, ClassObject) }

// NewArrayOf returns an array holding the given values.
func (r *Runtime) NewArrayOf(vals []Value) *Object { return r.newArrayFrom(vals) }

// NewFunc returns a callable object wrapping a Go function.
func (r *Runtime) NewFunc(name string, length int, fn NativeFunc) *Object {
	return r.newNativeFunc(name, length, fn)
}

// DefineProp installs a data property that a script may write, enumerate and
// delete, which is what a plain assignment would have made.
func (r *Runtime) DefineValue(o *Object, name string, v Value) {
	o.setOwnRaw(r.atoms.intern(name), v, propDefault)
}

// DefineMethod installs a method the way a built-in one is installed: writable
// and configurable, but not enumerable.
func (r *Runtime) DefineMethod(o *Object, name string, length int, fn NativeFunc) *Object {
	return r.defMethod(o, name, length, fn)
}

// DefineGetter installs a read-only accessor, which is how a host exposes
// something it computes on demand.
func (r *Runtime) DefineGetter(o *Object, name string, fn NativeFunc) {
	r.defGetter(o, name, fn)
}

// HostPromise is a promise the host settles.
//
// Resolve and Reject queue the reactions rather than running them, exactly as
// settling a promise from script does; the host runs them by draining the job
// queue. Settling one twice does nothing, as it does in script.
type HostPromise struct {
	rt      *Runtime
	promise *Object
	resolve *Object
	reject  *Object
}

// NewHostPromise returns a pending promise and the means to settle it.
func (r *Runtime) NewHostPromise() *HostPromise {
	p := r.newPromise()
	resolve, reject := r.resolvingFunctions(p)
	return &HostPromise{rt: r, promise: p, resolve: resolve, reject: reject}
}

// Value is the promise itself, which is what a host hands back to script.
func (h *HostPromise) Value() Value { return Obj(h.promise) }

// Resolve settles the promise with a value. A thenable is adopted, so
// resolving with another promise makes this one follow it.
func (h *HostPromise) Resolve(v Value) {
	_, _ = h.rt.callObject(h.resolve, Undefined, []Value{v}, Undefined)
}

// Reject settles the promise with a reason.
func (h *HostPromise) Reject(v Value) {
	_, _ = h.rt.callObject(h.reject, Undefined, []Value{v}, Undefined)
}

// RejectError rejects with an Error carrying the message of a Go error, which
// is what a host operation that failed usually has to offer.
func (h *HostPromise) RejectError(err error) {
	h.Reject(h.rt.NewError("Error", err.Error()))
}

// ThrowTypeErrorf returns the error a native function returns to throw a
// TypeError, which is what a host rejects a bad argument with.
func (r *Runtime) ThrowTypeErrorf(format string, args ...any) error {
	return r.throwTypeError(format, args...)
}

// EnqueueJob adds a microtask, which runs when the queue is next drained.
func (r *Runtime) EnqueueJob(fn func()) { r.enqueueJob(fn) }
