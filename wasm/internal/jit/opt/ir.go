// Package opt is the optimizing tier of the WebAssembly JIT: it builds SSA
// from a validated function body, optimizes it and lowers it to native code
// that keeps the baseline's frames, context and calls (see README.md).
package opt

import (
	"fmt"
	"math"
	"strings"
)

// Type is an SSA value's type: the four numeric types, v128, a call's
// tuple of results, or none.
type Type uint8

const (
	TNone Type = iota
	TI32
	TI64
	TF32
	TF64
	// TTuple is a call's results, read by OpResult.
	TTuple
	// TRef is a reference's, which is never a value: the builder's stack
	// and locals say by it that a reference is in its reference slot.
	TRef
	// TV128 is a vector of 128 bits, which lives in a float register or
	// two slots.
	TV128
	numTypes
)

func (t Type) String() string {
	return [...]string{"none", "i32", "i64", "f32", "f64", "tuple", "ref", "v128"}[t]
}

// InFloatReg reports whether a register of t is a float register: an f32,
// an f64 or a v128.
func (t Type) InFloatReg() bool { return t == TF32 || t == TF64 || t == TV128 }

// Slots is the number of the frame's slots a value of t takes.
func (t Type) Slots() int32 {
	if t == TV128 {
		return 2
	}
	return 1
}

// IsFloat reports whether t is f32 or f64.
func (t Type) IsFloat() bool { return t == TF32 || t == TF64 }

// IsInt reports whether t is i32 or i64.
func (t Type) IsInt() bool { return t == TI32 || t == TI64 }

// Op is an SSA operation.
type Op uint8

// The operations. An integer or float operation's width is its type's; a
// comparison's, a conversion's and a reinterpretation's operand's is its
// argument's.
const (
	OpInvalid Op = iota
	// OpConst is the constant Aux: an i32's or f32's bits zero-extended; a
	// v128's low half, its high half Aux2.
	OpConst
	// OpParam is parameter Aux, as the frame holds it when the function
	// begins.
	OpParam
	// OpPhi is the value of its Args[i] on the edge from Preds[i].
	OpPhi
	// OpCopy is Args[0].
	OpCopy

	OpAdd
	OpSub
	OpMul
	OpAnd
	OpOr
	OpXor
	OpShl
	OpShrS
	OpShrU
	OpRotl
	OpRotr
	// The divisions and remainders trap at At.
	OpDivS
	OpDivU
	OpRemS
	OpRemU
	OpClz
	OpCtz
	OpPopcnt
	OpEqz
	OpEq
	OpNe
	OpLtS
	OpLtU
	OpGtS
	OpGtU
	OpLeS
	OpLeU
	OpGeS
	OpGeU
	// OpWrap is i32.wrap_i64; OpExtendS and OpExtendU extend an i32 to an
	// i64; OpExtend8S, OpExtend16S and OpExtend32S sign-extend the low bits
	// of an integer within its type.
	OpWrap
	OpExtendS
	OpExtendU
	OpExtend8S
	OpExtend16S
	OpExtend32S

	OpFAdd
	OpFSub
	OpFMul
	OpFDiv
	OpFMin
	OpFMax
	OpFCopysign
	OpFAbs
	OpFNeg
	OpFSqrt
	OpFCeil
	OpFFloor
	OpFTrunc
	OpFNearest
	OpFEq
	OpFNe
	OpFLt
	OpFGt
	OpFLe
	OpFGe
	OpPromote
	OpDemote
	// OpReinterpret moves bits between an integer and a float of a width.
	OpReinterpret
	// OpConvertS and OpConvertU convert an integer to a float.
	OpConvertS
	OpConvertU
	// OpTruncS and OpTruncU convert a float to an integer, trapping at At
	// on NaN and out of range; OpTruncSatS and OpTruncSatU saturate.
	OpTruncS
	OpTruncU
	OpTruncSatS
	OpTruncSatU
	// OpSelect is Args[0] if Args[2] is not zero, else Args[1].
	OpSelect

	// OpLoad loads from memory Aux2 (memIndex, with memShared and memWide)
	// at Args[0] plus offset Aux: Size bytes, sign-extended if Signed; it
	// traps at At past the memory's end, unless NoCheck says a dominating
	// access checked it.
	OpLoad
	// OpStore stores Args[1]'s low Size bytes at Args[0] plus Aux, in
	// memory Aux2.
	OpStore
	// OpGlobalAddr is the address of global Aux's value, which the
	// instance keeps for the code's run; OpGlobalGet and OpGlobalSet read
	// and write the global at that address, Args[0].
	OpGlobalAddr
	OpGlobalGet
	OpGlobalSet
	// OpCall calls function Aux with Args, at At; its results, a tuple,
	// are read by OpResult.
	OpCall
	// OpCallIndirect is call_indirect of type Aux through table Aux2, its
	// arguments and the index (last) in Args.
	OpCallIndirect
	// OpGoOp runs the instruction whose opcode is Aux in Go, with
	// immediate Aux2 (goImms), on Args; its result, if Type is not TNone,
	// is its value: memory.size, memory.grow, memory.copy, memory.fill and
	// the other instructions goops.go lists. InCode says the code does it
	// itself (BulkInCode).
	OpGoOp
	// OpResult is result Aux of the call Args[0].
	OpResult
	// OpCallRef is call_ref of type Aux, its numeric arguments in Args;
	// the reference it calls is in the reference slot of stack position
	// Pos plus the number of parameters.
	OpCallRef
	// OpRef is a reference instruction (refKind, Aux's low byte; an index
	// above it): it reads and writes reference slots -- the destination's
	// and the source's indices from R, the high and low words of Aux2 --
	// and the numeric arguments Args, and makes an i32 or an i64 where its
	// Type says so. It traps at At where the instruction does.
	OpRef
	// OpTail, OpTailIndirect and OpTailRef are return_call of function
	// Aux, return_call_indirect of type Aux through table Aux2 and
	// return_call_ref of type Aux, their numeric arguments (and the index)
	// in Args: the last value of a block that ends with them (BTail).
	OpTail
	OpTailIndirect
	OpTailRef
	// OpSIMD is the SIMD instruction whose opcode is Aux's low 24 bits
	// (simdOp), on Args, its immediate a lane in Aux2, or, for a shuffle,
	// the lanes (shuffleLanes). Loads and stores are OpLoad and OpStore of
	// v128s and of the scalars it widens, splats or puts in a lane.
	OpSIMD

	// The atomic accesses, each of memory Aux2 at Args[0] plus offset Aux,
	// Size bytes, sequentially consistent: OpAtomicLoad loads them
	// zero-extended, OpAtomicStore stores Args[1]'s, and the
	// read-modify-writes store Args[1] added, subtracted, and-ed, or-ed,
	// xor-ed or as it is (OpAtomicXchg), and OpAtomicCmpxchg Args[2] where
	// they equal Args[1]'s, each being the old bytes zero-extended. Each
	// traps at At where its address is not a multiple of Size, then past
	// the memory's end.
	OpAtomicLoad
	OpAtomicStore
	OpAtomicAdd
	OpAtomicSub
	OpAtomicAnd
	OpAtomicOr
	OpAtomicXor
	OpAtomicXchg
	OpAtomicCmpxchg
	// OpFence is atomic.fence.
	OpFence

	numOps
)

// The reference instructions an OpRef makes (refKind), dst and src being
// the reference slots its Aux2 names, idx the index its Aux holds above
// the kind.
const (
	// refNull makes dst null.
	refNull = iota
	// refFunc makes dst function idx.
	refFunc
	// refCopy copies src to dst.
	refCopy
	// refGlobalGet copies global idx to dst, refGlobalSet src to global idx.
	refGlobalGet
	refGlobalSet
	// refTableGet copies the element Args[0] of table idx to dst, and
	// refTableSet src to it, trapping past the table's end.
	refTableGet
	refTableSet
	// refIsNull is 1 if src is null, else 0.
	refIsNull
	// refEq is 1 if dst and src are the same reference, else 0.
	refEq
	// refAsNonNull traps if src is null.
	refAsNonNull
	// refSelect copies src to dst if Args[0] is 0.
	refSelect
	// refTableSize is table idx's number of elements.
	refTableSize
)

// opFlags says what an operation does besides compute its value.
type opFlags uint8

const (
	// fPure: no effect, no trap, the value a function of the arguments:
	// it may be shared (GVN), moved (LICM) and removed when unused (DCE).
	fPure opFlags = 1 << iota
	// fTrap: it may trap.
	fTrap
	// fEffect: it writes memory, a global, or calls: it stays where it is.
	fEffect
	// fComm: its two arguments commute.
	fComm
	// fCall: it clobbers every register (a call, or work done in Go).
	fCall
)

type opInfo struct {
	name  string
	flags opFlags
}

var opInfos = [numOps]opInfo{
	OpInvalid:      {"invalid", 0},
	OpConst:        {"const", fPure},
	OpParam:        {"param", 0},
	OpPhi:          {"phi", 0},
	OpCopy:         {"copy", fPure},
	OpAdd:          {"add", fPure | fComm},
	OpSub:          {"sub", fPure},
	OpMul:          {"mul", fPure | fComm},
	OpAnd:          {"and", fPure | fComm},
	OpOr:           {"or", fPure | fComm},
	OpXor:          {"xor", fPure | fComm},
	OpShl:          {"shl", fPure},
	OpShrS:         {"shr_s", fPure},
	OpShrU:         {"shr_u", fPure},
	OpRotl:         {"rotl", fPure},
	OpRotr:         {"rotr", fPure},
	OpDivS:         {"div_s", fTrap},
	OpDivU:         {"div_u", fTrap},
	OpRemS:         {"rem_s", fTrap},
	OpRemU:         {"rem_u", fTrap},
	OpClz:          {"clz", fPure},
	OpCtz:          {"ctz", fPure},
	OpPopcnt:       {"popcnt", fPure},
	OpEqz:          {"eqz", fPure},
	OpEq:           {"eq", fPure | fComm},
	OpNe:           {"ne", fPure | fComm},
	OpLtS:          {"lt_s", fPure},
	OpLtU:          {"lt_u", fPure},
	OpGtS:          {"gt_s", fPure},
	OpGtU:          {"gt_u", fPure},
	OpLeS:          {"le_s", fPure},
	OpLeU:          {"le_u", fPure},
	OpGeS:          {"ge_s", fPure},
	OpGeU:          {"ge_u", fPure},
	OpWrap:         {"wrap", fPure},
	OpExtendS:      {"extend_s", fPure},
	OpExtendU:      {"extend_u", fPure},
	OpExtend8S:     {"extend8_s", fPure},
	OpExtend16S:    {"extend16_s", fPure},
	OpExtend32S:    {"extend32_s", fPure},
	OpFAdd:         {"fadd", fPure},
	OpFSub:         {"fsub", fPure},
	OpFMul:         {"fmul", fPure},
	OpFDiv:         {"fdiv", fPure},
	OpFMin:         {"fmin", fPure},
	OpFMax:         {"fmax", fPure},
	OpFCopysign:    {"copysign", fPure},
	OpFAbs:         {"fabs", fPure},
	OpFNeg:         {"fneg", fPure},
	OpFSqrt:        {"fsqrt", fPure},
	OpFCeil:        {"fceil", fPure},
	OpFFloor:       {"ffloor", fPure},
	OpFTrunc:       {"ftrunc", fPure},
	OpFNearest:     {"fnearest", fPure},
	OpFEq:          {"feq", fPure},
	OpFNe:          {"fne", fPure},
	OpFLt:          {"flt", fPure},
	OpFGt:          {"fgt", fPure},
	OpFLe:          {"fle", fPure},
	OpFGe:          {"fge", fPure},
	OpPromote:      {"promote", fPure},
	OpDemote:       {"demote", fPure},
	OpReinterpret:  {"reinterpret", fPure},
	OpConvertS:     {"convert_s", fPure},
	OpConvertU:     {"convert_u", fPure},
	OpTruncS:       {"trunc_s", fTrap},
	OpTruncU:       {"trunc_u", fTrap},
	OpTruncSatS:    {"trunc_sat_s", fPure},
	OpTruncSatU:    {"trunc_sat_u", fPure},
	OpSelect:       {"select", fPure},
	OpLoad:         {"load", fTrap},
	OpStore:        {"store", fTrap | fEffect},
	OpGlobalAddr:   {"global.addr", fPure},
	OpGlobalGet:    {"global.get", 0},
	OpGlobalSet:    {"global.set", fEffect},
	OpCall:         {"call", fEffect | fTrap | fCall},
	OpCallIndirect: {"call_indirect", fEffect | fTrap | fCall},
	OpGoOp:         {"goop", fEffect | fTrap | fCall},
	OpResult:       {"result", 0},
	OpCallRef:      {"call_ref", fEffect | fTrap | fCall},
	OpRef:          {"ref", fEffect | fTrap},
	OpTail:         {"return_call", fEffect | fTrap},
	OpTailIndirect: {"return_call_indirect", fEffect | fTrap},
	OpTailRef:      {"return_call_ref", fEffect | fTrap},
	OpSIMD:         {"simd", fPure},

	OpAtomicLoad:    {"atomic.load", fEffect | fTrap},
	OpAtomicStore:   {"atomic.store", fEffect | fTrap},
	OpAtomicAdd:     {"atomic.add", fEffect | fTrap},
	OpAtomicSub:     {"atomic.sub", fEffect | fTrap},
	OpAtomicAnd:     {"atomic.and", fEffect | fTrap},
	OpAtomicOr:      {"atomic.or", fEffect | fTrap},
	OpAtomicXor:     {"atomic.xor", fEffect | fTrap},
	OpAtomicXchg:    {"atomic.xchg", fEffect | fTrap},
	OpAtomicCmpxchg: {"atomic.cmpxchg", fEffect | fTrap},
	OpFence:         {"fence", fEffect},
}

func (o Op) String() string { return opInfos[o].name }

// Value is an SSA value: an operation, its arguments, and its result.
type Value struct {
	ID    int32
	Op    Op
	Type  Type
	Args  []*Value
	Aux   uint64
	Aux2  uint64
	Block *Block
	// At is the offset in the module of the instruction a trap or a call
	// reports.
	At uint32
	// Size and Signed are a load's or a store's width in bytes and
	// whether a load sign-extends.
	Size   uint8
	Signed bool
	// NoCheck says a memory access needs no bounds check: a dominating
	// access of the same address checked as far.
	NoCheck bool
	// InCode says an instruction Go runs is done by the code itself
	// (BulkInCode).
	InCode bool
	// Pos is a call's or a tail call's arguments' first stack position,
	// whose reference slot and those after it hold its reference
	// arguments, and a call's reference results.
	Pos int32
	// Uses counts the value's uses: as arguments, as a block's control or
	// result.
	Uses int32
	// vreg is the lowering's number of the value among those it gives the
	// allocator.
	vreg int32
}

func (v *Value) String() string { return fmt.Sprintf("v%d", v.ID) }

// Pure reports whether v may be shared, moved and removed: it has no
// effect and cannot trap.
func (v *Value) Pure() bool {
	if opInfos[v.Op].flags&fPure != 0 {
		return true
	}
	switch v.Op {
	case OpGlobalGet:
		// An immutable global's.
		return v.Aux2 == 1
	case OpDivS, OpRemS, OpDivU, OpRemU:
		// A division by a constant other than 0 -- and, signed, -1 --
		// cannot trap.
		d := v.Args[1]
		if d.Op != OpConst {
			return false
		}
		c := d.Aux
		if v.Type == TI32 {
			c = uint64(uint32(c))
			if v.Op == OpDivS || v.Op == OpRemS {
				return c != 0 && c != 0xffffffff
			}
			return c != 0
		}
		if v.Op == OpDivS || v.Op == OpRemS {
			return c != 0 && c != ^uint64(0)
		}
		return c != 0
	}
	return false
}

// HasEffect reports whether v must stay where it is even if unused.
func (v *Value) HasEffect() bool {
	f := opInfos[v.Op].flags
	return f&fEffect != 0 || (f&fTrap != 0 && !v.Pure())
}

// IsCall reports whether v clobbers every register: a call, or work done
// in Go.
func (v *Value) IsCall() bool { return opInfos[v.Op].flags&fCall != 0 && !v.BulkInCode() }

// BulkInCode reports whether v is a memory.copy or a memory.fill of memory
// 0, neither shared nor of i64 addresses, which the code does itself
// rather than Go: the builder says so (InCode).
func (v *Value) BulkInCode() bool { return v.Op == OpGoOp && v.InCode }

// A memory access's memory, its Aux2: the memory's index in the low word,
// and memShared and memWide for a shared memory and one of i64 addresses.
const (
	memShared = 1 << 32
	memWide   = 1 << 33
)

// memIndex is the index of memory access v's memory.
func memIndex(v *Value) uint32 { return uint32(v.Aux2) }

// plainAccess reports whether memory access v is of memory 0, unshared and
// of i32 addresses, at an offset whose end is within 2^31-1: the access
// lowered with its offset a displacement and its end checked against the
// context's size.
func plainAccess(v *Value) bool { return v.Aux2 == 0 && v.Aux+uint64(v.Size) <= math.MaxInt32 }

// isAtomic reports whether op is an atomic access's.
func isAtomic(op Op) bool { return op >= OpAtomicLoad && op <= OpAtomicCmpxchg }

// isTail reports whether op is a tail call's.
func isTail(op Op) bool { return op == OpTail || op == OpTailIndirect || op == OpTailRef }

// Commutes reports whether v's two arguments commute.
func (v *Value) Commutes() bool { return opInfos[v.Op].flags&fComm != 0 }

// BlockKind is how a block ends.
type BlockKind uint8

const (
	// BPlain jumps to its one successor.
	BPlain BlockKind = iota
	// BIf goes to Succs[0] if Control is not zero, else to Succs[1].
	BIf
	// BSwitch goes to Succs[Table[i]] for Control i, the last entry of
	// Table for an index past the others.
	BSwitch
	// BReturn returns Results.
	BReturn
	// BTrap traps (unreachable) at At.
	BTrap
	// BTail ends with its last value, a tail call, which leaves the
	// function.
	BTail
)

// Block is a basic block.
type Block struct {
	ID   int32
	Kind BlockKind
	// Phis are the block's phis, Values the rest, in order.
	Phis    []*Value
	Values  []*Value
	Control *Value
	Succs   []*Block
	Preds   []*Block
	Table   []int32
	Results []*Value
	At      uint32
	// Loop is the header of the innermost loop the block is in, or nil;
	// Depth the number of loops it is in.
	Loop  *Block
	Depth int32
	// outer is a loop header's enclosing loop's header, or nil.
	outer *Block
	// dead says the block was removed.
	dead bool
	// The builder's state: sealed says every predecessor is known;
	// incomplete are the phis of locals made before it was sealed. The
	// locals' values at its end are the workspace's (workspace.defs).
	sealed     bool
	incomplete []incompletePhi
}

type incompletePhi struct {
	k   int32
	phi *Value
}

func (b *Block) String() string { return fmt.Sprintf("b%d", b.ID) }

// Func is a function in SSA form.
type Func struct {
	Blocks []*Block
	Entry  *Block
	// Params and Results are the types of the signature's numeric
	// parameters and results, ResultSlots the numeric slots the results
	// go in. RLocals is the number of reference slots before the operand
	// stack's -- the reference locals' or results', whichever is more --
	// and RefParams the reference parameters'; a stack position h's
	// reference slot is RLocals+h. MaxHeight is the most operands the
	// stack holds, and Refs says the function reads or writes a reference
	// slot.
	Params, Results    []Type
	ResultSlots        []int32
	RLocals, RefParams int
	MaxHeight          int
	Refs               bool
	nextValue          int32
	nextBlock          int32
	// ws is the workspace the function's values and blocks, and its
	// passes' tables, are made in; dom is its dominator tree, there, while
	// the control flow graph is as it was made (dominators).
	ws  *workspace
	dom *domTree
}

// NumValues is one past the highest value ID.
func (f *Func) NumValues() int { return int(f.nextValue) }

func (f *Func) newBlock() *Block {
	b := f.ws.blocks.new()
	b.ID = f.nextBlock
	f.nextBlock++
	f.Blocks = appendA(&f.ws.blks, f.Blocks, b)
	return b
}

// value makes a value, in no block's list.
func (f *Func) value(b *Block, op Op, t Type, args []*Value) *Value {
	v := f.ws.values.new()
	v.ID, v.Op, v.Type, v.Block = f.nextValue, op, t, b
	if len(args) > 0 {
		v.Args = append(f.ws.vals.take(len(args)), args...)
	}
	f.nextValue++
	return v
}

func (f *Func) newValue(b *Block, op Op, t Type, args ...*Value) *Value {
	v := f.value(b, op, t, args)
	if op == OpPhi {
		b.Phis = appendA(&f.ws.vals, b.Phis, v)
	} else {
		b.Values = appendA(&f.ws.vals, b.Values, v)
	}
	return v
}

// addArg appends a to v's arguments.
func (f *Func) addArg(v, a *Value) { v.Args = appendA(&f.ws.vals, v.Args, a) }

// setArgs makes v's arguments args, in the workspace.
func (f *Func) setArgs(v *Value, args ...*Value) {
	v.Args = append(f.ws.vals.take(len(args)), args...)
}

func (f *Func) addEdge(from, to *Block) {
	from.Succs = appendA(&f.ws.blks, from.Succs, to)
	to.Preds = appendA(&f.ws.blks, to.Preds, from)
}

// predIndex is the index of p among b's predecessors.
func (b *Block) predIndex(p *Block) int {
	for i, q := range b.Preds {
		if q == p {
			return i
		}
	}
	return -1
}

// String prints the function, a line for each value.
func (f *Func) String() string {
	var sb strings.Builder
	for _, b := range f.Blocks {
		if b.dead {
			continue
		}
		fmt.Fprintf(&sb, "%v: preds", b)
		for _, p := range b.Preds {
			fmt.Fprintf(&sb, " %v", p)
		}
		if b.Loop != nil {
			fmt.Fprintf(&sb, " (loop %v depth %d)", b.Loop, b.Depth)
		} else if b.Depth > 0 {
			fmt.Fprintf(&sb, " (depth %d)", b.Depth)
		}
		sb.WriteByte('\n')
		for _, v := range b.Phis {
			fmt.Fprintf(&sb, "  %s\n", v.long())
		}
		for _, v := range b.Values {
			fmt.Fprintf(&sb, "  %s\n", v.long())
		}
		switch b.Kind {
		case BPlain:
			fmt.Fprintf(&sb, "  jump %v\n", b.Succs[0])
		case BIf:
			fmt.Fprintf(&sb, "  if %v %v %v\n", b.Control, b.Succs[0], b.Succs[1])
		case BSwitch:
			fmt.Fprintf(&sb, "  switch %v", b.Control)
			for _, i := range b.Table {
				fmt.Fprintf(&sb, " %v", b.Succs[i])
			}
			sb.WriteByte('\n')
		case BReturn:
			fmt.Fprintf(&sb, "  return %v\n", b.Results)
		case BTrap:
			fmt.Fprintf(&sb, "  trap @%d\n", b.At)
		case BTail:
			fmt.Fprintf(&sb, "  tail\n")
		}
	}
	return sb.String()
}

func (v *Value) long() string {
	s := fmt.Sprintf("%v = %v %v", v, v.Op, v.Type)
	for _, a := range v.Args {
		s += " " + a.String()
	}
	switch v.Op {
	case OpConst, OpParam, OpGlobalAddr, OpGlobalGet, OpGlobalSet, OpCall, OpResult, OpLoad, OpStore, OpCallIndirect, OpGoOp,
		OpCallRef, OpTail, OpTailIndirect, OpTailRef:
		s += fmt.Sprintf(" [%#x]", v.Aux)
	case OpRef:
		s += fmt.Sprintf(" [kind %d %#x r%d<-r%d]", v.Aux&0xff, v.Aux>>8, v.Aux2>>32, uint32(v.Aux2))
	case OpSIMD:
		s += fmt.Sprintf(" [%s %#x]", simdOp(v).Name(), v.Aux2)
	}
	if v.Op == OpConst && v.Type == TV128 {
		s += fmt.Sprintf(" [%#x]", v.Aux2)
	}
	if isAtomic(v.Op) {
		s += fmt.Sprintf(" [%#x] mem %d size %d", v.Aux, memIndex(v), v.Size)
	}
	if v.Op == OpLoad || v.Op == OpStore {
		if v.Aux2 != 0 {
			s += fmt.Sprintf(" mem %d", memIndex(v))
		}
		s += fmt.Sprintf(" size %d", v.Size)
		if v.NoCheck {
			s += " nocheck"
		}
	}
	return s
}
