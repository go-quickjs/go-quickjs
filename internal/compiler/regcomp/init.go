package regcomp

import (
	"math/bits"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// trimInits leaves the entry's undefined stores only through the last
// binding that can be read before it is written. Bindings beyond it keep
// their registers, but every path to a read has already supplied a value.
func trimInits(fn *bytecode.Function, rc *bytecode.RegCode) {
	if rc.Inits == 0 || rc.Inits > 64 || len(rc.Code) > 4096 || len(rc.Handlers) != 0 ||
		fn.Generator || fn.Async || fn.HasDirectEval || fn.Debug != nil || rc.LocalReg != nil ||
		fn.UsesArguments || fn.MakesArguments {
		return
	}
	for _, l := range fn.Locals {
		if l.Captured {
			return
		}
	}
	first := regParams + rc.Params
	mask := func(regs []int) uint64 {
		var m uint64
		for _, r := range regs {
			if r >= first && r-first < rc.Inits {
				m |= 1 << uint(r-first)
			}
		}
		return m
	}
	type flow struct {
		use, def, in uint64
		next, to     int
	}
	code := rc.Code
	f := make([]flow, len(code)+1)
	for pc, w := range code {
		reads, writes, ok := regRoles(w)
		if !ok {
			return
		}
		x := &f[pc]
		x.use, x.def = mask(reads), mask(writes)
		switch w.Op() {
		case bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBin, bytecode.RAddI, bytecode.RSubI,
			bytecode.RMulI, bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr,
			bytecode.RUShr, bytecode.RAndI, bytecode.ROrI, bytecode.RXorI, bytecode.RShlI, bytecode.RShrI,
			bytecode.RUShrI, bytecode.RInc, bytecode.RDec, bytecode.RUnary, bytecode.RAddI32,
			bytecode.RSubI32, bytecode.RAddII32:
			// Reusing a binding's old BigInt reads its destination too.
			if w.X()&bytecode.RArithOwnA != 0 {
				x.use |= mask([]int{int(w.A())})
			}
		}
		x.next, x.to = pc+1, len(code)
		switch op := w.Op(); {
		case op == bytecode.RSwitch || op == bytecode.RSwitchTypeof:
			return
		case op == bytecode.RRet || op == bytecode.RRetUndef || op == bytecode.RThrow:
			x.next = len(code)
		case op == bytecode.RJmp:
			x.next = pc + 1 + int(int32(w.BC()))
		case op == bytecode.RJmpF || op == bytecode.RJmpT:
			x.to = pc + 1 + int(int32(w.BC()))
		case regJumps(op):
			x.to = pc + 1 + int(w.SC())
		}
		if x.next < 0 || x.next > len(code) || x.to < 0 || x.to > len(code) {
			return
		}
	}
	// A bounded fixed point keeps deeply nested loops from adding unbounded
	// compile work. Giving up retains the original initialization prefix.
	for range 64 {
		changed := false
		for pc := len(code) - 1; pc >= 0; pc-- {
			x := &f[pc]
			live := x.use | (f[x.next].in|f[x.to].in)&^x.def
			if live != x.in {
				x.in, changed = live, true
			}
		}
		if !changed {
			rc.SkipInit = uint8(rc.Inits - bits.Len64(f[0].in))
			return
		}
	}
}
