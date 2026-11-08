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
with `CGO_ENABLED=0 -pgo=off -trimpath` and JIT disabled, go-quickjs
`2d1ca1f7` gives these per-metric medians, measured on 2026-11-07. Compiler
figures use five fresh processes, each running both bundles with 500 ms per
benchmark. Load figures use twenty fresh processes per bundle, one load per
process. One process runs at a time, with 30-second pauses between the five
compiler rounds and between the twenty load rounds. Each load round runs
both bundles in separate processes, alternating their order. AC power was
used throughout, with no concurrent builds or tests. All samples are
included, including the first.

| Bundle | Compile | First load |
|---|---:|---:|
| Three.js | 11.04 ms | 27.78 ms |
| Babel | 29.95 ms | 154.77 ms |

The first-load interval includes parsing, compilation and execution. It
excludes runtime construction, source reads, host setup and the functional
checks. The compiler-only interval starts after parsing and measures
compilation of the existing AST; it does not include work deferred to
executing the bundle.

Memory figures for the same runs are below. Allocations are total bytes
allocated per operation, in decimal MB. Peak load RSS is the whole test
process, including its untimed setup and functional checks, in MiB. Each
metric is the median of its own samples:

| Bundle | Compiler MB/op | Load MB/op | Peak load RSS MiB |
|---|---:|---:|---:|
| Three.js | 5.50 | 21.69 | 45.43 |
| Babel | 19.39 | 90.92 | 113.08 |

The `qjs` binary built without the JIT, using `CGO_ENABLED=0 go build
-pgo=off -trimpath ./cmd/qjs`, is 42,606,722 bytes. The load test binary is
40,790,114 bytes, and the compiler test binary is 8,179,986 bytes. The
bundle sizes and SHA-256 checks are the pinned upstream files above.

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
[goja]. All three go-quickjs columns use revision `229e19b3`, measured alongside
goja on 2026-11-08. Each column is the median of three fresh-process runs
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
| Richards | 2,552 | 14,393 | 14,848 | 529 | 4.82x | 27.21x | 28.07x |
| DeltaBlue | 2,728 | 13,726 | 13,323 | 634 | 4.30x | 21.65x | 21.01x |
| Crypto | 3,252 | 20,382 | 20,434 | 335 | 9.71x | 60.84x | 61.00x |
| RayTrace | 6,185 | 32,749 | 34,853 | 813 | 7.61x | 40.28x | 42.87x |
| EarleyBoyer | 7,587 | 13,454 | 13,536 | 1,454 | 5.22x | 9.25x | 9.31x |
| RegExp | 8,938 | 10,344 | 10,365 | 567 | 15.76x | 18.24x | 18.28x |
| Splay | 12,908 | 20,487 | 21,261 | 2,486 | 5.19x | 8.24x | 8.55x |
| NavierStokes | 6,006 | 38,842 | 39,138 | 559 | 10.74x | 69.48x | 70.01x |
| **Composite score** | **5,387** | **18,714** | **18,811** | **754** | **7.14x** | **24.82x** | **24.95x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
go-quickjs and goja runs:

| Metric | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off relative to goja |
|---|---:|---:|---:|---:|---:|
| Suite wall time | 22.23 s | 20.11 s | 20.10 s | 51.35 s | 2.31x faster |
| Suite CPU time | 25.80 s | 26.15 s | 26.34 s | 72.64 s | 64.5% less |
| Total allocation | 7.80 GiB | 15.93 GiB | 16.03 GiB | 33.75 GiB | 76.9% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. All
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `229e19b3`, goja `a177d211bf60`, and
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
byte-identical; both JIT modes used the same binary. The tracked checkout
matched the revision above throughout measurement.

The composite samples were:

| Mode | Run 1 | Run 2 | Run 3 |
|---|---:|---:|---:|
| JIT off | 5,581 | 5,365 | 5,387 |
| JIT on | 18,714 | 17,781 | 19,033 |
| JIT background | 18,961 | 18,584 | 18,811 |
| goja | 754 | 754 | 748 |

All samples were included. The table takes the median of each workload and
of the printed composite scores independently. Background compilation's
composite was 0.52% higher than synchronous compilation's, within the spread
of these samples; this run does not establish an overall throughput gain.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is ahead of QuickJS. Its
interpreter takes 0.52 times QuickJS's time and has a 42% higher composite
score: it is level on Richards and DeltaBlue and ahead on the other six.
With its JIT it takes 0.24 times QuickJS's time, scores 5.1 times as
much, and is ahead on all eight. On the larger programs of Octane and Are
We Fast Yet the interpreter is ahead too, 0.91 and 0.97 times QuickJS's
time, and the JIT takes 0.39 and 0.22 times
([below](#octane-and-are-we-fast-yet)); a page React 18 renders on the
server takes 0.83 times, and 0.58 with the JIT
([below](#server-rendering)). Single operations cost less on
average, 0.80 times QuickJS's time, and 0.30 times with the JIT, as
[the micro-benchmarks](#micro-benchmarks) below show, though a few still
take about twice as long.

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The JIT, which is experimental and optional, compiles hot functions
to native code (see its [design notes](../internal/jit/README.md)). The
same suite ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.3,
go-quickjs `e7e79f4a` and QuickJS 2026-06-04. Two runs
went at once, each on the six CPUs of a level 3 cache of its own. The
JIT's column is a build with `-tags quickjs_jit` run with `-jit`. The
figures are milliseconds for the same fixed work, the median of three
fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 12.0 ms | 7.7 ms | 11.4 ms | 1.05x | 0.68x |
| DeltaBlue | 24.6 ms | 19.6 ms | 23.9 ms | 1.03x | 0.82x |
| Crypto | 296 ms | 62.1 ms | 329 ms | 0.90x | 0.19x |
| RayTrace | 117 ms | 62.7 ms | 142 ms | 0.82x | 0.44x |
| EarleyBoyer | 323 ms | 120 ms | 405 ms | 0.80x | 0.30x |
| RegExp | 147 ms | 131 ms | 859 ms | 0.17x | 0.15x |
| Splay | 186 ms | 127 ms | 493 ms | 0.38x | 0.26x |
| NavierStokes | 173 ms | 37.4 ms | 193 ms | 0.90x | 0.19x |
| **Total** | **1,299 ms** | **590 ms** | **2,489 ms** | **0.52x** | **0.24x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 917 | 5,313 | 954 | 0.96x | 5.57x |
| DeltaBlue | 866 | 5,607 | 838 | 1.03x | 6.69x |
| Crypto | 1,220 | 8,009 | 1,107 | 1.10x | 7.23x |
| RayTrace | 1,928 | 9,527 | 1,582 | 1.22x | 6.02x |
| EarleyBoyer | 2,590 | 4,886 | 2,173 | 1.19x | 2.25x |
| RegExp | 2,947 | 3,601 | 435 | 6.77x | 8.28x |
| Splay | 4,412 | 7,155 | 3,346 | 1.32x | 2.14x |
| NavierStokes | 2,714 | 15,784 | 2,387 | 1.14x | 6.61x |
| **Composite** | **1,901** | **6,773** | **1,340** | **1.42x** | **5.05x** |

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make most of the interpreter's lead. The composite score is the
  geometric mean of the workloads' scores, so each counts the same:
  Richards weighs as much as RegExp. By that mean the interpreter takes
  0.66 times QuickJS's time, and the JIT 0.32 times, where their scores
  are 42% higher and 5.1 times as high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over. The scores time
  warm runs, where Richards, DeltaBlue, Crypto, RayTrace and
  NavierStokes score 4.9 to 6.6 times what the interpreter does.
- Richards is 5% behind in fixed time and 4% in its score, where it
  was 15% and 11% behind at `217bd7f`: a method of a property
  of `this` is called as one node (`00e761a`), and an equality as a value
  is one node (`5544686`). This page had it level at v0.27.0, but that
  came from the
  clocks. The runners then timed go-quickjs with the clock Windows gives
  Go, which moves only every 0.5 ms, or every 15.6 ms while no program
  asks for more, and QuickJS with `Date.now`, which counts whole
  milliseconds. Both now read the performance counter (`ab0ef0b`), and
  timed that way v0.27.0 takes 13.0 ms on Richards, about what main takes.
- Splay's lead is largest in its fixed time. That is almost all setup and
  teardown: building a tree of 8,000 nodes and dropping it, which QuickJS
  frees by counting references where Go's collector frees it later. A
  warm run, which is what the score times, is splay-tree operations, calls
  and property reads, and a new node for each insertion. Each node's
  payload holds array literals of constants, which since `a2d9f12e` share
  their elements with the literal until one is written, as V8's
  copy-on-write arrays do: Splay's fixed time fell by about a fifth, and
  its score is 32% ahead of QuickJS's, where it was 20%. Giving the
  collector four times the room (`GOGC=400`) leaves Splay's score as it
  was, so collection is not what the score measures.
- The JIT changes least where the time is spent in Go code it calls
  rather than in the script's own: the allocation-heavy EarleyBoyer and
  Splay, 1.9 and 1.6 times the interpreter's score, and RegExp, 1.2 times,
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
| Richards | 4.4 ms | 4.7 ms | 3.9 ms | 1.13x | 1.21x |
| DeltaBlue | 8.1 ms | 8.3 ms | 7.8 ms | 1.04x | 1.06x |
| Crypto | 99.3 ms | 23.6 ms | 109 ms | 0.91x | 0.22x |
| RayTrace | 40.8 ms | 38.9 ms | 49.8 ms | 0.82x | 0.78x |
| EarleyBoyer | 112 ms | 41.7 ms | 136 ms | 0.83x | 0.31x |
| RegExp | 114 ms | 110 ms | 465 ms | 0.25x | 0.24x |
| Splay | 158 ms | 105 ms | 468 ms | 0.34x | 0.23x |
| NavierStokes | 64.9 ms | 17.2 ms | 67.0 ms | 0.97x | 0.26x |
| PdfJS | 241 ms | 241 ms | 375 ms | 0.64x | 0.64x |
| Mandreel | 3,411 ms | 940 ms | 3,356 ms | 1.02x | 0.28x |
| Gameboy | 488 ms | 333 ms | 464 ms | 1.05x | 0.72x |
| CodeLoad | 13.8 ms | 14.6 ms | 12.9 ms | 1.07x | 1.13x |
| Box2D | 136 ms | 124 ms | 154 ms | 0.88x | 0.81x |
| zlib | 7,740 ms | 2,035 ms | 8,310 ms | 0.93x | 0.24x |
| Typescript | 2,158 ms | 2,153 ms | 2,128 ms | 1.01x | 1.01x |
| **Total** | **15,234 ms** | **6,570 ms** | **16,712 ms** | **0.91x** | **0.39x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 836 ms | 303 ms | 711 ms | 1.18x | 0.43x |
| Richards | 4,696 ms | 765 ms | 4,770 ms | 0.98x | 0.16x |
| Json | 1,271 ms | 269 ms | 1,336 ms | 0.95x | 0.20x |
| CD | 2,578 ms | 515 ms | 2,833 ms | 0.91x | 0.18x |
| Havlak | 4,845 ms | 1,678 ms | 4,347 ms | 1.11x | 0.39x |
| Bounce | 1,251 ms | 246 ms | 1,483 ms | 0.84x | 0.17x |
| List | 1,160 ms | 153 ms | 1,246 ms | 0.93x | 0.12x |
| Mandelbrot | 409 ms | 41.0 ms | 697 ms | 0.59x | 0.06x |
| NBody | 984 ms | 122 ms | 1,033 ms | 0.95x | 0.12x |
| Permute | 1,531 ms | 227 ms | 1,695 ms | 0.90x | 0.13x |
| Queens | 1,049 ms | 122 ms | 1,074 ms | 0.98x | 0.11x |
| Sieve | 1,111 ms | 268 ms | 1,221 ms | 0.91x | 0.22x |
| Storage | 1,652 ms | 742 ms | 1,644 ms | 1.00x | 0.45x |
| Towers | 1,685 ms | 205 ms | 1,568 ms | 1.07x | 0.13x |
| **Total** | **25,051 ms** | **5,655 ms** | **25,716 ms** | **0.97x** | **0.22x** |

- Without the JIT, go-quickjs leads where the V8 suite has it lead --
  RegExp, Splay, the arithmetic of Crypto and NavierStokes, Mandelbrot's
  loops -- and is behind on code that is mostly calls and allocation, by
  as much as 18% (Are We Fast Yet's DeltaBlue) and 11% (Havlak) in this
  run. DeltaBlue moves most from one run to the next: it was 18% behind in
  the run at `9e9f029`, and v0.31.1 and `6a53e07`, five fresh runs each,
  took 924 and 888 ms (their medians). Their classes' common steps -- a field
  read into a local, a comparison with `null`, a field's element, a
  method of a field called, a return -- are now single nodes of the
  tree, and an arrow reads its `this` in place (`6b7a140` to `2d0acf3`);
  List, behind then too, is ahead, and Towers is 7% behind in this run,
  2% at `2d1ca1f7`. Of Octane's long
  programs, Typescript's compiler is 1% behind and CodeLoad's parsing 7%,
  and pdf.js is 36% ahead. The engine's own compiler spends less: Octane's
  sources compile in 0.81 times what they took before `4228b5c4` and
  `b71bc2a9`, 0.76 at one thread, with half the allocations and the same
  bytecode, a difference these tables hardly show, as compiling is a small
  part of a run. The code compiled from C and C++ is level or
  ahead: zlib's 7% ahead, where it was 20% behind at
  `3714c86`, its bitwise arithmetic and its elements at int32 keys run as
  int32 trees since `cf27162`, `5e22bb2` and `f838b8c`; Mandreel's level,
  where it was 19% behind at `882b9d0`, its globals' elements read and
  written as single nodes (`9d03a00`, `cbc097d`) and its negative
  literals taken as immediates (`3f790df`).
- With the JIT, Are We Fast Yet takes 0.06 to 0.45 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Havlak, Storage -- the most.
- With the JIT, the code compiled from C and C++ runs as int32
  arithmetic over typed arrays read and written for the element type
  their site met: Mandreel takes 0.28 times QuickJS's time and zlib 0.24,
  where they took 0.47 and 0.36 at `15a29721`. Exact integer arithmetic
  under a `| 0` is int32 arithmetic, a comparison of two int32s and a
  loop's phi that carries only int32s are int32 too, and a typed array's
  view keeps where its elements are and how many (`81517cb7`,
  `786ff4fe`, `50909d69`, `0bcf0b7d`, `534f1b00`).
- On five of Octane's fifteen the JIT is level with the interpreter or
  behind it: level on DeltaBlue, PdfJS and Typescript, behind on
  Richards (7%) and CodeLoad (6%); RayTrace, 22% behind at `6a53e07`, is
  5% ahead, and DeltaBlue was 12% behind at `915400c`. Octane's fixed work is one run of
  each benchmark, so compiling counts: DeltaBlue's 16 compiles took about
  2.5 ms of its 8.5 at `3714c86`, and a single run's time moves: with
  the JIT, five fresh runs of RayTrace took 42 to 49 ms at `6a53e07`, and
  44 to 49 at v0.31.1. Typescript, 4% behind in the run at `9e9f029`, and
  RegExp, 5% behind, are level and 4% ahead.

### Server rendering

The ssr suite renders a page with React 18's own builds, as a host behind
a web server does for each request: Render is one runtime rendering the
page again and again, FirstRender five fresh runtimes, each loading React
and rendering once. The same matrix, the median of three fresh-process
runs (lower is better):

| ssr | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Render | 1,562 ms | 1,088 ms | 1,877 ms | 0.83x | 0.58x |
| FirstRender | 199 ms | 368 ms | | | |

Render took 0.90 and 0.66 times QuickJS's time at `915400c`. React writes
a page as thousands of short chunks appended to a string a closure holds,
and an append now grows that string where it is, when nothing else can see
it (`00ce03b5`). An empty literal learns the room its arrays grow to
(`c62a9bb0`), and a for-in takes the last loop's cursor once that loop has
finished (`5326a3f8`). A render allocates about 60% fewer bytes for them.

QuickJS's runner loads a script once, so it has no FirstRender. A fresh
runtime's render is where the JIT costs most: these compiles are made on
the runtime's goroutine, which waits for each, as a runtime made without
`WithJITBackground` does; `qjs --jit` compiles in the background.

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, all three ran that same work in fresh processes at
`e7e79f4a`, and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.80 times QuickJS's time by the geometric mean: it is
slower on 17, faster on 43, and within 5% on the rest. With the JIT it
takes 0.30 times QuickJS's time, slower on 2 and faster on 66. The table
has the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `array_prop_create` | 31.9 ns | 14.7 ns | 15.0 ns | 2.13x | 0.98x |
| `global_read` | 10.0 ns | 0.96 ns | 5.54 ns | 1.81x | 0.17x |
| `weak_map_delete` | 405 ns | 439 ns | 251 ns | 1.61x | 1.74x |
| `weak_map_set` | 140 ns | 93.3 ns | 93.6 ns | 1.49x | 1.00x |
| `array_read` | 9.38 ns | 1.45 ns | 6.69 ns | 1.40x | 0.22x |
| `global_write` | 10.6 ns | 2.55 ns | 7.62 ns | 1.39x | 0.33x |
| `global_write_strict` | 10.6 ns | 2.54 ns | 7.65 ns | 1.38x | 0.33x |
| `typed_array_read` | 13.3 ns | 1.70 ns | 9.87 ns | 1.35x | 0.17x |
| `empty_do_loop` | 7.13 ns | 0.81 ns | 5.37 ns | 1.33x | 0.15x |
| `int_to_string` | 59.3 ns | 42.3 ns | 48.7 ns | 1.22x | 0.87x |
| `string_to_int` | 45.8 ns | 43.8 ns | 112 ns | 0.41x | 0.39x |
| `array_pop` | 43.9 ns | 17.0 ns | 111 ns | 0.40x | 0.15x |
| `prop_clone` | 27.4 ns | 9.53 ns | 72.1 ns | 0.38x | 0.13x |
| `regexp_replace` | 334 ns | 351 ns | 884 ns | 0.38x | 0.40x |
| `string_build1` | 33.3 ns | 37.8 ns | 90.7 ns | 0.37x | 0.42x |
| `array_slice` | 9.89 ns | 10.1 ns | 27.1 ns | 0.37x | 0.37x |
| `float_to_string` | 126 ns | 120 ns | 352 ns | 0.36x | 0.34x |
| `float_toString` | 123 ns | 109 ns | 377 ns | 0.33x | 0.29x |
| `string_build_large1` | 30.9 ns | 41.8 ns | 103 ns | 0.30x | 0.41x |
| `string_build2c` | 32.2 ns | 39.0 ns | 142 ns | 0.23x | 0.27x |

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.79 ns with it, `func_call` 1.00 ns, a tenth of either interpreter's
time or less. A test whose time is a built-in function's work runs much
as it does in the interpreter: regular expressions, `array_slice`, and
the conversions between numbers and strings. An object a function makes
and drops need not be made at all, even where it crosses a merge or goes
round a loop, as in `sum = sum.add(v)`: it is made only where it escapes,
by partial escape analysis, as in Graal (`41575a42`). `escape_accumulate`
and `carried_short` take 0.07 and 0.04 times QuickJS's time with the JIT,
and 0.84 and 0.72 without it. Code that keeps its objects gains nothing
from this, and the suites above hardly move for it. In the run made for this
page, five of these tests took 7% or more longer with the JIT than
without it: `date_now`, `string_build1`, `string_build2c`,
`string_build_large1` and `weak_map_delete`. Of the extra tests below that
QuickJS runs, so did `async_await`, `async_generator`,
`generator_delegate`, `template_escaped`, `array_push_args3` and
`array_push_args5`, `typed_u64_write`, `dataview_read`, `dataview_write`,
`dict_for_in`, `string_chain`, `regexp_sticky`, `construct_new_target`,
`carried_every_2`, and seven of the `json_` tests. A single run's list
moves: of the 27 in the run at `15a29721`, 15 are in it.

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
function's own code alone sees -- and since v0.35.0 strings grown in
place, arrays past 16,384 elements, objects a loop carries, and 49 tests
of `JSON.parse` and `JSON.stringify`. Over the 300 QuickJS can run (it has
no using declarations) go-quickjs takes 0.71 times its time, and 0.31 times
with the JIT; leaving out the JSON tests, 0.84 and 0.31. A switch on strings takes a tenth to a fifth of
QuickJS's time, its literal cases one lookup (`100eb7f`); the 64-bit
typed arrays, whose elements are BigInts, are among the largest gaps, a
write of an element 1.9 times QuickJS's time. An await costs much less,
resumed without allocating (`3af4f32`), as does an async generator, and
regular expressions that search a text or split it much less again. A
generator still costs more to resume from for-of, 1.41 times QuickJS's
time, though less than before: it is 96 bytes, made by its kind
(`d8fad84`, `7422742`). A call between two functions in tail position
costs more, as it goes through the pending tail call and its checks, and
so does a global exec, 1.29 times, which makes a result and a String for
each match; a sticky one is level. A for-in over a dictionary takes 1.29 times QuickJS's time,
though its keys now come from the dictionary's table, made strings once
(`212472e`), where it took 3.4 times, and callbacks made in a method
1.29 times (`method_closure`), each closure about twice the size of
QuickJS's; with the JIT 0.19 times, an arrow that reads `this` inlined
where its method makes it and never made (`ad3e6091`). The largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `array_index_append` | 38.9 ns | 21.3 ns | 17.0 ns | 2.29x | 1.25x |
| `array_find_upvalue` | 80.1 ns | 13.6 ns | 37.5 ns | 2.13x | 0.36x |
| `call_mutual` | 75.4 ns | 12.2 ns | 35.5 ns | 2.12x | 0.35x |
| `typed_i64_write` | 38.1 ns | 40.6 ns | 20.4 ns | 1.87x | 1.99x |
| `typed_i64_forEach` | 75.4 ns | 20.5 ns | 40.4 ns | 1.87x | 0.51x |
| `construct_new_target` | 488 ns | 561 ns | 265 ns | 1.84x | 2.12x |
| `array_literal_empty` | 66.3 ns | 60.9 ns | 38.3 ns | 1.73x | 1.59x |
| `typed_i64_reduce` | 64.6 ns | 32.0 ns | 37.5 ns | 1.72x | 0.85x |
| `typed_u64_reduce` | 130 ns | 119 ns | 75.9 ns | 1.72x | 1.56x |
| `typed_i64_map` | 88.2 ns | 24.2 ns | 51.5 ns | 1.71x | 0.47x |
| `json_stringify_long` | 27,017 ns | 27,969 ns | 206,232 ns | 0.13x | 0.14x |
| `json_stringify_index_keys` | 126 ns | 129 ns | 979 ns | 0.13x | 0.13x |
| `json_stringify_unique` | 119 ns | 121 ns | 945 ns | 0.13x | 0.13x |
| `string_switch_miss` | 33.2 ns | 15.4 ns | 265 ns | 0.13x | 0.06x |
| `json_stringify_decimals` | 35.6 ns | 37.3 ns | 333 ns | 0.11x | 0.11x |
| `string_switch_large` | 38.9 ns | 24.7 ns | 405 ns | 0.10x | 0.06x |
| `string_switch_nonstring` | 24.3 ns | 8.05 ns | 262 ns | 0.09x | 0.03x |
| `regexp_search_i` | 1,770 ns | 795 ns | 20,874 ns | 0.08x | 0.04x |
| `regexp_search_utf16` | 716 ns | 762 ns | 13,200 ns | 0.05x | 0.06x |
| `json_parse_long` | 1,842 ns | 1,850 ns | 85,338 ns | 0.02x | 0.02x |

The BigInt tests were the largest gaps, 9.65 and 5.56 times QuickJS's time
at `608415b`, where every BigInt was an allocation. A BigInt in the int64
range is now held in the value itself (`9746906`), as QuickJS holds one
that fits in a word, and so is one below 2^64 in magnitude (`f8c3229`);
a larger one is a block of no pointers, made by the engine's own
arithmetic (`f1bf681`). `bigint64_arith` takes 0.78 times QuickJS's time
and `bigint256_arith` 1.13 times, and the extra BigInt tests 0.64 to 1.11
times. The JIT makes BigInts of 64 bits natively: `bigint64_arith` takes
0.11 times QuickJS's time with it.

The largest gaps now are:

- `array_index_append` and `array_prop_create`, an array grown by assigning
  past its end, whose storage leaves what it outgrew to the collector;
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
  `empty_loop` (1.00), `int_arith` (0.98), `float_arith` (0.75),
  `array_update` -- are level with QuickJS or ahead of it, and the six
  loops above take 0.46 to 1.21 times QuickJS's time as trees. A loop whose body
  would need its branches chosen by a structure of their own, an `&&` or an
  else-if chain, still runs from block to block, which costs less.
- **Calls.** A call from JavaScript to JavaScript fills in a frame record of
  some fifteen fields, and each pointer it stores pays a write barrier's
  check. Even so a call costs about what QuickJS's does: a method call
  whose body needs a frame -- one with a loop, even an empty one -- about
  69 ns, against 67 in QuickJS (`method_call`), and a method that only
  returns its argument about 31 ns, against 43 (`method_call_leaf`). With
  the JIT they take 11.0 and 2.7 ns. Richards, DeltaBlue and
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
    collections do. Called that way, an empty function costs about 28 ns,
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
- **Strings built by appending.** A closure's `s += x` and a local's
  `out += esc(x)` grow the string in place, in storage with room to spare,
  rather than making a rope (`00ce03b5`): `string_build2c` takes 0.23 times
  QuickJS's time, `local_append_keep` 0.35 and `template_list` 0.37. The
  interpreter is ahead of the JIT on the first of these, 0.27 times with it.
- **JSON.** The 49 `json_` tests take 0.31 times QuickJS's time, and 0.31
  with the JIT, as the work is a built-in's. `JSON.parse` walks the shape
  tree as it reads, expecting the keys of the last object parsed in the same
  place, and makes each object once, at its last key. `JSON.stringify` keeps
  a layout's keys and slots from the second time it meets it (`097d71ec`).
  Numbers are read and written without strconv's general path (`baafd8e3`),
  and long strings are scanned with AVX2 or NEON where the machine has them:
  `json_parse_records` takes 0.33 times QuickJS's time,
  `json_stringify_records` 0.15 and `json_parse_long` 0.02.
- **Splay.** Its fixed work is mostly building the tree and dropping
  it. Go's allocator makes many small objects cheaply, a short string or an
  array of up to sixteen elements is one allocation with its contents, and
  dropping the tree costs nothing until the collector runs. An array
  literal of constants is not copied at all until it is written: its
  arrays share the literal's elements (`a2d9f12e`, and natively
  `15a29721`), which took a fifth off Splay's fixed time and a quarter off
  it with the JIT.
- **Array elements updated, walked and skipped.** `a[k] += y` and `a[k]++`
  on a number in an array's storage are one step in a tree, the element
  updated where it is: one node where there were three, with the array and
  the key checked once rather than twice (`array_update`, 0.59 times
  QuickJS's time). for-in over an array whose keys are its elements walks
  them by index rather than copying them into a list first
  (`array_for_in`, 0.65), `a[k]` by a key it gives is read from the
  elements (`1039786`), and the array methods pass over a hole
  without looking its index up, where nothing up the prototype chain could
  have it (`array_hole_length_decr`, 0.90). A typed array of a
  fixed length is written through the storage it keeps
  (`typed_array_write`, 0.96).
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
