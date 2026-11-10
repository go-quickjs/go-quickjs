//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64) || (linux && arm64))

package jit

import "testing"

// A reusing arena places code in the space released code left, the first
// space large enough, joining released neighbours into one; it unmaps a
// chunk once nothing in it is owned, and code is placed past a chunk's end
// only when no space released is large enough.
func TestReusingArena(t *testing.T) {
	a := NewReusingArena()
	place := func(n int) *RawCode {
		t.Helper()
		r, err := PlaceRaw(a, make([]byte, n))
		if err != nil {
			t.Skipf("no executable memory: %v", err)
		}
		return r
	}
	r1, r2, r3, r4 := place(100), place(64), place(200), place(64)
	if r2.Base() != r1.Base()+128 || r3.Base() != r2.Base()+64 || r4.Base() != r3.Base()+256 {
		t.Fatalf("not packed: %#x %#x %#x %#x", r1.Base(), r2.Base(), r3.Base(), r4.Base())
	}
	_, live := a.Mapped()
	if live != 128+64+256+64 {
		t.Fatalf("live %d, want %d", live, 128+64+256+64)
	}
	// r2's and r3's space, joined, holds a code of 320 bytes; r1's alone
	// does not, and it goes after it.
	base := r1.Base()
	r2.Close()
	r3.Close()
	r5 := place(320)
	if r5.Base() != base+128 {
		t.Fatalf("320 bytes at %#x, want r2's %#x", r5.Base(), base+128)
	}
	r1.Close()
	r6 := place(192)
	if r6.Base() != r4.Base()+64 {
		t.Fatalf("192 bytes at %#x, want past r4's end %#x", r6.Base(), r4.Base()+64)
	}
	r7 := place(64)
	if r7.Base() != base {
		t.Fatalf("64 bytes at %#x, want r1's %#x", r7.Base(), base)
	}
	_, live = a.Mapped()
	if live != 64+320+64+192 {
		t.Fatalf("live %d, want %d", live, 64+320+64+192)
	}
	for _, r := range []*RawCode{r4, r5, r6, r7} {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if n := a.Chunks(); n != 0 {
		t.Fatalf("%d chunks mapped once all code is released", n)
	}
}
