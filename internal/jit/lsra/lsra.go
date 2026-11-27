// Package lsra is a linear-scan register allocator over an abstract
// machine function: blocks of instructions that use and define virtual
// registers of classes, with fixed clobbers, calls and phis. It knows
// nothing of the source language or the target's instructions: a client
// describes its function, and lowers it with the locations the allocator
// gives.
//
// A virtual register's lifetime is a list of ranges, its holes left out,
// from liveness: the paths from each use back to the definition where
// every register is defined once (SSA), else sweeps of the blocks. A phi
// and its arguments are coalesced -- given one location, their edge's move
// gone -- where their lifetimes do not meet, the edges of the deepest
// loops first; the coalesced registers are a web, which has one register
// or a spill slot. Webs are allocated by a linear scan in order of their
// starts, as Wimmer and Mössenböck's allocator scans with active and
// inactive intervals, so that a web may take a register in another's
// hole. Where a class's registers run out, the web scanned takes the
// register whose webs in its way are least dense -- their uses' weight,
// eight times each loop around them, over the positions they cover; a
// use or a definition that costs the same wherever the web is -- a phi's
// argument of its phi's web, a phi all of whose arguments are, and a
// definition of a web stored where it is defined -- weighs nothing --
// and each of those is split: it gives the register up over a hole in its
// life, where it lives in its slot, where each one's holes would cost less
// than its uses and all the new holes less than spilling the web scanned;
// else, as without splitting, they are spilled whole where they are less
// dense than it, and it is spilled otherwise. With Config.Displace, the
// web scanned takes the register whatever their density, each split where
// its holes would cost less than its uses and spilled whole otherwise. A
// hole covers where the two meet,
// widened toward the split web's uses before and after it to where a
// block of a lower loop depth begins, so that the moves into and out of
// it run least; it costs the web's uses in it and those moves -- a store
// where the web goes in, none for one stored where it is defined, a load
// where it comes out -- each by its loop depth. A split web is in its
// register but over its holes; the client makes the moves where a hole
// begins and ends within a block (Result.Moves) and on the edges that
// cross its bounds (Result.Edge), and learns where each is as a block
// begins (Result.Entry). A function with a critical edge, which would
// have no place for an edge's moves, or one allocated with Config.NoSplit,
// is allocated without splitting: where the registers run out, the web
// scanned displaces those in its way only if they are less dense than it,
// spilling them whole, and is spilled itself otherwise. A web in a
// register that is live across a call (Inst.Call) is kept in a slot too,
// stored where each of its registers is defined and loaded again after
// each call it crosses in its register (Result.Across); a call clobbers
// every register. A web whose reloads would weigh as much as its uses
// gives its register up over the calls instead, where that costs less: a
// hole over each run of calls between two of its uses, which costs the
// load where it ends; and where its holes and its reloads after the other
// calls would still weigh as much, it lives in its slot. An instruction's
// fixed clobbers (Inst.Clobber) keep
// the registers they name from the webs live across it, and its
// temporaries (Inst.Temps) are registers apart from its operands and
// results. Slots are given by a second scan, with holes too, a web with a
// parameter keeping the slot it arrives in, a register wider than a slot
// (Func.Width) taking consecutive ones; a split web not stored where it is
// defined needs its slot over its holes alone.
//
// An Allocator keeps its tables, lists and result for its next
// allocation, so that a client allocating function after function
// allocates little once they have grown to its functions' sizes.
package lsra

import (
	"errors"
	"fmt"
	"slices"
)

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
	// Width is the number of consecutive slots each virtual register takes
	// where it is kept in memory, a fixed slot's included: one where it is
	// 0, or where Width is nil. Registers of different widths may share a
	// class -- its registers hold either -- but never a web.
	Width []int8
	// Blocks are the function's blocks in layout order, the entry first.
	Blocks []Block
	// Clobbers are the registers instructions write besides their Defs,
	// which an instruction names by index (Inst.Clobber): for each class,
	// as bits by register number.
	Clobbers [][]uint64
}

// ClobberOf is what in clobbers besides its definitions, by class.
func (f *Func) ClobberOf(in *Inst) []uint64 {
	if in.Clobber == 0 {
		return nil
	}
	return f.Clobbers[in.Clobber-1]
}

// Block is a basic block.
type Block struct {
	// Preds and Succs are block indices; a block with phis has an edge
	// to it from each predecessor, in order, that no other block shares
	// (no critical edges).
	Preds, Succs []int
	// Depth is the block's loop depth, which weighs its uses: a client
	// may give a block it takes to run less often than its loop's other
	// blocks a lower one.
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

// Inst is an instruction: it reads its uses, then writes its definitions.
// Regs holds them, then its temporaries: NUses uses, NDefs definitions,
// the rest temporaries (NewInst).
type Inst struct {
	Regs         []VReg
	NUses, NDefs uint16
	// Hint is a use whose register its first definition would like, the
	// two operands of a two-address instruction: None for none.
	Hint VReg
	// Call says it clobbers every register: the intervals live across it
	// in registers are stored at their definitions and loaded after it.
	Call bool
	// Clobber is, plus one, the index in Func.Clobbers of the registers it
	// writes besides its definitions, or 0 for none: no interval live
	// across it may be in them.
	Clobber int8
}

// NewInst is an instruction of uses, defs and temporaries temps, its
// registers appended to regs, whose capacity past its length they should
// fit (the client's arena), and its hint.
//
// Temporaries are registers the instruction works in: each live from its
// reads to its writes, so that it is none of the instruction's uses' or
// definitions' registers, nor of a register live across it. They are
// given registers ahead of anything they meet, and are defined and used
// nowhere else. An instruction with temporaries is not a call.
func NewInst(regs, uses, defs, temps []VReg, hint VReg) Inst {
	var in Inst
	in.Set(regs, uses, defs, temps, hint)
	return in
}

// Set makes in the instruction NewInst makes, in place: a client that
// keeps its instructions in an array makes each where it is.
func (in *Inst) Set(regs, uses, defs, temps []VReg, hint VReg) {
	// One at a time: the lists are a few registers each, and an append of
	// a slice is a call of the runtime's copy.
	for _, r := range uses {
		regs = append(regs, r)
	}
	for _, r := range defs {
		regs = append(regs, r)
	}
	for _, r := range temps {
		regs = append(regs, r)
	}
	in.Regs, in.NUses, in.NDefs, in.Hint, in.Call, in.Clobber = regs, uint16(len(uses)), uint16(len(defs)), hint, false, 0
}

// Uses are the registers in reads.
func (in *Inst) Uses() []VReg { return in.Regs[:in.NUses] }

// Defs are the registers in writes.
func (in *Inst) Defs() []VReg { return in.Regs[in.NUses : in.NUses+in.NDefs] }

// Temps are in's temporaries.
func (in *Inst) Temps() []VReg { return in.Regs[in.NUses+in.NDefs:] }

// Config is the target's registers and slots.
type Config struct {
	// Classes are the register classes: each its allocatable registers,
	// by register number below 64, in the order they are preferred.
	Classes [][]int
	// NumSlots is the number of spill slots there are.
	NumSlots int
	// NoSplit allocates without splitting: each web in its register or
	// its slot throughout.
	NoSplit bool
	// Displace has a web scanned that finds no register free take the
	// one whose webs in its way are least dense whatever their density,
	// where no temporary is in the way, each of them split where its
	// holes would cost less than its uses and spilled whole otherwise;
	// without it, they are split only where every one would be and the
	// holes cost less than spilling the web scanned, and are otherwise
	// spilled whole only where they are less dense than it, it being
	// spilled itself otherwise. With splitting alone.
	Displace bool
	// StressSplit, for tests, splits wherever it can: a web scanned
	// takes a register from those in its way however dense they are, and
	// they give it up over holes whatever the holes cost.
	StressSplit bool
	// MaxLiveBytes, if not 0, bounds the bytes of the liveness sets: a set
	// of the registers live across a block's boundary for each block, which
	// grow with the product of the two. Past it Allocate returns a
	// *LiveSetsError before it allocates them.
	MaxLiveBytes int64
	// Lap, if not nil, is called at the end of each of the allocator's
	// phases, for a client that times its compiles.
	Lap func(Phase)
	// dataflow has the allocation find liveness by the dataflow, for the
	// tests that compare it with explore.
	dataflow bool
	// slots has the slots' scan test each slot in turn (slotsLinear) or
	// keep the slots' keys from the first (slotsTree), for the tests that
	// compare the two.
	slots slotsMode
	// oneByOne has the allocation visit one by one what a web lives
	// across and what it has -- the calls for its holes over them
	// (callHoles) and its reloads after them, the clobbers for the
	// registers it avoids, its holes where one is punched or planned --
	// for the tests that compare it with finding them from the web's uses,
	// its ranges and the clobbers' sums, and with a hole cut out of its
	// ranges.
	oneByOne bool
	// twoPass has the coalescing test whether two webs' ranges meet and
	// then merge them, for the tests that compare it with the walk that
	// merges them as it tests.
	twoPass bool
	// markPass has liveness mark the registers used in a block other than
	// their definitions' in a walk of the instructions of its own, and
	// explore start its paths from a walk of them, for the tests that
	// compare it with the one walk that defines, marks and lists the uses.
	markPass bool
	// hintPass has the webs' hints found by a walk of the instructions of
	// their own, and the weights' walk of the phis apart, for the tests
	// that compare it with the hints liveness's walk lists and one walk
	// of the blocks for both.
	hintPass bool
	// blockByBlock has a hole's cost visit every block the hole spans for
	// the edges that cross its bounds, for the tests that compare it with
	// visiting those where its web lives.
	blockByBlock bool
	// mergeAll has a web's events merged from its members' runs as they
	// are, for the tests that compare it with joining first the runs that
	// lie one after another.
	mergeAll bool
	// regByReg has an eviction weigh the webs in the way of each register
	// by a walk of the scan's lists for it, for the tests that compare it
	// with one walk for all.
	regByReg bool
	// phiWalks has liveness, the ranges' walk and the events walk each
	// block's successors' phis for the arguments they read at its end, for
	// the tests that compare it with listing them once (edgeArgs).
	phiWalks bool
	// searchPreds has the walk that finds each edge's index among its
	// successor's predecessors (predIdx) search them whatever their number,
	// for the tests that compare it with sorting them where they are many
	// (predIndex).
	searchPreds bool
	// rangeLists has the ranges' walk append each register's ranges to a
	// list of its own as it finds them, the lists reversed after, and the
	// registers' webs of their own made in a visit of their own, for the
	// tests that compare it with keeping the range found last apart and
	// laying the ranges out with the webs made (walkRanges, layoutRanges).
	rangeLists bool
	// sortList has the webs listed in order of their starts by a sort,
	// for the tests that compare it with listing them from the order in
	// which the ranges' walk met their definitions.
	sortList bool
	// alwaysTest has the coalescing test and merge two webs' ranges
	// whatever their extents, for the tests that compare it with putting
	// the ranges of one that begins where the other has ended after it.
	alwaysTest bool
	// webHeap has the scan keep the parts of webs taken apart in a heap of
	// the webs, which compares two by going to each, for the tests that
	// compare it with the heap of their starts and members (partHeap).
	webHeap bool
	// plainLists has the scan read every web on its active and inactive
	// lists at every position, for the tests that compare it with reading
	// each only where its entry says it may have changed.
	plainLists bool
	// mutateHoles has the allocation punch holes that leave a position
	// where the webs meet out, for the test that the checks find it.
	mutateHoles bool
}

// A Phase is a stretch of an allocation that Config.Lap is told the end of.
type Phase int

// The allocator's phases, in order.
const (
	// PhaseLiveness is the registers live into and out of each block.
	PhaseLiveness Phase = iota
	// PhaseRanges is each register's ranges and weight, the clobbers, and
	// each register's web of its own.
	PhaseRanges
	// PhaseWebs is the coalescing of phis and their arguments, and the
	// hints.
	PhaseWebs
	// PhaseScan is the linear scan of each class.
	PhaseScan
	// PhaseSlots is the slots' scan and the result.
	PhaseSlots
	// NumPhases bounds the phases.
	NumPhases
)

// Result is where each virtual register lives.
type Result struct {
	// Reg is each virtual register's register, or -1 for one spilled or
	// not allocated. A split register (Split) is there outside its holes.
	Reg []int16
	// Slot is each virtual register's slot: a spilled one's home, one in
	// a register live across a call its copy there, and a split one's
	// place in its holes; else -1.
	Slot []int32
	// NumSlots is the number of slots used: those below it.
	NumSlots int
	// The registers each clobber reloads (Across): acrossKey is each
	// clobber's block and instruction, in order, and acrossV from
	// acrossOff[k] to acrossOff[k+1] clobber k's registers.
	acrossKey [][2]int32
	acrossOff []int32
	acrossV   []VReg
	// flags are each register's flagStored and flagSplit.
	flags []uint8
	// The split registers' moves: before an instruction (Moves), by its
	// block and index, on an edge (Edge), by its block and successor, and
	// into a block (Entry), entryMv from entryOff[b] to entryOff[b+1].
	gap, edge moveTable
	entryOff  []int32
	entryMv   []Move
	// splits is the number of split registers.
	splits int
}

// The flags of a register's Result.
const (
	// flagStored: it is stored into its slot where it is defined in its
	// register.
	flagStored uint8 = 1 << iota
	// flagSplit: it has holes.
	flagSplit
)

// A Move is where a split register is from a point of the function on:
// Reg, its register, or -1, its slot. Copy says the value is copied there
// from the other -- loaded into the register from the slot, or stored into
// the slot from the register; else the slot holds it already, or the
// register is defined there, and only where it is changes.
type Move struct {
	V    VReg
	Reg  int16
	Copy bool
}

// moveTable is moves by a key of a block and a number, the keys in order,
// key k's from off[k] to off[k+1], and block b's keys from block[b] to
// block[b+1].
type moveTable struct {
	key   [][2]int32
	off   []int32
	mv    []Move
	block []int32
}

// find returns the moves of key k.
func (t *moveTable) find(k [2]int32) []Move {
	lo, hi := t.block[k[0]], t.block[k[0]+1]
	for lo < hi {
		m := int32(uint32(lo+hi) >> 1)
		if t.key[m][1] < k[1] {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < t.block[k[0]+1] && t.key[lo][1] == k[1] {
		return t.mv[t.off[lo]:t.off[lo+1]]
	}
	return nil
}

func cmpKey(x, y [2]int32) int {
	if x[0] != y[0] {
		return int(x[0] - y[0])
	}
	return int(x[1] - y[1])
}

// Across is, for the call that is instruction inst of block b, the
// virtual registers in registers live across it, which it must load again
// from their slots.
func (r *Result) Across(b, inst int) []VReg {
	key := [2]int32{int32(b), int32(inst)}
	k, ok := slices.BinarySearchFunc(r.acrossKey, key, cmpKey)
	if !ok {
		return nil
	}
	return r.acrossV[r.acrossOff[k]:r.acrossOff[k+1]]
}

// Stored reports whether v is stored into its slot wherever it is defined
// in its register -- a phi where its block begins -- so that its slot
// holds it wherever it is live: one in a register that lives across a
// call.
func (r *Result) Stored(v VReg) bool { return r.flags[v]&flagStored != 0 }

// Split reports whether v is split: in its register (Reg) but in its slot
// (Slot) over holes in its life, as Moves, Entry and Edge say.
func (r *Result) Split(v VReg) bool { return r.flags[v]&flagSplit != 0 }

// Splits reports whether any register is split.
func (r *Result) Splits() bool { return r.splits > 0 }

// Moves are, before instruction inst of block b reads its operands, where
// split registers go: those that move into or out of their holes there,
// then those the instruction defines, each where it is defined. The moves
// that copy are made as if all at once.
func (r *Result) Moves(b, inst int) []Move {
	if r.splits == 0 {
		return nil
	}
	return r.gap.find([2]int32{int32(b), int32(inst)})
}

// GapsOf is block b's gaps -- its instructions before which split
// registers move -- from from to to, in the order of the instructions, as
// Gap numbers them.
func (r *Result) GapsOf(b int) (from, to int) {
	if r.splits == 0 {
		return 0, 0
	}
	return int(r.gap.block[b]), int(r.gap.block[b+1])
}

// Gap is gap k's instruction, by its index in its block, and its moves, as
// Moves gives them.
func (r *Result) Gap(k int) (inst int, moves []Move) {
	return int(r.gap.key[k][1]), r.gap.mv[r.gap.off[k]:r.gap.off[k+1]]
}

// Entry is where each split register live into block b, and each of its
// phis split, is where the block begins.
func (r *Result) Entry(b int) []Move {
	if r.splits == 0 {
		return nil
	}
	return r.entryMv[r.entryOff[b]:r.entryOff[b+1]]
}

// Edge is the moves on the edge from block b to its k-th successor of the
// split registers live across it, not as a phi, that are in their register
// at one end and their slot at the other; a phi's argument goes into the
// phi's location, where Entry says it is, as any other's. They are made as
// if all at once, with the phis', at b's end where b has one successor,
// and else where the successor, which has one predecessor, begins: a
// function with a critical edge is allocated without splitting.
func (r *Result) Edge(b, k int) []Move {
	if r.splits == 0 {
		return nil
	}
	return r.edge.find([2]int32{int32(b), int32(k)})
}

// ErrSlots is the error of a function whose spills need more slots than
// Config.NumSlots.
var ErrSlots = errors.New("lsra: not enough spill slots")

// LiveSetsError is the error of a function whose liveness sets would take
// more than Config.MaxLiveBytes: Need bytes.
type LiveSetsError struct{ Need int64 }

func (e *LiveSetsError) Error() string {
	return fmt.Sprintf("lsra: liveness sets of %d bytes", e.Need)
}
