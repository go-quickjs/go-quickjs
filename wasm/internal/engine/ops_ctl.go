package engine

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// Typed function references and exception handling.
//
// A wasm exception is a Go panic of an *Exception, caught by the
// statements of try_table and try, which run their bodies under a recover
// that takes nothing else: a trap is not an exception. The legacy delegate
// rethrows an exception to a construct further out in its function; it
// panics with a *delegation naming that construct, which the handlers in
// between let pass.

// delegation is an exception a delegate passes to the construct at index
// target of frame f's control stack, and the handlers of the constructs
// around it.
type delegation struct {
	exn    *Exception
	f      *frame
	target int
}

// catchClause is one handler of a try_table: the tag it catches (or any
// tag, for catch_all), whether it also delivers the exnref, and the slots
// its values go to and the signal it returns.
type catchClause struct {
	tag    uint32
	all    bool
	ref    bool
	dst    []slotRef
	sig    int32
	params int
}

// handler is a catch or catch_all of a legacy try.
type handler struct {
	tag  uint32
	all  bool
	dst  []slotRef
	body []stmt
}

// tryState is the execution state a handler restores, as it was when its
// construct was entered.
type tryState struct {
	depth, calls, caught, vals int
	nc, nsp, rc, rsp           int
	// jit is the number of native code's levels running.
	jit int
}

func saveTry(x *exec) tryState {
	return tryState{x.depth, len(x.calls), len(x.caught), len(x.vals), x.nc, x.nsp, x.rc, x.rsp, x.jitLevel()}
}

func (s tryState) restore(x *exec) {
	x.setJITLevel(s.jit)
	x.depth = s.depth
	x.calls = x.calls[:s.calls]
	x.caught = x.caught[:s.caught]
	x.vals = x.vals[:s.vals]
	x.nc, x.nsp, x.rc, x.rsp = s.nc, s.nsp, s.rc, s.rsp
	x.tail = nil
}

// runTry runs body, returning its signal, or the exception it threw that
// the construct at index idx of f's control stack handles.
func runTry(f *frame, body []stmt, idx int) (sig int32, exn *Exception) {
	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case *Exception:
				exn = e
				return
			case *delegation:
				if e.f != f || idx <= e.target {
					exn = e.exn
					return
				}
			}
			panic(r)
		}
	}()
	return runSeq(f, body), nil
}

// blockSignal is the signal a construct returns for its body's signal.
func blockSignal(c int32) int32 {
	if c > 0 {
		return c - 1
	}
	return c
}

// refOps compiles the typed reference and exception instructions, reporting
// whether op is one.
func (c *compiler) refOps(op opcode.Opcode, at uint32) bool {
	switch op {
	case opcode.RefAsNonNull:
		e := c.pop()
		v := c.ref(e)
		c.pushTree(e.t.AsNonNull(), tref(func(f *frame) Ref {
			r := v(f)
			if r.IsNull() {
				trapAt(TrapNullDereference, at)
			}
			return r
		}))
	case opcode.RefEq:
		y := c.ref(c.pop())
		x := c.ref(c.pop())
		c.pushTree(module.I32, ti32(func(f *frame) uint32 {
			a := x(f)
			return b2i(a.Eq(y(f)))
		}))
	case opcode.BrOnNull, opcode.BrOnNonNull:
		c.brOnNull(op, int(c.r.u32()))
	case opcode.CallRef, opcode.ReturnCallRef:
		ti := c.r.u32()
		sig := c.m.Types[ti].Func
		v := c.ref(c.pop())
		target := func(f *frame) *Function {
			r := v(f)
			if r.IsNull() {
				trapAt(TrapNullDereference, at)
			}
			return r.Func()
		}
		c.callTarget(sig, target, at, op == opcode.ReturnCallRef)
	case opcode.Throw:
		c.throw(c.r.u32())
	case opcode.ThrowRef:
		v := c.ref(c.pop())
		c.flushBelow(0)
		c.emit(func(f *frame) int32 {
			r := v(f)
			if r.IsNull() {
				trapAt(TrapRethrowNull, at)
			}
			panic(r.Exn())
		})
		c.markDead()
	case opcode.TryTable:
		c.tryTable()
	case opcode.Try:
		params, results := c.r.blockType(c.m)
		t := c.enter(op, params, results)
		t.index = len(c.ctrl) - 1
		n := 0
		for _, t := range c.ctrl {
			if t.op == opcode.Try {
				n++
			}
		}
		c.tries = max(c.tries, n)
	case opcode.Catch:
		c.legacyCatch(c.r.u32(), false)
	case opcode.CatchAll:
		c.legacyCatch(0, true)
	case opcode.Delegate:
		c.delegate(int(c.r.u32()))
	case opcode.Rethrow:
		c.rethrow(int(c.r.u32()))
	default:
		return false
	}
	return true
}

// brOnNull compiles br_on_null and br_on_non_null.
func (c *compiler) brOnNull(op opcode.Opcode, depth int) {
	t := c.ctrl[len(c.ctrl)-1-depth]
	types := t.labelTypes()
	c.flushBelow(0)
	// Every value the branch may carry, the reference included, is stored
	// into its own slot first.
	for h := 0; h < len(c.stack); h++ {
		c.materialize(h)
	}
	ref := c.stack[len(c.stack)-1]
	rs := ref.slot
	n := len(types)
	base := len(c.stack) - 1
	if op == opcode.BrOnNonNull {
		base = len(c.stack) - n
	} else {
		base = len(c.stack) - 1 - n
	}
	var copies []stmt
	for i, vt := range types {
		if s := copySlot(vt, c.slotEntry(vt, base+i).slot, c.slotEntry(vt, t.base+i).slot); s != nil {
			copies = append(copies, s)
		}
	}
	sig := int32(depth + 1)
	if op == opcode.BrOnNull {
		c.emit(func(f *frame) int32 {
			if f.r[rs].IsNull() {
				for _, s := range copies {
					s(f)
				}
				return sig
			}
			return 0
		})
		c.stack[len(c.stack)-1].t = ref.t.AsNonNull()
		return
	}
	c.emit(func(f *frame) int32 {
		if !f.r[rs].IsNull() {
			for _, s := range copies {
				s(f)
			}
			return sig
		}
		return 0
	})
	c.pop()
}

// throw compiles throw.
func (c *compiler) throw(tag uint32) {
	sig := c.m.TagType(tag)
	es := c.popN(len(sig.Params))
	c.flushBelow(0)
	vals := make([]func(*frame) Val, len(es))
	for i, e := range es {
		vals[i] = c.val(e)
	}
	c.emit(func(f *frame) int32 {
		e := &Exception{hdr: header{kind: ObjExn}, Tag: f.inst.Tags[tag], Vals: evalArgs(f, vals)}
		panic(e)
	})
	c.markDead()
}

// labelSlots returns the slots a branch to the construct at depth (counted
// from the innermost) delivers its values to, and its signal.
func (c *compiler) labelSlots(depth int) ([]slotRef, int32) {
	t := c.ctrl[len(c.ctrl)-1-depth]
	types := t.labelTypes()
	dst := make([]slotRef, len(types))
	for i, vt := range types {
		dst[i] = slotRef{ref: vt.IsRef(), v128: vt.Kind == module.KindV128, i: c.slotEntry(vt, t.base+i).slot}
	}
	return dst, int32(depth + 1)
}

// tryTable compiles the start of a try_table, whose catch clauses branch to
// labels counted from outside it.
func (c *compiler) tryTable() {
	params, results := c.r.blockType(c.m)
	n := c.r.u32()
	clauses := make([]catchClause, n)
	for i := range clauses {
		kind := c.r.byte()
		cl := catchClause{all: kind >= 2, ref: kind == 1 || kind == 3}
		if !cl.all {
			cl.tag = c.r.u32()
			cl.params = len(c.m.TagType(cl.tag).Params)
		}
		cl.dst, cl.sig = c.labelSlots(int(c.r.u32()))
		clauses[i] = cl
	}
	t := c.enter(opcode.TryTable, params, results)
	t.index = len(c.ctrl) - 1
	t.clauses = clauses
}

// putSlots stores vals into frame f's slots dst.
func putSlots(f *frame, dst []slotRef, vals []Val) {
	for i, s := range dst {
		v := vals[i]
		switch {
		case s.ref:
			f.r[s.i] = v.R
		case s.v128:
			f.l[s.i], f.l[s.i+1] = v.N, v.Hi
		default:
			f.l[s.i] = v.N
		}
	}
}

// tryTableStmt is a try_table's statement.
func tryTableStmt(body []stmt, idx int, clauses []catchClause) stmt {
	return func(f *frame) int32 {
		x := f.x
		st := saveTry(x)
		sig, exn := runTry(f, body, idx)
		if exn == nil {
			return blockSignal(sig)
		}
		st.restore(x)
		for i := range clauses {
			cl := &clauses[i]
			if !cl.all && exn.Tag != f.inst.Tags[cl.tag] {
				continue
			}
			if !cl.all {
				putSlots(f, cl.dst[:cl.params], exn.Vals)
			}
			if cl.ref {
				f.r[cl.dst[len(cl.dst)-1].i] = ExnRef(exn)
			}
			return cl.sig
		}
		panic(exn)
	}
}

// legacyCatch begins a catch or catch_all of a legacy try.
func (c *compiler) legacyCatch(tag uint32, all bool) {
	t := c.top()
	c.fallthroughResults(t)
	if len(t.handlers) == 0 {
		t.tryBody = t.body
	} else {
		t.handlers[len(t.handlers)-1].body = t.body
	}
	t.body = nil
	t.dead = false
	h := handler{tag: tag, all: all}
	if !all {
		for i, pt := range c.m.TagType(tag).Params {
			e := c.slotEntry(pt, t.base+i)
			c.push(e)
			h.dst = append(h.dst, slotRef{ref: pt.IsRef(), v128: pt.Kind == module.KindV128, i: e.slot})
		}
	}
	t.handlers = append(t.handlers, h)
}

// tryStmt is a legacy try's statement, with its handlers.
func tryStmt(body []stmt, idx int, handlers []handler) stmt {
	return func(f *frame) int32 {
		x := f.x
		st := saveTry(x)
		sig, exn := runTry(f, body, idx)
		if exn == nil {
			return blockSignal(sig)
		}
		st.restore(x)
		for i := range handlers {
			h := &handlers[i]
			if !h.all && exn.Tag != f.inst.Tags[h.tag] {
				continue
			}
			putSlots(f, h.dst, exn.Vals)
			x.caught = append(x.caught, exn)
			sig := runSeq(f, h.body)
			x.caught = x.caught[:len(x.caught)-1]
			return blockSignal(sig)
		}
		panic(exn)
	}
}

// delegate ends a legacy try whose exceptions go to the construct depth
// levels out.
func (c *compiler) delegate(depth int) {
	t := c.top()
	c.fallthroughResults(t)
	c.ctrl = c.ctrl[:len(c.ctrl)-1]
	target := len(c.ctrl) - 1 - depth
	body, idx := t.body, t.index
	c.emit(func(f *frame) int32 {
		x := f.x
		st := saveTry(x)
		sig, exn := runTry(f, body, idx)
		if exn == nil {
			return blockSignal(sig)
		}
		st.restore(x)
		panic(&delegation{exn: exn, f: f, target: target})
	})
	if !c.top().dead {
		c.pushResults(t)
	}
}

// rethrow compiles rethrow of the exception the catch depth levels out
// caught.
func (c *compiler) rethrow(depth int) {
	// The exceptions being handled are on the exec's caught stack, the
	// innermost catch's last.
	n := 0
	for i := len(c.ctrl) - 1; i > len(c.ctrl)-1-depth; i-- {
		if t := c.ctrl[i]; t.op == opcode.Try && len(t.handlers) > 0 {
			n++
		}
	}
	c.flushBelow(0)
	c.emit(func(f *frame) int32 {
		x := f.x
		panic(x.caught[len(x.caught)-1-n])
	})
	c.markDead()
}

// endOther ends a try_table or a legacy try.
func (c *compiler) endOther(t *ctl, body []stmt) stmt {
	switch t.op {
	case opcode.TryTable:
		return tryTableStmt(body, t.index, t.clauses)
	case opcode.Try:
		if len(t.handlers) == 0 {
			return tryStmt(body, t.index, nil)
		}
		t.handlers[len(t.handlers)-1].body = body
		return tryStmt(t.tryBody, t.index, t.handlers)
	}
	panic("engine: unsupported construct " + t.op.Name())
}
