package wasm_test

import (
	"context"
	"runtime"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm"
)

// TestReadViewOutlivesDroppedRuntime checks that a view Memory.Read gave
// out stays valid when its module's runtime is dropped without being
// closed and is collected: a module holds its memory until it is closed,
// so the bytes are never released under the view. Before, the memory's
// reservation was released once the Memory was collected, and the write
// below ended the process with a fault.
func TestReadViewOutlivesDroppedRuntime(t *testing.T) {
	b := func() []byte {
		ctx := context.Background()
		r := wasm.NewRuntime(ctx)
		mod := instantiate(t, ctx, r, `(module (memory (export "mem") 1))`)
		b, ok := mod.Memory().Read(0, 16)
		if !ok {
			t.Fatal("no view")
		}
		return b
	}()
	for range 5 {
		runtime.GC()
	}
	b[0], b[15] = 1, 2
	if b[0] != 1 || b[15] != 2 {
		t.Fatal("the view's bytes changed")
	}
}

// TestClosedModuleRunsThroughImport checks that closing a module does not
// release what its functions still use while another module can call them:
// a module importing a function of a closed module calls it, and it reads
// and writes its own module's memory.
func TestClosedModuleRunsThroughImport(t *testing.T) {
	ctx, r := newRuntime(t)
	a, err := r.InstantiateWithConfig(ctx, compile(t, `(module
		(memory 1)
		(func (export "bump") (result i32)
			i32.const 0
			i32.const 0
			i32.load
			i32.const 1
			i32.add
			i32.store
			i32.const 0
			i32.load))`), wasm.NewModuleConfig().WithName("a"))
	if err != nil {
		t.Fatal(err)
	}
	b := instantiate(t, ctx, r, `(module
		(import "a" "bump" (func $bump (result i32)))
		(func (export "twice") (result i32)
			call $bump
			drop
			call $bump))`)
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a = nil
	for range 5 {
		runtime.GC()
	}
	res, err := b.ExportedFunction("twice").Call(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res[0] != 2 {
		t.Fatalf("twice() = %d; want 2", res[0])
	}
}
