package main

import (
	"math"

	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
)

// arm64's handlers are amd64's (amd64.go) in arm64's instructions: the
// same handlers, tables, context and exit protocol. The machine registers,
// kept across the dispatch:
//
//	R19  the instruction's address      R20  the frame's numeric slots
//	R21  the native table               R23  the frame's tick
//	R25  the context
//
// and, in a handler, R0 the instruction word, R1 to R7, R16 and R17 its
// scratch (R6 the helpers' own), V0 to V5 its vector scratch. The code
// never moves SP and never writes R18 (the platform's), R26 but as the func
// value an exit gives Go, R27 (the assembler's temporary) but through the
// assembler, R28 (the goroutine), R29 (the frame pointer) or R30, which
// holds the return to the Go that entered the code throughout, so that a
// traceback taken in the code, or in the trampolines' frames, reads it. It
// executes no call instruction and is never a safe point: Go is reached
// through the exit stub alone. The block is in the binary, so no
// instruction cache needs flushing.
const (
	aW   Reg = 0  // the instruction word (amd64's RAX)
	aC   Reg = 1  // RCX
	aD   Reg = 2  // RDX
	aS   Reg = 3  // RSI
	a8   Reg = 4  // R8: a stored pointer's address (storeRef)
	a9   Reg = 5  // R9: a stored pointer (storeRef)
	aT   Reg = 6  // the helpers' temporary
	a10  Reg = 7  // R10
	aJ   Reg = 16 // a jump's address; storeRef's way back
	aK   Reg = 17 // a scratch the call path keeps across storeRef
	aIP  Reg = 19 // RBX
	aL   Reg = 20 // R12
	aTab Reg = 21 // R13
	aTk  Reg = 23 // R11
	aCtx Reg = 25 // RDI
	aFn  Reg = 26 // the func value of an exit's call (ABIInternal)
)

// The vector registers the handlers use, their scratch.
const (
	V0 FReg = 0
	V1 FReg = 1
	V2 FReg = 2
	V3 FReg = 3
	V4 FReg = 4
	V5 FReg = 5
)

// rorv is RORV, a rotation right by a register, which the assembler's
// Shift has as its other shifts' form.
const rorv Shift = 0x1AC02C00

type armGen struct {
	a                         *Asm
	exit, exitSlow, retNative Label
	labels                    map[int]Label
	names                     map[int]string
	order                     []int
	extLabels                 map[int]Label
	extNames                  map[int]string
	simdLabels                map[int]Label
	simdNames                 map[int]string
}

func (g *armGen) h(op int, name string, body func()) {
	if _, dup := g.labels[op]; dup {
		panic("nativegen: two arm64 handlers of " + name)
	}
	l := g.a.NewLabel()
	g.a.Bind(l)
	g.labels[op], g.names[op] = l, name
	g.order = append(g.order, op)
	body()
}

func (g *armGen) hx(x int, name string, body func()) {
	if _, dup := g.extLabels[x]; dup {
		panic("nativegen: two arm64 handlers of " + name)
	}
	l := g.a.NewLabel()
	g.a.Bind(l)
	g.extLabels[x], g.extNames[x] = l, name
	body()
}

func (g *armGen) hs(x int, name string, body func()) {
	if _, dup := g.simdLabels[x]; dup {
		panic("nativegen: two arm64 handlers of " + name)
	}
	l := g.a.NewLabel()
	g.a.Bind(l)
	g.simdLabels[x], g.simdNames[x] = l, name
	body()
}

func (g *armGen) dispatch() {
	a := g.a
	a.Load(aW, aIP, 0)
	a.Ubfx(aJ, aW, 0, 8)
	a.LoadIdx(aJ, aTab, aJ, true)
	a.Br(aJ)
}

// next goes on at the instruction n words on.
func (g *armGen) next(n int32) {
	g.a.AddImm(aIP, aIP, int64(8*n), true)
	g.dispatch()
}

// field puts in dst slot field i's index (A, B, C).
func (g *armGen) field(dst Reg, i int) { g.a.Ubfx(dst, aW, uint8(16+16*i), 16) }

// addr puts in dst the address of the slot field i names.
func (g *armGen) addr(dst Reg, i int) {
	g.field(dst, i)
	g.a.AddLsl(dst, aL, dst, 3)
}

// load is dst = the slot field i names; store the slot = src. R5 is their
// scratch.
func (g *armGen) load(dst Reg, i int) {
	g.field(a9, i)
	g.a.LoadIdx(dst, aL, a9, true)
}

func (g *armGen) store(i int, src Reg) {
	g.field(a9, i)
	g.a.AddLsl(a9, aL, a9, 3)
	g.a.Store(a9, 0, src)
}

// load32 is dst = the slot's low 32 bits, zero-extended.
func (g *armGen) load32(dst Reg, i int) {
	g.addr(a9, i)
	g.a.LoadU32(dst, a9, 0)
}

// immC and immB are dst = C and B as 16-bit immediates, sign-extended;
// bc dst = BC, sign-extended (signed) or not.
func (g *armGen) immC(dst Reg) { g.a.ShiftImm(Asr, dst, aW, 48, true) }

func (g *armGen) immB(dst Reg) { g.a.Sbfx(dst, aW, 32, 16) }

func (g *armGen) bc(dst Reg, signed bool) {
	sh := Lsr
	if signed {
		sh = Asr
	}
	g.a.ShiftImm(sh, dst, aW, 32, true)
}

// take takes a branch of the word offset in off: a branch back counts
// down the tick, and where that is spent leaves to the slow function.
func (g *armGen) take(off Reg) {
	a := g.a
	fwd := a.NewLabel()
	a.CmpImm(off, 0, true)
	a.BCond(GT, fwd)
	a.SubsImm(aTk, aTk, 1, false)
	a.BCond(LE, g.exitSlow)
	a.Bind(fwd)
	a.AddLsl(aIP, aIP, off, 3)
	g.dispatch()
}

func emitARM64() (*block, error) {
	g := &armGen{a: &Asm{}, labels: map[int]Label{}, names: map[int]string{}, extLabels: map[int]Label{}, extNames: map[int]string{},
		simdLabels: map[int]Label{}, simdNames: map[int]string{}}
	a := g.a
	g.exit, g.exitSlow, g.retNative = a.NewLabel(), a.NewLabel(), a.NewLabel()

	// The entry: the pinned registers from the context, and the first
	// dispatch.
	entry := a.NewLabel()
	a.Bind(entry)
	a.Load(aL, aCtx, rd.CtxL)
	a.Load(aTab, aCtx, rd.CtxTable)
	a.Load(aD, aCtx, rd.CtxF)
	a.LoadS32(aTk, aD, rd.FrameTick)
	a.Load(aS, aCtx, rd.CtxCode)
	a.Load(aC, aCtx, rd.CtxPC)
	a.AddLsl(aIP, aS, aC, 3)
	g.dispatch()
	g.exitStub()

	g.moves()
	g.control()
	g.h(rd.Ext, "Ext", func() {
		a.Ubfx(aJ, aW, 8, 8)
		a.Load(aT, aCtx, rd.CtxExtTable)
		a.LoadIdx(aJ, aT, aJ, true)
		a.Br(aJ)
	})
	g.h(rd.SIMD, "SIMD", func() {
		a.Ubfx(aJ, aW, 8, 8)
		a.Load(aT, aCtx, rd.CtxSIMDTable)
		a.LoadIdx(aJ, aT, aJ, true)
		a.Br(aJ)
	})
	g.integers()
	g.floats()
	g.refs()
	g.calls()
	g.memory()
	g.simd()
	g.simdMemory()

	code, err := a.Finish()
	if err != nil {
		return nil, err
	}
	b := &block{code: code, at: map[int]int{}, names: g.names, order: g.order, entry: a.Offset(entry), exit: a.Offset(g.exit),
		extAt: map[int]int{}, extNames: g.extNames, simdAt: map[int]int{}, simdNames: g.simdNames, words: true}
	for _, op := range g.order {
		b.at[op] = a.Offset(g.labels[op])
	}
	for x, l := range g.extLabels {
		b.extAt[x] = a.Offset(l)
	}
	for x, l := range g.simdLabels {
		b.simdAt[x] = a.Offset(l)
	}
	if b.entry != 0 {
		panic("nativegen: the arm64 entry is not at 0")
	}
	return b, nil
}

// exitStub leaves the instruction at R19 to Go: its Go handler (exit) or
// its slow function (exitSlow), called through the context's trampoline
// (nativeCallH) as handler(l, w, pc, f) -- R0 to R3, the func value in
// R26 -- the tick stored in the frame first. Go answers in R0's low half
// the pc to go on at, ^pc for the instruction's slow function, or regDone
// once the frame has returned.
func (g *armGen) exitStub() {
	a := g.a
	common, after, resume, done, noCount := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(g.exit)
	a.Load(aJ, aCtx, rd.CtxGoTable)
	a.B(common)
	a.Bind(g.exitSlow)
	a.Load(aJ, aCtx, rd.CtxSlowTable)
	a.Bind(common)
	a.Adr(aT, after)
	a.Store(aCtx, rd.CtxAfter, aT)
	a.Load(aD, aCtx, rd.CtxF)
	a.Store32(aD, rd.FrameTick, aTk)
	a.Load(aW, aIP, 0)
	// Counted where a test counts.
	a.Load(aC, aCtx, rd.CtxExits)
	a.Cbz(aC, noCount, true)
	a.Ubfx(aD, aW, 0, 8)
	a.AddLsl(aC, aC, aD, 3)
	a.Load(aD, aC, 0)
	a.AddImm(aD, aD, 1, true)
	a.Store(aC, 0, aD)
	a.Bind(noCount)
	a.Ubfx(aT, aW, 0, 8)
	a.LoadIdx(aFn, aJ, aT, true)
	a.MovRR(aC, aW)
	a.Load(aT, aCtx, rd.CtxCode)
	a.Op(Sub, aD, aIP, aT, true)
	a.ShiftImm(Lsr, aD, aD, 3, true)
	a.Load(aS, aCtx, rd.CtxF)
	a.MovRR(aW, aL)
	a.Load(aJ, aCtx, rd.CtxCallH)
	a.Br(aJ)

	// Back from Go, the result in R0's low half, R25 the context, every
	// other register Go's.
	a.Bind(after)
	a.Load(aL, aCtx, rd.CtxL)
	a.Load(aTab, aCtx, rd.CtxTable)
	a.Load(aD, aCtx, rd.CtxF)
	a.LoadS32(aTk, aD, rd.FrameTick)
	a.CmpImm(aW, 0, false)
	a.BCond(GE, resume)
	a.MovImm(aT, math.MaxInt32+1)
	a.Cmp(aW, aT, false)
	a.BCond(EQ, done)
	// ^pc: the slow function of the instruction at pc.
	a.Mvn(aC, aW, false)
	a.Load(aS, aCtx, rd.CtxCode)
	a.AddLsl(aIP, aS, aC, 3)
	a.Load(aJ, aCtx, rd.CtxSlowTable)
	a.B(common)
	a.Bind(resume)
	a.MovRR32(aW, aW)
	a.Load(aS, aCtx, rd.CtxCode)
	a.AddLsl(aIP, aS, aW, 3)
	g.dispatch()
	// The frame has returned: to its native caller where it has one.
	a.Bind(done)
	a.B(g.retNative)
}

func (g *armGen) moves() {
	a := g.a
	g.h(rd.Mov, "Mov", func() {
		g.load(aD, 1)
		g.store(0, aD)
		g.next(1)
	})
	g.h(rd.Mov2, "Mov2", func() {
		g.addr(aC, 1)
		a.LoadQ(V0, aC, 0)
		g.addr(aC, 0)
		a.StoreQ(aC, 0, V0)
		g.next(1)
	})
	g.h(rd.Const32, "Const32", func() {
		g.bc(aD, false)
		g.store(0, aD)
		g.next(1)
	})
	g.h(rd.Const32S, "Const32S", func() {
		g.bc(aD, true)
		g.store(0, aD)
		g.next(1)
	})
	g.h(rd.Const64, "Const64", func() {
		a.Load(aD, aIP, 8)
		g.store(0, aD)
		g.next(2)
	})
	g.h(rd.Const128, "Const128", func() {
		a.LoadQ(V0, aIP, 8)
		g.addr(aC, 0)
		a.StoreQ(aC, 0, V0)
		g.next(3)
	})
	g.h(rd.Select, "Select", func() {
		skip := a.NewLabel()
		g.load(aC, 2)
		a.Cbnz(aC, skip, true)
		g.load(aD, 1)
		g.store(0, aD)
		a.Bind(skip)
		g.next(1)
	})
	// A global: the instance's globals' table of pointers, then the
	// global's number.
	global := func() {
		g.bc(aC, false)
		a.Load(a8, aCtx, rd.CtxGlobals)
		a.LoadIdx(a8, a8, aC, true)
	}
	g.h(rd.GlobalGet32, "GlobalGet32", func() {
		global()
		a.LoadU32(aD, a8, rd.GlobalNum)
		g.store(0, aD)
		g.next(1)
	})
	g.h(rd.GlobalGet64, "GlobalGet64", func() {
		global()
		a.Load(aD, a8, rd.GlobalNum)
		g.store(0, aD)
		g.next(1)
	})
	g.h(rd.GlobalSet, "GlobalSet", func() {
		global()
		g.load(aD, 0)
		a.Store(a8, rd.GlobalNum, aD)
		g.next(1)
	})
}

func (g *armGen) control() {
	a := g.a
	g.h(rd.Jmp, "Jmp", func() {
		g.bc(aC, true)
		g.take(aC)
	})
	brz := func(op int, name string, nz bool) {
		g.h(op, name, func() {
			tk := a.NewLabel()
			g.load(aD, 0)
			if nz {
				a.Cbnz(aD, tk, true)
			} else {
				a.Cbz(aD, tk, true)
			}
			g.next(1)
			a.Bind(tk)
			g.bc(aC, true)
			g.take(aC)
		})
	}
	brz(rd.BrNZ, "BrNZ", true)
	brz(rd.BrZ, "BrZ", false)
	g.h(rd.BrTable, "BrTable", func() {
		ok, fwd := a.NewLabel(), a.NewLabel()
		g.load32(aC, 0)
		// The table: tables[BC], a slice of int32 pcs.
		g.bc(aD, false)
		a.Load(a8, aCtx, rd.CtxTables)
		a.MovImm(aT, rd.SliceSize)
		a.Mul(aD, aD, aT)
		a.Op(Add, a8, a8, aD, true)
		a.Load(aS, a8, rd.SliceLen)
		a.Load(a8, a8, 0)
		a.Cmp(aC, aS, true)
		a.BCond(LO, ok)
		a.AddImm(aC, aS, -1, true)
		a.Bind(ok)
		a.ShiftImm(Lsl, aC, aC, 2, true)
		a.LoadS32Idx(aD, a8, aC)
		a.Load(aS, aCtx, rd.CtxCode)
		a.AddLsl(aS, aS, aD, 3)
		a.Cmp(aS, aIP, true)
		a.BCond(HI, fwd)
		a.SubsImm(aTk, aTk, 1, false)
		a.BCond(LE, g.exitSlow)
		a.Bind(fwd)
		a.MovRR(aIP, aS)
		g.dispatch()
	})
	g.h(rd.Ret, "Ret", func() {
		a.B(g.retNative)
	})
	g.h(rd.Ret1, "Ret1", func() {
		g.load(aD, 0)
		a.Store(aL, 0, aD)
		a.B(g.retNative)
	})
}

// armConds are the integer comparisons' conditions, by amd64's tables'
// names.
var armConds = map[string]Cond{"Eq": EQ, "Ne": NE, "LtS": LT, "LtU": LO, "GtS": GT, "GtU": HI, "LeS": LE, "LeU": LS, "GeS": GE, "GeU": HS}

func (g *armGen) integers() {
	for _, t := range []intType{i32, i64} {
		g.intType(t)
	}
	a := g.a
	g.h(rd.I32ConvertI64, "I32ConvertI64", func() {
		g.load(aS, 1)
		a.MovRR32(aD, aS)
		g.store(0, aD)
		g.next(1)
	})
	for _, op := range []int{rd.I64SConvertI32, rd.I64SExtendI32} {
		name := map[int]string{rd.I64SConvertI32: "I64SConvertI32", rd.I64SExtendI32: "I64SExtendI32"}[op]
		g.h(op, name, func() {
			g.load(aS, 1)
			a.Sxtw(aD, aS)
			g.store(0, aD)
			g.next(1)
		})
	}
}

func (g *armGen) intType(t intType) {
	a := g.a
	w := t.wide
	// rr loads B into R2 and C (or C's immediate) into R3.
	rr := func(imm bool) {
		g.load(aD, 1)
		if imm {
			g.immC(aS)
		} else {
			g.load(aS, 2)
		}
	}
	bin := func(op, opI int, name string, body func()) {
		for _, imm := range []bool{false, true} {
			o, n := op, t.name+name
			if imm {
				o, n = opI, n+"I"
			}
			if o < 0 {
				continue
			}
			g.h(o, n, func() {
				rr(imm)
				body()
				g.store(0, aD)
				g.next(1)
			})
		}
	}
	alu := func(op, opI int, name string, alu ALU) {
		bin(op, opI, name, func() { a.Op(alu, aD, aD, aS, w) })
	}
	alu(t.add, t.addI, "Add", Add)
	alu(t.sub, t.subI, "Sub", Sub)
	alu(t.and, t.andI, "And", And)
	alu(t.or, t.orI, "Ior", Orr)
	alu(t.xor, t.xorI, "Xor", Eor)
	bin(t.mul, t.mulI, "Mul", func() {
		if w {
			a.Mul(aD, aD, aS)
		} else {
			a.MulW(aD, aD, aS)
		}
	})
	// The shifts take their count modulo the width, as the instructions
	// do.
	shift := func(op, opI int, name string, sh Shift) {
		bin(op, opI, name, func() { a.ShiftReg(sh, aD, aD, aS, w) })
	}
	shift(t.shl, t.shlI, "Shl", Lsl)
	shift(t.shrS, t.shrSI, "ShrS", Asr)
	shift(t.shrU, t.shrUI, "ShrU", Lsr)
	bin(t.rol, t.rolI, "Rol", func() {
		a.Neg(aS, aS, w)
		a.ShiftReg(rorv, aD, aD, aS, w)
	})
	bin(t.ror, t.rorI, "Ror", func() { a.ShiftReg(rorv, aD, aD, aS, w) })
	// The divisions leave a zero divisor, and a signed one of -1, to Go:
	// the trap, the overflow, the remainder's 0.
	div := func(op int, name string, signed, rem bool) {
		g.h(op, t.name+name, func() {
			g.load(aC, 2)
			a.Cbz(aC, g.exit, w)
			if signed {
				a.CmpImm(aC, -1, w)
				a.BCond(EQ, g.exit)
			}
			g.load(aS, 1)
			switch {
			case signed && w:
				a.Sdiv(aD, aS, aC)
			case signed:
				a.SdivW(aD, aS, aC)
			case w:
				a.Udiv(aD, aS, aC)
			default:
				a.UdivW(aD, aS, aC)
			}
			if rem {
				if w {
					a.Msub(aD, aD, aC, aS)
				} else {
					a.MsubW(aD, aD, aC, aS)
				}
			}
			g.store(0, aD)
			g.next(1)
		})
	}
	div(t.divS, "DivS", true, false)
	div(t.divU, "DivU", false, false)
	div(t.remS, "RemS", true, true)
	div(t.remU, "RemU", false, true)
	for _, c := range t.cmp {
		cond := armConds[c.name]
		for _, imm := range []bool{false, true} {
			o, n := c.op, t.name+c.name
			if imm {
				o, n = c.opI, n+"I"
			}
			body := func() {
				rr(imm)
				a.Cmp(aD, aS, w)
				a.Cset(aD, cond)
				g.store(0, aD)
				g.next(1)
			}
			if imm && c.extI {
				g.hx(o, n, body)
			} else {
				g.h(o, n, body)
			}
		}
		for _, imm := range []bool{false, true} {
			o, n := c.br, "Br"+t.name+c.name
			if imm {
				o, n = c.brI, n+"I"
			}
			g.h(o, n, func() {
				tk := a.NewLabel()
				g.load(aD, 0)
				if imm {
					g.immB(aS)
				} else {
					g.load(aS, 1)
				}
				a.Cmp(aD, aS, w)
				a.BCond(cond, tk)
				g.next(1)
				a.Bind(tk)
				g.immC(aC)
				g.take(aC)
			})
		}
	}
	g.h(t.eqz, t.name+"Eqz", func() {
		g.load(aS, 1)
		a.CmpImm(aS, 0, w)
		a.Cset(aD, EQ)
		g.store(0, aD)
		g.next(1)
	})
	g.h(t.clz, t.name+"Clz", func() {
		g.load(aS, 1)
		a.Clz(aD, aS, w)
		g.store(0, aD)
		g.next(1)
	})
	g.h(t.ctz, t.name+"Ctz", func() {
		g.load(aS, 1)
		a.Rbit(aD, aS, w)
		a.Clz(aD, aD, w)
		g.store(0, aD)
		g.next(1)
	})
	g.h(t.ext8, t.name+"SExtendI8", func() {
		g.load(aS, 1)
		a.Sxtb(aD, aS, w)
		g.store(0, aD)
		g.next(1)
	})
	g.h(t.ext16, t.name+"SExtendI16", func() {
		g.load(aS, 1)
		a.Sxth(aD, aS, w)
		g.store(0, aD)
		g.next(1)
	})
}

// armAccess is a load or a store of memory 0 on arm64.
type armAccess struct {
	op    int
	name  string
	width int64
	load  func(a *Asm, dst, base, index Reg)
	store func(a *Asm, base, index, src Reg)
}

var armAccesses = []armAccess{
	{rd.I32LoadMem, "I32LoadMem", 4, func(a *Asm, d, b, i Reg) { a.LoadU32Idx(d, b, i) }, nil},
	{rd.I64LoadMem, "I64LoadMem", 8, func(a *Asm, d, b, i Reg) { a.LoadIdx(d, b, i, false) }, nil},
	{rd.F32LoadMem, "F32LoadMem", 4, func(a *Asm, d, b, i Reg) { a.LoadU32Idx(d, b, i) }, nil},
	{rd.F64LoadMem, "F64LoadMem", 8, func(a *Asm, d, b, i Reg) { a.LoadIdx(d, b, i, false) }, nil},
	{rd.I32LoadMem8S, "I32LoadMem8S", 1, func(a *Asm, d, b, i Reg) { a.LoadS8Idx(d, b, i, false) }, nil},
	{rd.I32LoadMem8U, "I32LoadMem8U", 1, func(a *Asm, d, b, i Reg) { a.LoadU8Idx(d, b, i) }, nil},
	{rd.I32LoadMem16S, "I32LoadMem16S", 2, func(a *Asm, d, b, i Reg) { a.LoadS16Idx(d, b, i, false) }, nil},
	{rd.I32LoadMem16U, "I32LoadMem16U", 2, func(a *Asm, d, b, i Reg) { a.LoadU16Idx(d, b, i) }, nil},
	{rd.I64LoadMem8S, "I64LoadMem8S", 1, func(a *Asm, d, b, i Reg) { a.LoadS8Idx(d, b, i, true) }, nil},
	{rd.I64LoadMem8U, "I64LoadMem8U", 1, func(a *Asm, d, b, i Reg) { a.LoadU8Idx(d, b, i) }, nil},
	{rd.I64LoadMem16S, "I64LoadMem16S", 2, func(a *Asm, d, b, i Reg) { a.LoadS16Idx(d, b, i, true) }, nil},
	{rd.I64LoadMem16U, "I64LoadMem16U", 2, func(a *Asm, d, b, i Reg) { a.LoadU16Idx(d, b, i) }, nil},
	{rd.I64LoadMem32S, "I64LoadMem32S", 4, func(a *Asm, d, b, i Reg) { a.LoadS32Idx(d, b, i) }, nil},
	{rd.I64LoadMem32U, "I64LoadMem32U", 4, func(a *Asm, d, b, i Reg) { a.LoadU32Idx(d, b, i) }, nil},
	{rd.I32StoreMem, "I32StoreMem", 4, nil, func(a *Asm, b, i, s Reg) { a.Store32Idx(b, i, s) }},
	{rd.I64StoreMem, "I64StoreMem", 8, nil, func(a *Asm, b, i, s Reg) { a.StoreIdx(b, i, s) }},
	{rd.F32StoreMem, "F32StoreMem", 4, nil, func(a *Asm, b, i, s Reg) { a.Store32Idx(b, i, s) }},
	{rd.F64StoreMem, "F64StoreMem", 8, nil, func(a *Asm, b, i, s Reg) { a.StoreIdx(b, i, s) }},
	{rd.I32StoreMem8, "I32StoreMem8", 1, nil, func(a *Asm, b, i, s Reg) { a.Store8Idx(b, i, s) }},
	{rd.I32StoreMem16, "I32StoreMem16", 2, nil, func(a *Asm, b, i, s Reg) { a.Store16Idx(b, i, s) }},
	{rd.I64StoreMem8, "I64StoreMem8", 1, nil, func(a *Asm, b, i, s Reg) { a.Store8Idx(b, i, s) }},
	{rd.I64StoreMem16, "I64StoreMem16", 2, nil, func(a *Asm, b, i, s Reg) { a.Store16Idx(b, i, s) }},
	{rd.I64StoreMem32, "I64StoreMem32", 4, nil, func(a *Asm, b, i, s Reg) { a.Store32Idx(b, i, s) }},
}

// memory lays out the loads and stores of memory 0, as amd64's.
func (g *armGen) memory() {
	a := g.a
	for _, m := range armAccesses {
		g.h(m.op, m.name, func() {
			addr := 1
			if m.store != nil {
				addr = 0
			}
			g.memAddr(addr, m.width)
			if m.load != nil {
				m.load(a, aD, a8, aS)
				g.store(0, aD)
			} else {
				g.load(aD, 1)
				m.store(a, a8, aS, aD)
			}
			g.next(1)
		})
	}
}

// memAddr puts in R4 memory 0's bytes and in R3 the effective address of
// the slot field i's i32 plus C, exiting where memory 0 is shared or the
// width bytes there are out of bounds.
func (g *armGen) memAddr(i int, width int64) {
	a := g.a
	a.Load(a8, aCtx, rd.CtxMem0)
	a.Cbz(a8, g.exit, true)
	g.load32(aS, i)
	a.ShiftImm(Lsr, aC, aW, 48, true)
	a.Op(Add, aS, aS, aC, true)
	a.AddImm(aC, aS, width, true)
	a.Load(aD, a8, rd.MemoryBuf+rd.SliceLen)
	a.Cmp(aC, aD, true)
	a.BCond(HI, g.exit)
	a.Load(a8, a8, rd.MemoryBuf)
}
