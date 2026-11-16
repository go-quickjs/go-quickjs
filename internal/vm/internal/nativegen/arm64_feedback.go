package nativegen

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// feedbackARM64 is the feedback variant's own handlers on arm64, as
// feedbackAMD64 makes amd64's: each counts, in the counts the top frame's
// cc has, and goes on in plain's handler of its operation, through the
// table, R21, by a load into R16 and a jump through it. At a handler's
// start rW is the instruction and R16 the handler's address; the count
// uses rC, rD, rS, r8 and r9, which plain's handlers set before they read
// them.
func feedbackARM64(v *variant) (*block, error) {
	a := &Asm{}
	counts := feedbackCounts()
	at := map[bytecode.ROp]int{}
	labels := map[bytecode.ROp]Label{}
	for _, op := range handlerOps {
		if !v.has(op) {
			continue
		}
		l := a.NewLabel()
		a.Bind(l)
		labels[op] = l
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
