package wasmjs

import (
	"math"
	"strconv"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// moduleData is what a WebAssembly.Module carries: the compiled module and
// the compile-time imports it was compiled with.
type moduleData struct {
	m    *module.Module
	code *engine.Code
	ci   compileImports
	// builtins holds, for each import, the JS String builtin it is, or
	// noBuiltin.
	builtins []builtin
}

// compileImports are the compile-time imports a compile is asked for: the
// JS String builtins, and the module name string constants are imported
// from.
type compileImports struct {
	jsString     bool
	hasConstants bool
	constants    string
}

// bufferSource returns a copy of the bytes of args[0], a buffer source, or
// the error of one that is not or is empty.
func (a *api) bufferSource(t thrower, v vm.Value) ([]byte, error) {
	b, ok := vm.HostBufferSource(v)
	if !ok {
		return nil, t.typeError("Argument 0 must be a buffer source")
	}
	if len(b) == 0 {
		return nil, t.compileError("BufferSource argument is empty")
	}
	if uint64(len(b)) > a.limits.ModuleSize {
		return nil, t.compileError("buffer source exceeds maximum size of %d (is %d)", a.limits.ModuleSize, len(b))
	}
	return append([]byte(nil), b...), nil
}

// compileOptions reads the compile-time imports of a compile's options
// argument; anything but an object asks for none.
func (a *api) compileOptions(v vm.Value) (compileImports, error) {
	var ci compileImports
	if !v.IsObject() {
		return ci, nil
	}
	r := a.rt
	builtins, err := r.GetProp(v, r.Intern("builtins"))
	if err != nil {
		return ci, err
	}
	if builtins.IsObject() {
		lv, err := r.GetProp(builtins, r.Intern("length"))
		if err != nil {
			return ci, err
		}
		n, err := r.ToNumber(lv)
		if err != nil {
			return ci, err
		}
		length := uint32(0)
		switch {
		case n >= math.MaxUint32:
			length = math.MaxUint32
		case n > 0:
			length = uint32(n)
		}
		for i := uint32(0); i < length; i++ {
			key := r.Intern(strconv.FormatUint(uint64(i), 10))
			has, err := vm.HostHasProperty(r, builtins.Object(), key)
			if err != nil {
				return ci, err
			}
			if !has {
				continue
			}
			e, err := r.GetProp(builtins, key)
			if err != nil {
				return ci, err
			}
			if e.IsString() && e.String().Go() == "js-string" {
				ci.jsString = true
			}
		}
	}
	key := r.Intern("importedStringConstants")
	has, err := vm.HostHasProperty(r, v.Object(), key)
	if err != nil || !has {
		return ci, err
	}
	c, err := r.GetProp(v, key)
	if err != nil {
		return ci, err
	}
	if c.IsString() {
		ci.hasConstants, ci.constants = true, c.String().Go()
	}
	return ci, nil
}

// compile decodes and validates b. A synchronous compile checks the
// compile-time imports before the function bodies, and says where the
// import is; an asynchronous one checks them last, as validating the whole
// module again says nothing of them.
func (a *api) compile(t thrower, b []byte, ci compileImports, async bool) (*moduleData, error) {
	m, derr := decoder.Decode(b, a.limits)
	if derr != nil {
		return nil, t.compileError("%s", derr.Error())
	}
	md := &moduleData{m: m, ci: ci}
	if !async {
		if msg, off, ok := md.checkBuiltins(); !ok {
			return nil, t.compileError("%s @+%d", msg, off)
		}
	}
	if derr := decoder.ValidateFunctions(m, a.limits); derr != nil {
		return nil, t.compileError("%s", derr.Error())
	}
	if async {
		if msg, _, ok := md.checkBuiltins(); !ok {
			return nil, t.compileError("%s", msg)
		}
	}
	md.code = engine.NewCode(m)
	md.resolveBuiltins()
	return md, nil
}

// validate is WebAssembly.validate(bytes, options).
func (a *api) validate(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.validate()")
	b, err := a.bufferSource(t, arg(args, 0))
	if err != nil {
		// A buffer source that is empty or too large is not valid; anything
		// that is not one is a TypeError.
		if b, ok := vm.HostBufferSource(arg(args, 0)); ok && (len(b) == 0 || uint64(len(b)) > a.limits.ModuleSize) {
			return vm.False, nil
		}
		return vm.Undefined, err
	}
	ci, err := a.compileOptions(arg(args, 1))
	if err != nil {
		if _, ok := thrownValue(err); !ok {
			return vm.Undefined, err
		}
		return vm.False, nil
	}
	m, derr := decoder.Decode(b, a.limits)
	if derr != nil {
		return vm.False, nil
	}
	if derr := decoder.ValidateFunctions(m, a.limits); derr != nil {
		return vm.False, nil
	}
	md := &moduleData{m: m, ci: ci}
	_, _, ok := md.checkBuiltins()
	return vm.Bool(ok), nil
}

// compile is WebAssembly.compile(bytes, options).
func (a *api) compileFn(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.compile()")
	p := rt.NewHostPromise()
	md, err := a.compileArgs(t, arg(args, 0), arg(args, 1))
	a.settle(p, err, func() vm.Value { return vm.Obj(a.moduleObject(md, a.moduleProto)) })
	return p.Value(), nil
}

// compileArgs compiles a buffer source asynchronously, with options.
func (a *api) compileArgs(t thrower, source, options vm.Value) (*moduleData, error) {
	b, err := a.bufferSource(t, source)
	if err != nil {
		return nil, err
	}
	ci, err := a.compileOptions(options)
	if err != nil {
		return nil, err
	}
	return a.compile(t, b, ci, true)
}

// settle settles p: rejected with what err throws, or fulfilled with what
// value makes. A failure that is not an exception -- the host stopping the
// script -- is left to stop it.
func (a *api) settle(p *vm.HostPromise, err error, value func() vm.Value) {
	if err != nil {
		if v, ok := thrownValue(err); ok {
			p.Reject(v)
		}
		return
	}
	p.Resolve(value())
}

// newModule is new WebAssembly.Module(bytes, options).
func (a *api) newModule(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Module()")
	if !rt.Constructing() {
		return vm.Undefined, t.typeError("WebAssembly.Module must be invoked with 'new'")
	}
	proto, err := a.newTargetProto(rt)
	if err != nil {
		return vm.Undefined, err
	}
	b, err := a.bufferSource(t, arg(args, 0))
	if err != nil {
		return vm.Undefined, err
	}
	ci, err := a.compileOptions(arg(args, 1))
	if err != nil {
		return vm.Undefined, err
	}
	md, err := a.compile(t, b, ci, false)
	if err != nil {
		return vm.Undefined, err
	}
	return vm.Obj(a.moduleObject(md, proto)), nil
}

// moduleObject makes the WebAssembly.Module of md.
func (a *api) moduleObject(md *moduleData, proto *vm.Object) *vm.Object {
	return vm.HostObject(proto, md)
}

// moduleOf returns the module v is the WebAssembly.Module of, or nil.
func moduleOf(v vm.Value) *moduleData {
	md, _ := vm.HostData(v).(*moduleData)
	return md
}

// moduleStatics defines Module.imports, exports and customSections, and
// makes Module a subclass of %AbstractModuleSource%, as a module source.
func (a *api) moduleStatics(ns *vm.Object) {
	r := a.rt
	ctorVal, _ := r.GetProp(vm.Obj(ns), r.Intern("Module"))
	ctor := ctorVal.Object()
	if ams := r.AbstractModuleSource(); ams != nil {
		vm.HostSetPrototype(ctor, ams)
		if p, err := r.GetProp(vm.Obj(ams), r.Intern("prototype")); err == nil && p.IsObject() {
			vm.HostSetPrototype(a.moduleProto, p.Object())
		}
	}
	a.method(ctor, "imports", 1, a.moduleImports)
	a.method(ctor, "exports", 1, a.moduleExports)
	a.method(ctor, "customSections", 2, a.moduleCustomSections)
}

var kindNames = [...]string{
	module.ExternFunc:   "function",
	module.ExternTable:  "table",
	module.ExternMemory: "memory",
	module.ExternGlobal: "global",
	module.ExternTag:    "tag",
}

// descriptor makes an object of the properties given, in order.
func (a *api) descriptor(kv ...string) vm.Value {
	o := a.rt.NewPlainObject()
	for i := 0; i < len(kv); i += 2 {
		vm.HostDefineOwn(o, a.rt.Intern(kv[i]), vm.Str(vm.NewString(kv[i+1])), true, true, true)
	}
	return vm.Obj(o)
}

// moduleImports is WebAssembly.Module.imports(module).
func (a *api) moduleImports(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	md := moduleOf(arg(args, 0))
	if md == nil {
		return vm.Undefined, a.thrower("WebAssembly.Module.imports()").typeError("Argument 0 must be a WebAssembly.Module")
	}
	var out []vm.Value
	for i, imp := range md.m.Imports {
		if md.compileTimeImport(i) {
			continue
		}
		out = append(out, a.descriptor("module", imp.Module, "name", imp.Name, "kind", kindNames[imp.Kind]))
	}
	return vm.Obj(rt.NewArrayOf(out)), nil
}

// moduleExports is WebAssembly.Module.exports(module).
func (a *api) moduleExports(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	md := moduleOf(arg(args, 0))
	if md == nil {
		return vm.Undefined, a.thrower("WebAssembly.Module.exports()").typeError("Argument 0 must be a WebAssembly.Module")
	}
	out := make([]vm.Value, 0, len(md.m.Exports))
	for _, e := range md.m.Exports {
		out = append(out, a.descriptor("name", e.Name, "kind", kindNames[e.Kind]))
	}
	return vm.Obj(rt.NewArrayOf(out)), nil
}

// moduleCustomSections is WebAssembly.Module.customSections(module, name).
func (a *api) moduleCustomSections(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.Module.customSections()")
	md := moduleOf(arg(args, 0))
	if md == nil {
		return vm.Undefined, t.typeError("Argument 0 must be a WebAssembly.Module")
	}
	if arg(args, 1).IsUndefined() {
		return vm.Undefined, t.typeError("Argument 1 is required")
	}
	s, err := rt.ToString(arg(args, 1))
	if err != nil {
		return vm.Undefined, err
	}
	name := s.Go()
	out := []vm.Value{}
	for _, c := range md.m.Customs {
		if c.Name != name {
			continue
		}
		data := append([]byte{}, md.m.Bytes[c.Data.Offset:c.Data.End()]...)
		out = append(out, rt.NewArrayBufferOf(data, false, false))
	}
	return vm.Obj(rt.NewArrayOf(out)), nil
}

// compileTimeImport reports whether import i is provided at compile time:
// a JS String builtin, or a string constant.
func (md *moduleData) compileTimeImport(i int) bool {
	if md.builtins != nil && md.builtins[i] != noBuiltin {
		return true
	}
	imp := md.m.Imports[i]
	return imp.Kind == module.ExternGlobal && md.ci.hasConstants && imp.Module == md.ci.constants
}
