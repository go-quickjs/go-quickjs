package vm

import "strconv"

// What a host needs of the module system to give a kind of module of its own
// -- a WebAssembly module, imported from JavaScript -- a place in it: a module
// it makes from the source a loader gives, which imports from other modules
// and exports bindings of its own, and the module source a source phase
// import of it gives. And what JSON and structured clone make of the opaque
// objects a host stands for by proxies -- WebAssembly's GC objects.
//
// The functions are package variables assigned in init, as host_hooks.go's
// are, so that adding them moves no code before them.

// HostModule is a module a host makes from source of a kind of its own. It
// is loaded, linked and evaluated in its turn in the graph, as a module whose
// body imports each of Requests as a namespace and declares a binding for
// each of Exports, and whose body Evaluate is.
type HostModule struct {
	// Name is the name the module is loaded under: what the loader resolved
	// to it, which its requests are resolved against.
	Name string
	// Requests are the specifiers of the modules it imports from, in the
	// order they are loaded and evaluated.
	Requests []string
	// Exports are the names it exports, which are distinct. Each is in its
	// temporal dead zone until the module is evaluated, and undefined from
	// when its evaluation starts until Evaluate gives it a value.
	Exports []string
	// Source is the module source a source phase import of the module
	// gives, or undefined for a module that has none.
	Source Value
	// Evaluate gives the exports their values, in the order of Exports,
	// from the namespaces of Requests, in theirs. An error is the module's
	// evaluation's.
	Evaluate func(namespaces []*Object) ([]Value, error)
}

var (
	// HostSetModuleCompiler installs, for r, what makes the modules of a
	// host's kind from the source a loader gives: compile reports whether
	// source, under the name the loader resolved, is of that kind and, if
	// it is, the module HostDefineModule made of it, or the error importing
	// it fails with. Source it does not take is compiled as JavaScript.
	HostSetModuleCompiler func(r *Runtime, compile func(name, source string) (*Module, bool, error))
	// HostDefineModule registers the module h describes under its name, and
	// returns it; a name taken already keeps its module, which is returned.
	HostDefineModule func(r *Runtime, h HostModule) *Module
)

// HostModuleSourceClass is the data (HostObject) of a module source a host
// makes: %AbstractModuleSource%.prototype[Symbol.toStringTag] is, for the
// object, the name of its class ModuleSourceClassName gives.
type HostModuleSourceClass interface {
	ModuleSourceClassName() string
}

// HostOpaque is the data (HostObject) of a proxy handler whose proxies stand
// for a host's objects that have no JSON form -- WebAssembly's structs and
// arrays: JSON.stringify leaves such a proxy out, as it leaves out a
// function.
type HostOpaque struct{}

var (
	// hostNoJSON reports whether o, a proxy, stands for an object with no
	// JSON form (HostOpaque).
	hostNoJSON func(o *Object) bool
	// hostDescribeTarget names the object t, which a proxy that cannot be
	// cloned stands for, where it may run no code: an error by its name and
	// message, anything else as describeReceiver names it.
	hostDescribeTarget func(r *Runtime, t *Object) string
	// hostModuleSourceTag is what %AbstractModuleSource%.prototype's
	// Symbol.toStringTag getter answers for this: the class of a module
	// source, or undefined for anything else.
	hostModuleSourceTag func(this Value) Value
	// hostCompileModule makes the module of the source a loader gave for
	// name: the host's, if the module compiler HostSetModuleCompiler
	// installed takes it, or the runtime's compile of it as JavaScript.
	hostCompileModule func(r *Runtime, name, source string) (*Module, error)
	// hostModuleSource is the module source a source phase import of m
	// gives, or the SyntaxError of a module that has none.
	hostModuleSource func(r *Runtime, m *Module) (Value, error)
)

// hostModuleCompilerSymbol is the key the global object keeps the host's
// module compiler under, where script cannot reach it.
var hostModuleCompilerSymbol = NewSymbol("host module compiler", true)

// moduleSourceBinding is the binding a host's module keeps its module source
// in: a name no identifier can be, as the compiler's own are.
const moduleSourceBinding = "*source*"

func init() {
	HostSetModuleCompiler = func(r *Runtime, compile func(name, source string) (*Module, bool, error)) {
		holder := newObjectExt(nil, ClassObject, 0)
		holder.setData(compile)
		r.global.setOwnRaw(r.atoms.internSymbol(hostModuleCompilerSymbol), Obj(holder), propPrivate)
	}
	hostCompileModule = func(r *Runtime, name, source string) (*Module, error) {
		p := r.global.getOwn(r.atoms.internSymbol(hostModuleCompilerSymbol))
		if p.ok() && p.flags()&propPrivate != 0 && p.value().IsObject() {
			if compile, ok := p.value().Object().dataOf().(func(string, string) (*Module, bool, error)); ok {
				var m *Module
				var taken bool
				var err error
				r.hostCall(func() { m, taken, err = compile(name, source) })
				if r.stopped != nil {
					return nil, r.stopped
				}
				if taken {
					return m, err
				}
			}
		}
		if r.compileModule == nil {
			return nil, r.throwError(errType, "this runtime cannot compile modules")
		}
		return r.compileModule(name, source)
	}
	HostDefineModule = func(r *Runtime, h HostModule) *Module {
		if m, ok := r.modules[h.Name]; ok {
			return m
		}
		m := r.newModule(h.Name, nil)
		m.requests = append([]string(nil), h.Requests...)
		// The bindings have names of their own, which no export name can
		// collide with.
		locals := make([]Atom, len(h.Exports))
		for i, name := range h.Exports {
			local := "*export " + strconv.Itoa(i) + "*"
			m.exports[name] = local
			locals[i] = r.atoms.intern(local)
			m.env.setOwnRaw(locals[i], uninitialized, 0)
		}
		if !h.Source.IsUndefined() {
			m.env.setOwnRaw(r.atoms.intern(moduleSourceBinding), h.Source, 0)
		}
		evaluate := h.Evaluate
		m.synthetic = func() ([]NativeExport, error) {
			for _, k := range locals {
				m.env.setOwnRaw(k, Undefined, 0)
			}
			namespaces := make([]*Object, len(m.requests))
			for i, request := range m.requests {
				dep, err := r.loadDependency(request, m.Specifier)
				if err != nil {
					return nil, err
				}
				if namespaces[i], err = r.namespaceObject(dep); err != nil {
					return nil, err
				}
			}
			values, err := evaluate(namespaces)
			if err != nil {
				return nil, err
			}
			for i, v := range values {
				if i < len(locals) {
					m.env.setOwnRaw(locals[i], v, 0)
				}
			}
			return nil, nil
		}
		if r.modules == nil {
			r.modules = make(map[string]*Module)
		}
		r.modules[h.Name] = m
		return m
	}
	hostNoJSON = func(o *Object) bool {
		p := proxyOf(o)
		if p == nil || p.handler == nil {
			return false
		}
		_, ok := p.handler.dataOf().(HostOpaque)
		return ok
	}
	hostDescribeTarget = func(r *Runtime, t *Object) string {
		if t.class != ClassError {
			return r.describeReceiver(t)
		}
		// A data property's string, through the prototypes, or "".
		data := func(key Atom) (string, bool) {
			for p := t; p != nil && proxyOf(p) == nil; p = p.proto() {
				if d := p.getOwnVisible(key); d.ok() {
					if d.isAccessor() || !d.value().IsString() {
						return "", false
					}
					return d.value().String().Go(), true
				}
			}
			return "", false
		}
		name, ok := data(atomName)
		if !ok {
			name = "Error"
		}
		msg, _ := data(atomMessage)
		switch {
		case name == "":
			return msg
		case msg == "":
			return name
		}
		return name + ": " + msg
	}
	hostModuleSourceTag = func(this Value) Value {
		if !this.IsObject() {
			return Undefined
		}
		if c, ok := this.Object().dataOf().(HostModuleSourceClass); ok {
			return Str(NewString(c.ModuleSourceClassName()))
		}
		return Undefined
	}
	hostModuleSource = func(r *Runtime, m *Module) (Value, error) {
		if p := m.env.getOwn(r.atoms.intern(moduleSourceBinding)); p.ok() {
			return p.value(), nil
		}
		return Undefined, r.moduleSourceError(m)
	}
}
