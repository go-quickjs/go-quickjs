package ir

import "fmt"

const (
	// MaxInstructions bounds validation, lowering, and native emission work.
	MaxInstructions = 4096
	// MaxSlots bounds pointer-free scratch storage, including locals.
	MaxSlots = 256
)

// Validate checks all memory operands and control-flow destinations before
// native emission. Unreachable instructions are never emitted or entered.
func (p *Program) Validate() error {
	if p == nil || len(p.Code) == 0 || len(p.Code) > MaxInstructions || len(p.Code) != len(p.Maps) {
		return fmt.Errorf("jit IR: invalid instruction layout")
	}
	if p.Locals < 0 || p.StackSize < 0 || p.Locals > MaxSlots || p.StackSize > MaxSlots || p.Locals+p.StackSize > MaxSlots || len(p.Globals) > p.Locals || p.This && p.Locals <= len(p.Globals) {
		return fmt.Errorf("jit IR: invalid slot layout")
	}
	for pc, state := range p.Maps {
		if state.PC != uint32(pc) || state.Depth < -1 || state.Depth > p.StackSize {
			return fmt.Errorf("jit IR: pc %d: invalid state map", pc)
		}
	}
	if p.Maps[0].Depth != 0 {
		return fmt.Errorf("jit IR: invalid initial stack depth")
	}
	calls := uint32(0)
	for pc, in := range p.Code {
		if p.Maps[pc].Depth < 0 {
			continue
		}
		bad := func(why string) error { return fmt.Errorf("jit IR: pc %d: %s", pc, why) }
		active := p.Locals + p.Maps[pc].Depth
		source := func(o Operand) bool {
			if o.Slot == -1 {
				// A literal is a scalar. A handle (Opaque, String) indexes the
				// caller's root tables, so only a slot the caller filled may
				// hold one: a literal handle would be a forged reference.
				return o.Literal.Kind <= Uninitialized && (o.Literal.Kind != Boolean || o.Literal.Bits <= 1)
			}
			return o.Slot >= 0 && o.Slot < active
		}
		dest := func(slot int) bool { return slot >= 0 && slot < p.Locals+p.StackSize }
		target := func(n int) bool { return n >= 0 && n < len(p.Maps) && p.Maps[n].Depth >= 0 }
		if in.Check && (in.CheckSlot < 0 || in.CheckSlot >= active) {
			return bad("invalid guard slot")
		}
		left, right, third, write, extra := false, false, false, false, false
		switch in.Op {
		case Nop, Host:
		case StringMethod, TypeTest:
			left, write = true, true
		case BindingWrite:
			left = true
		case BindingCheck:
			write = true
		case Resolved:
			left, right, write = true, true, true
		case ObjectLiteral, StringConst:
			write = true
		case UpvalueRead, UpvalueWrite:
			left, write = in.Op == UpvalueWrite, in.Op == UpvalueRead
			if int(in.Key) >= p.Upvalues || p.UpvalueBase+p.Upvalues > p.Locals {
				return bad("invalid captured binding")
			}
		case FieldDefine:
			left, right = true, true
		case ArrayLiteral:
			write = true
			if in.Extra < 0 || in.Extra > MaxArrayLiteral || in.Dest < 0 || in.Dest+in.Extra != active {
				return bad("invalid array literal")
			}
		case StringCode:
			left, right, third, write = true, true, true, true
		case Call:
			if calls == MaxCallSites || in.Key != calls || in.Left.Slot < 0 ||
				in.Right.Slot < 0 && in.Right != Literal(Value{}) ||
				in.Third.Slot < 0 || in.Extra < 0 || in.Extra > MaxSlots || in.Third.Slot > active-in.Extra {
				return bad("invalid native call")
			}
			calls++
			left, right, write = true, true, true
		case ArrayRead, ArrayKey, ArrayUpdate:
			left, right, write = true, true, in.Op != ArrayKey
			if in.Op == ArrayUpdate {
				extra = true
				if in.Operator != Add && in.Operator != Sub || in.Extra >= active {
					return bad("invalid array update")
				}
			}
		case ArrayWrite:
			left, right, third = true, true, true
		case ArrayLength:
			left, write = true, true
		case PropertyRead, PropertyWrite, BindingRead, ReferenceRead:
			left, right, write = true, in.Op == PropertyWrite, in.Op != PropertyWrite
		case Insert3:
			if in.Dest < 0 || in.Dest+2 >= active || !dest(in.Dest+3) {
				return bad("invalid insert")
			}
		case Insert2:
			if in.Dest < 0 || in.Dest+1 >= active || !dest(in.Dest+2) {
				return bad("invalid insert")
			}
		case Copy:
			left, write = true, true
		case CopyPair, StoreLoad:
			left, right, write, extra = true, true, true, true
		case Swap:
			write, extra = true, true
			if in.Dest >= active || in.Extra >= active {
				return bad("swap reads an inactive slot")
			}
		case Binary, StringAdd:
			left, right, write = true, true, true
			if in.Operator > Ne && (in.Operator < BitAnd || in.Operator > UShr) && in.Operator != Mod {
				return bad("invalid binary operator")
			}
		case Unary:
			left, write = true, true
			if in.Operator != Neg && in.Operator != Pos && in.Operator != Not && in.Operator != Int32 && in.Operator != BitNot {
				return bad("invalid unary operator")
			}
		case Update:
			left, write, extra = true, true, in.Extra >= 0
			if in.Extra < -1 || in.Operator != Add && in.Operator != Sub {
				return bad("invalid update")
			}
		case Jump:
			if !target(in.Target) {
				return bad("invalid branch target")
			}
		case Branch:
			left, right = true, in.Operator != Truth
			if !target(in.Target) || in.Operator != Truth && (in.Operator < Lt || in.Operator > Ne) {
				return bad("invalid conditional branch")
			}
		case Return:
			left = true
		default:
			return bad("invalid opcode")
		}
		if left && !source(in.Left) || right && !source(in.Right) || third && !source(in.Third) || write && !dest(in.Dest) || extra && !dest(in.Extra) {
			return bad("invalid slot operand")
		}
		if in.Op != Return && in.Op != Jump && !target(pc+1) {
			return bad("invalid fallthrough")
		}
	}
	return nil
}
