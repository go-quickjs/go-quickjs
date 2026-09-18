package stdlib

import (
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Timers installs setTimeout, setInterval, their clear counterparts and
// queueMicrotask.
//
// A timer needs somewhere to run, which is the loop: a callback is not run by
// whatever armed it but by the loop, on the loop's goroutine, when its time
// comes. A host that installs timers and never runs the loop has armed nothing.
//
// The identifiers handed back are numbers, as on the web, and are not reused
// while the timer they name is armed.
func Timers(rt *quickjs.Runtime, loop *Loop) error {
	arm := func(fn quickjs.Value, delayMS float64, repeat bool, args []any) (float64, error) {
		if !fn.IsFunction() {
			return 0, rt.Throw(rt.NewError("TypeError",
				"the first argument must be a function"))
		}
		if !(delayMS >= 0) || delayMS != delayMS {
			// A negative or missing delay means as soon as possible, which is
			// what a browser does with it.
			delayMS = 0
		}
		d := time.Duration(delayMS * float64(time.Millisecond))
		loop.nextID++
		t := &timer{id: loop.nextID, at: time.Now().Add(d), fn: fn, args: args}
		if repeat {
			// An interval of nothing would spin; the web clamps it to a
			// millisecond and so does this.
			t.repeat = max(d, time.Millisecond)
		}
		loop.timers.add(t)
		return float64(t.id), nil
	}

	set := func(name string, repeat bool) error {
		return rt.Set(name, func(r *quickjs.Runtime, fn quickjs.Value, delay quickjs.Value, rest ...quickjs.Value) (float64, error) {
			args := make([]any, len(rest))
			for i, v := range rest {
				args[i] = v
			}
			return arm(fn, delay.Float(), repeat, args)
		})
	}
	if err := set("setTimeout", false); err != nil {
		return err
	}
	if err := set("setInterval", true); err != nil {
		return err
	}

	clear := func(id float64) { loop.timers.cancel(int64(id)) }
	if err := rt.Set("clearTimeout", clear); err != nil {
		return err
	}
	if err := rt.Set("clearInterval", clear); err != nil {
		return err
	}

	// queueMicrotask runs before any timer, whatever its delay: it is a job
	// rather than a task, and the queue drains between them.
	return rt.Set("queueMicrotask", func(fn quickjs.Value) error {
		if !fn.IsFunction() {
			return rt.Throw(rt.NewError("TypeError", "the argument must be a function"))
		}
		rt.EnqueueJob(func() { fn.Call() })
		return nil
	})
}
