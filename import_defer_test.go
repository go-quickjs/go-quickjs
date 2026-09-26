package quickjs_test

import (
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
