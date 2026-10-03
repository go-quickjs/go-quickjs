package quickjs_test

import "testing"

// TestBuiltinIteratorSteps covers loops over a Map, Set or array iterator and
// over a generator, which step the built-in next themselves rather than calling it
// for a result object. Nothing a script can see may differ: a next replaced
// on the prototype or on the iterator is called, a yield* hands over the
// delegate's own result objects, whose done and value the loop reads through
// any getters they have -- after yield* has read done itself -- the
// generator's next keeps its frame in a stack trace, and errors and
// re-entrance behave as through the call.
func TestBuiltinIteratorSteps(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"map and set", `var m = new Map([[1, "a"], [2, "b"]]), s = new Set(["x", "y"]), r = []
		  for (var [k, v] of m) r.push(k + v)
		  for (var k of m.keys()) r.push(k)
		  for (var v of s) r.push(v)
		  r.push([...m.values()].join(""), Array.from(s.entries()).join("|"), new Set(m.keys()).size)
		  var [first, second] = s; r.push(first + second)
		  r.join()`, "1a,2b,1,2,x,y,ab,x,x|y,y,2,xy"},
		{"next replaced on the prototype", `var MapIter = Object.getPrototypeOf(new Map().values()), next = MapIter.next, n = 0
		  MapIter.next = function () { n++; return next.call(this) }
		  var r = [...new Map([[1, 1], [2, 2]]).values()]
		  MapIter.next = next; r.push(n); r.join()`, "1,2,3"},
		{"next replaced on the generator", `function* g() { yield 1; yield 2 }
		  var it = g(), n = 0, next = it.next
		  it.next = function () { n++; return next.call(this) }
		  var r = []; for (var v of it) r.push(v)
		  r.push(n); r.join()`, "1,2,3"},
		{"yield* hands over the delegate's results", `var log = []
		  var inner = { [Symbol.iterator]() { return this }, i: 0,
		    next() { var i = this.i++; return { get done() { log.push("done" + i); return i == 2 }, get value() { log.push("value" + i); return i } } } }
		  function* g() { yield* inner }
		  for (var v of g()) log.push("got" + v)
		  log.join()`, "done0,done0,value0,got0,done1,done1,value1,got1,done2,value2"},
		{"next keeps its frame", `function* g() { yield new Error().stack.split("\n")[2].trim() }
		  var a; for (a of g()); var b = [...g()][0], c = g().next().value;
		  [a == c, b == c, c].join()`, "true,true,at Object.next (<anonymous>)"},
		{"generator throws", `function* g() { yield 1; throw new RangeError("stop") }
		  var r = []
		  try { for (var v of g()) r.push(v) } catch (e) { r.push(e.name) }
		  r.join()`, "1,RangeError"},
		{"generator re-entered", `var it = (function* () { yield 1; it.next() })(), r = []
		  try { for (var v of it) r.push(v) } catch (e) { r.push(e.name) }
		  r.join()`, "1,TypeError"},
		{"array iterator reads", `var log = [], o = { get length() { log.push("len"); return 2 }, get 0() { log.push("g0"); return "a" }, 1: "b" }
		  var r = []; for (var v of Array.prototype.values.call(o)) r.push(v)
		  r.join() + " " + log.join()`, "a,b len,g0,len,len"},
		{"array iterator getter throws", `var o = { length: 3, 0: 1, get 1() { throw new RangeError("g1") } }, r = []
		  try { for (var v of Array.prototype.values.call(o)) r.push(v) } catch (e) { r.push(e.name) }
		  r.join()`, "1,RangeError"},
		{"array iterator next frame", `var seen = []
		  var o = { length: 1, get 0() { seen.push(new Error().stack.split("\n")[2].trim()); return 0 } }
		  for (var v of Array.prototype.values.call(o)); Array.prototype.values.call(o).next()
		  seen[0] == seen[1]`, "true"},
		{"array iterator kinds", `function g() { var r = []; for (var v of arguments) r.push(v); return r.join("") }
		  var a = ["x", "y"], r = [g(1, 2, 3)]
		  for (var e of a.entries()) r.push(e.join(":")); for (var k of a.keys()) r.push(k)
		  for (var t of new Int8Array([5, 6])) r.push(t)
		  r.push(Math.max(...new Uint8Array([3, 9, 4])), [...("ab")].join("+"))
		  r.join()`, "123,0:x,1:y,0,1,5,6,9,a+b"},
		{"array iterator next replaced", `var AI = Object.getPrototypeOf([][Symbol.iterator]()), next = AI.next, n = 0
		  AI.next = function () { n++; return next.call(this) }
		  function g() { return [...arguments].length } var len = g(1, 2)
		  AI.next = next; [len, n].join()`, "2,3"},
		{"typed array detached during the loop", `var b = new ArrayBuffer(8), t = new Uint8Array(b), r = []
		  try { for (var v of t) { r.push(v); if (r.length == 2) b.transfer() } } catch (e) { r.push(e.name) }
		  r.join()`, "0,0,TypeError"},
		{"loop exits early", `var closed = 0
		  function* g() { try { yield 1; yield 2 } finally { closed++ } }
		  for (var v of g()) break
		  var [x] = g(); [closed, x].join()`, "2,1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkEval(t, tc.src, tc.want)
		})
	}
}
