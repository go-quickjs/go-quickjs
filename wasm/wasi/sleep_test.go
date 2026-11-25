package wasi_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
)

// clockSub writes, at sub, poll_oneoff's subscription to the monotonic
// clock d from now.
func clockSub(h *harness, sub uint32, d time.Duration) {
	h.mem.WriteUint64Le(sub, 7)    // userdata
	h.mem.Write(sub+8, []byte{0})  // eventtype clock
	h.mem.WriteUint32Le(sub+16, 1) // clockid monotonic
	h.mem.WriteUint64Le(sub+24, uint64(d))
	h.mem.WriteUint64Le(sub+32, 0) // precision
	h.mem.WriteUint32Le(sub+40, 0) // flags: relative
}

// TestSleepEndsWithContext checks that poll_oneoff's sleep ends when the
// call's context does, whatever sleeps -- the system's sleeper, a host's
// that watches the context, and a host's that does not, which the engine
// cuts off -- and ends the call as the engine's own check of the context
// would: with the context's error, or, with WithCloseOnContextDone, with
// the exit code for it.
func TestSleepEndsWithContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	sleepers := []struct {
		name string
		cfg  func(wasm.ModuleConfig) wasm.ModuleConfig
	}{
		{"system", func(c wasm.ModuleConfig) wasm.ModuleConfig { return c.WithSysNanosleep() }},
		{"watching", func(c wasm.ModuleConfig) wasm.ModuleConfig {
			return c.WithNanosleep(func(ctx context.Context, ns int64) {
				select {
				case <-ctx.Done():
				case <-time.After(time.Duration(ns)):
				}
			})
		}},
		{"deaf", func(c wasm.ModuleConfig) wasm.ModuleConfig {
			return c.WithNanosleep(func(context.Context, int64) { <-release })
		}},
	}
	for _, s := range sleepers {
		for _, closeOnDone := range []bool{false, true} {
			rcfg := wasm.NewRuntimeConfig().WithCloseOnContextDone(closeOnDone)
			h := newHarnessIn(t, rcfg, s.cfg(wasm.NewModuleConfig()))
			clockSub(h, scratch, time.Hour)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			start := time.Now()
			_, err := h.mod.ExportedFunction("poll_oneoff").Call(ctx, scratch, out, 1, out+64)
			cancel()
			if d := time.Since(start); d > 30*time.Second {
				t.Fatalf("%s, closeOnDone=%v: the sleep ran %v past its context", s.name, closeOnDone, d)
			}
			if closeOnDone {
				var exit *sys.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != sys.ExitCodeDeadlineExceeded {
					t.Errorf("%s, closeOnDone: got %v; want the deadline's exit code", s.name, err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s: got %v; want the context's deadline", s.name, err)
			}
		}
	}
}

// TestNanosleepIsGivenTheSleep checks that a host's sleeper is asked for
// the whole sleep, with the call's context, and that the call waits for it
// while the context is not done.
func TestNanosleepIsGivenTheSleep(t *testing.T) {
	type key struct{}
	var slept int64
	var sawCall bool
	h := newHarness(t, wasm.NewModuleConfig().WithNanosleep(func(ctx context.Context, ns int64) {
		slept, sawCall = ns, ctx.Value(key{}) == "call"
	}))
	clockSub(h, scratch, time.Hour)
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), key{}, "call"), time.Minute)
	defer cancel()
	if _, err := h.mod.ExportedFunction("poll_oneoff").Call(ctx, scratch, out, 1, out+64); err != nil {
		t.Fatal(err)
	}
	if slept != int64(time.Hour) {
		t.Errorf("the sleeper was asked for %d ns; want an hour's", slept)
	}
	if !sawCall {
		t.Error("the sleeper was not given the call's context")
	}
}

// TestNanosleepPanicIsTheCalls checks that a host's sleeper that panics,
// on the goroutine the engine runs it on, fails the call rather than the
// process.
func TestNanosleepPanicIsTheCalls(t *testing.T) {
	h := newHarness(t, wasm.NewModuleConfig().WithNanosleep(func(context.Context, int64) {
		panic("the sleeper broke")
	}))
	clockSub(h, scratch, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := h.mod.ExportedFunction("poll_oneoff").Call(ctx, scratch, out, 1, out+64)
	if err == nil || !strings.Contains(err.Error(), "the sleeper broke") {
		t.Fatalf("got %v; want the sleeper's panic", err)
	}
}
