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

// TestJSONStringifyShapes covers the places where the serializer writes into
// its buffer and then has to take something back: a property whose value turns
// out to have no JSON form is omitted along with its key and the separator
// written in front of it.
func TestJSONStringifyShapes(t *testing.T) {
	cases := []struct{ src, want string }{
		// The dropped property is the first, the last, and the only one.
		{`JSON.stringify({a: undefined, b: 1})`, `{"b":1}`},
		{`JSON.stringify({a: 1, b: undefined})`, `{"a":1}`},
		{`JSON.stringify({a: undefined})`, `{}`},
		{`JSON.stringify({a: 1, b: undefined, c: 2})`, `{"a":1,"c":2}`},
		{`JSON.stringify({a: undefined, b: undefined})`, `{}`},
		{`JSON.stringify({a: function () {}, b: Symbol()})`, `{}`},
		{`JSON.stringify({a: {b: undefined}, c: 1})`, `{"a":{},"c":1}`},
		// The same with an indent, where the separator carries a newline.
		{`JSON.stringify({a: undefined, b: 1}, null, 1)`, "{\n \"b\": 1\n}"},
		{`JSON.stringify({a: undefined}, null, 1)`, `{}`},
		{`JSON.stringify({a: [1, undefined]}, null, 1)`,
			"{\n \"a\": [\n  1,\n  null\n ]\n}"},

		// An element with no JSON form becomes null rather than disappearing,
		// because an array's indices may not shift.
		{`JSON.stringify([undefined, 1, function () {}])`, `[null,1,null]`},
		{`JSON.stringify([])`, `[]`},
		{`JSON.stringify([[], {}, [[]]])`, `[[],{},[[]]]`},

		// Numbers go through the append path, which writes an integer directly
		// and everything else the long way.
		{`JSON.stringify([0, -0, 1, -1, 1e21, 1e-7, 0.5, NaN, Infinity])`,
			`[0,0,1,-1,1e+21,1e-7,0.5,null,null]`},
		// Past 2^53 what is printed is the shortest decimal that reads back as
		// the same double, which is not the double's exact value.
		{`JSON.stringify(9007199254740993e3)`, `9007199254740993000`},
		{`JSON.stringify(2 ** 53)`, `9007199254740992`},
		{`JSON.stringify(2 ** 53 + 2)`, `9007199254740994`},
		{`JSON.stringify(1e300)`, `1e+300`},

		// Escaping: a string with nothing to escape is taken whole, and one
		// with an escape in it is rebuilt from the first escape on.
		{`JSON.stringify("plain")`, `"plain"`},
		{`JSON.stringify("a\"b")`, `"a\"b"`},
		{`JSON.stringify("tab\tend")`, `"tab\tend"`},
		{`JSON.stringify("\\")`, `"\\"`},
		{`JSON.stringify("héllo →")`, `"h` + "é" + `llo ` + "→" + `"`},
		{`JSON.stringify("é\né")`, `"` + "é" + `\n` + "é" + `"`},
		// A control character with no short escape is written in full.
		{`JSON.stringify("\u0000\u001f")`, `"\u0000\u001f"`},
		// A key is escaped the same way a value is.
		{`JSON.stringify({"a\"b": 1, "c\td": 2})`, `{"a\"b":1,"c\td":2}`},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestJSONParseStrings covers the parser's fast path, which returns a slice of
// the source text for a string with no escape in it, and the built path it
// falls into at the first escape.
func TestJSONParseStrings(t *testing.T) {
	cases := []struct{ src, want string }{
		{`JSON.parse('"plain"')`, "plain"},
		{`JSON.parse('""')`, ""},
		{`JSON.parse('"a\\"b"')`, `a"b`},
		{`JSON.parse('"pre\\tpost"')`, "pre\tpost"},
		{`JSON.parse('"\\u0041\\u0042"')`, "AB"},
		{`JSON.parse('"tail\\\\"')`, `tail\`},
		{`JSON.parse('"é→"')`, "é→"},
		{`JSON.parse('"é\\u00e9"')`, "éé"},
		// The parsed key is an ordinary property name.
		{`Object.keys(JSON.parse('{"a\\tb":1}')).join()`, "a\tb"},
		{`JSON.parse('{"x":"y"}').x`, "y"},
		{`JSON.parse('[ "a" , "b" ]').join("-")`, "a-b"},
		{`String(JSON.parse('[1,[2,[3,[4]]]]'))`, "1,2,3,4"},
		{`JSON.parse('[[],[[]]]').length`, "2"},

		// What the fast path must still reject.
		{`try { JSON.parse('"unterminated') } catch (e) { e.constructor.name }`, "SyntaxError"},
		{`try { JSON.parse('"a\tb"') } catch (e) { e.constructor.name }`, "SyntaxError"},
		{`try { JSON.parse('"a\\qb"') } catch (e) { e.constructor.name }`, "SyntaxError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestJSONLoneSurrogateRoundTrip keeps the escaping of a surrogate that has no
// partner, which has no UTF-8 form and so must survive as an escape.
func TestJSONLoneSurrogateRoundTrip(t *testing.T) {
	cases := []struct{ src, want string }{
		{`JSON.stringify("\uD800")`, `"\ud800"`},
		{`JSON.stringify("a\uDC00b")`, `"a\udc00b"`},
		{`JSON.stringify("😀")`, `"` + "\U0001F600" + `"`},
		{`JSON.stringify({"\uD800": 1})`, `{"\ud800":1}`},
		{`JSON.parse(JSON.stringify("\uD800")).charCodeAt(0).toString(16)`, "d800"},
		{`JSON.parse('"\\uD800"').length`, "1"},
		{`JSON.parse('"\\uD83D\\uDE00"').length`, "2"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestJSONNestingIsBounded covers the depth limit. Parsing, serializing and
// reviving all walk the structure by recursion, and the stack a deep enough
// document would exhaust cannot be grown for ever -- nor can its exhaustion be
// caught, which would take the host down rather than the script.
func TestJSONNestingIsBounded(t *testing.T) {
	cases := []struct{ src, want string }{
		// Ordinary depth is unaffected.
		{`JSON.parse("[".repeat(100) + "]".repeat(100)).length`, "1"},
		{`JSON.stringify(JSON.parse("[".repeat(500) + "]".repeat(500))).length`, "1000"},

		// Past the limit it is an error a script can catch, rather than the
		// end of the process.
		{`var deep = "[".repeat(3000000) + "]".repeat(3000000)
		  try { JSON.parse(deep); "parsed" } catch (e) { e.constructor.name }`, "RangeError"},
		{`var deep = "{\"a\":".repeat(200000) + "1" + "}".repeat(200000)
		  try { JSON.parse(deep); "parsed" } catch (e) { e.constructor.name }`, "RangeError"},
		// The same for building one and serializing it.
		{`var o = [], t = o
		  for (var i = 0; i < 200000; i++) { var n = []; t.push(n); t = n }
		  try { JSON.stringify(o); "ok" } catch (e) { e.constructor.name }`, "RangeError"},
		// And for reviving one, which walks it again.
		{`var text = "[".repeat(20000) + "]".repeat(20000)
		  try { JSON.parse(text, function (k, v) { return v }); "ok" }
		  catch (e) { e.constructor.name }`, "RangeError"},
		// A structure just inside the limit still works.
		{`var text = "[".repeat(9000) + "]".repeat(9000)
		  JSON.stringify(JSON.parse(text)).length`, "18000"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
