//go:build quickjs_wasm

package wasmjs_test

import (
	"context"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
	"github.com/go-quickjs/go-quickjs/wasm/wasmjs"
)

// runWorkers runs src in a runtime that can start workers, each with the
// WebAssembly namespace, until its loop has nothing left to do, and returns
// the global out, which src sets. node:worker_threads is Worker's module.
func runWorkers(t *testing.T, src string, opts ...quickjs.Option) string {
	t.Helper()
	rt := quickjs.New(opts...)
	defer rt.Close()
	loop := stdlib.NewLoop(rt)
	workers := &stdlib.Workers{
		New:       func() (*quickjs.Runtime, error) { return quickjs.New(opts...), nil },
		Installed: func(w *quickjs.Runtime) error { return wasmjs.Install(w) },
	}
	if err := stdlib.Install(rt, stdlib.Config{Loop: loop, Workers: workers}); err != nil {
		t.Fatal(err)
	}
	if err := wasmjs.Install(rt); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.EvalModule("<main>", `import { Worker } from "node:worker_threads";
		globalThis.inWorker = (code, data) => new Promise((resolve, reject) => {
			const w = new Worker(code, { eval: true });
			w.on("error", reject);
			w.on("message", (m) => w.terminate().then(() => resolve(m)));
			w.postMessage(data);
		});`); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(src); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := loop.Run(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval(`String(globalThis.out)`)
	if err != nil {
		t.Fatal(err)
	}
	return v.String()
}

// TestPostModuleAndMemory pins a Module and a shared Memory posted to a
// worker, without Node's quirks: the worker instantiates the Module over the
// Memory, writes and grows it, and posts both back, which the parent sees as
// the memory it has.
func TestPostModuleAndMemory(t *testing.T) {
	mod := watBytes(t, `(module (import "m" "mem" (memory 1 4 shared)) (export "mem" (memory 0))
		(func (export "grow") (param i32) (result i32) (memory.grow (local.get 0))))`)
	got := runWorkers(t, `
		const mod = new WebAssembly.Module(`+mod+`);
		const mem = new WebAssembly.Memory({ initial: 1, maximum: 4, shared: true });
		new Int32Array(mem.buffer)[0] = 5;
		inWorker(`+"`"+`
			import { parentPort } from "node:worker_threads";
			parentPort.once("message", ({ mod, mem }) => {
				const i = new WebAssembly.Instance(mod, { m: { mem } });
				const a = new Int32Array(mem.buffer);
				a[1] = a[0] * 2;
				parentPort.postMessage({ grew: i.exports.grow(2), mem, same: i.exports.mem === mem });
			});`+"`"+`, { mod, mem }).then(({ grew, mem: back, same }) => {
			globalThis.out = [grew, same, new Int32Array(mem.buffer)[1], mem.buffer.byteLength, back === mem,
				back.buffer.byteLength, new Int32Array(back.buffer)[1]].join();
		});`)
	if want := "1,true,10,196608,false,196608,10"; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
