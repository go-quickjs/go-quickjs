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

	offRCCode = off("regCode.code")
	offRCSrc = off("regCode.src")
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
