package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestGoRecursionIsBounded pins that recursion the engine does in Go ends in
// a RangeError, as V8's does, rather than growing the goroutine's stack past
// Go's limit, which ends the process with nothing to recover (KI-04).
func TestGoRecursionIsBounded(t *testing.T) {
	const rangeError = "RangeError: maximum call stack size exceeded"
	catch := func(src string) string {
		return `try { ` + src + `; "finished" } catch (e) { e.name + ": " + e.message }`
	}
	for name, src := range map[string]string{
		"proxy cycle get": `var a = {}; var p = new Proxy(a, {}); Object.setPrototypeOf(a, p); a.x`,
		"proxy cycle set": `var a = {}; var p = new Proxy(a, {}); Object.setPrototypeOf(a, p); a.x = 1`,
		"proxy cycle has": `var a = {}; var p = new Proxy(a, {}); Object.setPrototypeOf(a, p); "x" in a`,
		"proxy chain":     `var o = {}; for (var i = 0; i < 2e6; i++) o = Object.create(new Proxy(o, {})); o.x`,
		// Each name is reset, so that the chain's names do not grow with it.
		"bound chain": `var f = function () { return 1 };
			for (var i = 0; i < 1e6; i++) { f = f.bind(null); Object.defineProperty(f, "name", {value: ""}) }
			f()`,
		"toJSON": `var level = 0;
			function deep(n, leaf) { let d = leaf; for (let i = 0; i < n; i++) d = [d]; return d }
			var trig = {toJSON() { level++; return JSON.stringify(deep(9000, level < 2000 ? trig : 1)) }};
			JSON.stringify(deep(9000, trig))`,
		"reviver": `var level = 0; var text = "[".repeat(9000) + "1" + "]".repeat(9000);
			function reviver(k, v) { if (level++ < 2000 && v === 1) JSON.parse(text, reviver); return v }
			JSON.parse(text, reviver)`,
		"flat": `var D, lvl = 0;
			var P = new Proxy([1], {has(t, k) { if (lvl++ < 500) D.flat(Infinity); return k in t }});
			D = P; for (var i = 0; i < 1e5; i++) D = [D]; D.flat(Infinity)`,
	} {
		t.Run(name, func(t *testing.T) { checkEval(t, catch(src), rangeError) })
	}

	// A proxy chain long enough to reach the limit is refused only there:
	// one of a few thousand links works, and is quick to make.
	checkEval(t, `var p = {x: 7}; for (var i = 0; i < 5000; i++) p = new Proxy(p, {}); [p.x, typeof p].join()`, "7,object")
	checkEval(t, `var f = function () {}; for (var i = 0; i < 5000; i++) f = new Proxy(f, {}); typeof f`, "function")

	// A host asking for more depth than the Go stack holds gets what it holds.
	rt := quickjs.New(quickjs.WithMaxCallDepth(400000), quickjs.WithStackSize(1<<24))
	defer rt.Close()
	if got := evalString(t, rt, catch(`function f() { f() } f()`)); got != rangeError {
		t.Errorf("recursion with a raised depth limit: %s", got)
	}
}

// TestLongAsyncGeneratorQueue pins that a long queue of requests on an async
// generator that then ends is served in a loop, rather than a recursion as
// deep as the queue is long (KI-04).
func TestLongAsyncGeneratorQueue(t *testing.T) {
	checkAsync(t, `
		var settled = 0, release;
		async function* g() { await new Promise(r => release = r); }
		var it = g();
		it.next();
		for (var i = 0; i < 300000; i++) it.next().then(() => settled++);
		release();`,
		`settled`, "300000")
}
