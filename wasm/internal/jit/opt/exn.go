package opt

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Exceptions. throw, throw_ref and rethrow run in Go (OpGoOp), which throws
// the exception as the interpreter does. A try_table and a legacy try are
// blocks, and a call or a throw within the body of one or more of them --
// a site -- ends its block (BInvoke): its successors are the block after
// it and a landing pad for each catch clause of each construct around it
// whose handlers may take its exceptions, innermost first, each a block of
// its own with the site's block its one predecessor. When an exception
// reaches Go during an exit, Go searches the level's native frames for a
// clause that takes it (the engine's jitCatch), by the records the code
// returns (wjit.Handler): for each site, one for each construct around it,
// innermost first, over the site's instruction alone. Go drops the frames
// above the one that catches, stores the exception's references in the
// reference slots of the clause's label's positions and its numbers in the
// call area (Clause.Vals), and enters the pad, which loads what an exit's
// resumption loads and every value its site's call left in its slot -- as
// after the call, every value live across a call being in its slot -- reads
// the numbers (OpExnVal), and goes to the clause's label with them: a
// try_table's clause's target, or a legacy catch's body, which the pads of
// every site of the try's body reach. An edge to a pad is an edge of the
// control flow graph like any other, so that what the pad's label reads is
// live across the site's call, and in its slot when the exception comes.

// site is a call or a throw within the body of constructs whose handlers
// may take its exceptions.
type site struct {
	v *Value
	b *Block
	// chain is the constructs around the site whose handlers may take its
	// exceptions, innermost first; pads its landing pads, by construct and
	// clause.
	chain []*tryCtl
	pads  []sitePad
}

// sitePad is the landing pad of clause clause of construct t, for a site.
type sitePad struct {
	t      *tryCtl
	clause int
	pad    *Block
}

// tryCtl is what a try_table or a legacy try being built keeps of its
// handlers.
type tryCtl struct {
	// index is the construct's depth among the function's constructs, the
	// body's 0 (Handler.Index).
	index  int
	legacy bool
	// caught is a legacy try's reference slot for the exception its
	// catches take; inCatch says its body has ended, at its first catch.
	caught  int
	inCatch bool
	// delegate and target are a legacy try's that ends with a delegate to
	// the construct at depth target.
	delegate bool
	target   int
	clauses  []tryClause
	// sites are a legacy try's body's sites, which each catch makes pads
	// for.
	sites []*site
}

// tryClause is a catch clause: of exceptions of tag tag, or of all; ref
// delivers the exnref after the tag's values; target is its label's
// construct, ret says that is the function's body.
type tryClause struct {
	tag      uint32
	all, ref bool
	target   *ctl
	ret      bool
}

// tagParams is the values of tag clause cl takes, none for one of all.
func (bd *builder) tagParams(cl *tryClause) []module.ValueType {
	if cl.all {
		return nil
	}
	return bd.m.TagType(cl.tag).Params
}

// tryTable begins a try_table: a block, and its catch clauses.
func (bd *builder) tryTable() {
	r := &bd.r
	params, results := r.blockType(bd.m)
	n := r.u32()
	clauses := make([]tryClause, n)
	for i := range clauses {
		kind := r.byte()
		cl := tryClause{all: kind >= 2, ref: kind == 1 || kind == 3}
		if !cl.all {
			cl.tag = r.u32()
		}
		depth := int(r.u32())
		cl.target = bd.ctrl[len(bd.ctrl)-1-depth]
		cl.ret = cl.target.label == nil
		clauses[i] = cl
	}
	label := bd.f.newBlock()
	t := bd.enter(opcode.TryTable, bd.typesOf(params, "try_table parameter"), bd.typesOf(results, "try_table result"), label)
	t.try = &tryCtl{index: len(bd.ctrl) - 1, clauses: clauses}
}

// legacyTry begins a legacy try: a block, whose catches, or delegate, come
// after its body.
func (bd *builder) legacyTry() {
	params, results := bd.r.blockType(bd.m)
	caught := 0
	for _, c := range bd.ctrl {
		if c.op == opcode.Try {
			caught++
		}
	}
	label := bd.f.newBlock()
	t := bd.enter(opcode.Try, bd.typesOf(params, "try parameter"), bd.typesOf(results, "try result"), label)
	t.try = &tryCtl{index: len(bd.ctrl) - 1, legacy: true, caught: caught}
}

// handlersAround is the constructs whose handlers may take the exceptions
// of a site in the current block, innermost first: every try_table around
// it, and every legacy try whose body it is in.
func (bd *builder) handlersAround() []*tryCtl {
	var chain []*tryCtl
	for i := len(bd.ctrl) - 1; i >= 0; i-- {
		if t := bd.ctrl[i].try; t != nil && !t.inCatch {
			chain = append(chain, t)
		}
	}
	return chain
}

// site ends the current block after call or throw v, if constructs around
// it may take its exceptions, with the landing pads of their try_tables'
// clauses -- a legacy try's catches make theirs when they begin -- and
// goes on in a block of its own.
func (bd *builder) site(v *Value) {
	chain := bd.handlersAround()
	if len(chain) == 0 {
		return
	}
	b := bd.b
	s := &site{v: v, b: b, chain: chain}
	bd.f.sites = append(bd.f.sites, s)
	b.Kind = BInvoke
	cont := bd.f.newBlock()
	cont.sealed = true
	bd.f.addEdge(b, cont)
	for _, t := range chain {
		if t.legacy {
			t.sites = append(t.sites, s)
			continue
		}
		for j := range t.clauses {
			bd.tablePad(s, t, j)
		}
	}
	bd.startBlock(cont)
}

// pad makes a landing pad of site s for clause j of t, and the values of
// the exception it takes: the tag's, each a number read (OpExnVal) or
// nil for a reference, which Go left in the slot of its position.
func (bd *builder) pad(s *site, t *tryCtl, j int, params []module.ValueType) (*Block, []*Value) {
	p := bd.f.newBlock()
	p.sealed = true
	p.Pad = s.v
	bd.f.addEdge(s.b, p)
	s.pads = append(s.pads, sitePad{t: t, clause: j, pad: p})
	bd.f.exnArea = max(bd.f.exnArea, len(params))
	vals := bd.ws.vals.take(len(params) + 1)
	for i, pt := range params {
		if typeOf(pt, "tag parameter") == TRef {
			vals = append(vals, nil)
			continue
		}
		x := bd.f.newValue(p, OpExnVal, typeOf(pt, "tag parameter"))
		x.Aux = uint64(i)
		vals = append(vals, x)
	}
	return p, vals
}

// tablePad makes the landing pad of site s for clause j of try_table t,
// which goes to the clause's label with the exception's values, or
// returns them from the function.
func (bd *builder) tablePad(s *site, t *tryCtl, j int) {
	cl := &t.clauses[j]
	p, vals := bd.pad(s, t, j, bd.tagParams(cl))
	if cl.ref {
		vals = append(vals, nil)
	}
	if cl.ret {
		// The values are in the positions from 0: the references are
		// copied into the result slots.
		p.Kind = BReturn
		res := bd.ws.vals.take(len(vals))
		for i, x := range vals {
			if x == nil {
				bd.refOp(p, refCopy, uint32(bd.refResults[i]), bd.rpos(i), 0, TNone, 0)
				continue
			}
			res = append(res, x)
		}
		p.Results = res
		return
	}
	// The references are in the label's positions' slots already.
	for i, x := range vals {
		if x == nil {
			vals[i] = &bd.refMark
		}
	}
	p.Kind = BPlain
	bd.edge(p, cl.target, vals)
}

// legacyCatch begins a catch, or a catch_all, of the innermost legacy try:
// its body, which the pads of every site of the try's body reach, the
// tag's numbers its phis and its references in their positions' slots.
func (bd *builder) legacyCatch(tag uint32, all bool) {
	t := bd.top()
	tc := t.try
	bd.fallthroughEnd(t)
	bd.stack = bd.stack[:t.base]
	tc.inCatch = true
	cl := tryClause{tag: tag, all: all, target: t}
	j := len(tc.clauses)
	tc.clauses = append(tc.clauses, cl)
	params := bd.tagParams(&cl)
	c := bd.f.newBlock()
	phis := bd.ws.vals.take(len(params))
	for _, pt := range params {
		if typ := typeOf(pt, "tag parameter"); typ != TRef {
			phis = append(phis, bd.f.newValue(c, OpPhi, typ))
		}
	}
	for _, s := range tc.sites {
		p, vals := bd.pad(s, tc, j, params)
		p.Kind = BPlain
		bd.f.addEdge(p, c)
		k := 0
		for _, x := range vals {
			if x != nil {
				bd.f.addArg(phis[k], x)
				k++
			}
		}
	}
	bd.seal(c)
	if len(c.Preds) == 0 {
		// No site of the body throws: the catch's body is never reached.
		bd.markDead()
		return
	}
	t.dead = false
	k := 0
	for _, pt := range params {
		if typeOf(pt, "tag parameter") == TRef {
			bd.pushRef()
			continue
		}
		bd.push(phis[k])
		k++
	}
	bd.startBlock(c)
}

// delegate ends the innermost legacy try, its body's exceptions going to
// the handlers of the construct depth labels out of it and beyond.
func (bd *builder) delegate(depth int) {
	t := bd.top()
	t.try.delegate = true
	t.try.target = len(bd.ctrl) - 2 - depth
	bd.end()
}

// throwOp builds throw, throw_ref or rethrow, which Go runs, and ends the
// block: the exception goes to the handlers around it, or out of the
// function.
func (bd *builder) throwOp(op opcode.Opcode, at uint32) {
	var x uint32
	switch op {
	case opcode.Throw:
		x = bd.r.u32()
	case opcode.Rethrow:
		depth := int(bd.r.u32())
		x = uint32(bd.ctrl[len(bd.ctrl)-1-depth].try.caught)
	}
	// A rethrow's immediate is its try's depth among the legacy trys, the
	// caught slot's past the first (gen.imms).
	v := bd.gcGo(op, x, 0, at)
	bd.site(v)
	bd.b.Kind = BTrap
	bd.b.At = at
	bd.markDead()
}

// clauses reads a try_table's catch clauses, after its block type, each
// clause's tag, if it has one, given to tag if not nil.
func (r *reader) clauses(tag func(uint32)) {
	for n := r.u32(); n > 0; n-- {
		if kind := r.byte(); kind < 2 {
			x := r.u32()
			if tag != nil {
				tag(x)
			}
		}
		r.u32()
	}
}

// checkTryTable refuses a try_table, its opcode read, whose block type or
// clauses' tags have a v128 value.
func checkTryTable(m *module.Module, r *reader) {
	params, results := r.blockType(m)
	checkValues(params, opcode.TryTable, true, "parameter")
	checkValues(results, opcode.TryTable, true, "result")
	r.clauses(func(tag uint32) { checkValues(m.TagType(tag).Params, opcode.TryTable, true, "tag parameter") })
}
