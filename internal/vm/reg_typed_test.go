package vm

import "testing"

// TestRegTypedElements pins get_index's and set_index's typed array
// elements, which the cases read and write from the view's data pointer
// (regTypedGet, regTypedSet): every kind, an index out of the view, a key
// that is no index, a store of a number no integer kind holds exactly,
// a BigInt kind's element, a constant key's read and store (get_index_imm, set_index_imm), a
// NaN's bits as a Float64Array holds them, a buffer detached or shrunk
// under a view the loop has read, and a view of an immutable buffer.
func TestRegTypedElements(t *testing.T) {
	const src = `
		function rd(a, k) { return a[k] }
		function wr(a, k, v) { a[k] = v; return a[k] }
		function each(C) {
			var a = new C(4), s = [];
			for (var i = 0; i < 4; i++) a[i] = i * 100 - 150;
			for (var i = 0; i < 4; i++) s.push(rd(a, i));
			s.push(rd(a, 4), rd(a, -1), rd(a, 1.5), rd(a, -0), wr(a, 1, 2.5), wr(a, 2, NaN),
				wr(a, 3, 4294967295), wr(a, 0, -2147483649), wr(a, 9, 1));
			return C.name + ":" + s.join("|");
		}
		var kinds = [Int8Array, Uint8Array, Uint8ClampedArray, Int16Array, Uint16Array,
			Int32Array, Uint32Array, Float32Array, Float64Array];
		var out = kinds.map(each);
		var bi = new BigInt64Array([-5n, 2n ** 63n - 1n]), bu = new BigUint64Array([2n ** 64n - 1n]);
		out.push([rd(bi, 0), rd(bi, 1), rd(bu, 0), rd(bi, 2), bi[1] === 2n ** 63n - 1n].join("|"));
		out.push(kinds.map(function (C) { var a = new C(3); a[1] = 300; a[2] = -5.5; return a[1] + "/" + a[2] }).join(","));
		var dv = new DataView(new ArrayBuffer(8));
		dv.setUint32(0, 0x12345678, true); dv.setUint32(4, 0xfff12345, true);
		var f = new Float64Array(dv.buffer), x = rd(f, 0);
		out.push(typeof x + ":" + Object.is(x, NaN) + ":" + (x !== x));
		var buf = new ArrayBuffer(16, { maxByteLength: 32 }), u = new Uint32Array(buf);
		for (var i = 0; i < 4; i++) wr(u, i, i + 7);
		buf.resize(8);
		var shrunk = [rd(u, 1), rd(u, 2), wr(u, 3, 5), u.length];
		buf.resize(16);
		shrunk.push(rd(u, 2), rd(u, 3));
		out.push(shrunk.join("|"));
		var d = new Int32Array(4); wr(d, 1, 9);
		var moved = d.buffer.transfer();
		out.push([rd(d, 1), wr(d, 1, 3), d.length, new Int32Array(moved)[1]].join("|"));
		if (typeof ArrayBuffer.prototype.transferToImmutable === "function") {
			var im = new Int32Array(new Int32Array([1, 2]).buffer.transferToImmutable());
			out.push(wr(im, 0, 5) + "|" + rd(im, 1));
		}
		out.join(" ")`
	const want = "Int8Array:106|-50|50|-106||||106|2|0|-1|-1|" +
		" Uint8Array:106|206|50|150||||106|2|0|255|255|" +
		" Uint8ClampedArray:0|0|50|150||||0|2|0|255|0|" +
		" Int16Array:-150|-50|50|150||||-150|2|0|-1|-1|" +
		" Uint16Array:65386|65486|50|150||||65386|2|0|65535|65535|" +
		" Int32Array:-150|-50|50|150||||-150|2|0|-1|2147483647|" +
		" Uint32Array:4294967146|4294967246|50|150||||4294967146|2|0|4294967295|2147483647|" +
		" Float32Array:-150|-50|50|150||||-150|2.5|NaN|4294967296|-2147483648|" +
		" Float64Array:-150|-50|50|150||||-150|2.5|NaN|4294967295|-2147483649|" +
		" -5|9223372036854775807|18446744073709551615||true" +
		" 44/-5,44/251,255/0,300/-5,300/65531,300/-5,300/4294967291,300/-5.5,300/-5.5" +
		" number:true:true 8|||2|0|0 ||0|9 1|2"
	regRunBoth(t, src, want, 3)
}
