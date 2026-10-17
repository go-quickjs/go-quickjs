package vm

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// xorshift128 is V8's XorShift128 and ToDouble, written out apart from
// refillRandom: the numbers a state makes, in the order they are made.
func xorshift128(s0, s1 uint64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		x, y := s0, s1
		s0 = y
		x ^= x << 23
		x ^= x >> 17
		x ^= y
		x ^= y >> 26
		s1 = x
		out[i] = math.Float64frombits(s0>>12|0x3FF0000000000000) - 1
	}
	return out
}

// seedRandomForTest gives r's Math.random the state s0, s1, its cache
// empty.
func seedRandomForTest(r *Runtime, s0, s1 uint64) {
	m := r.mathRandom()
	m.s0, m.s1, m.count = s0, s1, 0
}

// Math.random takes its numbers as V8's does: xorshift128+ fills a cache of
// 64 at a time, taken from its end, each 52 of the generator's bits as a
// double in [1, 2), less one. So from a state, a run of calls gives each
// cache's numbers last made first.
func TestMathRandomSequence(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	const s0, s1 = 0x9e3779b97f4a7c15, 0xbf58476d1ce4e5b9
	seedRandomForTest(r, s0, s1)
	v, err := r.Run(compileForTest(t, `let out = []; for (let i = 0; i < 200; i++) out.push(Math.random()); out.join(',')`))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(v.String().Go(), ",")
	made := xorshift128(s0, s1, 256)
	for i, g := range got {
		block, k := i/mathRandomCache, i%mathRandomCache
		want := made[block*mathRandomCache+mathRandomCache-1-k]
		if parseFloatForTest(t, g) != want {
			t.Fatalf("call %d: %s, want %v", i, g, want)
		}
	}
}

// A call applies Math.random without a frame (collectionOp), as it does
// Math's other functions, from the interpreter, a tree and a pure body
// alike, and each call takes one number. A pure body that draws where it
// could still miss -- first's, at a getter read after the draw -- is run
// again in a frame when it misses, so it is not let draw there; one that
// draws where nothing can miss after (last's, scaled's) draws without a
// frame. Each call takes the next number, its body run pure or not.
func TestMathRandomFramelessOnce(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	const s0, s1 = 0x452821e638d01377, 0xbe5466cf34e90c6c
	seedRandomForTest(r, s0, s1)
	v, err := r.Run(compileForTest(t, `function first(o) { return Math.random() + o.k; }
		function last(o) { return o.k + Math.random(); }
		function scaled(n) { return Math.random() * n / n; }
		function framed(o) { var x = Math.random(); return o.k + x; }
		var plain = {k: 0}, getter = {get k() { return 0; }};
		let out = [];
		for (let i = 0; i < 300; i++) {
			const o = i % 3 ? plain : getter;
			out.push([first, last, scaled, framed][i % 4](i % 4 == 2 ? 4 : o));
		}
		out.join(',')`))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(v.String().Go(), ",")
	made := xorshift128(s0, s1, 320)
	for i, g := range got {
		block, k := i/mathRandomCache, i%mathRandomCache
		if want := made[block*mathRandomCache+mathRandomCache-1-k]; parseFloatForTest(t, g) != want {
			t.Fatalf("call %d: %s, want %v: a call took other than one number", i, g, want)
		}
	}
}

func parseFloatForTest(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// Math.random's numbers are in [0, 1), each a multiple of 2**-52; two
// runtimes draw different sequences, each seeded from the process's
// source; and building a runtime makes no state for it, which its first
// call does.
func TestMathRandomRange(t *testing.T) {
	r, other := New(Config{}), New(Config{})
	defer r.Close()
	defer other.Close()
	if r.rnd != nil {
		t.Fatal("a new runtime has Math.random's state before its first call")
	}
	src := `let out = []; for (let i = 0; i < 1000; i++) out.push(Math.random()); out.join(',')`
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	w, err := other.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if v.String().Go() == w.String().Go() {
		t.Fatal("two runtimes drew the same 1000 numbers")
	}
	sum := 0.0
	for _, s := range strings.Split(v.String().Go(), ",") {
		f := parseFloatForTest(t, s)
		if f < 0 || f >= 1 || f*(1<<52) != math.Trunc(f*(1<<52)) {
			t.Fatalf("Math.random() gave %v", f)
		}
		sum += f
	}
	if mean := sum / 1000; mean < 0.45 || mean > 0.55 {
		t.Fatalf("the mean of 1000 numbers is %v", mean)
	}
}
