package quickjs_test

import "testing"

// TestShortBigInt covers BigInts in the int64 range, which a Value holds
// without an allocation, and their meeting with the ones beyond it, which it
// does not: the 50-bit split of the held form, results that cross between
// the two forms in both directions, the edges of the int64 range in every
// operator, and the places a BigInt is a key, a stored element or text.
// The functions with loops and no calls run as trees; the rest is the
// interpreter.
func TestShortBigInt(t *testing.T) {
	cases := []struct{ src, want string }{
		{`[0n, 1n, -1n, 2n**49n, -(2n**49n), 2n**50n - 1n, 2n**50n, -(2n**50n), -(2n**50n) - 1n,
		   2n**63n - 1n, -(2n**63n), 2n**63n, -(2n**63n) - 1n, 2n**64n].map(String).join()`, "0,1,-1,562949953421312,-562949953421312,1125899906842623,1125899906842624,-1125899906842624,-1125899906842625,9223372036854775807,-9223372036854775808,9223372036854775808,-9223372036854775809,18446744073709551616"},
		{`var M = 2n**63n - 1n, m = -(2n**63n);
		  [M + 1n, M + 1n - 1n === M, m - 1n, m - 1n + 1n === m, M - m, m - M,
		   (2n**64n - 2n**63n - 2n**63n + 5n) === 5n, typeof (M + 1n - 1n)].join()`, "9223372036854775808,true,-9223372036854775809,true,18446744073709551615,-18446744073709551615,true,bigint"},
		{`var m = -(2n**63n);
		  [(-(2n**32n)) * 2n**31n, 2n**32n * 2n**31n, -1n * m, m * 1n, m * -1n,
		   3037000499n * 3037000499n, 3037000500n * 3037000500n, 0n * m, -7n * 6n].join()`, "-9223372036854775808,9223372036854775808,9223372036854775808,-9223372036854775808,9223372036854775808,9223372030926249001,9223372037000250000,0,-42"},
		{`var m = -(2n**63n);
		  [m / -1n, m % -1n, -7n / 2n, -7n % 2n, 7n / -2n, 7n % -2n, m / 2n, 2n**64n / 2n,
		   (2n**64n + 1n) % 2n, (2n**64n) / (2n**62n)].join()`, "9223372036854775808,0,-3,-1,-3,1,-4611686018427387904,9223372036854775808,1,4"},
		{`[2n**62n, 2n**63n, (-2n)**63n, (-2n)**64n, 3n**39n, 3n**40n, 0n**0n, (-1n)**(2n**70n),
		   (-1n)**(2n**70n + 1n), 1n**(2n**80n), 0n**5n, (-3n)**3n, (2n**64n)**1n].join()`, "4611686018427387904,9223372036854775808,-9223372036854775808,18446744073709551616,4052555153018976267,12157665459056928801,1,1,-1,1,0,-27,18446744073709551616"},
		{`[1n << 62n, 1n << 63n, -1n << 63n, -1n << 64n, 5n >> 100n, -5n >> 100n, 1n << -1n, -1n >> -3n,
		   3n << 0n, -(2n**63n) >> 63n, 2n**63n >> 1n, 1n << 70n >> 70n, 0n << (2n**40n),
		   -(2n**63n) << 0n, 1n >> (2n**64n)].join()`, "4611686018427387904,9223372036854775808,-9223372036854775808,-18446744073709551616,0,-1,0,-8,3,-1,4611686018427387904,1,0,-9223372036854775808,0"},
		{`[-1n & 255n, 6n | -9n, 5n ^ -1n, ~0n, ~(-(2n**63n)), ~(2n**63n - 1n), (2n**64n + 3n) & 7n,
		   -(2n**64n) | 1n, ((2n**63n) ^ (2n**63n)) === 0n].join()`, "255,-9,-6,-1,9223372036854775807,-9223372036854775808,3,-18446744073709551615,true"},
		{`var big = 2n**64n;
		  [5n < big, big > -big, -(2n**63n) < 2n**63n - 1n, 3n <= 3n, 3n == 3, 3n === 3n,
		   (2n**63n) === (2n**63n), (2n**63n - 1n + 1n) === 2n**63n, 1n < 1.5, 2n**64n > 1e19,
		   "10" > 9n, 0n == "", Object.is(0n, -0n), -0n === 0n, [1n, 2n**64n].includes(2n**64n),
		   [5n, 1n].indexOf(1n), 2n**63n - 1n == 9223372036854775807n].join()`, "true,true,true,true,true,true,true,true,true,true,true,true,true,true,true,1,true"},
		{`var mp = new Map([[2n**63n - 1n, "a"], [2n**64n, "b"], [-1n, "c"]]);
		  var s = new Set([1n, 1n, 2n**64n, 2n**64n, 2n**63n - 1n, 2n**62n * 2n - 1n, -0n, 0n]);
		  [mp.get(2n**62n * 2n - 1n), mp.get(2n**65n / 2n), mp.get(0n - 1n), mp.has(5n), s.size,
		   [...s].join(" ")].join()`, "a,b,c,false,4,1 18446744073709551616 9223372036854775807 0"},
		{`[String(-(2n**63n)), (2n**63n - 1n).toString(16), (-255n).toString(2), (2n**50n).toString(36),
		   Number(2n**63n - 1n), Number(-(2n**53n) - 1n), BigInt(2**53), BigInt("-9223372036854775808") === -(2n**63n),
		   BigInt.asIntN(64, 2n**63n), BigInt.asUintN(64, -1n), BigInt.asIntN(8, 255n), Boolean(0n), Boolean(-1n),
		   !!(2n**64n), typeof (1n + 1n), "" + -1n, (-(2n**63n)).toLocaleString("en-US")].join()`, "-9223372036854775808,7fffffffffffffff,-11111111,b33j9ynrb4,9223372036854776000,-9007199254740992,9007199254740992,true,-9223372036854775808,18446744073709551615,-1,false,true,true,bigint,-1,-9,223,372,036,854,775,808"},
		{`var a = new BigInt64Array([2n**63n - 1n, -(2n**63n), -1n]), u = new BigUint64Array([2n**64n - 1n, 2n**63n]);
		  a[2] += 1n; u[1] += 2n**63n;
		  var dv = new DataView(new ArrayBuffer(8)); dv.setBigInt64(0, -(2n**63n));
		  var ta = new BigInt64Array(new SharedArrayBuffer(16)); Atomics.add(ta, 0, 2n**62n); Atomics.add(ta, 0, 2n**62n);
		  [a.join(" "), u.join(" "), dv.getBigInt64(0), dv.getBigUint64(0), ta[0],
		   Atomics.compareExchange(ta, 0, -(2n**63n), 1n), ta[0]].join()`, "9223372036854775807 -9223372036854775808 0,18446744073709551615 0,-9223372036854775808,9223372036854775808,-9223372036854775808,-9223372036854775808,1"},
		{`var o = {}; o[2n**64n] = 1; o[-1n] = 2; o[5n] = 3;
		  [Object.keys(o).join(" "), [3n, -1n, 2n**64n, -(2n**63n)].sort((x, y) => (x < y ? -1 : x > y ? 1 : 0)).join(" "),
		   new BigInt64Array([3n, -(2n**63n), 2n**63n - 1n, 0n]).sort().join(" ")].join()`, "5 18446744073709551616 -1,-9223372036854775808 -1 3 18446744073709551616,-9223372036854775808 0 3 9223372036854775807"},
		{`function step(x) { var y = x; for (var i = 0; i < 3; i++) { y++; y--; y++ } return y }
		  function neg(x) { var r; for (var i = 0; i < 2; i++) r = -x; return r }
		  function lp(a, b) { var r; for (var i = 0; i < 2; i++) r = [a + b, a - b, a * b, a / b, a % b, a ** 2n,
		      a << 1n, a >> 1n, a & b, a | b, a ^ b, ~a, a < b, a === b, a == b]; return r.join() }
		  var M = 2n**63n - 1n, m = -(2n**63n), x = M; x++; var y = m; y--; var z = M + 1n; --z;
		  [step(M - 2n), step(M), neg(m), neg(5n), lp(M, 2n), lp(m, -1n), lp(3n, 2n), lp(2n**64n, 2n**64n), x, y, z === M].join(" ")`, "9223372036854775808 9223372036854775810 9223372036854775808 -5 9223372036854775809,9223372036854775805,18446744073709551614,4611686018427387903,1,85070591730234615847396907784232501249,18446744073709551614,4611686018427387903,2,9223372036854775807,9223372036854775805,-9223372036854775808,false,false,false -9223372036854775809,-9223372036854775807,9223372036854775808,9223372036854775808,0,85070591730234615865843651857942052864,-18446744073709551616,-4611686018427387904,-9223372036854775808,-1,9223372036854775807,9223372036854775807,true,false,false 5,1,6,1,1,9,6,1,2,3,1,-4,false,false,false 36893488147419103232,0,340282366920938463463374607431768211456,1,0,340282366920938463463374607431768211456,36893488147419103232,9223372036854775808,18446744073709551616,18446744073709551616,0,-18446744073709551617,false,true,true 9223372036854775808 -9223372036854775809 true"},
		{`[BigInt(2**63), BigInt(-(2**63)), BigInt(-0), BigInt(true), BigInt(false), BigInt(2**53 + 2), BigInt(-1e18),
		   BigInt(2**63) === 2n**63n, BigInt(-(2**63)) === -(2n**63n), BigInt(1e18) === 10n**18n,
		   new Map([[BigInt(7), 1]]).get(7n), BigInt(2**64) - BigInt(2**63) * 2n].join()`, "9223372036854775808,-9223372036854775808,0,1,0,9007199254740994,-1000000000000000000,true,true,true,1,0"},
		{`var r = [];
		  for (var f of [() => 1n / 0n, () => 1n % 0n, () => 2n ** -1n, () => 1n + 1, () => 1n >>> 0n,
		                 () => 1n << (2n**40n), () => +1n, () => 1n >> -(2n**63n), () => Math.max(1n)]) {
		    try { r.push(String(f())) } catch (e) { r.push(e.name) }
		  }
		  r.join()`, "RangeError,RangeError,RangeError,TypeError,TypeError,RangeError,TypeError,RangeError,TypeError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
