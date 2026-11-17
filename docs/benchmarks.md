# Benchmarks

The Go benchmarks, bundle timings, and V8 comparison with goja below were
measured at `b37daa23` on the register interpreter and the JIT that compiles
its register code. The comparison with C QuickJS, under [Compared with C
QuickJS](#compared-with-c-quickjs), was measured at `3ecb8200`. Other
comparisons retain the revisions and measurement dates their sections name.

On an Apple M5 Max with Go 1.27.3 and the native JIT disabled, taking
per-metric medians of five fresh-process runs at go-quickjs `b37daa23` on
2026-11-16, with 30 seconds between runs and AC power throughout:

```
BenchmarkEvalArithmetic-18  1.23µs/op  4170 B/op  28 allocs/op
BenchmarkFibonacci-18        333µs/op    20 B/op   1 allocs/op
BenchmarkPropertyAccess-18  2.14µs/op  6442 B/op  36 allocs/op
BenchmarkCallGoFunction-18  1.91µs/op  6226 B/op  43 allocs/op
```

`fib(20)` costs one allocation because the interpreter loop allocates nothing
per call; the other benchmarks include parsing and compiling their source each
iteration.

`go test -run '^$' -bench . -benchmem .` also runs compile-once interpreter
workloads, plus JSON parsing and serialization, compilation, runtime
construction, and a recursive factorial workload. The remaining root-package
benchmarks are:

```
BenchmarkLoopArithmetic-18        31.3µs/op      0 B/op     0 allocs/op
BenchmarkDateUTC-18                262µs/op      0 B/op     0 allocs/op
BenchmarkLoopPropertyAccess-18     121µs/op      0 B/op     0 allocs/op
BenchmarkLoopArrayIndex-18        6.72µs/op      0 B/op     0 allocs/op
BenchmarkLoopFunctionCall-18       156µs/op      0 B/op     0 allocs/op
BenchmarkLoopMethodCall-18         174µs/op      0 B/op     0 allocs/op
BenchmarkAllocObjects-18          42.3µs/op   160 KB/op  2000 allocs/op
BenchmarkAllocArrays-18           56.3µs/op   256 KB/op  2000 allocs/op
BenchmarkStringConcat-18          19.6µs/op  12.7 KB/op    13 allocs/op
BenchmarkClosureCreateAndCall-18   402µs/op  1.12 MB/op  5001 allocs/op
BenchmarkArrayCallbacks-18        28.9µs/op  36.1 KB/op    18 allocs/op
BenchmarkMapOperations-18          108µs/op   311 KB/op    25 allocs/op
BenchmarkRegExpExec-18             283µs/op   320 KB/op  1000 allocs/op
BenchmarkThrowCatch-18             211µs/op   672 KB/op  6000 allocs/op
BenchmarkJSONRoundTrip-18          161µs/op   232 KB/op  3000 allocs/op
BenchmarkFactorial-18             16.3ms/op   417 KB/op  2284 allocs/op
BenchmarkCompileScript-18         22.9µs/op  42.6 KB/op   343 allocs/op
BenchmarkNewRuntime-18            71.9µs/op   317 KB/op  2146 allocs/op
BenchmarkNewRuntimeSmallStack-18  82.9µs/op   314 KB/op  2146 allocs/op
BenchmarkJSONParse-18              278µs/op   652 KB/op  4751 allocs/op
BenchmarkJSONStringify-18          221µs/op   826 KB/op    21 allocs/op
```

Each compile-once benchmark runs its workload many times: the main loop
benchmarks use 10,000 iterations, the object and array benchmarks allocate
2,000 values, and the callback, collection, regexp, exception and JSON
benchmarks exercise their operation hundreds or thousands of times.
`CompileScript` parses, compiles and evaluates one program per operation,
while the runtime benchmarks construct and close one runtime per operation.
`Factorial` constructs one runtime and evaluates 100,000 calls to
`factorial(10)` per operation, totaling one million recursive calls
(16.3 ns/call). Each of the five samples came from a separate invocation,
with one second per benchmark and `-benchmem`, not repeated measurements
inside a single test process. `JSONParse` and `JSONStringify` process a
1,000-record document, with input construction and compilation outside the
timed interval.

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
`b37daa23` gives these per-metric medians, measured on 2026-11-16. Compiler
figures use five fresh processes, each running both bundles with 500 ms per
benchmark. Load figures use twenty fresh processes per bundle, one load per
process. One process runs at a time, with 30-second pauses between the five
compiler rounds and between the twenty load rounds. Each load round runs
both bundles in separate processes, alternating their order. AC power was
used throughout, with no concurrent builds or tests. All samples are
included, including the first.

| Bundle | Compile | First load |
|---|---:|---:|
| Three.js | 13.23 ms | 40.63 ms |
| Babel | 36.32 ms | 212.88 ms |

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
| Three.js | 7.85 | 26.74 | 50.20 |
| Babel | 25.62 | 118.02 | 136.52 |

The `qjs` binary built without the JIT, using `CGO_ENABLED=0 go build
-pgo=off -trimpath ./cmd/qjs`, is 42,792,210 bytes. The load test binary is
41,065,618 bytes, and the compiler test binary is 9,343,618 bytes. The
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
[goja]. All three go-quickjs columns use revision `b37daa23`, measured alongside
goja on 2026-11-16. Each column is the median of three fresh-process runs
on an Apple M5 Max with Go 1.27.3, with 30 seconds between runs and AC
power throughout. One engine ran at a time, with the order rotated each
round. JIT off was built without the `quickjs_jit` tag. JIT on compiles
synchronously; JIT background enables the JIT and compiles in the background
with `-jit -jitbackground`. Both use the same binary built with
`-tags quickjs_jit` and the runner's 32 MiB JIT budget rather than the
runtime's default 8 MiB. This revision uses the register interpreter and
register JIT by default.
Higher suite scores are better; the relative columns divide each
go-quickjs score by goja's.

| Workload | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off / goja | JIT on / goja | JIT background / goja |
|---|---:|---:|---:|---:|---:|---:|---:|
| Richards | 3,160 | 16,182 | 16,189 | 538 | 5.87x | 30.08x | 30.09x |
| DeltaBlue | 2,860 | 14,877 | 14,956 | 644 | 4.44x | 23.10x | 23.22x |
| Crypto | 5,311 | 22,291 | 22,399 | 350 | 15.17x | 63.69x | 64.00x |
| RayTrace | 6,499 | 35,522 | 36,777 | 849 | 7.65x | 41.84x | 43.32x |
| EarleyBoyer | 8,782 | 14,163 | 14,221 | 1,503 | 5.84x | 9.42x | 9.46x |
| RegExp | 9,110 | 11,410 | 11,376 | 593 | 15.36x | 19.24x | 19.18x |
| Splay | 15,402 | 24,130 | 24,187 | 2,727 | 5.65x | 8.85x | 8.87x |
| NavierStokes | 10,737 | 40,176 | 40,662 | 582 | 18.45x | 69.03x | 69.87x |
| **Composite score** | **6,707** | **20,434** | **20,544** | **782** | **8.58x** | **26.13x** | **26.27x** |

The runner's timed suite interval includes runtime construction, parsing,
compilation, the suite's warmups and its measured iterations, but excludes
reading the source files and the final forced garbage collection. CPU time
includes work on other threads, including the collector. These are the
go-quickjs and goja runs:

| Metric | go-quickjs JIT off | go-quickjs JIT on | go-quickjs JIT background | goja | JIT off relative to goja |
|---|---:|---:|---:|---:|---:|
| Suite wall time | 20.18 s | 20.10 s | 20.10 s | 50.80 s | 2.52x faster |
| Suite CPU time | 23.33 s | 26.21 s | 26.49 s | 71.99 s | 67.6% less |
| Total allocation | 7.39 GiB | 16.40 GiB | 16.70 GiB | 34.91 GiB | 78.8% less |

Score mode does different amounts of work as throughput changes, so the CPU
and allocation totals are not equal-work efficiency measurements. The
composite is the median of the three printed composite scores, not a
composite calculated from the workload medians.

The suite warms every workload for at least one second, then measures for at
least another second and continues until it has 32 measured iterations. All
engines evaluated the same benchmark sources and validation code.
The measured revisions were go-quickjs `b37daa23`, goja `a177d211bf60`, and
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
| JIT off | 6,797 | 6,481 | 6,707 |
| JIT on | 20,434 | 20,508 | 19,322 |
| JIT background | 20,544 | 20,671 | 18,695 |
| goja | 782 | 792 | 764 |

All samples were included. The table takes the median of each workload and
of the printed composite scores independently. Background compilation's
composite was 0.54% higher than synchronous compilation's; this run does not
establish an overall throughput advantage for either compilation mode.

## Compared with C QuickJS

Over the V8 suite as a whole, go-quickjs is well ahead of QuickJS. Its
interpreter takes 0.35 times QuickJS's time and has 2.05 times its
composite score, ahead on all eight workloads. With its JIT it takes 0.22
times QuickJS's time and scores 5.5 times as much. On the larger programs
of Octane and Are We Fast Yet the interpreter takes 0.44 and 0.66 times
QuickJS's time, and the JIT 0.31 and 0.20 times
([below](#octane-and-are-we-fast-yet)); a page React 18 renders on the
server takes 0.64 times, and 0.55 with the JIT
([below](#server-rendering)). Single operations cost about half as much,
0.50 times QuickJS's time by the geometric mean of its micro-benchmarks,
and 0.28 times with the JIT, as [the micro-benchmarks](#micro-benchmarks)
below show; the slowest of them takes 1.5 times as long.

At `3ecb8200`, the revision measured here, go-quickjs compiles every
function to register code -- three-address instructions that name the
frame's registers as their operands, as V8's Ignition and Lua's VM do --
and nothing else: the stack bytecode is gone from the front end too
(`d6ac45be`). It runs that code on the register interpreter
(docs/register-interpreter.md): on amd64 and arm64 by native threaded
handlers, machine code that `internal/vm/internal/nativegen` generates,
and elsewhere by a table of Go handlers. A function of a validated asm.js
module -- code compiled from C, as Octane's zlib is -- is also compiled to
typed code, whose registers each hold a raw int32 or float64 or a Value for
the whole function, as the validator proved, and which runs by a typed
table of its own, natively too (register-interpreter.md, "Typed code";
`d8cbc918`). The JIT, which is experimental and optional, compiles a hot
function's register code to native code that runs on the interpreter's
own frame, and an asm.js module's typed code at its link, with no
speculation (see its [design notes](../internal/jit/README.md);
`5ae2317e`). The page's previous figures, at `258b4e96`, were of the same
register design before the typed code, the frameless native calls below
and the stack form's removal from the front end. The same suites ran on an
AMD Ryzen 5 3600 under Windows, with Go 1.27.3, go-quickjs `3ecb8200` and
QuickJS 2026-06-04. Two runs went at once, each on the six CPUs of a level
3 cache of its own. The JIT's column is a build with `-tags quickjs_jit`
run with `-jit`. The figures are milliseconds for the same fixed work, the
median of three fresh-process runs (lower is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 7.9 ms | 8.8 ms | 11.4 ms | 0.69x | 0.77x |
| DeltaBlue | 15.4 ms | 16.1 ms | 24.0 ms | 0.64x | 0.67x |
| Crypto | 156 ms | 55.5 ms | 326 ms | 0.48x | 0.17x |
| RayTrace | 77.1 ms | 54.0 ms | 140 ms | 0.55x | 0.39x |
| EarleyBoyer | 205 ms | 118 ms | 398 ms | 0.52x | 0.30x |
| RegExp | 145 ms | 131 ms | 862 ms | 0.17x | 0.15x |
| Splay | 141 ms | 108 ms | 488 ms | 0.29x | 0.22x |
| NavierStokes | 102 ms | 36.5 ms | 192 ms | 0.53x | 0.19x |
| **Total** | **865 ms** | **550 ms** | **2,469 ms** | **0.35x** | **0.22x** |

The suite's own scores, the median of three (higher is better):

| Workload | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Richards | 1,350 | 5,696 | 957 | 1.41x | 5.95x |
| DeltaBlue | 1,406 | 5,865 | 845 | 1.66x | 6.94x |
| Crypto | 2,343 | 8,243 | 1,111 | 2.11x | 7.42x |
| RayTrace | 3,056 | 12,603 | 1,592 | 1.92x | 7.92x |
| EarleyBoyer | 3,403 | 4,939 | 2,186 | 1.56x | 2.26x |
| RegExp | 2,895 | 4,087 | 438 | 6.61x | 9.33x |
| Splay | 5,490 | 7,986 | 3,375 | 1.63x | 2.37x |
| NavierStokes | 4,899 | 16,555 | 2,407 | 2.04x | 6.88x |
| **Composite** | **2,781** | **7,382** | **1,354** | **2.05x** | **5.45x** |

At `258b4e96` the interpreter took 0.37 times QuickJS's time over the suite
and had 1.97 times its composite score. It gained most on the workloads
that construct many small objects and call small methods: RayTrace 99.1 to
77.1 ms and a score 31% higher, DeltaBlue 16.7 to 15.4 ms and a score 15%
higher, EarleyBoyer 222 to 205 ms. On amd64 and arm64 a call of a body
that only stores its parameters in `this` (an initializer), of a
constructor that only forwards to one with `this.initialize.apply(this,
arguments)`, as RayTrace's do, of a getter of `this.k`, `this.k.length` or
`this.k[p]`, as DeltaBlue's methods are, of a sum, difference or product
of its parameters or captured values, or of a `typeof` test, is now
answered by the native call and construction handlers without a frame
(`cf1bbfda`, `d54099e3`, `09409332`, `746d9438`, `bdab5ba4`). Those are
the plain interpreter's handlers: a runtime with the JIT calls through its
own, which keep the frames, and the JIT moved little, 547 to 550 ms, and a
composite 5.55 to 5.45 times QuickJS's.
v0.38.0 (`440db6bd`), the last release of the stack interpreter, measured
in the same session, takes 0.51 and 0.23 times QuickJS's time.

The two kinds of figure time different things, which is why they do not
agree on how far ahead go-quickjs is:

- A fixed run times each workload's setup, its runs and its teardown. The
  scored run times only the runs, after a second of warming up.
- The total adds milliseconds, so the longest workloads, RegExp and Splay,
  make much of the lead. The composite score is the geometric mean of the
  workloads' scores, so each counts the same: Richards weighs as much as
  RegExp. By that mean the interpreter takes 0.45 times QuickJS's time,
  and the JIT 0.30 times, where their scores are 2.05 and 5.45 times as
  high.
- The JIT's lead is much larger in the scores than in fixed time. A fixed
  run of Richards or DeltaBlue lasts a few milliseconds, and a function
  runs in the interpreter until it is hot enough to compile, so much of
  that run is spent before the JIT's code takes over: with the JIT,
  Richards takes 1.11 times the interpreter's fixed time and DeltaBlue
  1.05 (Richards' three runs with it took 7.0 to 9.8 ms). The scores time
  warm runs, where Richards, DeltaBlue, Crypto, RayTrace and NavierStokes
  score 3.4 to 4.2 times what the interpreter does.
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
  Splay, 1.45 times the interpreter's score, and RegExp, 1.41 times, though
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
| Richards | 2.9 ms | 4.7 ms | 3.9 ms | 0.74x | 1.21x |
| DeltaBlue | 5.4 ms | 7.8 ms | 7.6 ms | 0.71x | 1.03x |
| Crypto | 51.7 ms | 22.5 ms | 107 ms | 0.48x | 0.21x |
| RayTrace | 25.7 ms | 32.6 ms | 55.2 ms | 0.47x | 0.59x |
| EarleyBoyer | 70.7 ms | 47.4 ms | 136 ms | 0.52x | 0.35x |
| RegExp | 114 ms | 130 ms | 462 ms | 0.25x | 0.28x |
| Splay | 142 ms | 93.1 ms | 461 ms | 0.31x | 0.20x |
| NavierStokes | 40.7 ms | 18.2 ms | 65.7 ms | 0.62x | 0.28x |
| PdfJS | 193 ms | 259 ms | 373 ms | 0.52x | 0.69x |
| Mandreel | 2,518 ms | 856 ms | 3,364 ms | 0.75x | 0.25x |
| Gameboy | 400 ms | 313 ms | 463 ms | 0.86x | 0.67x |
| CodeLoad | 12.3 ms | 13.3 ms | 12.8 ms | 0.96x | 1.04x |
| Box2D | 97.0 ms | 98.0 ms | 153 ms | 0.63x | 0.64x |
| zlib | 1,556 ms | 844 ms | 8,316 ms | 0.19x | 0.10x |
| Typescript | 1,759 ms | 1,992 ms | 2,131 ms | 0.83x | 0.93x |
| **Total** | **7,373 ms** | **5,109 ms** | **16,691 ms** | **0.44x** | **0.31x** |

| Are We Fast Yet | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| DeltaBlue | 574 ms | 319 ms | 713 ms | 0.81x | 0.45x |
| Richards | 3,297 ms | 682 ms | 4,790 ms | 0.69x | 0.14x |
| Json | 1,027 ms | 258 ms | 1,343 ms | 0.76x | 0.19x |
| CD | 1,711 ms | 485 ms | 2,825 ms | 0.61x | 0.17x |
| Havlak | 3,604 ms | 1,173 ms | 4,297 ms | 0.84x | 0.27x |
| Bounce | 788 ms | 244 ms | 1,484 ms | 0.53x | 0.16x |
| List | 585 ms | 159 ms | 1,250 ms | 0.47x | 0.13x |
| Mandelbrot | 175 ms | 40.9 ms | 694 ms | 0.25x | 0.06x |
| NBody | 644 ms | 121 ms | 1,021 ms | 0.63x | 0.12x |
| Permute | 936 ms | 232 ms | 1,677 ms | 0.56x | 0.14x |
| Queens | 674 ms | 117 ms | 1,057 ms | 0.64x | 0.11x |
| Sieve | 566 ms | 319 ms | 1,212 ms | 0.47x | 0.26x |
| Storage | 1,495 ms | 716 ms | 1,648 ms | 0.91x | 0.43x |
| Towers | 935 ms | 200 ms | 1,564 ms | 0.60x | 0.13x |
| **Total** | **17,013 ms** | **5,076 ms** | **25,667 ms** | **0.66x** | **0.20x** |

- Octane's zlib moved most: 5,675 to 1,556 ms in the interpreter, 0.19
  times QuickJS's time where it took 0.68, and 3,249 to 844 ms with the
  JIT, 0.10 times. zlib is an asm.js module, compiled from C by
  Emscripten, and its 51 functions now pass the validator
  (`b920ca25`): each runs as typed code (`047ee156`, `d8cbc918`), whose
  int32 and float64 registers hold raw words, so its `| 0` arithmetic, its
  compares and its heap's loads and stores convert nothing and check no
  kind; the ordinary register code, which it ran before, converted each
  element read and each `| 0` through a float. With the JIT the typed code
  is compiled at the module's link, as V8 compiled validated asm.js at its
  instantiation, with no guard on any kind (`bc00f894`, `5ae2317e`), and
  the members of a phi group that spill share one slot, which took zlib's
  largest function from 2.13 to 1.81 MB of code, its moves at the edges
  from 1.80 to 1.31 MB (`83a58f53`). The stack JIT of v0.38.0 took 2,027 ms, measured in
  the same session.
- Mandreel, compiled from C++ too, is not asm.js and runs as ordinary
  register code: 2,430 to 2,518 ms, 0.75 times QuickJS's time, and 808 to
  856 with the JIT. Its largest function, compiled at load before, is
  compiled at its first call now, within the timed run (`d6ac45be`). Its
  `| 0` arithmetic runs as int32 instructions (`add_i32`, `add_imm_i32`,
  the bitwise operators with an immediate) and its typed arrays' elements
  are read and written by native handlers, but each element read and each
  `| 0` still converts through a float: raw registers outside asm.js are
  designed and not built (register-interpreter.md, "Known limits and open
  work").
- Without the JIT, go-quickjs is ahead on every benchmark of both suites.
  Octane's CodeLoad, which is mostly compiling, takes 0.96 times QuickJS's
  time, and 1.04 with the JIT, where it took 1.22 and 1.37 at `258b4e96`:
  the front end compiled every function to the stack bytecode before the
  register backend compiled its register code, and now compiles to
  register code alone (`d6ac45be`). On Are We Fast Yet the interpreter is
  19% ahead on DeltaBlue, 16% on Havlak and about a third ahead on
  Richards, Queens and Towers. A call's window of registers is the
  callee's frame, so nothing is copied, a frame record is written only
  where a reader needs it, and on amd64 and arm64 a call, a construction
  and a return are native handlers that never enter Go
  ([Calls](#where-the-time-goes) below). The programs that allocate most
  are the least ahead: Storage 9%, Havlak 16%. Octane's RayTrace took 23%
  less than at `258b4e96`, 25.7 ms against 33.5, by the frameless
  constructions above, and Are We Fast Yet's List, Permute and Towers 6
  to 11% less.
- With the JIT, Are We Fast Yet takes 0.06 to 0.45 times QuickJS's time,
  its loops of arithmetic the least, its programs that allocate most --
  DeltaBlue, Storage -- the most, its total level with `258b4e96`'s.
- When the stack JIT was removed, its timing had the register JIT slower
  than the stack JIT just before it on Octane's RayTrace (+7.6%) and
  CodeLoad (+25%) and on Are We Fast Yet's Json (+8.2%), Permute (+7.0%)
  and Queens (+6.3%) (docs/performance-plan.md, "The register JIT's
  remaining gaps"). Against the stack JIT's figures at `acffb1aa` (270,
  227 and 119 ms), Json, Permute and Queens are level, 258, 232 and 117 ms;
  CodeLoad is faster, 14.2 then and 13.3 ms now. RayTrace took 32.6 ms,
  its six runs with the JIT in this session 31.3 to 33.4 ms.
- On eight of Octane's fifteen the JIT is behind the interpreter or level
  with it: Richards (62% behind), DeltaBlue (44%), PdfJS (34%), RayTrace
  (27%), RegExp (14%), Typescript (13%) and CodeLoad (8%) behind, Box2D
  level. RayTrace is the one more than at `258b4e96`: the interpreter's
  time fell by a quarter, the JIT's did not, since a runtime with the JIT
  keeps the frames the plain interpreter's calls and constructions now do
  without. Octane's fixed work is one run of each benchmark, so compiling
  counts, and the code runs in the interpreter until it is hot. The JIT's
  own figures are much what they were at `258b4e96` -- Richards 4.1 ms
  then, DeltaBlue 7.1, RayTrace 30.7, PdfJS 257, Typescript 2,033 --
  while the interpreter's are as fast or faster, which leaves less for the
  JIT's code to win back.

### Server rendering

The ssr suite renders a page with React 18's own builds, as a host behind
a web server does for each request: Render is one runtime rendering the
page again and again, FirstRender five fresh runtimes, each loading React
and rendering once. The same matrix, the median of three fresh-process
runs (lower is better):

| ssr | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| Render | 1,200 ms | 1,021 ms | 1,861 ms | 0.64x | 0.55x |
| FirstRender | 159 ms | 290 ms | | | |

Render took 0.64 and 0.54 times QuickJS's time at `258b4e96`, and
FirstRender 164 and 296 ms. Render is level; FirstRender, which compiles
React anew in each runtime, is 2 to 3% shorter, the front end compiling
register code alone (`d6ac45be`). v0.38.0, the stack design,
took 0.85 and 0.58 times, and FirstRender 197 and 366 ms, in the same
session. React writes a page as thousands of short
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
`3ecb8200`, and the figure is the fastest of three runs. The runners'
micro mode does this, from a copy of the file kept in `internal/v8bench`, and
the matrix above runs it. `sort_bench` is left out: it reports a run's time
over its fastest sort, not the time of an operation. Over the other 71 tests
go-quickjs takes 0.50 times QuickJS's time by the geometric mean, where it
took 0.49 at `258b4e96` and 0.79 in v0.38.0, the stack design, measured in
the same session: it is slower on 6, faster on 63, and within 5% on the
other two. With the JIT it takes 0.28 times QuickJS's time, slower on 2
and faster on 65. The table has the interpreter's largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `weak_map_delete` | 373 ns | 422 ns | 251 ns | 1.49x | 1.69x |
| `weak_map_set` | 123 ns | 95.2 ns | 92.7 ns | 1.32x | 1.03x |
| `bigint256_arith` | 166 ns | 109 ns | 126 ns | 1.32x | 0.86x |
| `date_parse` | 961 ns | 981 ns | 825 ns | 1.16x | 1.19x |
| `int_to_string` | 55.0 ns | 49.1 ns | 48.3 ns | 1.14x | 1.02x |
| `func_closure_call` | 35.7 ns | 2.79 ns | 31.8 ns | 1.12x | 0.09x |
| `prop_create` | 51.9 ns | 43.1 ns | 52.0 ns | 1.00x | 0.83x |
| `regexp_utf16` | 250 ns | 254 ns | 254 ns | 0.99x | 1.00x |
| `array_prop_create` | 14.0 ns | 15.7 ns | 15.0 ns | 0.93x | 1.05x |
| `map_set_bigint` | 196 ns | 106 ns | 215 ns | 0.91x | 0.50x |
| `string_length` | 3.76 ns | 0.92 ns | 14.1 ns | 0.27x | 0.07x |
| `global_func_call` | 8.59 ns | 2.07 ns | 32.5 ns | 0.26x | 0.06x |
| `string_build_large1` | 26.2 ns | 45.7 ns | 100 ns | 0.26x | 0.46x |
| `prop_update` | 2.79 ns | 1.88 ns | 11.5 ns | 0.24x | 0.16x |
| `func_call` | 7.08 ns | 0.94 ns | 30.9 ns | 0.23x | 0.03x |
| `array_for_in` | 16.0 ns | 11.6 ns | 77.0 ns | 0.21x | 0.15x |
| `string_build2c` | 25.7 ns | 42.0 ns | 141 ns | 0.18x | 0.30x |
| `array_for_of` | 7.49 ns | 4.87 ns | 46.1 ns | 0.16x | 0.11x |
| `array_slice` | 4.18 ns | 4.37 ns | 27.1 ns | 0.15x | 0.16x |
| `prop_clone` | 10.4 ns | 9.60 ns | 71.8 ns | 0.15x | 0.13x |

Of the gaps the stack design had, the largest are gone: in v0.38.0,
measured in the same session, `array_prop_create` took 2.25 times
QuickJS's time and takes 0.93, `global_read` 1.79 and 0.87, `array_read`
1.41 and 0.88, `empty_do_loop` 1.34 and 0.53.

The JIT does the most for a test that is a loop of arithmetic, locals,
property reads and writes, array elements and calls: `prop_read` takes
0.80 ns with it, `func_call` 0.94 ns, a fifth of the interpreter's time
or less. A test whose time is a built-in function's work runs much as it
does in the interpreter: regular expressions, `array_slice`, and the
conversions between numbers and strings. An object a function makes and
drops need not be made at all, even where it crosses a merge or goes round
a loop, as in `sum = sum.add(v)`: it is made only where it escapes, by
partial escape analysis, as in Graal (`41575a42`). `escape_accumulate`
and `carried_short` take 0.07 and 0.04 times QuickJS's time with the JIT,
and 0.57 without it. Code that keeps its objects gains nothing from this,
and the suites above hardly move for it.

The interpreter has gained more than the JIT, so some tests run slower
with the JIT than without it. In the run made for this page, 11 of these
71 took 7% or more longer with the JIT: `prop_delete`, `array_update`,
`array_prop_create`, `local_destruct`, `global_destruct`,
`global_destruct_strict`, `weak_map_delete`, `string_build1`,
`string_build2c`, `string_build_large1` and `string_to_int`. Of the extra
tests below that QuickJS runs, 31 did: `generator_delegate`,
`async_await`, `async_chain` and `async_generator`, two for-in loops over
an object, `hasOwnProperty` called through a borrowed reference, pushes
and spread calls of several arguments, `object_rest`, the five sorts, the
64-bit typed arrays' reads, a regular-expression search,
`json_parse_new_layout`, four of the `carried_` tests,
`array_index_append`, `template_numbers`, `prop_delete_last`, and the
64-bit typed arrays' writes, which take 5.3 to 5.6 times the
interpreter's time with the JIT (`typed_i64_write` 6.79 against 37.7 ns),
1.48 and 1.88 times QuickJS's.

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
of `JSON.parse` and `JSON.stringify` -- and since v0.39.0 Crypto's `am3`,
classes constructed and tested with `instanceof`, the kernels of an asm.js
module -- its loops over its heap of bytes, words and doubles, its calls,
a switch and a function table -- and a call from plain code into typed
code. Over the 328 QuickJS can run (it has no using declarations)
go-quickjs takes 0.52 times its time, and 0.26 times with the JIT, where it
took 0.53 and 0.26 over the 318 at `258b4e96`, and v0.38.0 0.70 and 0.30
over its 313; leaving out the JSON tests, 0.57 and 0.25. The asm.js
kernels' loops (`asm_adler`, `asm_crc32`, `asm_copy`, `asm_fnv`,
`asm_dot`, `asm_switch`) take 0.12 to 0.29 times QuickJS's time as typed
code, and 0.03 to 0.07 with the JIT, which compiles them at the module's
link; their recursion and calls (`asm_fib`, `asm_call_leaf`) 0.83 and
0.54, 0.30 and 0.21 with the JIT, and a call through a function table
1.05, 0.17 with the JIT. A switch on strings past its first few cases
takes 0.05 to 0.16 times QuickJS's time, its literal cases one lookup
(`100eb7f`). An await costs a fiftieth of QuickJS's time (`async_await`
15.1 against 742 ns): an async function resumed by its await's job goes
on in place at an await of a value that needs no job of its own. A
generator resumed from for-of is level (`generator_for_of` 1.02), where
v0.38.0 took 1.44 times, and a for-in over a dictionary takes 0.22 times,
where it took 1.41: its keys are made once for the dictionary's layout and
handed out by the native handlers. Calls in tail position take 0.57 to
0.60 times QuickJS's time. A global `exec` still takes 1.32 times, as it
makes a result and a String for each match; a sticky one 1.07. The
largest gaps either way:

| Test | go-quickjs | go-quickjs JIT | QuickJS | go-quickjs / QuickJS | JIT / QuickJS |
|---|---:|---:|---:|---:|---:|
| `scope_chain` | 685 ns | 704 ns | 362 ns | 1.89x | 1.94x |
| `array_literal_empty` | 74.9 ns | 61.5 ns | 40.4 ns | 1.85x | 1.52x |
| `subclass_fresh` | 1,160 ns | 1,209 ns | 679 ns | 1.71x | 1.78x |
| `set_proto_fresh` | 358 ns | 375 ns | 212 ns | 1.69x | 1.77x |
| `typed_u64_reduce` | 117 ns | 120 ns | 72.7 ns | 1.61x | 1.64x |
| `object_create_fresh` | 363 ns | 371 ns | 226 ns | 1.61x | 1.64x |
| `typed_u8_find` | 43.2 ns | 9.86 ns | 27.2 ns | 1.59x | 0.36x |
| `array_fill_down` | 135 ns | 144 ns | 85.6 ns | 1.58x | 1.68x |
| `typed_u64_forEach` | 120 ns | 109 ns | 79.9 ns | 1.50x | 1.36x |
| `typed_findIndex` | 45.5 ns | 10.3 ns | 30.4 ns | 1.50x | 0.34x |
| `asm_fnv` | 6.66 ns | 1.47 ns | 54.6 ns | 0.12x | 0.03x |
| `string_switch_last` | 27.2 ns | 13.0 ns | 265 ns | 0.10x | 0.05x |
| `json_stringify_decimals` | 32.8 ns | 34.5 ns | 330 ns | 0.10x | 0.10x |
| `string_switch_large` | 29.0 ns | 21.5 ns | 405 ns | 0.07x | 0.05x |
| `string_switch_miss` | 17.5 ns | 12.6 ns | 267 ns | 0.07x | 0.05x |
| `regexp_search_utf16` | 676 ns | 729 ns | 13,173 ns | 0.05x | 0.06x |
| `string_switch_nonstring` | 12.2 ns | 7.43 ns | 264 ns | 0.05x | 0.03x |
| `regexp_search_i` | 687 ns | 704 ns | 21,027 ns | 0.03x | 0.03x |
| `json_parse_long` | 1,858 ns | 1,869 ns | 84,773 ns | 0.02x | 0.02x |
| `async_await` | 15.1 ns | 16.8 ns | 742 ns | 0.02x | 0.02x |

A BigInt in the int64 range is held in the value itself (`9746906`), as
QuickJS holds one that fits in a word, and so is one below 2^64 in
magnitude (`f8c3229`); a larger one is a block of no pointers, made by the
engine's own arithmetic (`f1bf681`). Short BigInts' `+`, `-`, `*` and
comparisons are native handlers: `bigint64_arith` takes 0.37 times
QuickJS's time. A larger one is made by the engine's arithmetic in Go:
`bigint256_arith` takes 1.32 times, and the extra BigInt
tests 0.43 to 1.23 times, `bigint_toString` and `bigint64_xorshift` the
slowest. The JIT makes BigInts of 64 bits natively: `bigint64_arith`
takes 0.10 times QuickJS's time with it, `bigint64_xorshift` 0.05.

The largest gaps now are:

- objects made on a prototype that is itself new -- `Object.create` of a
  fresh object, `Object.setPrototypeOf` to one, a scope chain built as
  objects, a subclass made per call and its instance -- 1.61 to 1.89
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
  a `replace` by a template with `$` (1.32 and 1.35 times).

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
  0.21 to 0.66 times QuickJS's time, 0.43 by their geometric mean, where
  v0.38.0, its stack interpreter and its tree tier, took 0.45 to 1.15 and
  0.75 by the mean. `int_arith` takes 0.46 times QuickJS's time and
  `float_arith` 0.36.
- **Calls.** A call's window of registers is the callee's frame: the
  caller writes the callee, the receiver and the arguments there, and
  nothing is copied. A callee the call site's cache knows is entered with
  no question asked again, and only the fields of the frame record a
  reader needs are written. Natively, a call, a construction and a return
  are handlers of their own that keep the caller's state on a native
  frame stack and never enter Go (register-interpreter.md, "Frames and
  calls"). A method call whose body needs a frame takes 35.3 ns against
  68.4 in QuickJS (`method_call`), one that only returns its argument
  12.9 against 42.2 (`method_call_leaf`), an empty function 7.08 against
  30.9 (`func_call`); with the JIT they take 11.4, 2.63 and 0.94 ns. A
  recursive call is level: `call_fib` and `call_mutual` take 0.93 times
  QuickJS's time. A body that only returns a parameter or a constant, or a
  short pure one of numbers and upvalues, is answered at the call site
  without a frame; natively, so are a getter of `this.k`, `this.k.length`
  or `this.k[p]`, a `typeof` test, an initializer that stores its
  parameters in `this`, and a constructor that only forwards to one
  (`d54099e3`, `09409332`, `bdab5ba4`).
- **Property access.** Objects built the same way share a shape, as in
  QuickJS and V8, and each property read and write, global variable read
  and write, call and `instanceof` remembers where it last found what it
  looked for; a site that meets a second shape keeps up to four, and one
  that keeps meeting new shapes reads through the realm's shared cache, as
  V8's megamorphic stub cache does. The native handlers check the cache
  where the instruction runs: `prop_read` takes 0.51 times QuickJS's time,
  `prop_write` 0.37, `global_read` 0.87 and `global_write` 0.42. An object
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
- **Numbers as Values.** Outside asm.js's typed code every register holds
  a Value, so an element read and a `| 0` convert through a float where an
  int32 would do; raw registers for other code are designed and not
  built.

Where go-quickjs is ahead by most:

- **Crypto.** Its arithmetic is register code whose bitwise operators have
  forms with an immediate, and `(x + y) | 0` is one instruction, all
  native handlers:
  `crypto_am3` takes 0.71 times QuickJS's time, 0.20 with the JIT, and
  Crypto 0.48.
- **Code compiled from C to asm.js.** A validated asm.js module runs as
  typed code, its int32s and float64s raw in their registers, its heap's
  loads and stores natively, and with the JIT is compiled at its link with
  no speculation (`d8cbc918`, `5ae2317e`): Octane's zlib takes 0.19 times
  QuickJS's time, 0.10 with the JIT, and the asm.js kernels' loops 0.12 to
  0.29, 0.03 to 0.07 with it.
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
  rather than making a rope (`00ce03b5`): `string_build2c` takes 0.18 times
  QuickJS's time, `local_append_keep` 0.28 and `template_list` 0.35. The
  interpreter is ahead of the JIT on the first of these, 0.30 times with it.
- **JSON.** The 49 `json_` tests take 0.30 times QuickJS's time, and 0.31
  with the JIT, as the work is a built-in's. `JSON.parse` walks the shape
  tree as it reads, expecting the keys of the last object parsed in the same
  place, and makes each object once, at its last key. `JSON.stringify` keeps
  a layout's keys and slots from the second time it meets it (`097d71ec`).
  Numbers are read and written without strconv's general path (`baafd8e3`),
  and long strings are scanned with AVX2 or NEON where the machine has them:
  `json_parse_records` takes 0.33 times QuickJS's time,
  `json_stringify_records` 0.14 and `json_parse_long` 0.02.
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
  index rather than copying them into a list first (`array_for_in`, 0.21),
  and the array methods pass over a hole without looking its index up,
  where nothing up the prototype chain could have it
  (`array_hole_length_decr`, 0.34). A typed array's elements are read and
  written by native handlers for every element type but Float16
  (`typed_array_write`, 0.41).
- **`apply(this, arguments)`.** In a function whose only use of
  `arguments` is passing it to `apply` (a common way to write a class
  constructor), the arguments object is never made: the call hands on the
  frame's arguments as they were passed (`apply_args`). A constructor
  whose whole body is `this.initialize.apply(this, arguments)`, as
  RayTrace's are, is constructed by its call site as a forwarder, and
  natively, where the target is an initializer, without entering either
  frame (`746d9438`, `bdab5ba4`): `forward_construct` takes 0.23 times
  QuickJS's time, where it took 0.54 at `258b4e96`, and 0.02 with the
  JIT.

What remains in the interpreter is mostly in Go: the operations with no
native handler and the fast paths that miss, which exit to their Go
handlers, the built-ins and the object model -- the gaps above are in
them -- and the collector. The JIT is the way past the rest: a hot
function compiled to native code dispatches nothing, which is why, with
it, the loops and calls above take from a third of the interpreter's
time down to a tenth.

[goja]: https://github.com/dop251/goja
[v8-v7]: https://github.com/mozilla/arewefastyet/tree/master/benchmarks/v8-v7
