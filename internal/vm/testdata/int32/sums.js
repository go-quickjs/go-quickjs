(function () {
	var out = [];
	var log = [];
	function obj(name, v) { return { valueOf() { log.push(name); return v } } }
	function str(e) { return e instanceof Error ? e.constructor.name : String(e) }
	var vals = [0, -0, 1, -1, 7, 0x7fffffff, -0x80000000, 0xffffffff, 4294967296, 2.5, -2.5, 1e20, 2 ** 53, NaN, Infinity, '5', 'x', ' 3 ', null, undefined, true, [], [4]];
	// Sums under every operator, of locals, constants and int32 trees, in
	// both orders, stored and as operands.
	function sums(a, b) {
		var r = [];
		r.push((a + b) | 0, (a - b) | 0, (a + 1) | 0, (a - 1) >> 1, (1 + a) >>> 0, (262 - a) | 0, (a + 8) >> 2, (a - 0x80000000) >>> 0);
		r.push((a + (b | 0)) | 0, ((a | 0) + b) ^ 3, (a - (b >>> 0)) >>> 0, ((a >>> 0) - b) | 0, ((a | 0) + (b | 0)) | 0, ((a >>> 0) + (b >>> 0)) >>> 0,
			((a | 0) - (b | 0)) & 0xffff, ((a >>> 0) + 1) | 0, (1 + (a >>> 0)) >>> 0, ((a | 0) - 2.5) | 0, (2.5 - (a | 0)) | 0, ((a | 0) + 2 ** 60) | 0,
			(9007199254740991 - (a >>> 0)) | 0, ((a | 0) - 3) | 0, ((a >>> 0) - 1) >>> 0, (5 - (a >>> 0)) | 0, (3 + (a | 0)) >> 1, ((a | 0) << ((b | 0) + 1 | 0)) | 0);
		var C = a + b | 0; r.push(C);
		var D; D = a - 1 | 0; r.push(D);
		var E = (a >>> 0) + 7 >>> 0; r.push(E);
		var F = (a | 0) - (b | 0) >> 1; r.push(F);
		r.push(((a + b) | 0) + 1, ((a - b) >>> 0) / 2);
		if (((a + b) | 0) < 3) r.push('lt');
		return r.join(' ');
	}
	for (var i = 0; i < vals.length; i++) {
		for (var j = 0; j < vals.length; j += 2) {
			try { out.push(sums(vals[i], vals[j])) } catch (e) { out.push(str(e)) }
		}
	}
	// BigInts: the sum is a BigInt, the operator throws; a BigInt and a
	// number throw at the sum.
	function big(a, b) {
		var r = [];
		try { r.push((a + b) | 0) } catch (e) { r.push(str(e)) }
		try { r.push((a - 1) | 0) } catch (e) { r.push(str(e)) }
		try { r.push((a + (b | 0)) | 0) } catch (e) { r.push(str(e)) }
		try { var x = a + b | 0; r.push(x) } catch (e) { r.push(str(e)) }
		return r.join(' ');
	}
	out.push(big(1n, 2n), big(1n, 2), big(1, 2n));
	// valueOf and toString: once each, the sum's operands in order, then
	// the operator's conversion of a string sum.
	function vo(a, b) {
		log = [];
		var r = [(a + b) | 0, (b - a) >> 1, (1 + a) | 0, (a + (b | 0)) | 0];
		var s = a + b | 0;
		r.push(s);
		return r.join(' ') + ':' + log.join('');
	}
	out.push(vo(obj('a', 3), obj('b', 4)), vo(obj('a', '3'), obj('b', 4)), vo(obj('a', 2.5), obj('b', 0.5)));
	out.push(vo({ toString() { log.push('s'); return '6' }, valueOf: undefined }, obj('b', 1)));
	try { vo(obj('a', 1n), obj('b', 4)) } catch (e) { out.push(str(e) + ':' + log.join('')) }
	// A loop of them, as zlib has.
	var H = new Int8Array(64);
	for (var i = 0; i < 64; i++) H[i] = (i * 37) & 255;
	function match(b, B, o) {
		var C, n = 0;
		while (1) {
			C = b + 1 | 0;
			if ((H[C] | 0) != (H[B + 1 | 0] | 0)) break;
			b = C; B = B + 1 | 0; n = n + 1 | 0;
			if ((b | 0) >= (o | 0)) break;
		}
		return n;
	}
	out.push(match(0, 0, 60), match(3, 5, 60), match(10, 10, 20));
	return out.join('\n');
})()
