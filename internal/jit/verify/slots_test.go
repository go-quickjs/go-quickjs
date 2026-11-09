//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// manySlots is a function of n locals whose loop reads and writes its
// first, middle and last, exits at a guard of each, and calls a function
// it is given, its state naming them.
func manySlots(n int) string {
	var b strings.Builder
	b.WriteString("function f(g, rounds) {")
	for i := range n {
		fmt.Fprintf(&b, " let v%d = %d;", i, i)
	}
	fmt.Fprintf(&b, " for (let i = 0; i < rounds; i++) { v0 = v0 + i; v%d = v%d * 2 + v0; v%d = g(v%d, v%d) } return v0 + v%d + v%d }",
		n/2, n/2, n-1, n-2, n/2, n-1, n-2)
	return b.String()
}

// loweredSSA is src's functions as the new pipeline lowers them for Go's
// entry.
func loweredSSA(t *testing.T, src string) []*ir.Program {
	t.Helper()
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	top, err := compiler.Compile(prog, compiler.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var out []*ir.Program
	for _, c := range top.Constants {
		if c.Kind == bytecode.ConstFunction {
			p, err := jitcompile.LowerSSA(c.Fn)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
	}
	return out
}

// Functions of 300, 1,000 and 5,000 locals compile for both targets, their
// frames' slots past every short displacement -- arm64's scaled offsets end
// at 2,047 slots of 16 bytes -- and decode in full.
func TestMirManySlots(t *testing.T) {
	enc := abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
		Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
		False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
		UndefinedCell: 0x60000, StoreRef: 0x70000}
	for _, n := range []int{300, 1000, 5000} {
		for i, p := range loweredSSA(t, manySlots(n)) {
			if p.Locals < n {
				t.Fatalf("%d locals: program of %d", n, p.Locals)
			}
			for _, fb := range []ssa.Feedback{nil, calls{props{p: p}}} {
				f, err := ssa.BuildWith(p, fb)
				if err != nil {
					t.Fatalf("%d locals (%d): %v", n, i, err)
				}
				f.CallEntry = true
				ssa.Optimize(f)
				for _, arch := range arches {
					compile := mir.CompileAMD64
					if arch == "arm64" {
						compile = mir.CompileARM64
					}
					mc, err := compile(f, enc)
					if err != nil {
						t.Fatalf("%d locals (%d), %s: %v", n, i, arch, err)
					}
					if err := check(arch, mc.Bytes[:mc.Text]); err != nil {
						t.Fatalf("%d locals (%d), %s: %v", n, i, arch, err)
					}
				}
			}
		}
	}
}
