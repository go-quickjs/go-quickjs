package quickjs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Nearly every Array.prototype method is generic: it works on anything with a
// length and numeric keys. That is what lets
// Array.prototype.map.call(arguments, f) work, and it is most of what test262
// exercises.
func TestArrayMethodsAreGeneric(t *testing.T) {
	const obj = `var o = {length: 3, 0: "a", 1: "b", 2: "c"}; `
	const nums = `var o = {length: 3, 0: 1, 1: 2, 2: 3}; `

	cases := []struct{ src, want string }{
		{obj + `Array.prototype.map.call(o, x => x).join(",")`, "a,b,c"},
		{obj + `Array.prototype.filter.call(o, x => x > "a").join(",")`, "b,c"},
		{obj + `Array.prototype.join.call(o, "-")`, "a-b-c"},
		{obj + `Array.prototype.slice.call(o, 1).join(",")`, "b,c"},
		{obj + `String(Array.prototype.indexOf.call(o, "b"))`, "1"},
		{obj + `String(Array.prototype.lastIndexOf.call(o, "c"))`, "2"},
		{obj + `String(Array.prototype.includes.call(o, "c"))`, "true"},
		{obj + `Array.prototype.concat.call([], o).length + "," +
		        Array.prototype.concat.call([1], 2).join(",")`, "1,1,2"},
		{nums + `String(Array.prototype.reduce.call(o, (a, b) => a + b))`, "6"},
		{nums + `String(Array.prototype.reduceRight.call(o, (a, b) => a + b))`, "6"},
		{nums + `String(Array.prototype.every.call(o, x => x > 0))`, "true"},
		{nums + `String(Array.prototype.some.call(o, x => x > 2))`, "true"},
		{nums + `String(Array.prototype.find.call(o, x => x > 1))`, "2"},
		{nums + `String(Array.prototype.findIndex.call(o, x => x > 1))`, "1"},
		{nums + `String(Array.prototype.findLast.call(o, x => x < 3))`, "2"},
		{nums + `var out = []; Array.prototype.forEach.call(o, x => out.push(x)); out.join(",")`,
			"1,2,3"},
		// The arguments object is the canonical array-like.
		{`function f() { return Array.prototype.map.call(arguments, x => x * 2).join(","); }
		  f(1, 2, 3)`, "2,4,6"},
		// A string is array-like too.
		{`Array.prototype.map.call("abc", c => c.toUpperCase()).join("")`, "ABC"},

		// The callback receives (value, index, object).
		{`var seen; [7].map(function (v, i, arr) { seen = [v, i, arr.length].join(":"); });
		  seen`, "7:0:1"},
		{`[1].map(function () { return this.x; }, {x: 9}).join(",")`, "9"},
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

// TestArrayHoles pins that an elision is an absent property rather than one
// holding undefined, which decides whether the iteration methods visit it and
// whether the prototype shows through.
func TestArrayHoles(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(1 in [1, , 3])`, "false"},
		{`String(0 in [1, , 3])`, "true"},
		{`JSON.stringify(Object.keys([1, , 3]))`, `["0","2"]`},
		{`String([1, , 3].length)`, "3"},
		{`var n = 0; [1, , 3].forEach(() => n++); String(n)`, "2"},
		{`var n = 0; [1, , 3].map(() => n++); String(n)`, "2"},
		{`String([1, , 3].filter(() => true).length)`, "2"},
		// find visits holes, unlike forEach, because it is looking for a
		// position.
		{`var n = 0; [1, , 3].find(() => { n++; return false; }); String(n)`, "3"},
		// A hole falls through to the prototype.
		{`Array.prototype[1] = "proto"; [1, , 3].join("-")`, "1-proto-3"},
		{`Array.prototype[1] = "proto"; String([1, , 3].includes("proto"))`, "true"},
		// new Array(n) is all holes.
		{`var a = new Array(3); String(0 in a) + "," + a.length`, "false,3"},
		{`var a = []; a[2] = 1; JSON.stringify(Object.keys(a))`, `["2"]`},
		{`var a = [1, 2]; delete a[0]; String(0 in a)`, "false"},
		// A hole stays a hole through map and slice.
		{`String(1 in [1, , 3].map(x => x))`, "false"},
		{`String(0 in [1, , 3].slice(1))`, "false"},
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

// TestArrayIndexAccessor pins that an index redefined as an accessor is
// actually called. Dense storage cannot express one, so defining it has to
// vacate the slot.
func TestArrayIndexAccessor(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var a = [0, 1]; Object.defineProperty(a, 0, {get() { return 5; }});
		  String(a[0])`, "5"},
		{`var log = []; var a = [0, 1];
		  Object.defineProperty(a, 0, {get() { log.push("g"); return 5; }});
		  a.map(x => x).join(",") + "|" + log.length`, "5,1|1"},
		{`var a = [0]; Object.defineProperty(a, 0, {value: 7, writable: false});
		  a[0] = 9; String(a[0])`, "7"},
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

// TestHugeArrayLikeIsInterruptible pins that walking an array-like which claims
// an enormous length can still be stopped. Such a walk runs no bytecode, so
// without an explicit check it would be a hang the context bound could not
// reach -- the exact failure the bound exists to prevent.
func TestHugeArrayLikeIsInterruptible(t *testing.T) {
	for _, src := range []string{
		`Array.prototype.includes.call({length: 9007199254740991}, 1)`,
		`Array.prototype.indexOf.call({length: 9007199254740991}, 1)`,
		`Array.prototype.join.call({length: 9007199254740991})`,
	} {
		rt := quickjs.New()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)

		done := make(chan error, 1)
		go func() {
			_, err := rt.EvalContext(ctx, src)
			done <- err
		}()

		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s: got %v, want a deadline error", src, err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("%s: not interrupted", src)
		}
		cancel()
		rt.Close()
	}
}

// TestFunctionNameAndLength pins that a function's name and length behave like
// the own properties they are, even though they are synthesized on demand.
func TestFunctionNameAndLength(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var f = function foo(a, b) {}; f.name + "," + f.length`, "foo,2"},
		{`JSON.stringify(Object.getOwnPropertyDescriptor(function foo() {}, "name"))`,
			`{"value":"foo","writable":false,"enumerable":false,"configurable":true}`},
		{`JSON.stringify(Object.getOwnPropertyNames(function foo(a, b) {}))`,
			`["length","name","prototype"]`},
		// Non-writable: assignment does nothing rather than shadowing it.
		{`var f = function foo() {}; f.name = "no"; f.name`, "foo"},
		// But configurable: defineProperty and delete both work.
		{`var f = function foo() {}; Object.defineProperty(f, "name", {value: "z"}); f.name`, "z"},
		{`var f = function foo() {}; delete f.name;
		  String(Object.getOwnPropertyDescriptor(f, "name"))`, "undefined"},
		// After deleting it, the read falls through to Function.prototype.
		{`var f = function foo() {}; delete f.name; f.name`, ""},
		{`var f = function foo(a, b) {}; delete f.length; String(f.length)`, "0"},
		{`Array.prototype.map.name + "," + Array.prototype.map.length`, "map,1"},
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

	// In strict mode the failed assignment throws instead of being ignored.
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`"use strict"; var f = function foo() {}; f.name = "x";`); err == nil {
		t.Error("assigning to a function's name in strict mode should throw")
	}
}

// Array.from over an async iterable produces an array of promises rather than a
// promise of an array, which is almost never what the caller wanted.
// Array.fromAsync awaits each value and resolves once.
func TestArrayFromAsync(t *testing.T) {
	cases := []struct{ src, want string }{
		{`Array.fromAsync([1, 2, 3]).then(a => r = a.join(","))`, "1,2,3"},
		{`Array.fromAsync([Promise.resolve(1), 2]).then(a => r = a.join(","))`, "1,2"},
		{`async function* g() { yield 1; yield 2; }
		  Array.fromAsync(g()).then(a => r = a.join(","))`, "1,2"},
		{`Array.fromAsync([1, 2], x => x * 2).then(a => r = a.join(","))`, "2,4"},
		// The mapper may itself be async.
		{`Array.fromAsync([1, 2], async x => x * 3).then(a => r = a.join(","))`, "3,6"},
		// An array-like with no iterator is read by index.
		{`Array.fromAsync({length: 2, 0: "a", 1: "b"}).then(a => r = a.join(","))`, "a,b"},
		{`Array.fromAsync([1]).then(a => r = a instanceof Array)`, "true"},
		// A rejection anywhere rejects the whole thing.
		{`Array.fromAsync([Promise.reject(new Error("x"))]).catch(e => r = e.message)`, "x"},
		{`Array.fromAsync(null).catch(e => r = e.constructor.name)`, "TypeError"},
		{`Array.fromAsync([1], 1).catch(e => r = e.constructor.name)`, "TypeError"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		if _, err := rt.Eval("var r;" + tc.src); err != nil {
			t.Errorf("%s: %v", tc.src, err)
			rt.Close()
			continue
		}
		got, err := rt.Eval("String(r)")
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got.String() != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got.String(), tc.want)
		}
		rt.Close()
	}
}

// Setting an array's length is defining it: the value is converted twice, in
// that order, and nothing else is decided until both conversions have run --
// which matters because a conversion can change the array.
func TestArrayLengthCoercionOrder(t *testing.T) {
	cases := []struct{ name, src, want string }{
		// ToUint32 first, then ToNumber; a value that survives neither is not
		// a length, and that is a RangeError rather than a TypeError.
		{"invalid length", `try { Object.defineProperty([], "length", {value: -1}) }
		  catch (e) { e.constructor.name }`, "RangeError"},
		{"invalid length assigned", `try { [].length = -1 } catch (e) { e.constructor.name }`,
			"RangeError"},
		{"converted twice", `var log = []
		  var length = {valueOf: function () { log.push("valueOf"); return 0 }}
		  Object.defineProperty([1], "length", {value: length})
		  log.join(",")`, "valueOf,valueOf"},
		// The writability is read after the conversions, so a conversion that
		// takes it away is the one that decides.
		{"writability read last", `var a = [1, 2]
		  var calls = 0
		  var length = {valueOf: function () {
		    if (++calls !== 1) Object.defineProperty(a, "length", {writable: false})
		    return a.length
		  }}
		  var caught = ""
		  try { Object.defineProperty(a, "length", {value: length, writable: true}) }
		  catch (e) { caught = e.constructor.name }
		  caught + "," + calls`, "TypeError,2"},
		{"assignment sees it too", `var a = [1, 2, 3]
		  var hints = []
		  var length = {}
		  length[Symbol.toPrimitive] = function (hint) {
		    hints.push(hint)
		    Object.defineProperty(a, "length", {writable: false})
		    return 0
		  }
		  String(Reflect.set(a, "length", length)) + "," + hints.join(",") + "," + a.length`,
			"false,number,number,3"},
		// A length that cannot be redefined refuses a descriptor that would
		// change it, and accepts one that describes it as it is.
		{"frozen length", `var a = [1]
		  Object.defineProperty(a, "length", {writable: false});
		  [String(Reflect.defineProperty(a, "length", {writable: true})),
		   String(Reflect.defineProperty(a, "length", {value: 0})),
		   String(Reflect.defineProperty(a, "length", {value: 1})),
		   String(Reflect.defineProperty(a, "length", {}))].join(",")`,
			"false,false,true,true"},
		// A refused assignment reports as one.
		{"set refused", `var a = Object.freeze([1])
		  String(Reflect.set(a, "length", 0)) + "," + String(Reflect.set(a, "0", 9))`,
			"false,false"},
		{"set refused on a string", `var s = Object(" ")
		  String(Reflect.set(s, "length", 5)) + "," + String(Reflect.set(s, "0", "x"))`,
			"false,false"},
		{"set refused on a sealed object", `var o = Object.preventExtensions({})
		  String(Reflect.set(o, "x", 1))`, "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkEval(t, tc.src, tc.want) })
	}
}
