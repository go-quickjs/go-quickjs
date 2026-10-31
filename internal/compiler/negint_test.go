package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestNegativeImmediate pins a negative int32 literal, which compileUnary
// folds into a constant, taken by an arithmetic or bitwise operator as an
// immediate (fuse), as a positive one is: x + -8 and x & -4 of a local, y
// - -2 of another operand. -0, a fraction and a number past an int32 stay
// constants, as does -1 compared or passed.
func TestNegativeImmediate(t *testing.T) {
	prog, err := parser.Parse(`function f(x, y) {
		return [x + -8, x & -4, (x + y) - -2, x + -0, x * -1.5, x + -2147483649, x === -1, g(-1), x + -2147483648];
	}`, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	main, err := Compile(prog, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var f *bytecode.Function
	for _, k := range main.Constants {
		if k.Kind == bytecode.ConstFunction {
			f = k.Fn
		}
	}
	if f == nil {
		t.Fatal("no function")
	}
	type imm struct {
		op bytecode.Op
		k  int32
	}
	var imms []imm
	var consts []float64
	for _, in := range f.Code {
		switch in.Op {
		case bytecode.OpLocalBinImm:
			imms = append(imms, imm{bytecode.Op(in.A >> 24), int32(in.B)})
		case bytecode.OpBinImm:
			imms = append(imms, imm{bytecode.Op(in.B), int32(in.A)})
		case bytecode.OpPushConst:
			if k := f.Constants[in.A]; k.Kind == bytecode.ConstNumber {
				consts = append(consts, k.Num)
			}
		}
	}
	want := []imm{{bytecode.OpAdd, -8}, {bytecode.OpBitAnd, -4}, {bytecode.OpSub, -2}, {bytecode.OpAdd, -2147483648}}
	if len(imms) != len(want) {
		t.Fatalf("immediates %v, want %v", imms, want)
	}
	for i := range want {
		if imms[i] != want[i] {
			t.Errorf("immediate %d: %v, want %v", i, imms[i], want[i])
		}
	}
	// -0, -1.5, -2147483649, and -1 twice.
	if len(consts) != 5 {
		t.Errorf("number constants pushed %v, want five", consts)
	}
}
