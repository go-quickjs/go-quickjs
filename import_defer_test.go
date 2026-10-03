package quickjs_test

import (
	"fmt"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// deferRuntime is a runtime whose modules are the given sources, named by
// their specifiers without the leading "./".
func deferRuntime(files map[string]string) *quickjs.Runtime {
	rt := quickjs.New()
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		name := strings.TrimPrefix(specifier, "./")
		return files[name], name, nil
	})
	return rt
}

func TestImportDefer(t *testing.T) {
	rt := deferRuntime(map[string]string{
		"setup.js": `globalThis.log = [];`,
		"dep.js":   `globalThis.log.push("dep"); export const x = 1, then = 2;`,
		"async.js": `globalThis.log.push("async"); await null; export const y = 2;`,
		"uses.js":  `import "./async.js"; globalThis.log.push("uses"); export const z = 3;`,
	})
	defer rt.Close()
	ns, err := rt.EvalModule("main.js", `
		import "./setup.js";
		import defer * as dep from "./dep.js";
		import defer * as uses from "./uses.js";
		log.push("main");
		// Nothing about an export, a symbol or "then" runs the module.
		const tag = dep[Symbol.toStringTag], then = dep.then;
		Object.getPrototypeOf(dep); Object.isExtensible(dep);
		try { dep.x = 5 } catch {}
		log.push("untouched");
		const x = dep.x;
		const keys = Object.keys(uses).join();
		export const out = [log.join(), tag, then, x, keys].join("|");
	`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ns.Get("out")
	// A deferred module's graph that awaits at the top level runs up front.
	if got, want := out.String(), "async,main,untouched,dep,uses|Deferred Module||1|z"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

func TestImportDeferErrors(t *testing.T) {
	rt := deferRuntime(map[string]string{
		"throws.js": `throw new RangeError("boom")`,
		"defer.js":  `import defer * as ns from "./throws.js"; export { ns };`,
		"self.js": `import defer * as self from "./self.js";
			try { self.x } catch (e) { globalThis.selfError = e.constructor.name }
			export const x = 1;`,
	})
	defer rt.Close()
	ns, err := rt.EvalModule("main.js", `
		let first, second;
		await import("./throws.js").catch(e => { first = e });
		// A module whose evaluation threw links again, and using a deferred
		// namespace of it throws the same error.
		const { ns } = await import("./defer.js");
		try { ns.anything } catch (e) { second = e }
		await import("./self.js");
		export const out = [first === second, first.message, globalThis.selfError].join();
	`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ns.Get("out")
	if got, want := out.String(), "true,boom,TypeError"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

func TestImportDeferCall(t *testing.T) {
	rt := deferRuntime(map[string]string{
		"dep.js": `globalThis.log.push("dep"); export const x = 1;`,
	})
	defer rt.Close()
	ns, err := rt.EvalModule("main.js", `
		globalThis.log = [];
		import defer * as a from "./dep.js";
		const b = await import.defer("./dep.js");
		log.push("got");
		export const out = [a === b, log.join(), b.x, log.join()].join("|");
	`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ns.Get("out")
	if got, want := out.String(), "true|got|1|got,dep"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
	for _, src := range []string{
		`import defer x from "./dep.js"`, `import defer {x} from "./dep.js"`,
		`new import.defer("./dep.js")`, `import.defer()`,
	} {
		rt := deferRuntime(nil)
		_, err := rt.EvalModule("bad.js", src)
		if err == nil || !strings.Contains(err.Error(), "SyntaxError") {
			t.Errorf("%s: got %v, want a SyntaxError", src, err)
		}
		rt.Close()
	}
}

func TestImportSource(t *testing.T) {
	files := map[string]string{
		"dep.js":  `export default "the default";`,
		"link.js": `import { missing } from "./link.js";`,
	}
	// `import source from` is a default import named source, and
	// `import source source from` a source import of a binding named source.
	rt := deferRuntime(files)
	ns, err := rt.EvalModule("main.js", `
		import source from "./dep.js";
		export const out = source;
	`)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := ns.Get("out"); out.String() != "the default" {
		t.Errorf("out = %q", out.String())
	}
	rt.Close()

	// No module here has a source, so a source import fails to link.
	rt = deferRuntime(files)
	_, err = rt.EvalModule("main.js", `import source source from "./dep.js";`)
	if err == nil || !strings.Contains(err.Error(), "SyntaxError") {
		t.Errorf("a source import linked: %v", err)
	}
	rt.Close()

	// The whole graph is loaded before any of it is linked, so a module that
	// cannot be found is reported rather than a link error elsewhere.
	rt = deferRuntime(files)
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		name := strings.TrimPrefix(specifier, "./")
		if src, ok := files[name]; ok {
			return src, name, nil
		}
		return "", "", fmt.Errorf("no module %q", name)
	})
	_, err = rt.EvalModule("main.js", `import "./link.js"; import source x from "./nowhere.js";`)
	if err == nil || strings.Contains(err.Error(), "SyntaxError") {
		t.Errorf("got %v, want the failure to find nowhere.js", err)
	}
	rt.Close()

	rt = deferRuntime(files)
	defer rt.Close()
	rt.Set("abstractModuleSource", rt.AbstractModuleSource())
	ns, err = rt.EvalModule("main2.js", `
		let error;
		await import.source("./dep.js").catch(e => { error = e.constructor.name });
		const AMS = abstractModuleSource;
		let constructed;
		try { new AMS() } catch (e) { constructed = e.constructor.name }
		const tag = Object.getOwnPropertyDescriptor(AMS.prototype, Symbol.toStringTag).get.call({});
		export const out = [error, constructed, tag, typeof globalThis.AbstractModuleSource].join();
	`)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := ns.Get("out"); out.String() != "SyntaxError,TypeError,,undefined" {
		t.Errorf("out = %q", out.String())
	}
}

// TestImportIsNoBinding checks that import(), import.defer() and
// import.source() reach no function a script can see: they are not properties
// of the global object, and neither a global assigned under their names nor a
// with object holding one stands in for them.
func TestImportIsNoBinding(t *testing.T) {
	rt := deferRuntime(map[string]string{
		"dep.js": `export const x = 1;`,
	})
	defer rt.Close()
	ns, err := rt.EvalModule("main.js", `
		const names = Object.getOwnPropertyNames(globalThis).filter(n => n.startsWith("import"));
		const has = ["import", "import.defer", "import.source"].map(n => n in globalThis);
		globalThis.import = globalThis["import.defer"] = globalThis["import.source"] = () => "replaced";
		const a = await import("./dep.js");
		const b = await import.defer("./dep.js");
		const c = await import.source("./dep.js").then(() => "resolved", e => e.constructor.name);
		const w = await new Function("with ({ import: () => 'with' }) return import('./dep.js')")();
		export const out = [names.length, has.join(), a.x, b.x, c, w.x].join("|");
	`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ns.Get("out")
	if got, want := out.String(), "0|false,false,false|1|1|SyntaxError|1"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}
