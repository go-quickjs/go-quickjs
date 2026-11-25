# go-quickjs

A JavaScript engine for Go, written in pure Go, with an interpreter of its
own design and an optional, experimental JIT that compiles hot functions to
native code, as V8's optimizing compilers do.

It began as a reimplementation of [QuickJS-NG], and keeps its name, its
`qjs` command and its small embedding model. How it runs code is its own:
functions compile to register code, run by an interpreter whose handlers
are native code generated for each architecture, as V8's Ignition runs
bytecode -- not QuickJS's stack bytecode.

No cgo, no C toolchain, and no C engine compiled to WebAssembly, the JIT
included. It cross-compiles anywhere Go does; the JIT runs on `linux/amd64`,
`windows/amd64`, `darwin/arm64` and `linux/arm64`, and elsewhere the engine
runs without it.

```go
rt := quickjs.New()
defer rt.Close()

v, err := rt.Eval(`[1, 2, 3].map(x => x * 2).join("-")`)
fmt.Println(v) // 2-4-6
```

Four things live here:

| | |
|---|---|
| `quickjs` | the engine, to embed in a Go program |
| `qjs` | a command that runs JavaScript, with the capabilities you allow |
| `wasm` | a WebAssembly engine for Go, with WASI, which scripts reach through the `WebAssembly` namespace |
| `jsregexp` | ECMAScript regular expressions for Go, which RE2 cannot express |

```
go get github.com/go-quickjs/go-quickjs
go install github.com/go-quickjs/go-quickjs/cmd/qjs@latest
```

## Status

The language is substantially complete: expressions, closures, classes with
inheritance and private members, destructuring, generators, `async`/`await`,
Promises, regular expressions, modules with top-level `await`, Proxy, typed
arrays, explicit resource management and proper tail calls all work, as does
the web-compatibility annex, and all of it is exercised against [test262], the
official ECMAScript conformance suite.

`Intl` is there too, from [go-intl], a pure-Go implementation of ECMA-402 built
to answer as ICU 78.3 does in Node 26, with CLDR 48.2 data for every locale ICU
has -- rather than linking ICU -- including the Unicode collation order, where
a text may be broken into words and sentences, how a measurement is written,
how long something took, eighteen calendars, and what every time zone is
called in every language. It is held against a full ICU build:
[7,948 of 7,949 cases match it exactly](intl_test.go), and the one that does
not is named.

Of the 100,319 test262 variants in the areas the engine claims -- the language,
the built-ins, `Intl` and Annex B -- 100,277 pass, none fail, and the other 42
are skipped because decorators are not implemented, on
test262 commit [`e4fbf577`](https://github.com/tc39/test262/commit/e4fbf57745c6aae3d1f12f181ecef78b1665d812). See [Conformance](docs/status.md#conformance) for the
measurement and [Not implemented](docs/status.md#not-implemented) for what is missing.

## Performance

Functions run in the register interpreter ([its
design](docs/register-interpreter.md)), whose handlers are native code
generated for it on amd64 and arm64, and a table of Go functions on other
architectures. The JIT, which is optional and experimental, compiles hot
functions from their register code to native code on `linux/amd64`,
`windows/amd64`, `darwin/arm64` and `linux/arm64`. It follows V8's
optimizing compilers: it speculates on what the inline caches have seen,
inlines calls, and falls back to the interpreter when a speculation fails.
It needs both a build with `-tags quickjs_jit` and a runtime made with
`quickjs.WithJIT()` (or `qjs --jit`); anything else runs exactly as before:

```go
// go build -tags quickjs_jit
rt := quickjs.New(quickjs.WithJIT())
```

What the JIT holds is bounded by `WithJITMemoryLimit` -- by default 8 MB,
or an eighth of `WithMemoryLimit` when that is set, and as much again for
the runtime's WebAssembly code -- and never counts against
`WithMemoryLimit`. A function it cannot compile, a runtime made
`WithDebugger`, or a process denied executable memory runs in the
interpreter as before. Neither native execution nor a speedup is
guaranteed: a few operations that are mostly a built-in's work run slower
with it, so measure a workload with it on and off. See
[its design notes](internal/jit/README.md).

Its V8 version 7 composite score is 11.96 times goja's with the JIT off,
30.32 times with synchronous JIT compilation, and 30.02 times with background
JIT compilation, on an AMD Ryzen 5 3600 with Go 1.27.3. This older
workload suite does not represent every application; see the
[goja comparison](docs/benchmarks.md#v8-version-7-benchmark-suite) for the
individual scores and measurement details.

Measured at `976e5cc1`, on the register interpreter, on the V8 benchmark
suite as a whole it is well ahead of C QuickJS: it takes 0.33 times
QuickJS's time over the suite and has 2.26 times its composite score, and
with the experimental JIT 0.22 times its time and a composite score 5.8
times as high. On the larger programs of Octane and Are We Fast Yet it
takes 0.43 and 0.64 times QuickJS's time, and 0.28 and 0.20 times with the
JIT; Octane's zlib, an asm.js module run as typed code, takes 0.19 times,
and 0.07 with the JIT. A page React 18 renders on the server takes 0.58
times, and 0.47 with the JIT. Single operations cost about half as much:
QuickJS's own micro-benchmarks take 0.49 times as long on average, and
0.24 times with the JIT, though a few
take up to 1.5 times as long, the weak collections and the BigInts past 64 bits among them. See [Compared with C QuickJS](docs/benchmarks.md#compared-with-c-quickjs).

## WebAssembly

The `wasm` package runs WebAssembly from Go, with an API shaped like
wazero's, and `wasm/wasi` gives modules WASI preview 1, sandboxed: nothing
of the process reaches a module unless the host configures it. Scripts reach
the same engine through the `WebAssembly` namespace, which a host installs
with `wasm/wasmjs`. The namespace is built only with `-tags quickjs_wasm`, as
the JIT is with `quickjs_jit`; `qjs` built so installs it as Node has it.
Both are opt-in: the JavaScript engine does not import the wasm engine.

```go
r := wasm.NewRuntime(ctx)
defer r.Close(ctx)

mod, err := r.Instantiate(ctx, binary)
results, err := mod.ExportedFunction("add").Call(ctx, api.EncodeI32(40), api.EncodeI32(2))
```

It implements WebAssembly 3.0 -- GC, exceptions, typed function references,
tail calls, memory64, multiple memories, SIMD and relaxed SIMD -- and the
threads proposal, and passes all 65,824 assertions of the spec's tests and
the threads proposal's, at pinned commits. Invalid modules, failed links and
traps are reported with V8's messages: validation matches Node 26 over
345,311 modules with no difference. Functions run in a register
interpreter, its handlers native code on amd64 and arm64; a baseline JIT
compiles hot ones on amd64 (Linux and Windows)
and arm64 (macOS and Linux), with `-tags quickjs_jit` and
`wasm.NewRuntimeConfigCompiler()` or a runtime made `WithJIT`. See
[WebAssembly](docs/wasm.md).

## Documentation

| | |
|---|---|
| [Status and conformance](docs/status.md) | what is implemented and what is not, and how test262 is run against it |
| [Known issues](docs/known-issues.md) | the issues a review of the engine found, by severity, and what became of each |
| [The qjs command](docs/qjs.md) | running JavaScript from the command line, with only the capabilities you allow |
| [Embedding the engine](docs/embedding.md) | calling Go from JavaScript and JavaScript from Go, compiled programs, realms, modules |
| [Building a host](docs/hosting.md) | work that finishes on other goroutines, async context, closing a runtime, moving values between runtimes |
| [The standard library](docs/stdlib.md) | the console, timers, fetch, the filesystem, workers and the node modules, each a capability of its own |
| [WebAssembly](docs/wasm.md) | the wasm engine, from Go and from JavaScript: WASI, limits, error texts, the JIT, conformance |
| [Sandboxing](docs/sandboxing.md) | what a script can reach, and how its time and memory are bounded |
| [Debugging](docs/debugging.md) | Chrome's DevTools and VS Code attached to a runtime, from qjs --inspect or a host |
| [Design notes](docs/design.md) | how the engine is built, for reading the source |
| [Benchmarks](docs/benchmarks.md) | Go benchmarks, and the V8 suite and micro-benchmarks against goja and C QuickJS, with the JIT and without |
| [The JIT](internal/jit/README.md) | the experimental native tier: how it compiles, speculates and deoptimizes, and the contracts its code keeps |
| [Performance plan](docs/performance-plan.md) | the optimizations still to make, how each is measured, and what was tried and rejected |
| [Optimizing](docs/optimizing.md) | what an optimization must show before it lands, and how to tell its cost from where code and data were placed |
| [jsregexp](jsregexp/README.md) | the engine's regular expressions as a Go package, against Go's `regexp` |

## License

MIT, the license of QuickJS-NG, where the engine began.

`internal/fdlibm`, which computes `Math`, is ported from other code under
its own terms, kept in [its LICENSE](internal/fdlibm/LICENSE): fdlibm, by way
of V8's `src/base/ieee754.cc` (the fdlibm notice and V8's BSD license), and
the `pow` of [Arm's Optimized Routines][arm-aor] (MIT).

[QuickJS-NG]: https://github.com/quickjs-ng/quickjs
[test262]: https://github.com/tc39/test262
[go-intl]: https://github.com/go-quickjs/go-intl
[arm-aor]: https://github.com/ARM-software/optimized-routines
