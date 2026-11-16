package regcomp

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Verify checks what the register interpreter takes on trust, reading
// registers and code without bounds checks: every register an instruction
// of rc names is below NumRegs, a call's window too; every constant, name,
// site and binding it names exists; every jump lands in the code; and the
// last instruction does not run on past the end. It returns what is wrong,
// or "".
func Verify(rc *bytecode.RegCode, fn *bytecode.Function) string {
	n := rc.NumRegs
	code := rc.Code
	if len(code) == 0 || len(rc.SrcPC) != len(code) || n >= limit16 || n < regParams+rc.Params+rc.Inits {
		return "no code"
	}
	reg := func(r uint16) bool { return int(r) < n }
	in := func(pc int, off int64) bool {
		t := int64(pc) + 1 + off
		return t >= 0 && t < int64(len(code))
	}
	site := func(s uint16) bool { return int(s) < len(rc.SiteName) && int(s) <= int(fn.PropSites) }
	for pc, w := range code {
		a, b, c := w.A(), w.B(), w.C()
		ok := true
		switch w.Op() {
		case bytecode.RMove, bytecode.RInc, bytecode.RDec, bytecode.RUnary, bytecode.RPostInc, bytecode.RPostDec, bytecode.RGetLength:
			ok = reg(a) && reg(b)
		case bytecode.RLoadInt, bytecode.RLoadUndef, bytecode.RLoadNull, bytecode.RLoadTrue,
			bytecode.RLoadFalse, bytecode.RLoadEmpty, bytecode.RLoadUninit, bytecode.RRet:
			ok = reg(a)
		case bytecode.RGetUpval, bytecode.RSetUpval:
			ok = reg(a) && int(w.BC()) < len(fn.Upvalues)
		case bytecode.RGetGlobal, bytecode.RSetGlobal:
			ok = reg(a) && int(b) < len(fn.Names) && site(c)
		case bytecode.RCheckRef:
			ok = reg(a) && int(b) < len(fn.Locals)
		case bytecode.RLoadConst:
			ok = reg(a) && int(w.BC()) < len(fn.Constants)
		case bytecode.RClosureW:
			k := int(w.BC())
			ok = reg(a) && k < len(fn.Constants) && k < len(rc.Caps) &&
				fn.Constants[k].Kind == bytecode.ConstFunction && fn.Constants[k].Fn != nil &&
				len(rc.Caps[k]) == len(fn.Constants[k].Fn.Upvalues)
			if ok {
				for i, u := range fn.Constants[k].Fn.Upvalues {
					if u.FromParent && !reg(rc.Caps[k][i]) {
						ok = false
					}
				}
			}
		case bytecode.RGetProp, bytecode.RGetMethod:
			ok = reg(a) && reg(b) && site(c)
		case bytecode.RArgsLen:
			ok = reg(a) && reg(b)
		case bytecode.RArgsIndex:
			ok = reg(a) && reg(b) && reg(c)
		case bytecode.RGetMethodW:
			ok = int(a)+1 < n && reg(b) && site(c)
		case bytecode.RSetProp:
			ok = reg(a) && reg(b) && site(c)
		case bytecode.RDefineField:
			ok = reg(a) && reg(b) && int(c) < len(fn.Names)
		case bytecode.RGetIndex, bytecode.RSetIndex, bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBin,
			bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr:
			ok = reg(a) && reg(b) && reg(c)
		case bytecode.RGetIndexPre:
			ok = reg(a) && reg(b) && reg(c)
		case bytecode.RJLtU, bytecode.RJNLtU, bytecode.RJLeU, bytecode.RJNLeU:
			ok = reg(a) && int(b) < len(fn.Upvalues) && in(pc, int64(w.SC()))
		case bytecode.RGetIndexI:
			ok = reg(a) && reg(b)
		case bytecode.RSetIndexI:
			ok = reg(a) && reg(c)
		case bytecode.RAddI, bytecode.RSubI, bytecode.RAndI, bytecode.ROrI, bytecode.RXorI,
			bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI:
			ok = reg(a) && reg(b)
		case bytecode.RNewObject:
			ok = reg(a) && site(c)
		case bytecode.RNewArray:
			ok = reg(a) && int(b)+int(c) <= n
		case bytecode.RCallW, bytecode.RCallMW:
			ok = reg(b) && int(a)+int(w.X())+2 <= n && int(c) < rc.CallSites
		case bytecode.RNewW:
			ok = reg(b) && int(a)+int(w.X())+2 <= n
		case bytecode.RJmp:
			ok = in(pc, int64(int32(w.BC())))
		case bytecode.RJmpF, bytecode.RJmpT:
			ok = reg(a) && in(pc, int64(int32(w.BC())))
		case bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt,
			bytecode.RJGe, bytecode.RJNGe, bytecode.RJSEq, bytecode.RJSNe, bytecode.RJEq, bytecode.RJNe:
			ok = reg(a) && reg(b) && in(pc, int64(w.SC()))
		case bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI,
			bytecode.RJGeI, bytecode.RJNGeI, bytecode.RJSEqI, bytecode.RJSNeI, bytecode.RJEqI, bytecode.RJNeI,
			bytecode.RJNull, bytecode.RJNotNull:
			ok = reg(a) && in(pc, int64(w.SC()))
		case bytecode.RThrow, bytecode.RArguments:
			ok = reg(a)
		case bytecode.RIn, bytecode.RDelete:
			ok = reg(a) && reg(b) && reg(c)
		case bytecode.RAddProp:
			ok = reg(a) && reg(b) && site(c)
		case bytecode.RAddPropI:
			ok = reg(a) && site(c)
		case bytecode.RNewArrayK:
			ok = reg(a) && site(c) && pc < len(rc.Aux) && int(rc.Aux[pc]) < len(rc.Literals) && len(rc.Literals[rc.Aux[pc]]) == int(b) && b > 0
			if ok {
				for _, e := range rc.Literals[rc.Aux[pc]] {
					if e.Kind > bytecode.LitUndefined || e.Kind == bytecode.LitConst && int(e.K) >= len(fn.Constants) {
						ok = false
					}
				}
			}
		case bytecode.RInstanceOf:
			ok = reg(a) && reg(b) && reg(c) && pc < len(rc.Aux) && rc.Aux[pc] != 0 && int(rc.Aux[pc]) <= int(fn.PropSites)
		case bytecode.RApplyArgs:
			ok = reg(a) && int(b)+3 <= n && rc.ForwardsArguments
		case bytecode.RRetUndef:
		default:
			ok = false
		}
		if !ok {
			return "instruction " + rc.Format(pc) + " is out of range"
		}
		if int(rc.SrcPC[pc]) >= len(fn.Code) {
			return "a position out of the code"
		}
	}
	switch code[len(code)-1].Op() {
	case bytecode.RJmp, bytecode.RRet, bytecode.RRetUndef, bytecode.RThrow:
	default:
		return "the code runs past its end"
	}
	return ""
}
