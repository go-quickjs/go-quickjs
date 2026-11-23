package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// The handlers of a product by an immediate, mul_imm, and of the int32
// results of a sum or a difference, (x + y) | 0, (x - y) | 0 and
// (x + k) | 0 in one instruction (docs/register-interpreter.md,
// "Loop-kernel instructions"): in a file of their own, sorting after
// reg_table_fast.go, so that what they add moves no handler before them.
func init() {
	regTable[bytecode.RMulI], regSlowTable[bytecode.RMulI] = rhMulI, rsMulI
	regTable[bytecode.RAddI32], regSlowTable[bytecode.RAddI32] = rhAddI32, rsAddI32
	regTable[bytecode.RSubI32], regSlowTable[bytecode.RSubI32] = rhSubI32, rsSubI32
	regTable[bytecode.RAddII32], regSlowTable[bytecode.RAddII32] = rhAddII32, rsAddII32
}

func rhMulI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			p.num = regNum(a.Number() * float64(w.SC())).num
			return pc + 1
		}
	}
	return ^pc
}

// rsMulI is mul_imm's slow path, mul's of a Number: a string's number, a
// BigInt's TypeError, valueOf, as mul makes them.
func rsMulI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		regs[w.A()] = regNum(a.Number() * float64(w.SC()))
		return pc + 1
	}
	st.at(pc)
	v, err := st.r.regBinImm(bytecode.OpMul, regs[w.B()], w.SC())
	return st.put(regs, w, v, err)
}

// The int32 forms' fast paths: two numbers whose sum or difference is
// below 2^63 in magnitude, whose ToInt32 is the low 32 bits of its
// truncation, into a register that holds no pointer. A NaN fails both
// compares.

func rhAddI32(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && b.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			if s := a.Number() + b.Number(); s > -0x1p63 && s < 0x1p63 {
				p.num = float64(int32(int64(s)))
				return pc + 1
			}
		}
	}
	return ^pc
}

func rhSubI32(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && b.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			if s := a.Number() - b.Number(); s > -0x1p63 && s < 0x1p63 {
				p.num = float64(int32(int64(s)))
				return pc + 1
			}
		}
	}
	return ^pc
}

func rhAddII32(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		if p := &regs[w.A()]; p.ref == nil {
			if s := a.Number() + float64(w.SC()); s > -0x1p63 && s < 0x1p63 {
				p.num = float64(int32(int64(s)))
				return pc + 1
			}
		}
	}
	return ^pc
}

// The int32 forms' slow paths: of two numbers, ToInt32 of any sum, into
// any register; otherwise the + or the - as add, sub, add_imm or sub_imm
// make it, placed at the instruction's position, and then the | 0 of its
// result (regOrZero).

func rsAddI32(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && b.IsNumber() {
		regs[w.A()] = Int32(jsnum.ToInt32(a.Number() + b.Number()))
		return pc + 1
	}
	st.at(pc)
	v, err := st.addSeen(pc, regs[w.B()], regs[w.C()])
	v, err = regOrZero(st, pc, v, err)
	return st.put(regs, w, v, err)
}

func rsSubI32(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a, b := regs[w.B()], regs[w.C()]; a.IsNumber() && b.IsNumber() {
		regs[w.A()] = Int32(jsnum.ToInt32(a.Number() - b.Number()))
		return pc + 1
	}
	st.at(pc)
	v, err := st.r.arith(bytecode.OpSub, regs[w.B()], regs[w.C()])
	v, err = regOrZero(st, pc, v, err)
	return st.put(regs, w, v, err)
}

// rsAddII32 is add_imm_i32's slow path: of x - K, SC -K, sub_imm's
// subtraction, as "5" - 1 is 4 where "5" + -1 is "5-1".
func rsAddII32(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if a := regs[w.B()]; a.IsNumber() {
		regs[w.A()] = Int32(jsnum.ToInt32(a.Number() + float64(w.SC())))
		return pc + 1
	}
	st.at(pc)
	var v Value
	var err error
	if w.X() == bytecode.RAddII32Sub {
		v, err = st.r.regBinImm(bytecode.OpSub, regs[w.B()], -w.SC())
	} else {
		v, err = st.addImmSeen(pc, bytecode.OpAdd, regs[w.B()], w.SC())
	}
	v, err = regOrZero(st, pc, v, err)
	return st.put(regs, w, v, err)
}

// regOrZero is v | 0, v the result of the operator of the int32 form at
// pc, or err, that operator's: a Number's ToInt32; a string's number's,
// whose conversion runs nothing; and a BigInt's TypeError, the |'s,
// placed at the position of the pc Aux[pc]. Its caller has kept the pc
// (st.at) and stores what it gives (st.put), so that the throws-keep-pc
// rule (TestRegThrowsKeepPC) sees both in each slow function.
func regOrZero(st *regState, pc int, v Value, err error) (Value, error) {
	if err != nil {
		return v, err
	}
	if v.IsNumber() {
		return Int32(jsnum.ToInt32(v.Number())), nil
	}
	st.f.pc = st.c.rc.Aux[pc] + 1
	return st.r.regBinImm(bytecode.OpBitOr, v, 0)
}
