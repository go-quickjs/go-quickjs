package quickjs_test

import "testing"

// TestMathMatchesV8 pins Math's functions to V8's to the last bit: they are
// fdlibm's, ported from V8 14.6's src/base/ieee754.cc, where Go's math
// package answered differently in the last place for as many as 900 of 3000
// arguments, and far off in places -- the logarithm of a subnormal, sinh and
// cosh of 710, which overflowed. pow and ** are Arm's, from its Optimized
// Routines -- the code glibc ships, which V8 calls on Linux: 10 ** 308 was
// 1.0000000000000006e+308. Math.sumPrecise lost 5e-324
// beside 1e308 (KI-43). The hash is of the answers to 3000 seeded arguments
// for each function, node 26's to the last bit. (Node on Windows calls the C
// library's pow, not Arm's, and rounds a few in a thousand arguments near
// a midpoint the other way; none of these.)
func TestMathMatchesV8(t *testing.T) {
	checkEval(t, `[Math.log(5e-324), Math.log(1e-310), Math.log10(5e-324), Math.cosh(710), Math.sinh(-710),
		Math.cosh(710.4758600739439), 10 ** 308 === 1e308, 10 ** -307, 1.1 ** 1000, Math.pow(10, 308),
		Math.sumPrecise([1e308, 5e-324, -1e308]), Math.sumPrecise([0.1, 0.2, -0.3]),
		(-Infinity) ** 0.5, (-0) ** 0.5, Object.is((-0) ** 0.5, 0), 1 ** NaN, (-1) ** Infinity].join()`,
		"-744.4400719213812,-713.8013788281542,-323.3062153431158,1.1169973830808557e+308,-1.1169973830808557e+308,"+
			"1.7976931348621744e+308,true,1e-307,2.4699329180060256e+41,1e+308,5e-324,2.7755575615628914e-17,"+
			"Infinity,0,true,NaN,NaN")
	checkEval(t, `let seed = 7;
		const rnd = () => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed / 0x7fffffff; };
		const buf = new DataView(new ArrayBuffer(8));
		const rbits = () => { buf.setUint32(0, (rnd() * 2 ** 32) >>> 0); buf.setUint32(4, (rnd() * 2 ** 32) >>> 0); return buf.getFloat64(0); };
		const gens = [() => (rnd() - 0.5) * 20, () => (rnd() - 0.5) * 2, () => (rnd() - 0.5) * 2000, () => rbits(), () => rnd() * 1e-300, () => (rnd() - 0.5) * 1e6, () => 1 + (rnd() - 0.5) * 1e-6];
		const out = [];
		for (const f of ["acos","acosh","asin","asinh","atan","atanh","cbrt","cos","cosh","exp","expm1","log","log1p","log10","log2","sin","sinh","sqrt","tan","tanh"])
			for (let i = 0; i < 3000; i++) out.push(Math[f](gens[i % gens.length]()));
		for (const f of ["pow", "atan2"])
			for (let i = 0; i < 3000; i++) { const x = gens[i % gens.length](), y = gens[(i + 3) % gens.length](); out.push(Math[f](x, f === "pow" ? y / 10 : y)); }
		let h = 0; const all = out.join(","); for (let i = 0; i < all.length; i++) h = (h * 31 + all.charCodeAt(i)) | 0;
		out.length + " " + h`, "66000 -684820804")
	// Math.hypot sums its scaled squares with Kahan's compensation, as V8's
	// MathHypot does: √54 to the last bit, and 20000 vectors of one to five
	// numbers of every magnitude, node 26's bits.
	checkEval(t, `[Math.hypot(7, 1, 2), Math.hypot(1, 2, 3, 4, 5)].join()`, "7.348469228349534,7.416198487095663")
	checkEval(t, `let seed = 12345;
		const rnd = () => { seed = (Math.imul(seed, 1103515245) + 12345) | 0; return (seed >>> 8) / 16777216; };
		const pick = () => { const k = rnd();
			if (k < 0.2) return Math.floor(rnd() * 20) - 10;
			if (k < 0.4) return (rnd() - 0.5) * 100;
			if (k < 0.55) return (rnd() - 0.5) * 1e300;
			if (k < 0.7) return (rnd() - 0.5) * 1e-300;
			if (k < 0.8) return rnd() * 2 ** (Math.floor(rnd() * 200) - 100);
			return Math.floor(rnd() * 1000); };
		const f64 = new Float64Array(1), u32 = new Uint32Array(f64.buffer);
		const out = [];
		for (let i = 0; i < 20000; i++) {
			const xs = []; for (let j = 0; j <= i % 5; j++) xs.push(pick());
			f64[0] = Math.hypot(...xs); out.push(u32[1].toString(16) + '.' + u32[0].toString(16));
		}
		let h = 0x811c9dc5; const s = out.join(','); for (let k = 0; k < s.length; k++) h = Math.imul(h ^ s.charCodeAt(k), 16777619);
		out.length + " " + (h >>> 0)`, "20000 1701707555")
}
