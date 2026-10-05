package vm

import (
	"fmt"
	"sort"
)

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

// DefineSyntheticModule registers a module a host makes rather than compiles,
// under the name a module loader resolves to it: the names it exports, known
// now, and evaluate, which gives them their values when the module is
// evaluated, in its turn in the graph that imports it, as a body would run. An
// export evaluate gives no value is undefined.
//
// A name already taken keeps its module, which is returned: a loader asked
// for the same module by two importers defines it twice.
func (r *Runtime) DefineSyntheticModule(specifier string, names []string, evaluate func() ([]NativeExport, error)) *Module {
	if m, ok := r.modules[specifier]; ok {
		return m
	}
	m := r.newModule(specifier, nil)
	m.synthetic = evaluate
	for _, name := range names {
		if _, ok := m.exports[name]; ok {
			continue
		}
		m.env.setOwnRaw(r.atoms.intern(name), Undefined, propEnumerable)
		m.exports[name] = name
	}
	// It imports nothing, so there is nothing to link.
	m.state = ModuleLinked
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

// NewUint8ArrayOf returns a Uint8Array holding a copy of the given bytes.
//
// It is what a host hands back for the contents of a file or the body of a
// response: an array of numbers would be a hundred times the memory and
// nothing a script could pass to anything expecting bytes.
func (r *Runtime) NewUint8ArrayOf(b []byte) Value {
	o := newObject(r.typedArrayProtoFor(elemUint8), ClassTypedArray)
	buf := newObject(r.arrayBufferProto, ClassArrayBuffer)
	storage := make([]byte, len(b))
	copy(storage, b)
	buf.data = &arrayBufferData{bytes: storage}
	o.data = &typedArrayData{buffer: buf, kind: elemUint8, fixedLength: len(b)}
	return Obj(o)
}

// NewArrayBufferOf returns an ArrayBuffer over b itself, not a copy. external
// marks memory the host keeps using, which the buffer is never transferred to
// another agent with -- that would share it with another goroutine -- and
// immutable makes the buffer immutable: read, and never written, detached or
// transferred.
func (r *Runtime) NewArrayBufferOf(b []byte, external, immutable bool) Value {
	buf := newObject(r.arrayBufferProto, ClassArrayBuffer)
	buf.data = &arrayBufferData{bytes: b, external: external, immutable: immutable}
	return Obj(buf)
}

// NewTypedArrayOf returns a typed array of the kind named -- "Float64Array"
// -- over the whole of an ArrayBuffer, whose length is a whole number of the
// kind's elements.
func (r *Runtime) NewTypedArrayOf(kind string, buffer Value) (Value, error) {
	k := -1
	for i, info := range elemInfos {
		if info.name == kind {
			k = i
		}
	}
	if k < 0 {
		return Undefined, fmt.Errorf("no typed array is called %s", kind)
	}
	var b *arrayBufferData
	if buffer.IsObject() && buffer.Object().class == ClassArrayBuffer {
		b, _ = buffer.Object().data.(*arrayBufferData)
	}
	if b == nil || b.detached {
		return Undefined, fmt.Errorf("a %s is made over an ArrayBuffer", kind)
	}
	size := elemInfos[k].size
	if len(b.bytes)%size != 0 {
		return Undefined, fmt.Errorf("a %s over %d bytes would end part-way through an element", kind, len(b.bytes))
	}
	o := newObject(r.typedArrayProtoFor(elemType(k)), ClassTypedArray)
	o.data = &typedArrayData{buffer: buffer.Object(), kind: elemType(k), fixedLength: len(b.bytes) / size}
	return Obj(o), nil
}

// Bytes returns the bytes behind a typed array, a DataView or an ArrayBuffer,
// and whether the value was one of those.
//
// The bytes are the ones the object is looking at, not a copy: a host writing
// through them writes what the script sees, which is what makes it possible to
// fill a buffer a script supplied. The bytes of an immutable buffer, which a
// script is promised never change, must not be written, and those of a
// resizable one are only its bytes until the script next resizes it.
func (r *Runtime) Bytes(v Value) ([]byte, bool) {
	if !v.IsObject() {
		return nil, false
	}
	o := v.Object()
	switch data := o.data.(type) {
	case *arrayBufferData:
		if data.detached {
			return nil, false
		}
		return data.bytes, true
	case *typedArrayData:
		storage := data.storage()
		if storage == nil || data.outOfBounds() {
			return nil, false
		}
		start := data.byteOffset
		end := start + data.count()*data.info().size
		if start > len(storage.bytes) || end > len(storage.bytes) {
			return nil, false
		}
		return storage.bytes[start:end], true
	}
	return nil, false
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

// AsyncContext is the async context: the value a job carries from where it
// was queued to where it runs.
func (r *Runtime) AsyncContext() Value { return r.asyncCtx }

// SetAsyncContext makes v the async context, and returns the one before.
func (r *Runtime) SetAsyncContext(v Value) Value {
	prev := r.asyncCtx
	r.asyncCtx = v
	return prev
}

// EnqueueJob adds a microtask, which runs when the queue is next drained.
func (r *Runtime) EnqueueJob(fn func()) { r.enqueueJob(fn) }

// NewHostIterator is an iterator over a host's sequence, as for-of, spread
// and the iterator helpers use one: it inherits from %IteratorPrototype%,
// and its next and return are the methods of a prototype the realm makes
// the first time one is asked for. next gives the sequence's next value, or
// false at its end, or an error, which the call to next throws; stop ends
// the sequence early. Once it has ended, at its end, by an error or by
// return, stop has been called and next answers done.
func (r *Runtime) NewHostIterator(next func() (Value, bool, error), stop func()) Value {
	o := newObject(r.hostIterProto(), ClassObject)
	o.data = &hostIter{next: next, stop: stop}
	return Obj(o)
}

// hostIter is a host iterator's sequence and whether it has ended.
type hostIter struct {
	next func() (Value, bool, error)
	stop func()
	// running is set while next runs, which a call of next or return from
	// inside the sequence is refused for, as a generator refuses one.
	running, done bool
}

// end ends the sequence, once.
func (h *hostIter) end() {
	if !h.done {
		h.done = true
		h.stop()
	}
}

// hostIterProto is the realm's prototype of host iterators, made when one
// is first asked for.
func (r *Runtime) hostIterProto() *Object {
	if p := r.proto.hostIter; p != nil {
		return p
	}
	p := newObject(r.proto.iterator, ClassObject)
	of := func(rt *Runtime, this Value, name string) (*hostIter, error) {
		if this.IsObject() {
			if h, ok := this.Object().data.(*hostIter); ok {
				if h.running {
					return nil, rt.throwTypeError("the iterator is already running")
				}
				return h, nil
			}
		}
		return nil, rt.throwTypeError("%s called on an incompatible receiver", name)
	}
	r.defMethod(p, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		h, err := of(rt, this, "next")
		if err != nil {
			return Undefined, err
		}
		if h.done {
			return Obj(rt.iterResult(Undefined, true)), nil
		}
		h.running = true
		v, ok, err := h.next()
		h.running = false
		if err != nil || !ok {
			h.end()
			if err != nil {
				return Undefined, err
			}
			return Obj(rt.iterResult(Undefined, true)), nil
		}
		return Obj(rt.iterResult(v, false)), nil
	})
	r.defMethod(p, "return", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		h, err := of(rt, this, "return")
		if err != nil {
			return Undefined, err
		}
		h.end()
		return Obj(rt.iterResult(arg(args, 0), true)), nil
	})
	r.proto.hostIter = p
	return p
}

// PromiseResult is a promise's state: whether v is a promise of this
// runtime's, whether it has settled, and if so with what and whether it was
// rejected. The caller is taking the promise's result, as a then does, so a
// rejection -- now, or later for one still pending -- counts as handled.
func (r *Runtime) PromiseResult(v Value) (result Value, settled, rejected, ok bool) {
	if !v.IsObject() || v.Object().class != ClassPromise {
		return Undefined, false, false, false
	}
	p, ok := v.Object().data.(*promiseData)
	if !ok {
		return Undefined, false, false, false
	}
	p.handled = true
	switch p.state {
	case promiseFulfilled:
		return p.value, true, false, true
	case promiseRejected:
		return p.value, true, true, true
	}
	return Undefined, false, false, true
}
