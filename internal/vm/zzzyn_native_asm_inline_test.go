//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64)) && !quickjs_nativelayout

package vm

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
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

// TestAsmJITOpsThroughCalls runs asm_ops.js's every typed operation as a
// callee of typed code (testdata/asm_calls.js) with the JIT, under its
// stress settings, with the collector marking and on a CPU without
// SSE4.1's rounding: each answer exactly Node's (testdata/asm_ops.want,
// which Node gives for asm_calls.js's module too), the typed
// interpreter's and the ordinary code's; the module's functions native;
// and its typed callees inlined in their typed callers, as the ordinary
// inliner inlines ordinary ones -- the operations' dispatchers inlining
// the operations nothing inside of which exits.
func TestAsmJITOpsThroughCalls(t *testing.T) {
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
	was := jitEncoding.Round
	t.Cleanup(func() { jitEncoding.Round = was })
	for _, c := range []struct{ stress, marking, noRound bool }{{false, false, false}, {true, false, false}, {false, true, false}, {false, false, true}} {
		t.Run(fmt.Sprintf("stress=%v/marking=%v/noRound=%v", c.stress, c.marking, c.noRound), func(t *testing.T) {
			if c.marking {
				jitMarkingForTest(t)
			}
			jitEncoding.Round = was && !c.noRound || runtime.GOARCH == "arm64"
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
			// Each operation nothing inside of which exits -- dop's all but
			// % and pow, every store and load -- iop's as many as the
			// slots exits name by number allow (abi.MaxRecords), the
			// inlined frames' laid one past another.
			for name, least := range map[string]int{"iop": 22, "ibr": 2, "dop": 21, "dbr": 2, "store": 8, "load": 8, "loadOff": 1, "globals": 1, "loopRun": 1} {
				if n := len(inlined[name]); n < least {
					t.Errorf("%s inlined %d callees, want %d or more", name, n, least)
				}
			}
		})
	}
}
