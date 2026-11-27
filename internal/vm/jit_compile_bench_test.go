//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

func BenchmarkJITCallFeedback(b *testing.B) {
	for _, mode := range []string{"cached", "snapshot"} {
		b.Run(mode, func(b *testing.B) {
			sites := []ssa.CallSite{{Go: ssa.GoCall, Argc: 1, ThisSlot: -1}}
			f := &jitRegFeedback{
				p:     &ir.Program{Code: []ir.Instruction{{Op: ir.Host}}},
				asked: []jitRegAsked{{calls: sites, did: askedCalls}}, askedAt: []int32{1},
			}
			s := &jitSnapshot{}
			s.a.NativeCalls.put(0, 0, true, jitSnapAnsNativeCalls{r0: sites})
			s.root = s
			var fb ssa.Feedback = f
			if mode == "snapshot" {
				fb = &jitRegSnapshot{s: s}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				got := fb.NativeCalls(0)
				if len(got) != 1 || got[0].Argc != 1 {
					b.Fatal("call feedback changed")
				}
			}
		})
	}
}

// Measure snapshot preparation separately from the work a background worker
// does. Repeating compiles through v8bench's -jitcompile uses live feedback.
func BenchmarkJITBackgroundCompile(b *testing.B) {
	r := newTestRuntime(b, Config{JIT: true})
	r.SetJITBackground(false)
	if _, err := r.Run(compileForTest(b, `
function work(n) {
	let s = 0;
	for (let i = 0; i < n; i++) s += JSON.stringify([i, i + 1]).length;
	return s;
}
work(128)`)); err != nil {
		b.Fatal(err)
	}
	j := jitRegJobForTest(b, r, jitGlobalClosureForTest(r, "work"))
	defer jit.PutWorkspace(j.w)
	b.Run("snapshot", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			fb := r.jitRegFeedbackFor(j.cl, j.p)
			s := jitRegSnapshotOf(fb, j.p)
			if s.s.bad || s.s.a.answered() == 0 {
				b.Fatal("invalid snapshot")
			}
		}
	})
	b.Run("worker", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			j.snap.s.uses = j.snap.s.uses[:0]
			j.workRecovering()
			if j.err != nil || j.buildErr != nil || j.missed || j.mc == nil {
				b.Fatalf("worker: compile %v, build %v, missed %v", j.err, j.buildErr, j.missed)
			}
			j.w.SSA.Rewind()
			j.w.MIR.Rewind()
		}
	})
}
