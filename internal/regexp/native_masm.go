//go:build quickjs_jit

package regexp

import (
	"encoding/binary"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// The macro assembler the code generator (native_gen.go) writes through, as
// Irregexp's code generator writes through its RegExpMacroAssembler: one
// generator, and a backend for each machine, so that every instruction's
// code is made the same way on each.
//
// The operations are amd64's two-address forms, which arm64's backend
// makes in one to three instructions with its own scratch registers. The
// flags are set by aluSub, aluAnd, aluCmp and aluTest (and the compare of
// cmpMem), and read by the next jcc, whose conditions have amd64's meanings
// -- condB is unsigned less, which arm64 calls LO.

// vreg is a register the generator keeps a value in, which each backend
// maps to one of its own.
type vreg uint8

// The generator's registers: the context, the subject's start and end, the
// position, the steps left, the stack's top, a character's width, the
// captures, and the scratch rA, rB, rC; the character tests write rT and
// rD.
const (
	rCtx vreg = iota
	rSubj
	rEnd
	rCur
	rLeft
	rTop
	rW
	rCaps
	rA
	rB
	rC
	rT
	rD
	numVregs
)

// cond is a condition, as amd64 names it.
type cond uint8

const (
	condE cond = iota
	condNE
	condA  // unsigned >
	condAE // unsigned >=
	condB  // unsigned <
	condBE // unsigned <=
	condS  // negative
	condNS
	condL // signed <
	condGE
	condLE
	condG
)

// alu is a two-address operation: dst op= src.
type alu uint8

const (
	aluAdd alu = iota
	aluSub
	aluAnd
	aluOr
	aluXor
	// aluCmp and aluTest set the flags of dst-src and dst&src alone.
	aluCmp
	aluTest
)

// shift is a shift's direction; a right shift is logical.
type shift uint8

const (
	shiftLeft shift = iota
	shiftRight
)

// label is a place in the code.
type label int

// masm is what the generator writes the code with.
type masm interface {
	NewLabel() label
	Bind(l label)
	// Offset is where l is bound, in the code Finish returns.
	Offset(l label) int
	Finish() ([]byte, error)
	// Data places b, a whole number of words, among the code.
	Data(b []byte)

	// Load, LoadU32, LoadU16 and LoadU8 are dst = [base+disp], zero
	// extended; Store and StoreImm are [base+disp] = src, 64 bits.
	Load(dst, base vreg, disp int32)
	LoadU32(dst, base vreg, disp int32)
	LoadU16(dst, base vreg, disp int32)
	LoadU8(dst, base vreg, disp int32)
	Store(base vreg, disp int32, src vreg)
	StoreImm(base vreg, disp int32, imm int32)

	MovRR(dst, src vreg)
	MovRR32(dst, src vreg)
	MovImm(dst vreg, v uint64)
	// Lea is dst = base + disp, setting no flags.
	Lea(dst, base vreg, disp int32)
	// LeaLabel is dst = l's address.
	LeaLabel(dst vreg, l label)
	// Op and OpImm are dst op= src or imm, 64 bits if wide, else 32,
	// clearing dst's upper half.
	Op(op alu, dst, src vreg, wide bool)
	OpImm(op alu, dst vreg, imm int32, wide bool)
	ShiftImm(op shift, dst vreg, n uint8, wide bool)
	// CmpMem sets the flags of r - [base+disp].
	CmpMem(r, base vreg, disp int32, wide bool)

	Jcc(c cond, l label)
	Jmp(l label)
	JmpReg(r vreg)
	// JmpMem jumps to the address at [base+disp].
	JmpMem(base vreg, disp int32)
	// JmpBitClear jumps to l if bit bit&63 of src is clear.
	JmpBitClear(src, bit vreg, l label)
	Ret()
}

// newMasm is the macro assembler for arch, or nil where there is none.
func newMasm(arch string) masm {
	switch arch {
	case "amd64":
		return &amd64Masm{}
	case "arm64":
		return &arm64Masm{}
	}
	return nil
}

// amd64Masm writes amd64 code: each operation is one instruction. The
// context is in DI, as enterRaw leaves it.
type amd64Masm struct{ a amd64.Asm }

var amd64Regs = [numVregs]amd64.Reg{
	rCtx: amd64.RDI, rSubj: amd64.RSI, rEnd: amd64.R8, rCur: amd64.RDX, rLeft: amd64.R9, rTop: amd64.R11,
	rW: amd64.R12, rCaps: amd64.R13, rA: amd64.RAX, rB: amd64.RBX, rC: amd64.RCX, rT: amd64.R10, rD: amd64.R15,
}

var amd64Conds = [...]amd64.Cond{
	condE: amd64.CondE, condNE: amd64.CondNE, condA: amd64.CondA, condAE: amd64.CondAE, condB: amd64.CondB,
	condBE: amd64.CondBE, condS: amd64.CondS, condNS: amd64.CondNS, condL: amd64.CondL, condGE: amd64.CondGE,
	condLE: amd64.CondLE, condG: amd64.CondG,
}

var amd64ALUs = [...]amd64.ALU{
	aluAdd: amd64.Add, aluSub: amd64.Sub, aluAnd: amd64.And, aluOr: amd64.Or, aluXor: amd64.Xor,
	aluCmp: amd64.Cmp, aluTest: amd64.Test,
}

func (m *amd64Masm) NewLabel() label         { return label(m.a.NewLabel()) }
func (m *amd64Masm) Bind(l label)            { m.a.Bind(amd64.Label(l)) }
func (m *amd64Masm) Offset(l label) int      { return m.a.Offset(amd64.Label(l)) }
func (m *amd64Masm) Finish() ([]byte, error) { return m.a.Finish() }
func (m *amd64Masm) Load(d, b vreg, x int32) { m.a.Load(amd64Regs[d], amd64Regs[b], x) }
func (m *amd64Masm) LoadU32(d, b vreg, x int32) {
	m.a.LoadU32(amd64Regs[d], amd64Regs[b], x)
}
func (m *amd64Masm) LoadU16(d, b vreg, x int32) {
	m.a.LoadU16(amd64Regs[d], amd64Regs[b], x)
}
func (m *amd64Masm) LoadU8(d, b vreg, x int32)     { m.a.LoadU8(amd64Regs[d], amd64Regs[b], x) }
func (m *amd64Masm) Store(b vreg, x int32, s vreg) { m.a.Store(amd64Regs[b], x, amd64Regs[s]) }
func (m *amd64Masm) StoreImm(b vreg, x, imm int32) { m.a.StoreImm(amd64Regs[b], x, imm) }
func (m *amd64Masm) MovRR(d, s vreg)               { m.a.MovRR(amd64Regs[d], amd64Regs[s]) }
func (m *amd64Masm) MovRR32(d, s vreg)             { m.a.MovRR32(amd64Regs[d], amd64Regs[s]) }
func (m *amd64Masm) MovImm(d vreg, v uint64)       { m.a.MovImm(amd64Regs[d], v) }
func (m *amd64Masm) Lea(d, b vreg, x int32)        { m.a.Lea(amd64Regs[d], amd64Regs[b], x) }
func (m *amd64Masm) LeaLabel(d vreg, l label)      { m.a.LeaLabel(amd64Regs[d], amd64.Label(l)) }
func (m *amd64Masm) Op(op alu, d, s vreg, wide bool) {
	m.a.Op(amd64ALUs[op], amd64Regs[d], amd64Regs[s], wide)
}
func (m *amd64Masm) OpImm(op alu, d vreg, imm int32, wide bool) {
	m.a.OpImm(amd64ALUs[op], amd64Regs[d], imm, wide)
}
func (m *amd64Masm) ShiftImm(op shift, d vreg, n uint8, wide bool) {
	s := amd64.Shl
	if op == shiftRight {
		s = amd64.Shr
	}
	m.a.ShiftImm(s, amd64Regs[d], n, wide)
}
func (m *amd64Masm) CmpMem(r, b vreg, x int32, wide bool) {
	m.a.CmpMem(amd64Regs[r], amd64Regs[b], x, wide)
}
func (m *amd64Masm) Jcc(c cond, l label)    { m.a.Jcc(amd64Conds[c], amd64.Label(l)) }
func (m *amd64Masm) Jmp(l label)            { m.a.Jmp(amd64.Label(l)) }
func (m *amd64Masm) JmpReg(r vreg)          { m.a.JmpReg(amd64Regs[r]) }
func (m *amd64Masm) JmpMem(b vreg, x int32) { m.a.JmpMem(amd64Regs[b], x) }
func (m *amd64Masm) Ret()                   { m.a.Ret() }
func (m *amd64Masm) JmpBitClear(src, bit vreg, l label) {
	m.a.Bt(amd64Regs[src], amd64Regs[bit])
	m.a.Jcc(amd64.CondAE, amd64.Label(l))
}
func (m *amd64Masm) Data(b []byte) {
	for i := 0; i < len(b); i += 8 {
		m.a.Quad(binary.LittleEndian.Uint64(b[i:]))
	}
}

// arm64Masm writes arm64 code. The context is in X0, as enterRaw leaves
// it; X16 and X17 are the backend's scratch, and X27 the encoder's. None of
// the registers native code leaves alone -- X18, X28, X29, X30, SP -- is
// one it writes.
type arm64Masm struct{ a arm64.Asm }

var arm64Regs = [numVregs]arm64.Reg{
	rCtx: 0, rSubj: 1, rEnd: 2, rCur: 3, rLeft: 4, rTop: 5, rW: 6, rCaps: 7, rA: 8, rB: 9, rC: 10, rT: 11, rD: 12,
}

const (
	x16 arm64.Reg = 16
	x17 arm64.Reg = 17
)

var arm64Conds = [...]arm64.Cond{
	condE: arm64.EQ, condNE: arm64.NE, condA: arm64.HI, condAE: arm64.HS, condB: arm64.LO, condBE: arm64.LS,
	condS: arm64.MI, condNS: arm64.PL, condL: arm64.LT, condGE: arm64.GE, condLE: arm64.LE, condG: arm64.GT,
}

func (m *arm64Masm) NewLabel() label         { return label(m.a.NewLabel()) }
func (m *arm64Masm) Bind(l label)            { m.a.Bind(arm64.Label(l)) }
func (m *arm64Masm) Offset(l label) int      { return m.a.Offset(arm64.Label(l)) }
func (m *arm64Masm) Finish() ([]byte, error) { return m.a.Finish() }
func (m *arm64Masm) Data(b []byte)           { m.a.Data(b) }
func (m *arm64Masm) Load(d, b vreg, x int32) { m.a.Load(arm64Regs[d], arm64Regs[b], x) }
func (m *arm64Masm) LoadU32(d, b vreg, x int32) {
	m.a.LoadU32(arm64Regs[d], arm64Regs[b], x)
}
func (m *arm64Masm) LoadU16(d, b vreg, x int32) {
	m.a.LoadU16(arm64Regs[d], arm64Regs[b], x)
}
func (m *arm64Masm) LoadU8(d, b vreg, x int32)     { m.a.LoadU8(arm64Regs[d], arm64Regs[b], x) }
func (m *arm64Masm) Store(b vreg, x int32, s vreg) { m.a.Store(arm64Regs[b], x, arm64Regs[s]) }
func (m *arm64Masm) StoreImm(b vreg, x, imm int32) {
	m.a.MovImm(x16, uint64(int64(imm)))
	m.a.Store(arm64Regs[b], x, x16)
}
func (m *arm64Masm) MovRR(d, s vreg)         { m.a.MovRR(arm64Regs[d], arm64Regs[s]) }
func (m *arm64Masm) MovRR32(d, s vreg)       { m.a.MovRR32(arm64Regs[d], arm64Regs[s]) }
func (m *arm64Masm) MovImm(d vreg, v uint64) { m.a.MovImm(arm64Regs[d], v) }
func (m *arm64Masm) Lea(d, b vreg, x int32) {
	m.a.AddImm(arm64Regs[d], arm64Regs[b], int64(x), true)
}
func (m *arm64Masm) LeaLabel(d vreg, l label) { m.a.Adr(arm64Regs[d], arm64.Label(l)) }
func (m *arm64Masm) Op(op alu, d, s vreg, wide bool) {
	dst, src := arm64Regs[d], arm64Regs[s]
	switch op {
	case aluAdd:
		m.a.Op(arm64.Add, dst, dst, src, wide)
	case aluSub:
		m.a.Op(arm64.Subs, dst, dst, src, wide)
	case aluAnd:
		m.a.Op(arm64.Ands, dst, dst, src, wide)
	case aluOr:
		m.a.Op(arm64.Orr, dst, dst, src, wide)
	case aluXor:
		m.a.Op(arm64.Eor, dst, dst, src, wide)
	case aluCmp:
		m.a.Cmp(dst, src, wide)
	case aluTest:
		m.a.Tst(dst, src, wide)
	}
}
func (m *arm64Masm) OpImm(op alu, d vreg, imm int32, wide bool) {
	dst := arm64Regs[d]
	switch op {
	case aluAdd:
		m.a.AddImm(dst, dst, int64(imm), wide)
	case aluSub:
		m.a.SubsImm(dst, dst, int64(imm), wide)
	case aluCmp:
		m.a.CmpImm(dst, int64(imm), wide)
	default:
		// The logical operations' immediates are bit patterns arm64
		// encodes only some of: the constant goes through a register.
		m.a.MovImm(x16, uint64(int64(imm)))
		switch op {
		case aluAnd:
			m.a.Op(arm64.Ands, dst, dst, x16, wide)
		case aluOr:
			m.a.Op(arm64.Orr, dst, dst, x16, wide)
		case aluXor:
			m.a.Op(arm64.Eor, dst, dst, x16, wide)
		case aluTest:
			m.a.Tst(dst, x16, wide)
		}
	}
}
func (m *arm64Masm) ShiftImm(op shift, d vreg, n uint8, wide bool) {
	s := arm64.Lsl
	if op == shiftRight {
		s = arm64.Lsr
	}
	m.a.ShiftImm(s, arm64Regs[d], arm64Regs[d], n, wide)
}
func (m *arm64Masm) CmpMem(r, b vreg, x int32, wide bool) {
	if wide {
		m.a.Load(x17, arm64Regs[b], x)
	} else {
		m.a.LoadU32(x17, arm64Regs[b], x)
	}
	m.a.Cmp(arm64Regs[r], x17, wide)
}
func (m *arm64Masm) Jcc(c cond, l label) { m.a.BCond(arm64Conds[c], arm64.Label(l)) }
func (m *arm64Masm) Jmp(l label)         { m.a.B(arm64.Label(l)) }
func (m *arm64Masm) JmpReg(r vreg)       { m.a.Br(arm64Regs[r]) }
func (m *arm64Masm) JmpMem(b vreg, x int32) {
	m.a.Load(x16, arm64Regs[b], x)
	m.a.Br(x16)
}
func (m *arm64Masm) JmpBitClear(src, bit vreg, l label) {
	// The bit is shifted to the top, and the word tested for zero.
	m.a.ShiftReg(arm64.Lsr, x16, arm64Regs[src], arm64Regs[bit], true)
	m.a.ShiftImm(arm64.Lsl, x16, x16, 63, true)
	m.a.Cbz(x16, arm64.Label(l), true)
}
func (m *arm64Masm) Ret() { m.a.Ret() }
