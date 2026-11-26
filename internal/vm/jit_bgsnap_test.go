//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
)

func TestJITBackgroundSnapshotStatePCs(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.SetJITBackground(false)
	if _, err := r.Run(compileForTest(t, `function work(n) {
		let s = 0; for (let i = 0; i < n; i++) s += i; return s;
	} work(128)`)); err != nil {
		t.Fatal(err)
	}
	j := jitRegJobForTest(t, r, jitGlobalClosureForTest(r, "work"))
	defer jit.PutWorkspace(j.w)
	n := len(j.cl.fn.RegCode().Code)
	if n < 8 {
		t.Fatalf("fixture has only %d register instructions", n)
	}
	for _, mode := range []string{"no-entry", "empty", "learned", "recursive", "cached"} {
		t.Run(mode, func(t *testing.T) {
			fb := r.jitRegFeedbackFor(j.cl, j.p)
			e := &jitEntry{jitLearned: &jitLearned{}}
			fb.fb.e = e
			var resumed, generic []int
			switch mode {
			case "no-entry":
				fb.fb.e = nil
				fb.fb.selfKnown = true
				fb.fb.selfCalls = []int32{1}
			case "learned":
				e.resumed = []uint32{0, 3, 3, uint32(n - 1), uint32(n), ^uint32(0)}
				e.failedEntries = []uint32{3, 4, 4, uint32(n - 1), uint32(n), ^uint32(0)}
				resumed, generic = []int{0, 3, n - 1}, []int{3, 4, n - 1}
			case "recursive", "cached":
				e.resumed = []uint32{2, 4}
				e.failedEntries = []uint32{0, 4}
				generic = []int{0, 4}
				self := &closure{fn: j.cl.fn}
				other := &closure{fn: &bytecode.Function{}}
				e.nativeCalls = []jitInline{{pc: 1, cl: self}, {pc: 3, cl: other}, {pc: 4}, {pc: -1, cl: self}}
				e.inlines = []jitInline{{pc: 5, cl: self}, {pc: 1, cl: self}, {pc: int32(n - 2), cl: self},
					{pc: -2, cl: self}, {pc: int32(n - 1), cl: self}, {pc: 1<<31 - 1, cl: self}}
				resumed = []int{0, 2, 4, 6, n - 1}
				if mode == "cached" {
					fb.fb.selfKnown = true
					fb.fb.selfCalls = []int32{-1, 2, 2, int32(n - 2), -2, int32(n - 1), 1<<31 - 1}
					resumed = []int{0, 2, 3, 4, n - 1}
				}
			}
			snap := jitRegSnapshotOf(fb, j.p)
			// Replace every source after capture, including the cached call PCs.
			clear(e.resumed)
			clear(e.failedEntries)
			clear(e.nativeCalls)
			clear(e.inlines)
			clear(fb.fb.selfCalls)
			for pc := range n {
				if got, want := snap.Resumed(pc), slices.Contains(resumed, pc); got != want {
					t.Errorf("Resumed(%d) = %v, want %v", pc, got, want)
				}
				if got, want := snap.EntryGeneric(pc), slices.Contains(generic, pc); got != want {
					t.Errorf("EntryGeneric(%d) = %v, want %v", pc, got, want)
				}
			}
			for _, pc := range []int{-1, n, int(^uint32(0))} {
				for _, m := range []jitSnapMethod{snapResumed, snapEntryGeneric} {
					func() {
						defer func() {
							if miss, ok := recover().(jitSnapMiss); !ok || miss != (jitSnapMiss{m, int64(pc), 0}) {
								t.Errorf("out-of-range method %d at %d: got %#v", m, pc, miss)
							}
						}()
						if m == snapResumed {
							snap.Resumed(pc)
						} else {
							snap.EntryGeneric(pc)
						}
					}()
				}
			}
		})
	}
}

// A compile in the background asks its snapshot nothing ahead of a callee
// no site inlines (ssa.NeverInlined), whose feedback the build never asks
// -- each callee the function had met had been asked every question,
// nested to jitSnapDepth: Typescript's snapshots took 620 ms on the
// runtime's goroutine, more than its compiles on the spot took. A callee
// it may inline is asked as before, and the build misses nothing.
func TestJITBackgroundSnapshotSkipsLongCallees(t *testing.T) {
	t.Parallel()
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var long strings.Builder
	long.WriteString("function long(x) { var s = x;")
	for i := range 80 {
		long.WriteString(" s = (s * 31 + " + string(rune('0'+i%10)) + ") % 1000003;")
	}
	long.WriteString(" return s }\n")
	r := jitRuntimeForTest(t, Config{JIT: true})
	src := long.String() + `function small(x) { return x + 1 }
		function f(n) { var s = 0; for (var i = 0; i < n; i++) s += small(i) + long(i); return s }`
	if _, err := r.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	want, err := r.Run(compileForTest(t, "f(300)"))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := r.Run(compileForTest(t, "f(300)")); err != nil {
			t.Fatal(err)
		}
	}
	j := jitRegJobForTest(t, r, jitGlobalClosureForTest(r, "f"))
	asked := map[string]int{}
	for k, c := range j.snap.s.callees {
		if k.cl != nil {
			asked[k.cl.fn.Name] = c.a.answered()
		}
	}
	if n, ok := asked["small"]; !ok || n == 0 {
		t.Errorf("small: asked %v, %d answers: want its questions asked ahead (%v)", ok, n, asked)
	}
	if n := asked["long"]; n != 0 {
		t.Errorf("long: %d answers: want none asked of a callee no site inlines (%v)", n, asked)
	}
	r.SetJITBackground(true)
	r.jitStress = jitStressConfig{background: true, checks: 1}
	r.jitRegSubmit(j)
	v, err := r.Run(compileForTest(t, "f(300)"))
	if err != nil || v.Number() != want.Number() {
		t.Fatalf("f(300) = %v, %v; want %v", v, err, want)
	}
	if j.missed || r.jit.regSnapMisses != 0 || r.jit.regPendingJobs[j.key] != nil {
		t.Errorf("missed %v (%d), pending %v: want the build from the snapshot installed", j.missed, r.jit.regSnapMisses, r.jit.regPendingJobs[j.key] != nil)
	}
}
