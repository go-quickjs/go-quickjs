package quickjs_test

import (
	"errors"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestUncaughtExceptions pins OnUncaughtException: what a queueMicrotask
// callback or a FinalizationRegistry callback throws, which nothing in the
// script can catch, reaches the host as an *Error, in order, and the job
// queue goes on after it. Both were dropped: queueMicrotask's on purpose,
// a finalization callback's through a hook nothing set. Without a handler
// they are dropped still.
func TestUncaughtExceptions(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	var got []string
	rt.OnUncaughtException(func(err error) {
		var e *quickjs.Error
		if !errors.As(err, &e) {
			got = append(got, "not an *Error: "+err.Error())
			return
		}
		got = append(got, err.Error())
	})
	if _, err := rt.Eval(`
		var log = []
		queueMicrotask(() => { throw new TypeError("first") })
		queueMicrotask(() => log.push("ran after it"))
		queueMicrotask(() => { throw 42 })`); err != nil {
		t.Fatal(err)
	}
	if v, err := rt.Eval(`log.join()`); err != nil || v.String() != "ran after it" {
		t.Errorf("the queue after an uncaught exception: %v, %v", v, err)
	}
	if want := "TypeError: first|42"; strings.Join(got, "|") != want {
		t.Errorf("reported %q, want %q", strings.Join(got, "|"), want)
	}

	got = nil
	if _, err := rt.Eval(`
		var registry = new FinalizationRegistry(held => { throw new RangeError("cleanup " + held) });
		(function () { registry.register({}, "x") })()`); err != nil {
		t.Fatal(err)
	}
	collect(t, rt)
	if want := "RangeError: cleanup x"; strings.Join(got, "|") != want {
		t.Errorf("reported %q, want %q", strings.Join(got, "|"), want)
	}

	// Without a handler, what nothing can catch is dropped, as before.
	rt.OnUncaughtException(nil)
	got = nil
	if _, err := rt.Eval(`queueMicrotask(() => { throw new Error("dropped") }); 1`); err != nil {
		t.Errorf("an uncaught exception without a handler: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("reported %q with no handler", got)
	}
}
