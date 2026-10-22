package ssa

import (
	"errors"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// prologueProgram is a parameter list's prologue of two parameters, an
// instruction Go goes on after in it (Host), then a return; loop marks pc
// 2, inside the prologue, a loop header (ir.Program's Entries).
func prologueProgram(loop bool) *ir.Program {
	p := &ir.Program{Locals: 2, StackSize: 1, Code: []ir.Instruction{
		{Op: ir.ParamsDead, Key: 2},
		{Op: ir.Host},
		{Op: ir.ParamInit, Dest: 0, Key: 0},
		{Op: ir.ParamDefault, Dest: 2, Key: 1},
		{Op: ir.Return, Left: ir.Slot(2)},
	}, Maps: []ir.StateMap{{PC: 0}, {PC: 1}, {PC: 2}, {PC: 3}, {PC: 4, Depth: 1}}}
	if loop {
		p.Entries = []int{2}
	}
	return p
}

// A parameter list's prologue binds its parameters from what their slots
// had at the function's start (ParamsDead), so native code is entered
// nowhere inside it: not after an instruction Go goes on after, where it
// would be entered elsewhere, and a loop inside it is refused.
func TestPrologueEnteredAtStartOnly(t *testing.T) {
	p := prologueProgram(false)
	if err := p.Validate(); err != nil {
		t.Fatalf("the test's program is invalid: %v", err)
	}
	f, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(f); err != nil {
		t.Fatalf("%v\n%s", err, f)
	}
	for _, e := range f.Entries {
		if e.PC != 0 {
			t.Errorf("an entry at pc %d, inside the prologue", e.PC)
		}
	}
	if _, err := Build(prologueProgram(true)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("a loop in the prologue built: %v", err)
	}
}
