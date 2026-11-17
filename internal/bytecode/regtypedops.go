package bytecode

import "fmt"

// The typed operations: the values of the cold range (RNewRegExp ..
// RFarInit), which typed code never holds, read through the typed table
// (docs/register-interpreter.md, "The typed table"). An I register holds
// an int32 in its number word's low 32 bits, the bits above them unread;
// an F register a float64 of any NaN payload; V is a Value. A float32 is
// an F holding a float64 that is a float32's value.
//
// The int operations wrap to 32 bits: asm.js lets an intish result reach
// only a coercion or a bitwise operator, whose ToInt32 of JavaScript's
// exact result is its low 32 bits. Signed and unsigned are the same bits,
// told apart only by the operations that read them so: the compares,
// division and remainder, and the conversions to a double.
const (
	// TMov: R[A] = R[B], a raw word, I or F.
	TMov ROp = RNewRegExp + iota
	// TIConst: I R[A] = int32(BC).
	TIConst
	// TFConst: F R[A] = Floats[BC].
	TFConst
	// TConvI: I R[A] = ToInt32(V R[B]), which may run code and throw.
	TConvI
	// TConvF: F R[A] = ToNumber(V R[B]), which may run code and throw.
	TConvF
	// TIBox: V R[A] = I R[B] as a signed int32; TUBox as a uint32; TFBox:
	// V R[A] = F R[B], its NaN canonical.
	TIBox
	TUBox
	TFBox
	// The int arithmetic and bitwise operations: I R[A] = R[B] op R[C].
	// TDivS, TDivU, TRemS and TRemU of 0 are 0, and MinInt32 / -1 is
	// MinInt32: what | 0 makes of JavaScript's results.
	TAdd
	TSub
	TMul
	TDivS
	TDivU
	TRemS
	TRemU
	TAnd
	TOr
	TXor
	TShl
	TShr
	TUShr
	// The immediate forms: I R[A] = R[B] op SC.
	TAddI
	TAndI
	TOrI
	TXorI
	TShlI
	TShrI
	TUShrI
	// The unary int operations: I R[A] = op R[B]: -, ~, !, Math.clz32,
	// Math.abs (of a signed, a uint32's bits).
	TNeg
	TNot
	TLNot
	TClz
	TAbs
	// TMinS and TMaxS: I R[A] = Math.min or Math.max of signed R[B], R[C].
	TMinS
	TMaxS
	// The int compares: I R[A] = R[B] op R[C], 0 or 1, signed or unsigned.
	TLtS
	TLeS
	TLtU
	TLeU
	TEq
	TNe
	// The int compare-and-branches: if R[A] op R[B], go SC on.
	TJLtS
	TJLeS
	TJLtU
	TJLeU
	TJEq
	TJNe
	// The immediate ones: if R[A] op SB, go SC on, signed.
	TJLtSI
	TJLeSI
	TJGtSI
	TJGeSI
	TJEqI
	TJNeI
	// TJZ and TJNZ: if R[A] == 0 (!= 0), go SC on.
	TJZ
	TJNZ
	// The float arithmetic: F R[A] = R[B] op R[C]; TFMod is %'s fmod.
	TFAdd
	TFSub
	TFMul
	TFDiv
	TFMod
	// The unary float operations: F R[A] = op R[B]; TFRound is
	// Math.fround.
	TFNeg
	TFAbs
	TFSqrt
	TFRound
	// TFMath1: F R[A] = Math function X of R[B] (TMathFloor ...);
	// TFMath2: F R[A] = Math function X of R[B], R[C] (TMathPow,
	// TMathAtan2), by the engine's own Math.
	TFMath1
	TFMath2
	// TFMin and TFMax: F R[A] = Math.min or Math.max of R[B], R[C].
	TFMin
	TFMax
	// The float compares: I R[A] = R[B] op R[C], 0 or 1, false of a NaN.
	TFLt
	TFLe
	TFEq
	TFNe
	// The float compare-and-branches: if R[A] op R[B], go SC on; the
	// negations jump where a NaN makes the compare false.
	TJFLt
	TJFNLt
	TJFLe
	TJFNLe
	TJFEq
	TJFNe
	// The conversions: F R[A] = signed or unsigned R[B]; I R[A] =
	// ToInt32(F R[B]).
	TI2FS
	TI2FU
	TF2I
	// The heap's loads: R[A] = the element at byte address (int32(R[B] +
	// SC) >> shift) << shift, an int sign- or zero-extended into I, a
	// float into F; out of the heap 0, or NaN for a float.
	TLd8S
	TLd8U
	TLd16S
	TLd16U
	TLd32
	TLdF32
	TLdF64
	// The heap's stores: the element at that address = R[A], an int's low
	// bits or a float rounded; out of the heap, nothing.
	TSt8
	TSt16
	TSt32
	TStF32
	TStF64
	// The module's globals, numbers in the upvalue cells of the function's
	// upvalue BC: TGGetI: I R[A] = the int; TGSetI: the cell = I R[A] as a
	// signed int32; TGGetF and TGSetF a double's, its NaN canonical.
	TGGetI
	TGSetI
	TGGetF
	TGSetF
	// TTabGet: V R[A] = the element R[B] & (n - 1) of the function table
	// of n functions in upvalue C.
	TTabGet
	// TOpEnd is past the last typed operation.
	TOpEnd
)

// The Math functions of TFMath1 and TFMath2, by X.
const (
	TMathFloor = iota
	TMathCeil
	TMathSin
	TMathCos
	TMathTan
	TMathAsin
	TMathAcos
	TMathAtan
	TMathExp
	TMathLog
	TMathPow
	TMathAtan2
)

// TMathNames are the Math functions' names, by X.
var TMathNames = [...]string{"floor", "ceil", "sin", "cos", "tan", "asin", "acos", "atan", "exp", "log", "pow", "atan2"}

var _ [RFarInit - TOpEnd + 1]struct{} // the typed operations fit the cold range

var typedNames = map[ROp]string{
	TMov: "tmov", TIConst: "ticonst", TFConst: "tfconst", TConvI: "tconv_i", TConvF: "tconv_f",
	TIBox: "tibox", TUBox: "tubox", TFBox: "tfbox",
	TAdd: "tadd", TSub: "tsub", TMul: "tmul", TDivS: "tdiv_s", TDivU: "tdiv_u", TRemS: "trem_s", TRemU: "trem_u",
	TAnd: "tand", TOr: "tor", TXor: "txor", TShl: "tshl", TShr: "tshr", TUShr: "tushr",
	TAddI: "tadd_imm", TAndI: "tand_imm", TOrI: "tor_imm", TXorI: "txor_imm", TShlI: "tshl_imm", TShrI: "tshr_imm", TUShrI: "tushr_imm",
	TNeg: "tneg", TNot: "tnot", TLNot: "tlnot", TClz: "tclz", TAbs: "tabs", TMinS: "tmin_s", TMaxS: "tmax_s",
	TLtS: "tlt_s", TLeS: "tle_s", TLtU: "tlt_u", TLeU: "tle_u", TEq: "teq", TNe: "tne",
	TJLtS: "tjlt_s", TJLeS: "tjle_s", TJLtU: "tjlt_u", TJLeU: "tjle_u", TJEq: "tjeq", TJNe: "tjne",
	TJLtSI: "tjlt_imm", TJLeSI: "tjle_imm", TJGtSI: "tjgt_imm", TJGeSI: "tjge_imm", TJEqI: "tjeq_imm", TJNeI: "tjne_imm",
	TJZ: "tjz", TJNZ: "tjnz",
	TFAdd: "tfadd", TFSub: "tfsub", TFMul: "tfmul", TFDiv: "tfdiv", TFMod: "tfmod",
	TFNeg: "tfneg", TFAbs: "tfabs", TFSqrt: "tfsqrt", TFRound: "tfround", TFMath1: "tfmath1", TFMath2: "tfmath2",
	TFMin: "tfmin", TFMax: "tfmax", TFLt: "tflt", TFLe: "tfle", TFEq: "tfeq", TFNe: "tfne",
	TJFLt: "tjflt", TJFNLt: "tjfnlt", TJFLe: "tjfle", TJFNLe: "tjfnle", TJFEq: "tjfeq", TJFNe: "tjfne",
	TI2FS: "ti2f_s", TI2FU: "ti2f_u", TF2I: "tf2i",
	TLd8S: "tld8_s", TLd8U: "tld8_u", TLd16S: "tld16_s", TLd16U: "tld16_u", TLd32: "tld32", TLdF32: "tldf32", TLdF64: "tldf64",
	TSt8: "tst8", TSt16: "tst16", TSt32: "tst32", TStF32: "tstf32", TStF64: "tstf64",
	TGGetI: "tgget_i", TGSetI: "tgset_i", TGGetF: "tgget_f", TGSetF: "tgset_f", TTabGet: "ttab_get",
}

// Typed reports whether op is a typed operation.
func (op ROp) Typed() bool { return op >= TMov && op < TOpEnd }

// TypedName is the typed operation op's name.
func TypedName(op ROp) string { return typedNames[op] }

// OpName is op's name in rc: a typed operation's in typed code, whose
// values are the cold range's.
func (rc *RegCode) OpName(op ROp) string {
	if rc.Typed && op.Typed() {
		return typedNames[op]
	}
	return op.String()
}

// TypedHeapShift is the heap access op's element size's log2, and
// whether op is one.
func TypedHeapShift(op ROp) (uint8, bool) {
	switch op {
	case TLd8S, TLd8U, TSt8:
		return 0, true
	case TLd16S, TLd16U, TSt16:
		return 1, true
	case TLd32, TLdF32, TSt32, TStF32:
		return 2, true
	case TLdF64, TStF64:
		return 3, true
	}
	return 0, false
}

// formatTyped renders typed instruction w at pc.
func (rc *RegCode) formatTyped(pc int, w RInstr) string {
	op := w.Op()
	n := typedNames[op]
	r := func(x uint16) string { return fmt.Sprintf("r%d", x) }
	target := func(off int32) string { return fmt.Sprintf("-> %d", pc+1+int(off)) }
	switch op {
	case TIConst:
		return fmt.Sprintf("%s %s, %d", n, r(w.A()), int32(w.BC()))
	case TFConst:
		v := 0.0
		if int(w.BC()) < len(rc.Floats) {
			v = rc.Floats[w.BC()]
		}
		return fmt.Sprintf("%s %s, %v", n, r(w.A()), v)
	case TAddI, TAndI, TOrI, TXorI, TShlI, TShrI, TUShrI:
		return fmt.Sprintf("%s %s, %s, %d", n, r(w.A()), r(w.B()), w.SC())
	case TJLtS, TJLeS, TJLtU, TJLeU, TJEq, TJNe, TJFLt, TJFNLt, TJFLe, TJFNLe, TJFEq, TJFNe:
		return fmt.Sprintf("%s %s, %s %s", n, r(w.A()), r(w.B()), target(w.SC()))
	case TJLtSI, TJLeSI, TJGtSI, TJGeSI, TJEqI, TJNeI:
		return fmt.Sprintf("%s %s, %d %s", n, r(w.A()), w.SB(), target(w.SC()))
	case TJZ, TJNZ:
		return fmt.Sprintf("%s %s %s", n, r(w.A()), target(w.SC()))
	case TLd8S, TLd8U, TLd16S, TLd16U, TLd32, TLdF32, TLdF64:
		return fmt.Sprintf("%s %s, [%s%+d]", n, r(w.A()), r(w.B()), w.SC())
	case TSt8, TSt16, TSt32, TStF32, TStF64:
		return fmt.Sprintf("%s [%s%+d], %s", n, r(w.B()), w.SC(), r(w.A()))
	case TGGetI, TGGetF:
		return fmt.Sprintf("%s %s, u%d", n, r(w.A()), w.BC())
	case TGSetI, TGSetF:
		return fmt.Sprintf("%s u%d, %s", n, w.BC(), r(w.A()))
	case TTabGet:
		return fmt.Sprintf("%s %s, u%d[%s]", n, r(w.A()), w.C(), r(w.B()))
	case TFMath1:
		return fmt.Sprintf("%s %s, %s(%s)", n, r(w.A()), TMathNames[w.X()%uint8(len(TMathNames))], r(w.B()))
	case TFMath2:
		return fmt.Sprintf("%s %s, %s(%s, %s)", n, r(w.A()), TMathNames[w.X()%uint8(len(TMathNames))], r(w.B()), r(w.C()))
	case TMov, TConvI, TConvF, TIBox, TUBox, TFBox, TNeg, TNot, TLNot, TClz, TAbs,
		TFNeg, TFAbs, TFSqrt, TFRound, TI2FS, TI2FU, TF2I:
		return fmt.Sprintf("%s %s, %s", n, r(w.A()), r(w.B()))
	}
	return fmt.Sprintf("%s %s, %s, %s", n, r(w.A()), r(w.B()), r(w.C()))
}
