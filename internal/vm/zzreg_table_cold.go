package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// The handlers of the operations of no hot loop -- declarations, classes,
// destructuring, generators, with, eval (bytecode.ROp.Cold) -- each a
// handler of its own in regTable, as every other operation's is: the
// frame's pc set, as the operation may throw or run code (regState.at),
// and the operation's function called, its error the fail tail's.

func regColdInit() {
	regTable[bytecode.RNewRegExp] = rhNewRegExp
	regTable[bytecode.RForOfStart] = rhForOfStart
	regTable[bytecode.RForInStart] = rhForInStart
	regTable[bytecode.RIterClose] = rhIterClose
	regTable[bytecode.RIterCloseThrow] = rhIterCloseThrow
	regTable[bytecode.RCheckGlobalLex] = rhCheckGlobalLex
	regTable[bytecode.RCheckGlobalVar] = rhCheckGlobalVar
	regTable[bytecode.RDefineGlobalVar] = rhDefineGlobalVar
	regTable[bytecode.RDeclareGlobalLex] = rhDeclareGlobalLex
	regTable[bytecode.RDefineGlobalFunc] = rhDefineGlobalFunc
	regTable[bytecode.RInitGlobalLex] = rhInitGlobalLex
	regTable[bytecode.RGetGlobalOpt] = rhGetGlobalOpt
	regTable[bytecode.RDeclareModuleLex] = rhDeclareModuleLex
	regTable[bytecode.RInitModuleLex] = rhInitModuleLex
	regTable[bytecode.RIterStep] = rhIterStep
	regTable[bytecode.RIterRest] = rhIterRest
	regTable[bytecode.RCheckCoercible] = rhCheckCoercible
	regTable[bytecode.RToPropertyKey] = rhToPropertyKey
	regTable[bytecode.RObjectRest] = rhObjectRest
	regTable[bytecode.RRestParam] = rhRestParam
	regTable[bytecode.RToString] = rhToString
	regTable[bytecode.RTemplateObject] = rhTemplateObject
	regTable[bytecode.RCopyDataProps] = rhCopyDataProps
	regTable[bytecode.RDefineGetter] = rhDefineGetter
	regTable[bytecode.RDefineSetter] = rhDefineSetter
	regTable[bytecode.RDefineGetterIndex] = rhDefineGetterIndex
	regTable[bytecode.RDefineSetterIndex] = rhDefineSetterIndex
	regTable[bytecode.RDefineIndex] = rhDefineIndex
	regTable[bytecode.RSetFuncName] = rhSetFuncName
	regTable[bytecode.RSetHomeObject] = rhSetHomeObject
	regTable[bytecode.RSetProtoOf] = rhSetProtoOf
	regTable[bytecode.RArrayPush] = rhArrayPush
	regTable[bytecode.RArraySpread] = rhArraySpread
	regTable[bytecode.RCallSpread] = rhCallSpread
	regTable[bytecode.RAssignConst] = rhAssignConst
	regTable[bytecode.RCheckGlobalRef] = rhCheckGlobalRef
	regTable[bytecode.RAssertResolved] = rhAssertResolved
	regTable[bytecode.RNewClass] = rhNewClass
	regTable[bytecode.RSetFieldInit] = rhSetFieldInit
	regTable[bytecode.RDefineMethod] = rhDefineMethod
	regTable[bytecode.RClassGetter] = rhClassGetter
	regTable[bytecode.RClassSetter] = rhClassSetter
	regTable[bytecode.RClassGetterIndex] = rhClassGetterIndex
	regTable[bytecode.RClassSetterIndex] = rhClassSetterIndex
	regTable[bytecode.RClassIndex] = rhClassIndex
	regTable[bytecode.RPrivateName] = rhPrivateName
	regTable[bytecode.RNewPrivateMethods] = rhNewPrivateMethods
	regTable[bytecode.RAddPrivateMethod] = rhAddPrivateMethod
	regTable[bytecode.RAddPrivateGetter] = rhAddPrivateGetter
	regTable[bytecode.RAddPrivateSetter] = rhAddPrivateSetter
	regTable[bytecode.RInstallPrivateMethods] = rhInstallPrivateMethods
	regTable[bytecode.RDefinePrivate] = rhDefinePrivate
	regTable[bytecode.RDefinePrivateMethod] = rhDefinePrivateMethod
	regTable[bytecode.RDefinePrivateGetter] = rhDefinePrivateGetter
	regTable[bytecode.RDefinePrivateSetter] = rhDefinePrivateSetter
	regTable[bytecode.RPrivateIn] = rhPrivateIn
	regTable[bytecode.RSuperCall] = rhSuperCall
	regTable[bytecode.RSuperForward] = rhSuperForward
	regTable[bytecode.RSuperSpread] = rhSuperSpread
	regTable[bytecode.RSuperGet] = rhSuperGet
	regTable[bytecode.RSuperSet] = rhSuperSet
	regTable[bytecode.RSuperBase] = rhSuperBase
	regTable[bytecode.RSuperGetIndex] = rhSuperGetIndex
	regTable[bytecode.RSuperSetIndex] = rhSuperSetIndex
	regTable[bytecode.RThis] = rhThis
	regTable[bytecode.RCheckThis] = rhCheckThis
	regTable[bytecode.RNewTarget] = rhNewTarget
	regTable[bytecode.RDerivedRet] = rhDerivedRet
	regTable[bytecode.RDeleteSuper] = rhDeleteSuper
	regTable[bytecode.RCloseRegs] = rhCloseRegs
	regTable[bytecode.RWrap] = rhWrap
	regTable[bytecode.RGenEntry] = rhGenEntry
	regTable[bytecode.REndParams] = rhEndParams
	regTable[bytecode.RAwait] = rhAwait
	regTable[bytecode.RYieldStar] = rhYieldStar
	regTable[bytecode.RIterResume] = rhIterResume
	regTable[bytecode.RIterUnpackDelegate] = rhIterUnpackDelegate
	regTable[bytecode.RForAwaitOfStart] = rhForAwaitOfStart
	regTable[bytecode.RAsyncIterNext] = rhAsyncIterNext
	regTable[bytecode.RIterResult] = rhIterResult
	regTable[bytecode.RAsyncIterClose] = rhAsyncIterClose
	regTable[bytecode.RAsyncIterCheck] = rhAsyncIterCheck
	regTable[bytecode.RGetUpvalCheck] = rhGetUpvalCheck
	regTable[bytecode.RSetUpvalCheck] = rhSetUpvalCheck
	regTable[bytecode.RDeleteVar] = rhDeleteVar
	regTable[bytecode.RImport] = rhImport
	regTable[bytecode.RImportMeta] = rhImportMeta
	regTable[bytecode.RNewDispose] = rhNewDispose
	regTable[bytecode.RAddDisposable] = rhAddDisposable
	regTable[bytecode.RDispose] = rhDispose
	regTable[bytecode.RDisposeStep] = rhDisposeStep
	regTable[bytecode.RDirectEval] = rhDirectEval
	regTable[bytecode.RWithGet] = rhWithGet
	regTable[bytecode.RWithGetThis] = rhWithGetThis
	regTable[bytecode.RWithDelete] = rhWithDelete
	regTable[bytecode.RWithSet] = rhWithSet
	regTable[bytecode.RWithResolve] = rhWithResolve
	regTable[bytecode.RWithRead] = rhWithRead
	regTable[bytecode.RWithWrite] = rhWithWrite
	regTable[bytecode.RWithPush] = rhWithPush
	regTable[bytecode.RWithPop] = rhWithPop
	regTable[bytecode.RThrowRef] = rhThrowRef
	regTable[bytecode.RDebug] = rhDebug
	regTable[bytecode.RMovFar] = rhMovFar
	regTable[bytecode.RMovToFar] = rhMovToFar
	regTable[bytecode.RFarInit] = rhFarInit
}

// rhNewRegExp is new_regexp.
func rhNewRegExp(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	v, err := st.r.newRegExpLiteral(regConst(f.cl.fn, st.c, w.BC()))
	return st.put(regs, w, v, err)
}

// rhForOfStart is for_of_start.
func rhForOfStart(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	v, err := st.r.startForOf(regs[w.B()])
	return st.put(regs, w, v, err)
}

// rhForInStart is for_in_start.
func rhForInStart(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	v, err := st.r.startForIn(regs[w.B()])
	return st.put(regs, w, v, err)
}

// rhIterClose is iter_close.
func rhIterClose(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	return st.done(st.r.iterCloseNormal(regs[w.B()]))
}

// rhIterCloseThrow is iter_close_throw.
func rhIterCloseThrow(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	// As closeIteratorsIn closes a cursor an exception leaves: what its
	// return method throws is dropped.
	r := st.r
	st.at(pc)
	cur := cursorOf(regs[w.B()])
	if cur == nil || cur.forIn || cur.done || cur.delegation || cur.arr != nil && !r.unwalkToClose(cur) {
		return pc + 1
	}
	cur.done = true
	r.closeIterator(cur.iter)
	return pc + 1
}

// rhCheckGlobalLex is check_global_lex.
func rhCheckGlobalLex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regCheckGlobalLex(st.at(pc), st.c, pc, w, regs))
}

// rhCheckGlobalVar is check_global_var.
func rhCheckGlobalVar(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regCheckGlobalVar(st.at(pc), st.c, pc, w, regs))
}

// rhDefineGlobalVar is define_global_var.
func rhDefineGlobalVar(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefineGlobalVar(st.at(pc), st.c, pc, w, regs))
}

// rhDeclareGlobalLex is declare_global_lex.
func rhDeclareGlobalLex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDeclareGlobalLex(st.at(pc), st.c, pc, w, regs))
}

// rhDefineGlobalFunc is define_global_func.
func rhDefineGlobalFunc(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefineGlobalFunc(st.at(pc), st.c, pc, w, regs))
}

// rhInitGlobalLex is init_global_lex.
func rhInitGlobalLex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regInitGlobalLex(st.at(pc), st.c, pc, w, regs))
}

// rhGetGlobalOpt is get_global_opt.
func rhGetGlobalOpt(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	v, err := st.r.regGlobalOpt(f, regName(f.cl, st.c, pc, w.B()))
	regs[w.A()] = v
	return st.done(err)
}

// rhDeclareModuleLex is declare_module_lex.
func rhDeclareModuleLex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDeclareModuleLex(st.at(pc), st.c, pc, w, regs))
}

// rhInitModuleLex is init_module_lex.
func rhInitModuleLex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regInitModuleLex(st.at(pc), st.c, pc, w, regs))
}

// rhIterStep is iter_step.
func rhIterStep(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regIterStep(st.at(pc), st.c, pc, w, regs))
}

// rhIterRest is iter_rest.
func rhIterRest(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regIterRest(st.at(pc), st.c, pc, w, regs))
}

// rhCheckCoercible is check_coercible.
func rhCheckCoercible(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regCheckCoercible(st.at(pc), st.c, pc, w, regs))
}

// rhToPropertyKey is to_property_key.
func rhToPropertyKey(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regToPropertyKey(st.at(pc), st.c, pc, w, regs))
}

// rhObjectRest is object_rest.
func rhObjectRest(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regObjectRest(st.at(pc), st.c, pc, w, regs))
}

// rhRestParam is rest_param.
func rhRestParam(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regRestParam(st.at(pc), st.c, pc, w, regs))
}

// rhToString is to_string.
func rhToString(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regToString(st.at(pc), st.c, pc, w, regs))
}

// rhTemplateObject is template_object.
func rhTemplateObject(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regTemplateObject(st.at(pc), st.c, pc, w, regs))
}

// rhCopyDataProps is copy_data_props.
func rhCopyDataProps(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regCopyDataProps(st.at(pc), st.c, pc, w, regs))
}

// rhDefineGetter is define_getter.
func rhDefineGetter(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefineAccessor(st.at(pc), st.c, pc, w, regs))
}

// rhDefineSetter is define_setter.
func rhDefineSetter(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefineAccessor(st.at(pc), st.c, pc, w, regs))
}

// rhDefineGetterIndex is define_getter_index.
func rhDefineGetterIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefineAccessorIndex(st.at(pc), st.c, pc, w, regs))
}

// rhDefineSetterIndex is define_setter_index.
func rhDefineSetterIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefineAccessorIndex(st.at(pc), st.c, pc, w, regs))
}

// rhDefineIndex is define_index.
func rhDefineIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefineIndex(st.at(pc), st.c, pc, w, regs))
}

// rhSetFuncName is set_func_name.
func rhSetFuncName(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSetFuncName(st.at(pc), st.c, pc, w, regs))
}

// rhSetHomeObject is set_home_object.
func rhSetHomeObject(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSetHomeObject(st.at(pc), st.c, pc, w, regs))
}

// rhSetProtoOf is set_proto_of.
func rhSetProtoOf(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSetProtoOf(st.at(pc), st.c, pc, w, regs))
}

// rhArrayPush is array_push.
func rhArrayPush(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regArrayPush(st.at(pc), st.c, pc, w, regs))
}

// rhArraySpread is array_spread.
func rhArraySpread(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regArraySpread(st.at(pc), st.c, pc, w, regs))
}

// rhCallSpread is call_spread.
func rhCallSpread(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regCallSpread(st.at(pc), st.c, pc, w, regs))
}

// rhAssignConst is assign_const.
func rhAssignConst(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	// An assignment to a constant is a runtime error, not an early one.
	r, f := st.r, st.at(pc)
	return st.fail(r.throwTypeError("assignment to constant variable %q", r.atoms.name(regName(f.cl, st.c, pc, w.B()))))
}

// rhCheckGlobalRef is check_global_ref.
func rhCheckGlobalRef(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	found, err := st.r.regCheckGlobalRef(f, regName(f.cl, st.c, pc, w.B()), int(w.C()))
	regs[w.A()] = Bool(found)
	return st.done(err)
}

// rhAssertResolved is assert_resolved.
func rhAssertResolved(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r, f := st.r, st.at(pc)
	if !regs[w.A()].Truthy() {
		return st.fail(r.throwReferenceError("%s is not defined", r.atoms.name(regName(f.cl, st.c, pc, w.B()))))
	}
	return pc + 1
}

// rhNewClass is new_class.
func rhNewClass(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regNewClass(st.at(pc), st.c, pc, w, regs))
}

// rhSetFieldInit is set_field_init.
func rhSetFieldInit(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSetFieldInit(st.at(pc), st.c, pc, w, regs))
}

// rhDefineMethod is define_method.
func rhDefineMethod(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefineMethod(st.at(pc), st.c, pc, w, regs))
}

// rhClassGetter is class_getter.
func rhClassGetter(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regClassAccessor(st.at(pc), st.c, pc, w, regs))
}

// rhClassSetter is class_setter.
func rhClassSetter(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regClassAccessor(st.at(pc), st.c, pc, w, regs))
}

// rhClassGetterIndex is class_getter_index.
func rhClassGetterIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regClassAccessorIndex(st.at(pc), st.c, pc, w, regs))
}

// rhClassSetterIndex is class_setter_index.
func rhClassSetterIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regClassAccessorIndex(st.at(pc), st.c, pc, w, regs))
}

// rhClassIndex is class_index.
func rhClassIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regClassIndex(st.at(pc), st.c, pc, w, regs))
}

// rhPrivateName is private_name.
func rhPrivateName(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regPrivateName(st.at(pc), st.c, pc, w, regs))
}

// rhNewPrivateMethods is new_private_methods.
func rhNewPrivateMethods(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regNewPrivateMethods(st.at(pc), st.c, pc, w, regs))
}

// rhAddPrivateMethod is add_private_method.
func rhAddPrivateMethod(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regAddPrivateMethod(st.at(pc), st.c, pc, w, regs))
}

// rhAddPrivateGetter is add_private_getter.
func rhAddPrivateGetter(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regAddPrivateGetter(st.at(pc), st.c, pc, w, regs))
}

// rhAddPrivateSetter is add_private_setter.
func rhAddPrivateSetter(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regAddPrivateSetter(st.at(pc), st.c, pc, w, regs))
}

// rhInstallPrivateMethods is install_private_methods.
func rhInstallPrivateMethods(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regInstallPrivateMethods(st.at(pc), st.c, pc, w, regs))
}

// rhDefinePrivate is define_private.
func rhDefinePrivate(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefinePrivateField(st.at(pc), st.c, pc, w, regs))
}

// rhDefinePrivateMethod is define_private_method.
func rhDefinePrivateMethod(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefinePrivateField(st.at(pc), st.c, pc, w, regs))
}

// rhDefinePrivateGetter is define_private_getter.
func rhDefinePrivateGetter(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefinePrivateAccessor(st.at(pc), st.c, pc, w, regs))
}

// rhDefinePrivateSetter is define_private_setter.
func rhDefinePrivateSetter(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDefinePrivateAccessor(st.at(pc), st.c, pc, w, regs))
}

// rhPrivateIn is private_in.
func rhPrivateIn(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regPrivateIn(st.at(pc), st.c, pc, w, regs))
}

// rhSuperCall is super_call.
func rhSuperCall(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSuperCallOf(st.at(pc), st.c, pc, w, regs))
}

// rhSuperForward is super_forward.
func rhSuperForward(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSuperCallOf(st.at(pc), st.c, pc, w, regs))
}

// rhSuperSpread is super_spread.
func rhSuperSpread(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSuperCallOf(st.at(pc), st.c, pc, w, regs))
}

// rhSuperGet is super_get.
func rhSuperGet(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSuperGet(st.at(pc), st.c, pc, w, regs))
}

// rhSuperSet is super_set.
func rhSuperSet(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSuperSet(st.at(pc), st.c, pc, w, regs))
}

// rhSuperBase is super_base.
func rhSuperBase(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSuperBase(st.at(pc), st.c, pc, w, regs))
}

// rhSuperGetIndex is super_get_index.
func rhSuperGetIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSuperGetIndex(st.at(pc), st.c, pc, w, regs))
}

// rhSuperSetIndex is super_set_index.
func rhSuperSetIndex(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSuperSetIndex(st.at(pc), st.c, pc, w, regs))
}

// rhThis is this_bound.
func rhThis(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regThis(st.at(pc), st.c, pc, w, regs))
}

// rhCheckThis is check_this_init.
func rhCheckThis(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regCheckThis(st.at(pc), st.c, pc, w, regs))
}

// rhNewTarget is new_target.
func rhNewTarget(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regNewTarget(st.at(pc), st.c, pc, w, regs))
}

// rhDerivedRet is derived_ret.
func rhDerivedRet(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDerivedRet(st.at(pc), st.c, pc, w, regs))
}

// rhDeleteSuper is delete_super.
func rhDeleteSuper(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDeleteSuper(st.at(pc), st.c, pc, w, regs))
}

// rhCloseRegs is close_regs.
func rhCloseRegs(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regCloseRegsOp(st.at(pc), st.c, pc, w, regs))
}

// rhWrap is wrap.
func rhWrap(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	return st.done(st.r.regColdWrap(st.cl, st.c, &st.kept, w, regs))
}

// rhGenEntry is gen_entry.
func rhGenEntry(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regGenEntry(st.at(pc), st.c, &st.kept, w, regs))
}

// rhEndParams is end_params.
func rhEndParams(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSuspend(st.at(pc), st.c, &st.kept, w, regs))
}

// rhAwait is await.
func rhAwait(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSuspend(st.at(pc), st.c, &st.kept, w, regs))
}

// rhYieldStar is yield_star.
func rhYieldStar(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regSuspend(st.at(pc), st.c, &st.kept, w, regs))
}

// rhIterResume is iter_resume.
func rhIterResume(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regIterResume(st.at(pc), st.c, &st.kept, w, regs))
}

// rhIterUnpackDelegate is iter_unpack_delegate.
func rhIterUnpackDelegate(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regIterUnpackDelegate(st.at(pc), st.c, &st.kept, w, regs))
}

// rhForAwaitOfStart is for_await_of_start.
func rhForAwaitOfStart(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regAsyncStart(st.at(pc), st.c, &st.kept, w, regs))
}

// rhAsyncIterNext is async_iter_next.
func rhAsyncIterNext(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regAsyncStart(st.at(pc), st.c, &st.kept, w, regs))
}

// rhIterResult is iter_result.
func rhIterResult(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regIterResult(st.at(pc), st.c, &st.kept, w, regs))
}

// rhAsyncIterClose is async_iter_close.
func rhAsyncIterClose(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regAsyncIterClose(st.at(pc), st.c, &st.kept, w, regs))
}

// rhAsyncIterCheck is async_iter_check.
func rhAsyncIterCheck(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regAsyncIterCheck(st.at(pc), st.c, &st.kept, w, regs))
}

// rhGetUpvalCheck is get_upval_check.
func rhGetUpvalCheck(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regUpvalCheck(st.at(pc), w, regs))
}

// rhSetUpvalCheck is set_upval_check.
func rhSetUpvalCheck(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regUpvalCheck(st.at(pc), w, regs))
}

// rhDeleteVar is delete_var.
func rhDeleteVar(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	v, err := deleteVar(st.r, f, f.cl, regName(f.cl, st.c, pc, w.B()))
	regs[w.A()] = v
	return st.done(err)
}

// rhImport is import.
func rhImport(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	options := Undefined
	if w.C()&3 == 2 {
		options = regs[w.B()+1]
	}
	regs[w.A()] = st.r.importCall(uint32(w.C()>>2), regs[w.B()], options)
	return pc + 1
}

// rhImportMeta is import_meta.
func rhImportMeta(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	f := st.at(pc)
	regs[w.A()] = st.r.importMeta(f.cl.env)
	return pc + 1
}

// rhNewDispose is new_dispose_capability.
func rhNewDispose(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	return st.done(st.r.regNewDispose(w, regs))
}

// rhAddDisposable is add_disposable.
func rhAddDisposable(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	return st.done(st.r.regAddDisposable(w, regs))
}

// rhDispose is dispose.
func rhDispose(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	return st.done(st.r.regDispose(w, regs))
}

// rhDisposeStep is dispose_step.
func rhDisposeStep(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	st.at(pc)
	return st.done(st.r.regDisposeStep(w, regs))
}

// rhDirectEval is direct_eval.
func rhDirectEval(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDirectEval(st.at(pc), st.c, pc, w, regs))
}

// rhWithGet is with_get.
func rhWithGet(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regWithGet(st.at(pc), st.c, pc, w, regs))
}

// rhWithGetThis is with_get_this.
func rhWithGetThis(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regWithGet(st.at(pc), st.c, pc, w, regs))
}

// rhWithDelete is with_delete.
func rhWithDelete(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regWithDelete(st.at(pc), st.c, pc, w, regs))
}

// rhWithSet is with_set.
func rhWithSet(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regWithSet(st.at(pc), st.c, pc, w, regs))
}

// rhWithResolve is with_resolve.
func rhWithResolve(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regWithResolve(st.at(pc), st.c, pc, w, regs))
}

// rhWithRead is with_read.
func rhWithRead(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regWithRead(st.at(pc), st.c, pc, w, regs))
}

// rhWithWrite is with_write.
func rhWithWrite(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regWithWrite(st.at(pc), st.c, pc, w, regs))
}

// rhWithPush is with_push.
func rhWithPush(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regWithPush(st.at(pc), st.c, pc, w, regs))
}

// rhWithPop is with_pop.
func rhWithPop(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regWithPop(st.at(pc), st.c, pc, w, regs))
}

// rhThrowRef is throw_reference_error.
func rhThrowRef(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r, f := st.r, st.at(pc)
	return st.fail(r.throwError(errReference, "%s", r.atoms.name(regName(f.cl, st.c, pc, w.B()))))
}

// rhDebug is debug.
func rhDebug(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	return st.done(st.r.regDebug(st.at(pc), st.c, pc, w, regs))
}

// rhMovFar is mov_far.
func rhMovFar(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	regs[w.A()] = *regFar(regs, w.BC())
	return pc + 1
}

// rhMovToFar is mov_to_far.
func rhMovToFar(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	*regFar(regs, w.BC()) = regs[w.A()]
	return pc + 1
}

// rhFarInit is far_init.
func rhFarInit(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	for i, n := w.BC(), w.BC()+regAuxAt(st.c, pc); i < n; i++ {
		*regFar(regs, i) = Undefined
	}
	return pc + 1
}
