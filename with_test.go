package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Inside a `with` body an unqualified name may resolve to a property of the
// object rather than to any binding, and which it is depends on what the object
// holds at the moment the name is evaluated. Nothing else in the language works
// that way, which is why it is forbidden in strict mode.
func TestWithStatement(t *testing.T) {
	cases := []struct{ src, want string }{
		// The object shadows an outer binding, and only while it has the name.
		{`var o = {x: 1}; var x = 2; with (o) { var r = x } String(r)`, "1"},
		{`var x = 1; with ({}) { var r = x } String(r)`, "1"},
		{`var o = {x: 1}; var x = 2; with (o) { x = 5 } [o.x, x].join(",")`, "5,2"},
		{`var o = {}; var x = 2; with (o) { x = 5 } [o.x, x].join(",")`, ",5"},

		// Every form of access goes through the object.
		{`var o = {x: 1}; with (o) { var t = typeof x } t`, "number"},
		{`with ({}) { var t = typeof nowhere } t`, "undefined"},
		{`var o = {x: 1}; with (o) { var d = delete x } [d, o.x].join(",")`, "true,"},
		{`var o = {x: 1}; with (o) { x += 2 } String(o.x)`, "3"},
		{`var o = {x: 1}; with (o) { x++ } String(o.x)`, "2"},
		// A call through the object has the object as its receiver.
		{`var o = {f() { return this === o }}; with (o) { var r = f() } String(r)`, "true"},
		{`function f() { return this === undefined || this === globalThis }
		  with ({}) { var r = f() } String(r)`, "true"},

		// The object is consulted once per evaluation, not cached.
		{`var n = 0; var o = {get x() { n++; return 1 }};
		  with (o) { x; x; } String(n)`, "2"},
		// And a name can appear part-way through.
		{`var o = {}; var x = "outer";
		  with (o) { var a = x; o.x = "inner"; var b = x } [a, b].join(",")`,
			"outer,inner"},

		// A function written inside a body resolves its own names against the
		// objects too, and keeps them after the body is done.
		{`var o = {x: 1}; with (o) { var g = function () { return x } } String(g())`, "1"},
		{`var o = {x: 1}; var g; with (o) { g = () => x } o.x = 7; String(g())`, "7"},

		// Nesting: the innermost object wins.
		{`with ({x: 1}) { with ({x: 2}) { var r = x } } String(r)`, "2"},
		{`with ({x: 1}) { with ({y: 2}) { var r = x } } String(r)`, "1"},

		// Symbol.unscopables is how an object hides a name, which is what lets
		// the language keep adding array methods without breaking code that
		// uses the same name.
		{`var a = [1]; with (a) { var r = typeof values } r`, "undefined"},
		{`var values = 1; var a = [1]; with (a) { var r = values } String(r)`, "1"},
		{`var a = [1]; with (a) { var r = length } String(r)`, "1"},
		{`var a = [1]; with (a) { var r = typeof join } r`, "function"},
		{`var o = {x: 1, [Symbol.unscopables]: {x: true}}; var x = 9;
		  with (o) { var r = x } String(r)`, "9"},
		{`var o = {x: 1, [Symbol.unscopables]: {x: false}}; var x = 9;
		  with (o) { var r = x } String(r)`, "1"},

		// A primitive is boxed, as everywhere else properties are read.
		{`with ("abc") { var r = length } String(r)`, "3"},
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

	bad := []struct{ src, want string }{
		{`with (null) {}`, "TypeError"},
		{`with (undefined) {}`, "TypeError"},
		// Strict mode has no `with` at all.
		{`"use strict"; with ({}) {}`, "SyntaxError"},
		{`function f() { "use strict"; with ({}) {} }`, "SyntaxError"},
		{`class C { m() { with ({}) {} } }`, "SyntaxError"},
	}
	for _, tc := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(tc.src); err == nil {
			t.Errorf("%s: accepted, want %s", tc.src, tc.want)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %s", tc.src, err, tc.want)
		}
		rt.Close()
	}
}

// A `with` object shadows the bindings outside it and not those inside it: a
// variable declared in the body, or anywhere in a function written there, wins.
func TestWithShadowing(t *testing.T) {
	cases := []struct{ src, want string }{
		// A var hoists out of the body, so its initializer is an ordinary
		// assignment and goes to the object.
		{`var o = {value: "obj"}; with (o) { var value = "set" }
		  [o.value, value].join(",")`, "set,"},
		// In a nested function the var belongs to the function, which is
		// inside the body, so the object is not written to.
		{`var o = {value: "obj"};
		  with (o) { var f = function () { var value = "set"; return value } }
		  [f(), o.value].join(",")`, "set,obj"},
		// A let in the body is inside it too.
		{`var o = {x: "obj"}; var r;
		  with (o) { let x = "let"; r = x } [r, o.x].join(",")`, "let,obj"},
		// And a parameter of a function written in the body.
		{`var o = {x: "obj"}; var f;
		  with (o) { f = function (x) { return x } } f("param")`, "param"},

		// Between two objects, the inner one shadows a binding declared
		// between them and the outer one does not.
		{`var r; with ({x: "outer"}) { let x = "let"; with ({y: 1}) { r = x } } r`, "let"},
		{`var r; with ({x: "outer"}) { let x = "let"; with ({x: "inner"}) { r = x } } r`,
			"inner"},

		// A named function expression's own name is its own binding.
		{`var o = {f: 1}; var g = function f() { return typeof f };
		  with (o) { var r = g() } r`, "function"},
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

// A generator may suspend inside a with body, and when it resumes the body
// still has to resolve names against the object. The scope chain lives on the
// frame, which a suspension discards, so it has to be saved with everything
// else -- and until it was, resuming past the end of the body underflowed the
// chain and took the host down.
func TestWithSurvivesSuspension(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function* g() { var x = 1; yield x; with ({x: 2}) { yield x } yield x }
		  var it = g();
		  [it.next().value, it.next().value, it.next().value, it.next().done].join(",")`,
			"1,2,1,true"},
		// Two objects deep, and a yield in each.
		{`function* g() {
		    with ({x: 1}) { yield x; with ({x: 2}) { yield x } yield x }
		  }
		  var it = g(); [...it].join(",")`, "1,2,1"},
		// A generator created inside a with body sees the object from its
		// declaration, not from where it is driven.
		{`var g; with ({x: "obj"}) { g = function* () { yield x } }
		  g().next().value`, "obj"},
		// yield* delegating from inside a with body.
		{`function* g() { with ({x: [1, 2]}) { yield* x } }
		  [...g()].join(",")`, "1,2"},
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

// The same for an async function, whose awaits are suspensions of the same
// machinery.
func TestWithSurvivesAwait(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	v, err := rt.Eval(`
	    var log = [];
	    async function a() {
	      with ({x: 1}) {
	        log.push(x);
	        await 0;
	        log.push(x);
	        with ({x: 2}) { await 0; log.push(x) }
	        log.push(x);
	      }
	    }
	    a();
	    log`)
	if err != nil {
		t.Fatal(err)
	}
	// The awaits resolve as the job queue drains, which Eval does before it
	// returns.
	if got := v.String(); got != "1,1,2,1" {
		t.Errorf("log = %q, want %q", got, "1,1,2,1")
	}
}

// TestWithReferenceResolvedOnce covers a compound assignment or an update
// inside a `with` body. The name is resolved once, and the write goes back to
// whatever the read found -- even if the read removed it.
func TestWithReferenceResolvedOnce(t *testing.T) {
	cases := []struct{ src, want string }{
		// The getter deletes the property, so a second resolution would find
		// the function's own binding instead of the object.
		{`function t() {
		    var x = 0
		    var scope = {get x() { delete this.x; return 2 }}
		    with (scope) { x ^= 3 }
		    return scope.x + "," + x
		  }
		  t()`, "1,0"},
		{`function t() {
		    var x = 0
		    var scope = {get x() { delete this.x; return 2 }}
		    with (scope) { x++ }
		    return scope.x + "," + x
		  }
		  t()`, "3,0"},
		{`function t() {
		    var x = 0
		    var scope = {get x() { delete this.x; return 2 }}
		    with (scope) { x &&= 9 }
		    return scope.x + "," + x
		  }
		  t()`, "9,0"},

		// A strict reference to a binding the object no longer has is a
		// ReferenceError rather than a property being created.
		{`var scope = {get x() { delete this.x; return 2 }}
		  var caught = ""
		  with (scope) {
		    (function () {
		      "use strict"
		      try { x ^= 3 } catch (e) { caught = e.constructor.name }
		    })()
		  }
		  caught + "," + ("x" in scope)`, "ReferenceError,false"},

		// The ordinary cases still read and write the object.
		{`function t() {
		    var x = 5, s = {x: 1}
		    with (s) { x += 2 }
		    return s.x + "," + x
		  }
		  t()`, "3,5"},
		{`function t() {
		    var x = 5
		    with ({}) { x += 2; x++ }
		    return x
		  }
		  t()`, "8"},
		{`function t() {
		    var x = 0, s = {x: 1}
		    var post, pre
		    with (s) { post = x++; pre = ++x }
		    return [post, pre, s.x, x].join(",")
		  }
		  t()`, "1,3,3,0"},
		// The postfix result is the coerced value, not the original string.
		{`function t() {
		    var s = {x: "1"}
		    var r
		    with (s) { r = x++ }
		    return typeof r + "," + r + "," + s.x
		  }
		  t()`, "number,1,2"},
		// A const in the enclosing scope is still a const when no `with`
		// object answers.
		{`var caught = ""
		  function t() {
		    const x = 1
		    with ({}) { try { x += 1 } catch (e) { caught = e.constructor.name } }
		  }
		  t(); caught`, "TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestWithAssignmentResolvesFirst covers a plain assignment inside a `with`
// body. The name is resolved before the value is evaluated, so a value that
// removes the property still writes to the object the name named.
func TestWithAssignmentResolvesFirst(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function t() {
		    var x = 0
		    var scope = {x: 1}
		    with (scope) { x = (delete scope.x, 2) }
		    return scope.x + "," + x
		  }
		  t()`, "2,0"},
		{`function t() {
		    var x = 0, s = {x: 1}
		    with (s) { x = 5 }
		    return s.x + "," + x
		  }
		  t()`, "5,0"},
		// A name the object does not have still lands where it would have.
		{`function t() {
		    var x = 0, s = {}
		    with (s) { x = 5 }
		    return String(s.x) + "," + x
		  }
		  t()`, "undefined,5"},
		// A strict reference to a binding the object no longer has is a
		// ReferenceError.
		{`var scope = {x: 1}
		  var caught = ""
		  with (scope) {
		    (function () {
		      "use strict"
		      try { x = (delete scope.x, 2) } catch (e) { caught = e.constructor.name }
		    })()
		  }
		  caught + "," + ("x" in scope)`, "ReferenceError,false"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
