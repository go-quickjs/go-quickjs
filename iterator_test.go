package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestIteratorHelpers(t *testing.T) {
	cases := []struct{ src, want string }{
		{`[1,2,3].values().map(x => x * 2).toArray().join(",")`, "2,4,6"},
		{`[1,2,3,4].values().filter(x => x % 2 === 0).toArray().join(",")`, "2,4"},
		{`[1,2,3,4,5].values().take(2).toArray().join(",")`, "1,2"},
		{`[1,2,3,4,5].values().drop(2).toArray().join(",")`, "3,4,5"},
		{`[[1,2],[3]].values().flatMap(x => x).toArray().join(",")`, "1,2,3"},
		{`String([1,2,3].values().reduce((a, b) => a + b))`, "6"},
		{`String([1,2,3].values().reduce((a, b) => a + b, 10))`, "16"},
		{`String([1,2,3].values().some(x => x > 2))`, "true"},
		{`String([1,2,3].values().every(x => x > 0))`, "true"},
		{`String([1,2,3].values().find(x => x > 1))`, "2"},
		{`var out = []; [1,2].values().forEach(x => out.push(x)); out.join(",")`, "1,2"},
		{`[10,20].values().map((x, i) => i + ":" + x).toArray().join(",")`, "0:10,1:20"},

		// The whole point of the lazy helpers: a pipeline over an infinite
		// source terminates as long as something downstream stops asking.
		{`function* nat() { let i = 0; while (true) yield i++; }
		  nat().take(5).toArray().join(",")`, "0,1,2,3,4"},
		{`function* nat() { let i = 0; while (true) yield i++; }
		  nat().map(x => x * x).filter(x => x % 2 === 0).take(3).toArray().join(",")`, "0,4,16"},

		// A helper owns the iterator beneath it, so reaching a limit or
		// throwing closes the source and its finally blocks run.
		{`var closed = false;
		  function* g() { try { yield 1; yield 2; } finally { closed = true; } }
		  g().take(1).toArray(); String(closed)`, "true"},
		{`var closed = false;
		  function* g() { try { yield 1; } finally { closed = true; } }
		  try { g().map(() => { throw new Error("x"); }).toArray(); } catch (e) {}
		  String(closed)`, "true"},

		{`Iterator.from([1,2,3]).toArray().join(",")`, "1,2,3"},
		{`Iterator.from("ab").toArray().join(",")`, "a,b"},
		// An object with only a next method is an iterator too.
		{`var it = {i: 0, next() { return this.i < 3 ? {value: this.i++, done: false} : {done: true}; }};
		  Iterator.from(it).map(x => x * 2).toArray().join(",")`, "0,2,4"},

		{`Object.prototype.toString.call([].values().map(x => x))`, "[object Iterator Helper]"},
		{`String([].values() instanceof Iterator)`, "true"},
		{`String([1,2,3].values().take(Infinity).toArray().length)`, "3"},
		{`[Iterator.prototype.map.length, Iterator.prototype.reduce.length,
		   Iterator.prototype.toArray.length].join(",")`, "1,1,0"},

		// flatMap flattens one level, over iterables only.
		{`[new Set([1,2])].values().flatMap(x => x).toArray().join(",")`, "1,2"},

		// Iterator is abstract, but subclassing it is the supported way to get
		// the helpers on a custom iterator.
		{`class C extends Iterator {
		    #i = 0;
		    next() { return this.#i < 3 ? {value: this.#i++, done: false} : {done: true}; }
		  }
		  new C().map(x => x * 2).toArray().join(",")`, "0,2,4"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

func TestIteratorHelperErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		{`new Iterator()`, "TypeError"},
		{`[1].values().take(-1)`, "RangeError"},
		{`[1].values().drop(-1)`, "RangeError"},
		// NaN would otherwise compare false against every bound and behave as
		// zero.
		{`[1].values().take(NaN)`, "RangeError"},
		{`[1].values().map(1)`, "TypeError"},
		{`Iterator.prototype.map.call(1, x => x)`, "TypeError"},
		{`[].values().reduce((a, b) => a)`, "TypeError"},
		{`Iterator.from(1)`, "TypeError"},
		// Flattening a string into characters is almost never meant.
		{`["a"].values().flatMap(x => x).toArray()`, "TypeError"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		_, err := rt.Eval(tc.src)
		if err == nil {
			t.Errorf("%s: no error, want %s", tc.src, tc.want)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %s", tc.src, err, tc.want)
		}
		rt.Close()
	}
}

// TestIteratorClosing pins that an iterator abandoned part-way through is told
// so, which is what lets a generator run its finally blocks and release
// whatever it was holding.
func TestIteratorClosing(t *testing.T) {
	const gen = `var closed = false;
	  function* g() { try { yield 1; yield 2; } finally { closed = true; } }
	  `
	cases := []struct{ src, want string }{
		{gen + `for (const x of g()) break; String(closed)`, "true"},
		{gen + `try { for (const x of g()) { throw 0; } } catch (e) {} String(closed)`, "true"},
		{gen + `(function () { for (const x of g()) return; })(); String(closed)`, "true"},
		{gen + `outer: for (const x of g()) break outer; String(closed)`, "true"},
		{gen + `outer: for (const x of [1,2]) { for (const y of g()) break outer; } String(closed)`, "true"},
		// A pattern that names fewer elements than the iterator has is done
		// with it.
		{gen + `var [a] = g(); String(closed)`, "true"},
		{gen + `function f([a]) {} f(g()); String(closed)`, "true"},
		{gen + `var it = g(); it.next(); it.return(9); String(closed)`, "true"},
		// Running to exhaustion closes it the ordinary way, and must not close
		// it twice.
		{gen + `for (const x of g()) {} String(closed)`, "true"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// TestGeneratorReturnRunsFinally pins that generator.return() behaves as if a
// return statement ran at the suspension point, rather than simply marking the
// generator finished.
func TestGeneratorReturnRunsFinally(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function* g() { try { yield 1; } finally {} }
		  var it = g(); it.next(); JSON.stringify(it.return(9))`, `{"value":9,"done":true}`},
		// A finally that returns overrides the value, as it would for an
		// ordinary return.
		{`function* g() { try { yield 1; } finally { return 42; } }
		  var it = g(); it.next(); JSON.stringify(it.return(9))`, `{"value":42,"done":true}`},
		// A catch must not see it: a return statement would not trigger one.
		{`var caught = false;
		  function* g() { try { yield 1; } catch (e) { caught = true; } finally {} }
		  var it = g(); it.next(); it.return(1); String(caught)`, "false"},
		// A finally may yield again, suspending the generator once more.
		{`function* g() { try { yield 1; } finally { yield 2; } }
		  var it = g(); it.next(); JSON.stringify([it.return(9), it.next()])`,
			`[{"value":2,"done":false},{"value":9,"done":true}]`},
		// Every enclosing finally runs, innermost first.
		{`var log = [];
		  function* g() { try { try { yield 1; } finally { log.push("inner"); } }
		                  finally { log.push("outer"); } }
		  var it = g(); it.next(); it.return(0); log.join(",")`, "inner,outer"},
		// The same nesting for an ordinary function, which had the same bug.
		{`var log = [];
		  function f() { try { try { return 1; } finally { log.push("inner"); } }
		                 finally { log.push("outer"); } }
		  f(); log.join(",")`, "inner,outer"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// TestDestructuringUsesIterator pins that an array pattern unpacks through the
// iterator protocol rather than by index, which is the difference between
// destructuring a Set working and silently producing undefined.
func TestDestructuringUsesIterator(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var [a] = new Set([1,2]); String(a)`, "1"},
		{`var [a, b] = "xy"; a + b`, "xy"},
		{`function* g() { yield 1; yield 2; } var [a, b] = g(); a + "," + b`, "1,2"},
		{`var [a, ...r] = new Set([1,2,3]); a + "|" + r.join(",")`, "1|2,3"},
		{`var [a] = new Map([[1,2]]); JSON.stringify(a)`, "[1,2]"},
		{`var [a, b] = [1, 2]; a + "," + b`, "1,2"},
		{`var [, b] = [1, 2]; String(b)`, "2"},
		{`var [a = 5] = []; String(a)`, "5"},
		{`function f([a, b]) { return a + b; } String(f(new Set([1, 2])))`, "3"},
		// Only as many values as the pattern names are pulled, so an infinite
		// generator terminates.
		{`function* nat() { let i = 0; while (true) yield i++; }
		  var [a, b, c] = nat(); a + "," + b + "," + c`, "0,1,2"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// TestSubclassingBuiltins pins that super() adopts the object the base
// constructor built. Every native constructor makes its own -- an Error needs a
// stack, an Array needs array storage -- so a derived class that ignored the
// result got an inert plain object.
func TestSubclassingBuiltins(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class E extends Error { constructor(m) { super(m); } }
		  var e = new E("boom");
		  [e.message, e instanceof Error, e instanceof E].join(",")`, "boom,true,true"},
		{`class A extends Array {}
		  var a = new A(); a.push(1); [a.length, a instanceof Array].join(",")`, "1,true"},
		{`class T extends TypeError {}
		  var e = new T("x"); [e.message, e instanceof TypeError, e instanceof Error].join(",")`,
			"x,true,true"},
		{`class S extends Set {} var s = new S([1,2]);
		  [s.size, s instanceof Set, s instanceof S].join(",")`, "2,true,true"},
		{`class M extends Map {} var m = new M(); m.set(1, 2);
		  [m.get(1), m instanceof M].join(",")`, "2,true"},
		{`class R extends RegExp {} var r = new R("a+");
		  [r.test("aaa"), r instanceof R].join(",")`, "true,true"},
		{`class D extends Date {} var d = new D(0);
		  [d.getTime(), d instanceof D].join(",")`, "0,true"},
		{`class U extends Uint8Array {} var u = new U(3); u[0] = 7;
		  [u.length, u[0], u instanceof U].join(",")`, "3,7,true"},
		{`class P extends Promise {} String(new P(r => r(1)) instanceof P)`, "true"},

		// new.target is the class the caller wrote new against, all the way
		// down to the base constructor.
		{`class B { constructor() { this.nt = new.target.name; } } class D extends B {}
		  new D().nt`, "D"},
		{`class B { constructor() { this.nt = new.target.name; } } new B().nt`, "B"},

		// An ordinary class hierarchy is unaffected.
		{`class A { constructor() { this.a = 1; } }
		  class B extends A { constructor() { super(); this.b = 2; } }
		  var o = new B(); o.a + "," + o.b`, "1,2"},
		{`class A { constructor() { this.v = 1; } } class B extends A {} class C extends B {}
		  String(new C().v)`, "1"},
		{`function F() { this.x = 1; } class G extends F {} String(new G().x)`, "1"},
		// A base constructor returning its own object still wins.
		{`class A { constructor() { return {custom: 1}; } } class B extends A {}
		  String(new B().custom)`, "1"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// A helper owns the iterator beneath it, and closing it is not always
// best-effort: when nothing else is in flight, a failure to close is the
// result rather than something to swallow.
func TestIteratorHelperClosing(t *testing.T) {
	const thrower = `
	  var closed = false;
	  function source() {
	    return {
	      i: 0,
	      next() { return {value: this.i++, done: false}; },
	      get return() { throw new TypeError("from return"); },
	      [Symbol.iterator]() { return this; },
	    };
	  }
	`
	// Calling return() on a helper surfaces whatever closing the source threw.
	for _, src := range []string{
		thrower + `var it = source().map(x => x); it.next(); it.return();`,
		thrower + `var it = source().filter(x => true); it.next(); it.return();`,
		thrower + `var it = source().drop(0); it.next(); it.return();`,
		// take reaching its limit closes as an ordinary completion.
		thrower + `source().take(1).toArray();`,
		// So does a terminal that stops early.
		thrower + `source().every(x => false);`,
		thrower + `source().find(x => true);`,
		thrower + `source().some(x => true);`,
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: no error, want the close failure to surface", src)
		}
		rt.Close()
	}

	// An argument that fails to convert still abandons the iterator.
	rt := quickjs.New()
	defer rt.Close()
	v, err := rt.Eval(`
		var closed = false;
		var it = {
		  next: () => ({done: false, value: 1}),
		  return() { closed = true; return {done: true}; },
		  [Symbol.iterator]() { return this; },
		};
		try {
		  Iterator.prototype.drop.call(it, {valueOf() { throw new Error("x"); }});
		} catch (e) {}
		String(closed);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "true" {
		t.Errorf("a failed conversion should still close the iterator, got %s", v.String())
	}
}

// TestIteratorHelperLimits pins the range a take or drop count must be in.
func TestIteratorHelperLimits(t *testing.T) {
	for _, src := range []string{
		`[1].values().drop()`,
		`[1].values().drop(undefined)`,
		`[1].values().drop(NaN)`,
		`[1].values().drop(-1)`,
		// A finite count past the integer range cannot be counted down to.
		`[1].values().drop(Number.MAX_SAFE_INTEGER + 1)`,
		`[1].values().take(NaN)`,
		`[1].values().take(Number.MAX_SAFE_INTEGER + 1)`,
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want RangeError", src)
		}
		rt.Close()
	}

	// Infinity is fine: it simply never runs out.
	rt := quickjs.New()
	defer rt.Close()
	if v, err := rt.Eval(`[1, 2].values().take(Infinity).toArray().join(",")`); err != nil {
		t.Fatal(err)
	} else if v.String() != "1,2" {
		t.Errorf("take(Infinity) = %q", v.String())
	}

	// Symbol.iterator present but not callable is a mistake, not an absence.
	if _, err := rt.Eval(`Iterator.from({[Symbol.iterator]: 0, next: () => ({done: true})})`); err == nil {
		t.Error("a non-callable Symbol.iterator should be a TypeError")
	}
	// Absent, it means the object is already an iterator.
	if v, err := rt.Eval(`
		var n = 0;
		Array.from(Iterator.from({
		  [Symbol.iterator]: undefined,
		  next: () => n < 2 ? {value: n++, done: false} : {done: true},
		})).join(",")`); err != nil {
		t.Fatal(err)
	} else if v.String() != "0,1" {
		t.Errorf("Iterator.from with no Symbol.iterator = %q", v.String())
	}
}

// An async generator awaits what it yields, and that await happens inside the
// generator, at the yield. A rejection is therefore a throw there: a try round
// the yield can catch it, and an uncaught one finishes the generator instead of
// leaving it suspended.
func TestAsyncGeneratorYieldRejection(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var log = [];
		  async function* gen() { yield Promise.reject(new Error("e")); yield "unreachable"; }
		  var it = gen();
		  it.next().then(() => log.push("resolved"), e => {
		    log.push("rejected:" + e.message);
		    it.next().then(r => log.push("done=" + r.done + " value=" + String(r.value)));
		  });`, "rejected:e | done=true value=undefined"},

		{`var log = [];
		  async function* gen() {
		    try { yield Promise.reject(new Error("e")); }
		    catch (x) { log.push("caught:" + x.message); yield "after"; }
		  }
		  gen().next().then(r => log.push("r1=" + String(r.value)));`,
			"caught:e | r1=after"},

		// A yielded promise that resolves gives the value, not the promise.
		{`var log = [];
		  async function* gen() { yield Promise.resolve(7); }
		  gen().next().then(r => log.push("v=" + r.value));`, "v=7"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		if _, err := rt.Eval(tc.src); err != nil {
			t.Errorf("%s: %v", tc.src, err)
			rt.Close()
			continue
		}
		v, err := rt.Eval(`log.join(" | ")`)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// A `for await` over a synchronous iterator awaits each value, and a value that
// rejects ends the iteration -- so the iterator is closed there, before the
// rejection is delivered, rather than when the loop finally unwinds.
func TestForAwaitOverSyncIterator(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var log = [];
		  function* g() { try { yield Promise.reject("r"); } finally { log.push("finally"); } }
		  (async () => {
		    try { for await (const x of g()); } catch (e) { log.push("caught:" + e); }
		  })();`, "finally | caught:r"},

		{`var log = [];
		  (async () => {
		    for await (const x of [Promise.resolve(1), 2]) log.push("x=" + x);
		  })();`, "x=1 | x=2"},

		// Symbol.asyncIterator present but not callable is a mistake rather
		// than an absence, so the synchronous protocol is not tried.
		{`var log = [];
		  async function* g() {
		    yield* {
		      [Symbol.asyncIterator]: false,
		      [Symbol.iterator]() { log.push("sync"); throw new Error("sync"); },
		    };
		  }
		  g().next().then(() => log.push("ok"), v => log.push("rej:" + v.constructor.name));`,
			"rej:TypeError"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		if _, err := rt.Eval(tc.src); err != nil {
			t.Errorf("%s: %v", tc.src, err)
			rt.Close()
			continue
		}
		v, err := rt.Eval(`log.join(" | ")`)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// TestLoopExitClosesIterators covers leaving a for-of loop by break, continue
// or a labelled jump. Every iterator between the jump and its target is closed,
// innermost first, and what the close reports is the result.
func TestLoopExitClosesIterators(t *testing.T) {
	cases := []struct{ src, want string }{
		// A labelled jump out of a nested loop closes both iterators.
		{`var log = []
		  function* g(n) { try { yield n; yield n } finally { log.push("c" + n) } }
		  outer: for (var a of g(1)) { for (var b of g(2)) { break outer } }
		  log.join(",")`, "c2,c1"},
		{`var log = []
		  function* g(n) { try { yield n; yield n } finally { log.push("c" + n) } }
		  outer: for (var a of g(1)) { for (var b of g(2)) { continue outer } }
		  log.join(",")`, "c2,c2,c1"},
		{`var r = 0
		  outer: for (var a of [1, 2]) { for (var b of [3, 4]) { r++; continue outer } }
		  String(r)`, "2"},
		// A break out of a switch inside a loop leaves the stack as it found it.
		{`var r = []
		  outer: for (var a of [1, 2]) { switch (a) { case 1: r.push("a"); break outer } }
		  r.join(",") + "|done"`, "a|done"},
		// Leaving a `with` by a labelled break takes the object off the chain.
		{`function f() { var o = {x: 1}; L: with (o) { break L } return typeof x }
		  f()`, "undefined"},

		// Closing on break is a normal completion: what the return method does
		// is the result of the loop.
		{`var it = {[Symbol.iterator]() { return {
		    next() { return {done: false, value: 1} },
		    return() { return 0 },
		  }}}
		  try { for (var x of it) break; "no throw" } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`var it = {[Symbol.iterator]() { return {
		    next() { return {done: false, value: 1} },
		    return() { throw new RangeError() },
		  }}}
		  try { for (var x of it) break; "no throw" } catch (e) { e.constructor.name }`,
			"RangeError"},
		{`var it = {[Symbol.iterator]() { return {
		    next() { return {done: false, value: 1} },
		    return: 1,
		  }}}
		  try { for (var x of it) break; "no throw" } catch (e) { e.constructor.name }`,
			"TypeError"},
		// A throw out of the body keeps its own completion, so the close's
		// failure is swallowed.
		{`var it = {[Symbol.iterator]() { return {
		    next() { return {done: false, value: 1} },
		    return() { throw new RangeError() },
		  }}}
		  try { for (var x of it) { throw new EvalError() } } catch (e) { e.constructor.name }`,
			"EvalError"},
		// An iterator with no return method simply ends.
		{`var it = {[Symbol.iterator]() { return {
		    next() { return {done: false, value: 1} },
		  }}}
		  try { for (var x of it) break; "no throw" } catch (e) { e.constructor.name }`,
			"no throw"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestReturnClosesThroughDestructuring covers a generator forced to return
// while it is suspended inside a destructuring pattern. The pattern's iterator
// is told, and what its return method reports is the result: unlike a throw, a
// return carries nothing that outranks it.
func TestReturnClosesThroughDestructuring(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var closed = 0
		  var it = {next() { return {done: false, value: undefined} },
		            return() { closed++; return {} }}
		  var able = {[Symbol.iterator]() { return it }}
		  function* g() { var a; [a = yield] = able }
		  var i = g(); i.next()
		  var r = i.return(9)
		  closed + "," + r.value + "," + r.done`, "1,9,true"},
		// A return method that hands back something other than an object is a
		// TypeError, which the caller of return() sees.
		{`var closed = 0
		  var it = {next() { return {done: false, value: undefined} },
		            return() { closed++; return null }}
		  var able = {[Symbol.iterator]() { return it }}
		  function* g() { var a; [a = yield] = able }
		  var i = g(); i.next()
		  var caught = "none"
		  try { i.return(9) } catch (e) { caught = e.constructor.name }
		  closed + "," + caught`, "1,TypeError"},
		// A finally clause still runs, on a throw completion rather than the
		// return the unwind was carrying.
		{`var log = []
		  var it = {next() { return {done: false, value: undefined} },
		            return() { return null }}
		  var able = {[Symbol.iterator]() { return it }}
		  function* g() { try { var a; [a = yield] = able } finally { log.push("f") } }
		  var i = g(); i.next()
		  var caught = "none"
		  try { i.return(9) } catch (e) { caught = e.constructor.name }
		  log.join() + "," + caught`, "f,TypeError"},

		// A throw keeps its own completion, so the close's failure is
		// swallowed.
		{`var it = {next() { return {done: false, value: undefined} },
		            return() { return null }}
		  var able = {[Symbol.iterator]() { return it }}
		  function* g() { var a; [a = yield] = able }
		  var i = g(); i.next()
		  try { i.throw(new RangeError()) } catch (e) { e.constructor.name }`, "RangeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}

	// A synchronous iterator whose value rejects is closed once, where the
	// rejection is noticed, and not again as it unwinds.
	checkAsync(t, `
		var closed = 0
		var src = {[Symbol.iterator]() { return {
		  next() { return {value: Promise.reject("reject"), done: false} },
		  return() { closed++ },
		}}}
		async function f() {
		  var out = "none"
		  try { for await (var _ of src); } catch (e) { out = String(e) }
		  return closed + "," + out
		}
		f().then(v => { r = v })`, "r", "1,reject")
}

// An async iterator's result is awaited as a whole and its value is handed on
// untouched; a synchronous one's result is a plain object whose value is what
// gets awaited. `yield*` is where the difference shows.
func TestAsyncDelegationAwaitsTheResultNotTheValue(t *testing.T) {
	// A promise an async iterator puts in its result is yielded as itself:
	// `yield*` passes on what the delegate produced without awaiting it again.
	checkAsync(t, `var r = "";
		var inner = Promise.resolve("unwrapped");
		var asyncIter = {
			[Symbol.asyncIterator]() { return this },
			next() { return {done: false, value: inner} },
			get return() { throw new Error("return should not be read") },
			get throw() { throw new Error("throw should not be read") },
		};
		async function* f() { yield* asyncIter }
		f().next().then(v => { r = String(v.value === inner) })`, `r`, "true")

	// A synchronous delegate is the other way round: its value is awaited, so
	// the loop sees what the promise settles to.
	checkAsync(t, `var r = "";
		async function* f() { yield* [Promise.resolve(1), 2] }
		(async () => { for await (const v of f()) r += v })()`, `r`, "12")

	// Whatever the delegate's throw returns is awaited whole, thenable or not,
	// before its done and value are read.
	checkAsync(t, `var log = []; var r = "";
		var obj = {
			[Symbol.asyncIterator]() {
				return {
					next() { return {value: "v1", done: false} },
					throw(arg) {
						log.push("throw " + arg);
						return {
							get then() {
								log.push("then");
								return function (resolve) {
									resolve({
										get done() { log.push("done"); return false },
										get value() { log.push("value"); return "tv" },
									});
								};
							},
						};
					},
				};
			},
		};
		async function* g() { yield* obj }
		var it = g();
		it.next()
			.then(() => it.throw("arg"))
			.then(v => { r = log.join(",") + "|" + v.value + "/" + v.done })`,
		`r`, "throw arg,then,done,value|tv/false")
}

// A result the iterator has handed over is its last word: if reading done or
// value from it throws, the iteration is over and the iterator is not asked to
// return. It was not the loop that gave up.
func TestIteratorResultErrorsDoNotClose(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var returnCount = 0, iterationCount = 0
		  var iterable = {}
		  iterable[Symbol.iterator] = function () {
		    return {
		      next: function () { return {done: false, get value() { throw new RangeError() }} },
		      return: function () { returnCount++; return {} }
		    }
		  }
		  var caught = ""
		  try { for (var x of iterable) { iterationCount++ } }
		  catch (e) { caught = e.constructor.name }
		  [caught, iterationCount, returnCount].join(",")`, "RangeError,0,0"},
		{`var returnCount = 0
		  var iterable = {}
		  iterable[Symbol.iterator] = function () {
		    return {
		      next: function () { return {get done() { throw new RangeError() }} },
		      return: function () { returnCount++; return {} }
		    }
		  }
		  var caught = ""
		  try { for (var x of iterable) {} } catch (e) { caught = e.constructor.name }
		  caught + "," + returnCount`, "RangeError,0"},
		// A loop that gives up early does close the iterator.
		{`var returnCount = 0
		  var iterable = {}
		  iterable[Symbol.iterator] = function () {
		    return {
		      next: function () { return {done: false, value: 1} },
		      return: function () { returnCount++; return {} }
		    }
		  }
		  for (var x of iterable) { break }
		  String(returnCount)`, "1"},
		{`var returnCount = 0
		  var iterable = {}
		  iterable[Symbol.iterator] = function () {
		    return {
		      next: function () { return {done: false, value: 1} },
		      return: function () { returnCount++; return {} }
		    }
		  }
		  try { for (var x of iterable) { throw new RangeError() } } catch (e) {}
		  String(returnCount)`, "1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A synchronous iterator driven asynchronously still has to hand back a result
// object; anything else is a TypeError, which reaches the caller as a rejection
// rather than a throw.
func TestAsyncDelegationRequiresResultObjects(t *testing.T) {
	checkAsync(t, `
		var obj = {}
		obj[Symbol.iterator] = function () {
		  return {
		    next: function () { return {value: 1, done: false} },
		    return: function () { return 1 }
		  }
		}
		async function* asyncg() { yield* obj }
		var iter = asyncg()
		var r = ""
		iter.next().then(function () {
		  iter.return().then(function (res) { r = "resolved:" + res.value },
		    function (e) { r = "rejected:" + e.constructor.name })
		})`, `r`, "rejected:TypeError")
	checkAsync(t, `
		var obj = {}
		obj[Symbol.iterator] = function () {
		  return {
		    next: function () { return {value: 1, done: false} },
		    throw: function () { return 1 }
		  }
		}
		async function* asyncg() { yield* obj }
		var iter = asyncg()
		var r = ""
		iter.next().then(function () {
		  iter.throw(new Error("x")).then(function () { r = "resolved" },
		    function (e) { r = "rejected:" + e.constructor.name })
		})`, `r`, "rejected:TypeError")
	// A delegate that finishes hands its own value back through the yield*.
	checkAsync(t, `
		var obj = {}
		obj[Symbol.iterator] = function () {
		  return {
		    next: function () { return {value: 1, done: false} },
		    return: function () { return {value: 9, done: true} }
		  }
		}
		async function* asyncg() { yield* obj }
		var iter = asyncg()
		var r = ""
		iter.next().then(function () {
		  iter.return(5).then(function (res) { r = res.value + ":" + res.done },
		    function (e) { r = "rejected:" + e.constructor.name })
		})`, `r`, "9:true")
}
