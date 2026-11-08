// Package amd64 encodes the x86-64 instructions the JIT emits, one function
// per instruction form. It knows nothing of JavaScript or of Go's register
// conventions; callers keep to those (internal/jit/mir). Every form is tested
// against an independent disassembler in internal/jit/verify.
package amd64

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

// Reg is a general-purpose register, in the hardware's numbering.
type Reg uint8

const (
	RAX Reg = iota
	RCX
	RDX
	RBX
	RSP
	RBP
	RSI
	RDI
	R8
	R9
	R10
	R11
	R12
	R13
	R14
	R15
)

// XReg is an SSE register, X0 to X15.
type XReg uint8

// Cond is a condition code, in the hardware's numbering.
type Cond uint8

const (
	CondO  Cond = 0x0
	CondNO Cond = 0x1
	CondB  Cond = 0x2 // below, unsigned <; also carry
	CondAE Cond = 0x3
	CondE  Cond = 0x4
	CondNE Cond = 0x5
	CondBE Cond = 0x6
	CondA  Cond = 0x7
	CondS  Cond = 0x8
	CondNS Cond = 0x9
	CondP  Cond = 0xA // parity: an unordered (NaN) comparison
	CondNP Cond = 0xB
	CondL  Cond = 0xC
	CondGE Cond = 0xD
	CondLE Cond = 0xE
	CondG  Cond = 0xF
)

// Invert returns the opposite condition.
func (c Cond) Invert() Cond { return c ^ 1 }

// Label is a position in the code, bound once.
type Label int

// Asm accumulates code. Branches to labels are patched by Finish, which
// shortens those that reach their labels with a byte's displacement.
type Asm struct {
	buf    []byte
	labels []label
	fixups []fixup
	// short says which fixups' jumps Finish shortened, and removed how many
	// bytes the fixups before each, and it, removed (Moved).
	short   []bool
	removed []int
	// bound are the labels in the order they were bound, which is their
	// positions' order.
	bound []Label
	// open are the fixups whose jumps Finish may shorten, in order; openQ
	// those of them it may yet, by place in open, candPast how many of them
	// are before each's label, and sums what those before each remove.
	open, openQ, candPast, sums []int32
	// aligned says the code has alignments (Align).
	aligned bool
}

// label is a label's place: its offset, or -1 while unbound, and the
// index of the first fixup at or past it, noted as it is bound (Bind) --
// what the jumps before that remove moves it by. Kept together, as Finish
// reads them, in what the offset alone had taken.
type label struct{ pos, first int32 }

type fixup struct {
	// at is where the rel32 is, or an alignment's padding starts, and
	// start where its instruction starts (at less opLen), kept so that
	// Finish's walks over the fixups compute it at none of them: positions
	// in a function's code, which MaxCodeBytes bounds, as 32 bits each,
	// the fixup sixteen bytes, where 24 had been.
	start, at int32
	label     int32
	// kn is the fixup's kind and, past its low byte, an alignment's n
	// (kind, align): four fields, which the compiler keeps in registers
	// as it makes a fixup -- of five, it made it on the stack, a store
	// for each field, and copied it out by one load of all sixteen
	// bytes, which the processor could not take from the stores it
	// waited on: each jump's emission stalled on it, and fix was the
	// costliest line of the code's emission.
	kn uint16
}

// kind is the fixup's kind.
func (f fixup) kind() uint8 { return uint8(f.kn) }

// align is an alignment's n.
func (f fixup) align() uint8 { return uint8(f.kn >> 8) }

// fix notes a fixup of kind to l, its rel32 or padding at the end of the
// code so far.
func (a *Asm) fix(l Label, kind, n uint8) {
	at := len(a.buf)
	a.fixups = append(a.fixups, fixup{start: int32(at - int(opLens[kind&3])), at: int32(at), label: int32(l), kn: uint16(kind) | uint16(n)<<8})
}

// The fixups' kinds: a lea's displacement, which stays 32 bits, and a
// jump's or a conditional jump's, which Finish shortens where it can --
// to two bytes from five and from six.
const (
	fixLea uint8 = iota
	fixJmp
	fixJcc
	fixAlign
)

// shortened is how many bytes a fixup of kind k's jump loses shortened, and
// opLen how many bytes precede its displacement.
func shortened(k uint8) int {
	switch k {
	case fixJmp:
		return 3
	case fixJcc:
		return 4
	}
	return 0
}

// opLens are opLen's, by kind, for fix, in which a table costs the
// inliner less than opLen's switch: a jump's emission, which calls fix, is
// inlined where it is emitted.
var opLens = [4]uint8{fixLea: 3, fixJmp: 1, fixJcc: 2, fixAlign: 0}

func opLen(k uint8) int {
	switch k {
	case fixJmp:
		return 1
	case fixJcc:
		return 2
	case fixAlign:
		return 0
	}
	return 3
}

// Len is the number of bytes emitted so far.
func (a *Asm) Len() int { return len(a.buf) }

// Reset empties the assembler for the next function, keeping its memory.
func (a *Asm) Reset() {
	a.buf, a.labels, a.fixups = a.buf[:0], a.labels[:0], a.fixups[:0]
	a.short, a.removed, a.bound = a.short[:0], a.removed[:0], a.bound[:0]
	a.open, a.openQ, a.candPast, a.sums = a.open[:0], a.openQ[:0], a.candPast[:0], a.sums[:0]
	a.aligned = false
}

// Bytes is the memory the assembler keeps for the next function.
func (a *Asm) Bytes() int {
	return cap(a.buf)*int(unsafe.Sizeof(a.buf[:1][0])) + cap(a.labels)*int(unsafe.Sizeof(label{})) + cap(a.fixups)*int(unsafe.Sizeof(fixup{})) +
		cap(a.short) + cap(a.removed)*int(unsafe.Sizeof(0)) + cap(a.bound)*int(unsafe.Sizeof(Label(0))) +
		(cap(a.open)+cap(a.openQ)+cap(a.candPast)+cap(a.sums))*4
}

// Used is what of Bytes the last function used: its tables' lengths.
func (a *Asm) Used() int {
	return len(a.buf)*int(unsafe.Sizeof(a.buf[:1][0])) + len(a.labels)*int(unsafe.Sizeof(label{})) + len(a.fixups)*int(unsafe.Sizeof(fixup{})) +
		len(a.short) + len(a.removed)*int(unsafe.Sizeof(0)) + len(a.bound)*int(unsafe.Sizeof(Label(0))) +
		(len(a.open)+len(a.openQ)+len(a.candPast)+len(a.sums))*4
}

// NewLabel makes an unbound label.
func (a *Asm) NewLabel() Label {
	a.labels = append(a.labels, label{pos: -1})
	return Label(len(a.labels) - 1)
}

// Bind places l at the current position.
func (a *Asm) Bind(l Label) {
	if a.labels[l].pos >= 0 {
		panic(fmt.Sprintf("amd64: label %d bound twice", l))
	}
	// The first fixup at or past it is the next one made: fixups are made
	// in their positions' order, each past the labels bound before it.
	a.labels[l] = label{pos: int32(len(a.buf)), first: int32(len(a.fixups))}
	a.bound = append(a.bound, l)
}

// Align pads the code to the next multiple of n bytes, n a power of two
// at most 128: it emits n-1 bytes now, of which Finish keeps as many as
// the code after them, its jumps shortened, needs -- one NOP, or, past
// nine bytes, a jump over the rest.
func (a *Asm) Align(n int) {
	if n <= 1 {
		return
	}
	end := a.NewLabel()
	a.fix(end, fixAlign, uint8(n))
	for range n - 1 {
		a.emit(0x90)
	}
	a.Bind(end)
	a.aligned = true
}

// nops are NOPs of 0 to 9 bytes.
var nops = [...][]byte{nil, {0x90}, {0x66, 0x90}, {0x0f, 0x1f, 0x00}, {0x0f, 0x1f, 0x40, 0x00},
	{0x0f, 0x1f, 0x44, 0x00, 0x00}, {0x66, 0x0f, 0x1f, 0x44, 0x00, 0x00},
	{0x0f, 0x1f, 0x80, 0x00, 0x00, 0x00, 0x00}, {0x0f, 0x1f, 0x84, 0x00, 0x00, 0x00, 0x00, 0x00},
	{0x66, 0x0f, 0x1f, 0x84, 0x00, 0x00, 0x00, 0x00, 0x00}}

// pad fills b with code that does nothing: one NOP of its length, or a
// jump over it.
func pad(b []byte) {
	if len(b) < len(nops) {
		copy(b, nops[len(b)])
		return
	}
	b[0], b[1] = 0xEB, byte(len(b)-2)
	for i := 2; i < len(b); i++ {
		b[i] = 0x90
	}
}

// Offset is a bound label's position.
func (a *Asm) Offset(l Label) int { return int(a.labels[l].pos) }

// Finish patches every branch and returns the code. A jump whose label is
// within a byte's displacement of it is shortened, as an assembler relaxes
// branches: most of a function's jumps are to code near them -- a guard's
// to its exit's stub, a branch's over a block -- and a jump of four or five
// bytes' displacement had been an eighth of all the code. Shortening one
// only brings others' labels nearer, so jumps are shortened until no more
// can be. Positions taken before (Len) are where Moved says after.
func (a *Asm) Finish() ([]byte, error) {
	n := len(a.fixups)
	a.short = grow(a.short, n)
	a.removed = grow(a.removed, n)
	// Each label's index of the first fixup at or past it -- what the
	// jumps before that remove moves it by -- is noted as it is bound
	// (Bind), where a walk of the labels and the fixups had found it.
	// Each fixup's label's index of the first fixup at or past it (past,
	// its label's first, where a table of the fixups' own had been written
	// for it); whether its jump may be shortened: a jump too
	// far for a byte's displacement even were every jump between it and
	// its label shortened never is, and is passed over from the first round
	// on, as most -- a guard's to its stub, past the function's blocks --
	// are. The rounds look at the jumps that may be shortened (cand, by
	// fixup), those still open each until it is shortened. Only they remove
	// bytes: what is removed before a fixup is what those before it remove,
	// a sum over them alone (sums, by place in cand, made again each round),
	// and candPast is, by place in cand, how many of them are before its
	// label's fixup (past). A round shortens what fits as the round before
	// left them; the last shortens none.
	a.candPast = grow(a.candPast, n+1)
	cand := a.open[:0]
	for i, f := range a.fixups {
		at := int(a.labels[f.label].pos)
		if at < 0 {
			return nil, fmt.Errorf("amd64: label %d never bound", f.label)
		}
		past := a.labels[f.label].first
		a.candPast[i] = int32(len(cand))
		if f.kind() == fixLea || f.kind() == fixAlign {
			continue
		}
		d, between := at-(int(f.start)+2), int(past)-i-1
		if d < 0 {
			d, between = -d, i-int(past)
		}
		if d-4*max(between, 0)-4 <= 127 {
			cand = append(cand, int32(i))
		}
	}
	m := len(cand)
	a.candPast[n] = int32(m)
	a.sums = grow(a.sums, m+1)
	for q, j := range cand {
		// How many are before the fixup's label's: those before the
		// fixup it is by index, taken before they are overwritten.
		a.sums[q] = a.candPast[a.labels[a.fixups[j].label].first]
	}
	copy(a.candPast, a.sums[:m])
	a.candPast = a.candPast[:m]
	open := a.openList(m)
	for q := range cand {
		open = append(open, int32(q))
	}
	for changed := true; changed; {
		changed = false
		sum := int32(0)
		for q, j := range cand {
			a.sums[q] = sum
			if a.short[j] {
				sum += int32(shortened(a.fixups[j].kind()))
			}
		}
		a.sums[m] = sum
		kept := open[:0]
		for _, q := range open {
			i := int(cand[q])
			f := a.fixups[i]
			start := int(f.start)
			from := start + 2 - int(a.sums[q])
			to := int(a.labels[f.label].pos)
			d := to - int(a.sums[a.candPast[q]]) - from
			if to > start {
				// A label past it comes nearer by what it loses itself.
				d -= shortened(f.kind())
			}
			if d >= -128 && d <= 127 {
				a.short[i], changed = true, true
			} else {
				kept = append(kept, q)
			}
		}
		open = kept
	}
	a.open, a.openQ = cand[:0], open[:0]
	if !a.aligned {
		a.count()
	} else {
		// The jumps shortened with every alignment's padding whole, each
		// keeps what the code after it, moved, needs: no shortened jump's
		// displacement grows. This walk is count's, with the paddings.
		sum := 0
		for i, f := range a.fixups {
			switch {
			case f.kind() == fixAlign:
				n := int(f.align())
				sum += n - 1 - (n-(int(f.at)-sum)%n)%n
			case a.short[i]:
				sum += shortened(f.kind())
			}
			a.removed[i] = sum
		}
	}
	// The code is the buffer, moved down over what the shortened jumps
	// remove: what is written never passes what is still to be read, and
	// a jump's own bytes are read before they are written over.
	// Until the first jump shortened or padding cut, the code is where it
	// was, and is not copied onto itself. Only the jumps shortened and the
	// paddings remove bytes: the code is moved in stretches between them,
	// and every other fixup's displacement written after, where its code
	// has gone -- moved past each fixup, seven in ten of which keep their
	// length, a copy's call had moved some seventeen bytes each.
	buf, w, last := a.buf, 0, 0
	for i, f := range a.fixups {
		if f.kind() != fixAlign && !a.short[i] {
			continue
		}
		start := int(f.start)
		if w != last {
			w += copy(buf[w:], buf[last:start])
		} else {
			w = start
		}
		if f.kind() == fixAlign {
			n := int(f.align())
			k := (n - w%n) % n
			pad(buf[w : w+k])
			w, last = w+k, int(f.at)+n-1
			continue
		}
		to := a.movedLabel(a.labels[f.label])
		if f.kind() == fixJmp {
			buf[w], buf[w+1] = 0xEB, byte(int8(to-(w+2)))
		} else {
			cc := buf[start+1] & 0x0F
			buf[w], buf[w+1] = 0x70|cc, byte(int8(to-(w+2)))
		}
		w += 2
		last = int(f.at) + 4
	}
	if w != last {
		w += copy(buf[w:], buf[last:])
	} else {
		w = len(buf)
	}
	// The displacements kept 32 bits, each where the bytes removed before
	// its fixup moved it.
	for i, f := range a.fixups {
		if f.kind() == fixAlign || a.short[i] {
			continue
		}
		at := int(f.at)
		if i > 0 {
			at -= a.removed[i-1]
		}
		to := a.movedLabel(a.labels[f.label])
		binary.LittleEndian.PutUint32(buf[at:], uint32(int32(to-(at+4))))
	}
	for _, l := range a.bound {
		a.labels[l].pos = int32(a.movedLabel(a.labels[l]))
	}
	a.buf = buf[:w]
	return a.buf, nil
}

// grow is s with n elements, cleared, from its memory where it has room.
// openList is an empty list of room n, a's own.
func (a *Asm) openList(n int) []int32 {
	if cap(a.openQ) < n {
		a.openQ = make([]int32, 0, n)
	}
	return a.openQ[:0]
}

func grow[T any](s []T, n int) []T {
	if cap(s) < n {
		return make([]T, n, n+n/4)
	}
	s = s[:n]
	clear(s)
	return s
}

// count fills removed: how many bytes the shortened jumps up to each
// fixup's, it included, remove.
func (a *Asm) count() {
	sum := 0
	for i, f := range a.fixups {
		if a.short[i] {
			sum += shortened(f.kind())
		}
		a.removed[i] = sum
	}
}

// after is the index of the first fixup whose jump starts at or past at.
func (a *Asm) after(at int) int {
	lo, hi := 0, len(a.fixups)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if f := a.fixups[m]; int(f.start) < at {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

// movedPast is where position at, before Finish, is in the code it returns,
// i the index of the first fixup at or past it: a position is never inside
// a jump, so it moves back by what the jumps before it remove.
func (a *Asm) movedPast(at, i int) int {
	if i == 0 {
		return at
	}
	return at - a.removed[i-1]
}

// movedLabel is where a bound label is in the code Finish returns.
func (a *Asm) movedLabel(l label) int { return a.movedPast(int(l.pos), int(l.first)) }

// moved is movedPast, the fixup found.
func (a *Asm) moved(at int) int { return a.movedPast(at, a.after(at)) }

// Moved is where position at, a Len taken before Finish, is in the code
// Finish returned.
func (a *Asm) Moved(at int) int { return a.moved(at) }

// emit, emit2 and emit3 append one, two and three bytes: appends of a
// fixed number, which the compiler makes a capacity check and stores, not
// a variadic append's copy of a slice made for it.
func (a *Asm) emit(b byte) { a.buf = append(a.buf, b) }

func (a *Asm) emit2(b0, b1 byte) { a.buf = append(a.buf, b0, b1) }

func (a *Asm) emit3(b0, b1, b2 byte) { a.buf = append(a.buf, b0, b1, b2) }

func (a *Asm) imm32(v int32) { a.buf = binary.LittleEndian.AppendUint32(a.buf, uint32(v)) }

// rex emits a REX prefix when one is needed: w for 64-bit operands, r for
// the ModRM reg field, b for the r/m (or base) field. force emits one even
// without bits, so that SPL, BPL, SIL and DIL are byte registers rather
// than AH, CH, DH and BH.
func (a *Asm) rex(w bool, r, b byte, force bool) {
	v := byte(0x40)
	if w {
		v |= 8
	}
	if r&8 != 0 {
		v |= 4
	}
	if b&8 != 0 {
		v |= 1
	}
	if v != 0x40 || force {
		a.emit(v)
	}
}

// modrmRR is ModRM for two registers.
func (a *Asm) modrmRR(reg, rm byte) { a.emit(0xC0 | (reg&7)<<3 | rm&7) }

// modrmMem is ModRM (and SIB) for [base + disp]: a byte of displacement
// where it fits, as most of a frame's slots' do, else four. RSP and R12 as
// a base need a SIB byte; RBP and R13 are fine with a displacement, which
// is always given.
func (a *Asm) modrmMem(reg byte, base Reg, disp int32) {
	short := disp == int32(int8(disp))
	mod := byte(0x80)
	if short {
		mod = 0x40
	}
	a.emit(mod | (reg&7)<<3 | byte(base)&7)
	if base&7 == 4 {
		a.emit(0x24)
	}
	if short {
		a.emit(byte(disp))
	} else {
		a.imm32(disp)
	}
}

// Ret returns.
func (a *Asm) Ret() { a.emit(0xC3) }

// Jmp jumps to l.
func (a *Asm) Jmp(l Label) {
	a.emit(0xE9)
	a.fix(l, fixJmp, 0)
	a.imm32(0)
}

// JmpReg jumps to the address in r.
func (a *Asm) JmpReg(r Reg) {
	if r >= 8 {
		a.emit(0x41)
	}
	a.emit2(0xFF, 0xE0|byte(r&7))
}

// JmpMem jumps to the address at [base + disp].
func (a *Asm) JmpMem(base Reg, disp int32) {
	a.rex(false, 0, byte(base), false)
	a.emit(0xFF)
	a.modrmMem(4, base, disp)
}

// LeaLabel is dst = the address l is bound at, RIP-relative.
func (a *Asm) LeaLabel(dst Reg, l Label) {
	rex := byte(0x48)
	if dst >= 8 {
		rex |= 0x04
	}
	a.emit3(rex, 0x8D, byte(dst&7)<<3|5)
	a.fix(l, fixLea, 0)
	a.imm32(0)
}

// LoadLabel is dst = the word at the address l is bound at, RIP-relative:
// a constant of the code's own, seven bytes where a 64-bit immediate takes
// ten.
func (a *Asm) LoadLabel(dst Reg, l Label) {
	rex := byte(0x48)
	if dst >= 8 {
		rex |= 0x04
	}
	a.emit3(rex, 0x8B, byte(dst&7)<<3|5)
	a.fix(l, fixLea, 0)
	a.imm32(0)
}

// LoadSDLabel is dst = the double at the address l is bound at,
// RIP-relative (movsd dst, [l]): a constant of the code's own, loaded
// into the SSE register that wants it, where a load into a general
// register and a move from it had taken two.
func (a *Asm) LoadSDLabel(dst XReg, l Label) {
	a.emit(0xF2)
	if dst >= 8 {
		a.emit(0x44)
	}
	a.emit3(0x0F, 0x10, byte(dst&7)<<3|5)
	a.fix(l, fixLea, 0)
	a.imm32(0)
}

// CmpLabel compares r with the word at the address l is bound at,
// RIP-relative (cmp r, [l]): the flags are r's less the word's.
func (a *Asm) CmpLabel(r Reg, l Label) {
	rex := byte(0x48)
	if r >= 8 {
		rex |= 0x04
	}
	a.emit3(rex, 0x3B, byte(r&7)<<3|5)
	a.fix(l, fixLea, 0)
	a.imm32(0)
}

// Quad emits the word v as data.
func (a *Asm) Quad(v uint64) { a.buf = binary.LittleEndian.AppendUint64(a.buf, v) }

// Jcc jumps to l if c holds.
func (a *Asm) Jcc(c Cond, l Label) {
	a.emit2(0x0F, 0x80|byte(c))
	a.fix(l, fixJcc, 0)
	a.imm32(0)
}

// MovRR is dst = src, 64 bits.
func (a *Asm) MovRR(dst, src Reg) {
	a.rex(true, byte(src), byte(dst), false)
	a.emit(0x89)
	a.modrmRR(byte(src), byte(dst))
}

// MovRR32 is dst = src, 32 bits, clearing dst's upper half.
func (a *Asm) MovRR32(dst, src Reg) {
	a.rex(false, byte(src), byte(dst), false)
	a.emit(0x89)
	a.modrmRR(byte(src), byte(dst))
}

// MovImm sets dst to a 64-bit constant, in the shortest form: a 32-bit move
// for values that zero-extend, a sign-extended 32-bit move, or movabs.
func (a *Asm) MovImm(dst Reg, v uint64) {
	switch {
	case v>>32 == 0:
		a.rex(false, 0, byte(dst), false)
		a.emit(0xB8 + byte(dst)&7)
		a.imm32(int32(uint32(v)))
	case int64(v) == int64(int32(v)):
		a.rex(true, 0, byte(dst), false)
		a.emit(0xC7)
		a.modrmRR(0, byte(dst))
		a.imm32(int32(v))
	default:
		a.rex(true, 0, byte(dst), false)
		a.emit(0xB8 + byte(dst)&7)
		a.buf = binary.LittleEndian.AppendUint64(a.buf, v)
	}
}

// MovAddr sets dst to a 64-bit constant in ten bytes, whatever its value:
// an address, so that the code's length does not depend on where memory
// is.
func (a *Asm) MovAddr(dst Reg, v uint64) {
	a.rex(true, 0, byte(dst), false)
	a.emit(0xB8 + byte(dst)&7)
	a.buf = binary.LittleEndian.AppendUint64(a.buf, v)
}

// Load is dst = [base + disp], 64 bits.
func (a *Asm) Load(dst, base Reg, disp int32) {
	a.rex(true, byte(dst), byte(base), false)
	a.emit(0x8B)
	a.modrmMem(byte(dst), base, disp)
}

// Lea is dst = base + disp.
func (a *Asm) Lea(dst, base Reg, disp int32) {
	a.rex(true, byte(dst), byte(base), false)
	a.emit(0x8D)
	a.modrmMem(byte(dst), base, disp)
}

// LoadU32 is dst = [base + disp], 32 bits, zero-extended.
func (a *Asm) LoadU32(dst, base Reg, disp int32) {
	a.rex(false, byte(dst), byte(base), false)
	a.emit(0x8B)
	a.modrmMem(byte(dst), base, disp)
}

// Cqo sign-extends RAX into RDX.
func (a *Asm) Cqo() { a.emit2(0x48, 0x99) }

// Idiv divides RDX:RAX by src, signed, 64 bits: RAX is the quotient and RDX
// the remainder. It faults on a zero divisor, or a quotient past 64 bits.
func (a *Asm) Idiv(src Reg) {
	a.rex(true, 0, byte(src), false)
	a.emit(0xF7)
	a.modrmRR(7, byte(src))
}

// Div divides RDX:RAX by src, unsigned, 64 bits: RAX is the quotient and
// RDX the remainder. It faults on a zero divisor, or a quotient past 64
// bits.
func (a *Asm) Div(src Reg) {
	a.rex(true, 0, byte(src), false)
	a.emit(0xF7)
	a.modrmRR(6, byte(src))
}

// Mul is RDX:RAX = RAX * src, unsigned, 64 bits each: RDX the product's
// high word, RAX its low.
func (a *Asm) Mul(src Reg) {
	a.rex(true, 0, byte(src), false)
	a.emit(0xF7)
	a.modrmRR(4, byte(src))
}

// LoadU16 is dst = [base + disp], 16 bits, zero-extended.
func (a *Asm) LoadU16(dst, base Reg, disp int32) {
	a.rex(false, byte(dst), byte(base), false)
	a.emit2(0x0F, 0xB7)
	a.modrmMem(byte(dst), base, disp)
}

// LoadU8 is dst = [base + disp], a byte, zero-extended.
func (a *Asm) LoadU8(dst, base Reg, disp int32) {
	a.rex(false, byte(dst), byte(base), false)
	a.emit2(0x0F, 0xB6)
	a.modrmMem(byte(dst), base, disp)
}

// LoadS8, LoadS16 and LoadS32 are dst = [base + disp], a byte, 16 bits or
// 32, sign-extended to 64 bits: a typed array's signed elements.
func (a *Asm) LoadS8(dst, base Reg, disp int32) {
	a.rex(true, byte(dst), byte(base), false)
	a.emit2(0x0F, 0xBE)
	a.modrmMem(byte(dst), base, disp)
}

func (a *Asm) LoadS16(dst, base Reg, disp int32) {
	a.rex(true, byte(dst), byte(base), false)
	a.emit2(0x0F, 0xBF)
	a.modrmMem(byte(dst), base, disp)
}

// Movsxd is dst = src's low 32 bits, sign-extended to 64.
func (a *Asm) Movsxd(dst, src Reg) {
	a.rex(true, byte(dst), byte(src), false)
	a.emit2(0x63, 0xC0|byte(dst&7)<<3|byte(src&7))
}

func (a *Asm) LoadS32(dst, base Reg, disp int32) {
	a.rex(true, byte(dst), byte(base), false)
	a.emit(0x63)
	a.modrmMem(byte(dst), base, disp)
}

// Store8, Store16 and Store32 are [base + disp] = src's low byte, 16 bits
// or 32: a typed array's elements.
func (a *Asm) Store8(base Reg, disp int32, src Reg) {
	// SPL to DIL, not AH to BH, take a REX prefix.
	a.rex(false, byte(src), byte(base), src >= 4)
	a.emit(0x88)
	a.modrmMem(byte(src), base, disp)
}

func (a *Asm) Store16(base Reg, disp int32, src Reg) {
	a.emit(0x66)
	a.rex(false, byte(src), byte(base), false)
	a.emit(0x89)
	a.modrmMem(byte(src), base, disp)
}

func (a *Asm) Store32(base Reg, disp int32, src Reg) {
	a.rex(false, byte(src), byte(base), false)
	a.emit(0x89)
	a.modrmMem(byte(src), base, disp)
}

// CmpMem sets the flags for r - [base + disp], 64 bits if wide: a bound
// compared where it is, which spares a register.
func (a *Asm) CmpMem(r, base Reg, disp int32, wide bool) {
	a.rex(wide, byte(r), byte(base), false)
	a.emit(0x3B)
	a.modrmMem(byte(r), base, disp)
}

// AddMem is r += [base + disp], 64 bits: an address's part added where it
// is, without a register to load it into.
func (a *Asm) AddMem(r, base Reg, disp int32) {
	a.rex(true, byte(r), byte(base), false)
	a.emit(0x03)
	a.modrmMem(byte(r), base, disp)
}

// Store is [base + disp] = src, 64 bits.
func (a *Asm) Store(base Reg, disp int32, src Reg) {
	a.rex(true, byte(src), byte(base), false)
	a.emit(0x89)
	a.modrmMem(byte(src), base, disp)
}

// StoreImm stores imm, sign-extended to 64 bits, at [base + disp].
func (a *Asm) StoreImm(base Reg, disp int32, imm int32) {
	a.rex(true, 0, byte(base), false)
	a.emit(0xC7)
	a.modrmMem(0, base, disp)
	a.imm32(imm)
}

// TestMem8 sets the flags by the byte at [base + disp] and imm, as TEST
// does: Z where they have no bit in common.
func (a *Asm) TestMem8(base Reg, disp int32, imm uint8) {
	a.rex(false, 0, byte(base), false)
	a.emit(0xF6)
	a.modrmMem(0, base, disp)
	a.emit(imm)
}

// IncMem increments the 64-bit integer at [base + disp].
func (a *Asm) IncMem(base Reg, disp int32) {
	a.rex(true, 0, byte(base), false)
	a.emit(0xFF)
	a.modrmMem(0, base, disp)
}

// DecMem decrements the 64-bit integer at [base + disp], setting the flags.
func (a *Asm) DecMem(base Reg, disp int32) {
	a.rex(true, 0, byte(base), false)
	a.emit(0xFF)
	a.modrmMem(1, base, disp)
}

// ALU is a two-register integer operation.
type ALU uint8

const (
	Add ALU = 0x01
	Or  ALU = 0x09
	And ALU = 0x21
	Sub ALU = 0x29
	Xor ALU = 0x31
	Cmp ALU = 0x39
	// Test sets the flags for dst & src.
	Test ALU = 0x85
)

// subcode is an ALU's ModRM extension in its immediate form.
func (op ALU) subcode() byte {
	switch op {
	case Add:
		return 0
	case Or:
		return 1
	case And:
		return 4
	case Sub:
		return 5
	case Xor:
		return 6
	case Cmp:
		return 7
	}
	panic("amd64: no immediate form")
}

// Op is dst op= src, 64 bits if wide, else 32 (clearing dst's upper half,
// except for Cmp and Test, which only set flags).
// Imul is dst *= src, a signed 64-bit multiply, which sets OF where the
// product does not fit.
func (a *Asm) Imul(dst, src Reg) {
	a.rex(true, byte(dst), byte(src), false)
	a.emit2(0x0F, 0xAF)
	a.modrmRR(byte(dst), byte(src))
}

// Imul32 is dst = dst*src, 32 bits: the product's low half, as Math.imul
// has it, dst's upper half cleared.
func (a *Asm) Imul32(dst, src Reg) {
	a.rex(false, byte(dst), byte(src), false)
	a.emit2(0x0F, 0xAF)
	a.modrmRR(byte(dst), byte(src))
}

// Bsr is dst = the index of src's highest set bit, 64 bits if wide, else
// 32: BSR, which every x86-64 has. Of a src of 0 it sets ZF and leaves dst
// undefined, so a caller gives it a src that cannot be 0. It is not LZCNT,
// which counts the leading zeros instead: LZCNT's bytes are BSR's with an
// F3 prefix, which a CPU without LZCNT ignores, running them as BSR.
func (a *Asm) Bsr(dst, src Reg, wide bool) {
	a.rex(wide, byte(dst), byte(src), false)
	a.emit2(0x0F, 0xBD)
	a.modrmRR(byte(dst), byte(src))
}

func (a *Asm) Op(op ALU, dst, src Reg, wide bool) {
	a.rex(wide, byte(src), byte(dst), false)
	a.emit(byte(op))
	a.modrmRR(byte(src), byte(dst))
}

// OpImm is dst op= imm, sign-extended to 64 bits if wide: in a byte where
// it fits one, signed, as most do -- an increment, a slot's offset.
func (a *Asm) OpImm(op ALU, dst Reg, imm int32, wide bool) {
	a.rex(wide, 0, byte(dst), false)
	if imm >= -128 && imm <= 127 {
		a.emit2(0x83, 0xC0|op.subcode()<<3|byte(dst)&7)
		a.emit(byte(int8(imm)))
		return
	}
	a.emit(0x81)
	a.modrmRR(op.subcode(), byte(dst))
	a.imm32(imm)
}

// Shift is a shift's ModRM extension.
type Shift uint8

const (
	Shl Shift = 4
	Shr Shift = 5 // logical
	Sar Shift = 7 // arithmetic
)

// ShiftCL shifts dst by CL (masked by the hardware to five bits for 32-bit
// operands, six for 64), 64 bits if wide.
func (a *Asm) ShiftCL(op Shift, dst Reg, wide bool) {
	a.rex(wide, 0, byte(dst), false)
	a.emit(0xD3)
	a.modrmRR(byte(op), byte(dst))
}

// ShiftImm shifts dst by a constant, 64 bits if wide.
func (a *Asm) ShiftImm(op Shift, dst Reg, n uint8, wide bool) {
	a.rex(wide, 0, byte(dst), false)
	a.emit(0xC1)
	a.modrmRR(byte(op), byte(dst))
	a.emit(n)
}

// Bt sets CF to bit bit&63 of src: a set of 64 members tested at once, as a
// regular expression's class is (internal/regexp).
func (a *Asm) Bt(src, bit Reg) {
	a.rex(true, byte(bit), byte(src), false)
	a.emit2(0x0F, 0xA3)
	a.modrmRR(byte(bit), byte(src))
}

// Not32 is dst = ^dst, 32 bits.
func (a *Asm) Not32(dst Reg) {
	a.rex(false, 0, byte(dst), false)
	a.emit(0xF7)
	a.modrmRR(2, byte(dst))
}

// Setcc sets dst's low byte to 1 if c holds, else 0; MovZX8 widens it.
func (a *Asm) Setcc(c Cond, dst Reg) {
	a.rex(false, 0, byte(dst), dst >= 4)
	a.emit2(0x0F, 0x90|byte(c))
	a.modrmRR(0, byte(dst))
}

// MovZX8 is dst = src's low byte, zero-extended.
func (a *Asm) MovZX8(dst, src Reg) {
	a.rex(false, byte(dst), byte(src), src >= 4)
	a.emit2(0x0F, 0xB6)
	a.modrmRR(byte(dst), byte(src))
}

// MovQToX moves 64 bits from a general register to an SSE register.
func (a *Asm) MovQToX(dst XReg, src Reg) {
	a.emit(0x66)
	a.rex(true, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x6E)
	a.modrmRR(byte(dst), byte(src))
}

// MovQFromX moves 64 bits from an SSE register to a general register.
func (a *Asm) MovQFromX(dst Reg, src XReg) {
	a.emit(0x66)
	a.rex(true, byte(src), byte(dst), false)
	a.emit2(0x0F, 0x7E)
	a.modrmRR(byte(src), byte(dst))
}

// LoadSD is dst = the double at [base + disp].
func (a *Asm) LoadSD(dst XReg, base Reg, disp int32) {
	a.emit(0xF2)
	a.rex(false, byte(dst), byte(base), false)
	a.emit2(0x0F, 0x10)
	a.modrmMem(byte(dst), base, disp)
}

// StoreSD is [base + disp] = the double in src.
func (a *Asm) StoreSD(base Reg, disp int32, src XReg) {
	a.emit(0xF2)
	a.rex(false, byte(src), byte(base), false)
	a.emit2(0x0F, 0x11)
	a.modrmMem(byte(src), base, disp)
}

// SSE is a two-register double operation.
type SSE uint8

const (
	AddSD   SSE = 0x58
	MulSD   SSE = 0x59
	SubSD   SSE = 0x5C
	DivSD   SSE = 0x5E
	UcomiSD SSE = 0x2E // sets ZF, PF and CF; PF on an unordered (NaN) pair
	XorPD   SSE = 0x57
	MovAPD  SSE = 0x28 // a register copy
	SqrtSD  SSE = 0x51
	AndPD   SSE = 0x54
	OrPD    SSE = 0x56
)

// SSEOp is dst op= src (for UcomiSD, compares dst with src).
func (a *Asm) SSEOp(op SSE, dst, src XReg) {
	switch op {
	case AddSD, MulSD, SubSD, DivSD, SqrtSD:
		a.emit(0xF2)
	default:
		a.emit(0x66)
	}
	a.rex(false, byte(dst), byte(src), false)
	a.emit2(0x0F, byte(op))
	a.modrmRR(byte(dst), byte(src))
}

// Rounding modes of RoundSD: to the nearest, ties to even, and down, up
// and toward zero.
const (
	RoundNearest = 0
	RoundFloor   = 1
	RoundCeil    = 2
	RoundTrunc   = 3
)

// RoundSD rounds src to an integer in dst, as mode says (RoundFloor...),
// an inexact result unreported: SSE4.1's ROUNDSD.
func (a *Asm) RoundSD(dst, src XReg, mode uint8) {
	a.emit(0x66)
	a.rex(false, byte(dst), byte(src), false)
	a.emit3(0x0F, 0x3A, 0x0B)
	a.modrmRR(byte(dst), byte(src))
	a.emit(mode | 8)
}

// Cvtsd2ss converts a double to the nearest float, in dst's low 32 bits.
func (a *Asm) Cvtsd2ss(dst, src XReg) {
	a.emit(0xF2)
	a.rex(false, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x5A)
	a.modrmRR(byte(dst), byte(src))
}

// Cvtss2sd converts a float, src's low 32 bits, to a double.
func (a *Asm) Cvtss2sd(dst, src XReg) {
	a.emit(0xF3)
	a.rex(false, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x5A)
	a.modrmRR(byte(dst), byte(src))
}

// Cvttsd2si truncates a double to a signed 64-bit integer. Out of range,
// NaN and infinities give 0x8000000000000000.
func (a *Asm) Cvttsd2si(dst Reg, src XReg) {
	a.emit(0xF2)
	a.rex(true, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x2C)
	a.modrmRR(byte(dst), byte(src))
}

// Cvtsi2sd converts a signed integer, 64 bits if wide, else 32, to a double.
func (a *Asm) Cvtsi2sd(dst XReg, src Reg, wide bool) {
	a.emit(0xF2)
	a.rex(wide, byte(dst), byte(src), false)
	a.emit2(0x0F, 0x2A)
	a.modrmRR(byte(dst), byte(src))
}
