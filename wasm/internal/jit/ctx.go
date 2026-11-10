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
	// Globals is a table of each global's address: its number's, or a
	// reference global's Ref.
	Globals uintptr
	// Funcs and Tables are tables of the instance's functions' and tables'
	// addresses, each an engine object's: what ref.func makes, and what
	// table.get and table.set read the elements of.
	Funcs, Tables uintptr
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
	// R is the running frame's reference slots, two words each, and
	// RChunkEnd the end of the chunk they are in. A function that holds
	// references keeps R in a register and moves it past its own frame
	// for a native call; one that holds none leaves it as its caller set
	// it.
	R, RChunkEnd uintptr
	// Inst is the address of the instance the code runs for, which a
	// function a table or a reference holds is compared with: the instance
	// that defined it (Layout.FuncInst). Frames and RFrames are the Code's
	// tables of each function's frame, which a native call's callee's must
	// have room for: for a call through a table or a reference, whose
	// callee is known only when it is made. Frames holds the bytes of its
	// numeric slots, or, for a function that holds references, a number
	// past any chunk's end; RFrames the bytes of its numeric slots in the
	// low word and those of the reference slots its native code uses in
	// the high one.
	Inst, Frames, RFrames uintptr

	// The store of a pointer word while the collector marks: native code
	// leaves the word's address in WBSlot and the pointer in WBPtr and
	// goes to its function's barrier routine, which keeps where to go on
	// in WBRet and the registers the code keeps in Save around the
	// runtime's store (internal/jit.StoreRef).
	WBSlot, WBPtr, WBRet uintptr
	Save                 [SaveWords]uint64

	// The exit record: what the code exits for, its arguments, the
	// function whose code exits and the address to resume at.
	Exit       uint64
	Arg0, Arg1 uint64
	Arg2, Arg3 uint64
	Arg4       uint64
	ExitFn     uint64
	ExitAddr   uintptr
	// Resume is where the code goes on after an exit its sites share --
	// a poll's, a call's through the engine -- whose resumption is shared
	// too: the site's own code, which the site leaves here.
	Resume uintptr
	// RArgs, where it is not 0, says where the references among an exit's
	// operands and results are: the reference slot of the operand whose
	// numeric slot is s is RArgs - RArgsBias + s/2, not the slot of s's
	// stack position. The optimizing tier, whose operands of an exit are in
	// a call area of their own while its references stay in their stack
	// positions' reference slots, sets it for an exit with references; the
	// engine reads it at the exit and clears it.
	RArgs uint64
	// Pools is the table of the pools of new objects the code takes from,
	// by the module's type index: each entry the address of a struct
	// type's pool (Layout.PoolCount, Layout.PoolObjs), of an array type's
	// table of pools by class (Layout.ArrayClass), or 0, and the table's
	// address 0 until the first ExitAlloc of the level, at which the
	// engine sets it.
	Pools uintptr
	// SaveV is where a function with v128 values keeps its float
	// registers whole around a poll's exit and the runtime's store, each
	// register's 16 bytes at 16 times its number, its general registers
	// staying in Save. It is at a multiple of 16 bytes from the context's
	// start, which arm64's 16-byte loads and stores scale their offsets by.
	SaveV [2 * 32]uint64
}

// RArgsBias is what Ctx.RArgs adds to its offset, which may be negative,
// so that a word that is not 0 says the offset is set.
const RArgsBias = 1 << 32

// SaveWords is the size of the context's register save area.
const SaveWords = 56

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
	// its operands in the frame's slots from Arg3, two slots apart -- a
	// reference in its stack position's reference slot -- where its result
	// goes; Arg4 is its offset.
	ExitOp
	// ExitCallRef: call_ref of type Arg0, the arguments from slot Arg1 and
	// the reference after them; Arg2 is the call's offset.
	ExitCallRef
	// ExitTailRef: return_call_ref of type Arg0, the arguments from slot
	// Arg1 and the reference after them; Arg2 is the call's offset. To
	// resume is to return.
	ExitTailRef
	// ExitSIMD: run the SIMD instruction at offset Arg0 of the module as
	// the interpreter does, its operands in the frame's slots from Arg1, two
	// slots apart, its result left in the first's.
	ExitSIMD
	// ExitTierUp: the function's baseline code, at its entry, counted
	// enough calls for the optimizing tier: the engine compiles it and
	// enters the optimized code with the frame as the call made it, or
	// resumes the baseline's.
	ExitTierUp
	// ExitAlloc: the pool of new objects of the type whose index is Arg0's
	// low word is empty, or the context has no table of pools (Ctx.Pools):
	// the engine fills it, charging the instance's budget, and the code
	// takes from it again; Arg1 is the instruction's offset. Arg0's high
	// word is 0 for a struct; for an array, the class of its block plus 1,
	// or AllocLength for the class of an array of Arg2 elements; or
	// AllocMake and a reference slot's index from R: an array of Arg2
	// elements, past what pools hold, which the engine makes, as the
	// interpreter does, and leaves in that slot, the code going on past
	// the instruction. Nothing of the frame is read or written but its
	// reference slots, which a count of the budget's reaches.
	ExitAlloc
)

// The high word of an ExitAlloc's Arg0 for an array whose length is known
// only as the code runs, Arg2: the pool of its class's (AllocLength), or
// one made by the engine into a reference slot (AllocMake, the slot's
// index from R in the bits below it).
const (
	AllocLength = 1 << 30
	AllocMake   = 1 << 31
)

// The trap codes an ExitTrap carries, the engine's TrapCode values.
const (
	TrapUnreachable          = 1
	TrapMemOutOfBounds       = 2
	TrapUnalignedAccess      = 3
	TrapDivByZero            = 4
	TrapDivUnrepresentable   = 5
	TrapRemByZero            = 6
	TrapFloatUnrepresentable = 7
	TrapTableOutOfBounds     = 8
	TrapNullDereference      = 13
	TrapIllegalCast          = 14
	TrapArrayOutOfBounds     = 15
)
