//go:build amd64 || arm64

package opt

import (
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// imms are the immediates instruction Go runs v passes to the engine's
// exit: goImms', but a rethrow's, which is the reference slot of the
// exception its try's catch took, past the frame's own (CaughtSlot).
func (g *lowering) imms(v *Value) (x, y uint64) {
	x, y = goImms(v)
	if opcode.Opcode(v.Aux) == opcode.Rethrow {
		x += uint64(g.req.CaughtSlot)
	}
	return x, y
}

// siteGap is, for landing pad b, its site's block and the index, as the
// allocator numbers its instructions, of the one after the site's call,
// its block's end: the pad makes the moves of split values there, as the
// code after the call does.
func (g *lowering) siteGap(b *Block) (int, int) {
	p := b.Preds[0]
	n := 0
	for _, v := range p.Values {
		if v.Op != OpConst && v.Op != OpResult && !g.fused[v.ID] {
			n++
		}
	}
	return int(g.index[p.ID]), n
}

// handlers returns the records of the sites' handlers for Go, each site's
// innermost first over its instruction alone, their pads' offsets by off
// and their exceptions' numbers in the call area from vals.
func (f *Func) handlers(off func(*Block) int, vals int, caughtSlot int) []wjit.Handler {
	var out []wjit.Handler
	for _, s := range f.sites {
		if s.b.dead || s.v.Block != s.b {
			continue
		}
		for _, t := range s.chain {
			h := wjit.Handler{Start: s.v.At, End: s.v.At + 1, Index: t.index, Delegate: t.delegate, Target: t.target}
			for _, sp := range s.pads {
				if sp.t != t {
					continue
				}
				cl := &t.clauses[sp.clause]
				caught := -1
				if t.legacy {
					caught = caughtSlot + t.caught
				}
				base := 0
				if !cl.ret {
					base = cl.target.base
				}
				h.Clauses = append(h.Clauses, wjit.Clause{Tag: cl.tag, All: cl.all, Ref: cl.ref, Base: base,
					Vals: vals, Pad: off(sp.pad), Caught: caught})
			}
			if len(h.Clauses) > 0 || h.Delegate {
				out = append(out, h)
			}
		}
	}
	return out
}
