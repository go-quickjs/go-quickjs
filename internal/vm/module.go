package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Modules.
//
// A module's top-level bindings live in an environment object whose prototype
// is the global object, so an unqualified name finds a module binding first and
// falls through to a global otherwise -- the prototype chain does the scope
// lookup for free.
//
// Imports are accessor properties on the importing module's environment that
// forward to the exporting module's. That is what makes a binding live: the
// importer reads through to the exporter's current value rather than a copy
// taken at link time, so `export let n = 0; n++` is visible to whoever imported
// n.

// ModuleState is where a module is in its lifecycle.
type ModuleState uint8

const (
	// ModuleUnlinked is a module that has been compiled but whose imports have
	// not been resolved.
	ModuleUnlinked ModuleState = iota
	ModuleLinking
	ModuleLinked
	ModuleEvaluating
	ModuleEvaluated
	ModuleFailed
)

// Module is one compiled module.
type Module struct {
	// Specifier is the resolved name the module was loaded under, and is what
	// an import inside it resolves against.
	Specifier string
	fn        *bytecode.Function

	// env holds the module's top-level bindings. Its prototype is the global
	// object, so that an unqualified name finds a module binding first and a
	// global otherwise -- which is the opposite of what a namespace needs, and
	// why the two are different objects.
	env *Object
	// ns is the module namespace object, built on first use.
	ns *Object

	// imports records what the module needs, resolved during linking.
	imports []moduleImport
	// exports maps an exported name to the local name that backs it.
	exports map[string]string
	// starExports lists the modules re-exported wholesale.
	starExports []string

	state ModuleState
	// err holds the failure of a module that threw while evaluating, which is
	// re-raised for every later importer rather than re-running the body.
	err error
}

// moduleImport is one resolved import binding.
type moduleImport struct {
	specifier string
	// local is the name bound in the importing module.
	local string
	// imported is the name read from the source module, empty for a namespace
	// import.
	imported string
	// namespace marks `import * as ns`.
	namespace bool
	// isDefault marks `import d from`.
	isDefault bool
}

// ModuleLoader resolves and fetches a module's source.
//
// The engine never touches the filesystem itself: a host that wants imports
// supplies a loader, and one that does not gets a runtime error for any import.
// That is what keeps a sandboxed runtime sealed.
type ModuleLoader func(specifier, referrer string) (source string, resolved string, err error)

// SetModuleLoader installs the loader used to resolve imports.
func (r *Runtime) SetModuleLoader(fn ModuleLoader) { r.moduleLoader = fn }

// ModuleNamespace returns a module's namespace object, for a host that wants to
// read its exports.
func (r *Runtime) ModuleNamespace(m *Module) *Object { return r.namespaceObject(m) }

// newModule prepares a compiled module for linking.
func (r *Runtime) newModule(specifier string, fn *bytecode.Function) *Module {
	m := &Module{
		Specifier: specifier,
		fn:        fn,
		// The environment inherits from the global object, so an unqualified
		// name finds a module binding first and a global otherwise.
		env:     newObject(r.global, ClassObject),
		exports: make(map[string]string),
	}
	r.defToStringTag(m.env, "Module")
	return m
}

// LoadModule compiles and links a module, returning it without evaluating.
func (r *Runtime) LoadModule(specifier string, fn *bytecode.Function, imports []ModuleImportRequest, exports map[string]string, starExports []string) (*Module, error) {
	if m, ok := r.modules[specifier]; ok {
		return m, nil
	}
	m := r.newModule(specifier, fn)
	for _, req := range imports {
		m.imports = append(m.imports, moduleImport{
			specifier: req.Specifier,
			local:     req.Local,
			imported:  req.Imported,
			namespace: req.Namespace,
			isDefault: req.IsDefault,
		})
	}
	for k, v := range exports {
		m.exports[k] = v
	}
	m.starExports = starExports

	if r.modules == nil {
		r.modules = make(map[string]*Module)
	}
	r.modules[specifier] = m
	return m, nil
}

// ModuleImportRequest is one import a compiled module declares.
type ModuleImportRequest struct {
	Specifier string
	Local     string
	Imported  string
	Namespace bool
	IsDefault bool
}

// Link resolves a module's imports, loading whatever they name.
//
// A cycle is not an error: a module already being linked is left alone, and its
// bindings resolve once it finishes. That is why imports are accessors rather
// than copies -- a cyclic import reads a binding that does not have a value
// yet, and must see it appear rather than capture undefined.
func (r *Runtime) Link(m *Module) error {
	switch m.state {
	case ModuleLinked, ModuleEvaluating, ModuleEvaluated:
		return nil
	case ModuleLinking:
		// Part of a cycle; the module that started the cycle finishes the job.
		return nil
	case ModuleFailed:
		return m.err
	}
	m.state = ModuleLinking

	for _, imp := range m.imports {
		src, err := r.loadDependency(imp.specifier, m.Specifier)
		if err != nil {
			m.state, m.err = ModuleFailed, err
			return err
		}
		if err := r.Link(src); err != nil {
			m.state, m.err = ModuleFailed, err
			return err
		}
		r.bindImport(m, imp, src)
	}

	for _, spec := range m.starExports {
		src, err := r.loadDependency(spec, m.Specifier)
		if err != nil {
			m.state, m.err = ModuleFailed, err
			return err
		}
		if err := r.Link(src); err != nil {
			m.state, m.err = ModuleFailed, err
			return err
		}
		// A star re-export forwards every name the source exports.
		for exported, local := range src.exports {
			if exported == "default" {
				// `export *` deliberately does not forward the default export.
				continue
			}
			m.exports[exported] = exported
			r.forwardBinding(m.env, exported, src, local)
		}
	}

	// An export whose public name differs from the binding that backs it needs
	// an entry in the namespace. The default export always does, since it is
	// stored under a name no identifier can spell.
	for exported, local := range m.exports {
		if exported == local {
			continue
		}
		if m.env.getOwn(r.atoms.intern(exported)) != nil {
			continue
		}
		r.forwardBinding(m.env, exported, m, local)
	}

	m.state = ModuleLinked
	return nil
}

// loadDependency resolves and compiles a module a specifier names.
func (r *Runtime) loadDependency(specifier, referrer string) (*Module, error) {
	if r.moduleLoader == nil {
		return nil, r.throwError(errType,
			"cannot import %q: this runtime has no module loader", specifier)
	}
	source, resolved, err := r.moduleLoader(specifier, referrer)
	if err != nil {
		return nil, r.throwError(errType, "cannot resolve %q: %s", specifier, err.Error())
	}
	if m, ok := r.modules[resolved]; ok {
		return m, nil
	}
	if r.compileModule == nil {
		return nil, r.throwError(errType, "this runtime cannot compile modules")
	}
	return r.compileModule(resolved, source)
}

// SetModuleCompiler installs the function that turns module source into a
// linked Module. The public package supplies it, which keeps the parser and
// compiler out of this package's imports.
func (r *Runtime) SetModuleCompiler(fn func(specifier, source string) (*Module, error)) {
	r.compileModule = fn
}

// bindImport installs one import as a live binding.
func (r *Runtime) bindImport(m *Module, imp moduleImport, src *Module) {
	if imp.namespace {
		// A namespace import is the source module's environment itself.
		m.env.setOwnRaw(r.atoms.intern(imp.local), Obj(r.namespaceObject(src)),
			moduleBindingFlags(imp.local))
		return
	}
	name := imp.imported
	if imp.isDefault {
		name = "default"
	}
	local, ok := src.exports[name]
	if !ok {
		// Resolving to nothing is deferred rather than reported here, so that a
		// cyclic import still linking is not rejected.
		local = name
	}
	r.forwardBinding(m.env, imp.local, src, local)
}

// forwardBinding installs an accessor that reads through to another module's
// binding, which is what makes an import live.
func (r *Runtime) forwardBinding(env *Object, as string, src *Module, local string) {
	key := r.atoms.intern(local)
	getter := r.newNativeFunc("get "+as, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.getProp(src.env, key, Obj(src.env))
	})
	// An imported binding is read-only; assigning to one is a TypeError, which
	// falls out of defining no setter in strict mode, and module code is
	// always strict.
	r.defineAccessor(env, r.atoms.intern(as), getter, nil, moduleBindingFlags(as))
}

// moduleBindingFlags decides whether a module binding is part of the namespace.
//
// A module's environment doubles as its namespace object, so anything the
// compiler puts there for its own use -- the slot behind `export default`, or
// the private name a re-export imports under -- would otherwise show up as an
// export. Those names begin with a character no identifier may contain, which
// is what makes them safe to recognize.
func moduleBindingFlags(name string) propFlags {
	if isInternalModuleName(name) {
		return 0
	}
	return propEnumerable
}

// isInternalModuleName reports whether a binding is one the compiler
// synthesized rather than one the source named.
func isInternalModuleName(name string) bool {
	return len(name) > 0 && name[0] == '*'
}

// EvaluateModule runs a module's body, evaluating its dependencies first.
func (r *Runtime) EvaluateModule(m *Module) (Value, error) {
	switch m.state {
	case ModuleEvaluated:
		return Undefined, nil
	case ModuleEvaluating:
		// A cycle: the module is already on the stack, and its bindings will be
		// filled in when it finishes.
		return Undefined, nil
	case ModuleFailed:
		return Undefined, m.err
	case ModuleUnlinked, ModuleLinking:
		if err := r.Link(m); err != nil {
			return Undefined, err
		}
	}
	m.state = ModuleEvaluating

	// Dependencies run first, in the order they were imported.
	seen := make(map[string]bool)
	for _, imp := range m.imports {
		if seen[imp.specifier] {
			continue
		}
		seen[imp.specifier] = true
		dep, ok := r.modules[r.resolvedNameOf(imp.specifier, m.Specifier)]
		if !ok {
			continue
		}
		if _, err := r.EvaluateModule(dep); err != nil {
			m.state, m.err = ModuleFailed, err
			return Undefined, err
		}
	}

	cl := r.prepare(m.fn)
	cl.env = m.env
	// Module code has undefined as its top-level `this`.
	v, err := r.runModuleBody(m, cl)
	if err != nil {
		m.state, m.err = ModuleFailed, err
		return Undefined, err
	}
	m.state = ModuleEvaluated
	return v, nil
}

// runModuleBody evaluates a module, driving it as an async function when it
// uses top-level await.
//
// A module with a top-level await is asynchronous, so its body suspends and its
// completion is a promise. EvaluateModule is synchronous, so the microtask
// queue is drained here: a module awaiting something already settled finishes
// before this returns, which is every case that does not depend on a host
// timer. One that never settles leaves the module evaluating, and its failure
// -- if any -- surfaces as a rejection rather than being lost.
func (r *Runtime) runModuleBody(m *Module, cl *closure) (Value, error) {
	if !m.fn.Async {
		return r.run(cl, Undefined, nil, Undefined, nil)
	}

	gen, err := r.newGenerator(cl, Undefined, nil, nil, true)
	if err != nil {
		return Undefined, err
	}
	promise := r.runAsync(gen.data.(*generator))
	if err := r.DrainJobs(); err != nil {
		return Undefined, err
	}

	p, _ := promise.Object().data.(*promiseData)
	if p != nil && p.state == promiseRejected {
		p.handled = true
		return Undefined, r.throw(p.value)
	}
	if p != nil && p.state == promiseFulfilled {
		return p.value, nil
	}
	// Still pending: the module is waiting on something the host must settle.
	return Undefined, nil
}

// resolvedNameOf asks the loader what a specifier resolves to, so that the
// dependency can be found in the module table.
func (r *Runtime) resolvedNameOf(specifier, referrer string) string {
	if r.moduleLoader == nil {
		return specifier
	}
	_, resolved, err := r.moduleLoader(specifier, referrer)
	if err != nil {
		return specifier
	}
	return resolved
}

// scope returns the object a closure's unqualified names resolve against.
//
// Module code resolves against its own environment, which inherits from the
// global object; everything else resolves directly against the global object.
func (c *closure) scope() *Object {
	if c.env != nil {
		return c.env
	}
	return c.realm.global
}

// initDynamicImport defines the global that `import(...)` compiles into a call
// to.
//
// The parser turns `import(x)` into a call to an identifier named "import",
// which cannot collide with anything a script could write, since `import` is a
// reserved word.
func (r *Runtime) initDynamicImport() {
	fn := r.newNativeFunc("import", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// A dynamic import always returns a promise, so a failure to resolve
		// rejects rather than throws.
		result := rt.newPromise()

		spec, err := rt.toString(arg(args, 0))
		if err != nil {
			rt.rejectPromise(result, thrownValue(err))
			return Obj(result), nil
		}

		// A module that will not load, parse or link fails the way a static
		// import of it would, as an error the script can catch and inspect --
		// not as a Go error the host would have to interpret.
		mod, err := rt.loadDependency(spec.Go(), "")
		if err != nil {
			rt.rejectPromise(result, thrownValue(rt.wrapEvalError(err)))
			return Obj(result), nil
		}
		if err := rt.Link(mod); err != nil {
			rt.rejectPromise(result, thrownValue(rt.wrapEvalError(err)))
			return Obj(result), nil
		}
		if _, err := rt.EvaluateModule(mod); err != nil {
			rt.rejectPromise(result, thrownValue(err))
			return Obj(result), nil
		}
		rt.resolvePromise(result, Obj(rt.namespaceObject(mod)))
		return Obj(result), nil
	})
	r.global.setOwnRaw(r.atoms.intern("import"), Obj(fn), propWritable|propConfigurable)
}

// importMeta returns the running module's import.meta object, creating it on
// first use.
//
// It is an ordinary object with no prototype, and the host is free to put
// whatever it likes on it -- the specification defines no properties at all.
// Each module gets its own, and the same one every time, so a module can use it
// as a place to keep something of its own.
//
// It lives in the module's environment under a name beginning with the
// character the synthesized bindings use, which no source name may contain, so
// it is neither exported nor visible to the module's own code.
func (r *Runtime) importMeta(env *Object) Value {
	if env == nil {
		return Undefined
	}
	key := r.atoms.intern("*meta*")
	if p := env.getOwn(key); p != nil {
		return p.value
	}
	meta := Obj(newObject(nil, ClassObject))
	env.setOwnRaw(key, meta, 0)
	return meta
}
