package nativegen

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// handlerOps are the operations the native handlers answer, in the order
// every architecture's emitter lays their handlers out: an architecture
// may leave some out, whose table entries are then its exit stub's, but
// emits the others in this order, so that one list says what the native
// tier covers and an emitter is checked against it (handlerOrder). What
// came later goes last, so that it moves none of the code before it.
var handlerOps = []bytecode.ROp{
	bytecode.RNop, bytecode.RMove, bytecode.RLoadConst, bytecode.RGetGlobal,
	bytecode.RLoadInt, bytecode.RLoadUndef, bytecode.RLoadNull, bytecode.RLoadTrue,
	bytecode.RLoadFalse, bytecode.RLoadUninit, bytecode.RLoadEmpty,
	bytecode.RGetUpval, bytecode.RSetUpval, bytecode.RJLtU, bytecode.RSetGlobal,
	bytecode.RAddProp, bytecode.RAddPropI,
	bytecode.RJNLtU, bytecode.RJLeU, bytecode.RJNLeU, bytecode.RJEq, bytecode.RJNe,
	bytecode.RKeyOfBase,
	bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RAddI, bytecode.RSubI,
	bytecode.RInc, bytecode.RDec, bytecode.RPostInc,
	bytecode.RAndI, bytecode.ROrI, bytecode.RShlI, bytecode.RShrI, bytecode.RBitAnd,
	bytecode.RJmp, bytecode.RJLt, bytecode.RJNLt, bytecode.RJNGt, bytecode.RJGe, bytecode.RJNGe,
	bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI,
	bytecode.RJGtI, bytecode.RJNGtI, bytecode.RJGeI, bytecode.RJNGeI, bytecode.RJNeI,
	bytecode.RJmpF, bytecode.RJmpT, bytecode.RJEqI, bytecode.RUnary,
	bytecode.RJNull, bytecode.RJNotNull, bytecode.RGetMethodW,
	bytecode.RJSEqI, bytecode.RJSEq, bytecode.RJSNe, bytecode.RJSNeI,
	bytecode.RGetProp, bytecode.RGetMethod, bytecode.RSetProp,
	bytecode.RGetIndex, bytecode.RGetIndexI, bytecode.RGetIndexPre, bytecode.RGetIndexPost,
	bytecode.RSetIndex, bytecode.RSetIndexI,
	bytecode.RTypeof, bytecode.RIn, bytecode.RGetLength,
	bytecode.RCallW, bytecode.RCallMW, bytecode.RRet, bytecode.RRetUndef,
	// The bitwise operations and the compares of no kernel's, after the
	// others.
	bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr,
	bytecode.RXorI, bytecode.RUShrI, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt,
	bytecode.RPostDec, bytecode.RIterNext, bytecode.RCallWT, bytecode.RCallMWT,
	bytecode.RCharCodeAt,
	bytecode.RAddIndex, bytecode.RAddIndexK, bytecode.RAddIndexI,
	bytecode.RGetPrivate, bytecode.RSetPrivate,
	bytecode.RYield,
	bytecode.RCloseRegs, bytecode.RClosureW, bytecode.RSwitch,
	bytecode.RBin,
	bytecode.RNewObjectLit, bytecode.RNewArrayE, bytecode.RStrFreeze,
	bytecode.RJTypeof, bytecode.RStrPart,
	bytecode.RGenEntry,
	// The step-and-branches.
	bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI,
	bytecode.RStepJLeU,
	// A product by an immediate and the int32 results, (x op y) | 0
	// (docs/register-interpreter.md, "Loop-kernel instructions").
	bytecode.RMulI, bytecode.RAddI32, bytecode.RSubI32, bytecode.RAddII32,
	// Constructions entered as calls are (new_w).
	bytecode.RNewW,
	// instanceof by ordinaryHasInstance (instanceOf).
	bytecode.RInstanceOf,
	// super() entering the parent constructor as call_w enters a
	// callee (superW).
	bytecode.RSuperCall, bytecode.RSuperForward,
	// A derived class's constructor's return value (derivedRet).
	bytecode.RDerivedRet,
	// f.apply(this, arguments) entering f (applyW).
	bytecode.RApplyArgs,
	// arguments[i] of the frame's kept arguments (argsIndex).
	bytecode.RArgsIndex,
	// A cursor's close and a destructuring's step.
	bytecode.RIterClose, bytecode.RIterStep,
	bytecode.RGetPropNext,
	bytecode.RInstanceOfNext,
	bytecode.RJBool,
	// A computed key's define by its site's keyed store cache
	// (keyedStore).
	bytecode.RDefineIndex,
}

// handlerOrder checks that order, the operations an emitter laid out
// handlers for, keeps to handlerOps: each once, in its order.
func handlerOrder(arch string, order []bytecode.ROp) error {
	i := 0
	for _, op := range order {
		for i < len(handlerOps) && handlerOps[i] != op {
			i++
		}
		if i == len(handlerOps) {
			return fmt.Errorf("nativegen: %s's handler for %s is not in handlerOps, or not in its order", arch, op)
		}
		i++
	}
	return nil
}
