package quickjs_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs"
)

// runLoop is the event loop AsyncWork's documentation gives.
func runLoop(ctx context.Context, rt *quickjs.Runtime) error {
	for rt.Busy() {
		select {
		case <-rt.Wake():
			if err := rt.RunJobsContext(ctx); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func loopContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A timer built on AsyncWork runs its callback on the runtime's goroutine, and
// the loop ends when nothing is left.
func TestAsyncWorkTimer(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.Set("setTimeout", func(cb quickjs.Value, ms int) {
		w := rt.StartAsyncWork()
		time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
			w.Complete(func(err error) {
				if err == nil {
					cb.Call()
				}
			})
		})
	})
	if _, err := rt.Eval(`
		var out = []
		setTimeout(() => { out.push("b"); Promise.resolve().then(() => out.push("c")) }, 20)
		setTimeout(() => out.push("a"), 5)
	`); err != nil {
		t.Fatal(err)
	}
	if !rt.Busy() {
		t.Fatal("not busy with two timers outstanding")
	}
	if err := runLoop(loopContext(t), rt); err != nil {
		t.Fatal(err)
	}
	if v, _ := rt.Eval(`out.join("")`); v.String() != "abc" {
		t.Errorf("out = %q, want %q", v.String(), "abc")
	}
	if rt.Busy() {
		t.Error("still busy with nothing left")
	}
}

// A source of many results -- a socket, a port -- posts each from its own
// goroutine, in order, and keeps the runtime busy until it is done and they
// have run.
func TestAsyncWorkStream(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	var got []int
	w := rt.StartAsyncWork()
	go func() {
		for i := 0; i < 100; i++ {
			w.Post(func(err error) {
				if err != nil {
					t.Errorf("posted %d: %v", i, err)
					return
				}
				got = append(got, i)
			})
		}
		w.Done()
	}()
	if err := runLoop(loopContext(t), rt); err != nil {
		t.Fatal(err)
	}
	if len(got) != 100 {
		t.Fatalf("%d results, want 100", len(got))
	}
	for i, n := range got {
		if n != i {
			t.Fatalf("result %d is %d: out of order", i, n)
		}
	}
	// Posted after Done: told so at once, on the posting goroutine.
	var after error
	w.Post(func(err error) { after = err })
	if !errors.Is(after, quickjs.ErrWorkDone) {
		t.Errorf("posted after Done: %v, want ErrWorkDone", after)
	}
	w.Done() // a second Done does nothing
}

// Close calls what is waiting with ErrClosed, before it returns, and what is
// posted afterwards with ErrClosed at once.
func TestAsyncWorkClose(t *testing.T) {
	rt := quickjs.New()
	var calls []string
	w := rt.StartAsyncWork()
	for _, name := range []string{"a", "b"} {
		w.Post(func(err error) { calls = append(calls, name+":"+errName(err)) })
	}
	rt.Close()
	if got, want := strings.Join(calls, " "), "a:ErrClosed b:ErrClosed"; got != want {
		t.Errorf("after Close: %q, want %q", got, want)
	}
	w.Post(func(err error) { calls = append(calls, "c:"+errName(err)) })
	rt.StartAsyncWork().Complete(func(err error) { calls = append(calls, "d:"+errName(err)) })
	if got, want := strings.Join(calls[2:], " "), "c:ErrClosed d:ErrClosed"; got != want {
		t.Errorf("posted after Close: %q, want %q", got, want)
	}
	if rt.Busy() {
		t.Error("a closed runtime is busy")
	}
	select {
	case <-rt.Wake():
	default:
		t.Error("a closed runtime's Wake is not ready")
	}
}

// Closing from inside a posted function stops it, and what waits behind it is
// called with ErrClosed.
func TestAsyncWorkCloseFromInside(t *testing.T) {
	rt := quickjs.New()
	var calls []string
	w := rt.StartAsyncWork()
	w.Post(func(err error) { calls = append(calls, "a:"+errName(err)); rt.Close() })
	w.Post(func(err error) { calls = append(calls, "b:"+errName(err)) })
	if err := rt.RunJobs(); !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("RunJobs = %v, want ErrClosed", err)
	}
	if got, want := strings.Join(calls, " "), "a:nil b:ErrClosed"; got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

// A posted function that panics closes the runtime, as any engine panic does,
// and those behind it are called with ErrClosed rather than lost.
func TestAsyncWorkPanic(t *testing.T) {
	rt := quickjs.New()
	var calls []string
	w := rt.StartAsyncWork()
	w.Post(func(err error) { panic("host bug") })
	w.Post(func(err error) { calls = append(calls, "b:"+errName(err)) })
	if err := rt.RunJobs(); !errors.Is(err, quickjs.ErrInternal) {
		t.Errorf("RunJobs = %v, want ErrInternal", err)
	}
	if got, want := strings.Join(calls, " "), "b:ErrClosed"; got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

// Unreferenced work does not keep the runtime busy, but what it posts runs.
func TestAsyncWorkUnref(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	w := rt.StartAsyncWork()
	w.Unref()
	if rt.Busy() {
		t.Error("busy with only unreferenced work")
	}
	ran := false
	w.Post(func(err error) { ran = err == nil })
	if !rt.Busy() {
		t.Error("not busy with a posted function waiting")
	}
	if err := runLoop(loopContext(t), rt); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("an unreferenced work's function did not run")
	}
	w.Ref()
	if !rt.Busy() {
		t.Error("not busy after Ref")
	}
	w.Done()
	if rt.Busy() {
		t.Error("busy after Done")
	}
}

// An interrupted run leaves what is still waiting queued for the next.
func TestAsyncWorkInterruption(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	spin, err := rt.Eval(`(function () { for (;;) {} })`)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	w := rt.StartAsyncWork()
	w.Post(func(err error) {
		calls = append(calls, "a")
		spin.Call()
	})
	w.Post(func(err error) { calls = append(calls, "b:"+errName(err)) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := rt.RunJobsContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunJobsContext = %v, want the deadline", err)
	}
	if got := strings.Join(calls, " "); got != "a" {
		t.Fatalf("after the interruption: %q, want %q", got, "a")
	}
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(calls, " "), "a b:nil"; got != want {
		t.Errorf("after the next run: %q, want %q", got, want)
	}
}

// Posting from many goroutines while the runtime closes calls every function
// exactly once. Run with -race.
func TestAsyncWorkExactlyOnce(t *testing.T) {
	for round := 0; round < 20; round++ {
		rt := quickjs.New()
		w := rt.StartAsyncWork()
		var calls atomic.Int64
		var wg sync.WaitGroup
		const posters, each = 8, 50
		for p := 0; p < posters; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < each; i++ {
					w.Post(func(error) { calls.Add(1) })
				}
			}()
		}
		rt.RunJobs()
		rt.Close()
		wg.Wait()
		if n := calls.Load(); n != posters*each {
			t.Fatalf("round %d: %d calls, want %d", round, n, posters*each)
		}
	}
}

// AbortOn stops a script once its channel is closed, from another goroutine.
func TestAbortOn(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	stop := make(chan struct{})
	rt.AbortOn(stop)
	time.AfterFunc(50*time.Millisecond, func() { close(stop) })
	done := make(chan error, 1)
	go func() {
		_, err := rt.Eval(`for (;;) {}`)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("an aborted script returned no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still running 10s after the abort")
	}
}

func errName(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, quickjs.ErrClosed):
		return "ErrClosed"
	case errors.Is(err, quickjs.ErrWorkDone):
		return "ErrWorkDone"
	}
	return err.Error()
}
