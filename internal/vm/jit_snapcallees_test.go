//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"os"
	"testing"
	"unsafe"

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

// TestJITSnapshotCalleeWalk holds the walk of the callees a compile may
// inline (jitSnapCalleeWalk), made before the snapshot's questions are
// asked, to the callees the snapshot then asks: each one whose feedback it
// asks -- one the answers name as inlined, short enough for the inliner
// to take -- is one the walk found, so that a snapshot copying the walk's
// callees' state has every one the worker's inliner may take. The
// functions are jitSnapAskedSrc's, and with QUICKJS_JIT_CORPUS, Octane's.
func TestJITSnapshotCalleeWalk(t *testing.T) {
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
	functions, asked, walked, missed := 0, 0, 0, 0
	for _, cl := range cls {
		c := r.regOf(cl)
		if c == nil {
			continue
		}
		p, err := r.jitRegLower(cl, c.rc, jitLowerSSA)
		if err != nil {
			continue
		}
		r.jitRegSeenFeedback(cl, c)
		walk := r.jitSnapCalleeWalk(&jitSnapData{rt: r}, cl)
		snap := jitRegSnapshotOf(r.jitRegFeedbackFor(cl, p), p)
		functions++
		walked += len(walk)
		for k, s := range snap.s.callees {
			if !s.d.gathered || k.cl == nil || k.cl == cl {
				continue
			}
			asked++
			if walk[k.cl] == 0 {
				missed++
				if missed <= 20 {
					t.Errorf("%s: callee %s, %d deep, asked but not walked%s", cl.fn.Name, k.cl.fn.Name, s.depth, jitSnapNamedBy(snap.s, k.cl))
				}
			}
		}
	}
	if functions == 0 {
		t.Fatal("nothing compiled")
	}
	t.Logf("%d functions: %d callees asked, %d walked, %d asked not walked", functions, asked, walked, missed)
}

// jitSnapNamedBy says which answers of the snapshots of root's compile
// name cl's closure as a callee to inline, for a test's message.
func jitSnapNamedBy(root *jitSnapshot, cl *closure) string {
	at := uintptr(unsafe.Pointer(cl))
	out := ""
	for _, s := range append([]*jitSnapshot{root}, mapValues(root.callees)...) {
		name := "?"
		if s.fb != nil && s.fb.fn != nil {
			name = s.fb.fn.Name
		}
		for _, e := range s.a.Inline.e {
			if e.answered && e.v.r0.Closure == at {
				out += fmt.Sprintf("; Inline(%d) of %s", e.arg, name)
			}
		}
		for _, e := range s.a.CalledClosure.e {
			if e.answered && e.v.r1.Closure == at {
				out += fmt.Sprintf("; CalledClosure(%d) of %s", e.arg, name)
			}
		}
		for _, e := range s.a.Accessor.e {
			if e.answered && e.v.r0.Inline != nil && e.v.r0.Inline.Closure == at {
				out += fmt.Sprintf("; Accessor(%d) of %s", e.arg, name)
			}
		}
		for _, e := range s.a.ForEach.e {
			if e.answered && e.v.r0.Inline != nil && e.v.r0.Inline.Closure == at {
				out += fmt.Sprintf("; ForEach(%d) of %s", e.arg, name)
			}
		}
	}
	return out
}
