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
