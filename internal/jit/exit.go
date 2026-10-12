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
	f := exitFrame{ctx: ctx, d: d, enc: d.Enc,
		locals: asPointer(&ctx.Regs[d.LocalsReg]), stack: asPointer(&ctx.Regs[d.StackReg])}
	if d.Direct {
		// No slot's reference comes from another written here.
		for i := range d.Slots {
			if at, n, r, ok := f.value(&d.Slots[i]); ok {
				refs += f.write(at, n, r)
			}
		}
	} else {
		writes := s.writes[:0]
		for i := range d.Slots {
			if at, n, r, ok := f.value(&d.Slots[i]); ok {
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
		first := int64((uintptr(f.stack)-uintptr(ctx.StackBase))/uintptr(f.enc.ValueSize)) - int64(d.Locals)
		next := ctx
		for _, in := range d.Inline {
			next = (*abi.Context)(next.Next)
			next.InlineClosure, next.InlineLocals, next.InlineThis, next.InlineCallee = in.Closure, in.Locals, in.ThisSlot, in.Callee
			next.ExitKind, next.ExitPC, next.ExitDepth, next.ExitSite = in.Kind, in.PC, in.Depth, uint64(in.Site)
			next.Live, next.Base = abi.LiveInline, uint64(first+in.Base)
		}
	}
	ctx.Records = 0
	ctx.ExitKind, ctx.ExitPC, ctx.ExitDepth, ctx.ExitSite = d.Kind, d.PC, d.Depth, uint64(d.Site)
	return refs
}

// exitFrame is the frame an exit leaves, as its description and the
// registers it saved find it.
type exitFrame struct {
	ctx           *abi.Context
	d             *abi.ExitDescriptor
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
		return l.Word
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

// value is where slot s is and what is written there -- its number and
// pointer words -- or false where nothing is: it holds its value already.
func (f *exitFrame) value(s *abi.ExitSlot) (at unsafe.Pointer, n uint64, r unsafe.Pointer, ok bool) {
	at = f.addr(s.At)
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
		o := f.addr(s.OriginAt)
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
