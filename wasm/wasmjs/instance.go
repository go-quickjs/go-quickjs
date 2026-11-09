package wasmjs

import (
	"fmt"
	"math"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// instanceData is what a WebAssembly.Instance carries.
type instanceData struct {
	inst    *engine.Instance
	exports *vm.Object
}

// importName names import i as a LinkError does: Import #0 "m" "f".
func importName(m *module.Module, i int) string {
	imp := m.Imports[i]
	return fmt.Sprintf("Import #%d %q %q", i, imp.Module, imp.Name)
}

// newInstance is new WebAssembly.Instance(module, imports).
func (a *api) newInstance(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Instance()")
	if !rt.Constructing() {
		return vm.Undefined, t.typeError("WebAssembly.Instance must be invoked with 'new'")
	}
	proto, err := a.newTargetProto(rt)
	if err != nil {
		return vm.Undefined, err
	}
	md := moduleOf(arg(args, 0))
	if md == nil {
		return vm.Undefined, t.typeError("Argument 0 must be a WebAssembly.Module")
	}
	ffi := arg(args, 1)
	if !ffi.IsUndefined() && !ffi.IsObject() {
		return vm.Undefined, t.typeError("Argument 1 must be an object")
	}
	inst, err := a.instantiateModule(t, md, ffi)
	if err != nil {
		return vm.Undefined, err
	}
	return vm.Obj(a.instanceObject(inst, proto)), nil
}

// instanceObject makes the WebAssembly.Instance of inst, with its exports
// object.
func (a *api) instanceObject(inst *engine.Instance, proto *vm.Object) *vm.Object {
	exports := vm.HostObject(nil, nil)
	for _, e := range inst.Module.Exports {
		var v vm.Value
		switch e.Kind {
		case module.ExternFunc:
			v = vm.Obj(a.functionObject(inst.Funcs[e.Index]))
		case module.ExternTable:
			v = vm.Obj(a.tableObject(inst.Tables[e.Index], a.tableProto))
		case module.ExternMemory:
			v = vm.Obj(a.memoryObject(inst.Memories[e.Index], a.memoryProto))
		case module.ExternGlobal:
			v = vm.Obj(a.globalObject(inst.Globals[e.Index], a.globalProto))
		case module.ExternTag:
			v = vm.Obj(a.tagObject(inst.Tags[e.Index], a.tagProto))
		}
		vm.HostDefineOwn(exports, a.rt.Intern(e.Name), v, false, true, false)
	}
	vm.HostPreventExtensions(exports)
	return vm.HostObject(proto, &instanceData{inst: inst, exports: exports})
}

// instanceExports is the getter of WebAssembly.Instance.prototype.exports.
func (a *api) instanceExports(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	d, ok := vm.HostData(this).(*instanceData)
	if !ok {
		return vm.Undefined, a.thrower("WebAssembly.Instance.exports()").typeError("Receiver is not a WebAssembly.Instance")
	}
	return vm.Obj(d.exports), nil
}

// importValue is an import as the imports object gives it, or as the
// compile-time imports do.
type importValue struct {
	v       vm.Value
	builtin builtin
	// constant is set for a string constant.
	constant bool
}

// lookupImports reads each import from the imports object, in order: its
// module, then the import in it.
func (a *api) lookupImports(t thrower, md *moduleData, ffi vm.Value) ([]importValue, error) {
	m := md.m
	vals := make([]importValue, len(m.Imports))
	for i, imp := range m.Imports {
		if imp.Kind == module.ExternGlobal && md.ci.hasConstants && imp.Module == md.ci.constants {
			vals[i] = importValue{v: vm.Str(vm.NewString(imp.Name)), constant: true}
			continue
		}
		if md.builtins != nil && md.builtins[i] != noBuiltin {
			vals[i] = importValue{builtin: md.builtins[i]}
			continue
		}
		if !ffi.IsObject() {
			return nil, t.typeError("Imports argument must be present and must be an object")
		}
		mod, err := a.rt.GetProp(ffi, a.rt.Intern(imp.Module))
		if err != nil {
			return nil, err
		}
		if !mod.IsObject() {
			return nil, t.typeError("Import #%d %q: module is not an object or function", i, imp.Module)
		}
		v, err := a.rt.GetProp(mod, a.rt.Intern(imp.Name))
		if err != nil {
			return nil, err
		}
		vals[i] = importValue{v: v}
	}
	return vals, nil
}

// link makes import i of md the entity v stands for, checking it as the
// module declares it.
func (a *api) link(t thrower, md *moduleData, i int, iv importValue) (engine.Extern, error) {
	m := md.m
	imp := m.Imports[i]
	v := iv.v
	ext := engine.Extern{Kind: imp.Kind}
	switch imp.Kind {
	case module.ExternFunc:
		c := m.Canonical[m.Functions[imp.Index].TypeIndex]
		switch {
		case iv.builtin != noBuiltin:
			ext.Func = a.builtinFunction(iv.builtin, imp.Index)
		case a.exportedFunction(v) != nil:
			ext.Func = a.exportedFunction(v)
		case a.suspendingOf(v) != nil:
			ext.Func = a.suspendingFunction(a.suspendingOf(v), c, imp.Index)
		case vm.IsCallable(v):
			ext.Func = a.hostFunction(v, c, imp.Index)
		default:
			return ext, t.linkError("%s: function import requires a callable", importName(m, i))
		}
	case module.ExternTable:
		d, ok := vm.HostData(v).(*tableData)
		if !ok {
			return ext, t.linkError("%s: table import requires a WebAssembly.Table", importName(m, i))
		}
		ext.Table = d.t
	case module.ExternMemory:
		d, ok := vm.HostData(v).(*memoryData)
		if !ok {
			return ext, t.linkError("%s: memory import must be a WebAssembly.Memory object", importName(m, i))
		}
		ext.Memory = d.mem
	case module.ExternGlobal:
		g, err := a.importGlobal(t, md, i, v)
		if err != nil {
			return ext, err
		}
		ext.Global = g
	case module.ExternTag:
		d, ok := vm.HostData(v).(*tagData)
		if !ok {
			return ext, t.linkError("%s: tag import requires a WebAssembly.Tag", importName(m, i))
		}
		ext.Tag = d.tag
	}
	if err := engine.CheckImport(m, i, ext); err != nil {
		return ext, t.linkError("%s", err.Error())
	}
	return ext, nil
}

// importGlobal returns the global import i of md is given by v: a
// WebAssembly.Global, or a value for an immutable global.
func (a *api) importGlobal(t thrower, md *moduleData, i int, v vm.Value) (*engine.Global, error) {
	m := md.m
	decl := m.Globals[m.Imports[i].Index]
	if d, ok := vm.HostData(v).(*globalData); ok {
		return d.g, nil
	}
	if decl.Type.Kind == module.KindV128 {
		return nil, t.linkError("%s: global import of type v128 must be a WebAssembly.Global", importName(m, i))
	}
	if decl.Mutable {
		return nil, t.linkError("%s: imported mutable global must be a WebAssembly.Global object", importName(m, i))
	}
	g := &engine.Global{Type: decl.Type, Module: m}
	if decl.Type.IsRef() {
		ref, msg := a.toWasmRef(v, canon(decl.Type, m))
		if msg != "" {
			return nil, t.linkError("%s: %s", importName(m, i), msg)
		}
		g.Ref = ref
		return g, nil
	}
	switch {
	case v.IsNumber() && decl.Type.Kind != module.KindI64:
		f := v.Number()
		switch decl.Type.Kind {
		case module.KindI32:
			g.Num = uint64(uint32(toInt32(f)))
		case module.KindF32:
			g.Num = uint64(math.Float32bits(float32(f)))
		default:
			g.Num = math.Float64bits(f)
		}
		return g, nil
	case decl.Type.Kind == module.KindI64 && v.IsBigInt():
		n, err := vm.HostToBigInt64(a.rt, v)
		if err != nil {
			return nil, err
		}
		g.Num = uint64(n)
		return g, nil
	}
	return nil, t.linkError("%s: global import must be a number, valid Wasm reference, or WebAssembly.Global object", importName(m, i))
}

// instantiateModule instantiates md with the imports object ffi: it reads
// every import, links the memories, then the rest in order, and runs the
// start function.
func (a *api) instantiateModule(t thrower, md *moduleData, ffi vm.Value) (*engine.Instance, error) {
	m := md.m
	vals, err := a.lookupImports(t, md, ffi)
	if err != nil {
		return nil, err
	}
	externs := make([]engine.Extern, len(m.Imports))
	for i, imp := range m.Imports {
		if imp.Kind == module.ExternMemory {
			if externs[i], err = a.link(t, md, i, vals[i]); err != nil {
				return nil, err
			}
		}
	}
	for i, imp := range m.Imports {
		if imp.Kind != module.ExternMemory {
			if externs[i], err = a.link(t, md, i, vals[i]); err != nil {
				return nil, err
			}
		}
	}
	inst, err := a.runInstantiate(md, externs)
	if err != nil {
		switch e := err.(type) {
		case *engine.LinkError:
			return nil, t.linkError("%s", e.Msg)
		case *engine.AllocError:
			return nil, t.rangeError("%s", e.Msg)
		case *engine.Trap:
			if len(e.Frames) == 0 {
				// Initializing the instance trapped, not its start
				// function.
				return nil, a.trapError(t.name+": ", e)
			}
		}
		return nil, a.runError(err)
	}
	return inst, nil
}

// runInstantiate runs engine.Instantiate in the runtime's context.
func (a *api) runInstantiate(md *moduleData, externs []engine.Extern) (*engine.Instance, error) {
	return engine.Instantiate(runtimeContext{a.rt}, md.code, externs)
}

// instantiate is WebAssembly.instantiate(module or bytes, imports, options).
func (a *api) instantiate(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.instantiate()")
	p := rt.NewHostPromise()
	first, ffi := arg(args, 0), arg(args, 1)
	if !first.IsObject() {
		a.settle(p, t.typeError("Argument 0 must be a buffer source or a WebAssembly.Module object"), nil)
		return p.Value(), nil
	}
	if !ffi.IsUndefined() && !ffi.IsObject() {
		a.settle(p, t.typeError("Argument 1 must be an object"), nil)
		return p.Value(), nil
	}
	if md := moduleOf(first); md != nil {
		inst, err := a.instantiateModule(t, md, ffi)
		a.settle(p, err, func() vm.Value { return vm.Obj(a.instanceObject(inst, a.instanceProto)) })
		return p.Value(), nil
	}
	md, err := a.compileArgs(t, first, arg(args, 2))
	if err != nil {
		a.settle(p, err, nil)
		return p.Value(), nil
	}
	a.instantiateLater(p, md, ffi)
	return p.Value(), nil
}

// instantiateLater instantiates a module just compiled in a job of its own,
// as the compile's promise reaction would, settling p with the module and
// the instance.
func (a *api) instantiateLater(p *vm.HostPromise, md *moduleData, ffi vm.Value) {
	a.rt.EnqueueJob(func() {
		mod := a.moduleObject(md, a.moduleProto)
		inst, err := a.instantiateModule(a.thrower("WebAssembly.instantiate()"), md, ffi)
		a.settle(p, err, func() vm.Value {
			o := a.rt.NewPlainObject()
			vm.HostDefineOwn(o, a.rt.Intern("module"), vm.Obj(mod), true, true, true)
			vm.HostDefineOwn(o, a.rt.Intern("instance"), vm.Obj(a.instanceObject(inst, a.instanceProto)), true, true, true)
			return vm.Obj(o)
		})
	})
}
