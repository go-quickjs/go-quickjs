//go:build amd64 || arm64

package jit

import "github.com/go-quickjs/go-quickjs/wasm/internal/module"

// Calls whose callee is known only when they are made: call_indirect and
// call_ref, and their tail forms. Each is made natively where the callee
// -- the table's element within the table and not null, or the reference
// -- is a function of the call's type exactly (its canonical index,
// Layout.FuncType), defined by the instance the code runs for
// (Layout.FuncInst against Ctx.Inst), with native code (the link table at
// its index, Layout.FuncIndex) and room for its frame and its reference
// slots, which the engine's tables of each function's frame say
// (Ctx.Frames, Ctx.RFrames). Anything else -- null in a
// table, past the table, another type or a subtype, a host function,
// another instance's, no native code or no room -- exits, and Go finds the
// callee or traps as the interpreter does. A null reference traps in the
// code, as Go would, at the instruction. Each backend's indirect_GOARCH.go
// emits the checks and the call.

// indirect is a call through a table or a reference.
type indirect struct {
	ft        *module.FuncType
	typeIndex uint32
	table     uint32
	// ref says call_ref: the callee is the reference at stack position
	// base+len(ft.Params); else the index into table is there.
	ref  bool
	base int
	at   uint32
}

// calleeAt is the stack position of the table's index or the reference.
func (d *indirect) calleeAt() int { return d.base + len(d.ft.Params) }

// exit returns the exit Go makes the call in, and its arguments.
func (d *indirect) exit(c *compiler, tail bool) (uint64, []uint64) {
	slot := c.slotIndex(d.base)
	if d.ref {
		kind := uint64(ExitCallRef)
		if tail {
			kind = ExitTailRef
		}
		return kind, []uint64{uint64(d.typeIndex), slot, uint64(d.at)}
	}
	kind := uint64(ExitCallIndirect)
	if tail {
		kind = ExitTailIndirect
	}
	return kind, []uint64{uint64(d.typeIndex), uint64(d.table), slot, uint64(d.at)}
}

// native reports whether the call may be made natively: the module's types
// are canonical, which the callee's type is compared by.
func (d *indirect) native(c *compiler) bool {
	return c.req.Layout != nil && int(d.typeIndex) < len(c.m.Canonical)
}

// indirectOperands stores every entry into its slot and pops a call's
// arguments and the table's index or the reference.
func (c *compiler) indirectOperands(ft *module.FuncType, typeIndex, table uint32, ref bool, at uint32) *indirect {
	c.spillAll()
	base := len(c.stack) - len(ft.Params) - 1
	c.truncate(base)
	return &indirect{ft: ft, typeIndex: typeIndex, table: table, ref: ref, base: base, at: at}
}

// callIndirect compiles call_indirect and return_call_indirect.
func (c *compiler) callIndirect(typeIndex, table uint32, at uint32, tail bool) {
	d := c.indirectOperands(c.m.Types[typeIndex].Func, typeIndex, table, false, at)
	if tail {
		c.indirectTail(d)
		return
	}
	c.indirectCall(d)
}

// callRef compiles call_ref and return_call_ref of type typeIndex.
func (c *compiler) callRef(typeIndex uint32, at uint32, tail bool) {
	d := c.indirectOperands(c.m.Types[typeIndex].Func, typeIndex, 0, true, at)
	if tail {
		c.indirectTail(d)
		return
	}
	c.indirectCall(d)
}

// nullTrap traps where a call_ref's reference is null.
func (c *compiler) nullTrap(d *indirect) {
	if d.ref {
		c.jcc(c.refNull(c.refPos(d.calleeAt())), c.trap(TrapNullDereference, d.at))
	}
}

// indirectCall compiles a call through a table or a reference: the poll
// counted down, the callee checked and called natively, else the exit,
// its results in the slots of the positions its arguments took.
func (c *compiler) indirectCall(d *indirect) {
	a := c.a
	kind, args := d.exit(c, false)
	if !d.native(c) {
		resume := a.NewLabel()
		c.exit(kind, resume, args...)
		c.resumeAt(resume)
		c.pushResults(d.ft.Results)
		return
	}
	start, slow, poll, after := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	c.saveHomes()
	c.nullTrap(d)
	a.Bind(start)
	c.indirectNative(d, poll, slow)
	a.Bind(after)
	c.later(func() {
		a.Bind(poll)
		c.sharedExit(ExitPoll, start)
		a.Bind(slow)
		for i, v := range args {
			c.storeCtx(offArg0+int32(8*i), v)
		}
		c.sharedExit(kind, after)
	})
	c.pushResults(d.ft.Results)
}

// indirectTail compiles a tail call through a table or a reference: made
// natively in place of the frame, else by the exit, whose resumption
// returns.
func (c *compiler) indirectTail(d *indirect) {
	a := c.a
	kind, args := d.exit(c, true)
	retResume := a.NewLabel()
	if !d.native(c) {
		c.exit(kind, retResume, args...)
		c.resumeAt(retResume)
		c.jmp(c.retL)
		c.markDead()
		return
	}
	start, slow, poll := a.NewLabel(), a.NewLabel(), a.NewLabel()
	c.nullTrap(d)
	a.Bind(start)
	c.indirectTailNative(d, poll, slow)
	c.later(func() {
		a.Bind(poll)
		c.sharedExit(ExitPoll, start)
		a.Bind(slow)
		c.exit(kind, retResume, args...)
		c.resumeAt(retResume)
		c.jmp(c.retL)
	})
	c.markDead()
}
