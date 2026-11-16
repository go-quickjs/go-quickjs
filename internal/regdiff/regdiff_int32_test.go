package regdiff

import (
	"fmt"
	"testing"
)

// TestRegDiffMulImmInt32 compares mul_imm, x * k and k * x of a 16-bit k,
// and the int32 forms, (x + y) | 0, (x - y) | 0, (x + k) | 0 and
// (x - k) | 0, and the | 0 left out of a bitwise operator's int32 result,
// with the register tier off and on: sums at 2^31 and past it, past 2^32,
// 2^53 and 2^63, of -2^31 - 1; NaN, the infinities and -0 operands;
// strings ("12" + 3 is 123, "5" - 1 is 4 where "5" + -1 is "5-1"); valueOf's
// calls, counted and in their order; the products' -0s and the NaN of
// Infinity * 0, by Object.is and a Float64Array's bytes; and the
// TypeErrors -- a BigInt sum's at the |, a mixed sum's and product's at
// their operator, a bitwise operator's of a BigInt -- by their stacks.
// Each function runs in a loop, warm, over every operand.
func TestRegDiffMulImmInt32(t *testing.T) {
	regDiffParallel(t)
	src := `
var vals = [0, -0, 1, -1, 3, 2147483647, -2147483647, -2147483648, 2147483648, 4294967295, 4294967296, 4294967297,
	2 ** 53, -(2 ** 53), 2 ** 53 + 2, 2 ** 63, -(2 ** 63), 2 ** 64, 1e300, -1e300, NaN, Infinity, -Infinity, 0.5, -0.5, 1.5, -2.5,
	"12", "5", "-0", " 7 ", "x", "", "0x10", true, false, null, undefined, [], [3], {}];
function show(v) { return Object.is(v, -0) ? "-0" : typeof v === "bigint" ? v + "n" : typeof v === "string" ? JSON.stringify(v) : String(v) }
function frames(e) { return e.name + ": " + e.message + " | " + (e.stack || "").split("\n").slice(1, 3).map(function (s) { return s.trim() }).join(" | ") }
function bytes(v) { var f = new Float64Array(1); f[0] = v; return Array.from(new Uint8Array(f.buffer)).join(".") }
function sums(x, y) { return [(x + y) | 0, (x - y) | 0, (y - x) | 0, (x + 1) | 0, (x - 1) | 0, (x + -1) | 0, (x - -1) | 0,
	(x + 32767) | 0, (x + -32768) | 0, (x - 32767) | 0, (x - -32767) | 0, (x - -32768) | 0, (x - 0) | 0, (x + 0) | 0, (-0 - x) | 0] }
function prods(x) { return [x * 3, 3 * x, x * 0, x * -0, 0 * x, x * -3, -3 * x, x * 32767, x * -32768, x * 70000, 1103 * x] }
function folds(x, y) { return [(x & 255) | 0, (255 & x) | 0, (x ^ (y >>> 7)) | 0, ((y >>> 1) | x) | 0, (x << 31) | 0, (x >> 1) | 0] }
function grid(f, n) {
	var out = [];
	for (var r = 0; r < n; r++)
		for (var i = 0; i < vals.length; i++)
			for (var j = 0; j < vals.length; j++) {
				try { out.push(f(vals[i], vals[j]).map(show).join(" ")) } catch (e) { out.push(frames(e)) }
			}
	return out.join(",")
}
function one(f, n) {
	var out = [];
	for (var r = 0; r < n; r++)
		for (var i = 0; i < vals.length; i++) {
			try { out.push(f(vals[i]).map(show).join(" ")) } catch (e) { out.push(frames(e)) }
		}
	return out.join(",")
}
function nanBits() {
	var inf = Infinity, z = 0, out = [];
	for (var i = 0; i < 50; i++) out.push(bytes(inf * 0), bytes(-inf * 0), bytes(z * -3), bytes(-1 * 0), Object.is(z * -3, -0), Object.is(-1 * 0, -0), Object.is((-0 - 0) | 0, 0));
	return out.slice(-7).join()
}
function order() {
	var log = [], out = [];
	var o = { valueOf: function () { log.push("o"); return 7 } }, p = { valueOf: function () { log.push("p"); return "3" } };
	var q = { toString: function () { log.push("q"); return "8" }, valueOf: null };
	for (var i = 0; i < 20; i++) out.push((o + p) | 0, (o - p) | 0, (p + 1) | 0, (p - 1) | 0, (q + 1) | 0, (q - 1) | 0, (q + o) | 0, o * 3, 3 * p, q * 2);
	return out.slice(-10).join() + " " + log.length + " " + log.slice(-20).join("")
}
function strs() { var out = []; for (var i = 0; i < 30; i++) out.push(("12" + 3) | 0, ("5" - 1) | 0, ("5" + -1) | 0, ("5" + 1) | 0, ("4" * 3), (3 * "4")); return out.slice(-6).join() }
function threw(f) { try { return show(f()) } catch (e) { return frames(e) } }
function bigs() {
	var a = 1n, b = 2n, n = 1, out = [];
	for (var i = 0; i < 3; i++) {
		out.push(threw(function () { return (a + b) | 0 }), threw(function () { return (a - b) | 0 }),
			threw(function () { return (a + 1) | 0 }), threw(function () { return (a - 1) | 0 }),
			threw(function () { return (a + n) | 0 }), threw(function () { return (n - b) | 0 }),
			threw(function () { return a * 3 }), threw(function () { return 3 * a }),
			threw(function () { return (a ^ (n >>> 1)) | 0 }), threw(function () { return (a & 255) | 0 }),
			threw(function () { return (a ^ b) | 0 }), threw(function () { return ("1" + a) | 0 }));
	}
	return out.join(" ; ")
}
function kernel(n) {
	var j = 0, a = 1, b = 2, s = 0, c = 0, p = 1, q = 2;
	for (j = 0; (j | 0) < (n | 0); j = (j + 1) | 0) {
		a = (a * 1103 + b) | 0;
		b = (b ^ (a >>> 7)) | 0;
		s = (s + (a & 255) - (b >> 29)) | 0;
		c = (p + q) * 3 - c;
		p = q - c;
		q = c & 1023;
	}
	return [a, b, s, c, p, q, j].join()
}
function tryFinally(n) { var i, s = 0; for (i = 0; i < n; i++) { try { s = (s + i) | 0 } finally { s ^= 1 } } return s }
`
	var calls []string
	for _, c := range []string{
		"grid(sums, 2)", "grid(folds, 2)", "one(prods, 3)", "nanBits()", "order()", "strs()", "bigs()",
		"kernel(5000)", "kernel(100000)", "tryFinally(100000)",
	} {
		calls = append(calls, c)
	}
	for _, n := range []string{"0x7fffffff", "-0x80000000", "4294967296", "-0", "NaN"} {
		calls = append(calls, fmt.Sprintf("[(%[1]s + 1) | 0, (%[1]s - 1) | 0, %[1]s * 3].map(show).join()", n))
	}
	regDiffAll(t, src, calls)
}
