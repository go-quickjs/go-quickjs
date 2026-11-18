//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A register compile is placed only where there is room for its code and
// for what it had its function's state hold besides -- the pools of the
// closures it makes, filled as it was built. Placed in room for the code
// alone, top-level code declaring forty functions held 106 KB of code and
// 214 KB of state at a budget of 256 KB on arm64 under CI's stress
// (TestJITRegRealmsWithinTheBudgetClocked), neither the cache's
// to reclaim while the code ran. Here a function making arrows is compiled
// once with room to spare, which measures its code and its state, and then
// at a budget its code fits and the two do not: the JIT holds no more than
// the budget after each run.
func TestJITRegPlaceCountsStateGrowth(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var arrows []string
	for k := range 24 {
		arrows = append(arrows, fmt.Sprintf("() => i + %d", k))
	}
	src := `function hot(n) { const out = []; for (let i = 0; i < n; i++) out.push(` + strings.Join(arrows, ", ") + `); return out.length }`
	setup := compileForTest(t, src)
	warm, run := compileForTest(t, `hot(4)`), compileForTest(t, `hot(4000)`)
	runAll := func(limit int64) (*Runtime, int, int) {
		r := jitRuntimeForTest(t, Config{JIT: true, JITMemoryLimit: limit})
		for i, p := range []*bytecode.Function{setup, warm, run, run, run} {
			if _, err := r.Run(p); err != nil {
				t.Fatal(err)
			}
			if s := r.jit; s != nil && s.held() > s.budget {
				t.Fatalf("budget %d KB: held %d of %d after run %d: register code and state %d, programs %d, shared %d",
					limit>>10, s.held(), s.budget, i, s.regHeld, s.lowerBytes, s.sharedBytes())
			}
		}
		e := jitRegEntryNamedForTest(r, "hot")
		if e == nil || e.life == nil {
			return r, 0, 0
		}
		return r, e.bytes, e.life.bytes
	}
	r, code, state := runAll(64 << 20)
	if code == 0 || state < code/2 {
		t.Fatalf("hot's code %d bytes, its state %d: the test wants a state at least half the code's size", code, state)
	}
	held := r.jit.held()
	// Room for hot's code and all else, short of half its state.
	runAll(int64(held - (state-jitEntryFixed)/2))
}
