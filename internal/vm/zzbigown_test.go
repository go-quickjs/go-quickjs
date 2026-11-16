package vm

import (
	"math/bits"
	"runtime"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// TestSoleLocals pins which locals soleLocals finds hold their values
// alone: those a function reads only as an operator's operands, and none
// whose value is copied to another local, pushed, stored to a global, read
// a property of, left on the stack across a jump -- s's too, which is
// beneath the conditional -- returned, captured, or in a function with an
// arguments object, a try statement, a postfix ++ whose value is read, or
// a generator's body; nor s, in pending, whose read is still on the stack,
// an operand, when it is assigned.
func TestSoleLocals(t *testing.T) {
	t.Parallel()
	if bits.UintSize != 64 {
		t.Skip("no BigInt is given back on a 32-bit machine")
	}
	top := compileForTest(t, `
		function alone(n) {
			var sum = 0n, a = 5n, q = 1n, r = 7n, t = 9n, c = 0n;
			for (var i = 0; i < n; i++) {
				sum = sum + a * a; a = a + 3n; q = 10n * q * (a % 97n + 1n);
				r = (r * a + q) % (t + 1n); t = t * 3n >> 1n;
				if (a > r) c++;
			}
			return n;
		}
		function copied(n) { var k = 1n, x; for (var i = 0; i < n; i++) { k = k * 3n; x = k; } return x }
		function pushed(n, xs) { var k = 1n; for (var i = 0; i < n; i++) { k = k * 3n; xs.push(k) } }
		function global(n) { var k = 1n; for (var i = 0; i < n; i++) { k = k * 3n; g = k } }
		function method(n) { var k = 1n; for (var i = 0; i < n; i++) { k = k * 3n; k.toString() } }
		function jump(n, c) { var k = 1n, j = 2n, s = 0n; for (var i = 0; i < n; i++) s = s + (c ? k : j) }
		function returned(n) { var k = 1n; for (var i = 0; i < n; i++) k = k * 3n; return k }
		function captured(n) { var k = 1n; for (var i = 0; i < n; i++) k = k * 3n; return () => k }
		function args(n) { var k = 1n; for (var i = 0; i < n; i++) k = k * 3n; return arguments.length }
		function tried(n) { var k = 1n; try { for (var i = 0; i < n; i++) k = k * 3n } catch (e) {} }
		function* gen(n) { var k = 1n; for (var i = 0; i < n; i++) k = k * 3n; yield 1 }
		function update(n) { var k = 1n, s = 0n; for (var i = 0; i < n; i++) s = s + k++ }
		function pending(n) { var s = 1n, m = 7n; for (var i = 0; i < n; i++) s = s * (s = s * 3n % m, 5n) % m }
		function appending(n) { var k = 1n, s = ""; for (var i = 0; i < n; i++) { k = k * 3n; s += k; g = s } }`)
	want := map[string]string{
		"alone": "sum a q r t c i", "copied": "i", "pushed": "i", "global": "i", "method": "i",
		"jump": "i", "returned": "i", "captured": "", "args": "", "tried": "", "gen": "", "update": "", "appending": "k i", "pending": "m i",
	}
	var walk func(fn *bytecode.Function)
	seen := 0
	walk = func(fn *bytecode.Function) {
		for _, c := range fn.Constants {
			if c.Kind != bytecode.ConstFunction || c.Fn == nil {
				continue
			}
			f := c.Fn
			if w, ok := want[f.Name]; ok {
				seen++
				m := soleLocals(f)
				var got []string
				for k := range f.Locals {
					if k < 64 && m&(1<<uint(k)) != 0 {
						got = append(got, f.Locals[k].Name)
					}
				}
				if g := strings.Join(got, " "); g != w {
					t.Errorf("%s: sole locals %q, want %q", f.Name, g, w)
				}
			}
			walk(f)
		}
	}
	walk(top)
	if seen != len(want) {
		t.Fatalf("found %d of %d functions", seen, len(want))
	}
}

// Register code gives back the BigInts the tree's operators give back
// (regBigRelease): a fresh operand's block and the value a local that
// holds its values alone held, so that `sum += a * a; a += incr` makes
// each result in a block given back before, natively and by the table,
// with the answers of code that gives nothing back.
func TestRegBigIntGivenBack(t *testing.T) {
	if bits.UintSize != 64 {
		t.Skip("no BigInt is given back on a 32-bit machine")
	}
	compiler.SetRegisterForm(true)
	SetRegTier(true)
	t.Cleanup(func() { compiler.SetRegisterForm(bytecode.RegDefault()); SetRegTier(bytecode.RegDefault()) })
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
	ref := newOldTiersForTest(Config{})
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
