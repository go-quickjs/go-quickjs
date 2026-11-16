package vm

import (
	"os"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler/regform"
)

// Hand-written register code, a throwaway experiment: QJS_REGHAND (with
// QJS_REG) gives am3, call_fib_rec, call_fib and loop_locals the code the
// register form's designed instruction set would have for them, to time the
// hot operations by themselves before a backend makes them.

var regHandOn = os.Getenv("QJS_REGHAND") != ""

// regSites lists the name and cache site of each instruction of op in fn's
// stack code, in order.
func regSites(fn *bytecode.Function, op bytecode.Op) [][2]uint16 {
	var out [][2]uint16
	for _, in := range fn.Code {
		if in.Op == op {
			out = append(out, [2]uint16{uint16(in.A), uint16(in.B)})
		}
	}
	return out
}

func regHandTree(fn *bytecode.Function) *tree {
	M := bytecode.MakeR
	MB := bytecode.MakeRBC
	var code []bytecode.RInstr
	var siteName []uint32
	wide := false
	numRegs, inits := 0, 0
	names := make([]uint32, fn.PropSites+1)
	switch fn.Name {
	case "call_fib_rec":
		// r0 the function, r1 this, r2 k; windows at r3 and r6.
		g := regSites(fn, bytecode.OpGetGlobal)
		if len(g) != 2 {
			return nil
		}
		wide, numRegs = true, 9
		code = []bytecode.RInstr{
			M(bytecode.RJNLtI, 0, 2, 2, 1), // if !(k < 2) skip the return
			M(bytecode.RRet, 0, 2, 0, 0),
			M(bytecode.RGetGlobal, 0, 3, g[0][0], g[0][1]),
			M(bytecode.RSubI, 0, 5, 2, 1),
			M(bytecode.RCallW, 1, 3, 3, 0),
			M(bytecode.RGetGlobal, 0, 6, g[1][0], g[1][1]),
			M(bytecode.RSubI, 0, 8, 2, 2),
			M(bytecode.RCallW, 1, 6, 6, 0),
			M(bytecode.RAdd, 0, 3, 3, 6),
			M(bytecode.RRet, 0, 3, 0, 0),
		}
	case "call_fib":
		// The stack form's layout (it is entered from the driver): r0 n,
		// r1 j, r2 s; the call's window at r4.
		g := regSites(fn, bytecode.OpGetGlobal)
		s := regSites(fn, bytecode.OpSetGlobal)
		if len(g) != 1 || len(s) != 1 {
			return nil
		}
		numRegs = 8
		code = []bytecode.RInstr{
			MB(bytecode.RLoadInt, 0, 2, 0),
			MB(bytecode.RLoadInt, 0, 1, 0),
			M(bytecode.RJNLt, 0, 1, 0, 7), // -> 10
			M(bytecode.RGetGlobal, 0, 4, g[0][0], g[0][1]),
			MB(bytecode.RLoadInt, 0, 6, 10),
			M(bytecode.RCallW, 1, 4, 4, 0),
			M(bytecode.RAdd, 0, 3, 2, 4),
			M(bytecode.ROrI, 0, 2, 3, 0),
			M(bytecode.RAddI, 0, 1, 1, 177),
			M(bytecode.RJLt, 0, 1, 0, uint16(0x10000-7)), // -> 3
			M(bytecode.RSetGlobal, 0, 2, s[0][0], s[0][1]),
			M(bytecode.RRet, 0, 0, 0, 0),
		}
	case "loop_locals":
		s := regSites(fn, bytecode.OpSetGlobal)
		if len(s) != 1 {
			return nil
		}
		numRegs = 8
		code = []bytecode.RInstr{
			MB(bytecode.RLoadInt, 0, 2, 1),
			MB(bytecode.RLoadInt, 0, 3, 2),
			MB(bytecode.RLoadInt, 0, 4, 3),
			MB(bytecode.RLoadInt, 0, 5, 4),
			MB(bytecode.RLoadInt, 0, 1, 0),
			M(bytecode.RJNLt, 0, 1, 0, 6), // -> 12
			M(bytecode.RMove, 0, 2, 3, 0),
			M(bytecode.RMove, 0, 3, 4, 0),
			M(bytecode.RMove, 0, 4, 5, 0),
			M(bytecode.RMove, 0, 5, 2, 0),
			M(bytecode.RInc, 0, 1, 1, 0),
			M(bytecode.RJLt, 0, 1, 0, uint16(0x10000-6)), // -> 6
			M(bytecode.RAdd, 0, 6, 2, 3),
			M(bytecode.RAdd, 0, 6, 6, 4),
			M(bytecode.RAdd, 0, 6, 6, 5),
			M(bytecode.RSetGlobal, 0, 6, s[0][0], s[0][1]),
			M(bytecode.RRet, 0, 0, 0, 0),
		}
	case "am3":
		// The stack form's layout: r0 i, r1 x, r2 w, r3 j, r4 c, r5 n,
		// r6 this_array, r7 w_array, r8 xl, r9 xh, r10 l, r11 h, r12 m;
		// r13 this, r14 the mask, r15 and r16 temporaries.
		p := regSites(fn, bytecode.OpGetProp)
		if len(p) != 2 {
			return nil
		}
		names[p[0][1]] = uint32(p[0][0])
		names[p[1][1]] = uint32(p[1][0])
		numRegs = 17
		code = []bytecode.RInstr{
			M(bytecode.RLoadThis, 0, 13, 0, 0),
			M(bytecode.RGetProp, 0, 6, 13, p[0][1]),
			M(bytecode.RGetProp, 0, 7, 2, p[1][1]),
			M(bytecode.RAndI, 0, 8, 1, 0x3fff),
			M(bytecode.RShrI, 0, 9, 1, 14),
			MB(bytecode.RLoadInt, 0, 14, 0xfffffff),
			M(bytecode.RDec, 0, 5, 5, 0),
			M(bytecode.RJNGeI, 0, 5, 0, 25),    // -> 32
			M(bytecode.RGetIndex, 0, 15, 6, 0), // 8
			M(bytecode.RAndI, 0, 10, 15, 0x3fff),
			M(bytecode.RGetIndex, 0, 15, 6, 0),
			M(bytecode.RInc, 0, 0, 0, 0),
			M(bytecode.RShrI, 0, 11, 15, 14),
			M(bytecode.RMul, 0, 15, 9, 10),
			M(bytecode.RMul, 0, 16, 11, 8),
			M(bytecode.RAdd, 0, 12, 15, 16),
			M(bytecode.RMul, 0, 15, 8, 10),
			M(bytecode.RAndI, 0, 16, 12, 0x3fff),
			M(bytecode.RShlI, 0, 16, 16, 14),
			M(bytecode.RAdd, 0, 15, 15, 16),
			M(bytecode.RGetIndex, 0, 16, 7, 3),
			M(bytecode.RAdd, 0, 15, 15, 16),
			M(bytecode.RAdd, 0, 10, 15, 4),
			M(bytecode.RShrI, 0, 15, 10, 28),
			M(bytecode.RShrI, 0, 16, 12, 14),
			M(bytecode.RAdd, 0, 15, 15, 16),
			M(bytecode.RMul, 0, 16, 9, 11),
			M(bytecode.RAdd, 0, 4, 15, 16),
			M(bytecode.RBitAnd, 0, 15, 10, 14),
			M(bytecode.RSetIndex, 0, 7, 3, 15),
			M(bytecode.RInc, 0, 3, 3, 0),
			M(bytecode.RDec, 0, 5, 5, 0),
			M(bytecode.RJGeI, 0, 5, 0, uint16(0x10000-25)), // -> 8
			M(bytecode.RRet, 0, 4, 0, 0),
		}
		// The Dec/JmpCmpI pair at 6-7 jumps past the loop to 33.
		code[7] = code[7].WithSC(int32(33 - 8))
	default:
		return nil
	}
	if wide {
		numRegs = max(numRegs, 2+fn.ParamCount)
	} else {
		numRegs = max(numRegs, fn.LocalCount+1)
	}
	siteName = names
	src := make([]uint32, len(code))
	rc := &bytecode.RegCode{Code: code, NumRegs: numRegs, SrcPC: src, SiteName: siteName, This: -1}
	if why := regform.VerifyRegisterCode(rc, fn); why != "" {
		println("reg hand:", fn.Name, why)
		return nil
	}
	regsBuilt.Add(1)
	if regDump {
		println("reg hand:", fn.Name)
		println(rc.String())
	}
	c := &regCode{
		code: unsafe.Pointer(unsafe.SliceData(rc.Code)), src: rc.SrcPC, siteName: rc.SiteName,
		numRegs: int32(rc.NumRegs), locals: int32(fn.LocalCount), params: int32(fn.ParamCount),
		coerceThis: fn.CoerceThis, rc: rc, fn: fn, wide: wide, inits: int32(inits),
	}
	if wide {
		c.locals = 0
	}
	t := &tree{reg: c}
	t.blocks = []tblock{{body: []tstmt{regStmt(c)}, next: regNext}}
	return t
}
