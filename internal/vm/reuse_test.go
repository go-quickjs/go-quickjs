package vm

import (
	"runtime"
	"testing"
)

// A loop's function expression that the compiler makes once per invocation
// (internal/compiler's reusedClosures) sees the enclosing function's vars
// as they are at each call, as the one each iteration made would: each
// iteration's i, a nested function's call of it, its own recursion, each
// invocation's own, one invocation inside another, and a generator's across
// its yields. A loop of 10,000 iterations makes one function rather than
// 10,000.
func TestReusedClosureAnswers(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function sum(n) {
			'use strict'
			var t = 0
			for (var i = 0; i < n; i++) {
				var get = function get(k) { return k > 0 ? get(k - 1) : i }
				t += get(2) + (function () { return get(0) })()
			}
			return t
		}
		function nest(n) {
			'use strict'
			var out = []
			for (var i = 0; i < n; i++) {
				var g = function () { return i + (n > 1 ? nest(n - 1).length : 0) }
				out.push(g())
			}
			return out
		}
		function* gen(n) {
			'use strict'
			for (var i = 0; i < n; i++) {
				var g = function () { return i * 10 }
				yield g()
				yield g() + 1
			}
		}
		[sum(10), sum(3), nest(3).join(','), [...gen(3)].join(',')].join(' ')`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "90 6 2,3,4 0,1,10,11,20,21"; got != want {
		t.Fatalf("= %q, want %q", got, want)
	}
	if _, err := r.Run(compileForTest(t, `
		function loop(n) { 'use strict'; var t = 0; for (var i = 0; i < n; i++) { var g = function () { return i }; t += g() } return t }
		loop(100)`)); err != nil {
		t.Fatal(err)
	}
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	v, err = r.Run(compileForTest(t, `loop(10000)`))
	runtime.ReadMemStats(&b)
	if err != nil || v.Number() != 49995000 {
		t.Fatalf("= %v, %v", v, err)
	}
	if n := b.Mallocs - a.Mallocs; n > 1000 {
		t.Errorf("10,000 iterations made %d allocations", n)
	}
}
