package wasm_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
)

// TestCompilerConfigRunsNative checks that NewRuntimeConfigCompiler has a
// hot function compiled to native code where the JIT runs, and that its
// result is the interpreter's.
func TestCompilerConfigRunsNative(t *testing.T) {
	if !engine.JITAvailable() {
		t.Skip("no native code in this build")
	}
	ctx, r := newRuntime(t, wasm.NewRuntimeConfigCompiler())
	mod := instantiate(t, ctx, r, `(module
		(func (export "sum") (param i32) (result i32) (local i32)
			(block $done (loop $l
				(br_if $done (i32.eqz (local.get 0)))
				(local.set 1 (i32.add (local.get 1) (local.get 0)))
				(local.set 0 (i32.sub (local.get 0) (i32.const 1)))
				(br $l)))
			(local.get 1)))`)
	before := engine.ReadJITStats().Compiled
	for i := 0; i < 2*engine.DefaultJITThreshold; i++ {
		res, err := mod.ExportedFunction("sum").Call(ctx, 100)
		if err != nil || res[0] != 5050 {
			t.Fatalf("sum(100) = %v, %v", res, err)
		}
	}
	if engine.ReadJITStats().Compiled == before {
		t.Fatal("no function was compiled to native code")
	}
}
