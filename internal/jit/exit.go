package jit

import (
	"math"
	"slices"
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

// RebaseExitLocals has an exit that left its frame to Go (abi.ExitTable),
// whose code's frame was at from, write it at to instead: the locals'
// register it saved, if it is from, made to. Go moves a frame so where
// native code ran on memory of the VM's own (a register generator's
// values) and Go reads the frame where it reads any frame's. Any other
// exit it leaves alone.
func RebaseExitLocals(ctx *abi.Context, from, to unsafe.Pointer) {
	if ctx.ExitKind != abi.ExitTable {
		return
	}
	d := (*abi.ExitDescriptor)(asPointer((*uint64)(unsafe.Pointer(&ctx.ExitDesc))))
	if at := &ctx.Regs[d.LocalsReg]; asPointer(at) == from {
		*at = uint64(uintptr(to))
	}
}

// ExitScratch is the memory ApplyExit reads an exit's values into before
// it writes them, kept from exit to exit: an array on the stack, cleared at
// each exit as Go clears any memory that holds pointers, cost more than the
// rest of the exit. A runtime keeps one.
type ExitScratch struct {
	writes []exitWrite
	// rehomes and sources are Rehome's.
	rehomes []rehomeEntry
	sources []uintptr
	keys    []uint64
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
		if d.Root != 0 {
			refs += f.writeNode(d.Root)
		}
	} else {
		writes := s.writes[:0]
		if d.Root != 0 {
			writes = f.readNode(d.Root, writes)
		}
		for _, x := range writes {
			refs += f.write(x.at, x.num, x.ref)
		}
		// The scratch keeps no pointer past the exit.
		clear(writes)
		s.writes = writes[:0]
	}
	if d.InlineN != 0 {
		// The callers' frames' first slot, in the VM's stack.
		first := int64((uintptr(f.stack)-uintptr(ctx.StackBase))/uintptr(f.enc.ValueSize)) - int64(d.Frame.Locals)
		next := ctx
		for _, in := range d.Inlines() {
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

// writeNode writes the slots of node off of the frame's Tree
// (abi.ExitFrame's), one by one, and returns how many held a reference.
func (f *exitFrame) writeNode(off uint32) (refs int) {
	t := f.d.Tree
	h := t[off]
	for _, k := range t[off+1 : off+1+h>>1] {
		if h&1 == 0 {
			refs += f.writeNode(k)
		} else if at, n, r, ok := f.value(&f.d.Slots[k]); ok {
			refs += f.write(at, n, r)
		}
	}
	return refs
}

// readNode appends to writes what the slots of node off of the frame's
// Tree are to be written with.
func (f *exitFrame) readNode(off uint32, writes []exitWrite) []exitWrite {
	t := f.d.Tree
	h := t[off]
	for _, k := range t[off+1 : off+1+h>>1] {
		if h&1 == 0 {
			writes = f.readNode(k, writes)
		} else if at, n, r, ok := f.value(&f.d.Slots[k]); ok {
			writes = append(writes, exitWrite{at, n, r})
		}
	}
	return writes
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
		return *ctx.SpillAt(l.N)
	case abi.ExitConst:
		if l.N < 0 {
			return 0
		}
		return f.d.Words[l.N]
	case abi.ExitF64Reg, abi.ExitF64Spill:
		bits := ctx.XRegs[l.N&31]
		if l.Kind == abi.ExitF64Spill {
			bits = *ctx.SpillAt(l.N)
		}
		if f := math.Float64frombits(bits); f != f {
			return enc.CanonicalNaN
		}
		return bits
	case abi.ExitI32Reg, abi.ExitI32Spill:
		w := ctx.Regs[l.N&31]
		if l.Kind == abi.ExitI32Spill {
			w = *ctx.SpillAt(l.N)
		}
		return math.Float64bits(float64(int32(uint32(w))))
	case abi.ExitBoolReg, abi.ExitBoolSpill:
		b := ctx.Regs[l.N&31]
		if l.Kind == abi.ExitBoolSpill {
			b = *ctx.SpillAt(l.N)
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
		bits = *f.ctx.SpillAt(l.N)
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
	if k := f.d.Kinds; int(s.Slot) < len(k) && s.Slot >= 0 && k[s.Slot] != abi.SlotValue {
		// Typed code's raw slot: its word raw, no reference.
		return at, abi.RawWord(k[s.Slot], w), nil, true
	}
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
	ctx.CallLast, ctx.CallState = ctx.CallState, 0
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

// rehomeEntry is a slot of a poll's state (Rehome): where it is (at),
// its value's number word, where its reference is read from (src, nil for
// none), and where that is kept, if the code holds it in a register or a
// spill slot (shadow); same if the slot holds the value already. num and
// ref are the words written, read from src only for a slot Rehome writes
// (loaded): a source is a cell anywhere in the heap, and reading each
// state value's at every poll had cost more than the rest.
type rehomeEntry struct {
	slot         int32
	at, src      unsafe.Pointer
	num          uint64
	ref          unsafe.Pointer
	shadow       abi.ExitLoc
	mark         uint8
	same, loaded bool
	// Not comparable: no equality function made for it, which would
	// move the code after it (the layout lock).
	_ [0]func()
}

// Rehome brings ctx's frame up to date at a poll of a loop's back edge
// after which its code goes on (abi.GoCallState's Rehome, h), as an exit
// to the loop's header would write it, but only where native code reads
// nothing of what it overwrites: it writes a frame register the header's
// value of, unless that is the slot's own -- unchanged since the code was
// entered -- or another of the state's values reads its reference from
// the slot, or the slot is pinned (h's Marks), which a value live there
// reads through. For a register marked to be rewired, whose value's
// source is known only at run time, the code's word for the source -- in
// a register its call saved, or a spill slot -- is made the register's
// own address once the register is written, so that the cell it came from,
// which may be an object's nothing else reaches, is read no more; where
// such a register cannot be written and its source is no slot of the VM's
// stack nor a cell of the context, nothing is written at all. It returns
// how many registers it wrote.
//
// Native code keeps a reference in the slot that held it when the code was
// entered and writes the frame only at its exits: a loop that reassigns a
// register kept the entry's object there, which nothing it reads reaches,
// for as long as it ran natively. Go writes the frame here, with Go's
// stores and their write barrier; what native code reads after it --
// each reference through its source -- is the same value it read before.
func (s *ExitScratch) Rehome(ctx *abi.Context, h *abi.Rehome) (written int) {
	d := h.Desc
	if d == nil || d.InlineN != 0 || d.Frame.Kinds != nil || len(h.Marks) != len(h.Slots) {
		return 0
	}
	f := exitFrame{ctx: ctx, d: d.Frame, enc: d.Frame.Enc, locals: ctx.Locals, stack: ctx.Stack}
	// Every slot's source found before any slot is written: a slot may be
	// another's source.
	es, srcs := s.rehomes[:0], s.sources[:0]
	some := false
	for i := range h.Slots {
		x := f.rehomeOf(&h.Slots[i], h.Marks[i])
		if x.src == nil && x.ref == nil && f.num(x.at) == x.num && f.ref(x.at) == nil {
			// A primitive the slot holds already, as after the poll
			// before: nothing to write, every poll of a loop whose
			// registers hold numbers.
			x.same = true
		}
		es = append(es, x)
		if x.src != nil && !x.same {
			srcs = append(srcs, uintptr(x.src))
		}
		some = some || x.mark&abi.RehomePinned == 0 && !x.same && x.slot < d.Frame.FrameLocals
	}
	s.rehomes, s.sources = es, srcs
	if some {
		slices.Sort(srcs)
		if s.rehomeChecked(&f) {
			for i := range es {
				x := &es[i]
				if !s.rehomeWritable(&f, x) {
					continue
				}
				f.rehomeLoad(x)
				if f.num(x.at) != x.num || f.ref(x.at) != x.ref {
					f.write(x.at, x.num, x.ref)
					written++
				}
				if x.shadow.Kind != abi.ExitNone {
					w := uint64(uintptr(x.at))
					if x.shadow.Kind == abi.ExitReg {
						ctx.Regs[x.shadow.N] = w
					} else {
						*ctx.SpillAt(x.shadow.N) = w
					}
				}
			}
		}
	}
	// The scratch keeps no pointer past the poll.
	clear(es)
	s.rehomes, s.sources = es[:0], srcs[:0]
	return written
}

// rehomeWritable reports whether Rehome writes x's slot: a frame register
// whose value is not its own, nor an object the code does not make, from
// which no value's reference is read (s.sources, in order), and not
// pinned.
func (s *ExitScratch) rehomeWritable(f *exitFrame, x *rehomeEntry) bool {
	if x.same || x.slot < 0 || x.slot >= f.d.FrameLocals || x.mark&abi.RehomePinned != 0 {
		return false
	}
	if _, _, virtual := abi.IsVirtualWord(x.num); virtual {
		// An object the code does not make, which an exit makes.
		return false
	}
	if len(s.sources) == 0 {
		return true
	}
	_, read := slices.BinarySearch(s.sources, uintptr(x.at))
	return !read
}

// rehomeChecked keeps, of Rehome's slots, the sources' words it makes the
// slots' own -- those marked RehomeRewire whose source is in a register
// or a spill slot -- and reports whether Rehome may write: not where a
// value rewired stays where it is, its source no place that holds what it
// held whatever Rehome writes (kept), which may be a cell of an object
// only the slots written hold; nor where two values whose source is one
// word are not the same value's words.
func (s *ExitScratch) rehomeChecked(f *exitFrame) bool {
	es := s.rehomes
	vs := uintptr(f.enc.ValueSize)
	locals := uintptr(f.locals)
	frameEnd := locals + uintptr(f.d.FrameLocals)*vs
	keys := s.keys[:0]
	ok := true
	for i := range es {
		x := &es[i]
		if x.mark&abi.RehomeRewire == 0 || x.src == nil || x.same ||
			x.shadow.Kind != abi.ExitReg && x.shadow.Kind != abi.ExitSpill {
			x.shadow = abi.ExitLoc{}
			continue
		}
		if !s.rehomeWritable(f, x) && !f.kept(x.src, locals, frameEnd) {
			ok = false
			break
		}
		keys = append(keys, uint64(x.shadow.Kind)<<56|uint64(uint32(x.shadow.N))<<24|uint64(i))
	}
	if ok && len(keys) > 1 {
		// By the word's place, then the slot: alike places side by side.
		slices.Sort(keys)
		for k := 1; k < len(keys); k++ {
			if keys[k]>>24 != keys[k-1]>>24 {
				continue
			}
			x, y := &es[keys[k]&(1<<24-1)], &es[keys[k-1]&(1<<24-1)]
			f.rehomeLoad(x)
			f.rehomeLoad(y)
			if x.num != y.num || x.ref != y.ref {
				ok = false
				break
			}
		}
	}
	s.keys = keys[:0]
	return ok
}

// rehomeOf is what Rehome finds of state slot e, as value finds it but
// for the words at its source (rehomeLoad).
func (f *exitFrame) rehomeOf(e *abi.ExitSlot, mark uint8) rehomeEntry {
	x := rehomeEntry{slot: e.Slot, at: f.slot(e.Slot), mark: mark, loaded: true}
	if big, _ := e.Value.IsBig(); big {
		num, ref := f.big(e.Value)
		x.num, x.ref = num, *(*unsafe.Pointer)(unsafe.Pointer(&ref))
		return x
	}
	w := f.word(e.Value)
	x.num = w
	switch {
	case e.Shadow.Kind != abi.ExitNone:
		fw := f.word(e.Shadow)
		from := asPointer(&fw)
		x.src, x.shadow = from, e.Shadow
		x.same = from == x.at
		x.loaded = from == nil
	case e.Origin >= 0:
		o := f.slot(e.Origin)
		if p := f.ref(o); p != nil && (e.Load || w == f.num(o)) {
			x.src, x.num, x.ref = o, f.num(o), p
			x.same = e.Origin == e.Slot
		}
	}
	return x
}

// rehomeLoad reads the words x's value is where its source is, if it has
// not: the reference there, if any, with its number word.
func (f *exitFrame) rehomeLoad(x *rehomeEntry) {
	if x.loaded {
		return
	}
	x.loaded = true
	if p := f.ref(x.src); p != nil {
		x.num, x.ref = f.num(x.src), p
	}
}

// kept reports whether p, a value's source, is where the collector keeps
// what it holds whatever Rehome writes: a register of the frame, a slot
// of the VM's stack, a cell of the context.
func (f *exitFrame) kept(p unsafe.Pointer, locals, frameEnd uintptr) bool {
	a := uintptr(p)
	if a >= locals && a < frameEnd {
		return true
	}
	if base := uintptr(f.ctx.StackBase); base != 0 && a >= base && a < base+uintptr(f.ctx.StackEnd)*uintptr(f.enc.ValueSize) {
		return true
	}
	c := uintptr(unsafe.Pointer(f.ctx))
	return a >= c && a < c+unsafe.Sizeof(*f.ctx)
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

// CalledFunction is the address of the function object that the native
// call ctx's code is making found its callee to be, where the call left
// it out of the callee's window (abi.CallState's Callees): its site's, or
// one checked by its code, which its checks kept (abi.Context's
// CallCallee). Nil where the call wrote it there, or none was found. The
// words are read as pointers, which the caller's code keeps alive.
func CalledFunction(ctx *abi.Context) unsafe.Pointer {
	at := &ctx.CallState
	if *at == 0 {
		at = &ctx.CallLast
	}
	if *at == 0 {
		return nil
	}
	st := (*abi.CallState)(asPointer((*uint64)(unsafe.Pointer(at))))
	if st.Callees == nil {
		return nil
	}
	k := uint64(0)
	if st.Which {
		k = ctx.CallTarget
	}
	if k >= uint64(len(st.Callees)) {
		return nil
	}
	if st.Callees[k] != 0 {
		return *(*unsafe.Pointer)(unsafe.Pointer(&st.Callees[k]))
	}
	return *(*unsafe.Pointer)(unsafe.Pointer(&ctx.CallCallee))
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
		ctx.GoLast = 0
		return 0
	}
	// The description, which the code keeps alive (Code.GoCalls), by its
	// address.
	st := (*abi.GoCallState)(asPointer((*uint64)(unsafe.Pointer(&ctx.GoState))))
	ctx.GoLast, ctx.GoState = ctx.GoState, 0
	ctx.GoOp, ctx.GoKeep = st.Op, st.Keep
	if st.SetsPC {
		ctx.ExitPC, ctx.ExitDepth = uint64(st.PC), uint64(st.Depth)
	}
	// The encoding, read once: a field of st, read at each use, would be
	// a load more for every operand, the call's state having been kept
	// apart from it.
	enc := st.Enc
	for i := range st.Args {
		a := &st.Args[i]
		var n uint64
		switch a.Kind {
		case abi.GoArgNone:
			continue
		case abi.GoArgConst:
			n = a.Word
		case abi.GoArgWord:
			n = *(*uint64)(ctx.Word(a.At))
		case abi.GoArgF64:
			n = *(*uint64)(ctx.Word(a.At))
			if f := math.Float64frombits(n); f != f {
				n = enc.CanonicalNaN
			}
		case abi.GoArgBool:
			n = enc.False
			if *(*uint32)(ctx.Word(a.At)) != 0 {
				n = enc.True
			}
		}
		var r unsafe.Pointer
		if a.Ref != abi.GoRefNone && !(a.Number && n < abi.NumberLimit) {
			switch a.Ref {
			case abi.GoRefSource:
				if from := *(*unsafe.Pointer)(ctx.Word(a.From)); from != nil {
					r = *(*unsafe.Pointer)(unsafe.Add(from, enc.RefOffset))
				}
			case abi.GoRefSlotSource:
				r = *(*unsafe.Pointer)(unsafe.Add(goSlot(ctx, enc, a), enc.RefOffset))
			case abi.GoRefOrigin:
				o := goSlot(ctx, enc, a)
				if p := *(*unsafe.Pointer)(unsafe.Add(o, enc.RefOffset)); p != nil && (a.Load || n == *(*uint64)(unsafe.Add(o, enc.NumOffset))) {
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
func goSlot(ctx *abi.Context, enc *abi.Encoding, a *abi.GoCallArg) unsafe.Pointer {
	switch a.Base {
	case abi.ExitInLocals:
		return unsafe.Add(ctx.Locals, int(a.From))
	case abi.ExitInStack:
		return unsafe.Add(ctx.Stack, int(a.From))
	case abi.ExitInThis:
		return unsafe.Add(unsafe.Pointer(&ctx.This), -int(enc.NumOffset))
	}
	cell := *(*unsafe.Pointer)(unsafe.Add(ctx.Upvalues, int(a.From)*8))
	return *(*unsafe.Pointer)(unsafe.Add(cell, int(enc.UpvalueSlot)))
}
