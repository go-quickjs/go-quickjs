// Package jit compiles WebAssembly function bodies to native code: the
// baseline tier, one pass over a validated body (see README.md). It knows
// the interpreter's frame layout and the context block native code is
// entered with, and nothing of the engine's objects; the engine
// (wasm/internal/engine, its jit*.go files) places the code, enters it and
// handles its exits.
package jit

// Ctx is the context block native code is entered with, in DI. Native code
// reads and writes its words; the engine fills it before each entry and
// reads the exit record after. Every field is a word, and none holds a Go
// pointer: what native code addresses through it -- frames, the return
// stack, the link table, globals, memory -- is kept alive by the engine.
type Ctx struct {
	// L is the running frame's numeric slots: set by the engine before an
	// entry from Go, and stored by native code at every exit.
	L uintptr
	// Links is the link table: for each function index of the module, the
	// address of its native entry, or 0.
	Links uintptr
	// MemBase and MemSize are memory 0's first byte and size in bytes, set
	// before every entry; MemSizePtr is the address of a shared memory's
	// size, which other threads change, read at each access.
	MemBase    uintptr
	MemSize    uint64
	MemSizePtr uintptr
	// Mems is the instance's table of its memories, MemEntryWords words
	// each -- the first byte, the size in bytes, and the address of the
	// size a shared memory's grows store -- set before every entry where
	// the instance has more than one memory: memories other than 0 are
	// reached through it.
	Mems uintptr
	// Globals is a table of each global's address.
	Globals uintptr
	// ChunkEnd is the end of the stack chunk the frames are in: a callee's
	// frame must end at or before it.
	ChunkEnd uintptr
	// RSP and RSEnd are the native return stack's next free entry, stored
	// at every exit, and its end, or where the call depth would pass its
	// limit if that is before. An entry is three words: the address to
	// return to, the caller's L, and the callee's function index above the
	// call's offset in the caller (Site).
	RSP, RSEnd uintptr
	// Poll counts down to the next poll; native code exits when it runs
	// out.
	Poll int64

	// The exit record: what the code exits for, its arguments, the
	// function whose code exits and the address to resume at.
	Exit       uint64
	Arg0, Arg1 uint64
	Arg2, Arg3 uint64
	Arg4       uint64
	ExitFn     uint64
	ExitAddr   uintptr
}

// RSEntryWords is the number of words of a return stack entry.
const RSEntryWords = 3

// MemEntryWords is the number of words of an entry of the memories' table
// (Ctx.Mems).
const MemEntryWords = 3

// Site is a return stack entry's third word, for a callee and the offset of
// the call in its caller.
func Site(callee, offset uint32) uint64 { return uint64(callee)<<32 | uint64(offset) }

// The exits. Each says what Ctx's Arg words hold.
const (
	// ExitReturn: the frame the engine entered returned.
	ExitReturn = 1 + iota
	// ExitTrap: Arg0 is the trap code, Arg1 the offset of the trapping
	// instruction.
	ExitTrap
	// ExitCall: call function Arg0, its arguments in the frame's slots from
	// Arg1, two slots apart, where its results go; Arg2 is the call's
	// offset.
	ExitCall
	// ExitCallIndirect: call_indirect of type Arg0 through table Arg1, the
	// arguments from slot Arg2 and the index after them; Arg3 is the call's
	// offset.
	ExitCallIndirect
	// ExitTailCall: return_call of function Arg0, the arguments from slot
	// Arg1; Arg2 is the call's offset. To resume is to return.
	ExitTailCall
	// ExitTailIndirect: return_call_indirect of type Arg0 through table
	// Arg1, the arguments from slot Arg2 and the index after them; Arg3 is
	// the call's offset. To resume is to return.
	ExitTailIndirect
	// ExitPoll: the poll counter ran out.
	ExitPoll
	// ExitOp: run instruction Arg0 in Go, with immediates Arg1 and Arg2,
	// its operands in the frame's slots from Arg3, two slots apart, where
	// its result goes; Arg4 is its offset.
	ExitOp
)

// The trap codes an ExitTrap carries, the engine's TrapCode values.
const (
	TrapUnreachable          = 1
	TrapMemOutOfBounds       = 2
	TrapDivByZero            = 4
	TrapDivUnrepresentable   = 5
	TrapRemByZero            = 6
	TrapFloatUnrepresentable = 7
)
