package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// feedbackARM64 is the feedback variant's own handlers on arm64, as
// feedbackAMD64 makes amd64's: each counts, in the counts the top frame's
// cc has, or looks at its BigInts (feedbackBigARM64), and goes on in
// plain's handler of its operation, through the
// table, R21, by a load into R16 and a jump through it. At a handler's
// start rW is the instruction and R16 the handler's address; the count
// uses rC, rD, rS, r8 and r9, which plain's handlers set before they read
// them.
func feedbackARM64(v *variant) (*block, error) {
	a := &Asm{}
	counts, tick, enters := feedbackCounts(), feedbackTick(), feedbackEnters()
	at := map[bytecode.ROp]int{}
	labels := map[bytecode.ROp]Label{}
	for _, op := range handlerOps {
		if !v.has(op) {
			continue
		}
		l := a.NewLabel()
		a.Bind(l)
		labels[op] = l
		if big, ab := feedbackMarksBig(op); big {
			feedbackBigARM64(a, v, op, ab)
			continue
		}
		skip := a.NewLabel()
		a.Load(r9, rCtx, ncCC)
		a.Load(rD, r9, counts+8)
		a.Load(r8, r9, counts)
		if feedbackCountsReturn(op) {
			// counts[0]++, the function's runs.
			a.Cbz(rD, skip, true)
		} else {
			// counts[1+C]++, the calls at site C.
			a.ShiftImm(Lsr, rC, rW, 48, true)
			a.AddImm(rC, rC, 1, true)
			a.Cmp(rC, rD, true)
			a.BCond(HS, skip)
			a.AddLsl(r8, r8, rC, 3)
		}
		a.Load(rS, r8, 0)
		a.AddImm(rS, rS, 1, true)
		a.Store(r8, 0, rS)
		a.Bind(skip)
		if feedbackCountsReturn(op) {
			// The budget's runs left, and the next call Go's at none, as
			// amd64's.
			left := a.NewLabel()
			a.Load(rS, r9, tick)
			a.SubsImm(rS, rS, 1, true)
			a.Store(r9, tick, rS)
			a.BCond(GT, left)
			a.MovImm(rD, 1)
			a.Store8(r9, enters, rD)
			a.Bind(left)
		}
		a.Load(rJ, rTab, 8*int32(v.slotPlain(op)))
		a.Br(rJ)
	}
	code, err := a.Finish()
	if err != nil {
		return nil, err
	}
	for op, l := range labels {
		at[op] = a.Offset(l)
	}
	return feedbackBlock(v, code, at), nil
}

// feedbackBigARM64 is feedbackBigAMD64's handler on arm64: where op's two
// operands, R[A] and R[B] where ab, else R[B] and R[C], are BigInts held
// in their Values and the site's marks have none of its register pc, the
// exit stub, through the table; plain's handler of op otherwise. It uses
// rC, rD, rS, r8, r9 and rJ, which plain's handlers and the exit stub set
// before they read them.
func feedbackBigARM64(a *Asm, v *variant, op bytecode.ROp, ab bool) {
	plainOp, exit := a.NewLabel(), a.NewLabel()
	seen, kinds := feedbackBigSeen(), feedbackBigKinds()
	// The operands' 16-bit register indexes' places in the instruction,
	// as regA, regB and regC read them.
	lsbs := [2]uint8{32, 48}
	if ab {
		lsbs = [2]uint8{16, 32}
	}
	a.MovImm(rC, shortBigBase)
	for _, lsb := range lsbs {
		a.Ubfx(rS, rW, lsb, 16)
		a.AddLsl(rS, rRegs, rS, 4)
		a.Load(r8, rS, 0)
		a.Cmp(r8, rC, true)
		a.BCond(LO, plainOp)
	}
	// Two BigInts held in their Values: the site's marks, none yet.
	a.Load(r9, rCtx, ncRC)
	a.Load(r9, r9, seen)
	a.Cbz(r9, exit, true)
	// The register pc, the word of its bit, and the bit.
	a.Load(rD, rCtx, ncCodeBase)
	a.Op(Sub, rC, rIP, rD, true)
	a.ShiftImm(Lsr, rC, rC, 3, true)
	a.ShiftImm(Lsr, rS, rC, 5, true)
	a.AndImm(rC, rC, 31, true)
	for i, k := range kinds {
		dst := r8
		if i > 0 {
			dst = rD
		}
		a.Load(dst, r9, k)
		a.AddLsl(dst, dst, rS, 2)
		a.LoadU32(dst, dst, 0)
		if i > 0 {
			a.Op(Orr, r8, r8, rD, true)
		}
	}
	a.ShiftReg(Lsr, r8, r8, rC, true)
	a.TstImm(r8, 1, true)
	a.BCond(NE, plainOp)
	a.Bind(exit)
	a.Load(rJ, rTab, 8*slotExit)
	a.Br(rJ)
	a.Bind(plainOp)
	a.Load(rJ, rTab, 8*int32(v.slotPlain(op)))
	a.Br(rJ)
}
