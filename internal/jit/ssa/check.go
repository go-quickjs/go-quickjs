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
		for _, v := range s.Slots {
			if v == nil {
				continue
			}
			if err := use(where+"'s state", v); err != nil {
				return err
			}
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
