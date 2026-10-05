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
	case fn.Generator && fn.Async:
		return false, "an async generator"
	case fn.Generator:
		return false, "a generator"
	case fn.Async:
		return false, "an async function"
	case fn.TopLevel:
		return false, "top-level code"
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
		// buildBlock gave up: an instruction the tier builds, in a place or
		// with operands it does not build it in.
		return false, "the builder gave up on a block"
	}
	return true, ""
}
