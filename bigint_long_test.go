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
				d := time.Since(start)
				if d > 2*time.Second {
					t.Errorf("stopped after %v", d)
				}
				t.Logf("stopped after %v", d)
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

// TestLongBigIntConversions pins the conversions of BigInts of hundreds of
// thousands of bits, of either sign, in bases 10, 16, 7, 2 and 36, and back
// from decimal, hexadecimal, octal and binary literals' strings: Node's
// digits.
func TestLongBigIntConversions(t *testing.T) {
	checkEval(t, `const x = 3n ** 300000n + 12345n;
const r = [];
for (const v of [x, -x, x * x, -(x << 1000n)]) {
  for (const b of [10, 16, 7, 2, 36]) {
    const s = v.toString(b);
    r.push(b + ':' + s.length + ':' + s.slice(0, 12) + ':' + s.slice(-12));
  }
}
const s = String(-x * x);
r.push(s.length, s.slice(0, 15), s.slice(-15), BigInt(s) === -x * x, BigInt("0x" + (x*x).toString(16)) === x * x, BigInt("0o" + x.toString(8)) === x, BigInt("0b" + x.toString(2)) === x);
r.join(",")`,
		"10:143137:237911753945:596566012346,16:118873:1ae9a8126486:1953551941ba,7:169373:246146655223:611665551405,2:475489:110101110100:000110111010,36:91973:1brkl51hml1m:wdw1wxwrvwl6,10:143138:-23791175394:596566012346,16:118874:-1ae9a812648:1953551941ba,7:169374:-24614665522:611665551405,2:475490:-11010111010:000110111010,36:91974:-1brkl51hml1:wdw1wxwrvwl6,10:286273:566020026653:407824423716,16:237745:2d44b671c3e6:babf6b33fb24,7:338746:101662456614:336552451534,2:950978:101101010001:101100100100,36:183945:1rdkvena9yuw:5gg11scyb410,10:143439:-25492449210:540800516096,16:119124:-1ae9a812648:000000000000,7:169730:-40162454536:006036624463,2:476490:-11010111010:000000000000,36:92167:-645mdwz43fa:qnk854xtju2o,286274,-56602002665327,098407824423716,true,true,true,true")
}
