package mir

import (
	"fmt"
	"strings"
	"testing"

	jscompile "github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// Many edges supply the same values to a join, with enough live phis to
// spill. Each edge still contributes a use and its own spill cost.
func repeatedPhiSource(edges, values int) string {
	var s strings.Builder
	s.WriteString("function f(n,a){")
	for i := range values {
		fmt.Fprintf(&s, "let a%d=a+%d,x%d;", i, i, i)
	}
	s.WriteString("switch(n){")
	for i := range edges {
		fmt.Fprintf(&s, "case %d:", i)
		for j := range values {
			fmt.Fprintf(&s, "x%d=a%d;", j, (j+i%2)%values)
		}
		s.WriteString("break;")
	}
	s.WriteString("default:")
	for j := range values {
		fmt.Fprintf(&s, "x%d=a%d;", j, j)
	}
	s.WriteString("}return 0")
	for j := range values {
		fmt.Fprintf(&s, "+x%d", j)
	}
	s.WriteString("}")
	return s.String()
}

func TestRepeatedPhiInputs(t *testing.T) {
	a, err := parser.Parse(repeatedPhiSource(64, 32), parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fn, err := jscompile.Compile(a, jscompile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fn = fn.Constants[0].Fn
	p, err := jitcompile.LowerReg(fn.RegCode(), fn)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ssa.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	ssa.Optimize(f)
	duplicates := 0
	for _, block := range f.Blocks {
		for _, v := range block.Values {
			if v.Op != ssa.OpPhi {
				continue
			}
			seen := map[*ssa.Value]bool{}
			for _, arg := range v.Args {
				if seen[arg] && arg != v {
					duplicates++
				}
				seen[arg] = true
			}
		}
	}
	if duplicates < 100 {
		t.Fatalf("fixture has only %d repeated phi inputs", duplicates)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			var w Workspace
			w.init()
			opts := Settings()
			opts.CheckAllocations = true
			enc := abi.Encoding{ValueSize: 16, StoreRef: 1, CallGo: 1}
			for range 2 {
				var code *Code
				if arch == "amd64" {
					code, err = compileAMD64(&w, f, enc, opts)
				} else {
					code, err = compileARM64(&w, f, enc, opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				if code.Spills == 0 {
					t.Fatal("fixture did not spill")
				}
				w.Rewind()
			}
		})
	}
}
