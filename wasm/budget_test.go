package wasm_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/api"
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

// TestMemoryLimitCountWhileOthersRun runs, on goroutines of their own, a
// module that sets its globals and grows and sets its table and a module
// that allocates past the limit's sixteenth over and over, so that its
// allocations count what is reachable again and again while the first
// changes what it holds. Under the race detector this checks that a count
// does not read the globals and tables of instances in use on other
// goroutines; without it, that the counts neither crash nor stop either
// module.
func TestMemoryLimitCountWhileOthersRun(t *testing.T) {
	ctx, r := newRuntime(t, wasm.NewRuntimeConfig().WithMemoryLimit(32<<20))
	holder := instantiate(t, ctx, r, `(module (type $a (array i64))
		(global $g (mut (ref null $a)) (ref.null $a))
		(table $t 1 (ref null $a))
		(func (export "churn") (param i32)
			(loop $l
				(global.set $g (array.new_default $a (i32.const 4)))
				(drop (table.grow $t (global.get $g) (i32.const 8)))
				(table.set $t (i32.const 0) (global.get $g))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1)))))))`)
	allocator := instantiate(t, ctx, r, `(module (type $a (array i64))
		(func (export "garbage") (param i32)
			(loop $l
				(drop (array.new_default $a (i32.const 65536)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1)))))))`)
	// A third module's table and global, which the host changes.
	other := instantiate(t, ctx, r, `(module (type $a (array i64))
		(global (export "g") (mut (ref null $a)) (ref.null $a))
		(table (export "t") 1 (ref null $a)))`)
	tab := other.ExportedTable("t")
	g := other.ExportedGlobal("g").(api.MutableGlobal)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		for range 20000 {
			if _, err := tab.Grow(1, api.Value{}); err != nil {
				errs <- err
				return
			}
			if err := tab.Set(0, api.Value{}); err != nil {
				errs <- err
				return
			}
			if err := g.SetValue(api.Value{Type: g.Type()}); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			if _, err := holder.ExportedFunction("churn").Call(ctx, 2000); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 20 {
			if _, err := allocator.ExportedFunction("garbage").Call(ctx, 200); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
