package opt

// stageGo stores the numeric operands of instruction Go runs v into the
// call area, each at its place among the instruction's operands, and, for
// one with a reference operand, tells the engine where the references are
// (Ctx.RArgs): in their positions' reference slots.
func (g *gen) stageGo(v *Value) {
	if isGCGo(v) {
		g.stageGC(v)
		return
	}
	for i, x := range v.Args {
		g.move(loc{kind: lSlot, slot: int32(g.argBase + 2*goArgIndex(v, i))}, g.locOf(x), x.Type)
	}
	if goRefs(v) {
		g.storeCtx(offRArgs, g.rargs(v))
	}
}
