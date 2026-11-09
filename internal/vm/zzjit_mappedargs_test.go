//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"testing"
)

// mappedArgsSetup are sloppy functions whose mapped arguments object
// aliases parameters they, or a closure, assign, and which read arguments
// by index and count them, the object never let out: native code passes
// the parameters to Go's read (ir.Program's ArgumentParams). Each is
// called with fewer arguments than parameters too, an index past them
// making the object at an exit, and natively, by a loop.
const mappedArgsSetup = `
	function less(x) {
		for (var i = 1; i < arguments.length; i++) { if (x >= arguments[i]) return false; x = arguments[i] }
		return true
	}
	function greater(x, y) {
		for (var i = 1; i < arguments.length; i++) { if (x <= arguments[i]) return false; x = arguments[i] }
		return true
	}
	function twice(a, b) {
		a = a * 2
		let s = 0
		for (let i = 0; i < 3; i++) { s += arguments[0] + (arguments[1] === undefined ? 100 : arguments[1]) + arguments.length; b = i }
		return s + (b === undefined ? 7 : b)
	}
	function dyn(a, b, c) {
		let s = ''
		for (let i = 0; i < arguments.length + 1; i++) { b = 'b' + i; s += arguments[i] + ',' }
		return s + a + b + c
	}
	function dup(a, a) {
		a = 5
		let s = 0
		for (let i = 0; i < 3; i++) s += arguments[0] * 10 + arguments[1]
		return s
	}
	function closed(a, b) {
		const set = v => { a = v }
		let s = 0
		for (let i = 0; i < 3; i++) { set(i * 10); s += arguments[0] + arguments.length }
		return s + a
	}
	function strict(a, b) {
		'use strict'
		a = 9
		let s = 0
		for (let i = 0; i < 3; i++) s += arguments[0] + arguments.length
		return s
	}
	function kinds(a) {
		let s = ''
		for (let i = 0; i < 4; i++) { a = i & 1 ? 'x' + i : i + 0.5; s += typeof arguments[0] + arguments[0] }
		return s
	}
	function late(a, b) {
		let s = 0
		for (let i = 0; i < 4; i++) { s += arguments[3] === undefined ? 1 : 0; a = i * 3; s += arguments[0] + arguments[1] * 100; b = i }
		return s
	}
	function* gen(a, b) {
		a = 2
		for (let i = 0; i < 3; i++) { yield arguments[0] + arguments.length + (arguments[1] === undefined ? 0 : arguments[1]); a = i; b = -i }
	}
	function loop(n) {
		let s = 0, t = ''
		for (let i = 0; i < n; i++) {
			s += less(i, i + 1, i + 2) + less(i, i) + greater(i + 2, i + 1, i) + greater(i)
			s += twice(i, 1) + twice(i) + twice(i, 2, 3) + dup(i, i + 1) + dup(i) + closed(i, 1) + strict(i, 3)
			s += late(i, 2) + late(i, 2, 3, 4) + [...gen(i, 1)].join() + [...gen()].join()
			t = dyn(i, 'q') + dyn(i, 2, 3, 4) + kinds(i) + kinds()
		}
		return s + t
	}
`

// A sloppy function that assigns a parameter its mapped arguments object
// aliases, or whose closure may, and reads arguments by index, runs
// natively, its own code and as a native callee: arguments[k] is the
// parameter k aliases as it is now -- one passed, of a name no later
// parameter repeats -- and the call's argument otherwise, the object made
// at an exit for a key past them. Each answer is the interpreter's, with
// the collector idle and marking, and under budgets and deoptimizations.
func TestJITMappedArgumentsAssigned(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	rounds := []string{"loop(30)", "loop(5)", "[less(1, 2, 3), less(3, 2), greater(5, 4, 3), greater(1, 2)].join()",
		"twice(1, 2) + twice(4) + twice(4, 5, 6)", "dyn(1) + dyn(1, 2, 3, 4, 5)", "dup(1, 2) + dup(1)",
		"closed(1, 2) + closed()", "strict(1) + strict(1, 2)", "kinds(1) + kinds()",
		"late(1, 2) + late(1, 2, 3, 4)", "[...gen(1, 5)].join() + [...gen()].join()"}
	for _, stress := range []jitStressConfig{{}, {threshold: true, budget: 3, deopt: 7}} {
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("stress=%v/marking=%v", stress != jitStressConfig{}, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				r := jitAgreesStressForTest(t, stress, mappedArgsSetup, rounds...)
				if stress == (jitStressConfig{}) {
					for _, name := range []string{"loop", "twice", "dyn", "dup", "closed", "kinds"} {
						jitNativeOrCalleeForTest(t, r, name)
					}
				}
			})
		}
	}
}

// jitAgreesStressForTest is jitAgreesForTest with the JIT's runtime under
// stress from its setup on, each round, which gives the same answer each
// time, run twice in a row.
func jitAgreesStressForTest(t *testing.T, stress jitStressConfig, setup string, rounds ...string) *Runtime {
	t.Helper()
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.jitStress = stress
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	for i, src := range rounds {
		src = "''+(" + src + ")"
		wv, werr := want.Run(compileForTest(t, src))
		for range 2 {
			gv, gerr := r.Run(compileForTest(t, src))
			if (werr == nil) != (gerr == nil) {
				t.Fatalf("round %d (%s): error %v, interpreter's %v", i, src, gerr, werr)
			}
			if werr != nil {
				if got, want := gerr.Error(), werr.Error(); got != want {
					t.Fatalf("round %d (%s): error %q, interpreter's %q", i, src, got, want)
				}
				continue
			}
			if got, want := gv.String().Go(), wv.String().Go(); got != want {
				t.Fatalf("round %d (%s): got %s, interpreter %s", i, src, got, want)
			}
		}
	}
	return r
}
