package bytecode

// The locals that hold their values alone: the tree tier and the JIT give
// back a BigInt such a local held when it is assigned the next result of
// arithmetic (the VM's zzbigown.go), and the register backend marks the
// operators whose results such a local takes (regcomp, RArithOwnA).

// SoleLocals is the locals of fn, of the first 64, that hold their values
// alone: each a let or var of its own -- not a parameter, nor captured --
// whose value fn's code reads only as an operand of an operator that
// makes a value of its own, in a function that is no generator, no async
// function, has no direct eval and no arguments object, of instructions
// that soleEffect knows.
func SoleLocals(fn *Function) uint64 {
	if fn.Generator || fn.Async || fn.HasDirectEval || fn.UsesArguments {
		return 0
	}
	code := fn.Code
	// The candidates, and the instructions control can come to other than
	// from the one before, where nothing pushed before may still be on the
	// stack.
	var cand uint64
	for k := fn.ParamCount; k < len(fn.Locals) && k < 64; k++ {
		if !fn.Locals[k].Captured {
			cand |= 1 << uint(k)
		}
	}
	if cand == 0 {
		return 0
	}
	target := make([]bool, len(code)+1)
	for _, in := range code {
		switch in.Op {
		case OpJump, OpJumpIfFalse, OpJumpIfTrue, OpJumpIfCmpFalse:
			if int(in.A) > len(code) {
				return 0
			}
			target[in.A] = true
		}
	}
	// stack holds, for each value the code has pushed, the local it is a
	// read of plus one, or zero for any other value; below its bottom is
	// what was pushed before a jump's target, none of it a read.
	var stack []uint8
	escaped := uint64(0)
	escape := func(tag uint8) {
		if tag != 0 {
			escaped |= 1 << uint(tag-1)
		}
	}
	pop := func() uint8 {
		if len(stack) == 0 {
			return 0
		}
		t := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		return t
	}
	read := func(k uint32) uint8 {
		if k < 64 && cand&(1<<k) != 0 {
			return uint8(k + 1)
		}
		return 0
	}
	settle := func() {
		for _, t := range stack {
			escape(t)
		}
		stack = stack[:0]
	}
	// A store to a local whose read is still on the stack, an operand
	// yet to be taken -- s = s * (s = s * 3n, 5n) -- leaves the value it
	// held to that operand: given back at the store, its block would be
	// made the next result in while the operand is still to be read.
	stored := func(k uint32) {
		if tag := read(k); tag != 0 {
			for _, t := range stack {
				if t == tag {
					escape(tag)
				}
			}
		}
	}
	for pc, in := range code {
		if target[pc] {
			settle()
		}
		switch in.Op {
		case OpGetLocal, OpGetLocalCheck:
			stack = append(stack, read(in.A))
		case OpGetLocal2:
			stack = append(stack, read(in.A), read(in.B))
		case OpSetLocal, OpInitLocal, OpSetLocalCheck:
			escape(pop())
			stored(in.A)
		case OpPutLocal:
			if len(stack) != 0 {
				escape(stack[len(stack)-1])
			}
			stored(in.A)
		case OpSetLocalGet:
			escape(pop())
			stored(in.A)
			stack = append(stack, read(in.B))
		case OpClearLocal, OpIncLocal, OpDecLocal:
			// A store of undefined, a step that reads the value as an
			// operand.
			stored(in.A)
		case OpBinLocal, OpBinImm, OpNeg, OpNot, OpBitNot:
			// An operand, and the result; bin_local's local an operand too.
			pop()
			stack = append(stack, 0)
		case OpLocalBinImm:
			stack = append(stack, 0)
		case OpAdd, OpSub, OpMul, OpDiv, OpMod,
			OpBitAnd, OpBitOr, OpBitXor, OpShl, OpShr, OpUShr,
			OpLt, OpLe, OpGt, OpGe,
			OpEq, OpNe, OpStrictEq, OpStrictNe:
			pop()
			pop()
			stack = append(stack, 0)
		case OpPushConst, OpPushInt, OpPushUndef, OpPushNull,
			OpPushTrue, OpPushFalse, OpPushEmptyString, OpGetGlobal:
			stack = append(stack, 0)
		case OpDup:
			t := pop()
			stack = append(stack, t, t)
		case OpDrop:
			pop()
		case OpSwap:
			b, a := pop(), pop()
			stack = append(stack, b, a)
		case OpSetGlobal, OpSetGlobalStrict:
			escape(pop())
		case OpGetPropThis:
			// The receiver stays, and the method is read of it.
			if len(stack) != 0 {
				escape(stack[len(stack)-1])
			}
			stack = append(stack, 0)
		case OpCall, OpCallMethod:
			n := int(in.A) + 1
			if in.Op == OpCallMethod {
				n++
			}
			for i := 0; i < n; i++ {
				escape(pop())
			}
			stack = append(stack, 0)
		case OpJumpIfFalse, OpJumpIfTrue:
			pop()
			settle()
		case OpJumpIfCmpFalse:
			pop()
			pop()
			settle()
		case OpJump, OpReturnUndef:
			settle()
		case OpReturn:
			escape(pop())
			settle()
		case OpArgumentsIndex:
			// str_freeze, after a read of a string's binding, leaves the
			// value where it is; a function with arguments is out above.
			if !in.StrFreeze() {
				return 0
			}
		default:
			return 0
		}
	}
	return cand &^ escaped
}
