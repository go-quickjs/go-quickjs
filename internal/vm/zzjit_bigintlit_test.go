//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"
)

// Functions with BigInt literals, in the int64 range, negative and on the
// heap, compiled (ir.BigIntConst): their remainders and divisions by Go
// from native code (ssa.GoArith), strings built of BigInts by Go's
// additions (jitAddOperand), and comparisons natively, none leaving native
// code once warm. A negated literal is one constant (the compiler's).
const bigIntLitSetup = `
	function digits(n) {
		let c = 0
		for (let j = 0; j < n; j++) {
			let x = 9007199254740993000n + BigInt(j), s = 0n
			while (x > 0n) { s += x % 10n; x /= 10n }
			if (s > 70n) c++
			if (s <= -1n) c += 100
		}
		return c
	}
	function text(n) {
		let s = '', t = 0
		for (let j = 0; j < n; j++) {
			const y = 123456789012345678901234567890n * BigInt(j) - 77n
			s += y
			s = s + ',' + (y % 1000n) + -5n
			if (s.length > 2000) { t += s.length; s = '' }
		}
		return s.length + t
	}
	function all(n) { return digits(n) + text(n) * 3 }
`

func TestJITBigIntLiterals(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var srcs []string
	for range 40 {
		srcs = append(srcs, `all(300)`)
	}
	r := jitDefaultRuns(t, bigIntLitSetup, srcs...)
	for _, name := range []string{"digits", "text"} {
		e := r.jit.entryFor(r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn)
		if e == nil || e.ssa == nil || e.invalid {
			if e != nil {
				t.Fatalf("%s has no code: invalid %v, refusal %q", name, e.invalid, e.refusal)
			}
			t.Fatalf("%s has no entry", name)
		}
		hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
		for range 3 {
			if _, err := r.Run(compileForTest(t, name+`(300)`)); err != nil {
				t.Fatal(err)
			}
		}
		if e.ssaStats.hosts != hosts || e.ssaStats.guards != guards {
			t.Errorf("%s left native code: %d host exits, %d guard failures", name, e.ssaStats.hosts-hosts, e.ssaStats.guards-guards)
		}
	}
}
