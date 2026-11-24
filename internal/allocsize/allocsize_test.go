package allocsize

import (
	"runtime"
	"testing"
)

// TestClassesAreTheRuntimes checks the table against the size classes the
// running Go's allocator reports, as many as runtime.MemStats has room for:
// a Go whose classes changed fails it.
func TestClassesAreTheRuntimes(t *testing.T) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	n := 0
	for _, c := range ms.BySize[1:] {
		if c.Size == 0 {
			break
		}
		if n >= len(classes) || uint32(classes[n]) != c.Size {
			t.Fatalf("class %d is %d bytes in the runtime, %d in the table", n+1, c.Size, classes[min(n, len(classes)-1)])
		}
		n++
	}
	if n < 60 {
		t.Fatalf("the runtime reported %d classes", n)
	}
}

// Of rounds a size up to the class it falls in, and past the largest class
// to whole pages.
func TestOf(t *testing.T) {
	for _, c := range []struct{ n, want int }{
		{-1, 0}, {0, 0}, {1, 8}, {8, 8}, {9, 16}, {28, 32}, {44, 48}, {48, 48}, {49, 64},
		{257, 288}, {1025, 1152}, {32767, 32768}, {32768, 32768}, {32769, 40960}, {40960, 40960}, {1 << 20, 1 << 20},
	} {
		if got := Of(c.n); got != c.want {
			t.Errorf("Of(%d) = %d, want %d", c.n, got, c.want)
		}
	}
	for n := 1; n <= 32768; n++ {
		got := Of(n)
		if got < n || Of(got) != got || (n > 8 && Of(n-1) > got) {
			t.Fatalf("Of(%d) = %d: not the least class at or above it", n, got)
		}
	}
}
