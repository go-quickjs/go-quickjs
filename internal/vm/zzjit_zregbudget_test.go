//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"strings"
	"testing"
)

// regBudgetFuncs is how many functions each of regBudgetSrc's programs
// has.
const regBudgetFuncs = 24

// regBudgetSrc is two programs of regBudgetFuncs functions each, a<i> and
// b<i>, a loop each, which driveA and driveB call natively, none inlined:
// the first fills the JIT's budget, the second is compiled after it, its
// callees at its calls of Go, under its native frame.
func regBudgetSrc() string {
	var b strings.Builder
	for _, p := range []string{"a", "b"} {
		for i := 0; i < regBudgetFuncs; i++ {
			fmt.Fprintf(&b, "function %s%d(n) { var s = %d; for (var k = 0; k < n; k++) s = (s * 31 + k * %d + (s >> 3)) | 0; return s }\n", p, i, i, i+1)
		}
		fmt.Fprintf(&b, "function drive%s(n) { var s = 0; for (var i = 0; i < n; i++) {", strings.ToUpper(p))
		for i := 0; i < regBudgetFuncs; i++ {
			fmt.Fprintf(&b, " s = (s + %s%d(i & 7)) | 0;", p, i)
		}
		b.WriteString(" } return s }\n")
	}
	return b.String()
}

// The register code of a program the program no longer runs is reclaimed
// for a later one's, its programs kept for compiles with it, under the
// later program's native frame: driveA's callees fill the budget, and
// driveB's, compiled at driveB's calls of Go, find the room. Before, every
// function native callers call counted as in use while any native code ran
// (regInUse) -- and a program kept for inlining or compiling again went
// only with its function's code -- so none of driveA's was let go for
// driveB's callees, which were refused room at every call: Octane's
// Typescript and zlib after Mandreel. Each answer is the interpreter's,
// the collector idle and marking; "room" makes it at the compiles
// (makeRoom), "flush" lets the unused code go first (regFlush).
func TestJITRegBudgetFreesUnused(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, how := range []string{"room", "flush"} {
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/marking=%v", how, marking), func(t *testing.T) {
				if marking {
					jitMarkingForTest(t)
				}
				setEpoch := jitFlushForTest(t)
				if how == "room" {
					jitNoFlushForTest(t)
				}
				regNoInline(t)
				rts := regNativeRuntimes(t, regBudgetSrc())
				r := rts[0]
				// Compiled at their first calls: each drive before its
				// callees, which its calls of Go compile.
				r.SetJITWarmUp(1, 1)
				call := func(name string, n float64) [2]string {
					var got [2]string
					for i, rt := range rts {
						fn, _ := rt.GetProp(Obj(rt.Global()), rt.Intern(name))
						v, err := rt.Call(fn, Undefined, []Value{Float(n)})
						got[i] = regNativeShow(rt, v, err)
					}
					if got[0] != got[1] {
						t.Fatalf("%s(%v): with the JIT %q, without %q", name, n, got[0], got[1])
					}
					return got
				}
				native := func(p string) int {
					n := 0
					for i := 0; i < regBudgetFuncs; i++ {
						cl := jitGlobalClosure(r, fmt.Sprintf("%s%d", p, i))
						if e := r.jit.regCache[r.jitRegKeyOf(cl)]; e != nil && e.code != nil {
							n++
						}
					}
					return n
				}
				epoch := uint64(1000)
				// idle has the code compiled so far found unused from here
				// on: old at the next compile's room ("room"), or flushed at
				// it, the flush's second look ("flush").
				idle := func() {
					s := r.jit
					if how == "flush" {
						setEpoch(epoch + jitFlushAge)
						s.flush()
						epoch += 2 * jitFlushAge
					} else {
						// Its uses noted at a look of the cache's in an epoch
						// of their own, then old.
						epoch++
						setEpoch(epoch)
						s.age(epoch)
						epoch += jitCodeAge + 1
					}
					setEpoch(epoch)
				}
				// driveA(40)'s answer, and driveB(40)'s, as Node gives them.
				const want = "2129559703"
				run := func(p string, first bool) {
					var refused0, evicted0, flushed0 uint64
					if s := r.jit; s != nil {
						refused0, evicted0, flushed0 = s.refusedRoom, s.evicted, s.flushed
					}
					for range 4 {
						if got := call("drive"+strings.ToUpper(p), 40); got[0] != want {
							t.Fatalf("drive%s(40) = %s, want %s", strings.ToUpper(p), got[0], want)
						}
					}
					if n := native(p); n != regBudgetFuncs {
						t.Errorf("%d of drive%s's %d callees compiled", n, strings.ToUpper(p), regBudgetFuncs)
					}
					if first {
						return
					}
					s := r.jit
					if s.refusedRoom != refused0 {
						t.Errorf("drive%s: %d compiles refused room", strings.ToUpper(p), s.refusedRoom-refused0)
					}
					if held := s.held(); held > r.jitBudget() {
						t.Errorf("drive%s: holds %d of %d", strings.ToUpper(p), held, r.jitBudget())
					}
					if how == "room" && s.evicted == evicted0 {
						t.Errorf("drive%s: no code dropped to make room", strings.ToUpper(p))
					}
					if how == "flush" && s.flushed == flushed0 {
						t.Errorf("drive%s: no code flushed", strings.ToUpper(p))
					}
					t.Logf("drive%s: %d dropped, %d flushed, holds %d of %d", strings.ToUpper(p), s.evicted-evicted0, s.flushed-flushed0, s.held(), r.jitBudget())
				}
				run("a", true)
				s := r.jit
				// Room for driveA's callees and about half of driveB's: each
				// program's compiles need what the other's left.
				held := s.held()
				r.jitMemoryLimit = int64(held + held/3)
				idle()
				// driveB's callees, compiled as the interpreter calls them.
				run("b", false)
				idle()
				// driveA's again, compiled at driveA's calls of Go, under its
				// native frame: driveB's code, which native callers call, is
				// not in use.
				run("a", false)
				if how == "flush" {
					// What the flush let go: driveB's callees' code and
					// programs, which no compile asked for since.
					for i := 0; i < regBudgetFuncs; i++ {
						key := r.jitRegKeyOf(jitGlobalClosure(r, fmt.Sprintf("b%d", i)))
						if e := s.regCache[key]; e != nil && e.code != nil {
							t.Errorf("b%d's unused code kept", i)
						}
						if s.regLowered[key] != nil {
							t.Errorf("b%d's unused programs kept", i)
						}
					}
				}
			})
		}
	}
}
