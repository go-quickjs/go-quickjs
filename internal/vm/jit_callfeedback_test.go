//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"reflect"
	"slices"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A worker reads the descriptions captured before the runtime changes
// its call targets. Building and rewinding must leave those answers intact.
func TestJITCallFeedbackSnapshot(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.SetJITBackground(false)
	if _, err := r.Run(compileForTest(t, regBackgroundSrc+`work(128)`)); err != nil {
		t.Fatal(err)
	}
	j := jitRegJobForTest(t, r, jitGlobalClosureForTest(r, "work"))
	defer jit.PutWorkspace(j.w)
	type answer struct {
		sites, want []ssa.CallSite
	}
	var answers []answer
	snapshots := append([]*jitSnapshot{j.snap.s}, mapValues(j.snap.s.callees)...)
	for _, s := range snapshots {
		for _, e := range s.a.NativeCalls.e {
			if e.answered {
				sites := e.v.r0
				answers = append(answers, answer{sites, slices.Clone(sites)})
			}
		}
	}
	if len(answers) == 0 {
		t.Fatal("snapshot has no native-call feedback")
	}
	if _, err := r.Run(compileForTest(t, `norm=function(p){return 99};work(32)`)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		j.workRecovering()
		close(done)
	}()
	unchanged := true
	for range 100 {
		for _, a := range answers {
			unchanged = reflect.DeepEqual(a.sites, a.want) && unchanged
		}
	}
	<-done
	if j.err != nil || j.buildErr != nil || j.missed || j.mc == nil {
		t.Fatalf("worker: compile %v, build %v, missed %v", j.err, j.buildErr, j.missed)
	}
	j.w.SSA.Rewind()
	j.w.MIR.Rewind()
	for _, a := range answers {
		unchanged = reflect.DeepEqual(a.sites, a.want) && unchanged
	}
	if !unchanged {
		t.Fatal("worker or workspace rewind changed captured call feedback")
	}
}
