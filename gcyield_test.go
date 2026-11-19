package quickjs

import (
	"runtime"
	"testing"
)

// WithGCYield reaches the runtime, is off without it, and changes no
// answer: at one P, with the collector marking for the first three
// questions (vm's SetGCMarkingForTest), a runtime made with it waits once,
// and one made without it never asks and never waits; both compute the
// same.
func TestWithGCYield(t *testing.T) {
	const src = `(() => {
		let keep = [], sum = 0;
		for (let i = 0; i < 300000; i++) {
			const o = {i, s: "x" + i, a: [i, i + 1]};
			if (i % 64 == 0) keep.push(o);
			sum += o.a[1] - o.i;
		}
		return sum + ":" + keep.length;
	})()`
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for _, on := range []bool{false, true} {
		var opts []Option
		if on {
			opts = append(opts, WithGCYield())
		}
		r := New(opts...)
		asked := 0
		r.rt.SetGCMarkingForTest(func() bool {
			asked++
			return asked <= 3
		})
		v, err := r.Eval(src)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); got != "300000:4688" {
			t.Fatalf("WithGCYield %v: %q, want %q", on, got, "300000:4688")
		}
		pauses, _, _ := r.rt.GCYieldStats()
		if on && (pauses != 1 || asked <= 4) || !on && (pauses != 0 || asked != 0) {
			t.Fatalf("WithGCYield %v: %d waits, %d questions", on, pauses, asked)
		}
		r.Close()
	}
}
