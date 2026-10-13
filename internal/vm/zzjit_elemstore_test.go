//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// An element is stored natively whatever the value -- an object, a string,
// null, a boolean, a number over a reference or a reference over a number
// -- as a property is (OpElemStore). What native code does not store --
// a hole, past the dense elements, a frozen or sealed array's element, an
// index with a setter up the chain, a typed array's element converted from
// an object, whose valueOf runs -- does what the interpreter does; a value
// read from an element before a store to it is the value it read. Each
// answer is the interpreter's, in every tier and under every stress
// setting.
func TestJITElementStores(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		const log = []
		const A = {v: 3}, B = {v: 5}
		const values = [A, 'str', null, undefined, true, 1.5, B, -0, 'x'.repeat(3), A]
		function fill(a, n) { for (let i = 0; i < n; i++) a[i] = values[i % values.length]; return a }
		function show(a) { const out = []; for (let i = 0; i < a.length; i++) { const x = a[i]; out.push(x === null ? 'null' : typeof x === 'object' ? 'o' + x.v : String(x)) } return out.join() }
		function swap(a, i, j) { const x = a[i]; a[i] = a[j]; a[j] = x; return x }
		function overwrite(a, i, y) { const x = a[i]; a[i] = y; return x && typeof x === 'object' ? x.v : x }
		function grow(a, n) { for (let i = 0; i < n; i++) a[a.length] = (i & 1) ? A : 'g' + i; return a.length }
		function sparse(a, n) { for (let i = 0; i < n; i++) a[i * 3] = B; return a.length }
		for (let r = 0; r < 4; r++) {
			const a = fill(new Array(20), 20)
			log.push(show(a))
			for (let i = 0; i < 19; i++) swap(a, i, 19 - i)
			log.push(show(a))
			const b = fill([], 10)
			log.push(overwrite(b, 0, 7), overwrite(b, 0, B), overwrite(b, 0, null), overwrite(b, 1, A), show(b))
			log.push(grow([], 9), sparse([], 6))
			const holes = [A, , B, , A]
			fill(holes, 5)
			log.push(show(holes))
			const frozen = Object.freeze([A, B, 1])
			try { fill(frozen, 3) } catch (e) { log.push(e.name) }
			log.push(show(frozen))
			const sealed = Object.seal([A, B, 1])
			fill(sealed, 5)
			log.push(show(sealed), sealed.length)
			const ta = new Float64Array(4)
			let calls = 0
			const o = {valueOf() { calls++; return 2.5 }}
			for (let i = 0; i < 4; i++) ta[i] = i & 1 ? o : i
			log.push(Array.from(ta).join(), calls)
		}
		Object.defineProperty(Array.prototype, 7, {set(v) { log.push('set ' + (v && v.v)) }, configurable: true})
		const p = fill(new Array(9), 9)
		delete Array.prototype[7]
		log.push(show(p))
		log.join('|')`
	jitDifferential(t, src)
}

// Arrays of objects are written natively: pushing and popping disks onto
// piles, as Are We Fast Yet's Towers does, leaves no store to Go, while
// the collector marks too, where Go makes the stores with its write
// barrier. Before, each store of an object left native code: the
// functions were judged slow, and every call of them from their native
// callers finished the callers' frames in the interpreter.
func TestJITElementStoresNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	const setup = `
		function Disk(size) { this.size = size; this.next = null }
		const piles = [null, null, null]
		function push(disk, pile) { const top = piles[pile]; disk.next = top; piles[pile] = disk }
		function pop(pile) { const top = piles[pile]; piles[pile] = top.next; top.next = null; return top }
		function move(n) {
			let moves = 0
			for (let i = 0; i < n; i++) { const from = i % 3, to = (i + 1) % 3; if (piles[from] !== null) { push(pop(from), to); moves++ } }
			return moves
		}
		function build(n) { for (let i = 0; i < 3; i++) piles[i] = null; for (let i = n; i > 0; i--) push(new Disk(i), 0); return n }`
	r := jitAgreesForTest(t, setup, `build(50)`, `move(2000)`, `move(2000)`, `move(2000)`)
	for _, name := range []string{"push", "pop"} {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		// Inlined in move, or called natively from it, and never judged
		// slow.
		if e := r.jitEntryOf(cl); e != nil && (e.entrySlow || e.notNative) {
			t.Fatalf("%s does not run natively: %+v", name, e)
		}
	}
	e := jitNativeForTest(t, r, "move")
	hosts := e.ssaStats.hosts
	if _, err := r.Run(compileForTest(t, `move(2000)`)); err != nil {
		t.Fatal(err)
	}
	if n := e.ssaStats.hosts - hosts; n > 4 {
		t.Errorf("move left native code %d times in 2000 moves", n)
	}
	// While the collector marks, the stores are Go's, and the disks stay
	// whole.
	jitMarkingForTest(t)
	if _, err := r.Run(compileForTest(t, `move(2000)`)); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	v, err := r.Run(compileForTest(t, `let s = 0; for (const p of piles) for (let d = p; d !== null; d = d.next) s += d.size; String(s)`))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != "1275" {
		t.Errorf("the disks' sizes sum to %s, want 1275", got)
	}
}

// A typed array's element stored from a value not known to be a number --
// a parameter -- is stored natively when it is one, converted to the
// element's type, as a number's store is: before, such a store, any
// value's, was Go's at every element, and Octane's zlib and Gameboy ran a
// twentieth slower for it.
func TestJITElementStoresTypedFromAnyValue(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	r := jitAgreesForTest(t, `
		const h = new Int32Array(64), u = new Uint8Array(64), d = new Float64Array(64)
		// Capturing stores, it is not inlined: its x is a parameter's.
		var put = (() => { let stores = 0; return function put(a, i, x) { stores++; a[i] = x } })()
		function fill(n) { for (let i = 0; i < n; i++) { put(h, i & 63, i * 3); put(u, i & 63, i); put(d, i & 63, i / 4) } return h[5] + u[7] + d[9] }`,
		`fill(1000)`, `fill(1000)`, `fill(1000)`, `[h[1], u[2], d[3], h.length].join()`)
	cl := r.global.getOwn(r.atoms.intern("put")).value().Object().fn().closure
	e := r.jitEntryOf(cl)
	if e == nil || e.ssa == nil {
		t.Fatalf("put has no code: %+v", e)
	}
	in, calls := e.link.in, 0
	jitGoCallWork = func() { calls++ }
	defer func() { jitGoCallWork = nil }()
	if _, err := r.Run(compileForTest(t, `fill(1000)`)); err != nil {
		t.Fatal(err)
	}
	if e.link.in-in < 3000 {
		t.Fatalf("put was called natively %d times of 3000", e.link.in-in)
	}
	if calls > 4 {
		t.Errorf("Go was called %d times for 3000 stores", calls)
	}
}

// A comparison an arithmetic or bitwise operator takes is a number, 1 or
// 0, natively (ssa.OpBoolToF64, OpBoolToI32): Emscripten's code combines
// its comparisons with | and &, and Octane's zlib had left native code at
// two such operators of every call of its hottest functions. Each answer
// is the interpreter's, in every tier and under every stress setting.
func TestJITBooleansAsNumbers(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		function f(a, b, c, d) {
			const x = (a | 0) < (b | 0) | (c | 0) > (d | 0)
			const y = (a === b) & (c !== d)
			const z = ((a < c) + (b < d)) * 3 - (a > d)
			const w = ((a <= b) ^ (c >= d)) << 2 | !(a & 1)
			const u = ((a < b) >>> 0) + ((c < d) >> 1) - -(a === 0)
			return x * 1000 + y * 100 + z * 10 + w + u * 7
		}
		let s = 0
		for (let i = 0; i < 4000; i++) s = (s * 31 + f(i & 7, (i >> 3) & 7, i % 5, (i * 7) % 9)) | 0
		String(s)`
	jitDifferential(t, src)
}

// Comparisons combined by | and &, as Emscripten's code combines them,
// run natively: the operator's operands, booleans, are taken as numbers
// in its code, not by Go at an exit (TestJITBooleansAsNumbers' answers).
func TestJITBooleansAsNumbersNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	const setup = `
		function f(a, b, c, d) { let n = 0; for (let i = 0; i < 100; i++) n = (n + ((a | 0) < (i | 0) | (c | 0) > (d | 0)) + ((a === i) & (c !== d))) | 0; return n }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if _, err := r.Run(compileForTest(t, `f(3, 4, 5, 6)`)); err != nil {
			t.Fatal(err)
		}
	}
	e := jitNativeForTest(t, r, "f")
	entries, hosts := e.ssaStats.entries, e.ssaStats.hosts
	if _, err := r.Run(compileForTest(t, `for (let k = 0; k < 50; k++) f(k, 4, k & 7, 6)`)); err != nil {
		t.Fatal(err)
	}
	// Entered at each call: one that left at each was judged not to pay,
	// and the tree tier ran it.
	if n := e.ssaStats.entries - entries; n < 50 {
		t.Fatalf("f ran natively %d times of 50", n)
	}
	if n := e.ssaStats.hosts - hosts; n > 5 {
		t.Errorf("f left native code %d times in 5,000 iterations", n)
	}
}

