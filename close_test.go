package quickjs_test

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs"
)

// A script that closes its runtime ends at the call that closed it, as
// process.exit ends a node worker: nothing after it runs -- not the next
// statement, not a catch, not a finally.
func TestCloseFromInsideRunsNothingMore(t *testing.T) {
	t.Parallel()
	rt := quickjs.New()
	mem := rt.NewBytes(make([]byte, 4))
	b, _ := mem.Bytes()
	rt.Set("mem", mem)
	rt.Set("closeNow", func(r *quickjs.Runtime) { r.Close() })
	_, err := rt.Eval(`
		try { mem[0] = 1; closeNow(); mem[1] = 1 }
		catch (e) { mem[2] = 1 }
		finally { mem[3] = 1 }
	`)
	if !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("= %v, want ErrClosed", err)
	}
	if got := string([]byte{'0' + b[0], '0' + b[1], '0' + b[2], '0' + b[3]}); got != "1000" {
		t.Errorf("before, after, catch, finally = %s, want 1000", got)
	}
}

// Close runs the hooks OnClose registered before it returns, newest first,
// after the runtime's context has ended and pending work has been told --
// from inside the script as from outside -- and a removed hook not at all.
func TestOnClose(t *testing.T) {
	t.Parallel()
	for _, inside := range []bool{false, true} {
		rt := quickjs.New()
		var order []string
		var workErr error
		rt.StartAsyncWork().Post(func(err error) { workErr = err })
		for _, name := range []string{"a", "b", "c"} {
			remove := rt.OnClose(func() {
				if rt.Context().Err() == nil {
					t.Errorf("hook %s ran before the context ended", name)
				}
				if workErr == nil {
					t.Errorf("hook %s ran before pending work was told", name)
				}
				order = append(order, name)
			})
			if name == "b" {
				remove()
			}
		}
		if inside {
			rt.Set("exit", func(r *quickjs.Runtime) { r.Close() })
			if _, err := rt.Eval(`exit()`); !errors.Is(err, quickjs.ErrClosed) {
				t.Errorf("exit() = %v, want ErrClosed", err)
			}
		} else {
			rt.Close()
		}
		if got := strings.Join(order, ""); got != "ca" {
			t.Errorf("inside=%v: hooks ran %q, want %q", inside, got, "ca")
		}
		ran := false
		rt.OnClose(func() { ran = true })
		if !ran {
			t.Errorf("inside=%v: a hook added after Close did not run at once", inside)
		}
	}
}

// Closing a runtime from inside the script it runs -- a Go function the
// script called closing it, as process.exit does -- stops that script: the
// call that ran it returns ErrClosed, nothing after the Close runs, and no
// catch sees it. It used to leave the script running, for ever if it looped.
func TestCloseFromInsideStopsTheScript(t *testing.T) {
	const tail = `closeNow(); note("ran on"); for (;;) {}`
	cases := []struct {
		name string
		run  func(rt *quickjs.Runtime) error
	}{
		{"Eval", func(rt *quickjs.Runtime) error {
			_, err := rt.Eval(tail)
			return err
		}},
		{"Eval, a loop with no call in it", func(rt *quickjs.Runtime) error {
			_, err := rt.Eval(`closeNow(); let i = 0; for (;;) { i++ }`)
			return err
		}},
		{"Eval, inside try", func(rt *quickjs.Runtime) error {
			_, err := rt.Eval(`try { ` + tail + ` } catch (e) { note("caught") }`)
			return err
		}},
		{"EvalModule", func(rt *quickjs.Runtime) error {
			_, err := rt.EvalModule("main.js", tail)
			return err
		}},
		{"Value.Call", func(rt *quickjs.Runtime) error {
			fn, err := rt.Eval(`(function () { ` + tail + ` })`)
			if err != nil {
				return err
			}
			_, err = fn.Call()
			return err
		}},
		{"Value.New", func(rt *quickjs.Runtime) error {
			fn, err := rt.Eval(`(function () { ` + tail + ` })`)
			if err != nil {
				return err
			}
			_, err = fn.New()
			return err
		}},
		{"a job Eval drains", func(rt *quickjs.Runtime) error {
			_, err := rt.Eval(`Promise.resolve().then(() => { ` + tail + ` })`)
			return err
		}},
		{"a job RunJobs drains", func(rt *quickjs.Runtime) error {
			p := rt.NewPromise()
			rt.Set("later", func() quickjs.Value { return p.Value() })
			if _, err := rt.Eval(`later().then(() => { ` + tail + ` })`); err != nil {
				return err
			}
			if err := p.Resolve(1); err != nil {
				return err
			}
			return rt.RunJobs()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := quickjs.New()
			var noted []string
			rt.Set("closeNow", func(r *quickjs.Runtime) { r.Close() })
			rt.Set("note", func(s string) { noted = append(noted, s) })

			done := make(chan error, 1)
			go func() { done <- tc.run(rt) }()
			select {
			case err := <-done:
				if !errors.Is(err, quickjs.ErrClosed) {
					t.Errorf("= %v, want ErrClosed", err)
				}
				if len(noted) != 0 {
					t.Errorf("ran after Close: %q", noted)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("still running 10s after Close; ran after it: %q", noted)
			}
			if _, err := rt.Eval(`1`); !errors.Is(err, quickjs.ErrClosed) {
				t.Errorf("after Close, Eval = %v, want ErrClosed", err)
			}
		})
	}
}

// TestClosedRuntimesValues pins that a closed runtime's values and promises
// answer as the runtime does, with ErrClosed from each method that returns
// an error, and do nothing to its heap (KI-94). Only Call asked whether the
// runtime was closed: Get, Set, Index, Equal and Decode worked on the closed
// heap, Set writing, Len and Keys read it, New threw a RangeError, and a
// Promise settled after Close dereferenced the runtime Close let go of.
func TestClosedRuntimesValues(t *testing.T) {
	t.Parallel()
	rt := quickjs.New()
	var ran []string
	rt.Set("note", func(s string) { ran = append(ran, s) })
	obj, err := rt.Eval(`({ a: 1,
		toString() { note("toString"); return "s" },
		valueOf() { note("valueOf"); return 7 } })`)
	if err != nil {
		t.Fatal(err)
	}
	arr, _ := rt.Eval(`[1, 2, 3]`)
	ctor, _ := rt.Eval(`(function F() { this.x = 1 })`)
	buf, _ := rt.Eval(`new Uint8Array(4)`)
	str, _ := rt.Eval(`"str"`)
	num, _ := rt.Eval(`42`)
	settled, _ := rt.Eval(`Promise.resolve(5)`)
	p, q, r := rt.NewPromise(), rt.NewPromise(), rt.NewPromise()
	_, thrown := rt.EvalFile("main.js", `function thrower() { throw new TypeError("bad") }; thrower()`)
	rt.Close()

	errs := map[string]error{}
	_, errs["Get"] = obj.Get("a")
	errs["Set"] = obj.Set("b", 2)
	_, errs["Index"] = arr.Index(1)
	_, errs["Equal"] = obj.Equal(num)
	_, errs["Equal of another's"] = num.Equal(obj)
	var decoded any
	errs["Decode"] = obj.Decode(&decoded)
	errs["Decode of a primitive"] = num.Decode(&decoded)
	_, errs["Call"] = ctor.Call()
	_, errs["New"] = ctor.New()
	_, errs["Await"] = settled.Await(nil)
	errs["Promise.Resolve"] = p.Resolve("a string")
	errs["Promise.Reject"] = q.Reject(map[string]any{"a": 1})
	for name, err := range errs {
		if !errors.Is(err, quickjs.ErrClosed) {
			t.Errorf("%s = %v, want ErrClosed", name, err)
		}
	}
	r.RejectError(errors.New("after Close")) // nothing, and no panic
	if decoded != nil {
		t.Errorf("Decode wrote %v", decoded)
	}
	if n := arr.Len(); n != 0 {
		t.Errorf("Len = %d, want 0", n)
	}
	if keys := obj.Keys(); keys != nil {
		t.Errorf("Keys = %q, want none", keys)
	}
	if b, ok := buf.Bytes(); ok || b != nil {
		t.Errorf("Bytes = %v, %v, want nothing", b, ok)
	}
	if obj.SetCloneBrand(quickjs.CloneHost) {
		t.Error("SetCloneBrand branded a closed runtime's object")
	}
	if got := p.Value(); got.IsObject() {
		t.Errorf("Promise.Value = %v, want the zero Value", got)
	}
	// A conversion of an object would run its script, which a closed
	// runtime runs none of; a primitive's needs none.
	if s := obj.String(); s != "<object>" {
		t.Errorf("String = %q, want the placeholder", s)
	}
	if f := obj.Float(); !math.IsNaN(f) {
		t.Errorf("Float = %v, want NaN", f)
	}
	if s, f := str.String(), num.Float(); s != "str" || f != 42 {
		t.Errorf("String, Float of primitives = %q, %v", s, f)
	}
	if len(ran) != 0 {
		t.Errorf("ran script after Close: %q", ran)
	}
	// What a host reports of an error once it has closed the runtime -- a
	// deferred Close -- is still the error's.
	var jsErr *quickjs.Error
	if !errors.As(thrown, &jsErr) || jsErr.Error() != "TypeError: bad" ||
		!strings.Contains(jsErr.Stack(), "at thrower (main.js:1:") {
		t.Errorf("an error after Close = %v, stack %q", thrown, jsErr.Stack())
	}

	// Closed from inside, by a Go function the script called, the stack is
	// still in use, and the script's functions are still there to run.
	rt = quickjs.New()
	ran = nil
	rt.Set("note", func(s string) { ran = append(ran, s) })
	var inner []string
	rt.Set("closeAndUse", func(r *quickjs.Runtime, o quickjs.Value) {
		r.Close()
		_, err := o.Get("a")
		inner = append(inner, fmt.Sprint(errors.Is(err, quickjs.ErrClosed)), o.String())
	})
	_, err = rt.Eval(`closeAndUse({ a: 1, toString() { note("toString"); return "s" } })`)
	if !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("closed from inside: %v, want ErrClosed", err)
	}
	if got := strings.Join(inner, ","); got != "true,<object>" || len(ran) != 0 {
		t.Errorf("closed from inside: Get closed, String = %s; ran %q", got, ran)
	}
	if !obj.IsObject() || obj.Kind() != quickjs.KindObject || !arr.IsArray() || !obj.StrictEqual(obj) {
		t.Error("a closed runtime's value lost what it is")
	}
}
