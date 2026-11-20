//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"sync"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// jitRegEntryOfForTest is the register cache's entry of cl's code, nil
// where the cache holds no code compiled from it.
func jitRegEntryOfForTest(r *Runtime, cl *closure) *jitRegEntry {
	if r.jit == nil || cl == nil || cl.closureCode == nil {
		return nil
	}
	e := r.jit.regCache[r.jitRegKeyOf(cl)]
	if e == nil || e.code == nil {
		return nil
	}
	return e
}

// jitRegEntryForTest is jitRegEntryOfForTest of the global function name.
func jitRegEntryForTest(r *Runtime, name string) *jitRegEntry {
	return jitRegEntryOfForTest(r, jitGlobalClosureForTest(r, name))
}

// jitRegEntryNamedForTest is the register cache's entry with code of a
// function named name, any closure's: for functions no global names.
func jitRegEntryNamedForTest(r *Runtime, name string) *jitRegEntry {
	if r.jit == nil {
		return nil
	}
	for k, e := range r.jit.regCache {
		if fn := k.Value(); fn != nil && fn.Name == name && e.code != nil {
			return e
		}
	}
	return nil
}

// jitCodeEntryNamedForTest is the entry of a function named name with code,
// any closure's: its register code's state that outlives its compiles
// (jitRegEntry's life). nil where it has none.
func jitCodeEntryNamedForTest(r *Runtime, name string) *jitEntry {
	if e := jitRegEntryNamedForTest(r, name); e != nil {
		return e.life
	}
	return nil
}

// jitInlinedOfForTest are the closures of the callees cl's code inlines,
// or that its compile asked to inline: its stack form's entry's
// ssaInlined, its register code's feedback's inlined (jitRegPlace's keep).
func jitInlinedOfForTest(r *Runtime, cl *closure) []*closure {
	if e := jitRegEntryOfForTest(r, cl); e != nil && e.keep != nil {
		return e.keep.inlined
	}
	return nil
}

// jitCalledForTest is what the code of a function named name, any
// closure's, does with the closures it makes: how many it only calls and
// does not make (ssa.Func's Called), how many loops' callbacks it inlines
// and, of those, does not make (Callbacks, Unmade); ok false where it has
// no code. Its stack form's entry keeps them, its register code's feedback
// (jitRegPlace's keep).
func jitCalledForTest(r *Runtime, name string) (called, callbacks, unmade int, ok bool) {
	e := jitRegEntryNamedForTest(r, name)
	if e == nil || e.keep == nil {
		return 0, 0, 0, false
	}
	return e.keep.called, e.keep.callbacks, e.keep.unmade, true
}

// jitBuiltFacts is what a function's last compile's SSA said (ssa.Func):
// the calls of its own it inlines (InlinedAt), the callees it inlines in
// all (Inlined), the objects it does not make (Virtuals), and whether it
// makes loops of built-ins' calls (Loops).
type jitBuiltFacts struct {
	inlinedAt, inlined, virtuals int
	loops                        bool
}

// jitBuiltForTest records each compile's SSA facts by its function, under
// either form (jitSSAOptimized, which the register JIT's compiles call
// too), for the facts the register form keeps no counterpart of in its
// entry: the stack form's entry's ssaInlinedAt, ssaVirtuals and loops.
type jitBuiltForTest struct {
	mu sync.Mutex
	by map[*bytecode.Function]jitBuiltFacts
}

// jitRecordBuiltForTest records every compile's facts while the test runs
// (jitBuiltForTest), passing each compile on to a hook set before. A test
// that uses it does not run in parallel with others that set the hook.
func jitRecordBuiltForTest(t *testing.T) *jitBuiltForTest {
	t.Helper()
	b := &jitBuiltForTest{by: map[*bytecode.Function]jitBuiltFacts{}}
	prev := jitTestConfigOf(t).ssaOptimized
	jitTestSwitch(t, func(jc *jitTestConfig) {
		jc.ssaOptimized = func(fn *bytecode.Function, f *ssa.Func) {
			b.mu.Lock()
			b.by[fn] = jitBuiltFacts{inlinedAt: len(f.InlinedAt), inlined: len(f.Inlined), virtuals: f.Virtuals, loops: f.Loops}
			b.mu.Unlock()
			if prev != nil {
				prev(fn, f)
			}
		}
	})
	return b
}

// of is the facts of fn's last compile, and whether it had one.
func (b *jitBuiltForTest) of(fn *bytecode.Function) (jitBuiltFacts, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	x, ok := b.by[fn]
	return x, ok
}

// fnForTest is the function of e's code.
func (e *jitRegEntry) fnForTest() *bytecode.Function {
	return e.key.Value()
}

// jitRegLowerKindForTest is the lowering e's code was compiled from
// (jitRegLower's kinds), and whether the function's lowering still holds
// it to tell.
func jitRegLowerKindForTest(r *Runtime, e *jitRegEntry) (jitLowerKind, bool) {
	l := r.jit.regLowered[e.key]
	if l == nil || e.p == nil {
		return 0, false
	}
	for k := range jitLowerKinds {
		if l.done[k] && l.progs[k] == e.p {
			return jitLowerKind(k), true
		}
	}
	return 0, false
}

// jitCompiledHotForTest reports whether the global function name's code was
// compiled once its calls had made it hot (LowerSSAHot, LowerRegHot): a
// function of host operations alone, its stack form's entry marked hot,
// or its register code compiled from its lowering for that (jitLowerHot).
func jitCompiledHotForTest(t *testing.T, r *Runtime, name string) bool {
	t.Helper()
	e := jitRegEntryForTest(r, name)
	if e == nil {
		return false
	}
	k, ok := jitRegLowerKindForTest(r, e)
	if !ok {
		t.Logf("%s: the lowering its code was compiled from is gone", name)
	}
	return ok && k == jitLowerHot
}

// jitWorkForTest is the entry whose ssaStats count the work of the global
// function name's code (jitRegStretchWork) -- its register code's state
// that outlives its compiles -- and the mean length of its loops a back
// edge is counted as (the register code's loopMean); nil where it has no code.
func jitWorkForTest(r *Runtime, name string) (*jitEntry, uint64) {
	e := jitRegEntryForTest(r, name)
	if e == nil || e.life == nil {
		return nil, 0
	}
	return e.life, uint64(e.loopMean)
}

// jitNamedLinkForTest is the global function name's code's state and the link
// its native callers call it through, failing the test where either is
// missing (jitRegEntryForTest): its register code's state that outlives
// its compiles and the register link (jitState's regLinks). callee says
// the code is for native callers alone, compiled from the lowering for
// them (the register code's callee).
func jitNamedLinkForTest(t *testing.T, r *Runtime, name string) (e *jitEntry, link *jitLink, callee bool) {
	t.Helper()
	return jitLinkedOfForTest(t, r, jitGlobalClosureForTest(r, name), name)
}

// jitLinkedOfForTest is jitNamedLinkForTest of closure cl, which name names.
func jitLinkedOfForTest(t *testing.T, r *Runtime, cl *closure, name string) (e *jitEntry, link *jitLink, callee bool) {
	t.Helper()
	re := jitRegEntryOfForTest(r, cl)
	if re == nil || re.life == nil {
		t.Fatalf("%s has no code: %+v", name, re)
	}
	link = r.jit.regLinks[re.key]
	if link == nil {
		t.Fatalf("%s has no link: %+v", name, re)
	}
	return re.life, link, re.callee
}

// jitInlinedAtForTest is, of the code of a function named name, any
// closure's, how many of its own calls it inlines (the stack form's
// entry's ssaInlinedAt; ssa.Func's InlinedAt of its register code's last
// compile, which built recorded) and how many of the closures it makes it
// only calls (jitCalledForTest); ok false where it has no code.
func jitInlinedAtForTest(r *Runtime, built *jitBuiltForTest, name string) (inlinedAt, called int, ok bool) {
	e := jitRegEntryNamedForTest(r, name)
	if e == nil {
		return 0, 0, false
	}
	x, ok := built.of(e.fnForTest())
	return x.inlinedAt, e.keep.called, ok
}

// jitBuiltIfRegForTest records the compiles' SSA facts where runtimes run
// register code (jitRecordBuiltForTest), whose entries keep no count of
// the calls their code inlines, and has the test run in parallel
// otherwise: the hook that tells them is the package's, which a test in
// parallel would share.
func jitBuiltIfRegForTest(t *testing.T) *jitBuiltForTest {
	t.Helper()
	return jitRecordBuiltForTest(t)
}

// jitLearnedForTest is what the global function name's code learned, nil
// where it learned nothing: its stack form's entry, code or none, or its
// register code's state that outlives its compiles (jitState's
// regLearned), code or none.
func jitLearnedForTest(r *Runtime, name string) *jitEntry {
	if r.jit == nil {
		return nil
	}
	cl := jitGlobalClosureForTest(r, name)
	return r.jit.regLearned[r.jitRegKeyOf(cl)]
}

// jitStatsOfForTest is the state whose ssaStats count what cl's code did
// -- its stack form's entry, its register code's state that outlives its
// compiles -- and the identity of the cache's entry of its code, which is
// the same while the cache keeps it: the stack form's entry, the register
// cache's. nil, nil where cl has no code.
func jitStatsOfForTest(r *Runtime, cl *closure) (*jitEntry, any) {
	e := jitRegEntryOfForTest(r, cl)
	if e == nil || e.life == nil {
		return nil, nil
	}
	return e.life, e
}

// jitEntryLinkOfForTest is what cl's code learned and the link native callers
// call it through, nil where there is none: its stack form's entry and
// that entry's link, or its register code's state that outlives its
// compiles and the register link (jitState's regLinks), code or none.
func jitEntryLinkOfForTest(r *Runtime, cl *closure) (*jitEntry, *jitLink) {
	if r.jit == nil {
		return nil, nil
	}
	key := r.jitRegKeyOf(cl)
	return r.jit.regLearned[key], r.jit.regLinks[key]
}

// jitCodeOfForTest is cl's code's state, nil where cl has no code: its
// stack form's entry with code, whose link and invalid it gives, or its
// register code's state that outlives its compiles, with the register link
// (jitState's regLinks) and the register code's invalid.
func jitCodeOfForTest(r *Runtime, cl *closure) (e *jitEntry, link *jitLink, invalid bool) {
	re := jitRegEntryOfForTest(r, cl)
	if re == nil || re.life == nil {
		return nil, nil, false
	}
	return re.life, r.jit.regLinks[re.key], re.invalid
}

// jitNamedStateForTest is a function's state in r's JIT, found by its name
// (jitNamedOfForTest).
type jitNamedStateForTest struct {
	// e is what its code learned and what its runs did: the stack form's
	// entry, the register code's state that outlives its compiles; nil
	// for none.
	e *jitEntry
	// link is the link native callers call it through, nil for none.
	link *jitLink
	// code says it has native code; loops that the code makes loops of
	// built-ins' calls (the stack form's entry's loops; ssa.Func's Loops
	// of the register code's last compile, which built recorded); inlined
	// the closures of the callees it inlines or asked to inline
	// (jitInlinedOfForTest).
	code    bool
	loops   bool
	inlined []*closure
}

// jitNamedOfForTest is the state of a function named name, any closure's,
// in r's JIT, code or none: the stack form's entry in the cache, or the
// register form's entry, link and state that outlives its compiles, by
// its function's key. built records the register compiles' facts
// (jitRecordBuiltForTest); nil for the stack form.
func jitNamedOfForTest(r *Runtime, built *jitBuiltForTest, name string) jitNamedStateForTest {
	if r.jit == nil {
		return jitNamedStateForTest{}
	}

	var key jitKey
	found := false
	named := func(k jitKey) bool {
		if fn := k.Value(); !found && fn != nil && fn.Name == name {
			key, found = k, true
		}
		return found
	}
	for k := range r.jit.regCache {
		if named(k) {
			break
		}
	}
	for k := range r.jit.regLinks {
		if named(k) {
			break
		}
	}
	for k := range r.jit.regLearned {
		if named(k) {
			break
		}
	}
	if !found {
		return jitNamedStateForTest{}
	}
	n := jitNamedStateForTest{e: r.jit.regLearned[key], link: r.jit.regLinks[key]}
	if re := r.jit.regCache[key]; re != nil && re.code != nil {
		n.code = true
		if re.keep != nil {
			n.inlined = re.keep.inlined
		}
		if built != nil {
			x, _ := built.of(key.Value())
			n.loops = x.loops
		}
	}
	return n
}

// jitRegMethodSiteForTest is the cache of the first method read of fn's
// register code, get_method's or get_method_w's (its C).
func jitRegMethodSiteForTest(t *testing.T, fn *bytecode.Function) uint32 {
	t.Helper()
	for _, w := range fn.RegCode().Code {
		if op := w.Op(); op == bytecode.RGetMethod || op == bytecode.RGetMethodW {
			return uint32(w.C())
		}
	}
	t.Fatalf("no method read in %s's register code", fn.Name)
	return 0
}

// jitRunnerOfForTest is the closure whose code runs cl's body when one of
// callers calls it: cl's own code, or, where cl runs register code and a
// caller's register code inlines it, that caller's. A function of calls
// and no loop that calls one with a loop is compiled once hot
// (LowerRegHot) with its callee inlined, loop and all, where the stack
// form's native call runs the callee's own code: what the callee's code
// does -- its work, its exits, its calls of Go -- that caller's does.
func jitRunnerOfForTest(r *Runtime, cl *closure, callers ...*closure) *closure {

	for _, c := range callers {
		if slices.Contains(jitInlinedOfForTest(r, c), cl) {
			return c
		}
	}
	return cl
}

// jitRunnerForTest is jitRunnerOfForTest of global functions, by name.
func jitRunnerForTest(r *Runtime, name string, callers ...string) string {
	cl := jitGlobalClosureForTest(r, name)
	for _, c := range callers {
		if jitRunnerOfForTest(r, cl, jitGlobalClosureForTest(r, c)) != cl {
			return c
		}
	}
	return name
}
