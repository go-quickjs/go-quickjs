// Package lsra is a linear-scan register allocator over an abstract
// machine function: blocks of instructions that use and define virtual
// registers of classes, with fixed clobbers, calls and phis. It knows
// nothing of the source language or the target's instructions: a client
// describes its function, and lowers it with the locations the allocator
// gives.
//
// A virtual register's lifetime is a list of ranges, its holes left out,
// from liveness. A phi and its arguments are coalesced -- given one
// location, their edge's move gone -- where their lifetimes do not meet,
// the edges of the deepest loops first; the coalesced registers are a web,
// and each web has one location for its whole life: a register, or a spill
// slot. Webs are allocated by a linear scan in order of their starts, as
// Wimmer and Mössenböck's allocator scans with active and inactive
// intervals, so that a web may take a register in another's hole; without
// splitting, where a class's registers run out the cheaper of the web and
// those it would displace -- weighed by their uses, eight times each loop
// around them -- is spilled whole. A web in a register that is live across
// a call (Inst.Call) is kept in a slot too, stored where each of its
// registers is defined and loaded again after each call it crosses
// (Result.Across); a call clobbers every register, and a web whose reloads
// would weigh as much as its uses lives in its slot. An instruction's
// fixed clobbers (Inst.Clobber) keep the registers they name from the webs
// live across it. Slots are given by a second scan, with holes too, a web
// with a parameter keeping the slot it arrives in.
package lsra

import "errors"

// VReg is a virtual register: an index into Func's per-register slices.
type VReg int32

// None is no virtual register.
const None VReg = -1

// NoClass is the class of a virtual register that is not allocated: its
// client makes its value where it is used (a constant).
const NoClass int8 = -1

// Func is a function to allocate.
type Func struct {
	// Class is each virtual register's class, an index into
	// Config.Classes, or NoClass.
	Class []int8
	// FixedSlot is the slot a virtual register arrives in -- a parameter,
	// in its frame slot -- which it keeps if it needs one, or -1. The slot
	// holds nothing else until the register is defined.
	FixedSlot []int32
	// Blocks are the function's blocks in layout order, the entry first.
	Blocks []Block
}

// Block is a basic block.
type Block struct {
	// Preds and Succs are block indices; a block with phis has an edge
	// to it from each predecessor, in order, that no other block shares
	// (no critical edges).
	Preds, Succs []int
	// Depth is the block's loop depth, which weighs its uses.
	Depth int
	// Phis are defined at the block's start, each from Args[i] on the
	// edge from Preds[i].
	Phis []Phi
	// Insts are the block's instructions: the last uses what the block's
	// branch or return reads.
	Insts []Inst
}

// Phi is a phi: its definition and an argument for each predecessor
// (None for one that is not allocated).
type Phi struct {
	Def  VReg
	Args []VReg
}

// Inst is an instruction: it reads Uses, then writes Defs.
type Inst struct {
	Uses, Defs []VReg
	// Clobber is, for each class, the registers it writes besides its
	// Defs, as bits by register number: no interval live across it may
	// be in them.
	Clobber []uint64
	// Call says it clobbers every register: the intervals live across it
	// in registers are stored at their definitions and loaded after it.
	Call bool
	// Hint is a use whose register Defs[0] would like, the two operands
	// of a two-address instruction: None for none.
	Hint VReg
}

// Config is the target's registers and slots.
type Config struct {
	// Classes are the register classes: each its allocatable registers,
	// by register number below 64, in the order they are preferred.
	Classes [][]int
	// NumSlots is the number of spill slots there are.
	NumSlots int
}

// Result is where each virtual register lives.
type Result struct {
	// Reg is each virtual register's register, or -1 for one spilled or
	// not allocated.
	Reg []int16
	// Slot is each virtual register's slot: a spilled one's home, or one
	// in a register live across a call, its copy there; else -1.
	Slot []int32
	// Across is, for each call by block and instruction index, the
	// virtual registers in registers live across it, which it must load
	// again from their slots.
	Across map[[2]int32][]VReg
	// NumSlots is the number of slots used: those below it.
	NumSlots int
}

// ErrSlots is the error of a function whose spills need more slots than
// Config.NumSlots.
var ErrSlots = errors.New("lsra: not enough spill slots")
