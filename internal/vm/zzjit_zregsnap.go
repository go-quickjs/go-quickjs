//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Register code's feedback asked ahead, for its compiles in the
// background, as the stack form's is (zzjit_snapshot.go): a jitRegSnapshot
// answers what jitRegFeedback answers -- its methods alone, generated from
// them (snapgen, zzjit_snapshot_gen.go) -- from a jitSnapshot asked of it
// on the runtime's goroutine, numbered as jitFeedback's methods of the
// same names. A question it was not asked ahead is a miss, which has the
// compile built on the runtime's goroutine at its install.
type jitRegSnapshot struct {
	s *jitSnapshot
	// regs is how long the register code is: the methods asked by a
	// state's PC, a register pc (jitRegSnapStatePC), are asked at each.
	regs int
}

// jitRegSnapStatePC are the methods the build asks by a state's PC, which
// in a register program is a register pc, not an instruction's (the
// parity tool's Resumed and EntryGeneric): asked at every register pc.
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
	// The questions of other arguments, of what the answers name, as the
	// stack form's (jitSnapshotOf): the leaves' kinds, and how Go makes
	// the pools' objects escape analysis may leave unmade.
	fns, pools := map[uintptr]bool{}, map[uintptr]bool{}
	for _, c := range append([]*jitSnapshot{s}, mapValues(s.callees)...) {
		for k, a := range c.answers {
			if k.method == snapLeaf {
				if fn, ok := a[0].(uintptr); ok && fn != 0 {
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
// those asked by a state's PC at each register pc.
func (s *jitSnapshot) askReg(fb *jitRegFeedback, p *ir.Program) {
	s.codeLen = len(p.Code)
	s.ops = make([]ir.Op, len(p.Code))
	s.askOnceReg(fb)
	for pc := range p.Code {
		op := p.Code[pc].Op
		s.ops[pc] = op
		asks := jitSnapAsks[op] & jitRegSnapMethods &^ jitRegSnapStatePC
		for m := jitSnapMethod(0); int(m) < len(jitRegSnapAskers); m++ {
			if asks&(1<<m) != 0 && jitRegSnapAskers[m] != nil {
				jitRegSnapAskers[m](s, fb, pc)
			}
		}
	}
	for rpc := range fb.rc.Code {
		for _, m := range [...]jitSnapMethod{snapResumed, snapEntryGeneric} {
			if ask := jitRegSnapAskers[m]; ask != nil {
				ask(s, fb, rpc)
			}
		}
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
	if c.depth < jitSnapDepth && site.Program != nil {
		c.askReg(f, site.Program)
	}
	site.Feedback = c.reg
	return site
}
