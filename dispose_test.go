package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestDisposableStack(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		// Resources are disposed of last-first, and use hands back its value.
		{`var log = []; var s = new DisposableStack();
		  var r = s.use({[Symbol.dispose]() { log.push("a") }});
		  s.defer(() => log.push("b")); s.adopt(7, v => log.push("c" + v)); s.use(null);
		  s.dispose(); s.dispose(); log.join() + "|" + s.disposed + "|" + typeof r`, "c7,b,a|true|object"},
		// An error does not stop the rest, and a later one suppresses it.
		{`var s = new DisposableStack(); s.defer(() => { throw 1 }); s.defer(() => { throw 2 });
		  try { s.dispose() } catch (e) {
		    [e.constructor.name, e.error, e.suppressed, e.message].join()
		  }`, "SuppressedError,1,2,An error was suppressed during disposal"},
		{`var errs = []; var s = new DisposableStack();
		  for (var f of [() => s.use(1), () => s.use({}), () => s.defer(1)]) {
		    try { f() } catch (e) { errs.push(e.constructor.name) }
		  }
		  s.dispose(); try { s.use({}) } catch (e) { errs.push(e.constructor.name) }
		  errs.join()`, "TypeError,TypeError,TypeError,ReferenceError"},
		{`var log = []; var s = new DisposableStack(); s.defer(() => log.push(1));
		  var m = s.move(); s.dispose(); log.push("moved"); m.dispose();
		  [log, s.disposed, m.disposed, DisposableStack.prototype[Symbol.dispose] === DisposableStack.prototype.dispose].join("|")`,
			"moved,1|true|true|true"},
		{`var e = new SuppressedError(1, 2, "m"); [e.error, e.suppressed, e.message, e instanceof Error,
		   SuppressedError.length, Object.keys(e).length].join()`, "1,2,m,true,3,0"},
		{`[typeof Symbol.dispose, Symbol.asyncDispose.description].join()`, "symbol,Symbol.asyncDispose"},
		{`var closed = 0; function* g() { try { yield 1 } finally { closed++ } }
		  var it = g(); it.next(); it[Symbol.dispose](); closed`, "1"},
	})
}

func TestAsyncDisposableStack(t *testing.T) {
	// An async disposer is awaited before the next one runs, and a sync one
	// on an async stack is wrapped to return a promise.
	checkAsync(t, `var log = []; var r = ""; var s = new AsyncDisposableStack();
	  s.use({[Symbol.dispose]() { log.push("sync") }});
	  s.defer(async () => { await null; log.push("async") });
	  s.disposeAsync().then(() => { r = log.join() + "|" + s.disposed })`, `r`, "async,sync|true")
	checkAsync(t, `var r = ""; var s = new AsyncDisposableStack();
	  s.defer(() => Promise.reject(1)); s.defer(async () => { throw 2 });
	  s.disposeAsync().catch(e => { r = [e.constructor.name, e.error, e.suppressed].join() })`,
		`r`, "SuppressedError,1,2")
	checkAsync(t, `var r = ""; AsyncDisposableStack.prototype.disposeAsync.call({}).catch(e => { r = e.constructor.name })`,
		`r`, "TypeError")
	checkAsync(t, `var r = ""; var called = 0;
	  var it = { __proto__: Object.getPrototypeOf(Object.getPrototypeOf((async function* () {}).prototype)),
	    async return() { called = arguments.length + 1; return 5 } };
	  it[Symbol.asyncDispose]().then(v => { r = called + "," + v })`, `r`, "1,undefined")
}

func TestUsingDeclarations(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		// Disposed of last-first when the block is left, however it is left.
		{`var log = []; var res = n => ({[Symbol.dispose]() { log.push(n) }});
		  { using a = res("a"), b = res("b"); log.push("body") }
		  (function () { using c = res("c"); return log.push("return") })();
		  for (var i = 0; i < 2; i++) { using d = res("d" + i); if (i == 0) continue; break }
		  log.join()`, "body,b,a,return,c,d0,d1"},
		// A for-of head is disposed of each iteration, a for head when the loop ends.
		{`var log = []; var res = n => ({[Symbol.dispose]() { log.push(n) }});
		  for (using x of [res(1), res(2)]) log.push("it");
		  for (using y = res("y"); log.length < 6; ) log.push(log.length);
		  log.join()`, "it,1,it,2,4,5,y"},
		// An error from disposing suppresses the one leaving the block, and
		// replaces a return.
		{`try { { using a = {[Symbol.dispose]() { throw 1 }}; throw 2 } } catch (e) { [e.error, e.suppressed].join() }`, "1,2"},
		{`function f() { using a = {[Symbol.dispose]() { throw "disposed" }}; return "returned" }
		  try { f() } catch (e) { e }`, "disposed"},
		// null and undefined are nothing to dispose of; anything else must be.
		{`{ using a = null, b = undefined } try { { using c = {} } } catch (e) { e.constructor.name }`, "TypeError"},
		// using is still an identifier where a declaration cannot stand.
		{`var using = [1, 2], x = 1; using[x]`, "2"},
		{`var using = 5; if (true) using
		  ; using`, "5"},
		{`var log = []; var of = [[9], [8], [7]]; for (using of of [0, 1, 2]) log.push(using); log.join()`, "7"},
	})
	for _, src := range []string{
		// using [x] is an element access, not a pattern, so it is no error.
		`{ using x; }`, `{ using {x} = null; }`,
		`switch (0) { case 0: using x = null; }`, `if (true) using x = null;`,
		`for (using x in {}) {}`, `for (using x = null of []) {}`, `using x = null;`,
		`{ using let = null; }`,
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil || !strings.Contains(err.Error(), "SyntaxError") {
			t.Errorf("%s: got %v, want a SyntaxError", src, err)
		}
		rt.Close()
	}
}

func TestAwaitUsingDeclarations(t *testing.T) {
	// An async disposer is awaited, and a scope that never reached its
	// `await using` does not await at all.
	checkAsync(t, `var r = ""; var log = [];
	  (async () => {
	    { await using a = {async [Symbol.asyncDispose]() { await null; log.push("a") }}, b = null; log.push("body") }
	    log.push("after");
	  })().then(() => { r = log.join() })`, `r`, "body,a,after")
	checkAsync(t, `var r = ""; var same = true;
	  async function f() { x: { if (true) break x; await using _ = null } r = same }
	  f(); same = false`, `r`, "true")
}
