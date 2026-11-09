package wasmjs

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// globalData is what a WebAssembly.Global carries.
type globalData struct {
	g *engine.Global
}

// valueTypes are the types a Global's or a Tag's descriptor may name.
var valueTypes = map[string]module.ValueType{
	"i32":       module.I32,
	"i64":       module.I64,
	"f32":       module.F32,
	"f64":       module.F64,
	"v128":      module.V128,
	"externref": module.ExternRef,
	"anyfunc":   module.FuncRef,
	"eqref":     module.EqRef,
	"anyref":    module.AnyRef,
	"structref": module.StructRef,
	"arrayref":  module.ArrayRef,
	"i31ref":    module.I31Ref,
	"exnref":    module.ExnRef,
}

// valueType reads a type a descriptor names, false for one it does not.
func (a *api) valueType(v vm.Value) (module.ValueType, bool, error) {
	s, err := a.rt.ToString(v)
	if err != nil {
		return module.ValueType{}, false, err
	}
	t, ok := valueTypes[s.Go()]
	return t, ok, nil
}

// newGlobal is new WebAssembly.Global(descriptor, value).
func (a *api) newGlobal(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Global()")
	if !rt.Constructing() {
		return vm.Undefined, t.typeError("WebAssembly.Global must be invoked with 'new'")
	}
	desc := arg(args, 0)
	if !desc.IsObject() {
		return vm.Undefined, t.typeError("Argument 0 must be a global descriptor")
	}
	mv, err := rt.GetProp(desc, rt.Intern("mutable"))
	if err != nil {
		return vm.Undefined, err
	}
	tv, err := rt.GetProp(desc, rt.Intern("value"))
	if err != nil {
		return vm.Undefined, err
	}
	typ, ok, err := a.valueType(tv)
	if err != nil {
		return vm.Undefined, err
	}
	if !ok {
		return vm.Undefined, t.typeError("Descriptor property 'value' must be a WebAssembly type")
	}
	proto, err := vm.HostProtoFromNewTarget(rt, a.globalProto)
	if err != nil {
		return vm.Undefined, err
	}
	g := &engine.Global{Type: typ, Mutable: mv.Truthy()}
	v := arg(args, 1)
	switch {
	case typ.Kind == module.KindV128:
		return vm.Undefined, t.typeError("A global of type 'v128' cannot be created in JavaScript")
	case typ.IsRef():
		if len(args) < 2 {
			g.Ref = a.hostRef(defaultRef(typ))
			break
		}
		ref, msg := a.toWasmRef(v, typ)
		if msg != "" {
			return vm.Undefined, t.typeError("%s", msg)
		}
		g.Ref = ref
	case v.IsUndefined():
	default:
		n, err := a.toWasmNum(v, typ)
		if err != nil {
			return vm.Undefined, err
		}
		g.Num = n.N
	}
	return vm.Obj(a.globalObject(g, proto)), nil
}

// globalObject returns the WebAssembly.Global of g, made the first time it
// is asked for.
func (a *api) globalObject(g *engine.Global, proto *vm.Object) *vm.Object {
	if o := a.wrappers.get(unsafe.Pointer(g)); o != nil {
		return o
	}
	o := vm.HostObject(proto, &globalData{g: g})
	a.wrappers.put(unsafe.Pointer(g), o)
	return o
}

// globalGet returns the value of the global this is the WebAssembly.Global
// of.
func (a *api) globalGet(t thrower, this vm.Value) (vm.Value, error) {
	d, ok := vm.HostData(this).(*globalData)
	if !ok {
		return vm.Undefined, t.typeError("Receiver is not a WebAssembly.Global")
	}
	g := d.g
	switch {
	case g.Type.Kind == module.KindV128:
		return vm.Undefined, t.typeError("Can't get the value of s128 WebAssembly.Global")
	case g.Type.IsRef():
		return a.refResult(t, g.Ref, g.Type)
	}
	return a.toJS(engine.Val{N: g.Num}, g.Type)
}

// globalValueOf is WebAssembly.Global.prototype.valueOf().
func (a *api) globalValueOf(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	return a.globalGet(a.thrower("WebAssembly.Global.valueOf()"), this)
}

// globalValue is the getter of WebAssembly.Global.prototype.value.
func (a *api) globalValue(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	return a.globalGet(a.thrower("get WebAssembly.Global.value)"), this)
}

// setGlobalValue is the setter of WebAssembly.Global.prototype.value.
func (a *api) setGlobalValue(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("set WebAssembly.Global.value)")
	d, ok := vm.HostData(this).(*globalData)
	if !ok {
		return vm.Undefined, t.typeError("Receiver is not a WebAssembly.Global")
	}
	g := d.g
	if !g.Mutable {
		return vm.Undefined, t.typeError("Can't set the value of an immutable global.")
	}
	if len(args) == 0 {
		return vm.Undefined, t.typeError("Argument 0 is required")
	}
	switch {
	case g.Type.Kind == module.KindV128:
		return vm.Undefined, t.typeError("Can't set the value of s128 WebAssembly.Global")
	case g.Type.IsRef():
		ref, msg := a.toWasmRef(args[0], canon(g.Type, g.Module))
		if msg != "" {
			return vm.Undefined, t.typeError("%s", msg)
		}
		g.Ref = ref
	default:
		n, err := a.toWasmNum(args[0], g.Type)
		if err != nil {
			return vm.Undefined, err
		}
		g.Num = n.N
	}
	return vm.Undefined, nil
}
