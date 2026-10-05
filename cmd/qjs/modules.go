package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Node's module system, as qjs runs it: require and module for CommonJS,
// files found as node finds them -- by path, with the extensions and index
// files require tries, and in node_modules, through a package's "exports" and
// "imports" -- and the two kinds of module meeting as they do in node. An ES
// module that imports a CommonJS file gets a synthetic module whose names are
// read from the file's source, and which runs the file in its turn; require of
// an ES module evaluates it synchronously.
//
// A file is an ES module if it is named .mjs, or .js in a package whose
// package.json says "type": "module"; one named .cjs, or .js in a package
// that says "commonjs", is CommonJS; and a .js file with no type said is
// judged by its source, as node now judges it: one that uses import or export
// is a module.

// builtinModules are node's modules, by the names require knows them by, as
// node's module.builtinModules lists them. require reaches those qjs installs
// -- and the stand-ins it installs for those a flag withheld -- and refuses
// the rest.
var builtinModules = []string{
	"assert", "assert/strict", "async_hooks", "buffer", "child_process",
	"cluster", "console", "constants", "crypto", "dgram", "diagnostics_channel",
	"dns", "dns/promises", "domain", "events", "fs", "fs/promises", "http",
	"http2", "https", "inspector", "module", "net", "os", "path", "path/posix",
	"path/win32", "perf_hooks", "process", "punycode", "querystring",
	"readline", "readline/promises", "repl", "stream", "stream/consumers",
	"stream/promises", "stream/web", "string_decoder", "sys", "timers",
	"timers/promises", "tls", "trace_events", "tty", "url", "util",
	"util/types", "v8", "vm", "wasi", "worker_threads", "zlib",
}

// isBuiltin reports whether a request names one of node's modules: any name
// with the node: scheme, or one of builtinModules.
func isBuiltin(request string) bool {
	return strings.HasPrefix(request, "node:") || slices.Contains(builtinModules, request)
}

// The conditions a package's "exports" and "imports" are read with.
var (
	requireConditions = []string{"require", "node"}
	importConditions  = []string{"import", "node"}
)

// nodeModules is a runtime's module system.
type nodeModules struct {
	rt *quickjs.Runtime
	// api is what modulesJS returns: load, createRequire, runMain and the
	// rest, which keep the Module objects and require.cache.
	api quickjs.Value
}

// modulesOf finds a runtime's module system, for the worker it is the
// runtime of.
var modulesOf sync.Map // *quickjs.Runtime -> *nodeModules

// installModules gives a runtime node's module system: the module loader,
// import.meta, require's machinery and the module module.
func installModules(rt *quickjs.Runtime) (*nodeModules, error) {
	m := &nodeModules{rt: rt}
	boot, err := rt.Eval(modulesJS)
	if err != nil {
		return nil, err
	}
	m.api, err = boot.Call(map[string]any{
		"cwd":            cwd(),
		"sep":            string(filepath.Separator),
		"dirname":        filepath.Dir,
		"extname":        filepath.Ext,
		"join":           func(a, b string) string { return filepath.Join(a, b) },
		"resolve":        m.resolveRequire,
		"lookupPaths":    lookupPaths,
		"toPath":         m.toPath,
		"read":           readSource,
		"compile":        m.compile,
		"isESM":          isESM,
		"requireESM":     m.requireESM,
		"isBuiltin":      isBuiltin,
		"requireBuiltin": m.requireBuiltin,
		"builtinModules": builtinModules,
	})
	if err != nil {
		return nil, err
	}
	module, err := m.api.Get("Module")
	if err != nil {
		return nil, err
	}
	exports := map[string]quickjs.Value{"default": module}
	for _, name := range []string{"createRequire", "builtinModules", "isBuiltin"} {
		if exports[name], err = module.Get(name); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"module", "node:module"} {
		if err := rt.SetModuleValues(name, exports); err != nil {
			return nil, err
		}
	}
	rt.SetModuleLoader(m.loadESM)
	// What a module is told about itself: where it came from, in the forms
	// node offers, so that the usual ways of finding a file beside a module
	// work.
	rt.OnImportMeta(func(specifier string, meta quickjs.Value) {
		if !filepath.IsAbs(specifier) {
			meta.Set("url", specifier)
			return
		}
		meta.Set("url", fileURL(specifier))
		meta.Set("filename", specifier)
		meta.Set("dirname", filepath.Dir(specifier))
		meta.Set("resolve", func(request string) (string, error) {
			if isBuiltin(request) {
				return request, nil
			}
			f, err := m.resolve(request, filepath.Dir(specifier), importConditions, false)
			if err != nil {
				return "", err
			}
			return fileURL(f), nil
		})
	})
	modulesOf.Store(rt, m)
	rt.OnClose(func() { modulesOf.Delete(rt) })
	return m, nil
}

// runMain runs a CommonJS file as node runs its entry point.
func (m *nodeModules) runMain(ctx context.Context, filename string) error {
	runMain, err := m.api.Get("runMain")
	if err != nil {
		return err
	}
	_, err = runMain.CallContext(ctx, filename)
	return err
}

// evalGlobals gives code that is not a file -- -e, stdin, the REPL, an eval
// worker -- the require and module node gives it: resolving from the working
// directory, under a name of its own.
func (m *nodeModules) evalGlobals(name string) error {
	evalGlobals, err := m.api.Get("evalGlobals")
	if err != nil {
		return err
	}
	_, err = evalGlobals.Call(name)
	return err
}

// codedError throws an Error with a code, as node's loader throws its own.
func (m *nodeModules) codedError(code, format string, args ...any) error {
	e := m.rt.NewError("Error", fmt.Sprintf(format, args...))
	e.Set("code", code)
	return m.rt.Throw(e)
}

// resolveRequire is require.resolve: a file, as require finds one from a
// directory.
func (m *nodeModules) resolveRequire(request, fromDir string) (string, error) {
	return m.resolve(request, fromDir, requireConditions, true)
}

// resolve is node's resolution of a request from a directory: for require
// (cjs), which tries extensions and index files, or for import, which takes a
// path as it is written. It returns the file's real path.
func (m *nodeModules) resolve(request, fromDir string, conditions []string, cjs bool) (string, error) {
	switch {
	case strings.HasPrefix(request, "#"):
		return m.resolveImports(request, fromDir, conditions, cjs)
	case isPathRequest(request):
		p, err := m.requestPath(request, fromDir)
		if err != nil {
			return "", err
		}
		if cjs {
			if f, ok := probe(p); ok {
				return realPath(f), nil
			}
			return "", m.codedError("MODULE_NOT_FOUND", "Cannot find module '%s'", request)
		}
		return m.exactFile(p, request, fromDir)
	}
	return m.resolvePackage(request, fromDir, conditions, cjs)
}

// isPathRequest reports whether a request names a path rather than a
// package: relative, absolute or a file URL.
func isPathRequest(request string) bool {
	return request == "." || request == ".." || strings.HasPrefix(request, "./") ||
		strings.HasPrefix(request, "../") || strings.HasPrefix(request, "/") ||
		strings.HasPrefix(request, `.\`) || strings.HasPrefix(request, `..\`) ||
		strings.HasPrefix(request, "file:") || filepath.IsAbs(request)
}

// requestPath is the absolute path a path request names from a directory.
func (m *nodeModules) requestPath(request, fromDir string) (string, error) {
	if strings.HasPrefix(request, "file:") {
		p, err := fileURLPath(request, filepath.Separator == '\\')
		if err != nil {
			return "", m.codedError("ERR_INVALID_URL", "%s", err.Error())
		}
		return filepath.Abs(p)
	}
	if strings.HasPrefix(request, "/") || filepath.IsAbs(request) {
		return filepath.Abs(request)
	}
	return filepath.Join(fromDir, filepath.FromSlash(request)), nil
}

// exactFile is a file import names exactly: an ES module's import tries no
// extension and no index file.
func (m *nodeModules) exactFile(p, request, fromDir string) (string, error) {
	isFile, isDir := stat(p)
	switch {
	case isFile:
		return realPath(p), nil
	case isDir:
		return "", m.codedError("ERR_UNSUPPORTED_DIR_IMPORT",
			"Directory import '%s' is not supported resolving ES modules imported from %s", p, fromDir)
	}
	return "", m.codedError("ERR_MODULE_NOT_FOUND", "Cannot find module '%s' imported from %s", p, fromDir)
}

// probe finds a file as require does: the path, with .js or .json, or as a
// directory -- its package.json's "main", or its index file.
func probe(p string) (string, bool) {
	for _, f := range []string{p, p + ".js", p + ".json"} {
		if isFile, _ := stat(f); isFile {
			return f, true
		}
	}
	if _, isDir := stat(p); !isDir {
		return "", false
	}
	if pkg := readPackage(p); pkg != nil && pkg.Main != "" {
		main := filepath.Join(p, filepath.FromSlash(pkg.Main))
		for _, f := range []string{main, main + ".js", main + ".json"} {
			if isFile, _ := stat(f); isFile {
				return f, true
			}
		}
		if f, ok := indexFile(main); ok {
			return f, true
		}
	}
	return indexFile(p)
}

func indexFile(dir string) (string, bool) {
	for _, name := range []string{"index.js", "index.json"} {
		f := filepath.Join(dir, name)
		if isFile, _ := stat(f); isFile {
			return f, true
		}
	}
	return "", false
}

// resolvePackage finds a package's module: the package itself, when the
// request names the package the directory is in and it has "exports", or
// one in node_modules, from the directory up.
func (m *nodeModules) resolvePackage(request, fromDir string, conditions []string, cjs bool) (string, error) {
	name, sub, ok := splitPackage(request)
	if !ok {
		return "", m.codedError("ERR_INVALID_MODULE_SPECIFIER", "Invalid module %q", request)
	}
	if scope := packageScope(fromDir); scope != nil && scope.Name == name && scope.exports != nil {
		return m.packageExports(scope, sub, conditions, request)
	}
	for dir := fromDir; ; {
		if filepath.Base(dir) != "node_modules" {
			pkgDir := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
			if _, isDir := stat(pkgDir); isDir {
				pkg := readPackage(pkgDir)
				if pkg != nil && pkg.exports != nil {
					return m.packageExports(pkg, sub, conditions, request)
				}
				p := filepath.Join(pkgDir, filepath.FromSlash(sub))
				if f, ok := probe(p); ok && (cjs || sub == ".") {
					return realPath(f), nil
				}
				if !cjs && sub != "." {
					return m.exactFile(p, request, fromDir)
				}
				break
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if cjs {
		return "", m.codedError("MODULE_NOT_FOUND", "Cannot find module '%s'", request)
	}
	return "", m.codedError("ERR_MODULE_NOT_FOUND", "Cannot find package '%s' imported from %s", name, fromDir)
}

// splitPackage is a request's package name and the subpath in it: "a/b" is
// a and ./b, "@s/a/b" @s/a and ./b.
func splitPackage(request string) (name, sub string, ok bool) {
	parts := strings.SplitN(request, "/", 3)
	switch {
	case request == "" || strings.HasPrefix(request, ".") || strings.ContainsAny(request, `\%`):
		return "", "", false
	case strings.HasPrefix(request, "@"):
		if len(parts) < 2 || parts[1] == "" {
			return "", "", false
		}
		name = parts[0] + "/" + parts[1]
	default:
		name = parts[0]
	}
	sub = "." + strings.TrimPrefix(request, name)
	return name, sub, true
}

// packageExports is a subpath of a package, through its "exports": the
// target the first condition it is read with gives, which has to be a file
// in the package.
func (m *nodeModules) packageExports(pkg *packageJSON, sub string, conditions []string, request string) (string, error) {
	target, found, err := matchSubpath(pkg.exports, sub, ".", conditions)
	if err != nil {
		return "", m.codedError("ERR_INVALID_PACKAGE_TARGET",
			"Invalid \"exports\" target for '%s' in %s: %s", sub, pkg.file(), err.Error())
	}
	if !found {
		if sub == "." {
			return "", m.codedError("ERR_PACKAGE_PATH_NOT_EXPORTED",
				"No \"exports\" main defined in %s", pkg.file())
		}
		return "", m.codedError("ERR_PACKAGE_PATH_NOT_EXPORTED",
			"Package subpath '%s' is not defined by \"exports\" in %s", sub, pkg.file())
	}
	f := filepath.Join(pkg.dir, filepath.FromSlash(target))
	if isFile, _ := stat(f); !isFile {
		return "", m.codedError("MODULE_NOT_FOUND", "Cannot find module '%s'", f)
	}
	return realPath(f), nil
}

// resolveImports is a #name, through the "imports" of the package the
// directory is in: a file of the package, or another package's module.
func (m *nodeModules) resolveImports(request, fromDir string, conditions []string, cjs bool) (string, error) {
	pkg := packageScope(fromDir)
	if pkg == nil || pkg.imports == nil {
		return "", m.codedError("ERR_PACKAGE_IMPORT_NOT_DEFINED",
			"Package import specifier %q is not defined imported from %s", request, fromDir)
	}
	target, found, err := matchSubpath(pkg.imports, request, "#", conditions)
	if err != nil || !found {
		return "", m.codedError("ERR_PACKAGE_IMPORT_NOT_DEFINED",
			"Package import specifier %q is not defined in package %s imported from %s", request, pkg.file(), fromDir)
	}
	if !strings.HasPrefix(target, "./") {
		// Another package's module.
		return m.resolvePackage(target, pkg.dir, conditions, cjs)
	}
	f := filepath.Join(pkg.dir, filepath.FromSlash(target))
	if isFile, _ := stat(f); !isFile {
		return "", m.codedError("MODULE_NOT_FOUND", "Cannot find module '%s'", f)
	}
	return realPath(f), nil
}

// matchSubpath finds a subpath in an "exports" or "imports" map -- exactly,
// or by the pattern with a "*" that matches it with the longest prefix -- and
// resolves the target it maps to with the conditions. prefix is "." for
// exports, whose map may be a bare target for "." alone, or "#" for imports.
func matchSubpath(mapping *jsonValue, sub, prefix string, conditions []string) (string, bool, error) {
	if prefix == "." && !(mapping.kind == 'o' && len(mapping.keys) > 0 && strings.HasPrefix(mapping.keys[0], ".")) {
		// Sugar: a target, or conditions, for "." alone.
		if sub != "." {
			return "", false, nil
		}
		return resolveTarget(mapping, "", conditions, prefix == "#")
	}
	if mapping.kind != 'o' {
		return "", false, nil
	}
	if v := mapping.get(sub); v != nil && !strings.Contains(sub, "*") {
		return resolveTarget(v, "", conditions, prefix == "#")
	}
	best, bestStar := -1, ""
	for i, key := range mapping.keys {
		before, after, ok := strings.Cut(key, "*")
		if !ok || strings.Contains(after, "*") || !strings.HasPrefix(sub, before) ||
			!strings.HasSuffix(sub, after) || len(sub) < len(key) {
			continue
		}
		if best < 0 || patternBefore(key, mapping.keys[best]) {
			best, bestStar = i, sub[len(before):len(sub)-len(after)]
		}
	}
	if best < 0 {
		return "", false, nil
	}
	return resolveTarget(mapping.vals[best], bestStar, conditions, prefix == "#")
}

// patternBefore orders pattern keys as node does: the longer prefix before
// the "*" first, and then the longer key.
func patternBefore(a, b string) bool {
	ai, bi := strings.Index(a, "*"), strings.Index(b, "*")
	if ai != bi {
		return ai > bi
	}
	return len(a) > len(b)
}

// resolveTarget is an "exports" or "imports" target with the conditions: a
// path in the package, with the pattern's match put for its "*"; the first
// of an array that resolves; or the value of the first condition the object
// lists that is one of conditions, or "default", and resolves. null excludes
// the subpath. An imports target may be another package's name.
func resolveTarget(v *jsonValue, star string, conditions []string, imports bool) (string, bool, error) {
	switch v.kind {
	case 's':
		t := v.str
		if !strings.HasPrefix(t, "./") {
			if imports && !strings.HasPrefix(t, "../") && !strings.HasPrefix(t, "/") {
				return strings.ReplaceAll(t, "*", star), true, nil
			}
			return "", false, fmt.Errorf("%q does not start with \"./\"", t)
		}
		t = strings.ReplaceAll(t, "*", star)
		for _, seg := range strings.Split(t, "/")[1:] {
			if seg == "." || seg == ".." || seg == "node_modules" {
				return "", false, fmt.Errorf("%q leaves the package", t)
			}
		}
		return t, true, nil
	case 'a':
		var firstErr error
		for _, item := range v.items {
			t, ok, err := resolveTarget(item, star, conditions, imports)
			if ok {
				return t, true, nil
			}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return "", false, firstErr
	case 'o':
		for i, key := range v.keys {
			if key != "default" && !slices.Contains(conditions, key) {
				continue
			}
			t, ok, err := resolveTarget(v.vals[i], star, conditions, imports)
			if ok || err != nil {
				return t, ok, err
			}
			if v.vals[i].kind == 'n' {
				return "", false, nil
			}
		}
	}
	return "", false, nil
}

// packageJSON is what a package.json says that loading needs.
type packageJSON struct {
	dir              string
	Name, Main, Type string
	exports, imports *jsonValue
}

func (p *packageJSON) file() string { return filepath.Join(p.dir, "package.json") }

// The package.json files read, by directory -- nil where there is none --
// which every runtime of the process shares, as node's cache is the
// process's.
var (
	packagesMu sync.Mutex
	packages   = map[string]*packageJSON{}
)

// readPackage is a directory's package.json, or nil.
func readPackage(dir string) *packageJSON {
	packagesMu.Lock()
	defer packagesMu.Unlock()
	if p, ok := packages[dir]; ok {
		return p
	}
	var p *packageJSON
	if src, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		p = parsePackage(dir, src)
	}
	packages[dir] = p
	return p
}

func parsePackage(dir string, src []byte) *packageJSON {
	src = bytes.TrimPrefix(src, []byte("\ufeff"))
	root, err := parseJSON(src)
	if err != nil || root.kind != 'o' {
		return nil
	}
	p := &packageJSON{dir: dir}
	str := func(key string) string {
		if v := root.get(key); v != nil && v.kind == 's' {
			return v.str
		}
		return ""
	}
	p.Name, p.Main, p.Type = str("name"), str("main"), str("type")
	if v := root.get("exports"); v != nil && v.kind != 'n' {
		p.exports = v
	}
	if v := root.get("imports"); v != nil && v.kind == 'o' {
		p.imports = v
	}
	return p
}

// packageScope is the package a directory is in: the nearest package.json
// from it up, short of a node_modules directory.
func packageScope(dir string) *packageJSON {
	for {
		if filepath.Base(dir) == "node_modules" {
			return nil
		}
		if p := readPackage(dir); p != nil {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// lookupPaths are the node_modules directories require looks in from a
// directory, nearest first: module.paths.
func lookupPaths(dir string) []string {
	var out []string
	for {
		if filepath.Base(dir) != "node_modules" {
			out = append(out, filepath.Join(dir, "node_modules"))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return out
		}
		dir = parent
	}
}

// isESM says whether a file is an ES module: named .mjs; named .js in a
// package of type module, or of no type with source that uses import or
// export.
func isESM(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".mjs":
		return true
	case ".js":
		if pkg := packageScope(filepath.Dir(filename)); pkg != nil && pkg.Type != "" {
			return pkg.Type == "module"
		}
		src, err := readSource(filename)
		return err == nil && looksLikeModule(src)
	}
	return false
}

// readSource is a file's text, without a byte order mark.
func readSource(filename string) (string, error) {
	b, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(b), "\ufeff"), nil
}

// compile compiles a CommonJS file as node does, in a function of the five
// names a module has; the wrapper's line is placed above the file's first,
// so that stack traces and syntax errors give the file's own positions.
func (m *nodeModules) compile(filename string) (quickjs.Value, error) {
	src, err := readSource(filename)
	if err != nil {
		return quickjs.Value{}, err
	}
	if strings.HasPrefix(src, "#!") {
		src = "//" + src[2:]
	}
	p, err := m.rt.Compile(filename,
		"(function (exports, require, module, __filename, __dirname) {\n"+src+"\n})",
		quickjs.WithOffset(-1, 0))
	if err != nil {
		return quickjs.Value{}, err
	}
	return m.rt.RunProgram(p)
}

// requireESM is require of an ES module: its namespace, from the module an
// import of it is, evaluated synchronously.
func (m *nodeModules) requireESM(filename string) (quickjs.Value, error) {
	ns, err := m.rt.RequireModule(filename, "")
	switch {
	case errors.Is(err, quickjs.ErrModuleAwaits):
		return ns, m.codedError("ERR_REQUIRE_ASYNC_MODULE",
			"require() cannot be used on an ESM graph with top-level await. Use import() instead. To see where the top-level await comes from, use --experimental-print-required-tla.\n  From %s", filename)
	case errors.Is(err, quickjs.ErrModuleEvaluating):
		return ns, m.codedError("ERR_REQUIRE_CYCLE_MODULE",
			"Cannot require() ES Module %s in a cycle.", filename)
	}
	return ns, err
}

// requireBuiltin is require of one of node's modules: the module qjs
// installed under that name, as node's require gives it -- the object a
// default import gives.
func (m *nodeModules) requireBuiltin(name string) (quickjs.Value, error) {
	ns, err := m.rt.RequireModule(name, "")
	if err != nil {
		return quickjs.Value{}, err
	}
	if d, err := ns.Get("default"); err == nil && !d.IsUndefined() {
		return d, nil
	}
	return ns, nil
}

// toPath is the file createRequire is given: a path or a file URL, absolute.
func (m *nodeModules) toPath(from string) (string, error) {
	p := from
	if strings.HasPrefix(from, "file:") {
		var err error
		if p, err = fileURLPath(from, filepath.Separator == '\\'); err != nil {
			return "", m.codedError("ERR_INVALID_ARG_VALUE", "%s", err.Error())
		}
	}
	if !filepath.IsAbs(p) && !strings.HasPrefix(p, "/") {
		return "", m.codedError("ERR_INVALID_ARG_VALUE",
			"The argument 'filename' must be a file URL object, file URL string, or absolute path string. Received %q", from)
	}
	return filepath.Abs(p)
}

// loadESM is the module loader. One of node's modules not installed is
// refused; an ES module, and a JSON file a typed import asks for, is its
// source; and a CommonJS file is a synthetic module, whose default export --
// and "module.exports" -- is module.exports and whose named exports are the
// names its source exports, run by requiring it in its turn.
func (m *nodeModules) loadESM(specifier, referrer string) (string, string, error) {
	if isBuiltin(specifier) {
		// An installed one never reaches the loader.
		return "", "", m.codedError("ERR_UNKNOWN_BUILTIN_MODULE", "No such built-in module: %s", specifier)
	}
	from := cwd()
	if filepath.IsAbs(referrer) {
		from = filepath.Dir(referrer)
	}
	filename, err := m.resolve(specifier, from, importConditions, false)
	if err != nil {
		return "", "", err
	}
	if isESM(filename) || strings.EqualFold(filepath.Ext(filename), ".json") {
		src, err := readSource(filename)
		return src, filename, err
	}
	names, err := m.commonJSNames(filename, map[string]bool{})
	if err != nil {
		return "", "", err
	}
	load, err := m.api.Get("load")
	if err != nil {
		return "", "", err
	}
	exportNames := append([]string{"default", "module.exports"}, names...)
	err = m.rt.DefineSyntheticModule(filename, exportNames, func() (map[string]any, error) {
		exports, err := load.Call(filename, nil, false)
		if err != nil {
			return nil, err
		}
		values := map[string]any{"default": exports, "module.exports": exports}
		if exports.IsObject() {
			for _, name := range names {
				if values[name], err = exports.Get(name); err != nil {
					return nil, err
				}
			}
		}
		return values, nil
	})
	return "", filename, err
}

// commonJSNames is what a CommonJS file exports by name, as node finds it
// without running the file: what its source exports, and what the CommonJS
// files it hands on whole export, but default.
func (m *nodeModules) commonJSNames(filename string, seen map[string]bool) ([]string, error) {
	if seen[filename] {
		return nil, nil
	}
	seen[filename] = true
	if strings.EqualFold(filepath.Ext(filename), ".json") {
		return nil, nil
	}
	src, err := readSource(filename)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(src, "#!") {
		src = "//" + src[2:]
	}
	names, reexports, err := quickjs.CommonJSExports(filename, src)
	if err != nil {
		return nil, err
	}
	for _, request := range reexports {
		if isBuiltin(request) {
			continue
		}
		f, err := m.resolve(request, filepath.Dir(filename), requireConditions, true)
		if err != nil || isESM(f) {
			continue
		}
		more, err := m.commonJSNames(f, seen)
		if err != nil {
			return nil, err
		}
		names = append(names, more...)
	}
	out := names[:0]
	for _, name := range names {
		if name != "default" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out, nil
}

// stat reports whether a path is a file or a directory.
func stat(p string) (isFile, isDir bool) {
	st, err := os.Stat(p)
	if err != nil {
		return false, false
	}
	return st.Mode().IsRegular(), st.IsDir()
}

// realPath is a path with its symbolic links followed, as node identifies a
// module by: a package reached through two links is one module.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		if abs, err := filepath.Abs(r); err == nil {
			return abs
		}
	}
	return p
}

// jsonValue is a JSON value that keeps an object's keys in their order, which
// is what decides a package's conditions.
type jsonValue struct {
	kind  byte // 's'tring, 'o'bject, 'a'rray, 'n'ull, or 'x' for the rest
	str   string
	keys  []string
	vals  []*jsonValue
	items []*jsonValue
}

func (v *jsonValue) get(key string) *jsonValue {
	if v == nil || v.kind != 'o' {
		return nil
	}
	for i, k := range v.keys {
		if k == key {
			return v.vals[i]
		}
	}
	return nil
}

func parseJSON(src []byte) (*jsonValue, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	v, err := decodeJSON(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

func decodeJSON(dec *json.Decoder) (*jsonValue, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v := &jsonValue{kind: 'o'}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				val, err := decodeJSON(dec)
				if err != nil {
					return nil, err
				}
				v.keys = append(v.keys, k.(string))
				v.vals = append(v.vals, val)
			}
			_, err := dec.Token()
			return v, err
		case '[':
			v := &jsonValue{kind: 'a'}
			for dec.More() {
				item, err := decodeJSON(dec)
				if err != nil {
					return nil, err
				}
				v.items = append(v.items, item)
			}
			_, err := dec.Token()
			return v, err
		}
	case string:
		return &jsonValue{kind: 's', str: t}, nil
	case nil:
		return &jsonValue{kind: 'n'}, nil
	}
	return &jsonValue{kind: 'x'}, nil
}
