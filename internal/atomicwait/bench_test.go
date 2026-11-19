package atomicwait

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// The table's cost to agents of separate memories, each goroutine its own
// memory, all at once: what one lock for the process costs them, against
// none shared. Each operation takes the table's lock once.

// BenchmarkNotifyApart is a notify no agent waits for, by each goroutine
// on its own memory.
func BenchmarkNotifyApart(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		mem := new(int)
		for pb.Next() {
			Notify(mem, 0, 1)
		}
	})
}

// BenchmarkWaitNotEqualApart is a wait whose memory does not hold the value
// expected, by each goroutine on its own memory.
func BenchmarkWaitNotEqualApart(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		mem := new(int)
		for pb.Next() {
			Wait(mem, 0, no, -1, nil, nil)
		}
	})
}

// BenchmarkPingPongApart is pairs of agents, GOMAXPROCS of them, each pair
// on a memory of its own handing a turn to and fro: each waits while the
// turn is the other's and, having taken it, gives it back and notifies.
// b.N is the turns each pair hands over.
func BenchmarkPingPongApart(b *testing.B) {
	pairs := max(runtime.GOMAXPROCS(0)/2, 1)
	var wg sync.WaitGroup
	b.ResetTimer()
	for range pairs {
		mem := new(int)
		var turn atomic.Int32
		for side := range int32(2) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range b.N {
					for turn.Load() != side {
						Wait(mem, 0, func() bool { return turn.Load() != side }, -1, nil, nil)
					}
					turn.Store(1 - side)
					Notify(mem, 0, 1)
				}
			}()
		}
	}
	wg.Wait()
}
