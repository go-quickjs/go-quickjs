//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// A function whose compile is deferred for want of room, the JIT's memory
// full of young code, warms up again before it is asked for again, as the
// stack form's deferred entry does (tryJITAt): its loop's back edges ask
// once a budget of work, not at each check. Its budget left spent, every
// check of its loop had asked -- each a lookup of what it learned and the
// cache's settling -- millions of times in a run of
// TestJITRegRealmsWithinTheBudgetClocked, three quarters of its time.
func TestJITRegDeferredWarmsUpAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	compiler.SetRegisterForm(true)
	fn := compileForTest(t, jitHotProgram(40))
	call := compileForTest(t, `f39(100000)`)
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	was := SetJITRegTier(true)
	epoch, flush := jitEpochNow, jitFlushNow
	t.Cleanup(func() {
		jitEpochNow, jitFlushNow = epoch, flush
		SetRegTier(bytecode.RegDefault())
		SetJITRegTier(was)
	})
	// Clocks that do not move: the code compiled first stays young, and
	// what comes after it is deferred.
	jitEpochNow = func() uint64 { return 1000 }
	jitFlushNow = func() uint64 { return 250 }
	r := New(Config{JIT: true, JITMemoryLimit: 96 << 10})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	// Its asks counted at the JIT's own back-edge budget, not
	// QJS_JIT_STRESS's, which checks every few back edges, and compiled
	// on the spot.
	r.jitStress = jitStressConfig{}
	r.SetJITWarmUp(8, 8)
	if _, err := r.Run(fn); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f39")).value().Object().fn().closure
	s := r.jit
	life := s.regLearned[r.jitRegKeyOf(cl)]
	if life == nil || !life.deferred || life.generation != s.generation || cl.closureCode.jitReg != nil {
		t.Fatalf("f39 is not deferred for want of room: %+v", life)
	}
	c := cl.reg
	asked := s.refusedRoom
	if _, err := r.Run(call); err != nil {
		t.Fatal(err)
	}
	// A hundred thousand turns of a loop no longer than the function, a
	// budget of eight invocations' worth of its length between asks.
	n := s.refusedRoom - asked
	if most := uint64(100000/8 + 2); n == 0 || n > most {
		t.Errorf("f39 asked for %d times over 100,000 turns of its loop, want 1 to %d (its length %d)", n, most, len(c.rc.Code))
	}
	if !life.deferred || cl.closureCode.jitReg != nil {
		t.Errorf("f39 compiled with the JIT's memory full of young code")
	}
}
