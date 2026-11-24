// Package regdef holds what the engine's register code and its native
// handlers' generator share (wasm/internal/engine/README.md): the values of
// the first table's operations, and the layout of the native handlers'
// context and of the engine's structures they read. The engine aliases the
// operations and checks the layout against its own (TestNativeLayout).
package regdef

// The hand-written operations of the first table; the numeric ones follow
// from NumFirst (num_gen.go, written by internal/numgen).
const (
	Invalid     = iota
	Ext         // regExtTable[X]
	Mov         // A = B
	Const32     // A = BC, zero-extended
	Const32S    // A = BC, sign-extended
	Const64     // A = the next word
	Jmp         // pc += BC
	BrNZ        // if A != 0: pc += BC
	BrZ         // if A == 0: pc += BC
	BrTable     // pc = tables[BC][min(A, last)]
	Ret         // the frame returns, its results in place
	Ret1        // slot 0 = A; the frame returns
	Call        // the call of sites[BC]
	ReturnCall  // the tail call of sites[BC]
	Select      // if C == 0: A = B
	GlobalGet32 // A = global BC's low half
	GlobalGet64 // A = global BC
	GlobalSet   // global BC = A
	Mov2        // A, A+1 = B, B+1: a v128
	RefMov      // reference A = reference B
	RefNull     // reference A = null
	Const128    // A, A+1 = the next two words
	SIMD        // regSIMDTable[X]
	Far         // the instruction after the next two words, its slots past 16 bits

	// NumFirst is the first numeric operation's value.
	NumFirst = 24

	// ExtNumFirst is the first numeric operation's value in the second
	// table, rExt's.
	ExtNumFirst = 32
)

// The hand-written operations of rSIMD's table, after the generated ones
// (simd_gen.go).
const (
	SShuffle = SIMDGenCount + iota
	SExtractI8S
	SExtractI8U
	SExtractI16S
	SExtractI16U
	SExtract32
	SExtract64
	SReplace8
	SReplace16
	SReplace32
	SReplace64
	SSplat8
	SSplat16
	SSplat32
	SSplat64
)

// call_indirect and call_ref, of the second table.
const (
	XCallIndirect = 104
	XCallRef      = 105
)

// The reference operations of the second table the native handlers run.
const (
	XSelectRef    = 7
	XSelect128    = 8
	XRefIsNull    = 9
	XRefEq        = 11
	XRefAsNonNull = 12
	XBrNull       = 13
	XBrNonNull    = 14
	XBrOnCast     = 130
	XBrOnCastFail = 131
)

// The GC operations of the second table whose operands are the
// instruction's fields, not a record's: a struct's field of a kind (A the
// value's slot, B the struct's reference slot, C the field's cell; a set's
// A the struct's, B the value's), an array's element of a kind (a get's A
// the value's slot, B the array's, C the index's; a set's A the array's, B
// the index's, C the value's), an array's length (A, of B), ref.i31 (A of
// B), i31.get (A of B), and ref.test and ref.cast (A the result, B the
// reference -- a cast's A the reference -- C the GC record of the type).
const (
	XSGet8S = 192 + iota
	XSGet8U
	XSGet16S
	XSGet16U
	XSGet32
	XSGet64
	XSGet128
	XSGetRef
	XSSet8
	XSSet16
	XSSet32
	XSSet64
	XSSet128
	XSSetRef
	XAGet8S
	XAGet8U
	XAGet16S
	XAGet16U
	XAGet32
	XAGet64
	XAGet128
	XAGetRef
	XASet8
	XASet16
	XASet32
	XASet64
	XASet128
	XASetRef
	XALen
	XRefI31
	XI31GetS
	XI31GetU
	XRefTest
	XRefCast
)

// How native code stores a value into a new object (the engine's
// natVal.kind): a number into a struct's cell's payload, all 64 bits (Nat64),
// the low 32, 16 or 8 zero-extended (NatW32, NatW16, NatW8), or a v128 into
// two cells' payloads (NatV128); a number into an array's element, of 8, 4,
// 2 or 1 bytes (Nat64, NatE32, NatE16, NatE8); or a reference, the whole
// slot (NatRef).
const (
	Nat64 = iota + 1
	NatW32
	NatW16
	NatW8
	NatV128
	NatE32
	NatE16
	NatE8
	NatRef
)

// How native code tests a reference (the engine's regGC.mode).
const (
	CastGo = iota
	CastIndex
	CastI31
	CastStruct
	CastArray
	CastEq
	CastTop
)

// The kinds of objects a reference may point to (the engine's ObjKind),
// at an object's header's HeaderKind.
const (
	ObjStruct  = 2
	ObjArray   = 3
	HeaderKind = 8
)

// The SIMD memory operations of the second table, rExt's.
const (
	XV128Load = 80 + iota
	XV128Store
	XV128LoadLane
	XV128StoreLane
)

// The allocations of the second table (the engine's xStructNew ...).
const (
	XStructNew        = 112
	XStructNewDefault = 113
	XArrayNew         = 116
	XArrayNewDefault  = 117
	XArrayNewFixed    = 118
)

// The native handlers' context (the engine's nativeCtx): the byte offset
// of each field.
const (
	CtxL         = 0   // the frame's numeric slots
	CtxCode      = 8   // the code's first word
	CtxTable     = 16  // the native table
	CtxPC        = 24  // the pc to enter at
	CtxF         = 32  // the frame
	CtxGoTable   = 40  // the Go handlers' table, regTable
	CtxSlowTable = 48  // the slow functions' table, regSlowTable
	CtxCallH     = 56  // the trampoline that calls Go (nativeCallH)
	CtxAfter     = 64  // where the exit stub goes on after its call of Go
	CtxMem0      = 72  // memory 0 where it is an unshared memory, or 0
	CtxGlobals   = 80  // the instance's globals' first
	CtxTables    = 88  // the code's branch tables' first
	CtxExits     = 96  // a test's counts of exits by operation, or 0
	CtxR         = 104 // the frame's reference slots
	CtxX         = 112 // the exec
	CtxInst      = 120 // the instance
	CtxFuncs     = 128 // the instance's functions' first
	CtxFns       = 136 // the Code's records' first
	CtxSites     = 144 // the code's sites' first
	CtxCalls     = 152 // not 0 where native code calls functions itself
	CtxMaxDepth  = 160 // MaxCallDepth
	CtxStoreRef  = 168 // the barrier routine
	CtxWB        = 176 // the collector's marking flag's address
	CtxCaller    = 184 // the calling frame's context, or 0
	CtxRetAddr   = 192 // the caller's call instruction's address
	CtxSite      = 200 // the call's site
	CtxNc        = 208 // the exec's chunk to give back to
	CtxNsp       = 216 // the exec's chunk's slot to give back to
	CtxFn        = 224 // the call's scratch: the function
	CtxCd        = 232 // the call's scratch: the record
	CtxRc        = 240 // the call's scratch: the register code
	CtxExtTable  = 248 // the native second table, rExt's
	CtxRch       = 256 // the exec's reference chunk to give back to
	CtxRsp       = 264 // the exec's reference chunk's slot to give back to
	CtxSIMDTable = 272 // rSIMD's native table, or one of the exit stub alone
	CtxGC        = 280 // the code's GC records' first
	CtxI31       = 288 // what an i31's pointer word points to
	CtxSize      = 296
)

// The engine's structures the handlers read, on amd64 and arm64.
const (
	SliceLen  = 8  // a slice's length
	SliceCap  = 16 // a slice's capacity
	SliceSize = 24 // a slice

	FrameL       = 8   // frame.l
	FrameR       = 32  // frame.r
	FrameInst    = 56  // frame.inst
	FrameOsr     = 64  // frame.osr, an int32
	FrameRc      = 72  // frame.rc
	FrameTick    = 80  // frame.tick, an int32
	FrameTickSet = 84  // frame.tickSet, an int32
	FrameSig     = 88  // frame.sig, an int32
	FrameNctx    = 232 // frame.nctx

	XNchunks = 16  // exec.nchunks
	XRchunks = 40  // exec.rchunks
	XNc      = 64  // exec.nc
	XRc      = 72  // exec.rc
	XNsp     = 80  // exec.nsp
	XRsp     = 88  // exec.rsp
	XDepth   = 96  // exec.depth
	XCalls   = 104 // exec.calls
	XPoll    = 128 // exec.poll, an int32
	XFrames  = 192 // exec.frames
	// exec.natPools, natClasses and natAlloc: the table of pools of new
	// objects for the instance whose code runs, or 0; the classes of
	// arrays' blocks by their elements' bytes; the allocation stub.
	XNatPools   = 264
	XNatClasses = 272
	XNatAlloc   = 280

	CallSiteOffset = 8  // callSite.offset
	CallSiteSize   = 16 // a callSite

	FnType  = 16 // Function.Type, a uint32
	FnInst  = 32 // Function.Inst
	FnIndex = 40 // Function.Index

	CdNslots = 0  // compiled.nslots
	CdRslots = 8  // compiled.rslots
	CdRefs   = 32 // compiled.refs, a bool
	CdReg    = 56 // compiled.reg

	RcCode   = 0   // regCode.code
	RcSites  = 48  // regCode.sites
	RcTables = 72  // regCode.tables
	RcGC     = 128 // regCode.gc
	RcCallee = 152 // regCode.callee

	SiteFn      = 0  // regSite.fn
	SiteAt      = 4  // regSite.at
	SiteArgs    = 8  // regSite.args
	SiteRets    = 32 // regSite.rets
	SiteTable   = 60 // regSite.table
	SiteChecked = 64 // regSite.checked
	SiteTarget  = 68 // regSite.target
	SiteWant    = 72 // regSite.want
	SiteSize    = 80 // a regSite

	InstTables = 40 // Instance.Tables
	TableElems = 0  // Table.Elems

	MoveFrom = 0  // regMove.from
	MoveTo   = 4  // regMove.to
	MoveK    = 8  // regMove.k
	MoveSize = 12 // a regMove
	MoveV128 = 1  // regMove.k of a v128
	MoveRef  = 2  // regMove.k of a reference
	RefSize  = 16 // a Ref, its pointer word first

	MemoryBuf  = 0  // Memory.buf
	MemoryIs64 = 40 // Memory.Is64, a bool

	GCSize     = 192 // a regGC
	GCNullable = 132 // regGC.nullable
	GCMode     = 144 // regGC.mode, a byte
	GCDepth    = 148 // regGC.depth, a uint32
	GCCanon    = 152 // regGC.canon, a uint32
	GCDst      = 8   // regGC.dst, a uint32
	GCOps      = 16  // regGC.ops, a slice of uint32
	GCNatTi    = 156 // regGC.natTi, a uint32
	GCNatShift = 160 // regGC.natShift, a uint32
	GCNatVals  = 168 // regGC.natVals, a slice of natVal

	NatFrom = 0  // natVal.from
	NatTo   = 4  // natVal.to
	NatKind = 8  // natVal.kind
	NatSize = 12 // a natVal

	PoolCount = 0 // gcPool.n
	PoolObjs  = 8 // gcPool.objs

	ArrayBlockBytes = 480 // the most bytes of elements a pool's array has room for
	RttSupers       = 16  // rtt.supers, a slice of uint32
	ArrayElems      = 16  // Array.e
	ArrayLen        = 24  // Array.n
	GlobalNum       = 0   // Global.Num
)
