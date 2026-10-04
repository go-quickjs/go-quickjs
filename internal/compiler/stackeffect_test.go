package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestStackEffectOfPushes pins that the instructions that push a value
// without popping one count it, which is what a frame's operand stack is
// sized by. A RegExp literal, a tagged template's strings, a private name
// and a using declaration's capability were once counted as pushing
// nothing, so a function whose deepest point held one had a stack a slot
// short: the interpreter wrote past its frame, into the next call's, and
// the tree tier, which slices a frame to its size, failed outright.
func TestStackEffectOfPushes(t *testing.T) {
	for _, op := range []bytecode.Op{
		bytecode.OpNewRegExp, bytecode.OpTemplateObject,
		bytecode.OpPrivateName, bytecode.OpNewDisposeCapability,
		bytecode.OpPushConst, bytecode.OpClosure, bytecode.OpNewObject,
	} {
		if n := stackEffect(op, 0, 0); n != 1 {
			t.Errorf("%s pushes one value; its stack effect is %d", op, n)
		}
	}
}
