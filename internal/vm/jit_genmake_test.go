//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import "testing"

// A loop making a short generator at each turn and running it, compiled,
// makes the generator natively, from its call's pool, and takes the loop's
// cursor from the generator's room, where each had been a call of Go: a
// few calls of Go in all -- the pool's fillings, the polls -- and every
// step resumes its generator natively. So too while the collector marks
// (jitMarkingForTest), its pointer words stored through the write barrier:
// a making or a cursor left to Go while it marked had the loop judged
// slow, left to the interpreter, which makes no native step.
func TestJITGenMakeCallsNoGo(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "marking"}[marking], func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: true})
			if _, err := r.Run(compileForTest(t, `function* pair(a, b) { yield a; yield b }
				function f(n) { let s = 0; for (let j = 0; j < n; j++) for (const v of pair(j, 1)) s += v; return s }`)); err != nil {
				t.Fatal(err)
			}
			for range 8 {
				if v, err := r.Run(compileForTest(t, "''+f(100)")); err != nil || v.String().Go() != "5050" {
					t.Fatalf("f(100) = %v, %v", v, err)
				}
			}
			in := jitGenInForTest(r, "pair")
			v, calls := jitCountGoCalls(t, r, "''+f(10000)")
			if v.String().Go() != "50005000" {
				t.Fatalf("f(10000) = %v", v)
			}
			if calls > 10000/50 {
				t.Errorf("a loop making 10000 generators made %d calls of Go", calls)
			}
			if n := jitGenInForTest(r, "pair") - in; n < 30000 {
				t.Errorf("30000 steps resumed the generators natively %d times", n)
			}
		})
	}
}
