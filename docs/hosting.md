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

A source of many results, such as a socket, calls `Post` for each and `Done`
when it ends. `Unref` stops work keeping the runtime busy, as `unref()` does in
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

[QuickJS-NG]: https://github.com/quickjs-ng/quickjs
[test262]: https://github.com/tc39/test262
[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
[go-intl]: https://github.com/go-quickjs/go-intl
[go-intl-compat]: https://github.com/go-quickjs/go-intl/blob/main/compat.go
[arm-aor]: https://github.com/ARM-software/optimized-routines
