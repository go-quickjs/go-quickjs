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
		if unfused, ok := feedbackUnfused[op]; ok {
			a.Load(rJ, rTab, 8*int32(unfused))
			a.Br(rJ)
			continue
		}
		if jumps, bc := feedbackJumps(op); jumps {
			feedbackEdgeARM64(a, bc)
		}
		if latticeRecords(op) {
			// The lattice, then, for an operator that marks BigInts, the
			// marks, of the words the lattice loaded: numbers go on in
			// plain's handler at once.
			var x *latticeExitsARM64
			big, ab := feedbackMarksBig(op)
			if big {
				x = &latticeExitsARM64{none: a.NewLabel(), known: a.NewLabel(), check: a.NewLabel()}
			}
			tail := latticeARM64(a, op, x)
			a.Load(rJ, rTab, 8*int32(v.slotPlain(op)))
			a.Br(rJ)
			if big {
				feedbackBigARM64(a, v, op, ab, x)
			}
			tail()
			continue
		}
		if big, ab := feedbackMarksBig(op); big {
			feedbackBigARM64(a, v, op, ab, nil)
			continue
		}
		if jumps, _ := feedbackJumps(op); jumps {
			a.Load(rJ, rTab, 8*int32(v.slotPlain(op)))
			a.Br(rJ)
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
			// The lattice's last byte: the function has run to a return.
			latticeRanARM64(a)
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

// feedbackEdgeARM64 counts a backward jump, as amd64's feedbackEdgeAMD64:
// its offset int32(BC) where bc, else SC, of the work its function's
// budget has left for back edges, and, where none is left, the interrupt
// budget, rBud, ended. It uses rC, r8 and r9, which plain's handlers set
// before they read them.
func feedbackEdgeARM64(a *Asm, bc bool) {
	fwd := a.NewLabel()
	if bc {
		a.Sbfx(rC, rW, 32, 32)
	} else {
		a.Sbfx(rC, rW, 48, 16)
	}
	a.CmpImm(rC, 0, true)
	a.BCond(GE, fwd)
	a.Load(r9, rCtx, ncCC)
	a.Load(r8, r9, feedbackEdges())
	a.Op(Add, r8, r8, rC, true)
	a.Store(r9, feedbackEdges(), r8)
	a.CmpImm(r8, 0, true)
	a.BCond(GT, fwd)
	a.MovImm(rBud, 1)
	a.Bind(fwd)
}

// feedbackBigARM64 is feedbackBigAMD64's handler on arm64: where op's two
// operands, R[A] and R[B] where ab, else R[B] and R[C], are BigInts held
// in their Values and the site's marks have none of its register pc, the
// exit stub, through the table; plain's handler of op otherwise. Where x
// is not nil, the lattice's code (latticeARM64) comes in at its labels, as
// at feedbackBigAMD64's, the words in rK and r8. It uses rC, rD, rS, rK,
// r8, r9 and rJ, which plain's handlers and the exit stub set before they
// read them.
func feedbackBigARM64(a *Asm, v *variant, op bytecode.ROp, ab bool, x *latticeExitsARM64) {
	plainOp, exit := a.NewLabel(), a.NewLabel()
	seen, kinds := feedbackBigSeen(), feedbackBigKinds()
	// The operands' 16-bit register indexes' places in the instruction,
	// as regA, regB and regC read them.
	lsbs := [2]uint8{32, 48}
	if ab {
		lsbs = [2]uint8{16, 32}
	}
	words := latticeWordsARM64(2)
	if x != nil {
		a.Bind(x.none)
	}
	for i, lsb := range lsbs {
		a.Ubfx(rS, rW, lsb, 16)
		a.AddLsl(rS, rRegs, rS, 4)
		a.Load(words[i], rS, 0)
	}
	if x != nil {
		a.Bind(x.check)
	}
	a.MovImm(rC, shortBigBase)
	for _, w := range words {
		a.Cmp(w, rC, true)
		a.BCond(LO, plainOp)
	}
	if x != nil {
		a.Bind(x.known)
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
