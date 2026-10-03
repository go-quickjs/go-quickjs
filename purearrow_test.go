package quickjs_test

import "testing"

// TestPureArrows covers arrows the compiler marks pure, which a sort or an
// array method calls without a frame, as it does other pure bodies. Only an
// arrow that never reads this is one: an arrow that does sees the this of
// where it was written, through a nested arrow too, whatever thisArg a
// method is given; and what a callback throws, after its frameless try has
// given up, shows its frame in the trace.
func TestPureArrows(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var r = []
		  r.push([3, 1, 2].sort((x, y) => x - y).join(""), [1, 2, 3].map(x => x * 2).join(""),
		    [1, 2, 3, 4].filter(x => x % 2 == 0).join(""), [1, 2, 3].reduce((p, c) => p + c, 0),
		    [{ id: 1 }, { id: 2 }].find(o => o.id === 2).id, [1, 2].some(x => x > 1), [1, 2].every(x => x > 1))
		  var s = 0; [1, 2, 3].forEach(x => { s += x }); r.push(s)
		  r.join()`, "123,246,24,6,2,true,false,6"},
		{`var o = { k: -1, sortDesc(a) { return a.sort((x, y) => this.k * (x - y)) },
		    nested(a) { return a.map(x => (() => this.k * x)()) }, withArg(a) { return a.map(x => this === o, { other: 1 }) } }
		  ;[o.sortDesc([1, 3, 2]).join(""), o.nested([1, 2]).join(), o.withArg([1]).join()].join(" ")`, "321 -1,-2 true"},
		{`function f() { "use strict"; return [1, 2].map(x => typeof this) } [f.call(5).join(), f.call("s").join()].join(" ")`,
			"number,number string,string"},
		{`var e; try { [1, 2].map(x => x.y.z) } catch (err) { e = err }
		  [e.name, /at .*\n.*at Array\.map|at Array\.map/.test(e.stack) || e.stack.indexOf("map") >= 0].join()`, "TypeError,true"},
		{`function f() { return [1, 2].map(x => arguments.length + x) } f(7, 8, 9).join()`, "4,5"},
		{`var n = 0; [{}, {}].forEach(o => { n += (o.x === undefined) ? 1 : 0 }); [5, 6].map(Math.floor).join() + n`, "5,62"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
