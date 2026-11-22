package ssa

import "fmt"

// Check verifies f's structural invariants, which every pass must keep:
// phis lead their blocks with one argument per predecessor; every guard has a
// frame state; every value used is defined in f; an exit and a loop header
// have a frame state, as has an entry block for its guards; a block's
// successors match its kind; and, once shadows are made (Finish), a phi of
// two slots' values has one.
func Check(f *Func) error {
	defined := map[*Value]bool{}
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			defined[v] = true
		}
	}
	use := func(where string, v *Value) error {
		if v == nil || !defined[v] {
			return fmt.Errorf("ssa: %s uses an undefined value %v", where, v)
		}
		return nil
	}
	state := func(where string, s *FrameState) error {
		if s == nil {
			return fmt.Errorf("ssa: %s has no frame state", where)
		}
		for _, v := range s.All() {
			if err := use(where+"'s state", v); err != nil {
				return err
			}
		}
		if !s.checkSlots() {
			// Its nodes: an entry for each bit, none past its slots.
			return fmt.Errorf("ssa: %s's state's slots are malformed", where)
		}
		return nil
	}
	for _, b := range f.Blocks {
		where := fmt.Sprintf("b%d", b.ID)
		inPhis := true
		for _, v := range b.Values {
			w := fmt.Sprintf("%s %v (%v)", where, v, v.Op)
			if v.Block != b {
				return fmt.Errorf("ssa: %s records block b%d", w, v.Block.ID)
			}
			if v.Op == OpPhi {
				if !inPhis {
					return fmt.Errorf("ssa: %s follows other values", w)
				}
				if len(v.Args) != len(b.Preds) {
					return fmt.Errorf("ssa: %s has %d arguments for %d predecessors", w, len(v.Args), len(b.Preds))
				}
			} else {
				inPhis = false
			}
			for _, a := range v.Args {
				if err := use(w, a); err != nil {
					return err
				}
			}
			if v.Op.isGuard() {
				if err := state(w, v.State); err != nil {
					return err
				}
			}
		}
		switch b.Kind {
		case BlockPlain:
			if len(b.Succs) != 1 {
				return fmt.Errorf("ssa: %s is plain with %d successors", where, len(b.Succs))
			}
		case BlockIf:
			if len(b.Succs) != 2 || b.Control == nil || b.Control.Type != Bool {
				return fmt.Errorf("ssa: %s is a conditional without two successors and a bool", where)
			}
		case BlockReturn:
			if b.Control == nil || b.Control.Type != Tagged {
				return fmt.Errorf("ssa: %s returns without a tagged value", where)
			}
		case BlockExit:
			if err := state(where+" exit", b.State); err != nil {
				return err
			}
		}
		if b.Control != nil {
			if err := use(where+" control", b.Control); err != nil {
				return err
			}
		}
		if b.LoopHeader || b.PC < 0 {
			if err := state(where+" header", b.Header); err != nil {
				return err
			}
		}
		if len(b.Backedge) != len(b.Preds) {
			return fmt.Errorf("ssa: %s has %d back-edge marks for %d predecessors", where, len(b.Backedge), len(b.Preds))
		}
	}
	if err := checkDominance(f); err != nil {
		return err
	}
	origins := Origins(f)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			o, ok := origins.Of(v)
			if !ok {
				continue
			}
			if s := v.Shadow; o == OriginAmbiguous && f.shadowed && (s == nil || s.Op != OpPhi || s.Type != Source || s.Block != v.Block) {
				return fmt.Errorf("ssa: b%d %v merges two slots' values with no shadow", v.Block.ID, v)
			}
			if o == OriginHeap && (v.Shadow == nil || v.Shadow != v.Args[0]) {
				return fmt.Errorf("ssa: b%d %v is loaded from a cell it does not name", v.Block.ID, v)
			}
		}
	}
	return nil
}

// checkDominance verifies that f is strict: each value is made where it
// dominates every use of it -- an operand or a shadow before its user in
// their block, or in a block dominating the user's; a phi's operand by the
// end of the predecessor it comes from; a frame state's slot where the
// state is taken, a value's state at the value, a block's at its end, a
// loop header's at its start but for its own block's boxes of its phis,
// and an entry's after the loads it makes. The allocator's liveness takes a value live where it is
// used as live everywhere between its definition and the use, and its
// spill classes take two values live at once only where one's definition
// dominates the other's.
func checkDominance(f *Func) error {
	d := dominators(f)
	at := make(map[*Value]int)
	for _, b := range f.Blocks {
		for i, v := range b.Values {
			at[v] = i
		}
	}
	// made reports whether x is made before position i of block b: i is
	// len(b.Values) for its end, -1 for its start, where its phis are.
	made := func(x *Value, b *Block, i int) bool {
		if x.Block == b {
			return x.Op == OpPhi || at[x] < i
		}
		return d.dominates(x.Block, b)
	}
	state := func(where string, s *FrameState, b *Block, i int) error {
		if s == nil {
			return nil
		}
		for k, x := range s.All() {
			if x != nil && !made(x, b, i) {
				return fmt.Errorf("ssa: %s's state names %v of b%d in slot %d, which does not dominate it", where, x, x.Block.ID, k)
			}
		}
		return nil
	}
	for _, b := range f.Blocks {
		where := fmt.Sprintf("b%d", b.ID)
		for i, v := range b.Values {
			w := fmt.Sprintf("%s %v (%v)", where, v, v.Op)
			if v.Op == OpPhi {
				for k, x := range v.Args {
					if p := b.Preds[k]; x != nil && !made(x, p, len(p.Values)) {
						return fmt.Errorf("ssa: %s's operand %v of b%d does not dominate its predecessor b%d", w, x, x.Block.ID, p.ID)
					}
				}
				if s := v.Shadow; s != nil && !made(s, b, -1) {
					return fmt.Errorf("ssa: %s's shadow %v of b%d does not dominate it", w, s, s.Block.ID)
				}
				continue
			}
			for _, x := range v.Args {
				if x != nil && !made(x, b, i) {
					return fmt.Errorf("ssa: %s's operand %v of b%d does not dominate it", w, x, x.Block.ID)
				}
			}
			if s := v.Shadow; s != nil && !made(s, b, i) {
				return fmt.Errorf("ssa: %s's shadow %v of b%d does not dominate it", w, s, s.Block.ID)
			}
			if err := state(w, v.State, b, i); err != nil {
				return err
			}
		}
		if x := b.Control; x != nil && !made(x, b, len(b.Values)) {
			return fmt.Errorf("ssa: %s's control %v of b%d does not dominate it", where, x, x.Block.ID)
		}
		if err := state(where, b.State, b, len(b.Values)); err != nil {
			return err
		}
		switch {
		case b.PC < 0:
			if err := state(where+" entry", b.Header, b, len(b.Values)); err != nil {
				return err
			}
		case b.LoopHeader:
			// At its start, where it is taken, but naming its own block's
			// boxes of its phis too, which Optimize's unboxing puts after
			// them and the allocator makes where they are read.
			if err := state(where+" header", b.Header, b, len(b.Values)); err != nil {
				return err
			}
		}
	}
	return nil
}
