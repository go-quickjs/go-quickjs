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
	ClassObject, ClassArray, ClassTypedArray, ClassCursor Class

	objExtensible, objHasExt, objHasData, objHasSparseElements, objSharedElems,
	objPolyProto, objMappedArguments, objArrayLengthWritable, objHTMLDDA objFlags

	elemInt8, elemUint8, elemUint8Clamped, elemInt16, elemUint16, elemInt32,
	elemUint32, elemFloat32, elemFloat64, elemBigInt64, elemBigUint64 elemType
)

// Calls, leaves and caches; and the bases of the Runtime's and a
// closureCode's fields the code reads relative to the context's rt and cc.
var (
	elemStrCharCodeAt int32

	regCallEnter, regCallThis, regCallObjThis, regCallOut, regCallLeaf, regLeafArg int32
	regFrameCall, regFrameEnter                                                    int32

	tailCallDepth, linearScanLimit int32
	hasCacheBits                   uint8
	hasCacheMul                    uint64

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
	}{{&ClassObject, "ClassObject"}, {&ClassArray, "ClassArray"}, {&ClassTypedArray, "ClassTypedArray"}, {&ClassCursor, "ClassCursor"}} {
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
	}{{&elemStrCharCodeAt, "elemStrCharCodeAt"},
		{&regCallEnter, "regCallEnter"}, {&regCallThis, "regCallThis"}, {&regCallObjThis, "regCallObjThis"},
		{&regCallOut, "regCallOut"}, {&regCallLeaf, "regCallLeaf"}, {&regLeafArg, "regLeafArg"},
		{&regFrameCall, "regFrameCall"}, {&regFrameEnter, "regFrameEnter"},
		{&tailCallDepth, "tailCallDepth"}, {&linearScanLimit, "linearScanLimit"}} {
		*c.p = int32(k(c.name))
	}
	hasCacheBits, hasCacheMul = uint8(k("hasCacheBits")), k("hasCacheMul")
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
