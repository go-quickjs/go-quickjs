# Benchmarks

The comparison with C QuickJS, under [Compared with C
QuickJS](#compared-with-c-quickjs), was measured at `258b4e96` on the
register interpreter (docs/register-interpreter.md) and the JIT that
compiles its register code. Every other figure on this page was measured
at the revision its section names, before the register interpreter became
the default on amd64 and arm64: their interpreter columns are the stack
interpreter's and the tree tier's, since removed, and their JIT columns
the JIT's compiles of the stack bytecode; the register interpreter's
figures replace them when they are next measured.

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
[goja]. All three go-quickjs columns use revision `c603ede4`, measured alongside
goja on 2026-11-10. Each column is the median of three fresh-process runs
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
| Richards | 2,634 | 14,961 | 15,152 | 534 | 4.93x | 28.02x | 28.37x |
| DeltaBlue | 2,843 | 14,222 | 13,429 | 645 | 4.41x | 22.05x | 20.82x |
| Crypto | 3,344 | 20,893 | 21,051 | 341 | 9.81x | 61.27x | 61.73x |
| RayTrace | 6,191 | 33,596 | 34,409 | 828 | 7.48x | 40.57x | 41.56x |
| EarleyBoyer | 8,024 | 14,229 | 13,756 | 1,517 | 5.29x | 9.38x | 9.07x |
| RegExp | 9,074 | 10,819 | 10,707 | 597 | 15.20x | 18.12x | 17.93x |
| Splay | 13,430 | 23,502 | 22,712 | 2,706 | 4.96x | 8.69x | 8.39x |
| NavierStokes | 6,024 | 40,392 | 39,800 | 574 | 10.49x | 70.37x | 69.34x |
| **Composite score** | **5,551** | **19,544** | **19,337** | **781** | **7.11x** | **25.02x** | **24.76x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
go-quickjs and goja runs:

| Metric | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off relative to goja |
|---|---:|---:|---:|---:|---:|
| Suite wall time | 21.22 s | 20.09 s | 20.09 s | 51.07 s | 2.41x faster |
| Suite CPU time | 24.52 s | 25.97 s | 26.21 s | 72.63 s | 66.2% less |
| Total allocation | 7.02 GiB | 15.53 GiB | 15.14 GiB | 34.73 GiB | 79.8% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. All
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `c603ede4`, goja `a177d211bf60`, and
AreWeFastYet `0e21608`. The isolated goja runner pins
`v0.0.0-20261102223426-a177d211bf60` for this comparison. These results
characterize this older workload suite rather than every application;
go-quickjs leads all eight of its workloads in this comparison.

The comparison used `internal/cmd/v8bench/matrix`, selecting `gq`, `gqjit`,
`gqjitbg` and `goja` in separate invocations with `-suites score -runs 1
-parallel=false -v8 SUITE_DIR` and a separate `-out` directory for each
sample, with 30-second pauses. The engine orders were `gq,gqjit,gqjitbg,goja`,
`gqjit,gqjitbg,goja,gq` and `gqjitbg,goja,gq,gqjit`. `-report` combined the saved outputs
into the median score table. Each engine's three binaries were
byte-identical; both JIT modes used the same binary. The engine source
matched the revision above throughout measurement.

The composite samples were:

| Mode | Run 1 | Run 2 | Run 3 |
|---|---:|---:|---:|
| JIT off | 5,551 | 5,530 | 5,698 |
| JIT on | 19,544 | 19,405 | 19,907 |
| JIT background | 19,337 | 19,340 | 19,283 |
| goja | 781 | 770 | 791 |

All samples were included. The table takes the median of each workload and
of the printed composite scores independently. Background compilation's
composite was 1.06% lower than synchronous compilation's; this run does not
establish an overall throughput advantage for either compilation mode.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is well ahead of QuickJS. Its
interpreter takes 0.37 times QuickJS's time and has 1.97 times its
composite score, ahead on all eight workloads. With its JIT it takes 0.22
times QuickJS's time and scores 5.6 times as much. On the larger programs
of Octane and Are We Fast Yet the interpreter takes 0.70 and 0.68 times
QuickJS's time, and the JIT 0.46 and 0.20 times
([below](#octane-and-are-we-fast-yet)); a page React 18 renders on the
server takes 0.64 times, and 0.54 with the JIT
([below](#server-rendering)). Single operations cost about half as much,
0.49 times QuickJS's time by the geometric mean of its micro-benchmarks,
and 0.28 times with the JIT, as [the micro-benchmarks](#micro-benchmarks)
below show; the slowest of them takes 1.5 times as long.

At `258b4e96`, the revision measured here, go-quickjs compiles every
function to register code -- three-address instructions that name the
frame's registers as their operands, as V8's Ignition and Lua's VM do --
and runs it on the register interpreter (docs/register-interpreter.md):
on amd64 and arm64 by native threaded handlers, machine code that
`internal/vm/internal/nativegen` generates, and elsewhere by a table of
Go handlers. The JIT, which is experimental and optional, compiles a hot
function's register code to native code that runs on the interpreter's
own frame (see its [design notes](../internal/jit/README.md)). The page's
previous figures, at `acffb1aa`, were of the design before it: a
stack-based interpreter after QuickJS's, a tier that ran most functions as
trees of Go closures, and a JIT that compiled the stack bytecode, all
since removed (`388324de`). The same suites ran on an AMD Ryzen 5 3600
under Windows, with Go 1.27.3, go-quickjs `258b4e96` and QuickJS
2026-06-04. Two runs went at once, each on the six CPUs of a level 3 cache
of its own. The JIT's column is a build with `-tags quickjs_jit` run with
`-jit`. The figures are milliseconds for the same fixed work, the median of
three fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 7.7 ms | 6.6 ms | 11.2 ms | 0.69x | 0.59x |
| DeltaBlue | 16.7 ms | 15.9 ms | 23.7 ms | 0.70x | 0.67x |
| Crypto | 156 ms | 54.5 ms | 323 ms | 0.48x | 0.17x |
| RayTrace | 99.1 ms | 52.3 ms | 141 ms | 0.70x | 0.37x |
| EarleyBoyer | 222 ms | 119 ms | 395 ms | 0.56x | 0.30x |
| RegExp | 145 ms | 124 ms | 854 ms | 0.17x | 0.15x |
| Splay | 143 ms | 113 ms | 488 ms | 0.29x | 0.23x |
| NavierStokes | 102 ms | 37.3 ms | 189 ms | 0.54x | 0.20x |
| **Total** | **917 ms** | **547 ms** | **2,454 ms** | **0.37x** | **0.22x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 1,435 | 5,651 | 949 | 1.51x | 5.95x |
| DeltaBlue | 1,220 | 5,840 | 834 | 1.46x | 7.00x |
| Crypto | 2,312 | 8,211 | 1,114 | 2.08x | 7.37x |
| RayTrace | 2,338 | 12,542 | 1,568 | 1.49x | 8.00x |
| EarleyBoyer | 3,263 | 4,956 | 2,178 | 1.50x | 2.28x |
| RegExp | 2,841 | 4,125 | 436 | 6.52x | 9.46x |
| Splay | 5,522 | 8,117 | 3,390 | 1.63x | 2.39x |
| NavierStokes | 4,894 | 16,621 | 2,416 | 2.03x | 6.88x |
| **Composite** | **2,641** | **7,443** | **1,341** | **1.97x** | **5.55x** |

At `acffb1aa` the interpreter took 0.53 times QuickJS's time over the suite
and had 1.41 times its composite score, level on Richards and DeltaBlue;
the register interpreter took nearly a third off the suite's time, and most off
the workloads that are mostly calls, property reads and arithmetic:
Richards 11.8 to 7.7 ms, DeltaBlue 23.2 to 16.7, Crypto 301 to 156,
NavierStokes 172 to 102. The JIT moved less, 592 to 547 ms and a composite
5.20 to 5.55 times QuickJS's.

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make much of the lead. The composite score is the geometric mean of the
  workloads' scores, so each counts the same: Richards weighs as much as
  RegExp. By that mean the interpreter takes 0.47 times QuickJS's time,
  and the JIT 0.29 times, where their scores are 1.97 and 5.55 times as
  high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over: the JIT takes 0.86
  and 0.95 times the interpreter's fixed time there. The scores time warm
  runs, where Richards, DeltaBlue, Crypto, RayTrace and NavierStokes score
  3.4 to 5.4 times what the interpreter does.
- Splay's lead is largest in its fixed time. That is almost all setup and
  teardown: building a tree of 8,000 nodes and dropping it, which QuickJS
  frees by counting references where Go's collector frees it later. A warm
  run, which is what the score times, is splay-tree operations, calls and
  property reads, and a new node for each insertion. Each node's payload
  holds array literals of constants, which share their elements with the
  literal until one is written, as V8's copy-on-write arrays do
  (`a2d9f12e`); its score is 63% ahead of QuickJS's.
- The JIT changes least where the time is spent in Go code it calls
  rather than in the script's own: the allocation-heavy EarleyBoyer and
  Splay, 1.5 times the interpreter's score, and RegExp, 1.45 times, though
  a runtime with the JIT compiles its patterns to machine code as V8's
  Irregexp does (`2be0068`).

Figures move by a few percent between builds from where the linker places
the code, and a single benchmark by up to a tenth. The layout lock holds
the interpreter's hot code -- the Go table's loop, the call path and the
native handlers' block -- at the offsets it measured best
(register-interpreter.md, "Code layout"), so that a change elsewhere does
not move it; what lies between its anchors still moves. To reproduce
these figures, the suites below and the micro-benchmarks, run `go run
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
| Richards | 2.8 ms | 4.1 ms | 3.9 ms | 0.72x | 1.05x |
| DeltaBlue | 5.9 ms | 7.1 ms | 7.7 ms | 0.77x | 0.92x |
| Crypto | 52.4 ms | 22.3 ms | 108 ms | 0.48x | 0.21x |
| RayTrace | 33.5 ms | 30.7 ms | 49.2 ms | 0.68x | 0.62x |
| EarleyBoyer | 76.0 ms | 47.4 ms | 136 ms | 0.56x | 0.35x |
| RegExp | 118 ms | 133 ms | 460 ms | 0.26x | 0.29x |
| Splay | 143 ms | 89.3 ms | 455 ms | 0.31x | 0.20x |
| NavierStokes | 41.4 ms | 17.6 ms | 67.2 ms | 0.62x | 0.26x |
| PdfJS | 193 ms | 257 ms | 374 ms | 0.52x | 0.69x |
| Mandreel | 2,430 ms | 808 ms | 3,321 ms | 0.73x | 0.24x |
| Gameboy | 406 ms | 310 ms | 457 ms | 0.89x | 0.68x |
| CodeLoad | 15.7 ms | 17.7 ms | 12.9 ms | 1.22x | 1.37x |
| Box2D | 104 ms | 105 ms | 153 ms | 0.68x | 0.69x |
| zlib | 5,675 ms | 3,249 ms | 8,290 ms | 0.68x | 0.39x |
| Typescript | 1,768 ms | 2,033 ms | 2,107 ms | 0.84x | 0.97x |
| **Total** | **11,615 ms** | **7,701 ms** | **16,577 ms** | **0.70x** | **0.46x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 578 ms | 314 ms | 711 ms | 0.81x | 0.44x |
| Richards | 3,219 ms | 680 ms | 4,781 ms | 0.67x | 0.14x |
| Json | 1,036 ms | 258 ms | 1,343 ms | 0.77x | 0.19x |
| CD | 1,744 ms | 483 ms | 2,802 ms | 0.62x | 0.17x |
| Havlak | 3,735 ms | 1,171 ms | 4,285 ms | 0.87x | 0.27x |
| Bounce | 800 ms | 246 ms | 1,475 ms | 0.54x | 0.17x |
| List | 655 ms | 152 ms | 1,239 ms | 0.53x | 0.12x |
| Mandelbrot | 176 ms | 40.6 ms | 686 ms | 0.26x | 0.06x |
| NBody | 644 ms | 123 ms | 1,017 ms | 0.63x | 0.12x |
| Permute | 1,010 ms | 229 ms | 1,681 ms | 0.60x | 0.14x |
| Queens | 671 ms | 118 ms | 1,060 ms | 0.63x | 0.11x |
| Sieve | 570 ms | 318 ms | 1,205 ms | 0.47x | 0.26x |
| Storage | 1,505 ms | 746 ms | 1,644 ms | 0.92x | 0.45x |
| Towers | 995 ms | 203 ms | 1,551 ms | 0.64x | 0.13x |
| **Total** | **17,407 ms** | **5,068 ms** | **25,466 ms** | **0.68x** | **0.20x** |

- Without the JIT, go-quickjs is ahead on every benchmark of both suites
  but Octane's CodeLoad. At `acffb1aa` the interpreter was behind on code
  that is mostly calls and allocation, Are We Fast Yet's DeltaBlue by 17%
  and Havlak by 10%, and level on Richards, Queens and Towers; it is now
  19% ahead on DeltaBlue, 13% on Havlak, and about a third ahead on those
  three.
  A call's window of registers is the callee's frame, so nothing is
  copied, a frame record is written only where a reader needs it, and on
  amd64 and arm64 a call, a construction and a return are native handlers
  that never enter Go ([Calls](#where-the-time-goes) below). The programs
  that allocate most are the least ahead: Storage 8%, Havlak 13%.
- Octane's CodeLoad, which is mostly compiling, takes 1.22 times QuickJS's
  time, and 1.37 with the JIT, where it took 1.09 and 1.12 at `acffb1aa`:
  the front end still compiles every function to the stack bytecode
  before the register backend compiles its register code
  (register-interpreter.md, "Known limits and open work"), and the
  register JIT does nothing for CodeLoad yet.
- The code compiled from C and C++ is further ahead than it was: zlib
  takes 0.68 times QuickJS's time and Mandreel 0.73, where they took 0.92
  and 0.98. Their `| 0` arithmetic runs as int32 instructions (`add_i32`,
  `add_imm_i32`, the bitwise operators with an immediate) and their typed
  arrays' elements are read and written by native handlers, though each
  element read and each `| 0` still converts through a float, where the
  tree tier had kept an int32 in a Go variable; raw int32 registers are
  designed and not built (register-interpreter.md, "Known limits and open
  work").
- With the JIT, Are We Fast Yet takes 0.06 to 0.45 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Storage -- the most. Mandreel takes 0.24 times QuickJS's time
  with it, against 0.28 at `acffb1aa`; zlib takes 0.39, against 0.24: the
  register JIT is furthest behind the stack JIT on zlib, 3,249 against
  2,000 ms, which is not profiled yet.
- When the stack JIT was removed, its timing had the register JIT slower
  than the stack JIT just before it on Octane's RayTrace (+7.6%) and
  CodeLoad (+25%) and on Are We Fast Yet's Json (+8.2%), Permute (+7.0%)
  and Queens (+6.3%) (docs/performance-plan.md, "The register JIT's
  remaining gaps"). Against this page's figures at `acffb1aa`, Json,
  Permute and Queens are level (270, 227 and 119 ms then), CodeLoad is
  slower, 14.2 to 17.7 ms, and RayTrace moves too much from one run to the
  next to tell: its six runs in the two matrices made for this page took 30
  to 50 ms.
- On seven of Octane's fifteen the JIT is behind the interpreter or level
  with it: Richards (46% behind), PdfJS (33%), DeltaBlue (20%), Typescript
  (15%), RegExp (13%) and CodeLoad (13%) behind, Box2D level. At
  `acffb1aa` it was behind on Richards and DeltaBlue alone, and level on
  RayTrace, PdfJS and CodeLoad. Octane's fixed work is one run of each
  benchmark, so compiling counts, and the code runs in the interpreter
  until it is hot. The JIT's own figures are much what they were --
  Richards 4.4 ms then, DeltaBlue 8.1, PdfJS 246, Typescript 2,107 --
  while the interpreter's fell by a fifth to a third on those four, which
  leaves less for the JIT's code to win back. RegExp with the JIT is
  slower than it was, 111 to 133 ms, and so is CodeLoad.

### Server rendering

The ssr suite renders a page with React 18's own builds, as a host behind
a web server does for each request: Render is one runtime rendering the
page again and again, FirstRender five fresh runtimes, each loading React
and rendering once. The same matrix, the median of three fresh-process
runs (lower is better):

| ssr | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Render | 1,193 ms | 1,013 ms | 1,871 ms | 0.64x | 0.54x |
| FirstRender | 164 ms | 296 ms | | | |

Render took 0.84 and 0.58 times QuickJS's time at `acffb1aa`, and
FirstRender 197 and 366 ms. React writes a page as thousands of short
chunks appended to a string a closure holds, and an append grows that
string where it is, when nothing else can see it (`00ce03b5`). An empty
literal learns the room its arrays grow to (`c62a9bb0`), and a for-in
takes the last loop's cursor once that loop has finished (`5326a3f8`).

QuickJS's runner loads a script once, so it has no FirstRender. A fresh
runtime's render is where the JIT costs most: these compiles are made on
the runtime's goroutine, which waits for each, as a runtime made without
`WithJITBackground` does; `qjs --jit` compiles in the background.

### Micro-benchmarks

QuickJS's own micro-benchmarks, [`tests/microbench.js`](https://github.com/bellard/quickjs/blob/master/tests/microbench.js),
time one operation each, in nanoseconds. Each test was given the work QuickJS
takes about 150 ms for, all three ran that same work in fresh processes at
`258b4e96`, and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.49 times QuickJS's time by the geometric mean, where it
took 0.81 at `acffb1aa`: it is slower on 6, faster on 64, and within 5% on
the other one. With the JIT it takes 0.28 times QuickJS's time, slower on 2
and faster on 65. The table has the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `weak_map_delete` | 379 ns | 442 ns | 249 ns | 1.52x | 1.78x |
| `weak_map_set` | 128 ns | 92.4 ns | 93.8 ns | 1.37x | 0.98x |
| `bigint256_arith` | 166 ns | 120 ns | 126 ns | 1.32x | 0.95x |
| `int_to_string` | 56.7 ns | 50.1 ns | 48.5 ns | 1.17x | 1.03x |
| `date_parse` | 967 ns | 986 ns | 830 ns | 1.17x | 1.19x |
| `func_closure_call` | 35.4 ns | 2.81 ns | 31.7 ns | 1.12x | 0.09x |
| `regexp_utf16` | 257 ns | 264 ns | 256 ns | 1.00x | 1.03x |
| `array_prop_create` | 14.2 ns | 15.7 ns | 15.0 ns | 0.94x | 1.04x |
| `float_toExponential` | 160 ns | 133 ns | 180 ns | 0.89x | 0.74x |
| `map_set_int` | 143 ns | 112 ns | 161 ns | 0.89x | 0.69x |
| `global_func_call` | 8.54 ns | 2.11 ns | 32.2 ns | 0.26x | 0.07x |
| `string_length` | 3.73 ns | 0.91 ns | 14.2 ns | 0.26x | 0.06x |
| `string_build_large1` | 26.7 ns | 45.2 ns | 102 ns | 0.26x | 0.44x |
| `prop_update` | 2.81 ns | 1.89 ns | 11.5 ns | 0.25x | 0.17x |
| `func_call` | 7.10 ns | 0.95 ns | 30.6 ns | 0.23x | 0.03x |
| `array_for_in` | 15.1 ns | 11.8 ns | 76.9 ns | 0.20x | 0.15x |
| `string_build2c` | 26.3 ns | 42.9 ns | 140 ns | 0.19x | 0.31x |
| `array_for_of` | 7.16 ns | 4.83 ns | 46.3 ns | 0.15x | 0.10x |
| `array_slice` | 4.08 ns | 4.57 ns | 27.4 ns | 0.15x | 0.17x |
| `prop_clone` | 11.1 ns | 9.06 ns | 81.9 ns | 0.14x | 0.11x |

Of the gaps the stack design had, the largest are gone: `array_prop_create`
took 2.15 times QuickJS's time and takes 0.94, `global_read` 1.81 and
0.86, `array_read` 1.42 and 0.85, `empty_do_loop` 1.34 and 0.52.

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.79 ns with it, `func_call` 0.95 ns, a fifth of the interpreter's time
or less. A test whose time is a built-in function's work runs much as it
does in the interpreter: regular expressions, `array_slice`, and the
conversions between numbers and strings. An object a function makes and
drops need not be made at all, even where it crosses a merge or goes round
a loop, as in `sum = sum.add(v)`: it is made only where it escapes, by
partial escape analysis, as in Graal (`41575a42`). `escape_accumulate`
and `carried_short` take 0.06 and 0.03 times QuickJS's time with the JIT,
and 0.64 and 0.62 without it. Code that keeps its objects gains nothing
from this, and the suites above hardly move for it.

The interpreter has gained more than the JIT, so more tests than before
run slower with the JIT than without it. In the run made for this page, 12
of these 71 took 7% or more longer with the JIT: `prop_delete`,
`array_update`, `array_prop_create`, `array_slice`, `local_destruct`,
`global_destruct`, `global_destruct_strict`, `weak_map_delete`,
`string_build1`, `string_build2c`, `string_build_large1` and
`string_to_int`. Of the extra tests below that QuickJS runs, 38 did:
`generator_delegate`, `async_await`, `async_call` and `async_generator`,
three for-in loops over an object, `hasOwnProperty` called through a
borrowed reference, pushes and spread calls of several arguments,
`object_rest`, four of the five sorts, Map's and Set's forEach, a global
`match` and the regular-expression searches, objects made on fresh
prototypes, five of the `json_` tests, three of the `carried_` tests,
`array_index_append`, `template_numbers`, `prop_delete_last`, and the
64-bit typed arrays' writes, which take 5.3 to 5.6 times the
interpreter's time with the JIT (`typed_i64_write` 6.79 against 37.8 ns),
1.47 and 1.89 times QuickJS's.

Some of these tests depend on where the linker places the code more than
on the code. The layout lock keeps the interpreter's hot code where it
measured best, but the code between its anchors, the Go handlers after the
loop and the closures still move with every change to the package, and a
change of a few per cent in one of these tests is not by itself a change in
the engine.

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
of `JSON.parse` and `JSON.stringify`. Over the 318 QuickJS can run (it has
no using declarations) go-quickjs takes 0.53 times its time, and 0.26 times
with the JIT, where it took 0.71 and 0.30 over the 300 at `acffb1aa`;
leaving out the JSON tests, 0.59 and 0.25. A switch on strings past its
first few cases takes 0.05 to 0.16 times QuickJS's time, its literal cases
one lookup (`100eb7f`). An await costs a fiftieth of
QuickJS's time (`async_await` 14.9 against 754 ns): an async function
resumed by its await's job goes on in place at an await of a value that
needs no job of its own. A generator resumed from for-of is level
(`generator_for_of` 1.03), where it took 1.33 times, and a for-in over a
dictionary takes 0.22 times, where it took 1.36: its keys are made once
for the dictionary's layout and handed out by the native handlers. Calls
in tail position take 0.57 to 0.61 times QuickJS's time. A global `exec`
still takes 1.30 times, as it makes a result and a String for each
match; a sticky one is level. The largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `scope_chain` | 647 ns | 676 ns | 363 ns | 1.78x | 1.86x |
| `set_proto_fresh` | 350 ns | 375 ns | 210 ns | 1.67x | 1.79x |
| `array_literal_empty` | 65.1 ns | 61.8 ns | 39.8 ns | 1.64x | 1.55x |
| `subclass_fresh` | 1,118 ns | 1,210 ns | 684 ns | 1.64x | 1.77x |
| `typed_u8_find` | 43.8 ns | 8.73 ns | 27.0 ns | 1.62x | 0.32x |
| `array_fill_down` | 133 ns | 135 ns | 86.3 ns | 1.54x | 1.56x |
| `typed_u64_forEach` | 122 ns | 99.8 ns | 80.1 ns | 1.52x | 1.25x |
| `typed_findIndex` | 46.2 ns | 8.37 ns | 30.6 ns | 1.51x | 0.27x |
| `typed_find` | 45.2 ns | 8.75 ns | 30.5 ns | 1.48x | 0.29x |
| `object_create_fresh` | 334 ns | 361 ns | 226 ns | 1.48x | 1.60x |
| `object_spread_large` | 182 ns | 187 ns | 1,585 ns | 0.12x | 0.12x |
| `string_switch_last` | 27.2 ns | 13.3 ns | 262 ns | 0.10x | 0.05x |
| `json_stringify_decimals` | 32.7 ns | 36.5 ns | 331 ns | 0.10x | 0.11x |
| `string_switch_large` | 28.9 ns | 21.4 ns | 407 ns | 0.07x | 0.05x |
| `string_switch_miss` | 17.6 ns | 13.1 ns | 265 ns | 0.07x | 0.05x |
| `regexp_search_utf16` | 692 ns | 740 ns | 13,053 ns | 0.05x | 0.06x |
| `string_switch_nonstring` | 12.1 ns | 7.86 ns | 262 ns | 0.05x | 0.03x |
| `regexp_search_i` | 659 ns | 745 ns | 20,772 ns | 0.03x | 0.04x |
| `json_parse_long` | 1,829 ns | 1,866 ns | 84,916 ns | 0.02x | 0.02x |
| `async_await` | 14.9 ns | 16.8 ns | 754 ns | 0.02x | 0.02x |

A BigInt in the int64 range is held in the value itself (`9746906`), as
QuickJS holds one that fits in a word, and so is one below 2^64 in
magnitude (`f8c3229`); a larger one is a block of no pointers, made by the
engine's own arithmetic (`f1bf681`). Short BigInts' `+`, `-`, `*` and
comparisons are native handlers: `bigint64_arith` takes 0.37 times
QuickJS's time. A larger one is made by the engine's arithmetic in Go:
`bigint256_arith` takes 1.32 times, and the extra BigInt
tests 0.43 to 1.26 times, `bigint_toString` and `bigint64_xorshift` the
slowest. The JIT makes BigInts of 64 bits natively: `bigint64_arith`
takes 0.10 times QuickJS's time with it, `bigint64_xorshift` 0.05.

The largest gaps now are:

- objects made on a prototype that is itself new -- `Object.create` of a
  fresh object, `Object.setPrototypeOf` to one, a scope chain built as
  objects, a subclass made per call and its instance -- 1.48 to 1.78
  times QuickJS's time;
- arrays made empty or grown downward from past their end
  (`array_literal_empty`, `array_fill_down`);
- the typed arrays' `find` and `findIndex` in the interpreter, and the
  64-bit typed arrays' reads and callbacks, whose elements are BigInts;
- the weak collections. A WeakMap holds each value on its key rather than
  in a table of its own (`2bc1620`), so that a value which refers to its
  key does not keep the key alive, as the specification requires; adding
  an entry costs more for it;
- a global `exec`, which makes a result and a String for each match, and
  a `replace` by a template with `$` (1.30 and 1.33 times).

The suite as a whole does better than the operations it is made of
because of the places go-quickjs leads by most: strings and regular
expressions over whole texts, which Go's runtime and the engine's matcher
do well, and Splay's allocation-heavy setup.

#### Where the time goes

- **Dispatching instructions.** Register code names its operands in the
  instruction, so an expression is one instruction rather than the pushes
  and pops of a stack machine: Crypto's `am3` runs 25 register
  instructions a turn where the stack form ran 34. On amd64 and arm64
  each instruction is run by a native handler that ends in a dispatch of
  its own -- the next instruction read, its handler's address loaded from
  the table, an indirect jump: four instructions -- with the interpreter's
  state pinned in machine registers across it, which Go cannot do: in Go
  every handler returns to a loop and is called again, its state stored
  and reloaded around the call. 128 of the 241 operations have a native
  handler; the rest, and every fast path that misses, go to their Go
  handler through an exit that calls it in Go's convention
  (register-interpreter.md, "Native handlers"). The six loops of simple
  instructions -- `empty_loop` and the extra tests `loop_sum`,
  `loop_arith`, `loop_locals`, `loop_elements` and `loop_props` -- take
  0.21 to 0.67 times QuickJS's time, 0.43 by their geometric mean, where
  the stack interpreter took 1.8 times and the tree tier 0.46 to 1.21.
  `int_arith` takes 0.46 times QuickJS's time and `float_arith` 0.35.
- **Calls.** A call's window of registers is the callee's frame: the
  caller writes the callee, the receiver and the arguments there, and
  nothing is copied. A callee the call site's cache knows is entered with
  no question asked again, and only the fields of the frame record a
  reader needs are written. Natively, a call, a construction and a return
  are handlers of their own that keep the caller's state on a native
  frame stack and never enter Go (register-interpreter.md, "Frames and
  calls"). A method call whose body needs a frame takes 35.3 ns against
  66.8 in QuickJS (`method_call`), one that only returns its argument
  13.0 against 42.1 (`method_call_leaf`), an empty function 7.10 against
  30.6 (`func_call`); with the JIT they take 11.6, 2.61 and 0.95 ns. A
  recursive call is level: `call_fib` takes 0.92 times QuickJS's time and
  `call_mutual` 0.95. A body that only returns a parameter or a constant,
  or a short pure one of numbers and upvalues, is answered at the call
  site without a frame.
- **Property access.** Objects built the same way share a shape, as in
  QuickJS and V8, and each property read and write, global variable read
  and write, call and `instanceof` remembers where it last found what it
  looked for; a site that meets a second shape keeps up to four, and one
  that keeps meeting new shapes reads through the realm's shared cache, as
  V8's megamorphic stub cache does. The native handlers check the cache
  where the instruction runs: `prop_read` takes 0.50 times QuickJS's time,
  `prop_write` 0.37, `global_read` 0.86 and `global_write` 0.43. An object
  holds only its values, its keys in its shape, as a QuickJS object does
  (`944e2e8`).
- **Memory management.** QuickJS counts references: an object is freed the
  moment the last reference to it goes, and a cycle collector handles the
  rest. go-quickjs relies on Go's garbage collector. It traces the live heap
  concurrently and needs a write barrier on every pointer stored. The
  native handlers store a number without one, and a pointer through the
  barrier's routine only while the collector marks. The collector does most
  of its work on other cores, so with cores to spare it costs less
  wall-clock time than CPU time. A program that allocates heavily still
  pays for it in CPU time: Storage and Havlak, which allocate most, are
  the least ahead.
- **Numbers as Values.** Every register holds a Value, so an element read
  and a `| 0` convert through a float where an int32 would do; raw int32
  and float64 registers are designed and not built.

Where go-quickjs is ahead by most:

- **Crypto.** Its arithmetic is register code whose bitwise operators have
  forms with an immediate, and `(x + y) | 0` is one instruction, all
  native handlers:
  `crypto_am3` takes 0.70 times QuickJS's time, 0.19 with the JIT, and
  Crypto 0.48.
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
  rather than making a rope (`00ce03b5`): `string_build2c` takes 0.19 times
  QuickJS's time, `local_append_keep` 0.27 and `template_list` 0.34. The
  interpreter is ahead of the JIT on the first of these, 0.31 times with it.
- **JSON.** The 49 `json_` tests take 0.30 times QuickJS's time, and 0.31
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
  `15a29721`).
- **Array elements updated, walked and skipped.** `a[k] += y` and `a[k]++`
  are one instruction, `add_index`, the element updated where it is, with
  the array and the key checked once (`array_update`, 0.28 times QuickJS's
  time). A for-in over an array whose keys are its elements walks them by
  index rather than copying them into a list first (`array_for_in`, 0.20),
  and the array methods pass over a hole without looking its index up,
  where nothing up the prototype chain could have it
  (`array_hole_length_decr`, 0.35). A typed array's elements are read and
  written by native handlers for every element type but Float16
  (`typed_array_write`, 0.41).
- **`apply(this, arguments)`.** In a function whose only use of
  `arguments` is passing it to `apply` (a common way to write a class
  constructor), the arguments object is never made: the call hands on the
  frame's arguments as they were passed (`apply_args`). A constructor
  whose whole body is `this.initialize.apply(this, arguments)`, as
  RayTrace's are, is constructed by its call site as a forwarder
  (`forward_construct`, 0.54 times QuickJS's time, 0.02 with the JIT).

What remains in the interpreter is mostly in Go: the operations with no
native handler and the fast paths that miss, which exit to their Go
handlers, the built-ins and the object model -- the gaps above are in
them -- and the collector. The JIT is the way past the rest: a hot
function compiled to native code dispatches nothing, which is why, with
it, the loops and calls above take from a third of the interpreter's
time down to a tenth.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
