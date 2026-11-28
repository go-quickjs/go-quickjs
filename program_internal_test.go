package quickjs

import (
	"context"
	"testing"
)

// A program run again finds its literals' layouts: its runs make none of
// them, which is six shapes a run of this one, each an allocation, and keys
// beside them, in a runtime's realm or any other. The same bytecode run
// otherwise makes them each time, for literals that say no list of keys --
// a computed key's: those with keys, and {}, are the realm's, whatever runs
// them (literalkeys.go), and {}'s run anew makes no more than a program's.
func TestProgramRerunsMakeNoLayouts(t *testing.T) {
	testProgramRerunsMakeNoLayouts(t, `var o = {["a"]: 1}, q = {["c"]: 3}; o.b = 2; q.d = 4; o.a + q.d`, 6)
	testProgramRerunsMakeNoLayouts(t, `var o = {}, q = {}; o.a = 1; o.b = 2; q.c = 3; q.d = 4; o.a + q.d`, 0)
}

// testProgramRerunsMakeNoLayouts is TestProgramRerunsMakeNoLayouts for src,
// whose runs as a program make fewer allocations than its runs anew by
// fewer at least.
func testProgramRerunsMakeNoLayouts(t *testing.T, src string, fewer float64) {
	r := New()
	defer r.Close()
	re, err := r.NewRealm()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile("rerun.js", src)
	if err != nil {
		t.Fatal(err)
	}
	fn, err := p.code(r)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check := func(v Value, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if v.Int() != 5 {
			t.Fatalf("got %v, want 5", v)
		}
	}
	anew := testing.AllocsPerRun(200, func() { check(r.runIn(ctx, nil, fn)) })
	again := testing.AllocsPerRun(200, func() { check(r.RunProgram(p)) })
	inRealm := testing.AllocsPerRun(200, func() { check(re.RunProgram(p)) })
	t.Logf("%s: allocations a run: %v anew, %v as a program, %v in a realm", src, anew, again, inRealm)
	if again > anew-fewer {
		t.Errorf("%s: a run as a program made %v allocations, %v anew: want %v fewer at least", src, again, anew, fewer)
	}
	if inRealm > anew-fewer {
		t.Errorf("%s: a run as a realm's program made %v allocations, %v anew: want %v fewer at least", src, inRealm, anew, fewer)
	}
}
