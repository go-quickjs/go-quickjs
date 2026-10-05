package quickjs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"testing"
	"testing/fstest"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A host's CommonJS require, built on the public API alone, as an embedder
// would build one: Go resolves and reads files and compiles each in the
// module wrapper; a little JavaScript keeps the Module objects, the cache and
// the cycles, as node's own loader does. ES modules import CommonJS through
// a module the loader writes for each, and CommonJS requires an ES module
// through EvalModule.

type cjsHost struct {
	rt    *quickjs.Runtime
	files fstest.MapFS
	api   quickjs.Value // what cjsBootstrap returns
}

const cjsBootstrap = `(host) => {
	const cache = Object.create(null);
	const dirname = f => f.slice(0, f.lastIndexOf("/")) || "/";
	class Module {
		constructor(id, parent) {
			this.id = id; this.filename = id; this.path = dirname(id);
			this.exports = {}; this.loaded = false; this.parent = parent; this.children = [];
		}
	}
	let main;
	function makeRequire(mod) {
		const require = request => load(request, mod);
		require.resolve = request => host.resolve(String(request), mod.path, "require");
		require.cache = cache;
		Object.defineProperty(require, "main", { get: () => main, enumerable: true });
		return require;
	}
	function load(request, parent, isMain) {
		if (typeof request !== "string") {
			const e = new TypeError('The "id" argument must be of type string. Received ' + typeof request);
			e.code = "ERR_INVALID_ARG_TYPE";
			throw e;
		}
		const filename = host.resolve(request, parent ? parent.path : host.cwd, "require");
		const cached = cache[filename];
		if (cached !== undefined) {
			if (parent && !parent.children.includes(cached)) parent.children.push(cached);
			return cached.exports;
		}
		const mod = new Module(filename, parent);
		if (isMain) main = mod;
		if (parent) parent.children.push(mod);
		cache[filename] = mod;
		let ok = false;
		try {
			if (filename.endsWith(".json")) {
				try { mod.exports = JSON.parse(host.read(filename)) }
				catch (e) { e.message = filename + ": " + e.message; throw e }
			} else if (host.isESM(filename)) {
				mod.exports = host.requireESM(filename);
			} else {
				host.compile(filename).call(mod.exports, mod.exports, makeRequire(mod), mod, filename, mod.path);
			}
			ok = true;
		} finally {
			if (!ok) {
				delete cache[filename];
				if (parent) parent.children.splice(parent.children.indexOf(mod), 1);
			}
		}
		mod.loaded = true;
		return mod.exports;
	}
	const createRequire = from => {
		from = String(from).replace(/^file:\/\//, "");
		return makeRequire(new Module(from, null));
	};
	return { load, createRequire, cache };
}`

func newCJSHost(t *testing.T, files fstest.MapFS) *cjsHost {
	t.Helper()
	h := &cjsHost{rt: quickjs.New(), files: files}
	t.Cleanup(func() { h.rt.Close() })
	boot, err := h.rt.Eval(cjsBootstrap)
	if err != nil {
		t.Fatal(err)
	}
	h.api, err = boot.Call(map[string]any{
		"cwd":        "/app",
		"resolve":    h.resolve,
		"read":       h.read,
		"compile":    h.compile,
		"isESM":      h.isESM,
		"requireESM": h.requireESM,
	})
	if err != nil {
		t.Fatal(err)
	}
	load, _ := h.api.Get("load")
	createRequire, _ := h.api.Get("createRequire")
	// The modules the loader writes reach the CommonJS loader through a
	// module of the host's, so that nothing of it is a global.
	if err := h.rt.SetModule("cjs-host", map[string]any{"load": load}); err != nil {
		t.Fatal(err)
	}
	if err := h.rt.SetModule("node:module", map[string]any{"createRequire": createRequire}); err != nil {
		t.Fatal(err)
	}
	h.rt.OnImportMeta(func(specifier string, meta quickjs.Value) {
		meta.Set("url", "file://"+specifier)
		meta.Set("filename", specifier)
		meta.Set("dirname", path.Dir(specifier))
	})
	h.rt.SetModuleLoader(h.loadESM)
	return h
}

// runMain requires a file as node runs its entry point.
func (h *cjsHost) runMain(filename string) (quickjs.Value, error) {
	load, _ := h.api.Get("load")
	return load.Call(filename, nil, true)
}

// codedError throws an Error with a code, as node's loader throws its errors.
func (h *cjsHost) codedError(code, format string, args ...any) error {
	e := h.rt.NewError("Error", fmt.Sprintf(format, args...))
	e.Set("code", code)
	return h.rt.Throw(e)
}

func (h *cjsHost) stat(p string) (isFile, isDir bool) {
	st, err := fs.Stat(h.files, strings.TrimPrefix(p, "/"))
	if err != nil {
		return false, false
	}
	return !st.IsDir(), st.IsDir()
}

func (h *cjsHost) read(filename string) (string, error) {
	b, err := fs.ReadFile(h.files, strings.TrimPrefix(filename, "/"))
	return string(b), err
}

type packageJSON struct {
	Main    string          `json:"main"`
	Type    string          `json:"type"`
	Exports json.RawMessage `json:"exports"`
}

func (h *cjsHost) packageJSON(dir string) *packageJSON {
	src, err := h.read(path.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var p packageJSON
	if json.Unmarshal([]byte(src), &p) != nil {
		return nil
	}
	return &p
}

// resolve is node's resolution, for require ("require") or import
// ("import"): a path, tried as a file, with an extension, and as a
// directory; or a package found in node_modules, through its "exports"
// where it has them.
func (h *cjsHost) resolve(request, fromDir, kind string) (string, error) {
	if request == "." || request == ".." || strings.HasPrefix(request, "./") ||
		strings.HasPrefix(request, "../") || strings.HasPrefix(request, "/") {
		p := request
		if !strings.HasPrefix(p, "/") {
			p = path.Join(fromDir, request)
		}
		if f, ok := h.resolvePath(p); ok {
			return f, nil
		}
		return "", h.codedError("MODULE_NOT_FOUND", "Cannot find module '%s'", request)
	}
	name, sub := request, "."
	parts := strings.SplitN(request, "/", 3)
	if strings.HasPrefix(request, "@") && len(parts) > 2 {
		name, sub = parts[0]+"/"+parts[1], "./"+parts[2]
	} else if !strings.HasPrefix(request, "@") && len(parts) > 1 {
		name, sub = parts[0], "./"+strings.Join(parts[1:], "/")
	}
	for dir := fromDir; ; dir = path.Dir(dir) {
		pkgDir := path.Join(dir, "node_modules", name)
		if _, isDir := h.stat(pkgDir); isDir {
			return h.resolvePackage(pkgDir, sub, kind, request)
		}
		if dir == "/" {
			break
		}
	}
	return "", h.codedError("MODULE_NOT_FOUND", "Cannot find module '%s'", request)
}

func (h *cjsHost) resolvePath(p string) (string, bool) {
	for _, f := range []string{p, p + ".js", p + ".json", p + ".cjs", p + ".mjs"} {
		if isFile, _ := h.stat(f); isFile {
			return f, true
		}
	}
	if _, isDir := h.stat(p); isDir {
		if pkg := h.packageJSON(p); pkg != nil && pkg.Main != "" {
			if f, ok := h.resolvePath(path.Join(p, pkg.Main)); ok {
				return f, true
			}
		}
		for _, f := range []string{"index.js", "index.json", "index.cjs"} {
			if isFile, _ := h.stat(path.Join(p, f)); isFile {
				return path.Join(p, f), true
			}
		}
	}
	return "", false
}

func (h *cjsHost) resolvePackage(pkgDir, sub, kind, request string) (string, error) {
	pkg := h.packageJSON(pkgDir)
	if pkg == nil || len(pkg.Exports) == 0 {
		if f, ok := h.resolvePath(path.Join(pkgDir, sub)); ok {
			return f, nil
		}
		return "", h.codedError("MODULE_NOT_FOUND", "Cannot find module '%s'", request)
	}
	var exports any
	json.Unmarshal(pkg.Exports, &exports)
	// exports is a target for "." alone, or a map of subpaths.
	if m, ok := exports.(map[string]any); ok {
		subpaths := false
		for k := range m {
			subpaths = subpaths || strings.HasPrefix(k, ".")
		}
		if subpaths {
			exports = m[sub]
		} else if sub != "." {
			exports = nil
		}
	} else if sub != "." {
		exports = nil
	}
	// A condition is taken in node's order of preference here; node takes the
	// first the object lists, which a decoder into a Go map does not keep.
	for exports != nil {
		if target, ok := exports.(string); ok {
			f := path.Join(pkgDir, target)
			if isFile, _ := h.stat(f); isFile {
				return f, nil
			}
			return "", h.codedError("MODULE_NOT_FOUND", "Cannot find module '%s'", f)
		}
		conds, _ := exports.(map[string]any)
		exports = nil
		for _, c := range []string{kind, "node", "default"} {
			if x, ok := conds[c]; ok {
				exports = x
				break
			}
		}
	}
	return "", h.codedError("ERR_PACKAGE_PATH_NOT_EXPORTED",
		"Package subpath '%s' is not defined by \"exports\" in %s/package.json", sub, pkgDir)
}

// isESM says whether a file is an ES module: a .mjs file, or a .js file
// whose nearest package.json says "type": "module".
func (h *cjsHost) isESM(filename string) bool {
	switch path.Ext(filename) {
	case ".mjs":
		return true
	case ".js":
		for dir := path.Dir(filename); ; dir = path.Dir(dir) {
			if pkg := h.packageJSON(dir); pkg != nil {
				return pkg.Type == "module"
			}
			if dir == "/" {
				return false
			}
		}
	}
	return false
}

// compile compiles a CommonJS file as node does, in a function of the five
// names a module has; the wrapper's line is placed above the file's first, so
// a stack trace gives the file's own lines and columns.
func (h *cjsHost) compile(filename string) (quickjs.Value, error) {
	src, err := h.read(filename)
	if err != nil {
		return quickjs.Value{}, err
	}
	if strings.HasPrefix(src, "#!") {
		src = "//" + src[2:]
	}
	p, err := h.rt.Compile(filename,
		"(function (exports, require, module, __filename, __dirname) {\n"+src+"\n})",
		quickjs.WithOffset(-1, 0))
	if err != nil {
		return quickjs.Value{}, err
	}
	return h.rt.RunProgram(p)
}

// requireESM is require of an ES module: its namespace.
func (h *cjsHost) requireESM(filename string) (quickjs.Value, error) {
	src, err := h.read(filename)
	if err != nil {
		return quickjs.Value{}, err
	}
	return h.rt.EvalModule(filename, src)
}

// loadESM is the module loader. An ES module is its source; a JSON file, for
// an import with type "json", is its text; and a CommonJS file is a module
// written for it, whose default export is module.exports and whose named
// exports are its properties.
func (h *cjsHost) loadESM(specifier, referrer string) (string, string, error) {
	from := "/app"
	if referrer != "" {
		from = path.Dir(referrer)
	}
	filename, err := h.resolve(specifier, from, "import")
	if err != nil {
		return "", "", err
	}
	if h.isESM(filename) || strings.HasSuffix(filename, ".json") {
		src, err := h.read(filename)
		return src, filename, err
	}
	// The named exports have to be known before the module is linked, which
	// node finds by reading the source; this runs the module instead.
	load, _ := h.api.Get("load")
	exports, err := load.Call(filename, nil, false)
	if err != nil {
		return "", "", err
	}
	var b strings.Builder
	quoted, _ := json.Marshal(filename)
	fmt.Fprintf(&b, "import {load} from \"cjs-host\";\nconst m = load(%s);\nexport default m;\n", quoted)
	if exports.IsObject() {
		for _, k := range exports.Keys() {
			if k != "default" && isIdentifier(k) {
				fmt.Fprintf(&b, "export const %s = m.%s;\n", k, k)
			}
		}
	}
	return b.String(), filename, nil
}

func isIdentifier(s string) bool {
	for i, r := range s {
		if !(r == '_' || r == '$' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return s != ""
}

var cjsFiles = fstest.MapFS{
	"app/package.json": {Data: []byte(`{"name": "app"}`)},
	"app/main.js": {Data: []byte(`const out = [];
const lib = require("./lib");
out.push("lib " + lib.name + " " + (this === module.exports) + " " + (require.main === module));
out.push("names " + __filename + " " + __dirname + " " + module.id);
out.push("json " + require("./data.json").x);
const a = require("./cycle/a");
out.push("cycle " + a.done + " " + a.sawB);
out.push("pkg " + require("pkg").hello() + ", " + require("pkg/feature").f);
try { require("pkg/private") } catch (e) { out.push(e.code) }
try { require("./nope") } catch (e) { out.push(e.code + " " + e.message) }
out.push("resolve " + require.resolve("pkg") + " " + require.resolve("./lib"));
out.push("cached " + (require("./lib") === lib) + " " + module.children.length);
const first = require("./counter");
delete require.cache[require.resolve("./counter")];
out.push("counter " + first + " " + require("./counter"));
try { require("./bad") } catch (e) { out.push(e.message + " " + e.stack.split("\n")[1].trim()) }
try { require("./syntax") } catch (e) { out.push(e.name) }
out.push("cached after failing " + ("/app/bad.js" in require.cache));
const esm = require("./esm.mjs");
out.push("esm " + esm.value + " " + esm.default);
out.push("dynamic " + typeof require("./dynamic").then);
module.exports = out;
`)},
	"app/lib/index.js": {Data: []byte(`exports.name = "lib";`)},
	"app/data.json":    {Data: []byte(`{"x": 1}`)},
	"app/cycle/a.js":   {Data: []byte("exports.done = false;\nconst b = require(\"./b\");\nexports.sawB = b.sawA;\nexports.done = true;\n")},
	"app/cycle/b.js":   {Data: []byte("const a = require(\"./a\");\nexports.sawA = \"a.done=\" + a.done;\n")},
	"app/counter.js":   {Data: []byte("globalThis.count = (globalThis.count || 0) + 1;\nmodule.exports = globalThis.count;\n")},
	"app/bad.js":       {Data: []byte("\nfunction boom() { throw new Error(\"boom\") }\nboom();\n")},
	"app/syntax.js":    {Data: []byte("let x = ;\n")},
	"app/esm.mjs":      {Data: []byte(`export const value = 42; export default "esm-default";`)},
	"app/dynamic.js":   {Data: []byte(`module.exports = import("./esm.mjs");`)},
	"app/entry.mjs":    {Data: []byte(``)},
	"app/node_modules/pkg/package.json": {Data: []byte(`{"name": "pkg", "exports": {
		".": {"import": "./esm.mjs", "require": "./main.cjs"},
		"./feature": "./feature.js"}}`)},
	"app/node_modules/pkg/main.cjs":   {Data: []byte(`module.exports = { hello: () => "hi from cjs" };`)},
	"app/node_modules/pkg/esm.mjs":    {Data: []byte(`export const hello = () => "hi from esm"; export default "pkg-default";`)},
	"app/node_modules/pkg/feature.js": {Data: []byte(`exports.f = "feature";`)},
	"app/node_modules/pkg/private.js": {Data: []byte(`exports.p = 1;`)},
}

// TestHostRequire runs a CommonJS program under the host's require.
func TestHostRequire(t *testing.T) {
	h := newCJSHost(t, cjsFiles)
	v, err := h.runMain("/app/main.js")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	if err := v.Decode(&out); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"lib lib true true",
		"names /app/main.js /app /app/main.js",
		"json 1",
		"cycle true a.done=false",
		"pkg hi from cjs, feature",
		"ERR_PACKAGE_PATH_NOT_EXPORTED",
		"MODULE_NOT_FOUND Cannot find module './nope'",
		"resolve /app/node_modules/pkg/main.cjs /app/lib/index.js",
		"cached true 5",
		"counter 1 2",
		"boom at boom (/app/bad.js:2:25)",
		"SyntaxError",
		"cached after failing false",
		"esm 42 esm-default",
		"dynamic function",
	}
	if strings.Join(out, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(out, "\n"), strings.Join(want, "\n"))
	}

}

// TestHostRequireFromESM runs an ES module that imports CommonJS, a package
// with both, JSON, and requires through createRequire.
func TestHostRequireFromESM(t *testing.T) {
	h := newCJSHost(t, cjsFiles)
	ns, err := h.rt.EvalModule("/app/entry.mjs", `
		import lib, {name} from "./lib/index.js";
		import pkg, {hello} from "pkg";
		import data from "./data.json" with {type: "json"};
		import {createRequire} from "node:module";
		const require = createRequire(import.meta.url);
		export const out = [
			"lib " + lib.name + " " + name,
			"pkg " + pkg + " " + hello(),
			"json " + data.x,
			"require " + require("pkg").hello() + " " + import.meta.dirname,
			"same " + (require("./lib") === lib),
		];
		export const later = import("./dynamic.js").then(m => m.default).then(ns => "dynamic " + ns.value);
	`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ns.Get("out")
	later, _ := ns.Get("later")
	l, err := later.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := out.Decode(&got); err != nil {
		t.Fatal(err)
	}
	got = append(got, l.String())
	want := []string{
		"lib lib lib",
		"pkg pkg-default hi from esm",
		"json 1",
		"require hi from cjs /app",
		"same true",
		"dynamic 42",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
