package stdlib_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
)

// workerFiles is a Workers that loads code from a map, by the name a script
// gives, and judges a module by its name.
func workerFiles(files map[string]string) *stdlib.Workers {
	return &stdlib.Workers{
		Load: func(specifier string) (string, string, bool, error) {
			src, ok := files[specifier]
			if !ok {
				return "", "", false, fmt.Errorf("Cannot find module '%s'", specifier)
			}
			return src, specifier, strings.HasSuffix(specifier, ".mjs"), nil
		},
	}
}

// TestWorkerThreads pins node:worker_threads as node 26 has it: the events a
// Worker emits and their order, what a worker knows of itself, and how it
// ends -- of itself, by process.exit, by terminate, or by failing.
func TestWorkerThreads(t *testing.T) {
	worker := `
		import { parentPort, workerData, isMainThread, threadId } from "node:worker_threads";
		parentPort.postMessage({ isMainThread, main: threadId === 0, workerData, envX: process.env.X, argv: process.argv.slice(2) });
		parentPort.on("message", (m) => {
			if (m === "exit") { process.exit(7); parentPort.postMessage("after exit"); }
			if (m === "throw") throw new RangeError("from worker");
			if (m === "reject") Promise.reject(new TypeError("rejected"));
			parentPort.postMessage("echo " + m);
		});`
	out, _ := run(t, stdlib.Config{
		Workers: workerFiles(map[string]string{"./w.mjs": worker}),
		Process: &stdlib.Process{Args: []string{"qjs", "main.js"}, Env: map[string]string{"X": "parent"}},
	}, `
		const { Worker, isMainThread, threadId, parentPort, workerData } = require_worker_threads;
		console.log("main", isMainThread, threadId, parentPort, workerData);
		const each = async (cmds) => {
			for (const cmd of cmds) await new Promise((resolve) => {
				const events = [];
				const w = new Worker("./w.mjs", { workerData: { n: 1 }, argv: ["a", 2], env: { X: "y" } });
				w.on("online", () => events.push("online"));
				w.on("message", (m) => {
					events.push("message " + JSON.stringify(m));
					if (typeof m === "object") w.postMessage(cmd);
					else if (cmd === "hi") w.terminate().then((c) => events.push("terminate resolved " + c));
				});
				w.on("error", (e) => events.push("error " + e.constructor.name + " " + e.message));
				w.on("exit", (c) => { events.push("exit " + c + " " + w.threadId); setTimeout(() => { console.log(cmd, "|", events.join(" | ")); resolve(); }, 10); });
			});
			try { new Worker("w.mjs") } catch (e) { console.log(e.code, e.message) }
			const missing = new Worker("./missing.mjs");
			missing.on("error", (e) => console.log("missing", e.message));
			missing.on("exit", (c) => console.log("missing exit", c));
		};
		each(["hi", "exit", "throw", "reject"]);
	`)
	want := strings.Join([]string{
		"main true 0 null null",
		`hi | online | message {"isMainThread":false,"main":false,"workerData":{"n":1},"envX":"y","argv":["a","2"]} | message "echo hi" | exit 1 -1 | terminate resolved 1`,
		`exit | online | message {"isMainThread":false,"main":false,"workerData":{"n":1},"envX":"y","argv":["a","2"]} | exit 7 -1`,
		`throw | online | message {"isMainThread":false,"main":false,"workerData":{"n":1},"envX":"y","argv":["a","2"]} | error RangeError from worker | exit 1 -1`,
		`reject | online | message {"isMainThread":false,"main":false,"workerData":{"n":1},"envX":"y","argv":["a","2"]} | message "echo reject" | error TypeError rejected | exit 1 -1`,
		`ERR_WORKER_PATH The worker script or module filename must be an absolute path or a relative path starting with './' or '../'. Received "w.mjs"`,
		"missing Cannot find module './missing.mjs'",
		"missing exit 1",
	}, "\n")
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// TestWebWorker pins the web's Worker: a worker's global scope is its own,
// what it posts is a message event, close ends it, and its error is an error
// event -- which, handled, goes no further.
func TestWebWorker(t *testing.T) {
	worker := `
		self.onmessage = (e) => {
			if (e.data === "close") { postMessage("closing " + name + " " + (e.target === self)); close(); return; }
			if (e.data === "boom") throw new TypeError("web boom");
			postMessage({ echo: e.data, name, self: self === globalThis, port: e.ports.length });
		};`
	out, _ := run(t, stdlib.Config{Workers: workerFiles(map[string]string{"./web.js": worker})}, `
		(async () => {
			await new Promise((resolve) => {
				const w = new Worker("./web.js", { name: "wkr" });
				let n = 0;
				w.onmessage = (e) => {
					console.log("web", e.constructor.name, JSON.stringify(e.data));
					if (++n === 1) w.postMessage("close"); else resolve();
				};
				w.postMessage(1, [new MessageChannel().port1]);
			});
			const w = new Worker("./web.js");
			w.onerror = (e) => {
				console.log("error", e.constructor.name, e.message, e.error.constructor.name);
				e.preventDefault();
				w.terminate();
			};
			w.postMessage("boom");
			try { new Worker("./web.js", { type: "shared" }) } catch (e) { console.log(e.name, e.message) }
		})();
	`)
	want := strings.Join([]string{
		`web MessageEvent {"echo":1,"name":"wkr","self":true,"port":1}`,
		`web MessageEvent "closing wkr true"`,
		`TypeError Worker constructor: 'shared' is not a valid value for enumeration WorkerType.`,
		"error ErrorEvent web boom TypeError",
	}, "\n")
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// TestWorkerSharing pins what a worker shares with the runtime that started
// it: memory, which one waits on and the other notifies; a port posted to it;
// and a BroadcastChannel's name.
func TestWorkerSharing(t *testing.T) {
	worker := `
		import { parentPort, workerData } from "node:worker_threads";
		const i = new Int32Array(workerData.sab);
		Atomics.store(i, 1, 1);
		Atomics.wait(i, 0, 0);
		parentPort.postMessage("woken " + Atomics.load(i, 0));
		const bc = new BroadcastChannel("news");
		bc.onmessage = (e) => { parentPort.postMessage("bc " + e.data); bc.close(); };
		workerData.port.on("message", (m) => { workerData.port.postMessage("port got " + m); workerData.port.close(); });`
	out, _ := run(t, stdlib.Config{Workers: workerFiles(map[string]string{"./sab.mjs": worker})}, `
		const { Worker } = require_worker_threads;
		const sab = new SharedArrayBuffer(8), i = new Int32Array(sab);
		const { port1, port2 } = new MessageChannel();
		const w = new Worker("./sab.mjs", { workerData: { sab, port: port2 }, transferList: [port2] });
		const bc = new BroadcastChannel("news");
		const got = [];
		const step = () => {
			if (Atomics.load(i, 1) === 1) { Atomics.store(i, 0, 5); Atomics.notify(i, 0); } else setTimeout(step, 1);
		};
		step();
		w.on("message", (m) => {
			got.push(m);
			if (m.startsWith("woken")) { bc.postMessage("hello"); port1.postMessage("ping"); }
		});
		port1.on("message", (m) => got.push(m));
		w.on("exit", (c) => { console.log(got.sort().join(" | "), "exit", c); bc.close(); });
	`)
	if want := "bc hello | port got ping | woken 5 exit 0"; out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// TestWorkerTerminate pins that terminate stops a worker whatever it is
// doing -- here a timer's callback that never returns -- and that a parent
// whose loop closes takes its workers with it.
func TestWorkerTerminate(t *testing.T) {
	worker := `
		import { parentPort } from "node:worker_threads";
		parentPort.postMessage("spinning");
		setTimeout(() => { for (;;) {} }, 0);`
	files := map[string]string{"./spin.mjs": worker}
	out, _ := run(t, stdlib.Config{Workers: workerFiles(files)}, `
		const { Worker } = require_worker_threads;
		const w = new Worker("./spin.mjs");
		w.on("message", async () => { console.log("terminated", await w.terminate(), await w.terminate()); });
	`)
	if want := "terminated 1 undefined"; out != want {
		t.Errorf("got %q, want %q", out, want)
	}

	rt, loop := workerRuntime(t, files)
	if _, err := rt.Eval(`const { Worker } = require_worker_threads; new Worker("./spin.mjs").unref()`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Unref'd, the worker holds nothing, and the loop ends while it spins.
	if err := loop.Run(ctx); err != nil {
		t.Fatal(err)
	}
	loop.Close()
}

// TestWorkerOutlivesParent pins that a parent closed while its worker is
// still posting to it takes the worker with it, and that what the worker
// posts meanwhile goes nowhere rather than racing the close.
func TestWorkerOutlivesParent(t *testing.T) {
	worker := `
		import { parentPort } from "node:worker_threads";
		const spam = () => { parentPort.postMessage(new Uint8Array(64)); setTimeout(spam, 0); };
		spam();`
	for i := 0; i < 5; i++ {
		rt, loop := workerRuntime(t, map[string]string{"./spam.mjs": worker})
		if _, err := rt.Eval(`const { Worker } = require_worker_threads;
			new Worker("./spam.mjs").on("message", () => {})`); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		loop.Run(ctx)
		cancel()
		loop.Close()
		rt.Close()
	}
	// The workers see their loops closed, and end, while this one waits.
	time.Sleep(50 * time.Millisecond)
}

// workerRuntime is a runtime that may start workers loaded from files.
func workerRuntime(t *testing.T, files map[string]string) (*quickjs.Runtime, *stdlib.Loop) {
	t.Helper()
	rt := quickjs.New()
	t.Cleanup(func() { rt.Close() })
	loop := stdlib.NewLoop(rt)
	if err := stdlib.Install(rt, stdlib.Config{Loop: loop, Workers: workerFiles(files)}); err != nil {
		t.Fatal(err)
	}
	if err := requireWorkerThreads(rt); err != nil {
		t.Fatal(err)
	}
	return rt, loop
}

// requireWorkerThreads puts node:worker_threads where a script can reach it,
// as require_worker_threads.
func requireWorkerThreads(rt *quickjs.Runtime) error {
	_, err := rt.EvalModule("<require>", `import * as w from "node:worker_threads"; globalThis.require_worker_threads = w;`)
	if err == nil {
		err = rt.RunJobs()
	}
	return err
}

// TestWorkersRefused pins that a runtime not given Workers has no Worker,
// and that node:worker_threads', which is there for the rest of the module,
// refuses to start one.
func TestWorkersRefused(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		console.log(typeof Worker, typeof require_worker_threads.Worker, require_worker_threads.isMainThread)
		try { new require_worker_threads.Worker("./w.mjs") } catch (e) { console.log(e.message) }
	`)
	want := "undefined function true\nthis runtime may not start workers"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// TestLoopDeadlineStopsACallback pins that the context a loop runs under
// stops a callback that never returns, not only the waiting between
// callbacks: qjs's --timeout and Ctrl-C could not stop one (KI-26). The
// runtime is usable after, and a later Run has its own context.
func TestLoopDeadlineStopsACallback(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	loop := stdlib.NewLoop(rt)
	if err := stdlib.Install(rt, stdlib.Config{Loop: loop}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(`setTimeout(() => { for (;;) {} })`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := loop.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want the deadline", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("stopped after %v", d)
	}
	if _, err := rt.Eval(`var later = 0; setTimeout(() => { later = 1 })`); err != nil {
		t.Fatal(err)
	}
	if err := loop.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, _ := rt.Eval(`later`); v.Int() != 1 {
		t.Errorf("later = %v", v)
	}
}

// TestWorkerTerminateStopsAHostCall pins that terminate stops a worker waiting
// in a host function -- here a program it runs, which is killed -- and that
// the worker neither catches that nor posts anything after: the program ran
// to its end, and the worker on (KI-20).
func TestWorkerTerminateStopsAHostCall(t *testing.T) {
	sleep := `cp.execFileSync("sleep", ["30"])`
	if runtime.GOOS == "windows" {
		sleep = `cp.execFileSync("ping", ["-n", "30", "127.0.0.1"])`
	}
	worker := `
		import { parentPort } from "node:worker_threads";
		const cp = (await import("child_process")).default;
		parentPort.postMessage("sleeping");
		try { ` + sleep + ` } catch (e) { parentPort.postMessage("caught " + e.message) }
		parentPort.postMessage("after");`
	cfg := stdlib.Config{
		Workers: workerFiles(map[string]string{"./sleep.mjs": worker}),
		Run:     &stdlib.Run{Allow: func(string, []string) error { return nil }},
	}
	start := time.Now()
	out, _ := run(t, cfg, `
		const w = new require_worker_threads.Worker("./sleep.mjs");
		w.on("message", (m) => { console.log(m); if (m === "sleeping") w.terminate(); });
		w.on("exit", (code) => console.log("exit", code));
	`)
	if want := "sleeping\nexit 1"; out != want {
		t.Errorf("got %q, want %q", out, want)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("terminated after %v", d)
	}
}

// TestWebWorkerClose pins that a web worker's close ends it once what is
// running returns, as the HTML standard has it: the rest of the script runs,
// what it posts is delivered, and a timer set before is not run.
func TestWebWorkerClose(t *testing.T) {
	worker := `
		setTimeout(() => postMessage("timer"), 0);
		postMessage("before");
		close();
		postMessage("after close");`
	out, _ := run(t, stdlib.Config{Workers: workerFiles(map[string]string{"./close.js": worker})}, `
		new Worker("./close.js").onmessage = (e) => console.log(e.data);
	`)
	if want := "before\nafter close"; out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// TestLoopCloseEndsRun pins that closing a loop ends its Run, even one a
// worker is holding: the worker's notice that it has ended is dropped by the
// closed loop, and Run waited for it until its own context ended (KI-21).
func TestLoopCloseEndsRun(t *testing.T) {
	rt, loop := workerRuntime(t, map[string]string{"./spin.mjs": `for (;;) {}`})
	if _, err := rt.Eval(`new require_worker_threads.Worker("./spin.mjs")`); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(100*time.Millisecond, loop.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	if err := loop.Run(ctx); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Run ended after %v", d)
	}
}
