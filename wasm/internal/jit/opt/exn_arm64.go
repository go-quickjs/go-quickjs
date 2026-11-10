package opt

// padEntry begins landing pad b, which Go enters with only the context in
// its register: it loads what an exit's resumption loads, and every value
// its site's call left in its slot, as after the call.
func (g *gen) padEntry(b *Block) {
	g.mark("landing pad of %v", b.Pad)
	g.loadKept()
	g.reload(b.Pad)
}

// exnVal reads number Aux of the exception a landing pad takes, which Go
// left in the call area.
func (g *gen) exnVal(v *Value) {
	dst := g.locOf(v)
	g.move(dst, loc{kind: lSlot, slot: int32(g.argBase + 2*int(v.Aux))}, v.Type)
	if dst.kind != lSlot {
		g.done(v, dst.r)
	}
}
