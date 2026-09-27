package stdlib

import (
	"context"
	"errors"
	"io"
	"maps"
	"sync"
	"sync/atomic"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostjobs"
)

// Workers lets a script start workers -- node:worker_threads' Worker, and the
// web's -- each a runtime of its own, running on a goroutine of its own with a
// Loop of its own, which the script talks to by posting messages.
//
// A worker is installed with the same Config as the runtime that starts it:
// it can reach what its parent can, and nothing else. Its process.exit ends
// the worker rather than the program, and it has no stdin.
type Workers struct {
	// New makes a worker's runtime, as the host makes its own: with the
	// same options, and the same module loader. Nil makes one with
	// quickjs.New, which can import nothing but the modules installed here.
	New func() (*quickjs.Runtime, error)

	// Installed is called with a worker's runtime once everything the
	// Config asks for is installed in it, for the host to add what it adds to
	// its own. Nil adds nothing.
	Installed func(rt *quickjs.Runtime) error

	// Load reads a worker's code: the file or URL a script named, as it
	// named it. It returns the source, the name to evaluate it under -- the
	// resolved path, which the module loader resolves the worker's own
	// imports against -- and whether it is a module. Nil loads nothing, and
	// only code passed with eval can start a worker.
	Load func(specifier string) (source, name string, module bool, err error)
}

// threadIDs numbers the workers the process starts, as node's threadId does;
// the program itself is thread 0.
var threadIDs atomic.Int64

// workerContext is what a worker's runtime knows of itself.
type workerContext struct {
	threadID int
	name     string
	// web marks a worker the web's Worker started, whose global scope is a
	// worker's: self, postMessage, onmessage and close.
	web bool
	// port is the worker's end of the channel to its parent.
	port *portCore
	// data is workerData and the environment data, serialized, and the
	// ports posted with them.
	data  any
	ports []*portCore
	// m is the worker's messaging, once it is installed.
	m *messaging
	// exit ends the worker, with a code: process.exit, and close.
	exit func(code int)
}

// workerHost is a runtime's workers: those it started, and what it needs to
// start more.
type workerHost struct {
	rt   *quickjs.Runtime
	cfg  Config
	m    *messaging
	loop *Loop
	// self is the runtime's own place, if it is a worker.
	self *workerContext
	// event is the script's function that tells a Worker object what its
	// worker said.
	event quickjs.Value

	mu      sync.Mutex
	workers map[*workerHandle]bool
}

// workerHandle is a worker, as the runtime that started it holds it.
type workerHandle struct {
	host *workerHost
	obj  quickjs.Value
	// reffed is whether the worker keeps its parent's loop running, which it
	// does until it is unref'd or ends; holding, whether it is doing so.
	reffed, holding, ended bool
	// stop ends the worker: terminate, or the parent's loop closing.
	stop context.CancelFunc

	// code is the exit code the worker was ended with, by process.exit or by
	// being terminated, before it ended of itself.
	mu     sync.Mutex
	code   int
	coded  bool
	thread int
}

// installWorkers installs node:worker_threads -- whose Worker starts one only
// if the Config has Workers -- and, where it has, the web's Worker; and in a
// worker's runtime, what the worker's code finds: its parentPort, its
// workerData, and a web worker's global scope.
func installWorkers(rt *quickjs.Runtime, cfg Config, m *messaging, events quickjs.Value) error {
	h := &workerHost{rt: rt, cfg: cfg, m: m, loop: cfg.Loop, self: cfg.worker, workers: map[*workerHandle]bool{}}
	if cfg.Workers != nil && cfg.Loop != nil {
		// A runtime whose loop is closed has no use for its workers.
		context.AfterFunc(cfg.Loop.Context(), h.terminateAll)
	}
	host := rt.NewObject()
	if err := setAll(host, map[string]any{
		"EventEmitter":         events,
		"receiveMessageOnPort": m.receive,
		"bindWorkers": func(event quickjs.Value) {
			h.event = event
		},
		"info":      h.info,
		"start":     h.start,
		"terminate": func(w quickjs.Value) { h.handle(w, func(wh *workerHandle) { wh.terminate(1) }) },
		"ref": func(w quickjs.Value, ref bool) {
			h.handle(w, func(wh *workerHandle) {
				wh.reffed = ref
				wh.hold()
			})
		},
		"exit": func(code int) {
			if h.self != nil {
				h.self.exit(code)
			}
		},
	}); err != nil {
		return err
	}
	api, err := evalWithHost(rt, "<worker_threads>", workerJS, host)
	if err != nil {
		return err
	}
	threads, err := api.Get("worker_threads")
	if err != nil {
		return err
	}
	exports, err := moduleExports(rt, threads)
	if err != nil {
		return err
	}
	if err := rt.SetModuleValues("worker_threads", exports); err != nil {
		return err
	}
	if err := rt.SetModuleValues("node:worker_threads", exports); err != nil {
		return err
	}
	globals := []string{"ErrorEvent"}
	if cfg.Workers != nil && cfg.Loop != nil {
		globals = append(globals, "Worker")
	}
	for _, name := range globals {
		v, err := api.Get(name)
		if err != nil {
			return err
		}
		if err := rt.Set(name, v); err != nil {
			return err
		}
	}
	return nil
}

// info is what the script needs to know of where it runs: whether it is a
// worker, and if so the port to its parent and the data it was given.
func (h *workerHost) info() (quickjs.Value, error) {
	o := h.rt.NewObject()
	w := h.self
	if w == nil {
		return o, setAll(o, map[string]any{"isMainThread": true, "threadId": 0})
	}
	port, err := h.m.adopt(w.port)
	if err != nil {
		return quickjs.Value{}, err
	}
	data, _, err := h.m.deserialize(w.data, w.ports)
	if err != nil {
		return quickjs.Value{}, err
	}
	return o, setAll(o, map[string]any{
		"isMainThread": false, "threadId": w.threadID, "parentPort": port,
		"data": data, "web": w.web, "name": w.name,
	})
}

// handle finds the worker a Worker object stands for.
func (h *workerHost) handle(obj quickjs.Value, fn func(*workerHandle)) {
	h.mu.Lock()
	var found *workerHandle
	for wh := range h.workers {
		if wh.obj.StrictEqual(obj) {
			found = wh
			break
		}
	}
	h.mu.Unlock()
	if found != nil {
		fn(found)
	}
}

// workerOptions is what the script passes to start a worker.
type workerOptions struct {
	Eval   bool              `js:"eval"`
	Argv   []string          `js:"argv"`
	Env    map[string]string `js:"env"`
	HasEnv bool              `js:"hasEnv"`
	Name   string            `js:"name"`
	Web    bool              `js:"web"`
	// Type is a web worker's: "module" or "classic"; or "" for node's,
	// whose code says which it is.
	Type string `js:"type"`
}

// start starts a worker for obj, the script's Worker: the code a specifier
// names, or the code itself; given data -- workerData and the environment
// data -- with transfer transferred. It returns the port to the worker and
// the worker's thread id.
func (h *workerHost) start(obj quickjs.Value, specifier string, data quickjs.Value,
	transfer []quickjs.Value, opts quickjs.Value) (quickjs.Value, error) {
	if h.cfg.Workers == nil || h.loop == nil {
		return quickjs.Value{}, h.rt.Throw(h.rt.NewError("Error", "this runtime may not start workers"))
	}
	var o workerOptions
	if err := opts.Decode(&o); err != nil {
		return quickjs.Value{}, err
	}
	serialized, ports, err := h.m.serialize(data, transfer, nil)
	if err != nil {
		return quickjs.Value{}, err
	}
	parent, child := &portCore{}, &portCore{}
	parent.peer, child.peer = child, parent
	end, err := h.m.adoptEnd(parent)
	if err != nil {
		return quickjs.Value{}, err
	}

	ctx, stop := context.WithCancel(context.Background())
	wh := &workerHandle{host: h, obj: obj, reffed: true, stop: stop, thread: int(threadIDs.Add(1))}
	end.worker = wh
	// The port to a worker dispatches at once; the worker, not the port,
	// is what holds the loop.
	end.reffed = false
	end.start()
	h.mu.Lock()
	h.workers[wh] = true
	h.mu.Unlock()
	wh.hold()

	w := &workerContext{threadID: wh.thread, name: o.Name, web: o.Web, port: child, data: serialized, ports: ports}
	w.exit = func(code int) { wh.terminate(code) }
	cfg := h.childConfig(w, specifier, o)
	go runWorker(ctx, cfg, w, wh, specifier, o)

	result := h.rt.NewObject()
	return result, setAll(result, map[string]any{"port": end.obj, "threadId": wh.thread})
}

// childConfig is the Config a worker is installed with: its parent's, with
// what belongs to a runtime -- the loop, the process's exit and stdin -- its
// own.
func (h *workerHost) childConfig(w *workerContext, specifier string, o workerOptions) Config {
	c := h.cfg
	c.worker = w
	c.Loop = nil
	if h.cfg.Process != nil {
		p := *h.cfg.Process
		p.Stdin = nil
		p.Exit, p.worker = w.exit, true
		script := specifier
		if o.Eval {
			script = "[worker eval]"
		}
		exe := "qjs"
		if len(p.Args) > 0 {
			exe = p.Args[0]
		}
		p.Args = append([]string{exe, script}, o.Argv...)
		if o.HasEnv {
			p.Env = o.Env
		} else {
			p.Env = maps.Clone(p.Env)
		}
		c.Process = &p
	}
	return c
}

// bindLoop gives a worker's Config its own copies of what is bound to a
// loop, bound to the worker's.
func (c *Config) bindLoop(loop *Loop) {
	c.Loop = loop
	if c.FS != nil {
		fs := *c.FS
		fs.Loop = loop
		c.FS = &fs
	}
	if c.Fetch != nil {
		f := *c.Fetch
		f.Loop = loop
		c.Fetch = &f
	}
	if c.Run != nil {
		r := *c.Run
		r.Loop = loop
		c.Run = &r
	}
	if c.Serve != nil {
		s := *c.Serve
		s.Loop, s.upgrade = loop, nil
		c.Serve = &s
	}
	if c.Sockets != nil {
		ws := *c.Sockets
		ws.Loop = loop
		if ws.Serve != nil {
			ws.Serve = c.Serve
		}
		c.Sockets = &ws
	}
}

// hold keeps the parent's loop running while the worker runs and is ref'd.
func (wh *workerHandle) hold() {
	want := wh.reffed && !wh.ended
	if want == wh.holding {
		return
	}
	wh.holding = want
	if want {
		wh.host.loop.Begin()
	} else {
		wh.host.loop.Done()
	}
}

// terminate ends the worker with code, unless it has already ended or been
// given one. It is safe to call from any goroutine.
func (wh *workerHandle) terminate(code int) {
	wh.mu.Lock()
	if !wh.coded {
		wh.code, wh.coded = code, true
	}
	wh.mu.Unlock()
	wh.stop()
}

// exitCode is the code the worker was ended with, if it was.
func (wh *workerHandle) exitCode() (int, bool) {
	wh.mu.Lock()
	defer wh.mu.Unlock()
	return wh.code, wh.coded
}

// terminateAll ends every worker the runtime started.
func (h *workerHost) terminateAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for wh := range h.workers {
		wh.terminate(1)
	}
}

// online, failed and exited are what the worker said, heard in its parent.

func (wh *workerHandle) online() {
	wh.host.raise(wh.host.event.Call(wh.obj, "online"))
}

func (wh *workerHandle) failed(data any) {
	h := wh.host
	var v quickjs.Value
	if err, ok := data.(error); ok {
		v = h.rt.NewError("Error", err.Error())
	} else {
		var err error
		if v, _, err = h.m.deserialize(data, nil); err != nil {
			v = h.rt.NewError("Error", "the worker failed, with an error that could not be cloned")
		}
	}
	h.raise(h.event.Call(wh.obj, "error", v))
}

func (wh *workerHandle) exited(code int) {
	h := wh.host
	wh.ended = true
	wh.hold()
	h.mu.Lock()
	delete(h.workers, wh)
	h.mu.Unlock()
	h.raise(h.event.Call(wh.obj, "exit", code))
}

// raise makes an exception a Worker's listener threw uncaught.
func (h *workerHost) raise(_ quickjs.Value, err error) {
	if err != nil && h.loop != nil {
		h.loop.fail(err)
	}
}

// --- The worker's goroutine -----------------------------------------------------

// uncaughtRejection is a promise rejected with nothing to handle it, which
// ends a worker as an uncaught exception does.
type uncaughtRejection struct{ reason quickjs.Value }

func (u *uncaughtRejection) Error() string { return "uncaught (in promise) " + u.reason.String() }

// runWorker is a worker's goroutine: it makes the worker's runtime, installs
// it, runs its code and then its loop, and tells the parent how it went --
// ending, whatever happened, by telling it the worker has.
func runWorker(ctx context.Context, cfg Config, w *workerContext, wh *workerHandle, specifier string, o workerOptions) {
	code := 0
	var fail error
	var rt *quickjs.Runtime
	defer func() {
		if c, ok := wh.exitCode(); ok {
			// Ended by process.exit or terminate, whose code it is, and
			// whatever was running when it was stopped is no failure.
			code, fail = c, nil
		}
		if fail != nil {
			code = 1
			w.port.send(portMsg{kind: msgError, data: w.errorData(rt, fail)})
		}
		if w.m != nil {
			w.m.shutdown(w.port)
		}
		if cfg.Loop != nil {
			cfg.Loop.Close()
		}
		if rt != nil {
			rt.Close()
		}
		w.port.disentangle(code)
	}()

	var err error
	if cfg.Workers != nil && cfg.Workers.New != nil {
		rt, err = cfg.Workers.New()
	} else {
		rt = quickjs.New()
	}
	if err != nil {
		fail = err
		return
	}
	if rt == nil {
		fail = errors.New("the host made no runtime for the worker")
		return
	}
	loop := NewLoop(rt)
	cfg.bindLoop(loop)
	hostjobs.Abort(rt, ctx.Done())
	if err := Install(rt, cfg); err != nil {
		fail = err
		return
	}
	// A rejection nothing handles ends the worker, as in node.
	rt.OnUnhandledRejection(func(reason quickjs.Value) { loop.fail(&uncaughtRejection{reason}) })
	if cfg.Workers != nil && cfg.Workers.Installed != nil {
		if err := cfg.Workers.Installed(rt); err != nil {
			fail = err
			return
		}
	}

	src, name, module := specifier, "[worker eval]", false
	switch {
	case o.Eval:
		// Code that parses only as a module is one.
		module = o.Type == "module" || rt.CheckSyntax(src) != nil && rt.CheckModuleSyntax(src) == nil
	case cfg.Workers == nil || cfg.Workers.Load == nil:
		fail = errors.New("this runtime cannot load a worker's code, only run code passed with eval")
		return
	default:
		if src, name, module, err = cfg.Workers.Load(specifier); err != nil {
			fail = err
			return
		}
		switch o.Type {
		case "module":
			module = true
		case "classic":
			module = false
		}
	}

	w.port.send(portMsg{kind: msgOnline})
	if module {
		_, err = rt.EvalModuleContext(ctx, name, src)
	} else {
		_, err = rt.EvalFileContext(ctx, name, src)
	}
	if err == nil {
		err = loop.Run(ctx)
	}
	fail = err
}

// errorData is what a worker failed with, serialized for its parent: the
// exception a script threw, cloned, or failing that the error as a message.
func (w *workerContext) errorData(rt *quickjs.Runtime, err error) any {
	var v quickjs.Value
	var jsErr *quickjs.Error
	var rejection *uncaughtRejection
	switch {
	case w.m == nil || rt == nil:
		return err
	case errors.As(err, &jsErr):
		v = jsErr.Value()
	case errors.As(err, &rejection):
		v = rejection.reason
	default:
		return err
	}
	data, _, serr := w.m.serialize(v, nil, nil)
	if serr != nil {

		return errors.New(v.String())
	}
	return data
}

// syncWriter is a writer shared by a program and its workers, which write to
// it from goroutines of their own: one write at a time.
type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (s syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// shareWriters makes the writers of a Config that starts workers safe for
// them to share: each writer, however many fields name it, gets one lock.
func (c *Config) shareWriters() {
	locks := map[io.Writer]*sync.Mutex{}
	wrap := func(w io.Writer) io.Writer {
		if w == nil {
			return nil
		}
		if _, ok := w.(syncWriter); ok {
			return w
		}
		mu := locks[w]
		if mu == nil {
			mu = &sync.Mutex{}
			locks[w] = mu
		}
		return syncWriter{mu: mu, w: w}
	}
	c.Stdout, c.Stderr = wrap(c.Stdout), wrap(c.Stderr)
	if c.Process != nil {
		p := *c.Process
		p.Stdout, p.Stderr = wrap(p.Stdout), wrap(p.Stderr)
		c.Process = &p
	}
}

// errNoLoop is Workers without a Loop to run the workers' messages on.
var errNoLoop = errors.New("stdlib: Workers needs a Loop")
