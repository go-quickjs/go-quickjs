package quickjs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestLongBigIntOperationsAreInterruptible pins that a deadline stops a
// BigInt operation of millions of bits, each of which had run in one call
// for seconds or minutes past it, as V8 stops its BigInt library's: a
// product, a quotient, a remainder, a power, a conversion to a string and
// back, and a product native code makes, which it leaves to Go's call. x is
// made of shifts and a division by a word, which take no time.
func TestLongBigIntOperationsAreInterruptible(t *testing.T) {
	for name, src := range map[string]string{
		"toString":       `String(x).length`,
		"toString radix": `x.toString(7).length`,
		"template":       "`${x}`.length",
		"product":        `(x * (x + 1n)) & 1n`,
		"square":         `(x * x) & 1n`,
		"quotient":       `((1n << 40000000n) / (x + 7n)) & 1n`,
		"remainder":      `((1n << 40000000n) % (x + 7n)) & 1n`,
		"power":          `(3n ** 40000000n) & 1n`,
		"parse":          `BigInt(s) & 1n`,
		"compare":        `x < s`,
		"equal":          `x == s`,
		"native product": `function f(a, b) { return a * b }
			for (let i = 0; i < 2e4; i++) f(BigInt(i) << 100n, 3n << 100n);
			f(x, x + 1n) & 1n`,
	} {
		for _, jit := range []bool{false, true} {
			opts := []quickjs.Option{}
			label := name
			if jit {
				opts = append(opts, quickjs.WithJIT())
				label += "/jit"
			}
			t.Run(label, func(t *testing.T) {
				rt := quickjs.New(opts...)
				defer rt.Close()
				if _, err := rt.Eval(`var x = (1n << 20000000n) / 3n, s = "7".repeat(6e6); 0`); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				start := time.Now()
				if v, err := rt.EvalContext(ctx, src); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("= %v, %v, want the deadline", v, err)
				}
				if d := time.Since(start); d > 2*time.Second {
					t.Errorf("stopped after %v", d)
				}
				// The runtime's next call runs.
				if v, err := rt.Eval(`String(x % 1000n)`); err != nil || v.String() != "125" {
					t.Errorf("after: %v, %v", v, err)
				}
			})
		}
	}
}

// TestLongBigIntOperationsUnderAMemoryLimit pins that a long BigInt
// operation of a runtime with a memory limit, and no deadline, runs to its
// end: the check it makes at intervals finds the limit not reached.
func TestLongBigIntOperationsUnderAMemoryLimit(t *testing.T) {
	rt := quickjs.New(quickjs.WithMemoryLimit(256 << 20))
	defer rt.Close()
	v, err := rt.Eval(`const x = (1n << 100000n) / 3n;
		const s = String(x * x);
		[s.length, BigInt(s) === x * x, (x * x) / x === x, (x * x + 5n) % x].join()`)
	if err != nil || v.String() != "60206,true,true,5" {
		t.Errorf("= %v, %v", v, err)
	}
}
