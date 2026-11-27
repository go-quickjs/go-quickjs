package spanclock

import "testing"

var sink int

// Now resolves spans of microseconds on every system: fifty spans of some
// microseconds' work each read as more than none. Go's own clock on
// Windows, which moves at the system timer's tick, read ten such spans in
// ten as none.
func TestResolves(t *testing.T) {
	for i := 0; i < 50; i++ {
		from := Now()
		for k := 0; k < 20000; k++ {
			sink += k * k
		}
		if d := Now() - from; d <= 0 {
			t.Fatalf("span %d read as %v", i, d)
		}
	}
}

// Now never goes back.
func TestMonotonic(t *testing.T) {
	last := Now()
	for i := 0; i < 10000; i++ {
		now := Now()
		if now < last {
			t.Fatalf("went back from %v to %v", last, now)
		}
		last = now
	}
}
