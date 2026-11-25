//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math/bits"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Register code's feedback asked ahead, for its compiles in the background
// (jit_snapshot.go): a jitRegSnapshot answers what jitRegFeedback answers
// -- its methods alone, generated from them (snapgen,
// jit_snapshot_gen.go) -- from a jitSnapshot asked of it on the runtime's
// goroutine. A question it was not asked ahead is a miss, which has the
// compile built on the runtime's goroutine at its install.
type jitRegSnapshot struct {
	s *jitSnapshot
	// regs is how long the register code is: the methods asked by a
	// state's PC, a register pc (jitRegSnapStatePC), default to false
	// within this bound.
	regs int
}

// jitRegSnapStatePC are the methods the build asks by a state's PC, which
// in a register program is a register pc, not an instruction's (Resumed and
// EntryGeneric): only PCs with a true answer need to be captured.
const jitRegSnapStatePC = 1<<snapResumed | 1<<snapEntryGeneric

// get is the answer to a question, as jitSnapshot's get has it: one asked
// by a state's PC answered nothing where it is not kept, if the register
// code has that pc.
func (q *jitRegSnapshot) get(m jitSnapMethod, arg int64) []any {
	if jitRegSnapStatePC&(1<<m) == 0 {
		return q.s.get(m, arg)
	}
	if a, ok := q.s.answers[jitSnapKey{m, arg}]; ok {
		return a
	}
	if arg >= 0 && arg < int64(q.regs) {
		return nil
	}
	panic(jitSnapMiss{m, arg, q.s.depth})
}

// jitRegSnapshotOf is fb's snapshot for p, its function's register code
// lowered: every question asked ahead, the callees' of the inline sites
// its answers name to jitSnapDepth, and the kinds of the leaves they name.
func jitRegSnapshotOf(fb *jitRegFeedback, p *ir.Program) *jitRegSnapshot {
	s := &jitSnapshot{answers: map[jitSnapKey][]any{}, callees: map[jitSnapCallee]*jitSnapshot{}, fb: fb.fb, effects: map[jitSnapKey]*jitSnapEffect{}}
	s.root = s
	s.reg = &jitRegSnapshot{s: s, regs: len(fb.rc.Code)}
	s.askReg(fb, p)
	// The questions of other arguments, of what the answers name: the
	// leaves' kinds, and how Go makes the pools' objects escape analysis
	// may leave unmade.
	fns, pools := map[uintptr]bool{}, map[uintptr]bool{}
	for _, c := range append([]*jitSnapshot{s}, mapValues(s.callees)...) {
		for k, a := range c.answers {
			switch k.method {
			case snapLeaf:
				if fn, ok := a[0].(uintptr); ok && fn != 0 {
					fns[fn] = true
				}
			case snapViaLeaf:
				// G.call(x, ...)'s G, a leaf (ViaLeaf's fn).
				if fn, ok := a[1].(uintptr); ok && fn != 0 {
					fns[fn] = true
				}
			}
			for _, x := range a {
				jitSnapPools(x, pools)
			}
		}
	}
	for pool := range pools {
		shape, proto, ok := fb.VirtualRoot(pool)
		s.put(snapVirtualRoot, int64(pool), shape, proto, ok)
	}
	for fn := range fns {
		k := fb.LeafKind(fn)
		s.put(snapLeafKind, int64(fn), k)
		for _, c := range s.callees {
			c.put(snapLeafKind, int64(fn), k)
		}
	}
	return s.reg
}

// askReg asks fb every question of p's: at each instruction, the methods
// the register feedback has of its op (jitSnapAsks), Generic among them;
// those asked by a state's PC at the learned and recursive-call PCs.
func (s *jitSnapshot) askReg(fb *jitRegFeedback, p *ir.Program) {
	s.codeLen = len(p.Code)
	s.ops = make([]ir.Op, len(p.Code))
	s.askOnceReg(fb)
	for pc := range p.Code {
		op := p.Code[pc].Op
		s.ops[pc] = op
		asks := jitSnapAsks[op] & jitRegSnapMethods &^ jitRegSnapStatePC
		for asks != 0 {
			m := jitSnapMethod(bits.TrailingZeros64(asks))
			asks &= asks - 1
			if ask := jitRegSnapAskers[m]; ask != nil {
				ask(s, fb, pc)
			}
		}
	}
	fb.snapshotStatePCs(s)
	s.askLeafCalls(fb)
}

// askLeafCalls asks fb, at each PC its Leaf answered, the questions of the
// leaf's call that take the leaf as their second argument and are answered
// by their PC: its pool of objects (CreatePool) and whether its array is
// made again (ExecReuse). Not asked ahead, each was a miss wherever a leaf
// was called -- Object.create, a RegExp's exec -- the compile built on the
// runtime's goroutine at its install, from feedback asked then.
func (s *jitSnapshot) askLeafCalls(fb *jitRegFeedback) {
	var pcs []int
	for k := range s.answers {
		if k.method == snapLeaf {
			pcs = append(pcs, int(k.arg))
		}
	}
	slices.Sort(pcs)
	for _, pc := range pcs {
		fn, _ := s.answers[jitSnapKey{snapLeaf, int64(pc)}][0].(uintptr)
		before := s.begin(fb.fb)
		pool := fb.CreatePool(pc, fn)
		s.end(fb.fb, before, snapCreatePool, int64(pc))
		s.put(snapCreatePool, int64(pc), pool)
		before = s.begin(fb.fb)
		reuse := fb.ExecReuse(pc, fn)
		s.end(fb.fb, before, snapExecReuse, int64(pc))
		s.put(snapExecReuse, int64(pc), reuse)
	}
}

// nestReg is site with its callee's register feedback a snapshot, asked
// ahead, once for each callee the root has met; past jitSnapDepth, one
// that answers nothing.
func (s *jitSnapshot) nestReg(site ssa.InlineSite) ssa.InlineSite {
	f, ok := site.Feedback.(*jitRegFeedback)
	if !ok {
		return site
	}
	root := s.root
	key := jitSnapCallee{cl: f.fb.cl, fn: f.fb.fn, root: f.fb.root}
	if c := root.callees[key]; c != nil && c.reg != nil {
		site.Feedback = c.reg
		return site
	}
	c := &jitSnapshot{answers: map[jitSnapKey][]any{}, root: root, depth: s.depth + 1, fb: f.fb, effects: map[jitSnapKey]*jitSnapEffect{}}
	c.reg = &jitRegSnapshot{s: c, regs: len(f.rc.Code)}
	root.callees[key] = c
	if c.depth < jitSnapDepth && site.Program != nil && !ssa.NeverInlined(site.Program) {
		// Not a callee no site inlines (ssa.NeverInlined), whose feedback
		// the build never asks: every callee the root met had been asked
		// every question, nested to jitSnapDepth, Typescript's compiles in
		// the background taking 620 ms on the runtime's goroutine -- more
		// than its compiles there took, 390 -- most of it so.
		c.askReg(f, site.Program)
	}
	site.Feedback = c.reg
	return site
}
