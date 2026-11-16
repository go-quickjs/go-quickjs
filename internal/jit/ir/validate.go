package ir

import "fmt"

// Validate checks all memory operands and control-flow destinations before
// native emission. Unreachable instructions are never emitted or entered.
func (p *Program) Validate() error {
	if p == nil || len(p.Code) == 0 || len(p.Code) != len(p.Maps) {
		return fmt.Errorf("jit IR: invalid instruction layout")
	}
	if p.Locals < 0 || p.StackSize < 0 || len(p.Globals) > p.Locals || p.This && p.Locals <= len(p.Globals) {
		return fmt.Errorf("jit IR: invalid slot layout")
	}
	if p.ArgumentParams < 0 || p.ArgumentParams > p.Locals {
		return fmt.Errorf("jit IR: invalid aliased parameters")
	}
	reg := p.Origin != nil
	if reg && (len(p.Origin) != len(p.Code) || p.Regs <= 0 || p.Regs > p.Locals || p.StackSize != 0 || p.PrologueArgs != 0) ||
		!reg && p.Regs != 0 || p.ParamBase < 0 || p.ParamBase+p.Params > p.Locals {
		return fmt.Errorf("jit IR: invalid register layout")
	}
	for pc, state := range p.Maps {
		want := uint32(pc)
		if reg {
			want = p.Origin[pc]
			if pc > 0 && (want < p.Origin[pc-1] || want == p.Origin[pc-1] && state.Depth != p.Maps[pc-1].Depth) {
				// An origin's instructions are together, in order, and all
				// reachable or none.
				return fmt.Errorf("jit IR: pc %d: invalid origin", pc)
			}
		}
		if state.PC != want || state.Depth < -1 || state.Depth > p.StackSize {
			return fmt.Errorf("jit IR: pc %d: invalid state map", pc)
		}
	}
	if p.Maps[0].Depth != 0 {
		return fmt.Errorf("jit IR: invalid initial stack depth")
	}
	if p.PrologueArgs != 0 {
		// The arguments a prologue's states keep past their operands fit
		// in the frame's operand slots.
		if p.PrologueArgs < 0 || p.PrologueArgs > p.Locals || p.PrologueStart < 0 || p.PrologueStart >= p.PrologueEnd || p.PrologueEnd > len(p.Code) ||
			p.Code[p.PrologueStart].Op != ParamsDead || int(p.Code[p.PrologueStart].Key) != p.PrologueArgs {
			return fmt.Errorf("jit IR: invalid parameter prologue")
		}
		for pc := p.PrologueStart + 1; pc < p.PrologueEnd; pc++ {
			if p.Maps[pc].Depth+p.PrologueArgs > p.StackSize {
				return fmt.Errorf("jit IR: pc %d: no room for the prologue's arguments", pc)
			}
		}
	}
	if p.HandlerAt != nil && len(p.HandlerAt) != len(p.Code) {
		return fmt.Errorf("jit IR: invalid handler table")
	}
	for i, h := range p.Handlers {
		if int(h.PC) >= len(p.Code) || !p.starts(int(h.PC)) || h.Depth < 0 || h.Outer < 0 || int(h.Outer) > i {
			return fmt.Errorf("jit IR: invalid handler %d", i)
		}
	}
	for _, h := range p.HandlerAt {
		if h < 0 || int(h) > len(p.Handlers) {
			return fmt.Errorf("jit IR: invalid handler table")
		}
	}
	for _, pc := range p.Entries {
		if pc < 0 || pc >= len(p.Code) || p.Maps[pc].Depth < 0 || !p.starts(pc) {
			return fmt.Errorf("jit IR: invalid entry %d", pc)
		}
	}
	calls := uint32(0)
	// committed says an instruction of the current origin's has committed
	// (Instruction's Commits): none after it in the origin may leave.
	committed := false
	for pc, in := range p.Code {
		if p.starts(pc) {
			committed = false
		}
		if p.Maps[pc].Depth < 0 {
			continue
		}
		if reg {
			alone := p.starts(pc) && (pc+1 == len(p.Code) || p.starts(pc+1))
			switch {
			case in.Op == Insert2 || in.Op == Insert3 || in.Op == Call:
				return fmt.Errorf("jit IR: pc %d: a stack operation in register code", pc)
			case in.Op == Host && !alone:
				// Go runs the whole register instruction.
				return fmt.Errorf("jit IR: pc %d: a host operation in an expansion", pc)
			case in.Generic && (!genericOp(in.Op) || in.Check):
				return fmt.Errorf("jit IR: pc %d: no generic form", pc)
			case committed && in.MayExit():
				// The atomicity rule: an exit here would run the register
				// instruction again, what has committed of it twice.
				return fmt.Errorf("jit IR: pc %d: an exit after register pc %d committed", pc, p.Origin[pc])
			}
			committed = committed || p.CommitsAt(pc)
		} else if in.Generic {
			return fmt.Errorf("jit IR: pc %d: generic outside an expansion", pc)
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
		target := func(n int) bool { return n >= 0 && n < len(p.Maps) && p.Maps[n].Depth >= 0 && p.starts(n) }
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
		case Resolved, HasProperty, DeleteProperty:
			left, right, write = true, true, true
		case ArgumentRead:
			left, write = true, true
			third = in.Third.Slot >= 0
		case ObjectLiteral, StringConst, BigIntConst, Closure:
			write = true
		case IterClose, Coercible, StringFreeze:
			left = true
		case ParamsDead:
			if int(in.Key) > p.Locals {
				return bad("invalid parameter count")
			}
		case ParamInit, ParamDefault:
			write = true
			if int(in.Key) >= p.Locals {
				return bad("invalid parameter")
			}
		case CloseUpvalues:
			if int(in.Key) > p.Locals {
				return bad("invalid local")
			}
		case UpvalueRead, UpvalueWrite:
			left, write = in.Op == UpvalueWrite, in.Op == UpvalueRead
			if int(in.Key) >= p.Upvalues || p.UpvalueBase+p.Upvalues > p.Locals {
				return bad("invalid captured binding")
			}
		case FieldDefine:
			left, right = true, true
		case ArrayLiteral:
			write = true
			if in.Extra < 0 || in.Extra > MaxArrayLiteral || in.Dest < 0 || in.Dest+in.Extra != active && !(reg && in.Dest+in.Extra <= active) {
				return bad("invalid array literal")
			}
		case StringPart:
			left, write = true, true
		case StringConcat:
			write = true
			if in.Extra < 1 || in.Dest < 0 || in.Dest+in.Extra != active && !(reg && in.Dest+in.Extra <= active) {
				return bad("invalid string concat")
			}
		case StringCode:
			left, right, third, write = true, true, true, true
		case Call:
			if calls == MaxCallSites || in.Key != calls || in.Left.Slot < 0 ||
				in.Right.Slot < 0 && in.Right != Literal(Value{}) ||
				in.Third.Slot < 0 || in.Extra < 0 || in.Third.Slot > active-in.Extra {
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
			if in.Operator > Ne && (in.Operator < BitAnd || in.Operator > UShr) && in.Operator != Mod && in.Operator != Pow {
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
			left, right = true, in.Operator != Truth && in.Operator != Nullish
			if !target(in.Target) || in.Operator != Truth && in.Operator != Nullish && (in.Operator < Lt || in.Operator > Ne) {
				return bad("invalid conditional branch")
			}
		case Return, Rethrow:
			left = true
		case IterStep:
			left, write = true, true
			if !target(in.Target) {
				return bad("invalid branch target")
			}
		default:
			return bad("invalid opcode")
		}
		if left && !source(in.Left) || right && !source(in.Right) || third && !source(in.Third) || write && !dest(in.Dest) || extra && !dest(in.Extra) {
			return bad("invalid slot operand")
		}
		// What Go runs (Host) may end the code's flow there -- a throw, which
		// nothing after it reaches: the instruction is the end of its block,
		// which has no successor. A switch whose default throws had had every
		// such function refused.
		if in.Op != Return && in.Op != Jump && in.Op != Host && !(target(pc+1) || !p.starts(pc+1) && p.Maps[pc+1].Depth >= 0) {
			return bad("invalid fallthrough")
		}
	}
	return nil
}
