package opt

import "slices"

// thread threads jumps whose branch is known on their edge: where a block
// H ends with an if or a switch on a value its phis decide, and an edge
// from a predecessor p brings arguments that make it a constant, the edge
// goes from p straight to the target H would branch to. Go's code for
// WebAssembly runs every loop of a function through one loop whose header
// switches on a local each of its back edges sets to a constant: threaded,
// each back edge goes to its case, and the cases' loops are loops.
//
// H's values but its phis must be pure and used only by H, which a
// threaded path does not run. The edge from p takes no values but H's
// phis' arguments: the target's phis read, for the new edge, what they
// read on the edge from H, an H phi's argument from p in that phi's
// place. Then H's phis no longer hold on every path to the blocks H
// dominated: each is a variable defined by H, and by each threaded
// predecessor at its end, and its uses are looked up again as the builder
// looks up a local (see build.go), phis made where definitions meet.
func thread(f *Func) {
	computeUses(f)
	changed := false
	for _, h := range slices.Clone(f.Blocks) {
		if h.dead || (h.Kind != BIf && h.Kind != BSwitch) || len(h.Phis) == 0 || !threadable(h) {
			continue
		}
		var threaded []*Block
		ends := map[*Block]threadEnd{}
		for k := 0; k < len(h.Preds); k++ {
			p := h.Preds[k]
			if p == h || countOf(h.Preds, p) != 1 {
				continue
			}
			c := evalOnEdge(h.Control, h, k)
			if c == nil {
				continue
			}
			t := branchTarget(h, c)
			if t == h || slices.Contains(p.Succs, t) || countOf(h.Succs, t) != 1 {
				continue
			}
			// What H's phis are at p's end. The target's phis read on the
			// new edge what they read on the edge from H: an H phi there is
			// a use at p's end, which repairPhis looks up.
			at := make([]*Value, len(h.Phis))
			for i, phi := range h.Phis {
				at[i] = phi.Args[k]
			}
			hi := t.predIndex(h)
			for _, phi := range t.Phis {
				phi.Args = append(phi.Args, phi.Args[hi])
			}
			p.Succs[slices.Index(p.Succs, h)] = t
			t.Preds = append(t.Preds, p)
			removePred(h, k)
			k--
			threaded = append(threaded, p)
			ends[p] = threadEnd{t, at}
		}
		if len(threaded) == 0 {
			continue
		}
		changed = true
		repairPhis(f, h, ends)
	}
	if changed {
		removeUnreachable(f)
	}
}

// threadable reports whether h's values are pure and used by h alone: its
// other values and its branch.
func threadable(h *Block) bool {
	local := map[*Value]int32{}
	for _, v := range h.Values {
		if !v.Pure() {
			return false
		}
		local[v] = 0
	}
	for _, v := range h.Values {
		for _, a := range v.Args {
			if _, ok := local[a]; ok {
				local[a]++
			}
		}
	}
	if _, ok := local[h.Control]; ok {
		local[h.Control]++
	}
	for v, n := range local {
		if n != v.Uses {
			return false
		}
	}
	return true
}

// evalOnEdge is v's value, a constant, when h is entered from its
// predecessor k, or nil if that is not known: an h phi's argument from k,
// or h's pure values of them folded.
func evalOnEdge(v *Value, h *Block, k int) *Value {
	switch {
	case v.Op == OpConst:
		return v
	case v.Block != h:
		return nil
	case v.Op == OpPhi:
		if a := resolve(v.Args[k]); a.Op == OpConst {
			return a
		}
		return nil
	case len(v.Args) == 0 || len(v.Args) > 2:
		return nil
	}
	tmp := *v
	tmp.Args = make([]*Value, len(v.Args))
	for i, a := range v.Args {
		if tmp.Args[i] = evalOnEdge(a, h, k); tmp.Args[i] == nil {
			return nil
		}
	}
	foldValue(&tmp)
	if tmp.Op != OpConst {
		return nil
	}
	return &tmp
}

// branchTarget is the successor h's branch goes to on constant c.
func branchTarget(h *Block, c *Value) *Block {
	if h.Kind == BIf {
		if uint32(c.Aux) != 0 {
			return h.Succs[0]
		}
		return h.Succs[1]
	}
	i := uint64(uint32(c.Aux))
	if i >= uint64(len(h.Table)) {
		i = uint64(len(h.Table) - 1)
	}
	return h.Succs[h.Table[i]]
}

// repairPhis looks up again every use of h's phis, which hold where h was
// passed but no longer on the threaded edges: each phi is a variable
// defined by h, and on each threaded edge, from p, by ends[p]: the phi's
// argument p brought -- which, if it is another of h's phis, is that
// variable's value at p's end. p's other edges define nothing. A use in a
// block is the variable's value at the block's start, a phi's argument its
// value on the edge from its predecessor; where a block's predecessors bring different values,
// a phi of them is made there, and a phi made that turns out trivial --
// its arguments one value, or itself -- is that value, as the builder
// removes its own (Braun et al.'s tryRemoveTrivialPhi): its uses are
// forwarded, and the phis made that use it tried again.
func repairPhis(f *Func, h *Block, ends map[*Block]threadEnd) {
	vars := map[*Value]int{}
	for i, phi := range h.Phis {
		vars[phi] = i
	}
	start := make([][]*Value, len(h.Phis))
	for i := range start {
		start[i] = make([]*Value, f.nextBlock)
	}
	made := map[*Value]int{}
	users := map[*Value][]*Value{}
	fwd := map[*Value]*Value{}
	follow := func(v *Value) *Value {
		for {
			w, ok := fwd[v]
			if !ok {
				return v
			}
			v = w
		}
	}
	var tryRemove func(v *Value) *Value
	tryRemove = func(v *Value) *Value {
		var same *Value
		for _, a := range v.Args {
			a = follow(a)
			if a == v || a == same {
				continue
			}
			if same != nil {
				return v
			}
			same = a
		}
		if same == nil {
			return v
		}
		fwd[v] = same
		if s := start[made[v]]; s[v.Block.ID] == v {
			s[v.Block.ID] = same
		}
		for _, u := range users[v] {
			if _, gone := fwd[u]; !gone && u != v {
				tryRemove(u)
			}
		}
		return same
	}
	var atStart func(i int, b *Block) *Value
	atEnd := func(i int, b, s *Block) *Value {
		e, ok := ends[b]
		if !ok || e.to != s {
			return atStart(i, b)
		}
		if j, ok := vars[e.vals[i]]; ok {
			return atStart(j, b)
		}
		return e.vals[i]
	}
	atStart = func(i int, b *Block) *Value {
		if b == h {
			return h.Phis[i]
		}
		if v := start[i][b.ID]; v != nil {
			return follow(v)
		}
		switch len(b.Preds) {
		case 0:
			panic("wasm opt: a phi threaded past the entry")
		case 1:
			v := atEnd(i, b.Preds[0], b)
			start[i][b.ID] = v
			return v
		}
		v := f.newValue(b, OpPhi, h.Phis[i].Type)
		start[i][b.ID] = v
		made[v] = i
		v.Args = make([]*Value, len(b.Preds))
		for j, p := range b.Preds {
			a := atEnd(i, p, b)
			v.Args[j] = a
			if _, ok := made[a]; ok {
				users[a] = append(users[a], v)
			}
		}
		return tryRemove(v)
	}
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			if _, ok := made[v]; ok {
				continue
			}
			for j, a := range v.Args {
				if i, ok := vars[a]; ok {
					v.Args[j] = atEnd(i, b.Preds[j], b)
				}
			}
		}
		for _, v := range b.Values {
			for j, a := range v.Args {
				if i, ok := vars[a]; ok {
					v.Args[j] = atStart(i, b)
				}
			}
		}
		if i, ok := vars[b.Control]; ok && b.Control != nil {
			b.Control = atStart(i, b)
		}
		for j, a := range b.Results {
			if i, ok := vars[a]; ok {
				b.Results[j] = atStart(i, b)
			}
		}
	}
	if len(fwd) == 0 {
		return
	}
	// The uses of the phis made that turned out trivial take the values
	// they are, and the phis go.
	for _, b := range f.Blocks {
		if len(b.Phis) > 0 {
			b.Phis = slices.DeleteFunc(b.Phis, func(v *Value) bool { _, gone := fwd[v]; return gone })
		}
		for _, v := range b.Phis {
			for j, a := range v.Args {
				v.Args[j] = follow(a)
			}
		}
		for _, v := range b.Values {
			for j, a := range v.Args {
				v.Args[j] = follow(a)
			}
		}
		if b.Control != nil {
			b.Control = follow(b.Control)
		}
		for j, a := range b.Results {
			b.Results[j] = follow(a)
		}
	}
}

// threadEnd is what a threaded edge from a predecessor of a block H, to
// to, defines: the values of H's phis, an argument for each.
type threadEnd struct {
	to   *Block
	vals []*Value
}
