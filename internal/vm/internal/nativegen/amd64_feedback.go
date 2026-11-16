package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// feedbackAMD64 is the feedback variant's own handlers on amd64
// (feedback.go): each counts, in the counts the top frame's cc has, and
// goes on in plain's handler of its operation through the table, R13. At
// a handler's start RAX is the instruction and RCX its operation, and
// what plain's handler reads besides is pinned; the count uses RCX, R8
// and R9, which plain's handlers set before they read them.
func feedbackAMD64(v *variant) (*block, error) {
	a := &Asm{}
	counts := feedbackCounts()
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
		skip := a.NewLabel()
		a.Load(R9, RDI, ncCC)
		if feedbackCountsReturn(op) {
			// counts[0]++, the function's runs.
			a.CmpMemImm8(R9, counts+8, 0)
			a.Jcc(CondE, skip)
			a.Load(R8, R9, counts)
			a.IncMem(R8, 0)
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
