//go:build amd64 || arm64

package opt

import (
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// interpOperands is the operands of instruction v the interpreter's code
// runs: whether each is a reference, in the order they are on the stack.
func (g *lowering) interpOperands(v *Value) []bool {
	e, ok := wjit.Effect(g.req.Module, uint32(v.Aux2), nil)
	if !ok {
		panic("wasm opt: no exit for " + opcode.Opcode(v.Aux).Name())
	}
	refs := make([]bool, len(e.Params))
	for i, t := range e.Params {
		refs[i] = t.IsRef()
	}
	return refs
}

// stageInterp stores the numeric operands of instruction v the
// interpreter's code runs into the call area, each at its place among the
// instruction's operands, and, for one that takes or leaves a reference,
// tells the engine where its references are (Ctx.RArgs): in their
// positions' reference slots.
func (g *gen) stageInterp(v *Value) {
	e, ok := wjit.Effect(g.req.Module, uint32(v.Aux2), nil)
	if !ok {
		panic("wasm opt: no exit for " + opcode.Opcode(v.Aux).Name())
	}
	refs := e.HasResult() && e.Result.IsRef()
	j := 0
	for i, t := range e.Params {
		if t.IsRef() {
			refs = true
			continue
		}
		a := v.Args[j]
		j++
		g.move(loc{kind: lSlot, slot: int32(g.argBase + 2*i)}, g.locOf(a), a.Type)
	}
	if refs {
		g.storeCtx(offRArgs, g.rargs(v))
	}
}
