package vm

import (
	"math/bits"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
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
		function pending(n) { var s = 1n, m = 7n; for (var i = 0; i < n; i++) s = s * (s = s * 3n % m, 5n) % m }`)
	want := map[string]string{
		"alone": "sum a q r t c i", "copied": "i", "pushed": "i", "global": "i", "method": "i",
		"jump": "i", "returned": "i", "captured": "", "args": "", "tried": "", "gen": "", "update": "", "pending": "m i",
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
