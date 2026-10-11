package vm

import "testing"

// TestFastAddAndShortBigInts pins what add's two-strings path and the tree
// tier's shortcut to shortArith answer, on both tiers: two strings joined,
// a string beside a number converted as before, BigInts held in their
// Values on every operator, a result past the int64 range, a division by
// zero, a negative power, and a BigInt mixed with a number.
func TestFastAddAndShortBigInts(t *testing.T) {
	defer SetTreeTier(true)
	const src = `function f(a, b) { var r = []; r.push(a + b, a - b, a * b);
	try { r.push(a / b) } catch (e) { r.push(e.name) }
	try { r.push(a % b) } catch (e) { r.push(e.name) }
	try { r.push(a ** b) } catch (e) { r.push(e.name) }
	return r.join() }
function g(a, b) { return a + b }
[f(7n, 2n), f(-7n, 2n), f(7n, 0n), f(2n, -1n), f(3037000500n, 3037000500n), f(-9223372036854775808n, -1n),
 g('ab', 'cd'), g('', ''), g('x', 1), g(1, 'x'), g('n', null)].join(';') +
 (function () { try { return f(1n, 1) } catch (e) { return ';' + e.name } })()`
	const want = "9,5,14,3,1,49;-5,-9,-14,-3,-1,49;7,7,0,RangeError,RangeError,1;" +
		"1,3,-2,-2,0,RangeError;6074001000,0,9223372037000250000,1,0,RangeError;" +
		"-9223372036854775809,-9223372036854775807,9223372036854775808,9223372036854775808,0,RangeError;" +
		"abcd;;x1;1x;nnull;TypeError"
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		fn := compileForTest(t, src)
		r := New(Config{})
		v, err := r.Run(fn)
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", on, err)
		}
		got, err := r.ToString(v)
		if err != nil || got.Go() != want {
			t.Errorf("tree %v: got %v, %v; want %s", on, got, err, want)
		}
		r.Close()
		if on {
			assertTreeIn(t, fn, "f")
		}
	}
}
