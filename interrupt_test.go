package quickjs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestInterruptIsNotAValue pins that a deadline stops a script however the
// interrupt reaches it: a promise reaction used to turn it into a rejection
// that the next .catch took, and an iterator being closed dropped it, and the
// script ran on for good (KI-14). The runtime's next call is not stopped.
func TestInterruptIsNotAValue(t *testing.T) {
	for name, src := range map[string]string{
		"reaction": `var big = new Array(100000).fill(1);
			function spin() { while (true) big.slice(); }
			function again() { return Promise.resolve().then(spin).catch(again); }
			Promise.resolve().then(spin).catch(again);`,
		"iterator return": `var big = new Array(100000).fill(1);
			var it = {[Symbol.iterator]() { return this }, next() { return {done: false} },
				return() { while (true) big.slice(); }};
			Array.from({length: 2 ** 31}, function () { try { for (var x of it) throw 1; } catch (e) {} });`,
		"async catch": `(async () => { for (;;) { try { await null; while (true); } catch (e) {} } })()`,
	} {
		t.Run(name, func(t *testing.T) {
			rt := quickjs.New()
			defer rt.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := rt.EvalContext(ctx, src)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want the deadline", err)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("stopped after %v", d)
			}
			if v, err := rt.Eval(`1 + 1`); err != nil || v.Int() != 2 {
				t.Errorf("the next call: %v, %v", v, err)
			}
		})
	}
}
