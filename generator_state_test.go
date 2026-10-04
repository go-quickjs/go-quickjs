package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestSuspendedFrameState pins that a suspended generator or async function
// keeps its catch handlers and captured variables while other calls run at
// the depth it ran at. The frame it ran in is reused by those calls, and used
// to share its handler and upvalue arrays with them (KI-01).
func TestSuspendedFrameState(t *testing.T) {
	// An async function awaiting inside try, while a function with a try of
	// its own runs where it did.
	checkAsync(t, `
		var log = [];
		async function f() {
			try { await null; throw new Error("x"); } catch (e) { log.push("caught " + e.message); }
		}
		f();
		function h() { let a = 1, b = 2, c = 3; try { a++; } catch (e) {} return a + b + c; }
		h();
		Promise.resolve().then(() => log.push("then"));`,
		`log.join()`, "caught x,then")

	// A generator thrown into after another function used its depth for try
	// and finally.
	checkEval(t, `
		function* g() { try { yield 1; } catch (e) { return "caught " + e; } return "fell through"; }
		var it = g();
		it.next();
		function a() { return b(); }
		function b() {
			var s = 0;
			try { s += 1; } finally { s += 2; }
			try { s += 10; throw 5; } catch (x) { s += x; }
			return s;
		}
		a();
		JSON.stringify(it.throw("boom"))`, `{"value":"caught boom","done":true}`)

	// Closures over a loop's per-iteration binding across yields, with
	// another function's closure made between resumptions.
	checkEval(t, `
		var fns = [];
		function* g() { for (let i = 0; i < 3; i++) { fns.push(() => i); yield; } }
		function h() { let z = 42; var k = () => z; return k(); }
		function w() { return h(); }
		var it = g();
		it.next(); w(); it.next(); w(); it.next(); w(); it.next();
		fns.map(f => f()).join()`, "0,1,2")
}

// TestResumedFramesAreSetUp pins that a generator's and an async function's
// frames have what an ordinary call's has: somewhere for a direct eval's vars,
// which went onto the global object; and in an async arrow in a derived
// constructor, the constructor's `this` binding and new.target, which the
// arrow lost at its first await (KI-29). The answers are node's.
func TestResumedFramesAreSetUp(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`
		var out = [];
		function* g() { eval("var x = 1"); yield typeof x; }
		out.push([...g()].join(), "x" in globalThis);
		async function af() { eval("var y = 2"); await 0; return typeof y; }
		af().then(v => out.push("async " + v + " " + ("y" in globalThis)));
		class B { constructor() { this.b = 1; } }
		class D extends B {
			constructor() {
				let before;
				const f = async () => {
					try { this; before = "bound" } catch (e) { before = e.constructor.name }
					await 0;
					super();
					await 0;
					return [before, this.b, new.target === D].join();
				};
				f().then(v => out.push("derived " + v), e => out.push("derived threw " + e));
				this.q = 1;
			}
		}
		try { new D() } catch (e) { out.push("ctor " + e.constructor.name) }
		class E extends B {
			constructor() {
				const f = async () => { await 0; return new.target === E; };
				super();
				f().then(v => out.push("newTarget " + v));
			}
		}
		new E();`); err != nil {
		t.Fatal(err)
	}
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
	want := "number | false | ctor ReferenceError | async number false | newTarget true | derived ReferenceError,1,true"
	if got := evalString(t, rt, `out.join(" | ")`); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestGeneratorsSuspendingInTurn covers generators that resume others, and
// async generators awaiting between their yields: each suspension is read
// from the runtime's one signal before the next can be made, which a
// generator suspending inside another's resumption, yield*, and an await
// in an async generator's body must not upset. The answers are Node's.
func TestGeneratorsSuspendingInTurn(t *testing.T) {
	checkAsync(t, `function* inner(k) { var x = yield k + "a"; yield k + "b" + x; return k + "r" }
		function* outer() { var it = inner("i"); var r1 = it.next(); var y = yield r1.value; var r2 = it.next(y); yield r2.value + "|" + JSON.stringify(it.next()); var d = yield* inner("d"); yield d }
		var out = [], g = outer(), r, n = 0;
		while (!(r = g.next("s" + n++)).done) out.push(r.value);
		function* fib() { var a = 0, b = 1; for (;;) { yield a; [a, b] = [b, a + b] } }
		var f = [], it = fib(); for (var i = 0; i < 10; i++) f.push(it.next().value);
		out.push(f.join(), [...(function* () { yield* [1, 2]; yield* (function* () { yield 3 })() })()].join());
		var log = [];
		async function* ag() { var x = await 1; yield x; var y = await Promise.resolve(2); yield y + (yield 3) }
		(async () => { var a = ag(); log.push(JSON.stringify(await a.next()), JSON.stringify(await a.next()), JSON.stringify(await a.next()), JSON.stringify(await a.next(4)), JSON.stringify(await a.next())) })()`,
		`out.join(" ; ") + " ; " + log.join(" ")`,
		`ia ; ibs1|{"value":"ir","done":true} ; da ; dbs3 ; dr ; 0,1,1,2,3,5,8,13,21,34 ; 1,2,3 ; {"value":1,"done":false} {"value":3,"done":false} {"value":null,"done":false} {"done":true} {"done":true}`)
}
