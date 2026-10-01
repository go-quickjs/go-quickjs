package quickjs_test

import "testing"

// TestBranchConditions covers conditions compiled as branches rather than
// values: !x branching the other way, && and || jumping straight to where
// the whole condition goes, comparisons fused with their jumps in both
// directions -- NaN, which makes an ordering's negation another ordering
// no longer, included -- constant conditions with no test, and
// `if (test) break;` and `continue` as the condition's own jumps, and the
// ones that cannot be: through a finally, out of a try or a for-of. The
// order operands are evaluated in, and each answer, are Node's.
func TestBranchConditions(t *testing.T) {
	tests := []struct{ src, want string }{
		{`var log = []; function f(v) { log.push(v); return v }
			if (f(0) && f(1)) log.push("then"); if (f(1) && f(0)) log.push("then"); if (f(1) && f(2)) log.push("then");
			if (f(0) || f(0)) log.push("or"); if (f(0) || f(3)) log.push("or"); if (!f(0) || f(9)) log.push("not"); log.join()`,
			"0,1,0,1,2,then,0,0,0,3,or,0,not"},
		{`var log = []; function f(v) { log.push(v); return v }
			if (!(f(1) && f(0)) && !(f(0) || f(0))) log.push("x"); if (f(1) && !f(2) || f(3) && !(!f(4))) log.push("y"); log.join()`,
			"1,0,0,0,x,1,2,3,4,y"},
		{`var r = []; for (const [a, b] of [[1, 2], [2, 1], [NaN, 1], [1, NaN], [1, 1]]) {
			r.push([a < b, !(a < b), a >= b, !(a >= b), a <= b && !(a > b), !(a == b), !(a !== b)].map(Number).join(""));
			if (!(a < b)) r.push("!lt"); if (!(a <= b) || a != a) r.push("!le") } r.join()`,
			"1001110,0110010,!lt,!le,0101010,!lt,!le,0101010,!lt,!le,0110101,!lt"},
		{`var i = 0, r = []; while (true) { if (i++ > 4) break; if (i % 2 == 0) continue; r.push(i) } r.join() + " " + i`,
			"1,3,5 6"},
		{`var r = []; for (var i = 0; i < 10; i++) { if (!(i < 8)) break; if (i === 2 || i === 5) continue; r.push(i) } r.join()`,
			"0,1,3,4,6,7"},
		{`var x = 0; do { x++ } while (x != 5 && x < 9); var y = 0; do { y++; if (y < 3) continue; } while (false); x + " " + y`,
			"5 1"},
		{`var n = NaN, c = 0; do { c++ } while (n < 1); while (!(n >= 1)) { if (++c > 3) break } c`,
			"4"},
		{`var r = []; outer: for (var i = 0; i < 4; i++) { for (var j = 0; j < 4; j++) { if (j > i) continue outer; if (i + j > 4) break outer; r.push("" + i + j) } } r.join()`,
			"00,10,11,20,21,22,30,31"},
		{`var r = []; lbl: { if (r.length === 0) break lbl; r.push("no") } r.push("yes"); r.join()`,
			"yes"},
		{`var r = []; for (var i = 0; i < 3; i++) { try { if (i == 1) break; r.push("t" + i) } finally { r.push("f" + i) } } r.join()`,
			"t0,f0,f1"},
		{`var r = []; for (var i = 0; i < 3; i++) { try { if (i == 1) continue; r.push("t" + i) } catch (e) {} } try { throw 1 } catch (e) { r.push("caught") } r.join()`,
			"t0,t2,caught"},
		{`var closed = 0, it = { [Symbol.iterator]() { var k = 0; return { next() { return { value: k++, done: k > 9 } }, return() { closed++; return {} } } } };
			var r = []; for (var v of it) { if (v > 2) break; r.push(v) } r.join() + " closed " + closed`,
			"0,1,2 closed 1"},
		{`var fs = []; for (let i = 0; i < 5; i++) { if (i == 2) continue; if (!(i < 4)) break; fs.push(() => i) } fs.map(f => f()).join()`,
			"0,1,3"},
		{`var r = []; while (false) r.push("never"); while (0) r.push("never"); for (; false;) r.push("never"); if (false) r.push("no"); else r.push("else");
			if (1) r.push("one"); if (!0) r.push("not0"); if (!NaN) r.push("notNaN"); do r.push("once"); while (0); r.join()`,
			"else,one,not0,notNaN,once"},
		{`var o = { valueOf() { r.push("v"); return 1 } }, r = []; if (!(o < 2)) r.push("x"); while (o > 2 || o == 5) r.push("y"); r.join()`,
			"v,v,v"},
		{`var x = 3; (x > 2 && !(x > 5) ? "mid" : "out") + " " + (!x ? 1 : 2) + " " + (x || 0 ? "t" : "f")`,
			"mid 2 t"},
		{`var r = []; switch (1) { case 1: if (r.length == 0) break; r.push("no") } r.push("after"); r.join()`,
			"after"},
		{`function g(a) { for (var i = 0; ; i++) { if (a[i] === undefined) break } return i } g([1, 2, 3])`,
			"3"},
	}
	for _, tt := range tests {
		checkEval(t, tt.src, tt.want)
	}
}
