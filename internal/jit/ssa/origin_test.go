package ssa

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// TestBuildRegisterOrigins builds register code's programs (ir.Program's
// Origin) and keeps the atomicity rule as the build makes it: add_prop's
// expansion builds -- its read commits nothing where no accessor is
// called, its Generic addition and store leave only before anything has
// committed -- while a store that commits, followed by a Generic store the
// builder makes with an exit, is refused.
func TestBuildRegisterOrigins(t *testing.T) {
	addProp := &ir.Program{Locals: 4, Regs: 2, Origin: []uint32{0, 0, 0, 1},
		Code: []ir.Instruction{
			{Op: ir.PropertyRead, Left: ir.Slot(0), Dest: 2, Key: 7},
			{Op: ir.Binary, Operator: ir.Add, Left: ir.Slot(2), Right: ir.Slot(1), Dest: 3, Generic: true},
			{Op: ir.PropertyWrite, Left: ir.Slot(0), Right: ir.Slot(3), Key: 7, Generic: true},
			{Op: ir.Return, Left: ir.Slot(1)},
		},
		Maps: []ir.StateMap{{PC: 0}, {PC: 0}, {PC: 0}, {PC: 1}}}
	if _, err := BuildWith(addProp, nil); err != nil {
		t.Fatalf("add_prop's expansion: %v", err)
	}
	committed := &ir.Program{Locals: 3, Regs: 2, Origin: []uint32{0, 0, 1},
		Code: []ir.Instruction{
			{Op: ir.ArrayWrite, Left: ir.Slot(0), Right: ir.Literal(ir.Float(0)), Third: ir.Slot(1)},
			{Op: ir.PropertyWrite, Left: ir.Slot(0), Right: ir.Slot(1), Key: 7, Generic: true},
			{Op: ir.Return, Left: ir.Slot(1)},
		},
		Maps: []ir.StateMap{{PC: 0}, {PC: 0}, {PC: 1}}}
	if err := committed.Validate(); err != nil {
		t.Fatalf("the IR keeps the rule: %v", err)
	}
	_, err := BuildWith(committed, nil)
	if err == nil || !strings.Contains(err.Error(), "register pc 0: an exit after the instruction committed") {
		t.Fatalf("a store's exit after a commit: got %v", err)
	}
}
