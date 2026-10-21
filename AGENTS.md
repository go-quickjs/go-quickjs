# AGENTS.md

This file applies to the entire repository.

## Project overview

`go-quickjs` is a pure-Go JavaScript engine modeled after QuickJS-NG. Keep it
portable: production code must not require cgo, WebAssembly, a C compiler, or
platform-specific runtime dependencies.

The main areas are:

- `quickjs.go` and other root files: the public embedding API and public
  behavior tests.
- `internal/lexer`, `internal/parser`, `internal/ast`: source processing.
- `internal/compiler`, `internal/bytecode`: compilation and VM instructions.
- `internal/vm`: JavaScript runtime semantics and built-ins. `Intl`, `Date`'s
  local time and `Temporal` are bindings over
  [go-intl](https://github.com/go-quickjs/go-intl), which holds the locale
  data, the calendars and the time zones.
- `stdlib`: opt-in host functionality.
- `cmd/qjs`: the command-line host.
- `conformance`: the test262 runner. The test262 checkout is external.

The module targets Go 1.24. A `Runtime` is not safe for concurrent use; callers
give each goroutine its own runtime. Process-wide immutable data and caches,
go-intl's included, must still be safe when separate runtimes use them
concurrently.

## Working rules

- Preserve existing behavior unless the task explicitly changes it. This is a
  language engine, so apparently small coercion, property-order, error-type,
  locale, or rounding changes can affect many unrelated tests.
- Prefer focused changes in the owning layer. Do not work around a parser or
  compiler bug in the VM, or a VM bug in the public wrapper.
- Keep the public API small and document exported identifiers.
- Run `gofmt` on every changed Go file.
- `internal/vm` is sensitive to code layout. Go lays out a package's
  functions in file-name order, then all its closures, each aligned to 32
  bytes, so adding, moving or renaming code there -- even code a benchmark
  never runs -- can move the V8 suite by 5 to 10%. Do not rename or reorder
  its files or move code between them without comparing builds with
  `internal/cmd/v8bench/placements` (see Validation); the `zcall_*.go` and
  `ztree_*.go` files sort last on purpose, so that what is added there moves
  nothing before it.
- What decides the speed is how much of the hot path AMD Zen 2's op cache
  holds: 64 sets of 8 entries, each set taking the code at one offset in
  every 4 KB. Hot code that a change moves onto the same sets -- even by
  moving it, its instructions unchanged -- is decoded again on every pass,
  and a loop can run a quarter slower. So keep the interpreter's hot
  functions (`executeAt`, `runTree`, `runTreeNested`, `callObject`,
  `callTree`, `runFD`, `backEdgeCheck`) as they are -- a tree node that
  needs what one of `executeAt`'s cases does gets its own copy, rather than
  that case moving into a helper -- and keep the distance between the hot
  functions and the tree nodes' hot closures: every function comes before
  every closure, so a function added even in the last file moves every
  closure. Code the tree tier adds is therefore written in a `ztree_*.go`
  file as package variables, each assigned a closure in the file's `init`,
  which is laid out after every closure there was.
- What a newer Go adds -- generic methods, in Go 1.27 -- may be offered in a
  file built only with it, `//go:build go1.27`, which gives that file the
  newer language whatever go.mod says, as long as what it offers is there for
  the older Go too: `buffers_go127.go`'s method is `NewTypedArray`'s function.
  An older gofmt cannot parse such a file, so CI formats with the newest Go
  and tests with both.
- Add a regression test for every semantic bug. Prefer an exact result and
  error type over a broad smoke test.
- Do not weaken conformance expectations, add skips, or change expected
  failures merely to make a suite green. Establish the behavior in the
  applicable specification and, for compatibility work, in current engines.
- Run a file the same way in every engine you compare. `qjs file.js` loads
  a `.js` file as Node does, as a CommonJS module: its top-level `var` and
  function declarations belong to the module, not to the global object, and
  `this` is `module.exports`. C QuickJS's `qjs` runs a file as a classic
  script. So for script semantics -- globals, `globalThis` properties a
  `var` made, test262's global code, QuickJS's `microbench.js`, which finds
  its tests through `this[name]` -- run go-quickjs's `qjs` with `-s`
  (`--script`), and Node with `vm.runInThisContext`. A difference that comes
  down to a file's top-level bindings is usually this, not an engine bug:
  check it before reporting one. Go tests that call `Runtime.Eval` already
  run scripts.
- Preserve unrelated worktree changes. Do not rewrite history or use
  destructive Git commands unless explicitly requested.

## Generated files

Do not manually edit generated files or their binary companions:

- `internal/regexp/unicodetables.go`
- `internal/regexp/exec_ascii.go` and `internal/regexp/exec_utf8.go`, the
  regexp matchers over an ASCII string's bytes and over UTF-8 by code point,
  made from `exec.go`'s matcher over UTF-16 code units so that they stay the
  same code; the UTF-8 one is patched where `internal/asciigen` says. After
  changing `exec.go`, run `go generate ./internal/regexp`.
  `TestASCIIMatcherGenerated` fails while either file is out of date.
- `internal/vm/tree_operand.go`, the tree tier's nodes for an operator over
  each pairing of a local, an upvalue, a number constant and a tree, written
  by `internal/vm/internal/treegen`. After changing the generator, run
  `go generate ./internal/vm`; `TestTreeOperandsGenerated` fails while the
  file is out of date.

go-intl's data is generated in go-intl, and changes there; a new go-intl is
taken with `go get` and `go mod tidy`, never by copying its files here.

Run generators from the repository root. Use a temporary output file for
generators that write Go source to stdout so a failed run cannot truncate the
tracked file.

Unicode regular-expression tables require a test262 checkout:

```sh
generated=$(mktemp /tmp/quickjs-unicode.XXXXXX.go)
go run ./internal/regexp/internal/unicodegen /path/to/test262 > "$generated" && \
  mv "$generated" internal/regexp/unicodetables.go
gofmt -w internal/regexp/unicodetables.go
```

## Intl and data-loading invariants

- Ordinary runtime construction must not read Intl data. A service reads
  go-intl's data when it is first used: `Intl` is built the first time the
  global is read, and Temporal's calendars and zones are loaded when a
  Temporal value first needs them.
- go-intl's formatters are immutable and safe to share. What the engine keeps
  per runtime -- its locale, its zone, the `Date` environment, Temporal's
  data -- lives on the `Runtime`, never in package variables.
- A difference from Node is one of go-intl's named divergences, chosen in
  `intlCompat`: standards mode takes the standard's side, `WithNodeQuirks`
  Node's.
- `WithNodeQuirks` covers the language too. Where V8 departs from the
  standard, standards mode follows the standard and test262, and
  `WithNodeQuirks` follows V8, which the parser and compiler are told through
  their `NodeQuirks` options. Each such difference gets a test pinning both
  answers, as `TestNodeQuirksLanguage` does, and a line in the option's
  documentation.
- Error messages Intl and Temporal throw are V8's, word for word.
- Keep `new Date().toString()`, `new Date().toTimeString()`, and Intl time-zone
  names aligned with the selected Node/ICU data across locales, zones, seasons,
  and historical transitions.
- When Test262 disagrees with current Node and Chrome, retain a focused
  regression test documenting the observed engine behavior rather than
  silently changing the result to satisfy one stale expectation.

## Validation

Start with the smallest relevant package while iterating, then run the full
suite before handing off a substantial change:

```sh
go test ./...
git diff --check
```

For concurrency or shared-cache changes:

```sh
go test -race -count=1 -run TestIntlConcurrentRuntimes .
```

For portability-sensitive changes:

```sh
GOOS=windows GOARCH=amd64 go build ./...
GOOS=windows GOARCH=arm64 go build ./...
GOARCH=386 go build ./...
GOARCH=386 go test . -run TestLengthsPastAnInt32
```

An int is 32 bits on 386 and arm: convert an index, a length or an offset
from a uint32 or an int64 only once it is known to fit.

For Intl changes, also run the exact golden tests:

```sh
go test . -run '^(TestIntlFormats|TestIntlMatchesICU)$' -count=1 -v
```

For changes to dates, zones or Temporal, or a new go-intl, compare with Node
across every locale DateTimeFormat has and every zone Node lists. It needs a
Node with Temporal, runs shards in parallel, one per CPU unless
`QUICKJS_NODE_TEMPORAL_PARALLEL` says otherwise, and takes about ten minutes:

```sh
QUICKJS_COMPARE_NODE_TEMPORAL=1 go test . \
  -run '^TestTemporalMatchesNodeAcrossLocalesAndTimeZones$' -count=1 -timeout=60m -v
```

If `/tmp/test262` is available, run the relevant conformance area. For Intl:

```sh
TEST262_DIR=/tmp/test262 go test ./conformance \
  -run TestConformance -count=1 -timeout 30m -v \
  -args -conformance.dir=intl402 -conformance.max-failures=20
```

For parser, compiler, VM, or built-in changes, use the narrowest applicable
`-conformance.dir` while iterating, then broaden coverage in proportion to the
change. A compiler change runs test262 a second time with
`-conformance.debugger`, every runtime made `WithDebugger`: code compiled for
a debugger has an instruction at each statement no other code has, and must
do exactly what other code does. A passing Go test command is not enough if
its log reports newly introduced conformance failures.

Performance-sensitive work should compare fresh processes, not just repeated
operations in one warmed process. Report bundle size, first-use latency,
allocations, and peak RSS. Distinguish work moved from package startup to first
feature use.

`internal/cmd/v8bench` runs the V8 v7 suite: `-mode fixed` does the same
work on every run, which is what compares two builds (build each to a binary
and alternate them), `-mode compile` is the parser and compiler alone, and
`-mode score` gives the suite's own scores. `-cpuprofile` and `-memprofile`
write profiles.

```sh
go run ./internal/cmd/v8bench -dir /tmp/v8-v7 -fetch -mode fixed -n 5
```

`-bench` picks another suite, fetched the same way into a directory of its
own: `octane`, Octane 2.0 -- the V8 suite's benchmarks revised, and larger
programs: TypeScript, pdf.js, a Game Boy emulator, Box2D, zlib, Mandreel --
and `awfy`, Are We Fast Yet's fourteen benchmarks, written to compare
language implementations alike; the goja and external runners below take
`-bench` too. They reach far more of the JIT than the V8 suite does. With
`-jit`, `-jitreport` prints after each benchmark what the
JIT did: what it compiled, by how much each function ran natively, and what
it refused or never tried, with why. The runner gives the JIT a 32 MiB
budget (`-jitmem`), not the runtime's default 8 MiB, which Typescript's code
does not fit; `-jitmem 0` runs with the default.

```sh
go run -tags quickjs_jit ./internal/cmd/v8bench -jit -jitreport -bench awfy -dir /tmp/awfy -fetch -mode fixed -n 1
```

`-mode micro` runs QuickJS's micro-benchmarks, `internal/v8bench/microbench.js`
(QuickJS's `tests/microbench.js`, kept as it is there), and ours beside it,
`microbench_extra.js`, of what it has none of -- generators, async
functions, try statements, iterators, using declarations, closures
made and called anew, calls in tail position, method calls with a frame
and without, loops of a few simple instructions, which with `QJS_NOTREE=1`
time the interpreter's dispatch, private class members, Math.random, the in operator, a delete of an object's last property,
Array.prototype.forEach, reduce, map, filter and some, a typed array's forEach,
String.prototype.indexOf and Array.prototype.indexOf, BigInts' division,
shifts, hashing, comparisons, powers, modular exponentiation, digits of
pi and decimal strings --
written as
its tests are: each test is given
the same work every run -- what QuickJS takes about 150 ms for, the work
the comparison with QuickJS in docs/benchmarks.md is made with -- `-n`
times, and the fastest time of one operation is printed. It needs no
`-dir`; `-suite` names tests by the start of their names. Every runner
below has it, so the four engines run the same driver.

```sh
go run ./internal/cmd/v8bench -mode micro -n 1 -suite empty_loop,func_call
```

To compare two builds, use `internal/cmd/v8bench/placements`: one build of
each side compares where the linker put package vm's hot code as much as
the code. `build` makes eight builds of the working tree, each placing
`executeAt`, the call path, `runTree` and the tree's closures differently in
a balanced design, and checks every one with `go tool nm`; `compare` runs two
labels' placements in turn and reports each side's mean. Build both sides in
the same checkout, and leave the machine idle while they run.

```sh
git checkout base   && go run ./internal/cmd/v8bench/placements build -label base
git checkout change && go run ./internal/cmd/v8bench/placements build -label change
go run ./internal/cmd/v8bench/placements compare -dir /tmp/v8-v7 base change
```

`-v` prints each placement's figure: a change that is slower at some
placements and level at the others is placement, not the change.

`-parallel` runs both labels at once, each on a group of CPUs of its own
-- the fastest cores', a level 3 cache's, one NUMA node's, found by
`-cpus auto` or given as `-cpus "0-5;6-11"` -- the groups swapped from one
run to the next, and `-k 4` takes four of the eight placements. Screen a
change so, in minutes, and confirm a batch of them with the eight before
pushing; compare a label with itself in parallel first on a machine new to
it, to see that its groups measure alike.

```sh
go run ./internal/cmd/v8bench/placements compare -parallel -k 4 -bench octane -dir /tmp/octane base change
```

`internal/cmd/v8bench/goja` runs the same suite the same way on goja, for
comparing the two engines. It is a module of its own, run from its
directory, so that go-quickjs never depends on goja; `TestNoGojaDependency`
fails if goja reaches go-quickjs's `go.mod` or `go.sum`.

```sh
(cd internal/cmd/v8bench/goja && go run . -dir /tmp/v8-v7 -mode fixed -n 5)
```

`internal/cmd/v8bench/external` runs it on an engine that is a program of
its own -- QuickJS's `qjs`, or `node` -- from a driver script that does the
same work, and reports the same times without the allocations Go counts.

```sh
go run ./internal/cmd/v8bench/external -engine qjs -cmd /path/to/qjs -dir /tmp/v8-v7 -mode fixed -n 5
```

`-arg` passes the program an argument of its own, once for each:
`-engine node -arg --jitless` runs Node with its JIT compilers off.

`internal/cmd/v8bench/matrix` makes the tables comparing the engines --
go-quickjs without and with the JIT, QuickJS, Node without and with its
JIT, goja -- from every runner above, built from the checkout: the V8
suite, Octane and Are We Fast Yet as milliseconds for the fixed work, each
the median of three fresh-process runs, with go-quickjs's time over
QuickJS's; the V8 suite's own scores; the micro-benchmarks' largest gaps.
Where the OS gives a process CPUs of its own it runs two at once, on groups
as `placements -parallel` finds them (`-parallel=false` one at a time).
It keeps every run's output, what the machine and the engines were, and
the tables, which `-report DIR` prints again without running anything.
Use it rather than a script of one's own.

```sh
go run ./internal/cmd/v8bench/matrix -qjs /path/to/qjs -v8 /tmp/v8-v7 -octane /tmp/octane -awfy /tmp/awfy -fetch
```

Each Go runner ends with the live heap after a collection, with the runtime
still alive: one that grows with `-n` is a leak.

The fixed mode prints, beside each benchmark's time, the CPU time the
process spent on it (`cpu`), the collector's on other threads included,
and the external runner its engine's whole: on a machine with cores to
spare the time does not show what collecting costs, which an embedder
running a runtime on each core pays. A change that moves what is
allocated is compared by both.

`internal/cmd/disasm` prints the bytecode a script or module compiles to,
each function's instructions under the source lines they come from; `-tree`
says whether the tree tier builds each function and, where it does not,
why, and `-func` picks functions by name. Look there first when a
micro-benchmark moves: it shows which instructions and which tier a loop
runs as.

```sh
go run ./internal/cmd/disasm -tree -func global_read bench.js
```

A change that keeps memory longer shows in neither: compare the conformance
run's peak memory as well, which is how a cache that outlived its objects was
caught.

## Releases

Every release is named the same way, in go-quickjs and in go-intl alike:

| | Form | Example |
|---|---|---|
| Tag | `vX.Y.Z`, annotated | `v0.9.1` |
| Tag message | `<module> vX.Y.Z` | `go-quickjs v0.9.1` |
| GitHub release title | `<module> vX.Y.Z` | `go-quickjs v0.9.1` |

`<module>` is the repository's name: `go-quickjs` or `go-intl`. Nothing else
goes in a title, such as a date, a codename or a summary; what a release
contains belongs in its notes.

```sh
git tag -a v0.9.1 -m "go-quickjs v0.9.1"
git push origin v0.9.1
gh release create v0.9.1 --repo go-quickjs/go-quickjs --verify-tag \
  --title "go-quickjs v0.9.1" --notes-file notes.md
```

Versions follow semantic versioning. Before 1.0, a new exported identifier or
a change in behavior bumps the minor version, and a release with only fixes
bumps the patch.

A go-quickjs release requires a tagged go-intl, never a pseudo-version: tag
and release go-intl first, then take it with `go get` and `go mod tidy`.

Before tagging, run the full suite and the checks in [Validation](#validation)
that apply to what changed since the last release. After publishing, confirm
that `proxy.golang.org` serves the new version.
