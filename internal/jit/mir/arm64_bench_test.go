package mir

import (
	"strings"
	"testing"

	jscompile "github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// Measure the backend without building SSA or mapping executable memory.
// The same optimized function is compiled in a rewound workspace, as in
// the VM's repeated compiles, and the resulting bytes are never executed.
func BenchmarkCompileARM64(b *testing.B) {
	for _, tc := range []struct {
		name, src string
		feedback  ssa.Feedback
	}{
		{"sum", `function f(n){let s=0;for(let i=0;i<n;i++)s+=i;return s}`, nil},
		{"region", `function f(n){let a=1,b=2,c=3,d=4;for(let i=0;i<n;i++){` +
			strings.Repeat(`a=(a+b*i)|0;b=b+c*0.5;c=(c^a)&255;d=d+a-b;`, 8) + `}return a+b+c+d}`, nil},
		{"calls", `function f(o,n){let s=0;for(let i=0;i<n;i++){` +
			strings.Repeat(`s+=o.f(i);`, 8) + `}return s}`, nil},
		{"properties", `function f(o,n){let s=0;for(let i=0;i<n;i++){` +
			strings.Repeat(`s+=o.x;o=o.next;`, 8) + `}return s}`, arm64BenchFeedback{}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ast, err := parser.Parse(tc.src, parser.Options{})
			if err != nil {
				b.Fatal(err)
			}
			fn, err := jscompile.Compile(ast, jscompile.Options{})
			if err != nil {
				b.Fatal(err)
			}
			p, err := jitcompile.LowerSSA(fn.Constants[0].Fn)
			if err != nil {
				b.Fatal(err)
			}
			f, err := ssa.BuildWith(p, tc.feedback)
			if err != nil {
				b.Fatal(err)
			}
			ssa.Optimize(f)
			var w Workspace
			w.init()
			enc, opts := abi.Encoding{ValueSize: 16, StoreRef: 1, CallGo: 1}, Settings()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				code, err := compileARM64(&w, f, enc, opts)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(len(code.Bytes)), "code-B")
				w.Rewind()
			}
		})
	}
}

type arm64BenchFeedback struct{}

func (arm64BenchFeedback) Property(pc int) (ssa.PropertySite, bool) {
	return ssa.PropertySite{Shape: 0x10000, Index: int32(pc % 4), Cases: []ssa.PropertyCase{
		{Shape: 0x20000, Index: 1}, {Shape: 0x30000, Index: 2},
	}}, true
}
func (arm64BenchFeedback) Global(int) (ssa.GlobalSite, bool) { return ssa.GlobalSite{}, false }
func (arm64BenchFeedback) Generic(int) bool                  { return false }
func (arm64BenchFeedback) EntryGeneric(int) bool             { return false }
func (arm64BenchFeedback) Inline(int) (ssa.InlineSite, bool) { return ssa.InlineSite{}, false }
func (arm64BenchFeedback) NativeCalls(int) []ssa.CallSite    { return nil }
