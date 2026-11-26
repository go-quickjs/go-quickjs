package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// The step-and-branches' handlers (docs/register-interpreter.md,
// "Loop-kernel instructions"): a step of the counter R[A] and the
// compare-and-branch after it, its partner, in one dispatch. Each is the
// step, then the instruction at pc+1: the step committed, a jump to
// pc+1+SC where the partner's comparison holds, else on to pc+2, past
// it. Whatever the fast path does not make goes on at the partner, which
// stays in the code, so that nothing runs twice: a step of what is not a
// number, or of a register that holds a pointer, is rsStep's, which
// makes it and goes on at pc+1; a bound that is not a number, and a
// backward jump that finds the interrupt budget spent, go on at pc+1
// once the step is made, where the partner compares and checks the
// interrupt. In a file of their own, sorting after reg_table_fast.go, so
// that they move no handler before them; the Go table's, not
// layout-tuned.
func init() {
	for _, e := range []struct {
		op   bytecode.ROp
		fast regHandler
	}{
		{bytecode.RStepJLt, rhStepJLt},
		{bytecode.RStepJLe, rhStepJLe},
		{bytecode.RStepJLtI, rhStepJLtI},
		{bytecode.RStepJGtI, rhStepJGtI},
		{bytecode.RStepJGeI, rhStepJGeI},
		{bytecode.RStepJLeU, rhStepJLeU},
	} {
		regTable[e.op], regSlowTable[e.op] = e.fast, rsStep
	}
}

// regStepTaken is a step-and-branch's jump, its comparison holding: to
// pc+1+off, where its partner jumps; a backward one counts, with the JIT,
// its function's budget (regEdgeTick), as regJump does, and the interrupt
// budget, and where that is spent goes on at the partner, pc+1, whose
// slow path makes the interrupt check (regJumpSlow) as it does unfused.
func regStepTaken(st *regState, pc int, off int32) int {
	if off < 0 {
		if jitBuilt {
			regEdgeTick(st, off)
		}
		r := st.r
		if r.backEdges <= 1 {
			return pc + 1
		}
		r.backEdges--
	}
	return pc + 1 + int(off)
}

func rhStepJLt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	p := &regs[w.A()]
	if !p.IsNumber() || p.ref != nil {
		return ^pc
	}
	n := regNum(p.num + float64(bytecode.StepK(w.X()))).num
	p.num = n
	if b := regs[w.B()]; b.IsNumber() {
		if n < b.num {
			return regStepTaken(st, pc, w.SC())
		}
		return pc + 2
	}
	return pc + 1
}

func rhStepJLe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	p := &regs[w.A()]
	if !p.IsNumber() || p.ref != nil {
		return ^pc
	}
	n := regNum(p.num + float64(bytecode.StepK(w.X()))).num
	p.num = n
	if b := regs[w.B()]; b.IsNumber() {
		if n <= b.num {
			return regStepTaken(st, pc, w.SC())
		}
		return pc + 2
	}
	return pc + 1
}

func rhStepJLtI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	p := &regs[w.A()]
	if !p.IsNumber() || p.ref != nil {
		return ^pc
	}
	n := regNum(p.num + float64(bytecode.StepK(w.X()))).num
	p.num = n
	if n < float64(w.SB()) {
		return regStepTaken(st, pc, w.SC())
	}
	return pc + 2
}

func rhStepJGtI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	p := &regs[w.A()]
	if !p.IsNumber() || p.ref != nil {
		return ^pc
	}
	n := regNum(p.num + float64(bytecode.StepK(w.X()))).num
	p.num = n
	if n > float64(w.SB()) {
		return regStepTaken(st, pc, w.SC())
	}
	return pc + 2
}

func rhStepJGeI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	p := &regs[w.A()]
	if !p.IsNumber() || p.ref != nil {
		return ^pc
	}
	n := regNum(p.num + float64(bytecode.StepK(w.X()))).num
	p.num = n
	if n >= float64(w.SB()) {
		return regStepTaken(st, pc, w.SC())
	}
	return pc + 2
}

func rhStepJLeU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	p := &regs[w.A()]
	if !p.IsNumber() || p.ref != nil {
		return ^pc
	}
	n := regNum(p.num + float64(bytecode.StepK(w.X()))).num
	p.num = n
	if b := st.cl.upvalues[w.B()].get(); b.IsNumber() {
		if n <= b.num {
			return regStepTaken(st, pc, w.SC())
		}
		return pc + 2
	}
	return pc + 1
}

// rsStep is every step-and-branch's slow function: the step alone, made
// as inc and dec make it (regStep: ToNumeric, a BigInt's 1n, valueOf),
// or as add_imm and sub_imm do where X has RStepAdd (regBinImm: a
// string's concatenation, a BigInt's TypeError) -- the operator by k's
// sign -- stored to R[A], its throws placed at the step's position, and
// then on at the partner, pc+1, which compares.
func rsStep(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a := regs[w.A()]
	k := bytecode.StepK(w.X())
	if a.IsNumber() {
		regs[w.A()] = regNum(a.Number() + float64(k))
		return pc + 1
	}
	st.at(pc)
	var v Value
	var err error
	switch {
	case w.X()&bytecode.RStepAdd == 0:
		v, err = st.r.regStep(a, float64(k))
		if st.r.jitOn() && err == nil && v.IsBigInt() {
			st.r.regBigOf(st.c, pc, a, a, v)
		}
	case k > 0:
		v, err = st.r.regBinImm(bytecode.OpAdd, a, k)
	default:
		v, err = st.r.regBinImm(bytecode.OpSub, a, -k)
	}
	return st.put(regs, w, v, err)
}
