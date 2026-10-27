package vm

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestInt32Trees runs testdata/int32/ops.js, whose answers are Node's
// (ops.node.txt), on the trees and in the interpreter: chains of bitwise
// operators over int32s, floats, NaN, ±0, the infinities, strings, null,
// undefined and booleans; comparisons and equalities of int32 and uint32
// operands with each other, with constants and with anything; BigInt
// operands mixed with numbers, which throw a TypeError, and BigInts
// alone; valueOf, called once for each operand, in order, before the
// operator, and an exception from one; element reads under an operator
// and with int32 keys from every kind of typed array, an array with
// holes, strings, BigInts and a getter on its prototype, an object, a
// string and null, at keys negative, out of range and past 2^31; stores
// with int32 keys into every kind, of numbers, strings and objects with
// valueOf, a frozen array's, sloppy and strict; and an asm.js loop over an
// Int32Array in an upvalue. On the trees, the operators' int32 trees are
// built (i32Of).
func TestInt32Trees(t *testing.T) {
	of := i32Of
	defer func() { i32Of = of }()
	ofs := 0
	i32Of = func(e tentry) (ti32, bool) { ofs++; return of(e) }
	runInt32Script(t, "ops", func(tree bool) {
		if tree && ofs < 50 {
			t.Errorf("tree tier: %d int32 trees built", ofs)
		}
		ofs = 0
	})
}

// runInt32Script runs testdata/int32/name.js on the trees and in the
// interpreter, comparing its answer with name.node.txt, Node's, and calls
// check after each.
func runInt32Script(t *testing.T, name string, check func(tree bool)) {
	t.Helper()
	src, err := os.ReadFile("testdata/int32/" + name + ".js")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/int32/" + name + ".node.txt")
	if err != nil {
		t.Fatal(err)
	}
	// A checkout that converts line endings, as Windows's does by default,
	// gives Node's output CRLFs the script's lines do not have.
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	defer SetTreeTier(true)
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, string(src)))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := v.String().Go()
		if got != string(want) {
			g, w := strings.Split(got, "\n"), strings.Split(string(want), "\n")
			for i := range max(len(g), len(w)) {
				var a, b string
				if i < len(g) {
					a = g[i]
				}
				if i < len(w) {
					b = w[i]
				}
				if a != b {
					t.Errorf("tree tier %v, line %d: got\n%s\nwant\n%s", tree, i+1, a, b)
					break
				}
			}
		}
		check(tree)
	}
}
