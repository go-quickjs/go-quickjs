//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The state of a closure's code in either form, for the tests of
// zzjit_native_test.go that read it: the stack form's entry the cache
// holds for its function, or the register form's code entry and the
// state that outlives its compiles (jitRegEntry's life), whose counts --
// entries, hosts, guards, deopts, calls of Go -- and learning (nativeCalls,
// inlines, notNative, entrySlow) the register code's runs keep as the
// stack form's entry keeps its own.

// jitPortRegEntry is cl's register code entry in r's cache, with or
// without code; nil where the cache holds none, or cl runs the stack
// form's code.
func jitPortRegEntry(r *Runtime, cl *closure) *jitRegEntry {
	if r.jit == nil {
		return nil
	}
	return r.jit.regCache[r.jitRegKeyOf(cl)]
}

// jitPortCode is the entry of cl's native code in r, nil where it has
// none: the stack form's entry the cache holds for its function, with
// code, or the life of the register code the cache holds for it.
func jitPortCode(r *Runtime, cl *closure) *jitEntry {
	if r.jit == nil {
		return nil
	}
	e := r.jit.regCache[r.jitRegKeyOf(cl)]
	if e == nil || e.code == nil {
		return nil
	}
	return e.life
}

// jitPortLife is the entry of what cl's function's runs did and learned,
// whether it has code now or not, nil where there is none: the stack
// form's entry (its hint), or the register form's state that outlives
// its compiles, made by none of this.
func jitPortLife(r *Runtime, cl *closure) *jitEntry {
	if r.jit == nil {
		return nil
	}
	if e := cl.closureCode.jitReg; e != nil && e.life != nil {
		return e.life
	}
	return r.jit.regLearned[r.jitRegKeyOf(cl)]
}

// jitPortHas reports whether cl has native code in r.
func jitPortHas(r *Runtime, cl *closure) bool { return jitPortCode(r, cl) != nil }

// jitPortLink is the link native callers call cl's function's code
// through, nil for none: the stack form's entry's, or the register
// form's (regLinks).
func jitPortLink(r *Runtime, cl *closure) *jitLink {
	if r.jit == nil {
		return nil
	}
	return r.jit.regLinks[r.jitRegKeyOf(cl)]
}

// jitPortLinkIn is how many calls native callers made of cl's function's
// code through its link.
func jitPortLinkIn(r *Runtime, cl *closure) uint64 {
	if l := jitPortLink(r, cl); l != nil {
		return l.in
	}
	return 0
}

// jitPortRan is how often cl's code ran natively: the stretches Go
// entered it for and the calls native callers made of it through its
// link (jitNativeUsesForTest, by closure).
func jitPortRan(r *Runtime, cl *closure) uint64 {
	e := jitPortLife(r, cl)
	if e == nil {
		return 0
	}
	return e.ssaStats.entries + jitPortLinkIn(r, cl)
}

// jitPortNamed is the closure the global name holds in r.
func jitPortNamed(r *Runtime, name string) *closure {
	return r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
}

// jitPortCallPCs are the pcs of cl's method calls, by which its entry
// keeps what it learned of each (nativeCalls, callSites): the stack
// bytecode's call_method instructions, or the register code's call_mw
// ones, by register pc.
func jitPortCallPCs(r *Runtime, cl *closure) []int {
	var pcs []int
	for pc, w := range cl.reg.rc.Code {
		if w.Op() == bytecode.RCallMW {
			pcs = append(pcs, pc)
		}
	}
	return pcs
}

// jitPortDrop drops cl's function's register code in r as the cache drops
// code to make room (regLetGo), and reports whether it had code to drop.
func jitPortDrop(r *Runtime, cl *closure) bool {
	s := r.jit
	if s == nil {
		return false
	}
	key := r.jitRegKeyOf(cl)
	e := s.regCache[key]
	if e == nil || e.code == nil {
		return false
	}
	s.regLetGo(key, e)
	return true
}

// jitPortCodeLen is how many instructions the code cl runs has: its
// register code's, or its stack bytecode's.
func jitPortCodeLen(r *Runtime, cl *closure) int {
	return len(cl.reg.rc.Code)
}

// jitPortLinks are the links cl's code calls its native callees through:
// the stack form's entry's (ssaLinks), or what the register code keeps
// (jitFeedback's links); nil where it has no code.
func jitPortLinks(r *Runtime, cl *closure) []*jitLink {
	if e := jitPortRegEntry(r, cl); e != nil && e.code != nil && e.keep != nil {
		return e.keep.links
	}
	return nil
}

// jitPortInlined are the closures cl's code inlined: the stack form's
// entry's (ssaInlined), or what the register code keeps (jitFeedback's
// inlined); nil where it has no code.
func jitPortInlined(r *Runtime, cl *closure) []*closure {
	if e := jitPortRegEntry(r, cl); e != nil && e.code != nil && e.keep != nil {
		return e.keep.inlined
	}
	return nil
}

// jitPortInlinesNew reports whether cl's code inlines its construction
// of o, the receiver from a pool: the stack form's entry's inlines say so
// (the pool's with it); the register form's life's inlines name o where
// its builder inlined the construction (jitRegFeedback's inlineSite),
// and the call it learned, the pool's, is among its native calls
// (regCallSites' Pool).
func jitPortInlinesNew(r *Runtime, cl *closure, o *Object) bool {
	e := jitPortCode(r, cl)
	if e == nil {
		return false
	}

	return slices.ContainsFunc(e.inlines, func(x jitInline) bool { return x.obj == o }) &&
		slices.ContainsFunc(slices.Concat(e.nativeCalls, e.inlines), func(x jitInline) bool { return x.obj == o && x.pool != nil })
}

// jitPortKeeps is how many keep cells cl's code uses: the stack form's
// entry's (ssaKeeps), or the register code's (jitRegEntry's keeps).
func jitPortKeeps(r *Runtime, cl *closure) int {
	if e := jitPortRegEntry(r, cl); e != nil && e.code != nil {
		return e.keeps
	}
	return 0
}

// jitPortStringCells is how many cells cl's code reads string constants
// from: the stack form's entry's (ssaCells), or those among what the
// register code keeps (jitFeedback's cells, StringCell's) that hold a
// string.
func jitPortStringCells(r *Runtime, cl *closure) int {
	e := jitPortRegEntry(r, cl)
	if e == nil || e.code == nil || e.keep == nil {
		return 0
	}
	n := 0
	for _, c := range e.keep.cells {
		if c.IsString() {
			n++
		}
	}
	return n
}

// jitPortValid reports whether cl has native code that is valid: no
// guard of it has failed since it was compiled.
func jitPortValid(r *Runtime, cl *closure) bool {
	e := jitPortRegEntry(r, cl)
	return e != nil && e.code != nil && !e.invalid
}

// jitPortEntryPCs is where cl's code may be entered: its register code's
// pcs, each one whose lowering's first instruction the code has an entry
// at.
func jitPortEntryPCs(r *Runtime, cl *closure) []int {

	e := jitPortRegEntry(r, cl)
	var pcs []int
	for rpc := range cl.reg.rc.Code {
		if ipc := e.ipc(rpc); ipc >= 0 && e.code.HasEntry(ipc) && !slices.Contains(pcs, rpc) {
			pcs = append(pcs, rpc)
		}
	}
	return pcs
}

// jitPortLoopHeaders is cl's start and its loops' headers, in order: the
// headers of the loops its register code was compiled with entries at
// (jitRegEntry's loops), by register pc.
func jitPortLoopHeaders(r *Runtime, cl *closure) []int {

	pcs := []int{0}
	for _, pc := range jitPortRegEntry(r, cl).loops {
		if !slices.Contains(pcs, int(pc)) {
			pcs = append(pcs, int(pc))
		}
	}
	slices.Sort(pcs)
	return pcs
}

// jitPortCalleeLiterals are the literals cl's code makes natively for
// the callees it inlined: those of the stack form's entry's whose
// function is not cl's, made from pools of its own; or, the register
// form's, those of each inlined callee's state that outlives its
// compiles, whose pools its inlined literals are made from (the inlined
// level's feedback being the callee's own, jitRegCalleeOf).
func jitPortCalleeLiterals(r *Runtime, cl *closure) []jitLiteral {

	var lits []jitLiteral
	var seen []*closure
	for _, c := range jitPortInlined(r, cl) {
		if c.fn == cl.fn || slices.Contains(seen, c) {
			continue
		}
		seen = append(seen, c)
		if l := jitPortLife(r, c); l != nil {
			for _, x := range l.literals {
				if x.fn == c.fn {
					lits = append(lits, x)
				}
			}
		}
	}
	return lits
}

// jitPortLevels is how many levels of native calls under way the call
// of Go running now made frames for, or put off making them for: the
// stack form's (jitLevels' n), or the register form's (regNestLevels,
// regLazyLevels).
func jitPortLevels(r *Runtime) int {
	if r.jit == nil {
		return 0
	}
	return len(r.jit.regNestLevels) + len(r.jit.regLazyLevels)
}

// jitPortLevelsAt is how many levels the frames of the calls of Go from
// native callees reach: the stack form's (jitLevels' at), or what
// jitPortLevels counts of the register form's.
func jitPortLevelsAt(r *Runtime) int {
	if r.jit == nil {
		return 0
	}
	return jitPortLevels(r)
}
