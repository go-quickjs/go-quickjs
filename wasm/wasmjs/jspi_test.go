//go:build quickjs_wasm

package wasmjs_test

import (
	"runtime"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/wasm/wasmjs"
)

// TestPromisingStackEndsOnClose pins that closing a runtime ends the
// goroutines of its suspended promising calls, which nothing else would: a
// promise the script still holds, or one only the suspending function holds
// -- which the suspended goroutine itself keeps reachable -- may yet settle
// for all the collector can tell. Close returns with none of them left, and
// what the collector does after is harmless.
func TestPromisingStackEndsOnClose(t *testing.T) {
	before := promisingStacks()
	rt := quickjs.New()
	if err := wasmjs.Install(rt); err != nil {
		t.Fatal(err)
	}
	// (import "m" "f" (func (result i32))) (import "m" "g" (func (result i32)))
	// (func (export "run") (result i32) (call 0)) (func (export "run2") (result i32) (call 1))
	mod := watBytes(t, `(module (import "m" "f" (func $f (result i32))) (import "m" "g" (func $g (result i32)))
		(func (export "run") (result i32) (call $f))
		(func (export "run2") (result i32) (drop (call $g)) (call $f)))`)
	if _, err := rt.Eval(`
		globalThis.pending = new Promise(() => {});
		{
			const own = new Promise(() => {});
			let n = 0;
			const e = new WebAssembly.Instance(new WebAssembly.Module(` + mod + `), {m: {
				f: new WebAssembly.Suspending(() => n++ % 2 ? pending : own),
				g: new WebAssembly.Suspending(() => Promise.resolve(1)),
			}}).exports;
			for (let i = 0; i < 10; i++) WebAssembly.promising(e.run)();
			// These resume once, from g's promise, before suspending for good.
			for (let i = 0; i < 10; i++) WebAssembly.promising(e.run2)();
		}`); err != nil {
		t.Fatal(err)
	}
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if n := promisingStacks(); n != before+20 {
		t.Fatalf("%d promising stacks, want the 20 suspended calls' too (%d before)", n, before)
	}
	for range 3 {
		runtime.GC()
	}
	time.Sleep(10 * time.Millisecond)
	if n := promisingStacks(); n != before+20 {
		t.Fatalf("%d promising stacks after collecting, want 20 more than the %d before", n, before)
	}
	rt.Close()
	if n := promisingStacks(); n != before {
		t.Fatalf("%d promising stacks after Close, %d before", n, before)
	}
	for range 3 {
		runtime.GC()
	}
	time.Sleep(10 * time.Millisecond)
	if n := promisingStacks(); n != before {
		t.Fatalf("%d promising stacks after collecting, %d before", n, before)
	}
}

// TestPromisingStackEndsCollectedAfterResuming pins that a stack that has
// resumed and suspended again on a promise nothing holds ends once the
// collector finds that, the runtime open: each suspension is a cleanup of
// its own.
func TestPromisingStackEndsCollectedAfterResuming(t *testing.T) {
	before := promisingStacks()
	rt := newRuntime(t)
	mod := watBytes(t, `(module (import "m" "f" (func $f (result i32)))
		(func (export "run") (result i32) (drop (call $f)) (call $f)))`)
	if _, err := rt.Eval(`{
			let n = 0;
			const e = new WebAssembly.Instance(new WebAssembly.Module(` + mod + `), {m: {
				f: new WebAssembly.Suspending(() => n++ < 10 ? Promise.resolve(1) : new Promise(() => {})),
			}}).exports;
			for (let i = 0; i < 10; i++) WebAssembly.promising(e.run)();
		}`); err != nil {
		t.Fatal(err)
	}
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if n := promisingStacks(); n != before+10 {
		t.Fatalf("%d promising stacks, want 10 more than the %d before", n, before)
	}
	deadline := time.Now().Add(10 * time.Second)
	for promisingStacks() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d promising stacks remain, %d before", promisingStacks(), before)
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}
