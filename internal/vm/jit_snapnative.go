//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math/bits"
	"slices"
	"sync/atomic"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// The methods of register code's feedback that answer from what its code
// learned at its exits -- the lists of a function's learning -- and from
// what the interpreter marks of its code: whether an operator met
// BigInts, what an operand met, how often a call is made. A snapshot
// derives them on the worker (jit_snapderive.go): its gather, once a
// snapshot, takes the learning's lists by their headers, which only grow
// or are made anew (TestJITLearnedListsPersist), each an O(1) copy that
// is the list as it was then; and copies the interpreter's marks, which it
// writes in place, whole. The feedback answers from the same record of the
// live state, made at its first question (native): one implementation, the
// derivation the feedback's own.

// jitNativeData is what the learning's methods answer from: the function
// (fn), its register code lowered (p, rc), whether the feedback is a callee's
// (root), what its code learned (learned, the lists), the interpreter's
// marks, and, for a callee, whether its own code ran enough to tell a call
// seldom made (codeRan).
type jitNativeData struct {
	p                                                *ir.Program
	rc                                               *bytecode.RegCode
	fn                                               *bytecode.Function
	gathered                                         bool
	root                                             bool
	learned                                          bool
	codeRan                                          bool
	failed, bigints, bigwide, notInline, hostsLeft   []int32
	typedReads, stringReads, keyedReads, keyedWrites []int32
	made                                             []uint64
	// big, wide and heap are the operators' BigInt marks (regBigSeen), a
	// bit by register pc; marked, whether the code has them.
	big, wide, heap []uint32
	marked          bool
	// unknown are the register pcs whose operand met nothing before the
	// function ran to its end (jitNativeGather), of cl; markUnknown marks
	// the lattice to be marked taken for nothing at the finish, and
	// unknownUnasked one no PC of the program asks Operands at an unknown
	// of (jitNativeBind).
	cl                          *closure
	unknown                     []int32
	markUnknown, unknownUnasked bool
	// ops is the operands' lattice (jitLatticeAt), lattice whether the
	// code has one; counts the calls' counts (Frequency), nil where it
	// answers nothing.
	ops     []uint8
	lattice bool
	counts  []uint64
}

// jitNativeMethods are the methods answered from a jitNativeData.
const jitNativeMethods uint64 = 1<<snapGeneric | 1<<snapBigInt | 1<<snapBigIntWide | 1<<snapBigIntFailed |
	1<<snapStringRead | 1<<snapTypedRead | 1<<snapTypedBigRead | 1<<snapKeyedRead | 1<<snapKeyedWrite |
	1<<snapCallShape | 1<<snapFrequency | 1<<snapOperands | 1<<snapInlineExit |
	1<<snapBigIntSole | 1<<snapPureBody | 1<<snapGoCalls

func init() {
	jitSnapDerived |= jitNativeMethods
	jitSnapFuncGatherers = append(jitSnapFuncGatherers, jitNativeGather)
	jitSnapBinders = append(jitSnapBinders, jitNativeBind)
	jitSnapFinishers = append(jitSnapFinishers, jitNativeFinish)
	jitSnapDerivers[jitDeriveCopies] = append(jitSnapDerivers[jitDeriveCopies], jitNativeDerive)
}

// native is f's record of its function's learning and marks, the live
// state's: made at its first question, as nothing it reads changes while
// a compile asks.
func (f *jitRegFeedback) native() *jitNativeData {
	if f.nat == nil {
		f.nat = new(jitNativeData)
		f.gatherNative(f.nat, false)
	}
	return f.nat
}

// gatherNative fills d with f's learning and marks: the lists by their
// headers, and, where copies says, the marks the interpreter writes in
// place copied, for a worker to read.
func (f *jitRegFeedback) gatherNative(d *jitNativeData, copies bool) {
	fb := f.fb
	*d = jitNativeData{p: f.p, rc: f.rc, fn: fb.fn, gathered: true, root: fb.root != nil}
	if l := fb.e; l != nil {
		d.learned = true
		d.failed, d.bigints, d.bigwide, d.notInline = l.failed, l.bigints, l.bigwide, l.notInline
		d.typedReads, d.stringReads, d.keyedReads, d.keyedWrites = l.typedReads, l.stringReads, l.keyedReads, l.keyedWrites
		d.hostsLeft, d.made = l.ssaHostsLeft, l.regCallsMade
		if d.root {
			d.codeRan = fb.r.jitRegCodeRan(fb.cl, l)
		}
	}
	cl := fb.cl
	if cl == nil {
		return
	}
	if fn := fb.fn; cl.closureCode != nil && f.rc.Leaf == bytecode.LeafNone && !fn.Generator && !fn.Async {
		if c := cl.jitRegCounts; len(c) != 0 && c[0] >= uint64(jitCountsEnough) {
			d.counts = c
			if copies {
				d.counts = slices.Clone(c)
			}
		}
	}
	if cl.reg == nil || cl.reg.rc != f.rc {
		return
	}
	if s := cl.reg.bigSeen.Load(); s != nil {
		d.marked = true
		d.big, d.wide, d.heap = jitBigMarkWords(s.bigs), jitBigMarkWords(s.wide), jitBigMarkWords(s.heap)
	}
	if cl.closureCode != nil {
		d.ops, d.lattice = cl.jitRegOps, true
		if copies {
			d.ops = slices.Clone(d.ops)
		}
	}
}

// jitBigMarkWords is a BigInt mark's words, as they are now.
func jitBigMarkWords(w []atomic.Uint32) []uint32 {
	out := make([]uint32, len(w))
	for i := range w {
		out[i] = w[i].Load()
	}
	return out
}

// jitNativeGather is the family's gather of s, fb's snapshot: what the
// worker derives the learning's methods from, of the register code alone
// (jitSnapBinders). An operand that met nothing before the function ran
// to its end marks the lattice taken for nothing (jitLatticeUnknown), as
// the feedback's question of a PC lowered from it would have: the gather
// finds the register pcs that may (unknown), and the binding marks it
// where a PC of the program asks Operands at one (jitNativeBind).
func jitNativeGather(s *jitSnapshot, fb *jitRegFeedback) {
	d := &s.d.native
	fb.gatherNative(d, true)
	if !d.lattice || jitLatticeRanOps(d.ops) {
		return
	}
	d.cl = fb.fb.cl
	for rpc := range d.rc.Code {
		if _, _, unknown := d.operandsAt(rpc); unknown {
			d.unknown = append(d.unknown, int32(rpc))
		}
	}
	if len(d.unknown) != 0 && fb.p != nil {
		// Lowered already: marked here, before the gathers after this one,
		// as the asking marked it (jitNativeBind's, at once).
		jitNativeMark(d, fb.p)
	}
}

// jitNativeMark marks d's closure's lattice taken for nothing where a PC
// of p asks Operands at one of d's unknown register pcs, and no more is
// to be marked of it.
func jitNativeMark(d *jitNativeData, p *ir.Program) {
	for pc := range p.Code {
		if jitSnapAsks[p.Code[pc].Op]&(1<<snapOperands) != 0 && slices.Contains(d.unknown, int32(p.Origin[pc])) {
			jitLatticeUnknown(d.cl)
			d.unknown = nil
			return
		}
	}
	d.unknownUnasked = true
	d.unknown = nil
}

// jitNativeBind marks the lattice of s's closure taken for nothing
// (jitLatticeUnknown) where a PC of its program asks Operands at a
// register pc whose operand met nothing (jitNativeGather's unknown), as
// the asking's question there did: at once where the program is bound at
// the gather, on the runtime's goroutine; else at the compile's finish
// (jitNativeFinish), the interpreter's lattice no worker's to write.
func jitNativeBind(s *jitSnapshot) {
	d := &s.d.native
	if len(d.unknown) == 0 {
		return
	}
	for pc, op := range s.ops {
		if jitSnapAsks[op]&(1<<snapOperands) != 0 && slices.Contains(d.unknown, int32(s.p.Origin[pc])) {
			d.markUnknown = true
			break
		}
	}
	d.unknownUnasked = !d.markUnknown
	if d.markUnknown && !s.root.d.deriving {
		jitLatticeUnknown(d.cl)
		d.markUnknown = false
	}
}

// jitNativeFinish marks the lattice a worker's binding found taken for
// nothing (jitNativeBind), at the compile's finish.
func jitNativeFinish(s *jitSnapshot) {
	if d := &s.d.native; d.markUnknown {
		jitLatticeUnknown(d.cl)
		d.markUnknown = false
	}
}

// append to no list.
func jitNativeDerive(s *jitSnapshot) {
	d := &s.d.native
	if !d.gathered {
		return
	}
	a := &s.a
	// The questions of no argument, of the code alone.
	{
		r0, r1 := d.bigIntSole()
		s.heard(snapBigIntSole, 0, true, r0, r1)
		a.BigIntSole.put(0, 0, true, jitSnapAnsBigIntSole{r0, r1})
		p0 := d.pureBody()
		s.heard(snapPureBody, 0, true, p0)
		a.PureBody.put(0, 0, true, jitSnapAnsPureBody{p0})
		g0 := d.goCalls()
		s.heard(snapGoCalls, 0, true, g0)
		a.GoCalls.put(0, 0, true, jitSnapAnsGoCalls{g0})
	}
	for pc, op := range s.ops {
		asks := jitSnapAsks[op] & jitNativeMethods
		if asks == 0 {
			continue
		}
		at := int64(pc)
		if asks&(1<<snapGeneric) != 0 {
			r0 := d.generic(pc)
			s.heard(snapGeneric, at, r0, r0)
			if r0 {
				a.Generic.reserve(int(s.room[snapGeneric]))
				a.Generic.put(at, 0, true, jitSnapAnsGeneric{r0})
			}
		}
		if asks&(1<<snapBigInt) != 0 {
			r0 := d.bigInt(pc)
			s.heard(snapBigInt, at, r0, r0)
			if r0 {
				a.BigInt.reserve(int(s.room[snapBigInt]))
				a.BigInt.put(at, 0, true, jitSnapAnsBigInt{r0})
			}
		}
		if asks&(1<<snapBigIntWide) != 0 {
			r0 := d.bigIntWide(pc)
			s.heard(snapBigIntWide, at, r0, r0)
			if r0 {
				a.BigIntWide.reserve(int(s.room[snapBigIntWide]))
				a.BigIntWide.put(at, 0, true, jitSnapAnsBigIntWide{r0})
			}
		}
		if asks&(1<<snapBigIntFailed) != 0 {
			r0 := d.bigIntFailed(pc)
			s.heard(snapBigIntFailed, at, r0, r0)
			if r0 {
				a.BigIntFailed.reserve(int(s.room[snapBigIntFailed]))
				a.BigIntFailed.put(at, 0, true, jitSnapAnsBigIntFailed{r0})
			}
		}
		if asks&(1<<snapStringRead) != 0 {
			r0 := d.elemIn(pc, ir.ArrayRead, d.stringReads)
			s.heard(snapStringRead, at, r0, r0)
			if r0 {
				a.StringRead.reserve(int(s.room[snapStringRead]))
				a.StringRead.put(at, 0, true, jitSnapAnsStringRead{r0})
			}
		}
		if asks&(1<<snapTypedRead) != 0 {
			r0 := d.elemIn(pc, ir.ArrayRead, d.typedReads)
			s.heard(snapTypedRead, at, r0, r0)
			if r0 {
				a.TypedRead.reserve(int(s.room[snapTypedRead]))
				a.TypedRead.put(at, 0, true, jitSnapAnsTypedRead{r0})
			}
		}
		if asks&(1<<snapTypedBigRead) != 0 {
			r0, r1 := d.typedBigRead(pc)
			s.heard(snapTypedBigRead, at, r1, r0, r1)
			if r1 {
				a.TypedBigRead.reserve(int(s.room[snapTypedBigRead]))
				a.TypedBigRead.put(at, 0, true, jitSnapAnsTypedBigRead{r0, r1})
			}
		}
		if asks&(1<<snapKeyedRead) != 0 {
			r0 := d.elemIn(pc, ir.ArrayRead, d.keyedReads)
			s.heard(snapKeyedRead, at, r0, r0)
			if r0 {
				a.KeyedRead.reserve(int(s.room[snapKeyedRead]))
				a.KeyedRead.put(at, 0, true, jitSnapAnsKeyedRead{r0})
			}
		}
		if asks&(1<<snapKeyedWrite) != 0 {
			r0 := d.elemIn(pc, ir.ArrayWrite, d.keyedWrites)
			s.heard(snapKeyedWrite, at, r0, r0)
			if r0 {
				a.KeyedWrite.reserve(int(s.room[snapKeyedWrite]))
				a.KeyedWrite.put(at, 0, true, jitSnapAnsKeyedWrite{r0})
			}
		}
		if asks&(1<<snapCallShape) != 0 {
			r0, r1, r2 := d.callShape(pc)
			s.heard(snapCallShape, at, r2, r0, r1, r2)
			if r2 {
				a.CallShape.reserve(int(s.room[snapCallShape]))
				a.CallShape.put(at, 0, true, jitSnapAnsCallShape{r0, r1, r2})
			}
		}
		if asks&(1<<snapFrequency) != 0 {
			r0, r1 := d.frequency(pc)
			s.heard(snapFrequency, at, r1, r0, r1)
			if r1 {
				a.Frequency.reserve(int(s.room[snapFrequency]))
				a.Frequency.put(at, 0, true, jitSnapAnsFrequency{r0, r1})
			}
		}
		if asks&(1<<snapOperands) != 0 {
			r0, r1, _ := d.operands(pc)
			s.heard(snapOperands, at, r1, r0, r1)
			if r1 {
				a.Operands.reserve(int(s.room[snapOperands]))
				a.Operands.put(at, 0, true, jitSnapAnsOperands{r0, r1})
			}
		}
		if asks&(1<<snapInlineExit) != 0 {
			r0 := d.inlineExit(pc)
			s.heard(snapInlineExit, at, r0, r0)
			if r0 {
				a.InlineExit.reserve(int(s.room[snapInlineExit]))
				a.InlineExit.put(at, 0, true, jitSnapAnsInlineExit{r0})
			}
		}
	}
}

// rpc is the register pc of the program's instruction pc, false past it.
func (d *jitNativeData) rpc(pc int) (int, bool) {
	if pc < 0 || pc >= len(d.p.Origin) {
		return 0, false
	}
	return int(d.p.Origin[pc]), true
}

// bigSeen is regBigSeen's answer of the marks d took.
func (d *jitNativeData) bigSeen(rpc int) (big, wide, heap bool) {
	if !d.marked || rpc < 0 || rpc>>5 >= len(d.big) {
		return false, false, false
	}
	bit := uint32(1) << (rpc & 31)
	heap = d.heap[rpc>>5]&bit != 0
	wide = d.wide[rpc>>5]&bit != 0
	return heap || wide || d.big[rpc>>5]&bit != 0, wide, heap
}

// generic is Generic's answer.
func (d *jitNativeData) generic(pc int) bool {
	rpc, ok := d.rpc(pc)
	if !d.learned || !ok {
		return false
	}
	if w, _, ok := jitRegAt(d.p, d.rc, pc); ok && w.Op() == bytecode.RLit {
		// Its expansion, the call's last, which a read built generic
		// would commit before (regLitFed): fed no more instead.
		return false
	}
	if slices.Contains(d.failed, int32(rpc)) {
		return true
	}
	// Arithmetic the interpreter saw meet or make a BigInt on the heap,
	// which Go makes from native code (ssa.GoArith).
	_, _, heap := d.bigSeen(rpc)
	return heap
}

// bigInt is BigInt's answer.
func (d *jitNativeData) bigInt(pc int) bool {
	rpc, ok := d.rpc(pc)
	if !ok {
		return false
	}
	if d.learned && slices.Contains(d.bigints, int32(rpc)) {
		return true
	}
	big, _, _ := d.bigSeen(rpc)
	return big
}

// bigIntWide is BigIntWide's answer.
func (d *jitNativeData) bigIntWide(pc int) bool {
	rpc, ok := d.rpc(pc)
	if !ok {
		return false
	}
	if d.learned && slices.Contains(d.bigwide, int32(rpc)) {
		return true
	}
	_, wide, _ := d.bigSeen(rpc)
	return wide
}

// bigIntFailed is BigIntFailed's answer.
func (d *jitNativeData) bigIntFailed(pc int) bool {
	rpc, ok := d.rpc(pc)
	return d.learned && ok && slices.Contains(d.failed, int32(rpc))
}

// elemPC is the register pc of the program's instruction pc, an op, where
// the code learned (regElemPC).
func (d *jitNativeData) elemPC(pc int, op ir.Op) (int32, bool) {
	if !d.learned || pc < 0 || pc >= len(d.p.Code) || d.p.Code[pc].Op != op {
		return 0, false
	}
	return int32(d.p.Origin[pc]), true
}

// elemIn reports whether the element access at the program's instruction
// pc, an op, is among list, what the code learned of such accesses.
func (d *jitNativeData) elemIn(pc int, op ir.Op, list []int32) bool {
	rpc, ok := d.elemPC(pc, op)
	return ok && slices.Contains(list, rpc)
}

// typedBigRead is TypedBigRead's answer.
func (d *jitNativeData) typedBigRead(pc int) (unsigned, ok bool) {
	rpc, ok := d.elemPC(pc, ir.ArrayRead)
	if !ok {
		return false, false
	}
	m := jitBigReadAt(d.typedReads, int(rpc))
	return m&jitBigReadUnsigned != 0, m != 0
}

// callShape is CallShape's answer.
func (d *jitNativeData) callShape(pc int) (int, bool, bool) {
	w, _, ok := jitRegAt(d.p, d.rc, pc)
	if !ok {
		return 0, false, false
	}
	rpc := int(d.p.Origin[pc])
	if rpc < len(d.rc.Marks) && d.rc.Marks[rpc]&bytecode.RMarkTail != 0 {
		return 0, false, false
	}
	if !d.root && d.learned && slices.Contains(d.notInline, int32(rpc)) {
		// Its inlining left native code too often (jitInlineLeft).
		return 0, false, false
	}
	switch w.Op() {
	case bytecode.RCallW:
		return int(w.X()), false, true
	case bytecode.RCallMW:
		return int(w.X()), true, true
	}
	return 0, false, false
}

// frequency is Frequency's answer.
func (d *jitNativeData) frequency(pc int) (float64, bool) {
	counts := d.counts
	if len(counts) == 0 {
		return 0, false
	}
	w, op, ok := jitRegAt(d.p, d.rc, pc)
	if !ok || op != ir.Host || !d.p.Code[pc].Window {
		return 0, false
	}
	switch w.Op() {
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RCallWT, bytecode.RCallMWT:
	default:
		return 0, false
	}
	if 1+int(w.C()) >= len(counts) {
		return 0, false
	}
	return float64(counts[1+int(w.C())]) / float64(counts[0]), true
}

// operands is Operands' answer, and whether its asking marks the lattice
// taken for nothing (jitLatticeUnknown): met nothing, before the function
// ran to its end.
func (d *jitNativeData) operands(pc int) (k ssa.Operands, ok, unknown bool) {
	rpc, in := d.rpc(pc)
	if !in {
		return 0, false, false
	}
	return d.operandsAt(rpc)
}

// operandsAt is operands' of register pc rpc.
func (d *jitNativeData) operandsAt(rpc int) (k ssa.Operands, ok, unknown bool) {
	if d.lattice && rpc < len(d.ops) && rpc < len(d.rc.Code) && d.rc.Code[rpc].Op().Lattice() != bytecode.LatticeNone {
		k, ok = abi.Operands(d.ops[rpc]), true
	}
	if ok && d.rc.Leaf == bytecode.LeafPure {
		k |= abi.OperandNumber
	}
	if ok && k == 0 && !jitLatticeRanOps(d.ops) {
		// Met nothing, but before the function ran to its end: known no
		// more than without a lattice, and so from now on.
		return 0, false, true
	}
	return k, ok, false
}

// inlineExit is InlineExit's answer.
func (d *jitNativeData) inlineExit(pc int) bool {
	w, _, ok := jitRegAt(d.p, d.rc, pc)
	if !ok || !d.root {
		return false
	}
	rpc := int(d.p.Origin[pc])
	made := d.learned && jitRegCallMadeIn(d.made, rpc)
	switch jitRegCallOp(w.Op()) {
	case bytecode.RThrow, bytecode.RThrowRef:
		return true
	case bytecode.RCallW, bytecode.RCallMW, bytecode.RNewW:
		next := rpc + 1
		if next < len(d.rc.Code) && d.rc.Code[next].Op() == bytecode.RThrow && d.rc.Code[next].A() == w.B() {
			// What it makes is thrown: as seldom as the throw, unless its
			// own code has been seen to make it, a throw taken often.
			return !made
		}
		// A call its own code leaves to Go (HostsLeft) and has never been
		// seen to make in runs enough to tell (jitRegCodeRan), as V8's
		// code deoptimizes at a call that has no feedback.
		return d.learned && !made && slices.Contains(d.hostsLeft, int32(rpc)) && d.codeRan
	}
	return false
}

// bigIntSole is BigIntSole's answer: of the code alone.
func (d *jitNativeData) bigIntSole() (uint64, bool) {
	fn := d.fn
	if bits.UintSize != 64 || fn.Generator || fn.Async {
		return 0, false
	}
	return regSoleRegisters(fn, d.rc, d.p), true
}

// pureBody is PureBody's answer: of the code alone.
func (d *jitNativeData) pureBody() bool {
	fn := d.fn
	return fn != nil && d.rc.Leaf == bytecode.LeafPure && !fn.Generator && !fn.Async
}

// goCalls is GoCalls' answer: native code made from register code calls Go.
func (d *jitNativeData) goCalls() bool { return true }
