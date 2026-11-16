package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// The patterns regcomp makes mul_imm and the int32 forms of, and those it
// leaves alone: x * k and k * x of a 16-bit k, not a k past it;
// (x + y) | 0, (x - y) | 0, (x + k) | 0 and (x - k) | 0, the last with X
// RAddII32Sub and SC -k, and its Aux the stack instruction of the | 0,
// at the position the + or the - has; not a + of a string, a | 1, a product's
// | 0, nor x - -32768, whose -k is past 16 bits. A bitwise operator's
// | 0 is left out where an operand is a number literal or a >>>, never
// of two unknown operands.
func TestRegInt32Forms(t *testing.T) {
	for _, c := range []struct {
		body string
		ops  []bytecode.ROp
		x    uint8
		sc   int32
		none []bytecode.ROp
	}{
		{"return x * 3", []bytecode.ROp{bytecode.RMulI}, 0, 3, []bytecode.ROp{bytecode.RMul}},
		{"return 3 * x", []bytecode.ROp{bytecode.RMulI}, 0, 3, []bytecode.ROp{bytecode.RMul}},
		{"return -3 * x", []bytecode.ROp{bytecode.RMulI}, 0, -3, []bytecode.ROp{bytecode.RMul}},
		{"x *= 7; return x", []bytecode.ROp{bytecode.RMulI}, 0, 7, []bytecode.ROp{bytecode.RMul}},
		{"return x * 70000", []bytecode.ROp{bytecode.RMul}, 0, 0, []bytecode.ROp{bytecode.RMulI}},
		{"return x * 0.5", []bytecode.ROp{bytecode.RMul}, 0, 0, []bytecode.ROp{bytecode.RMulI}},
		{"return (x + y) | 0", []bytecode.ROp{bytecode.RAddI32}, 0, 0, []bytecode.ROp{bytecode.ROrI, bytecode.RAdd}},
		{"return (x - y) | 0", []bytecode.ROp{bytecode.RSubI32}, 0, 0, []bytecode.ROp{bytecode.ROrI, bytecode.RSub}},
		{"return (x + 1) | 0", []bytecode.ROp{bytecode.RAddII32}, 0, 1, []bytecode.ROp{bytecode.ROrI}},
		{"return (x + -32768) | 0", []bytecode.ROp{bytecode.RAddII32}, 0, -32768, []bytecode.ROp{bytecode.ROrI}},
		{"return (x - 4) | 0", []bytecode.ROp{bytecode.RAddII32}, bytecode.RAddII32Sub, -4, []bytecode.ROp{bytecode.ROrI}},
		{"return (x - -32767) | 0", []bytecode.ROp{bytecode.RAddII32}, bytecode.RAddII32Sub, 32767, []bytecode.ROp{bytecode.ROrI}},
		{"return (x - -32768) | 0", []bytecode.ROp{bytecode.RSubI32}, 0, 0, []bytecode.ROp{bytecode.RAddII32, bytecode.ROrI}},
		{"return (x * 3 + y) | 0", []bytecode.ROp{bytecode.RMulI, bytecode.RAddI32}, 0, 3, []bytecode.ROp{bytecode.ROrI}},
		{`return ("a" + x) | 0`, []bytecode.ROp{bytecode.ROrI}, 0, 0, []bytecode.ROp{bytecode.RAddI32, bytecode.RAddII32}},
		{"return (x + `a`) | 0", []bytecode.ROp{bytecode.ROrI}, 0, 0, []bytecode.ROp{bytecode.RAddI32, bytecode.RAddII32}},
		{"return (x + y) | 1", []bytecode.ROp{bytecode.ROrI, bytecode.RAdd}, 0, 0, []bytecode.ROp{bytecode.RAddI32}},
		{"return (x * y) | 0", []bytecode.ROp{bytecode.ROrI, bytecode.RMul}, 0, 0, []bytecode.ROp{bytecode.RAddI32}},
		{"return (x ^ y) | 0", []bytecode.ROp{bytecode.ROrI, bytecode.RBitXor}, 0, 0, nil},
		{"return (y ^ (x >>> 7)) | 0", []bytecode.ROp{bytecode.RBitXor}, 0, 0, []bytecode.ROp{bytecode.ROrI}},
		{"return (x & 255) | 0", []bytecode.ROp{bytecode.RAndI}, 0, 0, []bytecode.ROp{bytecode.ROrI}},
		{"return (255 & x) | 0", []bytecode.ROp{bytecode.RBitAnd}, 0, 0, []bytecode.ROp{bytecode.ROrI}},
		{"return (x << y) | 0", []bytecode.ROp{bytecode.ROrI, bytecode.RShl}, 0, 0, nil},
		{"return (x >>> 0) | 0", []bytecode.ROp{bytecode.ROrI, bytecode.RUShrI}, 0, 0, nil},
	} {
		src := "function f(x, y) { " + c.body + " }"
		prog, err := parser.Parse(src, parser.Options{})
		if err != nil {
			t.Fatal(err)
		}
		top, err := Compile(prog, Options{Text: src})
		if err != nil {
			t.Fatal(err)
		}
		var fn *bytecode.Function
		for _, k := range top.Constants {
			if k.Kind == bytecode.ConstFunction && k.Fn != nil && k.Fn.Name == "f" {
				fn = k.Fn
			}
		}
		rc := fn.RegCode()
		if rc == nil {
			t.Errorf("%s: no register code", c.body)
			continue
		}
		has := map[bytecode.ROp]int{}
		for pc, w := range rc.Code {
			has[w.Op()]++
			switch w.Op() {
			case bytecode.RMulI, bytecode.RAddII32:
				if w.X() != c.x || w.SC() != c.sc {
					t.Errorf("%s: %s, want X %d and SC %d", c.body, rc.Format(pc), c.x, c.sc)
				}
			}
			switch w.Op() {
			case bytecode.RAddI32, bytecode.RSubI32, bytecode.RAddII32:
				// The | 0's stack instruction, by its position, which is
				// the + or the -'s, the start of the expression they
				// share: the stack code's bin_imm after the add, at the
				// add's line entry.
				if pc >= len(rc.Aux) || rc.Aux[pc] != rc.SrcPC[pc] {
					t.Errorf("%s: %s placed at %d, its | 0 at %v", c.body, rc.Format(pc), rc.SrcPC[pc], rc.Aux)
				}
			}
		}
		for _, op := range c.ops {
			if has[op] == 0 {
				t.Errorf("%s: no %s in\n%s", c.body, op, rc)
			}
		}
		for _, op := range c.none {
			if has[op] != 0 {
				t.Errorf("%s: %s in\n%s", c.body, op, rc)
			}
		}
	}
}
