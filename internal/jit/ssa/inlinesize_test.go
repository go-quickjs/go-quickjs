package ssa

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// inlineSizeProgram is a straight program of n instructions, the last a
// return: register code's (Origin, Regs) where reg says, a slot program
// otherwise.
func inlineSizeProgram(n int, reg bool) *ir.Program {
	p := &ir.Program{Locals: 3}
	for i := range n - 1 {
		p.Code = append(p.Code, ir.Instruction{Op: ir.Binary, Operator: ir.Add, Left: ir.Slot(0), Right: ir.Slot(1), Dest: 2})
		p.Maps = append(p.Maps, ir.StateMap{PC: uint32(i)})
	}
	p.Code = append(p.Code, ir.Instruction{Op: ir.Return, Left: ir.Slot(2)})
	p.Maps = append(p.Maps, ir.StateMap{PC: uint32(n - 1)})
	if reg {
		p.Regs = 3
		for i := range n {
			p.Origin = append(p.Origin, uint32(i))
		}
	}
	return p
}

// Inlining's bounds are in the unit of an instruction of the stack
// bytecode they were measured in, which a slot program's instructions
// count one each; a register program's count 20/17 each (inlineSize): the
// largest callee inlined is 48 slot instructions or 41 register ones, the
// largest small one 10 or 8.
func TestInlineSizeRegister(t *testing.T) {
	for _, c := range []struct {
		n    int
		reg  bool
		size int
		ok   bool
	}{
		{48, false, 48, true}, {49, false, 49, false},
		{41, true, 48, true}, {42, true, 49, false},
		{8, true, 9, true}, {9, true, 11, true},
	} {
		p := inlineSizeProgram(c.n, c.reg)
		if err := p.Validate(); err != nil {
			t.Fatalf("%d instructions, register %v: %v", c.n, c.reg, err)
		}
		if got := inlineSize(p); got != c.size {
			t.Errorf("%d instructions, register %v: size %d, want %d", c.n, c.reg, got, c.size)
		}
		if _, ok := InlineCalls(p); ok != c.ok {
			t.Errorf("%d instructions, register %v: inlinable %v, want %v", c.n, c.reg, ok, c.ok)
		}
	}
	// Past a function's budget, a small callee only: 8 register
	// instructions are, 9 are not.
	if small, big := inlineSize(inlineSizeProgram(8, true)), inlineSize(inlineSizeProgram(9, true)); !inlineFits(maxInlineTotal, small) || inlineFits(maxInlineTotal, big) {
		t.Errorf("past the budget: 8 register instructions fit %v, 9 %v; want true, false", inlineFits(maxInlineTotal, small), inlineFits(maxInlineTotal, big))
	}
}
