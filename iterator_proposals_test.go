package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// evalCases runs each source in a fresh runtime and compares the string form
// of its result; a thrown error's constructor name is the result for a source
// that catches it.
func evalCases(t *testing.T, cases []struct{ src, want string }) {
	t.Helper()
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

func TestIteratorIncludes(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`[[1, 2, 3].values().includes(2), [1, 2, 3].values().includes(4)].join()`, "true,false"},
		// SameValueZero: NaN is found, and the zeros are one value.
		{`[[NaN].values().includes(NaN), [-0].values().includes(0)].join()`, "true,true"},
		{`[[1, 2, 1].values().includes(1, 1), [1, 2].values().includes(1, 1),
		   [1].values().includes(1, Infinity), [1].values().includes(1, -0)].join()`, "true,false,false,true"},
		// A match closes the iterator; running out does not.
		{`var log = []; function* g() { try { yield 1; yield 2 } finally { log.push("closed") } }
		  g().includes(1); log.join()`, "closed"},
		// The skip count is not converted, and a bad one closes the iterator
		// before next is read.
		{`var log = []; var it = { get next() { log.push("next") }, return() { log.push("return"); return {} } };
		  var errs = [];
		  for (var s of ["1", 1.5, NaN, {valueOf() { log.push("valueOf"); return 1 }}, -1, -Infinity, 2 ** 53]) {
		    try { Iterator.prototype.includes.call(it, 1, s) } catch (e) { errs.push(e.constructor.name) }
		  }
		  errs.join() + "|" + log.join()`,
			"TypeError,TypeError,TypeError,TypeError,RangeError,RangeError,RangeError|" +
				"return,return,return,return,return,return,return"},
		{`try { Iterator.prototype.includes.call(1, 1) } catch (e) { e.constructor.name }`, "TypeError"},
		{`Iterator.prototype.includes.length`, "1"},
	})
}

func TestIteratorJoin(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`[1, null, undefined, "a", true].values().join()`, "1,,,a,true"},
		{`[1, 2].values().join("") + [1, 2].values().join(undefined) + [].values().join("-")`, "121,2"},
		{`[1, 2].values().join({toString() { return "&" }})`, "1&2"},
		// The separator is converted before next is read, and a conversion
		// that throws closes the iterator.
		{`var log = [];
		  var it = { get next() { log.push("next"); return () => ({done: true}) }, return() { log.push("return"); return {} } };
		  Iterator.prototype.join.call(it, {toString() { log.push("sep"); return "," }});
		  try { Iterator.prototype.join.call(it, {toString() { throw 1 }}) } catch (e) {}
		  log.join()`, "sep,next,return"},
		// So does an element whose conversion throws.
		{`var log = []; function* g() { try { yield {toString() { throw new RangeError }} } finally { log.push("closed") } }
		  try { g().join() } catch (e) { log.push(e.constructor.name) } log.join()`, "closed,RangeError"},
		{`Iterator.prototype.join.length`, "1"},
	})
}
