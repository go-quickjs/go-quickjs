// Package opt is the optimizing tier of the WebAssembly JIT: it builds SSA
// from a validated function body, optimizes it and lowers it to native code
// that keeps the baseline's frames, context and calls (see README.md).
package opt

import (
	"fmt"
	"strings"

	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Type is an SSA value's type: the four numeric types, a call's tuple of
// results, or none.
type Type uint8

const (
	TNone Type = iota
	TI32
	TI64
	TF32
	TF64
	// TTuple is a call's results, read by OpResult.
	TTuple
)

func (t Type) String() string {
	return [...]string{"none", "i32", "i64", "f32", "f64", "tuple"}[t]
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
	// OpConst is the constant Aux: an i32's or f32's bits zero-extended.
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

	// OpLoad loads from memory 0 at Args[0] plus offset Aux: Size bytes,
	// sign-extended if Signed; it traps at At past the memory's end,
	// unless NoCheck says a dominating access checked it.
	OpLoad
	// OpStore stores Args[1]'s low Size bytes at Args[0] plus Aux.
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
	// immediate Aux2, on Args; its result, if Type is not TNone, is its
	// value: memory.size, memory.grow, memory.copy, memory.fill.
	OpGoOp
	// OpResult is result Aux of the call Args[0].
	OpResult

	numOps
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
	// Uses counts the value's uses: as arguments, as a block's control or
	// result.
	Uses int32
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
// 0, which the code does itself rather than Go.
func (v *Value) BulkInCode() bool {
	if v.Op != OpGoOp || v.Aux2 != 0 {
		return false
	}
	op := opcode.Opcode(v.Aux)
	return op == opcode.MemoryCopy || op == opcode.MemoryFill
}

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
	// The builder's state: sealed says every predecessor is known; defs
	// are the locals' values at the block's end; incomplete are the phis
	// of locals made before it was sealed.
	sealed     bool
	defs       map[int32]*Value
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
	// Params and Results are the signature's types.
	Params, Results []Type
	nextValue       int32
	nextBlock       int32
}

// NumValues is one past the highest value ID.
func (f *Func) NumValues() int { return int(f.nextValue) }

func (f *Func) newBlock() *Block {
	b := &Block{ID: f.nextBlock}
	f.nextBlock++
	f.Blocks = append(f.Blocks, b)
	return b
}

func (f *Func) newValue(b *Block, op Op, t Type, args ...*Value) *Value {
	v := &Value{ID: f.nextValue, Op: op, Type: t, Block: b}
	if len(args) > 0 {
		v.Args = append([]*Value(nil), args...)
	}
	f.nextValue++
	if op == OpPhi {
		b.Phis = append(b.Phis, v)
	} else {
		b.Values = append(b.Values, v)
	}
	return v
}

func addEdge(from, to *Block) {
	from.Succs = append(from.Succs, to)
	to.Preds = append(to.Preds, from)
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
	case OpConst, OpParam, OpGlobalAddr, OpGlobalGet, OpGlobalSet, OpCall, OpResult, OpLoad, OpStore, OpCallIndirect, OpGoOp:
		s += fmt.Sprintf(" [%#x]", v.Aux)
	}
	if v.Op == OpLoad || v.Op == OpStore {
		s += fmt.Sprintf(" size %d", v.Size)
		if v.NoCheck {
			s += " nocheck"
		}
	}
	return s
}
