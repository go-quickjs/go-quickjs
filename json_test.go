package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// The replacer and the reviver are the two hooks that make JSON.stringify and
// JSON.parse worth using over a hand-written walk.
func TestJSONReplacerAndReviver(t *testing.T) {
	cases := []struct{ src, want string }{
		// The array form fixes which properties appear, and in what order.
		{`JSON.stringify({a: 1, b: 2}, ["a"])`, `{"a":1}`},
		{`JSON.stringify({a: 1, b: 2}, ["b", "a"])`, `{"b":2,"a":1}`},
		{`JSON.stringify({a: 1, b: 2}, ["a", "a"])`, `{"a":1}`},
		{`JSON.stringify({1: "x"}, [1])`, `{"1":"x"}`},

		// The function form sees every key, including the empty one that stands
		// for the whole value.
		{`JSON.stringify({a: 1}, (k, v) => typeof v === "number" ? v * 2 : v)`, `{"a":2}`},
		{`var keys = []; JSON.stringify({a: {b: 1}}, (k, v) => { keys.push(k); return v; });
		  keys.join("|")`, "|a|b"},
		// Returning undefined drops the property.
		{`JSON.stringify({a: 1, b: 2}, (k, v) => k === "a" ? undefined : v)`, `{"b":2}`},

		// toJSON runs before the replacer.
		{`JSON.stringify({toJSON: () => 42})`, "42"},
		{`JSON.stringify(new Date(0))`, `"1970-01-01T00:00:00.000Z"`},

		{`JSON.parse('{"a":1}', (k, v) => typeof v === "number" ? v + 1 : v).a + ""`, "2"},
		// A reviver returning undefined prunes the entry.
		{`JSON.stringify(JSON.parse('{"a":1,"b":2}', (k, v) => k === "a" ? undefined : v))`,
			`{"b":2}`},
		{`JSON.parse('[1,2,3]', (k, v) => typeof v === "number" ? v * 10 : v).join(",")`,
			"10,20,30"},
		// The reviver walks bottom-up, so a parent sees revived children.
		{`var order = []; JSON.parse('{"a":{"b":1}}', function (k, v) { order.push(k); return v; });
		  order.join("|")`, "b|a|"},

		// A wrapper stands for its primitive.
		{`JSON.stringify(new Number(1)) + "," + JSON.stringify(new String("s"))`, `1,"s"`},
		{`JSON.stringify([new Boolean(true)])`, "[true]"},

		// A proxy is stringified through its traps, and one over an array is
		// still an array.
		{`JSON.stringify(new Proxy({a: 1}, {}))`, `{"a":1}`},
		{`JSON.stringify(new Proxy([1, 2], {}))`, "[1,2]"},
		{`String(Array.isArray(new Proxy([], {})))`, "true"},

		// Indentation, which is capped at ten.
		{`JSON.stringify({a: 1}, null, 2)`, "{\n  \"a\": 1\n}"},
		{`String(JSON.stringify({a: 1}, null, 100).length)`, "20"},
		{`JSON.stringify({a: 1}, null, "ab")`, "{\nab\"a\": 1\n}"},

		// Values with no JSON form.
		{`String(JSON.stringify(undefined)) + "," + String(JSON.stringify(function () {}))`,
			"undefined,undefined"},
		{`JSON.stringify([undefined, function () {}])`, "[null,null]"},
		{`JSON.stringify({a: undefined, b: function () {}, c: 1})`, `{"c":1}`},
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

// TestJSONStringifySpace covers the space argument, which names an indent.
func TestJSONStringifySpace(t *testing.T) {
	cases := []struct{ src, want string }{
		// A Number or String object stands for the primitive it wraps.
		{`JSON.stringify({a: 1}, null, new Number(3)) === JSON.stringify({a: 1}, null, 3)`,
			"true"},
		{`JSON.stringify({a: 1}, null, new String("--")) === JSON.stringify({a: 1}, null, "--")`,
			"true"},
		// The count is truncated, capped at ten, and ignored below one.
		{`JSON.stringify({a: 1}, null, 5.9) === JSON.stringify({a: 1}, null, 5)`, "true"},
		{`JSON.stringify({a: 1}, null, 100) === JSON.stringify({a: 1}, null, 10)`, "true"},
		{`JSON.stringify({a: 1}, null, -1.99999) === JSON.stringify({a: 1}, null, 0)`, "true"},
		{`JSON.stringify({a: 1}, null, 0)`, `{"a":1}`},
		// Ten code units of the string, not ten bytes: an indent of twenty
		// two-byte characters is cut to ten, leaving 10 + len(`"a": 1`).
		{`JSON.stringify({a: 1}, null, "é".repeat(20)).split("\n")[1].length`, "16"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestJSONKeysAreStrings covers the key a replacer or a reviver is given for an
// array element, which is the property name rather than the index.
func TestJSONKeysAreStrings(t *testing.T) {
	cases := []struct{ src, want string }{
		{`JSON.stringify([1, 2], function (k, v) { return typeof k === "string" ? v : "BAD" })`,
			"[1,2]"},
		{`var seen = []
		  JSON.stringify([1], function (k, v) { seen.push(typeof k); return v })
		  seen.join(",")`, "string,string"},
		{`JSON.parse("[1,2]", function (k, v) { return typeof k === "string" ? v : "BAD" })
		    .join(",")`, "1,2"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestJSONReviverWalk covers what the reviver may do to the object it is
// walking. The keys are settled before the walk starts, a refusal to write back
// is ignored, and a trap that throws is not.
func TestJSONReviverWalk(t *testing.T) {
	cases := []struct{ src, want string }{
		// A property the reviver deletes is still visited, and then read
		// through the prototype chain.
		{`Object.prototype.b = 3
		  var deleted
		  var o = JSON.parse('{"a": 1, "b": 2}', function (k, v) {
		    if (k === "a") deleted = delete this.b
		    return v
		  })
		  var out = [deleted, o.a, o.hasOwnProperty("b"), o.b].join(",")
		  delete Object.prototype.b
		  out`, "true,1,true,3"},
		// A property that cannot be redefined keeps its value rather than
		// failing the parse.
		{`var arr = JSON.parse("[1, 2]", function (k, v) {
		    if (k === "0") Object.defineProperty(this, "1", {configurable: false})
		    if (k === "1") return 22
		    return v
		  })
		  arr.join(",")`, "1,2"},
		// A trap that throws is the parse's result.
		{`var bad = new Proxy({a: 1}, {deleteProperty() { throw new RangeError() }})
		  try {
		    JSON.parse("[0,0]", function () { this[1] = bad })
		    "no throw"
		  } catch (e) { e.constructor.name }`, "RangeError"},
		{`var bad = new Proxy({0: null}, {defineProperty() { throw new RangeError() }})
		  try {
		    JSON.parse('["first", null]', function (_, value) {
		      if (value === "first") this[1] = bad
		      return value
		    })
		    "no throw"
		  } catch (e) { e.constructor.name }`, "RangeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestJSONBigInt covers a BigInt, which has no JSON form of its own but may be
// given one.
func TestJSONBigInt(t *testing.T) {
	cases := []struct{ src, want string }{
		{`try { JSON.stringify(0n) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { JSON.stringify(Object(0n)) } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { JSON.stringify({x: 0n}) } catch (e) { e.constructor.name }`, "TypeError"},
		{`BigInt.prototype.toJSON = function () { return this.toString() }
		  var out = JSON.stringify(0n)
		  delete BigInt.prototype.toJSON
		  out`, `"0"`},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
