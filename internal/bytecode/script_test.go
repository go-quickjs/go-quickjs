package bytecode

import (
	"sync"
	"testing"
)

func TestScriptPositions(t *testing.T) {
	s := NewScript("a.js", "ab\n\ncd😀e\n")
	s.SetOffset(10, 3)
	if s.lineStarts != nil {
		t.Error("the line table was made before a position was asked for")
	}
	for _, tt := range []struct{ pos, line, col int32 }{
		{0, 11, 4}, {2, 11, 6}, {3, 12, 1}, {4, 13, 1}, {6, 13, 3}, {10, 13, 5}, {11, 13, 6}, {12, 14, 1},
	} {
		if line, col := s.Position(tt.pos); line != tt.line || col != tt.col {
			t.Errorf("Position(%d) = %d:%d, want %d:%d", tt.pos, line, col, tt.line, tt.col)
		}
	}
	if line, col := s.Position(13); line != 0 || col != 0 {
		t.Errorf("past the end: %d:%d", line, col)
	}
	if len(s.lineStarts) != cap(s.lineStarts) {
		t.Errorf("a table of %d lines holds room for %d", len(s.lineStarts), cap(s.lineStarts))
	}
}

// A compiled Program's script is shared by the runtimes running it.
func TestScriptLinesConcurrent(t *testing.T) {
	s := NewScript("a.js", "a\nb\nc\nd\n")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if line, _ := s.Position(4); line != 3 {
				t.Errorf("line %d", line)
			}
		}()
	}
	wg.Wait()
}
