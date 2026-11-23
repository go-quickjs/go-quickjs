package vm

import (
	"fmt"
	"runtime"
	"testing"
)

func BenchmarkGeneratorAllocation(b *testing.B) {
	for _, kind := range []struct {
		name string
		kind int
	}{{"plain", genPlainKind}, {"loop", genLoopKind}, {"async", genAsyncKind}} {
		for _, n := range []int{2, 4, 6, 8, 9} {
			b.Run(fmt.Sprintf("%s/%d", kind.name, n), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					o, vals := genAlloc(n, kind.kind)
					o.g.vals = vals
					runtime.KeepAlive(o)
				}
			})
		}
	}
}
