//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit"
)

// regJITForTest reports whether runtimes made with the JIT run register
// code, which their JIT compiles (RegTier): QJS_JITREG set, or a test's
// SetJITRegTier. A runtime that keeps the old tiers (jitRuntimeForTest,
// newOldTiersForTest) runs the stack form's whatever it says.
func regJITForTest() bool { return regTier.Load() && jitRegTier.Load() }

// skipUnderRegJIT skips a test of the stack form's JIT where runtimes run
// register code: one of a mechanism the register form has no counterpart
// of -- the stack bytecode's shapes, the tree tier's and the stack
// interpreter's promotion, the stack JIT's own counters -- which goes with
// the stack form and its tests. why says what the test is of.
func skipUnderRegJIT(t *testing.T, why string) {
	t.Helper()
	if regJITForTest() {
		t.Skip("the register JIT runs this runtime's code: " + why)
	}
}

// jitRegCodeForTest reports whether the global function name runs register
// code in r, which r's JIT compiles: r runs register code, and its closure
// was given its function's. A function given the old tiers' -- one the
// register form does not cover, or loaded while a test had the register
// interpreter or the tree tier off -- has the stack form's code, entry
// and link, whatever r runs.
func jitRegCodeForTest(r *Runtime, name string) bool {
	return r.regOn() && r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.reg != nil
}

// jitRegLifeForTest is the state of the global function name's register
// code that outlives its compiles (jitRegLifeOf), if the cache holds code
// compiled from it, and nil otherwise: the register form's counterpart of
// its stack form's entry, whose counts -- entries, hosts, guards, deopts --
// and learning the register code's runs keep.
func jitRegLifeForTest(r *Runtime, name string) *jitEntry {
	cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	if r.jit == nil || cl.closureCode == nil {
		return nil
	}
	e := r.jit.regCache[r.jitRegKeyOf(cl)]
	if e == nil || e.code == nil || e.life == nil {
		return nil
	}
	return e.life
}

// jitCodeForTest is the entry of the global function name's code, nil
// where it has none: its stack form's entry with code, or, where it runs
// register code (jitRegCodeForTest), its register code's state
// (jitRegLifeForTest).
func jitCodeForTest(r *Runtime, name string) *jitEntry {
	if jitRegCodeForTest(r, name) {
		return jitRegLifeForTest(r, name)
	}
	e := r.jit.entryFor(r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn)
	if e == nil || e.ssa == nil {
		return nil
	}
	return e
}

// jitSSACodeForTest is the native code of the global function name, nil
// where it has none: its stack form's entry's, or its register code's.
func jitSSACodeForTest(r *Runtime, name string) *jit.SSACode {
	if jitRegCodeForTest(r, name) {
		cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
		if e := r.jit.regCache[r.jitRegKeyOf(cl)]; e != nil {
			return e.code
		}
		return nil
	}
	if e := jitCodeForTest(r, name); e != nil {
		return e.ssa
	}
	return nil
}

// jitNativeUsesForTest is how often the global function name's code ran
// natively: the stretches Go entered it for -- at its start, at a loop or
// after an exit -- and the calls of it native callers made through its
// link, in either form.
func jitNativeUsesForTest(r *Runtime, name string) uint64 {
	cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	if jitRegCodeForTest(r, name) {
		e := jitRegLifeForTest(r, name)
		if e == nil {
			return 0
		}
		n := e.ssaStats.entries
		if k := r.jit.regLinks[r.jitRegKeyOf(cl)]; k != nil {
			n += k.in
		}
		return n
	}
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil {
		return 0
	}
	n := e.ssaStats.entries
	if e.link != nil {
		n += e.link.in
	}
	return n
}

// jitHasCodeForTest reports whether r's JIT holds code of either form.
func jitHasCodeForTest(r *Runtime) bool {
	if r.jit == nil {
		return false
	}
	for _, e := range r.jit.regCache {
		if e.code != nil {
			return true
		}
	}
	return len(r.jit.cache) != 0
}
