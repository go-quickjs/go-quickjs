package quickjs_test

import (
	"errors"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestCommonJSExports pins what CommonJSExports reads from CommonJS source:
// the names each way of exporting gives, wherever it is nested, and what is
// handed on whole, without running anything.
func TestCommonJSExports(t *testing.T) {
	for _, c := range []struct {
		src                string
		exports, reexports string
	}{
		{"exports.a = 1; module.exports.b = 2; exports['c-d'] = 3; exports[computed] = 4; exports.a = 5;", "a b c-d", ""},
		{"#!/usr/bin/env node\nObject.defineProperty(exports, '__esModule', { value: true });\nreturn;", "__esModule", ""},
		{"if (x) { exports.e = 1 } else { exports.f = 2 }\nfunction later() { exports.g = 3 }\nconst o = { m: () => { exports.h = 4 } };", "e f g h", ""},
		{"module.exports = { a, b: 1, 'c': 2, [d]: 3, f() {}, ...require('./more'), ...other };", "a b c f", "./more"},
		{"module.exports = require('./other');", "", "./other"},
		{"__exportStar(require('./s1'), exports); tslib.__exportStar(require('./s2'), exports);", "", "./s1 ./s2"},
		{"module.exports = function () {}; exports.ignored += 1;", "", ""},
	} {
		exports, reexports, err := quickjs.CommonJSExports("/f.js", c.src)
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if got := strings.Join(exports, " "); got != c.exports {
			t.Errorf("%q: exports %q, want %q", c.src, got, c.exports)
		}
		if got := strings.Join(reexports, " "); got != c.reexports {
			t.Errorf("%q: reexports %q, want %q", c.src, got, c.reexports)
		}
	}
	_, _, err := quickjs.CommonJSExports("/bad.js", "let ok;\nlet x = ;")
	var se *quickjs.SyntaxError
	if !errors.As(err, &se) || !strings.HasSuffix(err.Error(), "(/bad.js:2:9)") {
		t.Errorf("syntax error: %v", err)
	}
}

// TestSyntheticModule pins a module a host defines: its exports are there to
// link against before it runs, it runs in its turn in the graph -- after what
// is imported before it, before its importer -- once however many import it,
// and its failures are the import's.
func TestSyntheticModule(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.Eval(`globalThis.order = []`)
	evaluations := 0
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		switch specifier {
		case "./first.mjs":
			return `order.push("first")`, "/first.mjs", nil
		case "./synthetic":
			err := rt.DefineSyntheticModule("/synthetic", []string{"default", "a", "b-c", "unset"}, func() (map[string]any, error) {
				evaluations++
				order, _ := rt.Eval(`order.push("synthetic"), order.join(" ")`)
				return map[string]any{"default": "def", "a": 1, "b-c": order.String()}, nil
			})
			return "", "/synthetic", err
		case "./throws":
			err := rt.DefineSyntheticModule("/throws", []string{"x"}, func() (map[string]any, error) {
				return nil, rt.ThrowRangeError("no %s", "x")
			})
			return "", "/throws", err
		case "./extra":
			err := rt.DefineSyntheticModule("/extra", []string{"x"}, func() (map[string]any, error) {
				return map[string]any{"y": 1}, nil
			})
			return "", "/extra", err
		}
		return "", "", errors.New("no " + specifier)
	})
	ns, err := rt.EvalModule("/main.mjs", `
		import "./first.mjs";
		import def, {a, "b-c" as bc, unset} from "./synthetic";
		import * as again from "./synthetic";
		order.push("main");
		export const got = [def, a, bc, typeof unset, again.a, Object.keys(again).join(",")].join(" | ");
		export const failures = Promise.all(["./throws", "./extra", "./missing"].map(
			s => import(s).catch(e => e.constructor.name + ": " + e.message)));
	`)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := ns.Get("got")
	if want := "def | 1 | first synthetic | undefined | 1 | a,b-c,default,unset"; got.String() != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	order, _ := rt.Eval(`order.join(" ")`)
	if order.String() != "first synthetic main" || evaluations != 1 {
		t.Errorf("order %q, evaluated %d times", order, evaluations)
	}
	failures, _ := ns.Get("failures")
	f, err := failures.Await(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	f.Decode(&lines)
	want := []string{
		"RangeError: no x",
		`ReferenceError: the module "/extra" does not export "y"`,
		"TypeError: cannot resolve \"./missing\": no ./missing",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("failures:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// TestRequireModule pins require of an ES module from Go: it is the module an
// import of it is, evaluated once, synchronously, with no job of the script's
// running meanwhile; it refuses a graph that awaits at the top level and a
// module whose evaluation led to it; and what the module throws is the error.
func TestRequireModule(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	sources := map[string]string{
		"/lib.mjs":    `globalThis.libRuns = (globalThis.libRuns || 0) + 1; export const x = 1; export default "lib";`,
		"/tla.mjs":    `await 0; export const y = 2;`,
		"/uses.mjs":   `import "./tla.mjs"; export const z = 3;`,
		"/throws.mjs": `throw new TypeError("thrown in /throws.mjs");`,
		"/cycle.mjs":  `import {back} from "./back.js"; export const c = back();`,
	}
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		resolved := "/" + strings.TrimPrefix(specifier, "./")
		if resolved == "/back.js" {
			err := rt.DefineSyntheticModule(resolved, []string{"back"}, func() (map[string]any, error) {
				_, err := rt.RequireModule("./cycle.mjs", "/back.js")
				return map[string]any{"back": func() string { return "cycle: " + err.Error() }}, nil
			})
			return "", resolved, err
		}
		return sources[resolved], resolved, nil
	})
	rt.Set("requireModule", func(specifier string) (quickjs.Value, error) {
		return rt.RequireModule(specifier, "/main.js")
	})

	got, err := rt.Eval(`
		const log = [];
		Promise.resolve().then(() => log.push("microtask"));
		const lib = requireModule("./lib.mjs");
		log.push("lib " + lib.x + " " + lib.default + " " + Object.prototype.toString.call(lib));
		for (const s of ["./tla.mjs", "./uses.mjs", "./throws.mjs"]) {
			try { requireModule(s); log.push(s + " loaded") } catch (e) { log.push(s + ": " + e.message) }
		}
		log`)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := got.Decode(&lines); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"lib 1 lib [object Module]",
		"./tla.mjs: the module, or a module it imports, awaits at the top level",
		"./uses.mjs: the module, or a module it imports, awaits at the top level",
		"./throws.mjs: thrown in /throws.mjs",
		"microtask",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	// The same module an import of it is, run once.
	ns, err := rt.EvalModule("/importer.mjs", `import * as lib from "./lib.mjs"; export const same = lib;`)
	if err != nil {
		t.Fatal(err)
	}
	same, _ := ns.Get("same")
	again, err := rt.RequireModule("./lib.mjs", "/x.js")
	if err != nil || !same.StrictEqual(again) {
		t.Errorf("require and import differ: %v", err)
	}
	runs, _ := rt.Eval(`libRuns`)
	if runs.Int() != 1 {
		t.Errorf("lib ran %d times", runs.Int())
	}

	// From Go, the errors are the package's and the exception's.
	if _, err := rt.RequireModule("./tla.mjs", ""); !errors.Is(err, quickjs.ErrModuleAwaits) {
		t.Errorf("tla: %v", err)
	}
	var jsErr *quickjs.Error
	if _, err := rt.RequireModule("./throws.mjs", ""); !errors.As(err, &jsErr) {
		t.Errorf("throws: %v", err)
	}
	ns, err = rt.EvalModule("/c.mjs", `import {c} from "./cycle.mjs"; export {c};`)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := ns.Get("c")
	if want := "cycle: " + quickjs.ErrModuleEvaluating.Error(); c.String() != want {
		t.Errorf("cycle: got %q, want %q", c, want)
	}
}
