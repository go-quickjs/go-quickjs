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
// threaded path does not run. The target's phis read on the new edge what
// they read on the edge from H. Then H's phis no longer hold on every path
// to the blocks H dominated: each is a variable defined by H, and on each
// threaded edge by the argument it brought, and its uses are looked up
// again as the builder looks up a local (see build.go), phis made where
// definitions meet (repairPhis).
func thread(f *Func) {
	ws := f.ws
	changed := false
	// The uses are counted where a block may be threadable, again after a
	// block is threaded.
	counted := false
	// The blocks do not change while threading: repairPhis makes values
	// only.
	for _, h := range f.Blocks {
		if h.dead || (h.Kind != BIf && h.Kind != BSwitch) || len(h.Phis) == 0 || !allPure(h) {
			continue
		}
		if !counted {
			computeUses(f)
			counted = true
		}
		if !threadable(f, h) {
			continue
		}
		// count is how many times each block is H's predecessor; gone the
		// predecessors threaded, removed together at the end.
		count := ws.blockMarks(f)
		for _, p := range h.Preds {
			count[p.ID]++
		}
		gone := ws.gone[:0]
		threaded := false
		ends := ws.threadEnds(f)
		for k, p := range h.Preds {
			if p == h || count[p.ID] != 1 {
				continue
			}
			c := evalOnEdge(ws, h.Control, h, k)
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
			at := ws.vals.take(len(h.Phis))
			for _, phi := range h.Phis {
				at = append(at, phi.Args[k])
			}
			hi := t.predIndex(h)
			for _, phi := range t.Phis {
				f.addArg(phi, phi.Args[hi])
			}
			p.Succs[slices.Index(p.Succs, h)] = t
			t.Preds = appendA(&ws.blks, t.Preds, p)
			gone = append(gone, int32(k))
			threaded = true
			ends.set(p, threadEnd{t, at})
		}
		ws.gone = gone
		if !threaded {
			continue
		}
		removePreds(h, gone)
		changed = true
		repairPhis(f, h, ends)
		counted = false
	}
	if changed {
		removeUnreachable(f)
	}
}

// removePreds removes b's predecessors at the indices gone, in order, and
// their phis' arguments.
func removePreds(b *Block, gone []int32) {
	cut := func(n int, at func(i int) bool) int {
		j := 0
		for i := 0; i < n; i++ {
			if !at(i) {
				if i != j {
					b.Preds[j] = b.Preds[i]
				}
				j++
			}
		}
		return j
	}
	isGone := func(g []int32) func(i int) bool {
		k := 0
		return func(i int) bool {
			if k < len(g) && int(g[k]) == i {
				k++
				return true
			}
			return false
		}
	}
	n := len(b.Preds)
	keep := cut(n, isGone(gone))
	clear(b.Preds[keep:])
	b.Preds = b.Preds[:keep]
	for _, p := range b.Phis {
		g := isGone(gone)
		j := 0
		for i := 0; i < n; i++ {
			if !g(i) {
				p.Args[j] = p.Args[i]
				j++
			}
		}
		clear(p.Args[j:])
		p.Args = p.Args[:j]
	}
}

// allPure reports whether h's values are pure.
func allPure(h *Block) bool {
	for _, v := range h.Values {
		if !v.Pure() {
			return false
		}
	}
	return true
}

// threadable reports whether h's values, which are pure, are used by h
// alone: its other values and its branch. The uses are counted.
func threadable(f *Func, h *Block) bool {
	ws := f.ws
	// local counts the uses within h of h's values, by value ID: those of
	// this call are marked with its stamp.
	ws.stamp++
	stamp := ws.stamp
	n := f.NumValues()
	if len(ws.localMark) < n {
		ws.localMark = append(ws.localMark, make([]uint32, n-len(ws.localMark))...)
		ws.localUses = append(ws.localUses, make([]int32, n-len(ws.localUses))...)
	}
	for _, v := range h.Values {
		ws.localMark[v.ID] = stamp
		ws.localUses[v.ID] = 0
	}
	use := func(a *Value) {
		if int(a.ID) < n && ws.localMark[a.ID] == stamp {
			ws.localUses[a.ID]++
		}
	}
	for _, v := range h.Values {
		for _, a := range v.Args {
			use(a)
		}
	}
	if h.Control != nil {
		use(h.Control)
	}
	for _, v := range h.Values {
		if ws.localUses[v.ID] != v.Uses {
			return false
		}
	}
	return true
}

// evalOnEdge is v's value, a constant, when h is entered from its
// predecessor k, or nil if that is not known: an h phi's argument from k,
// or h's pure values of them folded.
func evalOnEdge(ws *workspace, v *Value, h *Block, k int) *Value {
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
	tmp := ws.values.new()
	*tmp = *v
	tmp.Args = ws.vals.take(len(v.Args))
	for _, a := range v.Args {
		x := evalOnEdge(ws, a, h, k)
		if x == nil {
			return nil
		}
		tmp.Args = append(tmp.Args, x)
	}
	foldValue(tmp)
	if tmp.Op != OpConst {
		return nil
	}
	return tmp
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
func repairPhis(f *Func, h *Block, ends *threadEnds) {
	ws := f.ws
	// vars is each of h's phis' variable, by value ID, or -1.
	n0 := f.nextValue
	vars := grown(ws.vars, int(n0))
	for i := range vars {
		vars[i] = -1
	}
	ws.vars = vars
	for i, phi := range h.Phis {
		vars[phi.ID] = int32(i)
	}
	varOf := func(v *Value) (int, bool) {
		if v == nil || v.ID >= n0 || vars[v.ID] < 0 {
			return 0, false
		}
		return int(vars[v.ID]), true
	}
	// start is each variable's value at a block's start, by variable and
	// block ID.
	start := &ws.start
	start.reset(len(h.Phis) * 8)
	startKey := func(i int, b *Block) uint64 { return uint64(i)<<32 | uint64(uint32(b.ID)) }
	// The phis made, from ID n0 on: each one's variable, the phis made
	// that use it, and the value it is forwarded to once found trivial.
	made := ws.made[:0]
	defer func() {
		clear(made)
		ws.made = made[:0]
	}()
	info := func(v *Value) *madePhi { return &made[v.ID-n0] }
	isMade := func(v *Value) bool { return v.ID >= n0 }
	forwarded := 0
	follow := func(v *Value) *Value {
		for isMade(v) && info(v).fwd != nil {
			v = info(v).fwd
		}
		return v
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
		vi := info(v)
		vi.fwd = same
		forwarded++
		if k := startKey(vi.v, v.Block); start.at(k) == v {
			start.set(k, same)
		}
		for _, u := range vi.users {
			if info(u).fwd == nil && u != v {
				tryRemove(u)
			}
		}
		return same
	}
	var atStart func(i int, b *Block) *Value
	atEnd := func(i int, b, s *Block) *Value {
		e, ok := ends.get(b)
		if !ok || e.to != s {
			return atStart(i, b)
		}
		if j, ok := varOf(e.vals[i]); ok {
			return atStart(j, b)
		}
		return e.vals[i]
	}
	atStart = func(i int, b *Block) *Value {
		if b == h {
			return h.Phis[i]
		}
		k := startKey(i, b)
		if v := start.at(k); v != nil {
			return follow(v)
		}
		switch len(b.Preds) {
		case 0:
			panic("wasm opt: a phi threaded past the entry")
		case 1:
			v := atEnd(i, b.Preds[0], b)
			start.set(k, v)
			return v
		}
		v := f.newValue(b, OpPhi, h.Phis[i].Type)
		start.set(k, v)
		made = append(made, madePhi{v: i})
		v.Args = ws.vals.take(len(b.Preds))
		for _, p := range b.Preds {
			a := atEnd(i, p, b)
			v.Args = append(v.Args, a)
			if isMade(a) {
				ai := info(a)
				ai.users = appendA(&ws.vals, ai.users, v)
			}
		}
		return tryRemove(v)
	}
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			if isMade(v) {
				continue
			}
			for j, a := range v.Args {
				if i, ok := varOf(a); ok {
					v.Args[j] = atEnd(i, b.Preds[j], b)
				}
			}
		}
		for _, v := range b.Values {
			for j, a := range v.Args {
				if i, ok := varOf(a); ok {
					v.Args[j] = atStart(i, b)
				}
			}
		}
		if i, ok := varOf(b.Control); ok {
			b.Control = atStart(i, b)
		}
		for j, a := range b.Results {
			if i, ok := varOf(a); ok {
				b.Results[j] = atStart(i, b)
			}
		}
	}
	if forwarded == 0 {
		return
	}
	// The uses of the phis made that turned out trivial take the values
	// they are, and the phis go.
	for _, b := range f.Blocks {
		if len(b.Phis) > 0 {
			b.Phis = slices.DeleteFunc(b.Phis, func(v *Value) bool { return isMade(v) && info(v).fwd != nil })
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

// madePhi is what repairPhis knows of a phi it made: its variable, the
// phis made that use it, and the value it is forwarded to, once found
// trivial.
type madePhi struct {
	v     int
	users []*Value
	fwd   *Value
}

// threadEnd is what a threaded edge from a predecessor of a block H, to
// to, defines: the values of H's phis, an argument for each.
type threadEnd struct {
	to   *Block
	vals []*Value
}

// threadEnds are the threaded edges of a block, by their predecessor's ID:
// those marked with the current stamp.
type threadEnds struct {
	stamp uint32
	mark  []uint32
	end   []threadEnd
}

// threadEnds returns the workspace's threaded edges, empty, for f.
func (ws *workspace) threadEnds(f *Func) *threadEnds {
	e := &ws.ends
	if n := int(f.nextBlock); len(e.mark) < n {
		e.mark = append(e.mark, make([]uint32, n-len(e.mark))...)
		e.end = append(e.end, make([]threadEnd, n-len(e.end))...)
	}
	e.stamp++
	return e
}

func (e *threadEnds) set(p *Block, t threadEnd) {
	e.mark[p.ID] = e.stamp
	e.end[p.ID] = t
}

func (e *threadEnds) get(p *Block) (threadEnd, bool) {
	if e.mark[p.ID] != e.stamp {
		return threadEnd{}, false
	}
	return e.end[p.ID], true
}
