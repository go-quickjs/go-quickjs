//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// A typed array's element, read or written as a number, is native code's
// as an Array's is, of every type but Float16 and the BigInt ones: read
// and converted to a number, a store converted to the type -- ToInt32's
// low bits for an integer type, saturated and rounded half to even for
// Uint8Clamped, rounded to a float for Float32. What it does not make --
// an index the view does not have, a view out of its buffer, a detached
// or an immutable one, a view of a resizable buffer's length -- does what
// the interpreter does. Each answer is the interpreter's, in every tier
// and under every stress setting.
func TestJITTypedArrays(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		const log = []
		const values = [0, -0, 1, -1, 1.5, -1.5, 2.5, 3.5, 254.5, 255.5, 256, 300, -129, 127, 128, 32767, 32768, -32769,
			65535, 65536, 2147483647, 2147483648, -2147483649, 4294967295, 4294967296, 1e10, -1e10, 2**53, 2**63, -(2**63),
			2**64, 1e300, Infinity, -Infinity, NaN, 0.1, 1/3, 3.4028235677973366e38, 1e-46]
		function fill(a, n) { for (let i = 0; i < n; i++) a[i] = values[i % values.length]; return a }
		function sum(a, n) { let s = 0; for (let i = 0; i < n; i++) s += a[i]; return s }
		function each(a, n) { const out = []; for (let i = 0; i < n; i++) out.push(a[i]); return out.join() }
		function copy(d, s, n) { for (let i = 0; i < n; i++) d[i] = s[i] }
		function update(a, n) { for (let i = 0; i < n; i++) { a[i] += 3; a[i] = a[i] * 2 - 1 } }
		function reads(a, keys) { const out = []; for (let i = 0; i < keys.length; i++) out.push(String(a[keys[i]])); return out.join() }
		function writes(a, keys) { for (let i = 0; i < keys.length; i++) a[keys[i]] = i + 0.25 }
		const kinds = [Int8Array, Uint8Array, Uint8ClampedArray, Int16Array, Uint16Array, Int32Array, Uint32Array, Float32Array, Float64Array]
		if (typeof Float16Array === 'function') kinds.push(Float16Array)
		for (const K of kinds) {
			for (let round = 0; round < 3; round++) {
				const a = fill(new K(values.length * 2), values.length * 2)
				log.push(K.name + ' ' + each(a, a.length) + ' ' + sum(a, a.length))
				update(a, a.length)
				log.push(each(a, a.length))
				// A view at an offset into its buffer.
				const buf = new ArrayBuffer(64), sub = new K(buf, 8, 4)
				fill(sub, 4)
				log.push(each(new Uint8Array(buf), 64))
				// Keys the view does not have, which read undefined and store
				// nothing.
				log.push(reads(a, [0, -1, 1.5, NaN, a.length, a.length - 1, 2**32, -0, '1', 1e21]))
				writes(a, [-1, 0.5, a.length, 2**32, 3])
				log.push(each(a, 6))
			}
		}
		// Between kinds and into an Array, from one site.
		const mixed = [new Int8Array(8), new Float64Array(8), [1, 2, 3, 4, 5, 6, 7, 8], new Uint16Array(8)]
		for (let r = 0; r < 4; r++) for (const m of mixed) { fill(m, 8); log.push(sum(m, 8)) }
		const f64 = new Float64Array(16), u8 = new Uint8Array(16)
		for (let r = 0; r < 3; r++) { fill(f64, 16); copy(u8, f64, 16); log.push(each(u8, 16)) }
		// BigInt elements are Go's.
		const big = new BigInt64Array(4)
		for (let r = 0; r < 3; r++) { try { fill(big, 4) } catch (e) { log.push(e.name) } log.push(each(big, 4)) }
		// A buffer detached while a loop reads and writes it.
		function drain(a, n, at) { let s = 0; for (let i = 0; i < n; i++) { if (i === at) a.buffer.transfer(); s += a[i] | 0; a[i] = i } return s }
		for (let r = 0; r < 3; r++) { const a = fill(new Int32Array(32), 32); log.push(drain(a, 32, 10 + r), a.length) }
		// A resizable buffer, a view that tracks its length and one that does
		// not, shrunk under them.
		function shrink(rb, a, b, n) { let s = 0; for (let i = 0; i < n; i++) { if (i === 8) rb.resize(16); s += (a[i] | 0) + (b[i] | 0); a[i] = 1; b[i] = 2 } return s }
		for (let r = 0; r < 3; r++) {
			const rb = new ArrayBuffer(64, { maxByteLength: 128 })
			const a = fill(new Int16Array(rb), 32), b = fill(new Int16Array(rb, 0, 16), 16)
			log.push(shrink(rb, a, b, 32), a.length, b.length, each(new Uint8Array(rb), 16))
		}
		if (typeof ArrayBuffer.prototype.transferToImmutable === 'function') {
			for (let r = 0; r < 3; r++) {
				const im = new Uint8Array(fill(new Uint8Array(8), 8).buffer.transferToImmutable())
				writes(im, [0, 1, 2])
				log.push(each(im, 8))
			}
		}
		log.join('|')`
	jitDifferential(t, src)
}

// A loop over typed arrays runs natively: no guard of their elements'
// fails, and Go reads or writes none of them past the first, which looks
// at the view. Before, each typed array a site met failed its guard, the
// function was compiled again with the site Go's, and every read there
// was a call of Go: Octane's zlib, whose heap is typed arrays, ran several
// times slower with the JIT than without it.
func TestJITTypedArraysNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTest(t, `
		const heap = new ArrayBuffer(1 << 16), h8 = new Int8Array(heap), u8 = new Uint8Array(heap),
			h16 = new Int16Array(heap), u16 = new Uint16Array(heap), h32 = new Int32Array(heap),
			f32 = new Float32Array(heap), f64 = new Float64Array(heap)
		function kernel(n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				h32[i >> 2] = (h32[(i + 1) >> 2] + i) | 0
				u8[i] = h8[i + 7] + 1
				u16[i >> 1] = h16[(i >> 1) + 3] ^ i
				f64[i >> 3] = f32[i >> 2] * 0.5 + 1
				s = (s + h32[i >> 2] + u8[i] + u16[i >> 1]) | 0
			}
			return s + f64[1]
		}`, `kernel(4000)`, `kernel(4000)`, `kernel(4000)`)
	e := jitNativeForTest(t, r, "kernel")
	if e.ssaStats.guards != 0 || len(e.failed) != 0 {
		t.Errorf("guards failed: %d, at %v", e.ssaStats.guards, e.failed)
	}
	if e.ssaStats.goCalls > 8 || e.ssaStats.hosts > 8 {
		t.Errorf("%d calls of Go, %d exits", e.ssaStats.goCalls, e.ssaStats.hosts)
	}
}
