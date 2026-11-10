//go:build amd64 || arm64

package opt

import "github.com/go-quickjs/go-quickjs/wasm/internal/opcode"

// goCount is the number of operands of instruction Go runs v, which its
// staging in the call area takes.
func (g *lowering) goCount(v *Value) int {
	if isGCGo(v) {
		n, _ := gcOperandCount(g.req.Module, opcode.Opcode(v.Aux), uint32(v.Aux2), uint32(v.Aux2>>32))
		return n
	}
	return goOperandCount(v)
}

// stageGC stores the numeric operands of GC instruction or throw v, which Go
// runs, into the call area, each at its place among the operands, and tells
// the engine where its references are (Ctx.RArgs): in their positions'
// reference slots, the first operand's position's taking a new object.
func (g *gen) stageGC(v *Value) {
	m := g.req.Module
	op := opcode.Opcode(v.Aux)
	x, y := uint32(v.Aux2), uint32(v.Aux2>>32)
	n, _ := gcOperandCount(m, op, x, y)
	j := 0
	for i := 0; i < n; i++ {
		if gcOperandIsRef(m, op, x, y, i) {
			continue
		}
		a := v.Args[j]
		j++
		g.move(loc{kind: lSlot, slot: int32(g.argBase + 2*i)}, g.locOf(a), a.Type)
	}
	if op != opcode.Rethrow {
		g.storeCtx(offRArgs, g.rargs(v))
	}
}
