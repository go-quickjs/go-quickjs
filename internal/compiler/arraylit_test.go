package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestArrayLiteralHoles pins an array literal with holes and no spread
// compiled as one new_array of its elements, each hole the uninitialized
// value, as one without holes is, rather than a push of each element: so
// the JIT, which makes a new_array of its elements and refuses array_push,
// compiles the function it is in. A spread still pushes.
func TestArrayLiteralHoles(t *testing.T) {
	cases := []struct {
		src   string
		n     int // new_array's count, or -1 for pushes
		holes int
	}{
		{`[1, , 3]`, 3, 1},
		{`[,]`, 1, 1},
		{`[, ,]`, 2, 2},
		{`[x, , ]`, 2, 1},
		{`[, 1]`, 2, 1},
		{`[1, 2, 3]`, 3, 0},
		{`[1, , ...x]`, -1, 1},
	}
	for _, tc := range cases {
		prog, err := parser.Parse(tc.src, parser.Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		main, err := Compile(prog, Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		n, pushes, holes := -2, 0, 0
		for _, in := range main.Code {
			switch in.Op {
			case bytecode.OpNewArray:
				n = int(in.A)
			case bytecode.OpArrayPush, bytecode.OpArraySpread:
				pushes++
			case bytecode.OpPushUninitialized:
				holes++
			}
		}
		if pushes != 0 {
			if n != 0 {
				t.Errorf("%s: new_array %d before pushes", tc.src, n)
			}
			n = -1
		}
		if n != tc.n || holes != tc.holes {
			t.Errorf("%s: new_array of %d (-1: pushes), %d holes; want %d, %d", tc.src, n, holes, tc.n, tc.holes)
		}
	}
}
