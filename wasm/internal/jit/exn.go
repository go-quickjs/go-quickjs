//go:build amd64 || arm64

package jit

import "github.com/go-quickjs/go-quickjs/wasm/internal/opcode"

// Exceptions. throw and throw_ref exit to Go (ExitOp), which makes the
// exception and throws it as the interpreter does, a Go panic. A try_table
// compiles to a block, and to a record for Go (Handler): the offsets of its
// body's instructions and, for each catch clause, its tag and a landing
// pad in the code. When an exception reaches Go's handling of an exit --
// thrown there, or out of what Go called -- Go looks through the level's
// native frames, the innermost first, for a try_table whose body holds the
// instruction the frame is at, drops the frames above it, stores the
// clause's values in the slots of its target label, and enters the pad,
// which loads what a resumption does and branches to the label. Every
// entry beneath the try_table, and every local, is in its slot wherever an
// exception can arise: at a call and at an exit.

// Handler is a try_table's or a legacy try's catch clauses, for the
// instructions of its body, from Start up to End, offsets in the module,
// Index the construct's depth in the function's constructs. A function's
// handlers are in the order their bodies end, so that the first whose body
// holds an instruction is the innermost. A legacy try ending in a delegate
// has no clauses: its body's exceptions go to the handlers of the
// constructs at depth Target and out.
type Handler struct {
	Start, End uint32
	Index      int
	Delegate   bool
	Target     int
	Clauses    []Clause
}

// Clause is a catch clause: of exceptions of tag Tag, or of all; Ref says
// it delivers the exnref after the tag's values. Its values go to the
// stack positions from Base, and Pad is the code's offset that branches to
// its label with them there. A legacy catch keeps the exception in
// reference slot Caught for a rethrow; Caught is -1 for a try_table's.
type Clause struct {
	Tag      uint32
	All, Ref bool
	Base     int
	Pad      int
	Caught   int
}

// catchCtl is a catch clause being compiled: target is its label's
// construct, ret says that is the function's body, pad its landing pad.
type catchCtl struct {
	tag      uint32
	all, ref bool
	target   *ctl
	ret      bool
	pad      asmLabel
	// legacy says a legacy catch, body is its body, caught the slot of
	// its exception.
	legacy bool
	body   asmLabel
	caught int
}

// handlerCtl is a handler being compiled, its pads' labels to resolve.
type handlerCtl struct {
	start, end uint32
	index      int
	delegate   bool
	target     int
	clauses    []catchCtl
}

// tryTable begins a try_table, a block whose body's exceptions its catch
// clauses take.
func (c *compiler) tryTable() {
	params, results := c.r.blockType(c.m)
	n := c.r.u32()
	clauses := make([]catchCtl, n)
	for i := range clauses {
		kind := c.r.byte()
		cl := catchCtl{all: kind >= 2, ref: kind == 1 || kind == 3}
		if !cl.all {
			cl.tag = c.r.u32()
		}
		depth := int(c.r.u32())
		cl.target = c.ctrl[len(c.ctrl)-1-depth]
		cl.ret = depth == len(c.ctrl)-1
		clauses[i] = cl
	}
	t := c.enter(opcode.TryTable, params, results)
	t.start = uint32(c.r.pc)
	t.index = len(c.ctrl) - 1
	t.clauses = clauses
}

// closeTry records the handler of the try_table being ended at offset at,
// and makes its pads, out of line.
func (c *compiler) closeTry(at uint32) {
	t := c.top()
	h := handlerCtl{start: t.start, end: at, index: t.index, clauses: t.clauses}
	for i := range h.clauses {
		cl := &h.clauses[i]
		cl.pad = c.a.NewLabel()
		pad, target, ret := cl.pad, cl.target, cl.ret
		c.ool = append(c.ool, func() {
			c.resumeAt(pad)
			if ret {
				for i, rt := range c.sig.Results {
					c.copyPosResult(rt.Kind, i, i)
				}
				c.jmp(c.retL)
				return
			}
			c.jumpTo(target)
		})
	}
	c.handlers = append(c.handlers, h)
}

// inTry reports whether a try_table is open: a loop in one has no entry
// from the interpreter, whose recover around the body would outlive it.
func (c *compiler) inTry() bool {
	for _, t := range c.ctrl {
		if t.op == opcode.TryTable || t.op == opcode.Try {
			return true
		}
	}
	return false
}

// handlersOut returns the handlers with their pads' offsets.
func (c *compiler) handlersOut() []Handler {
	var out []Handler
	for _, h := range c.handlers {
		o := Handler{Start: h.start, End: h.end, Index: h.index, Delegate: h.delegate, Target: h.target}
		for _, cl := range h.clauses {
			caught := -1
			if cl.legacy {
				caught = cl.caught
			}
			o.Clauses = append(o.Clauses, Clause{Tag: cl.tag, All: cl.all, Ref: cl.ref, Base: cl.target.base, Pad: c.a.Offset(cl.pad), Caught: caught})
		}
		out = append(out, o)
	}
	return out
}

// legacyTry begins a legacy try, a block whose body's exceptions its
// catches, or its delegate, take.
func (c *compiler) legacyTry() {
	params, results := c.r.blockType(c.m)
	t := c.enter(opcode.Try, params, results)
	t.start = uint32(c.r.pc)
	t.index = len(c.ctrl) - 1
}

// caughtSlot returns the reference slot a legacy catch of the try at
// depth index of the function's constructs keeps its exception in.
func (c *compiler) caughtSlot(index int) int {
	n := 0
	for _, t := range c.ctrl[:index] {
		if t.op == opcode.Try {
			n++
		}
	}
	return c.req.CaughtSlot + n
}

// legacyCatch begins a catch, or a catch_all, of the innermost legacy try,
// at offset at: its body, which the try's body's exceptions of its tag
// reach through its landing pad, their values in the slots of the try's
// positions.
func (c *compiler) legacyCatch(at uint32, tag uint32, all bool) {
	t := c.top()
	if !t.hasCatch {
		t.hasCatch = true
		t.tryEnd = at
	}
	c.fallthroughResults(t)
	if !t.dead {
		c.jmp(t.label)
	}
	body := c.a.NewLabel()
	c.a.Bind(body)
	t.clauses = append(t.clauses, catchCtl{tag: tag, all: all, target: t, pad: c.a.NewLabel(), legacy: true, body: body, caught: c.caughtSlot(t.index)})
	t.dead = false
	if !all {
		for _, p := range c.m.TagType(tag).Params {
			c.pushSlot(p.Kind)
		}
	}
}

// closeLegacy records the handler of the legacy try being ended at offset
// at, and makes its catches' pads, out of line.
func (c *compiler) closeLegacy(at uint32) {
	t := c.top()
	if !t.hasCatch {
		return
	}
	h := handlerCtl{start: t.start, end: t.tryEnd, index: t.index, clauses: t.clauses}
	for _, cl := range h.clauses {
		pad, body := cl.pad, cl.body
		c.ool = append(c.ool, func() {
			c.resumeAt(pad)
			c.jmp(body)
		})
	}
	c.handlers = append(c.handlers, h)
}

// delegate ends the innermost legacy try, at offset at, its body's
// exceptions going to the construct depth labels out of it and beyond.
func (c *compiler) delegate(at uint32, depth int) {
	t := c.top()
	c.handlers = append(c.handlers, handlerCtl{start: t.start, end: at, index: t.index, delegate: true, target: len(c.ctrl) - 2 - depth})
	c.end()
}

// rethrow compiles rethrow of the exception the catch of the try depth
// labels out caught, which Go throws again.
func (c *compiler) rethrow(at uint32, depth int) {
	c.goOp(opcode.Rethrow, at, uint64(c.caughtSlot(len(c.ctrl)-1-depth)), 0)
	c.markDead()
}
