package engine

import (
	"math/bits"
	"sync"
	"testing"
)

// TestSharedMemoryNotHad checks that a shared memory the process cannot
// have fails to be made. A 32-bit process making one of 2 GiB's maximum --
// which a shared memory takes at once -- ended with a fatal "out of memory"
// from a Go allocation; it reserves the address space now, which fails.
func TestSharedMemoryNotHad(t *testing.T) {
	if bits.UintSize != 32 {
		t.Skip("a 64-bit process has the address space")
	}
	var held []*Memory
	for range 4 {
		m := NewMemory(1, MaxMemory32Pages, true, false, true)
		if m == nil {
			return
		}
		held = append(held, m)
	}
	t.Fatalf("made %d shared memories of 2 GiB in a 32-bit address space", len(held))
}

func TestMemoryGrow(t *testing.T) {
	for _, shared := range []bool{false, true} {
		m := NewMemory(1, 4, true, false, shared)
		if m == nil {
			t.Fatal("NewMemory failed")
		}
		b := m.Bytes()
		if len(b) != PageSize {
			t.Fatalf("shared=%v: size %d, want %d", shared, len(b), PageSize)
		}
		b[PageSize-1] = 7
		if got := m.Grow(2); got != 1 {
			t.Fatalf("shared=%v: Grow(2) = %d, want 1", shared, got)
		}
		b = m.Bytes()
		if len(b) != 3*PageSize || b[PageSize-1] != 7 || b[PageSize] != 0 || b[3*PageSize-1] != 0 {
			t.Fatalf("shared=%v: after grow: len %d, kept %d, new %d", shared, len(b), b[PageSize-1], b[PageSize])
		}
		if got := m.Grow(2); got != -1 {
			t.Fatalf("shared=%v: Grow past max = %d, want -1", shared, got)
		}
		if got := m.Grow(0); got != 3 {
			t.Fatalf("shared=%v: Grow(0) = %d, want 3", shared, got)
		}
		if got := m.Grow(1); got != 3 || m.Pages() != 4 {
			t.Fatalf("shared=%v: Grow(1) = %d, pages %d", shared, got, m.Pages())
		}
	}
}

func TestMemoryWithoutMaximum(t *testing.T) {
	m := NewMemory(0, 0, false, false, false)
	if m == nil || m.Max != MaxMemory32Pages || m.Size() != 0 {
		t.Fatalf("memory without maximum: %+v", m)
	}
	if got := m.Grow(16); got != 0 || m.Size() != 16*PageSize {
		t.Fatalf("Grow(16) = %d, size %d", got, m.Size())
	}
	if got := m.Grow(MaxMemory32Pages); got != -1 {
		t.Fatalf("Grow past the engine's limit = %d", got)
	}
	if NewMemory(MaxMemory32Pages+1, 0, false, false, false) != nil {
		t.Fatal("a memory past the engine's limit was made")
	}
}

func TestMemoryOnGrow(t *testing.T) {
	m := NewMemory(1, 2, true, false, false)
	var oldB, newB uint64
	m.OnGrow = func(o, n uint64) { oldB, newB = o, n }
	m.Grow(1)
	if oldB != PageSize || newB != 2*PageSize {
		t.Fatalf("OnGrow(%d, %d)", oldB, newB)
	}
}

func TestSharedMemoryConcurrentGrow(t *testing.T) {
	m := NewMemory(0, 64, true, false, true)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				if m.Grow(1) < 0 {
					t.Error("grow failed")
				}
				b := m.Bytes()
				b[len(b)-1]++
			}
		}()
	}
	wg.Wait()
	if m.Pages() != 64 {
		t.Fatalf("pages %d, want 64", m.Pages())
	}
}

// TestSharedMemoryHostView pins what a host keeps with a shared memory: a
// view made once, told of every grow -- by Grow and by GrowTo, from any
// goroutine -- before the grow returns; GrowTo refusing to shrink, to pass
// the maximum, and taking a grow to the size the memory has.
func TestSharedMemoryHostView(t *testing.T) {
	m := NewMemory(1, 8, true, false, true)
	var mu sync.Mutex
	var told []uint64
	made := 0
	view := func() (any, func(uint64)) {
		made++
		return "view", func(n uint64) { mu.Lock(); told = append(told, n); mu.Unlock() }
	}
	if v := m.HostView(view); v != "view" || m.HostView(view) != "view" || made != 1 {
		t.Fatalf("view %v, made %d times", v, made)
	}
	if len(m.Span()) != 8*PageSize || len(m.Bytes()) != PageSize {
		t.Fatalf("span %d bytes, size %d", len(m.Span()), len(m.Bytes()))
	}
	m.Grow(1)
	if !m.GrowTo(2) || !m.GrowTo(4) || m.GrowTo(3) || m.GrowTo(9) || m.Pages() != 4 {
		t.Fatalf("GrowTo: %d pages", m.Pages())
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); m.Grow(1) }()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	want := []uint64{2, 4, 5, 6, 7, 8}
	for i := range want {
		want[i] *= PageSize
	}
	if len(told) != len(want) {
		t.Fatalf("told %v, want %v", told, want)
	}
	for i := range want {
		if told[i] != want[i] {
			t.Fatalf("told %v, want %v", told, want)
		}
	}
	if NewMemory(1, 1, true, false, false).Span() != nil {
		t.Error("an unshared memory has a span")
	}
}

// TestSharedMemoryNoPages pins that a shared memory whose maximum is no
// pages can be made: there is nothing to reserve.
func TestSharedMemoryNoPages(t *testing.T) {
	m := NewMemory(0, 0, true, false, true)
	if m == nil || m.Size() != 0 || m.Grow(1) != -1 || m.Grow(0) != 0 {
		t.Fatalf("got %v", m)
	}
}
