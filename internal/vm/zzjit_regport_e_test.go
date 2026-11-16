//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// jitCodeView is a function's native code as a test of either JIT's form
// sees it: the entry its counts are kept in -- the stack form's entry, or
// the register form's life (jitRegEntry's life), which outlives its
// compiles -- its code, and, under the register JIT, the cache's entry the
// code is held by.
type jitCodeView struct {
	e    *jitEntry
	code *jit.SSACode
	reg  *jitRegEntry
}

// jitRegClosureForTest reports whether cl runs register code in r, which
// r's JIT compiles (jitRegCodeForTest, of a closure).
func jitRegClosureForTest(r *Runtime, cl *closure) bool {
	return r.regOn() && cl.reg != nil
}

// jitViewForTest is the native code of cl in r, and false where it has
// none: its stack form's entry with SSA code (r.jit.hint), or, where cl
// runs register code, the code the register cache holds for its function,
// with its life.
func jitViewForTest(r *Runtime, cl *closure) (jitCodeView, bool) {
	if r.jit == nil {
		return jitCodeView{}, false
	}
	if jitRegClosureForTest(r, cl) {
		e := r.jit.regCache[r.jitRegKeyOf(cl)]
		if e == nil || e.code == nil || e.life == nil {
			return jitCodeView{}, false
		}
		return jitCodeView{e: e.life, code: e.code, reg: e}, true
	}
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil {
		return jitCodeView{}, false
	}
	return jitCodeView{e: e, code: e.ssa}, true
}

// jitViewNamedForTest is jitViewForTest of the global function name.
func jitViewNamedForTest(r *Runtime, name string) (jitCodeView, bool) {
	return jitViewForTest(r, jitGlobalClosureForTest(r, name))
}

// same reports whether cl's code is still v's: the stack form's entry
// still cl's (its hint), or the register cache's code for it still v's
// code -- not compiled again.
func (v jitCodeView) same(r *Runtime, cl *closure) bool {
	if v.reg != nil {
		e := r.jit.regCache[r.jitRegKeyOf(cl)]
		return e == v.reg && e.code == v.code
	}
	return r.jit.hint(cl) == v.e
}

// link is the link native callers call cl's code through, nil where it
// has none: the stack form's entry's, or the register form's
// (regLinks).
func (v jitCodeView) link(r *Runtime, cl *closure) *jitLink {
	if v.reg != nil {
		return r.jit.regLinks[r.jitRegKeyOf(cl)]
	}
	if v.e == nil {
		return nil
	}
	return v.e.link
}

// jitLinkInForTest is how often native callers called cl's code through
// its link, in either form, 0 where it has no code or no link.
func jitLinkInForTest(r *Runtime, cl *closure) uint64 {
	v, ok := jitViewForTest(r, cl)
	if !ok {
		return 0
	}
	if l := v.link(r, cl); l != nil {
		return l.in
	}
	return 0
}

// jitCodeNamedForTest is the code of the global function name, found by
// its function (one only native callers call has no hint), failing t
// where it has none: the stack form's entry (jitEntryForTest, which wants
// its link too), or the register cache's code for it.
func jitCodeNamedForTest(t *testing.T, r *Runtime, name string) jitCodeView {
	t.Helper()
	cl := jitGlobalClosureForTest(r, name)
	if !jitRegClosureForTest(r, cl) {
		e := jitEntryForTest(t, r, name)
		return jitCodeView{e: e, code: e.ssa}
	}
	v, ok := jitViewForTest(r, cl)
	if !ok {
		t.Fatalf("%s has no code: %+v", name, r.jit.regCache[r.jitRegKeyOf(cl)])
	}
	return v
}

// jitLinkedForTest is jitCodeNamedForTest's code and the link native
// callers call it through, failing t where it has no link.
func jitLinkedForTest(t *testing.T, r *Runtime, name string) (jitCodeView, *jitLink) {
	t.Helper()
	v := jitCodeNamedForTest(t, r, name)
	l := v.link(r, jitGlobalClosureForTest(r, name))
	if l == nil {
		t.Fatalf("%s has no link: %+v", name, v.e)
	}
	return v, l
}

// jitViewByFnForTest is jitViewForTest with the stack form's entry found
// by cl's function (entryFor), not its hint: the entry of a function only
// native callers call.
func jitViewByFnForTest(r *Runtime, cl *closure) (jitCodeView, bool) {
	if r.jit == nil || jitRegClosureForTest(r, cl) {
		return jitViewForTest(r, cl)
	}
	e := r.jit.entryFor(cl.fn)
	if e == nil || e.ssa == nil {
		return jitCodeView{e: e}, false
	}
	return jitCodeView{e: e, code: e.ssa}, true
}

// inlined are the closures of the callees v's code inlines, or that its
// compile asked to inline: the stack form's entry's (ssaInlined), or those
// its register code's compile kept (jitRegEntry's keep), alike.
func (v jitCodeView) inlined() []*closure {
	if v.reg != nil {
		if v.reg.keep == nil {
			return nil
		}
		return v.reg.keep.inlined
	}
	if v.e == nil {
		return nil
	}
	return v.e.ssaInlined
}

// jitDerivedLinkForTest is the link native callers call the derived
// constructor name's code through, nil where it has none, in either form:
// the stack form's entry's (jitDerivedForTest), or the register form's.
func jitDerivedLinkForTest(t *testing.T, r *Runtime, name string) *jitLink {
	t.Helper()
	if !regJITForTest() {
		return jitDerivedForTest(t, r, name).link
	}
	for k, x := range r.jit.regCache {
		if fn := k.Value(); fn != nil && fn.Name == name && fn.Kind == bytecode.KindDerivedConstructor && x.code != nil {
			return r.jit.regLinks[k]
		}
	}
	t.Fatalf("%s has no register code", name)
	return nil
}

// jitInlinedNamesForTest names the callees the global function name's
// code inlines or its compile asked to inline (jitCodeView's inlined), for
// a failure's message.
func jitInlinedNamesForTest(r *Runtime, name string) []string {
	v, _ := jitViewByFnForTest(r, jitGlobalClosureForTest(r, name))
	var names []string
	for _, cl := range v.inlined() {
		if cl != nil && cl.fn != nil {
			names = append(names, cl.fn.Name)
		}
	}
	return names
}

// jitNativeCodeForTest is jitNativeForTest's check -- the global function
// name has native code that was entered -- and its code, in either form.
func jitNativeCodeForTest(t *testing.T, r *Runtime, name string) jitCodeView {
	t.Helper()
	e := jitNativeForTest(t, r, name)
	cl := jitGlobalClosureForTest(r, name)
	if !jitRegClosureForTest(r, cl) {
		return jitCodeView{e: e, code: e.ssa}
	}
	v, ok := jitViewForTest(r, cl)
	if !ok || v.e != e {
		t.Fatalf("%s has no register code: %+v", name, r.jit.regCache[r.jitRegKeyOf(cl)])
	}
	return v
}

// jitNativeOrCalleeCodeForTest is jitNativeOrCalleeForTest's code in
// either form: the global function name's, whether Go enters it or only
// native callers call it, failing t where it has none.
func jitNativeOrCalleeCodeForTest(t *testing.T, r *Runtime, name string) jitCodeView {
	t.Helper()
	cl := jitGlobalClosureForTest(r, name)
	if !jitRegClosureForTest(r, cl) {
		e := jitNativeOrCalleeForTest(t, r, name)
		return jitCodeView{e: e, code: e.ssa}
	}
	v, ok := jitViewForTest(r, cl)
	if !ok {
		t.Fatalf("%s has no code: %+v", name, r.jit.regCache[r.jitRegKeyOf(cl)])
	}
	return v
}

// jitViewOfFnNamedForTest is the code the cache holds for a function
// named name, any function of that name, global or not, and the link
// native callers call it through (nil where none): the stack form's
// entry's, or, under the register JIT, the register cache's.
func jitViewOfFnNamedForTest(r *Runtime, name string) (jitCodeView, *jitLink, bool) {
	if regJITForTest() {
		for k, x := range r.jit.regCache {
			if fn := k.Value(); fn != nil && fn.Name == name && x.code != nil && x.life != nil {
				return jitCodeView{e: x.life, code: x.code, reg: x}, r.jit.regLinks[k], true
			}
		}
		return jitCodeView{}, nil, false
	}
	var e *jitEntry
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && fn.Name == name {
			e = x
		}
	}
	if e == nil || e.ssa == nil {
		return jitCodeView{e: e}, nil, false
	}
	return jitCodeView{e: e, code: e.ssa}, e.link, true
}

// jitCountedForTest reports whether the function named name (a global
// one, under the register JIT) has native code, and how many of its runs
// the existing tiers counted for it: its stack form's entry's counts
// (jitCallbackEntry's calls), or its closure's counts of its register
// code's runs (jitRegCounts, which jitRegFeedback's Frequency reads).
func jitCountedForTest(r *Runtime, name string) (bool, uint64) {
	if !regJITForTest() {
		e := jitCallbackEntry(r, name)
		if e == nil || e.calls == nil {
			return e != nil && e.ssa != nil, 0
		}
		return e.ssa != nil, uint64(e.calls.runs)
	}
	cl := jitGlobalClosureForTest(r, name)
	_, ok := jitViewForTest(r, cl)
	if len(cl.jitRegCounts) == 0 {
		return ok, 0
	}
	return ok, cl.jitRegCounts[0]
}

// invalid reports whether v's code was invalidated: the stack form's
// entry's, or the register cache's entry's (jitRegInvalidate).
func (v jitCodeView) invalid() bool {
	if v.reg != nil {
		return v.reg.invalid
	}
	return v.e != nil && v.e.invalid
}

// jitEntryPCsOfForTest is where v's code may be entered, and its
// function's start and loops' headers, in order: the stack bytecode's pcs
// (jitEntryPCs, jitLoopHeadersOf), or, for register code, its program's
// (ir.Program's pcs), whose loops' headers are its backward branches'
// targets.
func jitEntryPCsOfForTest(v jitCodeView, cl *closure) (pcs, headers []int) {
	if v.reg == nil {
		return jitEntryPCs(v.e, cl.fn), jitLoopHeadersOf(cl.fn)
	}
	p := v.reg.p
	headers = []int{0}
	for pc, in := range p.Code {
		if v.code.HasEntry(pc) {
			pcs = append(pcs, pc)
		}
		if (in.Op == ir.Jump || in.Op == ir.Branch) && in.Target <= pc && !slices.Contains(headers, in.Target) {
			headers = append(headers, in.Target)
		}
	}
	slices.Sort(headers)
	return pcs, headers
}

// unresumed are the pcs after calls v's code has no entry after: the
// stack form's entry's (ssaUnresumed), or its register code's, by
// register pc (jitRegEntry's unresumed).
func (v jitCodeView) unresumed() []int {
	if v.reg != nil {
		return v.reg.unresumed
	}
	var pcs []int
	for _, pc := range v.e.ssaUnresumed {
		pcs = append(pcs, int(pc))
	}
	return pcs
}

// hasEntry reports whether v's code may be entered at pc: a stack
// bytecode pc, or, for register code, a register pc.
func (v jitCodeView) hasEntry(pc int) bool {
	if v.reg != nil {
		ipc := v.reg.ipc(pc)
		return ipc >= 0 && v.code.HasEntry(ipc)
	}
	return v.code.HasEntry(pc)
}

// called is how many closures v's code makes that it only calls, inlined
// and not made (ssa.Func's Called): the stack form's entry's
// (ssaCalled), or its register code's compile's, which its keep holds.
func (v jitCodeView) called() int {
	if v.reg != nil {
		if v.reg.keep == nil {
			return 0
		}
		return v.reg.keep.called
	}
	return v.e.ssaCalled
}

// inlinedAt are the pcs of the calls of its own v's code inlines: the
// stack form's entry's (ssaInlinedAt), or, by register pc, those its
// register code's compiles inlined, which its life notes where their exits
// are counted (jitEntry's inlines), each once.
func (v jitCodeView) inlinedAt() []int {
	var pcs []int
	if v.reg != nil {
		for _, x := range v.e.inlines {
			if !slices.Contains(pcs, int(x.pc)) {
				pcs = append(pcs, int(x.pc))
			}
		}
		return pcs
	}
	for _, pc := range v.e.ssaInlinedAt {
		pcs = append(pcs, int(pc))
	}
	return pcs
}

// jitRegInlinedForTest records, where runtimes run register code, the
// closures each function's last compile inlined (jitInlinedForTest), and
// is nil otherwise: the stack form's compiles are left as they were.
func jitRegInlinedForTest(t *testing.T) map[string][]uintptr {
	t.Helper()
	if !regJITForTest() {
		return nil
	}
	return jitInlinedForTest(t)
}

// jitRegInlinesForTest reports whether caller, a global function of r
// running register code, inlined the global function callee in its last
// compile, as inlined (jitRegInlinedForTest) recorded it.
func jitRegInlinesForTest(t *testing.T, r *Runtime, inlined map[string][]uintptr, caller, callee string) bool {
	t.Helper()
	return inlined != nil && jitRegCodeForTest(r, caller) && jitInlinedFnForTest(t, r, inlined, caller, callee)
}
