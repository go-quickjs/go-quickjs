package quickjs_test

import (
	"errors"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs"
)

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
