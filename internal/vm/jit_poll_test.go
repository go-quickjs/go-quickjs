//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// regPollSrc recurses natively with no loop: a tree of objects far past
// any memory limit, and a sum of calls that runs for minutes. Only the
// checks at register code's start (ssa.OpPoll) stop them: no back edge
// polls, and native calls are no interpreter's.
const regPollSrc = `
function tree(d) {
	if (d == 0) return { v: 1, s: "x" + d };
	return { l: tree(d - 1), r: tree(d - 1), d: d };
}
function spin(n) {
	if (n == 0) return 1;
	return (spin(n - 1) + spin(n - 1)) | 0;
}
`

// TestJITRegNativeHonoursLimits runs regPollSrc's recursions in register
// code, called natively, in a runtime with a memory limit and a deadline:
// the tree stops at the memory limit, the sum at the deadline, each in
// about the time the interpreter takes to notice -- not when the process
// runs out -- the collector idle and marking.
func TestJITRegNativeHonoursLimits(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			prog := compileForTest(t, regPollSrc)
			r := newTestRuntime(t, Config{JIT: true, MemoryLimit: 16 << 20})
			t.Cleanup(r.Close)
			r.jitStress = jitStressConfig{}
			r.SetJITWarmUp(1, 1)
			if _, err := r.Run(prog); err != nil {
				t.Fatal(err)
			}
			call := func(name string, arg float64, d time.Duration) error {
				ctx, cancel := context.WithTimeout(context.Background(), d)
				defer cancel()
				r.SetContext(ctx)
				defer r.SetContext(context.Background())
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
				_, err := r.Call(fn, Undefined, []Value{Float(arg)})
				r.ClearStop()
				return err
			}
			// Warm: compiled, and called natively by itself.
			for range 4 {
				if err := call("tree", 6, time.Minute); err != nil {
					t.Fatal(err)
				}
				if err := call("spin", 8, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			before := r.RegNativeStats()
			start := time.Now()
			if err := call("tree", 30, time.Minute); !errors.Is(err, ErrMemoryLimit) {
				t.Errorf("tree(30): %v, want the memory limit's error", err)
			}
			if took := time.Since(start); took > 20*time.Second {
				t.Errorf("tree(30) stopped after %v", took)
			}
			start = time.Now()
			if err := call("spin", 60, 200*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("spin(60): %v, want the deadline's error", err)
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("spin(60) stopped after %v", took)
			}
			if st := r.RegNativeStats(); st.Entries == before.Entries {
				t.Errorf("no native code ran")
			}
		})
	}
}
