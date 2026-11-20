//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64)) && !quickjs_nativelayout

package vm

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// asmCallsSource is testdata/asm_ops.js and testdata/asm_calls.js, whose
// asmCalls() runs asm_ops.js's driver over asm_calls.js's module: each
// operation of asm_ops.js a small function of the module, called by
// typed code.
func asmCallsSource(t *testing.T) (src, want string) {
	t.Helper()
	ops, err := os.ReadFile("testdata/asm_ops.js")
	if err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile("testdata/asm_calls.js")
	if err != nil {
		t.Fatal(err)
	}
	w, err := os.ReadFile("testdata/asm_ops.want")
	if err != nil {
		t.Fatal(err)
	}
	return string(ops) + "\n" + string(calls), strings.TrimRight(strings.ReplaceAll(string(w), "\r\n", "\n"), "\n")
}

// asmBackgroundForTest runs src, then call, twice in a runtime that
// compiles in the background, as qjs --jit does -- the link asks for each
// function's code there, built from a snapshot of its feedback, its
// typed callees' inlined with theirs -- and gives each run's value: while
// the compiles run, and once each is installed.
func asmBackgroundForTest(t *testing.T, src, call string) [2]string {
	t.Helper()
	r := jitBackgroundRuntime(t)
	r.jitStress = jitStressConfig{background: true}
	var out [2]string
	for round := range 2 {
		v, err := r.Run(compileForTest(t, src+"\n"+call))
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		out[round] = strings.ReplaceAll(v.String().Go(), "\r\n", "\n")
		for _, j := range r.jit.regJobs {
			<-j.done
		}
		r.jitDrain()
	}
	return out
}

// TestAsmJITOpsThroughCalls runs asm_ops.js's every typed operation as a
// callee of typed code (testdata/asm_calls.js) with the JIT, under its
// stress settings, with the collector marking, on a CPU without SSE4.1's
// rounding and compiled in the background: each answer exactly Node's (testdata/asm_ops.want,
// which Node gives for asm_calls.js's module too), the typed
// interpreter's and the ordinary code's; the module's functions native;
// and its typed callees inlined in their typed callers, as the ordinary
// inliner inlines ordinary ones -- the operations' dispatchers inlining
// the operations nothing inside of which exits.
func TestAsmJITOpsThroughCalls(t *testing.T) {
	t.Parallel()
	src, want := asmCallsSource(t)
	typed, ran := asmRun(t, src, "asmCalls()", true, true, "asmOpsModule", "iop")
	if !ran {
		t.Fatal("the module did not run typed")
	}
	if typed != want {
		t.Fatalf("typed code differs from Node (testdata/asm_ops.want):\n%s", firstDiff(typed, want))
	}
	if plain, _ := asmRun(t, src, "asmCalls()", true, false, "asmOpsModule", "iop"); plain != want {
		t.Fatalf("ordinary code differs from Node:\n%s", firstDiff(plain, want))
	}
	for _, c := range []struct{ stress, marking, noRound bool }{{false, false, false}, {true, false, false}, {false, true, false}, {false, false, true}} {
		t.Run(fmt.Sprintf("stress=%v/marking=%v/noRound=%v", c.stress, c.marking, c.noRound), func(t *testing.T) {
			if c.marking {
				jitMarkingForTest(t)
			}
			jitTestSwitch(t, func(jc *jitTestConfig) {
				jc.round = switchOf(jitEncoding.Round && !c.noRound || runtime.GOARCH == "arm64")
			})
			inlined := jitInlinedForTest(t)
			fn := compileForTest(t, src+"\nasmCalls()")
			r := jitRuntimeForTest(t, Config{JIT: true})
			if c.stress {
				r.jitStress = jitStressConfig{threshold: true, budget: 3, deopt: 7}
			}
			v, err := r.Run(fn)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.ReplaceAll(v.String().Go(), "\r\n", "\n"); got != want {
				t.Errorf("native code differs from Node and the typed interpreter:\n%s", firstDiff(got, want))
			}
			for name, how := range asmJITNative(r, "asmOpsModule", []string{"iop", "ibr", "dop", "dbr", "store", "load", "loadOff", "globals", "loop"}) {
				if how != "native" {
					t.Errorf("%s: %s", name, how)
				}
			}
			// Each operation -- every store and load, iop's and dop's as
			// many as the slots exits name by number allow (abi.MaxRecords),
			// the inlined frames' laid one past another: 22 of 32 and of 23.
			for name, least := range map[string]int{"iop": 22, "ibr": 2, "dop": 22, "dbr": 2, "store": 8, "load": 8, "loadOff": 1, "globals": 1, "loopRun": 1} {
				if n := len(inlined[name]); n < least {
					t.Errorf("%s inlined %d callees, want %d or more", name, n, least)
				}
			}
		})
	}
	for round, got := range asmBackgroundForTest(t, src, "asmCalls()") {
		if got != want {
			t.Errorf("compiled in the background, round %d differs from Node:\n%s", round, firstDiff(got, want))
		}
	}
}

// asmInlinedMostForTest records, for each function compiled while the
// test runs, the most closures any of its compiles inlined (ssa.Func's
// Inlined): a callee that left native code too often is not inlined by
// the compiles after (jitInlineLeft).
func asmInlinedMostForTest(t *testing.T) map[string]int {
	t.Helper()
	got := map[string]int{}
	jitTestSwitch(t, func(jc *jitTestConfig) {
		jc.ssaOptimized = func(fn *bytecode.Function, f *ssa.Func) {
			got[fn.Name] = max(got[fn.Name], len(f.Inlined))
		}
	})
	return got
}

// TestAsmJITInlinedExits runs testdata/asm_inline_exits.js with the JIT,
// under its stress settings, with the collector marking, with the
// back-edge budget alone made small and compiled in the background: typed callees inlined that leave
// native code inside -- a loop's poll, which the budget makes leave at
// every few turns -- or that compute % and pow of doubles, module
// variables and the heap written in a callee; and callees not inlined, as
// ordinary ones are not, that make a call nothing inlines -- an import's,
// whose result is converted, a recursion's, one past the stack's limit.
// Every exit inside an inlined callee writes its frame's raw slots raw,
// and the VM goes on in the level's typed code: the answers are Node's
// (testdata/asm_inline_exits.want), the typed interpreter's and the
// ordinary code's; the stacks of the errors made and thrown are the typed
// interpreter's, which are the ordinary code's; and Go finished inlined
// typed frames.
func TestAsmJITInlinedExits(t *testing.T) {
	t.Parallel()
	srcb, err := os.ReadFile("testdata/asm_inline_exits.js")
	if err != nil {
		t.Fatal(err)
	}
	wantb, err := os.ReadFile("testdata/asm_inline_exits.want")
	if err != nil {
		t.Fatal(err)
	}
	src, want := string(srcb), strings.TrimRight(strings.ReplaceAll(string(wantb), "\r\n", "\n"), "\n")
	const call = `inlineExits() + "\n----\n" + inlineExitStacks()`
	typed, ran := asmRun(t, src, call, true, true, "inlineExitsModule", "outerSum")
	if !ran {
		t.Fatal("the module did not run typed")
	}
	answers, stacks, ok := strings.Cut(typed, "\n----\n")
	if !ok {
		t.Fatalf("no stacks in %q", typed)
	}
	if answers != want {
		t.Fatalf("typed code differs from Node (testdata/asm_inline_exits.want):\n%s", firstDiff(answers, want))
	}
	if plain, _ := asmRun(t, src, call, true, false, "inlineExitsModule", "outerSum"); plain != typed {
		t.Fatalf("typed and ordinary code differ:\n%s", firstDiff(typed, plain))
	}
	for _, at := range []string{"at conv (test:", "at viaConv (test:", "at Object.outerConv (test:"} {
		if !strings.Contains(stacks, at) {
			t.Fatalf("the stacks have no %q:\n%s", at, stacks)
		}
	}
	// What each function inlines: sumTo, a small loop; dmod and dpow
	// twice; setG; none of the callees whose calls nothing inlines.
	inlines := map[string]int{"outerConv": 0, "outerConvF": 0, "outerSum": 1, "outerD": 3, "fibw": 0, "deepw": 0, "outerG": 1}
	for _, c := range []struct {
		name   string
		stress jitStressConfig
	}{{"plain", jitStressConfig{}}, {"stress", jitStressConfig{threshold: true, budget: 3, deopt: 7}}, {"marking", jitStressConfig{}},
		{"budget", jitStressConfig{budget: 3}}} {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "marking" {
				jitMarkingForTest(t)
			}
			inlined := asmInlinedMostForTest(t)
			fn := compileForTest(t, src+"\n"+call)
			r := jitRuntimeForTest(t, Config{JIT: true})
			r.jitStress = c.stress
			v, err := r.Run(fn)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.ReplaceAll(v.String().Go(), "\r\n", "\n"); got != typed {
				t.Errorf("native code differs from the typed interpreter:\n%s", firstDiff(got, typed))
			}
			for name, how := range asmJITNative(r, "inlineExitsModule", []string{"outerConv", "outerConvF", "outerSum", "outerD", "fibw", "deepw", "outerG"}) {
				if how != "native" {
					t.Errorf("%s: %s", name, how)
				}
			}
			if c.name == "stress" {
				// Compiled at every turn's threshold, deoptimized at random:
				// the answers alone.
				return
			}
			for name, n := range inlines {
				if got := inlined[name]; got != n {
					t.Errorf("%s inlined %d callees, want %d", name, got, n)
				}
			}
			if c.name == "budget" && r.jit.unwound == 0 {
				t.Error("Go finished no inlined frame: no poll left an inlined loop")
			}
		})
	}
	for round, got := range asmBackgroundForTest(t, src, call) {
		if got != typed {
			t.Errorf("compiled in the background, round %d differs from the typed interpreter:\n%s", round, firstDiff(got, typed))
		}
	}
}
