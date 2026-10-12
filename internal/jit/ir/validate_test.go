package ir

import "testing"

func TestValidateNativeSafety(t *testing.T) {
	fixture := func() *Program {
		return &Program{Locals: 1, StackSize: 1,
			Code: []Instruction{{Op: Copy, Left: Slot(0), Dest: 1}, {Op: Return, Left: Slot(1)}},
			Maps: []StateMap{{PC: 0}, {PC: 1, Depth: 1}}}
	}
	for _, tc := range []struct {
		name string
		edit func(*Program)
	}{
		{"source", func(p *Program) { p.Code[0].Left.Slot = -2 }},
		{"inactive", func(p *Program) { p.Code[0].Left.Slot = 1 }},
		{"destination", func(p *Program) { p.Code[0].Dest = 2 }},
		{"literal", func(p *Program) { p.Code[0].Left = Literal(Value{Kind: Kind(99)}) }},
		{"boolean", func(p *Program) { p.Code[0].Left = Literal(Value{Kind: Boolean, Bits: 2}) }},
		{"handle-literal", func(p *Program) { p.Code[0].Left = Literal(Value{Kind: Opaque, Bits: 1}) }},
		{"string-literal", func(p *Program) { p.Code[0].Left = Literal(Value{Kind: String}) }},
		{"guard", func(p *Program) { p.Code[0].Check, p.Code[0].CheckSlot = true, 1 }},
		{"initial-depth", func(p *Program) { p.Maps[0].Depth = 1 }},
		{"map", func(p *Program) { p.Maps[1].PC = 2 }},
		{"capacity", func(p *Program) { p.StackSize = MaxSlots }},
		{"missing-map", func(p *Program) { p.Maps = nil }},
		{"fallthrough", func(p *Program) { p.Code[1].Op = Nop }},
		{"target", func(p *Program) { p.Code[0].Op, p.Code[0].Target = Jump, 2 }},
		{"unreachable", func(p *Program) { p.Maps[1].Depth = -1 }},
		{"binary", func(p *Program) { p.Code[0].Op, p.Code[0].Operator = Binary, Truth }},
		{"unary", func(p *Program) { p.Code[0].Op, p.Code[0].Operator = Unary, Add }},
		{"update", func(p *Program) { p.Code[0].Op, p.Code[0].Operator = Update, Div }},
		{"swap", func(p *Program) { p.Code[0].Op, p.Code[0].Extra = Swap, 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture()
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(p)
			if err := p.Validate(); err == nil {
				t.Fatal("accepted unsafe native IR")
			}
		})
	}
	if err := (*Program)(nil).Validate(); err == nil {
		t.Fatal("accepted nil program")
	}
}

func TestValidateNativeCall(t *testing.T) {
	fixture := func() *Program {
		return &Program{Locals: 4,
			Code: []Instruction{
				{Op: Call, Left: Slot(0), Right: Literal(Value{}), Third: Slot(1), Extra: 2, Dest: 3},
				{Op: Return, Left: Slot(3)},
			}, Maps: []StateMap{{PC: 0}, {PC: 1}}}
	}
	for _, tc := range []struct {
		name string
		edit func(*Program)
	}{
		{"function-literal", func(p *Program) { p.Code[0].Left = Literal(Value{Kind: Opaque}) }},
		{"function-inactive", func(p *Program) { p.Code[0].Left = Slot(4) }},
		{"receiver-literal", func(p *Program) { p.Code[0].Right = Literal(Float(1)) }},
		{"receiver-inactive", func(p *Program) { p.Code[0].Right = Slot(4) }},
		{"args-literal", func(p *Program) { p.Code[0].Third = Literal(Value{}) }},
		{"negative-count", func(p *Program) { p.Code[0].Extra = -1 }},
		{"excess-count", func(p *Program) { p.Code[0].Extra = MaxSlots + 1 }},
		{"past-end", func(p *Program) { p.Code[0].Third = Slot(3) }},
		{"negative-start", func(p *Program) { p.Code[0].Third = Slot(-2) }},
		{"destination", func(p *Program) { p.Code[0].Dest = 4 }},
		{"site-index", func(p *Program) { p.Code[0].Key = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture()
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(p)
			if err := p.Validate(); err == nil {
				t.Fatal("accepted unsafe call layout")
			}
		})
	}
	p := fixture()
	// An empty argument span may start at the first inactive slot.
	p.Code[0].Third, p.Code[0].Extra = Slot(4), 0
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p = fixture()
	p.Code = append(p.Code[:1], p.Code[0], p.Code[1])
	p.Maps = []StateMap{{PC: 0}, {PC: 1}, {PC: 2}}
	if err := p.Validate(); err == nil {
		t.Fatal("accepted duplicate site index")
	}
	p.Code[1].Key = 1
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.Code = make([]Instruction, MaxCallSites+2)
	p.Maps = make([]StateMap, len(p.Code))
	for pc := range p.Maps {
		p.Maps[pc].PC = uint32(pc)
		p.Code[pc] = Instruction{Op: Call, Left: Slot(0), Right: Literal(Value{}), Third: Slot(1), Dest: 3, Key: uint32(pc)}
	}
	p.Code[len(p.Code)-1] = Instruction{Op: Return, Left: Slot(3)}
	if err := p.Validate(); err == nil {
		t.Fatal("accepted excessive call sites")
	}
}
