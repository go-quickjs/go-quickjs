//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// The operand lattice: the JIT's operand-type feedback, V8's
// BinaryOperationHint and CompareOperationHint. Each arithmetic,
// comparison and bitwise instruction of a function's register code
// (bytecode's LatticeForm) has a byte in its closure's code, jitRegOps,
// into which the feedback dispatcher's handler of it ORs the kinds of its
// operands (abi.Operands) each time it runs, before plain's handler makes
// it (internal/vm/internal/nativegen, lattice.go); the builder speculates
// what the byte says was met (jitRegFeedback's Operands). Plain's handlers,
// which a runtime without the JIT runs, record nothing, nor does the Go
// table, which counts nothing for the JIT: under QJS_NONATIVE, and where
// the native handlers are not built, no function has a lattice, and the
// builder speculates numbers as it did before there was one.
//
// The lattice is made with the template's first run in the runtime, so
// that every run the feedback dispatcher makes of the function is
// recorded: a site whose byte is none never ran, and the builder leaves it
// to Go, as V8's code deoptimizes softly at a site with no feedback. It is
// the runtime's own, as the call counts are, and only its goroutine writes
// it: the builder reads it there, or from the snapshot a background
// compile is given.

// jitLatticeMake makes cc's lattice for its register code c, at the
// template's first run (regExtend), where the runtime runs the JIT on the
// native handlers, whose feedback dispatcher records it: a byte for each
// instruction, and two past them (jitLatticeRan): the last, which the
// dispatcher's returns count, and the one before it, which a compile sets
// that took the lattice for nothing.
//
//go:noinline
func (r *Runtime) jitLatticeMake(cc *closureCode, c *regCode) {
	if r.jitEnabled && r.runNative && cc.jitRegOps == nil && !c.typed {
		cc.jitRegOps = make([]uint8, len(c.rc.Code)+2)
	}
}

// jitLatticeRan reports whether a site of cl's function whose byte is
// none never ran: the interpreter has run the function to a return
// abi.OperandRuns times since its lattice was made (the lattice's last
// byte, which the feedback dispatcher's returns count), and no compile
// took the lattice for nothing before then (the byte before it,
// jitLatticeUnknown). A compile made before -- at an early call, entered
// at a loop of the first, of a recursion inside its first run -- is made
// with as little feedback as one with no lattice, its sites taken for
// numbers; and its code, which runs them natively, records nothing of
// them in the lattice from then on, so that a site the interpreter never
// ran may since have run natively. V8 compiles only a function whose
// feedback the interpreter has had a budget to fill.
func jitLatticeRan(cl *closure) bool {
	ops := cl.jitRegOps
	return len(ops) >= 2 && ops[len(ops)-1] >= abi.OperandRuns && ops[len(ops)-2] == 0
}

// jitLatticeUnknown marks cl's lattice as taken for nothing by a compile:
// a site whose byte is none may run natively from now on, unrecorded
// (jitLatticeRan).
func jitLatticeUnknown(cl *closure) {
	if ops := cl.jitRegOps; len(ops) >= 2 {
		ops[len(ops)-2] = 1
	}
}

// jitLatticeKind is v's kind, as the feedback dispatcher's handlers find
// it (nativegen's latticeKind).
func jitLatticeKind(v Value) abi.Operands {
	bits := math.Float64bits(v.num)
	switch {
	case v.IsNumber():
		if f := v.num; f >= math.MinInt32 && f <= math.MaxInt32 && f == math.Trunc(f) && bits != 1<<63 {
			return abi.OperandInt32
		}
		return abi.OperandNumber
	case bits >= shortIntBase:
		return abi.OperandBigInt64
	case bits >= heapBigBits:
		return abi.OperandBigInt
	}
	switch Kind(bits & 0xFF) {
	case KindUndefined, KindNull, KindBool:
		return abi.OperandOddball
	case KindString:
		return abi.OperandString
	}
	return abi.OperandOther
}

// jitLatticeOperands are the kinds of the operands of w, a register
// instruction with a lattice, in the window at wb, as its feedback handler
// ORs them; false for an instruction with none.
func (r *Runtime) jitLatticeOperands(w bytecode.RInstr, wb int) (abi.Operands, bool) {
	at := func(n uint16) (abi.Operands, bool) {
		if wb < 0 || wb+int(n) >= len(r.stack) {
			return 0, false
		}
		return jitLatticeKind(r.stack[wb+int(n)]), true
	}
	var x, y abi.Operands
	var ok1, ok2 bool
	switch w.Op().Lattice() {
	case bytecode.LatticeBC:
		x, ok1 = at(w.B())
		y, ok2 = at(w.C())
	case bytecode.LatticeBImm:
		x, ok1 = at(w.B())
		y, ok2 = abi.OperandInt32, true
	case bytecode.LatticeB:
		x, ok1 = at(w.B())
		ok2 = true
	case bytecode.LatticeAB:
		x, ok1 = at(w.A())
		y, ok2 = at(w.B())
	case bytecode.LatticeAImm:
		x, ok1 = at(w.A())
		y, ok2 = abi.OperandInt32, true
	}
	return x | y, ok1 && ok2
}

// jitLatticeWiden ORs into cl's lattice, at register pc rpc of its code c,
// the kinds of the operands that instruction reads in the window at wb,
// and reports whether its byte changed: what a deoptimization at the site
// widens before the interpreter runs the instruction, and what a host exit
// at a site that had met nothing learns. False where the instruction has
// no lattice or cl none.
func (r *Runtime) jitLatticeWiden(cl *closure, c *regCode, wb, rpc int) bool {
	if cl == nil || cl.closureCode == nil || c == nil || cl.reg != c {
		return false
	}
	ops := cl.jitRegOps
	if rpc < 0 || rpc >= len(ops) || rpc >= len(c.rc.Code) {
		return false
	}
	k, ok := r.jitLatticeOperands(c.rc.Code[rpc], wb)
	if !ok {
		return false
	}
	old := ops[rpc]
	ops[rpc] = old | uint8(k)
	return ops[rpc] != old
}

// jitLatticeAt is what cl's lattice holds of register pc rpc of rc, cl's
// register code, and whether it holds anything of it: false where the
// instruction has no lattice, or the function none -- the runtime runs no
// feedback dispatcher -- in which the builder speculates as without one.
func jitLatticeAt(cl *closure, rc *bytecode.RegCode, rpc int) (abi.Operands, bool) {
	if cl == nil || cl.closureCode == nil || cl.reg == nil || cl.reg.rc != rc {
		return 0, false
	}
	ops := cl.jitRegOps
	if rpc < 0 || rpc >= len(ops) || rpc >= len(rc.Code) || rc.Code[rpc].Op().Lattice() == bytecode.LatticeNone {
		return 0, false
	}
	return abi.Operands(ops[rpc]), true
}

// jitLatticeAny reports whether cl's lattice at register pc rpc is the
// top the builder speculates nothing at: what it met is neither numbers
// and oddballs nor BigInts alone, and Go makes the operation (abi's
// Operands.Generic).
func (r *Runtime) jitLatticeAny(cl *closure, rpc int) bool {
	if cl == nil || cl.closureCode == nil || cl.reg == nil {
		return false
	}
	k, ok := jitLatticeAt(cl, cl.reg.rc, rpc)
	return ok && k.Generic()
}
