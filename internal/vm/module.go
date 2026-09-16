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

	// env holds the module's top-level bindings, and doubles as its namespace
	// object.
	env *Object

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
func (m *Module) Namespace() *Object { return m.env }

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
		m.env.setOwnRaw(r.atoms.intern(imp.local), Obj(src.env), propEnumerable)
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
	r.defineAccessor(env, r.atoms.intern(as), getter, nil, propEnumerable)
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
	v, err := r.run(cl, Undefined, nil, Undefined, nil)
	if err != nil {
		m.state, m.err = ModuleFailed, err
		return Undefined, err
	}
	m.state = ModuleEvaluated
	return v, nil
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
