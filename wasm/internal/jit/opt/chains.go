package opt

// forwardChains carries what the builder knows within a block -- the
// values of the mutable globals, the last loads and the whole values
// stores wrote (the builder's globalFact and memFact) -- along the chains
// of blocks of one predecessor, in which a block runs only after its
// predecessor has, with nothing between: a read whose value its
// predecessor knows at its end, on the chain back to where the value was
// read or written, becomes that value. The builder forgets at every block's
// start; jump threading then takes Go's code's edges out of its dispatch
// loop straight from a block to the block that goes on, so that a block
// reads back the stack pointer its predecessor wrote to a global, and the
// words it stored into its frame. A call, an instruction Go runs, an
// atomic access or a fence forgets it all, as in the builder, and a store
// what it may overlap (stored). A loop's header, of several predecessors,
// begins knowing nothing: no value is carried around a loop, nor past a
// merge.
//
// It reports whether it made a read a copy. A function whose frame the
// values it keeps live leave too few spill slots is compiled again without
// it (noForward), as without forwardGlobals.
func forwardChains(f *Func) bool {
	if f.noForward {
		return false
	}
	ws := f.ws
	fc := &ws.fc
	fc.reset(f)
	changed := false
	for i, b := range f.Blocks {
		// A block whose one predecessor is the block before it carries
		// what that one knew at its end, as it is: the facts are saved
		// only for the other successors of one predecessor.
		if i == 0 || len(b.Preds) != 1 || b.Preds[0] != f.Blocks[i-1] || f.Blocks[i-1].Kind == BInvoke {
			fc.g, fc.m = fc.g[:0], fc.m[:0]
			if len(b.Preds) == 1 {
				if e := fc.ends[b.Preds[0].ID]; e.saved {
					fc.g = append(fc.g, fc.savedG[e.g0:e.g1]...)
					fc.m = append(fc.m, fc.savedM[e.m0:e.m1]...)
				}
			}
		}
		for _, v := range b.Values {
			switch {
			case v.Op == OpGlobalGet && v.Aux2 != 1:
				if k := fc.global(v.Aux); k != nil && k.val != v {
					v.setCopy(k.val)
					changed = true
					continue
				}
				fc.know(v.Aux, v)
			case v.Op == OpGlobalSet:
				fc.know(v.Aux, resolve(v.Args[1]))
			case v.Op == OpLoad && v.Aux2&memShared == 0:
				addr := resolve(v.Args[0])
				if k := fc.load(addr, v); k != nil {
					v.setCopy(k)
					changed = true
					continue
				}
				fc.remember(memFact{addr: addr, mem: v.Aux2, off: v.Aux, size: v.Size, signed: v.Signed, typ: v.Type, val: v})
			case v.Op == OpStore:
				fc.stored(resolve(v.Args[0]), v.Aux2, v.Aux, v.Size, resolve(v.Args[1]))
			case v.IsCall() || v.Op == OpGoOp || isAtomic(v.Op) || v.Op == OpFence:
				fc.g, fc.m = fc.g[:0], fc.m[:0]
			}
		}
		// Kept for its successors of one predecessor but the next block,
		// if it knows anything.
		if len(fc.g)+len(fc.m) == 0 || b.Kind == BInvoke {
			continue
		}
		for _, s := range b.Succs {
			if len(s.Preds) == 1 && (i+1 == len(f.Blocks) || s != f.Blocks[i+1]) {
				e := &fc.ends[b.ID]
				e.saved = true
				e.g0, e.m0 = int32(len(fc.savedG)), int32(len(fc.savedM))
				fc.savedG = append(fc.savedG, fc.g...)
				fc.savedM = append(fc.savedM, fc.m...)
				e.g1, e.m1 = int32(len(fc.savedG)), int32(len(fc.savedM))
				break
			}
		}
	}
	fc.done()
	f.forwarded = f.forwarded || changed
	return changed
}

// The most facts of each kind a block carries: the oldest go first.
const (
	maxChainGlobals = 16
	maxChainMem     = 32
)

// fcState is forwardChains' state, kept in the workspace: the facts known
// at the walk's point (g, m), and each block's at its end, by block ID,
// where a successor of one predecessor takes them (ends, into savedG and
// savedM).
type fcState struct {
	g      []globalFact
	m      []memFact
	savedG []globalFact
	savedM []memFact
	ends   []fcEnd
}

// fcEnd is where a block's facts at its end are in the saved lists.
type fcEnd struct {
	g0, g1, m0, m1 int32
	saved          bool
}

func (fc *fcState) reset(f *Func) {
	fc.g, fc.m, fc.savedG, fc.savedM = fc.g[:0], fc.m[:0], fc.savedG[:0], fc.savedM[:0]
	fc.ends = grown(fc.ends, int(f.nextBlock))
	clear(fc.ends)
}

// done drops the values the lists hold, for the collector.
func (fc *fcState) done() {
	clear(fc.g[:cap(fc.g)])
	clear(fc.m[:cap(fc.m)])
	clear(fc.savedG[:cap(fc.savedG)])
	clear(fc.savedM[:cap(fc.savedM)])
}

// global is what global g is known to hold, or nil.
func (fc *fcState) global(g uint64) *globalFact {
	for i := range fc.g {
		if uint64(fc.g[i].g) == g {
			return &fc.g[i]
		}
	}
	return nil
}

// know knows that global g holds v.
func (fc *fcState) know(g uint64, v *Value) {
	if k := fc.global(g); k != nil {
		k.val = v
		return
	}
	if len(fc.g) == maxChainGlobals {
		fc.g = append(fc.g[:0], fc.g[1:]...)
	}
	fc.g = append(fc.g, globalFact{g: uint32(g), val: v})
}

// load is the value a known load or store of the bytes load v reads, at
// addr, has as v's type, or nil.
func (fc *fcState) load(addr *Value, v *Value) *Value {
	for _, k := range fc.m {
		if k.addr == addr && k.mem == v.Aux2 && k.off == v.Aux && k.size == v.Size && k.signed == v.Signed && k.typ == v.Type {
			return k.val
		}
	}
	return nil
}

// remember knows k.
func (fc *fcState) remember(k memFact) {
	if len(fc.m) == maxChainMem {
		fc.m = append(fc.m[:0], fc.m[1:]...)
	}
	fc.m = append(fc.m, k)
}

// stored forgets what is known of the bytes a store into memory mx at addr
// plus off may write, and knows what a store of a whole value wrote, as the
// builder's stored does.
func (fc *fcState) stored(addr *Value, mx, off uint64, size uint8, v *Value) {
	keep := fc.m[:0]
	for _, k := range fc.m {
		if disjointAccess(k.addr, k.mem, k.off, k.size, addr, mx, off, size) {
			keep = append(keep, k)
		}
	}
	fc.m = keep
	full := (v.Type == TI32 || v.Type == TF32) && size == 4 || (v.Type == TI64 || v.Type == TF64) && size == 8 ||
		v.Type == TV128 && size == 16
	if full && mx&memShared == 0 {
		fc.remember(memFact{addr: addr, mem: mx, off: off, size: size, typ: v.Type, val: v})
	}
}
