package wasm_test

import (
	"errors"
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm"
)

// TestMemoryLimitInGlobalInit checks that a global's initializer that
// passes the limit fails the instantiation with ErrMemoryLimit. The count
// of what is reachable that the allocation makes meets the instance half
// built, its later globals not made yet, and dereferenced the first of
// them: the instantiation panicked.
func TestMemoryLimitInGlobalInit(t *testing.T) {
	ctx, r := newRuntime(t, wasm.NewRuntimeConfig().WithMemoryLimit(64<<20))
	_, err := r.Instantiate(ctx, compile(t, `(module (type $a (array i64))
		(global (ref $a) (array.new_default $a (i32.const 130000000)))
		(global (ref $a) (array.new_default $a (i32.const 1))))`))
	if !errors.Is(err, wasm.ErrMemoryLimit) {
		t.Fatalf("got %v; want ErrMemoryLimit", err)
	}
	// Past the first count, as the later globals are made.
	_, err = r.Instantiate(ctx, compile(t, `(module (type $a (array i64))
		(global (ref $a) (array.new_default $a (i32.const 1000000)))
		(global (ref $a) (array.new_default $a (i32.const 1000000)))
		(global (ref $a) (array.new_default $a (i32.const 1000000)))
		(global (ref $a) (array.new_default $a (i32.const 1000000)))
		(global (ref $a) (array.new_default $a (i32.const 1000000)))
		(global (ref $a) (array.new_default $a (i32.const 1000000)))
		(global (ref $a) (array.new_default $a (i32.const 1000000)))
		(global (ref $a) (array.new_default $a (i32.const 1000000)))
		(global (ref $a) (array.new_default $a (i32.const 1000000)))
		(table 1 (ref null $a)))`))
	if !errors.Is(err, wasm.ErrMemoryLimit) {
		t.Fatalf("nine 8 MB arrays: got %v; want ErrMemoryLimit", err)
	}
}

// TestMemoryLimitOneBigObject checks that an object that alone passes the
// limit stops the call that makes it. The count of what is reachable that
// its charge set off forgot the object it was made for, which nothing
// reaches yet: the count found room, and the 1 GB array was made under a
// 64 MB limit.
func TestMemoryLimitOneBigObject(t *testing.T) {
	ctx, r := newRuntime(t, wasm.NewRuntimeConfig().WithMemoryLimit(64<<20))
	mod := instantiate(t, ctx, r, `(module (type $a (array i64))
		(func (export "big") (result i32)
			(array.len (array.new_default $a (i32.const 130000000)))))`)
	if _, err := mod.ExportedFunction("big").Call(ctx); !errors.Is(err, wasm.ErrMemoryLimit) {
		t.Fatalf("got %v; want ErrMemoryLimit", err)
	}
}

// TestMemoryLimitBeforeAllocating checks that a table or a memory past the
// limit is refused before it is allocated, not after.
func TestMemoryLimitBeforeAllocating(t *testing.T) {
	ctx, r := newRuntime(t, wasm.NewRuntimeConfig().WithMemoryLimit(64<<20))
	for _, src := range []string{
		`(module (table 10000000 funcref))`,
		`(module (memory 16384))`, // 1 GiB, within every platform's limit
	} {
		bin := compile(t, src)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := r.Instantiate(ctx, bin)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, wasm.ErrMemoryLimit) {
			t.Fatalf("%s: got %v; want ErrMemoryLimit", src, err)
		}
		if n := after.TotalAlloc - before.TotalAlloc; n > 8<<20 {
			t.Errorf("%s: %d bytes allocated before the limit refused it", src, n)
		}
	}
}
