package ssa

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/compiler"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

func BenchmarkBuildOptimize(b *testing.B) {
	for _, tc := range []struct {
		name, src string
		feedback  Feedback
	}{
		{"sum", `function f(n){let s=0;for(let i=0;i<n;i++)s+=i;return s}`, nil},
		{"branches", `function f(o,n){let s=0;for(let i=0;i<n;i++){` +
			strings.Repeat(`if(o.x>i)s+=o.y;else s-=o.z;`, 16) + `}return s}`, nil},
		{"calls", `function f(o,n){let s=0;for(let i=0;i<n;i++){` +
			strings.Repeat(`s+=o.f(i);`, 16) + `}return s}`, nil},
		{"prototype", `function f(o,n){let s=0;for(let i=0;i<n;i++){` +
			strings.Repeat(`s+=o.x;o=o.next;`, 16) + `}return s}`,
			siteFeedback{PropertySite{Shape: 0x10000, Holders: [2]Holder{{Object: 0x20000, Shape: 0x30000}}}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ast, err := parser.Parse(tc.src, parser.Options{})
			if err != nil {
				b.Fatal(err)
			}
			fn, err := compiler.Compile(ast, compiler.Options{})
			if err != nil {
				b.Fatal(err)
			}
			fn = fn.Constants[0].Fn
			p, err := jitcompile.LowerReg(fn.RegCode(), fn)
			if err != nil {
				b.Fatal(err)
			}
			var w Workspace
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				f, err := BuildIn(&w, p, tc.feedback)
				if err != nil {
					b.Fatal(err)
				}
				Optimize(f)
				w.Rewind()
			}
		})
	}
}

func BenchmarkWorkspaceMetadata(b *testing.B) {
	for _, name := range []string{"holder", "inline", "accessor"} {
		b.Run(name, func(b *testing.B) {
			var w Workspace
			w.init()
			f := &Func{ws: &w}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				for i := range 128 {
					switch name {
					case "holder":
						f.holderChain(HolderChain{Holders: [2]Holder{{Object: uintptr(i)}}})
					case "inline":
						f.inlineState(InlineState{Base: i})
					case "accessor":
						f.accessorSite(AccessorSite{Function: uintptr(i)})
					}
				}
				w.Rewind()
			}
		})
	}
}
