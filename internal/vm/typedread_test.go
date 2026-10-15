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

// The tree tier appends an element at an array's length where setElem has
// none to store (setIndexMiss, appendElem), and leaves the rest to the slow
// path: an index a setter up the chain takes, a frozen array, a length made
// read-only, a prototype with an element, a sparse array and a strict
// function's refusals answer as the interpreter has them.
func TestTreeArrayAppends(t *testing.T) {
	const src = `
		const out = []
		function put(a, k, v) { a[k] = v; return a.length }
		function putStrict(a, k, v) { 'use strict'; try { a[k] = v; return a.length } catch (e) { return e.name } }
		function fill(a, n) { for (let i = a.length; i < n; i++) put(a, i, i); return a.join() }
		for (let r = 0; r < 3; r++) {
			out.push(fill([], 6), fill([9], 4))
			const frozen = Object.freeze([1, 2])
			out.push(put(frozen, 2, 3), putStrict(frozen, 2, 3), frozen.join())
			const fixed = [1, 2]
			Object.defineProperty(fixed, 'length', { writable: false })
			out.push(put(fixed, 2, 3), putStrict(fixed, 2, 3), fixed.join())
			const sparse = [1]; sparse[5] = 5
			out.push(put(sparse, 6, 6), sparse.join())
			const proto = [7, 8, 9], child = Object.setPrototypeOf([1], proto)
			out.push(put(child, 1, 2), child.join(), Object.keys(child).join())
			const log = []
			Object.defineProperty(Array.prototype, '3', { set(v) { log.push(v) }, configurable: true })
			const viaSetter = [0, 1, 2]
			const got = [put(viaSetter, 3, 'x'), viaSetter.length, log.join()]
			delete Array.prototype[3]
			out.push(got.join(), put(viaSetter, 3, 'y'), viaSetter.join())
		}
		out.join(' | ')`
	const want = "0,1,2,3,4,5 | 9,1,2,3 | 2 | TypeError | 1,2 | 2 | TypeError | 1,2 | 7 | 1,,,,,5,6 | " +
		"2 | 1,2 | 0,1 | 3,3,x | 4 | 0,1,2,y"
	for _, tree := range []bool{false, true} {
		previous := treeTier.Swap(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		treeTier.Store(previous)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		if got := v.String().Go(); got != want+" | "+want+" | "+want {
			t.Errorf("tree %v:\n%s\nwant three of\n%s", tree, got, want)
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

// An array's length set longer, by new Array(n) or by length itself, gives
// holes up to it, grown at once (setArrayLength): no index is present, the
// elements read undefined, what is written after is an element, and a
// length past the holes an array keeps in its storage makes it sparse, as
// before.
func TestArrayLengthGrowsWithHoles(t *testing.T) {
	const src = `
		const out = []
		for (const n of [0, 1, 4, 10, 1000, 2000]) {
			const a = new Array(n)
			out.push([a.length, 0 in a, (n - 1) in a, a[n - 1], Object.keys(a).length].join())
		}
		const b = [1, 2, 3]
		b.length = 7
		b[6] = 'x'
		out.push([b.length, 3 in b, 6 in b, b.join('-')].join())
		const c = [1, , 3]
		c.length = 5
		c.push(9)
		out.push([c.length, 1 in c, 4 in c, c.join('-')].join())
		const d = []
		d.length = 1e6
		d[5] = 5
		out.push([d.length, 5 in d, 4 in d, Object.keys(d).join()].join())
		const e = Object.freeze([1])
		try { e.length = 3 } catch (err) {}
		out.push(e.length)
		out.join(' | ')`
	const want = "0,false,false,,0 | 1,false,false,,0 | 4,false,false,,0 | 10,false,false,,0 | 1000,false,false,,0 | 2000,false,false,,0 | " +
		"7,false,true,1-2-3----x | 6,false,false,1--3---9 | 1000000,true,false,5 | 1"
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
