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

// defaultBindingName is what `export default` binds when what it exports has
// no name of its own -- an anonymous function or class, or an expression. No
// identifier can spell it, so it cannot collide with a module's own bindings.
const defaultBindingName = "*default*"

// ModuleInfo is what the compiler learned about a module's shape, which the
// linker needs.
type ModuleInfo struct {
	// Imports lists every binding the module brings in.
	Imports []ImportRequest
	// Exports maps an exported name to the local name backing it.
	Exports map[string]string
	// StarExports lists the modules re-exported wholesale.
	StarExports []string
	// Init is the module's environment: its var and lexical bindings, and its
	// top-level function declarations. It runs when the module is linked
	// rather than when it is evaluated, which is what lets a module of a cycle
	// call a function of one that has not run yet.
	Init *bytecode.Function
	// Requests lists every module this one names, in the order it named them.
	//
	// It is separate from the lists above because those are about bindings,
	// and this is about dependencies: `export {} from "m"` binds nothing and
	// is still a dependency, and a star re-export is a dependency written
	// among the imports rather than after all of them. Loading and evaluation
	// follow this list, so they follow the source.
	Requests []string
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
	c.checkModuleDeclarations(prog.Body)
	c.checkScopes(prog.Body)
	c.collectModuleLex(prog.Body)
	// The declarations are compiled into a function of their own, which the
	// linker runs: a module's bindings exist, and its functions are callable,
	// before any of the graph has been evaluated.
	info.Init = compileModuleInit(prog, opts, &info)
	c.moduleBindingsDone = true
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
			c.module.Requests = append(c.module.Requests, n.Source)
			for _, spec := range n.Specifiers {
				if spec.Local == "arguments" || spec.Local == "eval" {
					// Neither may be bound, and an import binding is a binding
					// like any other.
					c.errorf(n.Start, "cannot import as %q", spec.Local)
				}
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
			if n.Source != "" {
				// Every `from "m"` is a dependency, whether or not it brings a
				// binding with it.
				c.module.Requests = append(c.module.Requests, n.Source)
			}
			c.collectExport(n)
		}
	}
}

// collectExport records the names one export declaration exposes.
func (c *compiler) collectExport(n *ast.ExportDecl) {
	switch {
	case n.All:
		if n.Alias != "" {
			// `export * as ns from "m"` exports the other module's namespace
			// under a name, and binds nothing locally. The import is what makes
			// the module a dependency; it lands under a name no identifier can
			// spell, and the export records which module it names rather than a
			// binding, so that two modules re-exporting the same namespace
			// under the same name agree rather than conflict.
			c.module.Imports = append(c.module.Imports, ImportRequest{
				Specifier: n.Source, Local: "*ns*" + n.Source, Namespace: true,
			})
			c.module.Exports[n.Alias] = "*ns*" + n.Source
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
		// A named declaration is exported under its own binding, so that the
		// export stays live: `export default function fn() {}` followed by
		// `fn = 2` is seen through the namespace.
		if names := declaredNames(n.Decl); len(names) > 0 {
			c.module.Exports["default"] = names[0]
			return
		}
		c.module.Exports["default"] = defaultBindingName

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
		if cd, ok := n.Decl.(*ast.ClassDecl); ok && cd.Class.Name == nil {
			// `export default class {}` declares no binding of its own, so it
			// is the expression it looks like, named after the export.
			c.compileClass(cd.Class, "default")
			c.emit(bytecode.OpInitModuleLex, c.nameIdx(defaultBindingName), 0)
			return
		}
		if fd, ok := n.Decl.(*ast.FuncDecl); ok {
			// A function declaration, named or not, was already emitted when
			// the statement list hoisted it.
			_ = fd
			return
		}
		if n.Decl != nil {
			// A named class: the export names its binding, which is all there
			// is to do.
			c.compileStatement(n.Decl)
			return
		}
		c.compileExprNamed(n.DefaultExpr, "default")
		c.emit(bytecode.OpInitModuleLex, c.nameIdx(defaultBindingName), 0)

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
// A lexical binding is created in its dead zone all the same, so reading one
// before its declaration is the ReferenceError it would be anywhere else. The
// property carries the marker a frame slot would carry as its value, since the
// linker needs the property to exist from the start.
func (c *compiler) hoistModuleBindings(body []ast.Stmt) {
	var names []string
	// Module code is strict, so Annex B's block-function alias does not apply.
	collectVarNamesIn(body, &names, true)
	for _, n := range names {
		c.emit(bytecode.OpDefineGlobalVar, c.nameIdx(n), 0)
	}

	c.collectModuleLex(body)
	for _, l := range moduleLexNames(body) {
		mutable := uint32(1)
		if l.kind == ast.DeclConst {
			mutable = 0
		}
		c.emit(bytecode.OpDeclareModuleLex, c.nameIdx(l.name), mutable)
	}
}

// collectModuleLex records which top-level names are lexical bindings, which
// both the declarations and the body need to know: a reference to one resolves
// to the module environment rather than to a global.
func (c *compiler) collectModuleLex(body []ast.Stmt) {
	lexical := moduleLexNames(body)
	c.moduleLex = make(map[string]bool, len(lexical))
	for _, l := range lexical {
		c.moduleLex[l.name] = true
	}
}

// moduleLexNames lists a module's top-level lexical bindings.
func moduleLexNames(body []ast.Stmt) []lexicalName {
	var lexical []lexicalName
	for _, s := range body {
		collectLexicalNames(s, &lexical)
	}
	return lexical
}

// compileModuleInit compiles what the linker runs: the module's bindings and
// its top-level function declarations.
//
// It is a function of its own rather than a prefix of the module's body because
// of when it runs. A module of a cycle may be asked for a function it declares
// before its body has run -- that is what makes a cycle work at all -- so the
// declarations belong to linking, which happens for the whole graph before any
// of it is evaluated.
func compileModuleInit(prog *ast.Program, opts Options, info *ModuleInfo) *bytecode.Function {
	c := newCompiler(nil, opts)
	c.fn.Name = "<module bindings>"
	c.fn.Strict = true
	c.fn.IsModule = true
	c.fn.Source = opts.Source
	c.lineOf = lineMapper(opts.Text)
	c.module = info
	c.completionSlot = int32(c.nextSlot)
	c.nextSlot++
	c.hoistModuleBindings(prog.Body)
	c.hoistBlockDeclarations(prog.Body)
	c.emit(bytecode.OpPushUndef, 0, 0)
	c.emit(bytecode.OpReturn, 0, 0)
	c.finish()
	return c.fn
}

// lexicalName is a top-level lexical binding and the kind of declaration it
// came from, which decides whether it can be assigned to.
type lexicalName struct {
	name string
	kind ast.DeclKind
}

// collectLexicalNames gathers the let, const and class bindings a top-level
// statement introduces, including through an export wrapper.
func collectLexicalNames(s ast.Stmt, out *[]lexicalName) {
	switch n := s.(type) {
	case *ast.VarDecl:
		if n.Kind == ast.DeclVar {
			return
		}
		var names []string
		for _, d := range n.Decls {
			collectPatternNames(d.Target, &names)
		}
		for _, name := range names {
			*out = append(*out, lexicalName{name, n.Kind})
		}
	case *ast.ClassDecl:
		if n.Class.Name != nil {
			// A class binding is mutable, like a let.
			*out = append(*out, lexicalName{n.Class.Name.Name, ast.DeclLet})
		}
	case *ast.ExportDecl:
		if !n.Default {
			if n.Decl != nil {
				collectLexicalNames(n.Decl, out)
			}
			return
		}
		switch d := n.Decl.(type) {
		case *ast.FuncDecl:
			// Hoisted, so it is never in a dead zone.
		case *ast.ClassDecl:
			if d.Class.Name != nil {
				collectLexicalNames(d, out)
				return
			}
			*out = append(*out, lexicalName{defaultBindingName, ast.DeclLet})
		default:
			// An exported expression. Its binding has no name a program can
			// write, but it has a dead zone all the same: the namespace can
			// reach it round a cycle before the statement has run.
			*out = append(*out, lexicalName{defaultBindingName, ast.DeclLet})
		}
	}
}

// checkModuleDeclarations reports the declaration errors that are particular to
// a module.
//
// A module's top level is a lexical scope in a way a script's is not: a
// function declaration there is a lexical binding rather than a var, and so is
// every imported name. Two lexical bindings of the same name collide, and so do
// a lexical one and a var.
//
// Its exports have rules of their own. A name may be exported once, and what an
// export names has to be something the module declares -- `export {nope}` is an
// error at compile time rather than an undefined at run time, which is the
// whole point of static module structure.
func (c *compiler) checkModuleDeclarations(body []ast.Stmt) {
	declared := make(map[string]bool)
	lexical := make(map[string]bool)

	declare := func(name string, lex bool, pos int) {
		if name == "" {
			return
		}
		// Two vars may name the same binding; anything involving a lexical one
		// may not.
		if declared[name] && (lex || lexical[name]) {
			c.errorf(pos, "identifier %q has already been declared", name)
		}
		declared[name] = true
		if lex {
			lexical[name] = true
		}
	}

	// A declaration's own names, with a var's hoisting already accounted for by
	// the caller.
	declareDecl := func(s ast.Stmt, pos int) {
		switch d := s.(type) {
		case *ast.VarDecl:
			var names []string
			for _, dd := range d.Decls {
				collectPatternNames(dd.Target, &names)
			}
			for _, n := range names {
				declare(n, d.Kind != ast.DeclVar, pos)
			}
		case *ast.FuncDecl:
			if d.Fn != nil && d.Fn.Name != nil {
				declare(d.Fn.Name.Name, true, pos)
			}
		case *ast.ClassDecl:
			if d.Class != nil && d.Class.Name != nil {
				declare(d.Class.Name.Name, true, pos)
			}
		}
	}

	for _, s := range body {
		switch n := s.(type) {
		case *ast.ImportDecl:
			c.module.Requests = append(c.module.Requests, n.Source)
			for _, spec := range n.Specifiers {
				declare(spec.Local, true, n.Start)
			}
		case *ast.ExportDecl:
			if n.Decl != nil {
				declareDecl(n.Decl, n.Start)
			}
		case *ast.VarDecl, *ast.FuncDecl, *ast.ClassDecl:
			declareDecl(s, s.Pos())
		default:
			// A var nested in a block or a loop hoists to the module's top
			// level, where it can collide with a lexical binding just the same.
			var names []string
			collectVarNamesIn([]ast.Stmt{s}, &names, true)
			for _, name := range names {
				declare(name, false, s.Pos())
			}
		}
	}

	exported := make(map[string]bool)
	exportName := func(name string, pos int) {
		if exported[name] {
			c.errorf(pos, "duplicate export %q", name)
		}
		exported[name] = true
	}

	for _, s := range body {
		n, ok := s.(*ast.ExportDecl)
		if !ok {
			continue
		}
		switch {
		case n.All:
			// `export * from "m"` names nothing statically; the linker decides
			// what it covers, and a collision there is not an early error.
			if n.Alias != "" {
				exportName(n.Alias, n.Start)
			}
		case len(n.Specifiers) > 0:
			for _, spec := range n.Specifiers {
				exportName(spec.Exported, spec.Start)
				// A re-export names a binding of the other module, which this
				// one knows nothing about.
				if n.Source == "" && !declared[spec.Local] {
					c.errorf(spec.Start, "%q is not declared in this module", spec.Local)
				}
			}
		case n.Default:
			exportName("default", n.Start)
		case n.Decl != nil:
			for _, name := range declaredNames(n.Decl) {
				exportName(name, n.Start)
			}
		}
	}
}
