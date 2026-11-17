package regcomp

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The locals that hold their values alone: the interpreter and the JIT
// give back a BigInt such a local held when it is assigned the next result
// of arithmetic (the VM's zzbigown.go), which the operators whose results
// such a local takes say (RArithOwnA).

// soleLocals is the locals of the function, of the first 64, that hold
// their values alone: each a binding of its own -- not a parameter, nor
// captured -- whose register the code reads only as an operand of an
// operator that makes a value of its own, in a function that is no
// generator, no async function, has no direct eval and no arguments
// object. An instruction regRoles does not know that names the register
// is taken to read it.
func (g *gen) soleLocals() uint64 {
	fn := g.fn
	if fn.Generator || fn.Async || fn.HasDirectEval || fn.UsesArguments || fn.Debug != nil {
		return 0
	}
	var cand uint64
	for k := fn.ParamCount; k < len(fn.Locals) && k < 64; k++ {
		if !fn.Locals[k].Captured && g.bindingReg(k) >= 0 {
			cand |= 1 << uint(k)
		}
	}
	if cand == 0 {
		return 0
	}
	// The registers of the candidates, by register.
	of := map[int]int{}
	for k := 0; k < 64; k++ {
		if cand&(1<<uint(k)) != 0 {
			of[g.bindingReg(k)] = k
		}
	}
	// A function of an operation past the few a local's value is kept
	// out of so plainly has none: loads, moves, arithmetic, comparisons,
	// branches, globals, calls and returns.
	for _, w := range g.code {
		if !soleOp(w) {
			return 0
		}
	}
	escape := func(r int) {
		if k, ok := of[r]; ok {
			cand &^= 1 << uint(k)
		}
	}
	var labels []bool
	for pc, w := range g.code {
		reads, _, known := regRoles(w)
		if !known {
			for r := range of {
				if mentions(w, r) {
					escape(r)
				}
			}
			continue
		}
		switch w.Op() {
		case bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBin, bytecode.RBitAnd, bytecode.RBitOr,
			bytecode.RBitXor, bytecode.RShl, bytecode.RShr, bytecode.RUShr, bytecode.RAddI, bytecode.RSubI,
			bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI,
			bytecode.RMulI, bytecode.RInc, bytecode.RDec, bytecode.RUnary,
			bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI,
			bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt,
			bytecode.RJGe, bytecode.RJNGe, bytecode.RJEq, bytecode.RJNe, bytecode.RJSEq, bytecode.RJSNe,
			bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI,
			bytecode.RJGeI, bytecode.RJNGeI, bytecode.RJEqI, bytecode.RJNeI, bytecode.RJSEqI, bytecode.RJSNeI,
			bytecode.RAppend, bytecode.RStrFreeze, bytecode.RAddI32, bytecode.RSubI32, bytecode.RAddII32:
			// An operand, read to make a value of the operator's own; an
			// append's, whose string is its binding's; str_freeze's,
			// which leaves the value where it is.
			continue
		case bytecode.RRet:
			// The value the instruction before made, returned, as the
			// stack form's return of it took it from the stack: the frame
			// ends, and nothing is assigned the binding again.
			if labels == nil {
				labels = g.labels()
			}
			if pc > 0 && !labels[pc] {
				if _, writes, ok := regRoles(g.code[pc-1]); ok && slices.Contains(writes, int(w.A())) {
					continue
				}
			}
		}
		for _, r := range reads {
			escape(r)
		}
	}
	return cand
}

// markSole gives each operator compiled into a binding's register (ownCands)
// RArithOwnA where the binding holds its values alone (soleLocals).
func (g *gen) markSole() {
	if len(g.ownCands) == 0 {
		return
	}
	sole := g.soleLocals()
	if sole == 0 {
		return
	}
	for _, pc := range g.ownCands {
		w := g.code[pc]
		if k := g.bindingOf(int(w.A())); k >= 0 && k < 64 && sole&(1<<uint(k)) != 0 {
			g.code[pc] = bytecode.MakeR(w.Op(), w.X()|bytecode.RArithOwnA, w.A(), w.B(), w.C())
		}
	}
}

// soleOp reports whether w is of the operations soleLocals reads the code
// of: an operator of its own operands' values -- an arithmetic one, a
// comparison, negation, not, a bitwise not -- or a load, a move, a branch,
// a global's read or write, a method's read and a call, a return.
func soleOp(w bytecode.RInstr) bool {
	switch w.Op() {
	case bytecode.RUnary:
		switch bytecode.Op(w.X()) {
		case bytecode.OpNeg, bytecode.OpNot, bytecode.OpBitNot:
			return true
		}
		return false
	case bytecode.RBin:
		switch bytecode.Op(w.X()) {
		case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod,
			bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr,
			bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe,
			bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe:
			return true
		}
		return false
	case bytecode.RMove, bytecode.RLoadInt, bytecode.RLoadConst, bytecode.RLoadUndef, bytecode.RLoadNull,
		bytecode.RLoadTrue, bytecode.RLoadFalse, bytecode.RLoadEmpty, bytecode.RLoadUninit,
		bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor,
		bytecode.RShl, bytecode.RShr, bytecode.RUShr, bytecode.RAddI, bytecode.RSubI, bytecode.RAndI,
		bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI, bytecode.RUShrI, bytecode.RMulI,
		bytecode.RInc, bytecode.RDec, bytecode.RAddI32, bytecode.RSubI32, bytecode.RAddII32,
		bytecode.RJmp, bytecode.RJmpF, bytecode.RJmpT,
		bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt,
		bytecode.RJGe, bytecode.RJNGe, bytecode.RJEq, bytecode.RJNe, bytecode.RJSEq, bytecode.RJSNe,
		bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI,
		bytecode.RJGeI, bytecode.RJNGeI, bytecode.RJEqI, bytecode.RJNeI, bytecode.RJSEqI, bytecode.RJSNeI,
		bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI,
		bytecode.RGetGlobal, bytecode.RSetGlobal, bytecode.RGetMethodW, bytecode.RCallW, bytecode.RCallMW,
		bytecode.RRet, bytecode.RRetUndef, bytecode.RStrFreeze, bytecode.RAppend:
		return true
	}
	return false
}
