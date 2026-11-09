package mir

import "testing"

// The encoded arm64 buffer is scratch memory, counted at its capacity
// while kept, and trimmed once the recent compiles no longer need it.
func TestWorkspaceARM64Code(t *testing.T) {
	var w Workspace
	before, used := w.Bytes(), w.scratchUsed()
	w.arm64.code = make([]byte, 1<<20)
	if got := w.Bytes() - before; got != cap(w.arm64.code) {
		t.Fatalf("counts %d bytes, want %d", got, cap(w.arm64.code))
	}
	w.Rewind()
	if w.arm64.code == nil {
		t.Fatal("discarded the last compile's buffer")
	}
	w.arm64.code = w.arm64.code[:16]
	if got := w.scratchUsed() - used; got != 16 {
		t.Fatalf("counts %d bytes used, want 16", got)
	}
	for range len(w.needs) {
		w.Rewind()
		if w.arm64.code == nil {
			t.Fatal("discarded a buffer a recent compile needed")
		}
	}
	w.Rewind()
	if w.arm64.code != nil || w.Bytes() != before {
		t.Fatalf("keeps %d bytes after the large compile aged out", w.Bytes()-before)
	}
}
