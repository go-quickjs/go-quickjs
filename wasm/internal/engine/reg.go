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
	rMov2              // A, A+1 = B, B+1: a v128
	rRefMov            // reference A = reference B
	rRefNull           // reference A = null
	rConst128          // A, A+1 = the next two words
	rSIMD              // regSIMDTable[X]
	rFar               // the instruction after the next two words, its slots past 16 bits (rhFar)

	// regNumFirst is the first numeric operation's value.
	regNumFirst regOp = 24
)

const (
	xUnreachable  regOp = iota // trap
	xMemorySize                // A = memory 0's size in pages
	xMemoryGrow                // A = memory 0 grown by B pages
	xGlobalGetRef              // reference A = global BC
	xGlobalSetRef              // global BC = reference A
	xGlobalGet128              // v128 A = global BC
	xGlobalSet128              // global BC = v128 A
	xSelectRef                 // if C == 0: reference A = reference B
	xSelect128                 // if C == 0: v128 A = v128 B
	xRefIsNull                 // A = reference B is null
	xRefFunc                   // reference A = function BC
	xRefEq                     // A = references B and C are the same
	xRefAsNonNull              // trap if reference A is null
	xBrNull                    // if reference A is null: pc += BC
	xBrNonNull                 // if reference A is not null: pc += BC
	xCaughtTo                  // drop the caught exceptions past the frame's BC-th

	// regExtNumFirst is the second table's first numeric operation.
	regExtNumFirst regOp = 32
)

// The kinds of value a move carries: a word, a v128's two, a reference.
const (
	vkNum uint8 = iota
	vkV128
	vkRef
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
	// regions are the bodies of the code's try constructs, which a frame
	// that has any runs under the catching loop (reg_exn.go).
	regions []regRegion
	// gc are the GC instructions' records (reg_gc.go).
	gc []regGC
}

// regSite is a call's site: the function called, the offset of the call,
// and the moves of the arguments into the callee's frame and of the results
// out of it.
type regSite struct {
	fn         uint32
	at         uint32
	args, rets []regMove
	// typeIndex, table and checked are a call_indirect's: its type, its
	// table, and whether it checks the callee's type (indirectCallee).
	typeIndex, table uint32
	checked          bool
	// target is the slot of the table index or the reference that names
	// the callee of a call_indirect or a call_ref.
	target uint32
}

// regMove is a value's move from one frame's slot to another's, of kind k.
type regMove struct {
	from, to uint32
	k        uint8
}

// regHandler is an operation's handler: given the frame's numeric slots,
// the instruction, its pc and the frame, it returns the pc to go on at: the
// next, a branch's target, ^pc for the loop to run the operation's slow
// function, or regDone once the frame has returned.
type regHandler func(l unsafe.Pointer, w uint64, pc int32, f *frame) int32

// regDone is what a handler returns once its frame has returned.
const regDone int32 = math.MinInt32

var (
	regTable        [256]regHandler
	regSlowTable    [256]regHandler
	regExtTable     [256]regHandler
	regExtSlowTable [256]regHandler
	regSIMDTable    [256]regHandler
)

// rs returns the slot an instruction's field e, in its low 16 bits, names.
// The compiler made every slot an instruction names one of the frame's.
func rs(l unsafe.Pointer, e uint64) *uint64 {
	return (*uint64)(unsafe.Add(l, uintptr(uint16(e))<<3))
}

// rv returns the v128 in the slots field e names; setV stores one there.
func rv(l unsafe.Pointer, e uint64) V128 {
	p := rs(l, e)
	return V128{*p, *(*uint64)(unsafe.Add(unsafe.Pointer(p), 8))}
}

func setV(l unsafe.Pointer, e uint64, v V128) {
	p := rs(l, e)
	*p = v.Lo
	*(*uint64)(unsafe.Add(unsafe.Pointer(p), 8)) = v.Hi
}

// rr returns the reference slot field e names.
func (f *frame) rr(e uint64) *Ref { return &f.r[uint16(e)] }

// getVal and putVal read and write a value of kind k in slot s of frame f,
// whose numeric slots are at l.
func getVal(l unsafe.Pointer, f *frame, k uint8, s uint32) Val {
	switch k {
	case vkRef:
		return Val{R: f.r[s]}
	case vkV128:
		p := (*[2]uint64)(unsafe.Add(l, uintptr(s)<<3))
		return Val{N: p[0], Hi: p[1]}
	}
	return Val{N: *(*uint64)(unsafe.Add(l, uintptr(s)<<3))}
}

func putVal(l unsafe.Pointer, f *frame, k uint8, s uint32, v Val) {
	switch k {
	case vkRef:
		f.r[s] = v.R
	case vkV128:
		p := (*[2]uint64)(unsafe.Add(l, uintptr(s)<<3))
		p[0], p[1] = v.N, v.Hi
	default:
		*(*uint64)(unsafe.Add(l, uintptr(s)<<3)) = v.N
	}
}

// regAt is the offset a handler at pc traps with: the pc's complement,
// which is past every offset of the module (the compiler makes sure), and
// which the trap's recovery makes the module offset of the instruction the
// pc's was compiled from (regOffset). So a handler's trap carries no
// offset on the path that does not trap.
func regAt(pc int32) uint32 { return ^uint32(pc) }

// regOffset returns the module offset a trap of the innermost frame
// reported as at: where at is past the module's offsets and the frame runs
// register code, the offset of the instruction at the pc regAt made it of.
func (x *exec) regOffset(at uint32) uint32 {
	c := x.calls[len(x.calls)-1]
	if c.fn == nil || c.fn.Inst == nil {
		return at
	}
	code := c.fn.Inst.Code
	if at < uint32(len(code.Module.Bytes)) {
		return at
	}
	if p := code.fns[c.fn.Index].Load(); p != nil && p.reg != nil && int(^at) < len(p.reg.offs) {
		return p.reg.offs[^at]
	}
	return at
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
	if len(rc.regions) > 0 {
		x.runRegCatching(f, rc)
	} else {
		regLoop(unsafe.Pointer(&f.l[0]), unsafe.Pointer(&rc.code[0]), 0, f)
	}
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

// rhFar runs the instruction behind a far prefix: one whose slot fields
// would name slots past 16 bits. Its fields name the frame's scratch slots
// instead -- numeric 0, 2, 4, 6 and reference 0 to 3 for A, B, C and the
// fourth operand in the word after it -- and the prefix the slots they
// stand for: the kind of each in its bits 16-23, two bits each, D in its
// high half, A and B in the next word and C in the one after. The operands
// are copied in, the instruction run on the scratch slots, its slow
// function too where it asks for it, and A, its destination if it has one,
// copied out, unless the frame has returned.
func rhFar(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	ab := f.rc.code[pc+1]
	ipc := pc + 3
	iw := f.rc.code[ipc]
	kinds := uint8(w >> 16)
	slots := [4]uint32{uint32(ab), uint32(ab >> 32), uint32(f.rc.code[pc+2]), uint32(w >> 32)}
	for i, s := range slots {
		switch kinds >> (2 * i) & 3 {
		case 1:
			f.farN[2*i] = *numAt(l, s)
		case 2:
			f.farN[2*i], f.farN[2*i+1] = *numAt(l, s), *numAt(l, s+1)
		case 3:
			f.farR[i] = f.r[s]
		}
	}
	sl := unsafe.Pointer(&f.farN[0])
	r := f.r
	f.r = f.farR[:]
	next := regTable[uint8(iw)](sl, iw, ipc, f)
	f.r = r
	if next < 0 && next != regDone {
		next = regSlowTable[uint8(iw)](sl, iw, ^next, f)
	}
	if next != regDone {
		switch kinds & 3 {
		case 1:
			*numAt(l, slots[0]) = f.farN[0]
		case 2:
			*numAt(l, slots[0]), *numAt(l, slots[0]+1) = f.farN[0], f.farN[1]
		case 3:
			f.r[slots[0]] = f.farR[0]
		}
	}
	clear(f.farR[:])
	return next
}

func rhInvalid(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	panic("wasm: register code holds an invalid operation")
}

func rhExt(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	return regExtTable[uint8(w>>8)](l, w, pc, f)
}

func rhExtSlow(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	return regExtSlowTable[uint8(w>>8)](l, w, pc, f)
}

func rhSIMD(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	return regSIMDTable[uint8(w>>8)](l, w, pc, f)
}

func rhMov2(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	setV(l, w>>16, rv(l, w>>32))
	return pc + 1
}

func rhRefMov(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*f.rr(w >> 16) = *f.rr(w >> 32)
	return pc + 1
}

func rhRefNull(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	*f.rr(w >> 16) = Null
	return pc + 1
}

func rhConst128(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	setV(l, w>>16, V128{f.rc.code[pc+1], f.rc.code[pc+2]})
	return pc + 3
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
		regHostCall(l, f, fn, s)
		return pc + 1
	}
	cd := fn.Inst.Code.get(fn.Index)
	nc, nsp, rc, rsp := x.nc, x.nsp, x.rc, x.rsp
	nl, _, _ := x.takeNum(cd.nslots)
	nr, _, _ := x.takeRef(cd.rslots)
	regArgs(l, f, s.args, nl, nr)
	x.run(fn, cd, nl, nr, nc, nsp, rc, rsp)
	regRets(l, f, s.rets, nl, nr)
	x.nc, x.nsp, x.rc, x.rsp = nc, nsp, rc, rsp
	return pc + 1
}

// regArgs moves a call's arguments into the callee's frame nl and nr.
func regArgs(l unsafe.Pointer, f *frame, args []regMove, nl []uint64, nr []Ref) {
	for _, m := range args {
		switch m.k {
		case vkNum:
			nl[m.to] = *(*uint64)(unsafe.Add(l, uintptr(m.from)<<3))
		case vkV128:
			p := (*[2]uint64)(unsafe.Add(l, uintptr(m.from)<<3))
			nl[m.to], nl[m.to+1] = p[0], p[1]
		default:
			nr[m.to] = f.r[m.from]
		}
	}
}

// regRets moves a call's results out of the callee's frame nl and nr.
func regRets(l unsafe.Pointer, f *frame, rets []regMove, nl []uint64, nr []Ref) {
	for _, m := range rets {
		switch m.k {
		case vkNum:
			*(*uint64)(unsafe.Add(l, uintptr(m.to)<<3)) = nl[m.from]
		case vkV128:
			p := (*[2]uint64)(unsafe.Add(l, uintptr(m.to)<<3))
			p[0], p[1] = nl[m.from], nl[m.from+1]
		default:
			f.r[m.to] = nr[m.from]
		}
	}
}

// regHostCall calls host function fn from site s.
func regHostCall(l unsafe.Pointer, f *frame, fn *Function, s *regSite) {
	x := f.x
	vals, top := x.takeVals(len(s.args))
	for i, m := range s.args {
		vals[i] = getVal(l, f, m.k, m.from)
	}
	res := x.hostCall(fn, vals)
	x.vals = x.vals[:top]
	for i, m := range s.rets {
		putVal(l, f, m.k, m.to, res[i])
	}
}

func rhReturnCall(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	s := &f.rc.sites[uint32(w>>32)]
	x := f.x
	x.tailArgs = x.tailArgs[:0]
	for _, m := range s.args {
		x.tailArgs = append(x.tailArgs, getVal(l, f, m.k, m.from))
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
		rMov2: rhMov2, rRefMov: rhRefMov, rRefNull: rhRefNull, rConst128: rhConst128, rSIMD: rhSIMD, rFar: rhFar,
	} {
		regTable[op] = h
	}
	regSlowTable[rJmp] = regSlow32
	regSlowTable[rBrNZ] = regSlow32
	regSlowTable[rBrZ] = regSlow32
	regSlowTable[rBrTable] = regSlowBrTable
	regSlowTable[rExt] = rhExtSlow
	regExtTable[xUnreachable] = rhUnreachable
	regExtTable[xMemorySize] = rhMemorySize
	regExtTable[xMemoryGrow] = rhMemoryGrow
	for _, t := range []*[256]regHandler{&regTable, &regSlowTable, &regExtTable, &regExtSlowTable, &regSIMDTable} {
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
