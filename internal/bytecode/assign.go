package bytecode

// WrittenLocals is the range of local slots in stores to, [lo, hi), empty
// for an instruction that stores to none: an assignment, an update, a let's
// or const's initialization, a parameter's binding by a prologue, or an
// arguments object kept in its slot. It is every store the interpreter
// makes to a frame's locals from an instruction.
func WrittenLocals(in Instr) (lo, hi int) {
	switch in.Op {
	case OpSetLocal, OpPutLocal, OpSetLocalCheck, OpInitLocal, OpClearLocal, OpSetLocalGet,
		OpIncLocal, OpDecLocal, OpUpdateLocal, OpInitParam,
		OpLazyArguments, OpApplyArguments, OpArgumentsIndex:
		return int(in.A), int(in.A) + 1
	case OpGetLocalIndexUpdate:
		// The key's slot, past the update's two flag bits; A is the
		// object's, which is only read.
		k := int(in.B >> 2)
		return k, k + 1
	case OpParamsToDeadZone:
		return 0, int(in.A)
	}
	return 0, 0
}

// UpvalueAssigned reports whether t, or a closure t makes, or one that
// closure makes in turn, may assign t's upvalue up: an instruction of t's
// stores to it, or a nested function's upvalue forwarding it is assigned.
// A function with a direct eval may assign any: a strict one's too, which
// declares nothing in it (HasDirectEval) but assigns what is in scope.
func UpvalueAssigned(t *Function, up int) bool {
	if t.HasDirectEval {
		return true
	}
	for _, in := range t.Code {
		switch in.Op {
		case OpSetUpvalue, OpSetUpvalueCheck, OpInitUpvalue:
			if int(in.A) == up {
				return true
			}
		case OpDirectEval, OpTailDirectEval:
			return true
		}
	}
	for _, c := range t.Constants {
		if c.Kind != ConstFunction || c.Fn == nil {
			continue
		}
		for j, d := range c.Fn.Upvalues {
			if !d.FromParent && int(d.Index) == up && UpvalueAssigned(c.Fn, j) {
				return true
			}
		}
	}
	return false
}
