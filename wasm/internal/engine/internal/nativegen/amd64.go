package main

import (
	"math"

	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
	rd "github.com/go-quickjs/go-quickjs/wasm/internal/engine/internal/regdef"
)

// The machine registers on amd64, kept across the dispatch:
//
//	RBX  the instruction's address      R12  the frame's numeric slots
//	R13  the native table               R11  the frame's tick
//	RDI  the context
//
// and, in a handler, RAX the instruction word and RCX, RDX, RSI, R8, R9 and
// R10 its scratch. The code never moves SP, never writes R14 (the
// goroutine), RBP or X15, never executes a call instruction, and is never a
// safe point: Go is reached through the exit stub alone.

type gen struct {
	a *Asm
	// exit leaves an instruction to its Go handler; exitSlow a branch back
	// that spent the tick to its slow function (regSlowTable).
	exit, exitSlow Label
	// retNative returns from a frame, to Go or to its native caller.
	retNative Label
	labels    map[int]Label
	// extLabels and extNames are the second table's handlers, rExt's,
	// by X.
	extLabels map[int]Label
	extNames  map[int]string
	names     map[int]string
	order     []int
}

// hx lays out the handler of operation x of the second table, named name.
func (g *gen) hx(x int, name string, body func()) {
	if _, dup := g.extLabels[x]; dup {
		panic("nativegen: two handlers of " + name)
	}
	g.a.Align(16)
	l := g.a.NewLabel()
	g.a.Bind(l)
	g.extLabels[x], g.extNames[x] = l, name
	body()
}

// h lays out op's handler, named name.
func (g *gen) h(op int, name string, body func()) {
	if _, dup := g.labels[op]; dup {
		panic("nativegen: two handlers of " + name)
	}
	g.a.Align(16)
	l := g.a.NewLabel()
	g.a.Bind(l)
	g.labels[op], g.names[op] = l, name
	g.order = append(g.order, op)
	body()
}

func (g *gen) dispatch() {
	a := g.a
	a.Load(RAX, RBX, 0)
	a.MovZX8(RCX, RAX)
	a.JmpIdx(R13, RCX, 8, 0)
}

// next goes on at the instruction n words on.
func (g *gen) next(n int32) {
	g.a.OpImm(Add, RBX, 8*n, true)
	g.dispatch()
}

// off puts in dst the byte offset of the slot field i names (A, B, C).
func (g *gen) off(dst Reg, i int) {
	a := g.a
	a.MovRR(dst, RAX)
	a.ShiftImm(Shr, dst, uint8(13+16*i), true)
	a.OpImm(And, dst, 0x7FFF8, true)
}

// load is dst = the slot field i names; R9 its scratch.
func (g *gen) load(dst Reg, i int) {
	g.off(R9, i)
	g.a.LoadIdx(dst, R12, R9, 1, 0)
}

// store is the slot field i names = src; R9 its scratch.
func (g *gen) store(i int, src Reg) {
	g.off(R9, i)
	g.a.StoreIdx(R12, R9, 1, 0, src)
}

// immC and immB are dst = C and B as 16-bit immediates, sign-extended;
// bc dst = BC, sign-extended (signed) or not.
func (g *gen) immC(dst Reg) {
	g.a.MovRR(dst, RAX)
	g.a.ShiftImm(Sar, dst, 48, true)
}

func (g *gen) immB(dst Reg) {
	g.a.MovRR(dst, RAX)
	g.a.ShiftImm(Shl, dst, 16, true)
	g.a.ShiftImm(Sar, dst, 48, true)
}

func (g *gen) bc(dst Reg, signed bool) {
	g.a.MovRR(dst, RAX)
	sh := Shr
	if signed {
		sh = Sar
	}
	g.a.ShiftImm(sh, dst, 32, true)
}

// take takes a branch of the word offset in off: a branch back counts
// down the tick, and where that is spent leaves to the slow function.
func (g *gen) take(off Reg) {
	a := g.a
	fwd := a.NewLabel()
	a.Op(Test, off, off, true)
	a.Jcc(CondG, fwd)
	a.OpImm(Sub, R11, 1, false)
	a.Jcc(CondLE, g.exitSlow)
	a.Bind(fwd)
	a.LeaIdx(RBX, RBX, off, 8, 0)
	g.dispatch()
}

// Shift's rotations, the group's /0 and /1.
const (
	rol Shift = 0
	ror Shift = 1
)

func emitAMD64() (*block, error) {
	g := &gen{a: &Asm{}, labels: map[int]Label{}, names: map[int]string{}, extLabels: map[int]Label{}, extNames: map[int]string{}}
	a := g.a
	g.exit, g.exitSlow, g.retNative = a.NewLabel(), a.NewLabel(), a.NewLabel()

	// The entry: the pinned registers from the context, and the first
	// dispatch.
	entry := a.NewLabel()
	a.Bind(entry)
	a.Load(R12, RDI, rd.CtxL)
	a.Load(R13, RDI, rd.CtxTable)
	a.Load(RDX, RDI, rd.CtxF)
	a.LoadS32(R11, RDX, rd.FrameTick)
	a.Load(RSI, RDI, rd.CtxCode)
	a.Load(RCX, RDI, rd.CtxPC)
	a.LeaIdx(RBX, RSI, RCX, 8, 0)
	g.dispatch()
	g.exitStub()

	g.moves()
	g.control()
	// The second table's operations: X picks their handler from the
	// native second table, the exit stub where they have none.
	g.h(rd.Ext, "Ext", func() {
		a.MovRR(RCX, RAX)
		a.ShiftImm(Shr, RCX, 8, true)
		a.MovZX8(RCX, RCX)
		a.Load(RDX, RDI, rd.CtxExtTable)
		a.JmpIdx(RDX, RCX, 8, 0)
	})
	g.integers()
	g.floats()
	g.refs()
	g.calls()
	g.memory()

	code, err := a.Finish()
	if err != nil {
		return nil, err
	}
	b := &block{code: code, at: map[int]int{}, names: g.names, order: g.order, entry: a.Offset(entry), exit: a.Offset(g.exit),
		extAt: map[int]int{}, extNames: g.extNames}
	for _, op := range g.order {
		b.at[op] = a.Offset(g.labels[op])
	}
	for x, l := range g.extLabels {
		b.extAt[x] = a.Offset(l)
	}
	if b.entry != 0 {
		panic("nativegen: the entry is not at 0")
	}
	return b, nil
}

// exitStub leaves the instruction at RBX to Go: its Go handler (exit) or
// its slow function (exitSlow), called through the context's trampoline
// (nativeCallH) as handler(l, w, pc, f), the tick stored in the frame
// first. Go answers the pc to go on at, ^pc for the instruction's slow
// function, or regDone once the frame has returned, which returns to the
// Go that entered the code.
func (g *gen) exitStub() {
	a := g.a
	common, after, resume, done, noCount := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Align(16)
	a.Bind(g.exit)
	a.Load(R8, RDI, rd.CtxGoTable)
	a.Jmp(common)
	a.Bind(g.exitSlow)
	a.Load(R8, RDI, rd.CtxSlowTable)
	a.Bind(common)
	a.LeaLabel(RCX, after)
	a.Store(RDI, rd.CtxAfter, RCX)
	a.Load(RDX, RDI, rd.CtxF)
	a.Store32(RDX, rd.FrameTick, R11)
	a.Load(RAX, RBX, 0)
	// Counted where a test counts.
	a.Load(RCX, RDI, rd.CtxExits)
	a.Op(Test, RCX, RCX, true)
	a.Jcc(CondE, noCount)
	a.MovZX8(RSI, RAX)
	a.LeaIdx(RCX, RCX, RSI, 8, 0)
	a.LockIncMem(RCX, 0)
	a.Bind(noCount)
	// handler(l, w, pc, f): the func value in DX, f in SI for the
	// trampoline, which keeps the context.
	a.MovZX8(RCX, RAX)
	a.LoadIdx(RDX, R8, RCX, 8, 0)
	a.MovRR(RCX, RBX)
	a.Load(RSI, RDI, rd.CtxCode)
	a.Op(Sub, RCX, RSI, true)
	a.ShiftImm(Shr, RCX, 3, true)
	a.MovRR(RBX, RAX)
	a.MovRR(RAX, R12)
	a.Load(RSI, RDI, rd.CtxF)
	a.JmpMem(RDI, rd.CtxCallH)

	// Back from Go, the result in AX's low half, DI the context, every
	// other register Go's.
	a.Bind(after)
	a.Load(R12, RDI, rd.CtxL)
	a.Load(RDX, RDI, rd.CtxF)
	a.LoadS32(R11, RDX, rd.FrameTick)
	a.Op(Test, RAX, RAX, false)
	a.Jcc(CondNS, resume)
	a.OpImm(Cmp, RAX, math.MinInt32, false)
	a.Jcc(CondE, done)
	// ^pc: the slow function of the instruction at pc.
	a.MovRR32(RCX, RAX)
	a.Not32(RCX)
	a.Load(RSI, RDI, rd.CtxCode)
	a.LeaIdx(RBX, RSI, RCX, 8, 0)
	a.Load(R8, RDI, rd.CtxSlowTable)
	a.Jmp(common)
	a.Bind(resume)
	a.MovRR32(RAX, RAX)
	a.Load(RSI, RDI, rd.CtxCode)
	a.LeaIdx(RBX, RSI, RAX, 8, 0)
	a.Load(R13, RDI, rd.CtxTable)
	g.dispatch()
	// The frame has returned: to its native caller where it has one.
	a.Bind(done)
	a.Jmp(g.retNative)
}

func (g *gen) moves() {
	a := g.a
	g.h(rd.Mov, "Mov", func() {
		g.load(RDX, 1)
		g.store(0, RDX)
		g.next(1)
	})
	g.h(rd.Mov2, "Mov2", func() {
		g.off(R9, 1)
		a.LoadIdx(RDX, R12, R9, 1, 0)
		a.LoadIdx(RSI, R12, R9, 1, 8)
		g.off(R9, 0)
		a.StoreIdx(R12, R9, 1, 0, RDX)
		a.StoreIdx(R12, R9, 1, 8, RSI)
		g.next(1)
	})
	g.h(rd.Const32, "Const32", func() {
		g.bc(RDX, false)
		g.store(0, RDX)
		g.next(1)
	})
	g.h(rd.Const32S, "Const32S", func() {
		g.bc(RDX, true)
		g.store(0, RDX)
		g.next(1)
	})
	g.h(rd.Const64, "Const64", func() {
		a.Load(RDX, RBX, 8)
		g.store(0, RDX)
		g.next(2)
	})
	g.h(rd.Const128, "Const128", func() {
		a.Load(RDX, RBX, 8)
		a.Load(RSI, RBX, 16)
		g.off(R9, 0)
		a.StoreIdx(R12, R9, 1, 0, RDX)
		a.StoreIdx(R12, R9, 1, 8, RSI)
		g.next(3)
	})
	g.h(rd.Select, "Select", func() {
		skip := a.NewLabel()
		g.load(RCX, 2)
		a.Op(Test, RCX, RCX, true)
		a.Jcc(CondNE, skip)
		g.load(RDX, 1)
		g.store(0, RDX)
		a.Bind(skip)
		g.next(1)
	})
	// A global: the instance's globals' table of pointers, then the
	// global's number.
	global := func() {
		g.bc(RCX, false)
		a.Load(R8, RDI, rd.CtxGlobals)
		a.LoadIdx(R8, R8, RCX, 8, 0)
	}
	g.h(rd.GlobalGet32, "GlobalGet32", func() {
		global()
		a.LoadU32(RDX, R8, rd.GlobalNum)
		g.store(0, RDX)
		g.next(1)
	})
	g.h(rd.GlobalGet64, "GlobalGet64", func() {
		global()
		a.Load(RDX, R8, rd.GlobalNum)
		g.store(0, RDX)
		g.next(1)
	})
	g.h(rd.GlobalSet, "GlobalSet", func() {
		global()
		g.load(RDX, 0)
		a.Store(R8, rd.GlobalNum, RDX)
		g.next(1)
	})
}

func (g *gen) control() {
	a := g.a
	g.h(rd.Jmp, "Jmp", func() {
		g.bc(RCX, true)
		g.take(RCX)
	})
	brz := func(op int, name string, cond Cond) {
		g.h(op, name, func() {
			tk := a.NewLabel()
			g.load(RDX, 0)
			a.Op(Test, RDX, RDX, true)
			a.Jcc(cond, tk)
			g.next(1)
			a.Bind(tk)
			g.bc(RCX, true)
			g.take(RCX)
		})
	}
	brz(rd.BrNZ, "BrNZ", CondNE)
	brz(rd.BrZ, "BrZ", CondE)
	g.h(rd.BrTable, "BrTable", func() {
		ok, fwd := a.NewLabel(), a.NewLabel()
		g.off(R9, 0)
		a.LoadU32Idx(RCX, R12, R9, 0)
		// The table: tables[BC], a slice of int32 pcs.
		g.bc(RDX, false)
		a.Load(R8, RDI, rd.CtxTables)
		a.LeaIdx(RDX, RDX, RDX, 2, 0)
		a.LeaIdx(R8, R8, RDX, 8, 0)
		a.Load(RSI, R8, rd.SliceLen)
		a.Load(R8, R8, 0)
		a.Op(Cmp, RCX, RSI, true)
		a.Jcc(CondB, ok)
		a.MovRR(RCX, RSI)
		a.OpImm(Sub, RCX, 1, true)
		a.Bind(ok)
		a.ShiftImm(Shl, RCX, 2, true)
		a.LoadS32Idx(RDX, R8, RCX, 0)
		a.Load(RSI, RDI, rd.CtxCode)
		a.LeaIdx(RSI, RSI, RDX, 8, 0)
		a.Op(Cmp, RSI, RBX, true)
		a.Jcc(CondA, fwd)
		a.OpImm(Sub, R11, 1, false)
		a.Jcc(CondLE, g.exitSlow)
		a.Bind(fwd)
		a.MovRR(RBX, RSI)
		g.dispatch()
	})
	g.h(rd.Ret, "Ret", func() {
		a.Jmp(g.retNative)
	})
	g.h(rd.Ret1, "Ret1", func() {
		g.load(RDX, 0)
		a.Store(R12, 0, RDX)
		a.Jmp(g.retNative)
	})
}

// intType is an integer type's operations.
type intType struct {
	name                                                           string
	wide                                                           bool
	add, addI, sub, subI, mul, mulI, and, andI, or, orI, xor, xorI int
	shl, shlI, shrS, shrSI, shrU, shrUI, rol, rolI, ror, rorI      int
	divS, divU, remS, remU, eqz, clz, ctz, ext8, ext16             int
	cmp                                                            [10]cmpOp
}

// cmpOp is a comparison's operations: as a boolean, with an immediate,
// fused with a branch, both ways; and its condition.
type cmpOp struct {
	name             string
	op, opI, br, brI int
	cond             Cond
	// extI says opI is of the second table.
	extI bool
}

var i32 = intType{name: "I32", wide: false,
	add: rd.I32Add, addI: rd.I32AddI, sub: rd.I32Sub, subI: rd.I32SubI, mul: rd.I32Mul, mulI: rd.I32MulI,
	and: rd.I32And, andI: rd.I32AndI, or: rd.I32Ior, orI: rd.I32IorI, xor: rd.I32Xor, xorI: rd.I32XorI,
	shl: rd.I32Shl, shlI: rd.I32ShlI, shrS: rd.I32ShrS, shrSI: rd.I32ShrSI, shrU: rd.I32ShrU, shrUI: rd.I32ShrUI,
	rol: rd.I32Rol, rolI: rd.I32RolI, ror: rd.I32Ror, rorI: rd.I32RorI,
	divS: rd.I32DivS, divU: rd.I32DivU, remS: rd.I32RemS, remU: rd.I32RemU,
	eqz: rd.I32Eqz, clz: rd.I32Clz, ctz: rd.I32Ctz, ext8: rd.I32SExtendI8, ext16: rd.I32SExtendI16,
	cmp: [10]cmpOp{
		{"Eq", rd.I32Eq, rd.I32EqI, rd.BrI32Eq, rd.BrI32EqI, CondE, false},
		{"Ne", rd.I32Ne, rd.I32NeI, rd.BrI32Ne, rd.BrI32NeI, CondNE, false},
		{"LtS", rd.I32LtS, rd.I32LtSI, rd.BrI32LtS, rd.BrI32LtSI, CondL, false},
		{"LtU", rd.I32LtU, rd.I32LtUI, rd.BrI32LtU, rd.BrI32LtUI, CondB, false},
		{"GtS", rd.I32GtS, rd.I32GtSI, rd.BrI32GtS, rd.BrI32GtSI, CondG, false},
		{"GtU", rd.I32GtU, rd.I32GtUI, rd.BrI32GtU, rd.BrI32GtUI, CondA, false},
		{"LeS", rd.I32LeS, rd.I32LeSI, rd.BrI32LeS, rd.BrI32LeSI, CondLE, false},
		{"LeU", rd.I32LeU, rd.I32LeUI, rd.BrI32LeU, rd.BrI32LeUI, CondBE, false},
		{"GeS", rd.I32GeS, rd.I32GeSI, rd.BrI32GeS, rd.BrI32GeSI, CondGE, false},
		{"GeU", rd.I32GeU, rd.I32GeUI, rd.BrI32GeU, rd.BrI32GeUI, CondAE, false},
	}}

var i64 = intType{name: "I64", wide: true,
	add: rd.I64Add, addI: rd.I64AddI, sub: rd.I64Sub, subI: rd.I64SubI, mul: rd.I64Mul, mulI: rd.I64MulI,
	and: rd.I64And, andI: rd.I64AndI, or: rd.I64Ior, orI: rd.I64IorI, xor: rd.I64Xor, xorI: rd.I64XorI,
	shl: rd.I64Shl, shlI: rd.I64ShlI, shrS: rd.I64ShrS, shrSI: rd.I64ShrSI, shrU: rd.I64ShrU, shrUI: rd.I64ShrUI,
	rol: rd.I64Rol, rolI: -1, ror: rd.I64Ror, rorI: -1,
	divS: rd.I64DivS, divU: rd.I64DivU, remS: rd.I64RemS, remU: rd.I64RemU,
	eqz: rd.I64Eqz, clz: rd.I64Clz, ctz: rd.I64Ctz, ext8: rd.I64SExtendI8, ext16: rd.I64SExtendI16,
	cmp: [10]cmpOp{
		{"Eq", rd.I64Eq, rd.XI64EqI, rd.BrI64Eq, rd.BrI64EqI, CondE, true},
		{"Ne", rd.I64Ne, rd.XI64NeI, rd.BrI64Ne, rd.BrI64NeI, CondNE, true},
		{"LtS", rd.I64LtS, rd.XI64LtSI, rd.BrI64LtS, rd.BrI64LtSI, CondL, true},
		{"LtU", rd.I64LtU, rd.XI64LtUI, rd.BrI64LtU, rd.BrI64LtUI, CondB, true},
		{"GtS", rd.I64GtS, rd.XI64GtSI, rd.BrI64GtS, rd.BrI64GtSI, CondG, true},
		{"GtU", rd.I64GtU, rd.XI64GtUI, rd.BrI64GtU, rd.BrI64GtUI, CondA, true},
		{"LeS", rd.I64LeS, rd.XI64LeSI, rd.BrI64LeS, rd.BrI64LeSI, CondLE, true},
		{"LeU", rd.I64LeU, rd.XI64LeUI, rd.BrI64LeU, rd.BrI64LeUI, CondBE, true},
		{"GeS", rd.I64GeS, rd.XI64GeSI, rd.BrI64GeS, rd.BrI64GeSI, CondGE, true},
		{"GeU", rd.I64GeU, rd.XI64GeUI, rd.BrI64GeU, rd.BrI64GeUI, CondAE, true},
	}}

func (g *gen) integers() {
	for _, t := range []intType{i32, i64} {
		g.intType(t)
	}
	a := g.a
	g.h(rd.I32ConvertI64, "I32ConvertI64", func() {
		g.load(RSI, 1)
		a.MovRR32(RDX, RSI)
		g.store(0, RDX)
		g.next(1)
	})
	for _, op := range []int{rd.I64SConvertI32, rd.I64SExtendI32} {
		name := map[int]string{rd.I64SConvertI32: "I64SConvertI32", rd.I64SExtendI32: "I64SExtendI32"}[op]
		g.h(op, name, func() {
			g.load(RSI, 1)
			a.Movsxd(RDX, RSI)
			g.store(0, RDX)
			g.next(1)
		})
	}
}

func (g *gen) intType(t intType) {
	a := g.a
	w := t.wide
	// rr loads B into RDX and C (or C's immediate) into RSI.
	rr := func(imm bool) {
		g.load(RDX, 1)
		if imm {
			g.immC(RSI)
		} else {
			g.load(RSI, 2)
		}
	}
	alu := func(op, opI int, name string, alu ALU) {
		for _, imm := range []bool{false, true} {
			o, n := op, t.name+name
			if imm {
				o, n = opI, n+"I"
			}
			g.h(o, n, func() {
				rr(imm)
				a.Op(alu, RDX, RSI, w)
				g.store(0, RDX)
				g.next(1)
			})
		}
	}
	alu(t.add, t.addI, "Add", Add)
	alu(t.sub, t.subI, "Sub", Sub)
	alu(t.and, t.andI, "And", And)
	alu(t.or, t.orI, "Ior", Or)
	alu(t.xor, t.xorI, "Xor", Xor)
	for _, imm := range []bool{false, true} {
		o, n := t.mul, t.name+"Mul"
		if imm {
			o, n = t.mulI, n+"I"
		}
		g.h(o, n, func() {
			rr(imm)
			if w {
				a.Imul(RDX, RSI)
			} else {
				a.Imul32(RDX, RSI)
			}
			g.store(0, RDX)
			g.next(1)
		})
	}
	shift := func(op, opI int, name string, sh Shift) {
		for _, imm := range []bool{false, true} {
			o, n := op, t.name+name
			if imm {
				o, n = opI, n+"I"
			}
			if o < 0 {
				continue
			}
			g.h(o, n, func() {
				g.load(RDX, 1)
				if imm {
					g.immC(RCX)
				} else {
					g.load(RCX, 2)
				}
				a.ShiftCL(sh, RDX, w)
				g.store(0, RDX)
				g.next(1)
			})
		}
	}
	shift(t.shl, t.shlI, "Shl", Shl)
	shift(t.shrS, t.shrSI, "ShrS", Sar)
	shift(t.shrU, t.shrUI, "ShrU", Shr)
	shift(t.rol, t.rolI, "Rol", rol)
	shift(t.ror, t.rorI, "Ror", ror)
	// The divisions leave a zero divisor, and a signed one of -1, to Go:
	// the trap, the overflow, the remainder's 0.
	div := func(op int, name string, signed, rem bool) {
		g.h(op, t.name+name, func() {
			g.load(RCX, 2)
			a.Op(Test, RCX, RCX, w)
			a.Jcc(CondE, g.exit)
			if signed {
				a.OpImm(Cmp, RCX, -1, w)
				a.Jcc(CondE, g.exit)
			}
			g.off(R10, 0)
			g.load(RAX, 1)
			switch {
			case signed && w:
				a.Cqo()
				a.Idiv(RCX)
			case signed:
				a.Cdq()
				a.Idiv32(RCX)
			case w:
				a.Op(Xor, RDX, RDX, false)
				a.Div(RCX)
			default:
				a.Op(Xor, RDX, RDX, false)
				a.Div32(RCX)
			}
			r := RAX
			if rem {
				r = RDX
			}
			a.StoreIdx(R12, R10, 1, 0, r)
			g.next(1)
		})
	}
	div(t.divS, "DivS", true, false)
	div(t.divU, "DivU", false, false)
	div(t.remS, "RemS", true, true)
	div(t.remU, "RemU", false, true)
	for _, c := range t.cmp {
		for _, imm := range []bool{false, true} {
			o, n := c.op, t.name+c.name
			if imm {
				o, n = c.opI, n+"I"
			}
			body := func() {
				rr(imm)
				a.Op(Cmp, RDX, RSI, w)
				a.Setcc(c.cond, RDX)
				a.MovZX8(RDX, RDX)
				g.store(0, RDX)
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
				g.load(RDX, 0)
				if imm {
					g.immB(RSI)
				} else {
					g.load(RSI, 1)
				}
				a.Op(Cmp, RDX, RSI, w)
				a.Jcc(c.cond, tk)
				g.next(1)
				a.Bind(tk)
				g.immC(RCX)
				g.take(RCX)
			})
		}
	}
	g.h(t.eqz, t.name+"Eqz", func() {
		g.load(RSI, 1)
		a.Op(Test, RSI, RSI, w)
		a.Setcc(CondE, RDX)
		a.MovZX8(RDX, RDX)
		g.store(0, RDX)
		g.next(1)
	})
	bits := int32(32)
	if w {
		bits = 64
	}
	count := func(op int, name string, clz bool) {
		g.h(op, t.name+name, func() {
			zero := a.NewLabel()
			g.load(RSI, 1)
			if clz {
				a.Bsr(RDX, RSI, w)
			} else {
				a.Bsf(RDX, RSI, w)
			}
			a.Jcc(CondE, zero)
			if clz {
				a.OpImm(Xor, RDX, bits-1, false)
			}
			g.store(0, RDX)
			g.next(1)
			a.Bind(zero)
			a.MovImm(RDX, uint64(bits))
			g.store(0, RDX)
			g.next(1)
		})
	}
	count(t.clz, "Clz", true)
	count(t.ctz, "Ctz", false)
	g.h(t.ext8, t.name+"SExtendI8", func() {
		g.load(RSI, 1)
		a.Movsx8(RDX, RSI, w)
		g.store(0, RDX)
		g.next(1)
	})
	g.h(t.ext16, t.name+"SExtendI16", func() {
		g.load(RSI, 1)
		a.Movsx16(RDX, RSI, w)
		g.store(0, RDX)
		g.next(1)
	})
}

// access is a load or a store of memory 0 and how it reads or writes.
type access struct {
	op    int
	name  string
	width int32
	// load is dst = the bytes at [base + index]; store writes src there.
	load  func(a *Asm, dst, base, index Reg)
	store func(a *Asm, base, index, src Reg)
}

var accesses = []access{
	{rd.I32LoadMem, "I32LoadMem", 4, func(a *Asm, d, b, i Reg) { a.LoadU32Idx(d, b, i, 0) }, nil},
	{rd.I64LoadMem, "I64LoadMem", 8, func(a *Asm, d, b, i Reg) { a.LoadIdx(d, b, i, 1, 0) }, nil},
	{rd.F32LoadMem, "F32LoadMem", 4, func(a *Asm, d, b, i Reg) { a.LoadU32Idx(d, b, i, 0) }, nil},
	{rd.F64LoadMem, "F64LoadMem", 8, func(a *Asm, d, b, i Reg) { a.LoadIdx(d, b, i, 1, 0) }, nil},
	{rd.I32LoadMem8S, "I32LoadMem8S", 1, func(a *Asm, d, b, i Reg) { a.LoadS8Idx(d, b, i, 0, false) }, nil},
	{rd.I32LoadMem8U, "I32LoadMem8U", 1, func(a *Asm, d, b, i Reg) { a.LoadU8Idx(d, b, i, 0) }, nil},
	{rd.I32LoadMem16S, "I32LoadMem16S", 2, func(a *Asm, d, b, i Reg) { a.LoadS16Idx(d, b, i, 0, false) }, nil},
	{rd.I32LoadMem16U, "I32LoadMem16U", 2, func(a *Asm, d, b, i Reg) { a.LoadU16Idx(d, b, i, 0) }, nil},
	{rd.I64LoadMem8S, "I64LoadMem8S", 1, func(a *Asm, d, b, i Reg) { a.LoadS8Idx(d, b, i, 0, true) }, nil},
	{rd.I64LoadMem8U, "I64LoadMem8U", 1, func(a *Asm, d, b, i Reg) { a.LoadU8Idx(d, b, i, 0) }, nil},
	{rd.I64LoadMem16S, "I64LoadMem16S", 2, func(a *Asm, d, b, i Reg) { a.LoadS16Idx(d, b, i, 0, true) }, nil},
	{rd.I64LoadMem16U, "I64LoadMem16U", 2, func(a *Asm, d, b, i Reg) { a.LoadU16Idx(d, b, i, 0) }, nil},
	{rd.I64LoadMem32S, "I64LoadMem32S", 4, func(a *Asm, d, b, i Reg) { a.LoadS32Idx(d, b, i, 0) }, nil},
	{rd.I64LoadMem32U, "I64LoadMem32U", 4, func(a *Asm, d, b, i Reg) { a.LoadU32Idx(d, b, i, 0) }, nil},
	{rd.I32StoreMem, "I32StoreMem", 4, nil, func(a *Asm, b, i, s Reg) { a.Store32Idx(b, i, 0, s) }},
	{rd.I64StoreMem, "I64StoreMem", 8, nil, func(a *Asm, b, i, s Reg) { a.StoreIdx(b, i, 1, 0, s) }},
	{rd.F32StoreMem, "F32StoreMem", 4, nil, func(a *Asm, b, i, s Reg) { a.Store32Idx(b, i, 0, s) }},
	{rd.F64StoreMem, "F64StoreMem", 8, nil, func(a *Asm, b, i, s Reg) { a.StoreIdx(b, i, 1, 0, s) }},
	{rd.I32StoreMem8, "I32StoreMem8", 1, nil, func(a *Asm, b, i, s Reg) { a.Store8Idx(b, i, 0, s) }},
	{rd.I32StoreMem16, "I32StoreMem16", 2, nil, func(a *Asm, b, i, s Reg) { a.Store16Idx(b, i, 0, s) }},
	{rd.I64StoreMem8, "I64StoreMem8", 1, nil, func(a *Asm, b, i, s Reg) { a.Store8Idx(b, i, 0, s) }},
	{rd.I64StoreMem16, "I64StoreMem16", 2, nil, func(a *Asm, b, i, s Reg) { a.Store16Idx(b, i, 0, s) }},
	{rd.I64StoreMem32, "I64StoreMem32", 4, nil, func(a *Asm, b, i, s Reg) { a.Store32Idx(b, i, 0, s) }},
}

// memory lays out the loads and stores of memory 0: the address a slot's
// i32 plus C, its bytes within the memory's (the context's memory 0, nil
// where it is shared, whose size another thread may change), else Go's.
func (g *gen) memory() {
	a := g.a
	for _, m := range accesses {
		g.h(m.op, m.name, func() {
			addr := 1
			if m.store != nil {
				addr = 0
			}
			a.Load(R8, RDI, rd.CtxMem0)
			a.Op(Test, R8, R8, true)
			a.Jcc(CondE, g.exit)
			g.off(R9, addr)
			a.LoadU32Idx(RSI, R12, R9, 0)
			a.MovRR(RCX, RAX)
			a.ShiftImm(Shr, RCX, 48, true)
			a.Op(Add, RSI, RCX, true)
			a.Lea(RCX, RSI, m.width)
			a.Load(RDX, R8, rd.MemoryBuf+rd.SliceLen)
			a.Op(Cmp, RCX, RDX, true)
			a.Jcc(CondA, g.exit)
			a.Load(R8, R8, rd.MemoryBuf)
			if m.load != nil {
				m.load(a, RDX, R8, RSI)
				g.store(0, RDX)
			} else {
				g.load(RDX, 1)
				m.store(a, R8, RSI, RDX)
			}
			g.next(1)
		})
	}
}
