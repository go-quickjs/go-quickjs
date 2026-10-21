package quickjs_test

import "testing"

// Number.prototype.toString of a radix but 10 writes V8's digits
// (DoubleToRadixCString): a fraction's only as far as they tell the number
// from its neighbours, the last rounded half to even -- 0.1 in base 3 is 34
// characters, which C QuickJS writes too, not the binary fraction's every
// digit -- and an integer's past 2**53 as zeros, 1e21 in base 36
// 5v1j4f4ds7c000. Node v26.10.0's answers, and its hash of 20000 seeded
// numbers of every kind, each in a radix from 2 to 36.
func TestNumberToStringRadix(t *testing.T) {
	checkEval(t, `[(2**60).toString(36), (2**63+2**11).toString(7), (1e19).toString(36), (2**54+2).toString(3), (1e21).toString(36), (123456789.123456789).toString(36), (0.1).toString(3), (-255.5).toString(16), (1e-7).toString(2), (2**-1074).toString(36), (Number.MAX_VALUE).toString(36).length].join(" ")`,
		"8rc4kbdvss00 22341010611245052046000 23z405fz79ts0 10020111100200200022122200101210001 5v1j4f4ds7c000 21i3v9.4g 0.0022002200220022002200220022002201 -ff.8 0.0000000000000000000000011010110101111111001010011010101111001010111101001 0.0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000003 199")
	checkEval(t, `var seed = 99;
		function rnd() { seed = (Math.imul(seed, 1103515245) + 12345) | 0; return (seed >>> 8) / 16777216 }
		var dv = new DataView(new ArrayBuffer(8));
		function anyDouble() { dv.setUint32(0, (rnd() * 2 ** 32) >>> 0); dv.setUint32(4, (rnd() * 2 ** 32) >>> 0); return dv.getFloat64(0) }
		var gens = [anyDouble, () => (rnd() - 0.5) * 1000, () => rnd(), () => Math.floor(rnd() * 2 ** 40) / 1024, () => rnd() * 2 ** Math.floor(rnd() * 140 - 70), () => Math.floor(rnd() * 1e6) / 100];
		var out = [];
		for (var i = 0; i < 20000; i++) {
			var x = gens[i % gens.length](), radix = 2 + (i * 7 + Math.floor(rnd() * 35)) % 35;
			out.push(x.toString(radix));
		}
		var h = 0x811c9dc5, s = out.join(",");
		for (var k = 0; k < s.length; k++) h = Math.imul(h ^ s.charCodeAt(k), 16777619);
		out.length + " " + s.length + " " + (h >>> 0)`, "20000 791166 3877061381")
}
