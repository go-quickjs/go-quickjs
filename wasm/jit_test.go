package wasm_test

import (
	"context"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
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
	for i := 0; i < 2*engine.DefaultJITThreshold+2; i++ {
		res, err := mod.ExportedFunction("sum").Call(ctx, 100)
		if err != nil || res[0] != 5050 {
			t.Fatalf("sum(100) = %v, %v", res, err)
		}
	}
	if engine.ReadJITStats().Compiled == before {
		t.Fatal("no function was compiled to native code")
	}
}

// Under WithJITMemoryLimit, the compiler holds no more than the limit for
// the runtime's modules, and refuses what does not fit, which the
// interpreter runs: the results are the same. A negative limit panics, and
// runtimes sharing a compilation cache share modules compiled under the
// same limit only.
func TestJITMemoryLimit(t *testing.T) {
	if !engine.JITAvailable() {
		t.Skip("no native code in this build")
	}
	src := `(module
		(func $fib (export "fib") (param i32) (result i32)
			(if (result i32) (i32.lt_u (local.get 0) (i32.const 2))
				(then (local.get 0))
				(else (i32.add (call $fib (i32.sub (local.get 0) (i32.const 1)))
					(call $fib (i32.sub (local.get 0) (i32.const 2)))))))
		(func (export "sum") (param i32) (result i32) (local i32)
			(block $done (loop $l
				(br_if $done (i32.eqz (local.get 0)))
				(local.set 1 (i32.add (local.get 1) (local.get 0)))
				(local.set 0 (i32.sub (local.get 0) (i32.const 1)))
				(br $l)))
			(local.get 1)))`
	bin, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{1, 4 << 10, 0} {
		ctx, r := newRuntime(t, wasm.NewRuntimeConfigCompiler().WithJITMemoryLimit(limit))
		cm, err := r.CompileModule(ctx, bin)
		if err != nil {
			t.Fatal(err)
		}
		mod, err := r.InstantiateModule(ctx, cm, wasm.NewModuleConfig().WithName(""))
		if err != nil {
			t.Fatal(err)
		}
		before := engine.ReadJITStats().Refused[engine.RefuseBudget]
		for i := 0; i < 3; i++ {
			if res, err := mod.ExportedFunction("fib").Call(ctx, 20); err != nil || res[0] != 6765 {
				t.Fatalf("limit %d: fib(20) = %v, %v", limit, res, err)
			}
			if res, err := mod.ExportedFunction("sum").Call(ctx, 1000); err != nil || res[0] != 500500 {
				t.Fatalf("limit %d: sum(1000) = %v, %v", limit, res, err)
			}
		}
		m := wasm.JITMemoryOf(cm)
		want := limit
		if want == 0 {
			want = wasm.DefaultJITMemoryLimit
		}
		refused := engine.ReadJITStats().Refused[engine.RefuseBudget] - before
		switch {
		case limit == 1 && (m.Code != 0 || refused == 0):
			t.Fatalf("limit 1: %+v, %d refused for the budget; want nothing compiled", m, refused)
		case limit != 1 && m.Code == 0:
			t.Fatalf("limit %d: nothing compiled", limit)
		case limit != 1 && m.Held() > want:
			t.Fatalf("limit %d: holds %d", limit, m.Held())
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a negative limit did not panic")
		}
	}()
	cache := wasm.NewCompilationCache()
	ctx := context.Background()
	r1 := wasm.NewRuntimeWithConfig(ctx, wasm.NewRuntimeConfigCompiler().WithCompilationCache(cache).WithJITMemoryLimit(1<<20))
	r2 := wasm.NewRuntimeWithConfig(ctx, wasm.NewRuntimeConfigCompiler().WithCompilationCache(cache).WithJITMemoryLimit(1<<20))
	r3 := wasm.NewRuntimeWithConfig(ctx, wasm.NewRuntimeConfigCompiler().WithCompilationCache(cache).WithJITMemoryLimit(2<<20))
	c1, _ := r1.CompileModule(ctx, bin)
	c2, _ := r2.CompileModule(ctx, bin)
	c3, _ := r3.CompileModule(ctx, bin)
	if !wasm.SameCode(c1, c2) || wasm.SameCode(c1, c3) {
		t.Fatalf("shared under one limit: %v, under two: %v", wasm.SameCode(c1, c2), wasm.SameCode(c1, c3))
	}
	wasm.NewRuntimeConfigCompiler().WithJITMemoryLimit(-1)
}
