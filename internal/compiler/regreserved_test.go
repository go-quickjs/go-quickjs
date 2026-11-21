package compiler

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler/regcomp"
)

// withCode is rc with its code replaced by code and its Aux by aux.
func withCode(rc *bytecode.RegCode, code []bytecode.RInstr, aux []uint32) *bytecode.RegCode {
	c := *rc
	c.Code = code
	c.Aux = aux
	return &c
}

// pairAt is the pc of the first step of op in rc followed by a branch of
// partner, or of the step-and-branch the compiler fused them to.
func pairAt(t *testing.T, rc *bytecode.RegCode, op, partner bytecode.ROp) int {
	t.Helper()
	for pc := 0; pc+1 < len(rc.Code); pc++ {
		fused, ok := rc.Code[pc].Op().StepPartner()
		if (rc.Code[pc].Op() == op || ok && fused == partner) && rc.Code[pc+1].Op() == partner {
			return pc
		}
	}
	t.Fatalf("no %s, %s in\n%s", op, partner, rc)
	return 0
}

// The verifier's rules for the reserved operations: a step-and-branch
// with its partner at pc+1, of the same A, B and target, and a step it
// can make; mul_imm's X 0; the int32 forms' Aux, the pc past the code of
// the | 0's position; add_imm_i32's X, and its SC where it
// stands for a subtraction. Nothing the compiler emits holds them yet but
// the step-and-branches.
func TestVerifyReservedOps(t *testing.T) {
	fns := regFunctions(t, `
		function up(n) { for (let i = 0; i < n; i++) {} }
		function down() { var j = 10; while (--j >= 0) {} }
		function arith(x, y) { return x * y }
		function bound(a) { return function inner() { for (var u = 0; u <= a; u++) {} } }
		up(1); down(); arith(1, 2); bound(1)();`)
	ok := func(name string, fn *bytecode.Function, rc *bytecode.RegCode, want bool) {
		t.Helper()
		why := regcomp.Verify(rc, fn)
		if (why == "") != want {
			t.Errorf("%s: Verify says %q, want accepted %v", name, why, want)
		}
	}
	for _, f := range fns {
		if rc := f.RegCode(); rc != nil {
			for pc, w := range rc.Code {
				if w.Op() >= bytecode.RMulI && w.Op() < bytecode.ROpCount {
					t.Errorf("%s emits %s at %d", f.Name, w.Op(), pc)
				}
			}
		}
	}

	// up: inc i, i; jlt i, n -> body.
	f := fns["up"]
	rc := f.RegCode()
	pc := pairAt(t, rc, bytecode.RInc, bytecode.RJLt)
	p := rc.Code[pc+1]
	fuse := func(op bytecode.ROp, x uint8, a, b uint16, off int32) []bytecode.RInstr {
		code := append([]bytecode.RInstr(nil), rc.Code...)
		code[pc] = bytecode.MakeR(op, x, a, b, 0).WithSC(off)
		return code
	}
	ok("step_jlt", f, withCode(rc, fuse(bytecode.RStepJLt, 1, p.A(), p.B(), p.SC()+1), rc.Aux), true)
	ok("step_jlt += 5", f, withCode(rc, fuse(bytecode.RStepJLt, bytecode.RStepAdd|5, p.A(), p.B(), p.SC()+1), rc.Aux), true)
	ok("step_jle before a jlt", f, withCode(rc, fuse(bytecode.RStepJLe, 1, p.A(), p.B(), p.SC()+1), rc.Aux), false)
	ok("another A", f, withCode(rc, fuse(bytecode.RStepJLt, 1, p.B(), p.B(), p.SC()+1), rc.Aux), false)
	ok("another B", f, withCode(rc, fuse(bytecode.RStepJLt, 1, p.A(), p.A(), p.SC()+1), rc.Aux), false)
	ok("another target", f, withCode(rc, fuse(bytecode.RStepJLt, 1, p.A(), p.B(), p.SC()), rc.Aux), false)
	ok("a register past the frame", f, withCode(rc, fuse(bytecode.RStepJLt, 1, p.A(), uint16(rc.NumRegs), p.SC()+1), rc.Aux), false)
	ok("an update of 2", f, withCode(rc, fuse(bytecode.RStepJLt, 2, p.A(), p.B(), p.SC()+1), rc.Aux), false)
	ok("a step of 0", f, withCode(rc, fuse(bytecode.RStepJLt, bytecode.RStepAdd, p.A(), p.B(), p.SC()+1), rc.Aux), false)

	// down: dec j, j; jge_imm j, 0 -> the dec.
	f = fns["down"]
	rc = f.RegCode()
	pc = pairAt(t, rc, bytecode.RDec, bytecode.RJGeI)
	p = rc.Code[pc+1]
	ok("step_jge_imm", f, withCode(rc, fuse(bytecode.RStepJGeI, 0x7f, p.A(), p.B(), p.SC()+1), rc.Aux), true)
	ok("step_jgt_imm before a jge_imm", f, withCode(rc, fuse(bytecode.RStepJGtI, 0x7f, p.A(), p.B(), p.SC()+1), rc.Aux), false)
	ok("another immediate", f, withCode(rc, fuse(bytecode.RStepJGeI, 0x7f, p.A(), p.B()+1, p.SC()+1), rc.Aux), false)

	// inner: inc u, u; jle_upval u, a.
	f = fns["inner"]
	rc = f.RegCode()
	pc = pairAt(t, rc, bytecode.RInc, bytecode.RJLeU)
	p = rc.Code[pc+1]
	ok("step_jle_upval", f, withCode(rc, fuse(bytecode.RStepJLeU, 1, p.A(), p.B(), p.SC()+1), rc.Aux), true)
	upPast := append([]bytecode.RInstr(nil), fuse(bytecode.RStepJLeU, 1, p.A(), 7, p.SC()+1)...)
	upPast[pc+1] = bytecode.MakeR(bytecode.RJLeU, 0, p.A(), 7, 0).WithSC(p.SC())
	ok("an upvalue past the function's", f, withCode(rc, upPast, rc.Aux), false)

	// instanceof_next keeps the conditional branch as its partner.
	f = regFunctions(t, `function test(o, C) { if (o instanceof C) return 1; return 0 }`)["test"]
	rc = f.RegCode()
	view := *f
	view.PropSites = rc.PropSites
	f = &view
	for pc, w := range rc.Code {
		if w.Op() != bytecode.RInstanceOfNext {
			continue
		}
		ok("instanceof branch", f, rc, true)
		for name, partner := range map[string]bytecode.RInstr{
			"another operand":   bytecode.MakeR(bytecode.RJmpF, 0, w.B(), 0, 0),
			"another operation": bytecode.MakeR(bytecode.RMove, 0, w.A(), w.B(), 0),
		} {
			code := append([]bytecode.RInstr(nil), rc.Code...)
			code[pc+1] = partner
			ok("instanceof "+name, f, withCode(rc, code, rc.Aux), false)
		}
		ok("instanceof without partner", f, withCode(rc, rc.Code[:pc+1], rc.Aux), false)
	}

	f = regFunctions(t, `function test(o) { if (o instanceof C) return 1; return 0 }`)["test"]
	rc = f.RegCode()
	view = *f
	view.PropSites = rc.PropSites
	view.Names = rc.Names
	f = &view
	for pc, w := range rc.Code {
		if w.Op() != bytecode.RGetGlobalInstanceOf {
			continue
		}
		ok("global instanceof", f, rc, true)
		p := rc.Code[pc+1]
		for name, partner := range map[string]bytecode.RInstr{
			"another constructor": bytecode.MakeR(p.Op(), p.X(), p.A(), p.B(), p.B()),
			"aliased value":       bytecode.MakeR(p.Op(), p.X(), p.A(), w.A(), w.A()),
			"another operation":   bytecode.MakeR(bytecode.RInstanceOf, p.X(), p.A(), p.B(), p.C()),
		} {
			code := append([]bytecode.RInstr(nil), rc.Code...)
			code[pc+1] = partner
			ok("global instanceof "+name, f, withCode(rc, code, rc.Aux), false)
		}
		ok("global instanceof without partner", f, withCode(rc, rc.Code[:pc+1], rc.Aux), false)
	}

	f = regFunctions(t, `function test(v) { return v === false ? 1 : 2 }`)["test"]
	rc = f.RegCode()
	for pc, w := range rc.Code {
		if w.Op() != bytecode.RJBool {
			continue
		}
		for _, x := range []uint8{0, 1, bytecode.RCmpTrue, bytecode.RCmpTrue | 1, 2, 0x40} {
			code := append([]bytecode.RInstr(nil), rc.Code...)
			code[pc] = bytecode.MakeR(bytecode.RJBool, x, w.A(), 0, 0).WithSC(w.SC())
			ok("boolean flags", f, withCode(rc, code, rc.Aux), x&^(bytecode.RCmpTrue|1) == 0)
		}
		for name, bad := range map[string]bytecode.RInstr{
			"another operand": bytecode.MakeR(bytecode.RJBool, w.X(), w.A(), 1, 0).WithSC(w.SC()),
			"bad register":    bytecode.MakeR(bytecode.RJBool, w.X(), uint16(rc.NumRegs), 0, 0).WithSC(w.SC()),
			"bad target":      w.WithSC(int32(len(rc.Code))),
		} {
			code := append([]bytecode.RInstr(nil), rc.Code...)
			code[pc] = bad
			ok("boolean "+name, f, withCode(rc, code, rc.Aux), false)
		}
	}

	// arith: mul r, x, y, and the operations of its form.
	f = fns["arith"]
	rc = f.RegCode()
	pc = pairAt(t, rc, bytecode.RMul, bytecode.RRet)
	m := rc.Code[pc]
	with := func(w bytecode.RInstr, aux uint32, hasAux bool) *bytecode.RegCode {
		code := append([]bytecode.RInstr(nil), rc.Code...)
		code[pc] = w
		var a []uint32
		if hasAux {
			a = make([]uint32, len(code))
			a[pc] = aux
		}
		c := withCode(rc, code, a)
		// The position of the | 0, a pc past the code (Lines).
		c.Lines = append(append([]bytecode.SourceLoc(nil), rc.Lines...), bytecode.SourceLoc{PC: uint32(len(code)), Pos: 0})
		return c
	}
	last := uint32(len(rc.Code))
	ok("mul_imm", f, with(bytecode.MakeR(bytecode.RMulI, 0, m.A(), m.B(), 0).WithSC(3), 0, false), true)
	ok("mul_imm of an X", f, with(bytecode.MakeR(bytecode.RMulI, 1, m.A(), m.B(), 0).WithSC(3), 0, false), false)
	for _, op := range []bytecode.ROp{bytecode.RAddI32, bytecode.RSubI32} {
		w := bytecode.MakeR(op, 0, m.A(), m.B(), m.C())
		ok(op.String(), f, with(w, last, true), true)
		ok(op.String()+" without Aux", f, with(w, 0, false), false)
		ok(op.String()+" of an Aux in the code", f, with(w, last-1, true), false)
		ok(op.String()+" of an Aux past the positions", f, with(w, last+1, true), false)
		ok(op.String()+" of a register past the frame", f, with(bytecode.MakeR(op, 0, m.A(), m.B(), uint16(rc.NumRegs)), last, true), false)
	}
	addI := func(x uint8, sc int32) bytecode.RInstr {
		return bytecode.MakeR(bytecode.RAddII32, x, m.A(), m.B(), 0).WithSC(sc)
	}
	ok("add_imm_i32", f, with(addI(0, -32768), last, true), true)
	ok("add_imm_i32 of x - 4", f, with(addI(bytecode.RAddII32Sub, -4), last, true), true)
	ok("add_imm_i32 of x - -32768", f, with(addI(bytecode.RAddII32Sub, -32768), last, true), false)
	ok("add_imm_i32 of X 2", f, with(addI(2, 1), last, true), false)
	ok("add_imm_i32 without Aux", f, with(addI(0, 1), 0, false), false)

	if s := withCode(rc, rc.Code, nil).Format(pc); strings.Contains(s, "rop(") {
		t.Errorf("format: %s", s)
	}
	if s := with(addI(bytecode.RAddII32Sub, -4), last, true).Format(pc); !strings.HasPrefix(s, "add_imm_i32 ") {
		t.Errorf("add_imm_i32 formats as %q", s)
	}
}
