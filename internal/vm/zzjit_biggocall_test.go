//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
)

// BigInt operators at sites whose speculation failed, or that met BigInts
// on the heap: Go makes each from native code (ssa.GoArith, GoAdd, GoAppend
// of a loop's sum, GoCompare), not after an exit, where it throws nothing;
// what would throw -- a division by zero, a shift past the size limit, a
// BigInt with a number -- leaves for Go, which throws it.
const bigGoCallSetup = `
	var one = BigInt(1), two = BigInt(2), three = BigInt(3), m = BigInt(1000003)
	var wide = BigInt(1) << BigInt(123), zero = BigInt(0), far = BigInt(1) << BigInt(40)
	function heap(n, a) {
		let s = zero, c = 0
		for (let i = 0; i < n; i++) {
			s += a * a
			const d = s - a, q = s / a, r = s % (a + three)
			const x = (a & s) | (d ^ q), y = (x << two) >> three
			if (d > a) c++
			if (q <= r) c += 2
			s = s + (r + y) % m
			a += one
		}
		return Number(s % m) + c
	}
	function throws(n, a, k, b) {
		let s = zero
		try {
			for (let i = 0; i < n; i++) {
				s += a / (i == k ? zero : one)
				s += i == k + 1 ? a << far : a
				s += i == k + 2 ? b : a
			}
		} catch (e) {
			return (e instanceof RangeError ? 1 : e instanceof TypeError ? 2 : 3) + Number(s % m) * 4
		}
		return Number(s % m)
	}
	function all(n, k) { return heap(n, wide) + throws(n, wide, k, 1) * 7 + throws(n, wide, k, wide) * 11 }
`

func TestJITBigIntGoCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var srcs []string
	for range 40 {
		srcs = append(srcs, `all(500, 1000)`)
	}
	for _, k := range []string{"5", "300", "497", "498", "499"} {
		srcs = append(srcs, `all(500, `+k+`)`)
	}
	r := jitDefaultRuns(t, bigGoCallSetup, srcs...)
	e := r.jit.entryFor(r.global.getOwn(r.atoms.intern("heap")).value().Object().fn().closure.fn)
	if e == nil || e.ssa == nil || e.invalid {
		if e != nil {
			t.Fatalf("heap has no code: invalid %v, refusal %q, deferred %v, deopts %d, learned %v, generic %v", e.invalid, e.refusal, e.deferred, e.deopts, e.bigints, e.failed)
		}
		t.Fatal("heap has no entry")
	}
	// Warm, every operator is made in native code or by Go from it.
	hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
	for range 5 {
		if _, err := r.Run(compileForTest(t, `heap(500, wide)`)); err != nil {
			t.Fatal(err)
		}
	}
	if e.ssaStats.hosts != hosts || e.ssaStats.guards != guards {
		t.Fatalf("heap left native code: %d host exits, %d guard failures (learned %v, generic %v)",
			e.ssaStats.hosts-hosts, e.ssaStats.guards-guards, e.bigints, e.failed)
	}
}
