package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// TestRegSoleLocals pins which operators are marked RArithOwnA, a
// binding's old BigInt given back as the operator's result replaces it:
// those whose binding holds its values alone, read only as an operand. An
// append to another binding -- a string built of the BigInts' digits --,
// the str_freeze before a read of it, the int32 forms (x + y) | 0, and a
// return of the value the instruction before stored, as the stack form's
// return took it, leave the bindings alone; a return of a binding read
// later, a copy and a call's argument do not.
func TestRegSoleLocals(t *testing.T) {
	fns := regFunctions(t, `
		function digits(n) {
			var k, q, r, t, s = "";
			q = 1n; r = 180n; t = 60n;
			for (k = 0; k < n; k++) { q = q * 3n; r = r + q; t = t * r; s += t; }
			return s
		}
		function int32s(n) { var a = 1n, b = 0, k; for (k = 0; k < n; k++) { b = (b + k) | 0; a = a * 3n } return b }
		function returned(x, y) { var r = x * y; r = r + x; return r }
		function readLater(x, y) { var r = x * y; r = r + x; y = 1; return r }
		function copied(x) { var r = x * x, c = r; return c }
		function called(x, f) { var r = x * x; f(r); return 0 }`)
	owned := func(name string) int {
		f := fns[name]
		if f == nil || f.RegCode() == nil {
			t.Fatalf("%s has no register code", name)
		}
		n := 0
		for _, w := range f.RegCode().Code {
			switch w.Op() {
			case bytecode.RAdd, bytecode.RSub, bytecode.RMul, bytecode.RBin:
				if w.X()&bytecode.RArithOwnA != 0 {
					n++
				}
			}
		}
		return n
	}
	for _, c := range []struct {
		name string
		want int
	}{
		{"digits", 3}, {"int32s", 1}, {"returned", 2}, {"readLater", 0}, {"copied", 0}, {"called", 0},
	} {
		if got := owned(c.name); got != c.want {
			t.Errorf("%s: %d operators RArithOwnA, want %d:\n%s", c.name, got, c.want, fns[c.name].RegCode())
		}
	}
}
