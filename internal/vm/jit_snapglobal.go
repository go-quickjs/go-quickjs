//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The globals', the generators' steps' and the closures' families of a
// snapshot's derived methods (jit_snapderive.go): Global, whose answer
// reads the global object and a module's environment, and GenSteps, which
// makes the links of the generators a step resumes, both asked on the
// runtime's goroutine by register pc, as the snapshot's gather keys what
// it asks of the heap, and put at the program's PCs lowered of it by their
// bindings; and Closure, whose literal and pool the gather makes by
// register pc, the rest of its answer derived on the worker of the
// program: a callee's snapshot asks them of its register code before
// anything lowers it.

// jitRpcAsked is an answer a gather asked of the feedback by register pc
// rpc: its effect's index past 0, whether it answered, its results, and
// whether a binding put it at a PC of the program (bound).
type jitRpcAsked[T any] struct {
	// Nothing compares it: comparable, the type had an equality function
	// the compiler lays out among package vm's code.
	_     [0]func()
	rpc   int32
	eff   int32
	ok    bool
	v     T
	bound bool
}

// jitGlobalAt is the global the register instruction w reads, writes or
// checks a strict assignment of -- its cache's site, -1 for typeof's read,
// which has none -- or false for one of no global.
func jitGlobalAt(w bytecode.RInstr) (site int, write, check, ok bool) {
	site = int(w.C())
	switch w.Op() {
	case bytecode.RGetGlobal, bytecode.RGetGlobalInstanceOf:
	case bytecode.RSetGlobal:
		write = true
	case bytecode.RGetGlobalOpt:
		// typeof's read, which has no site of its own.
		site = -1
	case bytecode.RCheckGlobalRef:
		check = true
	default:
		return 0, false, false, false
	}
	return site, write, check, true
}

// jitGlobalOp reports whether op is one Global answers at.
func jitGlobalOp(op ir.Op) bool {
	return op == ir.BindingRead || op == ir.BindingWrite || op == ir.BindingCheck
}

// globalAt is Global's answer for the register instruction w at rpc, of
// the heap as it is.
func (f *jitRegFeedback) globalAt(rpc int, w bytecode.RInstr) (ssa.GlobalSite, bool) {
	site, write, check, ok := jitGlobalAt(w)
	if !ok || f.fb.cl == nil {
		return ssa.GlobalSite{}, false
	}
	name, ok := f.name(rpc, w.B())
	if !ok {
		return ssa.GlobalSite{}, false
	}
	return f.fb.globalSite(name, site, write, check)
}

// jitGlobalData is a snapshot's Global answers, asked by register pc, and
// its GenSteps answers.
type jitGlobalData struct {
	gathered bool
	globals  []jitRpcAsked[jitSnapAnsGlobal]
	steps    []jitRpcAsked[jitSnapAnsGenSteps]
}

func init() {
	jitSnapDerived |= 1<<snapGlobal | 1<<snapGenSteps
	jitSnapAskedAtGather |= 1<<snapGlobal | 1<<snapGenSteps
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherGlobal)
	jitSnapBinders = append(jitSnapBinders, bindGlobal)
}

// gatherGlobal asks fb, for s, Global at each of its register code's
// instructions of a global, and GenSteps at each step the code learned
// generators of, by register pc, their effects taken back as an asker
// takes them.
func gatherGlobal(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.global
	*d = jitGlobalData{gathered: true}
	if fb.fb.cl == nil {
		return
	}
	for rpc, w := range fb.rc.Code {
		if _, _, _, ok := jitGlobalAt(w); !ok {
			continue
		}
		before := s.begin(fb.fb)
		r0, r1 := fb.globalAt(rpc, w)
		eff := s.end(fb.fb, before)
		if r1 || eff != 0 {
			d.globals = append(d.globals, jitRpcAsked[jitSnapAnsGlobal]{rpc: int32(rpc), eff: eff, ok: r1, v: jitSnapAnsGlobal{r0: r0, r1: r1}})
		}
	}
	if e := fb.fb.e; e != nil {
		for _, st := range e.genSteps {
			rpc := int(st.pc)
			if rpc >= len(fb.rc.Code) || jitRpcAskedOf(d.steps, rpc) != nil {
				continue
			}
			before := s.begin(fb.fb)
			r0 := fb.regGenSteps(rpc)
			eff := s.end(fb.fb, before)
			if len(r0) != 0 || eff != 0 {
				d.steps = append(d.steps, jitRpcAsked[jitSnapAnsGenSteps]{rpc: int32(rpc), eff: eff, ok: len(r0) != 0, v: jitSnapAnsGenSteps{r0: r0}})
			}
		}
	}
}

// jitRpcAskedOf is list's answer at register pc rpc, nil for none.
func jitRpcAskedOf[T any](list []jitRpcAsked[T], rpc int) *jitRpcAsked[T] {
	for i := range list {
		if int(list[i].rpc) == rpc {
			return &list[i]
		}
	}
	return nil
}

// bindGlobal puts the answers gatherGlobal asked by register pc at each PC
// of s's program lowered of it whose op asks their method.
func bindGlobal(s *jitSnapshot) {
	d := &s.d.global
	if !d.gathered || len(d.globals)+len(d.steps) == 0 {
		return
	}
	for pc, op := range s.ops {
		asks := jitSnapAsks[op]
		if asks&(1<<snapGlobal) != 0 && jitGlobalOp(op) {
			if a := jitRpcAskedOf(d.globals, int(s.p.Origin[pc])); a != nil {
				a.bound = true
				s.heard(snapGlobal, int64(pc), a.ok, a.v.r0, a.v.r1)
				s.a.Global.reserve(int(s.room[snapGlobal]))
				s.a.Global.put(int64(pc), a.eff, a.ok, a.v)
			}
		}
		if asks&(1<<snapGenSteps) != 0 {
			if a := jitRpcAskedOf(d.steps, int(s.p.Origin[pc])); a != nil {
				a.bound = true
				s.heard(snapGenSteps, int64(pc), a.ok, a.v.r0)
				s.a.GenSteps.reserve(int(s.room[snapGenSteps]))
				s.a.GenSteps.put(int64(pc), a.eff, a.ok, a.v)
			}
		}
	}
}

// jitClosureLit is Closure's literal made by register pc: its pool,
// and its effect's index past 0.
type jitClosureLit struct {
	// Nothing compares it: comparable, the type had an equality function
	// the compiler lays out among package vm's code.
	_     [0]func()
	rpc   int32
	pool  *abi.ObjectPool
	eff   int32
	bound bool
}

// jitClosureData is a snapshot's copy of what Closure's answers are made
// of: the closure's constants (their slice, never written) and the
// literals made by register pc.
type jitClosureData struct {
	gathered bool
	consts   []Value
	lits     []jitClosureLit
}

func init() {
	jitSnapDerived |= 1 << snapClosure
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherClosure)
	jitSnapDerivers[jitDeriveCopies] = append(jitSnapDerivers[jitDeriveCopies], deriveClosure)
}

// gatherClosure makes, for s, the snapshot of fb, the literal of each
// closure_w Closure may answer at (jitClosureAsked), as Closure's asking
// makes it, its effect taken back; and copies the closure's constants'
// slice, which the worker derives the rest of the answer of with the
// program.
func gatherClosure(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.closure
	*d = jitClosureData{gathered: true}
	f := fb.fb
	if f.cl == nil {
		return
	}
	d.consts = f.cl.consts
	for rpc, w := range fb.rc.Code {
		if !jitClosureAsked(f, w) {
			continue
		}
		before := s.begin(f)
		l := f.r.jitRegClosureLiteral(f.e, f.cl, rpc, int(w.BC()))
		if l != nil {
			f.keep().pools = append(f.keep().pools, l.pool)
		}
		eff := s.end(f, before)
		if l != nil {
			s.root.d.pools = append(s.root.d.pools, l.pool)
			d.lits = append(d.lits, jitClosureLit{rpc: int32(rpc), pool: l.pool, eff: eff})
		}
	}
}

// deriveClosure puts s's Closure answers at every PC whose op asks it, of
// the literals its gather made and the program.
func deriveClosure(s *jitSnapshot) {
	d := &s.d.closure
	if !d.gathered || len(d.lits) == 0 {
		return
	}
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapClosure) == 0 || op != ir.Closure || int(s.p.Origin[pc]) >= len(s.rc.Code) {
			continue
		}
		var eff int32
		r0, r1 := jitClosureOf(s.p, s.rc, d.consts, pc, func(rpc, k int) (*abi.ObjectPool, bool) {
			for i := range d.lits {
				if l := &d.lits[i]; int(l.rpc) == rpc {
					l.bound, eff = true, l.eff
					return l.pool, true
				}
			}
			return nil, false
		})
		s.heard(snapClosure, int64(pc), r1, r0, r1)
		if r1 || eff != 0 {
			s.a.Closure.reserve(int(s.room[snapClosure]))
			s.a.Closure.put(int64(pc), eff, r1, jitSnapAnsClosure{r0: r0, r1: r1})
		}
	}
}
