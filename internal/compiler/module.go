package compiler

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Compiling a module's import and export declarations.
//
// A module's top-level bindings are compiled exactly like a script's globals:
// as named properties of an environment object. The difference is entirely at
// runtime, where that object is the module's own rather than the global one.
// This is what makes an export live -- the exported name and the module's own
// binding are the same property.

// ModuleInfo is what the compiler learned about a module's shape, which the
// linker needs.
type ModuleInfo struct {
	// Imports lists every binding the module brings in.
	Imports []ImportRequest
	// Exports maps an exported name to the local name backing it.
	Exports map[string]string
	// StarExports lists the modules re-exported wholesale.
	StarExports []string
}

// ImportRequest is one import binding.
type ImportRequest struct {
	Specifier string
	Local     string
	Imported  string
	Namespace bool
	IsDefault bool
}

// CompileModule compiles module source, returning the function and the shape
// information the linker needs.
func CompileModule(prog *ast.Program, opts Options) (fn *bytecode.Function, info ModuleInfo, err error) {
	info.Exports = make(map[string]string)

	c := newCompiler(nil, opts)
	c.fn.Name = "<module>"
	c.fn.Strict = true
	c.fn.IsModule = true
	c.fn.Source = opts.Source
	c.lineOf = lineMapper(opts.Text)
	c.module = &info

	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*Error); ok {
				fn, err = nil, e
				return
			}
			panic(r)
		}
	}()

	c.completionSlot = int32(c.nextSlot)
	c.nextSlot++
	c.emit(bytecode.OpPushUndef, 0, 0)
	c.emit(bytecode.OpSetLocal, uint32(c.completionSlot), 0)

	c.collectModuleShape(prog.Body)
	c.checkScopes(prog.Body)
	c.hoistModuleBindings(prog.Body)
	c.compileStatements(prog.Body)

	c.emit(bytecode.OpGetLocal, uint32(c.completionSlot), 0)
	c.emit(bytecode.OpReturn, 0, 0)
	c.finish()
	return c.fn, info, nil
}

// collectModuleShape records the imports and exports before the body is
// compiled, so that a reference to an imported name resolves correctly
// wherever it appears.
func (c *compiler) collectModuleShape(body []ast.Stmt) {
	for _, s := range body {
		switch n := s.(type) {
		case *ast.ImportDecl:
			for _, spec := range n.Specifiers {
				c.module.Imports = append(c.module.Imports, ImportRequest{
					Specifier: n.Source,
					Local:     spec.Local,
					Imported:  spec.Imported,
					Namespace: spec.Kind == ast.ImportNamespace,
					IsDefault: spec.Kind == ast.ImportDefault,
				})
			}
			if len(n.Specifiers) == 0 {
				// A side-effect import still has to be loaded and evaluated.
				c.module.Imports = append(c.module.Imports, ImportRequest{Specifier: n.Source})
			}

		case *ast.ExportDecl:
			c.collectExport(n)
		}
	}
}

// collectExport records the names one export declaration exposes.
func (c *compiler) collectExport(n *ast.ExportDecl) {
	switch {
	case n.All:
		if n.Alias != "" {
			// `export * as ns from "m"` exports the namespace under a name.
			c.module.Imports = append(c.module.Imports, ImportRequest{
				Specifier: n.Source, Local: n.Alias, Namespace: true,
			})
			c.module.Exports[n.Alias] = n.Alias
			return
		}
		c.module.StarExports = append(c.module.StarExports, n.Source)

	case len(n.Specifiers) > 0:
		for _, spec := range n.Specifiers {
			if n.Source != "" {
				// A re-export imports under a name no identifier can spell,
				// and exports that. Using the public name would shadow a
				// binding the module declares itself -- which happens whenever
				// a module re-exports a name it also defines, including the
				// degenerate case of re-exporting from its own specifier.
				local := "*re*" + n.Source + ":" + spec.Local
				c.module.Imports = append(c.module.Imports, ImportRequest{
					Specifier: n.Source, Local: local, Imported: spec.Local,
				})
				c.module.Exports[spec.Exported] = local
				continue
			}
			c.module.Exports[spec.Exported] = spec.Local
		}

	case n.Default:
		c.module.Exports["default"] = "*default*"

	case n.Decl != nil:
		for _, name := range declaredNames(n.Decl) {
			c.module.Exports[name] = name
		}
	}
}

// declaredNames returns the bindings a declaration introduces.
func declaredNames(s ast.Stmt) []string {
	var out []string
	switch n := s.(type) {
	case *ast.VarDecl:
		for _, d := range n.Decls {
			collectPatternNames(d.Target, &out)
		}
	case *ast.FuncDecl:
		if n.Fn.Name != nil {
			out = append(out, n.Fn.Name.Name)
		}
	case *ast.ClassDecl:
		if n.Class.Name != nil {
			out = append(out, n.Class.Name.Name)
		}
	}
	return out
}

// compileImportDecl emits nothing: the bindings are installed by the linker
// before the body runs.
func (c *compiler) compileImportDecl(n *ast.ImportDecl) {}

// compileExportDecl emits the declaration an export wraps, if any.
func (c *compiler) compileExportDecl(n *ast.ExportDecl) {
	switch {
	case n.Default:
		if n.Decl != nil {
			c.compileStatement(n.Decl)
			// A default-exported declaration is also bound under the name the
			// linker looks for.
			if names := declaredNames(n.Decl); len(names) > 0 {
				c.compileIdentRead(&ast.Ident{Name: names[0], Start: n.Start})
				c.emit(bytecode.OpDefineGlobalFunc, c.nameIdx("*default*"), 0)
			}
			return
		}
		c.compileExprNamed(n.DefaultExpr, "default")
		c.emit(bytecode.OpDefineGlobalFunc, c.nameIdx("*default*"), 0)

	case n.Decl != nil:
		// A function declaration was already emitted when the statement list
		// hoisted it; anything else is compiled here.
		if _, isFunc := n.Decl.(*ast.FuncDecl); !isFunc {
			c.compileStatement(n.Decl)
		}

	default:
		// A clause or a re-export declares no code of its own.
	}
}

// hoistModuleBindings declares every top-level binding of a module as a
// property of its environment.
//
// A script keeps let and const in frame slots, but a module cannot: an export
// has to name something the linker can forward to, and a slot is not reachable
// from outside the frame. Putting them in the environment is also what makes an
// export live, since the exported name and the module's own binding become the
// same property.
//
// The cost is that a module's top-level lexical bindings lose their temporal
// dead zone, reading as undefined before their declaration rather than
// throwing.
func (c *compiler) hoistModuleBindings(body []ast.Stmt) {
	var names []string
	// Module code is strict, so Annex B's block-function alias does not apply.
	collectVarNamesIn(body, &names, true)
	for _, s := range body {
		collectLexicalNames(s, &names)
	}
	for _, n := range names {
		c.emit(bytecode.OpDefineGlobalVar, c.nameIdx(n), 0)
	}
}

// collectLexicalNames gathers the let, const and class bindings a top-level
// statement introduces, including through an export wrapper.
func collectLexicalNames(s ast.Stmt, out *[]string) {
	switch n := s.(type) {
	case *ast.VarDecl:
		if n.Kind == ast.DeclVar {
			return
		}
		for _, d := range n.Decls {
			collectPatternNames(d.Target, out)
		}
	case *ast.ClassDecl:
		if n.Class.Name != nil {
			*out = append(*out, n.Class.Name.Name)
		}
	case *ast.ExportDecl:
		if n.Decl != nil {
			collectLexicalNames(n.Decl, out)
		}
	}
}
