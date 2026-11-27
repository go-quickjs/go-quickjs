//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"os"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit"
)

func init() {
	// The inline sites of a snapshot whose answers were asked on the
	// runtime's goroutine (jitWalkEager): the snapshot holds their effects,
	// which the feedback asked again gives no more (askedOf).
	jitSnapGatheredAt = append(jitSnapGatheredAt, func(s *jitSnapshot, m jitSnapMethod, _ int64) bool {
		return jitInlineMethods&(1<<m) != 0 && !s.d.inline.derives
	})
}

// TestJITSnapshotOfNoProgram holds a snapshot gathered of the register code
// alone (jitRegSnapshotOf of no program), its program bound after it, as
// the worker binds the one it lowers (program), to the snapshot gathered of
// the program: both build the same function. A function whose asking asks
// of the program on the runtime's goroutine all the same has none (nil),
// counted. The functions are jitSnapAskedSrc's, and with
// QUICKJS_JIT_CORPUS, Octane's.
func TestJITSnapshotOfNoProgram(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var r *Runtime
	var cls []*closure
	if dir := os.Getenv("QUICKJS_JIT_CORPUS"); dir != "" {
		r, cls = jitOctaneCorpusForTest(t, dir)
	} else {
		r = jitRuntimeForTest(t, Config{JIT: true})
		r.jitState().unavailable = true
		if _, err := r.Run(compileForTest(t, jitSnapAskedSrc)); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"work", "make", "add", "Point"} {
			cls = append(cls, jitGlobalClosureForTest(r, name))
		}
	}
	r.jitState().unavailable = false
	built, none, differ := 0, 0, 0
	for _, cl := range cls {
		c := r.regOf(cl)
		if c == nil {
			continue
		}
		if _, err := r.jitRegLower(cl, c.rc, jitLowerSSA); err != nil {
			continue
		}
		j := jitRegJobForTest(t, r, cl)
		want := jitSnapBuildForTest(t, j, j.snap)
		free := jitRegSnapshotOf(r.jitRegFeedbackFor(cl, j.p), nil)
		if free == nil {
			none++
			jit.PutWorkspace(j.w)
			continue
		}
		free.s.program(j.p)
		got := jitSnapBuildForTest(t, j, free)
		jit.PutWorkspace(j.w)
		built++
		if got != want {
			differ++
			if differ <= 3 {
				t.Errorf("%s: built of no program:\n%s\nof the program:\n%s", cl.fn.Name, got, want)
			}
		}
	}
	if built == 0 {
		t.Fatal("nothing built of no program")
	}
	t.Logf("%d functions built of no program, %d asked of it, %d differed", built, none, differ)
}
