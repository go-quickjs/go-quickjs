//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitRunFirstForTest runs the script fn in r, as Run does, its functions
// compiled at their first call where r runs register code: the stack
// form's JIT compiles a function at its first call (jitCallThreshold 1,
// as jitRuntimeForTest sets it), before its sites have run, where the
// register form's first call runs in the interpreter and begins its
// budget, its next call compiling it (jitRegCallTier). Each template the
// script prepares -- its functions', and theirs within them -- has its
// first call sent to Go's tier-up (closureCode's jitEnters), which, its
// budget one invocation, compiles it there: so that what a test asks of
// native code is asked of it at the first call.
func jitRunFirstForTest(r *Runtime, fn *bytecode.Function) (Value, error) {
	if !r.jitEnabled {
		return r.Run(fn)
	}
	_, v, err := jitRunScriptForTest(r, fn, true)
	return v, err
}

// jitRunScriptForTest runs the script fn in r as Run does, and returns
// the closure it prepared for it, whose constants hold the templates of
// the functions it declares and makes (jitEnterFirstForTest). first has
// them compiled at their first call (jitRunFirstForTest).
func jitRunScriptForTest(r *Runtime, fn *bytecode.Function, first bool) (*closure, Value, error) {
	cl := prepareRun[struct{}](r, fn)
	if first {
		jitEnterFirstForTest(cl)
	}
	this := r.globalThis
	if fn.IsModule {
		this = Undefined
	}
	v, err := r.run(cl, this, nil, Undefined, nil)
	r.endTurn()
	return cl, v, err
}

// jitEnterFirstForTest has the next call of each template within cl's
// constants, and within theirs, go to Go's tier-up (jitRunFirstForTest),
// where r runs register code.
func jitEnterFirstForTest(cl *closure) {
	for _, c := range cl.consts {
		if tmpl := c.template(); tmpl != nil && tmpl.closureCode != nil {
			tmpl.closureCode.jitEnters = true
			jitEnterFirstForTest(tmpl)
		}
	}
}

// jitNativeCountsForTest has register frames run by the native handlers
// for the rest of t, as they do but under QJS_NONATIVE and in a verifying
// build (quickjs_verify), which run the Go dispatcher: the native handlers
// count the calls the JIT inlines by frequency (jitRegCounts), the Go
// dispatcher none. Not for a parallel test: regNativeOn is the process's.
func jitNativeCountsForTest(t *testing.T) {
	was := regNativeOn
	regNativeOn = true
	t.Cleanup(func() { regNativeOn = was })
}

// jitRunFirstSourceForTest is jitRunFirstForTest of source compiled.
func jitRunFirstSourceForTest(t *testing.T, r *Runtime, source string) (Value, error) {
	t.Helper()
	return jitRunFirstForTest(r, compileForTest(t, source))
}

// jitWarmForTest runs source in r, a runtime with the JIT, with its JIT
// off, as tests that fill their sites' caches before the JIT compiles do,
// and returns a function that turns the JIT on again. The functions
// source made, whose calls with the JIT off had their budgets stop
// counting (jitRegCallTier, regEdgesOff), have their next call go to Go's
// tier-up, which compiles them there (jitEnterFirstForTest).
func jitWarmForTest(t *testing.T, r *Runtime, source string) func() {
	t.Helper()
	r.jitEnabled = false
	cl, _, err := jitRunScriptForTest(r, compileForTest(t, source), false)
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		r.jitEnabled = true
		jitEnterFirstForTest(cl)
	}
}

// jitClosureCodeForTest is the state of cl's native code, nil where it
// has none: its register code's that outlives its compiles (life), whose
// counts and judgments -- entrySlow among them -- its runs keep.
func jitClosureCodeForTest(r *Runtime, cl *closure) *jitEntry {
	if e := jitClosureRegForTest(r, cl); e != nil {
		return e.life
	}
	return nil
}

// jitClosureRegForTest is the cache's register code of cl's function
// with code, nil where it has none or cl does not run register code.
func jitClosureRegForTest(r *Runtime, cl *closure) *jitRegEntry {
	if r.jit == nil || cl.reg == nil || cl.closureCode == nil {
		return nil
	}
	if e := r.jit.regCache[r.jitRegKeyOf(cl)]; e != nil && e.code != nil {
		return e
	}
	return nil
}

// jitClosureSSAForTest is cl's native code, nil where it has none.
func jitClosureSSAForTest(r *Runtime, cl *closure) *jit.SSACode {
	if e := jitClosureRegForTest(r, cl); e != nil {
		return e.code
	}
	return nil
}

// jitClosureRefusedForTest reports whether the JIT refused cl's
// function's register code for good.
func jitClosureRefusedForTest(r *Runtime, cl *closure) bool {
	return cl.closureCode != nil && cl.closureCode.jitRegRefused
}

// jitCodeBytesForTest is the size of the native code r holds: the
// register code in its cache.
func jitCodeBytesForTest(r *Runtime) int64 {
	var n int64
	if r.jit != nil {
		for _, e := range r.jit.regCache {
			if e.code != nil {
				n += int64(e.code.Size())
			}
		}
	}
	return n
}

// jitLinkCallsForTest is how many calls native callers made of cl's
// function's code through its link (regLinks).
func jitLinkCallsForTest(r *Runtime, cl *closure) uint64 {
	if r.jit == nil {
		return 0
	}
	if k := r.jit.regLinks[r.jitRegKeyOf(cl)]; k != nil {
		return k.in
	}
	return 0
}

// jitBuildForTest is what a function's last compile built (jitSSAOptimized):
// the closures it inlined (ssa.Func's Inlined), and whether it calls
// Array.prototype.push and pop natively (ssa.CallSite's Push and Pop),
// which register code's state does not keep as the stack form's entry
// does (ssaInlined, nativeCalls' push and pop).
type jitBuildForTest struct {
	inlined   []uintptr
	push, pop bool
}

// jitBuildsForTest has the test record each function's last compile's
// build, by its function, in the map it returns. It sets the process's
// SSA hook: a test that calls it is not parallel.
func jitBuildsForTest(t *testing.T) map[*bytecode.Function]jitBuildForTest {
	t.Helper()
	builds := map[*bytecode.Function]jitBuildForTest{}
	jitSSAOptimized = func(fn *bytecode.Function, f *ssa.Func) {
		b := jitBuildForTest{inlined: slices.Clone(f.Inlined)}
		for _, blk := range f.Blocks {
			for _, v := range blk.Values {
				for _, c := range v.Calls() {
					b.push = b.push || c.Push
					b.pop = b.pop || c.Pop
				}
			}
		}
		builds[fn] = b
	}
	t.Cleanup(func() { jitSSAOptimized = nil })
	return builds
}

// jitRegHeldCountsEachKindForTest is TestJITHeldCountsEachKind's check of
// what r's JIT holds where its code is the register form's: each kind it
// holds counted -- the code in its cache, with its on-stack replacement's
// (regHeld), and, as the stack form's entry counts them in its measure,
// what each function learned (learnedBytes), the pools of its literals'
// and constructions' objects among it; the lowerings, the stack form's
// and the register form's (lowerBytes); the contexts and the links native
// callers call through, the register form's (regLinks) among them
// (sharedBytes) -- and no more than the budget held.
func jitRegHeldCountsEachKindForTest(t *testing.T, r *Runtime) {
	t.Helper()
	s := r.jit
	code, learned, pools := 0, 0, 0
	for key, e := range s.regCache {
		if e.code == nil {
			continue
		}
		if e.bytes < e.code.Footprint() {
			t.Fatalf("an entry's code: %d bytes held, %d counted", e.code.Footprint(), e.bytes)
		}
		code += e.bytes
		if e.osr != nil {
			code += e.osr.bytes
		}
		l := s.regLearned[key]
		if l == nil {
			continue
		}
		learned += l.learnedBytes()
		// Each pool, with its objects: the literals' and the
		// constructions'.
		held := 0
		for _, x := range l.literals {
			if x.pool.Count > 0 {
				pools++
				held += int(unsafe.Sizeof(*x.pool)) + int(x.pool.Size)*int(objectSize)
			}
		}
		for _, x := range append(slices.Clone(l.inlines), l.nativeCalls...) {
			if x.pool != nil && x.pool.Count > 0 {
				pools++
				held += int(unsafe.Sizeof(*x.pool)) + int(x.pool.Size)*int(objectSize)
			}
		}
		if l.learnedBytes() < held {
			t.Fatalf("pools of %d bytes counted at %d", held, l.learnedBytes())
		}
	}
	if code+learned > s.regHeld || pools == 0 {
		t.Errorf("register code holds %d bytes of code and %d learned, counted %d; %d pools", code, learned, s.regHeld, pools)
	}
	lowered := 0
	lowering := func(l *jitLowering) {
		n := jitLoweringFixed + jitCapBytes(l.hints.StringAdds) + jitCapBytes(l.hints.ElementReads)
		for _, p := range l.progs {
			if p != nil {
				n += p.Footprint()
				if p.Footprint() < len(p.Code)*int(unsafe.Sizeof(ir.Instruction{})) {
					t.Fatal("a program's instructions uncounted")
				}
			}
		}
		if l.bytes != n {
			t.Fatalf("a lowering holds %d, counted %d", n, l.bytes)
		}
		lowered += n
	}
	for _, l := range s.lowered {
		lowering(l)
	}
	for _, l := range s.regLowered {
		lowering(l)
	}
	if lowered != s.lowerBytes || lowered == 0 {
		t.Fatalf("lowerings hold %d, counted %d", lowered, s.lowerBytes)
	}
	if len(s.ssaCtxs) == 0 || len(s.regLinks) == 0 {
		t.Fatalf("%d contexts, %d links", len(s.ssaCtxs), len(s.regLinks))
	}
	shared := s.ctxCounted()*jitContextBytes + len(s.regLinks)*jitLinkFixed + jitCapBytes(s.hints)
	if s.sharedBytes() < shared || s.held() != s.lowerBytes+s.sharedBytes()+s.regHeld {
		t.Errorf("shared %d counted, at least %d held", s.sharedBytes(), shared)
	}
	if r.jitHeldBytes() > int64(r.jitBudget()) {
		t.Fatalf("%d bytes held, past the budget's %d", r.jitHeldBytes(), r.jitBudget())
	}
}

// jitFunctionCodeForTest is jitClosureCodeForTest by cl's function: its
// register code's state, whatever closure of it cl is.
func jitFunctionCodeForTest(r *Runtime, cl *closure) *jitEntry {
	return jitClosureCodeForTest(r, cl)
}
