package vm

import (
	"sort"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

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
	// ModuleEvaluatingAsync is a module whose body has started and is waiting:
	// it awaited at the top level, or one of its dependencies did.
	ModuleEvaluatingAsync
	ModuleEvaluated
	ModuleFailed
)

// Module is one compiled module.
type Module struct {
	// Specifier is the resolved name the module was loaded under, and is what
	// an import inside it resolves against.
	Specifier string
	fn        *bytecode.Function
	// initFn creates the module's bindings and defines its top-level
	// functions. It runs when the module is linked, so that a module of a
	// cycle can call a function of one whose body has not run yet.
	initFn *bytecode.Function

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
	// requests lists every module this one names, in the order it named them.
	// Loading and evaluation follow it, so a dependency runs before the module
	// that asked for it and dependencies run in the order they were written --
	// whether the line that named one imports a binding, re-exports one, or
	// neither.
	requests []string

	state ModuleState
	// The evaluation walk's bookkeeping: where the module was reached, the
	// lowest index reachable from it -- which is what finds a cycle -- and the
	// root of the cycle it belongs to.
	dfsIndex    int
	dfsAncestor int
	cycleRoot   *Module
	// asyncEval marks a module that has started but not finished, asyncOrder
	// is when it started, and pendingAsync counts the dependencies it is still
	// waiting for. asyncParents are the modules waiting for this one.
	asyncEval    bool
	asyncOrder   int
	pendingAsync int
	asyncParents []*Module
	// topLevel is the promise for the whole graph, held by its root.
	topLevel *promiseCapability
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
func (r *Runtime) ModuleNamespace(m *Module) (*Object, error) { return r.namespaceObject(m) }

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

// ModuleShape is what the compiler learned about a module, which the linker
// needs: what it brings in, what it exposes, what it depends on, and the
// function that creates its bindings.
type ModuleShape struct {
	Body        *bytecode.Function
	Init        *bytecode.Function
	Imports     []ModuleImportRequest
	Exports     map[string]string
	StarExports []string
	Requests    []string
}

// LoadModule registers a compiled module, returning it without linking or
// evaluating.
func (r *Runtime) LoadModule(specifier string, shape ModuleShape) (*Module, error) {
	fn, imports, exports := shape.Body, shape.Imports, shape.Exports
	starExports, requests := shape.StarExports, shape.Requests
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
	m.requests = requests
	m.initFn = shape.Init

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

	// The dependencies are loaded and linked in the order they were named,
	// before any binding is resolved: what a name resolves to may live in a
	// module named on a later line.
	for _, spec := range m.requests {
		src, err := r.loadDependency(spec, m.Specifier)
		if err != nil {
			m.state, m.err = ModuleFailed, err
			return err
		}
		if err := r.Link(src); err != nil {
			m.state, m.err = ModuleFailed, err
			return err
		}
	}

	for _, imp := range m.imports {
		src, err := r.loadDependency(imp.specifier, m.Specifier)
		if err != nil {
			m.state, m.err = ModuleFailed, err
			return err
		}
		if err := r.bindImport(m, imp, src); err != nil {
			m.state, m.err = ModuleFailed, err
			return err
		}
	}

	// An export that names another module's binding has to name one that is
	// there, and unambiguously. Only the whole graph knows, which is why this
	// is a link-time error rather than one the compiler could have given.
	for _, local := range m.exports {
		spec, imported, indirect := indirectSource(local)
		if !indirect {
			continue
		}
		if _, err := r.requireExportFrom(m, spec, imported); err != nil {
			m.state, m.err = ModuleFailed, err
			return err
		}
	}

	// The bindings are created now, with the imports resolved and before any
	// of the graph is evaluated: a module of a cycle may be asked for a
	// function it declares before its body has run.
	if m.initFn != nil {
		cl := r.prepare(m.initFn)
		cl.env = m.env
		if _, err := r.run(cl, Undefined, nil, Undefined, nil); err != nil {
			m.state, m.err = ModuleFailed, err
			return err
		}
	}

	// An exported name is not a binding: `export { A as B } from "m"` gives
	// this module no B to read, only an entry in its namespace, which is built
	// from the export map rather than from the environment.
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
func (r *Runtime) bindImport(m *Module, imp moduleImport, src *Module) error {
	if imp.local == "" {
		// A side-effect import names nothing: loading the module is the whole
		// point of it.
		return nil
	}
	if imp.namespace {
		ns, err := r.namespaceObject(src)
		if err != nil {
			return err
		}
		m.env.setOwnRaw(r.atoms.intern(imp.local), Obj(ns), moduleBindingFlags(imp.local))
		return nil
	}
	name := imp.imported
	if imp.isDefault {
		name = "default"
	}
	b, err := r.requireExport(src, name, src.Specifier)
	if err != nil {
		return err
	}
	return r.bindResolved(m.env, imp.local, b)
}

// bindResolved installs the binding an export resolved to.
func (r *Runtime) bindResolved(env *Object, as string, b *exportBinding) error {
	if b.local == nsBindingName {
		// The export is another module's namespace rather than one of its
		// bindings.
		ns, err := r.namespaceObject(b.module)
		if err != nil {
			return err
		}
		env.setOwnRaw(r.atoms.intern(as), Obj(ns), moduleBindingFlags(as))
		return nil
	}
	r.forwardBinding(env, as, b.module, b.local)
	return nil
}

// requireExportFrom resolves a name in the module a specifier names.
func (r *Runtime) requireExportFrom(m *Module, specifier, name string) (*exportBinding, error) {
	src, err := r.loadDependency(specifier, m.Specifier)
	if err != nil {
		return nil, err
	}
	return r.requireExport(src, name, specifier)
}

// forwardBinding installs an accessor that reads through to another module's
// binding, which is what makes an import live.
func (r *Runtime) forwardBinding(env *Object, as string, src *Module, local string) {
	key := r.atoms.intern(local)
	getter := r.newNativeFunc("get "+as, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.readModuleBinding(src, key, as)
	})
	// An imported binding is read-only; assigning to one is a TypeError, which
	// falls out of defining no setter in strict mode, and module code is
	// always strict.
	r.defineAccessor(env, r.atoms.intern(as), getter, nil, moduleBindingFlags(as))
}

// readModuleBinding reads another module's binding, which is what both an
// import and a namespace's export resolve to.
//
// A cycle makes it possible to reach one before the module that owns it has run
// the declaration, and reading it then is the same ReferenceError that module's
// own code would get.
func (r *Runtime) readModuleBinding(src *Module, key Atom, as string) (Value, error) {
	v, err := r.getProp(src.env, key, Obj(src.env))
	if err != nil {
		return Undefined, err
	}
	if v.IsUninitialized() {
		return Undefined, r.throwReferenceError(
			"cannot access %q before it is initialized", as)
	}
	return v, nil
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

// EvaluateModule evaluates a module and everything it depends on, returning a
// promise for the graph's completion.
//
// The walk is the specification's: depth-first over the dependencies in the
// order they were named, running each module's body once its own dependencies
// have run. A module that awaits at the top level does not hold up the walk --
// its body suspends and the walk carries on, so a sibling of it still runs --
// and the modules that depend on it are run later, when it finishes, in the
// order they were reached.
func (r *Runtime) EvaluateModule(m *Module) (Value, error) {
	if m.state == ModuleEvaluatingAsync || m.state == ModuleEvaluated {
		if m.cycleRoot != nil {
			// A module of a cycle is evaluated as part of its root, which is
			// what holds the promise for the whole group.
			m = m.cycleRoot
		}
	}
	if m.topLevel != nil {
		return Obj(m.topLevel.promise), nil
	}
	switch m.state {
	case ModuleUnlinked, ModuleLinking:
		if err := r.Link(m); err != nil {
			return Undefined, err
		}
	}
	cap, err := r.newPromiseCapability(Obj(r.promiseCtor))
	if err != nil {
		return Undefined, err
	}
	m.topLevel = cap

	var stack []*Module
	if _, evalErr := r.innerModuleEvaluation(m, &stack, 0); evalErr != nil {
		// Everything still on the stack failed with it: a module that cannot
		// finish leaves nothing behind that could be used.
		for _, mod := range stack {
			mod.state, mod.err = ModuleFailed, evalErr
			mod.asyncEval = false
		}
		if _, err := r.call(cap.reject, Undefined, []Value{thrownValue(evalErr)}); err != nil {
			return Undefined, err
		}
		return Obj(cap.promise), nil
	}
	if !m.asyncEval {
		if _, err := r.call(cap.resolve, Undefined, []Value{Undefined}); err != nil {
			return Undefined, err
		}
	}
	return Obj(cap.promise), nil
}

// innerModuleEvaluation is the depth-first walk, returning the index the next
// module in the walk should have.
//
// The indices are what find the cycles: a module whose lowest reachable index
// is its own is the root of one, and everything above it on the stack belongs
// to its group. A group finishes together, which is why the states are settled
// only when its root is reached.
func (r *Runtime) innerModuleEvaluation(m *Module, stack *[]*Module, index int) (int, error) {
	switch m.state {
	case ModuleEvaluatingAsync, ModuleEvaluated:
		return index, nil
	case ModuleFailed:
		return index, m.err
	case ModuleEvaluating:
		// Part of a cycle being walked, which the walk that started it
		// finishes.
		return index, nil
	}

	m.state = ModuleEvaluating
	m.dfsIndex, m.dfsAncestor = index, index
	m.pendingAsync = 0
	index++
	*stack = append(*stack, m)

	for _, spec := range m.requests {
		dep, ok := r.modules[r.resolvedNameOf(spec, m.Specifier)]
		if !ok {
			continue
		}
		var err error
		index, err = r.innerModuleEvaluation(dep, stack, index)
		if err != nil {
			return index, err
		}
		if dep.state == ModuleEvaluating {
			if dep.dfsAncestor < m.dfsAncestor {
				m.dfsAncestor = dep.dfsAncestor
			}
		} else {
			if dep.cycleRoot != nil {
				dep = dep.cycleRoot
			}
			if dep.state == ModuleFailed {
				return index, dep.err
			}
		}
		if dep.asyncEval {
			// The dependency has not finished, so this module waits for it
			// rather than running now.
			m.pendingAsync++
			dep.asyncParents = append(dep.asyncParents, m)
		}
	}

	switch {
	case m.pendingAsync > 0 || m.fn.Async:
		m.asyncEval = true
		r.asyncModuleOrder++
		m.asyncOrder = r.asyncModuleOrder
		if m.pendingAsync == 0 {
			if err := r.executeAsyncModule(m); err != nil {
				return index, err
			}
		}
	default:
		if err := r.executeModuleBody(m); err != nil {
			return index, err
		}
	}

	if m.dfsAncestor == m.dfsIndex {
		for {
			last := (*stack)[len(*stack)-1]
			*stack = (*stack)[:len(*stack)-1]
			if last.asyncEval {
				last.state = ModuleEvaluatingAsync
			} else {
				last.state = ModuleEvaluated
			}
			last.cycleRoot = m
			if last == m {
				break
			}
		}
	}
	return index, nil
}

// executeModuleBody runs a module's body, which has no top-level await and so
// finishes before it returns.
func (r *Runtime) executeModuleBody(m *Module) error {
	cl := r.prepare(m.fn)
	cl.env = m.env
	// Module code has undefined as its top-level `this`.
	_, err := r.run(cl, Undefined, nil, Undefined, nil)
	return err
}

// executeAsyncModule starts a module whose body awaits at the top level.
//
// The body suspends at its first await, and what happens when it finishes is
// arranged here: the modules waiting on it are run then, and the promise for
// the graph settles.
func (r *Runtime) executeAsyncModule(m *Module) error {
	cl := r.prepare(m.fn)
	cl.env = m.env
	gen, err := r.newGenerator(cl, Undefined, nil, nil, true)
	if err != nil {
		return err
	}
	promise := r.runAsync(gen.data.(*generator))
	onFulfilled := r.newNativeFunc("", 0, func(rt *Runtime, _ Value, _ []Value) (Value, error) {
		rt.asyncModuleFulfilled(m)
		return Undefined, nil
	})
	onRejected := r.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
		rt.asyncModuleRejected(m, arg(a, 0))
		return Undefined, nil
	})
	r.promiseThen(r.toPromise(promise), Obj(onFulfilled), Obj(onRejected))
	return nil
}

// asyncModuleFulfilled runs what was waiting on a module that has finished.
//
// The ones whose last dependency this was are run in the order they were
// reached, which is the order they would have run in had nothing awaited.
func (r *Runtime) asyncModuleFulfilled(m *Module) {
	if m.state == ModuleFailed {
		return
	}
	m.asyncEval = false
	m.state = ModuleEvaluated
	if m.topLevel != nil {
		r.call(m.topLevel.resolve, Undefined, []Value{Undefined})
	}

	var ready []*Module
	r.gatherAvailableAncestors(m, &ready)
	sort.SliceStable(ready, func(i, j int) bool {
		return ready[i].asyncOrder < ready[j].asyncOrder
	})
	for _, mod := range ready {
		switch {
		case mod.state == ModuleEvaluated || mod.state == ModuleFailed:
			continue
		case mod.fn.Async:
			if err := r.executeAsyncModule(mod); err != nil {
				r.asyncModuleRejected(mod, thrownValue(err))
			}
		default:
			if err := r.executeModuleBody(mod); err != nil {
				r.asyncModuleRejected(mod, thrownValue(err))
				continue
			}
			mod.state = ModuleEvaluated
			mod.asyncEval = false
			if mod.topLevel != nil {
				r.call(mod.topLevel.resolve, Undefined, []Value{Undefined})
			}
		}
	}
}

// asyncModuleRejected fails a module and everything waiting on it.
func (r *Runtime) asyncModuleRejected(m *Module, reason Value) {
	if m.state == ModuleFailed {
		return
	}
	m.asyncEval = false
	m.state, m.err = ModuleFailed, r.throw(reason)
	// The failure reaches this module's own waiter before the modules waiting
	// on it, so a graph settles from the leaf that failed outwards.
	if m.topLevel != nil {
		r.call(m.topLevel.reject, Undefined, []Value{reason})
	}
	for _, parent := range m.asyncParents {
		r.asyncModuleRejected(parent, reason)
	}
}

// gatherAvailableAncestors collects the modules whose last outstanding
// dependency has just finished, and so are ready to run.
func (r *Runtime) gatherAvailableAncestors(m *Module, ready *[]*Module) {
	for _, parent := range m.asyncParents {
		if containsModule(*ready, parent) {
			continue
		}
		if root := parent.cycleRoot; root != nil && root.state == ModuleFailed {
			continue
		}
		if parent.pendingAsync > 0 {
			parent.pendingAsync--
		}
		if parent.pendingAsync != 0 {
			continue
		}
		*ready = append(*ready, parent)
		if !parent.fn.Async {
			// A module with nothing of its own to await finishes as soon as it
			// runs, so whatever waits on it is ready too.
			r.gatherAvailableAncestors(parent, ready)
		}
	}
}

func containsModule(list []*Module, m *Module) bool {
	for _, x := range list {
		if x == m {
			return true
		}
	}
	return false
}

// ModuleResult reports what a promise from EvaluateModule has settled to: the
// failure of a graph that failed, and nothing for one that finished or is still
// waiting on the host.
func (r *Runtime) ModuleResult(promise Value) error {
	if !promise.IsObject() {
		return nil
	}
	p, _ := promise.Object().data.(*promiseData)
	if p == nil || p.state != promiseRejected {
		return nil
	}
	p.handled = true
	return r.throw(p.value)
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

		// The module is fetched and evaluated in a job rather than here. A
		// host's loading is asynchronous even when this one's is not, and the
		// difference is observable: the code that asked for the module runs to
		// the end of its turn before the module is evaluated, so a dynamic
		// import cannot preempt the evaluation it was written inside.
		rt.enqueueJob(func() {
			// A module that will not load, parse or link fails the way a
			// static import of it would, as an error the script can catch and
			// inspect -- not as a Go error the host would have to interpret.
			mod, err := rt.loadDependency(spec.Go(), "")
			if err != nil {
				rt.rejectPromise(result, thrownValue(rt.wrapEvalError(err)))
				return
			}
			if err := rt.Link(mod); err != nil {
				rt.rejectPromise(result, thrownValue(rt.wrapEvalError(err)))
				return
			}
			// Evaluation hands back a promise for the whole graph: a module
			// that awaits at the top level has not finished when this returns,
			// and the namespace is only handed over once it has.
			done, err := rt.EvaluateModule(mod)
			if err != nil {
				rt.rejectPromise(result, thrownValue(err))
				return
			}
			onFulfilled := rt.newNativeFunc("", 0, func(rt *Runtime, _ Value, _ []Value) (Value, error) {
				ns, err := rt.namespaceObject(mod)
				if err != nil {
					rt.rejectPromise(result, thrownValue(rt.wrapEvalError(err)))
					return Undefined, nil
				}
				rt.resolvePromise(result, Obj(ns))
				return Undefined, nil
			})
			onRejected := rt.newNativeFunc("", 1, func(rt *Runtime, _ Value, a []Value) (Value, error) {
				rt.rejectPromise(result, arg(a, 0))
				return Undefined, nil
			})
			rt.promiseThen(rt.toPromise(done), Obj(onFulfilled), Obj(onRejected))
		})
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
