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
	// The uses are counted where a block may be threadable and an edge
	// into it would be threaded, and again where a block is threadable
	// whose values a threading since may have given uses or taken them
	// from (stale).
	counted := false
	ws.newStale()
	// The blocks do not change while threading: repairPhis makes values
	// only.
	for _, h := range f.Blocks {
		if h.dead || (h.Kind != BIf && h.Kind != BSwitch) || len(h.Phis) == 0 || !allPure(h) {
			continue
		}
		if counted && ws.anyStale(h.Values) {
			counted = false
		}
		if !counted {
			if !threadsAny(f, h) {
				continue
			}
			computeUses(f)
			counted = true
			ws.newStale()
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
			t := threadTarget(ws, h, k, count)
			if t == nil {
				continue
			}
			// What H's phis are at p's end. The target's phis read on the
			// new edge what they read on the edge from H: an H phi there is
			// a use at p's end, which repairPhis looks up.
			at := ws.vals.take(len(h.Phis))
			for _, phi := range h.Phis {
				at = append(at, phi.Args[k])
				ws.markStale(f, phi.Args[k])
			}
			hi := t.predIndex(h)
			for _, phi := range t.Phis {
				f.addArg(phi, phi.Args[hi])
				ws.markStale(f, phi.Args[hi])
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
	}
	if changed {
		removeUnreachable(f)
	}
}

// A threading changes the uses of the values its threaded edges bring
// into H's phis -- which H's phis lose and the uses repairPhis makes of
// them, and of phis, gain -- and of those the target's phis read on the
// edge from H, which they read on the new edges too: those it marks
// stale. No other value but phis gains or loses a use.

// newStale marks no value stale.
func (ws *workspace) newStale() {
	if ws.staleStamp++; ws.staleStamp == 0 {
		clear(ws.stale)
		ws.staleStamp = 1
	}
}

// markStale marks v stale.
func (ws *workspace) markStale(f *Func, v *Value) {
	if n := f.NumValues(); len(ws.stale) < n {
		ws.stale = append(ws.stale, make([]uint32, n-len(ws.stale))...)
	}
	ws.stale[v.ID] = ws.staleStamp
}

// anyStale reports whether any of vs is stale.
func (ws *workspace) anyStale(vs []*Value) bool {
	for _, v := range vs {
		if int(v.ID) < len(ws.stale) && ws.stale[v.ID] == ws.staleStamp {
			return true
		}
	}
	return false
}

// threadTarget is the block the edge from h's predecessor k would be
// threaded to, or nil where it would not be. count is how many times each
// block is h's predecessor.
func threadTarget(ws *workspace, h *Block, k int, count []int32) *Block {
	p := h.Preds[k]
	if p == h || count[p.ID] != 1 {
		return nil
	}
	c := evalOnEdge(ws, h.Control, h, k)
	if c == nil {
		return nil
	}
	t := branchTarget(h, c)
	if t == h || slices.Contains(p.Succs, t) || countOf(h.Succs, t) != 1 {
		return nil
	}
	return t
}

// threadsAny reports whether thread would thread an edge into h, its
// uses aside: the first it threads, found changing nothing.
func threadsAny(f *Func, h *Block) bool {
	ws := f.ws
	count := ws.blockMarks(f)
	for _, p := range h.Preds {
		count[p.ID]++
	}
	for k := range h.Preds {
		if threadTarget(ws, h, k, count) != nil {
			return true
		}
	}
	return false
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
	foldValue(ws, tmp)
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
	// vars is each of h's phis' variable, by value ID, or -1: the
	// workspace's are all -1 between calls, h's phis' set back when it
	// returns, so that a call costs h's phis, not every value.
	n0 := f.nextValue
	if n := len(ws.vars); n < int(n0) {
		ws.vars = append(ws.vars, make([]int32, int(n0)-n)...)
		for i := n; i < len(ws.vars); i++ {
			ws.vars[i] = -1
		}
	}
	r := repair{f: f, h: h, ends: ends, n0: n0, vars: ws.vars, start: &ws.start, made: ws.made[:0]}
	for i, phi := range h.Phis {
		r.vars[phi.ID] = int32(i)
	}
	// start is each variable's value at a block's start, by variable and
	// block ID.
	r.start.reset(len(h.Phis), int(f.nextBlock))
	phis := h.Phis
	defer func() {
		clear(r.made)
		ws.made = r.made[:0]
		for _, phi := range phis {
			r.vars[phi.ID] = -1
		}
	}()
	for _, b := range f.Blocks {
		for _, v := range b.Phis {
			if r.isMade(v) {
				continue
			}
			for j, a := range v.Args {
				if i, ok := r.varOf(a); ok {
					v.Args[j] = r.atEnd(i, b.Preds[j], b)
				}
			}
		}
		for _, v := range b.Values {
			for j, a := range v.Args {
				if i, ok := r.varOf(a); ok {
					v.Args[j] = r.atStart(i, b)
				}
			}
		}
		if i, ok := r.varOf(b.Control); ok {
			b.Control = r.atStart(i, b)
		}
		for j, a := range b.Results {
			if i, ok := r.varOf(a); ok {
				b.Results[j] = r.atStart(i, b)
			}
		}
	}
	if r.forwarded == 0 {
		return
	}
	// The uses of the phis made that turned out trivial take the values
	// they are, and the phis go.
	for _, b := range f.Blocks {
		if len(b.Phis) > 0 {
			b.Phis = slices.DeleteFunc(b.Phis, func(v *Value) bool { return r.isMade(v) && r.info(v).fwd != nil })
		}
		for _, v := range b.Phis {
			for j, a := range v.Args {
				v.Args[j] = r.follow(a)
			}
		}
		for _, v := range b.Values {
			for j, a := range v.Args {
				v.Args[j] = r.follow(a)
			}
		}
		if b.Control != nil {
			b.Control = r.follow(b.Control)
		}
		for j, a := range b.Results {
			b.Results[j] = r.follow(a)
		}
	}
}

// repair is repairPhis' state: the function, h, its threaded edges, the
// first ID of the phis it makes (n0), h's phis' variables by value ID
// (vars), each variable's value at blocks' starts, the phis made -- each
// one's variable, the phis made that use it, and the value it is forwarded
// to once found trivial -- and how many were.
type repair struct {
	f         *Func
	h         *Block
	ends      *threadEnds
	n0        int32
	vars      []int32
	start     *startTable
	made      []madePhi
	forwarded int
}

// varOf is the variable v is, if it is one of h's phis.
func (r *repair) varOf(v *Value) (int, bool) {
	if v == nil || v.ID >= r.n0 || r.vars[v.ID] < 0 {
		return 0, false
	}
	return int(r.vars[v.ID]), true
}

// isMade reports whether v is a phi repairPhis made.
func (r *repair) isMade(v *Value) bool { return v.ID >= r.n0 }

// info is what is known of phi v, which repairPhis made.
func (r *repair) info(v *Value) *madePhi { return &r.made[v.ID-r.n0] }

// follow is v, or the value it is forwarded to where it is a phi made
// that turned out trivial.
func (r *repair) follow(v *Value) *Value {
	for r.isMade(v) && r.info(v).fwd != nil {
		v = r.info(v).fwd
	}
	return v
}

// tryRemove forwards phi v, which repairPhis made, to the value it is if
// it is trivial, and tries again the phis made that use it.
func (r *repair) tryRemove(v *Value) *Value {
	var same *Value
	for _, a := range v.Args {
		a = r.follow(a)
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
	vi := r.info(v)
	vi.fwd = same
	r.forwarded++
	if r.start.at(vi.v, v.Block) == v {
		r.start.set(vi.v, v.Block, same)
	}
	for _, u := range vi.users {
		if r.info(u).fwd == nil && u != v {
			r.tryRemove(u)
		}
	}
	return same
}

// atEnd is variable i's value at the end of b, on its edge to s.
func (r *repair) atEnd(i int, b, s *Block) *Value {
	e, ok := r.ends.get(b)
	if !ok || e.to != s {
		return r.atStart(i, b)
	}
	if j, ok := r.varOf(e.vals[i]); ok {
		return r.atStart(j, b)
	}
	return e.vals[i]
}

// atStart is variable i's value at b's start.
func (r *repair) atStart(i int, b *Block) *Value {
	h := r.h
	if b == h {
		return h.Phis[i]
	}
	if v := r.start.at(i, b); v != nil {
		return r.follow(v)
	}
	switch len(b.Preds) {
	case 0:
		panic("wasm opt: a phi threaded past the entry")
	case 1:
		v := r.atEnd(i, b.Preds[0], b)
		r.start.set(i, b, v)
		return v
	}
	ws := r.f.ws
	v := r.f.newValue(b, OpPhi, h.Phis[i].Type)
	r.start.set(i, b, v)
	r.made = append(r.made, madePhi{v: i})
	v.Args = ws.vals.take(len(b.Preds))
	for _, p := range b.Preds {
		a := r.atEnd(i, p, b)
		v.Args = append(v.Args, a)
		if r.isMade(a) {
			ai := r.info(a)
			ai.users = appendA(&ws.vals, ai.users, v)
		}
	}
	return r.tryRemove(v)
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

// startTable is repairPhis' values of variables at blocks' starts, by
// variable and block ID: an array of an entry for each where that is not
// too many, its entries those marked with its stamp, else a hash table.
type startTable struct {
	nb    int
	vals  []*Value
	stamp []uint32
	gen   uint32
	m     u64map[*Value]
}

// maxStartDense is the most entries a startTable keeps in its array.
const maxStartDense = 1 << 16

// reset empties t for vars variables over nb blocks.
func (t *startTable) reset(vars, nb int) {
	t.nb = 0
	if n := vars * nb; n <= maxStartDense {
		t.nb = nb
		if len(t.vals) < n {
			t.vals, t.stamp, t.gen = make([]*Value, n), make([]uint32, n), 0
		}
		t.gen++
		if t.gen == 0 {
			clear(t.stamp)
			t.gen = 1
		}
		return
	}
	t.m.reset(vars * 8)
}

// at is variable i's value at b's start, or nil.
func (t *startTable) at(i int, b *Block) *Value {
	if t.nb > 0 {
		if k := i*t.nb + int(b.ID); t.stamp[k] == t.gen {
			return t.vals[k]
		}
		return nil
	}
	return t.m.at(uint64(i)<<32 | uint64(uint32(b.ID)))
}

// set makes v variable i's value at b's start.
func (t *startTable) set(i int, b *Block, v *Value) {
	if t.nb > 0 {
		k := i*t.nb + int(b.ID)
		t.vals[k], t.stamp[k] = v, t.gen
		return
	}
	t.m.set(uint64(i)<<32|uint64(uint32(b.ID)), v)
}
