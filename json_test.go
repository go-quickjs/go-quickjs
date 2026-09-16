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
