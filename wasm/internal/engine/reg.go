package engine

import (
	"math"
	"os"
	"sync"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Register code: a function body compiled to fixed-width instructions over
// the frame's slots, run by a loop over a table of handlers (README.md).
//
// An instruction is one 64-bit word:
//
//	bits  0- 7  Op  the operation, the index of its handler in regTable
//	bits  8-15  X   a small operand: rExt's operation in regExtTable
//	bits 16-31  A   a slot, most often the destination
//	bits 32-47  B   a slot, or a 16-bit immediate (SB)
//	bits 48-63  C   a slot, a 16-bit immediate or a branch's offset (SC)
//
// B and C read together are BC, a 32-bit immediate, index or offset.

// regOp is an operation's value, the index of its handler in its table.
type regOp = uint8

// The hand-written operations of the first table; the numeric ones follow
// from regNumFirst (reg_num_gen.go). The second table's start with the
// hand-written ones and go on from regExtNumFirst.
const (
	rInvalid     regOp = iota
	rExt               // regExtTable[X]
	rMov               // A = B
	rConst32           // A = BC, zero-extended
	rConst32S          // A = BC, sign-extended
	rConst64           // A = the next word
	rJmp               // pc += BC
	rBrNZ              // if A != 0: pc += BC
	rBrZ               // if A == 0: pc += BC
	rBrTable           // pc = tables[BC][min(A, last)]
	rRet               // the frame returns, its results in place
	rRet1              // slot 0 = A; the frame returns
	rCall              // the call of sites[BC]
	rReturnCall        // the tail call of sites[BC]
	rSelect            // if C == 0: A = B
	rGlobalGet32       // A = global BC's low half
	rGlobalGet64       // A = global BC
	rGlobalSet         // global BC = A

	// regNumFirst is the first numeric operation's value.
	regNumFirst regOp = 24
)

const (
	xUnreachable regOp = iota // trap
	xMemorySize               // A = memory 0's size in pages
	xMemoryGrow               // A = memory 0 grown by B pages

	regExtNumFirst
)

// regNum is how a numeric operator compiles (regNumeric): op is its
// operation, in the second table if ext, a move for an operator that
// changes no bit; imm its form with an immediate right operand, or 0; comm
// says the operator is commutative, so an immediate on the left can be its
// right; unary that it has one operand; res is the result's type. A
// comparison of integers fuses with a branch: br and brI are the branch's
// operations, swap the comparison with its operands swapped and neg its
// negation.
type regNum struct {
	op, imm     regOp
	ext         bool
	comm, unary bool
	br, brI     regOp
	swap, neg   opcode.Opcode
	res         module.ValueType
}

// regCode is a function's register code.
type regCode struct {
	code []uint64
	// offs is, for each instruction, the module offset of the instruction
	// it was compiled from, which a trap reports.
	offs []uint32
	// sites are the calls' sites; tables the br_tables' targets, by pc.
	sites  []regSite
	tables [][]int32
	// loops maps each loop's head, by pc, to the loop's offset in the
	// module, which names its entry in native code.
	loops map[int32]uint32
}

// regSite is a call's site: the function called, the offset of the call,
// and the moves of the arguments into the callee's frame and of the results
// out of it.
type regSite struct {
	fn         uint32
	at         uint32
	args, rets []regMove
}

// regMove is a slot's move from one frame to another.
type regMove struct{ from, to uint32 }

// regHandler is an operation's handler: given the frame's numeric slots,
// the instruction, its pc and the frame, it returns the pc to go on at: the
// next, a branch's target, ^pc for the loop to run the operation's slow
// function, or regDone once the frame has returned.
type regHandler func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32

// regDone is what a handler returns once its frame has returned.
const regDone int32 = math.MinInt32

var (
	regTable     [256]regHandler
	regSlowTable [256]regHandler
	regExtTable  [256]regHandler
)

// rs returns the slot an instruction's field e, in its low 16 bits, names.
// The compiler made every slot an instruction names one of the frame's.
func rs(l unsafe.Pointer, e uint64) *uint64 {
	return (*uint64)(unsafe.Add(l, uintptr(uint16(e))<<3))
}

// regPCMark marks a trap's offset as a pc of register code (regAt), which
// the trap's recovery makes the module offset of the instruction the pc's
// was compiled from: the register compiler takes no module past 2 GiB,
// whose offsets could have the mark.
const regPCMark = 1 << 31

// regAt is the offset a handler at pc traps with.
func regAt(pc int32) uint32 { return uint32(pc) | regPCMark }

// regOffset returns the module offset of the instruction at pc of the
// innermost frame's register code, which trapped.
func (x *exec) regOffset(pc uint32) uint32 {
	c := x.calls[len(x.calls)-1]
	return c.fn.Inst.Code.fns[c.fn.Index].Load().reg.offs[pc]
}

// regLoop runs register code from pc in frame f until the frame returns.
func regLoop(l, code unsafe.Pointer, pc int32, f *frame) {
	for {
		w := *(*uint64)(unsafe.Add(code, uintptr(pc)<<3))
		if pc = regTable[uint8(w)](l, w, pc, f); pc < 0 {
			if pc == regDone {
				return
			}
			pc = ^pc
			w = *(*uint64)(unsafe.Add(code, uintptr(pc)<<3))
			if pc = regSlowTable[uint8(w)](l, w, pc, f); pc < 0 {
				return
			}
		}
	}
}

// runReg runs cd's register code in frame f, returning 0, the results in
// place, or sigTail with the exec's tail call set.
func (x *exec) runReg(f *frame, cd *compiled) int32 {
	rc := cd.reg
	f.rc, f.sig = rc, 0
	if !jitBuilt {
		f.osr = math.MaxInt32
	}
	f.setTick()
	regLoop(unsafe.Pointer(&f.l[0]), unsafe.Pointer(&rc.code[0]), 0, f)
	f.rc = nil
	return f.sig
}

// setTick sets the frame's count of back edges to the next of its checks:
// of the context (the exec's poll), or of native code (osr).
func (f *frame) setTick() {
	f.tickSet = min(f.x.poll, f.osr)
	f.tick = f.tickSet
}

// regTake16 and regTake32 take a branch whose offset is SC or BC: a branch
// back counts toward the frame's tick, and where that is spent the slow
// function checks the context and native code (backEdge).
func regTake16(w uint64, pc int32, f *frame) int32 {
	off := int32(int16(w >> 48))
	if off <= 0 {
		if f.tick--; f.tick <= 0 {
			return ^pc
		}
	}
	return pc + off
}

func regTake32(w uint64, pc int32, f *frame) int32 {
	off := int32(w >> 32)
	if off <= 0 {
		if f.tick--; f.tick <= 0 {
			return ^pc
		}
	}
	return pc + off
}

// backEdge is a branch back to target, a loop's head, that spent the
// frame's tick: it counts the back edges since the last check toward the
// poll and the native code's threshold, checks the context, and goes on in
// native code from the loop's head where the threshold is spent. It reports
// whether native code ran the frame to its return.
func (f *frame) backEdge(target int32) bool {
	x := f.x
	n := f.tickSet - f.tick
	x.poll -= n
	f.osr -= n
	if x.poll <= 0 {
		x.pollContext()
	}
	if f.osr <= 0 {
		if jitBuilt {
			c := x.calls[len(x.calls)-1]
			if sig, ok := x.osr(f, c.fn.Index, f.rc.loops[target]); ok {
				if sig == sigTail {
					f.sig = sigTail
				}
				return true
			}
		}
		if f.osr <= 0 {
			f.osr = math.MaxInt32
		}
	}
	f.setTick()
	return false
}

// The slow functions of the branches, which their handlers leave a back
// edge to when the tick is spent.

func regSlow16(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	target := pc + int32(int16(w>>48))
	if f.backEdge(target) {
		return regDone
	}
	return target
}

func regSlow32(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	target := pc + int32(w>>32)
	if f.backEdge(target) {
		return regDone
	}
	return target
}

func regSlowBrTable(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	target := regTableTarget(l, w, f)
	if f.backEdge(target) {
		return regDone
	}
	return target
}

func rhInvalid(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	panic("wasm: register code holds an invalid operation")
}

func rhExt(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	return regExtTable[uint8(w>>8)](l, w, pc, f)
}

func rhMov(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = *rs(l, w>>32)
	return pc + 1
}

func rhConst32(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = uint64(uint32(w >> 32))
	return pc + 1
}

func rhConst32S(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = uint64(int64(int32(w >> 32)))
	return pc + 1
}

func rhConst64(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = f.rc.code[pc+1]
	return pc + 2
}

func rhJmp(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	return regTake32(w, pc, f)
}

func rhBrNZ(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	if *rs(l, w>>16) != 0 {
		return regTake32(w, pc, f)
	}
	return pc + 1
}

func rhBrZ(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	if *rs(l, w>>16) == 0 {
		return regTake32(w, pc, f)
	}
	return pc + 1
}

// regTableTarget is the target a br_table takes.
func regTableTarget(l unsafe.Pointer, w uint64, f *frame) int32 {
	t := f.rc.tables[uint32(w>>32)]
	i := uint64(uint32(*rs(l, w>>16)))
	if last := uint64(len(t) - 1); i > last {
		i = last
	}
	return t[i]
}

func rhBrTable(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	target := regTableTarget(l, w, f)
	if target <= pc {
		if f.tick--; f.tick <= 0 {
			return ^pc
		}
	}
	return target
}

func rhRet(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	return regDone
}

func rhRet1(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*(*uint64)(l) = *rs(l, w>>16)
	return regDone
}

func rhCall(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	s := &f.rc.sites[uint32(w>>32)]
	x := f.x
	fn := f.inst.Funcs[s.fn]
	x.calls[len(x.calls)-1].offset = s.at
	if fn.Host != nil {
		regHostCall(l, x, fn, s)
		return pc + 1
	}
	cd := fn.Inst.Code.get(fn.Index)
	nc, nsp, rc, rsp := x.nc, x.nsp, x.rc, x.rsp
	nl, _, _ := x.takeNum(cd.nslots)
	nr, _, _ := x.takeRef(cd.rslots)
	for _, m := range s.args {
		nl[m.to] = *rs(l, uint64(m.from))
	}
	x.run(fn, cd, nl, nr, nc, nsp, rc, rsp)
	for _, m := range s.rets {
		*rs(l, uint64(m.to)) = nl[m.from]
	}
	x.nc, x.nsp, x.rc, x.rsp = nc, nsp, rc, rsp
	return pc + 1
}

// regHostCall calls host function fn from site s.
func regHostCall(l unsafe.Pointer, x *exec, fn *Function, s *regSite) {
	vals, top := x.takeVals(len(s.args))
	for i, m := range s.args {
		vals[i] = Val{N: *rs(l, uint64(m.from))}
	}
	res := x.hostCall(fn, vals)
	x.vals = x.vals[:top]
	for i, m := range s.rets {
		*rs(l, uint64(m.to)) = res[i].N
	}
}

func rhReturnCall(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	s := &f.rc.sites[uint32(w>>32)]
	x := f.x
	x.tailArgs = x.tailArgs[:0]
	for _, m := range s.args {
		x.tailArgs = append(x.tailArgs, Val{N: *rs(l, uint64(m.from))})
	}
	x.tail = f.inst.Funcs[s.fn]
	f.sig = sigTail
	return regDone
}

func rhSelect(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	if *rs(l, w>>48) == 0 {
		*rs(l, w>>16) = *rs(l, w>>32)
	}
	return pc + 1
}

func rhGlobalGet32(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = uint64(uint32(f.inst.Globals[uint32(w>>32)].Num))
	return pc + 1
}

func rhGlobalGet64(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = f.inst.Globals[uint32(w>>32)].Num
	return pc + 1
}

func rhGlobalSet(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	f.inst.Globals[uint32(w>>32)].Num = *rs(l, w>>16)
	return pc + 1
}

func rhUnreachable(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	trapAt(TrapUnreachable, regAt(pc))
	return pc + 1
}

func rhMemorySize(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = uint64(uint32(f.inst.mem0.Pages()))
	return pc + 1
}

func rhMemoryGrow(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*rs(l, w>>16) = uint64(uint32(f.inst.mem0.Grow(uint64(uint32(*rs(l, w>>32))))))
	return pc + 1
}

func init() {
	for op, h := range map[regOp]regHandler{
		rExt: rhExt, rMov: rhMov, rConst32: rhConst32, rConst32S: rhConst32S, rConst64: rhConst64,
		rJmp: rhJmp, rBrNZ: rhBrNZ, rBrZ: rhBrZ, rBrTable: rhBrTable,
		rRet: rhRet, rRet1: rhRet1, rCall: rhCall, rReturnCall: rhReturnCall, rSelect: rhSelect,
		rGlobalGet32: rhGlobalGet32, rGlobalGet64: rhGlobalGet64, rGlobalSet: rhGlobalSet,
	} {
		regTable[op] = h
	}
	regSlowTable[rJmp] = regSlow32
	regSlowTable[rBrNZ] = regSlow32
	regSlowTable[rBrZ] = regSlow32
	regSlowTable[rBrTable] = regSlowBrTable
	regExtTable[xUnreachable] = rhUnreachable
	regExtTable[xMemorySize] = rhMemorySize
	regExtTable[xMemoryGrow] = rhMemoryGrow
	for _, t := range []*[256]regHandler{&regTable, &regSlowTable, &regExtTable} {
		for i := range t {
			if t[i] == nil {
				t[i] = rhInvalid
			}
		}
	}
}

// regDefault says whether new Codes compile function bodies to register
// code, where the register compiler takes them: WASM_INTERP=reg says so,
// and WASM_INTERP=tree or nothing leaves every body to the closure tree.
var regDefault = os.Getenv("WASM_INTERP") == "reg"

// InterpStats counts the function bodies the interpreter compiled to
// register code and those left to the closure tree, by the reason the
// register compiler gave.
type InterpStats struct {
	Register int
	Tree     map[string]int
}

var interpStats struct {
	mu       sync.Mutex
	register int
	tree     map[string]int
}

// ReadInterpStats returns the interpreter's counts so far.
func ReadInterpStats() InterpStats {
	interpStats.mu.Lock()
	defer interpStats.mu.Unlock()
	s := InterpStats{Register: interpStats.register, Tree: map[string]int{}}
	for k, v := range interpStats.tree {
		s.Tree[k] = v
	}
	return s
}

func countInterp(reason string) {
	interpStats.mu.Lock()
	defer interpStats.mu.Unlock()
	if reason == "" {
		interpStats.register++
		return
	}
	if interpStats.tree == nil {
		interpStats.tree = map[string]int{}
	}
	interpStats.tree[reason]++
}
