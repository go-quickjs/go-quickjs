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

// TestJITSnapshotCalleeWalk holds the callees a compile's gathers take on
// the runtime's goroutine, by Inline's decision before it lowers
// (jitInlinePrefix) and the loops' callbacks and the function's templates
// (jit_snapwalk.go), to the callees the worker's derivations take: each
// one an answer names as inlined -- Inline's, ForEach's, CalledClosure's --
// is one the gathers took, so that no derivation misses (jitWalkData's
// missed); and a compile whose asking met what a worker cannot decide is
// counted by why. The functions are jitSnapAskedSrc's, and with
// QUICKJS_JIT_CORPUS, Octane's.
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
	if b := jitSnapNodesBlocked(); b != 0 {
		t.Fatalf("methods asked at a PC neither derived nor gathered: %#x", b)
	}
	functions, walked, taken, misses := 0, 0, 0, 0
	eager := map[jitWalkEager]int{}
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
		snap := jitRegSnapshotOf(r.jitRegFeedbackFor(cl, p), p)
		functions++
		w := snap.s.d.walk
		if w == nil || w.eager != jitWalkDerived {
			if w != nil {
				eager[w.eager]++
			}
			continue
		}
		missed := func() (missed any) {
			defer func() {
				if v := recover(); v != nil {
					if _, ok := v.(jitSnapMiss); !ok {
						panic(v)
					}
					missed = v
				}
			}()
			snap.s.derive()
			return nil
		}()
		walked += len(w.order)
		for _, n := range w.order {
			if n.decided {
				taken++
			}
		}
		if missed != nil || w.missed {
			misses++
			if misses <= 20 {
				t.Errorf("%s: a derivation took a callee the gathers did not: %v", cl.fn.Name, missed)
			}
		}
	}
	if functions == 0 {
		t.Fatal("nothing compiled")
	}
	t.Logf("%d functions: %d callees gathered, %d taken, %d compiles missed one; asked on the goroutine: %d forwarder, %d parked, %d typed, %d sites",
		functions, walked, taken, misses, eager[jitWalkForwarder], eager[jitWalkParked], eager[jitWalkTyped], eager[jitWalkSites])
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
