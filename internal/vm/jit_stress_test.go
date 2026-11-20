//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// regStressForTest gives r what CI's stress step gives every runtime
// (QJS_JIT_STRESS=threshold,budget=3,deopt=7): a function compiled once
// it has run, a poll every three back edges, and every seventh poll
// finishing the invocation in the interpreter. The
// register tests' runtimes run without it (regNativeRuntimes), their counts
// not written for it; their answers are checked under it here.
func regStressForTest(r *Runtime, c jitStressConfig) {
	r.jitCallThreshold, r.jitICThreshold = 1, 1
	c.threshold = true
	r.jitStress = c
	r.jitState().stressBudget = int(c.budget)
}

// Programs whose register code's native code answered wrongly under CI's
// stress step, each against the interpreter:
//
//   - regTailSrc's deep: a call of Go native code made (jitRegGoCallOn)
//     whose callee's frame a call in tail position gave to its callee in
//     place (regTailTo, errRegRestart) left that as an error, which
//     unwound to the first frame that runs one again: the outer frame ran
//     the callee's code on its own registers, even(40, 0) answering 61.
//   - regClassSrc's D: a derived constructor's run, its super() made
//     natively, the binding in its context alone, called Go for a
//     super.m read, which found the frame's binding unbound ("this is
//     not bound until super() has been called").
//   - regBatchSrc's capturedResult and calledSetup's closures: closures
//     the code only calls inlined, left as their templates where a poll
//     finished the invocation in the interpreter ("not a function"); and
//     a template no call had run yet, which an exit inside its inlined
//     body found without register code (jitRegInlined's panic).
//   - bigReleasePrograms' captured: code of a function compiled for one of
//     its closures, run by another, whose template of get the frame's
//     closure does not hold (jitRegMadeIn's panic).
func TestJITRegStressAgrees(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	type call struct {
		name string
		n    float64
	}
	cases := []struct {
		name, src string
		calls     []call
		times     int
	}{
		{"tail", regTailSrc + `
function evens(n) { var s = 0; for (var i = 0; i < n; i++) s += even(40 + (i & 7), 0); return s }
`, []call{{"poly", 2000}, {"tasks", 2000}, {"evens", 1}, {"deep", 2000}}, 1},
		{"class", regClassSrc, []call{{"main", 0}}, 6},
		{"batch", regBatchSrc, []call{{"capturedResult", 400}, {"constructs", 400}, {"tails", 400}}, 6},
		{"given", givenSetup, nil, 0},
		{"called", calledSetup, nil, 0},
		{"lexThis", regLexThisSetup, nil, 0},
	}
	for _, c := range cases {
		for k, sc := range regStressConfigs {
			marking := k == 0
			t.Run(fmt.Sprintf("%s/budget=%d,deopt=%d/marking=%v", c.name, sc.budget, sc.deopt, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				rts := regNativeRuntimes(t, c.src)
				regStressForTest(rts[0], sc)
				if c.calls == nil {
					for _, round := range regCalledRounds(c.src) {
						prog := regBigCompile(t, round)
						var got [2]string
						for k, r := range rts {
							v, err := r.Run(prog)
							got[k] = regNativeShow(r, v, err)
						}
						if got[0] != got[1] {
							t.Fatalf("%s: with the JIT %q, without %q", round, got[0], got[1])
						}
					}
					return
				}
				for _, cl := range c.calls {
					var got [2]string
					for i, r := range rts {
						fn, _ := r.GetProp(Obj(r.Global()), r.Intern(cl.name))
						for range c.times {
							v, err := r.Call(fn, Undefined, []Value{Float(cl.n)})
							got[i] = regNativeShow(r, v, err)
						}
					}
					if got[0] != got[1] {
						t.Errorf("%s(%v): with the JIT %q, without %q", cl.name, cl.n, got[0], got[1])
					}
				}
			})
		}
	}
	t.Run("bigint", func(t *testing.T) {
		for i, p := range bigReleasePrograms {
			r := regBigRuntime(t)
			regStressForTest(r, regStressConfigs[0])
			prog := regBigCompile(t, p.src)
			for round := range 3 {
				got, err := r.Run(prog)
				if err != nil {
					t.Fatal(err)
				}
				if g := got.String().Go(); g != p.want {
					t.Fatalf("program %d, round %d: %s, Node's %s", i, round, g, p.want)
				}
			}
		}
	})
}

// regStressConfigs are the stresses TestJITRegStressAgrees runs under: CI's,
// compiling at the first call alone, and polls more often.
var regStressConfigs = []jitStressConfig{{budget: 3, deopt: 7}, {}, {budget: 1, deopt: 2}, {budget: 2, deopt: 3}, {budget: 1, deopt: 3}, {budget: 3, deopt: 5}}
