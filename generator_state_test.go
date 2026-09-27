package quickjs_test

import "testing"

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
