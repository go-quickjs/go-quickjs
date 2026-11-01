package compiler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/parser"
)

func BenchmarkCompileManyLocals(b *testing.B) {
	for _, count := range []int{8, 64, 512, 4096} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			var src strings.Builder
			src.WriteString("(function(){")
			for i := 0; i < count; i++ {
				fmt.Fprintf(&src, "var v%d = %d;", i, i)
			}
			for i := 0; i < count; i++ {
				fmt.Fprintf(&src, "function f%d(){ return v%d + v0 + Math.PI; }", i, i)
			}
			src.WriteString("})()")
			prog, err := parser.Parse(src.String(), parser.Options{})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Compile(prog, Options{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
