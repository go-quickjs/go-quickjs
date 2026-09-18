# go-quickjs

A JavaScript engine for Go, reimplementing [QuickJS-NG] in pure Go.

No cgo, no WebAssembly, no C toolchain. It cross-compiles anywhere Go does.

```go
rt := quickjs.New()
defer rt.Close()

v, err := rt.Eval(`[1, 2, 3].map(x => x * 2).join("-")`)
fmt.Println(v) // 2-4-6
```

Three things live here:

| | |
|---|---|
| `quickjs` | the engine, to embed in a Go program |
| `qjs` | a command that runs JavaScript, with the capabilities you allow |
| `jsregexp` | ECMAScript regular expressions for Go, which RE2 cannot express |

```
go get github.com/go-quickjs/go-quickjs
go install github.com/go-quickjs/go-quickjs/cmd/qjs@latest
```

## Status

The language is substantially complete: expressions, closures, classes with
inheritance and private members, destructuring, generators, `async`/`await`,
Promises, regular expressions, modules with top-level `await`, Proxy and typed
arrays all work, and are exercised against [test262], the official ECMAScript
conformance suite.

`Intl` is there too, with real CLDR data for 379 locales — the engine carries
its own, in Go, rather than linking ICU — including the Unicode collation
order, where a text may be broken into words and sentences, how a measurement
is written, and time zones out of the operating system's own database. It is
held against a full ICU build: [7,925 of 7,949 cases match it
exactly](intl_test.go), and the twenty-four that do not are named.

Of the 78,978 test262 tests it runs, 78,910 pass and the 68 that do not are
[named in the suite](conformance/conformance_test.go), each with what it asks
for that this engine does not carry. The 19,556 it skips are tagged with
features it does not implement, or ask the host for something it does not
provide: see [Conformance](#conformance) for the measurement and
[Not implemented](#not-implemented) for what is missing.

### Implemented

| Area | Notes |
|---|---|
| Expressions and operators | Full precedence, `??`, `?.`, `**`, BigInt |
| Variables | `var`, `let`, `const`, temporal dead zone, closures |
| Control flow | `if`, `for`, `for-in`, `for-of`, `while`, `do`, `switch`, labelled `break`/`continue` |
| Functions | Declarations, expressions, arrows, defaults, rest parameters, `arguments` |
| Classes | Constructors, methods, accessors, statics, `extends`, `super`, fields, private members, static blocks |
| Destructuring | Array and object patterns, defaults, nesting, rest elements |
| Exceptions | `throw`, `try`/`catch`/`finally`, stack traces |
| Iteration | Iterator protocol, spread, generators, `yield*` |
| Asynchrony | `Promise` with correct microtask ordering, `async`/`await` |
| Regular expressions | Backtracking engine: backreferences, lookahead, lookbehind, named groups, modifier groups, Unicode property escapes at Unicode 17, the `v` flag's set notation and properties of strings |
| Legacy | `with`, Annex B string methods, `escape`/`unescape`, sloppy-mode block functions |
| Modules | `import`/`export`, live bindings, cycles, namespace imports, dynamic `import()`, top-level `await` |
| Iterator helpers | `map`, `filter`, `take`, `drop`, `flatMap`, `reduce`, `toArray` and the rest, lazily |
| Unicode | Full case mappings including the final sigma, all four normalization forms, lone surrogates preserved end to end |
| Built-ins | `Object`, `Function`, `Array`, `String`, `Number`, `Boolean`, `Symbol`, `BigInt`, `Error`, `Math`, `JSON`, `Date`, `RegExp`, `Map`, `Set`, `Promise`, `Proxy`, `Reflect`, `ArrayBuffer`, `DataView`, typed arrays |
| Weak references | `WeakRef`, `FinalizationRegistry`, `WeakMap`, `WeakSet`, backed by Go's `weak.Pointer` and `runtime.AddCleanup`: a target really is released, and a registry really is called back |
| Reflection | `Proxy` with every trap and its invariants, `Reflect`, property descriptors, mapped `arguments` |
| Recent additions | Set operations, `Array.fromAsync`, `Object.groupBy`, `Promise.try`, `RegExp.escape`, `Error.isError`, `Math.sumPrecise`, `Uint8Array` base64 and hex |
| Eval | Direct `eval` runs in the caller's scope — its variables, `this`, `new.target` and `super`; indirect `eval` runs in global scope |
| Go interop | Function binding, marshalling, `context.Context` cancellation |

### Not implemented

`Temporal`, `Atomics`, `SharedArrayBuffer`, `ShadowRealm`, decorators,
resizable ArrayBuffers, `using` declarations, and the newer proposals test262
tracks.

Of `Intl`, what is missing is: calendars other than the Gregorian and the
Buddhist, and `Intl.DurationFormat`. Time zone names are English, where a zone
has a name rather than an offset. Sorting follows the Unicode algorithm with
each language's own tailoring, but not the orderings that are a whole script's
worth of data — Chinese and Japanese order their characters by sound or by
stroke, and those sort by code point here. `Intl.Segmenter` follows the Unicode
breaking rules, but not the word lists ICU consults for the scripts written
without spaces: a run of Chinese, Thai, Lao, Khmer or Burmese comes back as one
word rather than as several.

There is one realm per runtime: `$262.createRealm` has nothing to return, so
the four test262 variants that need a second realm are skipped along with the
`cross-realm` and `ShadowRealm` ones.

In the standard library: node's own streams (the web's are here instead, and
everything that takes a stream takes those), brotli, and BYOB readers. A
decompression stream gathers its input rather than being pulled through, since
nothing here can pull it without taking the runtime onto another goroutine.

Known semantic gaps, each covered by a test that documents it:

- A `WeakMap` or `WeakSet` value is held strongly, so a value that refers to its
  own key keeps that key alive. Breaking that cycle needs ephemeron marking,
  which Go's collector does not offer.
- `String.prototype.normalize` works from Unicode 13 decomposition tables, which
  is what the system the tables were generated from had. A character introduced
  after that decomposes to itself.

## Conformance

The engine is tested against test262. The suite is not vendored; point the
runner at a checkout:

```
git clone --depth 1 https://github.com/tc39/test262 /tmp/test262
TEST262_DIR=/tmp/test262 go test ./conformance -timeout 120m
```

The runner honours each test's frontmatter: the harness files to include, the
strict and sloppy variants, the expected-failure phase and type, and the feature
tags. A test tagged with a feature the engine does not implement is skipped
rather than counted against it.

Measured coverage, as of the most recent run over the whole suite — 78,910 of
78,978 executed variants, with the 68 that fail named in the runner along with
what each asks for that is not carried here. The other 19,556 are skipped
rather than counted: a test tagged with a feature the engine does not
implement, or one that asks the host for a second realm or an agent, is testing
something that was never claimed.

`built-ins/Atomics` is the ten tests for `Atomics.pause`, which is the only part
of that API a single-threaded engine could offer and which is not implemented.

Useful flags:

```
-conformance.dir=language/expressions   # restrict to one area
-conformance.report=/tmp/failures.txt   # write every failure for triage
-conformance.timeout=2s                 # bound any one test
-conformance.workers=4                  # how many to run at once
```

The suite runs on every core, which takes a few minutes rather than well over an
hour.

## The qjs command

```
qjs script.js arg1 arg2      run a file
qjs -e 'console.log(1 + 1)'  run an expression
qjs                          read from a prompt
cat script.js | qjs -        run what arrives on standard input
```

A file that imports is run as a module without being told to; a relative
specifier is resolved against the file that named it.

A script can do nothing outside the process until the command line says it may,
because the engine has no ambient authority to withhold:

```
qjs --allow-read=. build.js             read files under this directory
qjs --allow-write=/tmp --allow-read=/tmp generate.js
qjs --allow-net=api.example.com fetch.js reach one host, and serve on it
qjs --allow-env deploy.js                read the environment
qjs --allow-run=git release.js           start programs, or only some
qjs -A script.js                         all of it, for code you trust
```

A script that reaches for something it was not given is told which flag would
have given it, rather than finding a hole where a function should be:

```
$ qjs -e 'fetch("https://example.com")'
uncaught (in promise) Error: network access is not allowed: run qjs with --allow-net
```

Sockets work both ways, and the protocol — its frames, its fragments, its pings
— is handled underneath, so what a script sees is what the other end said:

```js
serve({port: 8080}, (request) => {
  const {socket, response} = upgradeWebSocket(request)
  socket.onmessage = (e) => socket.send("you said " + e.data)
  return response
})
```

A program that serves is a program, so `qjs` can be the whole of a small
service:

```js
// server.js -- qjs --allow-net server.js
serve({port: 8080}, async (request) => {
  const {pathname} = new URL(request.url)
  if (pathname === "/health") return new Response("ok")
  return Response.json({path: pathname})
})
```

The bounds are there too — `--memory-limit 64m`, `--stack-size`, `--timeout 5s`,
`--no-code-generation` — and `--check` parses without running. The prompt keeps
an unfinished line rather than refusing it, so a function can be typed over
several lines, leaves the last value in `_`, and takes a top-level `await`:

```
> const res = await fetch("https://example.com")
> res.status
200
```

## Calling Go from JavaScript

Set an ordinary Go function as a global. Arguments and results are converted
automatically.

```go
rt.Set("add", func(a, b int) int { return a + b })
rt.Eval(`add(1, 2)`) // 3
```

A function may return an `error`, which becomes a thrown JavaScript exception:

```go
rt.Set("mustBePositive", func(n int) (int, error) {
    if n < 0 {
        return 0, errors.New("negative")
    }
    return n, nil
})
rt.Eval(`try { mustBePositive(-1) } catch (e) { e.message }`) // "negative"
```

Taking a `*quickjs.Runtime` as the first parameter lets a Go function call back
into the engine:

```go
rt.Set("applyTwice", func(r *quickjs.Runtime, fn quickjs.Value, v int) (int, error) {
    once, err := fn.Call(v)
    if err != nil {
        return 0, err
    }
    twice, err := fn.Call(once.Int())
    return twice.Int(), err
})
rt.Eval(`applyTwice(n => n + 3, 1)`) // 7
```

## Reading values back

`Decode` follows the conventions of `encoding/json`, including struct tags. A
`js` tag takes precedence over a `json` tag, so a struct already annotated for
`encoding/json` works unchanged.

```go
var out struct {
    Name string   `js:"name"`
    Tags []string `js:"tags"`
}
v, _ := rt.Eval(`({name: "Ada", tags: ["math"]})`)
err := v.Decode(&out)
```

Decoding into `any` produces the natural Go form: `nil`, `bool`, `float64`,
`string`, `[]any` or `map[string]any`.

## Extending a runtime

A runtime starts with the language and nothing else. What a script can reach is
what the host puts there — a global, or a module it has to import:

```go
rt.Set("add", func(a, b int) int { return a + b })

rt.SetModule("storage", map[string]any{
    "get":     func(key string) string { return store[key] },
    "set":     func(key, value string) { store[key] = value },
    "default": map[string]any{"name": "storage"},
})
rt.EvalModule("main.js", `import {get} from "storage"; get("k")`)
```

A module registered this way answers to its name ahead of any loader, and works
in a runtime with no loader at all.

Anything that finishes later hands back a promise the host settles:

```go
rt.Set("readLater", func(name string) *quickjs.Promise {
    p := rt.NewPromise()
    go func() {
        b, err := os.ReadFile(name)
        loop.Post(func() {           // back on the runtime's goroutine
            if err != nil {
                p.RejectError(err)
                return
            }
            p.Resolve(string(b))
        })
    }()
    return p
})
```

`NewObject`, `NewArray`, `NewBytes`, `NewError` and `Throw` build the values a
marshalled Go value cannot express; `Value.Bytes` reads a typed array back.
`OnUnhandledRejection` reports a promise nobody took.

## Modules

Imports are resolved through a loader the host supplies. A runtime without one
rejects every import, which is the default:

```go
rt.SetModuleLoader(func(specifier, referrer string) (source, resolved string, err error) {
    b, err := os.ReadFile(filepath.Join(root, specifier))
    return string(b), specifier, err
})

ns, err := rt.EvalModule("main.js", `import {greet} from "./greet.js"; greet();`)
```

`EvalModule` returns the module's namespace, through which its exports can be
read. Bindings are live: an importer sees the exporter's current value, not a
copy taken at link time.

## The standard library

The `stdlib` package builds the environment a program expects out of those
pieces. Every capability is separate, because they are not equally dangerous:

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
| Always | `console`, `URL`, `TextEncoder`/`TextDecoder`, `atob`/`btoa`, `structuredClone`, `performance`, `crypto` (hashing, HMAC, PBKDF2, HKDF, `subtle`), `Blob`, `File`, `FormData`, `URLPattern`, `AbortController`, `Buffer`, the web's streams, `CompressionStream`, and the `path`, `events`, `util`, `assert`, `buffer`, `crypto`, `zlib`, `stream/web`, `url`, `querystring`, `string_decoder` modules |
| `Loop` | `setTimeout`, `setInterval`, `queueMicrotask`, and the `timers`, `timers/promises` modules |
| import `intldata` | the names of every language, region, script and currency, in every language — `Intl.DisplayNames` answers in English without it |
| `FS` | the `fs` module, sync and promise halves, `createReadStream`/`createWriteStream`, confined to `Root` |
| `Process` | `process.argv`, `env`, `cwd`, `stdout`, `exit` — what the host chooses to say |
| `OS` | the `os` module |
| `Fetch` | `fetch`, `Headers`, `Request`, `Response` — bodies read as they arrive |
| `Serve` | `serve`, the `http` module — an HTTP server whose handler is `(Request) => Response` |
| `Sockets` | `WebSocket`, and `upgradeWebSocket` where there is a server to accept one on |
| `Run` | the `child_process` module: `execFileSync`, `execFile`, `spawnSync`, `exec` |

A root is a boundary: a path that climbs out of it, or a symbolic link that
points out of it, is refused rather than followed. `Fetch.Allow` sees every
request before it is made, `Serve.Allow` every address before it is listened on,
and `Run.Allow` every program before it is started. What is not installed cannot
be reached.

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

## ECMAScript regular expressions for Go

Go's `regexp` is RE2: it buys linear time by refusing backreferences, lookaround
and the rest. The `jsregexp` package is this engine's regular expressions on
their own, for Go code that needs a pattern RE2 cannot express:

```go
re := jsregexp.MustCompile(`(?<user>\w+)@(\w+)\.com`, "i")
m, err := re.FindStringSubmatch("Write to Someone@Example.com today")
// m = ["Someone@Example.com", "Someone", "Example"]
```

Backreferences, lookahead and lookbehind, named groups, `\p{Script=Greek}`, the
`v` flag's set notation. The method set follows Go's `regexp`, with two
differences it documents: every method returns an error, because a backtracking
matcher can give up where RE2 cannot, and a replacement follows
`String.prototype.replace`.

## Sandboxing

A runtime has no I/O, no network access, no timers, no filesystem and no module
loader unless the host adds them. There is no `require`, no `process`, no
`fetch`. `Date` reads the clock through an injectable hook rather than the
process clock. Everything in [the standard library](#the-standard-library) is
something a host hands over deliberately, and the [qjs](#the-qjs-command)
command hands over only what its flags name.

`WithoutCodeGeneration()` removes `eval` and the `Function` constructor. Neither
grants a script a capability it does not already have — code it could `eval`, it
could also write inline — but both defeat review of the source a host is about
to run, which matters when that source is audited before use.

Bounds are set at construction:

```go
rt := quickjs.New(
    quickjs.WithMemoryLimit(64<<20),
    quickjs.WithStackSize(1<<16),
    quickjs.WithMaxCallDepth(1000),
)
```

Runaway recursion raises a catchable `RangeError` rather than overflowing the
goroutine stack. Deeply nested source is rejected at parse time for the same
reason — a goroutine stack overflow cannot be caught, so it would take the host
down — and so is deeply nested JSON, which `JSON.parse`, `JSON.stringify` and a
reviver all walk by recursion. A panic anywhere inside the engine is caught at
the `Eval` boundary and returned as `ErrInternal`: a host running untrusted code
must not be taken down by the code it is sandboxing.

Wall-clock bounds come from a `context.Context`, which the interpreter polls as
it executes, so an infinite loop is interrupted rather than hanging the process:

```go
ctx, cancel := context.WithTimeout(context.Background(), time.Second)
defer cancel()

_, err := rt.EvalContext(ctx, `while (true) {}`)
// errors.Is(err, context.DeadlineExceeded)
```

An interruption is deliberately **not** catchable from script, so a sandboxed
program cannot defeat its own timeout with `try`/`catch`. A catastrophically
backtracking regular expression fails with an error rather than stalling.

## Concurrency

A `Runtime` is not safe for concurrent use. Give each goroutine its own, which
also isolates untrusted scripts from one another.

## Design notes

A few decisions worth knowing about if you read the source.

**Values are NaN-boxed into 24 bytes.** A `Value` is a `float64` plus an
interface. Real numbers live in the float; every other kind is a quiet NaN whose
payload encodes the type, with the pointer in the interface. Nothing allocates —
a pointer stored in an interface does not — and a type check is a mask and
compare. The invariant this rests on is that a genuine NaN must never collide
with a tag, so every number is normalized on the way in.

**The interpreter allocates nothing per call.** One contiguous slice backs both
locals and operands; a frame is a window into it. Neither that slice nor the
frame stack is ever grown, which is what makes it safe for a closure to hold a
pointer into a live frame and for the interpreter to hold a `*frame` across
nested calls. Exhausting either is the stack limit, reported as the same
"maximum call stack size exceeded" a browser gives.

**Strings are UTF-8 with an ASCII fast path, and concatenation builds a rope.**
Repeated `s += x` is how scripts build large strings, and copying each time is
quadratic. Ropes defer the copy; flattening is iterative because a rope from a
200,000-iteration loop is 200,000 deep.

**Lone surrogates are fully supported.** A JavaScript string is a sequence of
UTF-16 code units with no well-formedness requirement, so `"\uD83D"` is an
ordinary one-code-unit string. Go cannot represent one — `WriteRune` substitutes
U+FFFD — so `internal/wtf8` encodes them in WTF-8. Well-formed text is
bit-identical to UTF-8.

**The regexp engine backtracks, because it has to.** Go's `regexp` is RE2, which
buys linear time by refusing backreferences, lookahead and lookbehind — exactly
what JavaScript requires. Backtracking brings an exponential worst case, so the
matcher gives up after a step budget rather than letting a hostile pattern stall
the host.

**Generators save their frame rather than running on a goroutine.** A goroutine
per generator is simpler but leaks one for every generator never exhausted.
Saving the frame works because `yield` is only valid inside the generator's own
body, so at suspension its frame is the top of the stack.

**Promise reactions never run synchronously.** Settling queues them; the queue
drains between turns. That ordering guarantee is the point of the design.

**The Array methods read through the property protocol, not the element
slice.** Nearly all of them are generic — `Array.prototype.map.call(arguments,
f)` is supposed to work — and a hole has to fall through to the prototype while
an index redefined as an accessor has to be called. There is a fast path for the
case that dominates, a dense array whose element really is there: such an
element shadows anything on the prototype and cannot be an accessor, so taking
it directly is not observable.

**An arrow function captures its surroundings when the closure is made.** It has
no `this`, `new.target`, `super` or `arguments` of its own, so they are read
from the creating frame and the arrow ignores whatever its own call supplies.
Nesting needs no extra work: an arrow created inside another has already
inherited them.

**The compiler knows whether a value is wanted.** A statement's expression
leaves a value the statement would then discard, and in a loop body that is an
instruction an iteration: `t += x` stores into a local and keeps a copy to
drop, `i++` keeps the value it had before, `o.x = v` carries the value back out
past the store. Each is compiled without that, and a comparison a branch tests
directly becomes one instruction rather than two. A counted loop of the kind
every program contains lost a fifth of its instructions to this.

**An object is allocated the size it is about to be.** An object literal says
how many properties it will be given, a constructor's body is walked for the
ones it assigns to `this`, and an array literal knows its length -- so the
table or the element storage travels in the object's own allocation rather than
being a second one. A closure is one allocation too: the function object, its
function data, the closure and the bindings it captures are laid out together.

## Benchmarks

On an Apple M5 Max:

```
BenchmarkEvalArithmetic-18  1.26µs/op   2952 B/op    20 allocs/op
BenchmarkFibonacci-18        849µs/op     24 B/op     1 allocs/op
BenchmarkPropertyAccess-18  1.85µs/op   3800 B/op    26 allocs/op
BenchmarkCallGoFunction-18  1.40µs/op   3584 B/op    31 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop allocates nothing
per call; the other benchmarks include parsing and compiling their source each
iteration.

`go test -bench .` also measures the interpreter on its own, compiling each
program once and then running it:

```
BenchmarkLoopArithmetic-18         245µs/op        0 B/op     0 allocs/op
BenchmarkLoopArrayIndex-18        30.3µs/op        0 B/op     0 allocs/op
BenchmarkLoopFunctionCall-18       397µs/op        0 B/op     0 allocs/op
BenchmarkLoopPropertyAccess-18     520µs/op        0 B/op     0 allocs/op
BenchmarkLoopMethodCall-18         646µs/op        0 B/op     0 allocs/op
BenchmarkAllocObjects-18           137µs/op   384 KB/op  2000 allocs/op
BenchmarkStringConcat-18          65.5µs/op   161 KB/op  2029 allocs/op
BenchmarkArrayCallbacks-18         108µs/op  42.3 KB/op    23 allocs/op
BenchmarkJSONRoundTrip-18          489µs/op   904 KB/op 10000 allocs/op
BenchmarkNewRuntimeSmallStack-18  80.1µs/op   532 KB/op   927 allocs/op
```

Each of those runs its program many times over: the loop benchmarks 10,000
iterations, the allocation ones 2,000 objects, `ArrayCallbacks` a map, a filter
and a reduce over 1,000 elements, `JSONRoundTrip` 500 parse-and-stringify
round trips.

## License

MIT, matching the upstream project.

[QuickJS-NG]: https://github.com/quickjs-ng/quickjs
[test262]: https://github.com/tc39/test262
