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

Of the 99,937 test262 variants in the areas the engine claims -- the language,
the built-ins, `Intl` and Annex B -- 99,595 pass, none fail, and 342 are
skipped because they require an unsupported feature or host facility. See [Conformance](docs/status.md#conformance) for the
measurement and [Not implemented](docs/status.md#not-implemented) for what is missing.

On the V8 benchmark suite as a whole it is on par with C QuickJS: it takes
less time over the suite and has a slightly higher composite score. Single
operations still cost more: QuickJS's own micro-benchmarks take about 1.4
times as long on average, and around three times as long for a few, small
BigInts' arithmetic among them. See [Compared with C QuickJS](docs/benchmarks.md#compared-with-c-quickjs).

## Documentation

| | |
|---|---|
| [Status and conformance](docs/status.md) | what is implemented and what is not, and how test262 is run against it |
| [The qjs command](docs/qjs.md) | running JavaScript from the command line, with only the capabilities you allow |
| [Embedding the engine](docs/embedding.md) | calling Go from JavaScript and JavaScript from Go, compiled programs, realms, modules |
| [Building a host](docs/hosting.md) | work that finishes on other goroutines, async context, closing a runtime, moving values between runtimes |
| [The standard library](docs/stdlib.md) | the console, timers, fetch, the filesystem, workers and the node modules, each a capability of its own |
| [Sandboxing](docs/sandboxing.md) | what a script can reach, and how its time and memory are bounded |
| [Design notes](docs/design.md) | how the engine is built, for reading the source |
| [Benchmarks](docs/benchmarks.md) | Go benchmarks, and the V8 suite against goja, C QuickJS and Node |
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
