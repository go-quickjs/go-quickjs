# Building a host

## Work that finishes on other goroutines

A runtime belongs to one goroutine, but a host's timers, reads and requests
finish on others. `AsyncWork` is how their results come back: it is Node-API's
async work, and Deno's op. Work is started on the runtime's goroutine, results
are posted to it from any goroutine, and every function posted is called
exactly once:

- with `nil`, on the runtime's goroutine, when the runtime next runs its jobs,
  in the order posted, each followed by the microtasks it queued;
- with `ErrClosed` if the runtime closed first, which is where the work
  releases what it holds;
- with `ErrWorkDone` if the work had already ended.

Nothing else drops one: not a loop that stops, not an interrupted script, not
another function that panics. A timer, and a whole event loop:

```go
rt.Set("setTimeout", func(cb quickjs.Value, ms int) {
    w := rt.StartAsyncWork() // keeps the runtime busy until it ends
    time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
        w.Complete(func(err error) { // Post, then Done
            if err == nil {
                cb.Call()
            }
        })
    })
})

for rt.Busy() {
    select {
    case <-rt.Wake():
        if err := rt.RunJobsContext(ctx); err != nil {
            return err
        }
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

Work with one result that settles a promise -- a query, a request -- is
what `rt.Go` does with an `AsyncWork` (see [Async functions written in
Go](embedding.md#async-functions-written-in-go)). A source of many results,
such as a socket, calls `Post` for each and `Done` when it ends. `Unref` stops work keeping the runtime busy, as `unref()` does in
node. `AbortOn` stops whatever the runtime runs once a channel closes, which is
how a parent ends a worker from another goroutine.

### Async context

The runtime holds one async context value, and the engine carries it to where
queued work runs: a promise reaction (a `then`, an `await`) runs in the context
it was registered in, a job in the one it was queued in, and what is posted to
an `AsyncWork` in the one the work was started in. `AsyncContext` reads it and
`SetAsyncContext` sets it. The value is the host's, and opaque to the engine.

This is what `AsyncLocalStorage` is built on, as node's is since node 24 and as
the TC39 AsyncContext proposal has it. The standard library's
`node:async_hooks` keeps a map from each storage to its store in the context,
and gives the same answers as node 26 for stores through `await`, `then`,
microtasks, timers, `Promise.all`, `snapshot`, `AsyncResource` and
`enterWith`. A host that runs callbacks of its own takes the context when it
starts the work and sets it around the callback, as the standard library's
timers do.

### Closing a runtime

`Close` may be called from inside the runtime's own script, as `process.exit`
would be, and then ends it as node ends a worker: the script stops at that
call, running no `catch` and no `finally`; what was posted to an `AsyncWork`
is called with `ErrClosed`; the runtime's `Context` is cancelled; and the hooks
`OnClose` registered run, newest first, before `Close` returns. Those hooks are
where a host releases what it holds for the script. `Close` is never for
another goroutine: that one closes the channel given to `AbortOn`, and the
runtime's own goroutine closes it.

## A runtime on a CPU of its own

Go's collector marks while the program runs. With cores to spare, that work
runs on them and a script hardly notices it. A host that gives each runtime one
CPU -- a prefork server of single-threaded processes, or a container with one
-- has none to spare: marking shares the thread with the script, and a cycle's
marking took 12 ms rather than 1.5, so that the script runs under the write
barrier for much of its time. Rendering a React page, the collector is marking
for 42% of a render at `GOMAXPROCS=1` and 10% on 32 cores. Two settings change
that, and neither needs anything of the engine.

Collect between requests, which is the better of the two: turn the collector's
pacing off, collect when no request is waiting, and keep a memory limit, so
that a request that outgrows it is still collected.

```go
debug.SetGCPercent(-1)
debug.SetMemoryLimit(512 << 20)

for served := 1; ; served++ {
    handle(<-requests, rt)
    if served%4 == 0 {
        runtime.GC()
    }
}
```

Or, with no change to the host's code, make every collection stop the world,
by `GODEBUG=gcstoptheworld=1` in the process's environment: a script is paused
while the collector marks, and never runs under the barrier.

One process, `GOMAXPROCS=1`, 800 renders of a 107 KB page by React 18:

| | With the JIT: a second | median | 99th percentile | Without: a second | median | 99th percentile |
|---|---|---|---|---|---|---|
| Go's default | 94 | 10.4 ms | 21.8 ms | 64 | 15.8 ms | 24.8 ms |
| `gcstoptheworld=1` | 107 | 9.6 ms | 18.3 ms | 75 | 13.2 ms | 21.3 ms |
| `runtime.GC()` after each | 98 | 8.5 ms | 16.3 ms | 74 | 12.0 ms | 20.6 ms |
| `runtime.GC()` after every fourth | 111 | 8.6 ms | 16.3 ms | 80 | 12.0 ms | 20.7 ms |

How often is the host's to choose: fewer collections take less time and more
memory. On this page, which allocates 5 MB a render, collecting after every
fourth took a heap of 33 MB against the default's 23 without the JIT, and the
same 47 MB with it. A collection after each request is the simplest, and the
heap the smallest, at the price of a whole cycle for each.

Neither is for a process with more than one CPU, or more than one runtime:

- Stopping the world on 32 cores took 3% longer than Go's default.
- A collection is the process's, so stopping the world stops every runtime in
  it. Thirty-two runtimes on one CPU, in test262's runner, kept taking each
  other's turns: a test that finishes in moments had waited 30 seconds.

`GOGC` trades in the same way, more plainly: `GOGC=400` took 20% off fifty
renders at one thread, and the peak heap went from 60 MB to 136.

## Moving values between runtimes

`Serialize` and `Deserialize` are structured cloning, what `structuredClone`
and `postMessage` do: a value is serialized in one runtime into a `Serialized`
that belongs to none, handed to another goroutine, and deserialized in the same
runtime or another. Shared references and cycles come out as they went in, an
`ArrayBuffer` listed in `Transfer` moves rather than being copied, and a
`SharedArrayBuffer`'s memory is shared, so two workers given one see each
other's writes.

```go
data, err := a.Serialize(v, &quickjs.CloneOptions{Transfer: []quickjs.Value{buf}})
// on b's goroutine:
w, err := b.Deserialize(data, nil)
```

A `Serialized` is deserialized once; `Copy` makes another, for a message sent
to many. What cannot be cloned is a `*DataCloneError` with V8's message, which
a host throws as a `DOMException`. A host's own objects take part through a
`CloneCodec`, which turns them into tokens and back, and `SetCloneBrand` marks
an object the host made as cloned through the codec, as an empty object, not
at all, or only by transfer.
