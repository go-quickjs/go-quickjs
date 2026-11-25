package wasm_test

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm"
)

// bigFrames is a module whose "down" calls itself n times, each frame
// holding 49,998 i64 locals -- some 400 KB -- besides its parameter.
var bigFrames = `(module
	(func $down (export "down") (param i32) (local` + strings.Repeat(" i64", 49_998) + `)
		(if (local.get 0)
			(then (call $down (i32.sub (local.get 0) (i32.const 1)))))))`

// TestStackBytesAreBounded checks that what a call's frames take is
// bounded, not only how deep they nest: frames of 400 KB recursed 1,000
// deep -- 400 MB -- trap with the stack overflow's message rather than
// allocating it all, with no memory limit and under one, whose bytes bound
// them when they are fewer. Before, the depth limit alone bounded them:
// 30,000 such frames, 12 GB, with no error even under WithMemoryLimit. A
// recursion that fits still runs.
func TestStackBytesAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  wasm.RuntimeConfig
	}{
		{"no limit", wasm.NewRuntimeConfig()},
		{"memory limit", wasm.NewRuntimeConfig().WithMemoryLimit(16 << 20)},
		{"compiler", wasm.NewRuntimeConfigCompiler()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, r := newRuntime(t, tc.cfg)
			down := instantiate(t, ctx, r, bigFrames).ExportedFunction("down")
			if tc.name != "memory limit" {
				if _, err := down.Call(ctx, 100); err != nil {
					t.Fatalf("100 frames, 40 MB: %v", err)
				}
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := down.Call(ctx, 1000)
			runtime.ReadMemStats(&after)
			if err == nil || !strings.Contains(err.Error(), "Maximum call stack size exceeded") {
				t.Fatalf("1,000 frames, 400 MB: got %v; want the stack overflow trap", err)
			}
			var rerr *wasm.RuntimeError
			if !errors.As(err, &rerr) {
				t.Errorf("got a %T; want a *wasm.RuntimeError", err)
			}
			if n := after.TotalAlloc - before.TotalAlloc; n > 128<<20 {
				t.Errorf("the call allocated %d bytes of frames", n)
			}
			// The runtime runs on.
			if _, err := down.Call(ctx, 3); err != nil {
				t.Fatalf("after the overflow: %v", err)
			}
		})
	}
}
