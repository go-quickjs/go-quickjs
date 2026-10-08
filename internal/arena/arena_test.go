package arena

import "testing"

// TestMake pins that slices made do not overlap, have no room to grow into
// a neighbour, and are zeroed, across chunk boundaries and a request bigger
// than a chunk.
func TestMake(t *testing.T) {
	var a Arena[int]
	var got [][]int
	for i, n := range []int{1, 3, 200, 60, 1000, 5, 400, 7} {
		s := a.Make(n)
		if len(s) != n || cap(s) != n {
			t.Fatalf("Make(%d): len %d cap %d", n, len(s), cap(s))
		}
		for j := range s {
			if s[j] != 0 {
				t.Fatalf("Make(%d)[%d] = %d, want 0", n, j, s[j])
			}
			s[j] = i + 1
		}
		got = append(got, s)
	}
	for i, s := range got {
		for j, v := range s {
			if v != i+1 {
				t.Fatalf("slice %d slot %d = %d, overwritten", i, j, v)
			}
		}
	}
	if a.Make(0) != nil {
		t.Error("Make(0) is not nil")
	}
}

// TestRewind pins that a rewound arena hands out zeroed slots from the
// chunks it kept, allocating nothing for a job no bigger than the last.
func TestRewind(t *testing.T) {
	var a Arena[*int]
	x := 1
	job := func() {
		for i := 0; i < 50; i++ {
			s := a.Make(i%9 + 1)
			for j := range s {
				if s[j] != nil {
					t.Fatalf("slot not cleared")
				}
				s[j] = &x
			}
		}
	}
	job()
	before := a.Cap()
	a.Rewind()
	if n := testing.AllocsPerRun(100, func() { job(); a.Rewind() }); n != 0 {
		t.Errorf("a warm job allocates %v times", n)
	}
	if a.Cap() != before {
		t.Errorf("chunks grew from %d to %d", before, a.Cap())
	}
}

// TestAppend pins Append's growth out of the arena.
func TestAppend(t *testing.T) {
	var a Arena[int]
	s := a.Make(1)[:0]
	for i := 0; i < 100; i++ {
		s = a.Append(s, i)
	}
	for i, v := range s {
		if v != i {
			t.Fatalf("s[%d] = %d", i, v)
		}
	}
}

// TestForget pins that Forget reuses the chunks without clearing them.
func TestForget(t *testing.T) {
	var a Arena[int32]
	s := a.Make(3)
	s[0] = 7
	a.Forget()
	if u := a.Make(3); &u[0] != &s[0] || u[0] != 7 {
		t.Error("Forget did not hand back the same slots as they were")
	}
}
