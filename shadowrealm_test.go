package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestShadowRealmEvaluate pins what crosses a ShadowRealm's boundary:
// primitives as they are, functions as wrappers, and nothing else.
func TestShadowRealmEvaluate(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`var r = new ShadowRealm(); [r.evaluate("1 + 1"), r.evaluate("'s'"), typeof r.evaluate("Symbol()"), r.evaluate("undefined")].join()`, "2,s,symbol,"},
		// Its globals are its own.
		{`var r = new ShadowRealm(); r.evaluate("var x = 1; globalThis.y = 2"); [typeof x, typeof y, r.evaluate("x + y")].join()`, "undefined,undefined,3"},
		{`var r = new ShadowRealm(); r.evaluate("Array.prototype.push = null"); typeof [].push`, "function"},
		// An object does not cross, and a failure crosses as a TypeError of
		// the realm it reached.
		{`var r = new ShadowRealm(); var e = []; for (var src of ["({})", "throw new RangeError('x')", "("]) { try { r.evaluate(src) } catch (x) { e.push(x.constructor.name) } } e.join()`,
			"TypeError,TypeError,SyntaxError"},
		{`try { new ShadowRealm().evaluate(1) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { ShadowRealm() } catch (e) { e.constructor.name }`, "TypeError"},
		{`Object.prototype.toString.call(new ShadowRealm())`, "[object ShadowRealm]"},
	})
}

// TestShadowRealmWrappedFunctions pins the wrapper a function crosses as:
// a function of the receiving realm that wraps its arguments on the way in
// and its result on the way out, and turns an exception into a TypeError.
func TestShadowRealmWrappedFunctions(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`var r = new ShadowRealm(); var f = r.evaluate("(function add(a, b) { return a + b })");
[f(1, 2), f.name, f.length, Object.getPrototypeOf(f) === Function.prototype, typeof f.prototype].join()`, "3,add,2,true,undefined"},
		// A function passed in is wrapped for the other side, and calling it
		// comes back through a wrapper again.
		{`var r = new ShadowRealm(); var apply = r.evaluate("(fn, x) => fn(x) * 2"); apply(x => x + 1, 4)`, "10"},
		{`var r = new ShadowRealm(); var f = r.evaluate("() => ({})"); try { f() } catch (e) { e.constructor.name }`, "TypeError"},
		{`var r = new ShadowRealm(); var f = r.evaluate("x => x"); try { f({}) } catch (e) { e.constructor.name }`, "TypeError"},
		{`var r = new ShadowRealm(); var f = r.evaluate("() => { throw new Error('inside') }"); try { f() } catch (e) { [e instanceof TypeError, e.message].join() }`,
			"true,Error: inside"},
		// Each crossing makes a new wrapper, and a wrapper is not a constructor.
		{`var r = new ShadowRealm(); r.evaluate("globalThis.g = () => 1"); var a = r.evaluate("g"), b = r.evaluate("g"); var n; try { new a() } catch (e) { n = e.constructor.name }
[a === b, n].join()`, "false,TypeError"},
		{`var r = new ShadowRealm(); var f = r.evaluate("Object.defineProperty(function () {}, 'length', { value: Infinity })"); String(f.length)`, "Infinity"},
	})
}

// TestShadowRealmImportValue pins that importValue imports into the shadow
// realm -- an instance of the module of its own -- and hands back the export,
// wrapped, or a TypeError.
func TestShadowRealmImportValue(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		switch specifier {
		case "counter.js":
			return `export let count = 0; export function bump() { return ++count }`, specifier, nil
		case "broken.js":
			return `throw new RangeError("no")`, specifier, nil
		}
		return "", "", errNotFound
	})
	v, err := rt.EvalModule("main.js", `
		import { bump } from "counter.js";
		bump();
		const r = new ShadowRealm();
		const inner = await r.importValue("counter.js", "bump");
		const results = [bump(), inner(), inner(), typeof inner];
		for (const [spec, name] of [["counter.js", "missing"], ["broken.js", "x"], ["nowhere.js", "x"]]) {
			try { await r.importValue(spec, name); results.push("ok") } catch (e) { results.push(e.constructor.name) }
		}
		globalThis.out = results.join();
	`)
	if err != nil {
		t.Fatal(v, err)
	}
	out, err := rt.Eval("out")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "2,1,2,function,TypeError,TypeError,TypeError"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

type notFound struct{}

func (notFound) Error() string { return "not found" }

var errNotFound error = notFound{}

// TestShadowRealmCallSitesStayOnTheirSide pins that a stack trace made on
// one side of a ShadowRealm's boundary gives no CallSite's this or function
// from the other: code inside the realm used to get the outer realm's
// objects from one, and could use them (KI-08).
func TestShadowRealmCallSitesStayOnTheirSide(t *testing.T) {
	checkEval(t, `
		var sr = new ShadowRealm();
		var probe = sr.evaluate(`+"`"+`
			Error.prepareStackTrace = (e, sites) => sites;
			(0, function named() {
				var sites = new Error().stack;
				Error.prepareStackTrace = undefined;
				var leaked = 0;
				for (var s of sites) {
					var f = s.getFunction(), t = s.getThis();
					if (f && f !== named) leaked++;
					if (t && typeof t === "object" && t !== globalThis) leaked++;
				}
				return leaked;
			})
		`+"`"+`);
		var holder = {secret: 1, run() { return probe() }};
		var inward = holder.run();

		// And outward: a function of this realm, called from inside the other,
		// sees none of the other's frames either.
		var seen = 0;
		function outer() {
			Error.prepareStackTrace = (e, sites) => sites;
			var sites = new Error().stack;
			Error.prepareStackTrace = undefined;
			for (var s of sites) {
				var f = s.getFunction(), t = s.getThis();
				if (f && f !== outer && f !== caller) seen++;
				if (t && typeof t === "object" && t !== globalThis) seen++;
			}
			return 0;
		}
		var inner = sr.evaluate("(function inner(cb) { return {m() { return cb() }}.m() })");
		function caller() { return inner(outer) }
		caller();
		inward + "," + seen`, "0,0")
}
