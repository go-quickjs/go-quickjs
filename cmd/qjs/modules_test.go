package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree writes files under a fresh directory, by slash-separated path.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// moduleTree is a program, its packages and its modules of both kinds.
var moduleTree = map[string]string{
	"package.json":       `{"name": "app", "imports": {"#util": "./lib/util.js", "#dep": "dep"}}`,
	"lib/index.js":       `exports.name = "lib";`,
	"lib/util.js":        `module.exports = "util";`,
	"data.json":          `{"x": 1}`,
	"cycle/a.js":         "exports.done = false;\nconst b = require(\"./b\");\nexports.sawB = b.sawA;\nexports.done = true;\n",
	"cycle/b.js":         "const a = require(\"./a\");\nexports.sawA = \"a.done=\" + a.done;\n",
	"counter.js":         "globalThis.count = (globalThis.count || 0) + 1;\nmodule.exports = globalThis.count;\n",
	"bad.js":             "\nfunction boom() { throw new Error(\"boom\") }\nboom();\n",
	"syntax.js":          "let x = ;\n",
	"esm.mjs":            `export const value = 42; export default "esm-default";`,
	"tla.mjs":            `await 0; export const late = 1;`,
	"typed/package.json": `{"type": "module"}`,
	"typed/m.js":         `export const typed = "module by package type";`,
	"order/a.mjs":        `(globalThis.order ??= []).push("a.mjs");`,
	"order/b.cjs":        `(globalThis.order ??= []).push("b.cjs"); exports.b = "b";`,
	"worker.cjs": `const { parentPort } = require("worker_threads");
parentPort.postMessage(require("./lib").name + " " + typeof module + " " + require.main.filename.endsWith("worker.cjs"));`,
	"node_modules/dep/package.json": `{"name": "dep", "exports": {
		".": {"import": "./esm.mjs", "require": "./main.cjs"},
		"./feature/*": "./features/*.js",
		"./feature/internal/*": null}}`,
	"node_modules/dep/main.cjs":                        `module.exports = { hello: "hi from cjs", version: "2" };`,
	"node_modules/dep/esm.mjs":                         `export const hello = "hi from esm"; export default "dep-default";`,
	"node_modules/dep/features/x.js":                   `exports.f = "feature x";`,
	"node_modules/dep/features/internal/z.js":          `exports.z = 1;`,
	"node_modules/legacy/package.json":                 `{"main": "lib/entry"}`,
	"node_modules/legacy/lib/entry.js":                 `exports.legacy = true;`,
	"node_modules/other/index.js":                      `exports.version = require("dep").version; exports.resolved = require.resolve("dep");`,
	"node_modules/other/node_modules/dep/package.json": `{"name": "dep"}`,
	"node_modules/other/node_modules/dep/index.js":     `exports.version = "1";`,
}

// TestRequire runs a CommonJS program under qjs: require as node has it --
// files, JSON, packages through their exports and imports, node's own
// modules, the cache and cycles -- and its errors, and require of an ES
// module.
func TestRequire(t *testing.T) {
	files := map[string]string{"main.js": `const path = require("path");
// Paths are the system's, which stdlib's path module does not read on
// Windows; they are compared as text.
const rel = f => f.slice(__dirname.length + 1).split(/[\\/]/).join("/");
const out = [];
const lib = require("./lib");
out.push("lib " + lib.name + " " + (this === module.exports) + " " + (require.main === module) + " " + module.id);
out.push("names " + rel(__filename) + " " + __filename.startsWith(__dirname));
out.push("json " + require("./data.json").x);
const a = require("./cycle/a");
out.push("cycle " + a.done + " " + a.sawB);
out.push("dep " + require("dep").hello + ", " + require("dep/feature/x").f);
for (const r of ["dep/feature/internal/z", "dep/nope", "./nope", "cluster"]) {
  try { require(r); out.push(r + " loaded") }
  catch (e) { out.push(r + ": " + e.code + (e.requireStack ? " " + e.requireStack.map(rel).join(",") : "")) }
}
out.push("imports " + require("#util") + ", " + require("#dep").hello);
out.push("legacy " + require("legacy").legacy);
const other = require("other");
out.push("nested " + require("dep").version + " / " + other.version + " " + rel(other.resolved));
out.push("builtins " + (require("node:path") === path) + " " + typeof require("fs").readFileSync);
out.push("resolve " + rel(require.resolve("dep")) + " " + require.resolve("node:fs"));
const first = require("./counter");
delete require.cache[require.resolve("./counter")];
out.push("counter " + first + " " + require("./counter"));
try { require("./bad") } catch (e) { out.push(e.message + " " + rel(e.stack.split("\n")[1].trim().replace(/^at boom \(|\)$/g, ""))) }
try { require("./syntax") } catch (e) { out.push(e.name + " " + e.message.endsWith("syntax.js:1:9)")) }
const esm = require("./esm.mjs");
out.push("esm " + esm.value + " " + esm.default);
try { require("./tla.mjs") } catch (e) { out.push(e.code) }
out.push("typed " + require("./typed/m.js").typed);
out.push("module " + module.children.length + " " + rel(module.paths[0]) + " " + typeof require("module").createRequire);
console.log(out.join("\n"));
`}
	for k, v := range moduleTree {
		files[k] = v
	}
	dir := writeTree(t, files)
	code, out, errOut := exec(t, "", filepath.Join(dir, "main.js"))
	want := strings.Join([]string{
		"lib lib true true .",
		"names main.js true",
		"json 1",
		"cycle true a.done=false",
		"dep hi from cjs, feature x",
		"dep/feature/internal/z: ERR_PACKAGE_PATH_NOT_EXPORTED",
		"dep/nope: ERR_PACKAGE_PATH_NOT_EXPORTED",
		"./nope: MODULE_NOT_FOUND main.js",
		"cluster: ERR_UNKNOWN_BUILTIN_MODULE",
		"imports util, hi from cjs",
		"legacy true",
		"nested 2 / 1 node_modules/other/node_modules/dep/index.js",
		"builtins true function",
		"resolve node_modules/dep/main.cjs node:fs",
		"counter 1 2",
		"boom bad.js:2:25",
		"SyntaxError true",
		"esm 42 esm-default",
		"ERR_REQUIRE_ASYNC_MODULE",
		"typed module by package type",
		"module 12 node_modules function",
	}, "\n") + "\n"
	if code != 0 || out != want {
		t.Errorf("code=%d err=%q\ngot:\n%s\nwant:\n%s", code, errOut, out, want)
	}
}

// TestImportCommonJS runs an ES module that imports CommonJS -- its default
// and its names, in the order the graph names it -- a package by its import
// condition, JSON, a .js file a package's type makes a module, and requires
// through createRequire.
func TestImportCommonJS(t *testing.T) {
	files := map[string]string{"entry.mjs": `import "./order/a.mjs";
import {b} from "./order/b.cjs";
import lib, {name} from "./lib/index.js";
import dep, {hello} from "dep";
import data from "./data.json" with {type: "json"};
import {createRequire} from "node:module";
import {typed} from "./typed/m.js";
globalThis.order.push("entry");
const require = createRequire(import.meta.url);
console.log([
  "order " + globalThis.order.join(", ") + " " + b,
  "esm " + lib.name + " " + name + " " + dep + " " + hello + " " + data.x,
  "require " + require("dep").hello + " " + typed,
  "meta " + import.meta.resolve("dep").endsWith("esm.mjs") + " " + import.meta.resolve("node:fs"),
].join("\n"));
`}
	for k, v := range moduleTree {
		files[k] = v
	}
	dir := writeTree(t, files)
	code, out, errOut := exec(t, "", filepath.Join(dir, "entry.mjs"))
	want := strings.Join([]string{
		"order a.mjs, b.cjs, entry b",
		"esm lib lib dep-default hi from esm 1",
		"require hi from cjs module by package type",
		"meta true node:fs",
	}, "\n") + "\n"
	if code != 0 || out != want {
		t.Errorf("code=%d err=%q\ngot:\n%s\nwant:\n%s", code, errOut, out, want)
	}
}

// TestRequireElsewhere pins require where the code is not a file run as
// CommonJS: -e, a CommonJS worker's file and an eval worker, which node runs
// as CommonJS too; and --script, which runs a file as a classic script --
// its top-level var a global -- where CommonJS keeps it the module's.
func TestRequireElsewhere(t *testing.T) {
	code, out, errOut := exec(t, "", "-e",
		`console.log(typeof require("path").join, typeof module, __filename, require.main === undefined)`)
	if code != 0 || out != "function object [eval] true\n" {
		t.Errorf("-e: code=%d out=%q err=%q", code, out, errOut)
	}

	files := map[string]string{"main.js": `const { Worker } = require("worker_threads");
const path = require("path");
const got = [];
const done = () => { if (got.length === 2) console.log(got.sort().join("\n")) };
new Worker(path.join(__dirname, "worker.cjs")).on("message", m => { got.push("file: " + m); done() });
new Worker('require("worker_threads").parentPort.postMessage(typeof require + " " + __filename)', { eval: true })
  .on("message", m => { got.push("eval: " + m); done() });
`,
		"script.js": `var x = 1; console.log(typeof globalThis.x, this === globalThis);`,
	}
	for k, v := range moduleTree {
		files[k] = v
	}
	dir := writeTree(t, files)
	code, out, errOut = exec(t, "", filepath.Join(dir, "main.js"))
	if want := "eval: function [worker eval]\nfile: lib object true\n"; code != 0 || out != want {
		t.Errorf("workers: code=%d out=%q err=%q", code, out, errOut)
	}
	code, out, errOut = exec(t, "", filepath.Join(dir, "script.js"))
	if code != 0 || out != "undefined false\n" {
		t.Errorf("CommonJS: code=%d out=%q err=%q", code, out, errOut)
	}
	code, out, errOut = exec(t, "", "--script", filepath.Join(dir, "script.js"))
	if code != 0 || out != "number true\n" {
		t.Errorf("--script: code=%d out=%q err=%q", code, out, errOut)
	}
}
