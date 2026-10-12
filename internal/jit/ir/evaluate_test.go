package ir

import (
	"errors"
	"reflect"
	"testing"
)

func TestEvaluateEntry(t *testing.T) {
	p := &Program{Code: []Instruction{{Op: Return, Left: Literal(Float(1))}, {Op: Nop}}, Maps: []StateMap{{PC: 0}, {PC: 1, Depth: -1}}}
	for _, tc := range []struct {
		p     *Program
		slots []Value
		pc    int
	}{
		{nil, nil, 0}, {p, nil, -1}, {p, nil, 1}, {p, nil, 2}, {p, []Value{{}}, 0},
	} {
		if _, err := tc.p.Evaluate(tc.slots, tc.pc, 1); !errors.Is(err, ErrState) {
			t.Errorf("entry pc %d: %v", tc.pc, err)
		}
	}
	exit, err := p.Evaluate(nil, 0, 0)
	if err != nil || exit.Kind != BudgetExit || exit.Steps != 0 || exit.State.PC != 0 {
		t.Fatalf("zero budget = %+v, %v", exit, err)
	}
	loop := &Program{Code: []Instruction{{Op: Jump, Target: 0}}, Maps: []StateMap{{PC: 0}}}
	exit, err = loop.Evaluate(nil, 0, 19)
	if err != nil || exit.Kind != BudgetExit || exit.Steps != 19 || exit.State.PC != 0 {
		t.Fatalf("bounded infinite loop = %+v, %v", exit, err)
	}
}

func TestGuardAtomicity(t *testing.T) {
	for _, in := range []Instruction{
		{Op: Binary, Operator: Add, Dest: 0, Left: Slot(0), Right: Slot(1)},
		{Op: Binary, Operator: Div, Dest: 0, Left: Slot(0), Right: Slot(1)},
		{Op: Unary, Operator: Pos, Dest: 0, Left: Slot(1)},
		{Op: Unary, Operator: Not, Dest: 0, Left: Slot(1)},
		{Op: Branch, Operator: Lt, Left: Slot(0), Right: Slot(1), Target: 0},
		{Op: Branch, Operator: Truth, Left: Slot(1), Target: 0},
		{Op: Update, Operator: Add, Left: Slot(1), Dest: 1, Extra: 0},
		{Op: Return, Left: Slot(2)},
		{Op: Copy, Left: Slot(0), Dest: 1, Check: true, CheckSlot: 2},
	} {
		slots := []Value{Float(42), {Kind: Opaque, Bits: 17}, {Kind: Uninitialized}}
		before := append([]Value(nil), slots...)
		p := &Program{Locals: 3, Code: []Instruction{in}, Maps: []StateMap{{PC: 8}}}
		exit, err := p.Evaluate(slots, 0, 1)
		if err != nil || exit.Kind != GuardExit || exit.Steps != 0 || exit.State.PC != 8 || !reflect.DeepEqual(slots, before) {
			t.Fatalf("op %d: exit %+v, %v; slots %+v", in.Op, exit, err, slots)
		}
	}
}

func TestReferenceReturn(t *testing.T) {
	slots := []Value{{Kind: Opaque, Bits: 17}}
	p := &Program{Locals: 1, Code: []Instruction{{Op: Return, Left: Slot(0)}}, Maps: []StateMap{{PC: 8}}}
	exit, err := p.Evaluate(slots, 0, 1)
	if err != nil || exit.Kind != Returned || exit.Value != slots[0] || exit.Steps != 1 || exit.State.PC != 8 {
		t.Fatalf("rooted return %+v, %v", exit, err)
	}
}

func TestStoreLoadAlias(t *testing.T) {
	p := &Program{
		Locals: 1, StackSize: 1,
		Code: []Instruction{
			{Op: StoreLoad, Dest: 0, Extra: 1, Left: Slot(1), Right: Slot(0)},
			{Op: Return, Left: Slot(1)},
		},
		Maps: []StateMap{{PC: 0, Depth: 1}, {PC: 1, Depth: 1}},
	}
	slots := []Value{Float(3), Float(7)}
	exit, err := p.Evaluate(slots, 0, 2)
	if err != nil || exit.Kind != Returned || exit.Value != Float(7) || slots[0] != Float(7) {
		t.Fatalf("aliased store/load = %+v, %v; locals %+v", exit, err, slots)
	}
}
