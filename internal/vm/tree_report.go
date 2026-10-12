package vm

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TreeReport says whether the tree tier builds fn and, where it does not,
// why: the first thing about the function the tier does not take. It is for
// tools that show what the engine makes of code (internal/cmd/disasm); the
// engine never calls it, so the linker leaves it out of the engine's own
// binaries and it moves none of this package's code.
func TreeReport(fn *bytecode.Function) (built bool, why string) {
	switch {
	case !treeTier.Load():
		return false, "the tree tier is off (QJS_NOTREE)"
	case fn.HasDirectEval:
		return false, "it calls eval directly"
	case fn.MaxStack > 64:
		return false, fmt.Sprintf("its operand stack is %d deep, past 64", fn.MaxStack)
	}
	for pc, in := range fn.Code {
		if !treeBuilds(in.Op) {
			return false, fmt.Sprintf("%s at %d", in.Op, pc)
		}
	}
	if buildTree(fn) == nil {
		if fn.TopLevel && !jumpsBack(fn) {
			return false, "top-level code without a loop"
		}
		// buildBlock gave up: an instruction the tier builds, in a place or
		// with operands it does not build it in.
		return false, "the builder gave up on a block"
	}
	return true, ""
}

// jumpsBack reports whether fn has a loop, a jump to an instruction no later
// than itself, which buildTree asks of top-level code.
func jumpsBack(fn *bytecode.Function) bool {
	for pc, in := range fn.Code {
		switch in.Op {
		case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse,
			bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep, bytecode.OpJumpIfNullish, bytecode.OpJumpIfNotNullish,
			bytecode.OpIterNextOrJump, bytecode.OpIterResultOrJump, bytecode.OpIterUnpackDelegate:
			if int(in.A) <= pc {
				return true
			}
		}
	}
	return false
}
