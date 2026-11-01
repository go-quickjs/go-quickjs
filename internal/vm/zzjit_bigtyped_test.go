//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// bigTypedSetup reads BigInt64Arrays' and BigUint64Arrays' elements -- the
// latter's past the int64 range at its odd indices -- in a loop, and past
// either end, and runs their forEach, reduce, filter, some, every, find,
// findIndex and map over them -- each kind by sites of its own -- with
// callbacks that capture, compare with
// numbers (a BigInt's comparison with a number, which native code made of
// BigInts exits for), make results past 64 bits that map's store wraps,
// and detach the buffer in the middle of a forEach.
const bigTypedSetup = `function mk(T, len, f) { var r = new T(len); for (var i = 0; i < len; i++) r[i] = f(i); return r }
var a = mk(BigInt64Array, 64, (i) => BigInt(i - 32) * 300000000000000000n)
var u = mk(BigUint64Array, 64, (i) => i & 1 ? 2n ** 63n + BigInt(i) : BigInt(i) * 3n)
function h(s, x) { return (s * 31n + x) & 0xffffffffffffn }
function reads(r, n) { var s = 0n; for (var i = 0; i < n; i++) s = h(s, r[i & 63]); return s }
function loops(r) {
  var s = 0n
  r.forEach((x, i) => { s = h(s, x + BigInt(i)) })
  var t = r.reduce((acc, x) => (acc ^ x) & 0xffffffffffffffffn, 0n)
  var f = r.filter((x) => x % 3n === 0n)
  var g = r.some((x) => x > 9000000000000000000n) + ',' + r.every((x) => x >= -9600000000000000000n)
  var k = r.find((x) => x > 5000000000000000000n) + ',' + r.findIndex((x) => x === r[17])
  var m = r.map((x) => x * 2n)
  var w = 0n
  for (var i = 0; i < m.length; i++) w = h(w, m[i])
  return [s, t, f.length, f[1], g, k, w, m.constructor.name].join(';')
}
function mixed(r) { return r.filter((x) => x <= 5).length + ',' + r.findIndex((x) => x > 1e18) + ',' + r.map((x) => x & 0xffn)[3] }
function detach(n) {
  var b = new ArrayBuffer(64), r = new BigInt64Array(b), s = []
  for (var i = 0; i < 8; i++) r[i] = BigInt(i) * 5n
  r.forEach((x, i) => { if (i === n) b.transfer(); s.push(String(x)) })
  return s.join(',')
}
// Each kind's sites their own: a site that met both is Go's, as before.
function readsU(r, n) { var s = 0n; for (var i = 0; i < n; i++) s = h(s, r[i & 63]); return s }
function loopsU(r) {
  var s = 0n
  r.forEach((x, i) => { s = h(s, x + BigInt(i)) })
  var t = r.reduce((acc, x) => (acc ^ x) & 0xffffffffffffffffn, 0n)
  var f = r.filter((x) => x % 3n === 0n)
  var g = r.some((x) => x > 9000000000000000000n) + ',' + r.every((x) => x >= -9600000000000000000n)
  var k = r.find((x) => x > 5000000000000000000n) + ',' + r.findIndex((x) => x === r[17])
  var m = r.map((x) => x * 2n)
  var w = 0n
  for (var i = 0; i < m.length; i++) w = h(w, m[i])
  return [s, t, f.length, f[1], g, k, w, m.constructor.name].join(';')
}
function ends(r) { return r[64] + ':' + r[-1] }
function rounds() { return [reads(a, 200) + ':' + ends(a), readsU(u, 200) + ':' + ends(u), loops(a), loopsU(u), mixed(a), mixed(u), detach(3)].join(' | ') }
`

// bigTypedWant is rounds()'s answer, Node's (v26.10.0).
const bigTypedWant = "70994965823488:undefined:undefined | 49326101049356:undefined:undefined | 10243839344672;9599999999995805696;61;-8700000000000000000;true,true;8846744073709551616,17;75836900245504;BigInt64Array | 248613056008320;64;43;9223372036854775809;true,true;9223372036854775809,17;271100356862144;BigUint64Array | 32,0,0 | 1,1,3 | 0,5,10,15,undefined,undefined,undefined,undefined"

// A BigInt typed array's element is read natively, as a BigInt held in its
// value (ssa.OpBigElemRead), at an element read and in the loops its
// methods are made into (zzjit_bigtyped.go), map's results stored as their
// low 64 bits (ssa.OpBigElemWrite): every answer the interpreter's and
// Node's, with the collector idle and while it marks, and the sums' loops
// and the methods' run natively, leaving native code nowhere.
func TestJITBigTypedArrays(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 8 {
			rounds = append(rounds, "rounds()")
		}
		r := jitAgreesForTest(t, bigTypedSetup, rounds...)
		v, err := r.Run(compileForTest(t, "rounds()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != bigTypedWant {
			t.Errorf("marking %v: rounds() = %s, want Node's %s", marking, got, bigTypedWant)
		}
		for _, name := range []string{"reads", "readsU", "loops", "loopsU"} {
			e := jitNativeForTest(t, r, name)
			if e.entrySlow {
				t.Errorf("marking %v: %s judged slow: %+v", marking, name, e.ssaStats)
			}
			ended, calls := e.ssaStats.ended, e.ssaStats.goCalls
			if _, err := r.Run(compileForTest(t, "reads(a, 200), readsU(u, 200), loops(a), loopsU(u)")); err != nil {
				t.Fatal(err)
			}
			// loops' calls of Go: filter's result made of what it kept,
			// map's array made, and the joins and conversions.
			if e.ssaStats.ended != ended || (name == "reads" || name == "readsU") && e.ssaStats.goCalls != calls {
				t.Errorf("marking %v: %s left native code %d times, made %d calls of Go", marking, name, e.ssaStats.ended-ended, e.ssaStats.goCalls-calls)
			}
		}
		debug.SetGCPercent(gc)
	}
}
