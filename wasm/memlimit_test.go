package wasm_test

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm"
)

// TestMemoryLimitBytes checks that WithMemoryLimit bounds what a runtime's
// modules hold together: a second module's memory counts against what the
// first left, a grow past the limit returns -1, and a memory that does not
// fit fails the instantiation with ErrMemoryLimit.
func TestMemoryLimitBytes(t *testing.T) {
	ctx, r := newRuntime(t, wasm.NewRuntimeConfig().WithMemoryLimit(4*65536))
	a := instantiate(t, ctx, r, `(module (memory 2)
		(func (export "grow") (param i32) (result i32) (memory.grow (local.get 0))))`)
	instantiate(t, ctx, r, `(module (memory 1))`)
	grow := a.ExportedFunction("grow")
	if res, err := grow.Call(ctx, 2); err != nil || int32(res[0]) != -1 {
		t.Fatalf("grow past the limit = %v, %v; want -1", res, err)
	}
	if res, err := grow.Call(ctx, 1); err != nil || res[0] != 2 {
		t.Fatalf("grow within the limit = %v, %v; want 2", res, err)
	}
	_, err := r.Instantiate(ctx, compile(t, `(module (memory 1))`))
	if !errors.Is(err, wasm.ErrMemoryLimit) {
		t.Fatalf("a memory past the limit: got %v, want ErrMemoryLimit", err)
	}
	if got, want := err.Error(), "Out of memory: Cannot allocate Wasm memory for new instance"; got != want {
		t.Fatalf("message %q, want %q", got, want)
	}
}

// TestMemoryLimitReleased checks that a closed module's memory is given back
// once Go collects it.
func TestMemoryLimitReleased(t *testing.T) {
	ctx, r := newRuntime(t, wasm.NewRuntimeConfig().WithMemoryLimit(4*65536))
	src := compile(t, `(module (memory 3))`)
	mod, err := r.Instantiate(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := mod.Close(ctx); err != nil {
		t.Fatal(err)
	}
	mod = nil
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		if _, err = r.Instantiate(ctx, src); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the closed module's memory was not given back: %v", err)
	}
}

// TestMemoryLimitGC checks that GC objects a program keeps stop it with
// ErrMemoryLimit, and that those it lets go do not.
func TestMemoryLimitGC(t *testing.T) {
	ctx, r := newRuntime(t, wasm.NewRuntimeConfig().WithMemoryLimit(1<<20))
	mod := instantiate(t, ctx, r, `(module
		(type $node (struct (field (ref null $node)) (field i64)))
		(global $keep (mut (ref null $node)) (ref.null $node))
		(func (export "churn") (param i32)
			(loop $l
				(drop (struct.new $node (ref.null $node) (i64.const 0)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1))))))
		(func (export "hoard") (param i32)
			(loop $l
				(global.set $keep (struct.new $node (global.get $keep) (i64.const 0)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1)))))))`)
	if _, err := mod.ExportedFunction("churn").Call(ctx, 1_000_000); err != nil {
		t.Fatalf("churn: %v", err)
	}
	if _, err := mod.ExportedFunction("hoard").Call(ctx, 1_000_000); !errors.Is(err, wasm.ErrMemoryLimit) {
		t.Fatalf("hoard: got %v, want ErrMemoryLimit", err)
	}
}
