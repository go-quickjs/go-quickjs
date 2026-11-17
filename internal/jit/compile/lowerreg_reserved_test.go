package compile

import (
	"strconv"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// The lowering's asserts for the reserved operations: a step-and-branch
// without its partner at pc+1, and an int32 form without its | 0's
// position, are internal errors before anything is lowered; a
// step-and-branch, an int32 form and a mul_imm that are well formed are
// lowered.
func TestLowerRegReservedAsserts(t *testing.T) {
	fns := regCompiled(t, `function up(n) { for (let i = 0; i < n; i++) {} }
		function arith(x, y) { return x * y }`)
	lowerWith := func(fn *bytecode.Function, pc int, w bytecode.RInstr, aux []uint32) error {
		rc := *fn.RegCode()
		rc.Code = append([]bytecode.RInstr(nil), rc.Code...)
		rc.Code[pc] = w
		rc.Aux = aux
		_, err := LowerReg(&rc, fn)
		return err
	}
	find := func(fn *bytecode.Function, op, next bytecode.ROp) int {
		code := fn.RegCode().Code
		for pc := 0; pc+1 < len(code); pc++ {
			fused, ok := code[pc].Op().StepPartner()
			if (code[pc].Op() == op || ok && fused == next) && code[pc+1].Op() == next {
				return pc
			}
		}
		t.Fatalf("%s: no %s, %s", fn.Name, op, next)
		return 0
	}
	want := func(err error, msg string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("lowered with %v, want an error of %q", err, msg)
		}
	}

	up := fns["up"]
	pc := find(up, bytecode.RInc, bytecode.RJLt)
	p := up.RegCode().Code[pc+1]
	if err := lowerWith(up, pc, bytecode.MakeR(bytecode.RStepJLt, 1, p.A(), p.B(), 0).WithSC(p.SC()+1), nil); err != nil {
		t.Errorf("step_jlt lowered with %v", err)
	}
	want(lowerWith(up, pc, bytecode.MakeR(bytecode.RStepJLt, 1, p.A(), p.B(), 0).WithSC(p.SC()), nil),
		"step_jlt at "+strconv.Itoa(pc)+" without its partner")

	arith := fns["arith"]
	pc = find(arith, bytecode.RMul, bytecode.RRet)
	m := arith.RegCode().Code[pc]
	aux := make([]uint32, len(arith.RegCode().Code))
	// The | 0's position, at a pc past the code (RegCode.Lines).
	aux[pc] = uint32(len(aux))
	add := bytecode.MakeR(bytecode.RAddI32, 0, m.A(), m.B(), m.C())
	if err := lowerWith(arith, pc, add, aux); err != nil {
		t.Errorf("add_i32: %v", err)
	}
	want(lowerWith(arith, pc, add, nil), "add_i32 at "+strconv.Itoa(pc)+" without its | 0's position")
	if err := lowerWith(arith, pc, bytecode.MakeR(bytecode.RMulI, 0, m.A(), m.B(), 0).WithSC(3), nil); err != nil {
		t.Errorf("mul_imm: %v", err)
	}
}

// mul_imm is lowered as mul of its immediate, and each int32 form as the
// pair it stands for: the + or the - into a scratch slot, past the
// registers, then the | 0's ToInt32 of it into A, of the instruction's
// origin -- add_imm_i32 of x - K a subtraction of K, as sub_imm's.
func TestLowerRegInt32Forms(t *testing.T) {
	fn := regCompiled(t, `function g(x, y, n) {
		var s = 0;
		for (var i = 0; i < n; i++) {
			var a = (x + y) | 0, b = (x - y) | 0, c = (x + 5) | 0, d = (x - 5) | 0, e = x * 7, f = 3 * y;
			s = s + a + b + c + d + e + f;
		}
		return s }`)["g"]
	rc := fn.RegCode()
	p, err := LowerReg(rc, fn)
	if err != nil {
		t.Fatal(err)
	}
	type form struct {
		op    ir.Operator
		right ir.Operand
	}
	seen := map[bytecode.ROp]int{}
	for rpc, w := range rc.Code {
		var ins []ir.Instruction
		for i, o := range p.Origin {
			if int(o) == rpc {
				ins = append(ins, p.Code[i])
			}
		}
		b, c := ir.Slot(int(w.B())), ir.Slot(int(w.C()))
		var want form
		switch w.Op() {
		case bytecode.RMulI:
			seen[w.Op()]++
			if len(ins) != 1 || ins[0].Op != ir.Binary || ins[0].Operator != ir.Mul || ins[0].Left != b ||
				ins[0].Right != ir.Literal(ir.Float(float64(w.SC()))) || ins[0].Dest != int(w.A()) {
				t.Errorf("%s lowered as %+v", rc.Format(rpc), ins)
			}
			continue
		case bytecode.RAddI32:
			want = form{ir.Add, c}
		case bytecode.RSubI32:
			want = form{ir.Sub, c}
		case bytecode.RAddII32:
			want = form{ir.Add, ir.Literal(ir.Float(float64(w.SC())))}
			if w.X() == bytecode.RAddII32Sub {
				want = form{ir.Sub, ir.Literal(ir.Float(float64(-w.SC())))}
			}
		default:
			continue
		}
		seen[w.Op()]++
		if len(ins) != 2 || ins[0].Op != ir.Binary || ins[0].Operator != want.op || ins[0].Left != b || ins[0].Right != want.right ||
			ins[0].Dest < p.Regs || ins[0].Generic ||
			ins[1].Op != ir.Unary || ins[1].Operator != ir.Int32 || ins[1].Left != ir.Slot(ins[0].Dest) || ins[1].Dest != int(w.A()) || ins[1].Generic {
			t.Errorf("%s lowered as %+v", rc.Format(rpc), ins)
		}
	}
	if seen[bytecode.RMulI] != 2 || seen[bytecode.RAddI32] != 1 || seen[bytecode.RSubI32] != 1 || seen[bytecode.RAddII32] != 2 {
		t.Errorf("forms seen %v in\n%s", seen, rc)
	}
}
