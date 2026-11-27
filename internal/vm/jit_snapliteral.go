//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// Literal's and Define's answers in a snapshot, asked by register pc on
// the runtime's goroutine (gatherLiteral) and put at the program's PCs
// lowered of each (bindLiteral): a literal is made, its pool filled, at
// its first ask, kept in what the function's code learned, and a field's
// definition reads the shapes its objects have taken since -- the heap --
// so both are asked on the goroutine, of the register code, in the order
// the asking at the PCs asks them: a literal, then its fields.

// jitLiteralData is a snapshot's Literal and Define answers, by register
// pc and, a definition's, its field's place among the literal's; the
// register pcs gathered for a literal (litAt), and the PCs of the program
// asking either that the gather asked at no register pc of (missed),
// which a test counts.
type jitLiteralData struct {
	lits   []jitPropAsked[jitSnapAnsLiteral]
	litAt  []int32
	defs   []jitDefineAsked
	missed int
}

// jitDefineAsked is a Define answer gathered for field i of the object
// literal at register pc rpc.
type jitDefineAsked struct {
	// Nothing compares it: comparable, the type had an equality function
	// the compiler lays out among package vm's code.
	_     [0]func()
	rpc   int32
	i     int32
	eff   int32
	ok    bool
	v     jitSnapAnsDefine
	bound bool
}

func init() {
	jitSnapDerived |= 1<<snapLiteral | 1<<snapDefine
	jitSnapAskedAtGather |= 1<<snapLiteral | 1<<snapDefine
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherLiteral)
	jitSnapBinders = append(jitSnapBinders, bindLiteral)
}

// jitLiteralOp reports whether the lowering makes the register instruction
// w at register pc rpc of rc, fn's, a literal natively (ir.ObjectLiteral,
// ir.ArrayLiteral), or may: a RegExp literal's call -- regexp_lit's, a
// call making its argument one -- is Go's whole where the lowering finds
// its window's registers read past it, which only the program says; its
// literal is the one the calls' gather makes (jitTargetsGather), found.
func jitLiteralOp(rc *bytecode.RegCode, fn *bytecode.Function, rpc int, w bytecode.RInstr) bool {
	switch w.Op() {
	case bytecode.RNewObject, bytecode.RNewObjectLit, bytecode.RNewArrayE, bytecode.RNewRegExp, bytecode.RLit:
		return true
	case bytecode.RNewArray, bytecode.RNewArrayK:
		return jitcompile.RegArrayLiteral(rc, fn, rpc)
	case bytecode.RCallMW:
		_, ok := regLitArgConst(rc, rpc)
		return ok
	}
	return false
}

// gatherLiteral asks fb, for s, Literal's answer at each register
// instruction the lowering makes a literal of (jitLiteralOp), and, an
// object literal's, Define's for each of its fields after it, as the
// asking at its PCs does: the effects taken back, kept with the answers.
func gatherLiteral(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.lits
	*d = jitLiteralData{}
	cl := fb.fb.cl
	if cl == nil || fb.fb.e == nil {
		return
	}
	rc := fb.rc
	for rpc, w := range rc.Code {
		if !jitLiteralOp(rc, cl.fn, rpc, w) {
			continue
		}
		d.litAt = append(d.litAt, int32(rpc))
		before := s.begin(fb.fb)
		r0, r1 := fb.literalSite(fb.literalOf(rpc, w))
		eff := s.end(fb.fb, before)
		if eff != 0 {
			// Its pool, which the snapshot asks the root of ahead
			// (jitSnapData's pools): bound on the worker, a callee's answer
			// is not among those asked before it.
			own, kept := s.root.lists(s.effects[eff-1])
			s.root.d.pools = append(append(s.root.d.pools, own.pools...), kept.pools...)
		}
		if r1 || eff != 0 {
			d.lits = append(d.lits, jitPropAsked[jitSnapAnsLiteral]{rpc: int32(rpc), eff: eff, ok: r1, v: jitSnapAnsLiteral{r0: r0, r1: r1}})
		}
		if w.Op() != bytecode.RNewObjectLit || rpc >= len(rc.Aux) || int(rc.Aux[rpc]) >= len(rc.ObjKeys) {
			continue
		}
		for i := range rc.ObjKeys[rc.Aux[rpc]] {
			before := s.begin(fb.fb)
			r0, r1 := fb.defineAt(rpc, w, i)
			eff := s.end(fb.fb, before)
			if r1 || eff != 0 {
				d.defs = append(d.defs, jitDefineAsked{rpc: int32(rpc), i: int32(i), eff: eff, ok: r1, v: jitSnapAnsDefine{r0: r0, r1: r1}})
			}
		}
	}
}

// bindLiteral puts the answers gatherLiteral asked by register pc at each
// PC of s's program lowered of it whose op asks their method: a literal's
// at its ObjectLiteral or ArrayLiteral, a field's definition at its
// FieldDefine, by its place after the literal's first instruction.
func bindLiteral(s *jitSnapshot) {
	d := &s.d.lits
	for pc, op := range s.ops {
		asks := jitSnapAsks[op] & (1<<snapLiteral | 1<<snapDefine)
		if asks == 0 {
			continue
		}
		rpc := int32(-1)
		if int(s.p.Origin[pc]) < len(s.rc.Code) {
			rpc = int32(s.p.Origin[pc])
		}
		if asks&(1<<snapLiteral) != 0 {
			bindLiteralAt(s, d, pc, op, rpc)
		}
		if asks&(1<<snapDefine) != 0 {
			bindDefineAt(s, d, pc, op, rpc)
		}
	}
}

// bindLiteralAt puts Literal's answer at pc, of register pc rpc.
func bindLiteralAt(s *jitSnapshot, d *jitLiteralData, pc int, op ir.Op, rpc int32) {
	var a *jitPropAsked[jitSnapAnsLiteral]
	if op == ir.ObjectLiteral || op == ir.ArrayLiteral {
		gathered := false
		for _, at := range d.litAt {
			gathered = gathered || at == rpc
		}
		if !gathered {
			// A literal the gather's ops did not take.
			d.missed++
		}
		for i := range d.lits {
			if d.lits[i].rpc == rpc {
				a = &d.lits[i]
				break
			}
		}
	}
	if a == nil {
		s.heard(snapLiteral, int64(pc), false, ssa.LiteralSite{}, false)
		return
	}
	a.bound = true
	s.heard(snapLiteral, int64(pc), a.ok, a.v.r0, a.v.r1)
	s.a.Literal.reserve(int(s.room[snapLiteral]))
	s.a.Literal.put(int64(pc), a.eff, a.ok, a.v)
}

// bindDefineAt puts Define's answer at pc, of register pc rpc: the field
// whose place is pc's after the literal's first instruction, an
// ObjectLiteral of the same register pc.
func bindDefineAt(s *jitSnapshot, d *jitLiteralData, pc int, op ir.Op, rpc int32) {
	var a *jitDefineAsked
	if op == ir.FieldDefine && rpc >= 0 && s.rc.Code[rpc].Op() == bytecode.RNewObjectLit {
		first := pc
		for first > 0 && s.p.Origin[first-1] == s.p.Origin[pc] {
			first--
		}
		if s.p.Code[first].Op == ir.ObjectLiteral {
			i := int32(pc - first - 1)
			for k := range d.defs {
				if x := &d.defs[k]; x.rpc == rpc && x.i == i {
					a = x
					break
				}
			}
		}
	}
	if a == nil {
		s.heard(snapDefine, int64(pc), false, (*ssa.PropertyAdd)(nil), false)
		return
	}
	a.bound = true
	s.heard(snapDefine, int64(pc), a.ok, a.v.r0, a.v.r1)
	s.a.Define.reserve(int(s.room[snapDefine]))
	s.a.Define.put(int64(pc), a.eff, a.ok, a.v)
}
