package ir

import (
	"errors"
	"strings"
	"testing"
)

// regFixture is register code's program of two registers and two scratch
// slots: register pc 0 is r0.x += r1 as three instructions -- the read
// into scratch 2, the addition into scratch 3, the store -- and pc 1
// returns r0.
func regFixture() *Program {
	return &Program{Locals: 4, Regs: 2, Origin: []uint32{0, 0, 0, 1},
		Code: []Instruction{
			{Op: PropertyRead, Left: Slot(0), Dest: 2, Key: 7},
			{Op: Binary, Operator: Add, Left: Slot(2), Right: Slot(1), Dest: 3, Generic: true},
			{Op: PropertyWrite, Left: Slot(0), Right: Slot(3), Key: 7, Generic: true},
			{Op: Return, Left: Slot(0)},
		},
		Maps: []StateMap{{PC: 0}, {PC: 0}, {PC: 0}, {PC: 1}}}
}

// TestValidateAtomicity checks the atomicity rule of register code's
// expansions (Program's Origin): once an instruction of an origin may have
// committed -- a property read that may call a getter, a register written,
// a store -- none after it in the origin may leave, which would run the
// register instruction again from its start; and an origin's shape.
func TestValidateAtomicity(t *testing.T) {
	if err := regFixture().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want string
		edit       func(*Program)
	}{
		// add_prop's addition left to deoptimize after the read, which
		// may have run a getter: the getter would run twice.
		{"exit-after-getter", "committed", func(p *Program) { p.Code[1].Generic = false }},
		{"store-after-getter", "committed", func(p *Program) { p.Code[2].Generic = false }},
		// A register written first: get_index_pre's step committed before
		// its read's guard.
		{"exit-after-register", "committed", func(p *Program) {
			p.Code[0] = Instruction{Op: Binary, Operator: Add, Left: Slot(1), Right: Literal(Float(1)), Dest: 1}
			p.Code[1] = Instruction{Op: ArrayRead, Left: Slot(0), Right: Slot(1), Dest: 3}
			p.Code[2] = Instruction{Op: Copy, Left: Slot(3), Dest: 0}
		}},
		// A dead zone's check after a store: a Check always leaves.
		{"check-after-store", "committed", func(p *Program) {
			p.Code[2].Generic = false
			p.Code[1] = Instruction{Op: PropertyWrite, Left: Slot(0), Right: Slot(1), Key: 7}
			p.Code[2] = Instruction{Op: Copy, Left: Slot(1), Dest: 2, Check: true, CheckSlot: 1}
		}},
		// A call made natively is an effect like any other.
		{"exit-after-host", "host operation", func(p *Program) { p.Code[0] = Instruction{Op: Host} }},
		{"generic-return", "generic", func(p *Program) { p.Code[3].Generic = true }},
		{"generic-check", "generic", func(p *Program) { p.Code[1].Check, p.Code[1].CheckSlot = true, 1 }},
		{"into-expansion", "branch target", func(p *Program) {
			p.Code[3] = Instruction{Op: Jump, Target: 1}
		}},
		{"entry-in-expansion", "entry", func(p *Program) { p.Entries = []int{2} }},
		{"origin-order", "origin", func(p *Program) { p.Origin[3], p.Maps[3].PC = 0, 0; p.Origin[2], p.Maps[2].PC = 1, 1 }},
		{"origin-state", "state map", func(p *Program) { p.Maps[1].PC = 1 }},
		{"origin-depth", "origin", func(p *Program) { p.Maps[1].Depth = -1 }},
		{"origin-length", "register layout", func(p *Program) { p.Origin = p.Origin[:3] }},
		{"registers", "register layout", func(p *Program) { p.Regs = 5 }},
		{"operand-stack", "register layout", func(p *Program) { p.StackSize = 1 }},
		{"stack-operation", "stack operation", func(p *Program) {
			p.Code[3] = Instruction{Op: Insert2, Dest: 0}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := regFixture()
			tc.edit(p)
			err := p.Validate()
			if err == nil {
				t.Fatal("accepted a program that breaks the atomicity rule or its layout")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error about %q", err, tc.want)
			}
		})
	}
	// Generic is an expansion's.
	stack := &Program{Locals: 1, Code: []Instruction{{Op: Binary, Operator: Add, Left: Slot(0), Right: Slot(0), Dest: 0, Generic: true}, {Op: Return, Left: Slot(0)}},
		Maps: []StateMap{{PC: 0}, {PC: 1}}}
	if err := stack.Validate(); err == nil {
		t.Fatal("accepted a generic instruction outside an expansion")
	}
}

// TestEvaluateOrigins runs the expansion: an exit before anything committed
// is at the register pc, the instruction to run again; a Generic one whose
// check fails after a commit is Go's (ErrGeneric).
func TestEvaluateOrigins(t *testing.T) {
	p := &Program{Locals: 4, Regs: 2, Origin: []uint32{0, 0, 1, 1, 2},
		Code: []Instruction{
			// pc 0: r1 = (r1 + 1) in scratch, then into r1.
			{Op: Binary, Operator: Add, Left: Slot(1), Right: Literal(Float(1)), Dest: 2},
			{Op: Copy, Left: Slot(2), Dest: 1},
			// pc 1: r0 = r0 * r1, then r1 - 1 Generic.
			{Op: Binary, Operator: Mul, Left: Slot(0), Right: Slot(1), Dest: 0},
			{Op: Binary, Operator: Sub, Left: Slot(1), Right: Literal(Float(1)), Dest: 3, Generic: true},
			{Op: Return, Left: Slot(0)},
		},
		Maps: []StateMap{{PC: 0}, {PC: 0}, {PC: 1}, {PC: 1}, {PC: 2}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	slots := []Value{Float(3), Float(4), {}, {}}
	exit, err := p.Evaluate(slots, 0, 100)
	if err != nil || exit.Kind != Returned || exit.Value != Float(15) {
		t.Fatalf("got %+v, %v; want 15 returned", exit, err)
	}
	// Not a number: the step leaves before it commits, at register pc 0.
	slots = []Value{Float(3), {Kind: Null}, {}, {}}
	exit, err = p.Evaluate(slots, 0, 100)
	if err != nil || exit.Kind != GuardExit || exit.State.PC != 0 || slots[1].Kind != Null {
		t.Fatalf("got %+v, %v; want a guard's exit at pc 0, r1 as it was", exit, err)
	}
	// r0 not a number: the multiplication leaves, the first of pc 1's.
	slots = []Value{{Kind: Boolean}, Float(1), {}, {}}
	exit, err = p.Evaluate(slots, 0, 100)
	if err != nil || exit.Kind != GuardExit || exit.State.PC != 1 || slots[1] != Float(2) {
		t.Fatalf("got %+v, %v; want a guard's exit at pc 1, r1 stepped", exit, err)
	}
	// Entered at pc 1, the subtraction's check fails after r0 is written:
	// Go's, which the evaluator is not.
	slots = []Value{Float(2), {Kind: Undefined}, {}, {}}
	slots[1] = Float(5)
	p.Code[3].Left = Literal(Value{Kind: Null})
	if _, err := p.Evaluate(slots, p.At(1), 100); !errors.Is(err, ErrGeneric) {
		t.Fatalf("got %v, want ErrGeneric", err)
	}
	// Entered or budgeted only at an origin's start.
	if _, err := p.Evaluate(slots, 1, 100); !errors.Is(err, ErrState) {
		t.Fatalf("entered inside an expansion: %v", err)
	}
	exit, err = p.Evaluate([]Value{Float(1), Float(1), {}, {}}, 0, 1)
	if err != nil || exit.Kind != BudgetExit || exit.State.PC != 1 {
		t.Fatalf("got %+v, %v; want the budget's exit at pc 1", exit, err)
	}
	if p.At(2) != 4 || p.At(3) != -1 {
		t.Fatalf("At: %d %d", p.At(2), p.At(3))
	}
}
