//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"os"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// TestJITLowerRegAnInstructionAPC holds the lowering of register code to
// what a snapshot's callee filter takes of it (ssa.MayInlineSize): every
// register instruction lowers to at least one of the program's -- one it
// does not reach, or one a fused branch takes, to a Nop -- so that a
// callee whose register code is longer than an inlined program may be is
// never inlined, whichever lowering is made of it. The functions are
// jitSnapAskedSrc's, and with QUICKJS_JIT_CORPUS, Octane's.
func TestJITLowerRegAnInstructionAPC(t *testing.T) {
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
	lowered := 0
	for _, cl := range cls {
		c := r.regOf(cl)
		if c == nil {
			continue
		}
		for _, kind := range []jitLowerKind{jitLowerSSA, jitLowerInline, jitLowerForward} {
			p, err := r.jitRegLower(cl, c.rc, kind)
			if err != nil {
				continue
			}
			lowered++
			n := len(c.rc.Code)
			if len(p.Starts) != n+1 {
				t.Fatalf("%s: %d starts for %d register instructions", cl.fn.Name, len(p.Starts), n)
			}
			for pc := range n {
				if p.Starts[pc+1] <= p.Starts[pc] {
					t.Errorf("%s, lowering %d: register pc %d lowers to no instruction", cl.fn.Name, kind, pc)
					break
				}
			}
			if len(p.Code) < n {
				t.Errorf("%s, lowering %d: %d instructions of %d register ones", cl.fn.Name, kind, len(p.Code), n)
			}
			if !ssa.MayInlineSize(n) && !ssa.NeverInlined(p) {
				t.Errorf("%s, lowering %d: %d register instructions, too many to inline, but its program may be", cl.fn.Name, kind, n)
			}
		}
	}
	if lowered == 0 {
		t.Fatal("nothing lowered")
	}
	t.Logf("%d lowerings of %d functions", lowered, len(cls))
}
