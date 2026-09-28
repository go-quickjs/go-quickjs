package quickjs_test

import "testing"

// TestBigIntSizeLimit pins V8's limit on a BigInt, 2^30 bits, and its
// message, however a BigInt would grow past it: BigInt.asUintN with a width
// of 2^53 - 1 made 2^(2^53 - 1) before reducing and panicked, and a power or
// product past the limit was worked out at length (KI-16). A shift the old
// limit refused, well within V8's, works.
func TestBigIntSizeLimit(t *testing.T) {
	checkEval(t, `const t = (f) => { try { const v = f(); return typeof v === "bigint" ? "bits:" + (v < 0n ? -v : v).toString(16).length : String(v) } catch (e) { return e.name + ": " + e.message } };
		[t(() => 1n << (2n ** 25n)), t(() => 1n << (2n ** 30n)), t(() => BigInt.asUintN(2 ** 53 - 1, 5n)),
		 t(() => BigInt.asUintN(2 ** 53 - 1, -1n)), t(() => BigInt.asIntN(2 ** 53 - 1, -5n)), t(() => 2n ** (2n ** 31n)),
		 t(() => (1n << (2n ** 29n)) * (1n << (2n ** 29n))), t(() => (-1n) ** (2n ** 70n + 1n)), t(() => 0n << (2n ** 40n))].join()`,
		"bits:8388609,RangeError: Maximum BigInt size exceeded,bits:1,RangeError: Maximum BigInt size exceeded,bits:1,"+
			"RangeError: Maximum BigInt size exceeded,RangeError: Maximum BigInt size exceeded,bits:1,bits:1")
}
