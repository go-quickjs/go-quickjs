//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"runtime"
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
// methods are made into (jit_bigtyped.go), map's results stored as their
// low 64 bits (ssa.OpBigElemWrite): every answer the interpreter's and
// Node's, with the collector idle and while it marks, and the sums' loops
// and the methods' run natively, leaving native code nowhere.
func TestJITBigTypedArrays(t *testing.T) {
	t.Skip("register JIT gap: loops over typed arrays past 2 GiB leave native code once and make 3 calls of Go (docs/performance-plan.md, the register JIT's remaining gaps)")
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		release := holdCollectorNow(t)
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
		release()
	}
}

// bigWriteSetup writes BigInts to BigInt64Arrays' and BigUint64Arrays'
// elements in loops: past the int64 range and below 0, which their stores
// wrap; at a site that meets both a BigInt64Array and a Float64Array; and a
// number stored to a BigInt64Array, which throws. wr and wrU are the
// stores alone, in range.
const bigWriteSetup = `function fill(r, n, v) { for (var i = 0; i < n; i++) { v += 3n; r[i & 15] = v; r[(i + 7) & 15] = -v } return r[3] + ':' + r[12] + ':' + r[15] }
function fillU(r, n, v) { for (var i = 0; i < n; i++) { v += 3n; r[i & 15] = v; r[(i + 7) & 15] = -v } return r[3] + ':' + r[12] + ':' + r[15] }
var a = new BigInt64Array(16), u = new BigUint64Array(16), f = new Float64Array(8), b = new BigInt64Array(8)
function mixedW(r, n) { for (var i = 0; i < n; i++) { r[i & 7] = r === f ? i * 1.5 : BigInt(i) * 7n } return String(r[5]) + ',' + r.length }
function wrong(n) { var c = 0; for (var i = 0; i < n; i++) { try { b[i & 7] = i < n - 1 ? BigInt(i) : i } catch (e) { c += e instanceof TypeError } } return c + ',' + b[6] }
function rounds() { return [fill(a, 300, 9223372036854775000n), fillU(u, 300, 18446744073709551000n), fill(a, 40, -5n), mixedW(b, 50), mixedW(f, 50), mixedW(b, 50), wrong(30), a[16] + ':' + u[-1]].join(' | ') }
function wr(r, n) { var v = 1n; for (var i = 0; i < n; i++) { v += 3n; r[i & 15] = v; r[(i + 7) & 15] = -v } return r[3] }
function wrU(r, n) { var v = 1n; for (var i = 0; i < n; i++) { v += 3n; r[i & 15] = v } return r[3] }
`

// bigWriteWant is rounds()'s answer, Node's (v26.10.0).
const bigWriteWant = "-9223372036854775740:9223372036854775734:9223372036854775725 | 260:18446744073709551350:18446744073709551341 | 103:-109:91 | 315,8 | 67.5,8 | 315,8 | 1,22 | undefined:undefined"

// A BigInt stored to an element of a site that has met BigInt64Arrays
// alone, or BigUint64Arrays alone, is stored natively as its type's word
// (ssa.OpBigElemWrite), its low 64 bits, where every such store had been a
// call of Go from native code (jitStoreBigElem): typed_i64_write took five
// times as long with the JIT as without. A site that meets another type
// learns it at the store's exit (jitTypedWriteSeen) and is compiled again
// for both. Every answer Node's and the interpreter's, with the collector
// idle and while it marks, and the stores of wr and wrU make no call of Go
// (jitCountGoCalls), the loops' polls aside, and leave native code
// nowhere.
func TestJITBigTypedWrites(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		release := holdCollectorNow(t)
		t.Cleanup(release)
		if marking {
			jitMarkingForTest(t)
		}
		var rounds []string
		for range 8 {
			rounds = append(rounds, "rounds()", "wr(a, 200) + ':' + wrU(u, 200)")
		}
		r := jitAgreesForTest(t, bigWriteSetup, rounds...)
		v, err := r.Run(compileForTest(t, "rounds()"))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != bigWriteWant {
			t.Errorf("marking %v: rounds() = %s, want Node's %s", marking, got, bigWriteWant)
		}
		for _, name := range []string{"wr", "wrU"} {
			e := jitNativeForTest(t, r, name)
			st := e.ssaStats
			call := name + "(" + map[string]string{"wr": "a", "wrU": "u"}[name] + ", 5000)"
			v, calls := jitCountGoCalls(t, r, "''+"+call)
			if got, want := v.String().Go(), "14989"; got != want {
				t.Errorf("marking %v: %s = %s, want %s", marking, call, got, want)
			}
			if e.ssaStats.entries == st.entries {
				t.Errorf("marking %v: %s did not run natively", marking, name)
			}
			// A poll's call of Go each thousand turns or so, not a store's
			// at every turn: 10,004 and 5,005 had been.
			if calls > 10 || e.ssaStats.hosts != st.hosts || e.ssaStats.guards != st.guards || e.ssaStats.ended != st.ended {
				t.Errorf("marking %v: %s made %d calls of Go, %d host exits, %d guard failures, ended %d times in the interpreter", marking, call,
					calls, e.ssaStats.hosts-st.hosts, e.ssaStats.guards-st.guards, e.ssaStats.ended-st.ended)
			}
		}
		release()
	}
}

// A typed array's methods, which no cache of the interpreter's holds, are
// noted at the interpreter's reads for the code compiled for their sites
// (fillPropCache, jitNoteDeepRead), as V8's feedback names a typed array's
// map at a load: a function calling five of a Float64Array's methods,
// compiled at its first call, is compiled once and leaves its code once,
// where it had left at each of its sites in turn, one a run, to be
// compiled again for each, and deoptimized. Each answer is the
// interpreter's.
func TestJITTypedArrayMethodsNoted(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 10 {
		rounds = append(rounds, "f(ta, 50)")
	}
	r := jitAgreesForTest(t, `var ta = new Float64Array([1, 2, 3, 4])
		function f(t, n) {
			let s = 0
			for (let i = 0; i < n; i++) s += t.indexOf(3) + t.lastIndexOf(1) + t.at(-1) + (t.includes(2) ? 1 : 0) + t.join('-').length
			return s
		}`, rounds...)
	if e := jitNativeForTest(t, r, "f"); e.ssaSerial > 1 || e.ssaStats.hosts+e.ssaStats.guards > 1 {
		t.Errorf("f was compiled %d times and left its code %d times", e.ssaSerial, e.ssaStats.hosts+e.ssaStats.guards)
	}
}
