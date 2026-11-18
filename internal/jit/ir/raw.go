package ir

// Typed code's raw operations (bytecode's regtypedops.go, lowered by
// compile's LowerTyped): an operation on the raw slots typed code keeps
// an int32 or a float64 in (Program's Kinds), which reads its operands
// as what their slots' kinds say and writes its result as its own kind
// says, with no check: the validator and the typed backend proved the
// kinds. An int32 is held in its slot's number word's low 32 bits, a
// float64 as its bits.
//
// A Raw instruction reads Left and Right -- a slot, or a literal number,
// an int32 constant's value or a float64's -- and writes Dest. A Branch
// whose Raw is RawBrI, RawBrU or RawBrF compares Left and Right, raw
// slots or literals, as signed or unsigned int32s or as float64s, its
// Operator and When as any Branch's.

// RawOp is a Raw instruction's operation (Instruction's Raw).
type RawOp uint8

// RawNumber, as a RawConvI's or RawConvF's Key, says its operand is a
// number: the result of typed code's call of a function of its module
// (call_w of a function binding or a table's element), every return of
// which validation proves of the function's declared type -- a signed
// int's, a double's or a float's -- however the callee runs.
const RawNumber = 1

// Slot kinds (Program's Kinds).
const (
	SlotValue uint8 = iota
	SlotI32
	SlotF64
)

const (
	RawNone RawOp = iota
	// RawIMov and RawFMov copy a raw slot's word; RawIConst and
	// RawFConst write a literal's number as an int32 or a float64.
	RawIMov
	RawFMov
	RawIConst
	RawFConst
	// The int32 operations, I = I op I: wrapping, as | 0 of JavaScript's
	// exact result; RawIMul is Math.imul's product. Division and
	// remainder are | 0's of JavaScript's results: 0 for a zero divisor,
	// MinInt32 for MinInt32 / -1; the U forms read their operands as
	// uint32s.
	RawIAdd
	RawISub
	RawIMul
	RawIDivS
	RawIDivU
	RawIRemS
	RawIRemU
	RawIAnd
	RawIOr
	RawIXor
	RawIShl
	RawIShr
	RawIUShr
	// RawIMinS and RawIMaxS are Math.min and Math.max of signed int32s.
	RawIMinS
	RawIMaxS
	// The unary int32 operations, I = op I: -, ~, ! (0 or 1),
	// Math.clz32, Math.abs of a signed int32 (MinInt32's is MinInt32).
	RawINeg
	RawINot
	RawILNot
	RawIClz
	RawIAbs
	// The int32 compares, I = I op I, 0 or 1, by Operator (Lt, Le, Eq,
	// Ne), signed, and unsigned in the U form.
	RawICmpS
	RawICmpU
	// The float64 operations, F = F op F; RawFMod is %'s; RawFMin and
	// RawFMax Math.min's and Math.max's.
	RawFAdd
	RawFSub
	RawFMul
	RawFDiv
	RawFMod
	RawFMin
	RawFMax
	// The unary float64 operations, F = op F: -, Math.abs, Math.sqrt,
	// Math.fround, Math.floor, Math.ceil.
	RawFNeg
	RawFAbs
	RawFSqrt
	RawFRound
	RawFFloor
	RawFCeil
	// RawFMath is Math function Key of the VM's (bytecode's TMath...) of
	// F Left and, for a function of two, F Right, by the engine's own
	// Math.
	RawFMath
	// The float64 compares, I = F op F, 0 or 1, by Operator (Lt, Le, Eq,
	// Ne), false of a NaN.
	RawFCmp
	// The conversions: F = I signed or unsigned; I = ToInt32(F).
	RawI2FS
	RawI2FU
	RawF2I
	// RawConvI and RawConvF are ToInt32 and ToNumber of a value slot, into
	// a raw one: a number's natively, anything else's by Go, which may run
	// code and throw -- but where their Key is RawNumber, of a value proven
	// a number, which neither converts by Go nor leaves.
	RawConvI
	RawConvF
	// RawIBox, RawUBox and RawFBox make a value slot's number of a raw
	// slot: a signed int32's, an unsigned's, a float64's (NaN canonical).
	RawIBox
	RawUBox
	RawFBox
	// RawLoad and RawStore are the heap's: the element of type Key
	// (abi's ElemInt8 ...) at the byte address int32(Left + Right) -- a
	// raw slot and a literal -- with its low bits cleared to the element's
	// size, read into Dest or written from Third; out of the heap a load
	// is 0, or NaN for a float, and a store nothing. Dest is -1 for a
	// store.
	RawLoad
	RawStore
	// RawCellGetI, RawCellGetF, RawCellSetI and RawCellSetF are a module
	// variable's: the number in the cell of captured binding Key, read as
	// an int32 (its ToInt32) or a float64 into Dest, or Left's written
	// there, a float64's NaN canonical; Dest is -1 for a write. The cell
	// holds a number throughout: validation keeps any other value from
	// it, so a write is its number word's alone.
	RawCellGetI
	RawCellGetF
	RawCellSetI
	RawCellSetF
	// The compares of a Branch's Raw.
	RawBrI
	RawBrU
	RawBrF
	// RawOps is one past the last.
	RawOps
)

// RawWritesFloat reports whether op's result is a float64.
func (op RawOp) RawWritesFloat() bool {
	switch op {
	case RawFMov, RawFConst, RawFAdd, RawFSub, RawFMul, RawFDiv, RawFMod, RawFMin, RawFMax,
		RawFNeg, RawFAbs, RawFSqrt, RawFRound, RawFFloor, RawFCeil, RawFMath, RawI2FS, RawI2FU, RawConvF, RawCellGetF:
		return true
	}
	return false
}

// RawWritesValue reports whether op's result is a value: a box's.
func (op RawOp) RawWritesValue() bool { return op == RawIBox || op == RawUBox || op == RawFBox }
