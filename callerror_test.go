package quickjs_test

import (
	"context"
	"errors"
	"testing"

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

// TestHostCancellationIsUncatchable pins that a Go function reporting a
// cancelled context stops the script, as the context itself would, rather
// than throwing something it can catch.
func TestHostCancellationIsUncatchable(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if err := rt.Set("stop", func() error { return context.Canceled }); err != nil {
		t.Fatal(err)
	}
	_, err := rt.Eval(`var after = false; try { stop() } catch (e) { after = "caught" } after = true`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
	if v, _ := rt.Eval("after"); v.Bool() {
		t.Errorf("after = %v: the script ran on", v)
	}
}
