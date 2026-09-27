package quickjs_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestHugeArrayLikesStaySmall pins that walking an array-like whose length
// is in the thousands of millions takes time -- as it does in V8, and which a
// deadline stops -- but not memory: a slice of one used to write every hole
// out, and an unshift interned a name for every index past 2^31, until the
// process ran out of memory (KI-06).
func TestHugeArrayLikesStaySmall(t *testing.T) {
	for name, src := range map[string]string{
		"slice":   `Array.prototype.slice.call({length: 2 ** 32 - 1})`,
		"unshift": `var d = []; d.length = 2 ** 32 - 1; d.unshift(1)`,
		"splice":  `Array.prototype.splice.call({length: 2 ** 32 + 5}, 0, 1)`,
	} {
		t.Run(name, func(t *testing.T) {
			rt := quickjs.New()
			defer rt.Close()
			before := heapInUse()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			peak := make(chan int64)
			go func() {
				// The heap at its largest while the script runs: what it
				// wrote out may be garbage by the time it is stopped.
				var most int64
				for ctx.Err() == nil {
					var m runtime.MemStats
					runtime.ReadMemStats(&m)
					most = max(most, int64(m.HeapAlloc))
					time.Sleep(20 * time.Millisecond)
				}
				peak <- most
			}()
			if _, err := rt.EvalContext(ctx, src); err == nil {
				t.Fatal("finished; expected the deadline")
			}
			if grew := <-peak - before; grew > 256<<20 {
				t.Errorf("the heap grew by %d MB in a second", grew>>20)
			}
			if kept := heapInUse() - before; kept > 64<<20 {
				t.Errorf("%d MB were kept", kept>>20)
			}
		})
	}
}

func heapInUse() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapInuse)
}
