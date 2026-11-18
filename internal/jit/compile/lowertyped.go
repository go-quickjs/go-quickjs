package compile

import (
	"fmt"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// The lowering of typed code (bytecode.RegCode's Typed: a function of a
// validated asm.js module, docs/register-interpreter.md, "Typed code")
// into the slot IR, beside LowerReg's of register code (internal/jit/
// README.md, "Typed code"). Its frame is register code's -- the window,
// its head the function, this and the parameters as passed, Values; a
// call's window Values -- and so are the ordinary operations it holds
// (bytecode.TypedPlainOps), lowered as LowerReg lowers them. Its raw
// registers are raw slots (ir.Program's Kinds), and its raw operations Raw
// instructions on them (ir's raw.go), which the builder makes the SSA
// operations they are with no check: the kinds are proven. A heap access
// is a RawLoad or RawStore of the heap the code's entry has, and a module
// variable an upvalue's read or write, converted or boxed.

// LowerTyped lowers fn's typed code rc. What it does not lower it refuses,
// with the operation's name: the function then runs in the typed
// interpreter.
func LowerTyped(rc *bytecode.RegCode, fn *bytecode.Function) (p *ir.Program, err error) {
	defer func() {
		if v := recover(); v != nil {
			p, err = nil, refuse(-1, fmt.Sprintf("internal error: %v", v))
		}
	}()
	return lowerTyped(rc, regView(rc, fn))
}

// typedReach is which of rc's typed instructions the code reaches from
// its first.
func typedReach(rc *bytecode.RegCode) ([]bool, error) {
	n := len(rc.Code)
	reach := make([]bool, n)
	work := []int{0}
	reach[0] = true
	for len(work) > 0 {
		pc := work[len(work)-1]
		work = work[:len(work)-1]
		for _, t := range typedSuccessors(rc, pc) {
			if t < 0 || t >= n {
				return nil, refuse(pc, "branch target out of bounds")
			}
			if !reach[t] {
				reach[t] = true
				work = append(work, t)
			}
		}
	}
	return reach, nil
}

// typedSuccessors is where typed code goes on from the instruction at pc.
func typedSuccessors(rc *bytecode.RegCode, pc int) []int {
	w := rc.Code[pc]
	switch op := w.Op(); {
	case op == bytecode.RJmp:
		return []int{regTarget(pc, int32(w.BC()))}
	case op == bytecode.RRet || op == bytecode.RRetUndef:
		return nil
	case op >= bytecode.TJLtS && op <= bytecode.TJNZ || op >= bytecode.TJFLt && op <= bytecode.TJFNe:
		return []int{pc + 1, regTarget(pc, w.SC())}
	}
	return []int{pc + 1}
}

// typedLowering is a typed lowering's state: regLowering's, and the kinds
// of the scratch slots its expansions take.
type typedLowering struct {
	*regLowering
	kinds []uint8
	// targets marks the instructions a jump or a branch goes to, made at
	// callResult's first ask.
	targets []bool
}

// callResult is the Key of the conversion at pc of register r: ir's
// RawNumber where r is the result of the call just before, of a function
// of the module -- its function register written by the read of a
// function binding (AsmUpFunc) or a table's element (TTabGet), which the
// link checks, with nothing jumping in between -- whose every return
// validation proves a number of its declared type; else 0, a value Go
// converts what is no number of: an import's result.
func (l *typedLowering) callResult(pc, r int) uint32 {
	rc := l.rc
	if pc < 1 {
		return 0
	}
	call := rc.Code[pc-1]
	if call.Op() != bytecode.RCallW || int(call.A()) != r {
		return 0
	}
	if l.targets == nil {
		l.targets = make([]bool, len(rc.Code)+1)
		for q := range rc.Code {
			for _, t := range typedSuccessors(rc, q) {
				if t != q+1 && t >= 0 && t < len(l.targets) {
					l.targets[t] = true
				}
			}
		}
	}
	fr := int(call.A())
	for q := pc - 2; q >= 0; q-- {
		if l.targets[q+1] {
			return 0
		}
		x := rc.Code[q]
		if int(x.A()) != fr {
			continue
		}
		switch x.Op() {
		case bytecode.RGetUpval:
			if k := int(x.BC()); k < len(rc.Asm) && rc.Asm[k].Kind == bytecode.AsmUpFunc {
				return ir.RawNumber
			}
		case bytecode.TTabGet:
			if k := int(x.C()); k < len(rc.Asm) && rc.Asm[k].Kind == bytecode.AsmUpTable {
				return ir.RawNumber
			}
		}
		return 0
	}
	return 0
}

// rawTemp is a scratch slot of kind k of the current instruction's.
func (l *typedLowering) rawTemp(k uint8) int {
	s := l.temp()
	for len(l.kinds) <= s {
		l.kinds = append(l.kinds, ir.SlotValue)
	}
	if l.kinds[s] != ir.SlotValue && l.kinds[s] != k {
		// A scratch slot is of one kind throughout: one of another's is
		// taken past it.
		return l.rawTemp(k)
	}
	l.kinds[s] = k
	return s
}

func lowerTyped(rc *bytecode.RegCode, fn *bytecode.Function) (*ir.Program, error) {
	switch {
	case fn == nil || rc == nil || !rc.Typed || !rc.Wide:
		return nil, refuse(-1, "no typed code")
	case len(rc.Code) == 0:
		return nil, refuse(-1, "no instructions")
	case len(rc.Kinds) != rc.NumRegs || len(rc.Handlers) != 0:
		return nil, refuse(-1, "typed code's tables")
	case rc.NumRegs < 2 || rc.NumRegs+len(fn.Upvalues) > maxSlots || 2+rc.Params > rc.NumRegs:
		return nil, refuse(-1, "slot budget")
	}
	n := len(rc.Code)
	reach, err := typedReach(rc)
	if err != nil {
		return nil, err
	}
	l := &typedLowering{regLowering: &regLowering{rc: rc, fn: fn, m: lowering{ssa: true}, scratch: rc.NumRegs + len(fn.Upvalues), this: -1}}
	l.kinds = make([]uint8, l.scratch)
	for r, k := range rc.Kinds {
		switch k {
		case bytecode.RegInt:
			l.kinds[r] = ir.SlotI32
		case bytecode.RegFloat:
			l.kinds[r] = ir.SlotF64
		}
	}
	start := make([]int, n+1)
	for pc := range n {
		start[pc] = len(l.code)
		l.pc, l.used = pc, 0
		if !reach[pc] {
			l.emit(ir.Instruction{Op: ir.Nop}, false)
			continue
		}
		if why := l.lowerTyped(pc); why != "" {
			return nil, refuse(pc, why)
		}
	}
	start[n] = len(l.code)
	if l.scratch+l.most > maxSlots {
		return nil, refuse(-1, "slot budget")
	}
	for i := range l.code {
		if l.patch[i] {
			t := l.code[i].Target
			if t < 0 || t >= n || !reach[t] {
				return nil, refuse(int(l.origin[i]), "branch target out of bounds")
			}
			l.code[i].Target = start[t]
		}
	}
	starts := make([]int32, n+1)
	for pc, i := range start {
		starts[pc] = int32(i)
	}
	locals := l.scratch + l.most
	for len(l.kinds) < locals {
		l.kinds = append(l.kinds, ir.SlotValue)
	}
	p := &ir.Program{Code: slices.Clone(l.code), Origin: slices.Clone(l.origin), Starts: starts, Maps: make([]ir.StateMap, len(l.code)),
		Regs: rc.NumRegs, Locals: locals, ParamBase: 2, Params: rc.Params,
		UpvalueBase: rc.NumRegs, Upvalues: len(fn.Upvalues), Kinds: l.kinds[:locals],
	}
	for i := range p.Maps {
		p.Maps[i] = ir.StateMap{PC: l.origin[i]}
		if !reach[l.origin[i]] {
			p.Maps[i].Depth = -1
		}
	}
	selectNumericProperties(p, true)
	liveRegs(p, rc, false)
	if err := p.Validate(); err != nil {
		return nil, refuse(-1, err.Error())
	}
	return p, nil
}

// lowerTyped lowers the typed instruction at pc, or says why it does not.
func (l *typedLowering) lowerTyped(pc int) string {
	rc := l.rc
	w := rc.Code[pc]
	a, b, c := int(w.A()), int(w.B()), int(w.C())
	unaryNone := ir.Literal(ir.Float(0))
	raw := func(op ir.RawOp, dest int, left, right ir.Operand) {
		l.emit(ir.Instruction{Op: ir.Raw, Raw: op, Dest: dest, Left: left, Right: right}, false)
	}
	rawK := func(op ir.RawOp, dest int, left, right ir.Operand, key uint32) {
		l.emit(ir.Instruction{Op: ir.Raw, Raw: op, Dest: dest, Left: left, Right: right, Key: key}, false)
	}
	cmp := func(op ir.RawOp, o ir.Operator, dest int, left, right ir.Operand) {
		l.emit(ir.Instruction{Op: ir.Raw, Raw: op, Operator: o, Dest: dest, Left: left, Right: right}, false)
	}
	br := func(op ir.RawOp, o ir.Operator, left, right ir.Operand, when bool) {
		l.emit(ir.Instruction{Op: ir.Branch, Raw: op, Operator: o, Left: left, Right: right, When: when, Target: regTarget(pc, w.SC())}, true)
	}
	slot := func(r int) ir.Operand { return ir.Slot(r) }
	imm := func(v int32) ir.Operand { return number(float64(v)) }
	switch op := w.Op(); op {
	case bytecode.RNop, bytecode.RMove, bytecode.RLoadInt, bytecode.RLoadUndef, bytecode.RGetUpval, bytecode.RJmp,
		bytecode.RCallW, bytecode.RRet, bytecode.RRetUndef:
		l.lower(pc)
	case bytecode.TMov:
		if l.kinds[a] == ir.SlotF64 {
			raw(ir.RawFMov, a, slot(b), unaryNone)
		} else {
			raw(ir.RawIMov, a, slot(b), unaryNone)
		}
	case bytecode.TIConst:
		raw(ir.RawIConst, a, imm(int32(w.BC())), unaryNone)
	case bytecode.TFConst:
		if int(w.BC()) >= len(rc.Floats) {
			return "float constant out of range"
		}
		raw(ir.RawFConst, a, number(rc.Floats[w.BC()]), unaryNone)
	case bytecode.TConvI:
		rawK(ir.RawConvI, a, slot(b), unaryNone, l.callResult(pc, b))
	case bytecode.TConvF:
		rawK(ir.RawConvF, a, slot(b), unaryNone, l.callResult(pc, b))
	case bytecode.TIBox:
		raw(ir.RawIBox, a, slot(b), unaryNone)
	case bytecode.TUBox:
		raw(ir.RawUBox, a, slot(b), unaryNone)
	case bytecode.TFBox:
		raw(ir.RawFBox, a, slot(b), unaryNone)
	case bytecode.TAdd, bytecode.TSub, bytecode.TMul, bytecode.TDivS, bytecode.TDivU, bytecode.TRemS, bytecode.TRemU,
		bytecode.TAnd, bytecode.TOr, bytecode.TXor, bytecode.TShl, bytecode.TShr, bytecode.TUShr:
		raw(ir.RawIAdd+ir.RawOp(op-bytecode.TAdd), a, slot(b), slot(c))
	case bytecode.TAddI:
		raw(ir.RawIAdd, a, slot(b), imm(w.SC()))
	case bytecode.TAndI, bytecode.TOrI, bytecode.TXorI, bytecode.TShlI, bytecode.TShrI, bytecode.TUShrI:
		raw(ir.RawIAnd+ir.RawOp(op-bytecode.TAndI), a, slot(b), imm(w.SC()))
	case bytecode.TNeg, bytecode.TNot, bytecode.TLNot, bytecode.TClz, bytecode.TAbs:
		raw(ir.RawINeg+ir.RawOp(op-bytecode.TNeg), a, slot(b), unaryNone)
	case bytecode.TMinS:
		raw(ir.RawIMinS, a, slot(b), slot(c))
	case bytecode.TMaxS:
		raw(ir.RawIMaxS, a, slot(b), slot(c))
	case bytecode.TLtS, bytecode.TLeS:
		cmp(ir.RawICmpS, ir.Lt+ir.Operator(op-bytecode.TLtS), a, slot(b), slot(c))
	case bytecode.TLtU, bytecode.TLeU:
		cmp(ir.RawICmpU, ir.Lt+ir.Operator(op-bytecode.TLtU), a, slot(b), slot(c))
	case bytecode.TEq:
		cmp(ir.RawICmpS, ir.Eq, a, slot(b), slot(c))
	case bytecode.TNe:
		cmp(ir.RawICmpS, ir.Ne, a, slot(b), slot(c))
	case bytecode.TJLtS, bytecode.TJLeS:
		br(ir.RawBrI, ir.Lt+ir.Operator(op-bytecode.TJLtS), slot(a), slot(b), true)
	case bytecode.TJLtU, bytecode.TJLeU:
		br(ir.RawBrU, ir.Lt+ir.Operator(op-bytecode.TJLtU), slot(a), slot(b), true)
	case bytecode.TJEq:
		br(ir.RawBrI, ir.Eq, slot(a), slot(b), true)
	case bytecode.TJNe:
		br(ir.RawBrI, ir.Ne, slot(a), slot(b), true)
	case bytecode.TJLtSI, bytecode.TJLeSI, bytecode.TJGtSI, bytecode.TJGeSI:
		br(ir.RawBrI, ir.Lt+ir.Operator(op-bytecode.TJLtSI), slot(a), imm(w.SB()), true)
	case bytecode.TJEqI:
		br(ir.RawBrI, ir.Eq, slot(a), imm(w.SB()), true)
	case bytecode.TJNeI:
		br(ir.RawBrI, ir.Ne, slot(a), imm(w.SB()), true)
	case bytecode.TJZ:
		br(ir.RawBrI, ir.Eq, slot(a), imm(0), true)
	case bytecode.TJNZ:
		br(ir.RawBrI, ir.Ne, slot(a), imm(0), true)
	case bytecode.TFAdd, bytecode.TFSub, bytecode.TFMul, bytecode.TFDiv, bytecode.TFMod:
		raw(ir.RawFAdd+ir.RawOp(op-bytecode.TFAdd), a, slot(b), slot(c))
	case bytecode.TFNeg:
		raw(ir.RawFNeg, a, slot(b), unaryNone)
	case bytecode.TFAbs:
		raw(ir.RawFAbs, a, slot(b), unaryNone)
	case bytecode.TFSqrt:
		raw(ir.RawFSqrt, a, slot(b), unaryNone)
	case bytecode.TFRound:
		raw(ir.RawFRound, a, slot(b), unaryNone)
	case bytecode.TFMath1:
		switch w.X() {
		case bytecode.TMathFloor:
			raw(ir.RawFFloor, a, slot(b), unaryNone)
		case bytecode.TMathCeil:
			raw(ir.RawFCeil, a, slot(b), unaryNone)
		default:
			if int(w.X()) >= len(bytecode.TMathNames) {
				return "unknown Math function"
			}
			rawK(ir.RawFMath, a, slot(b), unaryNone, uint32(w.X()))
		}
	case bytecode.TFMath2:
		if w.X() != bytecode.TMathPow && w.X() != bytecode.TMathAtan2 {
			return "unknown Math function"
		}
		rawK(ir.RawFMath, a, slot(b), slot(c), uint32(w.X()))
	case bytecode.TFMin:
		raw(ir.RawFMin, a, slot(b), slot(c))
	case bytecode.TFMax:
		raw(ir.RawFMax, a, slot(b), slot(c))
	case bytecode.TFLt, bytecode.TFLe:
		cmp(ir.RawFCmp, ir.Lt+ir.Operator(op-bytecode.TFLt), a, slot(b), slot(c))
	case bytecode.TFEq:
		cmp(ir.RawFCmp, ir.Eq, a, slot(b), slot(c))
	case bytecode.TFNe:
		cmp(ir.RawFCmp, ir.Ne, a, slot(b), slot(c))
	case bytecode.TJFLt, bytecode.TJFNLt, bytecode.TJFLe, bytecode.TJFNLe:
		k := op - bytecode.TJFLt
		br(ir.RawBrF, ir.Lt+ir.Operator(k/2), slot(a), slot(b), k%2 == 0)
	case bytecode.TJFEq:
		br(ir.RawBrF, ir.Eq, slot(a), slot(b), true)
	case bytecode.TJFNe:
		br(ir.RawBrF, ir.Eq, slot(a), slot(b), false)
	case bytecode.TI2FS:
		raw(ir.RawI2FS, a, slot(b), unaryNone)
	case bytecode.TI2FU:
		raw(ir.RawI2FU, a, slot(b), unaryNone)
	case bytecode.TF2I:
		raw(ir.RawF2I, a, slot(b), unaryNone)
	case bytecode.TLd8S, bytecode.TLd8U, bytecode.TLd16S, bytecode.TLd16U, bytecode.TLd32, bytecode.TLdF32, bytecode.TLdF64,
		bytecode.TSt8, bytecode.TSt16, bytecode.TSt32, bytecode.TStF32, bytecode.TStF64:
		return l.heap(w)
	case bytecode.TGGetI:
		// The module variable's number, in its cell.
		rawK(ir.RawCellGetI, a, unaryNone, unaryNone, w.BC())
	case bytecode.TGGetF:
		rawK(ir.RawCellGetF, a, unaryNone, unaryNone, w.BC())
	case bytecode.TGSetI:
		rawK(ir.RawCellSetI, -1, slot(a), unaryNone, w.BC())
	case bytecode.TGSetF:
		rawK(ir.RawCellSetF, -1, slot(a), unaryNone, w.BC())
	case bytecode.TTabGet:
		// The table's element at the index masked by its length: a dense
		// array of the module's functions.
		if c >= len(rc.Asm) || rc.Asm[c].Kind != bytecode.AsmUpTable || rc.Asm[c].Len <= 0 {
			return "function table"
		}
		m, k, t := l.rawTemp(ir.SlotI32), l.rawTemp(ir.SlotValue), l.rawTemp(ir.SlotValue)
		raw(ir.RawIAnd, m, slot(b), imm(int32(rc.Asm[c].Len-1)))
		raw(ir.RawIBox, k, slot(m), unaryNone)
		l.emit(ir.Instruction{Op: ir.UpvalueRead, Dest: t, Key: uint32(c)}, false)
		l.emit(ir.Instruction{Op: ir.ArrayRead, Left: slot(t), Right: slot(k), Dest: a}, false)
	default:
		return "typed operation " + rc.OpName(op)
	}
	return ""
}

// typedElems are the element types of the heap's accesses, by their
// operations (abi's ElemInt8 ...).
var typedElems = map[bytecode.ROp]int{
	bytecode.TLd8S: abi.ElemInt8, bytecode.TLd8U: abi.ElemUint8, bytecode.TLd16S: abi.ElemInt16, bytecode.TLd16U: abi.ElemUint16,
	bytecode.TLd32: abi.ElemInt32, bytecode.TLdF32: abi.ElemFloat32, bytecode.TLdF64: abi.ElemFloat64,
	bytecode.TSt8: abi.ElemInt8, bytecode.TSt16: abi.ElemInt16, bytecode.TSt32: abi.ElemInt32,
	bytecode.TStF32: abi.ElemFloat32, bytecode.TStF64: abi.ElemFloat64,
}

// heap lowers a heap access w, a load into R[A] or a store of it, at the
// byte address int32(R[B] + SC): a RawLoad or a RawStore, which native
// code makes of the heap the function's entry has (abi.HeapCell).
func (l *typedLowering) heap(w bytecode.RInstr) string {
	kind, ok := typedElems[w.Op()]
	if !ok {
		return "heap access"
	}
	in := ir.Instruction{Op: ir.Raw, Raw: ir.RawLoad, Key: uint32(kind), Dest: int(w.A()), Left: ir.Slot(int(w.B())), Right: number(float64(w.SC()))}
	switch w.Op() {
	case bytecode.TSt8, bytecode.TSt16, bytecode.TSt32, bytecode.TStF32, bytecode.TStF64:
		in.Raw, in.Dest, in.Third = ir.RawStore, -1, ir.Slot(int(w.A()))
	}
	l.emit(in, false)
	return ""
}
