package regcomp

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// stepPartners are the compare-and-branches a step-and-branch is made
// for, each its fused operation (bytecode.ROp.StepPartner's inverse).
var stepPartners = [256]bytecode.ROp{
	bytecode.RJLt:  bytecode.RStepJLt,
	bytecode.RJLe:  bytecode.RStepJLe,
	bytecode.RJLtI: bytecode.RStepJLtI,
	bytecode.RJGtI: bytecode.RStepJGtI,
	bytecode.RJGeI: bytecode.RStepJGeI,
	bytecode.RJLeU: bytecode.RStepJLeU,
}

// fuseSteps makes each step of a register in place followed by a
// compare-and-branch of it -- inc, dec, add_imm or sub_imm of r into r,
// then jlt r, ... and its kin -- a step-and-branch, which does both in
// one dispatch, the compare-and-branch kept after it as its partner
// (docs/register-interpreter.md, "Loop-kernel instructions"). It runs
// once every jump is patched, over the code's adjacent pairs, loops or
// not. A label needs no check: a jump to the step does both, and one to
// the compare -- a for loop's entry, which must not step -- the compare
// alone, as before. The fused instruction keeps the step's position.
//
// Left alone: a step into another register; an add_imm or a sub_imm of
// a literal past what X's 7 bits hold, or of one of 0 or below, whose
// sign would say the other operator (bytecode.RStepAdd); a compare of
// the counter with itself; an offset whose fused form, one more, would
// not fit 16 bits; and a function whose jumps are far, whose
// compare-and-branches each jump over a jmp.
func (g *gen) fuseSteps() {
	if g.farJumps {
		return
	}
	code := g.code
	for p := 0; p+2 < len(code); p++ {
		s, c := code[p], code[p+1]
		fused := stepPartners[c.Op()]
		if fused == 0 || s.A() != s.B() || c.A() != s.A() || c.SC() >= math.MaxInt16 {
			continue
		}
		var x uint8
		switch s.Op() {
		case bytecode.RInc:
			x = 1
		case bytecode.RDec:
			x = 0x7f // -1
		case bytecode.RAddI:
			if k := s.SC(); k >= 1 && k <= 63 {
				x = bytecode.RStepAdd | uint8(k)
			}
		case bytecode.RSubI:
			if k := s.SC(); k >= 1 && k <= 64 {
				x = bytecode.RStepAdd | uint8(-k)&0x7f
			}
		}
		if x == 0 || (fused == bytecode.RStepJLt || fused == bytecode.RStepJLe) && c.B() == c.A() || g.marks[p] != 0 {
			continue
		}
		if _, ok := g.auxes[p]; ok {
			continue
		}
		code[p] = bytecode.MakeR(fused, x, s.A(), c.B(), 0).WithSC(c.SC() + 1)
	}
}
