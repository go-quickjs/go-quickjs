package compiler

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// leafKind is which of the bodies bytecode.LeafKind names fn's compiled code
// is, or LeafNone. Only a function whose call does nothing before its body
// qualifies: no rest parameter or arguments object to make, no default or
// pattern to bind, no generator or async function to set up, no eval scope,
// and a `this` of its own -- not an arrow's, and not a derived
// constructor's, which super() binds.
func leafKind(fn *bytecode.Function) bytecode.LeafKind {
	if fn.Generator || fn.Async || fn.HasRest || fn.HasDirectEval || !fn.HasSimpleParams || fn.ParamsAreLexical {
		return bytecode.LeafNone
	}
	// this.k.apply(this, arguments) mentions arguments, but apply_arguments
	// hands the arguments on without the object being made.
	code := fn.Code
	if fn.Kind == bytecode.KindNormal && len(code) == 7 &&
		code[0].Op == bytecode.OpPushThis && code[1].Op == bytecode.OpGetProp &&
		code[2].Op == bytecode.OpGetPropThis && fn.Names[code[2].A] == "apply" &&
		code[3].Op == bytecode.OpPushThis && code[4].Op == bytecode.OpApplyArguments &&
		code[5].Op == bytecode.OpDrop && code[6].Op == bytecode.OpReturnUndef {
		return bytecode.LeafForward
	}
	if fn.UsesArguments {
		return bytecode.LeafNone
	}
	switch fn.Kind {
	case bytecode.KindNormal, bytecode.KindMethod, bytecode.KindGetter, bytecode.KindConstructor:
	default:
		return bytecode.LeafNone
	}
	param := func(in bytecode.Instr) bool {
		return in.Op == bytecode.OpGetLocal && int(in.A) < fn.ParamCount
	}
	// The return_undef that ends every body is unreachable after a return.
	if len(code) >= 4 && code[0].Op == bytecode.OpPushThis && code[1].Op == bytecode.OpGetProp {
		switch {
		case len(code) == 4 && code[2].Op == bytecode.OpReturn:
			return bytecode.LeafGetThis
		case len(code) == 5 && code[2].Op == bytecode.OpGetLength && code[3].Op == bytecode.OpReturn:
			return bytecode.LeafGetThisLength
		case len(code) == 6 && param(code[2]) && code[3].Op == bytecode.OpGetIndex && code[4].Op == bytecode.OpReturn:
			return bytecode.LeafGetThisIndex
		}
	}
	n := 0
	for n+3 <= len(code) && code[n].Op == bytecode.OpPushThis && param(code[n+1]) && code[n+2].Op == bytecode.OpSetProp {
		n += 3
	}
	rest := code[n:]
	ends := len(rest) == 1 && rest[0].Op == bytecode.OpReturnUndef ||
		fn.Kind == bytecode.KindConstructor && len(rest) == 3 &&
			rest[0].Op == bytecode.OpPushThis && rest[1].Op == bytecode.OpReturn
	if n > 0 && ends {
		return bytecode.LeafSetThis
	}
	if pureBody(fn) {
		return bytecode.LeafPure
	}
	return bytecode.LeafNone
}

// pureBody reports whether a function's code is a body LeafPure marks: no
// local but its parameters, every jump forward, and every instruction one
// that reads or computes, of a stack small enough to be evaluated in a
// fixed array, but for one store, to a property or an upvalue, in its tail.
// It sets fn.PureTail. An upvalue it reads is one get_upvalue reads, which
// has no dead zone to check.
func pureBody(fn *bytecode.Function) bool {
	if fn.LocalCount != fn.ParamCount || fn.MaxStack > 24 || len(fn.Code) > 64 {
		return false
	}
	param := func(k uint32) bool { return int(k) < fn.ParamCount }
	tail := len(fn.Code)
	for tail > 0 && pureSafe(fn.Code[tail-1], param) {
		tail--
	}
	for pc, in := range fn.Code {
		switch in.Op {
		case bytecode.OpPushThis, bytecode.OpPushInt, bytecode.OpPushConst, bytecode.OpPushNull,
			bytecode.OpPushTrue, bytecode.OpPushFalse, bytecode.OpPushUndef, bytecode.OpPushEmptyString,
			bytecode.OpGetProp, bytecode.OpGetLength, bytecode.OpGetGlobal,
			bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv,
			bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr,
			bytecode.OpBinImm, bytecode.OpNeg, bytecode.OpNot, bytecode.OpTypeOf,
			bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe,
			bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe,
			bytecode.OpDup, bytecode.OpDrop, bytecode.OpReturn, bytecode.OpReturnUndef,
			bytecode.OpGetPropThis, bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew,
			bytecode.OpGetUpvalue, bytecode.OpToNumeric, bytecode.OpInc, bytecode.OpDec:
			// A call is of a body that is itself pure, or it is not made
			// frameless: the VM sees which when it gets there. So is a
			// construction, of a constructor that only stores its
			// parameters.
		case bytecode.OpSetProp, bytecode.OpSetUpvalue:
			// The store is the last thing the body can give up at: a
			// property's, or an upvalue's -- a counter a closure keeps, as
			// count++ does.
			if pc+1 < tail {
				return false
			}
		case bytecode.OpGetLocal:
			if !param(in.A) {
				return false
			}
		case bytecode.OpGetLocal2:
			if !param(in.A) || !param(in.B) {
				return false
			}
		case bytecode.OpBinLocal:
			if !param(in.A) {
				return false
			}
		case bytecode.OpLocalBinImm:
			if !param(in.A & (1<<24 - 1)) {
				return false
			}
		case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse,
			bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep:
			if int(in.A) <= pc || int(in.A) > len(fn.Code) {
				return false
			}
		default:
			return false
		}
	}
	fn.PureTail = int32(tail)
	return true
}

// pureSafe reports whether an instruction of a pure body is one its
// frameless evaluation cannot give up at: it pushes, drops, branches or
// returns what it has, with no read of a property or a global and no
// operator that could call valueOf.
func pureSafe(in bytecode.Instr, param func(uint32) bool) bool {
	switch in.Op {
	case bytecode.OpPushThis, bytecode.OpPushInt, bytecode.OpPushConst, bytecode.OpPushNull,
		bytecode.OpPushTrue, bytecode.OpPushFalse, bytecode.OpPushUndef, bytecode.OpPushEmptyString,
		bytecode.OpDup, bytecode.OpDrop, bytecode.OpReturn, bytecode.OpReturnUndef,
		bytecode.OpNot, bytecode.OpTypeOf, bytecode.OpStrictEq, bytecode.OpStrictNe,
		bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue,
		bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep:
		return true
	case bytecode.OpGetLocal:
		return param(in.A)
	case bytecode.OpGetLocal2:
		return param(in.A) && param(in.B)
	}
	return false
}
