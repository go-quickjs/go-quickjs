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

Of the 99,979 test262 variants in the areas the engine claims -- the language,
the built-ins, `Intl` and Annex B -- 99,637 pass, none fail, and 342 are
skipped because they require an unsupported feature or host facility, on
test262 commit [`2e0a5676`](https://github.com/tc39/test262/commit/2e0a56762801e275a9fdf96dc49d90ba0cddcf63). See [Conformance](docs/status.md#conformance) for the
measurement and [Not implemented](docs/status.md#not-implemented) for what is missing.

With the native JIT disabled, its V8 version 7 composite score is 5.74 times
goja's on an Apple M5 Max with Go 1.27.0. This older workload suite does not
represent every application; see the [goja comparison](docs/benchmarks.md#v8-version-7-benchmark-suite)
for the individual scores and measurement details.

On the V8 benchmark suite as a whole it is ahead of C QuickJS: it takes 0.60
times QuickJS's time over the suite and has a 24% higher composite score,
and with the experimental JIT 0.35 times its time and a composite score 3.5
times as high. Single operations cost a little less: QuickJS's own
micro-benchmarks take 0.94 times as long on average, and 0.44 times with
the JIT, though a few take about twice as long, the weak collections and an
array grown by assigning past its end among them. See [Compared with C QuickJS](docs/benchmarks.md#compared-with-c-quickjs).

## Documentation

An experimental optimizing JIT compiles hot functions to native code on
`linux/amd64`, `windows/amd64` and `darwin/arm64`. It needs both a build with
`-tags quickjs_jit` and a runtime made with `quickjs.WithJIT()` (or
`qjs --jit`); anything else runs exactly as before. See
[its design notes](internal/jit/README.md).

| | |
|---|---|
| [Status and conformance](docs/status.md) | what is implemented and what is not, and how test262 is run against it |
| [Known issues](docs/known-issues.md) | the issues a review of the engine found, by severity, and what became of each |
| [The qjs command](docs/qjs.md) | running JavaScript from the command line, with only the capabilities you allow |
| [Embedding the engine](docs/embedding.md) | calling Go from JavaScript and JavaScript from Go, compiled programs, realms, modules |
| [Building a host](docs/hosting.md) | work that finishes on other goroutines, async context, closing a runtime, moving values between runtimes |
| [The standard library](docs/stdlib.md) | the console, timers, fetch, the filesystem, workers and the node modules, each a capability of its own |
| [Sandboxing](docs/sandboxing.md) | what a script can reach, and how its time and memory are bounded |
| [Debugging](docs/debugging.md) | Chrome's DevTools and VS Code attached to a runtime, from qjs --inspect or a host |
| [Design notes](docs/design.md) | how the engine is built, for reading the source |
| [Benchmarks](docs/benchmarks.md) | Go benchmarks, and the V8 suite against goja, C QuickJS and Node |
| [Performance plan](docs/performance-plan.md) | the optimizations still to make, how each is measured, and what was tried and rejected |
| [jsregexp](jsregexp/README.md) | the engine's regular expressions as a Go package, against Go's `regexp` |

## License

MIT, matching the upstream project.

`internal/fdlibm`, which computes `Math`, is ported from other code under
its own terms, kept in [its LICENSE](internal/fdlibm/LICENSE): fdlibm, by way
of V8's `src/base/ieee754.cc` (the fdlibm notice and V8's BSD license), and
the `pow` of [Arm's Optimized Routines][arm-aor] (MIT).

[QuickJS-NG]: https://github.com/quickjs-ng/quickjs
[test262]: https://github.com/tc39/test262
[go-intl]: https://github.com/go-quickjs/go-intl
[arm-aor]: https://github.com/ARM-software/optimized-routines
