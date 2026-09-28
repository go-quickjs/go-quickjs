package quickjs_test

import "testing"

// TestNumberParsingMatchesV8 pins parseInt and Number of digit strings
// against V8: a string of hex, octal or binary digits, and a long decimal
// one, is rounded once, correctly, where it was rounded at every digit; a
// radix other than a power of two or ten gets V8's own approximation, which
// the standard allows; parseInt("0x") is NaN, where it was 0; and
// (-0).toExponential() has no sign (KI-42). The hash is of the 22415 answers
// node 26 gives to the same seeded strings.
func TestNumberParsingMatchesV8(t *testing.T) {
	checkEval(t, `[(-0).toExponential(), parseInt("0x"), parseInt("-0x"), parseInt("0x", 16),
		Number("0x1000000000000081"), Number("0x10000000000000800001"),
		Number("0b1" + "0".repeat(52) + "11"), parseInt("123456789012345678901234567890")].join()`,
		"0e+0,NaN,NaN,NaN,1152921504606847200,7.555786372591434e+22,18014398509481988,1.2345678901234568e+29")
	checkEval(t, `let seed = 12345;
		const rnd = (n) => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed % n; };
		const D = "0123456789abcdefghijklmnopqrstuvwxyz";
		const out = [];
		for (let t = 0; t < 20000; t++) {
			const radix = 2 + rnd(35);
			const len = 1 + rnd(t % 10 === 0 ? 400 : 80);
			let s = "";
			for (let i = 0; i < len; i++) s += D[rnd(radix)];
			if (rnd(3) === 0) s = D[1 + rnd(radix - 1)] + s;
			out.push(String(parseInt(s, radix)));
			if (radix === 16) out.push(String(Number("0x" + s)));
			if (radix === 8) out.push(String(Number("0o" + s)));
			if (radix === 2) out.push(String(Number("0b" + s)));
			if (radix === 10) out.push(String(parseInt(s)), String(Number(s)));
		}
		let h = 0; const all = out.join("\n"); for (let i = 0; i < all.length; i++) h = (h * 31 + all.charCodeAt(i)) | 0;
		out.length + " " + h`, "22415 -1715467742")
}
