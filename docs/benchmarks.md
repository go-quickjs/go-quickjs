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
[goja]. All three go-quickjs columns use revision `ea0703f7`, measured alongside
goja on 2026-11-07. Each column is the median of three fresh-process runs
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
| Richards | 2,641 | 13,295 | 12,921 | 543 | 4.86x | 24.48x | 23.80x |
| DeltaBlue | 2,784 | 13,951 | 13,336 | 661 | 4.21x | 21.11x | 20.18x |
| Crypto | 3,286 | 18,216 | 18,272 | 344 | 9.55x | 52.95x | 53.12x |
| RayTrace | 6,265 | 32,716 | 34,819 | 842 | 7.44x | 38.86x | 41.35x |
| EarleyBoyer | 7,668 | 13,452 | 13,428 | 1,514 | 5.06x | 8.89x | 8.87x |
| RegExp | 8,983 | 10,686 | 10,776 | 594 | 15.12x | 17.99x | 18.14x |
| Splay | 12,778 | 21,383 | 21,620 | 2,727 | 4.69x | 7.84x | 7.93x |
| NavierStokes | 5,983 | 38,732 | 38,469 | 571 | 10.48x | 67.83x | 67.37x |
| **Composite score** | **5,452** | **18,338** | **18,383** | **787** | **6.93x** | **23.30x** | **23.36x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
go-quickjs and goja runs:

| Metric | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off relative to goja |
|---|---:|---:|---:|---:|---:|
| Suite wall time | 22.26 s | 20.11 s | 20.10 s | 51.33 s | 2.31x faster |
| Suite CPU time | 26.10 s | 26.00 s | 26.23 s | 72.44 s | 64.0% less |
| Total allocation | 7.87 GiB | 15.92 GiB | 16.03 GiB | 35.21 GiB | 77.6% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. All
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `ea0703f7`, goja `a177d211bf60`, and
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
| JIT off | 5,582 | 5,452 | 5,449 |
| JIT on | 18,573 | 18,338 | 18,262 |
| JIT background | 18,369 | 18,438 | 18,383 |
| goja | 787 | 787 | 777 |

All samples were included. The table takes the median of each workload and
of the printed composite scores independently. Background compilation's
composite was 0.25% higher than synchronous compilation's, within the spread
of these samples; this run does not establish an overall throughput gain.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is ahead of QuickJS. Its
interpreter takes 0.52 times QuickJS's time and has a 41% higher composite
score: it is level on Richards and DeltaBlue and ahead on the other six.
With its JIT it takes 0.24 times QuickJS's time, scores 4.8 times as
much, and is ahead on all eight. On the larger programs of Octane and Are
We Fast Yet the interpreter is ahead too, 0.90 and 0.98 times QuickJS's
time, and the JIT takes 0.49 and 0.23 times
([below](#octane-and-are-we-fast-yet)); a page React 18 renders on the
server takes 0.84 times, and 0.59 with the JIT
([below](#server-rendering)). Single operations cost less on
average, 0.80 times QuickJS's time, and 0.31 times with the JIT, as
[the micro-benchmarks](#micro-benchmarks) below show, though a few still
take about twice as long.

go-quickjs has the same design as QuickJS: a bytecode compiler, a
stack-based interpreter, NaN-boxed values. Running that design in Go costs
speed. The JIT, which is experimental and optional, compiles hot functions
to native code (see its [design notes](../internal/jit/README.md)). The
same suite ran on an AMD Ryzen 5 3600 under Windows, with Go 1.27.3,
go-quickjs `15a29721` and QuickJS 2026-06-04. Two runs
went at once, each on the six CPUs of a level 3 cache of its own. The
JIT's column is a build with `-tags quickjs_jit` run with `-jit`. The
figures are milliseconds for the same fixed work, the median of three
fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 11.8 ms | 7.5 ms | 11.3 ms | 1.04x | 0.66x |
| DeltaBlue | 23.5 ms | 20.9 ms | 24.1 ms | 0.98x | 0.87x |
| Crypto | 292 ms | 68.9 ms | 328 ms | 0.89x | 0.21x |
| RayTrace | 120 ms | 62.7 ms | 142 ms | 0.84x | 0.44x |
| EarleyBoyer | 321 ms | 121 ms | 397 ms | 0.81x | 0.31x |
| RegExp | 147 ms | 128 ms | 861 ms | 0.17x | 0.15x |
| Splay | 181 ms | 126 ms | 495 ms | 0.37x | 0.25x |
| NavierStokes | 186 ms | 38.8 ms | 191 ms | 0.97x | 0.20x |
| **Total** | **1,301 ms** | **599 ms** | **2,486 ms** | **0.52x** | **0.24x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 917 | 4,801 | 952 | 0.96x | 5.04x |
| DeltaBlue | 859 | 5,157 | 839 | 1.02x | 6.15x |
| Crypto | 1,240 | 6,997 | 1,106 | 1.12x | 6.33x |
| RayTrace | 1,912 | 9,342 | 1,591 | 1.20x | 5.87x |
| EarleyBoyer | 2,634 | 4,656 | 2,177 | 1.21x | 2.14x |
| RegExp | 2,906 | 3,637 | 435 | 6.68x | 8.36x |
| Splay | 4,506 | 7,057 | 3,341 | 1.35x | 2.11x |
| NavierStokes | 2,560 | 15,418 | 2,384 | 1.07x | 6.47x |
| **Composite** | **1,894** | **6,452** | **1,340** | **1.41x** | **4.81x** |

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make most of the interpreter's lead. The composite score is the
  geometric mean of the workloads' scores, so each counts the same:
  Richards weighs as much as RegExp. By that mean the interpreter takes
  0.66 times QuickJS's time, and the JIT 0.33 times, where their scores
  are 41% higher and 4.8 times as high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over. The scores time
  warm runs, where Richards, DeltaBlue, Crypto, RayTrace and
  NavierStokes score 4.9 to 6.0 times what the interpreter does.
- Richards is 4% behind in fixed time and in its score, where it
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
  its score is 35% ahead of QuickJS's, where it was 20%. Giving the
  collector four times the room (`GOGC=400`) leaves Splay's score as it
  was, so collection is not what the score measures.
- The JIT changes least where the time is spent in Go code it calls
  rather than in the script's own: the allocation-heavy EarleyBoyer and
  Splay, 1.8 and 1.6 times the interpreter's score, and RegExp, 1.3 times,
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
| Richards | 4.2 ms | 4.3 ms | 3.9 ms | 1.08x | 1.10x |
| DeltaBlue | 8.3 ms | 8.4 ms | 7.6 ms | 1.09x | 1.11x |
| Crypto | 97.9 ms | 27.4 ms | 108 ms | 0.91x | 0.25x |
| RayTrace | 41.5 ms | 38.4 ms | 49.7 ms | 0.84x | 0.77x |
| EarleyBoyer | 111 ms | 55.8 ms | 138 ms | 0.81x | 0.41x |
| RegExp | 123 ms | 112 ms | 465 ms | 0.26x | 0.24x |
| Splay | 155 ms | 108 ms | 469 ms | 0.33x | 0.23x |
| NavierStokes | 67.2 ms | 16.9 ms | 67.0 ms | 1.00x | 0.25x |
| PdfJS | 238 ms | 248 ms | 378 ms | 0.63x | 0.66x |
| Mandreel | 3,294 ms | 1,575 ms | 3,350 ms | 0.98x | 0.47x |
| Gameboy | 491 ms | 325 ms | 464 ms | 1.06x | 0.70x |
| CodeLoad | 13.8 ms | 14.6 ms | 12.9 ms | 1.07x | 1.13x |
| Box2D | 133 ms | 119 ms | 155 ms | 0.85x | 0.77x |
| zlib | 7,644 ms | 3,024 ms | 8,288 ms | 0.92x | 0.36x |
| Typescript | 2,192 ms | 2,135 ms | 2,139 ms | 1.02x | 1.00x |
| **Total** | **15,049 ms** | **8,239 ms** | **16,644 ms** | **0.90x** | **0.49x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 835 ms | 303 ms | 718 ms | 1.16x | 0.42x |
| Richards | 4,751 ms | 792 ms | 4,780 ms | 0.99x | 0.17x |
| Json | 1,246 ms | 271 ms | 1,336 ms | 0.93x | 0.20x |
| CD | 2,546 ms | 516 ms | 2,832 ms | 0.90x | 0.18x |
| Havlak | 4,835 ms | 1,670 ms | 4,341 ms | 1.11x | 0.38x |
| Bounce | 1,284 ms | 249 ms | 1,480 ms | 0.87x | 0.17x |
| List | 1,157 ms | 189 ms | 1,259 ms | 0.92x | 0.15x |
| Mandelbrot | 418 ms | 42.3 ms | 690 ms | 0.61x | 0.06x |
| NBody | 979 ms | 127 ms | 1,026 ms | 0.95x | 0.12x |
| Permute | 1,549 ms | 238 ms | 1,683 ms | 0.92x | 0.14x |
| Queens | 1,065 ms | 126 ms | 1,062 ms | 1.00x | 0.12x |
| Sieve | 1,107 ms | 305 ms | 1,223 ms | 0.91x | 0.25x |
| Storage | 1,664 ms | 736 ms | 1,648 ms | 1.01x | 0.45x |
| Towers | 1,675 ms | 221 ms | 1,557 ms | 1.08x | 0.14x |
| **Total** | **25,142 ms** | **5,800 ms** | **25,659 ms** | **0.98x** | **0.23x** |

- Without the JIT, go-quickjs leads where the V8 suite has it lead --
  RegExp, Splay, the arithmetic of Crypto and NavierStokes, Mandelbrot's
  loops -- and is behind on code that is mostly calls and allocation, by
  as much as 16% (Are We Fast Yet's DeltaBlue) and 11% (Havlak) in this
  run. DeltaBlue moves most from one run to the next: it was 18% behind in
  the run at `9e9f029`, and v0.31.1 and `6a53e07`, five fresh runs each,
  took 924 and 888 ms (their medians). Their classes' common steps -- a field
  read into a local, a comparison with `null`, a field's element, a
  method of a field called, a return -- are now single nodes of the
  tree, and an arrow reads its `this` in place (`6b7a140` to `2d0acf3`);
  List, behind then too, is ahead, and Towers is 8% behind in this run,
  2% at `2d1ca1f7`. Of Octane's long
  programs, Typescript's compiler is 2% behind and CodeLoad's parsing 7%,
  and pdf.js is 37% ahead. The engine's own compiler spends less: Octane's
  sources compile in 0.81 times what they took before `4228b5c4` and
  `b71bc2a9`, 0.76 at one thread, with half the allocations and the same
  bytecode, a difference these tables hardly show, as compiling is a small
  part of a run. The code compiled from C and C++ is level or
  ahead: zlib's 8% ahead, where it was 20% behind at
  `3714c86`, its bitwise arithmetic and its elements at int32 keys run as
  int32 trees since `cf27162`, `5e22bb2` and `f838b8c`; Mandreel's level,
  where it was 19% behind at `882b9d0`, its globals' elements read and
  written as single nodes (`9d03a00`, `cbc097d`) and its negative
  literals taken as immediates (`3f790df`).
- With the JIT, Are We Fast Yet takes 0.06 to 0.45 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Havlak, Storage -- the most.
- On five of Octane's fifteen the JIT is level with the interpreter or
  behind it: level on Richards, DeltaBlue and Typescript, behind on
  CodeLoad (6%) and PdfJS (4%); RayTrace, 22% behind at `6a53e07`, is 7%
  ahead, and DeltaBlue was 12% behind at `915400c`. Octane's fixed work is one run of
  each benchmark, so compiling counts: DeltaBlue's 16 compiles took about
  2.5 ms of its 8.5 at `3714c86`, and a single run's time moves: with
  the JIT, five fresh runs of RayTrace took 42 to 49 ms at `6a53e07`, and
  44 to 49 at v0.31.1. Typescript, 4% behind in the run at `9e9f029`, and
  RegExp, 5% behind, are 3% and 9% ahead.

### Server rendering

The ssr suite renders a page with React 18's own builds, as a host behind
a web server does for each request: Render is one runtime rendering the
page again and again, FirstRender five fresh runtimes, each loading React
and rendering once. The same matrix, the median of three fresh-process
runs (lower is better):

| ssr | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Render | 1,570 ms | 1,099 ms | 1,876 ms | 0.84x | 0.59x |
| FirstRender | 196 ms | 369 ms | | | |

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
`15a29721`, and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.80 times QuickJS's time by the geometric mean: it is
slower on 17, faster on 45, and within 5% on the rest. With the JIT it
takes 0.31 times QuickJS's time, slower on 2 and faster on 66. The table
has the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `array_prop_create` | 32.5 ns | 14.3 ns | 15.0 ns | 2.16x | 0.95x |
| `global_read` | 10.1 ns | 1.68 ns | 5.54 ns | 1.82x | 0.30x |
| `weak_map_delete` | 414 ns | 451 ns | 248 ns | 1.67x | 1.82x |
| `weak_map_set` | 139 ns | 93.0 ns | 92.9 ns | 1.49x | 1.00x |
| `array_read` | 9.42 ns | 1.50 ns | 6.61 ns | 1.43x | 0.23x |
| `global_write` | 10.6 ns | 2.58 ns | 7.62 ns | 1.39x | 0.34x |
| `global_write_strict` | 10.6 ns | 2.58 ns | 7.63 ns | 1.39x | 0.34x |
| `typed_array_read` | 13.4 ns | 2.31 ns | 9.90 ns | 1.35x | 0.23x |
| `empty_do_loop` | 7.13 ns | 0.81 ns | 5.36 ns | 1.33x | 0.15x |
| `int_to_string` | 61.5 ns | 45.4 ns | 49.0 ns | 1.26x | 0.93x |
| `array_pop` | 44.5 ns | 16.3 ns | 111 ns | 0.40x | 0.15x |
| `prop_clone` | 27.6 ns | 9.51 ns | 71.4 ns | 0.39x | 0.13x |
| `regexp_replace` | 333 ns | 335 ns | 889 ns | 0.37x | 0.38x |
| `string_build1` | 34.2 ns | 40.1 ns | 92.3 ns | 0.37x | 0.43x |
| `array_slice` | 9.99 ns | 9.96 ns | 27.2 ns | 0.37x | 0.37x |
| `string_to_int` | 44.6 ns | 43.5 ns | 131 ns | 0.34x | 0.33x |
| `float_to_string` | 122 ns | 122 ns | 360 ns | 0.34x | 0.34x |
| `float_toString` | 123 ns | 111 ns | 367 ns | 0.33x | 0.30x |
| `string_build_large1` | 32.3 ns | 43.0 ns | 101 ns | 0.32x | 0.43x |
| `string_build2c` | 34.0 ns | 40.1 ns | 140 ns | 0.24x | 0.29x |

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.79 ns with it, `func_call` 1.14 ns, a tenth of either interpreter's
time or less. A test whose time is a built-in function's work runs much
as it does in the interpreter: regular expressions, `array_slice`, and
the conversions between numbers and strings. An object a function makes
and drops need not be made at all, even where it crosses a merge or goes
round a loop, as in `sum = sum.add(v)`: it is made only where it escapes,
by partial escape analysis, as in Graal (`41575a42`). `escape_accumulate`
and `carried_short` take 0.07 and 0.04 times QuickJS's time with the JIT,
and 0.82 and 0.74 without it. Code that keeps its objects gains nothing
from this, and the suites above hardly move for it. In the run made for this
page, five of these tests took 7% or more longer with the JIT than
without it: `date_now`, `string_build1`, `string_build2c`,
`string_build_large1` and `weak_map_delete`. Of the extra tests below that
QuickJS runs, so did `async_await`, `async_chain`, `iterator_for_of`,
`template_escaped`, `array_push_args3` and `array_push_args5`,
`object_getter_spread`, `bigint1024_arith`, `typed_i64_read`,
`dataview_read`, `dataview_write`, `string_trim`, `string_chain`,
`regexp_sticky`, `subclass_fresh`, `construct_new_target`,
`carried_every_2`, and five of the `json_` tests. A single run's list
moves: of the 37 in the run at `2d1ca1f7`, 19 are in it.

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
of `JSON.parse` and `JSON.stringify`. Over the 297 QuickJS can run (it has
no using declarations) go-quickjs takes 0.71 times its time, and 0.33 times
with the JIT; leaving out the JSON tests, 0.83 and 0.33. A switch on strings takes a tenth to a fifth of
QuickJS's time, its literal cases one lookup (`100eb7f`); the 64-bit
typed arrays, whose elements are BigInts, are among the largest gaps, a
write of an element 2.0 times QuickJS's time. An await costs much less,
resumed without allocating (`3af4f32`), as does an async generator, and
regular expressions that search a text or split it much less again. A
generator still costs more to resume from for-of, 1.36 times QuickJS's
time, though less than before: it is 96 bytes, made by its kind
(`d8fad84`, `7422742`). A call between two functions in tail position
costs more, as it goes through the pending tail call and its checks, and
so does a global exec, 1.31 times, which makes a result and a String for
each match; a sticky one is level. A for-in over a dictionary takes 1.32 times QuickJS's time,
though its keys now come from the dictionary's table, made strings once
(`212472e`), where it took 3.4 times, and callbacks made in a method
1.26 times (`method_closure`), each closure about twice the size of
QuickJS's; with the JIT 0.19 times, an arrow that reads `this` inlined
where its method makes it and never made (`ad3e6091`). The largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `array_find_upvalue` | 84.6 ns | 11.9 ns | 37.8 ns | 2.24x | 0.31x |
| `typed_i64_forEach` | 85.6 ns | 18.8 ns | 40.9 ns | 2.09x | 0.46x |
| `array_index_append` | 34.8 ns | 21.6 ns | 16.8 ns | 2.07x | 1.29x |
| `typed_i64_write` | 39.8 ns | 40.6 ns | 20.0 ns | 1.99x | 2.03x |
| `typed_u64_forEach` | 143 ns | 103 ns | 79.7 ns | 1.80x | 1.29x |
| `construct_new_target` | 484 ns | 555 ns | 270 ns | 1.80x | 2.06x |
| `typed_i64_reduce` | 64.7 ns | 28.4 ns | 36.9 ns | 1.75x | 0.77x |
| `typed_u64_reduce` | 128 ns | 117 ns | 74.8 ns | 1.71x | 1.56x |
| `array_literal_empty` | 65.5 ns | 61.2 ns | 38.4 ns | 1.71x | 1.60x |
| `tail_mutual` | 58.8 ns | 8.29 ns | 35.2 ns | 1.67x | 0.24x |
| `regexp_search_i` | 2,947 ns | 792 ns | 20,921 ns | 0.14x | 0.04x |
| `json_stringify_last_differs` | 146 ns | 143 ns | 1,038 ns | 0.14x | 0.14x |
| `string_switch_last` | 35.3 ns | 16.3 ns | 264 ns | 0.13x | 0.06x |
| `json_stringify_long` | 27,245 ns | 28,906 ns | 207,066 ns | 0.13x | 0.14x |
| `string_switch_miss` | 33.0 ns | 16.0 ns | 267 ns | 0.12x | 0.06x |
| `json_stringify_decimals` | 34.5 ns | 35.5 ns | 344 ns | 0.10x | 0.10x |
| `string_switch_large` | 39.7 ns | 25.3 ns | 407 ns | 0.10x | 0.06x |
| `string_switch_nonstring` | 24.7 ns | 9.01 ns | 262 ns | 0.09x | 0.03x |
| `regexp_search_utf16` | 702 ns | 750 ns | 13,210 ns | 0.05x | 0.06x |
| `json_parse_long` | 1,817 ns | 1,860 ns | 85,298 ns | 0.02x | 0.02x |

The BigInt tests were the largest gaps, 9.65 and 5.56 times QuickJS's time
at `608415b`, where every BigInt was an allocation. A BigInt in the int64
range is now held in the value itself (`9746906`), as QuickJS holds one
that fits in a word, and so is one below 2^64 in magnitude (`f8c3229`);
a larger one is a block of no pointers, made by the engine's own
arithmetic (`f1bf681`). `bigint64_arith` takes 0.78 times QuickJS's time
and `bigint256_arith` 1.08 times, and the extra BigInt tests 0.65 to 1.16
times. The JIT makes BigInts of 64 bits natively: `bigint64_arith` takes
0.12 times QuickJS's time with it.

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
  `empty_loop` (0.99), `int_arith` (0.98), `float_arith` (0.76),
  `array_update` -- are level with QuickJS or ahead of it, and the six
  loops above take 0.46 to 1.21 times QuickJS's time as trees. A loop whose body
  would need its branches chosen by a structure of their own, an `&&` or an
  else-if chain, still runs from block to block, which costs less.
- **Calls.** A call from JavaScript to JavaScript fills in a frame record of
  some fifteen fields, and each pointer it stores pays a write barrier's
  check. Even so a call costs about what QuickJS's does: a method call
  whose body needs a frame -- one with a loop, even an empty one -- about
  75 ns, against 67 in QuickJS (`method_call`), and a method that only
  returns its argument about 31 ns, against 43 (`method_call_leaf`). With
  the JIT they take 11.5 and 2.8 ns. Richards, DeltaBlue and
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
  rather than making a rope (`00ce03b5`): `string_build2c` takes 0.24 times
  QuickJS's time, `local_append_keep` 0.37 and `template_list` 0.36. The
  interpreter is ahead of the JIT on the first of these, 0.29 times with it.
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
  the key checked once rather than twice (`array_update`, 0.60 times
  QuickJS's time). for-in over an array whose keys are its elements walks
  them by index rather than copying them into a list first
  (`array_for_in`, 0.69), `a[k]` by a key it gives is read from the
  elements (`1039786`), and the array methods pass over a hole
  without looking its index up, where nothing up the prototype chain could
  have it (`array_hole_length_decr`, 0.84). A typed array of a
  fixed length is written through the storage it keeps
  (`typed_array_write`, 0.95).
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
