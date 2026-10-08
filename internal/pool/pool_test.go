package pool

import (
	"sync"
	"testing"
)

type scratch struct{ buf []int }

// TestPool pins that Put resets what it keeps and Get hands back a reset
// value or a new one, from many goroutines at once.
func TestPool(t *testing.T) {
	made := 0
	var mu sync.Mutex
	p := New(func() *scratch {
		mu.Lock()
		made++
		mu.Unlock()
		return &scratch{}
	}, func(s *scratch) { s.buf = s.buf[:0] })
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				s := p.Get()
				if len(s.buf) != 0 {
					t.Error("Get gave a value that was not reset")
					return
				}
				s.buf = append(s.buf, i, i)
				p.Put(s)
			}
		}()
	}
	wg.Wait()
	if made == 0 {
		t.Error("nothing was made")
	}
}

// TestAllocator pins that the allocator hands back what it kept, reset,
// keeps no more than its cap, and is safe from many goroutines at once.
func TestAllocator(t *testing.T) {
	a := NewAllocator(2, func() *scratch { return &scratch{} }, func(s *scratch) { s.buf = s.buf[:0] })
	x := a.Get()
	x.buf = append(x.buf, 1)
	a.Put(x)
	if y := a.Get(); y != x || len(y.buf) != 0 {
		t.Fatal("Get did not give back the reset value Put kept")
	}
	a.Put(x)
	a.Put(&scratch{})
	a.Put(&scratch{})
	if len(a.free) != 2 {
		t.Errorf("the list holds %d, past its cap of 2", len(a.free))
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				s := a.Get()
				if len(s.buf) != 0 {
					t.Error("Get gave a value that was not reset")
					return
				}
				s.buf = append(s.buf, i)
				a.Put(s)
			}
		}()
	}
	wg.Wait()
}
