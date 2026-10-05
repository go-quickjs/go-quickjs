package vm

import (
	"context"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Fetching modules through an asynchronous loader.
//
// A ModuleLoader answers before it returns, which holds the runtime up for as
// long as a fetch takes. An AsyncModuleLoader answers later, from any
// goroutine, and the modules of a graph are asked for all at once -- as many
// at a time as the runtime's limit allows, the rest waiting in turn.
//
// What a module graph needs is fetched before it is linked: each answer is
// compiled on the runtime's goroutine, the modules it requests are asked for
// in turn, and what each request resolved to is kept in the realm's requested
// map -- where linking, evaluating and resolvedNameOf then find everything,
// as they find what a ModuleLoader loaded, without asking again.
//
// An import() fetches without holding the runtime: its graph's answers are
// posted to the runtime's goroutine as host work, the script and its jobs run
// meanwhile, and the import goes on when the last of them is in. EvalModule,
// RequireModule and the linking of a graph whose modules are not in hand
// return their module, and so wait -- running nothing else, as they would
// while a ModuleLoader read -- with the fetches still made at once.

// AsyncModuleLoader fetches a module's source without answering before it
// returns: it calls done once, from any goroutine, with what a ModuleLoader
// returns. It is called on the runtime's goroutine. ctx ends when the module
// is no longer wanted, after which done need not be called.
type AsyncModuleLoader func(ctx context.Context, specifier, referrer string, done func(source, resolved string, err error))

// moduleFetch is a runtime's asynchronous loader, the calls of it in flight,
// and the hook that makes a loader's error the exception it stands for.
type moduleFetch struct {
	loader AsyncModuleLoader
	limit  int
	// ctx is the runtime's lifetime, which every fetch's context is cut
	// from: a fetch for an import() outlives the call that started it.
	ctx context.Context
	// errHook makes a loader's error -- a ModuleLoader's or an
	// AsyncModuleLoader's -- into the exception it stands for, on the
	// runtime's goroutine.
	errHook func(error) error

	// queue holds the calls waiting for one in flight to come back, in the
	// order they were asked for. Only the runtime's goroutine touches it.
	queue []*fetchCall
	// mu guards inFlight, which done changes from any goroutine.
	mu       sync.Mutex
	inFlight int
	// slot has a value when a call has come back, for a fetch waiting on the
	// runtime's goroutine to start the next one queued.
	slot chan struct{}
}

// fetchCall is one call of the loader: a request of a referrer, for a graph.
type fetchCall struct {
	g                   *graphFetch
	key                 [2]string
	specifier, referrer string
	typ                 string
}

// fetchResult is what a call came back with.
type fetchResult struct {
	call             *fetchCall
	source, resolved string
	err              error
}

// moduleFetching returns the runtime's moduleFetch, made when first wanted.
func (r *Runtime) moduleFetching() *moduleFetch {
	if r.modfetch == nil {
		r.modfetch = &moduleFetch{limit: 1, slot: make(chan struct{}, 1)}
	}
	return r.modfetch
}

// SetAsyncModuleLoader installs an asynchronous loader in place of any loader,
// with at most limit calls of it outstanding at once -- one, for a limit below
// one -- and the runtime's lifetime, which ends every call's context. A nil
// loader removes it.
func (r *Runtime) SetAsyncModuleLoader(fn AsyncModuleLoader, limit int, lifetime context.Context) {
	f := r.moduleFetching()
	f.loader, f.limit, f.ctx = fn, max(limit, 1), lifetime
	r.moduleLoader = nil
	// What the old loader resolved requests to is the old loader's.
	r.requested = nil
}

// SetLoaderErrorHook installs what makes a module loader's error the
// exception it stands for, before loaderError decides what to throw.
func (r *Runtime) SetLoaderErrorHook(fn func(error) error) {
	r.moduleFetching().errHook = fn
}

// asyncLoader is the runtime's asynchronous loader, or nil.
func (r *Runtime) asyncLoader() AsyncModuleLoader {
	if r.modfetch == nil {
		return nil
	}
	return r.modfetch.loader
}

// graphFetch is the fetching of what one module graph needs.
type graphFetch struct {
	r *Runtime
	// ctx is the calls' context, which cancel ends when the fetch finishes:
	// a call still out then is no longer wanted.
	ctx    context.Context
	cancel context.CancelFunc
	// pending counts the calls asked for and not yet received.
	pending int
	// err is the first failure, after which nothing more is asked for and
	// what comes back is dropped; done marks a fetch that has finished.
	err  error
	done bool
	// seen are the modules whose requests have been asked for, asked the
	// requests asked for.
	seen  map[*Module]bool
	asked map[[2]string]bool
	// deliver hands what a call came back with to the runtime's goroutine,
	// where receive takes it. It may be called on any goroutine.
	deliver func(fetchResult)
}

func (r *Runtime) newGraphFetch() *graphFetch {
	parent := r.modfetch.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &graphFetch{r: r, ctx: ctx, cancel: cancel, seen: map[*Module]bool{}, asked: map[[2]string]bool{}}
}

// want asks for the module a request names from referrer, and then for what
// that module requests, unless it is in hand.
func (g *graphFetch) want(request, referrer string) {
	r := g.r
	request, _ = bytecode.SplitDeferRequest(request)
	request, _ = bytecode.SplitSourceRequest(request)
	specifier, typ := bytecode.SplitModuleRequest(request)
	if typ == "" && r.nativeModule(specifier) != nil {
		return
	}
	key := [2]string{referrer, request}
	if m, ok := r.requested[key]; ok {
		if typ == "" {
			g.visit(m)
		}
		return
	}
	if g.asked[key] || g.err != nil {
		return
	}
	g.asked[key] = true
	g.pending++
	f := r.modfetch
	f.queue = append(f.queue, &fetchCall{g: g, key: key, specifier: specifier, referrer: referrer, typ: typ})
	r.pumpFetches()
}

// visit asks for what a module requests that is not in hand. A module linked
// already has its graph in hand.
func (g *graphFetch) visit(m *Module) {
	if g.seen[m] || m.state != ModuleUnlinked {
		return
	}
	g.seen[m] = true
	for _, request := range m.requests {
		g.want(request, m.Specifier)
	}
}

// pumpFetches calls the loader for the calls queued, while fewer than the
// limit are in flight. A call for a fetch that has finished -- failed -- is
// dropped. It runs on the runtime's goroutine.
func (r *Runtime) pumpFetches() {
	f := r.modfetch
	for len(f.queue) > 0 {
		c := f.queue[0]
		if c.g.done {
			f.queue[0] = nil
			f.queue = f.queue[1:]
			continue
		}
		f.mu.Lock()
		if f.inFlight >= f.limit {
			f.mu.Unlock()
			return
		}
		f.inFlight++
		f.mu.Unlock()
		f.queue[0] = nil
		f.queue = f.queue[1:]
		// The call's place is given back when done is called, or when its
		// context ends first: a loader that stops then need not answer.
		var once sync.Once
		release := func() {
			f.mu.Lock()
			f.inFlight--
			f.mu.Unlock()
			select {
			case f.slot <- struct{}{}:
			default:
			}
		}
		stop := context.AfterFunc(c.g.ctx, func() { once.Do(release) })
		f.loader(c.g.ctx, c.specifier, c.referrer, func(source, resolved string, err error) {
			stop()
			answered := false
			once.Do(func() {
				release()
				answered = true
			})
			if answered {
				c.g.deliver(fetchResult{call: c, source: source, resolved: resolved, err: err})
			}
		})
	}
}

// receive takes what a call came back with, on the runtime's goroutine: the
// module it is, compiled, kept as what its request resolved to, and its own
// requests asked for.
func (g *graphFetch) receive(res fetchResult) {
	r := g.r
	g.pending--
	// A call has come back, so another queued may go out.
	defer r.pumpFetches()
	if g.err != nil {
		return
	}
	c := res.call
	if res.err != nil {
		g.err = r.loaderError(c.specifier, res.err)
		return
	}
	var m *Module
	var err error
	switch {
	case c.typ != "":
		m, err = r.makeTypedModule(c.specifier, c.typ, res.source, res.resolved)
	default:
		var ok bool
		if m, ok = r.modules[res.resolved]; !ok {
			if r.compileModule == nil {
				err = r.throwError(errType, "this runtime cannot compile modules")
			} else {
				m, err = r.compileModule(res.resolved, res.source)
			}
		}
	}
	if err != nil {
		g.err = err
		return
	}
	if r.requested == nil {
		r.requested = map[[2]string]*Module{}
	}
	r.requested[c.key] = m
	if c.typ == "" {
		g.visit(m)
	}
}

// fetchNow fetches what a graph needs, from where start begins it, and waits
// for it on the runtime's goroutine, running nothing else -- only starting
// the loader's calls as earlier ones come back. It stops early if the
// runtime's context ends.
func (r *Runtime) fetchNow(start func(g *graphFetch)) error {
	results := &resultQueue{ready: make(chan struct{}, 1)}
	g := r.newGraphFetch()
	g.deliver = results.push
	start(g)
	ctx := r.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	for g.pending > 0 && g.err == nil {
		if res, ok := results.pop(); ok {
			g.receive(res)
			continue
		}
		select {
		case <-results.ready:
		case <-r.modfetch.slot:
			r.pumpFetches()
		case <-ctx.Done():
			g.err = ctx.Err()
		}
	}
	g.done = true
	g.cancel()
	return g.err
}

// fetchThen fetches what a request's graph needs from referrer without
// waiting for it, and calls then on the runtime's goroutine, in the realm it
// was asked in, once all of it is in hand -- at once, if it is -- or once a
// call has failed. The fetch keeps the runtime busy until then.
func (r *Runtime) fetchThen(request, referrer string, then func(error)) {
	w := r.StartHostWork()
	realm := r.Realm
	g := r.newGraphFetch()
	finish := func() {
		g.done = true
		g.cancel()
		w.Done()
		then(g.err)
	}
	g.deliver = func(res fetchResult) {
		w.Post(func() {
			if g.done {
				return
			}
			r.InRealm(realm, func() {
				g.receive(res)
				if g.pending == 0 || g.err != nil {
					finish()
				}
			})
		}, nil)
	}
	g.want(request, referrer)
	if g.pending == 0 || g.err != nil {
		finish()
	}
}

// whenFetched calls then once what a request's graph needs from referrer is
// in hand: at once with a ModuleLoader, which loads as it links, and once the
// asynchronous loader has fetched it with one.
func (r *Runtime) whenFetched(request, referrer string, then func(error)) {
	if r.asyncLoader() == nil {
		then(nil)
		return
	}
	r.fetchThen(request, referrer, then)
}

// fetchedDependency is loadDependency with an asynchronous loader: what the
// request resolved to when it was fetched -- now, waiting for it, if it has
// not been.
func (r *Runtime) fetchedDependency(request, specifier, typ, referrer string) (*Module, error) {
	if typ == "" {
		if m := r.nativeModule(specifier); m != nil {
			return m, nil
		}
	}
	key := [2]string{referrer, request}
	if m, ok := r.requested[key]; ok {
		return m, nil
	}
	if err := r.fetchNow(func(g *graphFetch) { g.want(request, referrer) }); err != nil {
		return nil, err
	}
	if m, ok := r.requested[key]; ok {
		return m, nil
	}
	return nil, r.throwTypeError("cannot import %q", specifier)
}

// resultQueue carries what the loader's calls come back with, from any
// goroutine, to a fetch waiting on the runtime's.
type resultQueue struct {
	mu    sync.Mutex
	items []fetchResult
	ready chan struct{}
}

func (q *resultQueue) push(res fetchResult) {
	q.mu.Lock()
	q.items = append(q.items, res)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *resultQueue) pop() (fetchResult, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return fetchResult{}, false
	}
	res := q.items[0]
	q.items[0] = fetchResult{}
	q.items = q.items[1:]
	return res, true
}
