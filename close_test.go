package quickjs_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs"
)

// A script that closes its runtime ends at the call that closed it, as
// process.exit ends a node worker: nothing after it runs -- not the next
// statement, not a catch, not a finally.
func TestCloseFromInsideRunsNothingMore(t *testing.T) {
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
