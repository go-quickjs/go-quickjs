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
)

// The native handlers' context (the engine's nativeCtx): the byte offset
// of each field.
const (
	CtxL         = 0  // the frame's numeric slots
	CtxCode      = 8  // the code's first word
	CtxTable     = 16 // the native table
	CtxPC        = 24 // the pc to enter at
	CtxF         = 32 // the frame
	CtxGoTable   = 40 // the Go handlers' table, regTable
	CtxSlowTable = 48 // the slow functions' table, regSlowTable
	CtxCallH     = 56 // the trampoline that calls Go (nativeCallH)
	CtxAfter     = 64 // where the exit stub goes on after its call of Go
	CtxMem0      = 72 // memory 0 where it is an unshared memory, or nil
	CtxGlobals   = 80 // the instance's globals' first
	CtxTables    = 88 // the code's branch tables' first
	CtxExits     = 96 // a test's counts of exits by operation, or nil
	CtxSize      = 104
)

// The engine's structures the handlers read, on amd64 and arm64.
const (
	FrameTick = 80 // frame.tick, an int32
	MemoryBuf = 0  // Memory.buf, a slice: its pointer, then its length
	GlobalNum = 0  // Global.Num
	SliceLen  = 8  // a slice's length
	SliceSize = 24 // a slice
)
