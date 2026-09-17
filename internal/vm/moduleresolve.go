package vm

import (
	"sort"
	"strings"
)

// Resolving what a module exports.
//
// An export name does not always name a binding of the module it is asked of.
// `export {x} from "m"` forwards to another module without binding anything
// locally, and `export * from "m"` forwards every name that module exports --
// so answering "where does this name come from" means walking the graph.
//
// Two answers other than a binding are possible, and both are errors at the
// point somebody asks for the name rather than where it was written. A name no
// module in the graph exports is not found. A name two different `export *`
// clauses lead to different bindings for is ambiguous: neither is the answer,
// and the name is simply not part of the namespace.
//
// A cycle is not an error. Walking one returns nothing for that path, which
// lets the other paths answer -- which is what makes two modules that import
// from each other link at all.

// reExportPrefix marks the hidden local name a re-export imports under.
//
// `export {x} from "m"` is compiled as an import under a name no identifier can
// spell, plus an export of that name, so that the binding is live and does not
// collide with anything the module declares itself. Resolution has to see
// through it: the export belongs to the other module, not to this one.
const reExportPrefix = "*re*"

// nsExportPrefix marks an export that is another module's namespace, which
// `export * as ns from "m"` makes. What it names is the module rather than a
// binding, so two modules re-exporting the same one under the same name agree.
const nsExportPrefix = "*ns*"

// nsBindingName stands for a namespace in place of a binding name.
const nsBindingName = "*namespace*"

// indirectSource picks apart the hidden name a re-export is stored under,
// reporting the module it names and the export it asks that module for.
func indirectSource(local string) (specifier, imported string, ok bool) {
	if !strings.HasPrefix(local, reExportPrefix) {
		return "", "", false
	}
	rest := local[len(reExportPrefix):]
	i := strings.Index(rest, ":")
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// exportBinding is where an export name leads.
//
// A nil binding means the name is not exported at all; one marked ambiguous
// means two star re-exports disagree about it.
type exportBinding struct {
	module    *Module
	local     string
	ambiguous bool
}

// exportRequest identifies one step of a resolution, so that a cycle is noticed
// rather than followed forever.
type exportRequest struct {
	module *Module
	name   string
}

// resolveExport finds the binding an export name leads to.
func (r *Runtime) resolveExport(m *Module, name string,
	seen map[exportRequest]bool) (*exportBinding, error) {
	if seen == nil {
		seen = make(map[exportRequest]bool)
	}
	req := exportRequest{m, name}
	if seen[req] {
		// This path is a cycle. Another may still answer, so it reports
		// nothing rather than failing.
		return nil, nil
	}
	seen[req] = true

	if local, ok := m.exports[name]; ok {
		if spec, isNS := strings.CutPrefix(local, nsExportPrefix); isNS {
			src, err := r.loadDependency(spec, m.Specifier)
			if err != nil {
				return nil, err
			}
			return &exportBinding{module: src, local: nsBindingName}, nil
		}
		spec, imported, indirect := indirectSource(local)
		if !indirect {
			// A name this module imported and then exported leads to where it
			// was imported from, not to this module: two modules that re-export
			// the same binding agree about it rather than conflicting.
			for _, imp := range m.imports {
				if imp.local != local {
					continue
				}
				src, err := r.loadDependency(imp.specifier, m.Specifier)
				if err != nil {
					return nil, err
				}
				if imp.namespace {
					return &exportBinding{module: src, local: nsBindingName}, nil
				}
				want := imp.imported
				if imp.isDefault {
					want = "default"
				}
				return r.resolveExport(src, want, seen)
			}
			return &exportBinding{module: m, local: local}, nil
		}
		src, err := r.loadDependency(spec, m.Specifier)
		if err != nil {
			return nil, err
		}
		return r.resolveExport(src, imported, seen)
	}

	// `export *` deliberately does not forward the default export, so a module
	// with no default of its own has none.
	if name == "default" {
		return nil, nil
	}

	var found *exportBinding
	for _, spec := range m.starExports {
		src, err := r.loadDependency(spec, m.Specifier)
		if err != nil {
			return nil, err
		}
		got, err := r.resolveExport(src, name, seen)
		if err != nil {
			return nil, err
		}
		if got == nil {
			continue
		}
		if got.ambiguous {
			return got, nil
		}
		if found == nil {
			found = got
			continue
		}
		// Two stars leading to the same binding agree; leading to different
		// ones they do not, and the name belongs to neither.
		if found.module != got.module || found.local != got.local {
			return &exportBinding{ambiguous: true}, nil
		}
	}
	return found, nil
}

// exportedNames lists every name a module exports, including the ones its star
// re-exports contribute.
//
// The default export is not among the ones a star contributes, which is why a
// module has to export it itself to have one.
func (r *Runtime) exportedNames(m *Module, seen map[*Module]bool) ([]string, error) {
	if seen == nil {
		seen = make(map[*Module]bool)
	}
	if seen[m] {
		return nil, nil
	}
	seen[m] = true

	var out []string
	for exported := range m.exports {
		if !isInternalModuleName(exported) {
			out = append(out, exported)
		}
	}
	for _, spec := range m.starExports {
		src, err := r.loadDependency(spec, m.Specifier)
		if err != nil {
			return nil, err
		}
		names, err := r.exportedNames(src, seen)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if n != "default" {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// namespaceNames lists the names a module's namespace object has, in the order
// it reports them.
//
// A name two star re-exports disagree about is left out: the namespace cannot
// say which binding it means, so it does not have it at all. Reading it through
// an import is an error instead.
func (r *Runtime) namespaceNames(m *Module) ([]string, error) {
	names, err := r.exportedNames(m, nil)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		b, err := r.resolveExport(m, n, nil)
		if err != nil {
			return nil, err
		}
		if b == nil || b.ambiguous {
			continue
		}
		out = append(out, n)
	}
	// In code unit order, which is what makes Object.keys of a namespace the
	// same across engines.
	sort.Slice(out, func(i, j int) bool {
		return NewString(out[i]).Compare(NewString(out[j])) < 0
	})
	return out, nil
}

// requireExport resolves a name an import asked for, reporting the two ways it
// can fail.
//
// Both are syntax errors, raised when the graph is linked rather than when the
// module was compiled: only the graph knows what the other modules export.
func (r *Runtime) requireExport(m *Module, name, what string) (*exportBinding, error) {
	b, err := r.resolveExport(m, name, nil)
	if err != nil {
		return nil, err
	}
	switch {
	case b == nil:
		return nil, r.throwError(errSyntax, "%q does not export %q", what, name)
	case b.ambiguous:
		return nil, r.throwError(errSyntax, "%q exports %q ambiguously", what, name)
	}
	return b, nil
}
