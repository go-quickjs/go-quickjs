package quickjs_test

import "testing"

// The built-ins that call back into script reuse the list the arguments go in,
// which is only sound because a callee may not keep what it was called with --
// the same rule the interpreter's own stack relies on. These pin it: every
// callback has to see the arguments of its own call, whatever it does with them.
func TestCallbackArgumentsAreNotShared(t *testing.T) {
	cases := []struct{ src, want string }{
		// An arguments object outlives the call that made it.
		{`var kept = []
		  ;[10, 20, 30].forEach(function () { kept.push(arguments) })
		  kept.map(function (a) { return a[0] + ":" + a[1] }).join()`,
			"10:0,20:1,30:2"},
		{`var kept = []
		  ;[10, 20].map(function () { kept.push(arguments); return 0 })
		  kept.map(function (a) { return a.length + "/" + a[0] }).join()`, "3/10,3/20"},
		{`var kept = []
		  ;[1, 2, 3].filter(function () { kept.push(arguments); return true })
		  kept.map(function (a) { return a[1] }).join()`, "0,1,2"},
		{`var kept = []
		  ;[1, 2, 3].reduce(function (acc) { kept.push(arguments); return acc }, 0)
		  kept.map(function (a) { return a[0] + "+" + a[1] }).join()`, "0+1,0+2,0+3"},
		{`var kept = []
		  ;[3, 1, 2].sort(function (a, b) { kept.push(arguments); return a - b })
		  kept.every(function (a) { return a.length === 2 }) + ":" + (kept.length > 0)`,
			"true:true"},
		{`var kept = []
		  new Map([["a", 1], ["b", 2]]).forEach(function () { kept.push(arguments) })
		  kept.map(function (a) { return a[1] + "=" + a[0] }).join()`, "a=1,b=2"},
		{`var kept = []
		  new Set([1, 2]).forEach(function () { kept.push(arguments) })
		  kept.map(function (a) { return a[0] }).join()`, "1,2"},
		{`var kept = []
		  Array.from([1, 2], function () { kept.push(arguments); return 0 })
		  kept.map(function (a) { return a[0] + "@" + a[1] }).join()`, "1@0,2@1"},
		{`var kept = []
		  ;[[1], [2]].flatMap(function (x) { kept.push(arguments); return x })
		  kept.map(function (a) { return a[1] }).join()`, "0,1"},
		{`var kept = []
		  Object.groupBy([1, 2], function (v) { kept.push(arguments); return "k" })
		  kept.map(function (a) { return a[0] }).join()`, "1,2"},

		// Rest parameters, which copy into an array of their own.
		{`var kept = []
		  ;[1, 2, 3].forEach(function (...a) { kept.push(a) })
		  kept.map(function (a) { return a.join("/") }).join("|")`, "1/0/1,2,3|2/1/1,2,3|3/2/1,2,3"},

		// A generator callback keeps its frame, and with it the arguments it
		// was given, until something resumes it -- which is after the next
		// element has been handed to the next call.
		{`var gs = []
		  var made = [7, 8].map(function* () { gs.push(arguments); yield arguments[0] })
		  made.forEach(function (g) { g.next() })
		  gs.map(function (a) { return a[0] }).join()`, "7,8"},
		{`var out = [7, 8].map(function* (x) { yield arguments[0] })
		  out.map(function (g) { return g.next().value }).join()`, "7,8"},

		// An iterator helper is one call per element too.
		{`var kept = []
		  Array.from([1, 2].values().map(function (v) { kept.push(arguments); return v }))
		  kept.map(function (a) { return a[0] + "@" + a[1] }).join()`, "1@0,2@1"},
		{`var kept = []
		  ;[1, 2].values().forEach(function () { kept.push(arguments) })
		  kept.map(function (a) { return a[1] }).join()`, "0,1"},
		{`var kept = []
		  ;[1, 2].values().reduce(function (acc) { kept.push(arguments); return acc }, 0)
		  kept.map(function (a) { return a[1] }).join()`, "1,2"},
		{`var kept = []
		  Array.from([1, 2].values().filter(function () { kept.push(arguments); return true }))
		  kept.map(function (a) { return a[1] }).join()`, "0,1"},

		// A callback that calls back into the same method gets its own list.
		{`var out = []
		  ;[1, 2].forEach(function (x) {
		    [10, 20].forEach(function (y) { out.push(x + "-" + y) })
		  })
		  out.join()`, "1-10,1-20,2-10,2-20"},
		{`[[3, 1], [2, 4]].map(function (a) { return a.sort(function (x, y) {
		    return [x, y].sort(function (p, q) { return p - q })[0] - y
		  }).join("") }).join()`, "13,24"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// The same for the callbacks that are not per element of an array: a
// replacement function is called once per match, and a mapper once per item.
func TestPerMatchCallbackArguments(t *testing.T) {
	cases := []struct{ src, want string }{
		// String.prototype.replace with a function, which is called per match.
		{`var kept = []
		  "a-b-c".replaceAll("-", function () { kept.push(arguments); return "+" })
		  kept.map(function (a) { return a[0] + "@" + a[1] }).join()`, "-@1,-@3"},
		{`"abc".replace("b", function (m, i, s) { return m + i + s })`, "ab1abcc"},

		// The regular expression form, whose argument list also carries the
		// captures and, when there are named groups, the group object.
		{`var kept = []
		  "a1b2".replace(/([a-z])(\d)/g, function () { kept.push(arguments); return "" })
		  kept.map(function (a) { return a[0] + ":" + a[1] + a[2] + "@" + a[3] }).join()`,
			"a1:a1@0,b2:b2@2"},
		{`"a1b2".replace(/(?<l>[a-z])(?<d>\d)/g, function () {
		    var g = arguments[arguments.length - 1]
		    return g.l + g.d + "|"
		  })`, "a1|b2|"},
		{`var kept = []
		  "xyz".replace(/./g, function () { kept.push(Array.prototype.slice.call(arguments)) })
		  kept.map(function (a) { return a[0] + a[1] }).join()`, "x0,y1,z2"},

		// Array.from, over an array-like and over an iterator.
		{`var kept = []
		  Array.from({length: 2, 0: "a", 1: "b"}, function () { kept.push(arguments); return 0 })
		  kept.map(function (a) { return a[0] + "@" + a[1] }).join()`, "a@0,b@1"},
		{`var kept = []
		  Array.from(new Set(["a", "b"]), function () { kept.push(arguments); return 0 })
		  kept.map(function (a) { return a[0] + "@" + a[1] }).join()`, "a@0,b@1"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
