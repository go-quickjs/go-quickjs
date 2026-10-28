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

// TestBigIntSizeLimitOfText pins where V8 refuses a BigInt written out, past
// 2^30 bits however it is written: a string past it is no BigInt -- BigInt
// throws a SyntaxError, a comparison is false -- its leading zeros not
// counted, and a literal past it is an early SyntaxError, its leading zeros
// counted. Each was made at length before. The string BigInt's SyntaxError
// quotes is cut at 1,000 code units, as V8 cuts it: refusing one of 2^28
// characters wrote all of them out again.
func TestBigIntSizeLimitOfText(t *testing.T) {
	checkEval(t, `const t = (f) => { try { return String(f()) } catch (e) { return e.name + (e.name === "SyntaxError" && e.message.startsWith("Max") ? ": " + e.message : "") } };
		const m = (s) => { try { BigInt(s) } catch (e) { return e.message } };
		const over = "0x1" + "0".repeat(2 ** 28), face = String.fromCodePoint(0x1F600);
		[m(over).length, m("1".repeat(999) + "x") === 'cannot convert "' + "1".repeat(999) + 'x" to a BigInt',
		 m("1".repeat(1000) + "x") === 'cannot convert "' + "1".repeat(1000) + '…" to a BigInt',
		 m(face.repeat(500) + "x") === 'cannot convert "' + face.repeat(500) + '…" to a BigInt',
		 m("x" + face.repeat(500)) === 'cannot convert "x' + face.repeat(499) + '…" to a BigInt',
		 t(() => BigInt(over)), t(() => over == 1n), t(() => over < 1n), t(() => ("0x" + "0".repeat(2 ** 28) + "1") == 1n),
		 t(() => eval(over + "n")), t(() => Function("return -" + over + "n"))].join()`,
		"1030,true,true,true,true,SyntaxError,false,false,true,"+
			"SyntaxError: Maximum BigInt size exceeded (line 1, column 1),SyntaxError: Maximum BigInt size exceeded (line 3, column 9)")
}
