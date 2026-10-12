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

// TestKeepBig pins that an arena that keeps big requests (KeepBig) cuts
// the next job's big slice from the chunk the last one made, allocating
// nothing; one that does not hands big slices out on their own each time.
func TestKeepBig(t *testing.T) {
	job := func(a *Arena[int]) {
		a.Make(10)
		a.Make(5000)
		a.Make(3)
		a.Rewind()
	}
	var kept Arena[int]
	kept.KeepBig()
	job(&kept)
	if n := testing.AllocsPerRun(10, func() { job(&kept) }); n != 0 {
		t.Fatalf("a job no bigger than the last allocated %v times", n)
	}
	if s := kept.Make(5000); len(s) != 5000 || cap(s) != 5000 {
		t.Fatalf("Make(5000): len %d cap %d", len(s), cap(s))
	}
	var plain Arena[int]
	job(&plain)
	if n := testing.AllocsPerRun(10, func() { job(&plain) }); n == 0 {
		t.Fatal("an arena that does not keep big requests kept one")
	}
}

// TestKeepBigTrims pins what an arena that keeps big requests keeps: a job
// bigger than every recent one before it gives its chunks back as it is
// rewound, down to what those needed; jobs as big that come among the
// recent ones keep them.
func TestKeepBigTrims(t *testing.T) {
	small := func(a *Arena[int]) {
		a.Make(10)
		a.Make(500)
		a.Rewind()
	}
	big := func(a *Arena[int]) {
		a.Make(10)
		a.Make(1 << 20)
		a.Rewind()
	}
	var a Arena[int]
	a.KeepBig()
	small(&a)
	usual := a.Cap()
	big(&a)
	if a.Cap() > usual {
		t.Fatalf("a big job's chunks kept: %d slots, %d after the usual job", a.Cap(), usual)
	}
	small(&a)
	if n := testing.AllocsPerRun(10, func() { small(&a) }); n != 0 {
		t.Fatalf("the usual job allocated %v times after a big one", n)
	}
	// A big job among the recent ones: the next as big allocates nothing.
	big(&a)
	big(&a)
	if a.Cap() < 1<<20 {
		t.Fatalf("a big job among the recent ones did not keep its chunks: %d slots", a.Cap())
	}
	if n := testing.AllocsPerRun(10, func() { big(&a) }); n != 0 {
		t.Fatalf("a big job like a recent one allocated %v times", n)
	}
	// Once the big jobs are past the recent ones, the next big one goes.
	for range recentJobs {
		small(&a)
	}
	big(&a)
	if a.Cap() > usual {
		t.Fatalf("a big job past the recent ones kept its chunks: %d slots", a.Cap())
	}
}

// TestRewindSkippedTail pins that a rewind clears what was cut from a chunk
// a later request did not fit in, and that slots past it, never handed out,
// come out zeroed in the next job too: Rewind clears only what was cut.
func TestRewindSkippedTail(t *testing.T) {
	var a Arena[int]
	fill := func(s []int) {
		for i := range s {
			s[i] = 7
		}
	}
	for job := range 4 {
		for _, n := range []int{40 + job*8, 30, 64, 10} {
			s := a.Make(n)
			for i, v := range s {
				if v != 0 {
					t.Fatalf("job %d: Make(%d)[%d] = %d", job, n, i, v)
				}
			}
			fill(s)
		}
		a.Rewind()
	}
}
