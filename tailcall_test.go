package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestTailCalls pins that a strict function's calls in tail position do not
// keep its frame: a chain of them far deeper than the call stack allows runs
// to the end.
func TestTailCalls(t *testing.T) {
	cases := []struct{ src, want string }{
		{`"use strict"; function f(n) { return n === 0 ? "done" : f(n - 1) } f(100000)`, "done"},
		{`"use strict"; function even(n) { return n === 0 || odd(n - 1) } function odd(n) { return n !== 0 && even(n - 1) } String(even(100001))`, "false"},
		{`"use strict"; const o = { m(n) { return n === 0 ? this.tag : this.m(n - 1) }, tag: "method" }; o.m(100000)`, "method"},
		{`"use strict"; function f(n, acc) { if (n === 0) return acc; return (0, f)(n - 1, acc + 1) } String(f(100000, 0))`, "100000"},
		{`"use strict"; function f(n) { return n === 0 ? "bound" : g(n - 1) } const g = f.bind(null); g(100000)`, "bound"},
		{`"use strict"; const f = n => n === 0 ? "arrow" : f(n - 1); f(100000)`, "arrow"},
	}
	for _, tc := range cases {
		rt := quickjs.New(quickjs.WithMaxCallDepth(400))
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if v.String() != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, v.String(), tc.want)
		}
		rt.Close()
	}
	// Sloppy code, a try block and a for-of body have no tail calls: each of
	// these still runs out of stack.
	for _, src := range []string{
		`function f(n) { return n === 0 ? 0 : f(n - 1) } f(100000)`,
		`"use strict"; function f(n) { try { return n === 0 ? 0 : f(n - 1) } finally {} } f(100000)`,
		`"use strict"; function f(n) { for (const x of [1]) return n === 0 ? 0 : f(n - 1) } f(100000)`,
	} {
		rt := quickjs.New(quickjs.WithMaxCallDepth(400))
		_, err := rt.Eval(src)
		if err == nil || !strings.Contains(err.Error(), "RangeError") {
			t.Errorf("%s: got %v, want a RangeError", src, err)
		}
		rt.Close()
	}
}
