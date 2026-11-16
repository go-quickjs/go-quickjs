package nativegen

// The offsets the handlers read: the native context's (nc), the
// frame record's (rec), and the other types' (off, size), from vm's
// layout (loadOffsets). Runtime's and closureCode's are relative to
// the context's rt and cc (rtRel, ccRel).
var (
	ncIP        int32
	ncPC        int32
	ncTop       int32
	ncGoTable   int32
	ncSlowTable int32
	ncCallH     int32
	ncAfter     int32
	ncCntExits  int32
	ncEnded     int32
	ncSt        int32
	ncStates    int32
	ncProtoStr  int32
	ncProtoNum  int32
	ncPrimProps int32
	ncW         int32
	ncRegs      int32
	ncTable     int32
	ncIC        int32
	ncBudget    int32
	ncObjBits   int32
	ncTagBase   int32
	ncOne       int32
	ncNaN       int32
	ncHole      int32
	ncUndef     int32
	ncTrue      int32
	ncFalse     int32
	ncWB        int32
	ncStore     int32
	ncEpoch     int32
	ncCodeBase  int32
	ncRC        int32
	ncNF        int32
	ncDepth     int32
	ncRT        int32
	ncCC        int32
	ncCCBase    int32
	ncMade      int32
	ncFilled    int32
	ncUsed      int32
	ncTopOK     int32
	ncRetDone   int32
	ncStrBits   int32
	ncTypeofs   int32
	ncFuncT     int32
	ncProxyT    int32
	ncTypedT    int32
	ncTypedEp   int32
	ncHas       int32
	ncBigHigh   int32
	ncBigHighX  int32
	ncNull      int32
	ncUninit    int32
	ncEmptyStr  int32
	ncSetterNx  int32
	ncPolyEnt   int32
	ncScratch   int32
	ncFrames    int32
	ncCap       int32
	ncFV        int32
	recSize     int32

	offObjClass int32
	offObjFlags int32
	sizeObj     int32
	offExtData  int32
	offStrLen   int32

	offTData     int32
	offTLength   int32
	offTEpoch    int32
	offTKind     int32
	offTReadonly int32

	offObjShape   int32
	offShapeProto int32
	offCellValid  int32
	offHKey       int32
	offHShape     int32
	offHProto     int32
	offHCell      int32
	offHHas       int32
	sizeH         int32

	offRtStack      int32
	offRtStackTop   int32
	offRtStackHigh  int32
	offRtFrameDepth int32
	offRtFrameBase  int32
	offRtFrameHigh  int32
	offRtMaxFrames  int32
	offRtInterrupt  int32
	offRtCur        int32
	offRtArgStack   int32
	offRtBackEdges  int32
	offShapeIndex   int32
	offIdxIndexKeys int32
	offStrS         int32
	offStrLeft      int32
	offStrAux       int32
	offStrASCII     int32
	offStrBuilding  int32
	offFDElemOp     int32
	offFDBound      int32
	offStF          int32
	offStC          int32
	offStCl         int32
	offStWb         int32
	offStErr        int32
	offRCLocals     int32
	recNFOff        int32
	recRCOff        int32
	recCodeBaseOff  int32
	offItDone       int32
	offItArr        int32
	offItIdx        int32
	offCCNames      int32
	offRCSiteName   int32
	offPEKey        int32
	offPEIdx        int32
	offPEProto      int32
	offPEShape      int32
	offStRegs       int32

	offFCl       int32
	offFBase     int32
	offFPC       int32
	offFRegKind  int32
	offFCallee   int32
	offFArgs     int32
	offFLocals   int32
	offFNewTgt   int32
	offFOpenUp   int32
	offFThisRef  int32
	offFEvalVars int32
	offFWith     int32
	offFNative   int32
	sizeFrame    int32

	offClCode      int32
	offClUpvals    int32
	offCCIC        int32
	offCCConsts    int32
	offCCRegEpochs int32
	offCCRegCalls  int32

	offRCCode      int32
	offRCSrc       int32
	offRCAux       int32
	offCCReg       int32
	offSymAtom     int32
	offSymAtoms    int32
	offRtAtoms     int32
	offRCNumRegs   int32
	offRCParams    int32
	offRCInits     int32
	offRCKeep      int32
	offRCNewTarget int32
	offRCNcap      int32
	offRCLeaf      int32
	offRCArgs      int32
	offRCLeafArg   int32
	offRCLeafVal   int32

	offECallee int32
	offECl     int32
	offERC     int32
	offEThis   int32
	offEKind   int32
	sizeE      int32

	recRetIP int32
	recSaved int32
	recBOff  int32
	recGen   int32

	// A generator's step (genStep, genYield): the cursor's, the
	// generator's and its frame's fields.
	offItIter    int32
	offItNext    int32
	offItForIn   int32
	offGen       int32
	offGCl       int32
	offGThis     int32
	offGCallee   int32
	offGVals     int32
	offGPC       int32
	offGNargs    int32
	offGSaved    int32
	offGRare     int32
	offGState    int32
	offGDeleg    int32
	offGStarted  int32
	offGReg      int32
	offGAwaitJob int32
	offClRealm   int32
	offRCGenFrom int32
	offRealmNext int32
	offFDName    int32
	offFHandlers int32
	offFSavedSP  int32
	offFParams   int32
	offFThis     int32

	// Closures and upvalues (closeUps, closure_w).
	ncClosureFn  int32
	ncLitFn      int32
	ncBuiltinFn  int32
	ncIterResFn  int32
	ncCallMissFn int32
	ncMiss       int32
	offUpSlot    int32
	offUpClosed  int32
	offRCLeafUp  int32
	offRCTailTo  int32

	offFiKeys      int32
	offFiObj       int32
	offFiShape     int32
	offFiIndexKeys int32
	offRtKeyAtoms  int32
	offRtIntStrs   int32
	offKAS         int32
	offKAAtom      int32
	offKANone      int32
	sizeKA         int32

	// Switches (switchW).
	offRCSwitches int32
	offSwDense    int32
	offSwLo       int32
	offSwHi       int32
	offSwByLen    int32
	offSwFlat     int32
	offSCK        int32
	offSCTo       int32
	offSCStr      int32
	offSwFew      int32
	offSwNums     int32
	offSwOther    int32
)

// nativeTopFields are the record's fields that the top frame's state is
// copied from and to, with the context's (the call handler, nativePop):
// a call saves them, a return puts them back.
var nativeTopFields [][2]int32

// loadOffsets sets the offsets from the layout being generated from.
func loadOffsets() {
	ncIP = off("nativeCtx.ip")
	ncPC = off("nativeCtx.pc")
	ncTop = off("nativeCtx.top")
	ncGoTable = off("nativeCtx.goTable")
	ncSlowTable = off("nativeCtx.slowTable")
	ncCallH = off("nativeCtx.callH")
	ncAfter = off("nativeCtx.after")
	ncCntExits = off("nativeCtx.cntExits")
	ncEnded = off("nativeCtx.ended")
	ncSt = off("nativeCtx.st")
	ncStates = off("nativeCtx.states")
	ncProtoStr = off("nativeCtx.protoStr")
	ncProtoNum = off("nativeCtx.protoNum")
	ncPrimProps = off("nativeCtx.primProps")
	ncW = off("nativeCtx.w")
	ncRegs = off("nativeCtx.regs")
	ncTable = off("nativeCtx.table")
	ncIC = off("nativeCtx.ic")
	ncBudget = off("nativeCtx.budget")
	ncObjBits = off("nativeCtx.objBits")
	ncTagBase = off("nativeCtx.tagBase")
	ncOne = off("nativeCtx.one")
	ncNaN = off("nativeCtx.nan")
	ncHole = off("nativeCtx.hole")
	ncUndef = off("nativeCtx.undef")
	ncTrue = off("nativeCtx.trueB")
	ncFalse = off("nativeCtx.falseB")
	ncWB = off("nativeCtx.wb")
	ncStore = off("nativeCtx.storeRef")
	ncEpoch = off("nativeCtx.epoch")
	ncCodeBase = off("nativeCtx.codeBase")
	ncRC = off("nativeCtx.rc")
	ncNF = off("nativeCtx.nf")
	ncDepth = off("nativeCtx.depth")
	ncRT = off("nativeCtx.rt")
	ncCC = off("nativeCtx.cc")
	ncCCBase = off("nativeCtx.ccBase")
	ncMade = off("nativeCtx.made")
	ncFilled = off("nativeCtx.filled")
	ncUsed = off("nativeCtx.used")
	ncTopOK = off("nativeCtx.topOK")
	ncRetDone = off("nativeCtx.retDone")
	ncStrBits = off("nativeCtx.strBits")
	ncTypeofs = off("nativeCtx.typeofs")
	ncFuncT = off("nativeCtx.funcType")
	ncProxyT = off("nativeCtx.proxyType")
	ncTypedT = off("nativeCtx.typedType")
	ncTypedEp = off("nativeCtx.typedEpoch")
	ncHas = off("nativeCtx.has")
	ncBigHigh = off("nativeCtx.bigHigh")
	ncBigHighX = off("nativeCtx.bigHighX")
	ncNull = off("nativeCtx.null")
	ncUninit = off("nativeCtx.uninit")
	ncEmptyStr = off("nativeCtx.emptyStr")
	ncSetterNx = off("nativeCtx.setterNext")
	ncPolyEnt = off("nativeCtx.polyEntries")
	ncScratch = off("nativeCtx.scratch")
	ncFrames = off("nativeCtx.frames")
	ncCap = off("nativeCtx.cap")
	ncFV = off("nativeCtx.fv")
	recSize = size("nativeRec")

	offObjClass = off("Object.class")
	offObjFlags = off("Object.flags")
	sizeObj = size("Object")
	offExtData = off("objectExt.data")
	offStrLen = off("String.length")

	offTData = off("typedArrayData.data")
	offTLength = off("typedArrayData.length")
	offTEpoch = off("typedArrayData.epoch")
	offTKind = off("typedArrayData.kind")
	offTReadonly = off("typedArrayData.readonly")

	offObjShape = off("Object.shape")
	offShapeProto = off("shape.proto")
	offCellValid = off("protoCell.valid")
	offHKey = off("hasEntry.key")
	offHShape = off("hasEntry.shape")
	offHProto = off("hasEntry.proto")
	offHCell = off("hasEntry.cell")
	offHHas = off("hasEntry.has")
	sizeH = size("hasEntry")

	offRtStack = rtRel(off("Runtime.stack"))
	offRtStackTop = rtRel(off("Runtime.stackTop"))
	offRtStackHigh = rtRel(off("Runtime.stackHigh"))
	offRtFrameDepth = rtRel(off("Runtime.frameDepth"))
	offRtFrameBase = rtRel(off("Runtime.frameBase"))
	offRtFrameHigh = rtRel(off("Runtime.frameHigh"))
	offRtMaxFrames = rtRel(off("Runtime.maxFrames"))
	offRtInterrupt = rtRel(off("Runtime.interruptCounter"))
	offRtCur = rtRel(off("Runtime.cur"))
	offRtArgStack = rtRel(off("Runtime.argStack"))
	offRtBackEdges = rtRel(off("Runtime.backEdges"))
	offShapeIndex = off("shape.index")
	offIdxIndexKeys = off("propIndex.indexKeys")
	offStrS = off("String.s")
	offStrLeft = off("String.left")
	offStrAux = off("String.aux")
	offStrASCII = off("String.ascii")
	offStrBuilding = off("String.building")
	offFDElemOp = off("funcData.elemOp")
	offFDBound = off("funcData.bound")
	offStF = off("regState.f")
	offStC = off("regState.c")
	offStCl = off("regState.cl")
	offStWb = off("regState.wb")
	offStErr = off("regState.err")
	offRCLocals = off("regCode.locals")
	recNFOff = off("nativeRec.nf")
	recRCOff = off("nativeRec.rc")
	recCodeBaseOff = off("nativeRec.codeBase")
	offItDone = off("iterObject.st") + off("iterState.done")
	offItArr = off("iterObject.st") + off("iterState.arr")
	offItIdx = off("iterObject.st") + off("iterState.idx")
	offCCNames = ccRel(off("closureCode.names"))
	offRCSiteName = off("regCode.siteName")
	offPEKey = off("primEntry.key")
	offPEIdx = off("primEntry.idx")
	offPEProto = off("primEntry.proto")
	offPEShape = off("primEntry.shape")
	offStRegs = off("regState.regs")

	offFCl = off("frame.cl")
	offFBase = off("frame.base")
	offFPC = off("frame.pc")
	offFRegKind = off("frame.regKind")
	offFCallee = off("frame.callee")
	offFArgs = off("frame.args")
	offFLocals = off("frame.locals")
	offFNewTgt = off("frame.newTarget")
	offFOpenUp = off("frame.openUpvalues")
	offFThisRef = off("frame.thisRef")
	offFEvalVars = off("frame.evalVars")
	offFWith = off("frame.withScopes")
	offFNative = off("frame.native")
	sizeFrame = size("frame")

	offClCode = off("closure.closureCode")
	offClUpvals = off("closure.upvalues")
	offCCIC = ccRel(off("closureCode.ic"))
	offCCConsts = ccRel(off("closureCode.consts"))
	offCCRegEpochs = ccRel(off("closureCode.regEpochs"))
	offCCRegCalls = ccRel(off("closureCode.regCalls"))
	offCCReg = ccRel(off("closureCode.reg"))

	offRCCode = off("regCode.code")
	offRCSrc = off("regCode.src")
	offRCAux = off("regCode.aux")
	offSymAtom = off("Symbol.atom")
	offSymAtoms = off("Symbol.atoms")
	offRtAtoms = rtRel(off("Runtime.atoms"))
	offRCNumRegs = off("regCode.numRegs")
	offRCParams = off("regCode.params")
	offRCInits = off("regCode.inits")
	offRCKeep = off("regCode.keep")
	offRCNewTarget = off("regCode.newTarget")
	offRCNcap = off("regCode.ncap")
	offRCLeaf = off("regCode.leaf")
	offRCArgs = off("regCode.args")
	offRCLeafArg = off("regCode.leafArg")
	offRCLeafVal = off("regCode.leafVal")

	offECallee = off("regCallCache.callee")
	offECl = off("regCallCache.cl")
	offERC = off("regCallCache.rc")
	offEThis = off("regCallCache.this")
	offEKind = off("regCallCache.kind")
	sizeE = size("regCallCache")

	recRetIP = off("nativeRec.retIP")
	recSaved = off("nativeRec.saved")
	recBOff = off("nativeRec.bOff")
	recGen = off("nativeRec.gen")

	offItIter = off("iterObject.st") + off("iterState.iter")
	offItNext = off("iterObject.st") + off("iterState.next")
	offItForIn = off("iterObject.st") + off("iterState.forIn")
	offGen = off("generatorObject.g")
	offGCl = off("generator.cl")
	offGThis = off("generator.this")
	offGCallee = off("generator.callee")
	offGVals = off("generator.vals")
	offGPC = off("generator.pc")
	offGNargs = off("generator.nargs")
	offGSaved = off("generator.saved")
	offGRare = off("generator.rare")
	offGState = off("generator.state")
	offGDeleg = off("generator.delegating")
	offGStarted = off("generator.started")
	offGReg = off("generator.reg")
	offGAwaitJob = off("generator.awaitJob")
	offClRealm = off("closure.realm")
	offRCGenFrom = off("regCode.genFrom")
	offRealmNext = off("Realm.genNextFn")
	offFDName = off("funcData.name")
	offFHandlers = off("frame.handlers")
	offFSavedSP = off("frame.savedSP")
	offFParams = off("frame.paramsOnly")
	offFThis = off("frame.this")
	ncClosureFn = off("nativeCtx.closureFn")
	ncLitFn = off("nativeCtx.litFn")
	ncBuiltinFn = off("nativeCtx.builtinFn")
	ncIterResFn = off("nativeCtx.iterResFn")
	ncCallMissFn = off("nativeCtx.callMissFn")
	ncMiss = off("nativeCtx.miss")
	offUpSlot = off("upvalue.slot")
	offUpClosed = off("upvalue.closed")
	offRCLeafUp = off("regCode.leafUp")
	offRCTailTo = off("regCode.tailTo")
	offFiKeys = off("forInObject.in") + off("forInState.keys")
	offFiObj = off("forInObject.in") + off("forInState.obj")
	offFiShape = off("forInObject.in") + off("forInState.shape")
	offFiIndexKeys = off("forInObject.in") + off("forInState.indexKeys")
	offRtKeyAtoms = rtRel(off("Runtime.keyAtoms"))
	offRtIntStrs = rtRel(off("Runtime.intStrings"))
	offKAS = off("keyAtom.s")
	offKAAtom = off("keyAtom.atom")
	offKANone = off("keyAtom.none")
	sizeKA = size("keyAtom")
	offRCSwitches = off("regCode.switches")
	offSwDense = off("regSwitchTable.dense")
	offSwLo = off("regSwitchTable.lo")
	offSwHi = off("regSwitchTable.hi")
	offSwByLen = off("regSwitchTable.byLen")
	offSwFlat = off("regSwitchTable.flat")
	offSCK = off("SwitchCase.K")
	offSCTo = off("SwitchCase.To")
	offSCStr = off("SwitchCase.Str")
	offSwFew = off("regSwitchTable.few")
	offSwNums = off("regSwitchTable.nums")
	offSwOther = off("regSwitchTable.other")

	nativeTopFields = [][2]int32{
		{off("nativeRec.regs"), ncRegs},
		{off("nativeRec.ic"), ncIC},
		{off("nativeRec.cc"), ncCC},
		{off("nativeRec.codeBase"), ncCodeBase},
		{off("nativeRec.rc"), ncRC},
		{off("nativeRec.nf"), ncNF},
		{off("nativeRec.fv"), ncFV},
	}
}

// scr is scratch word i.
func scr(i int) int32 { return ncScratch + int32(8*i) }

// rtRel and ccRel are a Runtime's and a closureCode's field's offset from
// the context's rt and cc (rtBase, ccBase).
func rtRel(off int32) int32 { return off - int32(rtBase) }

func ccRel(off int32) int32 { return off - int32(ccBase) }
