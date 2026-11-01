package vm

import "testing"

// A BigInt64Array's and a BigUint64Array's elements are read and written on
// the fast path (typedRead, typedStore) where the BigInt is held in its
// Value, and converted from one without making a *BigInt where it is not
// (toBigIntOperandValue): each store keeps the low 64 bits, of a BigInt
// held in the Value and of one on the heap; a Number is refused with a
// TypeError, as a BigInt is by an Int32Array; DataView's and Atomics'
// BigInt stores and BigInt.asIntN convert alike. A pure body adds,
// subtracts, multiplies and orders BigInts an int64 holds itself
// (pureBigArith), and leaves the rest -- a result past the range, a BigInt
// beside a Number, which throws -- to its frame. Run on the tree and
// without it.
func TestBigIntElements(t *testing.T) {
	const src = `
		var out = []
		function stores(a, vals) {
			let _k = 0
			for (var i = 0; i < vals.length; i++) a[i] = vals[i]
			var s = []
			for (var i = 0; i < a.length; i++) s.push(String(a[i] + BigInt(_k)))
			return s.join(',')
		}
		var vals = [0n, -1n, 2n ** 63n - 1n, -(2n ** 63n), 2n ** 63n, 2n ** 64n - 1n, -(2n ** 64n) + 1n,
			2n ** 64n + 5n, -(2n ** 70n) - 3n, true, '12']
		for (var j = 0; j < 3; j++) {
			out.push(stores(new BigInt64Array(vals.length), vals))
			out.push(stores(new BigUint64Array(vals.length), vals))
		}
		function refuse(a, v) {
			try { a[0] = v; return 'stored ' + a[0] } catch (e) { return e.name }
		}
		out.push(refuse(new BigInt64Array(1), 1), refuse(new BigUint64Array(1), 1.5),
			refuse(new Int32Array(1), 1n), refuse(new Float64Array(1), 2n ** 64n))
		var dv = new DataView(new ArrayBuffer(16))
		dv.setBigInt64(0, -2n, true); dv.setBigUint64(8, 2n ** 64n + 7n)
		out.push([dv.getBigInt64(0, true), dv.getBigUint64(0, true), dv.getBigUint64(8)].join(','))
		try { dv.setBigInt64(0, 3) } catch (e) { out.push(e.name) }
		var sab = new BigInt64Array(new SharedArrayBuffer(16))
		out.push([Atomics.add(sab, 0, 5n), Atomics.sub(sab, 0, 2n ** 64n + 1n), sab[0], Atomics.store(sab, 1, -9n), sab[1]].join(','))
		out.push(BigInt.asIntN(8, 255n), BigInt.asUintN(8, '257'), BigInt.asIntN(64, 2n ** 63n))
		var big = new BigInt64Array([1n, -2n, 3n, 2n ** 62n, 2n ** 62n])
		var sum = 0n
		big.forEach((x) => { sum += x })
		out.push(sum, big.reduce((a, x) => a - x, 0n), big.map((x) => x * 3n).join(','),
			big.every((x) => x < 2n ** 62n + 1n), big.some((x) => x >= 2n ** 62n), big.findIndex((x) => x > 2n))
		var mixed = new BigInt64Array([1n, 2n])
		out.push(mixed.some((x) => x < 2), mixed.filter((x) => x == 2).join(','))
		try { mixed.forEach((x) => x + 1) } catch (e) { out.push(e.name) }
		var ubig = new BigUint64Array([2n ** 64n - 1n, 1n])
		var us = 0n
		ubig.forEach((x) => { us += x })
		out.push(us, ubig.map((x) => x * 2n).join(','))
		out.join(' | ')`
	const row1 = "0,-1,9223372036854775807,-9223372036854775808,-9223372036854775808,-1,1,5,-3,1,12"
	const row2 = "0,18446744073709551615,9223372036854775807,9223372036854775808,9223372036854775808,18446744073709551615,1,5,18446744073709551613,1,12"
	// QuickJS and Node give the same.
	const want = row1 + " | " + row2 + " | " + row1 + " | " + row2 + " | " + row1 + " | " + row2 + " | " +
		"TypeError | TypeError | TypeError | TypeError | -2,18446744073709551614,7 | TypeError | " +
		"0,5,4,-9,-9 | -1 | 1 | -9223372036854775808 | " +
		"9223372036854775810 | -9223372036854775810 | 3,-6,9,-4611686018427387904,-4611686018427387904 | " +
		"true | true | 2 | true | 2 | TypeError | 18446744073709551616 | 18446744073709551614,2"
	for _, tree := range []bool{true, false} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v:\n%s\nwant\n%s", tree, got, want)
		}
	}
}
