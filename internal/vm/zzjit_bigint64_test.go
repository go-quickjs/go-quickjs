//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"sync/atomic"
	"testing"
)

// BigInt arithmetic and comparisons whose operands, BigInts in the int64
// range, a site has met (jitBigIntSeen): native code makes them unboxed,
// each result boxed in a keep cell, until a speculation fails -- a result
// outside the range, a BigInt on the heap, a number -- and the site is
// Go's. BigInt literals are not compiled, so the constants are globals.
const bigInt64Setup = `
	var zero = BigInt(0), one = BigInt(1), two = BigInt(2), m = BigInt(1000003)
	var start = BigInt(1) << BigInt(20), near = BigInt(1) << BigInt(31)
	var max = (BigInt(1) << BigInt(63)) - one, min = -(BigInt(1) << BigInt(63)), huge = BigInt(1) << BigInt(80)
	function squares(n, a) {
		let s = zero
		for (let i = 0; i < n; i++) { s += a * a; a += one }
		return Number(s % m)
	}
	function down(n, x, step) {
		let lo = -x, mid = zero, c = 0
		for (let i = 0; i < n && x > lo; i++) {
			x -= step
			if (x < mid) c++
			if (x >= mid) c += 2
			if (x <= lo) c += 3
		}
		return c + Number(x % m)
	}
	function edges(n, a, b) {
		let c = 0, x
		for (let i = 0; i < n; i++) {
			x = a * b
			c += Number(x % m)
			x = a - b
			c += Number(x % m)
		}
		return c
	}
	function stores(n, a) {
		const o = { v: zero }, arr = [], fs = []
		let s = zero
		for (let i = 0; i < n; i++) {
			const b = a * two
			o.v = a + b
			arr[i & 15] = a - b
			if (i % 100 == 0) fs.push(() => b)
			s += id(o.v) + arr[i & 15]
			a += one
		}
		for (const f of fs) s += f()
		return Number(s % m)
	}
	function id(x) { return x }
	function changes(n, k, odd) {
		let s = zero, a = one
		for (let i = 0; i < n; i++) {
			s += i == k ? odd : a
			a += one
		}
		return Number(s % m)
	}
	function mixed(n, k) {
		try { return changes(n, k, 1) } catch (e) { return e instanceof TypeError ? -1 : -2 }
	}
	function over(n, a) {
		// Across 2^63: native code meets BigInts past int64 held in their
		// Values, which its unboxing exits at.
		let s = a, c = 0
		for (let i = 0; i < n; i++) {
			s = s + one
			if (s > a) c++
			if (s < max) c += 2
			c += Number((s - a) % m)
		}
		return c
	}
	function past(n, a, b) {
		// Compiled for BigInts in the int64 range, then given one past it,
		// held in its Value: its unboxing exits.
		let c = 0
		for (let i = 0; i < n; i++) {
			if (a < b) c++
			if (a >= b) c += 2
			c += Number((a - b) % m)
		}
		return c
	}
	function all(n, k) {
		return squares(n, start) + squares(n, near) * 3 + down(n, max, start) * 5 + down(n, min + one, -start) * 7 +
			edges(n, max, one) + edges(n, min, -one) * 11 + edges(n, near, near) * 13 + stores(n, start) * 17 +
			changes(n, k, huge) * 19 + mixed(n, k) * 23 + over(n, max - 1000n) * 29 +
			past(n, k == 1000 ? max + 5n : max - 5n, max - 7n) * 31
	}
`

func TestJITBigInt64(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// The collector marks now and then, so that boxes are written through
	// Go's write barrier too.
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			runtime.GC()
		}
	}()
	defer func() { stop.Store(true); <-done }()
	var srcs []string
	for range 40 {
		srcs = append(srcs, `all(2000, 5000)`)
	}
	for _, k := range []string{"5", "700", "1999", "1000"} {
		srcs = append(srcs, `all(2000, `+k+`)`)
	}
	r := jitDefaultRuns(t, bigInt64Setup, srcs...)
	// Native BigInt sites that held: some of each function's are learned
	// and none failed since.
	for _, name := range []string{"squares", "down", "stores"} {
		fn := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn
		e := r.jit.entryFor(fn)
		if e == nil || e.ssa == nil {
			t.Fatalf("%s has no code", name)
		}
		// Learned at a guard that failed, or from the tree tier.
		fb := &jitFeedback{r: r, fn: fn, e: e}
		held := 0
		for pc := range e.ssaProg.Code {
			if fb.BigInt(pc) && !fb.Generic(pc) {
				held++
			}
		}
		if held == 0 {
			t.Errorf("%s: no native BigInt site held (learned %v, failed %v)", name, e.bigints, e.failed)
		}
	}
}
