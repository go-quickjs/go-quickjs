package vm

import "testing"

// The tree tier reads a typed array's element straight from its buffer
// (typedRead), and leaves the rest to getElem: every kind of element, keys
// past the end, fractions and negatives, a detached buffer, a resizable one
// shrunk under a view of a fixed length or tracked by one, answer as the
// interpreter has them.
func TestTreeTypedArrayReads(t *testing.T) {
	const src = `
		const out = []
		function at(a, k) { return a[k] }
		function show(v) { return typeof v === 'bigint' ? v + 'n' : Object.is(v, -0) ? '-0' : String(v) }
		function each(a, keys) { for (let r = 0; r < 3; r++) for (const k of keys) out.push(show(at(a, k))) }
		const buf = new ArrayBuffer(16)
		new Uint8Array(buf).set([255, 1, 128, 0, 0, 0, 192, 127, 0, 0, 0, 0, 0, 0, 240, 63])
		const keys = [0, 1, 2, 3, 4.5, -1, 1e10, 100]
		for (const C of [Int8Array, Uint8Array, Uint8ClampedArray, Int16Array, Uint16Array, Int32Array, Uint32Array,
			Float32Array, Float64Array, BigInt64Array, BigUint64Array]) each(new C(buf), keys)
		each(new Uint8Array(buf, 4, 4), keys)
		const rab = new ArrayBuffer(8, { maxByteLength: 16 })
		const fixed = new Uint8Array(rab, 0, 8), tracking = new Uint8Array(rab)
		fixed[7] = 7; tracking[3] = 3
		each(fixed, [3, 7]); each(tracking, [3, 7])
		rab.resize(4)
		each(fixed, [3, 7]); each(tracking, [3, 7])
		const doomed = new Uint8Array(new ArrayBuffer(4)); doomed[1] = 1
		each(doomed, [1])
		doomed.buffer.transfer()
		each(doomed, [1])
		out.join()`
	var want string
	for _, tree := range []bool{false, true} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		got := v.String().Go()
		if !tree {
			want = got
			continue
		}
		if got != want {
			t.Errorf("tree tier:\n%s\ninterpreter:\n%s", got, want)
		}
	}
}

// The tree tier stores a number into a typed array's buffer as it finds
// one (setElem, typedWrite), and leaves the rest to the slow path: every
// kind of element, keys past the end, fractions and NaN, values that are
// not numbers and convert, a detached buffer, answer as the interpreter has
// them.
func TestTreeTypedArrayWrites(t *testing.T) {
	const src = `
		const out = []
		function put(a, k, v) { a[k] = v; return a[k] }
		function show(v) { return typeof v === 'bigint' ? v + 'n' : Object.is(v, -0) ? '-0' : String(v) }
		const vals = [1, -1, 300.7, -0, NaN, Infinity, 2147483647, 2147483648, -2147483649, 4294967301, 2 ** 63, '7', true, { valueOf() { return 9 } }]
		for (const C of [Int8Array, Uint8Array, Uint8ClampedArray, Int16Array, Uint16Array, Int32Array, Uint32Array,
			Float32Array, Float64Array]) {
			const a = new C(4)
			for (let r = 0; r < 3; r++) for (const k of [0, 3, 4, 1.5, NaN, -1]) for (const v of vals) out.push(show(put(a, k, v)))
			out.push(Array.from(a, show).join(' '))
		}
		const b = new BigInt64Array(2)
		for (let r = 0; r < 3; r++) { out.push(show(put(b, 0, 5n))); try { put(b, 1, 5) } catch (e) { out.push(e.name) } }
		const doomed = new Uint8Array(4)
		for (let r = 0; r < 3; r++) out.push(show(put(doomed, 1, 2)))
		doomed.buffer.transfer()
		for (let r = 0; r < 3; r++) out.push(show(put(doomed, 1, 3)))
		out.join()`
	var want string
	for _, tree := range []bool{false, true} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		got := v.String().Go()
		if !tree {
			want = got
			continue
		}
		if got != want {
			t.Errorf("tree tier:\n%s\ninterpreter:\n%s", got, want)
		}
	}
}
