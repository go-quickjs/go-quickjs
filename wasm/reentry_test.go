package wasm_test

import (
	"context"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/api"
)

// reentrySrc calls the host's "again" from "f", which the host answers by
// calling "f" once more: wasm and Go alternate without end.
const reentrySrc = `(module
	(import "env" "again" (func $again (param i32)))
	(func (export "f") (param i32)
		local.get 0
		i32.const 1
		i32.add
		call $again))`

// TestHostReentryIsBounded checks that MaxCallDepth bounds a chain of calls
// that goes back and forth between wasm and a host function, as it bounds
// wasm's own recursion: each call back into wasm made with the context its
// host function was given, or a context made from it, goes on counting
// from the depth of the call that made it, and the chain ends in the stack
// overflow trap rather than in a Go stack that runs out.
func TestHostReentryIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  wasm.RuntimeConfig
		wrap func(context.Context) context.Context
	}{
		{"interpreter", wasm.NewRuntimeConfigInterpreter(), nil},
		{"compiler", wasm.NewRuntimeConfigCompiler(), nil},
		{"derived context", wasm.NewRuntimeConfigInterpreter(), func(ctx context.Context) context.Context {
			return context.WithValue(ctx, reentryKey{}, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, r := newRuntime(t, tc.cfg)
			var f api.Function
			deepest := uint32(0)
			_, err := r.NewHostModuleBuilder("env").NewFunctionBuilder().
				WithFunc(func(ctx context.Context, n uint32) {
					deepest = max(deepest, n)
					if tc.wrap != nil {
						ctx = tc.wrap(ctx)
					}
					if _, err := f.Call(ctx, uint64(n)); err != nil {
						panic(err)
					}
				}).Export("again").Instantiate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			mod := instantiate(t, ctx, r, reentrySrc)
			f = mod.ExportedFunction("f")
			_, err = f.Call(ctx, 0)
			if err == nil || !strings.Contains(err.Error(), "Maximum call stack size exceeded") {
				t.Fatalf("got %v; want the stack overflow trap", err)
			}
			// Each round trip counts 16 of MaxCallDepth's 30,000: the
			// wasm function's level, the host's, and the 14 a call back
			// into wasm counts for.
			if deepest < 1_850 || deepest > 1_880 {
				t.Errorf("the chain ended after %d round trips; want about 1,875", deepest)
			}
			// The runtime is usable after it.
			if _, err := f.Call(context.Background(), 1<<31); err == nil {
				t.Fatal("a fresh call of the same chain did not end either")
			}
		})
	}
}

type reentryKey struct{}
