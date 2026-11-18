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

// call_indirect, of the second table.
const XCallIndirect = 104

// The SIMD memory operations of the second table, rExt's.
const (
	XV128Load = 80 + iota
	XV128Store
	XV128LoadLane
	XV128StoreLane
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
	CtxSize      = 280
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

	CallSiteOffset = 8  // callSite.offset
	CallSiteSize   = 16 // a callSite

	FnType  = 16 // Function.Type, a uint32
	FnInst  = 32 // Function.Inst
	FnIndex = 40 // Function.Index

	CdNslots = 0  // compiled.nslots
	CdRslots = 8  // compiled.rslots
	CdRefs   = 32 // compiled.refs, a bool
	CdReg    = 80 // compiled.reg

	RcCode   = 0   // regCode.code
	RcSites  = 48  // regCode.sites
	RcTables = 72  // regCode.tables
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
	GlobalNum  = 0  // Global.Num
)
