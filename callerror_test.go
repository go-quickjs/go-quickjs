package quickjs_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestCallReturnsError pins that an exception reaching Go through a Value --
// a call, a getter, a constructor -- is an *Error, as one from Eval is, and
// that a Go function passing it on rethrows the very exception.
func TestCallReturnsError(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	obj, err := rt.Eval(`({
		f() { throw new RangeError("from f") },
		get g() { throw new TypeError("from g") },
		C: class { constructor() { throw new SyntaxError("from C") } },
	})`)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := obj.Get("f")
	c, _ := obj.Get("C")
	for name, try := range map[string]func() error{
		"RangeError":  func() error { _, err := f.Call(); return err },
		"TypeError":   func() error { _, err := obj.Get("g"); return err },
		"SyntaxError": func() error { _, err := c.New(); return err },
	} {
		var jsErr *quickjs.Error
		if err := try(); !errors.As(err, &jsErr) {
			t.Errorf("%s: %T %v, want an *Error", name, err, err)
		} else if n, _ := jsErr.Value().Get("name"); n.String() != name {
			t.Errorf("%s: thrown %s", name, jsErr.Value())
		}
	}

	if err := rt.Set("pass", func() error { _, err := f.Call(); return err }); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval(`try { pass() } catch (e) { [e instanceof RangeError, e.message, e.stack.includes("at f")].join() }`)
	if err != nil || v.String() != "true,from f,true" {
		t.Errorf("rethrown = %v, %v", v, err)
	}
}

// TestHostCancellationIsUncatchable pins that a Go function reporting that
// the runtime's own context has ended stops the script, as the context itself
// would, rather than throwing something it can catch -- and that any other
// deadline a Go function reports, one of its own requests', is an error the
// script can catch, as it was before that (KI-19).
func TestHostCancellationIsUncatchable(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	ctx, cancel := context.WithCancel(context.Background())
	if err := rt.Set("stop", func() error { cancel(); return ctx.Err() }); err != nil {
		t.Fatal(err)
	}
	_, err := rt.EvalContext(ctx, `var after = false; try { stop() } catch (e) { after = "caught" } after = true`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if v, _ := rt.Eval("after"); v.Bool() {
		t.Errorf("after = %v: the script ran on", v)
	}

	if err := rt.Set("fetchSlowly", func() error {
		return fmt.Errorf("Get \"http://example.com\": %w", context.DeadlineExceeded)
	}); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval(`try { fetchSlowly(); "no error" } catch (e) { e instanceof Error ? "caught: " + e.message : "?" }`)
	if err != nil || !strings.HasPrefix(v.String(), "caught: ") {
		t.Errorf("= %v, %v", v, err)
	}
}

// TestErrorStaysInItsRuntime pins that an exception one runtime threw, which
// a Go function returns to another, reaches the other as an Error with its
// message and nothing of the first runtime's: it used to be rethrown as it
// was, handing the second runtime the first's objects (KI-09).
func TestErrorStaysInItsRuntime(t *testing.T) {
	a, b := quickjs.New(), quickjs.New()
	defer a.Close()
	defer b.Close()
	fail, err := a.Eval(`(function () { const e = new RangeError("from A"); e.secret = {x: 1}; throw e })`)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Set("callA", func() error { _, err := fail.Call(); return err }); err != nil {
		t.Fatal(err)
	}
	v, err := b.Eval(`try { callA() } catch (e) {
		[e instanceof Error, Object.getPrototypeOf(e) === Error.prototype, e.message, "secret" in e].join()
	}`)
	if err != nil || v.String() != "true,true,RangeError: from A,false" {
		t.Errorf("= %v, %v", v, err)
	}
}

// errWrapped is a Go error of a type of its own, for errors.As.
type errWrapped struct{ code int }

func (e *errWrapped) Error() string { return fmt.Sprintf("code %d", e.code) }

// TestErrorUnwrapsToTheGoError pins that an exception a Go function's error
// was thrown as unwraps to that error once it reaches Go: uncaught, or caught
// and rethrown as the same object, and from another runtime's Go function as
// well. An exception the script made itself, or a new one it threw in place
// of the Go function's, unwraps to nothing.
func TestErrorUnwrapsToTheGoError(t *testing.T) {
	errMine := errors.New("mine")
	rt := quickjs.New()
	defer rt.Close()
	other := quickjs.New()
	defer other.Close()
	if err := other.Set("fail", func() error { return fmt.Errorf("other: %w", errMine) }); err != nil {
		t.Fatal(err)
	}
	fromOther, err := other.Eval(`(function () { fail() })`)
	if err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]any{
		"fail":      func() error { return fmt.Errorf("wrapped: %w", errMine) },
		"failCode":  func() error { return &errWrapped{code: 7} },
		"failOther": func() error { _, err := fromOther.Call(); return err },
	} {
		if err := rt.Set(name, fn); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		src     string
		isMine  bool
		code    int
		message string
	}{
		{`fail()`, true, 0, "Error: wrapped: mine"},
		{`try { fail() } catch (e) { e.message += "!"; throw e }`, true, 0, "Error: wrapped: mine!"},
		{`(function f() { return [1].map(() => fail()) })()`, true, 0, "Error: wrapped: mine"},
		{`failCode()`, false, 7, "Error: code 7"},
		{`failOther()`, true, 0, "Error: Error: other: mine"},
		{`try { fail() } catch (e) { throw new Error(e.message) }`, false, 0, "Error: wrapped: mine"},
		{`throw new TypeError("own")`, false, 0, "TypeError: own"},
	} {
		_, err := rt.Eval(c.src)
		var jsErr *quickjs.Error
		if !errors.As(err, &jsErr) {
			t.Errorf("%s: %T %v, want an *Error", c.src, err, err)
			continue
		}
		if err.Error() != c.message {
			t.Errorf("%s: message %q, want %q", c.src, err.Error(), c.message)
		}
		if errors.Is(err, errMine) != c.isMine {
			t.Errorf("%s: errors.Is(err, errMine) = %v", c.src, !c.isMine)
		}
		var coded *errWrapped
		if got := errors.As(err, &coded); got != (c.code != 0) || got && coded.code != c.code {
			t.Errorf("%s: errors.As found %v", c.src, coded)
		}
		if c.code == 0 && !c.isMine && jsErr.Unwrap() != nil {
			t.Errorf("%s: unwraps to %v, want nil", c.src, jsErr.Unwrap())
		}
	}

	// A deadline a Go function reports of its own stays an exception, which
	// the script may catch, and unwraps to the deadline when it does not.
	if err := rt.Set("late", func() error { return context.DeadlineExceeded }); err != nil {
		t.Fatal(err)
	}
	if v, err := rt.Eval(`try { late(); "no" } catch (e) { e.message }`); err != nil || v.String() != context.DeadlineExceeded.Error() {
		t.Errorf("caught = %v, %v", v, err)
	}
	_, err = rt.Eval(`late()`)
	var jsErr *quickjs.Error
	if !errors.As(err, &jsErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("uncaught = %T %v", err, err)
	}
}

// TestCallContext pins that CallContext stops a function called from Go when
// its context ends, as EvalContext stops a script, with an error that wraps
// the context's; that the runtime is usable after; and that a call made from
// inside a script is bounded by the script's context as well as its own.
func TestCallContext(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	spin, err := rt.Eval(`(function spin(n) { for (;;) {} })`)
	if err != nil {
		t.Fatal(err)
	}
	add, err := rt.Eval(`(function (a, b) { return this.base + a + b })`)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = spin.CallContext(ctx, 1)
	var jsErr *quickjs.Error
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &jsErr) ||
		!strings.HasPrefix(err.Error(), "quickjs: execution interrupted") {
		t.Fatalf("spin = %T %v, want an interruption", err, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("stopping took %v", d)
	}

	// The runtime runs on, and a call with a live context answers.
	this, _ := rt.Eval(`({base: 10})`)
	if v, err := add.CallWithThisContext(context.Background(), this, 1, 2); err != nil || v.Int() != 13 {
		t.Errorf("add = %v, %v", v, err)
	}
	if v, err := rt.Eval(`1 + 1`); err != nil || v.Int() != 2 {
		t.Errorf("eval after = %v, %v", v, err)
	}
	// An exception is an *Error, as Call gives it.
	boom, _ := rt.Eval(`(function () { throw new RangeError("boom") })`)
	if _, err := boom.CallContext(context.Background()); !errors.As(err, &jsErr) {
		t.Errorf("boom = %T %v", err, err)
	}

	// A Go function the script calls spins with no deadline of its own, and
	// the script's stops it.
	if err := rt.Set("callSpin", func() error { _, err := spin.CallContext(context.Background()); return err }); err != nil {
		t.Fatal(err)
	}
	outer, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := rt.EvalContext(outer, `callSpin()`); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("nested = %T %v, want the script's deadline", err, err)
	}

	rt.Close()
	if _, err := add.CallContext(context.Background(), 1, 2); !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("after Close = %v, want ErrClosed", err)
	}
}
