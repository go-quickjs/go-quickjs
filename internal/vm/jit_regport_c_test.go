//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
)

// The state the tests of tier-up, of on-stack replacement and of compiles
// in the background read: the register code the cache holds for the
// function (jitRegEntry) and what outlives its compiles (its life, a
// jitEntry, whose counts are starts, deopts, ssaSerial, ssaStats, invalid,
// warming, tierBudget and nativeCalls).

// jitEntryOfForTest is the state of cl's function's code that outlives its
// compiles: its stack form's entry, or what its register code learned and
// did (regLearned); nil where it has none yet.
func jitEntryOfForTest(r *Runtime, cl *closure) *jitEntry {
	if r.jit == nil {
		return nil
	}
	return r.jit.regLearned[r.jitRegKeyOf(cl)]
}

// jitRegOfForTest is the register code the cache holds for cl's function,
// nil where it holds none.
func jitRegOfForTest(r *Runtime, cl *closure) *jitRegEntry {
	if r.jit == nil {
		return nil
	}
	return r.jit.regCache[r.jitRegKeyOf(cl)]
}

// jitHasCodeOfForTest reports whether cl's function has native code, valid
// or not: its stack form's entry's, or register code the cache holds.
func jitHasCodeOfForTest(r *Runtime, cl *closure) bool {
	e := jitRegOfForTest(r, cl)
	return e != nil && e.code != nil
}

// jitLinkOfForTest is the link native callers call cl's function through:
// its stack form's entry's, or the one its register code has by its key,
// asked for as a native caller asks for it (regLinkOf) where none has been
// yet -- the stack form's entry has one from its first compile on.
func jitLinkOfForTest(r *Runtime, cl *closure) *jitLink {
	return r.jitState().regLinkOf(cl, r.regOf(cl))
}

// jitRegLoopsForTest are the lengths of the loops of c, register code, in
// the order of their backward jumps: from a loop's header to its jump,
// what each of its back edges counts of its function's budget
// (jitRegEdges, regEdgeTick).
func jitRegLoopsForTest(c *regCode) []int {
	var n []int
	bl := c.rc.BackLive
	for i := 0; i+1 < len(bl); i += 2 {
		q := int(bl[i])
		// The jump goes to q+1+off, its header: -off instructions back.
		n = append(n, -int(regJumpOff(regAt(c.code, q))))
	}
	return n
}

// jitSiteMegamorphicForTest reports whether the property site of cl's
// function's first instruction of op -- its register code's first of
// regOps, where the runtime runs register code -- is megamorphic, e being
// what its code learned (jitRegSiteMegamorphic): its cache gave up, or its
// code learned it megamorphic at its exits (jitPolySite's mega), by the
// instruction's pc in its form.
func jitSiteMegamorphicForTest(t *testing.T, r *Runtime, cl *closure, e *jitEntry, op bytecode.Op, regOps ...bytecode.ROp) bool {
	t.Helper()

	c := r.regOf(cl)
	for rpc, w := range c.rc.Code {
		if !slices.Contains(regOps, w.Op()) {
			continue
		}
		if site := int(w.C()); site < len(cl.ic) && jitCacheMegamorphic(&cl.ic[site]) {
			return true
		}
		if e != nil {
			for i := range e.poly {
				if p := &e.poly[i]; int(p.pc) == rpc && p.mega {
					return true
				}
			}
		}
		return false
	}
	t.Fatalf("no %v in the register code", regOps)
	return false
}

// jitMethodSiteForTest is the cache, by its index among cl's caches, and
// the key of the method read of cl's function: its get_prop_this's, or its
// register code's get_method's.
func jitMethodSiteForTest(t *testing.T, r *Runtime, cl *closure) (int, Atom) {
	t.Helper()

	c := r.regOf(cl)
	for _, w := range c.rc.Code {
		if op := w.Op(); op == bytecode.RGetMethod || op == bytecode.RGetMethodW {
			site := int(w.C())
			return site, cl.names[c.rc.SiteName[site]]
		}
	}
	t.Fatal("no get_method in the register code")
	return 0, 0
}

// jitInlineSizeForTest is the length of cl's function in the instructions
// inlining's bounds count (ssa.MaxInline): its stack form's, or its
// register code's, converted (jitRegInlineSize).
func jitInlineSizeForTest(r *Runtime, cl *closure) int {
	return jitRegInlineSize(r.regOf(cl))
}

// jitLongRepeatsForTest is how many times a test's body repeats a
// statement of n in the stack form's, for a function as long, in the
// instructions inlining's bounds count, where runtimes with the JIT run
// register code: a repeat of x=(x*3+1)|0 is four of the stack form's
// instructions, and two and a half of register code's (jitRegInlineSize).
func jitLongRepeatsForTest(n int) int {
	return n * 3 / 2
}

// osrRegHeader is the register pc of the header of the only loop e's
// register code has no entry at, its objects left unmade there
// (jitRegEntry's unentered).
func osrRegHeader(t *testing.T, e *jitRegEntry) int {
	t.Helper()
	if e == nil || e.code == nil || len(e.unentered) != 1 {
		t.Fatalf("no one loop without an entry: %+v", e)
	}
	return e.unentered[0]
}

// regLoopHeaderForTest is the register pc of the header of the first loop
// of the global function name, by its backward jump.
func regLoopHeaderForTest(r *Runtime, name string) int {
	c := r.regOf(jitGlobalClosure(r, name))
	q := int(c.rc.BackLive[0])
	return q + 1 + int(regJumpOff(regAt(c.code, q)))
}

// jitRegJobForTest prepares a compile of cl's function's register code in
// the background as jitRegCompile does, for jitRegSubmit to hand over:
// lowered, its feedback asked ahead (jitRegSnapshotOf), to replace the
// code the cache holds for the function at its install.
func jitRegJobForTest(t testing.TB, r *Runtime, cl *closure) *jitRegJob {
	t.Helper()
	// The register code the function's calls made, read as they read it:
	// regOf writes the closure's, which a compile of it in the background
	// may be reading. The JIT's state is made here if nothing has been
	// compiled yet, as a first compile makes it.
	s, c := r.jitState(), cl.reg
	if c == nil {
		c = r.regOf(cl)
	}
	key := r.jitRegKeyOf(cl)
	p, err := r.jitRegLower(cl, c.rc, jitLowerSSA)
	if err != nil {
		t.Fatal(err)
	}
	life := s.regLearnedOf(key)
	e := &jitRegEntry{key: key, life: life, used: s.epoch(), p: p}
	s.regSerial++
	e.serial = s.regSerial
	r.jitRegSeenFeedback(cl, c)
	rf := r.jitRegFeedbackFor(cl, p)
	snap := jitRegSnapshotOf(rf, p)
	if snap.s.bad {
		t.Fatal("the snapshot's questions took back what they cannot")
	}
	return &jitRegJob{cl: cl, c: c, key: key, realm: r.Realm, e: e, p: p, rf: rf, snap: snap, deopts: life.deopts,
		w: jit.GetWorkspace(), enc: r.jit.enc, opts: r.jit.opts}
}

// jitRegPendingRecompileForTest is the register code of sum compiled on
// the spot, then compiled again in the background,
// not yet installed: its code, and the compile.
func jitRegPendingRecompileForTest(t *testing.T, r *Runtime) (*jitRegEntry, *jitRegJob) {
	t.Helper()
	cl := jitGlobalClosureForTest(r, "sum")
	stress := r.jitStress
	r.jitBackground, r.jitStress = false, jitStressConfig{}
	for range 2 {
		jitCallForTest(t, r, `sum(10)`, 45)
	}
	r.jitBackground, r.jitStress = true, stress
	e := jitRegOfForTest(r, cl)
	if e == nil || e.code == nil || e.invalid {
		t.Fatalf("sum has no code: %+v", e)
	}
	if ne := r.jitRegReoptimize(cl, r.regOf(cl), e, -1); ne != nil {
		t.Fatal("compiled again on the spot")
	}
	j := r.jit.regPendingJobs[e.key]
	if j == nil {
		t.Fatal("not compiled again in the background")
	}
	return e, j
}

// jitNamedForTest is the state of the code of the function named name
// that outlives its compiles (jitEntryOfForTest), found by its name among
// what the JIT holds, and its register code: nil for either it has none
// of.
func jitNamedForTest(r *Runtime, name string) (*jitEntry, *jitRegEntry) {

	if r.jit == nil {
		return nil, nil
	}
	var life *jitEntry
	for k, x := range r.jit.regLearned {
		if fn := k.Value(); fn != nil && fn.Name == name {
			life = x
		}
	}
	for k, x := range r.jit.regCache {
		if fn := k.Value(); fn != nil && fn.Name == name {
			return x.life, x
		}
	}
	return life, nil
}
