package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Deferred module evaluation: `import defer * as ns from "m"` and
// `import.defer("m")`.
//
// A deferred import loads and links its module with the rest of the graph but
// does not run it. What it binds is a deferred namespace, a second namespace
// object of the module's, and using that object -- asking it about an export,
// or for its keys -- is what runs the module, synchronously, then and there.
//
// Asking it about a symbol, or about "then", does not: a symbol is never an
// export, and a promise machinery that looks for a then method on everything
// it is handed must not start a module by doing so. Neither does anything that
// is not about an export at all, such as its prototype or its extensibility.
//
// A module can only be run then and there if nothing it needs is waiting for
// something: one that awaits at the top level, or depends on one that does, or
// is already part-way through running, cannot be, and using its deferred
// namespace is a TypeError. So the modules of a deferred import that await at
// the top level are run with the graph that imports it, as ordinary imports
// would be; only what is left can wait until it is used.

// deferredModule marks a deferred namespace object, and is the module it
// stands for.
type deferredModule struct{ m *Module }

// deferredNamespaceObject returns the module's deferred namespace, building it
// on first use.
func (r *Runtime) deferredNamespaceObject(m *Module) (*Object, error) {
	if m.deferredNS != nil {
		return m.deferredNS, nil
	}
	ns, err := r.buildNamespace(m, true)
	if err != nil {
		return nil, err
	}
	m.deferredNS = ns
	return ns, nil
}

// touchDeferred runs the module a deferred namespace stands for, if the key
// being asked about is one that does: any string but "then". It does nothing
// for any other object.
func (r *Runtime) touchDeferred(o *Object, key Atom) error {
	d, ok := o.data.(*deferredModule)
	if !ok || r.atoms.IsSymbol(key) || r.atoms.name(key) == "then" {
		return nil
	}
	return r.ensureDeferredEvaluation(d.m)
}

// touchDeferredKeys runs the module a deferred namespace stands for, because
// its keys are being asked for -- which, unlike a single key, always does.
func (r *Runtime) touchDeferredKeys(o *Object) error {
	d, ok := o.data.(*deferredModule)
	if !ok {
		return nil
	}
	return r.ensureDeferredEvaluation(d.m)
}

// ensureDeferredEvaluation runs a module synchronously, for the deferred
// namespace that stands for it. A module that ran and threw throws the same
// error again.
func (r *Runtime) ensureDeferredEvaluation(m *Module) error {
	if !isModuleSCCEvaluated(m) && !r.readyForSyncExecution(m, map[*Module]bool{}) {
		return r.throwTypeError("the deferred module %q cannot be evaluated now", m.Specifier)
	}
	done, err := r.EvaluateModule(m)
	if err != nil {
		return err
	}
	if p, ok := done.Object().data.(*promiseData); ok && p.state == promiseRejected {
		return r.throw(p.value)
	}
	return nil
}

// isModuleSCCEvaluated reports whether a module has run, and so have the
// modules of its cycle: one that has run while its cycle's root is still
// waiting at a top-level await has not finished in the sense that matters,
// since what it depends on has not.
func isModuleSCCEvaluated(m *Module) bool {
	if m.state != ModuleEvaluated && m.state != ModuleFailed {
		return false
	}
	root := m.cycleRoot
	return root == nil || root == m || root.state == ModuleEvaluated || root.state == ModuleFailed
}

// hasTLA reports whether a module's body awaits at the top level.
func hasTLA(m *Module) bool { return m.fn != nil && m.fn.Async }

// readyForSyncExecution reports whether a module, and everything it depends
// on, can run to the end now: nothing of it is running already, and none of it
// awaits at the top level.
func (r *Runtime) readyForSyncExecution(m *Module, seen map[*Module]bool) bool {
	if seen[m] {
		return true
	}
	seen[m] = true
	if isModuleSCCEvaluated(m) {
		return true
	}
	if m.state == ModuleEvaluating || m.state == ModuleEvaluatingAsync {
		return false
	}
	if hasTLA(m) {
		return false
	}
	for _, request := range m.requests {
		if _, source := bytecode.SplitSourceRequest(request); source {
			continue
		}
		dep, ok := r.modules[r.resolvedNameOf(request, m.Specifier)]
		if ok && !r.readyForSyncExecution(dep, seen) {
			return false
		}
	}
	return true
}

// gatherAsyncDependencies lists the modules of m's graph that await at the top
// level and have not run, stopping at each: those are what a deferred import
// of m runs up front, since nothing could run them later without waiting.
func (r *Runtime) gatherAsyncDependencies(m *Module, seen map[*Module]bool, out []*Module) []*Module {
	if seen[m] {
		return out
	}
	seen[m] = true
	if m.state == ModuleEvaluating || isModuleSCCEvaluated(m) {
		return out
	}
	if hasTLA(m) {
		return appendModule(out, m)
	}
	for _, request := range m.requests {
		if _, source := bytecode.SplitSourceRequest(request); source {
			continue
		}
		if dep, ok := r.modules[r.resolvedNameOf(request, m.Specifier)]; ok {
			out = r.gatherAsyncDependencies(dep, seen, out)
		}
	}
	return out
}

func appendModule(list []*Module, m *Module) []*Module {
	if containsModule(list, m) {
		return list
	}
	return append(list, m)
}

// evaluationList is what evaluating a module walks first: each module it
// requests, in order, except that a deferred request contributes only the
// modules of its graph that await at the top level.
func (r *Runtime) evaluationList(m *Module) []*Module {
	var list []*Module
	for _, request := range m.requests {
		if _, source := bytecode.SplitSourceRequest(request); source {
			continue
		}
		dep, ok := r.modules[r.resolvedNameOf(request, m.Specifier)]
		if !ok {
			continue
		}
		if _, deferred := bytecode.SplitDeferRequest(request); deferred {
			for _, a := range r.gatherAsyncDependencies(dep, map[*Module]bool{}, nil) {
				list = appendModule(list, a)
			}
			continue
		}
		list = appendModule(list, dep)
	}
	return list
}

// importDeferred settles import.defer()'s promise: it loads and links the
// module, runs what of its graph awaits at the top level, and hands back its
// deferred namespace.
func (r *Runtime) importDeferred(request, referrer string, result *Object) {
	r.enqueueJob(func() {
		mod, err := r.loadDependency(request, referrer)
		if err == nil {
			err = r.Link(mod)
		}
		if err != nil {
			r.rejectPromise(result, thrownValue(r.wrapEvalError(err)))
			return
		}
		settle := func() {
			ns, err := r.deferredNamespaceObject(mod)
			if err != nil {
				r.rejectPromise(result, thrownValue(r.wrapEvalError(err)))
				return
			}
			r.resolvePromise(result, Obj(ns))
		}
		async := r.gatherAsyncDependencies(mod, map[*Module]bool{}, nil)
		if len(async) == 0 {
			settle()
			return
		}
		// What awaits at the top level is run now, and the namespace handed
		// over once all of it has finished.
		pending := len(async)
		failed := false
		for _, dep := range async {
			done, err := r.EvaluateModule(dep)
			if err != nil {
				r.rejectPromise(result, thrownValue(err))
				return
			}
			r.awaitThen(done, func(rt *Runtime, _ Value) {
				if pending--; pending == 0 && !failed {
					settle()
				}
			}, func(rt *Runtime, reason Value) {
				if !failed {
					failed = true
					rt.rejectPromise(result, reason)
				}
			})
		}
	})
}

// importSource settles import.source()'s promise. It asks for a module's
// source, which a module here never has: once the module is found it rejects,
// as linking a static source import of it fails.
func (r *Runtime) importSource(request, referrer string, result *Object) {
	r.enqueueJob(func() {
		mod, err := r.loadDependency(request, referrer)
		if err == nil {
			err = r.moduleSourceError(mod)
		}
		r.rejectPromise(result, thrownValue(r.wrapEvalError(err)))
	})
}

// initModuleSource defines %AbstractModuleSource%, what the module source
// classes of a host extend. It is abstract, and not a global: a host reaches
// it through the Runtime.
func (r *Runtime) initModuleSource() {
	proto := newObject(r.proto.object, ClassObject)
	r.abstractModuleSource = r.newCtor("AbstractModuleSource", 0, proto,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			return Undefined, rt.throwTypeError("AbstractModuleSource is abstract and cannot be constructed")
		})
	r.global.deleteOwn(r.atoms.intern("AbstractModuleSource"))
	// Its tag names the class of a module source, and nothing that is not
	// one has a class: with no such classes here, it is always undefined.
	get := r.newNativeFunc("get [Symbol.toStringTag]", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Undefined, nil
	})
	r.defineAccessor(proto, r.atoms.internSymbol(r.wellKnown.toStringTag), get, nil, propConfigurable)
}

// AbstractModuleSource returns %AbstractModuleSource%, the constructor a host's
// module source classes extend. It is not a property of the global object.
func (r *Runtime) AbstractModuleSource() *Object { return r.abstractModuleSource }
