// Package arm64 encodes the A64 instructions the new pipeline's arm64 code
// generator (internal/jit/mir) and the WebAssembly baseline compiler
// (wasm/internal/jit) emit: each a 32-bit word, little-endian.
// Golden tests in internal/jit/verify check every encoding against the
// arm64asm disassembler.
package arm64

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"unsafe"
)

// Reg is a general register, X0 to X30. Register 31 is ZR in the
// instructions that take it as an operand.
type Reg uint8

// The registers the encoder names.
const (
	X0  Reg = 0
	X27 Reg = 27 // the encoder's temporary for offsets an instruction cannot hold
	ZR  Reg = 31
)

// tmp is the register the encoder builds large offsets in: Go's assembler
// temporary, which no caller keeps a value in.
const tmp = X27

// FReg is a floating-point register, D0 to D31.
type FReg uint8

// Cond is a condition code.
type Cond uint8

const (
	EQ Cond = iota
	NE
	HS // unsigned >=, carry set
	LO // unsigned <
	MI // negative; after FCMP, less than (false when unordered)
	PL
	VS // overflow; after FCMP, unordered
	VC
	HI // unsigned >
	LS // unsigned <=; after FCMP, less or equal (false when unordered)
	GE
	LT
	GT
	LE
)

// Invert is the opposite condition.
func (c Cond) Invert() Cond { return c ^ 1 }

// Label names a position in the code.
type Label int

// Asm accumulates instructions and resolves labels.
//
// A branch reaches as far as its offset field: B, 26 bits of
// instructions (128 MB either way); B.cond, CBZ and CBNZ, 19 (1 MB); ADR,
// 21 bits of bytes (1 MB). A function's code may be longer than any of
// them, so the assembler keeps every branch in reach itself, as V8's
// arm64 assembler does with its veneer pools. A branch back to a bound
// label past its reach is made of ones that reach: a conditional one, the
// opposite condition over a B; a B past 128 MB, the target's address
// built in the encoder's temporary (farJump). A branch forward to a label
// not yet bound goes in a queue, oldest first; once the oldest of either
// kind has come within a margin of its reach, the instruction being
// emitted is followed by a pool: a B over it, then a veneer for each
// branch of the queue that has come a quarter of its reach -- a B to its
// label, which the branch is pointed at instead, one for all of the
// pool's branches to a label. A veneer is a B, in the queue of Bs, which
// a later pool takes in turn once it has come 32 MB. A branch whose label
// is bound before a pool takes it stays as it is: code shorter than a
// quarter of a conditional branch's reach, 256 KB -- every function's but
// the largest -- is the same as without pools.
//
// An ADR reaches as far as a conditional branch, and is kept in reach as
// far as what it is the address of allows. Back past its reach, the
// address is built of an ADR and subtractions (farAdr). Forward, an Adr's
// address is itself the result, which no veneer can stand for: the
// backend's are of labels a few instructions on, and past its reach Finish
// refuses one. But an AdrJump's address is only jumped to, so a veneer may
// stand for its label as for a branch's; and an Adr of a Table, data the
// assembler places itself, has the table placed in the pool that would
// have taken it, as V8's constant pools are -- or, in code no pool needs
// to come in, after the code, where the caller places the rest (Tables).
//
// A pool comes after any instruction but within a run that must stay
// together, which the assembler blocks pools in (Data's words, its own
// sequences): between two of the caller's instructions it is a jump over
// it, which changes no register or flag, and every jump to a label still
// goes where the label is. So the caller never jumps by a count of
// instructions, nor keeps a value in the temporary at a branch's target.
type Asm struct {
	buf    []uint32
	labels []int
	fixups []fixup
	// near and far are the fixups, by their index, of conditional
	// branches and of Bs to labels unbound when they were made, oldest
	// first from nearAt and farAt: what a pool may have to take.
	near, far     []int
	nearAt, farAt int
	// due is the instruction from which the next pool is looked for, or 0
	// for none; blocked counts the runs pools wait for.
	due, blocked int
	// veneers is a pool's veneer of each label it took a branch to.
	veneers map[Label]Label
	// tables is the data of each label Table made, which a pool or
	// Tables places; tableOrder is their labels in the order made.
	tables     map[Label][]byte
	tableOrder []Label
	// narrow, which a test sets, narrows the branches' reaches by so many
	// bits.
	narrow uint
}

// A pool is looked for once the oldest branch of a queue has come nearDue
// (farDue) instructions, and takes the branches that have come nearVeneer
// (farVeneer). A branch it leaves has come less than nearVeneer; the next
// pool comes once that branch, or an older one, has come nearDue, a few
// instructions more past a run that blocks it, and its veneers are of the
// branches between nearVeneer and nearDue from it, which it puts first:
// the branch's veneer is at most 3/4 of the reach from it, a quarter left
// for runs that block a pool, which are a few hundred instructions at
// most. An assembler a test made narrows them by its narrow bits, to have
// pools in small code (narrowReach).
const (
	nearReach = 1 << 18
	farReach  = 1 << 25
)

// The assembler's reaches, dues and veneers, near and far: the
// instructions', narrowed by a test's narrow.
func (a *Asm) nearReach() int  { return nearReach >> a.narrow }
func (a *Asm) nearDue() int    { return a.nearReach() / 2 }
func (a *Asm) nearVeneer() int { return a.nearReach() / 4 }
func (a *Asm) farReach() int   { return farReach >> a.narrow }
func (a *Asm) farDue() int     { return a.farReach() / 2 }
func (a *Asm) farVeneer() int  { return a.farReach() / 4 }

type fixup struct {
	at    int // instruction index
	label Label
	kind  int
}

const (
	fixB       = iota // imm26
	fixCond           // imm19 at bit 5
	fixAdr            // ADR's imm21, of bytes: immhi at bit 5, immlo (0) at 29
	fixAdrJump        // fixAdr's, of an address only jumped to (AdrJump)
)

// Len is the bytes emitted so far.
func (a *Asm) Len() int { return 4 * len(a.buf) }

// Reset empties the assembler for the next function, keeping its memory.
func (a *Asm) Reset() {
	a.buf, a.labels, a.fixups = a.buf[:0], a.labels[:0], a.fixups[:0]
	a.near, a.far, a.nearAt, a.farAt, a.due, a.blocked = a.near[:0], a.far[:0], 0, 0, 0, 0
	clear(a.tables)
	a.tableOrder = a.tableOrder[:0]
}

// Bytes is the memory the assembler keeps for the next function.
func (a *Asm) Bytes() int {
	return cap(a.buf)*int(unsafe.Sizeof(a.buf[:1][0])) + cap(a.labels)*int(unsafe.Sizeof(0)) + cap(a.fixups)*int(unsafe.Sizeof(fixup{})) +
		(cap(a.near)+cap(a.far))*int(unsafe.Sizeof(0))
}

// Used is what of Bytes the last function used: its tables' lengths.
func (a *Asm) Used() int {
	return len(a.buf)*int(unsafe.Sizeof(a.buf[:1][0])) + len(a.labels)*int(unsafe.Sizeof(0)) + len(a.fixups)*int(unsafe.Sizeof(fixup{})) +
		(len(a.near)+len(a.far))*int(unsafe.Sizeof(0))
}

// NewLabel makes an unbound label.
func (a *Asm) NewLabel() Label {
	a.labels = append(a.labels, -1)
	return Label(len(a.labels) - 1)
}

// Bind places a label at the next instruction.
func (a *Asm) Bind(l Label) {
	if a.labels[l] >= 0 {
		panic("arm64: label bound twice")
	}
	a.labels[l] = len(a.buf)
}

// Offset is where label l is bound, in bytes from the start.
func (a *Asm) Offset(l Label) int { return 4 * a.labels[l] }

// Finish resolves the labels and returns the code.
func (a *Asm) Finish() ([]byte, error) {
	return a.FinishInto(nil)
}

// FinishInto is Finish using dst's backing array when it has room. The
// returned code belongs to the caller and is overwritten if dst is reused.
func (a *Asm) FinishInto(dst []byte) ([]byte, error) {
	for _, f := range a.fixups {
		to := a.labels[f.label]
		if to < 0 {
			return nil, fmt.Errorf("arm64: label %d not bound", f.label)
		}
		d := to - f.at
		switch f.kind {
		case fixB:
			if d < -a.farReach() || d >= a.farReach() {
				return nil, fmt.Errorf("arm64: branch out of range")
			}
			a.buf[f.at] |= uint32(d) & (1<<26 - 1)
		case fixCond:
			if d < -a.nearReach() || d >= a.nearReach() {
				return nil, fmt.Errorf("arm64: conditional branch out of range")
			}
			a.buf[f.at] |= (uint32(d) & (1<<19 - 1)) << 5
		case fixAdr, fixAdrJump:
			// d instructions are 4d bytes: immlo is 0, immhi d.
			if d < -a.nearReach() || d >= a.nearReach() {
				return nil, fmt.Errorf("arm64: address out of range")
			}
			a.buf[f.at] |= (uint32(d) & (1<<19 - 1)) << 5
		}
	}
	out := slices.Grow(dst[:0], 4*len(a.buf))[:4*len(a.buf)]
	for i, w := range a.buf {
		binary.LittleEndian.PutUint32(out[4*i:], w)
	}
	return out, nil
}

func (a *Asm) emit(w uint32) {
	a.buf = append(a.buf, w)
	if a.due != 0 && len(a.buf) >= a.due && a.blocked == 0 {
		a.pool()
	}
}

// block holds pools off until unblock, for a run of words that must stay
// together.
func (a *Asm) block() { a.blocked++ }

// unblock ends block's run, and makes the pool it held off.
func (a *Asm) unblock() {
	a.blocked--
	if a.blocked == 0 && a.due != 0 && len(a.buf) >= a.due {
		a.pool()
	}
}

// Data emits b, a whole number of words, as data among the instructions: a
// table the code reads (Adr finds it), which no pool comes into.
func (a *Asm) Data(b []byte) {
	if len(b)%4 != 0 {
		panic("arm64: data not a whole number of words")
	}
	a.block()
	for i := 0; i < len(b); i += 4 {
		a.emit(binary.LittleEndian.Uint32(b[i:]))
	}
	a.unblock()
}

// Table makes a label for b, data of a whole number of words, which the
// assembler places itself: in the first pool an Adr of the label needs it
// in, or at Tables. Only Adr refers to the label.
func (a *Asm) Table(b []byte) Label {
	if len(b)%4 != 0 {
		panic("arm64: table not a whole number of words")
	}
	l := a.NewLabel()
	if a.tables == nil {
		a.tables = map[Label][]byte{}
	}
	a.tables[l] = b
	a.tableOrder = append(a.tableOrder, l)
	return l
}

// isTable reports whether l is a Table's.
func (a *Asm) isTable(l Label) bool {
	_, ok := a.tables[l]
	return ok
}

// Tables places, here and in the order Table made them, the tables no pool
// has placed.
func (a *Asm) Tables() {
	for _, l := range a.tableOrder {
		if a.labels[l] < 0 {
			a.Bind(l)
			a.Data(a.tables[l])
		}
	}
}

// branch emits w, a branch of kind to l: noted for Finish, and in its
// queue while l is unbound and a pool can keep it in reach; back to a
// bound label past w's reach, made of instructions that reach (farBack,
// farAdr).
func (a *Asm) branch(w uint32, l Label, kind int) {
	if to := a.labels[l]; to >= 0 {
		d := to - len(a.buf)
		if kind == fixB && d < -a.farReach() || kind == fixCond && d < -a.nearReach() {
			a.farBack(w, l, kind)
			return
		}
		if (kind == fixAdr || kind == fixAdrJump) && d < -a.nearReach() {
			a.block()
			a.farAdr(Reg(w&31), l)
			a.unblock()
			return
		}
	} else if kind != fixAdr || a.isTable(l) {
		due := len(a.buf) + a.farDue()
		if kind != fixB {
			a.near = append(a.near, len(a.fixups))
			due = len(a.buf) + a.nearDue()
		} else {
			a.far = append(a.far, len(a.fixups))
		}
		if a.due == 0 || due < a.due {
			a.due = due
		}
	}
	a.fixups = append(a.fixups, fixup{len(a.buf), l, kind})
	a.emit(w)
}

// farBack emits w, a branch to l, which is bound behind past w's reach: a
// conditional branch as its opposite over a B, a B past 128 MB as a jump
// through the temporary (farJump).
func (a *Asm) farBack(w uint32, l Label, kind int) {
	a.block()
	if kind == fixCond {
		// B.cond's condition is its low four bits; CBZ and CBNZ differ in
		// bit 24.
		inv := w ^ 1
		if w&0x7E000000 == 0x34000000 {
			inv = w ^ 1<<24
		}
		at := len(a.buf)
		a.emit(inv)
		if a.labels[l]-len(a.buf) < -a.farReach() {
			a.farJump(l)
		} else {
			a.fixups = append(a.fixups, fixup{len(a.buf), l, fixB})
			a.emit(0x14000000)
		}
		a.buf[at] |= uint32(len(a.buf)-at) << 5
	} else {
		a.farJump(l)
	}
	a.unblock()
}

// farJump jumps to l, which is bound behind past a B's reach: its address
// built in the temporary (farAdr), and a BR.
func (a *Asm) farJump(l Label) {
	a.farAdr(tmp, l)
	a.Br(tmp)
}

// farAdr is dst = the address of l, which is bound behind: an ADR of the
// part under 4 KiB, then subtractions of multiples of 4 KiB, under 16 MB
// each. No flag is changed. The caller blocks pools around it.
func (a *Asm) farAdr(dst Reg, l Label) {
	m := 4 * (len(a.buf) - a.labels[l])
	a.emit(0x10000000 | (uint32(-(m%4096)/4)&(1<<19-1))<<5 | uint32(dst))
	for k := m / 4096; k > 0; {
		step := min(k, 0xFFF)
		a.emit(0xD1400000 | uint32(step)<<10 | uint32(dst)<<5 | uint32(dst))
		k -= step
	}
}

// pool makes a pool after the last instruction, if a branch of the queues
// has come far enough to need one, and drops from them the branches whose
// labels are bound. A table an Adr of the pool's takes is placed in it.
func (a *Asm) pool() {
	a.blocked++
	pos, over := len(a.buf), Label(-1)
	if a.veneers == nil {
		a.veneers = map[Label]Label{}
	}
	clear(a.veneers)
	take := func(i int) {
		l := a.fixups[i].label
		if a.labels[l] >= 0 {
			return
		}
		if over < 0 {
			over = a.NewLabel()
			a.fixups = append(a.fixups, fixup{len(a.buf), over, fixB})
			a.buf = append(a.buf, 0x14000000)
		}
		if b, ok := a.tables[l]; ok {
			a.labels[l] = len(a.buf)
			for j := 0; j < len(b); j += 4 {
				a.buf = append(a.buf, binary.LittleEndian.Uint32(b[j:]))
			}
			return
		}
		v, ok := a.veneers[l]
		if !ok {
			v = a.NewLabel()
			a.labels[v] = len(a.buf)
			a.far = append(a.far, len(a.fixups))
			a.fixups = append(a.fixups, fixup{len(a.buf), l, fixB})
			a.buf = append(a.buf, 0x14000000)
			a.veneers[l] = v
		}
		a.fixups[i].label = v
	}
	for a.nearAt < len(a.near) && pos-a.fixups[a.near[a.nearAt]].at >= a.nearVeneer() {
		take(a.near[a.nearAt])
		a.nearAt++
	}
	for a.farAt < len(a.far) && pos-a.fixups[a.far[a.farAt]].at >= a.farVeneer() {
		take(a.far[a.farAt])
		a.farAt++
	}
	if over >= 0 {
		a.labels[over] = len(a.buf)
	}
	for a.nearAt < len(a.near) && a.labels[a.fixups[a.near[a.nearAt]].label] >= 0 {
		a.nearAt++
	}
	for a.farAt < len(a.far) && a.labels[a.fixups[a.far[a.farAt]].label] >= 0 {
		a.farAt++
	}
	a.due = 0
	if a.nearAt < len(a.near) {
		a.due = a.fixups[a.near[a.nearAt]].at + a.nearDue()
	}
	if a.farAt < len(a.far) {
		if due := a.fixups[a.far[a.farAt]].at + a.farDue(); a.due == 0 || due < a.due {
			a.due = due
		}
	}
	a.blocked--
}

func sf(wide bool) uint32 {
	if wide {
		return 1 << 31
	}
	return 0
}

// ---------------------------------------------------------------- moves

// MovRR is dst = src, 64 bits.
func (a *Asm) MovRR(dst, src Reg) { a.emit(0xAA0003E0 | uint32(src)<<16 | uint32(dst)) }

// MovRR32 is dst = src, 32 bits, clearing dst's upper half.
func (a *Asm) MovRR32(dst, src Reg) { a.emit(0x2A0003E0 | uint32(src)<<16 | uint32(dst)) }

// MovImm sets dst to a 64-bit constant: MOVZ or MOVN, then MOVK for each
// other half-word that is not already right.
func (a *Asm) MovImm(dst Reg, v uint64) {
	zeros, ones := 0, 0
	for i := 0; i < 4; i++ {
		switch uint16(v >> (16 * i)) {
		case 0:
			zeros++
		case 0xFFFF:
			ones++
		}
	}
	fill, first := uint16(0), uint32(0xD2800000) // MOVZ
	if ones > zeros {
		fill, first = 0xFFFF, 0x92800000 // MOVN
	}
	emitted := false
	for i := 0; i < 4; i++ {
		h := uint16(v >> (16 * i))
		if h == fill {
			continue
		}
		if !emitted {
			imm := uint32(h)
			if fill == 0xFFFF {
				imm = uint32(^h)
			}
			a.emit(first | uint32(i)<<21 | imm<<5 | uint32(dst))
			emitted = true
			continue
		}
		a.emit(0xF2800000 | uint32(i)<<21 | uint32(h)<<5 | uint32(dst)) // MOVK
	}
	if !emitted {
		if fill == 0 {
			a.emit(0xD2800000 | uint32(dst)) // MOVZ #0
		} else {
			a.emit(0x92800000 | uint32(dst)) // MOVN #0: all ones
		}
	}
}

// MovAddr sets dst to a 64-bit constant in four instructions, whatever its
// value: an address, so that the code's length does not depend on where
// memory is.
func (a *Asm) MovAddr(dst Reg, v uint64) {
	a.emit(0xD2800000 | uint32(uint16(v))<<5 | uint32(dst)) // MOVZ
	for i := 1; i < 4; i++ {
		a.emit(0xF2800000 | uint32(i)<<21 | uint32(uint16(v>>(16*i)))<<5 | uint32(dst)) // MOVK
	}
}

// ---------------------------------------------------------------- memory

// size is a load's or store's access width.
type size uint32

const (
	b8  size = 0
	b16 size = 1
	b32 size = 2
	b64 size = 3
)

// mem emits a load or store of r at [base + disp]: LDR/STR with a scaled
// unsigned offset when it fits, LDUR/STUR for a small unscaled one, and
// otherwise through tmp.
func (a *Asm) mem(scaled, unscaled uint32, sz size, r, base Reg, disp int32) {
	if base == ZR {
		panic("arm64: SP as a base")
	}
	scale := int32(1) << sz
	if disp >= 0 && disp%scale == 0 && disp/scale < 4096 {
		a.emit(scaled | uint32(disp/scale)<<10 | uint32(base)<<5 | uint32(r))
		return
	}
	if disp >= -256 && disp < 256 {
		a.emit(unscaled | (uint32(disp)&0x1FF)<<12 | uint32(base)<<5 | uint32(r))
		return
	}
	if base == tmp {
		panic("arm64: a large offset from the temporary")
	}
	a.MovImm(tmp, uint64(int64(disp)))
	a.emit(0x8B000000 | uint32(tmp)<<16 | uint32(base)<<5 | uint32(tmp)) // ADD tmp, base, tmp
	a.emit(scaled | uint32(tmp)<<5 | uint32(r))
}

// Load is dst = [base + disp], 64 bits.
func (a *Asm) Load(dst, base Reg, disp int32) { a.mem(0xF9400000, 0xF8400000, b64, dst, base, disp) }

// Store is [base + disp] = src, 64 bits.
func (a *Asm) Store(base Reg, disp int32, src Reg) {
	a.mem(0xF9000000, 0xF8000000, b64, src, base, disp)
}

// LoadU32 is dst = [base + disp], 32 bits, zero-extended.
func (a *Asm) LoadU32(dst, base Reg, disp int32) { a.mem(0xB9400000, 0xB8400000, b32, dst, base, disp) }

// LoadU16 is dst = [base + disp], 16 bits, zero-extended.
func (a *Asm) LoadU16(dst, base Reg, disp int32) { a.mem(0x79400000, 0x78400000, b16, dst, base, disp) }

// LoadU8 is dst = [base + disp], a byte, zero-extended.
func (a *Asm) LoadU8(dst, base Reg, disp int32) { a.mem(0x39400000, 0x38400000, b8, dst, base, disp) }

// LoadS8, LoadS16 and LoadS32 are dst = [base + disp], a byte, 16 bits or
// 32, sign-extended to 64 bits (LDRSB, LDRSH, LDRSW): a typed array's
// signed elements.
func (a *Asm) LoadS8(dst, base Reg, disp int32) { a.mem(0x39800000, 0x38800000, b8, dst, base, disp) }

func (a *Asm) LoadS16(dst, base Reg, disp int32) { a.mem(0x79800000, 0x78800000, b16, dst, base, disp) }

func (a *Asm) LoadS32(dst, base Reg, disp int32) { a.mem(0xB9800000, 0xB8800000, b32, dst, base, disp) }

// Store8, Store16 and Store32 are [base + disp] = src's low byte, 16 bits
// or 32 (STRB, STRH, STR of the W view): a typed array's elements.
func (a *Asm) Store8(base Reg, disp int32, src Reg) {
	a.mem(0x39000000, 0x38000000, b8, src, base, disp)
}

func (a *Asm) Store16(base Reg, disp int32, src Reg) {
	a.mem(0x79000000, 0x78000000, b16, src, base, disp)
}

func (a *Asm) Store32(base Reg, disp int32, src Reg) {
	a.mem(0xB9000000, 0xB8000000, b32, src, base, disp)
}

// LoadIdx is dst = [base + idx<<3] where scaled, else [base + idx], 64
// bits (LDR, register offset): an entry of a table of words.
func (a *Asm) LoadIdx(dst, base, idx Reg, scaled bool) {
	s := uint32(0)
	if scaled {
		s = 1 << 12
	}
	a.emit(0xF8606800 | uint32(idx)<<16 | s | uint32(base)<<5 | uint32(dst))
}

// Ldxr is dst = [base], 64 bits, its address marked for an exclusive
// store (LDXR); Stxr is [base] = src where the mark holds, status 0, else
// status 1 and nothing stored (STXR): an atomic update is a loop of the
// two.
func (a *Asm) Ldxr(dst, base Reg) { a.emit(0xC85F7C00 | uint32(base)<<5 | uint32(dst)) }

func (a *Asm) Stxr(status, src, base Reg) {
	a.emit(0xC8007C00 | uint32(status)<<16 | uint32(base)<<5 | uint32(src))
}

// LoadS and StoreS are dst = the float at [base + disp], and [base + disp]
// = the float in src (the S views).
func (a *Asm) LoadS(dst FReg, base Reg, disp int32) {
	a.mem(0xBD400000, 0xBC400000, b32, Reg(dst), base, disp)
}

func (a *Asm) StoreS(base Reg, disp int32, src FReg) {
	a.mem(0xBD000000, 0xBC000000, b32, Reg(src), base, disp)
}

// LoadF is dst = the double at [base + disp].
func (a *Asm) LoadF(dst FReg, base Reg, disp int32) {
	a.mem(0xFD400000, 0xFC400000, b64, Reg(dst), base, disp)
}

// StoreF is [base + disp] = src, a double.
func (a *Asm) StoreF(base Reg, disp int32, src FReg) {
	a.mem(0xFD000000, 0xFC000000, b64, Reg(src), base, disp)
}

// ---------------------------------------------------------------- integer

// ALU is a three-register integer operation.
type ALU uint32

const (
	Add  ALU = 0x0B000000
	Sub  ALU = 0x4B000000
	Adds ALU = 0x2B000000
	Subs ALU = 0x6B000000
	And  ALU = 0x0A000000
	Orr  ALU = 0x2A000000
	Eor  ALU = 0x4A000000
	Ands ALU = 0x6A000000
)

// Op is dst = n op m.
func (a *Asm) Op(op ALU, dst, n, m Reg, wide bool) {
	a.emit(uint32(op) | sf(wide) | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// AddLsl is dst = n + m<<s, 64 bits (ADD, shifted register): an index
// scaled and added in one.
func (a *Asm) AddLsl(dst, n, m Reg, s uint8) {
	a.emit(uint32(Add) | sf(true) | uint32(m)<<16 | uint32(s&63)<<10 | uint32(n)<<5 | uint32(dst))
}

// Sxtw is dst = n's low 32 bits, sign-extended to 64 (SBFM #0, #31).
func (a *Asm) Sxtw(dst, n Reg) {
	a.emit(0x93407C00 | uint32(n)<<5 | uint32(dst))
}

// Cmp sets the flags for n - m.
func (a *Asm) Cmp(n, m Reg, wide bool) { a.Op(Subs, ZR, n, m, wide) }

// Tst sets the flags for n & m.
func (a *Asm) Tst(n, m Reg, wide bool) { a.Op(Ands, ZR, n, m, wide) }

// AddImm is dst = n + imm, for any imm: an immediate form when it fits
// twelve bits, possibly shifted by twelve, and tmp otherwise. n may be ZR
// only for a sum through tmp.
func (a *Asm) AddImm(dst, n Reg, imm int64, wide bool) {
	a.addImm(0x11000000, 0x51000000, dst, n, imm, wide)
}

// SubsImm is dst = n - imm, for any imm, setting the flags: a count
// taken down and tested in one, as a regular expression's steps are
// (internal/regexp).
func (a *Asm) SubsImm(dst, n Reg, imm int64, wide bool) {
	a.addImm(0x71000000, 0x31000000, dst, n, imm, wide)
}

// AddsImm is dst = n + imm, for any imm, setting the flags: C is the
// carry out of the unsigned sum, for a non-negative imm.
func (a *Asm) AddsImm(dst, n Reg, imm int64, wide bool) {
	a.addImm(0x31000000, 0x71000000, dst, n, imm, wide)
}

// CmpImm sets the flags for n - imm.
func (a *Asm) CmpImm(n Reg, imm int64, wide bool) { a.addImm(0x71000000, 0x31000000, ZR, n, imm, wide) }

// addImm emits op with imm, or its negation with -imm. A 32-bit
// operation's imm is taken modulo 2**32, as its operands are. The
// negation's flags are op's -- the sum and the difference are the same
// integer, and the carry out of n + -imm is the borrow's absence of
// n - imm -- but for the width's least integer, whose negation is itself:
// CMN W, #0x80000000 sets V for no operand, where CMP W, #0x80000000 sets
// it for every one that is not negative, and a signed comparison with
// -2**31 had gone the wrong way. That one is made in tmp.
func (a *Asm) addImm(op, neg uint32, dst, n Reg, imm int64, wide bool) {
	if !wide {
		imm = int64(int32(uint32(imm)))
	}
	if imm < 0 && imm != math.MinInt64 && (wide || imm != math.MinInt32) {
		op, neg, imm = neg, op, -imm
	}
	switch {
	case imm >= 0 && imm < 4096:
		a.emit(op | sf(wide) | uint32(imm)<<10 | uint32(n)<<5 | uint32(dst))
	case imm >= 0 && imm&0xFFF == 0 && imm < 1<<24:
		a.emit(op | sf(wide) | 1<<22 | uint32(imm>>12)<<10 | uint32(n)<<5 | uint32(dst))
	default:
		a.MovImm(tmp, uint64(imm))
		// The register form of the same operation (ADD/SUB/ADDS/SUBS).
		reg := uint32(Add)
		switch op {
		case 0x51000000:
			reg = uint32(Sub)
		case 0x71000000:
			reg = uint32(Subs)
		case 0x31000000:
			reg = uint32(Adds)
		}
		a.emit(reg | sf(wide) | uint32(tmp)<<16 | uint32(n)<<5 | uint32(dst))
	}
}

// Shift is a shift by a register's count, modulo the width.
type Shift uint32

const (
	Lsl Shift = 0x1AC02000
	Lsr Shift = 0x1AC02400
	Asr Shift = 0x1AC02800
)

// ShiftReg is dst = n shifted by m.
func (a *Asm) ShiftReg(op Shift, dst, n, m Reg, wide bool) {
	a.emit(uint32(op) | sf(wide) | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// ShiftImm is dst = n shifted, or rotated (Ror), by a constant count, below
// the width.
func (a *Asm) ShiftImm(op Shift, dst, n Reg, count uint8, wide bool) {
	if op == Ror {
		a.rorImm(dst, n, count, wide)
		return
	}
	bits := uint32(32)
	base := uint32(0x53000000) // UBFM W
	if op == Asr {
		base = 0x13000000 // SBFM W
	}
	if wide {
		bits = 64
		base |= 1<<31 | 1<<22
	}
	s := uint32(count) % bits
	var immr, imms uint32
	if op == Lsl {
		immr, imms = (bits-s)%bits, bits-1-s
	} else {
		immr, imms = s, bits-1
	}
	a.emit(base | immr<<16 | imms<<10 | uint32(n)<<5 | uint32(dst))
}

// Ubfx is dst = n's width bits from bit lsb, zero-extended, 64 bits
// (UBFM); Sbfx is the same sign-extended (SBFM). lsb+width is at most 64.
func (a *Asm) Ubfx(dst, n Reg, lsb, width uint8) { a.bfx(0xD3400000, dst, n, lsb, width) }

func (a *Asm) Sbfx(dst, n Reg, lsb, width uint8) { a.bfx(0x93400000, dst, n, lsb, width) }

func (a *Asm) bfx(op uint32, dst, n Reg, lsb, width uint8) {
	if width == 0 || int(lsb)+int(width) > 64 {
		panic("arm64: a bit field past the register")
	}
	a.emit(op | uint32(lsb)<<16 | uint32(lsb+width-1)<<10 | uint32(n)<<5 | uint32(dst))
}

// LogicalImm is the N:immr:imms field that encodes v as a logical
// operation's immediate of the width, if it has one: a run of ones,
// rotated, repeated in elements of 2 to 64 bits. Zero and all ones have
// none.
func LogicalImm(v uint64, wide bool) (uint32, bool) {
	if !wide {
		v = uint64(uint32(v)) | v<<32
	}
	if v == 0 || v == ^uint64(0) {
		return 0, false
	}
	size := uint(64)
	for size > 2 {
		half := size / 2
		mask := uint64(1)<<half - 1
		if v&mask != v>>half&mask {
			break
		}
		size = half
	}
	mask := ^uint64(0)
	if size < 64 {
		mask = uint64(1)<<size - 1
	}
	e := v & mask
	ones := uint(bits.OnesCount64(e))
	run := uint64(1)<<ones - 1
	for r := uint(0); r < size; r++ {
		rot := e
		if r > 0 {
			rot = (e>>r | e<<(size-r)) & mask
		}
		if rot != run {
			continue
		}
		n := uint32(0)
		if size == 64 {
			n = 1
		}
		immr := uint32((size - r) % size)
		imms := uint32((^(size-1)<<1 | (ones - 1)) & 0x3F)
		return n<<12 | immr<<6 | imms, true
	}
	return 0, false
}

// AndImm is dst = n & imm, and TstImm sets the flags for n & imm (ANDS),
// for any imm: the immediate form where imm has one (LogicalImm), tmp
// otherwise.
func (a *Asm) AndImm(dst, n Reg, imm uint64, wide bool) {
	a.logicalImm(0x12000000, And, dst, n, imm, wide)
}

func (a *Asm) TstImm(n Reg, imm uint64, wide bool) { a.logicalImm(0x72000000, Ands, ZR, n, imm, wide) }

func (a *Asm) logicalImm(op uint32, reg ALU, dst, n Reg, imm uint64, wide bool) {
	if !wide {
		imm = uint64(uint32(imm))
	}
	if f, ok := LogicalImm(imm, wide); ok {
		a.emit(op | sf(wide) | f<<10 | uint32(n)<<5 | uint32(dst))
		return
	}
	if n == tmp {
		panic("arm64: an immediate operation of the temporary")
	}
	a.MovImm(tmp, imm)
	a.Op(reg, dst, n, tmp, wide)
}

// Mvn is dst = ^n.
func (a *Asm) Mvn(dst, n Reg, wide bool) { a.emit(0x2A2003E0 | sf(wide) | uint32(n)<<16 | uint32(dst)) }

// Neg is dst = -n.
func (a *Asm) Neg(dst, n Reg, wide bool) { a.Op(Sub, dst, ZR, n, wide) }

// Cset is dst = 1 if c holds, else 0.
func (a *Asm) Cset(dst Reg, c Cond) {
	a.emit(0x1A9F07E0 | uint32(c.Invert())<<12 | uint32(dst))
}

// Sdiv is dst = n / m, signed, 64 bits: 0 when m is 0, and n when n is
// -2**63 and m is -1; it never faults.
func (a *Asm) Sdiv(dst, n, m Reg) { a.emit(0x9AC00C00 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst)) }

// Udiv is dst = n / m, unsigned, 64 bits: 0 when m is 0; it never faults.
func (a *Asm) Udiv(dst, n, m Reg) { a.emit(0x9AC00800 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst)) }

// Umulh is dst = the high 64 bits of n*m, unsigned.
func (a *Asm) Umulh(dst, n, m Reg) {
	a.emit(0x9BC07C00 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// Mul is dst = n*m, the product's low 64 bits.
func (a *Asm) Mul(dst, n, m Reg) {
	a.emit(0x9B007C00 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// MulW is dst = n*m, 32 bits: the product's low half, as Math.imul has it,
// dst's upper half cleared.
func (a *Asm) MulW(dst, n, m Reg) {
	a.emit(0x1B007C00 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// ClzW is dst = the count of n's leading zero bits, 32 bits: from 0 to 32,
// Math.clz32's of n's ToUint32, dst's upper half cleared.
func (a *Asm) ClzW(dst, n Reg) {
	a.emit(0x5AC01000 | uint32(n)<<5 | uint32(dst))
}

// Smulh is dst = the high 64 bits of n*m, signed: the product fits 64 bits
// where it is the low half's sign, repeated.
func (a *Asm) Smulh(dst, n, m Reg) {
	a.emit(0x9B407C00 | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// Msub is dst = acc - n*m, 64 bits.
func (a *Asm) Msub(dst, n, m, acc Reg) {
	a.emit(0x9B008000 | uint32(m)<<16 | uint32(acc)<<10 | uint32(n)<<5 | uint32(dst))
}

// ---------------------------------------------------------------- floating point

// FMovToF is dst = src's bits.
func (a *Asm) FMovToF(dst FReg, src Reg) { a.emit(0x9E670000 | uint32(src)<<5 | uint32(dst)) }

// FMovFromF is dst = src's bits.
func (a *Asm) FMovFromF(dst Reg, src FReg) { a.emit(0x9E660000 | uint32(src)<<5 | uint32(dst)) }

// FMov is dst = src.
func (a *Asm) FMov(dst, src FReg) { a.emit(0x1E604000 | uint32(src)<<5 | uint32(dst)) }

// FloatImm is the 8-bit immediate FMovImm encodes a double with, if it has
// one: +-(16 to 31)/16 times a power of two from 2**-3 to 2**4, such as 1,
// 0.5, 2, 3 or 10. Zero has none.
func FloatImm(bits uint64) (uint8, bool) {
	if bits&(1<<48-1) != 0 {
		return 0, false
	}
	b := bits >> 54 & 1
	if repl := bits >> 54 & 0xFF; repl != 0 && repl != 0xFF || bits>>62&1 == b {
		return 0, false
	}
	return uint8(bits>>63<<7 | b<<6 | bits>>48&0x3F), true
}

// FMovImm is dst = the double imm encodes (FloatImm).
func (a *Asm) FMovImm(dst FReg, imm uint8) { a.emit(0x1E601000 | uint32(imm)<<13 | uint32(dst)) }

// FOp is a two-operand double operation.
type FOp uint32

const (
	FAdd FOp = 0x1E602800
	FSub FOp = 0x1E603800
	FMul FOp = 0x1E600800
	FDiv FOp = 0x1E601800
	// FMax and FMin are NaN if either operand is, and order -0 below +0.
	FMax FOp = 0x1E604800
	FMin FOp = 0x1E605800
)

// FArith is dst = n op m.
func (a *Asm) FArith(op FOp, dst, n, m FReg) {
	a.emit(uint32(op) | uint32(m)<<16 | uint32(n)<<5 | uint32(dst))
}

// FCmp sets the flags for n compared with m.
func (a *Asm) FCmp(n, m FReg) { a.emit(0x1E602000 | uint32(m)<<16 | uint32(n)<<5) }

// FNeg is dst = -n, the sign flipped, NaN included.
func (a *Asm) FNeg(dst, n FReg) { a.emit(0x1E614000 | uint32(n)<<5 | uint32(dst)) }

// FSqrt is dst = the square root of n.
func (a *Asm) FSqrt(dst, n FReg) { a.emit(0x1E61C000 | uint32(n)<<5 | uint32(dst)) }

// FAbs is dst = n, its sign cleared, NaN included.
func (a *Asm) FAbs(dst, n FReg) { a.emit(0x1E60C000 | uint32(n)<<5 | uint32(dst)) }

// FRound is a rounding of a double to an integer (FRint).
type FRound uint32

const (
	FRintN FRound = 0x1E644000 // to the nearest, ties to even
	FRintM FRound = 0x1E654000 // toward -Inf
	FRintP FRound = 0x1E64C000 // toward +Inf
	FRintZ FRound = 0x1E65C000 // toward zero
)

// FRint is dst = n rounded to an integer, as op says.
func (a *Asm) FRint(op FRound, dst, n FReg) { a.emit(uint32(op) | uint32(n)<<5 | uint32(dst)) }

// FCvtToSingle is dst = n, a double, rounded to a float (dst's S view).
func (a *Asm) FCvtToSingle(dst, n FReg) { a.emit(0x1E624000 | uint32(n)<<5 | uint32(dst)) }

// FCvtFromSingle is dst = n, a float (n's S view), as a double.
func (a *Asm) FCvtFromSingle(dst, n FReg) { a.emit(0x1E22C000 | uint32(n)<<5 | uint32(dst)) }

// Fcvtzs is dst = n truncated to a signed 64-bit integer, saturating; NaN
// gives 0.
func (a *Asm) Fcvtzs(dst Reg, n FReg) { a.emit(0x9E780000 | uint32(n)<<5 | uint32(dst)) }

// Scvtf is dst = n, a signed integer, 64 bits or 32.
func (a *Asm) Scvtf(dst FReg, n Reg, wide bool) {
	a.emit(0x1E620000 | sf(wide) | uint32(n)<<5 | uint32(dst))
}

// Ucvtf is dst = n, an unsigned integer, 64 bits or 32.
func (a *Asm) Ucvtf(dst FReg, n Reg, wide bool) {
	a.emit(0x1E630000 | sf(wide) | uint32(n)<<5 | uint32(dst))
}

// ---------------------------------------------------------------- control

// B jumps to l.
func (a *Asm) B(l Label) { a.branch(0x14000000, l, fixB) }

// BCond jumps to l if c holds.
func (a *Asm) BCond(c Cond, l Label) { a.branch(0x54000000|uint32(c), l, fixCond) }

// Cbz jumps to l if r is zero.
func (a *Asm) Cbz(r Reg, l Label, wide bool) {
	a.branch(0x34000000|sf(wide)|uint32(r), l, fixCond)
}

// Cbnz jumps to l if r is not zero.
func (a *Asm) Cbnz(r Reg, l Label, wide bool) {
	a.branch(0x35000000|sf(wide)|uint32(r), l, fixCond)
}

// Ret returns through X30.
func (a *Asm) Ret() { a.emit(0xD65F03C0) }

// Br jumps to the address in r.
func (a *Asm) Br(r Reg) { a.emit(0xD61F0000 | uint32(r)<<5) }

// Adr is dst = l's address. A label bound ahead must be in reach, unless
// it is a Table's.
func (a *Asm) Adr(dst Reg, l Label) { a.branch(0x10000000|uint32(dst), l, fixAdr) }

// AdrJump is dst = an address that goes on at l when jumped to (Br): l's
// own, or in code longer than an ADR reaches, a veneer's, a B to l.
func (a *Asm) AdrJump(dst Reg, l Label) { a.branch(0x10000000|uint32(dst), l, fixAdrJump) }
