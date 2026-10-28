//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Call frequency, as V8's feedback has it: how often a call site calls for
// each invocation of its function, which V8's inliner orders its candidates
// by and cuts them at (ssa.FrequencyFeedback). V8's lower tiers count it,
// not its optimized code: each call bytecode's feedback slot counts the
// calls made there, and the function's feedback vector its invocations,
// from when the vector is allocated. Here the existing tiers count it
// where every call they make with a frame passes once the JIT is on: the
// call path's hook, tryJITFrame, which the interpreter's calls, the tree
// tier's call nodes and constructions, and the calls of built-ins and
// accessors all reach once the callee's frame is made. The callee's frame
// is the top one; its caller's, the one below, is at the call, its pc past
// it, as a stack trace reads it (jitCountCall). Native code counts
// nothing: its calls through Go, at its exits, and of callees with no code
// are left out (jitState's goCallDepth, nativeCallDepth), and so are its
// invocations.
//
// They are counted while the function's code is to be compiled: while
// its entry warms, before it is compiled, and while its code is
// invalidated, before it is compiled again (jitCounting) -- the existing
// tiers' runs a compile is made from. A function Go does not enter the
// code of, or that has none for good, runs in them without counting.

// jitCallCounts is what the existing tiers counted of a function's calls
// while its code was to be compiled (jitCounting), from its first entry
// on, as V8's feedback vector counts them from when it is allocated: runs,
// the invocations they ran of it, and, for each of its calls and
// constructions -- pcs, their PCs in order, a call in tail position among
// them -- calls, the calls made there of a function in a frame of its own.
// code is how many instructions the function has, which a ghost's counts
// are checked against (jitGhostCounts).
//
// A call the call path answers without a frame -- a leaf's
// (bytecode.LeafKind) but a pure body's (jitCountPure), a built-in's -- is
// not counted, nor a call made through Function.prototype.call, whose
// frame is the built-in's: such a site's frequency is not known
// (jitCallFrequency). A call in tail
// position deep in the stack gives its frame up (tailCallDepth), and its
// callee's frame is counted at its caller's caller's call.
type jitCallCounts struct {
	runs  uint32
	code  int
	pcs   []int32
	calls []uint32
	// learned marks the calls native code made that the existing tiers
	// had not (jitCountLearned), by index, or is nil for none.
	learned []bool
}

// jitCountsHalved is where a count is halved, all the function's counts
// with it, so that none wraps and the frequencies stay as they were.
const jitCountsHalved = 1 << 31

// jitCallCountsOf is fn's counts, none made yet, or nil for a function
// that makes no call.
func jitCallCountsOf(fn *bytecode.Function) *jitCallCounts {
	n := 0
	for _, in := range fn.Code {
		if jitCounted(in.Op) {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	c := &jitCallCounts{code: len(fn.Code), pcs: make([]int32, 0, n), calls: make([]uint32, n)}
	for pc, in := range fn.Code {
		if jitCounted(in.Op) {
			c.pcs = append(c.pcs, int32(pc))
		}
	}
	return c
}

// jitCounted reports whether a call of op is counted (jitCallCounts): a
// call, a method call, a construction, one in tail position -- what the
// JIT inlines.
func jitCounted(op bytecode.Op) bool {
	switch jitCallOp(op) {
	case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
		return true
	}
	return false
}

// at is the index of the call at pc among c's, or -1.
func (c *jitCallCounts) at(pc int32) int {
	lo, hi := 0, len(c.pcs)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if c.pcs[m] < pc {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < len(c.pcs) && c.pcs[lo] == pc {
		return lo
	}
	return -1
}

// halve halves c's counts.
//
//go:noinline
func (c *jitCallCounts) halve() {
	c.runs >>= 1
	for i := range c.calls {
		c.calls[i] >>= 1
	}
}

// bytes is what c holds.
func (c *jitCallCounts) bytes() int {
	if c == nil {
		return 0
	}
	return int(unsafe.Sizeof(*c)) + jitCapBytes(c.pcs) + jitCapBytes(c.calls) + jitCapBytes(c.learned)
}

// jitCountLearned notes that native code made the call at pc of e's
// function, decided at its exits (jitCallSeen), if the existing tiers have
// not counted any there: its frequency is not known (jitCallFrequency).
func jitCountLearned(e *jitEntry, pc int) {
	c := e.calls
	if c == nil {
		return
	}
	if i := c.at(int32(pc)); i >= 0 && c.calls[i] == 0 {
		if c.learned == nil {
			c.learned = make([]bool, len(c.calls))
		}
		c.learned[i] = true
	}
}

// jitCounting reports whether the existing tiers count the calls of e's
// function (jitCallCounts): its entry warms, or its code is invalidated --
// it is to be compiled.
func jitCounting(e *jitEntry) bool {
	return e.calls != nil && (e.warming || e.invalid)
}

// jitCountRun counts an invocation the existing tiers run of e's
// function, cl's, if they count its calls (jitCounting), and has the
// calls of the frames of cl's template counted from then on
// (jitCodeFields' jitCounting): those of a frame whose invocation they did
// not count -- one native code left to them, which goes on in the
// interpreter -- are not, until one of the template's is counted again.
func jitCountRun(e *jitEntry, cl *closure) {
	if jitCounting(e) {
		if e.calls.runs++; e.calls.runs >= jitCountsHalved {
			e.calls.halve()
		}
		if cl.closureCode != nil {
			cl.jitCounting = true
		}
	}
}

// jitCountCall counts the call that made the top frame at its caller's
// call site (jitCountCallAt).
func (r *Runtime) jitCountCall() { r.jitCountCallAt(r.frameDepth) }

// jitCountPure counts a call of a pure body the call path answered without
// a frame (pureCallAt) at its caller's call site, the top frame's
// (jitCountCallAt): one it does not answer so it makes with a frame, which
// jitCountCall counts.
func (r *Runtime) jitCountPure() { r.jitCountCallAt(r.frameDepth + 1) }

// jitCountCallAt counts a call whose callee's frame is, or would be, the
// d-th, at its caller's call site, the frame below's (jitCallCounts),
// where the caller is a frame of a function whose calls the existing tiers
// count (jitCounting), of a template whose are (jitCountRun), not a
// built-in's -- and the call not native code's: through Go (goCallDepth),
// at an exit, or of a callee with no code (nativeCallDepth).
func (r *Runtime) jitCountCallAt(d int) {
	s := r.jit
	if s == nil || d < 2 || d == s.goCallDepth || d == s.nativeCallDepth {
		return
	}
	var g *frame
	if off := d - 2 - r.frameBase; uint(off) < uint(len(r.cur)) {
		// The caller's frame in the top's block, as almost always.
		g = &r.cur[off]
	} else {
		g = r.frameAt(d - 2)
	}
	if cl := g.cl; cl == nil || cl.closureCode == nil || !cl.jitCounting {
		// A built-in's frame, or one whose template's calls are not
		// counted (jitCountRun).
		return
	}
	e := s.hint(g.cl)
	if e == nil || !jitCounting(e) {
		// Its code compiled since: the template's calls are counted no
		// more, till they are again.
		g.cl.jitCounting = false
		return
	}
	c := e.calls
	if i := c.at(int32(g.pc) - 1); i >= 0 {
		if c.calls[i]++; c.calls[i] >= jitCountsHalved {
			c.halve()
		}
	}
}

// jitGhostCounts is the counts a ghost of fn kept, if they are of a
// function of fn's length (a ghost is found by its function's address,
// which a function made since may have), or fn's made afresh.
func jitGhostCounts(g jitGhost, ok bool, fn *bytecode.Function) *jitCallCounts {
	if !ok || g.calls == nil || g.calls.code != len(fn.Code) {
		return jitCallCountsOf(fn)
	}
	return g.calls
}

// jitCallFrequency is how often the call at pc of e's function is made for
// each invocation of it, as the existing tiers counted them
// (jitCallCounts) -- V8's call frequency, a site's calls over its
// function's invocations -- if known: the function run by them at least
// once, and the call's callees, as its decisions name them, ones each
// call of which they count (jitFramed), one at least. A call of a
// built-in is not counted, nor a call they never made but native code did
// (jitCountLearned): one V8's feedback had none for, where its code
// deoptimizes, to be compiled again from what its interpreter counts then;
// native code learns the call at its exits instead, going on.
func jitCallFrequency(e *jitEntry, pc int) (float64, bool) {
	counted := false
	for _, list := range [...][]jitInline{e.inlines, e.nativeCalls} {
		for _, x := range list {
			if int(x.pc) == pc {
				if !jitFramed(x) {
					return 0, false
				}
				counted = true
			}
		}
	}
	if !counted {
		return 0, false
	}
	return jitCallCount(e, pc)
}

// jitFramed reports whether the existing tiers count every call of x's
// callee they make (jitCallCounts): a function of the script's, called
// with a frame of its own, or a pure body, which the call path answers
// without one where it can (jitCountPure) -- not another body the call
// path may answer without one (bytecode.LeafKind), nor one called through
// Function.prototype.call, whose frame is the built-in's.
func jitFramed(x jitInline) bool {
	return x.cl != nil && !x.via && jitCountedCallee(x.cl)
}

// jitCountedCallee reports whether the existing tiers count every call of
// cl's function they make (jitFramed).
func jitCountedCallee(cl *closure) bool {
	return cl.fn.Leaf == bytecode.LeafNone || cl.fn.Leaf == bytecode.LeafPure
}

// jitCallCount is the call at pc's count over its function's runs, as
// jitCallFrequency has it, whatever its callees.
func jitCallCount(e *jitEntry, pc int) (float64, bool) {
	if e == nil || e.calls == nil || e.calls.runs == 0 {
		return 0, false
	}
	c := e.calls
	i := c.at(int32(pc))
	if i < 0 || c.calls[i] == 0 && c.learned != nil && c.learned[i] {
		return 0, false
	}
	return float64(c.calls[i]) / float64(c.runs), true
}
