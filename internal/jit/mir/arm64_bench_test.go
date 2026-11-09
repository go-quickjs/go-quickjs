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
	for _, tc := range []struct{ name, src string }{
		{"sum", `function f(n){let s=0;for(let i=0;i<n;i++)s+=i;return s}`},
		{"region", `function f(n){let a=1,b=2,c=3,d=4;for(let i=0;i<n;i++){` +
			strings.Repeat(`a=(a+b*i)|0;b=b+c*0.5;c=(c^a)&255;d=d+a-b;`, 8) + `}return a+b+c+d}`},
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
			f, err := ssa.Build(p)
			if err != nil {
				b.Fatal(err)
			}
			ssa.Optimize(f)
			var w Workspace
			w.init()
			enc, opts := abi.Encoding{ValueSize: 16, StoreRef: 1}, Settings()
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
