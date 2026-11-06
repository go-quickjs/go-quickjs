# Benchmarks

On an Apple M5 Max with Go 1.27.3 and the native JIT disabled, taking
per-metric medians of five fresh-process runs at go-quickjs `3fd70f67` on
2026-10-31, with 30 seconds between runs and AC power throughout:

```
BenchmarkEvalArithmetic-18  1.30µs/op   4242 B/op    22 allocs/op
BenchmarkFibonacci-18        452µs/op     17 B/op     1 allocs/op
BenchmarkPropertyAccess-18  1.62µs/op   5380 B/op    26 allocs/op
BenchmarkCallGoFunction-18  1.76µs/op   5988 B/op    33 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop allocates nothing
per call; the other benchmarks include parsing and compiling their source each
iteration.

`go test -run '^$' -bench . -benchmem .` also runs compile-once interpreter
workloads, plus compilation, runtime construction, and a recursive factorial
workload. The remaining root-package benchmarks are:

```
BenchmarkLoopArithmetic-18         57.7µs/op        1 B/op     0 allocs/op
BenchmarkDateUTC-18                 381µs/op        1 B/op     0 allocs/op
BenchmarkLoopPropertyAccess-18      207µs/op        0 B/op     0 allocs/op
BenchmarkLoopArrayIndex-18         10.6µs/op        0 B/op     0 allocs/op
BenchmarkLoopFunctionCall-18        174µs/op        0 B/op     0 allocs/op
BenchmarkLoopMethodCall-18          248µs/op        0 B/op     0 allocs/op
BenchmarkAllocObjects-18           79.2µs/op   224 KB/op  2000 allocs/op
BenchmarkAllocArrays-18            70.5µs/op   320 KB/op  2000 allocs/op
BenchmarkStringConcat-18           37.4µs/op  97.2 KB/op  1999 allocs/op
BenchmarkClosureCreateAndCall-18    317µs/op  1.44 MB/op  5001 allocs/op
BenchmarkArrayCallbacks-18         44.2µs/op  36.3 KB/op    18 allocs/op
BenchmarkMapOperations-18           123µs/op   311 KB/op    25 allocs/op
BenchmarkRegExpExec-18              334µs/op   336 KB/op  2000 allocs/op
BenchmarkThrowCatch-18              242µs/op   704 KB/op  6000 allocs/op
BenchmarkJSONRoundTrip-18           503µs/op   612 KB/op  8500 allocs/op
BenchmarkFactorial-18              20.8ms/op   393 KB/op  1633 allocs/op
BenchmarkCompileScript-18          27.5µs/op  53.3 KB/op   465 allocs/op
BenchmarkNewRuntime-18             63.1µs/op   279 KB/op  1512 allocs/op
BenchmarkNewRuntimeSmallStack-18   66.2µs/op   277 KB/op  1512 allocs/op
```

Each compile-once benchmark runs its workload many times: the main loop
benchmarks use 10,000 iterations, the object and array benchmarks allocate
2,000 values, and the callback, collection, regexp, exception and JSON
benchmarks exercise their operation hundreds or thousands of times.
`CompileScript` parses, compiles and evaluates one program per operation,
while the runtime benchmarks construct and close one runtime per operation.
`Factorial` constructs one runtime and evaluates 100,000 calls to
`factorial(10)` per operation, totaling one million recursive calls
(20.8 ns/call). Each of the five samples came from a separate invocation,
with one second per benchmark and `-benchmem`, not repeated measurements
inside a single test process.

## JSON parsing and serialization

`BenchmarkJSONParse` and `BenchmarkJSONStringify` use a 161,481-byte UTF-8
document containing 1,000 records, nested objects and arrays, fractional
numbers, escaped strings and Unicode. Input construction, compilation and
runtime setup are outside the measured interval. Each operation parses or
serializes the whole document; `BenchmarkJSONRoundTrip` instead runs 500
small round trips per operation.

```sh
CGO_ENABLED=0 go test -c -pgo=off -trimpath -o /tmp/quickjs-json.test .
TZ=UTC GOMAXPROCS=1 /usr/bin/time -l /tmp/quickjs-json.test \
  -test.run '^$' -test.bench '^BenchmarkJSONStringify$' \
  -test.benchtime=1s -test.benchmem
```

Run each benchmark separately in fresh processes, alternating the two builds.
On an Apple M5 Max, macOS/arm64, Go 1.27.3, with JIT disabled and the settings
above, five runs per benchmark give these per-metric medians against
`4bd21ea0`:

| Operation | Before | After | Bytes allocated before / after | Allocations before / after |
|---|---:|---:|---:|---:|
| Parse | 690 us | 683 us | 876,163 / 876,163 | 12,015 / 12,015 |
| Stringify | 1,093 us | 803 us | 1,114,450 / 850,801 | 11,268 / 25 |
| 500 round trips | 598 us | 534 us | 612,013 / 544,012 | 8,500 / 6,500 |

Serialization creates callback key strings only when a callback observes
them, snapshots small ordinary objects' keys on the stack, and writes
fractional numbers directly into the output buffer while preserving
JavaScript's number-formatting rules. The parser is unchanged; its timing
difference is measurement variation.

With `-test.benchtime=1x`, five fresh processes measure the first stringify
after fixture setup at 972 us before and 705 us after, with 11,293 versus
50 allocations. This excludes runtime construction and fixture setup but
includes the first use of the JSON builtin. No work moves to later use and
no persistent cache is added. Peak process RSS for the one-second runs,
including untimed setup, is 41.36 / 41.02 MiB for stringify, 52.88 / 53.09 MiB
for parse and 30.36 / 30.45 MiB for round trips. The `qjs` binaries are both
42,055,698 bytes. Four balanced V8 code placements, two comparison rounds,
give 194.4 / 195.6 ms for fixed work, essentially level.

## Loading large bundles

The optional open-source corpus uses two unmodified MIT-licensed upstream
builds: [Three.js r160][three-bundle] (1,315,993 bytes, [license][three-license])
and [Babel standalone 7.26.10][babel-bundle] (3,014,882 bytes,
[license][babel-license]). Three.js exercises a large unminified library;
Babel adds a larger minified bundle with many nested functions. The
tests pin each file's SHA-256 and run without a network connection. Download
the corpus once, outside the checkout, as with the V8 suites:

```sh
go run ./internal/cmd/bundlefetch -dir /tmp/quickjs-bundles
export QUICKJS_BUNDLE_DIR=/tmp/quickjs-bundles

go test . -run '^TestOpenSourceBundles$' -count=1
GOMAXPROCS=1 go test ./internal/compiler -run '^$' \
  -bench '^BenchmarkCompileBundle$' -benchmem
```

`BenchmarkCompileBundle` parses once outside the measured interval and
compiles the AST anew each iteration, reporting compiler allocations.
`BenchmarkLoadBundle` measures parsing, compilation and execution in a new
runtime each iteration. Runtime construction, file reads, host setup and
checks are outside its interval. Both bundles run as classic scripts, as
`Runtime.EvalFile` does; the host supplies a console stub. The checks exercise
Three.js vector operations and use Babel to transform an arrow function,
with exact expected results. Without `QUICKJS_BUNDLE_DIR` the optional tests
and benchmarks skip; with it, missing files or checksum mismatches fail.

For first-use latency, build once and run one operation in each fresh
process, alternating the two builds being compared:

```sh
CGO_ENABLED=0 go test -c -pgo=off -trimpath -o /tmp/quickjs-load.test .
TZ=UTC GOMAXPROCS=1 /usr/bin/time -l /tmp/quickjs-load.test \
  -test.run '^$' -test.bench '^BenchmarkLoadBundle/Three$' \
  -test.benchtime=1x -test.benchmem
```

Choose `Babel` separately so that each fresh process loads just one bundle.
On macOS, `time -l` reports peak process RSS; Linux's equivalent is `time -v`.

On an Apple M5 Max, macOS/arm64, Go 1.27.3, `GOMAXPROCS=1`, `TZ=UTC`,
with `CGO_ENABLED=0 -pgo=off -trimpath` and JIT disabled, the compiler
changes compared with `925cf47c` give these medians. Compiler figures use
five fresh processes with 500 ms per benchmark; load figures use twenty
fresh processes per bundle, one load per process. Builds run sequentially
in alternating order, with no concurrent builds or tests:

| Bundle | Compile before | Compile after | Load before | Load after |
|---|---:|---:|---:|---:|
| Three.js | 17.89 ms | 14.52 ms | 40.77 ms | 36.46 ms |
| Babel | 156.07 ms | 43.12 ms | 303.38 ms | 198.66 ms |

Large functions now index local names from 32 bindings onward, restoring
shadowed bindings on scope exit; smaller functions keep linear lookup.
Compiler state and the buffers used to collect closure captures are reused
with the existing scratch buffers. Returned functions own exact-size copies
of their tables, and released compiler state is zeroed so the pool retains
neither the source nor settings from a previous function. Work is not moved
to later feature use.

Before/after memory figures for the same runs are below. Allocations are
total bytes allocated per operation, in decimal MB. Peak RSS is the whole
test process, including its untimed setup and functional checks, in MiB:

| Bundle | Compiler MB before / after | Load MB before / after | Peak RSS MiB before / after |
|---|---:|---:|---:|
| Three.js | 8.18 / 6.25 | 25.97 / 24.24 | 47.41 / 47.35 |
| Babel | 26.07 / 16.62 | 115.37 / 109.44 | 112.08 / 116.68 |

Babel's peak RSS rises by 4.60 MiB despite the lower allocation totals.
The language/Annex B conformance run's peak RSS changes from 546.19 to
557.30 MiB in a separate single-process comparison. The `qjs` binary grows
by 432 bytes, from 42,055,266 to 42,055,698 bytes. V8 fixed work (`-n 5`,
three alternating fresh processes per build) stays level: 1160.2 versus
1161.6 ms, with 9.0 MB live after GC in both builds. The hot VM functions
have identical addresses in these builds.

[three-bundle]: https://unpkg.com/three@0.160.0/build/three.js
[three-license]: https://github.com/mrdoob/three.js/blob/r160/LICENSE
[babel-bundle]: https://unpkg.com/@babel/standalone@7.26.10/babel.min.js
[babel-license]: https://github.com/babel/babel/blob/v7.26.10/LICENSE

## V8 version 7 benchmark suite

`go run ./internal/cmd/v8bench -dir /tmp/v8-v7 -fetch -mode score` runs the
suite on go-quickjs; see that command for its other modes. Run from
`internal/cmd/v8bench/goja`, `go run . -dir /tmp/v8-v7 -mode score` runs it
the same way on goja. That runner is a Go module of its own, so go-quickjs
does not depend on goja. `go run ./internal/cmd/v8bench/external -engine qjs
-cmd /path/to/qjs -dir /tmp/v8-v7 -mode score` runs it on QuickJS itself, or
with `-engine node` on Node. `-arg` passes the program an argument, once for
each: `-engine node -arg --jitless` runs Node without its JIT compilers.

For a comparison with another pure-Go JavaScript engine, the Mozilla
[AreWeFastYet V8 version 7 suite][v8-v7] was run against go-quickjs and
[goja]. All three go-quickjs columns use revision `3ea976f4`, measured alongside
goja on 2026-11-06. Each column is the median of three fresh-process runs
on an Apple M5 Max with Go 1.27.3, with 30 seconds between runs and AC
power throughout. One engine ran at a time, with the order rotated each
round. JIT off was built without the `quickjs_jit` tag. JIT on compiles
synchronously; JIT background enables the JIT and compiles in the background
with `-jit -jitbackground`. Both use the same binary built with
`-tags quickjs_jit` and the runner's 32 MiB JIT budget rather than the
runtime's default 8 MiB.
Higher suite scores are better; the relative columns divide each
go-quickjs score by goja's.

| Workload | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off / goja | JIT on / goja | JIT background / goja |
|---|---:|---:|---:|---:|---:|---:|---:|
| Richards | 2,588 | 12,931 | 12,585 | 524 | 4.94x | 24.68x | 24.02x |
| DeltaBlue | 2,735 | 13,197 | 12,668 | 650 | 4.21x | 20.30x | 19.49x |
| Crypto | 3,211 | 17,767 | 17,632 | 330 | 9.73x | 53.84x | 53.43x |
| RayTrace | 6,142 | 25,134 | 31,080 | 786 | 7.81x | 31.98x | 39.54x |
| EarleyBoyer | 7,441 | 12,525 | 12,349 | 1,447 | 5.14x | 8.66x | 8.53x |
| RegExp | 8,654 | 9,910 | 10,294 | 572 | 15.13x | 17.33x | 18.00x |
| Splay | 9,942 | 15,206 | 16,404 | 2,485 | 4.00x | 6.12x | 6.60x |
| NavierStokes | 5,866 | 38,732 | 38,617 | 550 | 10.67x | 70.42x | 70.21x |
| **Composite score** | **5,178** | **16,602** | **16,635** | **748** | **6.92x** | **22.20x** | **22.24x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
go-quickjs and goja runs:

| Metric | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off relative to goja |
|---|---:|---:|---:|---:|---:|
| Suite wall time | 22.28 s | 20.12 s | 20.12 s | 52.04 s | 2.34x faster |
| Suite CPU time | 26.12 s | 25.80 s | 26.23 s | 73.61 s | 64.5% less |
| Total allocation | 8.16 GiB | 14.93 GiB | 14.86 GiB | 33.98 GiB | 76.0% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. All
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `3ea976f4`, goja `a177d211bf60`, and
AreWeFastYet `0e21608`. The isolated goja runner pins
`v0.0.0-20261102223426-a177d211bf60` for this comparison. These results
characterize this older workload suite rather than every application;
go-quickjs leads all eight of its workloads in this comparison.

The comparison used `internal/cmd/v8bench/matrix`, selecting `gq`, `gqjit`,
`gqjitbg` and `goja` in separate invocations with `-suites score -runs 1
-parallel=false -v8 /tmp/v8-v7` and a separate `-out` directory for each
sample, with 30-second pauses. The engine orders were `gq,gqjit,gqjitbg,goja`,
`gqjit,gqjitbg,goja,gq` and `gqjitbg,goja,gq,gqjit`. `-report` combined the saved outputs
into the median score table. Each engine's three binaries were
byte-identical; both JIT modes used the same binary. The only tracked
changes during measurement added matrix support for the background JIT
selector and its report tests. The engine code matched the revision above.

The composite samples were:

| Mode | Run 1 | Run 2 | Run 3 |
|---|---:|---:|---:|
| JIT off | 5,359 | 5,034 | 5,178 |
| JIT on | 17,247 | 16,167 | 16,602 |
| JIT background | 16,635 | 16,329 | 17,166 |
| goja | 743 | 750 | 748 |

All samples were included. The table takes the median of each workload and
of the printed composite scores independently. Background compilation's
composite was 0.2% higher than synchronous compilation's, within the spread
of these samples; this run does not establish an overall throughput gain.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is ahead of QuickJS. Its
interpreter takes 0.54 times QuickJS's time and has a 42% higher composite
score: it is level on Richards and DeltaBlue and ahead on the other six.
With its JIT it takes 0.27 times QuickJS's time, scores 4.7 times as
much, and is ahead on all eight. On the larger programs of Octane and Are
We Fast Yet the interpreter is ahead too, 0.92 and 0.98 times QuickJS's
time, and the JIT takes 0.51 and 0.23 times
([below](#octane-and-are-we-fast-yet)); a page React 18 renders on the
server takes 0.90 times, and 0.66 with the JIT
([below](#server-rendering)). Single operations cost less on
average, 0.85 times QuickJS's time, and 0.32 times with the JIT, as
[the micro-benchmarks](#micro-benchmarks) below show, though a few still
take about twice as long.

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The JIT, which is experimental and optional, compiles hot functions
to native code (see its [design notes](../internal/jit/README.md)). The
same suite ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.3,
go-quickjs `915400c` (after v0.32.0) and QuickJS 2026-06-04. Two runs
went at once, each on the six CPUs of a level 3 cache of its own. The
JIT's column is a build with `-tags quickjs_jit` run with `-jit`. The
figures are milliseconds for the same fixed work, the median of three
fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 12.1 ms | 8.3 ms | 12.0 ms | 1.01x | 0.69x |
| DeltaBlue | 24.3 ms | 21.3 ms | 24.5 ms | 0.99x | 0.87x |
| Crypto | 291 ms | 72.4 ms | 342 ms | 0.85x | 0.21x |
| RayTrace | 121 ms | 65.9 ms | 150 ms | 0.81x | 0.44x |
| EarleyBoyer | 337 ms | 128 ms | 411 ms | 0.82x | 0.31x |
| RegExp | 150 ms | 136 ms | 870 ms | 0.17x | 0.16x |
| Splay | 222 ms | 187 ms | 510 ms | 0.44x | 0.37x |
| NavierStokes | 193 ms | 38.4 ms | 196 ms | 0.98x | 0.20x |
| **Total** | **1,378 ms** | **687 ms** | **2,551 ms** | **0.54x** | **0.27x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 918 | 4,719 | 937 | 0.98x | 5.04x |
| DeltaBlue | 872 | 5,051 | 802 | 1.09x | 6.30x |
| Crypto | 1,244 | 6,734 | 1,091 | 1.14x | 6.17x |
| RayTrace | 1,817 | 10,255 | 1,512 | 1.20x | 6.78x |
| EarleyBoyer | 2,464 | 4,270 | 2,068 | 1.19x | 2.06x |
| RegExp | 2,904 | 3,594 | 422 | 6.88x | 8.52x |
| Splay | 3,777 | 5,251 | 3,097 | 1.22x | 1.70x |
| NavierStokes | 2,616 | 14,517 | 2,329 | 1.12x | 6.23x |
| **Composite** | **1,825** | **6,012** | **1,285** | **1.42x** | **4.68x** |

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make most of the interpreter's lead. The composite score is the
  geometric mean of the workloads' scores, so each counts the same:
  Richards weighs as much as RegExp. By that mean the interpreter takes
  0.67 times QuickJS's time, and the JIT 0.34 times, where their scores
  are 42% higher and 4.7 times as high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over. The scores time
  warm runs, where Richards, DeltaBlue, Crypto, RayTrace and
  NavierStokes score 5.1 to 5.8 times what the interpreter does.
- Richards is level in fixed time and 2% behind in its score, where it
  was 15% and 11% behind at `217bd7f`: a method of a property
  of `this` is called as one node (`00e761a`), and an equality as a value
  is one node (`5544686`). This page had it level at v0.27.0, but that
  came from the
  clocks. The runners then timed go-quickjs with the clock Windows gives
  Go, which moves only every 0.5 ms, or every 15.6 ms while no program
  asks for more, and QuickJS with `Date.now`, which counts whole
  milliseconds. Both now read the performance counter (`ab0ef0b`), and
  timed that way v0.27.0 takes 13.0 ms on Richards, about what main takes.
- Splay's lead is in its fixed time. That is almost all setup and
  teardown: building a tree of 8,000 nodes and dropping it, which QuickJS
  frees by counting references where Go's collector frees it later. A
  warm run, which is what the score times, is splay-tree operations, calls
  and property reads, and is level with QuickJS's. Giving the collector
  four times the room (`GOGC=400`) leaves Splay's score as it was, so
  collection is not what the score measures.
- The JIT changes least where the time is spent in Go code it calls
  rather than in the script's own: the allocation-heavy EarleyBoyer and
  Splay, 1.7 and 1.4 times the interpreter's score, and RegExp, 1.2 times,
  though a runtime with the JIT compiles its patterns to machine code as
  V8's Irregexp does (`2be0068`).

Figures move by a few percent between builds from code placement alone,
and Richards, DeltaBlue and Crypto by more, up to a tenth. A build of
v0.28.0 and one of `3714c86`, made in two checkouts, had the
interpreter's Crypto 20% slower on main while another process ran; over
eight placements of each, made in one checkout, main took 2.6% longer
(96.1 against 93.7 ms a run), level at five of the eight. The extra time
is in `elemAt`, whose instructions are the same in both builds; only its
place in the op cache differs, and running beside another process makes
a bad place worse. To reproduce them, the
suites below and the micro-benchmarks, run `go run
./internal/cmd/v8bench/matrix -engines gq,gqjit,qjs -qjs /path/to/qjs -v8
/tmp/v8-v7 -octane /tmp/octane -awfy /tmp/awfy -ssr /tmp/ssr -fetch`.

### Octane and Are We Fast Yet

Octane 2.0 is the V8 suite's benchmarks revised, with larger programs
added: TypeScript's compiler, pdf.js, a Game Boy emulator, Box2D, zlib
and Mandreel, the last two compiled from C and C++. Are We Fast Yet's
fourteen benchmarks are written to compare language implementations
alike, each in the same style in every language. Both reach far more of
the language and of the JIT than the V8 suite does. They ran in the same
matrix, the same way: milliseconds for the fixed work, the median of
three fresh-process runs (lower is better).

| Octane | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 4.2 ms | 4.5 ms | 4.0 ms | 1.05x | 1.12x |
| DeltaBlue | 7.8 ms | 8.7 ms | 7.8 ms | 1.00x | 1.12x |
| Crypto | 96.4 ms | 28.5 ms | 112 ms | 0.86x | 0.26x |
| RayTrace | 41.8 ms | 42.1 ms | 51.1 ms | 0.82x | 0.82x |
| EarleyBoyer | 113 ms | 52.7 ms | 141 ms | 0.80x | 0.37x |
| RegExp | 123 ms | 113 ms | 470 ms | 0.26x | 0.24x |
| Splay | 200 ms | 135 ms | 472 ms | 0.42x | 0.29x |
| NavierStokes | 66.1 ms | 18.2 ms | 67.5 ms | 0.98x | 0.27x |
| PdfJS | 274 ms | 260 ms | 377 ms | 0.73x | 0.69x |
| Mandreel | 3,354 ms | 1,634 ms | 3,380 ms | 0.99x | 0.48x |
| Gameboy | 507 ms | 339 ms | 471 ms | 1.08x | 0.72x |
| CodeLoad | 14.8 ms | 15.3 ms | 13.1 ms | 1.13x | 1.17x |
| Box2D | 136 ms | 128 ms | 157 ms | 0.87x | 0.82x |
| zlib | 7,844 ms | 3,107 ms | 8,370 ms | 0.94x | 0.37x |
| Typescript | 2,227 ms | 2,227 ms | 2,195 ms | 1.01x | 1.01x |
| **Total** | **15,498 ms** | **8,600 ms** | **16,876 ms** | **0.92x** | **0.51x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 869 ms | 324 ms | 726 ms | 1.20x | 0.45x |
| Richards | 4,903 ms | 812 ms | 4,900 ms | 1.00x | 0.17x |
| Json | 1,278 ms | 282 ms | 1,382 ms | 0.92x | 0.20x |
| CD | 2,613 ms | 538 ms | 2,895 ms | 0.90x | 0.19x |
| Havlak | 5,012 ms | 1,752 ms | 4,566 ms | 1.10x | 0.38x |
| Bounce | 1,291 ms | 253 ms | 1,507 ms | 0.86x | 0.17x |
| List | 1,181 ms | 190 ms | 1,268 ms | 0.93x | 0.15x |
| Mandelbrot | 392 ms | 42.6 ms | 697 ms | 0.56x | 0.06x |
| NBody | 982 ms | 128 ms | 1,035 ms | 0.95x | 0.12x |
| Permute | 1,592 ms | 233 ms | 1,698 ms | 0.94x | 0.14x |
| Queens | 1,130 ms | 131 ms | 1,072 ms | 1.05x | 0.12x |
| Sieve | 1,116 ms | 316 ms | 1,225 ms | 0.91x | 0.26x |
| Storage | 1,702 ms | 821 ms | 1,673 ms | 1.02x | 0.49x |
| Towers | 1,667 ms | 219 ms | 1,561 ms | 1.07x | 0.14x |
| **Total** | **25,759 ms** | **6,071 ms** | **26,299 ms** | **0.98x** | **0.23x** |

- Without the JIT, go-quickjs leads where the V8 suite has it lead --
  RegExp, Splay, the arithmetic of Crypto and NavierStokes, Mandelbrot's
  loops -- and is behind on code that is mostly calls and allocation, by
  as much as 20% (Are We Fast Yet's DeltaBlue) and 10% (Havlak) in this
  run. DeltaBlue moves most from one run to the next: it was 18% behind in
  the run at `9e9f029`, and v0.31.1 and `6a53e07`, five fresh runs each,
  took 924 and 888 ms (their medians). Their classes' common steps -- a field
  read into a local, a comparison with `null`, a field's element, a
  method of a field called, a return -- are now single nodes of the
  tree, and an arrow reads its `this` in place (`6b7a140` to `2d0acf3`);
  List and Towers, behind then too, are ahead and level. Of Octane's long
  programs, Typescript's compiler is 1% behind and CodeLoad's parsing 13%,
  and pdf.js is 27% ahead. The code compiled from C and C++ is level or
  ahead: zlib's 6% ahead, where it was 20% behind at
  `3714c86`, its bitwise arithmetic and its elements at int32 keys run as
  int32 trees since `cf27162`, `5e22bb2` and `f838b8c`; Mandreel's level,
  where it was 19% behind at `882b9d0`, its globals' elements read and
  written as single nodes (`9d03a00`, `cbc097d`) and its negative
  literals taken as immediates (`3f790df`).
- With the JIT, Are We Fast Yet takes 0.06 to 0.49 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Havlak, Storage -- the most.
- On five of Octane's fifteen the JIT is level with the interpreter or
  behind it: level on RayTrace, CodeLoad and Typescript, behind on
  DeltaBlue (12%) and Richards (7%); RayTrace was 22% behind at
  `6a53e07`. Octane's fixed work is one run of
  each benchmark, so compiling counts: DeltaBlue's 16 compiles took about
  2.5 ms of its 8.5 at `3714c86`, and a single run's time moves: with
  the JIT, five fresh runs of RayTrace took 42 to 49 ms at `6a53e07`, and
  44 to 49 at v0.31.1. Typescript, 4% behind in the run at `9e9f029`, and
  RegExp, 5% behind, are level and 8% ahead.

### Server rendering

The ssr suite renders a page with React 18's own builds, as a host behind
a web server does for each request: Render is one runtime rendering the
page again and again, FirstRender five fresh runtimes, each loading React
and rendering once. The same matrix, the median of three fresh-process
runs (lower is better):

| ssr | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Render | 1,715 ms | 1,261 ms | 1,899 ms | 0.90x | 0.66x |
| FirstRender | 210 ms | 392 ms | | | |

QuickJS's runner loads a script once, so it has no FirstRender. A fresh
runtime's render is where the JIT costs most: these compiles are made on
the runtime's goroutine, which waits for each, as a runtime made without
`WithJITBackground` does; `qjs --jit` compiles in the background.

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, all three ran that same work in fresh processes at
`915400c`, and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.85 times QuickJS's time by the geometric mean: it is
slower on 16, faster on 44, and within 5% on the rest. With the JIT it
takes 0.32 times QuickJS's time, slower on 5 and faster on 66. The table
has the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `array_prop_create` | 38.8 ns | 17.9 ns | 14.9 ns | 2.61x | 1.21x |
| `global_read` | 10.1 ns | 1.67 ns | 5.65 ns | 1.78x | 0.30x |
| `weak_map_delete` | 427 ns | 454 ns | 249 ns | 1.71x | 1.82x |
| `weak_map_set` | 151 ns | 98.9 ns | 93.3 ns | 1.62x | 1.06x |
| `array_read` | 9.53 ns | 1.50 ns | 6.57 ns | 1.45x | 0.23x |
| `global_write` | 10.6 ns | 2.63 ns | 7.62 ns | 1.39x | 0.35x |
| `global_write_strict` | 10.6 ns | 2.60 ns | 7.65 ns | 1.38x | 0.34x |
| `typed_array_read` | 13.3 ns | 2.33 ns | 9.79 ns | 1.36x | 0.24x |
| `empty_do_loop` | 7.18 ns | 0.82 ns | 5.38 ns | 1.33x | 0.15x |
| `int_to_string` | 60.9 ns | 44.3 ns | 49.4 ns | 1.23x | 0.90x |
| `string_length` | 8.09 ns | 0.86 ns | 14.3 ns | 0.57x | 0.06x |
| `array_for_of` | 20.5 ns | 6.19 ns | 46.0 ns | 0.45x | 0.13x |
| `string_to_int` | 49.2 ns | 43.1 ns | 113 ns | 0.43x | 0.38x |
| `float_to_string` | 151 ns | 140 ns | 350 ns | 0.43x | 0.40x |
| `array_pop` | 46.0 ns | 17.9 ns | 112 ns | 0.41x | 0.16x |
| `string_build2c` | 57.9 ns | 40.7 ns | 143 ns | 0.40x | 0.28x |
| `float_toString` | 147 ns | 131 ns | 365 ns | 0.40x | 0.36x |
| `prop_clone` | 28.8 ns | 10.5 ns | 72.0 ns | 0.40x | 0.15x |
| `array_slice` | 10.5 ns | 11.6 ns | 27.2 ns | 0.39x | 0.43x |
| `regexp_replace` | 329 ns | 345 ns | 880 ns | 0.37x | 0.39x |

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.80 ns with it, `func_call` 1.12 ns, a tenth of either interpreter's
time or less. A test whose time is a built-in function's work runs much
as it does in the interpreter: regular expressions, `array_slice`, and
the conversions between numbers and strings. In the run made for this
page, only `array_slice` of these tests took 7% or more longer with
the JIT than without it, and, of the extra tests below that QuickJS runs,
`generator_delegate`, `typed_f64_sort`, the reads of the 64-bit typed
arrays, `dataview_read`, `dataview_write`, `map_forEach`, `string_trim`,
`string_pad_start`, `string_chain`, `regexp_sticky`, and five of the
tests of objects made on prototypes made anew (`object_create_fresh`,
`set_proto_fresh`, `class_factory`, `subclass_fresh`,
`construct_new_target`). A single run's list moves: of the fifteen in the
run at `6a53e07`, seven are in it.

Some of these tests depend on where the linker places the code more than on
the code, by more than eight placements can average out: those vary where
the code starts, not how far apart its pieces are. Grouping the hot files
(`250e8f6`) left the V8 suite level but made `array_for`, `array_update` and
`float_arith` 33–43% slower and `func_call` 26% slower, at every placement.
`6b12267` changed none of their code, only the size of two of the tree
tier's nodes; averaged over eight placements of
`6b12267`, `array_update` (29.8 ns) and `func_call` (48.5 ns) had come back
only part of the way, and `array_push` and `map_delete` were 7–10% slower. A
change of that size in one of these tests is not by itself a change in the
engine.

The runners also run `microbench_extra.js`, tests of what microbench.js has
none of, written as its tests are: generators and async functions, try
statements, iterators, using declarations, closures made and called anew,
calls in tail position, method calls with a frame and without, loops of a
few simple instructions, private class members, `Math.random`, the `in`
operator, `delete`, the array methods that take a callback, `indexOf`,
String's methods that run no script, Math's imul, clz32, pow, atan2 and
hypot, a template and a long chain of calls joined, for-in over an object
and over a dictionary, what classes do -- a list walked, small methods
moving objects between arrays, a method called for each element, callbacks
made in a method -- BigInts' division, shifts, hashing, powers and decimal
strings, and regular expressions as programs use them: validating,
tokenizing, replacing, splitting, named groups and the u flag -- and
since v0.31.1 switch statements of strings and numbers and `typeof`'s
tests, typed arrays of every element type and DataViews, calls of a
borrowed `hasOwnProperty`, pushes of several values, object literals and
strings appended to a captured binding -- and since v0.32.0 objects made
on prototypes made and dropped, classes made per call, and objects a
function's own code alone sees. Over the 209 QuickJS can run (it has no
using declarations) go-quickjs takes 0.86 times its time, and 0.35 times
with the JIT. A switch on strings takes a tenth to a fifth of
QuickJS's time, its literal cases one lookup (`100eb7f`); the 64-bit
typed arrays, whose elements are BigInts, are among the largest gaps, a
write of an element 1.9 times QuickJS's time. An await costs much less,
resumed without allocating (`3af4f32`), as does an async generator, and
regular expressions that search a text or split it much less again. A
generator still costs more to resume from for-of, 1.38 times QuickJS's
time, though less than before: it is 96 bytes, made by its kind
(`d8fad84`, `7422742`). A call between two functions in tail position
costs more, as it goes through the pending tail call and its checks, and
so does a global exec, 1.30 times, which makes a result and a String for
each match; a sticky one is level. A for-in over a dictionary takes 1.56 times QuickJS's time,
though its keys now come from the dictionary's table, made strings once
(`212472e`), where it took 3.4 times, and callbacks made in a method
1.27 times (`method_closure`), each closure about twice the size of
QuickJS's; with the JIT 0.20 times, an arrow that reads `this` inlined
where its method makes it and never made (`ad3e6091`). The largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `array_find_upvalue` | 87.3 ns | 12.7 ns | 37.7 ns | 2.31x | 0.34x |
| `typed_i64_write` | 39.2 ns | 39.3 ns | 20.2 ns | 1.94x | 1.94x |
| `construct_new_target` | 498 ns | 557 ns | 261 ns | 1.91x | 2.14x |
| `typed_u64_reduce` | 140 ns | 118 ns | 75.2 ns | 1.86x | 1.58x |
| `typed_u64_forEach` | 144 ns | 99.0 ns | 79.2 ns | 1.82x | 1.25x |
| `typed_i64_reduce` | 64.9 ns | 29.8 ns | 36.6 ns | 1.77x | 0.81x |
| `typed_i64_forEach` | 72.0 ns | 21.3 ns | 40.9 ns | 1.76x | 0.52x |
| `typed_findIndex` | 50.3 ns | 10.7 ns | 30.3 ns | 1.66x | 0.35x |
| `array_forEach_user` | 69.7 ns | 6.40 ns | 42.9 ns | 1.63x | 0.15x |
| `typed_i64_map` | 85.7 ns | 22.0 ns | 53.0 ns | 1.62x | 0.42x |
| `string_switch_prefix` | 37.6 ns | 13.5 ns | 188 ns | 0.20x | 0.07x |
| `regexp_split` | 1,075 ns | 990 ns | 5,719 ns | 0.19x | 0.17x |
| `string_switch_long` | 33.2 ns | 14.6 ns | 190 ns | 0.17x | 0.08x |
| `async_await` | 116 ns | 122 ns | 750 ns | 0.15x | 0.16x |
| `string_switch_last` | 35.1 ns | 16.5 ns | 264 ns | 0.13x | 0.06x |
| `string_switch_miss` | 31.3 ns | 16.3 ns | 267 ns | 0.12x | 0.06x |
| `string_switch_nonstring` | 25.3 ns | 8.87 ns | 263 ns | 0.10x | 0.03x |
| `string_switch_large` | 37.6 ns | 26.3 ns | 405 ns | 0.09x | 0.06x |
| `regexp_search_i` | 1,820 ns | 746 ns | 20,963 ns | 0.09x | 0.04x |
| `regexp_search_utf16` | 735 ns | 732 ns | 13,090 ns | 0.06x | 0.06x |

The BigInt tests were the largest gaps, 9.65 and 5.56 times QuickJS's time
at `608415b`, where every BigInt was an allocation. A BigInt in the int64
range is now held in the value itself (`9746906`), as QuickJS holds one
that fits in a word, and so is one below 2^64 in magnitude (`f8c3229`);
a larger one is a block of no pointers, made by the engine's own
arithmetic (`f1bf681`). `bigint64_arith` takes 0.78 times QuickJS's time
and `bigint256_arith` 1.07 times, and the extra BigInt tests 0.59 to 1.16
times. The JIT makes BigInts of 64 bits natively: `bigint64_arith` takes
0.12 times QuickJS's time with it.

The largest gaps now are:

- `array_prop_create`, an array made by assigning past its end, whose
  storage grows as it goes and leaves what it outgrew to the collector;
- `tail_mutual`, each call going through the pending tail call;
- `regexp_exec_g`, a global `exec`, which makes a result and a String
  for each match;
- the 64-bit typed arrays, whose elements are BigInts;
- `dict_for_in`, a for-in over a dictionary;
- `global_read` and `global_write`, a global variable's cache checked in a
  call;
- the weak collections. A WeakMap holds each value on its key rather than
  in a table of its own (`2bc1620`), so that a value which refers to its
  key does not keep the key alive, as the specification requires; adding
  an entry costs more for it.

Most of the others are the cost of each step, which the list below
explains. The suite as a whole does better than the operations it is
made of because of the places go-quickjs leads: strings and regular
expressions over whole texts, which Go's runtime and the engine's matcher do
well, and Splay's allocation-heavy setup.

What the gap is made of:

- **Dispatching instructions.** QuickJS's interpreter jumps straight from one
  instruction's code to the next, and the C compiler keeps the interpreter's
  state in registers. Go has neither computed jumps nor a way to keep state in
  registers across a switch that large.
  - Every instruction goes through one `switch`. Around it, the interpreter's
    state is stored to memory and loaded back.
  - Go inlines only the smallest functions into a function of that size, so
    the common cases are written out by hand where they are used.

  The interpreter takes nearly twice QuickJS's time for the same loop:
  1.1 to 3.0 times over six loops of simple instructions (1.8 by their
  geometric mean), `empty_loop` and the extra tests `loop_sum`,
  `loop_arith`, `loop_locals`, `loop_elements` and `loop_props` run with
  the tree tier off, `QJS_NOTREE=1`. Fusing common instruction sequences
  into single instructions (`a[i]`, `a[++i]`, `x & 0xff`,
  `local * local`) reduces how many are dispatched. Nearly
  every function runs instead as trees of Go closures, a node for every few
  instructions with no operand stack between them, and with locals and
  constants read in place (see the [design notes](design.md)). That is what
  closed the gap on Crypto and most of it on NavierStokes, which do little but
  arithmetic, local variables and array elements. A tree's loops run as Go
  loops, rebuilt from its blocks, with the comparison that ends a loop and
  the `i++` that steps it made in place: the tests that are mostly a loop --
  `empty_loop` (1.00), `int_arith` (0.97), `float_arith` (0.77),
  `array_update` -- are level with QuickJS or ahead of it, and the six
  loops above take 0.46 to 1.21 times QuickJS's time as trees. A loop whose body
  would need its branches chosen by a structure of their own, an `&&` or an
  else-if chain, still runs from block to block, which costs less.
- **Calls.** A call from JavaScript to JavaScript fills in a frame record of
  some fifteen fields, and each pointer it stores pays a write barrier's
  check. Even so a call costs about what QuickJS's does: a method call
  whose body needs a frame -- one with a loop, even an empty one -- about
  73 ns, against 69 in QuickJS (`method_call`), and a method that only
  returns its argument about 31 ns, against 42 (`method_call_leaf`). With
  the JIT they take 11.6 and 2.8 ns. Richards, DeltaBlue and
  EarleyBoyer are mostly small calls. Two kinds of function are answered
  without a frame, where nothing they do could call code or throw:
  - one whose whole body is `return this.x`, `return this.x.length`,
    `return this.x[i]`, or stores of its parameters in `this`, which is much
    of EarleyBoyer's constructions;
  - one that reads and computes, and may store to a property once as the
    last thing it can fail at, as `isHeldOrSuspended`, DeltaBlue's `input`,
    `output` and `execute` and RayTrace's `dot` do. It may call another such
    function, `new` a constructor of the first kind, as EarleyBoyer's
    `sc_cons` does, and push onto or pop from an array, as DeltaBlue's
    collections do. Called that way, an empty function costs about 27 ns,
    against 31 in QuickJS (`func_call`). One of seven
    instructions or more that calls nothing runs as its tree instead,
    whose frame costs less than evaluating it a step at a time.
- **Property access.** Objects built the same way share a shape, as in
  QuickJS and V8, and each property read and write, each getter and
  setter, each global variable read and each `instanceof` remembers where it last found
  what it looked for. The cache is checked in a call rather than where the
  instruction runs, since the interpreter's loop grows slower with every line
  added to it. An object holds only its values, its keys in its shape, as
  a QuickJS object does (`944e2e8`).
- **Memory management.** QuickJS counts references: an object is freed the
  moment the last reference to it goes, and a cycle collector handles the
  rest. go-quickjs relies on Go's garbage collector. It traces the live heap
  concurrently and needs a write barrier on every pointer stored, including
  every value written to the interpreter's stack. The collector does most of
  its work on other cores, so with cores to spare it costs less wall-clock
  time than CPU time. A program that allocates heavily still pays for it in
  CPU time.
- **Bounds checks.** Go checks every slice index, and the interpreter's stack,
  locals and array elements are all slices.

Where go-quickjs is level or ahead:

- **Crypto.** Its arithmetic runs as trees, whose nodes read their locals
  and constants in place, and each bitwise operator has a node of its own
  that skips the conversion where its operand is already an integer.
- **RegExp.** A pattern that contains a run of plain text every match must
  have, near where the match begins, is searched for that text first, and
  tried only where it could have been reached from -- what V8's regexp
  compiler gets from its lookahead analysis, and most of the lead here. With
  an unmodified RegExp, `replace` and `split` find their matches without
  building the arrays `exec` would return, a `replace` template without a
  `$` makes no strings of what matched, and `split` searches for each
  separator instead of trying every position. The matcher passes over
  positions where no match can begin in a loop of their own, reads a code
  unit that is a whole character without a call, and takes a greedy run of
  one character in a single step. A match whose result nothing reads makes
  no array, and a `replace` whose string is only measured makes its length
  alone. In a runtime with the JIT, a pattern is compiled to machine code
  at its second match, on amd64 and arm64, as V8's Irregexp compiles it
  (`2be0068`), and `exec`, `test` and `replace` are called from native
  code without a frame.
- **Splay, in fixed work.** That is mostly building the tree and dropping
  it. Go's allocator makes many small objects cheaply, a short string or an
  array of up to sixteen elements is one allocation with its contents, and
  dropping the tree costs nothing until the collector runs. The splay
  operations themselves, which the score times, are level with
  QuickJS's.
- **Array elements updated, walked and skipped.** `a[k] += y` and `a[k]++`
  on a number in an array's storage are one step in a tree, the element
  updated where it is: one node where there were three, with the array and
  the key checked once rather than twice (`array_update`, 0.67 times
  QuickJS's time). for-in over an array whose keys are its elements walks
  them by index rather than copying them into a list first
  (`array_for_in`, 0.70), `a[k]` by a key it gives is read from the
  elements (`1039786`), and the array methods pass over a hole
  without looking its index up, where nothing up the prototype chain could
  have it (`array_hole_length_decr`, 0.88). A typed array of a
  fixed length is written through the storage it keeps
  (`typed_array_write`, 0.92).
- **`apply(this, arguments)`.** In a function whose only use of `arguments` is
  passing it to `apply` (a common way to write a class constructor), the
  arguments object is never made. A constructor whose whole body is
  `this.initialize.apply(this, arguments)`, as RayTrace's are, calls the
  method directly, with the frames a stack trace would show standing in for
  its body.

What remains in the interpreter is mostly the cost of a Go indirect call,
which every instruction or tree node pays, and of the operations the gaps
above are in. The ways around the first which an interpreter written in C
has are not open to one written in Go, as measured here:
- Running calls in the same loop, rather than a Go call each, made the loop
  slower than the calls it saved.
- Turning each instruction into a Go closure, instead of a case of the switch,
  came to little more than 10% on the loop best suited to it.
- Building each expression into a tree of closures that reads its locals
  and constants in place nearly halved the time Crypto and NavierStokes
  take, but did not make a call cheaper: entering a tree costs what entering
  the interpreter does.

The JIT is the way past it: a hot function compiled to native code
dispatches nothing, which is why, with it, the loops and calls above take
from a quarter of the interpreter's time down to a twenty-fifth.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
