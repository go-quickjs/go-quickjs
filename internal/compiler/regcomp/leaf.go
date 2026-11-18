package regcomp

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// leafOf sets rc's Leaf -- which of the bodies bytecode.LeafKind names
// fn's register code is, or LeafNone -- and, for a pure one, its PureTail
// and PureCalls. Only a function whose call does nothing before its body
// qualifies: no rest parameter or arguments object to make, no default or
// pattern to bind, no generator or async function to set up, no eval
// scope, and a `this` of its own -- not an arrow's, and not a derived
// constructor's, which super() binds. Code compiled for a debugger has
// none: a leaf is answered without a frame, so a debugger could not stop
// in it.
func leafOf(fn *bytecode.Function, rc *bytecode.RegCode) {
	rc.Leaf, rc.PureTail, rc.PureCalls = bytecode.LeafNone, 0, false
	rc.Stores, rc.Forward = nil, nil
	if fn.Debug != nil || fn.Generator || fn.Async || fn.HasRest || fn.HasDirectEval || !fn.HasSimpleParams || fn.ParamsAreLexical {
		return
	}
	plansOf(fn, rc)
	code := rc.Code
	this := func(r uint16) bool { return r == regThis }
	// The register of parameter k, which the body never writes.
	param := func(r uint16) bool { return int(r) >= regParams && int(r) < regParams+fn.ParamCount }
	// this.k.apply(this, arguments) mentions arguments, but apply_args
	// hands the arguments on without the object being made.
	if fn.Kind == bytecode.KindNormal && len(code) == 5 && fn.ForwardsArguments &&
		code[0].Op() == bytecode.RGetProp && this(code[0].B()) &&
		code[1].Op() == bytecode.RGetProp && code[1].B() == code[0].A() && siteIs(fn, rc, code[1].C(), "apply") &&
		code[1].A() == code[0].A()+1 && code[2].Op() == bytecode.RMove && this(code[2].B()) && code[2].A() == code[0].A()+2 &&
		code[3].Op() == bytecode.RApplyArgs && code[3].B() == code[0].A() &&
		code[4].Op() == bytecode.RRetUndef {
		rc.Leaf = bytecode.LeafForward
		rc.Forward = &bytecode.ForwardPlan{Method: code[0].C(), Fn: code[1].C()}
		return
	}
	if fn.UsesArguments {
		return
	}
	switch fn.Kind {
	case bytecode.KindNormal, bytecode.KindMethod, bytecode.KindGetter, bytecode.KindConstructor:
	case bytecode.KindArrow:
		// An arrow's this is where it was written, not the call's, so the
		// kinds that read or write this are not for it. A pure body that
		// never reads this -- a comparator, a map callback -- is evaluated
		// as anyone's is.
		if !fn.UsesThis {
			pureBody(fn, rc)
		}
		return
	default:
		return
	}
	// The ret_undef that ends every body is unreachable after a ret.
	ret := func(pc int, r uint16) bool {
		return pc < len(code) && code[pc].Op() == bytecode.RRet && code[pc].A() == r
	}
	if len(code) >= 2 && code[0].Op() == bytecode.RGetProp && this(code[0].B()) {
		v := code[0].A()
		switch {
		case ret(1, v):
			rc.Leaf = bytecode.LeafGetThis
			return
		case len(code) >= 3 && code[1].Op() == bytecode.RGetLength && code[1].B() == v && ret(2, code[1].A()):
			rc.Leaf = bytecode.LeafGetThisLength
			return
		case len(code) >= 3 && code[1].Op() == bytecode.RGetIndex && code[1].B() == v && param(code[1].C()) && ret(2, code[1].A()):
			rc.Leaf = bytecode.LeafGetThisIndex
			return
		}
	}
	n := 0
	for n < len(code) && code[n].Op() == bytecode.RSetProp && this(code[n].A()) && param(code[n].B()) {
		n++
	}
	if n > 0 && n < len(code) && (code[n].Op() == bytecode.RRetUndef ||
		fn.Kind == bytecode.KindConstructor && code[n].Op() == bytecode.RRet && this(code[n].A())) {
		rc.Leaf = bytecode.LeafSetThis
		return
	}
	pureBody(fn, rc)
}

// siteIs reports whether the cache site s of rc's code is of the name.
func siteIs(fn *bytecode.Function, rc *bytecode.RegCode, s uint16, name string) bool {
	names := rc.Names
	if names == nil {
		names = fn.Names
	}
	return int(s) < len(rc.SiteName) && int(rc.SiteName[s]) < len(names) && names[rc.SiteName[s]] == name
}

// pureCode is the most instructions a pure body has: 64 of the stack
// form's had been, which a chain of comparisons, each of whose values the
// register code makes with a branch, takes 71 of (AWFY Json's isDigit).
const pureCode = 80

// pureBody marks rc's code LeafPure where it is a body the VM's frameless
// evaluator takes (pureCallAt): no binding but its parameters, which it
// never writes, every jump forward, and every instruction one that reads
// or computes, but for one store, to a property or an upvalue, in its
// tail. It sets PureTail, where the tail of instructions the evaluation
// cannot give up at begins, and PureCalls. An upvalue it reads is one
// get_upval reads, which has no dead zone to check.
func pureBody(fn *bytecode.Function, rc *bytecode.RegCode) {
	code := rc.Code
	// Nor one that reads its function object (register 0), which the
	// evaluation has not got.
	if fn.LocalCount != fn.ParamCount || rc.Inits != 0 || rc.NumRegs > bytecode.PureRegs || len(code) > pureCode || rc.Marks != nil || fn.FrameCallee {
		return
	}
	writes := func(r uint16) bool { return int(r) >= regParams+fn.ParamCount }
	forward := func(pc int, off int32) bool { return off >= 0 && pc+1+int(off) <= len(code) }
	tail := len(code)
	for tail > 0 && pureSafe(code[tail-1]) {
		tail--
	}
	calls := false
	for pc, w := range code {
		ok := true
		switch op := w.Op(); op {
		case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RNewW:
			// A call is of a body that is itself pure, or it is not made
			// frameless: the VM sees which when it gets there. So is a
			// construction, of a constructor that only stores its
			// parameters.
			calls = true
			ok = writes(w.B()) && writes(w.A())
		case bytecode.RMove, bytecode.RLoadInt, bytecode.RLoadConst, bytecode.RLoadUndef, bytecode.RLoadNull,
			bytecode.RLoadTrue, bytecode.RLoadFalse, bytecode.RLoadEmpty, bytecode.RGetProp, bytecode.RGetLength,
			bytecode.RGetGlobal, bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBitAnd, bytecode.RBitOr,
			bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr, bytecode.RAddI, bytecode.RSubI,
			bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI,
			bytecode.RMulI, bytecode.RInc, bytecode.RDec, bytecode.RTypeof, bytecode.RGetUpval, bytecode.RStrFreeze,
			bytecode.RAddI32, bytecode.RSubI32, bytecode.RAddII32:
			ok = writes(w.A())
		case bytecode.RGetMethodW:
			ok = writes(w.A()) && writes(w.A()+1)
		case bytecode.RBin:
			switch bytecode.Op(w.X()) {
			case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpBitAnd, bytecode.OpBitOr,
				bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr:
				ok = writes(w.A())
			default:
				ok = false
			}
		case bytecode.RUnary:
			switch bytecode.Op(w.X()) {
			case bytecode.OpNeg, bytecode.OpNot:
				ok = writes(w.A())
			default:
				ok = false
			}
		case bytecode.RSetProp, bytecode.RSetUpval, bytecode.RAddProp, bytecode.RAddPropI:
			// The store is the last thing the body can give up at: a
			// property's, or an upvalue's -- a counter a closure keeps, as
			// count++ does -- or a property's addition to itself, this.n++.
			ok = pc+1 >= tail
		case bytecode.RSwitch, bytecode.RSwitchTypeof:
			// Each case's body after it, as the default's jump.
			for _, sc := range rc.Switches[w.BC()] {
				ok = ok && int(sc.To) > pc
			}
		case bytecode.RRet, bytecode.RRetUndef:
		case bytecode.RJmp, bytecode.RJmpF, bytecode.RJmpT:
			ok = forward(pc, int32(w.BC()))
		case bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt,
			bytecode.RJGe, bytecode.RJNGe, bytecode.RJEq, bytecode.RJNe, bytecode.RJSEq, bytecode.RJSNe,
			bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI,
			bytecode.RJGeI, bytecode.RJNGeI, bytecode.RJEqI, bytecode.RJNeI, bytecode.RJSEqI, bytecode.RJSNeI,
			bytecode.RJNull, bytecode.RJNotNull, bytecode.RJLtU, bytecode.RJNLtU, bytecode.RJLeU, bytecode.RJNLeU,
			bytecode.RJTypeof:
			ok = forward(pc, w.SC())
		default:
			ok = false
		}
		if !ok {
			return
		}
	}
	rc.Leaf, rc.PureTail, rc.PureCalls = bytecode.LeafPure, int32(tail), calls
}

// pureSafe reports whether an instruction of a pure body is one its
// frameless evaluation cannot give up at: it loads, moves, branches or
// returns what it has, with no read of a property or a global and no
// operator that could call valueOf.
func pureSafe(w bytecode.RInstr) bool {
	switch w.Op() {
	case bytecode.RMove, bytecode.RLoadInt, bytecode.RLoadConst, bytecode.RLoadUndef, bytecode.RLoadNull,
		bytecode.RLoadTrue, bytecode.RLoadFalse, bytecode.RLoadEmpty, bytecode.RTypeof,
		bytecode.RRet, bytecode.RRetUndef, bytecode.RJmp, bytecode.RJmpF, bytecode.RJmpT,
		bytecode.RJSEq, bytecode.RJSNe, bytecode.RJSEqI, bytecode.RJSNeI, bytecode.RJTypeof,
		bytecode.RSwitch, bytecode.RSwitchTypeof:
		return true
	case bytecode.RUnary:
		return bytecode.Op(w.X()) == bytecode.OpNot
	}
	return false
}

// maxStores is the most stores a plan of RegCode.Stores has.
const maxStores = 8

// plansOf sets rc's Stores, where fn's body is an initializer's, and its
// Forward, where it hands its parameters on to a method of its this by
// call (leafOf sets an apply's, LeafForward's). The native interpreter
// makes the stores itself, without the body's frame, where every site's
// cache answers for the object it is given: a function's or a method's
// body that only stores up to maxStores of its parameters in this, some
// made 0 where they are falsy, and returns undefined; and a base class's
// constructor's that does the same and returns this, whose plan only a
// construction runs (a class is not called without new). leafOf has
// turned away what does anything before its body.
func plansOf(fn *bytecode.Function, rc *bytecode.RegCode) {
	ctor := fn.Kind == bytecode.KindConstructor
	if fn.Kind != bytecode.KindNormal && fn.Kind != bytecode.KindMethod && !ctor || fn.UsesArguments || rc.Params > 64 {
		return
	}
	code := rc.Code
	if !ctor && len(code) >= 5 && code[0].Op() == bytecode.RGetProp {
		rc.Forward = forwardCall(fn, rc)
		return
	}
	param := func(r uint16) bool { return r >= regParams && int(r) < regParams+rc.Params }
	// The parameters if (!p) p = 0 has made 0 where they were falsy.
	var defaults uint64
	var stores []bytecode.ThisStore
	for pc := 0; pc < len(code); {
		w := code[pc]
		st := bytecode.ThisStore{Site: w.C()}
		switch {
		case w.Op() == bytecode.RJmpT && param(w.A()) && int32(w.BC()) == 1 && pc+1 < len(code):
			// if (!p) p = 0: jmp_true p, +1; load_int p, 0.
			if next := code[pc+1]; next.Op() != bytecode.RLoadInt || next.A() != w.A() || next.BC() != 0 {
				return
			}
			defaults |= 1 << (w.A() - regParams)
			pc += 2
			continue
		case w.Op() == bytecode.RJmpF && param(w.A()) && int32(w.BC()) == 2 && pc+4 < len(code):
			// this.k = p ? p : 0: jmp_false p, +2; move t, p; jmp +1;
			// load_int t, 0; set_prop this, t.
			move, jump, load, set := code[pc+1], code[pc+2], code[pc+3], code[pc+4]
			if move.Op() != bytecode.RMove || move.B() != w.A() || int(move.A()) < regParams+rc.Params ||
				jump.Op() != bytecode.RJmp || int32(jump.BC()) != 1 ||
				load.Op() != bytecode.RLoadInt || load.A() != move.A() || load.BC() != 0 ||
				set.Op() != bytecode.RSetProp || set.A() != regThis || set.B() != move.A() {
				return
			}
			st = bytecode.ThisStore{Site: set.C(), Param: w.A() - regParams, Zero: true}
			pc += 5
		case w.Op() == bytecode.RSetProp && w.A() == regThis && param(w.B()):
			st.Param = w.B() - regParams
			st.Zero = defaults&(1<<st.Param) != 0
			pc++
		case w.Op() == bytecode.RRetUndef, ctor && w.Op() == bytecode.RRet && w.A() == regThis:
			rc.Stores = stores
			return
		default:
			return
		}
		if len(stores) == maxStores {
			return
		}
		stores = append(stores, st)
	}
}

// forwardCall is the plan of a body that is this.k.call(this, ...) and
// nothing else: get_prop of this, get_method_w of call, the move of this
// and of each argument -- a parameter, or a constant loaded -- then
// call_method_w and the return of undefined; nil for any other.
func forwardCall(fn *bytecode.Function, rc *bytecode.RegCode) *bytecode.ForwardPlan {
	code := rc.Code
	m, c, t := code[0], code[1], code[2]
	if m.B() != regThis || c.Op() != bytecode.RGetMethodW || c.B() != m.A() || !siteIs(fn, rc, c.C(), "call") ||
		t.Op() != bytecode.RMove || t.A() != c.A()+2 || t.B() != regThis {
		return nil
	}
	plan := &bytecode.ForwardPlan{Method: m.C(), Fn: c.C(), Call: true}
	for i, w := range code[3:] {
		if w.Op() == bytecode.RCallMW {
			if w.A() != c.A() || int(w.X()) != len(plan.Args)+1 {
				return nil
			}
			for _, ret := range code[3+i+1:] {
				if ret.Op() != bytecode.RRetUndef {
					return nil
				}
			}
			return plan
		}
		if len(plan.Args) == maxStores || w.A() != c.A()+3+uint16(len(plan.Args)) {
			return nil
		}
		switch w.Op() {
		case bytecode.RMove:
			if w.B() < regParams || int(w.B()) >= regParams+rc.Params {
				return nil
			}
			plan.Args = append(plan.Args, bytecode.ForwardArg{Param: int32(w.B() - regParams)})
		case bytecode.RLoadInt, bytecode.RLoadConst, bytecode.RLoadUndef, bytecode.RLoadNull, bytecode.RLoadTrue, bytecode.RLoadFalse:
			plan.Args = append(plan.Args, bytecode.ForwardArg{Param: -1, Load: w})
		default:
			return nil
		}
	}
	return nil
}
