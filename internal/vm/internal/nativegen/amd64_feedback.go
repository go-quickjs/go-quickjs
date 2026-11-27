package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// feedbackAMD64 is the feedback variant's own handlers on amd64
// (feedback.go): each counts, in the counts the top frame's cc has, or
// looks at its BigInts (feedbackBigAMD64), and goes on in plain's handler
// of its operation through the table, R13. At
// a handler's start RAX is the instruction and RCX its operation, and
// what plain's handler reads besides is pinned; the count uses RCX, R8
// and R9, which plain's handlers set before they read them.
func feedbackAMD64(v *variant) (*block, error) {
	a := &Asm{}
	counts, tick, enters := feedbackCounts(), feedbackTick(), feedbackEnters()
	at := map[bytecode.ROp]int{}
	labels := map[bytecode.ROp]Label{}
	for _, op := range handlerOps {
		if !v.has(op) {
			continue
		}
		a.Align(16)
		l := a.NewLabel()
		a.Bind(l)
		labels[op] = l
		if unfused, ok := feedbackUnfused[op]; ok {
			a.JmpMem(R13, 8*int32(unfused))
			continue
		}
		if jumps, bc := feedbackJumps(op); jumps {
			feedbackEdgeAMD64(a, bc)
		}
		if latticeRecords(op) {
			// The lattice, then, for an operator that marks BigInts, the
			// marks, of the words the lattice loaded: numbers go on in
			// plain's handler at once.
			var x *latticeExitsAMD64
			big, ab := feedbackMarksBig(op)
			if big {
				x = &latticeExitsAMD64{none: a.NewLabel(), known: a.NewLabel(), check: a.NewLabel()}
			}
			tail := latticeAMD64(a, op, x)
			a.JmpMem(R13, 8*int32(v.slotPlain(op)))
			if big {
				feedbackBigAMD64(a, v, op, ab, x)
			}
			tail()
			continue
		}
		if big, ab := feedbackMarksBig(op); big {
			feedbackBigAMD64(a, v, op, ab, nil)
			continue
		}
		if jumps, _ := feedbackJumps(op); jumps {
			a.JmpMem(R13, 8*int32(v.slotPlain(op)))
			continue
		}
		skip := a.NewLabel()
		a.Load(R9, RDI, ncCC)
		if feedbackCountsReturn(op) {
			// counts[0]++, the function's runs.
			a.CmpMemImm8(R9, counts+8, 0)
			a.Jcc(CondE, skip)
			a.Load(R8, R9, counts)
			a.IncMem(R8, 0)
			// The budget's runs left, and the next call Go's at none.
			a.Bind(skip)
			skip = a.NewLabel()
			a.DecMem(R9, tick)
			a.Jcc(CondG, skip)
			a.MovImm(R8, 1)
			a.Store8(R9, enters, R8)
			// The lattice's last byte: the function has run to a return.
			a.Bind(skip)
			skip = a.NewLabel()
			latticeRanAMD64(a, skip)
		} else {
			// counts[1+C]++, the calls at site C.
			a.MovRR(RCX, RAX)
			a.ShiftImm(Shr, RCX, 48, true)
			a.OpImm(Add, RCX, 1, true)
			a.CmpMem(RCX, R9, counts+8, true)
			a.Jcc(CondAE, skip)
			a.Load(R8, R9, counts)
			a.LeaIdx(R8, R8, RCX, 8, 0)
			a.IncMem(R8, 0)
		}
		a.Bind(skip)
		a.JmpMem(R13, 8*int32(v.slotPlain(op)))
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

// feedbackEdgeAMD64 counts a backward jump, its offset int32(BC) where bc,
// else SC, of the work its function's budget has left for back edges
// (feedback.go, feedbackEdges), and, where none is left, ends the
// interrupt budget, R11, so that plain's handler, the jump taken, leaves
// it to Go's check. It uses RCX, R8 and R9, which plain's handlers set
// before they read them.
func feedbackEdgeAMD64(a *Asm, bc bool) {
	fwd := a.NewLabel()
	a.MovRR(RCX, RAX)
	if bc {
		a.ShiftImm(Sar, RCX, 32, true)
	} else {
		a.ShiftImm(Sar, RCX, 48, true)
	}
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondNS, fwd)
	a.Load(R9, RDI, ncCC)
	a.Load(R8, R9, feedbackEdges())
	a.Op(Add, R8, RCX, true)
	a.Store(R9, feedbackEdges(), R8)
	a.Jcc(CondG, fwd)
	a.MovImm(R11, 1)
	a.Bind(fwd)
}

// feedbackBigAMD64 is the feedback variant's handler of op, an operator
// plain's handler makes of two BigInts held in their Values (feedback.go,
// feedbackBigOps), of R[A] and R[B] where ab, else of R[B] and R[C]: where
// both are such BigInts and the site's marks (regCode's bigSeen) have none
// of its register pc, the exit stub, through the table, for Go to make
// the operation and its mark; plain's handler of op otherwise. Where x is
// not nil, the lattice's code (latticeAMD64) comes in at its labels: at
// x.none the operands' words are loaded into RDX and R8, as the lattice
// loads them; at x.check, they are, and are looked at; at x.known, both
// are such BigInts. It uses RSI, RDX, RCX, R8 and R9, which plain's
// handlers and the exit stub set before they read them.
func feedbackBigAMD64(a *Asm, v *variant, op bytecode.ROp, ab bool, x *latticeExitsAMD64) {
	plainOp, exit := a.NewLabel(), a.NewLabel()
	seen, kinds := feedbackBigSeen(), feedbackBigKinds()
	// The operands' registers' offsets in the window, as decA, decB and
	// decC make them: their 16-bit indexes times 16.
	shifts := [2]uint8{28, 44}
	if ab {
		shifts = [2]uint8{12, 28}
	}
	if x != nil {
		a.Bind(x.none)
	}
	for i, sh := range shifts {
		a.MovRR(RSI, RAX)
		a.ShiftImm(Shr, RSI, sh, true)
		a.OpImm(And, RSI, 0xFFFF0, false)
		a.LoadIdx(latticeWordsAMD64(2)[i], R12, RSI, 1, 0)
	}
	if x != nil {
		a.Bind(x.check)
	}
	a.MovImm(RCX, shortBigBase)
	for _, w := range latticeWordsAMD64(2) {
		a.Op(Cmp, w, RCX, true)
		a.Jcc(CondB, plainOp)
	}
	if x != nil {
		a.Bind(x.known)
	}
	// Two BigInts held in their Values: the site's marks, none yet.
	a.Load(R9, RDI, ncRC)
	a.Load(R9, R9, seen)
	a.Op(Test, R9, R9, true)
	a.Jcc(CondE, exit)
	// The register pc, the word of its bit, and the bit.
	a.MovRR(RCX, RBX)
	a.Load(RDX, RDI, ncCodeBase)
	a.Op(Sub, RCX, RDX, true)
	a.ShiftImm(Shr, RCX, 3, true)
	a.MovRR(RSI, RCX)
	a.ShiftImm(Shr, RSI, 5, true)
	a.OpImm(And, RCX, 31, false)
	for i, k := range kinds {
		dst := R8
		if i > 0 {
			dst = RDX
		}
		a.Load(dst, R9, k)
		a.LeaIdx(dst, dst, RSI, 4, 0)
		a.LoadU32(dst, dst, 0)
		if i > 0 {
			a.Op(Or, R8, RDX, false)
		}
	}
	a.Bt(R8, RCX)
	a.Jcc(CondB, plainOp)
	a.Bind(exit)
	a.JmpMem(R13, 8*slotExit)
	a.Bind(plainOp)
	a.JmpMem(R13, 8*int32(v.slotPlain(op)))
}
