# The standard library

The `stdlib` package builds the environment a program expects out of the
pieces [Embedding the engine](embedding.md) and [Building a host](hosting.md)
describe. Every capability is separate, because they are not equally dangerous:

```go
rt := quickjs.New()
loop := stdlib.NewLoop(rt)

err := stdlib.Install(rt, stdlib.Config{
    Stdout:  os.Stdout,
    Stderr:  os.Stderr,
    Loop:    loop,
    FS:      &stdlib.FS{Root: "/srv/data", ReadOnly: true},
    Process: &stdlib.Process{Args: os.Args, Env: nil},
    Fetch:   &stdlib.Fetch{Allow: onlyMyAPI},
    Serve:   &stdlib.Serve{Allow: onlyLocalhost},
    Sockets: &stdlib.WebSockets{Allow: onlyMyFeed},
})

rt.Eval(src)
loop.Run(ctx)     // timers, and work that finished on other goroutines
```

| | |
|---|---|
| Always | `console`, `URL`, `TextEncoder`/`TextDecoder`, `atob`/`btoa`, `structuredClone`, `MessageChannel`, `DOMException`, `performance`, `crypto` (hashing, HMAC, PBKDF2, HKDF, `subtle`), `Blob`, `File`, `FormData`, `URLPattern`, `AbortController`, `Buffer`, the web's streams, `CompressionStream`, and the `path`, `events`, `util`, `assert`, `buffer`, `crypto`, `zlib`, `stream/web`, `url`, `querystring`, `string_decoder`, `vm` modules |
| `Loop` | `setTimeout`, `setInterval`, `queueMicrotask`, and the `timers`, `timers/promises` modules |
| Intl data | names of every language, region, script and currency are built in and decoded one locale at a time |
| `FS` | the `fs` module, sync and promise halves, `createReadStream`/`createWriteStream`, confined to `Root` |
| `Process` | `process.argv`, `env`, `cwd`, `stdout`, `exit` — what the host chooses to say |
| `OS` | the `os` module |
| `Fetch` | `fetch`, `Headers`, `Request`, `Response` — bodies read as they arrive |
| `Serve` | `serve`, the `http` module — an HTTP server whose handler is `(Request) => Response` |
| `DNS` | the `dns` and `dns/promises` modules — `lookup`, the `resolve` family, `reverse`, `lookupService`, `Resolver` |
| `Sockets` | `WebSocket`, and `upgradeWebSocket` where there is a server to accept one on |
| `Run` | the `child_process` module: `execFileSync`, `execFile`, `spawnSync`, `exec` |
| `Workers` | `Worker`, and `node:worker_threads`' — each worker a runtime of its own on a goroutine of its own, installed with the same `Config` |
| `WindowsPaths` | `path` is `path.win32`, as node's is on Windows, for a host whose script sees the machine's own paths; without it `path` is `path.posix` wherever the host runs |

`path` is node's, both flavors: `path.posix` and `path.win32` -- and the
`path/posix` and `path/win32` modules -- answer as node's do, for drives, UNC
shares, device paths and the rest. `path.resolve` resolves against
`process.cwd()`, read when it is called. And each of node's modules is one
module under both of its names: `require("fs") === require("node:fs")`.

`structuredClone` and a `MessagePort`'s `postMessage` clone as V8 does, word
for word where they refuse: an object is read by what it is rather than what it
says it is, cycles and shared references survive, a transferred `ArrayBuffer`
moves, and a `SharedArrayBuffer` is shared. A port posted through another
arrives with what was sent to it on the way. As in a browser, a port dispatches
once it is started -- by `start()` or by setting `onmessage` -- and, as in node,
a started port keeps the `Loop` running until it is closed or `unref()`'d.
`BroadcastChannel` reaches every channel of its name in the process.

A worker talks to the runtime that started it through such a port, so a
`SharedArrayBuffer` posted to it is memory the two share -- `Atomics.wait` in
one, `Atomics.notify` in the other -- and a port posted to it is a line to
anywhere. The host says how a worker's runtime is made and where its code comes
from; `terminate()` stops it whatever it is doing.

```go
Workers: &stdlib.Workers{
    New:  func() (*quickjs.Runtime, error) { return newRuntime(), nil },
    Load: func(spec string) (src, name string, module bool, err error) { ... },
},
```

A root is a boundary: a path that climbs out of it, or a symbolic link that
points out of it, is refused rather than followed. `Fetch.Allow` sees every
request before it is made, `Serve.Allow` every address before it is listened on,
`DNS.Allow` every name before it is looked up, and `Run.Allow` every program
before it is started. What is not installed cannot
be reached.

Names are answered by `DNS.Resolver`: the system's by default, or anything
with `net.Resolver`'s methods -- a cache, DNS over HTTPS, a fixed table. Node's
shapes and errors are kept (`queryA ENOTFOUND example.invalid`, with `code`,
`syscall` and `hostname`), and what Go's resolver cannot give is refused with
`ENOTIMP` rather than imitated: records of the types CAA, NAPTR, SOA, TLSA and
ANY, and a record's time to live. A TXT record comes back as one string, its
pieces joined, and `lookupService` names a service by its port's number.
`getServers` says what `DNS.Servers` says, and `setServers` is refused: which
servers are asked is the host's to decide.

Nor does anything outlive the runtime it was started for. `Runtime.Context` is
cancelled by `Close`, and `Loop.Context` by that or by the loop's own `Close`:
a request in flight is abandoned, a program killed, a socket and a server
closed, and a worker terminated. A host's own asynchronous functions start
their work with the same context.

A server's handler is script, so it runs on the loop; the connections are served
on their own goroutines and wait for it. A program that is given the environment
is the one that passes it on: `Run.Env` is what a started program sees, and nil
means none at all, so a script refused the environment cannot read it through a
program it starts.

Everything in the first row is arithmetic — it reads values and returns values,
and reaches nothing — so it is installed without being asked for. That includes
the streams, which carry whatever is plugged into either end of them:

```js
await fs.createReadStream("big.log")
  .pipeThrough(new CompressionStream("gzip"))
  .pipeTo(fs.createWriteStream("big.log.gz"))

for await (const chunk of response.body) { ... }
```

Streams are the transport, not only the shape: `fetch` answers when the headers
arrive and reads the body as the script asks for it, a handler that answers with
a stream has each piece written and flushed as it is produced, and a file is
read a chunk at a time. Nothing here has to fit in memory to go past.

and the hashing, which is Go's rather than a cipher written in script:

```js
import {createHmac, timingSafeEqual} from "crypto"
const mine = createHmac("sha256", secret).update(body).digest()
if (!timingSafeEqual(mine, theirs)) throw new Error("not from who it says")
```
