package quickjs_test

import "testing"

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
