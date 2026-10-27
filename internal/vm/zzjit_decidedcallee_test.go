//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// decidedSetup is SOM's Set over a Vector (AWFY Havlak): contains gives
// Set.hasSome a closure, which hands it to Vector.hasSome, whose loop calls
// it. contains is compiled while Set.hasSome's own call of Vector.hasSome
// is still undecided -- its compile met no target there -- so contains
// calls Set.hasSome natively; Set.hasSome decides the call at its exits and
// is compiled again, inlining nothing more itself (a loop calling what it
// is given is inlined only where a closure is given it). That compile has
// contains compiled again (jitCallersReopt), which now inlines Set.hasSome,
// Vector.hasSome and the closure, and does not make it.
const decidedSetup = `
	class Vector {
		constructor() { this.storage = []; this.firstIdx = 0; this.lastIdx = 0 }
		append(e) { this.storage[this.lastIdx] = e; this.lastIdx += 1 }
		hasSome(fn) { for (let i = this.firstIdx; i < this.lastIdx; i += 1) { if (fn(this.storage[i])) { return true } } return false }
	}
	class Set {
		constructor() { this.items = new Vector() }
		hasSome(fn) { return this.items.hasSome(fn) }
		contains(obj) { return this.hasSome((e) => e === obj) }
		add(obj) { if (!this.contains(obj)) { this.items.append(obj) } }
	}
	var set = new Set(), other = new Set()
	for (let i = 0; i < 10; i++) { set.add(i); other.add(-i) }
	other.items.storage[4] = { odd: 1 }
	function drive(n, s) { let r = 0; for (let i = 0; i < n; i++) r += s.contains(i % 13) ? 1 : 0; return r }
`

// A native callee that has since decided its calls has its native callers
// compiled again, which inline it with them where they now may; and every
// answer is the interpreter's.
func TestJITCallersInlineDecidedCallee(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{`drive(50, set)`, `drive(50, set)`, `drive(50, set)`, `drive(500, set)`, `drive(500, set)`}
	r := jitAgreesForTest(t, decidedSetup, rounds...)
	if e := jitCallbackEntry(r, "contains"); e == nil || e.ssa == nil || len(e.ssaInlinedAt) != 1 || e.ssaCalled != 1 {
		t.Fatalf("contains does not inline Set.hasSome with the closure it gives it: %+v", e)
	}
	rounds = append(rounds, `drive(500, other)`, `drive(500, set)`, `drive(500, other)`)
	jitAgreesForTest(t, decidedSetup, rounds...)
	for _, c := range []jitStressConfig{{threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}} {
		jitAgreesForTestWith(t, c, decidedSetup, rounds...)
	}
}
