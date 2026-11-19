package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// BenchmarkAtomicsApart is runtimes on goroutines of their own, each on a
// SharedArrayBuffer of its own, each operation an Atomics.notify that wakes
// no one and an Atomics.wait whose memory does not hold the value: what
// the process's one table of waiters costs runtimes that share nothing.
func BenchmarkAtomicsApart(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		rt := quickjs.New()
		defer rt.Close()
		if _, err := rt.Eval(`var ta = new Int32Array(new SharedArrayBuffer(8));
function ops(n) { var s = 0; for (var i = 0; i < n; i++) { s += Atomics.notify(ta, 0, 1); if (Atomics.wait(ta, 0, 1) === 'not-equal') s++ } return s }
ops(100)`); err != nil {
			b.Error(err)
			return
		}
		for pb.Next() {
			if v, err := rt.Eval(`ops(100)`); err != nil || v.Int() != 100 {
				b.Errorf("ops(100) = %v, %v", v, err)
				return
			}
		}
	})
}
