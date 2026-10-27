//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"strconv"
	"testing"
)

// tailInlineSetup's methods end in a call in tail position -- SOM's
// return this.items.hasSome(fn), what a class's strict code compiles a
// forwarding method to -- and are inlined in their callers with what they
// call: an inlined frame makes such a call as an ordinary one where the
// stack is shallow, and leaves for Go where it is deep, Go making the
// frame and the interpreter giving it up, as its own tail call does. The
// frames the call is inlined in count toward how deep it is (ssa's
// OpTailRoom), as the interpreter's frames do: a stack trace taken past
// them, from a throw inside the inlined frames, shows the frames the
// interpreter's would, at every depth around tailCallDepth.
const tailInlineSetup = `
	"use strict"
	class Box { constructor(v) { this.v = v } get() { return this.v } }
	class Fwd { constructor(b) { this.b = b } get() { return this.b.get() } twice() { return this.get() + this.get() } }
	class Leaf { constructor(x) { this.x = x } leaf() { return this.x.y } mid() { return this.leaf() } top() { return this.mid() } }
	var fwd = new Fwd(new Box(3)), odd = new Fwd({ w: 0, v: 5, get() { return this.v * 10 } })
	var good = new Leaf({ y: 1 }), bad = new Leaf(null)
	function sum(f, n) { let s = 0; for (let i = 0; i < n; i++) s += f.twice() + i; return s }
	function down(n, o) { if (n === 0) { const s = o.top(); return s } const r = down(n - 1, o); return r }
	function probe(n, o) { try { return down(n, o) } catch (e) { return e.stack } }
`

// A method ending in a call in tail position is inlined with what it calls;
// what leaves native code inside it -- a receiver of another shape, a throw
// -- has Go make its frames; every answer, the stack traces of throws at
// every depth from 20 to 44 included, is the interpreter's.
func TestJITTailCallsInlined(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// The methods run first, their calls decided before down's are; then
	// down where the stack is shallow, where it inlines top with the calls
	// it ends in.
	shallow := []string{`(() => { let r; for (let i = 0; i < 400; i++) r = good.top() + fwd.twice(); return r })()`}
	for range 6 {
		shallow = append(shallow, `sum(fwd, 40)`)
		for n := 0; n <= 16; n += 4 {
			shallow = append(shallow, `(() => { let r; for (let i = 0; i < 6; i++) r = down(`+strconv.Itoa(n)+`, good); return r })()`)
		}
	}
	r := jitAgreesForTest(t, tailInlineSetup, shallow...)
	for _, name := range []string{"sum", "down"} {
		if e := jitCallbackEntry(r, name); e == nil || e.ssa == nil || len(e.ssaInlinedAt) == 0 {
			t.Errorf("%s inlines no call ending in a call in tail position: %+v", name, e)
		}
	}
	// Then deeper, past tailCallDepth, where the inlined calls in tail
	// position leave for Go -- as many times as they leave, the inlining
	// given up (jitInlineLeft) -- and throws there.
	rounds := append([]string{}, shallow...)
	for n := 20; n <= 44; n++ {
		rounds = append(rounds, `(() => { let r; for (let i = 0; i < 6; i++) r = down(`+strconv.Itoa(n)+`, good); return r })()`,
			`probe(`+strconv.Itoa(n)+`, bad)`)
	}
	rounds = append(rounds, `sum(odd, 40)`, `sum(fwd, 40)`)
	jitAgreesForTest(t, tailInlineSetup, rounds...)
	for _, c := range []jitStressConfig{{threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}} {
		jitAgreesForTestWith(t, c, tailInlineSetup, rounds...)
	}
}
