//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"fmt"
	"testing"
)

// jitRunners run the functions natively, by the baseline tier and by the
// optimizing tier, each checked to have compiled every function it ran.
func jitRunners() []supersRunner {
	compiled := func(opt bool) func(inst *Instance, f func()) {
		return func(inst *Instance, f func()) {
			f()
			for _, e := range inst.Module.Exports {
				name := e.Name
				if !nativeRan(inst, name) {
					panic(fmt.Sprintf("%s has no native code", name))
				}
				if opt && !optimized(inst, name) {
					panic(fmt.Sprintf("%s is not optimized", name))
				}
			}
		}
	}
	return []supersRunner{
		{"baseline", func(t *testing.T, src string) *Instance { return nativeInstance(t, src) }, compiled(false)},
		{"opt", func(t *testing.T, src string) *Instance { return optInstance(t, src) }, compiled(true)},
	}
}

// TestJITSupersAnswers holds both tiers' native tests and casts against
// types of every depth -- those past the supertypes an object's type keeps
// at hand, which native code finds by the chain's jumps, too -- to a walk of
// the chains, idle and with the collector marking.
func TestJITSupersAnswers(t *testing.T) {
	n := 260
	if testing.Short() {
		n = 140
	}
	for _, mode := range []string{"idle", "marking"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "marking" {
				jitMarkingForTest(t, false)
			}
			for seed := int64(1); seed <= 2; seed++ {
				t.Run(fmt.Sprint(seed), func(t *testing.T) {
					checkSupers(t, n, seed, jitRunners())
				})
			}
		})
	}
}

// BenchmarkJITCastDepth is BenchmarkCastDepth in each tier's native code.
func BenchmarkJITCastDepth(b *testing.B) {
	src := castBenchWAT()
	b.Run("baseline", func(b *testing.B) {
		benchCasts(b, nativeInstance(b, src), func(_ *Instance, f func()) { f() })
	})
	b.Run("opt", func(b *testing.B) {
		benchCasts(b, optInstance(b, src), func(_ *Instance, f func()) { f() })
	})
}
