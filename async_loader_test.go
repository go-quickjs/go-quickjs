package quickjs_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// fetchCounter is an AsyncModuleLoader over a map of sources that answers
// from goroutines of its own, after delay, and counts the calls it has out at
// once.
type fetchCounter struct {
	sources map[string]string
	delay   func(specifier string) time.Duration

	mu          sync.Mutex
	out, maxOut int
	calls       []string
}

func (f *fetchCounter) load(ctx context.Context, req quickjs.ModuleRequest, done func(quickjs.LoadedModule, error)) {
	specifier := req.Specifier
	resolved := "/" + strings.TrimPrefix(specifier, "./")
	f.mu.Lock()
	f.out++
	f.maxOut = max(f.maxOut, f.out)
	f.calls = append(f.calls, specifier)
	f.mu.Unlock()
	go func() {
		if f.delay != nil {
			time.Sleep(f.delay(specifier))
		}
		f.mu.Lock()
		f.out--
		f.mu.Unlock()
		src, ok := f.sources[resolved]
		if !ok {
			done(quickjs.LoadedModule{}, fmt.Errorf("fetching %s: %w", resolved, fs.ErrNotExist))
			return
		}
		done(quickjs.LoadedModule{Source: src, Resolved: resolved}, nil)
	}()
}

// TestAsyncModuleLoaderGraph pins what EvalModule does with an asynchronous
// loader: a graph's modules are asked for at once, never more than the limit
// at a time, and evaluated in the order the graph names them, whatever order
// they arrive in.
func TestAsyncModuleLoaderGraph(t *testing.T) {
	sources := map[string]string{
		"/a.mjs":  `import "./a1.mjs"; order.push("a");`,
		"/a1.mjs": `order.push("a1");`,
		"/b.mjs":  `order.push("b");`,
		"/c.mjs":  `order.push("c");`,
		"/d.mjs":  `import data from "./d.json" with { type: "json" }; order.push("d " + data.n);`,
		"/d.json": `{"n": 4}`,
	}
	for _, limit := range []int{1, 2, 8} {
		rt := quickjs.New(quickjs.WithModuleFetchLimit(limit))
		f := &fetchCounter{sources: sources, delay: func(s string) time.Duration {
			// The first named arrives last.
			return time.Duration(10-len(s)) * time.Millisecond
		}}
		rt.SetAsyncModuleLoader(f.load)
		rt.Eval(`globalThis.order = []`)
		if _, err := rt.EvalModule("/main.mjs", `import "./a.mjs"; import "./b.mjs"; import "./c.mjs"; import "./d.mjs"; order.push("main");`); err != nil {
			t.Fatal(limit, err)
		}
		order, _ := rt.Eval(`order.join(" ")`)
		if want := "a1 a b c d 4 main"; order.String() != want {
			t.Errorf("limit %d: order %q, want %q", limit, order, want)
		}
		if want := min(limit, 4); f.maxOut != want {
			t.Errorf("limit %d: %d calls out at once, want %d", limit, f.maxOut, want)
		}
		rt.Close()
	}
}

// TestAsyncModuleLoaderImport pins that an import() through an asynchronous
// loader does not hold up the runtime: the script's jobs run while its graph
// is fetched, and the import settles once the graph is in -- or with the
// loader's error.
func TestAsyncModuleLoaderImport(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	release := make(chan struct{})
	rt.SetAsyncModuleLoader(func(ctx context.Context, req quickjs.ModuleRequest, done func(quickjs.LoadedModule, error)) {
		switch req.Specifier {
		case "./slow.mjs":
			go func() {
				<-release
				done(quickjs.LoadedModule{Source: `import {dep} from "./dep.mjs"; export const v = "slow " + dep;`, Resolved: "/slow.mjs"}, nil)
			}()
		case "./dep.mjs":
			// Answered before the loader returns, and a second answer ignored.
			done(quickjs.LoadedModule{Source: `export const dep = "dep";`, Resolved: "/dep.mjs"}, nil)
			done(quickjs.LoadedModule{Source: `export const dep = "twice";`, Resolved: "/dep.mjs"}, nil)
		case "./coded":
			// An error for script is made on the runtime's goroutine, and
			// handed over later.
			e := rt.NewError("Error", "Cannot find module './coded'")
			e.Set("code", "MODULE_NOT_FOUND")
			thrown := rt.Throw(e)
			go done(quickjs.LoadedModule{}, thrown)
		case "./synthetic":
			err := rt.DefineSyntheticModule("/synthetic", []string{"x"}, func() (map[string]any, error) {
				return map[string]any{"x": "synthetic"}, nil
			})
			go done(quickjs.LoadedModule{Resolved: "/synthetic"}, err)
		default:
			go done(quickjs.LoadedModule{}, fmt.Errorf("fetching %s: %w", req.Specifier, fs.ErrNotExist))
		}
	})

	p, err := rt.Eval(`
		globalThis.log = [];
		const describe = e => e.constructor.name + " " + e.code + " " + e.message;
		const all = Promise.all([
			import("./slow.mjs").then(m => log.push("imported " + m.v)),
			import("./coded").catch(e => log.push(describe(e))),
			import("./missing").catch(e => log.push(describe(e))),
			import("./synthetic").then(m => log.push("synthetic " + m.x)),
		]);
		Promise.resolve().then(() => log.push("microtask"));
		all`)
	if err != nil {
		t.Fatal(err)
	}
	// The script ran to its end, and its jobs, with slow.mjs outstanding.
	before, _ := rt.Eval(`log.includes("microtask") && !log.some(l => l.startsWith("imported"))`)
	if !before.Bool() {
		log, _ := rt.Eval(`log.join(" | ")`)
		t.Errorf("before slow.mjs arrived: %s", log)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.Await(ctx); err != nil {
		t.Fatal(err)
	}
	log, _ := rt.Eval(`log.sort().join("\n")`)
	want := strings.Join([]string{
		"Error MODULE_NOT_FOUND Cannot find module './coded'",
		"TypeError undefined cannot resolve \"./missing\": fetching ./missing: file does not exist",
		"imported slow dep",
		"microtask",
		"synthetic synthetic",
	}, "\n")
	if log.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", log, want)
	}

	// From Go, the loader's error is found through the exception.
	if _, err := rt.EvalModule("/m.mjs", `import "./gone"`); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("EvalModule: %v", err)
	}
}

// TestAsyncModuleLoaderSharesTheLimit pins that the limit is the runtime's,
// across an import() fetching in the background and a RequireModule that
// waits: the one waiting starts its call when the other's comes back, rather
// than waiting forever for a slot the background fetch cannot give back
// while the runtime waits.
func TestAsyncModuleLoaderSharesTheLimit(t *testing.T) {
	rt := quickjs.New(quickjs.WithModuleFetchLimit(1))
	defer rt.Close()
	f := &fetchCounter{
		sources: map[string]string{"/bg.mjs": `export const bg = 1;`, "/now.mjs": `export const now = "now";`},
		delay:   func(string) time.Duration { return 20 * time.Millisecond },
	}
	rt.SetAsyncModuleLoader(f.load)
	if _, err := rt.Eval(`globalThis.bg = import("./bg.mjs")`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now, err := rt.RequireModule("./now.mjs", "")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := now.Get("now"); v.String() != "now" {
		t.Errorf("now: %v", v)
	}
	bg, _ := rt.Get("bg")
	if _, err := bg.Await(ctx); err != nil {
		t.Fatal(err)
	}
	if f.maxOut != 1 || strings.Join(f.calls, " ") != "./bg.mjs ./now.mjs" {
		t.Errorf("calls %v, %d out at once", f.calls, f.maxOut)
	}
}

// TestAsyncModuleLoaderClose pins that a runtime closed with a fetch out is
// not troubled by the answer arriving later.
func TestAsyncModuleLoaderClose(t *testing.T) {
	rt := quickjs.New()
	answered := make(chan struct{})
	var done func(quickjs.LoadedModule, error)
	rt.SetAsyncModuleLoader(func(ctx context.Context, req quickjs.ModuleRequest, d func(quickjs.LoadedModule, error)) { done = d })
	if _, err := rt.Eval(`import("./later.mjs")`); err != nil {
		t.Fatal(err)
	}
	rt.Close()
	go func() {
		done(quickjs.LoadedModule{Source: `export {}`, Resolved: "/later.mjs"}, nil)
		close(answered)
	}()
	<-answered
}

// TestAsyncModuleLoaderCancels pins when a call's context ends: when another
// module of its graph fails, when the call waiting for the graph gives up,
// and when the runtime closes -- and that a call that stops then, without
// answering, gives its place under the limit back.
func TestAsyncModuleLoaderCancels(t *testing.T) {
	cancelled := make(chan string, 4)
	loader := func(ctx context.Context, req quickjs.ModuleRequest, done func(quickjs.LoadedModule, error)) {
		switch req.Specifier {
		case "./hangs":
			// Never answers: it waits to be told it is not wanted.
			go func() {
				<-ctx.Done()
				cancelled <- req.Specifier + " " + req.Referrer
			}()
		case "./fails":
			go done(quickjs.LoadedModule{}, errors.New("no such module"))
		default:
			go done(quickjs.LoadedModule{Source: `export const ok = "ok";`, Resolved: "/" + strings.TrimPrefix(req.Specifier, "./")}, nil)
		}
	}
	wait := func(want string) {
		t.Helper()
		select {
		case got := <-cancelled:
			if got != want {
				t.Errorf("cancelled %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was not cancelled", want)
		}
	}

	// A module of the graph failed: the one still out is not wanted.
	rt := quickjs.New(quickjs.WithModuleFetchLimit(2))
	rt.SetAsyncModuleLoader(loader)
	if _, err := rt.EvalModule("/a.mjs", `import "./hangs"; import "./fails";`); err == nil {
		t.Error("a graph with a module that failed loaded")
	}
	wait("./hangs /a.mjs")
	rt.Close()

	// The call waiting for the graph gave up; with a limit of one, the
	// place the call held is given back for the next.
	rt = quickjs.New(quickjs.WithModuleFetchLimit(1))
	rt.SetAsyncModuleLoader(loader)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := rt.EvalModuleContext(ctx, "/b.mjs", `import "./hangs";`)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("EvalModuleContext: %v", err)
	}
	wait("./hangs /b.mjs")
	ns, err := rt.RequireModule("./next.mjs", "")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := ns.Get("ok"); v.String() != "ok" {
		t.Errorf("next: %v", v)
	}

	// The runtime closed with an import() fetching.
	if _, err := rt.Eval(`import("./hangs")`); err != nil {
		t.Fatal(err)
	}
	rt.Close()
	wait("./hangs ")
}
