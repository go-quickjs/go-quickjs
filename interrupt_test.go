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

// TestNestedEvalKeepsTheOuterCall pins that a Go function evaluating more
// script, from inside a running one, leaves the running one as it was: it
// used to clear the outer deadline when it returned, so a script that
// looped after calling it ran for good, and to drain the outer script's jobs
// in the middle of it (KI-15).
func TestNestedEvalKeepsTheOuterCall(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if err := rt.Set("helper", func(r *quickjs.Runtime) (int, error) {
		v, err := r.Eval(`log.push("helper"); 21 * 2`)
		return v.Int(), err
	}); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval(`var log = []; Promise.resolve().then(() => log.push("job"));
		log.push("before"); const n = helper(); log.push("after " + n); log.join()`)
	if err != nil || v.String() != "before,helper,after 42" {
		t.Errorf("= %v, %v", v, err)
	}
	if v, _ := rt.Eval(`log.join()`); v.String() != "before,helper,after 42,job" {
		t.Errorf("after the turn: %v", v)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := rt.EvalContext(ctx, `helper(); for (;;) {}`); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the outer deadline", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("stopped after %v", d)
	}
}

// TestNativeLoopsAreInterruptible pins that a deadline stops the engine's
// own loops that run no script between their steps: fill over an array-like
// of four billion, JSON.stringify of a long dense array, and Intl's reading
// of a locale list of length 2^53 - 1 (KI-16).
func TestNativeLoopsAreInterruptible(t *testing.T) {
	for name, src := range map[string]string{
		"fill":        `Array.prototype.fill.call({length: 2 ** 32 + 3}, 7)`,
		"stringify":   `JSON.stringify(new Array(3e7).fill(1)).length`,
		"locale list": `Intl.getCanonicalLocales({length: 2 ** 53 - 1})`,
	} {
		t.Run(name, func(t *testing.T) {
			rt := quickjs.New()
			defer rt.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			if _, err := rt.EvalContext(ctx, src); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want the deadline", err)
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("stopped after %v", d)
			}
		})
	}
}
