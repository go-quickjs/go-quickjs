//go:build quickjs_wasm

package wasmjs

import (
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// A WebAssembly module in the module graph: the source a module loader gives
// for a name ending in .wasm, or that starts with the binary format's magic
// number, is compiled as WebAssembly. The module imports, as a namespace,
// each module its imports name, and exports a binding for each of its
// exports; evaluating it instantiates it with those namespaces as its
// imports object, and gives each binding its export -- a global's value, not
// the WebAssembly.Global -- once. A source phase import of it gives the
// WebAssembly.Module.

// wasmMagic is what a module in the binary format begins with, and no
// JavaScript source can.
const wasmMagic = "\x00asm"

// esmStringConstants is the module name a module imported as an ES module
// imports string constants from; it is compiled with the JS String builtins
// too.
const esmStringConstants = "wasm:js/string-constants"

// isWasmModule reports whether the source a loader gave for name is a
// WebAssembly module's.
func isWasmModule(name, source string) bool {
	if strings.HasPrefix(source, wasmMagic) {
		return true
	}
	path := name
	if strings.Contains(name, "://") {
		if i := strings.IndexAny(path, "?#"); i >= 0 {
			path = path[:i]
		}
	}
	return strings.HasSuffix(path, ".wasm")
}

// esmModule compiles the WebAssembly module of source, which a loader gave
// for name, and makes the module it is in the graph.
func (a *api) esmModule(name, source string) (*vm.Module, error) {
	r := a.rt
	t := a.thrower("WebAssembly.Module()")
	md, err := a.esmCompile(t, []byte(source))
	if err != nil {
		// The error says which module it is about.
		if v, ok := thrownValue(err); ok && v.IsObject() {
			key := r.Intern("message")
			if msg, gerr := r.GetProp(v, key); gerr == nil {
				if s, serr := r.ToString(msg); serr == nil {
					if serr := r.SetProp(v, key, vm.Str(vm.NewString(name+": "+s.Go()))); serr != nil {
						return nil, serr
					}
				}
			}
		}
		return nil, err
	}
	url := name
	if a.moduleURL != nil {
		url = a.moduleURL(name)
	}
	var requests []string
	seen := map[string]bool{}
	var globalImports []int
	for i, imp := range md.m.Imports {
		if md.compileTimeImport(i) {
			continue
		}
		if imp.Kind == module.ExternGlobal {
			globalImports = append(globalImports, i)
		}
		// Names of these prefixes are reserved.
		if strings.HasPrefix(imp.Module, "wasm-js:") {
			return nil, a.throwError(a.linkErrorProto, `Invalid Wasm import "`+imp.Module+`" in `+url)
		}
		if strings.HasPrefix(imp.Name, "wasm:") || strings.HasPrefix(imp.Name, "wasm-js:") {
			return nil, a.throwError(a.linkErrorProto, `Invalid Wasm import name "`+imp.Name+`" in `+url)
		}
		if !seen[imp.Module] {
			seen[imp.Module] = true
			requests = append(requests, imp.Module)
		}
	}
	exports := make([]string, 0, len(md.m.Exports))
	for _, e := range md.m.Exports {
		if strings.HasPrefix(e.Name, "wasm:") || strings.HasPrefix(e.Name, "wasm-js:") {
			return nil, a.throwError(a.linkErrorProto, `Invalid Wasm export name "`+e.Name+`" in `+url)
		}
		exports = append(exports, e.Name)
	}
	var m *vm.Module
	m = vm.HostDefineModule(r, vm.HostModule{
		Name:     name,
		Requests: requests,
		Exports:  exports,
		Source:   vm.Obj(a.moduleObject(md, a.moduleProto)),
		Evaluate: func(namespaces []*vm.Object) ([]vm.Value, error) {
			return a.esmEvaluate(m, md, requests, globalImports, namespaces)
		},
	})
	return m, nil
}

// esmCompile compiles the bytes of a module imported as an ES module, with
// the JS String builtins and string constants, as new WebAssembly.Module
// would.
func (a *api) esmCompile(t thrower, b []byte) (*moduleData, error) {
	if len(b) == 0 {
		return nil, t.compileError("BufferSource argument is empty")
	}
	if uint64(len(b)) > a.limits.ModuleSize {
		return nil, t.compileError("buffer source exceeds maximum size of %d (is %d)", a.limits.ModuleSize, len(b))
	}
	ci := compileImports{jsString: true, hasConstants: true, constants: esmStringConstants}
	return a.compile(t, b, ci, false)
}

// esmEvaluate instantiates the module of md with the namespaces of the
// modules it imports from and returns the values of its exports. Where a
// module it imports from is a WebAssembly module, its globals are given as
// the WebAssembly.Globals they are, not as the values its bindings hold.
func (a *api) esmEvaluate(m *vm.Module, md *moduleData, requests []string, globalImports []int, namespaces []*vm.Object) ([]vm.Value, error) {
	r := a.rt
	imports := vm.HostObject(nil, nil)
	for i, request := range requests {
		v := vm.Obj(namespaces[i])
		if inst := a.esmInstances[namespaces[i]]; inst != nil {
			copied := vm.HostObject(nil, nil)
			if _, err := r.Call(a.objectAssign, vm.Undefined, []vm.Value{vm.Obj(copied), v}); err != nil {
				return nil, err
			}
			for _, gi := range globalImports {
				imp := md.m.Imports[gi]
				if imp.Module != request {
					continue
				}
				g, err := r.GetProp(vm.Obj(inst), r.Intern(imp.Name))
				if err != nil {
					return nil, err
				}
				vm.HostDefineOwn(copied, r.Intern(imp.Name), g, true, true, true)
			}
			v = vm.Obj(copied)
		}
		vm.HostDefineOwn(imports, r.Intern(request), v, true, true, true)
	}
	inst, err := a.instantiateModule(a.thrower("WebAssembly.Instance()"), md, vm.Obj(imports))
	if err != nil {
		return nil, err
	}
	exports := vm.HostData(vm.Obj(a.instanceObject(inst, a.instanceProto))).(*instanceData).exports
	if ns, err := r.ModuleNamespace(m); err == nil {
		a.esmInstances[ns] = exports
	}
	values := make([]vm.Value, len(md.m.Exports))
	for i, e := range md.m.Exports {
		v, err := r.GetProp(vm.Obj(exports), r.Intern(e.Name))
		if err != nil {
			return nil, err
		}
		if e.Kind == module.ExternGlobal {
			// A global's value, or undefined for one that has no
			// JavaScript value -- a v128's.
			if v, err = r.GetProp(v, r.Intern("value")); err != nil {
				if _, ok := thrownValue(err); !ok {
					return nil, err
				}
				v = vm.Undefined
			}
		}
		values[i] = v
	}
	return values, nil
}
