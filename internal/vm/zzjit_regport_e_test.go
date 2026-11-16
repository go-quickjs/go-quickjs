//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"slices"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// jitCodeView is a function's native code as a test sees it: the entry its
// counts are kept in -- its register code's life (jitRegEntry's life),
// which outlives its compiles -- its code, and the cache's entry the code
// is held by.
type jitCodeView struct {
	e    *jitEntry
	code *jit.SSACode
	reg  *jitRegEntry
}

// jitViewForTest is the native code of cl in r, and false where it has
// none: the code the register cache holds for its function, with its
// life.
func jitViewForTest(r *Runtime, cl *closure) (jitCodeView, bool) {
	if r.jit == nil {
		return jitCodeView{}, false
	}
	e := r.jit.regCache[r.jitRegKeyOf(cl)]
	if e == nil || e.code == nil || e.life == nil {
		return jitCodeView{}, false
	}
	return jitCodeView{e: e.life, code: e.code, reg: e}, true
}

// jitViewNamedForTest is jitViewForTest of the global function name.
func jitViewNamedForTest(r *Runtime, name string) (jitCodeView, bool) {
	return jitViewForTest(r, jitGlobalClosureForTest(r, name))
}

// same reports whether cl's code is still v's: the register cache's code
// for it still v's code -- not compiled again.
func (v jitCodeView) same(r *Runtime, cl *closure) bool {
	e := r.jit.regCache[r.jitRegKeyOf(cl)]
	return v.reg != nil && e == v.reg && e.code == v.code
}

// link is the link native callers call cl's code through (regLinks), nil
// where it has none.
func (v jitCodeView) link(r *Runtime, cl *closure) *jitLink {
	if v.reg == nil {
		return nil
	}
	return r.jit.regLinks[r.jitRegKeyOf(cl)]
}

// jitLinkInForTest is how often native callers called cl's code through
// its link, 0 where it has no code or no link.
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

// jitCodeNamedForTest is the code of the global function name, the
// register cache's, failing t where it has none.
func jitCodeNamedForTest(t *testing.T, r *Runtime, name string) jitCodeView {
	t.Helper()
	cl := jitGlobalClosureForTest(r, name)
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

// jitViewByFnForTest is jitViewForTest: the register cache finds code by
// its function's key, a function only native callers call included.
func jitViewByFnForTest(r *Runtime, cl *closure) (jitCodeView, bool) {
	return jitViewForTest(r, cl)
}

// inlined are the closures of the callees v's code inlines, or that its
// compile asked to inline: those its register code's compile kept
// (jitRegEntry's keep).
func (v jitCodeView) inlined() []*closure {
	if v.reg == nil || v.reg.keep == nil {
		return nil
	}
	return v.reg.keep.inlined
}

// jitDerivedLinkForTest is the link native callers call the derived
// constructor name's code through (regLinks), nil where it has none.
func jitDerivedLinkForTest(t *testing.T, r *Runtime, name string) *jitLink {
	t.Helper()
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
// name has native code that was entered -- and its code.
func jitNativeCodeForTest(t *testing.T, r *Runtime, name string) jitCodeView {
	t.Helper()
	e := jitNativeForTest(t, r, name)
	cl := jitGlobalClosureForTest(r, name)
	v, ok := jitViewForTest(r, cl)
	if !ok || v.e != e {
		t.Fatalf("%s has no register code: %+v", name, r.jit.regCache[r.jitRegKeyOf(cl)])
	}
	return v
}

// jitNativeOrCalleeCodeForTest is the code of the global function name,
// whether Go enters it or only native callers call it, failing t where it
// has none.
func jitNativeOrCalleeCodeForTest(t *testing.T, r *Runtime, name string) jitCodeView {
	t.Helper()
	cl := jitGlobalClosureForTest(r, name)
	v, ok := jitViewForTest(r, cl)
	if !ok {
		t.Fatalf("%s has no code: %+v", name, r.jit.regCache[r.jitRegKeyOf(cl)])
	}
	return v
}

// jitViewOfFnNamedForTest is the code the cache holds for a function
// named name, any function of that name, global or not, and the link
// native callers call it through (nil where none): the register cache's.
func jitViewOfFnNamedForTest(r *Runtime, name string) (jitCodeView, *jitLink, bool) {
	for k, x := range r.jit.regCache {
		if fn := k.Value(); fn != nil && fn.Name == name && x.code != nil && x.life != nil {
			return jitCodeView{e: x.life, code: x.code, reg: x}, r.jit.regLinks[k], true
		}
	}
	return jitCodeView{}, nil, false
}

// jitCountedForTest reports whether the function named name (a global
// one) has native code, and how many of its runs the interpreter counted
// for it: its closure's counts of its register code's runs (jitRegCounts,
// which jitRegFeedback's Frequency reads).
func jitCountedForTest(r *Runtime, name string) (bool, uint64) {
	cl := jitGlobalClosureForTest(r, name)
	_, ok := jitViewForTest(r, cl)
	if len(cl.jitRegCounts) == 0 {
		return ok, 0
	}
	return ok, cl.jitRegCounts[0]
}

// invalid reports whether v's code was invalidated: the register cache's
// entry's (jitRegInvalidate).
func (v jitCodeView) invalid() bool {
	return v.reg != nil && v.reg.invalid
}

// jitEntryPCsOfForTest is where v's code may be entered, and its
// function's start and loops' headers, in order: its program's pcs
// (ir.Program's pcs), whose loops' headers are its backward branches'
// targets.
func jitEntryPCsOfForTest(v jitCodeView, cl *closure) (pcs, headers []int) {
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

// unresumed are the register pcs after calls v's code has no entry after
// (jitRegEntry's unresumed).
func (v jitCodeView) unresumed() []int {
	return v.reg.unresumed
}

// hasEntry reports whether v's code may be entered at pc, a register pc.
func (v jitCodeView) hasEntry(pc int) bool {
	ipc := v.reg.ipc(pc)
	return ipc >= 0 && v.code.HasEntry(ipc)
}

// called is how many closures v's code makes that it only calls, inlined
// and not made (ssa.Func's Called): its register code's compile's, which
// its keep holds.
func (v jitCodeView) called() int {
	if v.reg == nil || v.reg.keep == nil {
		return 0
	}
	return v.reg.keep.called
}

// inlinedAt are the register pcs of the calls of its own v's code
// inlines: those its register code's compiles inlined, which its life notes
// where their exits are counted (jitEntry's inlines), each once.
func (v jitCodeView) inlinedAt() []int {
	var pcs []int
	for _, x := range v.e.inlines {
		if !slices.Contains(pcs, int(x.pc)) {
			pcs = append(pcs, int(x.pc))
		}
	}
	return pcs
}

// jitRegInlinedForTest records the closures each function's last compile
// inlined (jitInlinedForTest).
func jitRegInlinedForTest(t *testing.T) map[string][]uintptr {
	t.Helper()
	return jitInlinedForTest(t)
}

// jitRegInlinesForTest reports whether caller, a global function of r
// running register code, inlined the global function callee in its last
// compile, as inlined (jitRegInlinedForTest) recorded it.
func jitRegInlinesForTest(t *testing.T, r *Runtime, inlined map[string][]uintptr, caller, callee string) bool {
	t.Helper()
	return inlined != nil && jitInlinedFnForTest(t, r, inlined, caller, callee)
}
