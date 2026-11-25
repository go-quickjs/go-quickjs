//go:build quickjs_wasm

package wasmjs_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// watBytes compiles text module src to a Uint8Array literal.
func watBytes(t *testing.T, src string) string {
	t.Helper()
	b, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return jsBytes(b)
}

// TestMemoryLimitBuffers checks that a memory's buffer is counted by the
// memory's size, not by the address space reserved for it: a script holding
// the buffers of a memory and of a shared memory of 1 GiB's maximum, under a
// limit of 64 MiB, ran out of memory at its next measurement. A shared
// memory's growable buffer, over all of the memory's reservation, is counted
// by its length too.
func TestMemoryLimitBuffers(t *testing.T) {
	rt := newRuntime(t, quickjs.WithMemoryLimit(64<<20))
	v, err := rt.Eval(`
		globalThis.keep = [
			new WebAssembly.Memory({initial: 1}).buffer,
			new WebAssembly.Memory({initial: 1, maximum: 16384, shared: true}).buffer,
			new WebAssembly.Memory({initial: 1, maximum: 16384, shared: true}).toResizableBuffer(),
		];
		let a = [];
		for (let i = 0; i < 200000; i++) a.push({i});
		a.length`)
	if err != nil || v.String() != "200000" {
		t.Fatalf("got %v, %v; want 200000", v, err)
	}
}

// TestMemoryLimitGrow checks that a memory grows only as far as the limit
// allows: Memory.prototype.grow then throws V8's RangeError, and memory.grow
// returns -1.
func TestMemoryLimitGrow(t *testing.T) {
	rt := newRuntime(t, quickjs.WithMemoryLimit(64<<20))
	got := eval(t, rt, `
		const m = new WebAssembly.Memory({initial: 1});
		let pages = 1, msg;
		try { for (;;) { m.grow(16); pages += 16; } } catch (e) { msg = String(e); }
		globalThis.out = [pages > 256 && pages < 1024, msg].join();`)
	if want := "true,RangeError: WebAssembly.Memory.grow(): Unable to grow instance memory"; got != want {
		t.Fatalf("Memory.prototype.grow: got %q, want %q", got, want)
	}
	rt = newRuntime(t, quickjs.WithMemoryLimit(64<<20))
	got = eval(t, rt, `
		const {exports} = new WebAssembly.Instance(new WebAssembly.Module(`+watBytes(t, `(module
			(memory 1)
			(func (export "grow") (param i32) (result i32) (memory.grow (local.get 0))))`)+`));
		let r = 0, grown = 0;
		while ((r = exports.grow(16)) !== -1) grown += 16;
		globalThis.out = [grown > 256 && grown < 1024, r].join();`)
	if want := "true,-1"; got != want {
		t.Fatalf("memory.grow: got %q, want %q", got, want)
	}
}

// TestMemoryLimitCountsWasm checks that the meter's walk counts a memory
// held only by an instance: with 32 MiB of it held, an ArrayBuffer of 40 MiB
// does not fit a limit of 64 MiB, and stops the script.
func TestMemoryLimitCountsWasm(t *testing.T) {
	rt := newRuntime(t, quickjs.WithMemoryLimit(64<<20))
	_, err := rt.Eval(`
		globalThis.inst = new WebAssembly.Instance(new WebAssembly.Module(` + watBytes(t, `(module (memory 512))`) + `));
		try { new ArrayBuffer(40 << 20); } catch {}`)
	if !errors.Is(err, quickjs.ErrMemoryLimit) {
		t.Fatalf("got %v, want ErrMemoryLimit", err)
	}
	// Without the instance, the buffer fits.
	rt = newRuntime(t, quickjs.WithMemoryLimit(64<<20))
	if _, err := rt.Eval(`new ArrayBuffer(40 << 20); 0`); err != nil {
		t.Fatal(err)
	}
}

// TestMemoryLimitGC checks that GC objects wasm allocates are charged: a
// program keeping them is stopped, which no try/catch catches, and one
// letting them go runs.
func TestMemoryLimitGC(t *testing.T) {
	src := watBytes(t, `(module
		(type $node (struct (field (ref null $node)) (field i64)))
		(global $keep (mut (ref null $node)) (ref.null $node))
		(func (export "churn") (param i32)
			(loop $l
				(drop (struct.new $node (ref.null $node) (i64.const 0)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1))))))
		(func (export "hoard") (param i32)
			(loop $l
				(global.set $keep (struct.new $node (global.get $keep) (i64.const 0)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1))))))
		(func (export "hoardLocal") (param i32) (local $list (ref null $node))
			(loop $l
				(local.set $list (struct.new $node (local.get $list) (i64.const 0)))
				(br_if $l (local.tee 0 (i32.sub (local.get 0) (i32.const 1)))))))`)
	rt := newRuntime(t, quickjs.WithMemoryLimit(16<<20))
	if _, err := rt.Eval(`
		const {exports} = new WebAssembly.Instance(new WebAssembly.Module(` + src + `));
		exports.churn(3000000);`); err != nil {
		t.Fatalf("churn: %v", err)
	}
	_, err := rt.Eval(`try { exports.hoard(3000000); } catch {}`)
	if !errors.Is(err, quickjs.ErrMemoryLimit) {
		t.Fatalf("hoard: got %v, want ErrMemoryLimit", err)
	}
	// What only a running call's frame holds is counted too.
	rt = newRuntime(t, quickjs.WithMemoryLimit(16<<20))
	_, err = rt.Eval(`new WebAssembly.Instance(new WebAssembly.Module(` + src + `)).exports.hoardLocal(3000000)`)
	if !errors.Is(err, quickjs.ErrMemoryLimit) {
		t.Fatalf("hoardLocal: got %v, want ErrMemoryLimit", err)
	}
}

// TestMemoryLimitTable checks that a table that does not fit stops the
// script, as an allocation of a size the script chose does.
func TestMemoryLimitTable(t *testing.T) {
	rt := newRuntime(t, quickjs.WithMemoryLimit(16<<20))
	_, err := rt.Eval(`try { new WebAssembly.Table({element: "externref", initial: 5000000}); } catch {}`)
	if !errors.Is(err, quickjs.ErrMemoryLimit) {
		t.Fatalf("got %v, want ErrMemoryLimit", err)
	}
}

// TestMemoryLimitCountsModules checks that the meter's walk counts what a
// WebAssembly.Module holds -- its copy of the module's bytes among it -- so
// that a script holding modules under a limit is stopped as one holding
// any other memory is: 64 modules of 1 MiB each, under a limit of 32 MiB,
// were never counted. One that lets them go runs.
func TestMemoryLimitCountsModules(t *testing.T) {
	const script = `
		const n = 1 << 20, leb = [];
		for (let v = n + 2; ; v >>>= 7) { if (v < 128) { leb.push(v); break; } leb.push(v & 127 | 128); }
		// A module of a custom section named "x" of n bytes.
		const bytes = new Uint8Array(8 + 1 + leb.length + 2 + n);
		bytes.set([0, 0x61, 0x73, 0x6d, 1, 0, 0, 0, 0, ...leb, 1, 0x78]);
		let modules = [];
		for (let i = 0; i < 64; i++) {
			const m = new WebAssembly.Module(bytes);
			if (KEEP) modules.push(m);
			for (let j = 0; j < 2000; j++) modules.length;
			new Array(1000).fill(i);
		}
		modules.length`
	rt := newRuntime(t, quickjs.WithMemoryLimit(32<<20))
	if _, err := rt.Eval(strings.Replace(script, "KEEP", "true", 1)); !errors.Is(err, quickjs.ErrMemoryLimit) {
		t.Fatalf("holding 64 MiB of modules: got %v, want ErrMemoryLimit", err)
	}
	rt = newRuntime(t, quickjs.WithMemoryLimit(32<<20))
	if v, err := rt.Eval(strings.Replace(script, "KEEP", "false", 1)); err != nil || v.String() != "0" {
		t.Fatalf("letting the modules go: got %v, %v; want 0", v, err)
	}
}
