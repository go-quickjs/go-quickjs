package quickjs_test

import (
	"errors"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestClosedRuntimeKeepsNoStack pins that a function kept from a runtime that
// has been closed -- whose stack another runtime may now be using -- fails
// rather than running on that stack, and leaves the other runtime as it was.
func TestClosedRuntimeKeepsNoStack(t *testing.T) {
	old := quickjs.New()
	fn, err := old.Eval(`(function f(n) { return n ? f(n - 1) + 1 : 0 })`)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()

	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`var state = [1, 2, 3]; function sum() { return state.reduce((a, b) => a + b) }`); err != nil {
		t.Fatal(err)
	}
	// A closed runtime's function does not run, as nothing of a closed
	// runtime does.
	if _, err = fn.Call(10); !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("a closed runtime's function = %v, want ErrClosed", err)
	}
	if v, err := rt.Eval(`sum()`); err != nil || v.Int() != 6 {
		t.Errorf("the new runtime's sum() = %v, %v", v, err)
	}

	// A runtime closed from inside its own script, by a Go function the
	// script called, runs the script to its end and then reports that it is
	// closed: it was an internal error, from the engine it had let go of.
	inner := quickjs.New()
	inner.Set("closeNow", func(r *quickjs.Runtime) { r.Close() })
	if _, err := inner.Eval(`function g(x) { closeNow(); return x * 2 } g(21)`); !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("closing from inside = %v, want ErrClosed", err)
	}
	if _, err := inner.Eval(`1`); !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("after closing from inside = %v, want ErrClosed", err)
	}
}
