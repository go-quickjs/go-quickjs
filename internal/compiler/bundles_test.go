package compiler

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bundletest"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// BenchmarkCompileBundle isolates the compiler from parsing and execution.
func BenchmarkCompileBundle(b *testing.B) {
	for _, bundle := range bundletest.All {
		b.Run(bundle.Name, func(b *testing.B) {
			src := bundletest.Read(b, bundle)
			prog, err := parser.Parse(src, parser.Options{})
			if err != nil {
				b.Fatal(err)
			}
			opts := Options{Source: bundle.File, Text: src}
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Compile(prog, opts); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
