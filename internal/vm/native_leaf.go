package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// regBinaryLeaf describes a leaf that returns a binary arithmetic operation
// of parameters or upvalues. The low byte is its register opcode; each
// operand uses 17 bits, starting at bits 8 and 32: its 16-bit index and a
// flag at bit 16 for an upvalue. Nothing the caller could observe is stored.
func regBinaryLeaf(c *regCode) uint64 {
	ops := c.leafOps
	for i, o := range ops {
		var op bytecode.ROp
		switch o.op {
		case rlMove, rlGetUp:
			continue
		case rlAdd:
			op = bytecode.RAdd
		case rlSub:
			op = bytecode.RSub
		case rlMul:
			op = bytecode.RMul
		default:
			return 0
		}
		if i+2 != len(ops) || ops[i+1].op != rlRet || ops[i+1].a != o.a {
			return 0
		}
		left, lok := regBinaryOperand(ops[:i], o.b, c.params)
		right, rok := regBinaryOperand(ops[:i], o.c, c.params)
		if lok && rok {
			return uint64(op) | uint64(left)<<8 | uint64(right)<<32
		}
		return 0
	}
	return 0
}

func regBinaryOperand(ops []regLeafOp, reg uint16, params int32) (uint32, bool) {
	for i := len(ops) - 1; i >= 0; i-- {
		o := &ops[i]
		if o.a != reg {
			continue
		}
		if o.op == rlGetUp {
			return uint32(o.b) | 1<<16, true
		}
		reg = o.b
	}
	return uint32(reg - 2), reg >= 2 && int32(reg)-2 < params
}

// regGetterLeaf describes the property cache and optional parameter of a
// this getter. RGetProp distinguishes it from a numeric binary leaf; the
// next bytes hold its LeafKind, the cache site of its get_prop and, for
// LeafGetThisIndex, the index of the parameter get_index reads.
func regGetterLeaf(rc *bytecode.RegCode) uint64 {
	switch rc.Leaf {
	case bytecode.LeafGetThis, bytecode.LeafGetThisLength, bytecode.LeafGetThisIndex:
		d := uint64(bytecode.RGetProp) | uint64(rc.Leaf)<<8 | uint64(rc.Code[0].C())<<16
		if rc.Leaf == bytecode.LeafGetThisIndex {
			// leafOf has checked that it is a parameter's register.
			d |= uint64(rc.Code[1].C()-2) << 32
		}
		return d
	}
	return 0
}
