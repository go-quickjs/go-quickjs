(function () {
	var out = [];
	var log = [];
	function obj(name, v) { return { valueOf() { log.push(name); return v } } }
	function str(e) { return e instanceof Error ? e.constructor.name : String(e) }
	var vals = [0, -0, 1, -1, 5, 31, 32, 33, 0x7fffffff, -0x80000000, 0xffffffff, 4294967296, 2.5, -2.5, 1e20, NaN, Infinity, -Infinity, '7', ' 12 ', 'x', null, undefined, true];
	// Chains over a local: imm operators over an imm operator's int32.
	function chains(a, b, c) {
		return [((a ^ b) & c) >>> 8, (a >> 2) | 0, (a | 0) | 0, ((a >>> 0) >> 3), ((a << 3) >>> 1), (a & 255) ^ 0x55,
			((a | 0) + (b | 0)) | 0, ((a >> 1) ^ (b >> 1)) | 0, (a >>> 0) & (b >>> 0), ((a | 0) << b) >>> 0,
			(5 & (a | 0)), ((a | 0) >> (b | 0)) >>> 0, (a | 0) ^ b, a ^ (b | 0), ((a >>> 0) >>> 0) >>> 1]
	}
	for (var i = 0; i < vals.length; i++) {
		for (var j = 0; j < vals.length; j += 5) {
			out.push(chains(vals[i], vals[j], 0xf0f0).join(' '));
		}
	}
	// Comparisons of int32 operands, signed and unsigned.
	function cmps(a, b) {
		var r = '';
		if ((a | 0) < (b | 0)) r += 'a'; if ((a >>> 0) < (b >>> 0)) r += 'b'; if ((a >>> 0) < (b | 0)) r += 'c';
		if ((a | 0) <= 262) r += 'd'; if ((a >>> 0) >= 262) r += 'e'; if ((a | 0) > b) r += 'f'; if (a >= (b >>> 0)) r += 'g';
		if ((a | 0) == (b | 0)) r += 'h'; if ((a >>> 0) === (b | 0)) r += 'i'; if ((a | 0) != 0) r += 'j'; if (0 === (a & 1)) r += 'k';
		if ((a | 0) == b) r += 'l'; if (a === (b | 0)) r += 'm'; if ((a >>> 0) !== b) r += 'n'; if ((a | 0) == null) r += 'o';
		if ((a | 0) < 2.5) r += 'p'; if ((a >>> 0) > -1) r += 'q'; if ((a | 0) == NaN) r += 'r'; if ((a | 0) != NaN) r += 's';
		if ((a | 0) >= 0 / 0) r += 't'; if ((a >>> 0) < 1e400) r += 'u'; if ((a | 0) > -1e400) r += 'U'; if ((a | 0) >= 1e400) r += 'T'; if ((a | 0) > -2.5) r += 'v'; if ((a | 0) <= -2.5) r += 'w';
		if ((a >>> 0) <= 4294967295) r += 'x'; if ((a | 0) < 1.5) r += 'A'; if ((a | 0) >= 0.5) r += 'B'; if ((a | 0) > 0.5) r += 'C'; if ((a | 0) <= 0.5) r += 'D'; if ((a | 0) < -2147483648) r += 'y'; if ((a >>> 0) > 4294967294.5) r += 'z';
		return r;
	}
	for (var i = 0; i < vals.length; i++) {
		for (var j = 0; j < vals.length; j += 3) {
			out.push(cmps(vals[i], vals[j]));
		}
	}
	// BigInts: mixing throws a TypeError, at the operator, after both sides.
	function big(a, b) {
		var r = [];
		try { r.push((a | 0) ^ b) } catch (e) { r.push(str(e)) }
		try { r.push(((a >> 1) & b) >>> 0) } catch (e) { r.push(str(e)) }
		try { r.push((a ^ b) | 0) } catch (e) { r.push(str(e)) }
		try { r.push((b | 0) < (a >>> 0)) } catch (e) { r.push(str(e)) }
		try { r.push((a | 0) < b) } catch (e) { r.push(str(e)) }
		try { r.push((a | 0) == b) } catch (e) { r.push(str(e)) }
		try { r.push((a | 0) === b) } catch (e) { r.push(str(e)) }
		return r.join(' ');
	}
	out.push(big(5, 3n), big(5n, 3n), big(5n, 3), big(-1, 2n ** 70n));
	// valueOf: each called once, in order, the operator after both operands.
	function vo(a, b, c) {
		log = [];
		var r = [];
		r.push(((a ^ b) & c) >>> 8);
		r.push((a >> 1) | b);
		r.push((a | 0) < (b | 0));
		r.push((b | 0) == (a >>> 0));
		r.push((a | 0) == c);
		return r.join(' ') + ':' + log.join('');
	}
	out.push(vo(obj('a', 0x12345), obj('b', 0x7f), obj('c', 0xff00)));
	out.push(vo(obj('a', '9'), obj('b', 2.9), obj('c', -1)));
	try { vo(obj('a', 1), obj('b', 1n), obj('c', 1)) } catch (e) { out.push(str(e) + ':' + log.join('')) }
	var thrower = { valueOf() { log.push('t'); throw new RangeError('t') } };
	try { log = []; out.push((obj('a', 1) | 0) ^ thrower) } catch (e) { out.push(str(e) + ':' + log.join('')) }
	// Element reads under an operator and int32 keys: every typed array, an
	// array with holes and strings, a getter on its prototype, keys out of
	// range, negative, fractional, past 2^31.
	var buf = new ArrayBuffer(64);
	var heaps = [new Int8Array(buf), new Uint8Array(buf), new Uint8ClampedArray(buf), new Int16Array(buf), new Uint16Array(buf),
		new Int32Array(buf), new Uint32Array(buf), new Float32Array(buf), new Float64Array(buf), new BigInt64Array(buf)];
	new Int32Array(buf).set([0x7fffffff, -1, 0x12345678, -0x80000000, 3, 300, -300, 65537]);
	new Float64Array(buf, 32, 4).set([2.75, -1e10, NaN, -0]);
	function reads(h, p) {
		var r = [];
		try {
			r.push(h[p >> 2] | 0, h[(p + 1) | 0] & 255, h[p >>> 0] >> 1, h[p >> 2] >>> 0, h[p | 0] << 3, h[p] ^ 1);
			var F = h[p >> 1] | 0; r.push(F);
			var G; G = h[p & 7] >>> 0; r.push(G);
			r.push(h[p >> 2], h[(p - 9) | 0], h[p >>> 30]);
			if ((h[p >> 2] | 0) == (h[(p >> 2) + 0 | 0] | 0)) r.push('eq');
			if ((h[p | 0] | 0) < (h[(p + 1) | 0] | 0)) r.push('lt');
		} catch (e) { r.push(str(e)) }
		return r.map(String).join(' ');
	}
	for (var k = 0; k < heaps.length; k++) {
		for (var p = -3; p < 20; p += 4) out.push(reads(heaps[k], p));
	}
	var arr = [1, 2.5, , 'x', '4', { valueOf() { return 9 } }, -7, 3n];
	var proto = Object.create(Array.prototype);
	Object.defineProperty(proto, 2, { get() { log.push('g'); return 11 } });
	Object.setPrototypeOf(arr, proto);
	log = [];
	for (var p = 0; p < 9; p++) out.push(reads(arr, p));
	out.push(log.join(''));
	out.push(reads({ 0: 4, 1: '5', '-1': 'neg', 2: { valueOf() { log.push('e'); return 6 } } }, 1));
	out.push(reads('abc', 1), reads(null, 1));
	// Stores with int32 keys, every kind, values of every kind.
	function writes(h) {
		var r = [];
		var vs = [7, -1, 2.75, 300, 1e10, NaN, -0, '12', obj('w', 5)];
		for (var i = 0; i < vs.length; i++) {
			try {
				h[(i << 1) >> 1] = vs[i];
				h[(i >>> 0)] = vs[i];
				r.push(String(h[i | 0]));
			} catch (e) { r.push(str(e)) }
		}
		try { h[-1 | 0] = 5; r.push(h[-1], h['-1']) } catch (e) { r.push(str(e)) }
		try { h[(100 >>> 0)] = 5; r.push(h[100]) } catch (e) { r.push(str(e)) }
		return r.map(String).join(' ');
	}
	log = [];
	var hs = [new Int8Array(16), new Uint8Array(16), new Uint8ClampedArray(16), new Int16Array(16), new Uint16Array(16), new Int32Array(16),
		new Uint32Array(16), new Float32Array(16), new Float64Array(16), [], {}, Object.freeze([1, 2, 3])];
	for (var k = 0; k < hs.length; k++) out.push(writes(hs[k]));
	out.push(log.join(''));
	try { (function () { 'use strict'; var f = Object.freeze([1]); f[0 | 0] = 2 })() } catch (e) { out.push(str(e)) }
	// Upvalues and globals as the heap, as asm.js and Mandreel have them.
	var H = new Int32Array(16);
	function asm(n) {
		var s = 0;
		for (var i = 0; (i | 0) < (n | 0); i = i + 1 | 0) {
			H[(i & 15) >> 0] = (H[(i & 15) >> 0] | 0) + i | 0;
			s = s + (H[i & 15 | 0] >>> 0) | 0;
			s = (s ^ (s << 5)) >>> 0;
		}
		return s;
	}
	out.push(asm(1000), H.join(','));
	return out.join('\n');
})()
