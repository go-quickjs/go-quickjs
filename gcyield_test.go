package quickjs

import (
	"runtime"
	"testing"
)

// WithGCYield reaches the runtime, is off without it, and changes no
// answer: with one P, a runtime made with it waits while the collector
// marks, one made without never does, and both compute the same.
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
		v, err := r.Eval(src)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); got != "300000:4688" {
			t.Fatalf("WithGCYield %v: %q, want %q", on, got, "300000:4688")
		}
		pauses, _, _ := r.rt.GCYieldStats()
		if on != (pauses > 0) {
			t.Fatalf("WithGCYield %v: %d waits", on, pauses)
		}
		r.Close()
	}
}
