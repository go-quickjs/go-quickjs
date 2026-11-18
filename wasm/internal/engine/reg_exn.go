package engine

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Register code's exceptions (README.md, "Traps and exceptions"). A throw
// is a Go panic of an *Exception. The body of a try_table or
// a legacy try is a region of the code's, by the module offsets of its
// instructions; a frame whose code has any runs under the catching loop
// (runRegCatching), which recovers an exception, finds the innermost region
// holding the offset the frame was at -- of the call or the throw it was
// making, which its entry in the exec's calls holds -- and goes on at the
// landing of the clause that takes it. A legacy catch's exception is kept
// on the exec's caught stack while its body runs, for rethrow; a branch out
// of the body drops it (xCaughtTo).

// regRegion is the body of a try_table or a legacy try: the module offsets
// of its instructions, [start, end), and what an exception thrown there
// does.
type regRegion struct {
	start, end uint32
	// depth is the construct's index in the control stack; caught the
	// number of the frame's caught exceptions while its body runs.
	depth, caught int
	// clauses are a try_table's catch clauses or a legacy try's catches,
	// legacy says which; delegate says a legacy try delegates to the
	// construct at index target.
	clauses  []regClause
	legacy   bool
	delegate bool
	target   int
}

// regClause is a catch clause: the tag it catches, or any (all); where its
// values go, and its exception's reference (ref, into reference slot exn);
// the number of the frame's caught exceptions where it lands; and the pc it
// lands at.
type regClause struct {
	tag      uint32
	all, ref bool
	vals     []regMove
	exn      uint32
	caught   int
	pc       int32
}

// The exception operations of the second table.
const (
	xThrow    regOp = 176 + iota // throw the tag and the values of sites[BC]
	xThrowRef                    // throw the exception reference in reference slot BC
	xRethrow                     // throw the frame's caught exception BC
)

// catching returns the number of legacy catches among the constructs at
// index 1 to last, inclusive.
func (c *regCompiler) catching(last int) int {
	k := 0
	for i := 1; i <= last; i++ {
		if c.ctrl[i].catching {
			k++
		}
	}
	return k
}

// exnOp compiles an exception instruction, reporting whether op is one.
func (c *regCompiler) exnOp(op opcode.Opcode) bool {
	switch op {
	case opcode.Throw:
		tag := c.r.u32()
		s := regSite{fn: tag, at: c.at}
		s.args, _ = c.siteArgs(c.m.TagType(tag))
		c.out.sites = append(c.out.sites, s)
		c.emitBC(rExt, xThrow, 0, uint32(len(c.out.sites)-1))
		c.markDead()
	case opcode.ThrowRef:
		e := c.pop()
		c.emitBC(rExt, xThrowRef, 0, uint32(c.slotOf(e, len(c.stack))))
		c.markDead()
	case opcode.Rethrow:
		ti := len(c.ctrl) - 1 - int(c.r.u32())
		c.emitBC(rExt, xRethrow, 0, uint32(c.catching(ti)-1))
		c.markDead()
	case opcode.TryTable:
		c.tryTable()
	case opcode.Try:
		params, results := c.r.blockType(c.m)
		start := uint32(c.r.pc)
		t := c.begin(op, params, results, nil)
		t.region = len(c.out.regions)
		c.out.regions = append(c.out.regions, regRegion{start: start, depth: len(c.ctrl) - 1, caught: c.catching(len(c.ctrl) - 1), legacy: true})
		n := 0
		for _, t := range c.ctrl {
			if t.op == opcode.Try {
				n++
			}
		}
		c.tries = max(c.tries, n)
	case opcode.Catch, opcode.CatchAll:
		t := c.top()
		r := &c.out.regions[t.region]
		if !t.catching {
			r.end = c.at
		}
		c.fallOut(t)
		if !t.dead {
			if t.catching {
				c.emitBC(rExt, xCaughtTo, 0, uint32(c.catching(len(c.ctrl)-2)))
			}
			t.fixes = append(t.fixes, c.jump(rJmp, 0))
		}
		t.dead = false
		t.catching = true
		cl := regClause{all: op == opcode.CatchAll, pc: int32(len(c.out.code)), caught: r.caught}
		c.last = -1
		if !cl.all {
			cl.tag = c.r.u32()
			for i, pt := range c.m.TagType(cl.tag).Params {
				cl.vals = append(cl.vals, regMove{to: uint32(c.posSlot(pt, t.base+i)), k: kindOf(pt)})
				c.pushSlot(pt, t.base+i)
			}
		}
		r.clauses = append(r.clauses, cl)
	case opcode.Delegate:
		depth := int(c.r.u32())
		t := c.top()
		r := &c.out.regions[t.region]
		r.delegate, r.target = true, len(c.ctrl)-2-depth
		c.end()
	default:
		return false
	}
	return true
}

// tryTable compiles the start of a try_table, whose catch clauses land at
// labels counted from outside it.
func (c *regCompiler) tryTable() {
	params, results := c.r.blockType(c.m)
	n := c.r.u32()
	var clauses []regClause
	var targets []int
	for range n {
		kind := c.r.byte()
		cl := regClause{all: kind >= 2, ref: kind == 1 || kind == 3}
		if !cl.all {
			cl.tag = c.r.u32()
		}
		ti := len(c.ctrl) - 1 - int(c.r.u32())
		t := c.ctrl[ti]
		types := t.labelTypes()
		np := len(types)
		if cl.ref {
			np--
			cl.exn = uint32(c.posSlot(types[np], t.base+np))
		}
		for i, vt := range types[:np] {
			cl.vals = append(cl.vals, regMove{to: uint32(c.posSlot(vt, t.base+i)), k: kindOf(vt)})
		}
		cl.caught = c.catching(ti)
		clauses = append(clauses, cl)
		targets = append(targets, ti)
	}
	start := uint32(c.r.pc)
	t := c.begin(opcode.TryTable, params, results, nil)
	t.region = len(c.out.regions)
	c.out.regions = append(c.out.regions, regRegion{start: start, depth: len(c.ctrl) - 1, caught: c.catching(len(c.ctrl) - 1), clauses: clauses})
	for i, ti := range targets {
		x := rfix{pc: t.region, kind: fixCatch, idx: i}
		if ti == 0 {
			c.retFixes = append(c.retFixes, x)
			continue
		}
		c.branchTo(c.ctrl[ti], x)
	}
}

// endTry ends a try_table or a legacy try (or its last catch).
func (c *regCompiler) endTry(t *rctl) {
	r := &c.out.regions[t.region]
	if !t.catching {
		r.end = c.at
	}
	c.fallOut(t)
	if t.catching && !t.dead {
		c.emitBC(rExt, xCaughtTo, 0, uint32(c.catching(len(c.ctrl)-2)))
	}
}

// retStub lands the branches to the function's label that only a catch
// clause makes: the results, in the function's positions, go where the
// caller finds them.
func (c *regCompiler) retStub() {
	if len(c.retFixes) == 0 {
		return
	}
	c.bind(c.retFixes)
	dst, _, _ := layout(c.results)
	for i, t := range c.results {
		c.store(rentry{t: t, slot: c.posSlot(t, i), local: -1, def: -1}, dst[i].i)
	}
	c.emit(rRet, 0, 0, 0, 0)
}

// runRegCatching runs register code rc, which has try regions, in frame f
// until the frame returns, taking the exceptions its regions catch.
func (x *exec) runRegCatching(f *frame, rc *regCode) {
	st := saveTry(x)
	f.cbase = st.caught
	ci := len(x.calls) - 1
	pc := int32(0)
	for {
		next, again := x.regTry(f, rc, pc, st, ci)
		if !again {
			break
		}
		pc = next
	}
	x.caught = x.caught[:f.cbase]
}

// regTry runs the frame's register code from pc, returning where to go on
// where one of its regions catches an exception, or again false once the
// frame has returned.
func (x *exec) regTry(f *frame, rc *regCode, pc int32, st tryState, ci int) (next int32, again bool) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		var exn *Exception
		switch e := r.(type) {
		case *Exception:
			exn = e
		case *delegation:
			exn = e.exn
		default:
			panic(r)
		}
		at := x.calls[ci].offset
		st.restore(x)
		if pc, ok := x.regCatch(f, rc, exn, at); ok {
			next, again = pc, true
			return
		}
		panic(exn)
	}()
	regRun(f, rc, pc)
	return 0, false
}

// regCatch finds the clause of frame f's regions that takes exception exn
// thrown at module offset at, delivers its values, and returns its landing.
func (x *exec) regCatch(f *frame, rc *regCode, exn *Exception, at uint32) (int32, bool) {
	limit := int(^uint(0) >> 1)
	for {
		var r *regRegion
		for i := range rc.regions {
			q := &rc.regions[i]
			if q.start <= at && at < q.end && q.depth <= limit && (r == nil || q.depth > r.depth) {
				r = q
			}
		}
		if r == nil {
			return 0, false
		}
		if r.delegate {
			limit = r.target
			continue
		}
		for i := range r.clauses {
			cl := &r.clauses[i]
			if !cl.all && exn.Tag != f.inst.Tags[cl.tag] {
				continue
			}
			l := unsafe.Pointer(&f.l[0])
			if !cl.all {
				for j, m := range cl.vals {
					putVal(l, f, m.k, m.to, exn.Vals[j])
				}
			}
			if cl.ref {
				f.r[cl.exn] = ExnRef(exn)
			}
			x.caught = x.caught[:f.cbase+cl.caught]
			if r.legacy {
				x.caught = append(x.caught, exn)
			}
			return cl.pc, true
		}
		limit = r.depth - 1
	}
}

func rhThrow(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	s := &f.rc.sites[uint32(w>>32)]
	vals := make([]Val, len(s.args))
	for i, m := range s.args {
		vals[i] = getVal(l, f, m.k, m.from)
	}
	x := f.x
	x.calls[len(x.calls)-1].offset = s.at
	panic(&Exception{hdr: header{kind: ObjExn}, Tag: f.inst.Tags[s.fn], Vals: vals})
}

func rhThrowRef(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	r := f.r[uint32(w>>32)]
	if r.IsNull() {
		trapAt(TrapRethrowNull, regAt(pc))
	}
	x := f.x
	x.calls[len(x.calls)-1].offset = f.rc.offs[pc]
	panic(r.Exn())
}

func rhRethrow(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	x := f.x
	x.calls[len(x.calls)-1].offset = f.rc.offs[pc]
	panic(x.caught[f.cbase+int(uint32(w>>32))])
}

func rhCaughtTo(l unsafe.Pointer, w uint64, pc int32, f *frame) int32 {
	x := f.x
	x.caught = x.caught[:f.cbase+int(uint32(w>>32))]
	return pc + 1
}

func init() {
	regExtTable[xThrow] = rhThrow
	regExtTable[xThrowRef] = rhThrowRef
	regExtTable[xRethrow] = rhRethrow
	regExtTable[xCaughtTo] = rhCaughtTo
}
