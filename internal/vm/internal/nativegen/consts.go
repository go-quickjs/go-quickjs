package nativegen

// vm's constants that the handlers test values and states against, by
// vm's names and of vm's types, from vm's layout (loadConsts).

// Kind is a value's kind (vm's Kind), Class an object's class, objFlags
// an object's flags and elemType a typed array's element type.
type (
	Kind     uint8
	Class    uint8
	objFlags uint16
	elemType uint8
)

// A value's words and kinds.
var (
	tagBase, shortIntBase, shortBigBase, heapBigBits uint64
	shortBigLowBits                                  uint8

	KindUndefined, KindNull, KindBool, KindString, KindSymbol, KindObject, KindBigInt Kind
)

// Objects' classes and flags, and typed arrays' element types.
var (
	ClassObject, ClassArray, ClassTypedArray, ClassCursor, ClassGenerator, ClassProxy Class

	objExtensible, objHasExt, objHasData, objHasSparseElements, objSharedElems,
	objPolyProto, objMappedArguments, objArrayLengthWritable, objHTMLDDA objFlags

	elemInt8, elemUint8, elemUint8Clamped, elemInt16, elemUint16, elemInt32,
	elemUint32, elemFloat32, elemFloat64, elemBigInt64, elemBigUint64 elemType
)

// Calls, leaves and caches; and the bases of the Runtime's and a
// closureCode's fields the code reads relative to the context's rt and cc.
var (
	elemStrCharCodeAt, elemPush int32
	// elemStrAt, elemStrCharAt and elemStrCodePointAt are the elemOps of
	// String.prototype's at, charAt and codePointAt, which with
	// charCodeAt's are four in a row (strIntrinsic).
	elemStrAt, elemStrCharAt, elemStrCodePointAt int32

	regCallEnter, regCallThis, regCallObjThis, regCallOut, regCallLeaf, regLeafArg int32
	regCallNative, regCallConstruct, regCallThrough                                int32
	regLeafCode                                                                    int32
	regFrameCall, regFrameEnter, regFrameGenStep, regFrameConstruct, regFrameSuper int32

	// A generator's states.
	genSuspendedYield, genExecuting, genCompleted int32

	tailCallDepth, linearScanLimit int32
	// maxCacheFills is when a site gives up; fwdApply and fwdCall a
	// forwarder's kinds, and fwdWays and fwdShift its table of its
	// method's reads (vm's fwdSite).
	maxCacheFills, fwdApply, fwdCall, fwdWays, fwdShift int32
	// atomIndexTag bounds an index an atom holds (hasIndexQuick's).
	atomIndexTag int32
	// atomPrototype is the key prototype, and propOddKey the flags of a
	// property that is no plain data property: an accessor, a private
	// name, one deleted or not initialized yet.
	atomPrototype, propOddKey int32
	// atomLength is the key length (arrayLength).
	atomLength   int32
	hasCacheBits uint8
	hasCacheMul  uint64
	// hasCacheWaysLog is log2 of hasCacheWays, the entries of a set.
	hasCacheWaysLog uint8
	hasCacheWays    int32
	// keySlotMul is keySet's multiplier, and intStringsLen how many
	// integers' strings the runtime keeps (intStrings).
	keySlotMul, intStringsLen uint64
	// keySetShift is how far keySet shifts the hash down to a set of
	// keyAtoms, keyWaysLog log2 of its keyWays entries, and
	// keyAddrShift how far it shifts a string's address down before it
	// multiplies it (keySetOf).
	keySetShift, keyWaysLog, keyAddrShift uint8

	rtBase, ccBase uint64
)

// The call handler's scratch words (vm's nativeCtx.scratch).
var sE, sWin, sKind, sRC, sArgc, sBase, sTop, sNF, sSaved, sI, sEnd, sV0, sV1, sEnt, sKa int

// loadConsts sets the constants from the layout being generated from.
func loadConsts() {
	k := lay.konst
	tagBase, shortIntBase, shortBigBase, heapBigBits = k("tagBase"), k("shortIntBase"), k("shortBigBase"), k("heapBigBits")
	shortBigLowBits = uint8(k("shortBigLowBits"))
	for _, c := range []struct {
		p    *Kind
		name string
	}{{&KindUndefined, "KindUndefined"}, {&KindNull, "KindNull"}, {&KindBool, "KindBool"}, {&KindString, "KindString"},
		{&KindSymbol, "KindSymbol"}, {&KindObject, "KindObject"}, {&KindBigInt, "KindBigInt"}} {
		*c.p = Kind(k(c.name))
	}
	for _, c := range []struct {
		p    *Class
		name string
	}{{&ClassObject, "ClassObject"}, {&ClassArray, "ClassArray"}, {&ClassTypedArray, "ClassTypedArray"}, {&ClassCursor, "ClassCursor"},
		{&ClassGenerator, "ClassGenerator"}, {&ClassProxy, "ClassProxy"}} {
		*c.p = Class(k(c.name))
	}
	for _, c := range []struct {
		p    *objFlags
		name string
	}{{&objExtensible, "objExtensible"}, {&objHasExt, "objHasExt"}, {&objHasData, "objHasData"},
		{&objHasSparseElements, "objHasSparseElements"}, {&objSharedElems, "objSharedElems"},
		{&objPolyProto, "objPolyProto"}, {&objMappedArguments, "objMappedArguments"},
		{&objArrayLengthWritable, "objArrayLengthWritable"}, {&objHTMLDDA, "objHTMLDDA"}} {
		*c.p = objFlags(k(c.name))
	}
	for _, c := range []struct {
		p    *elemType
		name string
	}{{&elemInt8, "elemInt8"}, {&elemUint8, "elemUint8"}, {&elemUint8Clamped, "elemUint8Clamped"},
		{&elemInt16, "elemInt16"}, {&elemUint16, "elemUint16"}, {&elemInt32, "elemInt32"}, {&elemUint32, "elemUint32"},
		{&elemFloat32, "elemFloat32"}, {&elemFloat64, "elemFloat64"}, {&elemBigInt64, "elemBigInt64"},
		{&elemBigUint64, "elemBigUint64"}} {
		*c.p = elemType(k(c.name))
	}
	for _, c := range []struct {
		p    *int32
		name string
	}{{&elemStrCharCodeAt, "elemStrCharCodeAt"}, {&elemPush, "elemPush"},
		{&elemStrAt, "elemStrAt"}, {&elemStrCharAt, "elemStrCharAt"}, {&elemStrCodePointAt, "elemStrCodePointAt"}, {&regCallNative, "regCallNative"},
		{&regCallEnter, "regCallEnter"}, {&regCallThis, "regCallThis"}, {&regCallObjThis, "regCallObjThis"},
		{&regCallOut, "regCallOut"}, {&regCallLeaf, "regCallLeaf"}, {&regLeafArg, "regLeafArg"}, {&regLeafCode, "regLeafCode"},
		{&regFrameCall, "regFrameCall"}, {&regFrameEnter, "regFrameEnter"}, {&regFrameGenStep, "regFrameGenStep"}, {&regFrameConstruct, "regFrameConstruct"}, {&regFrameSuper, "regFrameSuper"}, {&regCallConstruct, "regCallConstruct"}, {&regCallThrough, "regCallThrough"},
		{&genSuspendedYield, "genSuspendedYield"}, {&genExecuting, "genExecuting"},
		{&genCompleted, "genCompleted"},
		{&tailCallDepth, "tailCallDepth"}, {&linearScanLimit, "linearScanLimit"},
		{&maxCacheFills, "maxCacheFills"}, {&fwdApply, "fwdApply"}, {&fwdCall, "fwdCall"}, {&fwdWays, "fwdWays"}, {&fwdShift, "fwdShift"},
		{&atomIndexTag, "atomIndexTag"}, {&atomPrototype, "atomPrototype"}, {&propOddKey, "propOddKey"},
		{&atomLength, "atomLength"}} {
		*c.p = int32(k(c.name))
	}
	hasCacheBits, hasCacheMul = uint8(k("hasCacheBits")), k("hasCacheMul")
	hasCacheWaysLog = uint8(k("hasCacheWaysLog"))
	hasCacheWays = 1 << hasCacheWaysLog
	keySlotMul, intStringsLen = k("keySlotMul"), k("intStringsLen")
	keySetShift, keyWaysLog = uint8(k("keySetShift")), uint8(k("keyWaysLog"))
	keyAddrShift = uint8(k("keyAddrShift"))
	rtBase, ccBase = k("rtBase"), k("ccBase")
	for _, c := range []struct {
		p    *int
		name string
	}{{&sE, "sE"}, {&sWin, "sWin"}, {&sKind, "sKind"}, {&sRC, "sRC"}, {&sArgc, "sArgc"}, {&sBase, "sBase"},
		{&sTop, "sTop"}, {&sNF, "sNF"}, {&sSaved, "sSaved"}, {&sI, "sI"}, {&sEnd, "sEnd"}, {&sV0, "sV0"},
		{&sV1, "sV1"}, {&sEnt, "sEnt"}, {&sKa, "sKa"}} {
		*c.p = int(k(c.name))
	}
}
