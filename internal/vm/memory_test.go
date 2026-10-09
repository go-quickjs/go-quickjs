package vm

import (
	"runtime"
	"testing"
)

// TestMemoryMeterAccuracy pins that the memory meter's measure of what a
// script holds is close to what Go's heap holds for it, for each kind of
// value a script can keep: a measure that undercounts one kind lets a script
// holding that kind get past its limit by as much (KI-62: regular
// expressions, whose compiled programs were not counted at all, and dates).
// Each kind is built under a global, and the meter's walk and Go's live heap
// after a full collection are compared before and after.
func TestMemoryMeterAccuracy(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a hundred megabytes of values")
	}
	live := func() int64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapAlloc)
	}
	for _, c := range []struct{ name, build string }{
		{"objects", `for (let i = 0; i < 1e5; i++) keep.push({i, a: [1, 2, 3]})`},
		{"objects of 20 properties", `for (let i = 0; i < 1e4; i++) { let o = {}; for (let k = 0; k < 20; k++) o["p" + k] = k; keep.push(o) }`},
		{"class instances", `class P { constructor(x, y) { this.x = x; this.y = y } } for (let i = 0; i < 1e5; i++) keep.push(new P(i, i))`},
		{"an array of numbers", `keep.push(new Array(1e6).fill(1.5))`},
		{"short strings", `for (let i = 0; i < 1e5; i++) keep.push("s" + i)`},
		{"long strings", `for (let i = 0; i < 1e4; i++) keep.push(("x".repeat(1000) + i).toUpperCase())`},
		{"a non-ASCII string", `keep.push("é漢".repeat(1e6).toUpperCase())`},
		{"a Map", `let m = new Map(); for (let i = 0; i < 1e5; i++) m.set("k" + i, i); keep.push(m)`},
		{"a Set of objects", `let s = new Set(); for (let i = 0; i < 5e4; i++) s.add({i}); keep.push(s)`},
		{"closures", `for (let i = 0; i < 5e4; i++) { let v = i; keep.push(() => v) }`},
		{"a typed array", `keep.push(new Float64Array(1e6))`},
		{"BigInts", `for (let i = 0; i < 5e4; i++) keep.push(2n ** 200n + BigInt(i))`},
		{"dates", `for (let i = 0; i < 1e5; i++) keep.push(new Date(i))`},
		{"regular expressions", `for (let i = 0; i < 1e4; i++) keep.push(new RegExp("a" + i + "b+c"))`},
		{"one regular expression many times", `for (let i = 0; i < 1e5; i++) keep.push(/a(b+)c[\p{L}\d]/u)`},
	} {
		r := New(Config{MemoryLimit: 1 << 40})
		if _, err := r.Run(compileForTest(t, `var keep = []`)); err != nil {
			t.Fatal(err)
		}
		h0, e0 := live(), r.meter.walk(r)
		if _, err := r.Run(compileForTest(t, c.build)); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		h1, e1 := live(), r.meter.walk(r)
		held, counted := h1-h0, e1-e0
		if ratio := float64(counted) / float64(held); ratio < 0.85 || ratio > 1.15 {
			t.Errorf("%s: %.2f MB held, %.2f MB counted (%.2f)", c.name, float64(held)/1e6, float64(counted)/1e6, ratio)
		}
		r.Close()
	}
}
