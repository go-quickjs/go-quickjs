//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The constants' and the captured bindings' family of a snapshot's derived
// methods (jit_snapderive.go): RawMath, of the code alone; StringCell,
// StringContent and BigIntCell,
// the constants the program reads, of the closure's constants, which do
// not change once made (closureCode's consts); and UpvalueTyped, the typed
// array a captured binding holds, of what each binding held when the
// gather copied it. The feedback on the spot answers of the same, live.

// jitConstSrc is what a constant's answers are derived from: the program,
// its register code and the closure's constants, made once and never
// written (closureCode's consts, whose slice the closure's code made at
// its first run replaces, not changes, as it grows: regExtend).
type jitConstSrc struct {
	p      *ir.Program
	rc     *bytecode.RegCode
	consts []Value
	ok     bool
}

// constSrc is f's, live.
func (f *jitRegFeedback) constSrc() jitConstSrc {
	c := jitConstSrc{p: f.p, rc: f.rc}
	if cl := f.fb.cl; cl != nil {
		c.consts, c.ok = cl.consts, true
	}
	return c
}

// constant is the constant of op the program's instruction pc reads, as
// the register instruction it is lowered of loads it.
func (c *jitConstSrc) constant(pc int, op ir.Op) (Value, bool) {
	if pc < 0 || pc >= len(c.p.Code) || int(c.p.Origin[pc]) >= len(c.rc.Code) || c.p.Code[pc].Op != op || !c.ok {
		return Value{}, false
	}
	w := c.rc.Code[c.p.Origin[pc]]
	if w.Op() == bytecode.RLoadEmpty {
		return Str(emptyString), op == ir.StringConst
	}
	k := c.p.Code[pc].Key
	if w.Op() != bytecode.RLoadConst && w.Op() != bytecode.RSwitch && w.Op() != bytecode.RNewArrayK || int(k) >= len(c.consts) {
		return Value{}, false
	}
	v := c.consts[k]
	if op == ir.StringConst && !v.IsString() || op == ir.BigIntConst && !v.IsBigInt() {
		return Value{}, false
	}
	return v, true
}

// jitStringCellOf is StringCell's answer at pc of c, the cell appended to
// k's, the feedback that keeps what the code holds.
func jitStringCellOf(c *jitConstSrc, pc int, k *jitFeedback) (uintptr, bool) {
	v, ok := c.constant(pc, ir.StringConst)
	if !ok {
		return 0, false
	}
	cell := new(Value)
	*cell = v
	k.cells = append(k.cells, cell)
	return uintptr(unsafe.Pointer(cell)), true
}

// jitStringContentOf is StringContent's answer at pc of c.
func jitStringContentOf(c *jitConstSrc, pc int) (string, bool) {
	v, ok := c.constant(pc, ir.StringConst)
	if !ok {
		return "", false
	}
	if s := v.String(); s.left == nil {
		return s.s, true
	}
	return "", false
}

// jitBigIntCellOf is BigIntCell's answer at pc of c, the cell appended to
// k's.
func jitBigIntCellOf(c *jitConstSrc, pc int, k *jitFeedback) (uintptr, uint64, int, bool) {
	v, ok := c.constant(pc, ir.BigIntConst)
	if !ok {
		return 0, 0, ssa.BigConstHeap, false
	}
	cell := new(Value)
	*cell = v
	k.cells = append(k.cells, cell)
	if cell.isInt64Big() {
		return uintptr(unsafe.Pointer(cell)), uint64(cell.int64Big()), ssa.BigConstInt64, true
	}
	if cell.isShortBig() {
		if neg, mag := cell.shortBigParts(); !neg {
			return uintptr(unsafe.Pointer(cell)), mag, ssa.BigConstUint64, true
		}
	}
	return uintptr(unsafe.Pointer(cell)), 0, ssa.BigConstHeap, true
}

// jitUpvalueReadAt is the captured binding the program's instruction pc
// of p reads, UpvalueTyped's question, or false.
func jitUpvalueReadAt(p *ir.Program, pc int) (int, bool) {
	if pc < 0 || pc >= len(p.Code) || p.Code[pc].Op != ir.UpvalueRead {
		return 0, false
	}
	return int(p.Code[pc].Key), true
}

// jitUpvalueKind is the kind of the typed array cl's captured binding k
// holds now, or false.
func jitUpvalueKind(cl *closure, k int) (uint8, bool) {
	if cl == nil || k >= len(cl.upvalues) || cl.upvalues[k] == nil {
		return 0, false
	}
	v := cl.upvalues[k].get()
	if !jitTypedArray(v) {
		return 0, false
	}
	return jitTypedKind(true, v), true
}

// jitConstData is a snapshot's copy of what the family's answers are made
// of: the closure's constants (their slice, which is never written), and
// the kind of the typed array each captured binding held, plus one, 0 for
// none.
type jitConstData struct {
	gathered bool
	consts   []Value
	ok       bool
	upvalues []uint8
}

func init() {
	jitSnapDerived |= 1<<snapStringCell | 1<<snapStringContent | 1<<snapBigIntCell | 1<<snapUpvalueTyped | 1<<snapRawMath
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, gatherConst)
	jitSnapDerivers[jitDeriveCopies] = append(jitSnapDerivers[jitDeriveCopies], deriveConst)
}

// gatherConst copies, for s, the snapshot of fb, the closure's constants'
// slice and what its captured bindings hold: of the closure, not the
// program.
func gatherConst(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.consts
	*d = jitConstData{gathered: true}
	cl := fb.fb.cl
	if cl == nil {
		return
	}
	d.consts, d.ok = cl.consts, true
	for k := range cl.upvalues {
		if kind, ok := jitUpvalueKind(cl, k); ok {
			if d.upvalues == nil {
				d.upvalues = make([]uint8, len(cl.upvalues))
			}
			d.upvalues[k] = kind + 1
		}
	}
}

// deriveConst puts s's answers of the family's methods, at every PC whose
// op asks each, derived from what its gather copied; the cells, appended
// to its sink.
func deriveConst(s *jitSnapshot) {
	d := &s.d.consts
	if !d.gathered {
		return
	}
	c := jitConstSrc{p: s.p, rc: s.rc, consts: d.consts, ok: d.ok}
	sink := s.sinkOf()
	for pc, op := range s.ops {
		asks := jitSnapAsks[op]
		if asks&(1<<snapStringCell) != 0 {
			before := s.begin(sink)
			r0, r1 := jitStringCellOf(&c, pc, sink.keep())
			eff := s.endDerived(sink, before)
			s.heard(snapStringCell, int64(pc), r1, r0, r1)
			if r1 || eff != 0 {
				s.a.StringCell.reserve(int(s.room[snapStringCell]))
				s.a.StringCell.put(int64(pc), eff, r1, jitSnapAnsStringCell{r0: r0, r1: r1})
			}
		}
		if asks&(1<<snapStringContent) != 0 {
			r0, r1 := jitStringContentOf(&c, pc)
			s.heard(snapStringContent, int64(pc), r1, r0, r1)
			if r1 {
				s.a.StringContent.reserve(int(s.room[snapStringContent]))
				s.a.StringContent.put(int64(pc), 0, r1, jitSnapAnsStringContent{r0: r0, r1: r1})
			}
		}
		if asks&(1<<snapBigIntCell) != 0 {
			before := s.begin(sink)
			r0, r1, r2, r3 := jitBigIntCellOf(&c, pc, sink.keep())
			eff := s.endDerived(sink, before)
			s.heard(snapBigIntCell, int64(pc), r3, r0, r1, r2, r3)
			if r3 || eff != 0 {
				s.a.BigIntCell.reserve(int(s.room[snapBigIntCell]))
				s.a.BigIntCell.put(int64(pc), eff, r3, jitSnapAnsBigIntCell{r0: r0, r1: r1, r2: r2, r3: r3})
			}
		}
		if asks&(1<<snapRawMath) != 0 {
			r0, r1 := jitRawMathOf(s.rc, s.p, pc)
			s.heard(snapRawMath, int64(pc), r1, r0, r1)
			if r1 {
				s.a.RawMath.reserve(int(s.room[snapRawMath]))
				s.a.RawMath.put(int64(pc), 0, r1, jitSnapAnsRawMath{r0: r0, r1: r1})
			}
		}
		if asks&(1<<snapUpvalueTyped) != 0 {
			var r0 uint8
			r1 := false
			if k, ok := jitUpvalueReadAt(s.p, pc); ok && k < len(d.upvalues) && d.upvalues[k] != 0 {
				r0, r1 = d.upvalues[k]-1, true
			}
			s.heard(snapUpvalueTyped, int64(pc), r1, r0, r1)
			if r1 {
				s.a.UpvalueTyped.reserve(int(s.room[snapUpvalueTyped]))
				s.a.UpvalueTyped.put(int64(pc), 0, r1, jitSnapAnsUpvalueTyped{r0: r0, r1: r1})
			}
		}
	}
}
