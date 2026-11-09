package wasmjs

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
)

// wasm's GC objects -- structs and arrays -- in JavaScript are opaque: an
// object of no prototype and no properties, which refuses to be given any,
// and which wasm gets back as the object it was. It is a proxy of an empty,
// non-extensible target, whose handler refuses every change.

// gcSlot is what the proxy standing for a struct or an array carries.
type gcSlot struct {
	ref engine.Ref
}

const msgOpaque = "WebAssembly objects are opaque"

// makeOpaqueHandler makes the handler the proxies are made with.
func (a *api) makeOpaqueHandler() {
	r := a.rt
	h := vm.HostObject(nil, nil)
	trap := func(name string, length int, fn vm.NativeFunc) {
		vm.HostDefineOwn(h, r.Intern(name), vm.Obj(r.NewFunc(name, length, fn)), false, false, false)
	}
	refuse := func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		return vm.Undefined, rt.ThrowTypeError(msgOpaque)
	}
	trap("set", 4, refuse)
	trap("defineProperty", 3, refuse)
	trap("deleteProperty", 2, refuse)
	trap("preventExtensions", 1, refuse)
	trap("setPrototypeOf", 2, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		if arg(args, 1).IsNull() {
			return vm.False, nil
		}
		return vm.Undefined, rt.ThrowTypeError(msgOpaque)
	})
	trap("get", 3, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		return vm.Undefined, nil
	})
	trap("has", 2, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		return vm.False, nil
	})
	a.opaque = h
}

// gcObject returns the opaque object standing for struct or array r, made
// the first time it is asked for.
func (a *api) gcObject(r engine.Ref) *vm.Object {
	p := r.Pointer()
	if o := a.wrappers.get(p); o != nil {
		return o
	}
	target := vm.HostObject(nil, nil)
	vm.HostPreventExtensions(target)
	v, err := a.rt.Construct(a.proxy, []vm.Value{vm.Obj(target), vm.Obj(a.opaque)})
	if err != nil || !v.IsObject() {
		// The Proxy constructor taken at Install cannot fail with these
		// arguments; an ordinary empty object stands in if it did.
		v = vm.Obj(target)
	}
	o := v.Object()
	vm.HostSetSlot(a.rt, o, &gcSlot{ref: r})
	a.wrappers.put(unsafe.Pointer(p), o)
	return o
}

// gcRef returns the struct or array v stands for, and whether it stands
// for one.
func (a *api) gcRef(v vm.Value) (engine.Ref, bool) {
	if s, ok := vm.HostSlot(a.rt, v).(*gcSlot); ok {
		return s.ref, true
	}
	return engine.Null, false
}
