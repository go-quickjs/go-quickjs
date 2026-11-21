package compiler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestCompileLexicalLinear pins the compile of many lexical bindings in
// one scope as linear: n lets of a function's body, and n lets beside n
// vars at a script's top level and in a block, each compiled at n and at
// 2n, the lookups of a scope's names the compile makes and the bindings
// they look at (Options's looks) for the larger at most three times the
// smaller's -- twice is linear, four times quadratic. Declaring each
// binding had looked at every binding of its scope, and checking each var
// at every let around it; collecting a scope's names, which had compared
// each with those before it, compares none now (addKind): 65,600
// top-level lets took seconds. The count is the compiler's work, not a
// time, which other work on the machine would change.
func TestCompileLexicalLinear(t *testing.T) {
	t.Parallel()
	lets := func(b *strings.Builder, n int) {
		for i := 0; i < n; i++ {
			fmt.Fprintf(b, "let v%d = %d;\n", i, i)
		}
	}
	vars := func(b *strings.Builder, n int) {
		for i := 0; i < n; i++ {
			fmt.Fprintf(b, "var w%d = %d;\n", i, i)
		}
	}
	shapes := []struct {
		name string
		src  func(b *strings.Builder, n int)
	}{
		{"function lets", func(b *strings.Builder, n int) {
			b.WriteString("function f() {\n")
			lets(b, n)
			b.WriteString("return v0 }\n")
		}},
		{"lets and vars", func(b *strings.Builder, n int) {
			lets(b, n)
			vars(b, n)
		}},
		{"block lets and vars", func(b *strings.Builder, n int) {
			b.WriteString("function f() {\n{\n")
			lets(b, n)
			vars(b, n)
			b.WriteString("}\n}\n")
		}},
	}
	const n = 4100
	for _, sh := range shapes {
		looks := func(n int) uint64 {
			var b strings.Builder
			sh.src(&b, n)
			src := b.String()
			prog, err := parser.Parse(src, parser.Options{})
			if err != nil {
				t.Fatal(err)
			}
			var looks uint64
			if _, err := Compile(prog, Options{Source: "linear.js", Text: src, looks: &looks}); err != nil {
				t.Fatal(err)
			}
			return looks
		}
		small, large := looks(n), looks(2*n)
		if small == 0 || float64(large) > 3*float64(small) {
			t.Errorf("%s: %d bindings looked at %d, %d at %d", sh.name, n, small, 2*n, large)
		}
	}
}
