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
