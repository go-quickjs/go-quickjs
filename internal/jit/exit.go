package jit

import (
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// ApplyExit writes the frame of an exit that left it to Go (abi.ExitTable)
// from its description and the registers it saved, as the exit's code
// would have written it, records applied, and puts the exit's own kind,
// PC, depth and site in ctx -- and its inlined callees' frames in the
// contexts past it. For any other exit it does nothing. Run and Resume
// apply it to the context they were given; one who runs native calls
// applies it to the deepest callee's, where that is the one that left.
//
// Every value is read before any slot is written, as Go applies an exit's
// records: a slot may be another's source. It returns how many of the
// slots it wrote held a reference, before or after: what an exit's code
// would have left to Go as records.
func ApplyExit(ctx *abi.Context) (refs int) {
	var s ExitScratch
	return s.Apply(ctx)
}

// ExitScratch is the memory ApplyExit reads an exit's values into before
// it writes them, kept from exit to exit: an array on the stack, cleared at
// each exit as Go clears any memory that holds pointers, cost more than the
// rest of the exit. A runtime keeps one.
type ExitScratch struct {
	writes []exitWrite
}

// exitWrite is a slot an exit writes, and the words it writes.
type exitWrite struct {
	at  unsafe.Pointer
	num uint64
	ref unsafe.Pointer
}

// Apply is ApplyExit, with s's memory.
func (s *ExitScratch) Apply(ctx *abi.Context) (refs int) {
	if ctx.ExitKind != abi.ExitTable {
		return 0
	}
	// The description, which the code keeps alive (Code.Exits), by its
	// address: a word native code writes, not a pointer the collector sees.
	d := (*abi.ExitDescriptor)(asPointer((*uint64)(unsafe.Pointer(&ctx.ExitDesc))))
	f := exitFrame{ctx: ctx, d: d.Frame, enc: d.Frame.Enc,
		locals: asPointer(&ctx.Regs[d.LocalsReg]), stack: asPointer(&ctx.Regs[d.StackReg])}
	if d.Direct {
		// No slot's reference comes from another written here.
		for i := range int(d.Count) {
			if at, n, r, ok := f.value(d.Slot(i)); ok {
				refs += f.write(at, n, r)
			}
		}
	} else {
		writes := s.writes[:0]
		for i := range int(d.Count) {
			if at, n, r, ok := f.value(d.Slot(i)); ok {
				writes = append(writes, exitWrite{at, n, r})
			}
		}
		for _, x := range writes {
			refs += f.write(x.at, x.num, x.ref)
		}
		// The scratch keeps no pointer past the exit.
		clear(writes)
		s.writes = writes[:0]
	}
	if len(d.Inline) != 0 {
		// The callers' frames' first slot, in the VM's stack.
		first := int64((uintptr(f.stack)-uintptr(ctx.StackBase))/uintptr(f.enc.ValueSize)) - int64(d.Frame.Locals)
		next := ctx
		for _, in := range d.Inline {
			next = (*abi.Context)(next.Next)
			next.InlineClosure, next.InlineLocals, next.InlineThis, next.InlineCallee, next.InlineArgs = in.Closure, in.Locals, in.ThisSlot, in.Callee, in.Args
			next.ExitKind, next.ExitPC, next.ExitDepth, next.ExitSite = in.Kind, in.PC, in.Depth, uint64(in.Site)
			next.Live, next.Base = abi.LiveInline, uint64(first+in.Base)
		}
	}
	ctx.Records = 0
	ctx.ExitKind, ctx.ExitPC, ctx.ExitDepth, ctx.ExitSite = uint64(d.Kind), uint64(d.PC), uint64(d.Depth), uint64(int64(d.Site))
	return refs
}

// exitFrame is the frame an exit leaves, as its description and the
// registers it saved find it.
type exitFrame struct {
	ctx           *abi.Context
	d             *abi.ExitFrame
	enc           *abi.Encoding
	locals, stack unsafe.Pointer
}

// addr is the address of the slot a says, as mir's slotSource has it.
func (f *exitFrame) addr(a abi.ExitAddr) unsafe.Pointer {
	switch a.Base {
	case abi.ExitInLocals:
		return unsafe.Add(f.locals, int(a.Off))
	case abi.ExitInStack:
		return unsafe.Add(f.stack, int(a.Off))
	case abi.ExitInThis:
		return unsafe.Add(unsafe.Pointer(&f.ctx.This), -int(f.enc.NumOffset))
	}
	cell := *(*unsafe.Pointer)(unsafe.Add(f.ctx.Upvalues, int(a.Off)*8))
	return *(*unsafe.Pointer)(unsafe.Add(cell, int(f.enc.UpvalueSlot)))
}

// slot is the address of slot i, for a source that is one's.
func (f *exitFrame) slot(i int32) unsafe.Pointer {
	d, size := f.d, f.enc.ValueSize
	switch {
	case i < d.FrameLocals:
		return f.addr(abi.ExitAddr{Base: abi.ExitInLocals, Off: i * size})
	case i == d.ThisSlot:
		return f.addr(abi.ExitAddr{Base: abi.ExitInThis})
	case i < d.Locals:
		return f.addr(abi.ExitAddr{Base: abi.ExitInCell, Off: i - d.FrameLocals})
	}
	return f.addr(abi.ExitAddr{Base: abi.ExitInStack, Off: (i - d.Locals) * size})
}

func (f *exitFrame) num(p unsafe.Pointer) uint64 {
	return *(*uint64)(unsafe.Add(p, int(f.enc.NumOffset)))
}

func (f *exitFrame) ref(p unsafe.Pointer) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Add(p, int(f.enc.RefOffset)))
}

// word is the word l says where to find.
func (f *exitFrame) word(l abi.ExitLoc) uint64 {
	ctx, enc := f.ctx, f.enc
	switch l.Kind {
	case abi.ExitReg:
		return ctx.Regs[l.N]
	case abi.ExitSpill:
		return ctx.Spill[l.N]
	case abi.ExitConst:
		if l.N < 0 {
			return 0
		}
		return f.d.Words[l.N]
	case abi.ExitF64Reg, abi.ExitF64Spill:
		bits := ctx.XRegs[l.N&31]
		if l.Kind == abi.ExitF64Spill {
			bits = ctx.Spill[l.N]
		}
		if f := math.Float64frombits(bits); f != f {
			return enc.CanonicalNaN
		}
		return bits
	case abi.ExitBoolReg, abi.ExitBoolSpill:
		b := ctx.Regs[l.N&31]
		if l.Kind == abi.ExitBoolSpill {
			b = ctx.Spill[l.N]
		}
		if uint32(b) != 0 {
			return enc.True
		}
		return enc.False
	case abi.ExitSlotAddr:
		return uint64(uintptr(f.slot(l.N)))
	}
	return 0
}

// big is the words of the unboxed BigInt l says where to find (ExitLoc's
// IsBig).
func (f *exitFrame) big(l abi.ExitLoc) (uint64, uint64) {
	bits := f.ctx.Regs[l.N&31]
	if l.Kind == abi.ExitI64Spill || l.Kind == abi.ExitU64Spill {
		bits = f.ctx.Spill[l.N]
	}
	_, unsigned := l.IsBig()
	return f.enc.BigWords(bits, unsigned)
}

// value is where slot s is and what is written there -- its number and
// pointer words -- or false where nothing is: it holds its value already.
func (f *exitFrame) value(s *abi.ExitSlot) (at unsafe.Pointer, n uint64, r unsafe.Pointer, ok bool) {
	at = f.slot(s.Slot)
	if big, _ := s.Value.IsBig(); big {
		// Both its words, made of the unboxed BigInt: its pointer word an
		// address outside Go's heap (abi.Encoding's ShortBigHigh).
		num, ref := f.big(s.Value)
		return at, num, *(*unsafe.Pointer)(unsafe.Pointer(&ref)), true
	}
	w := f.word(s.Value)
	n = w
	switch {
	case s.Shadow.Kind != abi.ExitNone:
		// Where the reference came from is known only now: the slot's
		// own value is unchanged; a source's reference is copied.
		fw := f.word(s.Shadow)
		from := asPointer(&fw)
		if from == at {
			return nil, 0, nil, false
		}
		if from != nil {
			if p := f.ref(from); p != nil {
				n, r = f.num(from), p
			}
		}
	case s.Origin >= 0:
		// The reference a slot held on entry, if it holds it still.
		o := f.slot(s.Origin)
		if p := f.ref(o); p != nil && (s.Load || w == f.num(o)) {
			if s.Origin == s.Slot {
				return nil, 0, nil, false
			}
			n, r = f.num(o), p
		}
	}
	return at, n, r, true
}

// write writes a slot's words, and reports 1 if it held a reference or
// holds one now.
func (f *exitFrame) write(at unsafe.Pointer, n uint64, r unsafe.Pointer) (refs int) {
	if r != nil || f.ref(at) != nil {
		refs = 1
	}
	*(*uint64)(unsafe.Add(at, int(f.enc.NumOffset))) = n
	*(*unsafe.Pointer)(unsafe.Add(at, int(f.enc.RefOffset))) = r
	return refs
}

// asPointer is the address a word holds, as a pointer: native code's words
// are addresses Go's memory holds still (ssa's origin.go).
func asPointer(w *uint64) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(w)) }

// ExpandCall writes the state of the native call ctx's code is making, if
// it described it (abi.Context's CallState), into ctx as the call's code had
// written it: the abi.RecordDirect records Go makes the caller's frame from
// if the callee leaves native code -- each value's words where the
// description finds them, the frame and the spill slots as the call left
// them -- and the exit's kind (abi.ExitHost), PC, depth and site. Go
// expands every context below the one native code left from before it
// reads any of them. It does nothing for a context already expanded.
func ExpandCall(ctx *abi.Context) {
	if ctx.CallState == 0 {
		return
	}
	// The description, which the code keeps alive (Code.Calls), by its
	// address.
	st := (*abi.CallState)(asPointer((*uint64)(unsafe.Pointer(&ctx.CallState))))
	ctx.CallState = 0
	f := exitFrame{ctx: ctx, d: st.Frame, enc: st.Frame.Enc, locals: ctx.Locals, stack: ctx.Stack}
	for i := range st.Records {
		rec := &st.Records[i]
		n := ctx.Record[i].Word
		if rec.Num.Kind != abi.ExitNone {
			n = f.word(rec.Num)
		}
		var r unsafe.Pointer
		switch {
		case rec.Ref:
			r = ctx.RecordRef[i]
		case rec.Origin >= 0:
			o := f.slot(rec.Origin)
			if p := f.ref(o); p != nil && (rec.Load || n == f.num(o)) {
				r = p
			}
		}
		ctx.Record[i] = abi.Record{Slot: uint64(rec.Slot) | abi.RecordDirect, Word: n}
		ctx.RecordRef[i] = r
	}
	n := uint64(len(st.Records))
	ctx.Records, ctx.RecordHigh = n, max(ctx.RecordHigh, n)
	ctx.ExitKind, ctx.ExitPC, ctx.ExitDepth, ctx.ExitSite = abi.ExitHost, uint64(st.PC), uint64(st.Depth), uint64(int64(st.Site))
}

// CallPC is the PC and depth of the native call ctx's code is making: its
// description's (abi.Context's CallState), or, expanded, ctx's.
func CallPC(ctx *abi.Context) (pc, depth uint64) {
	if ctx.CallState != 0 {
		st := (*abi.CallState)(asPointer((*uint64)(unsafe.Pointer(&ctx.CallState))))
		return uint64(st.PC), uint64(st.Depth)
	}
	return ctx.ExitPC, ctx.ExitDepth
}

// ExpandGoCall reads into ctx the call of Go its code is making, if it
// described it (abi.Context's GoState), as the call's code had written it
// -- GoOp, GoKeep, a call's exit PC and depth, the operands -- from where
// the code keeps them as the call starts: the registers its routine saved,
// spill slots, constants, the frame, the sources of pointer words, which
// nothing has changed since. It returns how far past where it goes on
// (GoSite) the call goes on if Go does nothing (abi.GoCallState's Fail),
// or 0. It reads what the description's operands say, offsets into ctx and
// the frame, and no more: every call of Go from native code makes it.
func ExpandGoCall(ctx *abi.Context) int32 {
	if ctx.GoState == 0 {
		return 0
	}
	// The description, which the code keeps alive (Code.GoCalls), by its
	// address.
	st := (*abi.GoCallState)(asPointer((*uint64)(unsafe.Pointer(&ctx.GoState))))
	ctx.GoState = 0
	ctx.GoOp, ctx.GoKeep = st.Op, st.Keep
	if st.SetsPC {
		ctx.ExitPC, ctx.ExitDepth = uint64(st.PC), uint64(st.Depth)
	}
	base := unsafe.Pointer(ctx)
	for i := range st.Args {
		a := &st.Args[i]
		var n uint64
		switch a.Kind {
		case abi.GoArgNone:
			continue
		case abi.GoArgConst:
			n = a.Word
		case abi.GoArgWord:
			n = *(*uint64)(unsafe.Add(base, a.At))
		case abi.GoArgF64:
			n = *(*uint64)(unsafe.Add(base, a.At))
			if f := math.Float64frombits(n); f != f {
				n = st.NaN
			}
		case abi.GoArgBool:
			n = st.False
			if *(*uint32)(unsafe.Add(base, a.At)) != 0 {
				n = st.True
			}
		}
		var r unsafe.Pointer
		if a.Ref != abi.GoRefNone && !(a.Number && n < abi.NumberLimit) {
			switch a.Ref {
			case abi.GoRefSource:
				if from := *(*unsafe.Pointer)(unsafe.Add(base, a.From)); from != nil {
					r = *(*unsafe.Pointer)(unsafe.Add(from, st.RefOffset))
				}
			case abi.GoRefSlotSource:
				r = *(*unsafe.Pointer)(unsafe.Add(goSlot(ctx, st, a), st.RefOffset))
			case abi.GoRefOrigin:
				o := goSlot(ctx, st, a)
				if p := *(*unsafe.Pointer)(unsafe.Add(o, st.RefOffset)); p != nil && (a.Load || n == *(*uint64)(unsafe.Add(o, st.NumOffset))) {
					r = p
				}
			}
		}
		ctx.GoArgs[i] = abi.GoArg{Num: n, Ref: uintptr(r)}
	}
	return st.Fail
}

// goSlot is the address of the frame's slot operand a names (abi.GoCallArg's
// Base and From), as an exit's frame finds it (exitFrame's addr).
func goSlot(ctx *abi.Context, st *abi.GoCallState, a *abi.GoCallArg) unsafe.Pointer {
	switch a.Base {
	case abi.ExitInLocals:
		return unsafe.Add(ctx.Locals, int(a.From))
	case abi.ExitInStack:
		return unsafe.Add(ctx.Stack, int(a.From))
	case abi.ExitInThis:
		return unsafe.Add(unsafe.Pointer(&ctx.This), -int(st.NumOffset))
	}
	cell := *(*unsafe.Pointer)(unsafe.Add(ctx.Upvalues, int(a.From)*8))
	return *(*unsafe.Pointer)(unsafe.Add(cell, int(st.UpvalueSlot)))
}
