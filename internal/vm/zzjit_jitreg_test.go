//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/jit"
)

// jitRegLifeForTest is the state of the global function name's register
// code that outlives its compiles (jitRegLifeOf), if the cache holds code
// compiled from it, and nil otherwise: its counts -- entries, hosts,
// guards, deopts -- and what the register code's runs learn.
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
	return jitRegLifeForTest(r, name)
}

// jitSSACodeForTest is the native code of the global function name, nil
// where it has none: its stack form's entry's, or its register code's.
func jitSSACodeForTest(r *Runtime, name string) *jit.SSACode {
	cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	if e := r.jit.regCache[r.jitRegKeyOf(cl)]; e != nil {
		return e.code
	}
	return nil
}

// jitNativeUsesForTest is how often the global function name's code ran
// natively: the stretches Go entered it for -- at its start, at a loop or
// after an exit -- and the calls of it native callers made through its
// link, in either form.
func jitNativeUsesForTest(r *Runtime, name string) uint64 {
	cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
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

// jitHasCodeForTest reports whether r's JIT holds native code.
func jitHasCodeForTest(r *Runtime) bool {
	if r.jit == nil {
		return false
	}
	for _, e := range r.jit.regCache {
		if e.code != nil {
			return true
		}
	}
	return false
}
