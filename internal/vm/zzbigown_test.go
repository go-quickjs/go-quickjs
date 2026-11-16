package vm

import (
	"math/bits"
	"runtime"
	"testing"
)

// Register code gives back BigInts its operators no longer need
// (regBigRelease): a fresh operand's block and the value a local that
// holds its values alone held, so that `sum += a * a; a += incr` makes
// each result in a block given back before, natively and by the table,
// with the answers of code that gives nothing back.
func TestRegBigIntGivenBack(t *testing.T) {
	if bits.UintSize != 64 {
		t.Skip("no BigInt is given back on a 32-bit machine")
	}
	const src = `
		function arith(n, bits) {
			var sum = 0n, a = 1n << BigInt(bits), incr = 1n;
			for (var i = 0; i < n; i++) { sum += a * a; a += incr; }
			// An operand, as the micro-benchmark's global_res += sum: a
			// return would keep sum's value, which it then does not hold
			// alone.
			return sum + 0n;
		}
		function mixed(n) {
			var keep = [], s = (1n << 300n) + 5n, t = 7n ** 90n, u;
			for (var i = 0; i < n; i++) {
				u = s * t;
				keep.push((s * t) - u, u + (s * t) * 3n);
				s = s + (i & 1 ? t * t : s * 3n) - u;
				t = t * 3n - (t - 1n);
			}
			return keep.join(",") + "/" + s + "/" + t;
		}`
	ref := New(Config{})
	if _, err := ref.Run(compileForTest(t, src)); err != nil {
		t.Fatal(err)
	}
	v, err := ref.Run(compileForTest(t, `String(arith(50, 256)) + "|" + mixed(20)`))
	ref.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := v.String().Go()
	for _, native := range []bool{false, true} {
		was := SetRegNative(native)
		r := New(Config{})
		if _, err := r.Run(compileForTest(t, src)); err != nil {
			t.Fatal(err)
		}
		v, err := r.Run(compileForTest(t, `String(arith(50, 256)) + "|" + mixed(20)`))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("native %v: %s, want %s", native, got, want)
		}
		var a, b runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&a)
		if _, err := r.Run(compileForTest(t, `arith(10000, 256)`)); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&b)
		if n := b.Mallocs - a.Mallocs; n > 2000 && allocsOrdinary() {
			t.Errorf("native %v: 10,000 turns of sum += a * a; a += incr made %d allocations", native, n)
		}
		r.Close()
		SetRegNative(was)
	}
}
