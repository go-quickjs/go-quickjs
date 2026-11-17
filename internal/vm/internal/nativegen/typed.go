package nativegen

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// The typed dispatcher (the typed variant): the table typed register code
// is decoded through (package vm's zzreg_zzty.go). Its entries of the
// ordinary operations typed code holds (bytecode.TypedTableOps) are
// plain's handlers, call_w's among them, so that a call between two typed
// functions is plain's call handler's, its callee's frame decoded by the
// same table; every other entry is the exit stub, whose Go handler is the
// typed table's. It is in every build of amd64 and arm64, with the JIT
// or without, but the one that reads the layout.
//
// The call path's prototype has no handler of its own: its table proves
// the switch of tables, which the raw operations' handlers then use.

// typedNativeOps are the typed operations with native handlers, in the
// order the emitters lay them out; every other's entry is the exit stub,
// its Go handler the typed table's.
var typedNativeOps = []bytecode.ROp{
	bytecode.TMov, bytecode.TIConst, bytecode.TFConst, bytecode.TConvI, bytecode.TConvF,
	bytecode.TIBox, bytecode.TUBox, bytecode.TFBox,
	bytecode.TAdd, bytecode.TSub, bytecode.TMul, bytecode.TDivS, bytecode.TDivU, bytecode.TRemS, bytecode.TRemU,
	bytecode.TAnd, bytecode.TOr, bytecode.TXor, bytecode.TShl, bytecode.TShr, bytecode.TUShr,
	bytecode.TAddI, bytecode.TAndI, bytecode.TOrI, bytecode.TXorI, bytecode.TShlI, bytecode.TShrI, bytecode.TUShrI,
	bytecode.TNeg, bytecode.TNot, bytecode.TLNot,
	bytecode.TLtS, bytecode.TLeS, bytecode.TLtU, bytecode.TLeU, bytecode.TEq, bytecode.TNe,
	bytecode.TJLtS, bytecode.TJLeS, bytecode.TJLtU, bytecode.TJLeU, bytecode.TJEq, bytecode.TJNe,
	bytecode.TJLtSI, bytecode.TJLeSI, bytecode.TJGtSI, bytecode.TJGeSI, bytecode.TJEqI, bytecode.TJNeI,
	bytecode.TJZ, bytecode.TJNZ,
	bytecode.TFAdd, bytecode.TFSub, bytecode.TFMul, bytecode.TFDiv,
	bytecode.TFNeg, bytecode.TFAbs, bytecode.TFSqrt, bytecode.TFRound,
	bytecode.TFLt, bytecode.TFLe, bytecode.TFEq, bytecode.TFNe,
	bytecode.TJFLt, bytecode.TJFNLt, bytecode.TJFLe, bytecode.TJFNLe, bytecode.TJFEq, bytecode.TJFNe,
	bytecode.TI2FS, bytecode.TI2FU, bytecode.TF2I,
	bytecode.TLd8S, bytecode.TLd8U, bytecode.TLd16S, bytecode.TLd16U, bytecode.TLd32, bytecode.TLdF32, bytecode.TLdF64,
	bytecode.TSt8, bytecode.TSt16, bytecode.TSt32, bytecode.TStF32, bytecode.TStF64,
	bytecode.TGGetI, bytecode.TGGetF, bytecode.TGSetI, bytecode.TGSetF,
}

// The offsets the typed handlers read besides plain's: a typed context's
// heap, past its nativeCtx, and a typed regCode's float constants, past
// its regCode.
var ncHeapBase, ncHeapLen, offRCFloats int32

func loadTypedOffsets() {
	ncHeapBase = off("nativeTypedCtx.heapBase")
	ncHeapLen = off("nativeTypedCtx.heapLen")
	offRCFloats = off("regTypedCode.floats")
}

// typedBlock is the typed variant's own block of code, its handlers at
// at.
func typedBlock(v *variant, code []byte, at map[bytecode.ROp]int) *block {
	if at == nil {
		at = map[bytecode.ROp]int{}
	}
	b := &block{at: at, code: code}
	for _, op := range handlerOps {
		if _, ok := at[op]; ok && v.has(op) {
			b.order = append(b.order, op)
		}
	}
	return b
}
