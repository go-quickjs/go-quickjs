package quickjs

import (
	"context"
	"testing"
)

// A program run again finds its literals' layouts: its runs make none of
// them, which is six shapes a run of this one, each an allocation, and keys
// beside them, in a runtime's realm or any other. The same bytecode run
// otherwise makes them each time.
func TestProgramRerunsMakeNoLayouts(t *testing.T) {
	r := New()
	defer r.Close()
	re, err := r.NewRealm()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile("rerun.js", `var o = {a: 1, b: 2}, q = {c: 3, d: 4}; o.a + q.d`)
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
	t.Logf("allocations a run: %v anew, %v as a program, %v in a realm", anew, again, inRealm)
	if again > anew-6 {
		t.Errorf("a run as a program made %v allocations, %v anew: want 6 fewer at least", again, anew)
	}
	if inRealm > anew-6 {
		t.Errorf("a run as a realm's program made %v allocations, %v anew: want 6 fewer at least", inRealm, anew)
	}
}
