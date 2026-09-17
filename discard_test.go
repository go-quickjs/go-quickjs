package quickjs_test

import "testing"

// An expression whose value is discarded is compiled without the instruction
// that discards it: a store to a local becomes a store-and-pop, and a postfix
// update drops the copy of the old value that nothing reads. Neither may change
// what the expression does -- only what it leaves behind.

// TestDiscardedUpdates covers ++ and -- where the value is thrown away, which
// is compiled as the prefix form.
func TestDiscardedUpdates(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var i = 0; i++; i`, "1"},
		{`var i = 0; i--; i`, "-1"},
		{`var i = 0; for (var n = 0; n < 3; i++) n++; i`, "3"},
		// The coercion happens either way, so what is left behind is a number.
		{`var s = "5"; s++; typeof s + ":" + s`, "number:6"},
		{`var b = 1n; b++; typeof b + ":" + b`, "bigint:2"},
		{`var o = {valueOf() { return 7 }}; o++; o`, "8"},
		// A value that cannot be coerced still throws.
		{`try { var x = Symbol(); x++; "no throw" } catch (e) { e.constructor.name }`,
			"TypeError"},
		{`try { var x = {toString() { throw new RangeError() }, valueOf: null }; x++ }
		  catch (e) { e.constructor.name }`, "RangeError"},

		// A member target: the getter and the setter run the same way round.
		{`var log = []
		  var o = {_v: 1, get v() { log.push("get"); return this._v },
		           set v(x) { log.push("set " + x); this._v = x }}
		  o.v++
		  log.join("|") + "=" + o._v`, "get|set 2=2"},
		{`var o = {a: {b: 1}}; o.a.b++; o.a.b`, "2"},
		{`var k = 0, o = [10]
		  o[k++]++
		  [o[0], k].join()`, "11,1"},
		// A private field, which is reached through its own accessors.
		{`class C { #n = 1; bump() { this.#n++; return this.#n } }
		  new C().bump()`, "2"},
		// Through a with object, where the name is resolved once.
		{`var o = {x: 1}; with (o) { x++ }; o.x`, "2"},
		// A captured variable, which lives in an upvalue rather than a slot.
		{`var fns = []
		  for (let i = 0; i < 3; i++) fns.push(() => i)
		  fns.map(f => f()).join()`, "0,1,2"},
		{`function outer() { var n = 0; function bump() { n++ }; bump(); bump(); return n }
		  outer()`, "2"},
		// A global, which is a property of the global object.
		{`g = 1; g++; g`, "2"},

		// An update whose value is not discarded keeps its old-value semantics.
		{`var i = 0; var a = i++; [a, i].join()`, "0,1"},
		{`var i = 0; var a = ++i; [a, i].join()`, "1,1"},
		{`var i = 0; (i++, i++); i`, "2"},
		{`var i = 0; var r = (i++, i++); [r, i].join()`, "1,2"},
		{`var i = 0; var a = [i++, i++]; [a.join(), i].join(":")`, "0,1:2"},

		// An update that must fail still fails.
		{`try { const c = 1; c++; "no throw" } catch (e) { e.constructor.name }`, "TypeError"},
		{`"use strict"
		  try { undeclaredName++; "no throw" } catch (e) { e.constructor.name }`,
			"ReferenceError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestDiscardedAssignments covers the store that pops its own value, and the
// places where it may not: a short-circuiting assignment reaches the end of the
// statement without having stored anything.
func TestDiscardedAssignments(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var x = 0; x = 5; x`, "5"},
		{`var x = 1; x += 2; x`, "3"},
		{`var x = 1; for (var i = 0; i < 3; i++) x *= 2; x`, "8"},

		// Logical assignment stores only when the short circuit does not take.
		{`var x = 0; x ||= 7; x`, "7"},
		{`var x = 5; x ||= 7; x`, "5"},
		{`var x = 1; x &&= 7; x`, "7"},
		{`var x = 0; x &&= 7; x`, "0"},
		{`var x = null; x ??= 7; x`, "7"},
		{`var x = 0; x ??= 7; x`, "0"},
		{`var x = 0; for (var i = 0; i < 3; i++) x ||= i; x`, "1"},
		{`var x = 0, n = 0; while (n++ < 3) x ||= n; x`, "1"},
		// The same inside a comma expression, where the operand is discarded
		// too.
		{`var x = 5, y = 0; (x ||= 9, y = x); y`, "5"},

		// A conditional whose arms both assign: one path reaches the end of the
		// statement without going through the other's store.
		{`var a = 0, b = 0; (1 ? (a = 1) : (b = 2)); [a, b].join()`, "1,0"},
		{`var a = 0, b = 0; (0 ? (a = 1) : (b = 2)); [a, b].join()`, "0,2"},
		{`var x = 0; if (1) x = 3; x`, "3"},
		{`var x = 0; if (0) x = 3; x`, "0"},
		{`var x = 0, c = 1; while (c--) x = 9; x`, "9"},
		{`var x = 0; do { x = 1 } while (0); x`, "1"},
		{`var x = 0; try { x = 1 } catch (e) { x = 2 }; x`, "1"},
		{`var x = 0; try { throw 1 } catch (e) { x = 2 } finally { x += 1 }; x`, "3"},
		{`var x = 0; lbl: for (var i = 0; i < 3; i++) { if (i === 1) continue lbl; x += 10 }; x`,
			"20"},

		// A destructuring assignment statement, which leaves the object rather
		// than a stored value.
		{`var a, b; ({a, b} = {a: 1, b: 2}); [a, b].join()`, "1,2"},
		{`var a, b; [a, b] = [1, 2]; [a, b].join()`, "1,2"},

		// An assignment to a member or a global, neither of which stores into a
		// frame slot.
		{`var o = {}; o.x = 1; o.x`, "1"},
		{`gg = 4; gg`, "4"},
		{`var o = {set v(x) { this.got = x }}; o.v = 3; o.got`, "3"},

		// An assignment in a for's update clause, and one in its init.
		{`var x = 0; for (var i = 0; i < 3; x += i, i++); x`, "3"},
		{`var s = ""; for (var i = 0, j = 2; i < j; i++) s += i; s`, "01"},

		// A mapped argument stays mapped: assigning to the parameter is visible
		// through the arguments object, and the other way round.
		{`function f(a) { a = 9; return arguments[0] }; f(1)`, "9"},
		{`function f(a) { a++; return arguments[0] }; f(1)`, "2"},
		{`function f(a) { arguments[0] = 5; return a }; f(1)`, "5"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestCompletionValuesSurviveDiscard covers what eval returns, which is the
// value of the last statement that produced one -- and so is exactly the value
// the discarding rewrites must not be applied to.
func TestCompletionValuesSurviveDiscard(t *testing.T) {
	cases := []struct{ src, want string }{
		{`eval("var i = 0; i++")`, "0"},
		{`eval("var i = 0; ++i")`, "1"},
		{`eval("var i = 5; i--")`, "5"},
		{`eval("var x = 0; x = 7")`, "7"},
		{`eval("var x = 1; x += 2")`, "3"},
		{`eval("var x = 0; x ||= 4")`, "4"},
		{`eval("var x = 5; x ||= 4")`, "5"},
		{`eval("var i = 0; { i++ }")`, "0"},
		{`eval("var i = 0; if (1) i++")`, "0"},
		{`eval("var i = 0; for (var n = 0; n < 2; n++) i++")`, "1"},
		{`String(eval("var i = 0; for (var n = 0; n < 2; n++) ;"))`, "undefined"},
		{`eval("var i = 0, o = {}; o.x = i++")`, "0"},
		{`eval("var i = 0; (i++, i++)")`, "1"},
		// A nested eval, whose completion value is the outer one's.
		{`eval("eval('var i = 0; i++')")`, "0"},
		// The Function constructor compiles a body the same way a function
		// declaration does, where nothing is a completion value.
		{`var f = new Function("var i = 0; i++; return i"); f()`, "1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
