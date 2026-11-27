//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math/bits"
	"unsafe"

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

// absent is the answer to a question asked by a state's PC that the
// snapshot kept no answer to: none, if the register code has that pc, or
// a miss.
func (q *jitRegSnapshot) absent(m jitSnapMethod, arg int64) {
	if arg >= 0 && arg < int64(q.regs) {
		return
	}
	panic(jitSnapMiss{m, arg, q.s.depth})
}

// jitRegSnapshotOf is fb's snapshot for p, its function's register code
// lowered: every question asked ahead, the callees' of the inline sites
// its answers name to jitSnapDepth, and the kinds of the leaves they name.
func jitRegSnapshotOf(fb *jitRegFeedback, p *ir.Program) *jitRegSnapshot {
	s := &jitSnapshot{callees: map[jitSnapCallee]*jitSnapshot{}, fb: fb.fb}
	s.root = s
	s.reg = &jitRegSnapshot{s: s, regs: len(fb.rc.Code)}
	s.askReg(fb, p)
	// The questions of other arguments, of what the answers name: the
	// leaves' kinds, and how Go makes the pools' objects escape analysis
	// may leave unmade.
	fns, pools := map[uintptr]bool{}, map[uintptr]bool{}
	all := append([]*jitSnapshot{s}, mapValues(s.callees)...)
	for _, c := range all {
		for i := range c.a.Leaf.e {
			if e := &c.a.Leaf.e[i]; e.answered && e.v.r0 != 0 {
				fns[e.v.r0] = true
			}
		}
		for i := range c.a.ViaLeaf.e {
			// G.call(x, ...)'s G, a leaf (ViaLeaf's fn).
			if e := &c.a.ViaLeaf.e[i]; e.answered && e.v.r1 != 0 {
				fns[e.v.r1] = true
			}
		}
		c.a.pools(pools)
	}
	for _, fn := range s.d.leaves {
		// The leaves a derivation's answers may name (jitSnapData's
		// leaves).
		fns[fn] = true
	}
	for _, pool := range s.d.pools {
		// The pools a derivation's answers may name (jitSnapData's
		// pools), which the worker answers after this.
		pools[uintptr(unsafe.Pointer(pool))] = true
	}
	for pool := range pools {
		shape, proto, ok := fb.VirtualRoot(pool)
		if h := fb.fb.r.jitTest.snapAsked; h != nil {
			h(s, snapVirtualRoot, int64(pool), true, []any{shape, proto, ok})
		}
		s.a.VirtualRoot.put(int64(pool), 0, true, jitSnapAnsVirtualRoot{shape, proto, ok})
	}
	for fn := range fns {
		k := fb.LeafKind(fn)
		h := fb.fb.r.jitTest.snapAsked
		for _, c := range all {
			if !c.d.gathered {
				// A callee no site inlines (nestReg), asked nothing.
				continue
			}
			if h != nil {
				h(c, snapLeafKind, int64(fn), true, []any{k})
			}
			c.a.LeafKind.put(int64(fn), 0, true, jitSnapAnsLeafKind{k})
		}
	}
	keep := fb.fb.r.jitTest.snapKeepsFeedback
	for _, c := range all {
		c.a.seal()
		if !keep {
			// The asking is done: what a derivation reads is the copies.
			c.rf = nil
		}
	}
	return s.reg
}

// askReg asks fb every question of p's: at each instruction, the methods
// the register feedback has of its op (jitSnapAsks), Generic among them;
// those asked by a state's PC at the learned and recursive-call PCs.
func (s *jitSnapshot) askReg(fb *jitRegFeedback, p *ir.Program) {
	s.rf, s.rc = fb, fb.rc
	s.bindProgram(p)
	s.askOnceReg(fb)
	// The methods the worker derives are gathered, not asked
	// (jit_snapderive.go): of the register code, not of p.
	s.gather(fb)
	s.bind()
	for pc, op := range s.ops {
		asks := jitSnapAsks[op] & jitRegSnapMethods &^ jitRegSnapStatePC &^ jitSnapDerived
		for _, at := range jitSnapAskedAt {
			asks |= at(s, pc) & jitSnapAsks[op]
		}
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

// bindProgram makes p, the function's register code lowered, the program
// s answers the questions of: its ops by PC (ops), and the room each
// method's table is made at its first answer (room).
func (s *jitSnapshot) bindProgram(p *ir.Program) {
	s.p, s.codeLen = p, len(p.Code)
	s.ops = make([]ir.Op, len(p.Code))
	// The PCs of each op counted, the room of each method made of the
	// counts of the ops that ask it: an increment a PC, not one a method a
	// PC.
	var counts [ir.LastOp + 1]int32
	for pc := range p.Code {
		op := p.Code[pc].Op
		s.ops[pc] = op
		counts[op]++
	}
	for op, n := range counts {
		if n == 0 {
			continue
		}
		for asks := jitSnapAsks[op] & jitRegSnapMethods &^ jitRegSnapStatePC; asks != 0; asks &= asks - 1 {
			s.room[bits.TrailingZeros64(asks)] += n
		}
	}
}

// askLeafCalls asks fb, at each PC its Leaf answered, the questions of the
// leaf's call that take the leaf as their second argument and are answered
// by their PC: its pool of objects (CreatePool) and whether its array is
// made again (ExecReuse). Not asked ahead, each was a miss wherever a leaf
// was called -- Object.create, a RegExp's exec -- the compile built on the
// runtime's goroutine at its install, from feedback asked then.
func (s *jitSnapshot) askLeafCalls(fb *jitRegFeedback) {
	s.a.Leaf.seal()
	var leaves []jitSnapEntry[jitSnapAnsLeaf]
	for _, e := range s.a.Leaf.e {
		if e.answered {
			leaves = append(leaves, e)
		}
	}
	for _, e := range leaves {
		pc, fn := int(e.arg), e.v.r0
		before := s.begin(fb.fb)
		pool := fb.CreatePool(pc, fn)
		eff := s.end(fb.fb, before)
		if h := fb.fb.r.jitTest.snapAsked; h != nil {
			h(s, snapCreatePool, int64(pc), true, []any{pool})
		}
		s.a.CreatePool.put(int64(pc), eff, true, jitSnapAnsCreatePool{pool})
		before = s.begin(fb.fb)
		reuse := fb.ExecReuse(pc, fn)
		eff = s.end(fb.fb, before)
		if h := fb.fb.r.jitTest.snapAsked; h != nil {
			h(s, snapExecReuse, int64(pc), true, []any{reuse})
		}
		s.a.ExecReuse.put(int64(pc), eff, true, jitSnapAnsExecReuse{reuse})
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
	c := &jitSnapshot{root: root, depth: s.depth + 1, fb: f.fb}
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
