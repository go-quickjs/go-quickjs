//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
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
		// Inlined in move, or called natively from it: never left to Go by
		// its native callers. (Go's own calls of it, made while move's code
		// was invalidated and the tree tier ran it, may have judged it slow
		// for Go to enter, which is Go's alone: entrySlow.)
		if e := r.jitEntryOf(cl); e != nil && e.notNative {
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

// A method called by its index, obj[i](...) -- Emscripten's calls through
// its function table -- is compiled: the element read natively, the
// object the call's receiver. A function with one had been refused, and
// Mandreel's collision sorting ran in the tree tier. Each answer is the
// interpreter's, in every tier and under every stress setting: the
// callee's this the table, a hole, a function past the table's end, an
// object with an index for a key, and a table of getters.
func TestJITIndexedMethodCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		function f(x) { return x + 1 }
		function g(x) { return this.length * x }
		function h(x) { return this === T ? x * 3 : -1 }
		const T = [f, g, h, , f]
		const O = { 0: g, 1: h, length: 7 }
		const G = []; Object.defineProperty(G, 0, { get() { return f } })
		function run(t, n) { let s = 0; for (let i = 0; i < n; i++) { try { s += t[i % 4](i) } catch (e) { s += e instanceof TypeError ? 1000 : 0 } } return s }
		const log = []
		for (let r = 0; r < 30; r++) log.push(run(T, 40), run(O, 6), run(G, 1))
		try { log.push(run(T, 5) + T[9](1)) } catch (e) { log.push(e.name) }
		log.join()`
	jitDifferential(t, src)
}

// An element read whose result no native operation takes as a number --
// compared, passed to a call -- reads its element's cell (ir's Reference):
// one whose receivers are typed arrays, which have no cells, reads their
// number once it has met one (ssa.ElementFeedback), and Go reads any other
// receiver's. Each answer is the interpreter's: a typed array, an array
// of numbers and of objects at one site, past the end, a hole.
func TestJITTypedReferenceReads(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		const h = new Int32Array(64), u = new Uint16Array(64), f = new Float32Array(64)
		for (let i = 0; i < 64; i++) { h[i] = i * 7 - 100; u[i] = i * 1000; f[i] = i / 3 }
		const plain = [1, 'x', null, , {v: 2}, 7]
		function id(x) { return x }
		function read(a, n) {
			let s = 0, k = 0
			for (let i = 0; i < n; i++) {
				const x = a[i]
				if (x !== 0) k++
				const y = id(a[i + 1])
				s += typeof y === 'number' ? y : String(y).length
			}
			return s * 1000 + k
		}
		const log = []
		for (let r = 0; r < 30; r++) log.push(read(h, 63), read(u, 64), read(f, 40), read(plain, 8))
		log.join()`
	jitDifferential(t, src)
}

// The reads run natively once learned: Go reads none of a typed array's.
func TestJITTypedReferenceReadsNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	const setup = `
		const h = new Int32Array(256)
		for (let i = 0; i < 256; i++) h[i] = i & 3
		function count(n) { let k = 0; for (let i = 0; i < n; i++) { const x = h[i & 255]; if (x !== 0) k++ } return k }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := r.Run(compileForTest(t, `count(1000)`)); err != nil {
			t.Fatal(err)
		}
	}
	e := jitNativeForTest(t, r, "count")
	entries, in, hosts := e.ssaStats.entries, e.link.in, e.ssaStats.hosts
	if _, err := r.Run(compileForTest(t, `for (let k = 0; k < 20; k++) count(1000)`)); err != nil {
		t.Fatal(err)
	}
	// Entered by Go, or called by the loop's own native code.
	if n := e.ssaStats.entries - entries + e.link.in - in; n < 20 {
		t.Fatalf("count ran natively %d times of 20", n)
	}
	if n := e.ssaStats.hosts - hosts; n > 2 {
		t.Errorf("count left native code %d times in 20,000 reads", n)
	}
}

// A string read from an element, then a number written over that element
// (ssa.OpElemWrite), the string used after: native code holds the
// string's number word, its pointer word in the element's cell until an
// exit reads it. The write leaves to Go what native code does not do --
// an element that is no number, an array to grow -- and Go's store wrote
// over the cell, or moved the elements as it grew the array, so the next
// exit made a string with no pointer, which the interpreter's + met in
// concat. FuzzJITDifferential found it in CI; it predated the stores'
// checks (storeChecks), which this write had none of. Each answer is the
// interpreter's, in every tier and under every stress setting.
func TestJITNumberWriteOverReadString(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, src := range []string{
		`function f(a,n){let t='';let r='';for(let i=0;i<n;i++){t=a[i];a[i]=i;r=r+t;}return r}
		const out=[];for(let k=0;k<6;k++)out.push(f(['a','b','c','d','e','f','g','h'],8));out.join('|')`,
		// A fraction and the string: + converts both (concat).
		`const log=[];
		function f(a,o,n){let s=0.5,t=1;for(let i=0;i<n;i++){t=a[i];a[i]=i;s=s+t;}return [s,t]}
		function run(a,o,n){try{const r=f(a,o,n);log.push(String(r[0])+','+String(r[1]))}catch(e){log.push(e.name)}log.push(a.join(','))}
		for(let k=0;k<4;k++)run(['3','3','3','3','3','3','3','3'],{x:1},8);
		log.join('|')`,
		// The array grown past its capacity by the write, the string read
		// from it before.
		`function f(a,n){let t='';let r=0.5;for(let i=0;i<n;i++){t=a[i>>2];a[a.length]=i;r=r+t;}return r}
		const out=[];for(let k=0;k<20;k++)out.push(f(['x','y','z'],300).length);out.join('|')`,
	} {
		jitDifferential(t, src)
	}
}

// Element reads of globals that hold typed arrays -- a var's, a let's --
// read a typed array's number wherever the code compiled them, and Go's
// read stands in once a binding holds something else: a plain array, an
// array of holes and strings, a typed array of another type, a typed
// array detached... Each answer is the interpreter's, in every tier and
// under every stress setting.
func TestJITTypedGlobals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		var heap32 = new Int32Array(64), heapU16 = new Uint16Array(heap32.buffer)
		let heapF = new Float64Array(16)
		for (let i = 0; i < 64; i++) heap32[i] = i * 77 - 900
		for (let i = 0; i < 16; i++) heapF[i] = i / 4
		function step(k, n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				const j = (i + k) & 31
				if (k === 0) { if (heap32[j] > 0) s++ }
				else if (k === 1) { if (heapU16[j * 2] !== 0) s += 2 }
				else if (k === 2) { const x = heapF[j & 15]; if (x === 1.5) s += 3 }
				else s += String(heap32[j]).length
			}
			return s
		}
		const log = []
		for (let r = 0; r < 12; r++) for (let k = 0; k < 4; k++) log.push(step(k, 40))
		heap32 = [1, -2, 3]; heapU16 = new Float64Array(80).fill(0.5); heapF = ['a', , 1.5]
		for (let r = 0; r < 4; r++) for (let k = 0; k < 4; k++) log.push(step(k, 40))
		const buf = new ArrayBuffer(256); heap32 = new Int32Array(buf); heapU16 = new Uint16Array(buf)
		for (let k = 0; k < 4; k++) log.push(step(k, 40))
		buf.transfer()
		for (let k = 0; k < 4; k++) log.push(step(k, 40))
		log.join()`
	jitDifferential(t, src)
}

// A function that reads a global's typed array in each of its branches,
// one branch an invocation, is compiled for all of them at once: each read
// is a typed array's from the first compile. Learned at each read's exit
// instead, it had been compiled again once for each branch, as Mandreel's
// sorts were, a dozen times each -- 40% of its time.
func TestJITTypedGlobalsNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	const setup = `
		var heap32 = new Int32Array(256), heapU8 = new Uint8Array(heap32.buffer)
		for (let i = 0; i < 256; i++) heap32[i] = i & 3
		function branches(k, n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				switch (k) {
				case 0: if (heap32[i & 255] !== 0) s++; break
				case 1: if (heap32[(i * 3) & 255] === 2) s++; break
				case 2: if (heapU8[i & 1023] === 1) s++; break
				case 3: if (heap32[(i + 7) & 255] === heap32[i & 255]) s++; break
				case 4: if (heapU8[(i * 5) & 1023] !== 3) s++; break
				case 5: if (heap32[(i >> 1) & 255] === 1) s++; break
				}
			}
			return s
		}`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(compileForTest(t, `branches(0, 1000)`)); err != nil {
		t.Fatal(err)
	}
	e := jitNativeForTest(t, r, "branches")
	reopts, hosts := e.reopts, e.ssaStats.hosts
	for k := 1; k < 6; k++ {
		if _, err := r.Run(compileForTest(t, fmt.Sprintf(`branches(%d, 1000)`, k))); err != nil {
			t.Fatal(err)
		}
	}
	if e != jitNativeForTest(t, r, "branches") {
		t.Fatal("branches' entry was made again")
	}
	if n := e.reopts - reopts; n != 0 {
		t.Errorf("branches was compiled again %d times for its branches' reads", n)
	}
	if n := e.ssaStats.hosts - hosts; n > 2 {
		t.Errorf("branches left native code %d times in 5,000 reads", n)
	}
}

// A property read or write whose site met more shapes than it is compiled
// for, of objects too large to search, finds the property in the
// megamorphic cache (abi.MegaCache), as V8's stub cache: shapes of many
// properties each, the property at an index of each one's own, read and
// written at one site, a read-only property, an accessor, one on a
// prototype, one deleted, one frozen object. Each answer is the
// interpreter's, in every tier and under every stress setting.
func TestJITMegamorphicProperties(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		const objs = []
		for (let k = 0; k < 24; k++) {
			const o = {}
			for (let j = 0; j < k; j++) o['e' + j] = 0
			for (let j = 0; j < 12; j++) o['p' + ((j + k) % 12)] = j
			o.kind = k
			objs.push(o)
		}
		Object.defineProperty(objs[3], 'kind', { value: 3, writable: false })
		Object.defineProperty(objs[4], 'kind', { get() { return 40 }, set(v) {} })
		const proto = { kind: 50 }
		const inh = Object.create(proto); for (let j = 0; j < 12; j++) inh['q' + j] = j
		objs.push(inh)
		delete objs[5].kind
		Object.freeze(objs[6])
		function sum(n) { let s = 0; for (let i = 0; i < n; i++) { const o = objs[i % objs.length]; s += (o.kind | 0) + (o.p3 | 0); o.kind = (o.kind | 0) + 1 } return s }
		const log = []
		for (let r = 0; r < 20; r++) log.push(sum(250))
		log.push(objs.map(o => String(o.kind)).join(':'))
		log.join()`
	jitDifferential(t, src)
}

// The megamorphic cache names an object's own plain data property for its
// shape, at the property's index, writable if it is; never an accessor's,
// nor a small object's, which native code searches itself.
func TestJITMegamorphicCacheFills(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	v, err := r.Run(compileForTest(t, `
		const big = {}; for (let j = 0; j < 12; j++) big['p' + j] = j
		Object.defineProperty(big, 'ro', { value: 1, writable: false })
		Object.defineProperty(big, 'acc', { get() { return 2 } })
		const small = { a: 1 }
		;[big, small]`))
	if err != nil {
		t.Fatal(err)
	}
	r.jitState()
	if r.jit.mega == nil {
		r.jit.mega = new(abi.MegaCache)
	}
	pair := v.Object().elemsOf()
	big, small := pair[0].Object(), pair[1].Object()
	find := func(o *Object, name string) (abi.MegaEntry, bool) {
		key := r.atoms.intern(name)
		set := r.jit.mega[abi.MegaSet(uintptr(unsafe.Pointer(o.shape)), uint32(key)):][:abi.MegaWays]
		for _, m := range set {
			if m.Shape == unsafe.Pointer(o.shape) && m.Key == uint32(key) {
				return m, true
			}
		}
		return abi.MegaEntry{}, false
	}
	for _, name := range []string{"p5", "ro", "acc"} {
		r.jit.megaFill(big, r.atoms.intern(name))
	}
	r.jit.megaFill(small, r.atoms.intern("a"))
	if m, ok := find(big, "p5"); !ok || m.Index != uint32(big.findOwn(r.atoms.intern("p5")))|abi.MegaWritable {
		t.Errorf("p5: %+v, %v", m, ok)
	}
	if m, ok := find(big, "ro"); !ok || m.Index&abi.MegaWritable != 0 || m.Index != uint32(big.findOwn(r.atoms.intern("ro"))) {
		t.Errorf("ro: %+v, %v", m, ok)
	}
	if _, ok := find(big, "acc"); ok {
		t.Error("an accessor in the cache")
	}
	if _, ok := find(small, "a"); ok {
		t.Error("a small object's property in the cache")
	}
	if !big.shape.seen {
		t.Error("a shape the cache names not remembered")
	}
}
