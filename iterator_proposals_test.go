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

func TestIteratorChunksAndWindows(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`JSON.stringify([1, 2, 3, 4, 5].values().chunks(2).toArray())`, "[[1,2],[3,4],[5]]"},
		{`JSON.stringify([1, 2, 3, 4].values().chunks(2).toArray())`, "[[1,2],[3,4]]"},
		{`JSON.stringify([1, 2].values().chunks(2 ** 32 - 1).toArray())`, "[[1,2]]"},
		{`JSON.stringify([1, 2, 3, 4].values().windows(2).toArray())`, "[[1,2],[2,3],[3,4]]"},
		{`JSON.stringify([1, 2].values().windows(3).toArray())`, "[]"},
		{`JSON.stringify([1, 2].values().windows(3, "allow-partial").toArray())`, "[[1,2]]"},
		{`JSON.stringify([1, 2, 3].values().windows(3, "allow-partial").toArray())`, "[[1,2,3]]"},
		// Each window is an array of its own.
		{`var w = [1, 2, 3].values().windows(2); var a = w.next().value; a.push(9);
		  JSON.stringify(w.next().value)`, "[2,3]"},
		// The size is not converted, and a bad one closes the iterator before
		// next is read; windows checks undersized after the size.
		{`var log = []; var it = { get next() { log.push("next") }, return() { log.push("return"); return {} } };
		  var errs = [];
		  for (var s of ["2", 1.5, Infinity, NaN, 0, -1, 2 ** 32]) {
		    try { Iterator.prototype.chunks.call(it, s) } catch (e) { errs.push(e.constructor.name) }
		  }
		  for (var u of [null, "", "full", 0]) {
		    try { Iterator.prototype.windows.call(it, 1, u) } catch (e) { errs.push(e.constructor.name) }
		  }
		  try { Iterator.prototype.windows.call(it, 0, "bad") } catch (e) { errs.push(e.constructor.name) }
		  errs.join() + "|" + log.length + (log.includes("next") ? " next" : "")`,
			"TypeError,TypeError,TypeError,TypeError,RangeError,RangeError,RangeError," +
				"TypeError,TypeError,TypeError,TypeError,RangeError|12"},
		// Once the source runs out a return is not forwarded to it.
		{`var n = 0; var it = { i: 0, next() { return this.i++ < 3 ? {value: 1, done: false} : {done: true} },
		    return() { n++; return {} }, __proto__: Iterator.prototype };
		  var c = it.chunks(2); c.next(); c.next(); c.return(); n`, "0"},
		{`[Iterator.prototype.chunks.length, Iterator.prototype.windows.length].join()`, "1,1"},
	})
}

// TestIteratorHelperReturnWhileSuspended pins what a return method sees when it
// calls back into the helper being closed: before the helper's first step the
// helper is simply finished, and after it the helper is still running.
func TestIteratorHelperReturnWhileSuspended(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`var h, seen; var it = { next() { return {value: 1, done: false} },
		    return() { seen = JSON.stringify(h.next()); return {} }, __proto__: Iterator.prototype };
		  h = it.map(x => x); h.return(); seen`, `{"done":true}`},
		{`var h, seen; var it = { next() { return {value: 1, done: false} },
		    return() { try { h.next() } catch (e) { seen = e.constructor.name } return {} },
		    __proto__: Iterator.prototype };
		  h = it.map(x => x); h.next(); h.return(); seen`, "TypeError"},
	})
}

func TestIteratorConcat(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`Iterator.concat([1, 2], new Set([3]), "".split("")).toArray().join()`, "1,2,3"},
		{`Iterator.concat().next().done`, "true"},
		{`Iterator.concat.length`, "0"},
		{`Object.prototype.toString.call(Iterator.concat())`, "[object Iterator Helper]"},
		// Every argument is checked and its method read up front, but each is
		// opened only when its turn comes.
		{`var log = [];
		  function it(name) { return { get [Symbol.iterator]() { log.push("get " + name);
		    return () => { log.push("open " + name); return [name][Symbol.iterator]() } } } }
		  var c = Iterator.concat(it("a"), it("b")); log.push("made"); c.next(); c.next(); log.join()`,
			"get a,get b,made,open a,open b"},
		{`try { Iterator.concat([1], "ab") } catch (e) { e.constructor.name }`, "TypeError"},
		{`try { Iterator.concat({}) } catch (e) { e.constructor.name }`, "TypeError"},
		// A return is forwarded to the iterable being read, and to nothing
		// before concat starts.
		{`var log = []; var src = { [Symbol.iterator]() { return { next() { return {value: 1, done: false} },
		    return() { log.push("return"); return {} } } } };
		  var c = Iterator.concat(src); c.return(); log.push("|");
		  c = Iterator.concat(src); c.next(); c.return(); log.join("")`, "|return"},
	})
}
