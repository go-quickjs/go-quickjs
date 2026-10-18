//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// A for...of loop over an array runs natively from its start to its end,
// as V8's code iterates an array without a call: native code steps the
// array in place, the step that finds the cursor done -- a call of Go --
// compares its answer with the uninitialized value without leaving, and
// the iter_close the loop ends with, which closes nothing for a cursor
// done or an array walked in place, is native too. Before, each loop left
// native code twice -- at that comparison, which went to Go for any word
// but a boolean's, null's or undefined's, and at iter_close, always Go's
// -- and the interpreter ran each loop's body up to an entry: AWFY's
// Bounce left native code 150,000 times a run. Each answer is the
// interpreter's.
func TestJITForOfArrayNative(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `
		function B(x) { this.x = x } B.prototype.f = function () { return this.x & 1 }
		var bs = []; for (var i = 0; i < 100; i++) bs.push(new B(i))
		function whole(n) { let s = 0; for (let i = 0; i < n; i++) for (const b of bs) if (b.f()) s++; return s }
		function broken(n) { let s = 0; for (let i = 0; i < n; i++) for (const b of bs) { if (b.x === 40 + i) break; s += b.x } return s }
		function pairs(n) { let s = 0; for (let i = 0; i < n; i++) for (const a of bs) { if (a.x > 3) break; for (const b of bs) s += a.x * b.x } return s }
	`
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `whole(50)`, `broken(20)`, `pairs(10)`)
	}
	r := jitAgreesForTest(t, setup, rounds...)
	names := []string{"whole", "broken", "pairs"}
	type counts struct{ hosts, guards uint64 }
	before := map[string]counts{}
	for _, name := range names {
		e := jitNativeForTest(t, r, name)
		before[name] = counts{e.ssaStats.hosts, e.ssaStats.guards}
	}
	for _, src := range []string{`whole(50)`, `broken(20)`, `pairs(10)`} {
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range names {
		e := jitNativeForTest(t, r, name)
		if h, g := e.ssaStats.hosts-before[name].hosts, e.ssaStats.guards-before[name].guards; h != 0 || g != 0 {
			t.Errorf("%s left native code %d times for Go and failed %d guards", name, h, g)
		}
	}
}
