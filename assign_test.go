package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A compound assignment to a member evaluates the object and the key once. They
// are expressions: base[prop] *= f() must call prop.toString once, and reading
// the property and writing it back have to address the same place even if the
// read changed what is there.
func TestCompoundAssignmentEvaluatesOnce(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var n = 0; var p = {toString() { n++; return "x" }}; var b = {x: 2};
		  b[p] *= 3; [b.x, n].join(",")`, "6,1"},
		{`var n = 0; var p = {toString() { n++; return "x" }}; var b = {x: 2};
		  b[p] ??= 9; [b.x, n].join(",")`, "2,1"},
		{`var n = 0; var p = {toString() { n++; return "y" }}; var b = {};
		  b[p] ??= 9; [b.y, n].join(",")`, "9,1"},
		{`var n = 0; var o = {};
		  Object.defineProperty(o, "x", {get() { n++; return 1 }, set(v) {}});
		  o.x += 1; String(n)`, "1"},
		// The object expression is evaluated once too.
		{`var n = 0; function f() { n++; return {x: 1} } f().x += 1; String(n)`, "1"},

		{`var o = {a: 2}; o.a += 3; String(o.a)`, "5"},
		{`var o = {a: 2}; o.a ||= 9; String(o.a)`, "2"},
		{`var o = {a: 0}; o.a ||= 9; String(o.a)`, "9"},
		{`var o = {a: null}; o.a ??= 9; String(o.a)`, "9"},
		{`var o = {a: 1}; String(o.a ||= 2)`, "1"},
		{`var o = {a: null}; String(o.a ??= 2)`, "2"},
		{`var a = [1, 2]; a[0] += 5; a.join(",")`, "6,2"},

		{`class C { #v = 1; bump() { this.#v += 2; return this.#v } }
		  String(new C().bump())`, "3"},
		{`class C { #v = 0; f() { this.#v ||= 5; return this.#v } }
		  String(new C().f())`, "5"},

		// And the plain-identifier forms are unchanged.
		{`var x = 1; x += 2; String(x)`, "3"},
		{`var x = 0; x ||= 7; String(x)`, "7"},
		{`var x = 1; x &&= 7; String(x)`, "7"},
		{`var x; x ??= 7; String(x)`, "7"},
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

// TestParenthesizedTargetHasNoName covers naming an anonymous function after
// the variable it is assigned to, which a parenthesised target does not do:
// parentheses stop it being an identifier reference.
func TestParenthesizedTargetHasNoName(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var fn; (fn) = function () {}; JSON.stringify(fn.name)`, `""`},
		{`var fn; (fn) = () => {}; JSON.stringify(fn.name)`, `""`},
		{`var fn; (fn) = class {}; JSON.stringify(fn.name)`, `""`},
		{`var fn; fn = function () {}; JSON.stringify(fn.name)`, `"fn"`},
		{`var fn = function () {}; JSON.stringify(fn.name)`, `"fn"`},
		{`let fn = class {}; JSON.stringify(fn.name)`, `"fn"`},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestDuplicateProtoInPattern covers two __proto__ properties, which are an
// error in an object literal -- each would set the prototype and the second
// would silently win -- but not in a destructuring pattern, where each is
// simply a place to assign to.
func TestDuplicateProtoInPattern(t *testing.T) {
	cases := []struct{ src, want string }{
		{`try { eval("({__proto__: 1, __proto__: 2})") } catch (e) { e.constructor.name }`,
			"SyntaxError"},
		{`var value = Object.defineProperty({}, "__proto__", {value: 123})
		  var x, y
		  var r = ({__proto__: x, __proto__: y} = value)
		  x + "," + y + "," + (r === value)`, "123,123,true"},
		{`var o = {__proto__: null}; String(Object.getPrototypeOf(o))`, "null"},
		{`var p = {}; var o = {__proto__: p}; String(Object.getPrototypeOf(o) === p)`, "true"},
		// A computed or shorthand key is an ordinary property, so two are fine.
		{`var k = "__proto__"
		  var o = {[k]: 1, [k]: 2}
		  o.__proto__ + "," + String(Object.getPrototypeOf(o) === Object.prototype)`,
			"2,true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// The parts of `a[k] = v` are evaluated left to right, and the conversions the
// assignment needs come after all of them: the key becomes a property key and
// the base is checked only once there is a value to store.
func TestAssignmentToComputedMemberOrder(t *testing.T) {
	cases := []struct{ name, src, want string }{
		// The key expression runs before the right-hand side.
		{"key before value", `var log = []
		  var base = {}
		  base[(log.push("key"), "k")] = (log.push("value"), 1)
		  log.join(",")`, "key,value"},
		// ToPropertyKey runs after it, which is what makes a throwing toString
		// lose to a throwing right-hand side.
		{"key conversion last", `var log = []
		  var base = {}
		  var key = {toString: function () { log.push("toString"); return "k" }}
		  base[key] = (log.push("value"), 1)
		  log.join(",") + "|" + base.k`, "value,toString|1"},
		// So does the check that there is something to assign to.
		{"base checked last", `var log = []
		  try { null[(log.push("key"), "k")] = (log.push("value"), 1) }
		  catch (e) { log.push(e.constructor.name) }
		  log.join(",")`, "key,value,TypeError"},
		{"undefined base", `var log = []
		  var key = {toString: function () { log.push("toString"); return "k" }}
		  try { undefined[key] = (log.push("value"), 1) }
		  catch (e) { log.push(e.constructor.name) }
		  log.join(",")`, "value,TypeError"},
		// A compound assignment reads through the reference first, so there the
		// conversions happen before the operand is evaluated -- and only once.
		{"compound converts once", `var log = []
		  var o = {k: 1}
		  var key = {toString: function () { log.push("toString"); return "k" }}
		  o[key] += (log.push("operand"), 2)
		  log.join(",") + "|" + o.k`, "toString,operand|3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}

// A super reference whose home object has no prototype has a null base, which
// is an error only when the assignment actually happens -- after the key and
// the right-hand side have been evaluated.
func TestSuperAssignmentWithoutPrototype(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"named", `var count = 0
		  class C { static m() { super.x = count += 1 } }
		  Object.setPrototypeOf(C, null)
		  var caught = ""
		  try { C.m() } catch (e) { caught = e.constructor.name }
		  caught + "," + count`, "TypeError,1"},
		{"computed", `var count = 0
		  class C { static m() { super[0] = count += 1 } }
		  Object.setPrototypeOf(C, null)
		  var caught = ""
		  try { C.m() } catch (e) { caught = e.constructor.name }
		  caught + "," + count`, "TypeError,1"},
		{"read", `class C { static m() { return super.x } }
		  Object.setPrototypeOf(C, null)
		  try { C.m(); "no error" } catch (e) { e.constructor.name }`, "TypeError"},
		{"computed read", `var count = 0
		  class C { static m() { return super[(count += 1, "x")] } }
		  Object.setPrototypeOf(C, null)
		  var caught = ""
		  try { C.m() } catch (e) { caught = e.constructor.name }
		  caught + "," + count`, "TypeError,1"},
		// With a prototype the assignment lands on the receiver, as always.
		{"with a prototype", `class A {}
		  class B extends A { static m() { super.x = 5 } }
		  B.m(); B.x + "," + String(Object.getPrototypeOf(B).x)`, "5,undefined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}
