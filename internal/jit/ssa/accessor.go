package ssa

import "github.com/go-quickjs/go-quickjs/internal/jit/ir"

// accessor is the accessor the property read or write at pc of the frame
// being translated calls (AccessorFeedback), if any. The plan asks it of
// each instruction more than once, and each answer is a search of the VM's
// caches, so the frame keeps its answers.
func (b *builder) accessor(pc int) (*AccessorSite, bool) {
	in := b.p.Code[pc]
	if in.Op != ir.PropertyRead && in.Op != ir.PropertyWrite && in.Op != ir.ReferenceRead || in.Private || b.fb == nil {
		return nil, false
	}
	if in.Op == ir.ReferenceRead && in.Dest != in.Left.Slot {
		// A method's read, which keeps its receiver below the value
		// (get_prop_this): an accessor is a read's that replaces it.
		return nil, false
	}
	fr := b.cur
	if fr.asked == nil {
		fr.asked = b.f.bytes(len(b.p.Code))
	}
	switch fr.asked[pc] {
	case 1:
		return nil, false
	case 2:
		return fr.accessors[pc], true
	}
	fr.asked[pc] = 1
	var a *AccessorSite
	if _, str := b.stringProperty(pc); str {
		// Receivers that have been strings, whose method is read where
		// String.prototype has it: the VM's word, which a string's read
		// takes before an accessor's.
	} else if af, ok := b.fb.(AccessorFeedback); ok {
		if s, ok := af.Accessor(pc); ok && s.Function != 0 && s.Property.Shape != 0 && s.Property.Holders[0].Object != 0 &&
			s.Setter == (in.Op == ir.PropertyWrite) {
			a = b.f.accessorSite(s)
		}
	}
	if a == nil {
		return nil, false
	}
	if fr.accessors == nil {
		fr.accessors = map[int]*AccessorSite{}
	}
	fr.accessors[pc], fr.asked[pc] = a, 2
	return a, true
}

// accessorCalls are the calls an accessor's read or write makes where it
// is not inlined: its getter or setter natively (AccessorSite's Call), or
// the read or write by Go, called from native code, where the code may
// call Go -- what Go then finds it does, whatever the receiver.
func (b *builder) accessorCalls(a *AccessorSite) []*CallSite {
	kind := AccessorGet
	if a.Setter {
		kind = AccessorSet
	}
	if a.Call != nil {
		c := new(CallSite)
		*c = *a.Call
		c.Accessor, c.Key, c.Method, c.Argc = kind, a.Property.Key, true, b2i(a.Setter)
		return []*CallSite{c}
	}
	if !b.goCalls() {
		return nil
	}
	return []*CallSite{{Go: GoCall, Accessor: kind, Key: a.Property.Key, Argc: b2i(a.Setter), ThisSlot: -1}}
}

// accessorOp is a property read or write whose site found an accessor
// (AccessorSite): the receiver checked, as a read of a property its cache
// found on a prototype checks it, and the accessor there checked to be the
// one compiled for; then the getter or setter inlined, or called -- or,
// where Go makes the read or write, nothing checked. A call's result is a
// read's; a setter's is dropped.
func (b *builder) accessorOp(blk *Block, pc int, in ir.Instruction, a *AccessorSite, operand func(ir.Operand) *Value,
	state func() *FrameState) {
	f := b.f
	var calls []*CallSite
	if b.cur == b.root {
		calls = b.calls[pc]
	}
	fr := b.inlinedAt(pc)
	recv := operand(in.Left)
	var value *Value
	if a.Setter {
		value = operand(in.Right)
	}
	result := func(call *Value) {
		cell := f.newValue(blk, OpCallCell, Source, call)
		r := f.newValue(blk, OpKept, Tagged, cell, call)
		r.Shadow = cell
		if !a.Setter {
			b.assign(in.Dest, blk, r)
		}
		// The callee may have assigned a captured binding (ir.Host's
		// calls).
		if b.hasCells() {
			b.reloadAfterCall(blk)
		}
	}
	// On the stack: guardOf copies its operands, as newValue does.
	var argv [2]*Value
	args := append(argv[:0], recv)
	if value != nil {
		args = append(args, value)
	}
	if fr == nil && len(calls) == 1 && calls[0].Go == GoCall {
		// Go reads or writes it as the interpreter would.
		call := b.guardOf(blk, state, OpCall, Tagged, ir.HostExit, args...)
		f.cache(call).Calls, call.Index = calls, b.callKeeps[pc]
		result(call)
		return
	}
	if fr == nil && len(calls) == 0 {
		blk.ExitKind = ir.HostExit
		blk.State = state()
		blk.State.addUse()
		return
	}
	object := b.guardOf(blk, state, OpObjectOf, Ptr, ir.HostExit, recv)
	site := a.Property
	var holders *HolderChain
	if site.Holders[0].Object != 0 {
		holders = &HolderChain{Holders: site.Holders, More: site.More}
	}
	cell := b.guardOf(blk, state, OpPropCell, Source, ir.HostExit, object)
	cell.Const, cell.Index, cell.Key = ir.Value{Bits: uint64(site.Shape)}, int(site.Index), site.Key
	f.setProperty(cell, holders, site.Cases, nil, nil)
	is := b.guardOf(blk, state, OpAccessorIs, None, ir.HostExit, cell)
	is.Const, is.Index = ir.Value{Bits: uint64(a.Function)}, b2i(a.Setter)
	if fr != nil {
		b.accessorInline(blk, fr, recv, value, state)
		return
	}
	call := b.guardOf(blk, state, OpCall, Tagged, ir.HostExit, args...)
	f.cache(call).Calls, call.Index = calls, b.callKeeps[pc]
	result(call)
}

// accessorInline gives an inlined getter's or setter's start its slots,
// once its accessor is checked: the value, a setter's argument, past
// which undefined, the receiver, and undefined in every other.
func (b *builder) accessorInline(blk *Block, fr *frame, recv, value *Value, state func() *FrameState) {
	site := fr.site
	// Room past the operands for the callee's frame, as for a call's
	// (inlineCall).
	room := b.guardOf(blk, state, OpFrameRoom, None, ir.HostExit)
	room.Index = fr.base - b.root.p.Locals + fr.p.Locals + fr.p.StackSize
	room.Const = ir.Value{Bits: uint64(fr.depth)}
	fr.call = state()
	undefined := b.f.newValue(blk, OpConst, Tagged)
	undefined.Const = ir.Value{Kind: ir.Undefined}
	fr.init = b.f.refsOf(fr.p.Locals + fr.p.StackSize)
	for s := range fr.init {
		fr.init[s] = undefined
	}
	fr.args = nil
	if value != nil {
		fr.args = b.f.refsOf(1)
		fr.args[0] = value
	}
	for i := 0; i < site.Params && i < len(fr.args); i++ {
		fr.init[i] = fr.args[i]
	}
	if site.ThisSlot >= 0 {
		fr.init[site.ThisSlot] = recv
	}
}

// inlineAccessors inlines in fr, an inlined callee, the getters and
// setters its reads and writes call where they may be (AccessorSite): one
// that is not stays a read or write Go makes, at an exit.
func (b *builder) inlineAccessors(fr *frame, total *int) {
	cur := b.cur
	b.enter(fr)
	defer b.enter(cur)
	for pc, in := range fr.p.Code {
		if in.Op != ir.PropertyRead && in.Op != ir.PropertyWrite && in.Op != ir.ReferenceRead || !reachable(fr.p, pc) || fr.inlined[pc] != nil {
			continue
		}
		if a, ok := b.accessor(pc); !ok || a.Inline == nil {
			continue
		}
		if child, ok := b.inlineAt(pc, total); ok {
			if fr.inlined == nil {
				fr.inlined = map[int]*frame{}
			}
			fr.inlined[pc] = child
		}
	}
}

// b2i is 1 for true, 0 for false.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// guardOf is the guard of op on args a translation makes (translate's
// guard), as a method: called through a function value its operands were
// a slice made on the heap at every guard.
func (b *builder) guardOf(blk *Block, state func() *FrameState, op Op, t Type, kind ir.ExitKind, args ...*Value) *Value {
	if b.cur != b.root {
		// Inside an inlined callee Go does the operation, in the callee's
		// frame, and native code goes on where it can.
		kind = ir.HostExit
	}
	v := b.f.newValue(blk, op, t, args...)
	v.Aux = int(kind)
	v.State = state()
	v.State.addUse()
	return v
}

// accessorSite is s where the build keeps its sites: in the workspace,
// if the Func has one.
func (f *Func) accessorSite(s AccessorSite) *AccessorSite {
	if f.ws != nil {
		p := &f.ws.accessors.Make(1)[0]
		*p = s
		return p
	}
	return &s
}
