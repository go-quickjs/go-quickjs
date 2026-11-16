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
	// A window may be longer than 16 bits of registers reach: the far
	// registers past them, which mov_far, mov_to_far and far_init name by
	// a 32-bit BC, each checked below.
	if len(code) == 0 || len(rc.SrcPC) != len(code) || n < regParams+rc.Params+rc.Inits {
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
			ok = reg(a) && nameOK(rc, fn, pc, b) && site(c)
		case bytecode.RCheckRef:
			// The slot past 16 bits is FarNames', as a name is.
			ok = reg(a) && (b != bytecode.RNameFar && int(b) < len(fn.Locals) ||
				b == bytecode.RNameFar && pc < len(rc.FarNames) && rc.FarNames[pc] >= bytecode.RNameFar && int(rc.FarNames[pc]) < len(fn.Locals))
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
		case bytecode.RConcat:
			ok = reg(a) && c >= 2 && int(b)+int(c) <= n
		case bytecode.RStrPart:
			ok = reg(a)
		case bytecode.RKeyOfBase:
			ok = reg(a) && reg(b) && reg(c)
		case bytecode.RGetMethodW:
			ok = int(a)+1 < n && reg(b) && site(c)
		case bytecode.RSetProp:
			ok = reg(a) && reg(b) && site(c)
		case bytecode.RDefineField:
			ok = reg(a) && reg(b) && nameOK(rc, fn, pc, c)
		case bytecode.RGetIndex, bytecode.RSetIndex, bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBin,
			bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr:
			ok = reg(a) && reg(b) && reg(c)
		case bytecode.RGetIndexPre, bytecode.RGetIndexPost:
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
		case bytecode.RNewObjectLit:
			ok = reg(a) && site(c) && w.X() > 0 && int(b)+int(w.X()) <= n && pc < len(rc.Aux) && int(rc.Aux[pc]) < len(rc.ObjKeys) &&
				len(rc.ObjKeys[rc.Aux[pc]]) == int(w.X())
			if ok {
				for _, k := range rc.ObjKeys[rc.Aux[pc]] {
					ok = ok && int(k) < len(fn.Names)
				}
			}
		case bytecode.RGetPrivate, bytecode.RSetPrivate:
			// Aux: the cache site, or 0, and the name for the errors.
			ok = reg(a) && reg(b) && reg(c) && w.X() == bytecode.RPrivateX && pc < len(rc.Aux) &&
				site(uint16(rc.Aux[pc]>>16)) && int(rc.Aux[pc]&0xffff) < len(fn.Names)
		case bytecode.RTypeof, bytecode.RAppend:
			ok = reg(a) && reg(b)
		case bytecode.RStrFreeze:
			ok = reg(a)
		case bytecode.RCharCodeAt:
			ok = reg(a) && reg(b) && reg(uint16(w.X())) && site(c) && pc < len(rc.Aux) && int(rc.Aux[pc]) < len(fn.Code)
		case bytecode.RJTypeof:
			ok = reg(a) && w.X()&^bytecode.RCmpTrue < 8 && in(pc, int64(w.SC()))
		case bytecode.RSwitch, bytecode.RSwitchTypeof:
			ok = reg(a) && int(w.BC()) < len(rc.Switches)
			if ok {
				for _, sc := range rc.Switches[w.BC()] {
					ok = ok && int(sc.To) < len(code)
				}
			}
		case bytecode.RAddIndex:
			ok = reg(a) && reg(b) && reg(c)
		case bytecode.RAddIndexK, bytecode.RAddIndexI:
			ok = reg(a) && reg(b)
		case bytecode.RNewArrayE:
			ok = reg(a)
		case bytecode.RNewArray:
			ok = reg(a) && int(b)+int(c) <= n
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT:
			ok = reg(b) && int(a)+int(w.X())+2 <= n && int(c) < rc.CallSites
			if m := markAt(rc, pc); ok && m != 0 {
				aux := auxAt(rc, pc)
				switch {
				case !w.Op().MethodCall():
					ok = false
				case m&bytecode.RMarkReuse != 0:
					ok = int64(aux)+1 < int64(len(fn.Code)) && len(rc.LocalReg) == len(fn.Locals)
				case m&(bytecode.RMarkLitRecv|bytecode.RMarkLitArg) != 0:
					ok = int64(aux) < int64(len(fn.Constants)) && fn.Constants[aux].Kind == bytecode.ConstRegExp &&
						(m&bytecode.RMarkLitArg == 0 || w.X() > 0)
				}
			}
		case bytecode.RLit:
			k := int(c)
			ok = reg(a) && reg(a+1) && k < len(fn.Constants) && fn.Constants[k].Kind == bytecode.ConstRegExp
			if ok && w.X()&bytecode.RLitCall != 0 {
				ok = reg(b) && int(a)+3 <= n && pc < len(rc.Aux) && int64(rc.Aux[pc]) < int64(len(fn.Code))
			}
		case bytecode.RNewW:
			ok = reg(b) && int(a)+int(w.X())+2 <= n
		case bytecode.RJmp:
			ok = in(pc, int64(int32(w.BC())))
		case bytecode.RIterNext:
			ok = reg(a) && reg(b) && in(pc, int64(w.SC()))
		case bytecode.RYield:
			ok = fn.Generator && reg(a) && reg(b) && pc+2 < len(code) && code[pc+1].Op() == bytecode.RJmp
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
			ok = reg(a) && site(c) && pc < len(rc.Aux) && int(rc.Aux[pc]) < len(rc.Literals) && (len(rc.Literals[rc.Aux[pc]]) == int(b) && b > 0 || b == 0 && len(rc.Literals[rc.Aux[pc]]) >= limit16)
			if ok {
				for _, e := range rc.Literals[rc.Aux[pc]] {
					if e.Kind > bytecode.LitUndefined || e.Kind == bytecode.LitConst && int(e.K) >= len(fn.Constants) {
						ok = false
					}
				}
			}
		case bytecode.RInstanceOf:
			ok = reg(a) && reg(b) && reg(c) && pc < len(rc.Aux) && rc.Aux[pc] != 0 && int(rc.Aux[pc]) <= int(fn.PropSites)
		case bytecode.RCold:
			ok = coldOK(rc, fn, pc, w, reg, site)
		case bytecode.RApplyArgs:
			ok = reg(a) && int(b)+3 <= n && reg(c) && rc.ForwardsArguments
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
	for _, h := range rc.Handlers {
		if int(h.To) >= len(code) || !reg(h.Reg) || len(h.Ranges)%2 != 0 {
			return "a handler out of range"
		}
		for j := 0; j < len(h.Ranges); j += 2 {
			if h.Ranges[j] > h.Ranges[j+1] || int(h.Ranges[j+1]) > len(code) {
				return "a handler's range out of the code"
			}
		}
	}
	return ""
}

// markAt is the marks of the call at pc, or 0.
// nameOK reports whether op, the name operand of the instruction at pc,
// names one of fn's names: its index, or RNameFar and FarNames' index
// past 16 bits.
func nameOK(rc *bytecode.RegCode, fn *bytecode.Function, pc int, op uint16) bool {
	if op != bytecode.RNameFar {
		return int(op) < len(fn.Names)
	}
	return pc < len(rc.FarNames) && rc.FarNames[pc] >= bytecode.RNameFar && int(rc.FarNames[pc]) < len(fn.Names)
}

func markAt(rc *bytecode.RegCode, pc int) uint8 {
	if pc < len(rc.Marks) {
		return rc.Marks[pc]
	}
	return 0
}

// auxAt is the Aux of the instruction at pc, or 0.
func auxAt(rc *bytecode.RegCode, pc int) uint32 {
	if pc < len(rc.Aux) {
		return rc.Aux[pc]
	}
	return 0
}

// coldOK checks cold's operands, by its sub-operation.
func coldOK(rc *bytecode.RegCode, fn *bytecode.Function, pc int, w bytecode.RInstr, reg func(uint16) bool, site func(uint16) bool) bool {
	a, b := w.A(), w.B()
	regexpK := func(k uint32) bool { return int(k) < len(fn.Constants) && fn.Constants[k].Kind == bytecode.ConstRegExp }
	switch bytecode.RColdOp(w.X()) {
	case bytecode.RColdNewRegExp:
		return reg(a) && regexpK(w.BC())
	case bytecode.RColdForOfStart, bytecode.RColdForInStart:
		return reg(a) && reg(b)
	case bytecode.RColdIterClose, bytecode.RColdIterCloseThrow:
		return reg(b)
	case bytecode.RColdCheckGlobalLex, bytecode.RColdCheckGlobalVar, bytecode.RColdDefineGlobalVar,
		bytecode.RColdDeclareGlobalLex, bytecode.RColdDefineGlobalFunc, bytecode.RColdInitGlobalLex,
		bytecode.RColdDeclareModuleLex, bytecode.RColdInitModuleLex:
		return reg(a) && nameOK(rc, fn, pc, b) && fn.TopLevel
	case bytecode.RColdGetGlobalOpt, bytecode.RColdDeleteVar:
		return reg(a) && nameOK(rc, fn, pc, b)
	case bytecode.RColdImport:
		k := int(w.C()) & 3
		return reg(a) && (k == 1 || k == 2) && int(b)+k <= rc.NumRegs
	case bytecode.RColdDirectEval:
		k := auxAt(rc, pc) &^ bytecode.RDirectEvalSpread
		argc := int(w.C())
		if auxAt(rc, pc)&bytecode.RDirectEvalSpread != 0 {
			argc = 1
		}
		if !reg(b) || int(a)+2+argc > rc.NumRegs || int64(k) >= int64(len(fn.EvalScopes)) || markAt(rc, pc)&^bytecode.RMarkTail != 0 {
			return false
		}
		// Each binding of the caller's the code may reach has a register.
		for _, e := range fn.EvalScopes[k].Bindings {
			if e.FromLocal && (int(e.Index) >= len(rc.LocalReg) || rc.LocalReg[e.Index] < 0) {
				return false
			}
		}
		return true
	case bytecode.RColdWithGet, bytecode.RColdWithDelete, bytecode.RColdWithSet, bytecode.RColdWithResolve:
		return reg(a) && nameOK(rc, fn, pc, b) && pc < len(rc.Aux) && rc.Aux[pc] < uint32(rc.NumRegs)
	case bytecode.RColdWithGetThis:
		return reg(a) && int(a)+1 < rc.NumRegs && nameOK(rc, fn, pc, b) && pc < len(rc.Aux) && rc.Aux[pc] < uint32(rc.NumRegs)
	case bytecode.RColdWithRead, bytecode.RColdWithWrite:
		return reg(a) && reg(b) && nameOK(rc, fn, pc, w.C())
	case bytecode.RColdWithPush:
		return reg(a)
	case bytecode.RColdWithPop:
		return true
	case bytecode.RColdImportMeta, bytecode.RColdNewDispose:
		return reg(a)
	case bytecode.RColdAddDisposable, bytecode.RColdDispose:
		return reg(a) && reg(b) && w.C() <= 1
	case bytecode.RColdDisposeStep:
		return int(a)+1 < rc.NumRegs && reg(b)
	case bytecode.RColdIterStep, bytecode.RColdIterRest:
		return reg(a) && reg(b)
	case bytecode.RColdCheckCoercible, bytecode.RColdToPropertyKey:
		return reg(a)
	case bytecode.RColdWrap:
		// The operator Aux names, whose value is the second argument of
		// the two-argument method call right after it.
		if !reg(a) || !reg(b) || !reg(w.C()) || pc+1 >= len(rc.Code) || !wrapsOp(bytecode.ROp(auxAt(rc, pc))) {
			return false
		}
		cw := rc.Code[pc+1]
		return (cw.Op() == bytecode.RCallMW || cw.Op() == bytecode.RCallMWT) && cw.X() == 2 && int(cw.A())+3 == int(a)
	case bytecode.RColdObjectRest:
		return reg(a) && reg(b) && pc < len(rc.Aux) && int(w.C())+int(rc.Aux[pc]) <= rc.NumRegs
	case bytecode.RColdRestParam:
		return reg(a) && rc.KeepsArgs
	case bytecode.RColdToString:
		return reg(a)
	case bytecode.RColdCopyDataProps, bytecode.RColdSetHomeObject, bytecode.RColdSetProtoOf:
		return reg(a) && reg(b)
	case bytecode.RColdDefineGetter, bytecode.RColdDefineSetter:
		return reg(a) && reg(b) && nameOK(rc, fn, pc, w.C())
	case bytecode.RColdDefineGetterIndex, bytecode.RColdDefineSetterIndex, bytecode.RColdDefineIndex:
		return reg(a) && reg(b) && reg(w.C())
	case bytecode.RColdAssignConst, bytecode.RColdThrowRef:
		return nameOK(rc, fn, pc, b)
	case bytecode.RColdDebug:
		return b <= 1 && fn.Debug != nil && pc < len(rc.Aux) && int(rc.Aux[pc]) < len(fn.Debug.Statements)
	case bytecode.RColdCheckGlobalRef:
		return reg(a) && nameOK(rc, fn, pc, b) && site(w.C())
	case bytecode.RColdAssertResolved:
		return reg(a) && nameOK(rc, fn, pc, b)
	case bytecode.RColdArrayPush, bytecode.RColdArraySpread:
		return reg(a) && reg(b)
	case bytecode.RColdCallSpread:
		return int(a)+2 < rc.NumRegs && reg(b) && w.C() <= 1
	case bytecode.RColdSetFuncName:
		return reg(a) && reg(b) && w.C() <= 2
	case bytecode.RColdTemplateObject:
		return reg(a) && int(w.BC()) < len(fn.Templates)
	case bytecode.RColdNewClass, bytecode.RColdSetFieldInit, bytecode.RColdSuperSpread:
		return reg(a) && reg(b)
	case bytecode.RColdDefineMethod, bytecode.RColdClassGetter, bytecode.RColdClassSetter:
		return reg(a) && reg(b) && nameOK(rc, fn, pc, w.C())
	case bytecode.RColdClassGetterIndex, bytecode.RColdClassSetterIndex, bytecode.RColdClassIndex,
		bytecode.RColdAddPrivateMethod, bytecode.RColdAddPrivateGetter, bytecode.RColdAddPrivateSetter,
		bytecode.RColdDefinePrivate, bytecode.RColdDefinePrivateMethod, bytecode.RColdDefinePrivateGetter,
		bytecode.RColdDefinePrivateSetter, bytecode.RColdPrivateIn, bytecode.RColdSuperGetIndex, bytecode.RColdSuperSetIndex:
		return reg(a) && reg(b) && reg(w.C())
	case bytecode.RColdPrivateName, bytecode.RColdSuperGet, bytecode.RColdSuperSet:
		return reg(a) && nameOK(rc, fn, pc, b)
	case bytecode.RColdNewPrivateMethods, bytecode.RColdInstallPrivateMethods, bytecode.RColdSuperBase,
		bytecode.RColdThis, bytecode.RColdNewTarget, bytecode.RColdDerivedRet, bytecode.RColdSuperForward:
		return reg(a)
	case bytecode.RColdSuperCall:
		return reg(a) && int(w.C())+int(b) <= rc.NumRegs
	case bytecode.RColdCheckThis, bytecode.RColdDeleteSuper:
		return true
	case bytecode.RColdCloseRegs:
		return int(a)+int(b) <= rc.NumRegs
	case bytecode.RColdMovFar, bytecode.RColdMovToFar:
		// Far registers are reached past the 16 bits regs' type covers,
		// within the window NumRegs makes.
		return reg(a) && int64(w.BC()) < int64(rc.NumRegs)
	case bytecode.RColdFarInit:
		return pc < len(rc.Aux) && int64(w.BC())+int64(rc.Aux[pc]) <= int64(rc.NumRegs)
	case bytecode.RColdGetUpvalCheck, bytecode.RColdSetUpvalCheck:
		return reg(a) && int(w.BC()) < len(fn.Upvalues)
	}
	return suspendOK(rc, fn, pc, w, reg)
}

// suspendOK checks a generator's and an async function's cold operations:
// gen_entry first of the code and nowhere else, each suspension in a
// function that suspends, and a jump after each operation whose next
// instruction is one, with an instruction after it.
func suspendOK(rc *bytecode.RegCode, fn *bytecode.Function, pc int, w bytecode.RInstr, reg func(uint16) bool) bool {
	if !fn.Generator && !fn.Async {
		return false
	}
	jumpNext := func() bool {
		return pc+2 < len(rc.Code) && rc.Code[pc+1].Op() == bytecode.RJmp
	}
	a, b := w.A(), w.B()
	switch bytecode.RColdOp(w.X()) {
	case bytecode.RColdGenEntry:
		return pc == 0
	case bytecode.RColdEndParams:
		return fn.Generator
	case bytecode.RColdAwait:
		return fn.Async && reg(a) && reg(b)
	case bytecode.RColdYieldStar:
		return fn.Generator && int(a)+1 < rc.NumRegs && reg(b)
	case bytecode.RColdIterResume:
		// A synchronous generator's delegation may suspend at the
		// yield_star four instructions on (the VM's regDelegateNext).
		return reg(a) && reg(b) && int(w.C())+1 < rc.NumRegs && jumpNext() &&
			(fn.Async || pc+5 < len(rc.Code) && rc.Code[pc+4].Op() == bytecode.RCold &&
				bytecode.RColdOp(rc.Code[pc+4].X()) == bytecode.RColdYieldStar)
	case bytecode.RColdIterUnpackDelegate:
		return reg(a) && reg(b) && reg(w.C()) && jumpNext()
	case bytecode.RColdForAwaitOfStart, bytecode.RColdAsyncIterNext:
		return fn.Async && reg(a) && reg(b)
	case bytecode.RColdIterResult, bytecode.RColdAsyncIterClose:
		return fn.Async && reg(a) && reg(b) && jumpNext()
	case bytecode.RColdAsyncIterCheck:
		return fn.Async && reg(a)
	}
	return false
}
