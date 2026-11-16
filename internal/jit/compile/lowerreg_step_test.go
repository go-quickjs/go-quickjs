package compile

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regUnfused is rc's code with each step-and-branch made the step it
// fused again -- inc, dec, add_imm or sub_imm of its counter into itself
// -- its partner after it as it was: the code before the peephole.
func regUnfused(rc *bytecode.RegCode) (*bytecode.RegCode, int) {
	c := *rc
	c.Code = append([]bytecode.RInstr(nil), rc.Code...)
	n := 0
	for pc, w := range c.Code {
		if _, ok := w.Op().StepPartner(); !ok {
			continue
		}
		n++
		a, k := w.A(), bytecode.StepK(w.X())
		switch {
		case w.X()&bytecode.RStepAdd == 0 && k > 0:
			c.Code[pc] = bytecode.MakeR(bytecode.RInc, 0, a, a, 0)
		case w.X()&bytecode.RStepAdd == 0:
			c.Code[pc] = bytecode.MakeR(bytecode.RDec, 0, a, a, 0)
		case k > 0:
			c.Code[pc] = bytecode.MakeR(bytecode.RAddI, 0, a, a, 0).WithSC(k)
		default:
			c.Code[pc] = bytecode.MakeR(bytecode.RSubI, 0, a, a, 0).WithSC(-k)
		}
	}
	return &c, n
}

// TestLowerRegStep checks that a step-and-branch is lowered as its step
// alone, falling to its partner: the IR of each function, in each of the
// lowering's modes, is the IR of its code with the pairs unfused, for
// each of the six kinds and each kind of step.
func TestLowerRegStep(t *testing.T) {
	fns := regCompiled(t, `
		function up(n) { var s = 0; for (let i = 0; i < n; i++) s += i; return s }
		function upk(n) { var s = 0; for (var i = 0; i < n; i += 3) s += i; return s }
		function downk(n) { var s = 0; for (var i = n; i > 0; i -= 2) s += i; return s }
		function whileDec(n) { var c = 0; while (--n >= 0) c++; return c }
		function doDec(j) { var c = 0; do { c++ } while (--j > 0); return c }
		function le(n) { var c = 0; for (var i = 0; i <= n; i++) c++; return c }
		function imm(a) { var c = 0; for (var i = 0; i < 5; i++) c += a[i]; return c }
		function bound(a) { return function inner(x) { var c = 0; for (var u = 0; u <= a; u++) c += x; return c } }`)
	kinds := map[bytecode.ROp]bool{}
	for _, name := range []string{"up", "upk", "downk", "whileDec", "doDec", "le", "imm", "inner"} {
		fn := fns[name]
		rc := fn.RegCode()
		for _, w := range rc.Code {
			if _, ok := w.Op().StepPartner(); ok {
				kinds[w.Op()] = true
			}
		}
		plain, n := regUnfused(rc)
		if n == 0 {
			t.Errorf("%s: no step-and-branch:\n%s", name, rc)
			continue
		}
		for _, mode := range []struct {
			name  string
			lower func(*bytecode.RegCode, *bytecode.Function) (any, error)
		}{
			{"plain", func(rc *bytecode.RegCode, fn *bytecode.Function) (any, error) { return LowerReg(rc, fn) }},
			{"hot", func(rc *bytecode.RegCode, fn *bytecode.Function) (any, error) { return LowerRegHot(rc, fn) }},
			{"inline", func(rc *bytecode.RegCode, fn *bytecode.Function) (any, error) { return LowerRegInline(rc, fn) }},
		} {
			got, err := mode.lower(rc, fn)
			if err != nil {
				t.Errorf("%s (%s): %v", name, mode.name, err)
				continue
			}
			want, err := mode.lower(plain, fn)
			if err != nil {
				t.Errorf("%s (%s) unfused: %v", name, mode.name, err)
				continue
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s (%s): the IR differs from the unfused code's:\n%v\nwant\n%v", name, mode.name, fmt.Sprint(got), fmt.Sprint(want))
			}
		}
	}
	for _, op := range []bytecode.ROp{bytecode.RStepJLt, bytecode.RStepJLe, bytecode.RStepJLtI, bytecode.RStepJGtI, bytecode.RStepJGeI, bytecode.RStepJLeU} {
		if !kinds[op] {
			t.Errorf("no function has %s", op)
		}
	}
}
