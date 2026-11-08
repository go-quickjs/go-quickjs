//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"math"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// typedKindNames are the typed arrays' constructors of the element types
// native code reads and writes, in abi's Elem order but Float16's.
var typedKindNames = []string{"Int8Array", "Uint8Array", "Uint8ClampedArray", "Int16Array", "Uint16Array", "Int32Array", "Uint32Array", "Float32Array", "Float64Array"}

// typedKindSetup makes, for each element type native code reads and
// writes, functions of their own -- each its own bytecode and so its own
// sites -- that read and write typed arrays of that type: each site meets
// one element type, which it learns as native code runs
// (zzjit_typedkind.go) and is compiled again for, its element read or
// written inline with no other type's case.
var typedKindSetup = func() string {
	var b strings.Builder
	b.WriteString(`
	var values = [0, -0, 1, -1, 1.5, -1.5, 2.5, 3.5, 254.5, 255.5, 256, 300, -129, 127, 128, 32767, 32768, -32769,
		65535, 65536, 2147483647, 2147483648, -2147483649, 4294967295, 4294967296, 1e10, -1e10, 2**53, 2**63, -(2**63),
		2**64, 1e300, Infinity, -Infinity, NaN, 0.1, 1/3, 3.4028235677973366e38, 1e-46, -1e-46, 0.5, -0.5, 1.5e-45]
	var kinds = [Int8Array, Uint8Array, Uint8ClampedArray, Int16Array, Uint16Array, Int32Array, Uint32Array, Float32Array, Float64Array]
`)
	for _, n := range typedKindNames {
		fmt.Fprintf(&b, `
	function fill%[1]s(a, n, v) { for (let i = 0; i < n; i++) a[i] = v[i %% v.length]; return a }
	function sum%[1]s(a, n) { let s = 0; for (let i = 0; i < n; i++) s += a[i]; return s }
	function each%[1]s(a, n) { const out = []; for (let i = 0; i < n; i++) out.push(a[i]); return out.join() }
	function update%[1]s(a, n) { for (let i = 0; i < n; i++) { a[i] += 3; a[i] = a[i] * 2 - 1 } }
	function reads%[1]s(a, ks) { const out = []; for (let i = 0; i < ks.length; i++) out.push(String(a[ks[i]])); return out.join() }
	function writes%[1]s(a, ks) { for (let i = 0; i < ks.length; i++) a[ks[i]] = i + 0.25 }
`, n)
	}
	b.WriteString(typedKindRest)
	return b.String()
}()

const typedKindRest = `
	// A site that met one type, then another, then Arrays; and one of a
	// view whose buffer is detached as it runs.
	function poly(a, n) { let s = 0; for (let i = 0; i < n; i++) s += a[i]; return s }
	function polyw(a, n) { for (let i = 0; i < n; i++) a[i] = i * 1.75 - 3; return a }
	function drain(a, n, at) { let s = 0; for (let i = 0; i < n; i++) { if (i === at) a.buffer.transfer(); s += a[i] | 0; a[i] = i } return s }
	// A global's typed array, compiled for its type before its sites learn
	// any, then replaced by one of another type.
	var heap = new Int32Array(64)
	function gfill(n) { for (let i = 0; i < n; i++) heap[i & 63] = i * 3.5 - 50 }
	function gsum(n) { let s = 0; for (let i = 0; i < n; i++) s += heap[i & 63]; return s }
	function kindsRound() {
		const out = []
		for (const K of kinds) {
			const n = K.name, a = new K(values.length * 2)
			globalThis['fill' + n](a, a.length, values)
			out.push(n + ' ' + globalThis['each' + n](a, a.length) + ' ' + globalThis['sum' + n](a, a.length))
			globalThis['update' + n](a, a.length)
			out.push(globalThis['each' + n](a, a.length))
			// Keys the view does not have: undefined read, nothing stored.
			out.push(globalThis['reads' + n](a, [0, -1, 1.5, NaN, a.length, a.length - 1, 2**32, -0, 1e21, Infinity, -Infinity, 2**31]))
			globalThis['writes' + n](a, [-1, 0.5, a.length, 2**32, 3, NaN, -0, Infinity])
			out.push(globalThis['each' + n](a, 6))
		}
		return out.join('|')
	}
	function polyRound(phase) {
		const out = []
		const i8 = new Int8Array(40), f64 = new Float64Array(40), arr = new Array(40).fill(0.5)
		if (phase >= 0) { polyw(i8, 40); out.push(poly(i8, 40)) }
		if (phase >= 1) { polyw(f64, 40); out.push(poly(f64, 40)) }
		if (phase >= 2) { polyw(arr, 40); out.push(poly(arr, 40), arr.length) }
		for (let r = 0; r < 2; r++) { const d = polyw(new Int32Array(32), 32); out.push(drain(d, 32, 10 + r), d.length) }
		gfill(200); out.push(gsum(200))
		return out.join()
	}`

// typedKindCells is the union of the element types the sites of the
// global function name have learned, and how many sites learned any.
func typedKindCells(t *testing.T, r *Runtime, name string) (kinds uint64, sites int) {
	t.Helper()
	cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	l := r.jit.lowered[weak.Make(cl.fn)]
	if l == nil {
		t.Fatalf("%s has no lowering", name)
	}
	for _, c := range l.typedCells {
		if b := math.Float64bits(c.num); b != 0 {
			kinds |= b
			sites++
		}
	}
	return kinds, sites
}

// An element read or write of a number whose site has met typed arrays of
// one element type is compiled for it: inline, the type checked, an
// Array's element out of line (ssa.TypedKindOf). Each type is read and
// written so -- values converted to it, keys it has no element at -- and a
// site meets another type, then Arrays, then a view detached as it runs:
// every answer the interpreter's, with the collector idle and while it
// marks. The sites learn their types as native code runs, each its own,
// the code compiled again for them, and then run natively with no call of
// Go.
func TestJITTypedKinds(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			runtime.GC()
			gc := debug.SetGCPercent(-1)
			t.Cleanup(func() { debug.SetGCPercent(gc) })
			if marking {
				jitMarkingForTest(t)
			}
			rounds := []string{}
			for range 6 {
				rounds = append(rounds, "kindsRound()", "polyRound(0)")
			}
			r := jitAgreesForTest(t, typedKindSetup, rounds...)
			for i, k := range []int{abi.ElemInt8, abi.ElemUint8, abi.ElemUint8Clamped, abi.ElemInt16, abi.ElemUint16, abi.ElemInt32, abi.ElemUint32, abi.ElemFloat32, abi.ElemFloat64} {
				name := []string{"Int8Array", "Uint8Array", "Uint8ClampedArray", "Int16Array", "Uint16Array", "Int32Array", "Uint32Array", "Float32Array", "Float64Array"}[i]
				// update's a[i] += 3 is not native code's: its key's guard
				// (ir.ArrayKey) is an Array's.
				for _, f := range []string{"sum", "fill", "each"} {
					e := jitNativeForTest(t, r, f+name)
					if e.reopts == 0 {
						t.Errorf("%s%s never compiled again for what it learned", f, name)
					}
					if kinds, _ := typedKindCells(t, r, f+name); kinds != 1<<k {
						t.Errorf("%s%s learned element types %#x, want %#x", f, name, kinds, 1<<k)
					}
				}
				// Learned, the reads and writes are native code's alone: no
				// call of Go learns a type, nor reads or writes an element
				// but the first of a new view, which Go looks at.
				src := fmt.Sprintf("(() => { const a = fill%[1]s(new %[1]s(300), 300, [0, 1.5, -2, 77.25, -129, 300, 1e5]); return sum%[1]s(a, 300) })()", name)
				want, err := r.Run(compileForTest(t, src))
				if err != nil {
					t.Fatal(err)
				}
				_, calls := jitCountGoCalls(t, r, src)
				if calls > 1 {
					t.Errorf("%s: %d calls of Go, after the sites learned their type (%v)", name, calls, want)
				}
			}
			// The site that met two types tells them apart again; the global's
			// sites were compiled for its type from the first.
			r2 := jitAgreesForTest(t, typedKindSetup,
				"polyRound(0)", "polyRound(0)", "polyRound(0)", "polyRound(0)",
				"polyRound(1)", "polyRound(1)", "polyRound(1)", "polyRound(1)",
				"polyRound(2)", "polyRound(2)", "polyRound(2)",
				"(heap = new Float32Array(64), polyRound(2))", "polyRound(2)", "polyRound(2)",
				"(heap = new Uint8Array(64), polyRound(2))", "polyRound(2)", "polyRound(2)")
			if kinds, _ := typedKindCells(t, r2, "poly"); kinds != 1<<abi.ElemInt8|1<<abi.ElemFloat64 {
				t.Errorf("poly learned element types %#x, want Int8's and Float64's", kinds)
			}
			if kinds, _ := typedKindCells(t, r2, "gsum"); kinds != 1<<abi.ElemFloat32|1<<abi.ElemUint8 {
				t.Errorf("gsum learned element types %#x, want Float32's and Uint8's: Int32's it was compiled for", kinds)
			}
		})
	}
}

// The values TestJITTypedKinds' rounds convert, each type's, are Node's
// (v26.12.1): a check of the interpreter that the comparison stands on.
func TestJITTypedKindsWant(t *testing.T) {
	r := New(Config{})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if _, err := r.Run(compileForTest(t, typedKindSetup)); err != nil {
		t.Fatal(err)
	}
	v, err := r.Run(compileForTest(t, "kindsRound().split('|').filter((_, i) => i % 4 === 0).map(s => s.split(' ')[0] + ' ' + s.split(' ')[2]).join()"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "Int8Array 332,Uint8Array 5964,Uint8ClampedArray 10216,Int16Array 67916,Uint16Array 1247564,Int32Array 4295297356,Uint32Array 64424839500,Float32Array NaN,Float64Array NaN"
	if got := v.String().Go(); got != want {
		t.Errorf("sums %s, want %s", got, want)
	}
	if strings.Count(want, ",") != 8 {
		t.Fatal("want has a sum for each type")
	}
}

// An element read or write of what a captured binding holding a typed
// array was found to hold is compiled for that array's type from the first
// (ssa.UpvalueTypedFeedback), as one of a global's typed array is: an
// asm.js module's functions read its heap's views, bindings of the module
// they capture, at every branch, and had been compiled again for each
// branch's sites as native code came to them and learned their types.
// Replaced by a view of another type, the binding's sites tell it apart,
// every answer the interpreter's.
func TestJITTypedUpvalues(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `
		function Module(buffer) {
			var H32 = new Int32Array(buffer), HU8 = new Uint8Array(buffer), HF64 = new Float64Array(buffer)
			function branches(k, n) {
				var s = 0
				for (var i = 0; i < n; i++) {
					switch (k) {
					case 0: if (H32[i & 255] !== 0) s++; break
					case 1: H32[(i * 3) & 255] = i & 3; s += H32[(i * 7) & 255]; break
					case 2: if (HU8[i & 1023] === 1) s++; break
					case 3: HF64[i & 127] = i * 0.5; s += HF64[(i + 1) & 127]; break
					case 4: HU8[(i * 5) & 1023] = i; s += HU8[i & 1023]; break
					case 5: if (H32[(i >> 1) & 255] === 1) s++; break
					}
				}
				return s
			}
			function swap() { H32 = new Float32Array(buffer); HU8 = new Int16Array(buffer) }
			for (var i = 0; i < 256; i++) H32[i] = i & 3
			return { branches: branches, swap: swap }
		}
		var m = Module(new ArrayBuffer(1024)), branches = m.branches`
	rounds := []string{}
	for k := range 6 {
		rounds = append(rounds, fmt.Sprintf("branches(%d, 1000)", k))
	}
	r := jitAgreesForTest(t, setup, append(append(rounds, "m.swap()"), rounds...)...)
	if kinds, _ := typedKindCells(t, r, "branches"); kinds == 0 {
		t.Error("branches' sites learned no type once its bindings held views of others")
	}

	r = New(Config{JIT: true})
	// Compiled on the spot, as the counts below are written for, not in
	// the background (QJS_JIT_STRESS's background).
	r.jitStress.background = false
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
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
		t.Errorf("branches was compiled again %d times for its branches' reads and writes", n)
	}
	if n := e.ssaStats.hosts - hosts; n > 2 {
		t.Errorf("branches left native code %d times in 5,000 iterations", n)
	}
	if kinds, sites := typedKindCells(t, r, "branches"); kinds != 0 {
		t.Errorf("branches' %d sites learned element types %#x, compiled for their bindings' types", sites, kinds)
	}
}
