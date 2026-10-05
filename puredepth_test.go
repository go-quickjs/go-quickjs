package quickjs_test

import "testing"

// TestPureRecursionDepth covers recursion that the frameless evaluator for
// pure bodies follows up to pureCallDepth calls deep, and gives back to
// framed calls beyond: results at depths each side of the limit, a store a
// pure body makes as its last act at the bottom of the recursion, a getter
// met at the bottom after many frameless calls, which the call then makes
// with frames once, and runaway recursion, which still ends in a
// RangeError. Each runs often enough for the evaluator to be tried.
func TestPureRecursionDepth(t *testing.T) {
	cases := []struct{ src, want string }{
		{`function depth(n) { return n == 0 ? 0 : 1 + depth(n - 1) }
		  function factorial(n) { return n <= 1 ? 1 : n * factorial(n - 1) }
		  var r = [];
		  for (var i = 0; i < 100; i++) r = [depth(7), depth(8), depth(9), depth(31), depth(32), depth(33), depth(100), factorial(10), factorial(20)];
		  r.join()`, "7,8,9,31,32,33,100,3628800,2432902008176640000"},
		{`function bump(o, v) { o.hit = v }
		  function mark(o, n, v) { return n == 0 ? bump(o, v) : mark(o, n - 1, v) }
		  var o = { hit: 0 }, seen = [];
		  for (var i = 1; i <= 100; i++) { mark(o, 20, i); if (o.hit != i) seen.push(i) }
		  mark(o, 40, -1);
		  [o.hit, seen.length].join()`, "-1,0"},
		{`var count = 0, plain = { x: 1 }, getter = { get x() { count++; return 2 } };
		  function bottom(o, n) { return n == 0 ? o.x : bottom(o, n - 1) }
		  var s = 0;
		  for (var i = 0; i < 100; i++) s += bottom(plain, 20);
		  for (var i = 0; i < 3; i++) s += bottom(getter, 20);
		  [s, count].join()`, "106,3"},
		{`function down(n) { return down(n + 1) }
		  var r = [];
		  for (var i = 0; i < 3; i++) { try { down(0) } catch (e) { r.push(e.name) } }
		  r.join()`, "RangeError,RangeError,RangeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
