package quickjs_test

import "testing"

// An ordinary function's .prototype is built when something first asks for it,
// which nothing may be able to tell. These pin what has to look the same as it
// would have had the object been there from the start.

// TestFunctionPrototypeIsOrdinary covers what the property is: an own data
// property, writable but neither enumerable nor configurable, holding an object
// that points back at the function.
func TestFunctionPrototypeIsOrdinary(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function f() {}; typeof f.prototype`, "object"},
		{`function f() {}; f.prototype.constructor === f`, "true"},
		{`function f() {}; Object.getPrototypeOf(f.prototype) === Object.prototype`, "true"},
		// The same object every time, which is what makes it usable at all.
		{`function f() {}; f.prototype === f.prototype`, "true"},
		{`function f() {}; var p = f.prototype; p.x = 1; f.prototype.x`, "1"},
		// Two functions do not share one.
		{`function f() {}; function g() {}; f.prototype === g.prototype`, "false"},

		{`function f() {}
		  var d = Object.getOwnPropertyDescriptor(f, "prototype");
		  [d.writable, d.enumerable, d.configurable].join()`, "true,false,false"},
		{`function f() {}; f.hasOwnProperty("prototype")`, "true"},
		{`function f() {}; "prototype" in f`, "true"},
		{`function f() {}; Object.keys(f).length`, "0"},
		{`function f() {}; var seen = []; for (var k in f) seen.push(k); seen.length`, "0"},

		// It is where `new` looks, and what instanceof walks to.
		{`function f() {}; Object.getPrototypeOf(new f()) === f.prototype`, "true"},
		{`function f() {}; new f() instanceof f`, "true"},
		{`function f() {}; f.prototype.m = function () { return 7 }; new f().m()`, "7"},
		{`function f() {}; var p = {}; f.prototype = p
		  Object.getPrototypeOf(new f()) === p`, "true"},

		// A function that is not constructible has none.
		{`String((() => {}).prototype)`, "undefined"},
		{`String(({m() {}}).m.prototype)`, "undefined"},
		{`String(Object.getOwnPropertyDescriptor({m() {}}.m, "prototype"))`, "undefined"},
		{`var d = Object.getOwnPropertyDescriptor({get g() { return 1 }}, "g")
		  String(d.get.prototype)`, "undefined"},
		// A generator has one, though it is not a constructor.
		{`function* g() {}; typeof g.prototype`, "object"},
		{`function* g() {}; Object.getOwnPropertyDescriptor(g, "prototype").configurable`,
			"false"},
		{`async function a() {}; String(a.prototype)`, "undefined"},
		// A class's cannot be replaced.
		{`class C {}; Object.getOwnPropertyDescriptor(C, "prototype").writable`, "false"},
		{`class C {}; C.prototype.constructor === C`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestFunctionPrototypeOrder covers where the property sits in the table, which
// an ownKeys walk reports: after length and name, and before anything a script
// added to the function since.
func TestFunctionPrototypeOrder(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function f() {}; Object.getOwnPropertyNames(f).join()`, "length,name,prototype"},
		{`function f() {}; f.prototype; Object.getOwnPropertyNames(f).join()`,
			"length,name,prototype"},
		{`function f() {}; f.x = 1; Object.getOwnPropertyNames(f).join()`,
			"length,name,prototype,x"},
		{`function f() {}; f.x = 1; f.prototype; Object.getOwnPropertyNames(f).join()`,
			"length,name,prototype,x"},
		{`function f() {}; f.prototype; f.x = 1; Object.getOwnPropertyNames(f).join()`,
			"length,name,prototype,x"},
		// Reading name first materializes it, which must not push prototype in
		// front of it.
		{`function f() {}; f.name; f.x = 1; f.prototype
		  Object.getOwnPropertyNames(f).join()`, "length,name,prototype,x"},
		{`function f() {}; Object.defineProperty(f, "name", {value: "z"})
		  f.x = 1
		  Object.getOwnPropertyNames(f).join()`, "length,name,prototype,x"},
		{`function f() {}; Reflect.ownKeys(f).join()`, "length,name,prototype"},
		{`function f() {}; f[0] = "i"; Object.getOwnPropertyNames(f).join()`,
			"0,length,name,prototype"},
		// Through a proxy, whose own-keys trap reports the target's.
		{`Object.getOwnPropertyNames(new Proxy(function f() {}, {})).join()`,
			"length,name,prototype"},
		{`var p = new Proxy(function f() {}, {}); p.prototype
		  Object.getOwnPropertyNames(p).join()`, "length,name,prototype"},
		{`Reflect.ownKeys(new Proxy(function f() {}, {ownKeys: t => Reflect.ownKeys(t)})).join()`,
			"length,name,prototype"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestFunctionPrototypeWrites covers the operations that change the property
// rather than read it, each of which has to find it already there.
func TestFunctionPrototypeWrites(t *testing.T) {
	cases := []struct{ src, want string }{
		// Assignment overwrites the property rather than creating a fresh,
		// enumerable one beside it.
		{`function f() {}; f.prototype = 42; f.prototype`, "42"},
		{`function f() {}; f.prototype = 42
		  Object.getOwnPropertyDescriptor(f, "prototype").enumerable`, "false"},
		{`function f() {}; f.prototype = 42; Object.getOwnPropertyNames(f).join()`,
			"length,name,prototype"},
		{`function f() {}; f.prototype = 42; Object.keys(f).length`, "0"},

		// It is not configurable, so deleting it fails rather than removing it.
		{`function f() {}; delete f.prototype`, "false"},
		{`function f() {}; delete f.prototype; typeof f.prototype`, "object"},
		{`"use strict"
		  function f() {}
		  try { delete f.prototype; "no throw" } catch (e) { e.constructor.name }`,
			"TypeError"},

		// defineProperty finds the property to redefine, and may not make it
		// configurable.
		{`function f() {}; Object.defineProperty(f, "prototype", {value: 9}); f.prototype`, "9"},
		{`function f() {}
		  try {
		    Object.defineProperty(f, "prototype", {configurable: true})
		    "no throw"
		  } catch (e) { e.constructor.name }`, "TypeError"},
		{`function f() {}
		  Object.defineProperty(f, "prototype", {writable: false})
		  f.prototype = 1
		  typeof f.prototype`, "object"},

		// Freezing and sealing walk the keys, so they have to see it.
		{`function f() {}; Object.freeze(f); Object.isFrozen(f)`, "true"},
		{`function f() {}; Object.freeze(f)
		  Object.getOwnPropertyDescriptor(f, "prototype").writable`, "false"},
		{`function f() {}; Object.seal(f); typeof f.prototype`, "object"},
		{`function f() {}; Object.preventExtensions(f); typeof f.prototype`, "object"},
		{`function f() {}; Object.preventExtensions(f)
		  Object.getOwnPropertyNames(f).join()`, "length,name,prototype"},

		// Assigning through a receiver that is not the function leaves the
		// function's own property alone.
		{`function f() {}
		  var o = Object.create(f)
		  o.prototype = 5;
		  [o.hasOwnProperty("prototype"), typeof f.prototype].join()`, "true,object"},
		{`function f() {}; Reflect.set(f, "prototype", 3); f.prototype`, "3"},
		{`function f() {}; Reflect.deleteProperty(f, "prototype")`, "false"},
		{`function f() {}; Reflect.has(f, "prototype")`, "true"},
		{`function f() {}
		  Reflect.getOwnPropertyDescriptor(f, "prototype").value === f.prototype`, "true"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// A constructor's object is made with room for the properties the body assigns
// to `this`, which the compiler counts. The count is a hint: what the object
// ends up with, and in what order, may not depend on it.
func TestConstructedObjectShape(t *testing.T) {
	cases := []struct{ src, want string }{
		{`class P { constructor(x, y) { this.x = x; this.y = y } }
		  Object.keys(new P(1, 2)).join()`, "x,y"},
		{`class P { constructor(x, y) { this.x = x; this.y = y } }
		  var p = new P(1, 2); [p.x, p.y].join()`, "1,2"},
		// More than the object starts with room for.
		{`class Q { constructor() { this.a = 1; this.b = 2; this.c = 3; this.d = 4; this.e = 5 } }
		  var q = new Q()
		  Object.keys(q).join() + "=" + [q.a, q.e].join()`, "a,b,c,d,e=1,5"},
		// Fewer: the same name assigned twice is one property, in its first
		// position.
		{`class R { constructor() { this.a = 1; this.b = 2; this.a = 3 } }
		  var r = new R(); Object.keys(r).join() + "=" + r.a`, "a,b=3"},
		// A computed key is not counted, and still lands.
		{`class S { constructor(k) { this[k] = 1; this.z = 2 } }
		  Object.keys(new S("q")).join()`, "q,z"},
		// An arrow in the constructor shares its `this`.
		{`class T { constructor() { var f = () => { this.a = 1 }; f(); this.b = 2 } }
		  Object.keys(new T()).join()`, "a,b"},
		// A derived constructor, whose object the base makes.
		{`class A { constructor() { this.a = 1 } }
		  class B extends A { constructor() { super(); this.b = 2 } }
		  Object.keys(new B()).join()`, "a,b"},
		// Class fields, which are installed before the body runs.
		{`class F { f = 1; g = 2; constructor() { this.h = 3 } }
		  Object.keys(new F()).join()`, "f,g,h"},
		// An ordinary function used as a constructor.
		{`function P(x) { this.x = x }
		  var p = new P(5); [Object.keys(p).join(), p.x].join(":")`, "x:5"},
		// Reflect.construct and a subclass's prototype.
		{`class P { constructor() { this.x = 1 } }
		  class Q extends P {}
		  var o = Reflect.construct(P, [], Q);
		  [Object.keys(o).join(), Object.getPrototypeOf(o) === Q.prototype].join()`, "x,true"},
		// A bound constructor, which has no body of its own.
		{`class P { constructor(x) { this.x = x } }
		  var B = P.bind(null, 7)
		  var p = new B(); [p.x, p instanceof P].join()`, "7,true"},
		// A constructor that returns an object instead.
		{`function P() { this.x = 1; return {y: 2} }
		  Object.keys(new P()).join()`, "y"},
		// The object is extensible and its properties ordinary, whatever room
		// was made.
		{`class P { constructor() { this.x = 1 } }
		  var p = new P(); p.late = 2
		  var d = Object.getOwnPropertyDescriptor(p, "x");
		  [Object.keys(p).join(), d.writable, d.enumerable, d.configurable].join()`,
			"x,late,true,true,true"},
		{`class P { constructor() { this.x = 1 } }
		  var p = new P(); delete p.x; Object.keys(p).length`, "0"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
