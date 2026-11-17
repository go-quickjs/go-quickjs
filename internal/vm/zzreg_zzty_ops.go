package vm

import (
	"math"
	"math/bits"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/fdlibm"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// The typed operations' Go handlers (bytecode's regtypedops.go): the typed
// table's for the Go loop, and its exits' where native code leaves one to
// Go. An I register's number word holds an int32 in its low 32 bits (Go
// writes it sign-extended, native code zero-extended, and neither reads
// the bits above), an F register's a float64; a raw operation never
// writes a register's pointer word, which a box alone, into a Value
// register, writes as a Value's.

func init() {
	for op, h := range map[bytecode.ROp]regHandler{
		bytecode.TMov: rhTMov, bytecode.TIConst: rhTIConst, bytecode.TFConst: rhTFConst,
		bytecode.TConvI: rhTConvI, bytecode.TConvF: rhTConvF,
		bytecode.TIBox: rhTIBox, bytecode.TUBox: rhTUBox, bytecode.TFBox: rhTFBox,
		bytecode.TAdd:    rhTAdd,
		bytecode.TSub:    rhTSub,
		bytecode.TMul:    rhTMul,
		bytecode.TAnd:    rhTAnd,
		bytecode.TOr:     rhTOr,
		bytecode.TXor:    rhTXor,
		bytecode.TShl:    rhTShl,
		bytecode.TShr:    rhTShr,
		bytecode.TUShr:   rhTUShr,
		bytecode.TMinS:   rhTMinS,
		bytecode.TMaxS:   rhTMaxS,
		bytecode.TLtS:    rhTLtS,
		bytecode.TLeS:    rhTLeS,
		bytecode.TLtU:    rhTLtU,
		bytecode.TLeU:    rhTLeU,
		bytecode.TEq:     rhTEq,
		bytecode.TNe:     rhTNe,
		bytecode.TDivS:   rhTDivS,
		bytecode.TDivU:   rhTDivU,
		bytecode.TRemS:   rhTRemS,
		bytecode.TRemU:   rhTRemU,
		bytecode.TAddI:   rhTAddI,
		bytecode.TAndI:   rhTAndI,
		bytecode.TOrI:    rhTOrI,
		bytecode.TXorI:   rhTXorI,
		bytecode.TShlI:   rhTShlI,
		bytecode.TShrI:   rhTShrI,
		bytecode.TUShrI:  rhTUShrI,
		bytecode.TNeg:    rhTNeg,
		bytecode.TNot:    rhTNot,
		bytecode.TLNot:   rhTLNot,
		bytecode.TClz:    rhTClz,
		bytecode.TAbs:    rhTAbs,
		bytecode.TFAdd:   rhTFAdd,
		bytecode.TFSub:   rhTFSub,
		bytecode.TFMul:   rhTFMul,
		bytecode.TFDiv:   rhTFDiv,
		bytecode.TFMod:   rhTFMod,
		bytecode.TFMin:   rhTFMin,
		bytecode.TFMax:   rhTFMax,
		bytecode.TFMath2: rhTFMath2,
		bytecode.TFNeg:   rhTFNeg,
		bytecode.TFAbs:   rhTFAbs,
		bytecode.TFSqrt:  rhTFSqrt,
		bytecode.TFRound: rhTFRound,
		bytecode.TFMath1: rhTFMath1,
		bytecode.TI2FS:   rhTI2FS,
		bytecode.TI2FU:   rhTI2FU,
		bytecode.TF2I:    rhTF2I,
		bytecode.TFLt:    rhTFLt,
		bytecode.TFLe:    rhTFLe,
		bytecode.TFEq:    rhTFEq,
		bytecode.TFNe:    rhTFNe,
		bytecode.TJLtS:   rhTJLtS,
		bytecode.TJLeS:   rhTJLeS,
		bytecode.TJLtU:   rhTJLtU,
		bytecode.TJLeU:   rhTJLeU,
		bytecode.TJEq:    rhTJEq,
		bytecode.TJNe:    rhTJNe,
		bytecode.TJLtSI:  rhTJLtSI,
		bytecode.TJLeSI:  rhTJLeSI,
		bytecode.TJGtSI:  rhTJGtSI,
		bytecode.TJGeSI:  rhTJGeSI,
		bytecode.TJEqI:   rhTJEqI,
		bytecode.TJNeI:   rhTJNeI,
		bytecode.TJZ:     rhTJZ,
		bytecode.TJNZ:    rhTJNZ,
		bytecode.TJFLt:   rhTJFLt,
		bytecode.TJFNLt:  rhTJFNLt,
		bytecode.TJFLe:   rhTJFLe,
		bytecode.TJFNLe:  rhTJFNLe,
		bytecode.TJFEq:   rhTJFEq,
		bytecode.TJFNe:   rhTJFNe,
		bytecode.TLd8S:   rhTLd8S,
		bytecode.TLd8U:   rhTLd8U,
		bytecode.TLd16S:  rhTLd16S,
		bytecode.TLd16U:  rhTLd16U,
		bytecode.TLd32:   rhTLd32,
		bytecode.TLdF32:  rhTLdF32,
		bytecode.TLdF64:  rhTLdF64,
		bytecode.TSt8:    rhTSt8,
		bytecode.TSt16:   rhTSt16,
		bytecode.TSt32:   rhTSt32,
		bytecode.TStF32:  rhTStF32,
		bytecode.TStF64:  rhTStF64,
		bytecode.TGGetI:  rhTGGetI,
		bytecode.TGGetF:  rhTGGetF,
		bytecode.TGSetI:  rhTGSetI,
		bytecode.TGSetF:  rhTGSetF,
		bytecode.TTabGet: rhTTabGet,
	} {
		regTypedTable[op], regTypedSlowTable[op] = h, rhInvalid
	}
	regTypedSlowTable[bytecode.TConvI] = rsTConvI
	regTypedSlowTable[bytecode.TConvF] = rsTConvF
	regTypedSlowTable[bytecode.TIBox] = rsTBox
	regTypedSlowTable[bytecode.TUBox] = rsTBox
	regTypedSlowTable[bytecode.TFBox] = rsTBox
	for _, op := range []bytecode.ROp{bytecode.TJLtS, bytecode.TJLeS, bytecode.TJLtU, bytecode.TJLeU, bytecode.TJEq,
		bytecode.TJNe, bytecode.TJLtSI, bytecode.TJLeSI, bytecode.TJGtSI, bytecode.TJGeSI, bytecode.TJEqI, bytecode.TJNeI,
		bytecode.TJZ, bytecode.TJNZ, bytecode.TJFLt, bytecode.TJFNLt, bytecode.TJFLe, bytecode.TJFNLe, bytecode.TJFEq,
		bytecode.TJFNe} {
		regTypedSlowTable[op] = rsTJump
	}
	for _, op := range []bytecode.ROp{bytecode.TGGetI, bytecode.TGSetI, bytecode.TGGetF, bytecode.TGSetF} {
		regTypedSlowTable[op] = rsTGlobal
	}
	// The engine's own, as Math's functions compute them (unaryMathFuncs).
	for x, name := range bytecode.TMathNames[:bytecode.TMathLog+1] {
		tMath1[x] = unaryMathFuncs[name]
	}
}

// tMath1 is TFMath1's functions, by X.
var tMath1 [bytecode.TMathLog + 1]func(float64) float64

// ti is I register r's int32.
func ti(regs *[1 << 16]Value, r uint16) int32 {
	return int32(*(*int64)(unsafe.Pointer(&regs[r].num)))
}

// tset makes I register r hold v, its pointer word left as it is.
func tset(regs *[1 << 16]Value, r uint16, v int32) {
	*(*int64)(unsafe.Pointer(&regs[r].num)) = int64(v)
}

func rhTMov(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()].num = regs[w.B()].num
	return pc + 1
}

func rhTIConst(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	tset(regs, w.A(), int32(w.BC()))
	return pc + 1
}

func rhTFConst(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()].num = st.c.rc.Floats[w.BC()]
	return pc + 1
}

// rhTConvI is ToInt32 of a Value into an I register: a number's here,
// anything else's -- valueOf, its throw -- by rsTConvI.
func rhTConvI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if v := regs[w.B()]; v.IsNumber() {
		tset(regs, w.A(), jsnum.ToInt32(v.num))
		return pc + 1
	}
	return ^pc
}

func rsTConvI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	n, err := st.r.toInt32(regs[w.B()])
	if err != nil {
		return st.fail(err)
	}
	tset(regs, w.A(), n)
	return pc + 1
}

func rhTConvF(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if v := regs[w.B()]; v.IsNumber() {
		regs[w.A()].num = v.num
		return pc + 1
	}
	return ^pc
}

func rsTConvF(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	n, err := st.r.toNumber(regs[w.B()])
	if err != nil {
		return st.fail(err)
	}
	regs[w.A()].num = n
	return pc + 1
}

// The boxes: a raw word made a Value, into a register that holds no
// pointer, its number word alone written; any other is rsTBox's.

func rhTIBox(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if p := &regs[w.A()]; p.ref == nil {
		p.num = float64(ti(regs, w.B()))
		return pc + 1
	}
	return ^pc
}

func rhTUBox(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if p := &regs[w.A()]; p.ref == nil {
		p.num = float64(uint32(ti(regs, w.B())))
		return pc + 1
	}
	return ^pc
}

func rhTFBox(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if p := &regs[w.A()]; p.ref == nil {
		p.num = regNum(regs[w.B()].num).num
		return pc + 1
	}
	return ^pc
}

// rsTBox is a box into a register that holds a pointer: the Value stored
// whole, through the write barrier.
func rsTBox(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	switch w.Op() {
	case bytecode.TIBox:
		regs[w.A()] = Int32(ti(regs, w.B()))
	case bytecode.TUBox:
		regs[w.A()] = Value{num: float64(uint32(ti(regs, w.B())))}
	default:
		regs[w.A()] = regNum(regs[w.B()].num)
	}
	return pc + 1
}

func b2i(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

// tAbs is Math.abs of a signed int32, a uint32's bits: MinInt32's own.
func tAbs(a int32) int32 {
	if a < 0 {
		return -a
	}
	return a
}

// rsTJump is a compare-and-branch's slow function: its backward jump,
// taken, whose budget is spent.
func rsTJump(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return regJumpSlow(st, pc, w.SC())
}

// The arithmetic, the compares, the conversions and the branches, each
// operation a handler of its own, as each has a native one.

func rhTAdd(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), a+b)
	return pc + 1
}

func rhTSub(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), a-b)
	return pc + 1
}

func rhTMul(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), a*b)
	return pc + 1
}

func rhTAnd(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), a&b)
	return pc + 1
}

func rhTOr(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), a|b)
	return pc + 1
}

func rhTXor(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), a^b)
	return pc + 1
}

func rhTShl(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), a<<(uint32(b)&31))
	return pc + 1
}

func rhTShr(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), a>>(uint32(b)&31))
	return pc + 1
}

func rhTUShr(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), int32(uint32(a)>>(uint32(b)&31)))
	return pc + 1
}

func rhTMinS(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), min(a, b))
	return pc + 1
}

func rhTMaxS(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), max(a, b))
	return pc + 1
}

func rhTLtS(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), b2i(a < b))
	return pc + 1
}

func rhTLeS(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), b2i(a <= b))
	return pc + 1
}

func rhTLtU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), b2i(uint32(a) < uint32(b)))
	return pc + 1
}

func rhTLeU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), b2i(uint32(a) <= uint32(b)))
	return pc + 1
}

func rhTEq(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), b2i(a == b))
	return pc + 1
}

func rhTNe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	tset(regs, w.A(), b2i(a != b))
	return pc + 1
}

func rhTDivS(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	v := int32(0)
	switch b {
	case 0:
	case -1:
		v = -a
	default:
		v = a / b
	}
	tset(regs, w.A(), v)
	return pc + 1
}

func rhTDivU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := uint32(ti(regs, w.B())), uint32(ti(regs, w.C()))
	v := uint32(0)
	if b != 0 {
		v = a / b
	}
	tset(regs, w.A(), int32(v))
	return pc + 1
}

func rhTRemS(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.B()), ti(regs, w.C())
	v := int32(0)
	if b != 0 && b != -1 {
		v = a % b
	}
	tset(regs, w.A(), v)
	return pc + 1
}

func rhTRemU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := uint32(ti(regs, w.B())), uint32(ti(regs, w.C()))
	v := uint32(0)
	if b != 0 {
		v = a % b
	}
	tset(regs, w.A(), int32(v))
	return pc + 1
}

func rhTAddI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.B()), w.SC()
	tset(regs, w.A(), a+k)
	return pc + 1
}

func rhTAndI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.B()), w.SC()
	tset(regs, w.A(), a&k)
	return pc + 1
}

func rhTOrI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.B()), w.SC()
	tset(regs, w.A(), a|k)
	return pc + 1
}

func rhTXorI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.B()), w.SC()
	tset(regs, w.A(), a^k)
	return pc + 1
}

func rhTShlI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.B()), w.SC()
	tset(regs, w.A(), a<<(uint32(k)&31))
	return pc + 1
}

func rhTShrI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.B()), w.SC()
	tset(regs, w.A(), a>>(uint32(k)&31))
	return pc + 1
}

func rhTUShrI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.B()), w.SC()
	tset(regs, w.A(), int32(uint32(a)>>(uint32(k)&31)))
	return pc + 1
}

func rhTNeg(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a := ti(regs, w.B())
	tset(regs, w.A(), -a)
	return pc + 1
}

func rhTNot(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a := ti(regs, w.B())
	tset(regs, w.A(), ^a)
	return pc + 1
}

func rhTLNot(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a := ti(regs, w.B())
	tset(regs, w.A(), b2i(a == 0))
	return pc + 1
}

func rhTClz(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a := ti(regs, w.B())
	tset(regs, w.A(), int32(bits.LeadingZeros32(uint32(a))))
	return pc + 1
}

func rhTAbs(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a := ti(regs, w.B())
	tset(regs, w.A(), tAbs(a))
	return pc + 1
}

func rhTFAdd(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	regs[w.A()].num = x + y
	return pc + 1
}

func rhTFSub(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	regs[w.A()].num = x - y
	return pc + 1
}

func rhTFMul(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	regs[w.A()].num = x * y
	return pc + 1
}

func rhTFDiv(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	regs[w.A()].num = x / y
	return pc + 1
}

func rhTFMod(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	regs[w.A()].num = math.Mod(x, y)
	return pc + 1
}

func rhTFMin(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	regs[w.A()].num = extremum2(x, y, false)
	return pc + 1
}

func rhTFMax(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	regs[w.A()].num = extremum2(x, y, true)
	return pc + 1
}

func rhTFMath2(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	if w.X() == bytecode.TMathPow {
		regs[w.A()].num = jsPow(x, y)
	} else {
		regs[w.A()].num = fdlibm.Atan2(x, y)
	}
	return pc + 1
}

func rhTFNeg(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x := regs[w.B()].num
	regs[w.A()].num = -x
	return pc + 1
}

func rhTFAbs(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x := regs[w.B()].num
	regs[w.A()].num = math.Abs(x)
	return pc + 1
}

func rhTFSqrt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x := regs[w.B()].num
	regs[w.A()].num = math.Sqrt(x)
	return pc + 1
}

func rhTFRound(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x := regs[w.B()].num
	regs[w.A()].num = float64(float32(x))
	return pc + 1
}

func rhTFMath1(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x := regs[w.B()].num
	regs[w.A()].num = tMath1[w.X()](x)
	return pc + 1
}

func rhTI2FS(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()].num = float64(ti(regs, w.B()))
	return pc + 1
}

func rhTI2FU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()].num = float64(uint32(ti(regs, w.B())))
	return pc + 1
}

func rhTF2I(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	tset(regs, w.A(), jsnum.ToInt32(regs[w.B()].num))
	return pc + 1
}

func rhTFLt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	tset(regs, w.A(), b2i(x < y))
	return pc + 1
}

func rhTFLe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	tset(regs, w.A(), b2i(x <= y))
	return pc + 1
}

func rhTFEq(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	tset(regs, w.A(), b2i(x == y))
	return pc + 1
}

func rhTFNe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.B()].num, regs[w.C()].num
	tset(regs, w.A(), b2i(x != y))
	return pc + 1
}

func rhTJLtS(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.A()), ti(regs, w.B())
	if a < b {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJLeS(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.A()), ti(regs, w.B())
	if a <= b {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJLtU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.A()), ti(regs, w.B())
	if uint32(a) < uint32(b) {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJLeU(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.A()), ti(regs, w.B())
	if uint32(a) <= uint32(b) {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJEq(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.A()), ti(regs, w.B())
	if a == b {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJNe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, b := ti(regs, w.A()), ti(regs, w.B())
	if a != b {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJLtSI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.A()), w.SB()
	if a < k {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJLeSI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.A()), w.SB()
	if a <= k {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJGtSI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.A()), w.SB()
	if a > k {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJGeSI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.A()), w.SB()
	if a >= k {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJEqI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.A()), w.SB()
	if a == k {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJNeI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	a, k := ti(regs, w.A()), w.SB()
	if a != k {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJZ(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if ti(regs, w.A()) == 0 {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJNZ(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	if ti(regs, w.A()) != 0 {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJFLt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.A()].num, regs[w.B()].num
	if x < y {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJFNLt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.A()].num, regs[w.B()].num
	if !(x < y) {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJFLe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.A()].num, regs[w.B()].num
	if x <= y {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJFNLe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.A()].num, regs[w.B()].num
	if !(x <= y) {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJFEq(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.A()].num, regs[w.B()].num
	if x == y {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

func rhTJFNe(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	x, y := regs[w.A()].num, regs[w.B()].num
	if x != y {
		return regJump(st, pc, w.SC())
	}
	return pc + 1
}

//go:nocheckptr
func rhTLd8S(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 0); uint64(a) < uint64(h.len) {
		p := unsafe.Add(h.base, a)
		tset(regs, w.A(), int32(*(*int8)(p)))
	} else {
		tset(regs, w.A(), 0)
	}
	return pc + 1
}

//go:nocheckptr
func rhTLd8U(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 0); uint64(a) < uint64(h.len) {
		p := unsafe.Add(h.base, a)
		tset(regs, w.A(), int32(*(*uint8)(p)))
	} else {
		tset(regs, w.A(), 0)
	}
	return pc + 1
}

//go:nocheckptr
func rhTLd16S(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 1); uint64(a) < uint64(h.len) {
		p := unsafe.Add(h.base, a)
		tset(regs, w.A(), int32(*(*int16)(p)))
	} else {
		tset(regs, w.A(), 0)
	}
	return pc + 1
}

//go:nocheckptr
func rhTLd16U(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 1); uint64(a) < uint64(h.len) {
		p := unsafe.Add(h.base, a)
		tset(regs, w.A(), int32(*(*uint16)(p)))
	} else {
		tset(regs, w.A(), 0)
	}
	return pc + 1
}

//go:nocheckptr
func rhTLd32(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 2); uint64(a) < uint64(h.len) {
		p := unsafe.Add(h.base, a)
		tset(regs, w.A(), *(*int32)(p))
	} else {
		tset(regs, w.A(), 0)
	}
	return pc + 1
}

//go:nocheckptr
func rhTLdF32(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 2); uint64(a) < uint64(h.len) {
		p := unsafe.Add(h.base, a)
		regs[w.A()].num = float64(*(*float32)(p))
	} else {
		regs[w.A()].num = math.NaN()
	}
	return pc + 1
}

//go:nocheckptr
func rhTLdF64(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 3); uint64(a) < uint64(h.len) {
		p := unsafe.Add(h.base, a)
		regs[w.A()].num = *(*float64)(p)
	} else {
		regs[w.A()].num = math.NaN()
	}
	return pc + 1
}

//go:nocheckptr
func rhTSt8(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 0); uint64(a) < uint64(h.len) {
		*(*int8)(unsafe.Add(h.base, a)) = int8(ti(regs, w.A()))
	}
	return pc + 1
}

//go:nocheckptr
func rhTSt16(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 1); uint64(a) < uint64(h.len) {
		*(*int16)(unsafe.Add(h.base, a)) = int16(ti(regs, w.A()))
	}
	return pc + 1
}

//go:nocheckptr
func rhTSt32(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 2); uint64(a) < uint64(h.len) {
		*(*int32)(unsafe.Add(h.base, a)) = ti(regs, w.A())
	}
	return pc + 1
}

//go:nocheckptr
func rhTStF32(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 2); uint64(a) < uint64(h.len) {
		*(*float32)(unsafe.Add(h.base, a)) = float32(regs[w.A()].num)
	}
	return pc + 1
}

//go:nocheckptr
func rhTStF64(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	h := st.c.typedExt().heap
	h.at(st.r)
	if a := tAddr(regs, w, 3); uint64(a) < uint64(h.len) {
		*(*float64)(unsafe.Add(h.base, a)) = regs[w.A()].num
	}
	return pc + 1
}

func rhTGGetI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	cell := st.cl.upvalues[w.BC()].slot
	if !cell.IsNumber() {
		return ^pc
	}
	tset(regs, w.A(), jsnum.ToInt32(cell.num))
	return pc + 1
}

func rhTGGetF(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	cell := st.cl.upvalues[w.BC()].slot
	if !cell.IsNumber() {
		return ^pc
	}
	regs[w.A()].num = cell.num
	return pc + 1
}

func rhTGSetI(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	cell := st.cl.upvalues[w.BC()].slot
	if cell.ref != nil {
		return ^pc
	}
	cell.num = float64(ti(regs, w.A()))
	return pc + 1
}

func rhTGSetF(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	cell := st.cl.upvalues[w.BC()].slot
	if cell.ref != nil {
		return ^pc
	}
	cell.num = regNum(regs[w.A()].num).num
	return pc + 1
}

// asmHeap is a linked asm.js function's heap: a view of its buffer, whose
// bytes it reads as of the runtime's typedEpoch -- a buffer detached or
// transferred has none, every access out of it.
type asmHeap struct {
	view *typedArrayData
	size uintptr
	base unsafe.Pointer
	len  uintptr
	// epoch is the runtime's typedEpoch base and len are as of, plus one:
	// 0 is never current.
	epoch uint64
}

// asmNoHeap is the base of a heap of no bytes: not nil, so that no sum
// of it is nil.
var asmNoHeap [8]byte

// at makes h current, as of r's typedEpoch.
func (h *asmHeap) at(r *Runtime) {
	if h.epoch == r.typedEpoch+1 {
		return
	}
	t := h.view
	if t.epoch != r.typedEpoch {
		r.cacheView(t)
	}
	h.base, h.len = t.data, uintptr(t.length)*h.size
	if h.base == nil || t.noLength {
		h.base, h.len = unsafe.Pointer(&asmNoHeap), 0
	}
	h.epoch = r.typedEpoch + 1
}

// tAddr is a heap access's byte address, of its view's element size
// 1 << shift: int32(R[B] + SC) >> shift << shift, negative out of the heap.
func tAddr(regs *[1 << 16]Value, w bytecode.RInstr, shift uint8) int64 {
	return int64(int32(uint32(ti(regs, w.B()))+uint32(w.SC()))>>shift) << shift
}

// rsTGlobal is a module variable's read or write where its cell holds a
// pointer: ToInt32 or ToNumber of what it holds, or the number stored
// whole, through the write barrier.
func rsTGlobal(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	cell := st.cl.upvalues[w.BC()].slot
	switch w.Op() {
	case bytecode.TGGetI, bytecode.TGGetF:
		st.at(pc)
		n, err := st.r.toNumber(*cell)
		if err != nil {
			return st.fail(err)
		}
		if w.Op() == bytecode.TGGetI {
			tset(regs, w.A(), jsnum.ToInt32(n))
		} else {
			regs[w.A()].num = n
		}
	case bytecode.TGSetI:
		*cell = Int32(ti(regs, w.A()))
	default:
		*cell = regNum(regs[w.A()].num)
	}
	return pc + 1
}

// rhTTabGet reads a function table's element, R[B] masked by its length,
// from the array the table's binding holds; anything but a dense array's
// element is undefined, which the call then throws for.
func rhTTabGet(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	tab := st.cl.upvalues[w.C()].get()
	v := Undefined
	if tab.IsObject() {
		n := st.c.rc.Asm[w.C()].Len
		es := tab.object().elemsOf()
		if i := int(uint32(ti(regs, w.B())) & uint32(n-1)); i < len(es) && !isHole(es[i]) {
			v = es[i]
		}
	}
	regs[w.A()] = v
	return pc + 1
}
